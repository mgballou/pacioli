package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

const savings = "assets.savings"

func TestAnOpenedAccountIsInTheChartAtZero(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)

	if err := ledger.Open(ctx, tx, ledger.Account{
		Code: savings, Name: "Savings at bank", Kind: "asset", Currency: "GBP",
	}); err != nil {
		t.Fatalf("open: %v", err)
	}

	b, err := ledger.BalanceOf(ctx, tx, savings)
	if err != nil {
		t.Fatalf("balance of the account just opened: %v", err)
	}
	if b.Account != savings || b.Name != "Savings at bank" || b.Kind != "asset" || b.Currency != "GBP" {
		t.Errorf("balance = %+v, want the account that was opened", b)
	}
	if !b.AmountMinor.IsZero() || b.Postings != 0 {
		t.Errorf("balance = %s over %d postings, want 0 over 0", b.AmountMinor, b.Postings)
	}
}

func TestOpeningACodeThatIsAlreadyHeldIsRefused(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)

	first := ledger.Account{Code: savings, Name: "Savings at bank", Kind: "asset", Currency: "GBP"}
	if err := ledger.Open(ctx, tx, first); err != nil {
		t.Fatalf("open: %v", err)
	}

	second := ledger.Account{Code: savings, Name: "Someone else's savings", Kind: "asset", Currency: "GBP"}
	err := ledger.Open(ctx, tx, second)
	if !errors.Is(err, ledger.ErrAccountExists) {
		t.Fatalf("second open = %v, want %v", err, ledger.ErrAccountExists)
	}

	b, err := ledger.BalanceOf(ctx, tx, savings)
	if err != nil {
		t.Fatalf("balance after the refusal: %v", err)
	}
	if b.Name != "Savings at bank" {
		t.Errorf("name = %q, want the first account's — the refused open overwrote it", b.Name)
	}
	if n := accountCount(t, tx, savings); n != 1 {
		t.Errorf("%d accounts hold %s, want 1", n, savings)
	}
}

func TestOpeningAnAccountOfAKindThatDoesNotExistIsRefused(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)

	err := ledger.Open(ctx, tx, ledger.Account{
		Code: savings, Name: "Savings at bank", Kind: "assets", Currency: "GBP",
	})
	if !errors.Is(err, ledger.ErrUnknownKind) {
		t.Fatalf("open = %v, want %v", err, ledger.ErrUnknownKind)
	}
	if n := accountCount(t, tx, savings); n != 0 {
		t.Errorf("%d accounts hold %s after the refusal, want 0", n, savings)
	}
}

func TestTheSchemasOwnChecksRefuseAnAccountAndSayWhich(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		name    string
		account ledger.Account
	}{
		{"a code that is not a dotted lower-case path", ledger.Account{
			Code: "Assets.Savings", Name: "Savings", Kind: "asset", Currency: "GBP"}},
		{"a blank name", ledger.Account{
			Code: savings, Name: "   ", Kind: "asset", Currency: "GBP"}},
		{"a currency that is not three capitals", ledger.Account{
			Code: savings, Name: "Savings", Kind: "asset", Currency: "pounds"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx := testdb.Tx(t)

			err := ledger.Open(ctx, tx, c.account)
			if !errors.Is(err, ledger.ErrRejected) {
				t.Fatalf("open = %v, want %v", err, ledger.ErrRejected)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("open = %v, and the server's own error is not in it", err)
			}
			if pgErr.Code != "23514" {
				t.Errorf("SQLSTATE %s, want 23514 — a check constraint", pgErr.Code)
			}
		})
	}
}

func TestARefusedOpenLeavesTheTransactionUsable(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	if err := ledger.Open(ctx, tx, ledger.Account{
		Code: savings, Name: "Savings", Kind: "liabilty", Currency: "GBP",
	}); !errors.Is(err, ledger.ErrUnknownKind) {
		t.Fatalf("open = %v, want %v", err, ledger.ErrUnknownKind)
	}

	if got := balance(t, tx, cash); got != 0 {
		t.Errorf("%s = %d after the refusal, want 0", cash, got)
	}
	if err := ledger.Open(ctx, tx, ledger.Account{
		Code: savings, Name: "Savings", Kind: "asset", Currency: "GBP",
	}); err != nil {
		t.Fatalf("the open after the refusal: %v", err)
	}
}

func TestAChartIsOpenedAsOneUnitOfWork(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)

	for _, a := range []ledger.Account{
		{Code: savings, Name: "Savings at bank", Kind: "asset", Currency: "GBP"},
		{Code: "equity.opening", Name: "Opening balances", Kind: "equity", Currency: "GBP"},
	} {
		if err := ledger.Open(ctx, tx, a); err != nil {
			t.Fatalf("open %s: %v", a.Code, err)
		}
	}

	rows, err := ledger.Balances(ctx, tx, ledger.AccountFilter{Currency: "GBP"})
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d GBP accounts, want the 2 just opened", len(rows))
	}
	if rows[0].Account != savings || rows[1].Account != "equity.opening" {
		t.Errorf("chart = %s, %s; want %s then equity.opening", rows[0].Account, rows[1].Account, savings)
	}
}

func accountCount(t *testing.T, tx *sql.Tx, code string) int64 {
	t.Helper()

	return scalar(t, tx, `SELECT count(*) FROM accounts WHERE code = $1`, code)
}
