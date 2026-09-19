package schema_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/testdb"
)

const (
	codeUnbalanced   = "LB001"
	codeAppendOnly   = "LB002"
	codeUnsettledKey = "LB003"
	codeSettledKey   = "LB004"
	codeCheck        = "23514"
	codeForeignKey   = "23503"
	codeNotNullOrDup = "23505"
)

const (
	cash     = "11111111-1111-1111-1111-111111111111"
	customer = "22222222-2222-2222-2222-222222222222"
	fees     = "33333333-3333-3333-3333-333333333333"
	usdCash  = "44444444-4444-4444-4444-444444444444"
)

func TestBalancedTransactionIsAccepted(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	txnID := postTransaction(t, tx, "Customer deposit", leg{cash, 4500}, leg{customer, -4500})
	mustSettle(t, tx)

	if got := balance(t, tx, cash); got != 4500 {
		t.Errorf("cash balance = %d, want 4500", got)
	}
	if got := balance(t, tx, customer); got != -4500 {
		t.Errorf("customer balance = %d, want -4500", got)
	}
	if got := netOf(t, tx, txnID); got != 0 {
		t.Errorf("transaction nets to %d, want 0", got)
	}
}

func TestUnbalancedTransactionIsRejected(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	postTransaction(t, tx, "Fifty pence short", leg{cash, 4500}, leg{customer, -4450})

	_, err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
	assertCode(t, err, codeUnbalanced)
}

func TestTransactionWithNoPostingsIsRejected(t *testing.T) {
	tx := testdb.Tx(t)

	if _, err := tx.Exec(
		`INSERT INTO transactions (currency, description) VALUES ('GBP', 'Empty')`,
	); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}

	_, err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
	assertCode(t, err, codeUnbalanced)
}

func TestPostingsCannotBeChangedOrDeleted(t *testing.T) {
	for _, c := range []struct {
		name  string
		query string
	}{
		{"update", `UPDATE postings SET amount_minor = 1 WHERE transaction_id = $1`},
		{"delete", `DELETE FROM postings WHERE transaction_id = $1`},
		{"truncate", `TRUNCATE postings`},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx := testdb.Tx(t)
			seedAccounts(t, tx)
			txnID := postTransaction(t, tx, "Customer deposit", leg{cash, 4500}, leg{customer, -4500})
			mustSettle(t, tx)

			var err error
			if c.name == "truncate" {
				_, err = tx.Exec(c.query)
			} else {
				_, err = tx.Exec(c.query, txnID)
			}
			assertCode(t, err, codeAppendOnly)
		})
	}
}

func TestPostingCurrencyMustMatchAccountAndTransaction(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	txnID := newTransaction(t, tx, "GBP", "Mixed currency")
	_, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 VALUES ($1, $2, 'USD', 4500)`,
		txnID, usdCash,
	)
	assertCode(t, err, codeForeignKey)
}

func TestZeroAmountPostingIsRejected(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	txnID := newTransaction(t, tx, "GBP", "Nothing happened")
	_, err := tx.Exec(
		`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
		 VALUES ($1, $2, 'GBP', 0)`,
		txnID, cash,
	)
	assertCode(t, err, codeCheck)
}

func TestAccountCodesAreUnique(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := tx.Exec(
		`INSERT INTO accounts (code, name, kind, currency)
		 VALUES ('assets.cash', 'Cash again', 'asset', 'GBP')`,
	)
	assertCode(t, err, codeNotNullOrDup)
}

func TestAnIdempotencyKeyIsTakenOnlyOnce(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	txnID := settledTransaction(t, tx)

	reserve(t, tx, "a-key-nobody-else-has", txnID)

	_, err := tx.Exec(
		`INSERT INTO idempotency_keys (key, request_hash, transaction_id)
		 VALUES ('a-key-nobody-else-has', sha256('again'), $1)`, txnID)
	assertCode(t, err, codeNotNullOrDup)
}

func TestAKeyWithNoTransactionCannotBeCommitted(t *testing.T) {
	tx := testdb.Tx(t)

	if _, err := tx.Exec(
		`INSERT INTO idempotency_keys (key, request_hash)
		 VALUES ('reserved-and-abandoned', sha256('body'))`,
	); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	_, err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
	assertCode(t, err, codeUnsettledKey)
}

func TestAReservedKeyCanBeGivenUp(t *testing.T) {
	tx := testdb.Tx(t)

	if _, err := tx.Exec(
		`INSERT INTO idempotency_keys (key, request_hash)
		 VALUES ('reserved-then-released', sha256('body'))`,
	); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM idempotency_keys WHERE key = 'reserved-then-released'`); err != nil {
		t.Fatalf("give it up: %v", err)
	}
	mustSettle(t, tx)
}

func TestASettledKeyCannotBeRewritten(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	first := settledTransaction(t, tx)
	second := settledTransaction(t, tx)

	reserve(t, tx, "settled-once-and-for-all", first)

	_, err := tx.Exec(
		`UPDATE idempotency_keys SET transaction_id = $1 WHERE key = 'settled-once-and-for-all'`,
		second)
	assertCode(t, err, codeSettledKey)
}

