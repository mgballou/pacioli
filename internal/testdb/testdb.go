// Package testdb hands tests a connection to the container-backed Postgres, and
// a transaction that is always rolled back.
//
// Never a mock, and never a leak: a test sees its own writes and no other test's,
// so the order tests run in cannot matter.
package testdb

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mgballou/pacioli/internal/schema"

	// Registers "pgx" as a database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// DSNEnv names the environment variable that overrides the compose defaults.
const DSNEnv = "LEDGER_TEST_DSN"

// DefaultDSN matches compose.test.yaml, on port 55432 so it cannot reach a
// Postgres already running on this machine. It is exported so a test can hold
// cmd/pacioli's own copy of the string against it.
const DefaultDSN = "postgres://ledger:ledger@127.0.0.1:55432/ledger_test?sslmode=disable"

// testDBSuffix guards against pointing the suite at a database that matters.
const testDBSuffix = "_test"

var (
	once    sync.Once
	shared  *sql.DB
	openErr error
)

// DSN reports the connection string tests will use.
func DSN() string {
	if dsn := os.Getenv(DSNEnv); dsn != "" {
		return dsn
	}
	return DefaultDSN
}

// Open returns the shared pool, connecting on first use and installing the
// schema if it is not already there. A missing database fails the test rather
// than skipping it.
func Open(t *testing.T) *sql.DB {
	t.Helper()

	once.Do(func() {
		db, err := sql.Open("pgx", DSN())
		if err != nil {
			openErr = err
			return
		}
		if err := db.Ping(); err != nil {
			db.Close()
			openErr = err
			return
		}
		if err := checkDisposable(db); err != nil {
			db.Close()
			openErr = err
			return
		}
		// The container starts empty every run, and Apply is a no-op once
		// another test package has been first.
		if _, err := schema.Apply(context.Background(), db); err != nil {
			db.Close()
			openErr = err
			return
		}
		shared = db
	})

	if openErr != nil {
		var wrong *wrongDatabaseError
		if errors.As(openErr, &wrong) {
			t.Fatalf("%v\n\nCheck %s.", openErr, DSNEnv)
		}
		t.Fatalf("test database unreachable at %s: %v\n\nStart it with: make db-up", DSN(), openErr)
	}
	return shared
}

// Tx begins a transaction and rolls it back when the test ends. Whatever the
// test writes is visible to the test and to nothing else, ever.
func Tx(t *testing.T) *sql.Tx {
	t.Helper()

	tx, err := Open(t).Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		// ErrTxDone here is the expected outcome, not a failure.
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			t.Errorf("rollback: %v", err)
		}
	})
	return tx
}

// checkDisposable refuses any database whose name does not end in _test.
func checkDisposable(db *sql.DB) error {
	var name string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		return err
	}
	if !strings.HasSuffix(name, testDBSuffix) {
		return &wrongDatabaseError{name: name}
	}
	return nil
}

type wrongDatabaseError struct{ name string }

func (e *wrongDatabaseError) Error() string {
	return "refusing to run tests against database " + e.name +
		": the name must end in " + testDBSuffix
}
