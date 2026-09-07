package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestDemoBalanceReads(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedDemoChart(t, tx)

	fmt.Println("\nBEFORE  ledger.Balances / ledger.TrialBalance")
	report(t, ctx, tx)

	fmt.Println("\nPOST  two entries, one per currency")
	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: 4500},
		ledger.Leg{Account: customer, AmountMinor: -4350},
		ledger.Leg{Account: fees, AmountMinor: -150})
	fmt.Println("  GBP  deposit of 45.00, less a 1.50 fee")
	post(t, tx, "USD",
		ledger.Leg{Account: cashUSD, AmountMinor: 3000},
		ledger.Leg{Account: customerUSD, AmountMinor: -3000})
	fmt.Println("  USD  deposit of 30.00")

	fmt.Println("\nAFTER   nothing committed — the read runs on the caller's tx")
	report(t, ctx, tx)

	fmt.Println("\nREAD  a code no account holds")
	_, err := ledger.BalanceOf(ctx, tx, "assets.csah")
	fmt.Printf("  err:      %v\n", err)
	fmt.Printf("  errors.Is(err, ledger.ErrUnknownAccount) = %t\n", errors.Is(err, ledger.ErrUnknownAccount))

	if !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("unknown code returned %v, want %v", err, ledger.ErrUnknownAccount)
	}
}

func report(t *testing.T, ctx context.Context, tx *sql.Tx) {
	t.Helper()

	balances, err := ledger.Balances(ctx, tx, ledger.AccountFilter{})
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	fmt.Printf("  %-26s %-10s %10s %9s\n", "account", "kind", "balance", "postings")
	for _, b := range balances {
		fmt.Printf("  %-26s %-10s %10s %9d\n", b.Account, b.Kind, money(b.AmountMinor, b.Currency), b.Postings)
	}

	trial, err := ledger.TrialBalance(ctx, tx)
	if err != nil {
		t.Fatalf("trial balance: %v", err)
	}
	for _, row := range trial {
		verdict := "balanced"
		if !row.Balanced() {
			verdict = fmt.Sprintf("OUT BY %s", row.NetMinor)
		}
		fmt.Printf("  %-3s  debits %s  credits %s  %s\n",
			row.Currency, minor(row.DebitsMinor), minor(row.CreditsMinor), verdict)

		if !row.Balanced() || !row.DebitsMinor.Equal(row.CreditsMinor) {
			t.Errorf("%s does not balance: %+v", row.Currency, row)
		}
	}
}

// minor prints an amount in major units, by moving the point two places along
// the digits rather than dividing: a Minor is wider than the arithmetic.
func minor(m ledger.Minor) string {
	digits, sign := m.String(), ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	for len(digits) < 3 {
		digits = "0" + digits
	}
	return sign + digits[:len(digits)-2] + "." + digits[len(digits)-2:]
}

func seedDemoChart(t *testing.T, tx *sql.Tx) {
	t.Helper()

	if _, err := tx.Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES
		   ($1, 'Cash at bank',           'asset',     'GBP'),
		   ($2, 'Customer balances',      'liability', 'GBP'),
		   ($3, 'Fee income',             'revenue',   'GBP'),
		   ($4, 'Cash at bank (USD)',     'asset',     'USD'),
		   ($5, 'Customer balances (USD)','liability', 'USD')`,
		cash, customer, fees, cashUSD, customerUSD,
	); err != nil {
		t.Fatalf("seed the chart: %v", err)
	}
}
