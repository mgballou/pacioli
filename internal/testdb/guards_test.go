package testdb_test

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/testdb"
)

// A refusal is a t.Fatalf on the caller's own test, so it is watched from a child process.
const (
	childEnv     = "TESTDB_CHILD"
	childNameEnv = "TESTDB_CHILD_NAME"
)

func TestAsAChildProcess(t *testing.T) {
	switch os.Getenv(childEnv) {
	case "":
		t.Skip("runs only as the child of another test in this file")
	case "open":
		testdb.Open(t)
	case "disposable":
		testdb.Disposable(t, os.Getenv(childNameEnv))
	default:
		t.Fatalf("unknown child %q", os.Getenv(childEnv))
	}
}

func TestDSNReadsTheOverrideAndOtherwiseNamesTheComposeDatabase(t *testing.T) {
	const elsewhere = "postgres://ledger:ledger@db.internal:5432/ledger_ci_test?sslmode=disable"

	for _, c := range []struct {
		name  string
		unset bool
		env   string
		want  string
	}{
		{"no override set", true, "", testdb.DefaultDSN},
		{"an override set to nothing", false, "", testdb.DefaultDSN},
		{"an override set", false, elsewhere, elsewhere},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(testdb.DSNEnv, c.env)
			if c.unset {
				os.Unsetenv(testdb.DSNEnv)
			}
			if got := testdb.DSN(); got != c.want {
				t.Errorf("DSN() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestOpenRefusesADatabaseItCannotUseAndSaysWhatToDo(t *testing.T) {
	// Made here and empty, so the schema check below reads this run alone.
	const refused = "testdb_refused"
	admin := testdb.Open(t)
	mustExec(t, admin, `DROP DATABASE IF EXISTS `+refused+` WITH (FORCE)`)
	mustExec(t, admin, `CREATE DATABASE `+refused)
	t.Cleanup(func() { mustExec(t, admin, `DROP DATABASE IF EXISTS `+refused+` WITH (FORCE)`) })
	notTest := databaseOf(t, testdb.DSN(), refused)

	for _, c := range []struct {
		name string
		dsn  string
		want []string
	}{
		{"a database whose name lacks _test", notTest,
			[]string{"refusing to run tests against database " + refused, "Check " + testdb.DSNEnv}},
		{"a server that is not there", "postgres://ledger:ledger@127.0.0.1:1/ledger_test?sslmode=disable&connect_timeout=2",
			[]string{"test database unreachable at", "make db-up"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, ok := runChild(t, childEnv+"=open", testdb.DSNEnv+"="+c.dsn)
			if ok {
				t.Fatalf("Open went ahead, want a refusal\n%s", out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("the refusal does not say %q\n%s", w, out)
				}
			}
		})
	}

	// The refusal comes before the schema, so the database that does not end in _test is left as it was.
	db, err := sql.Open("pgx", notTest)
	if err != nil {
		t.Fatalf("open %s: %v", notTest, err)
	}
	defer db.Close()
	var kind sql.NullString
	if err := db.QueryRow(`SELECT to_regtype('account_kind')::text`).Scan(&kind); err != nil {
		t.Fatalf("look for the schema: %v", err)
	}
	if kind.Valid {
		t.Errorf("account_kind exists in %s — Open applied the schema to a database it refused", refused)
	}
}

func TestDisposableRefusesANameItWouldHaveToPasteUnsafelyIntoDDL(t *testing.T) {
	for _, c := range []struct {
		name   string
		dbName string
	}{
		{"an empty name", ""},
		{"a name without the _test suffix", "ledger"},
		{"a capital letter", "Ledger_test"},
		{"a hyphen", "ledger-probe_test"},
		{"a leading digit", "9ledger_test"},
		{"a leading underscore", "_test"},
		{"a statement smuggled in", "x_test; drop database ledger_test"},
		{"sixty-two characters, one past the limit", strings.Repeat("a", 57) + "_test"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, ok := runChild(t, childEnv+"=disposable", childNameEnv+"="+c.dbName)
			if ok {
				t.Fatalf("Disposable(%q) went ahead, want a refusal\n%s", c.dbName, out)
			}
			if !strings.Contains(out, "is not a name this will create") {
				t.Errorf("the refusal does not explain itself\n%s", out)
			}
			if databaseExists(t, c.dbName) {
				t.Errorf("database %q exists — the refusal came after the CREATE", c.dbName)
			}
		})
	}
}

func TestDisposableHandsOverAFreshDatabaseAndDropsItAfterwards(t *testing.T) {
	for _, c := range []struct {
		name       string
		dbName     string
		leftBehind bool
	}{
		{"a plain name", "testdb_probe_test", false},
		{"sixty-one characters, the longest allowed", strings.Repeat("b", 56) + "_test", false},
		{"a name a killed run left behind", "testdb_stale_test", true},
	} {
		if c.leftBehind {
			admin := testdb.Open(t)
			mustExec(t, admin, `DROP DATABASE IF EXISTS `+c.dbName+` WITH (FORCE)`)
			mustExec(t, admin, `CREATE DATABASE `+c.dbName)
			stale, err := sql.Open("pgx", databaseOf(t, testdb.DSN(), c.dbName))
			if err != nil {
				t.Fatalf("open %s: %v", c.dbName, err)
			}
			mustExec(t, stale, `CREATE TABLE left_behind (note text)`)
			stale.Close()
		}

		t.Run(c.name, func(t *testing.T) {
			db := testdb.Disposable(t, c.dbName)

			var current string
			var kind, leftover sql.NullString
			err := db.QueryRow(`SELECT current_database(), to_regtype('account_kind')::text, to_regclass('left_behind')::text`).
				Scan(&current, &kind, &leftover)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if current != c.dbName {
				t.Errorf("connected to %q, want %q", current, c.dbName)
			}
			if !kind.Valid {
				t.Errorf("account_kind is missing — the schema was not applied")
			}
			if leftover.Valid {
				t.Errorf("left_behind survived — the database is not fresh")
			}

			// What Disposable is for: a commit that lands.
			tx, err := db.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := tx.Exec(`CREATE TABLE committed_probe (note text)`); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			var probe sql.NullString
			if err := db.QueryRow(`SELECT to_regclass('committed_probe')::text`).Scan(&probe); err != nil {
				t.Fatalf("query: %v", err)
			}
			if !probe.Valid {
				t.Errorf("the committed table is not there")
			}
		})

		if databaseExists(t, c.dbName) {
			t.Errorf("database %q is still there after its test ended", c.dbName)
		}
	}
}

func TestTxCleansUpHoweverTheTestLeftTheTransaction(t *testing.T) {
	for _, c := range []struct {
		name string
		end  func(*sql.Tx) error
	}{
		{"left open", func(*sql.Tx) error { return nil }},
		{"rolled back by the test", (*sql.Tx).Rollback},
		{"committed by the test", (*sql.Tx).Commit},
	} {
		// A cleanup that complained would fail the subtest, and with it this test.
		t.Run(c.name, func(t *testing.T) {
			tx := testdb.Tx(t)
			var one int
			if err := tx.QueryRow(`SELECT 1`).Scan(&one); err != nil {
				t.Fatalf("query: %v", err)
			}
			if err := c.end(tx); err != nil {
				t.Fatalf("end the transaction: %v", err)
			}
		})
	}
}

// runChild runs TestAsAChildProcess in a fresh copy of this test binary and reports its output and whether it passed.
func runChild(t *testing.T, env ...string) (string, bool) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^TestAsAChildProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("run the child: %v", err)
	}
	return string(out), err == nil
}

func databaseOf(t *testing.T, dsn, name string) string {
	t.Helper()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", dsn, err)
	}
	u.Path = "/" + name
	return u.String()
}

func databaseExists(t *testing.T, name string) bool {
	t.Helper()

	var n int
	if err := testdb.Open(t).QueryRow(`SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("look for database %q: %v", name, err)
	}
	return n > 0
}
