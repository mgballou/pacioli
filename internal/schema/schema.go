// Package schema holds the ledger's data definition and applies it.
// 0001_ledger.sql is embedded verbatim and executed as written.
package schema

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

// SQL is the ledger schema, so the binary and the tests apply the same bytes.
//
//go:embed 0001_ledger.sql
var SQL string

// applyLockID is an arbitrary constant for pg_advisory_xact_lock. Test packages
// run in parallel against one database, so two can reach Apply at once.
const applyLockID = 0x1ed6e11ab

// Apply installs the schema if it is not already there and reports whether it
// did. It is safe to call concurrently.
func Apply(ctx context.Context, db *sql.DB) (applied bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, applyLockID); err != nil {
		return false, fmt.Errorf("lock: %w", err)
	}

	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('public.postings') IS NOT NULL`).Scan(&present); err != nil {
		return false, fmt.Errorf("probe for public.postings: %w", err)
	}
	if present {
		return false, tx.Commit()
	}

	if _, err := tx.ExecContext(ctx, SQL); err != nil {
		return false, fmt.Errorf("apply 0001_ledger.sql: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}
