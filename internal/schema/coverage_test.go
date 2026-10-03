package schema_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/mgballou/pacioli/internal/schema"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestApplyReturnsCanceledContext(t *testing.T) {
	db := testdb.Open(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	applied, err := schema.Apply(ctx, db)
	if applied {
		t.Error("canceled Apply reported that it installed the schema")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context.Canceled", err)
	}
}

func TestApplyRollsBackFailedInstallAndCanBeRetried(t *testing.T) {
	db := testdb.Disposable(t, fmt.Sprintf("schema_retry_%d_test", os.Getpid()))
	// Only this disposable database is cleared. A pre-existing accounts table
	// causes installation to fail after earlier schema statements have run.
	if _, err := db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;
		CREATE TABLE accounts (note text);
		INSERT INTO accounts VALUES ('keep this row')`); err != nil {
		t.Fatalf("prepare conflicting schema: %v", err)
	}

	applied, err := schema.Apply(context.Background(), db)
	if applied {
		t.Error("failed installation reported applied = true")
	}
	assertCode(t, err, "42P07") // duplicate_table

	var note string
	if err := db.QueryRow(`SELECT note FROM accounts`).Scan(&note); err != nil {
		t.Fatalf("read pre-existing data: %v", err)
	}
	if note != "keep this row" {
		t.Errorf("pre-existing row = %q, want keep this row", note)
	}
	var partialSchema bool
	if err := db.QueryRow(`SELECT to_regprocedure('public.is_blank(text)') IS NOT NULL
		OR to_regtype('public.account_kind') IS NOT NULL`).Scan(&partialSchema); err != nil {
		t.Fatalf("check partial schema: %v", err)
	}
	if partialSchema {
		t.Fatal("failed installation left functions or types behind")
	}

	if _, err := db.Exec(`DROP TABLE accounts`); err != nil {
		t.Fatalf("remove conflict: %v", err)
	}
	applied, err = schema.Apply(context.Background(), db)
	if err != nil || !applied {
		t.Fatalf("retry Apply = (%t, %v), want (true, nil)", applied, err)
	}
	if _, err := db.Exec(`INSERT INTO accounts (code, name, kind, currency)
		VALUES ('assets.retry', 'Cash after retry', 'asset', 'GBP')`); err != nil {
		t.Fatalf("write to installed schema: %v", err)
	}

	applied, err = schema.Apply(context.Background(), db)
	if err != nil || applied {
		t.Fatalf("repeated Apply = (%t, %v), want (false, nil)", applied, err)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM accounts WHERE code = 'assets.retry'`).Scan(&name); err != nil {
		t.Fatalf("read after repeated Apply: %v", err)
	}
	if name != "Cash after retry" {
		t.Errorf("name after repeated Apply = %q, want Cash after retry", name)
	}
}

func TestConcurrentApplyInstallsSchemaOnce(t *testing.T) {
	db := testdb.Disposable(t, fmt.Sprintf("schema_concurrent_%d_test", os.Getpid()))
	if _, err := db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("clear disposable schema: %v", err)
	}

	const callers = 8
	type result struct {
		applied bool
		err     error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			<-start
			applied, err := schema.Apply(context.Background(), db)
			results <- result{applied: applied, err: err}
		}()
	}
	close(start)

	installed := 0
	for range callers {
		r := <-results
		if r.err != nil {
			t.Errorf("concurrent Apply: %v", r.err)
		}
		if r.applied {
			installed++
		}
	}
	if installed != 1 {
		t.Errorf("%d calls reported an installation, want 1", installed)
	}
	if _, err := db.Exec(`INSERT INTO accounts (code, name, kind, currency)
		VALUES ('assets.concurrent', 'Concurrent cash', 'asset', 'GBP')`); err != nil {
		t.Fatalf("write after concurrent installation: %v", err)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM accounts WHERE code = 'assets.concurrent'`).Scan(&name); err != nil {
		t.Fatalf("read after concurrent installation: %v", err)
	}
	if name != "Concurrent cash" {
		t.Errorf("installed account name = %q, want Concurrent cash", name)
	}
}
