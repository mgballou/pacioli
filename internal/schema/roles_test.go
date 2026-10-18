package schema_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/testdb"
)

func TestANonOwnerRoleCannotDisableLedgerTriggers(t *testing.T) {
	role := createTestRole(t)
	tx := testdb.Tx(t)
	mustExec(t, tx, `SET LOCAL ROLE `+role)

	_, err := tx.Exec(`ALTER TABLE postings DISABLE TRIGGER ALL`)
	assertCode(t, err, "42501")
}

func TestANonOwnerRoleWithLedgerDMLWithoutBalanceCheckRightsCannotPost(t *testing.T) {
	role := createTestRole(t)
	grantLedgerDML(t, role)
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	mustExec(t, tx, `SET LOCAL ROLE `+role)
	txnID := newTransaction(t, tx, "GBP", "Queue privilege required")

	_, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 VALUES ($1, $2, 'GBP', 4500)`, txnID, cash,
	)
	assertCode(t, err, "42501")
}

func TestARoleWithQueueRightsCanPostWithoutOwningLedgerTables(t *testing.T) {
	role := createTestRole(t)
	grantLedgerDML(t, role)
	grantQueueAndReadRights(t, role)
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	mustExec(t, tx, `SET LOCAL ROLE `+role)
	txnID := newTransaction(t, tx, "GBP", "Balanced role entry")
	if _, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor) VALUES
		 ($1, $2, 'GBP', 4500), ($1, $3, 'GBP', -4500)`, txnID, cash, customer,
	); err != nil {
		t.Fatalf("post balanced entry as %s: %v", role, err)
	}
	mustSettle(t, tx)

	var balance string
	if err := tx.QueryRow(`SELECT balance_minor FROM account_balances WHERE account_id = $1`, cash).Scan(&balance); err != nil {
		t.Fatalf("read posted balance as %s: %v", role, err)
	}
	if balance != "4500" {
		t.Errorf("cash balance = %s, want 4500", balance)
	}

	mustExec(t, tx, `SAVEPOINT append_only`)
	_, err := tx.Exec(`UPDATE postings SET amount_minor = 1 WHERE transaction_id = $1`, txnID)
	assertCode(t, err, codeAppendOnly)
	mustExec(t, tx, `ROLLBACK TO SAVEPOINT append_only`)

	mustExec(t, tx, `SAVEPOINT trigger_ownership`)
	_, err = tx.Exec(`ALTER TABLE postings DISABLE TRIGGER ALL`)
	assertCode(t, err, "42501")
	mustExec(t, tx, `ROLLBACK TO SAVEPOINT trigger_ownership`)
}

func createTestRole(t *testing.T) string {
	t.Helper()

	role := "schema_test_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	db := testdb.Open(t)
	if _, err := db.Exec(`CREATE ROLE ` + role); err != nil {
		if code := sqlState(err); code == "42501" {
			t.Skipf("requires CREATEROLE privilege to create %s", role)
		}
		t.Fatalf("create role %s: %v", role, err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DROP OWNED BY ` + role); err != nil {
			t.Errorf("drop objects owned by %s: %v", role, err)
		}
		if _, err := db.Exec(`DROP ROLE ` + role); err != nil {
			t.Errorf("drop role %s: %v", role, err)
		}
	})
	return role
}

func grantLedgerDML(t *testing.T, role string) {
	t.Helper()
	db := testdb.Open(t)
	if _, err := db.Exec(fmt.Sprintf(
		`GRANT INSERT, SELECT, UPDATE, DELETE ON accounts, transactions, postings TO %s`, role,
	)); err != nil {
		t.Fatalf("grant ledger DML to %s: %v", role, err)
	}
	if _, err := db.Exec(fmt.Sprintf(`GRANT USAGE, SELECT ON SEQUENCE postings_id_seq TO %s`, role)); err != nil {
		t.Fatalf("grant posting sequence to %s: %v", role, err)
	}
}

func grantQueueAndReadRights(t *testing.T, role string) {
	t.Helper()
	db := testdb.Open(t)
	if _, err := db.Exec(fmt.Sprintf(
		`GRANT SELECT, INSERT, DELETE ON balance_checks TO %s;
         GRANT SELECT ON account_balances TO %s`, role, role,
	)); err != nil {
		t.Fatalf("grant queue rights to %s: %v", role, err)
	}
}

func sqlState(err error) string {
	if err == nil {
		return ""
	}
	var pgErr interface{ SQLState() string }
	if ok := errors.As(err, &pgErr); ok {
		return pgErr.SQLState()
	}
	return ""
}
