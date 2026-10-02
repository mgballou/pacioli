package testdb_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/mgballou/pacioli/internal/testdb"
)

func TestDisposableAllowsCommittedWritesAndDropsTheDatabase(t *testing.T) {
	admin := testdb.Open(t)
	name := fmt.Sprintf("disposable_coverage_%d_test", os.Getpid())

	t.Run("commit in an isolated database", func(t *testing.T) {
		db := testdb.Disposable(t, name)
		var connected string
		if err := db.QueryRow(`SELECT current_database()`).Scan(&connected); err != nil {
			t.Fatalf("current database: %v", err)
		}
		if connected != name {
			t.Fatalf("connected to %q, want %q", connected, name)
		}

		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
		if _, err := tx.Exec(`INSERT INTO accounts (code, name, kind, currency)
			VALUES ('assets.disposable', 'Disposable cash', 'asset', 'GBP')`); err != nil {
			t.Fatalf("insert using the installed schema: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var got string
		if err := db.QueryRow(`SELECT name FROM accounts WHERE code = 'assets.disposable'`).Scan(&got); err != nil {
			t.Fatalf("read committed account: %v", err)
		}
		if got != "Disposable cash" {
			t.Errorf("committed name = %q, want Disposable cash", got)
		}
	})

	var exists bool
	if err := admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("check database cleanup: %v", err)
	}
	if exists {
		t.Errorf("disposable database %q still exists after the test ended", name)
	}
}
