package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestDemoPostingAPI(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	fmt.Println("\nBEFORE")
	printBalances(t, tx)

	fmt.Println("\nPOST  ledger.Post — deposit of 45.00, less a 1.50 fee")
	id, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Customer deposit, less fee",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4350},
			{Account: fees, AmountMinor: -150},
		},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	fmt.Printf("  accepted, transaction %s\n", id)

	fmt.Println("\nAFTER")
	printBalances(t, tx)

	fmt.Println("\nREFUSE  the same deposit, 50p short")
	_, err = ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Fifty pence short",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4450},
		},
	})
	fmt.Printf("  err:      %v\n", err)
	fmt.Printf("  errors.Is(err, ledger.ErrUnbalanced) = %t\n", errors.Is(err, ledger.ErrUnbalanced))

	fmt.Println("\nAFTER THE REFUSAL")
	printBalances(t, tx)

	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("refusal returned %v, want %v", err, ledger.ErrUnbalanced)
	}
	if got := balance(t, tx, cash); got != 4500 {
		t.Errorf("%s = %d, want 4500 — the refused entry left a mark", cash, got)
	}
	if got := transactionCount(t, tx); got != 1 {
		t.Errorf("%d transactions, want 1 — the refused entry left a row", got)
	}
}

func printBalances(t *testing.T, tx *sql.Tx) {
	t.Helper()

	rows, err := tx.Query(
		`SELECT code, kind, currency, balance_minor, posting_count
		   FROM account_balances ORDER BY code`,
	)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	defer rows.Close()

	fmt.Printf("  %-22s %-10s %10s %9s\n", "account", "kind", "balance", "postings")
	var debits, credits ledger.Minor
	for rows.Next() {
		var code, kind, currency string
		var amount ledger.Minor
		var postings int64
		if err := rows.Scan(&code, &kind, &currency, &amount, &postings); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if amount.Sign() > 0 {
			debits = debits.Add(amount)
		} else {
			credits = credits.Add(amount.Neg())
		}
		fmt.Printf("  %-22s %-10s %10s %9d\n", code, kind, money(amount, currency), postings)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	fmt.Printf("  %-22s %10s debits, %s credits\n", "MUST BE EQUAL", debits, credits)

	if !debits.Equal(credits) {
		t.Errorf("debits %s, credits %s — the ledger does not balance", debits, credits)
	}
}

func money(m ledger.Minor, currency string) string {
	return minor(m) + " " + currency
}
