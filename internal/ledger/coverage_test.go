package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestAnInvalidCallbackResultUndoesTheWriteAndReleasesItsKey(t *testing.T) {
	for _, c := range []struct {
		name string
		id   string
		code string
	}{
		{"malformed UUID", "not-a-uuid", "22P02"},
		{"missing transaction", "99999999-9999-4999-8999-999999999999", "23503"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			tx := testdb.Tx(t)
			seedAccounts(t, tx)
			key := claim(t, "the deposit")
			_, err := ledger.Once(ctx, tx, key, func() (string, error) {
				if _, err := ledger.Post(ctx, tx, deposit); err != nil {
					t.Fatalf("write before returning the bad result: %v", err)
				}
				return c.id, nil
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != c.code {
				t.Fatalf("bad callback result gave %v, want SQLSTATE %s", err, c.code)
			}
			if got := transactionCount(t, tx); got != 0 {
				t.Errorf("%d transactions after the refusal, want 0", got)
			}
			if got := balance(t, tx, cash); got != 0 {
				t.Errorf("cash balance after the refusal = %d, want 0", got)
			}

			rec, err := ledger.Once(ctx, tx, key, writing(ctx, tx, deposit))
			if err != nil {
				t.Fatalf("retry under the same key: %v", err)
			}
			if rec.Replayed || rec.Transaction == "" || rec.Key != key.Key {
				t.Errorf("retry = %+v, want a new transaction under %q", rec, key.Key)
			}
			if got := transactionCount(t, tx); got != 1 {
				t.Errorf("%d transactions after retry, want 1", got)
			}
			if got := balance(t, tx, cash); got != 4500 {
				t.Errorf("cash balance after retry = %d, want 4500", got)
			}
		})
	}
}

func TestABadFingerprintDoesNotRunTheWriteOrBurnItsKey(t *testing.T) {
	for _, c := range []struct {
		name string
		hash []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"short", make([]byte, 31)},
		{"long", make([]byte, 33)},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			tx := testdb.Tx(t)
			seedAccounts(t, tx)
			key := claim(t, "the deposit")
			key.RequestHash = c.hash
			called := false
			_, err := ledger.Once(ctx, tx, key, func() (string, error) {
				called = true
				return ledger.Post(ctx, tx, deposit)
			})
			if !errors.Is(err, ledger.ErrBadKey) {
				t.Fatalf("bad fingerprint gave %v, want ErrBadKey", err)
			}
			if called {
				t.Error("the write ran despite a refused fingerprint")
			}
			if got := transactionCount(t, tx); got != 0 {
				t.Errorf("%d transactions after the refusal, want 0", got)
			}

			key.RequestHash = hashOf("the deposit")
			rec, err := ledger.Once(ctx, tx, key, writing(ctx, tx, deposit))
			if err != nil {
				t.Fatalf("retry with a valid fingerprint: %v", err)
			}
			if rec.Replayed || rec.Transaction == "" || rec.Key != key.Key {
				t.Errorf("retry = %+v, want a new transaction under %q", rec, key.Key)
			}
			if got := balance(t, tx, cash); got != 4500 {
				t.Errorf("cash balance after retry = %d, want 4500", got)
			}
		})
	}
}

func TestAnEntryLookupDistinguishesMissingMalformedAndFinishedTransactions(t *testing.T) {
	for _, c := range []struct {
		name     string
		id       string
		finished bool
		want     error
	}{
		{"missing", "99999999-9999-4999-8999-999999999999", false, ledger.ErrUnknownTransaction},
		{"malformed", "not-a-uuid", false, ledger.ErrBadTransactionID},
		{"finished", "99999999-9999-4999-8999-999999999999", true, sql.ErrTxDone},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx := testdb.Tx(t)
			if c.finished {
				if err := tx.Rollback(); err != nil {
					t.Fatalf("finish the transaction: %v", err)
				}
			}
			id, entry, err := ledger.EntryOf(context.Background(), tx, c.id)
			if !errors.Is(err, c.want) {
				t.Fatalf("lookup gave %v, want %v", err, c.want)
			}
			if id != "" || entry.Currency != "" || entry.Description != "" || !entry.OccurredAt.IsZero() || len(entry.Legs) != 0 {
				t.Errorf("refused lookup returned %q, %+v, want no entry", id, entry)
			}
			if c.want == ledger.ErrBadTransactionID {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
					t.Errorf("malformed ID gave %v, want the wrapped SQLSTATE 22P02", err)
				}
			}
			if c.finished && (errors.Is(err, ledger.ErrUnknownTransaction) || errors.Is(err, ledger.ErrBadTransactionID)) {
				t.Errorf("finished transaction was reported as a bad or missing ID: %v", err)
			}
		})
	}
}

func TestScanningMinorUnitsReplacesThePreviousAmountExactly(t *testing.T) {
	for _, c := range []struct {
		name string
		src  any
		want string
	}{
		{"SQL NULL", nil, "0"},
		{"minimum int64", int64(math.MinInt64), "-9223372036854775808"},
		{"bytes beyond int64", []byte("-9223372036854775809"), "-9223372036854775809"},
	} {
		t.Run(c.name, func(t *testing.T) {
			amount := ledger.MinorOf(99)
			if err := amount.Scan(c.src); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if got := amount.String(); got != c.want {
				t.Errorf("scanned amount = %s, want %s", got, c.want)
			}
			encoded, err := amount.MarshalJSON()
			if err != nil {
				t.Fatalf("encode the scanned amount: %v", err)
			}
			if string(encoded) != c.want {
				t.Errorf("encoded amount = %s, want %s", encoded, c.want)
			}
		})
	}
}
