package testdb_test

import (
	"database/sql"
	"testing"

	"github.com/mgballou/pacioli/internal/testdb"
)

func TestConnectsToTheContainerDatabase(t *testing.T) {
	db := testdb.Open(t)

	var name, version string
	if err := db.QueryRow(`SELECT current_database(), current_setting('server_version')`).Scan(&name, &version); err != nil {
		t.Fatalf("query: %v", err)
	}
	if name != "ledger_test" {
		t.Errorf("connected to database %q, want %q", name, "ledger_test")
	}
	t.Logf("connected to %s, Postgres %s, via %s", name, version, testdb.DSN())
}

func TestTxRollsBackWhenTheTestEnds(t *testing.T) {
	db := testdb.Open(t)

	// A real table, not TEMP: TEMP tables are per-connection and would vanish for reasons unrelated to the rollback.
	mustExec(t, db, `CREATE TABLE rollback_probe (note text NOT NULL)`)
	t.Cleanup(func() { mustExec(t, db, `DROP TABLE rollback_probe`) })

	t.Run("the write is visible inside the transaction", func(t *testing.T) {
		tx := testdb.Tx(t)

		if _, err := tx.Exec(`INSERT INTO rollback_probe (note) VALUES ($1)`, "rollback probe"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if got := countProbeRows(t, tx); got != 1 {
			t.Errorf("inside the transaction: %d rows, want 1", got)
		}
	})

	if got := countProbeRows(t, db); got != 0 {
		t.Errorf("after the transaction: %d rows, want 0 — the rollback did not happen", got)
	}
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func countProbeRows(t *testing.T, q querier) int {
	t.Helper()

	var n int
	if err := q.QueryRow(`SELECT count(*) FROM rollback_probe`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()

	if _, err := db.Exec(query); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
