package schema_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

// Every string a client chooses used to run to the 1 MB request body: a
// 100 kB account code reached a URL path segment, and four 250 kB names made a
// megabyte out of a four-row report. These are the lengths the schema now holds
// them to.

func TestAValueLongerThanItsColumnAllowsIsRefused(t *testing.T) {
	for _, c := range []struct {
		field string
		most  int
		write func(*sql.Tx, string) error
	}{
		{"accounts.code", ledger.MaxCode, func(tx *sql.Tx, v string) error {
			_, err := tx.Exec(
				`INSERT INTO accounts (code, name, kind, currency) VALUES ($1, 'Long', 'asset', 'GBP')`,
				"a"+strings.Repeat("b", len(v)-1))
			return err
		}},
		{"accounts.name", ledger.MaxName, func(tx *sql.Tx, v string) error {
			_, err := tx.Exec(
				`INSERT INTO accounts (code, name, kind, currency) VALUES ('assets.long', $1, 'asset', 'GBP')`, v)
			return err
		}},
		{"transactions.description", ledger.MaxDescription, func(tx *sql.Tx, v string) error {
			_, err := tx.Exec(`INSERT INTO transactions (currency, description) VALUES ('GBP', $1)`, v)
			return err
		}},
	} {
		t.Run(c.field, func(t *testing.T) {
			tx := testdb.Tx(t)

			assertCode(t, c.write(tx, strings.Repeat("x", c.most+1)), codeCheck)
		})
	}
}

// The bound is the last value it takes, not the first it refuses.
func TestTheLongestValueAColumnAllowsIsHeld(t *testing.T) {
	tx := testdb.Tx(t)

	code := "a" + strings.Repeat("b", ledger.MaxCode-1)
	if _, err := tx.Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES ($1, $2, 'asset', 'GBP')`,
		code, strings.Repeat("n", ledger.MaxName),
	); err != nil {
		t.Errorf("a %d-character code and a %d-character name were refused: %v",
			ledger.MaxCode, ledger.MaxName, err)
	}
	if _, err := tx.Exec(
		`INSERT INTO transactions (currency, description) VALUES ('GBP', $1)`,
		strings.Repeat("d", ledger.MaxDescription),
	); err != nil {
		t.Errorf("a %d-character description was refused: %v", ledger.MaxDescription, err)
	}
}

// The numbers are written twice — once in the CHECK, once in internal/ledger so
// a refusal can say them — and this is what stops the two drifting apart.
func TestTheSchemaHoldsTheLengthsTheLedgerPackageNames(t *testing.T) {
	for _, c := range []struct {
		constraint string
		// what the CHECK has to say for it to hold that number: the code's
		// length is inside its regex, after the leading letter.
		holds string
	}{
		{"accounts_code_check", fmt.Sprintf("{0,%d}", ledger.MaxCode-1)},
		{"accounts_name_check", fmt.Sprintf("<= %d", ledger.MaxName)},
		{"transactions_description_check", fmt.Sprintf("<= %d", ledger.MaxDescription)},
	} {
		t.Run(c.constraint, func(t *testing.T) {
			tx := testdb.Tx(t)

			var def string
			if err := tx.QueryRow(
				`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, c.constraint,
			).Scan(&def); err != nil {
				t.Fatalf("read %s: %v", c.constraint, err)
			}
			if !strings.Contains(def, c.holds) {
				t.Errorf("%s is %s, and internal/ledger tells clients it holds %q",
					c.constraint, def, c.holds)
			}
		})
	}
}

// The quadratic the bound was found next to. A deferred check has to be FOR
// EACH ROW, so one hung off postings queued a check per leg and each one summed
// every leg. These two say the check is queued once for the entry however many
// legs arrive, and that the queue is empty again once it has run.
func TestManyLegsQueueOneBalanceCheck(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	mustDefer(t, tx)

	txnID := postManyLegs(t, tx, 1000)
	if got := queued(t, tx, txnID); got != 1 {
		t.Errorf("1000 legs queued %d balance checks, want 1", got)
	}

	mustSettle(t, tx)
	if got := queued(t, tx, txnID); got != 0 {
		t.Errorf("%d balance checks are still queued after the check ran, want 0", got)
	}
}

// The check clears its own row, so a leg added after it queues another one.
// Without that, an entry could be settled and then quietly unbalanced.
func TestALegAddedAfterACheckIsCheckedAgain(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	mustDefer(t, tx)

	txnID := postTransaction(t, tx, "Customer deposit", leg{cash, 4500}, leg{customer, -4500})
	mustSettle(t, tx)
	mustDefer(t, tx)

	if _, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 VALUES ($1, $2, 'GBP', 1)`, txnID, fees,
	); err != nil {
		t.Fatalf("add a leg to a settled entry: %v", err)
	}
	if got := queued(t, tx, txnID); got != 1 {
		t.Errorf("the added leg queued %d balance checks, want 1", got)
	}

	_, err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
	assertCode(t, err, codeUnbalanced)
}

// One check for a thousand legs is still a check: an entry a penny out is
// refused at exactly the leg count the fast path is for.
func TestAThousandLeggedEntryAPennyOutIsRefused(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	mustDefer(t, tx)

	txnID := postManyLegs(t, tx, 1000)
	if _, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 VALUES ($1, $2, 'GBP', 1)`, txnID, fees,
	); err != nil {
		t.Fatalf("add the odd penny: %v", err)
	}

	_, err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
	assertCode(t, err, codeUnbalanced)
}

// postManyLegs writes one entry of n legs that cancel in pairs, in one
// statement, so the row trigger fires n times inside a single command.
func postManyLegs(t *testing.T, tx *sql.Tx, n int) string {
	t.Helper()

	txnID := newTransaction(t, tx, "GBP", "An entry of many legs")
	if _, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 SELECT $1, $2, 'GBP', CASE WHEN g % 2 = 0 THEN 100 ELSE -100 END
		   FROM generate_series(1, $3) g`,
		txnID, cash, n,
	); err != nil {
		t.Fatalf("write %d legs: %v", n, err)
	}
	return txnID
}

// queued counts the balance checks waiting on a transaction.
func queued(t *testing.T, tx *sql.Tx, txnID string) int {
	t.Helper()

	var n int
	if err := tx.QueryRow(
		`SELECT count(*) FROM balance_checks WHERE transaction_id = $1`, txnID,
	).Scan(&n); err != nil {
		t.Fatalf("count the queued checks: %v", err)
	}
	return n
}
