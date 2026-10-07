package testdb_test

import (
	"database/sql"
	"testing"

	"github.com/mgballou/pacioli/internal/testdb"
)

func TestDSNUsesTheOverrideOrContainerDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
		want string
	}{
		{name: "default", want: testdb.DefaultDSN},
		{name: "override", dsn: "postgres://tester@localhost/ledger_test", want: "postgres://tester@localhost/ledger_test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(testdb.DSNEnv, tc.dsn)
			if got := testdb.DSN(); got != tc.want {
				t.Errorf("DSN() = %q, want %q", got, tc.want)
			}
		})
	}
}

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

func TestDisposableCreatesAnEmptyDatabaseWithTheSchema(t *testing.T) {
	for _, tc := range []struct {
		name string
		db   string
	}{
		{name: "first", db: "testdb_first_disposable_test"},
		{name: "second", db: "testdb_second_disposable_test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testdb.Disposable(t, tc.db)
			var present bool
			if err := db.QueryRow(`SELECT to_regclass('public.accounts') IS NOT NULL`).Scan(&present); err != nil {
				t.Fatalf("check schema: %v", err)
			}
			if !present {
				t.Fatal("disposable database has no accounts table")
			}
		})
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
