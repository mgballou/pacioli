package schema_test

import (
	"database/sql"
	"testing"

	"github.com/mgballou/pacioli/internal/testdb"
)

func TestASecondTransactionCannotAddTheBalancingLegAfterRollback(t *testing.T) {
	db := testdb.Disposable(t, "rollback_balance_test")
	seed, err := db.Begin()
	if err != nil {
		t.Fatalf("begin account setup: %v", err)
	}
	seedAccounts(t, seed)
	if err := seed.Commit(); err != nil {
		t.Fatalf("commit account setup: %v", err)
	}

	first, err := db.Begin()
	if err != nil {
		t.Fatalf("begin first transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := first.Rollback(); err != nil && err != sql.ErrTxDone {
			t.Errorf("rollback first transaction: %v", err)
		}
	})
	txnID := newTransaction(t, first, "GBP", "Rolled-back entry")
	if _, err := first.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 VALUES ($1, $2, 'GBP', 4500)`, txnID, cash,
	); err != nil {
		t.Fatalf("insert first leg: %v", err)
	}
	_, err = first.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
	assertCode(t, err, codeUnbalanced)
	if err := first.Rollback(); err != nil {
		t.Fatalf("roll back first transaction: %v", err)
	}

	second, err := db.Begin()
	if err != nil {
		t.Fatalf("begin second transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Rollback(); err != nil && err != sql.ErrTxDone {
			t.Errorf("rollback second transaction: %v", err)
		}
	})
	_, err = second.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 VALUES ($1, $2, 'GBP', -4500)`, txnID, customer,
	)
	assertCode(t, err, codeForeignKey)
}

func TestSavepointRollbackClearsTheQueuedBalanceCheck(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	txnID := newTransaction(t, tx, "GBP", "Entry after savepoint rollback")
	mustExec(t, tx, `SAVEPOINT partial_entry`)
	if _, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 VALUES ($1, $2, 'GBP', 4500)`, txnID, cash,
	); err != nil {
		t.Fatalf("insert temporary leg: %v", err)
	}
	var queued int
	if err := tx.QueryRow(`SELECT count(*) FROM balance_checks WHERE transaction_id = $1`, txnID).Scan(&queued); err != nil {
		t.Fatalf("read queued balance checks before rollback: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued balance checks before savepoint rollback = %d, want 1", queued)
	}
	mustExec(t, tx, `ROLLBACK TO SAVEPOINT partial_entry`)
	queued = -1
	if err := tx.QueryRow(`SELECT count(*) FROM balance_checks WHERE transaction_id = $1`, txnID).Scan(&queued); err != nil {
		t.Fatalf("read queued balance checks: %v", err)
	}
	if queued != 0 {
		t.Fatalf("queued balance checks after savepoint rollback = %d, want 0", queued)
	}

	if _, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor) VALUES
		 ($1, $2, 'GBP', 4500), ($1, $3, 'GBP', -4500)`, txnID, cash, customer,
	); err != nil {
		t.Fatalf("insert balanced legs after rollback: %v", err)
	}
	mustSettle(t, tx)
}
