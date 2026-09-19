package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

const (
	cash     = "assets.cash"
	customer = "liabilities.customer"
	fees     = "revenue.fees"
	cashUSD  = "assets.cash_usd"
)

func TestPostWritesBothLegsAndMovesTheBalances(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	id, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Customer deposit",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4500},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if id == "" {
		t.Fatal("post returned an empty transaction id")
	}

	if got := balance(t, tx, cash); got != 4500 {
		t.Errorf("%s = %d, want 4500", cash, got)
	}
	if got := balance(t, tx, customer); got != -4500 {
		t.Errorf("%s = %d, want -4500", customer, got)
	}
	if got := legsOf(t, tx, id); got != 2 {
		t.Errorf("transaction has %d legs, want 2", got)
	}
	if got := ledgerTotal(t, tx); got != 0 {
		t.Errorf("the ledger nets to %d, want 0", got)
	}
}

func TestPostWritesMoreThanTwoLegs(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	if _, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Deposit, less a 1.50 fee",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4350},
			{Account: fees, AmountMinor: -150},
		},
	}); err != nil {
		t.Fatalf("post: %v", err)
	}

	if got := balance(t, tx, fees); got != -150 {
		t.Errorf("%s = %d, want -150", fees, got)
	}
	if got := ledgerTotal(t, tx); got != 0 {
		t.Errorf("the ledger nets to %d, want 0", got)
	}
}

func TestPostKeepsTheGivenOccurredAt(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	backdated := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)
	id, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Deposit banked on the first",
		OccurredAt:  backdated,
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4500},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	var got time.Time
	if err := tx.QueryRow(`SELECT occurred_at FROM transactions WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("read occurred_at: %v", err)
	}
	if !got.Equal(backdated) {
		t.Errorf("occurred_at = %s, want %s", got, backdated)
	}
}

func TestPostRefusesEntriesTheLedgerWillNotHold(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry ledger.Entry
		want  error
		code  string
	}{
		{
			name: "legs that do not cancel",
			entry: ledger.Entry{Currency: "GBP", Description: "Fifty pence short", Legs: []ledger.Leg{
				{Account: cash, AmountMinor: 4500},
				{Account: customer, AmountMinor: -4450},
			}},
			want: ledger.ErrUnbalanced,
			code: "LB001",
		},
		{
			name:  "no legs at all",
			entry: ledger.Entry{Currency: "GBP", Description: "Nothing attached"},
			want:  ledger.ErrUnbalanced,
			code:  "LB001",
		},
		{
			name: "one leg",
			entry: ledger.Entry{Currency: "GBP", Description: "Money from nowhere", Legs: []ledger.Leg{
				{Account: cash, AmountMinor: 4500},
			}},
			want: ledger.ErrUnbalanced,
			code: "LB001",
		},
		{
			name: "an account code nothing holds",
			entry: ledger.Entry{Currency: "GBP", Description: "Typo", Legs: []ledger.Leg{
				{Account: cash, AmountMinor: 4500},
				{Account: "liabilities.custmoer", AmountMinor: -4500},
			}},
			want: ledger.ErrUnknownAccount,
			code: "23502",
		},
		{
			name: "an account in another currency",
			entry: ledger.Entry{Currency: "GBP", Description: "Mixed currency", Legs: []ledger.Leg{
				{Account: cash, AmountMinor: 4500},
				{Account: cashUSD, AmountMinor: -4500},
			}},
			want: ledger.ErrCurrencyMismatch,
			code: "23503",
		},
		{
			name: "a leg that moves nothing",
			entry: ledger.Entry{Currency: "GBP", Description: "Zero leg", Legs: []ledger.Leg{
				{Account: cash, AmountMinor: 0},
				{Account: customer, AmountMinor: 0},
			}},
			want: ledger.ErrRejected,
			code: "23514",
		},
		{
			name: "a description that says nothing",
			entry: ledger.Entry{Currency: "GBP", Description: "   ", Legs: []ledger.Leg{
				{Account: cash, AmountMinor: 4500},
				{Account: customer, AmountMinor: -4500},
			}},
			want: ledger.ErrRejected,
			code: "23514",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx := testdb.Tx(t)
			seedAccounts(t, tx)

			id, err := ledger.Post(context.Background(), tx, c.entry)
			if err == nil {
				t.Fatalf("post returned id %s and no error — the ledger took it", id)
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("error is %v, want %v", err, c.want)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("error does not carry the server's own: %v", err)
			}
			if pgErr.Code != c.code {
				t.Fatalf("SQLSTATE %s, want %s: %v", pgErr.Code, c.code, err)
			}
			t.Logf("%s: %s", pgErr.Code, err)
		})
	}
}

func TestARefusedEntryLeavesNothingBehindAndTheCallerCarriesOn(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	if _, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Fifty pence short",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4450},
		},
	}); !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("post: %v, want %v", err, ledger.ErrUnbalanced)
	}

	if got := transactionCount(t, tx); got != 0 {
		t.Errorf("%d transactions after the refusal, want 0", got)
	}
	if got := balance(t, tx, cash); got != 0 {
		t.Errorf("%s = %d after the refusal, want 0", cash, got)
	}

	if _, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Customer deposit",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4500},
		},
	}); err != nil {
		t.Fatalf("post after a refusal: %v", err)
	}
	if got := balance(t, tx, cash); got != 4500 {
		t.Errorf("%s = %d, want 4500", cash, got)
	}
}

func seedAccounts(t *testing.T, tx *sql.Tx) {
	t.Helper()

	if _, err := tx.Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES
		   ($1, 'Cash at bank',       'asset',     'GBP'),
		   ($2, 'Customer balances',  'liability', 'GBP'),
		   ($3, 'Fee income',         'revenue',   'GBP'),
		   ($4, 'Cash at bank (USD)', 'asset',     'USD')`,
		cash, customer, fees, cashUSD,
	); err != nil {
		t.Fatalf("seed accounts: %v", err)
	}
}

func balance(t *testing.T, tx *sql.Tx, code string) int64 {
	t.Helper()

	var n int64
	if err := tx.QueryRow(
		`SELECT balance_minor FROM account_balances WHERE code = $1`, code,
	).Scan(&n); err != nil {
		t.Fatalf("balance of %s: %v", code, err)
	}
	return n
}

func ledgerTotal(t *testing.T, tx *sql.Tx) int64 {
	t.Helper()

	return scalar(t, tx, `SELECT coalesce(sum(amount_minor), 0) FROM postings`)
}

func legsOf(t *testing.T, tx *sql.Tx, id string) int64 {
	t.Helper()

	return scalar(t, tx, `SELECT count(*) FROM postings WHERE transaction_id = $1`, id)
}

func transactionCount(t *testing.T, tx *sql.Tx) int64 {
	t.Helper()

	return scalar(t, tx, `SELECT count(*) FROM transactions`)
}

func scalar(t *testing.T, tx *sql.Tx, query string, args ...any) int64 {
	t.Helper()

	var n int64
	if err := tx.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}
