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
	var debits, credits int64
	for rows.Next() {
		var code, kind, currency string
		var minor, postings int64
		if err := rows.Scan(&code, &kind, &currency, &minor, &postings); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if minor > 0 {
			debits += minor
		} else {
			credits += -minor
		}
		fmt.Printf("  %-22s %-10s %10s %9d\n", code, kind, money(minor, currency), postings)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	fmt.Printf("  %-22s %10d debits, %d credits\n", "MUST BE EQUAL", debits, credits)

	if debits != credits {
		t.Errorf("debits %d, credits %d — the ledger does not balance", debits, credits)
	}
}

func money(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, minor/100, minor%100, currency)
}