func TestTheRequestAKeyWasUsedForCannotBeRewritten(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	txnID := settledTransaction(t, tx)

	reserve(t, tx, "the-request-is-final", txnID)

	_, err := tx.Exec(
		`UPDATE idempotency_keys SET request_hash = sha256('a different body')
		  WHERE key = 'the-request-is-final'`)
	assertCode(t, err, codeSettledKey)
}

func TestAKeyTooShortToBeUnguessableIsRefused(t *testing.T) {
	tx := testdb.Tx(t)

	_, err := tx.Exec(
		`INSERT INTO idempotency_keys (key, request_hash) VALUES ('1', sha256('body'))`)
	assertCode(t, err, codeCheck)
}

func TestAKeyMustNameATransactionThatExists(t *testing.T) {
	tx := testdb.Tx(t)

	_, err := tx.Exec(
		`INSERT INTO idempotency_keys (key, request_hash, transaction_id)
		 VALUES ('names-a-transaction-that-is-not-there', sha256('body'),
		         '99999999-9999-9999-9999-999999999999')`)
	assertCode(t, err, codeForeignKey)
}

type leg struct {
	account string
	amount  int64
}

func seedAccounts(t *testing.T, tx *sql.Tx) {
	t.Helper()

	if _, err := tx.Exec(
		`INSERT INTO accounts (id, code, name, kind, currency) VALUES
		   ($1, 'assets.cash',          'Cash at bank',      'asset',     'GBP'),
		   ($2, 'liabilities.customer', 'Customer balances', 'liability', 'GBP'),
		   ($3, 'revenue.fees',         'Fee income',        'revenue',   'GBP'),
		   ($4, 'assets.cash_usd',      'Cash at bank (USD)','asset',     'USD')`,
		cash, customer, fees, usdCash,
	); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
}

func newTransaction(t *testing.T, tx *sql.Tx, currency, description string) string {
	t.Helper()

	var id string
	if err := tx.QueryRow(
		`INSERT INTO transactions (currency, description) VALUES ($1, $2) RETURNING id`,
		currency, description,
	).Scan(&id); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	return id
}

func postTransaction(t *testing.T, tx *sql.Tx, description string, legs ...leg) string {
	t.Helper()

	txnID := newTransaction(t, tx, "GBP", description)
	for _, l := range legs {
		if _, err := tx.Exec(
			`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
			 VALUES ($1, $2, 'GBP', $3)`,
			txnID, l.account, l.amount,
		); err != nil {
			t.Fatalf("insert posting %+v: %v", l, err)
		}
	}
	return txnID
}

func settledTransaction(t *testing.T, tx *sql.Tx) string {
	t.Helper()

	mustDefer(t, tx)
	id := postTransaction(t, tx, "Customer deposit", leg{cash, 4500}, leg{customer, -4500})
	mustSettle(t, tx)
	return id
}

func reserve(t *testing.T, tx *sql.Tx, key, txnID string) {
	t.Helper()

	mustDefer(t, tx)
	if _, err := tx.Exec(
		`INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, sha256('body'))`, key,
	); err != nil {
		t.Fatalf("reserve %s: %v", key, err)
	}
	if _, err := tx.Exec(
		`UPDATE idempotency_keys SET transaction_id = $2 WHERE key = $1`, key, txnID,
	); err != nil {
		t.Fatalf("settle %s: %v", key, err)
	}
}

func mustDefer(t *testing.T, tx *sql.Tx) {
	t.Helper()

	if _, err := tx.Exec(`SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatalf("defer: %v", err)
	}
}

// mustSettle forces the deferred checks to run now. Tests roll back rather than commit, so without it the constraint is never asked.
func mustSettle(t *testing.T, tx *sql.Tx) {
	t.Helper()

	if _, err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
		t.Fatalf("settle: %v", err)
	}
}

func balance(t *testing.T, tx *sql.Tx, accountID string) int64 {
	t.Helper()

	var n int64
	if err := tx.QueryRow(
		`SELECT balance_minor FROM account_balances WHERE account_id = $1`, accountID,
	).Scan(&n); err != nil {
		t.Fatalf("balance of %s: %v", accountID, err)
	}
	return n
}

func netOf(t *testing.T, tx *sql.Tx, txnID string) int64 {
	t.Helper()

	var n int64
	if err := tx.QueryRow(
		`SELECT coalesce(sum(amount_minor), 0) FROM postings WHERE transaction_id = $1`, txnID,
	).Scan(&n); err != nil {
		t.Fatalf("net of %s: %v", txnID, err)
	}
	return n
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got no error — the database accepted it", want)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected SQLSTATE %s, got a non-Postgres error: %v", want, err)
	}
	if pgErr.Code != want {
		t.Fatalf("SQLSTATE %s, want %s: %v", pgErr.Code, want, err)
	}
	t.Logf("%s: %s", pgErr.Code, pgErr.Message)
}
