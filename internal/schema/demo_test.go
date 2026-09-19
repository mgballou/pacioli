package schema_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestDemoBalanceTable(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	banner("BEFORE")
	printBalances(t, tx)

	banner("POST  a balanced transaction — customer deposits 45.00")
	fmt.Println("  assets.cash                    +4500")
	fmt.Println("  liabilities.customer           -4500")
	postTransaction(t, tx, "Customer deposit", leg{cash, 4500}, leg{customer, -4500})
	mustSettle(t, tx)
	fmt.Println("  SET CONSTRAINTS ALL IMMEDIATE  accepted")
	deferChecks(t, tx)

	banner("AFTER")
	printBalances(t, tx)

	banner("REJECT  the same deposit, 50p short")
	fmt.Println("  assets.cash                    +4500")
	fmt.Println("  liabilities.customer           -4450")
	mustExec(t, tx, `SAVEPOINT unbalanced`)
	postTransaction(t, tx, "Fifty pence short", leg{cash, 4500}, leg{customer, -4450})
	_, err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
	printServerError(t, err)
	mustExec(t, tx, `ROLLBACK TO SAVEPOINT unbalanced`)

	banner("AFTER THE REJECTION")
	printBalances(t, tx)

	assertCode(t, err, codeUnbalanced)
	if got := balance(t, tx, cash); got != 4500 {
		t.Errorf("cash balance = %d, want 4500 — the rejected transaction left a mark", got)
	}
}

func banner(title string) {
	fmt.Printf("\n%s\n", title)
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
	var total int64
	for rows.Next() {
		var code, kind, currency string
		var minor, postings int64
		if err := rows.Scan(&code, &kind, &currency, &minor, &postings); err != nil {
			t.Fatalf("scan: %v", err)
		}
		total += minor
		fmt.Printf("  %-22s %-10s %10s %9d\n", code, kind, money(minor, currency), postings)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	fmt.Printf("  %-22s %-10s %10d\n", "TOTAL (must be 0)", "", total)

	if total != 0 {
		t.Errorf("the ledger as a whole nets to %d, want 0", total)
	}
}

func printServerError(t *testing.T, err error) {
	t.Helper()

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a Postgres error, got %v", err)
	}
	fmt.Printf("  %s:  %s: %s\n", pgErr.Severity, pgErr.Code, pgErr.Message)
	fmt.Printf("  HINT:   %s\n", pgErr.Hint)
}

func money(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, minor/100, minor%100, currency)
}

func mustExec(t *testing.T, tx *sql.Tx, query string) {
	t.Helper()

	if _, err := tx.Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func deferChecks(t *testing.T, tx *sql.Tx) {
	t.Helper()

	mustExec(t, tx, `SET CONSTRAINTS ALL DEFERRED`)
}
