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
	for _, tc := range []struct {
		name string
		id   string
		code string
	}{
		{name: "malformed UUID", id: "not-a-uuid", code: "22P02"},
		{name: "missing transaction", id: "99999999-9999-4999-8999-999999999999", code: "23503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tx := testdb.Tx(t)
			seedAccounts(t, tx)
			key := claim(t, "the deposit")
			_, err := ledger.Once(ctx, tx, key, func() (string, error) {
				if _, err := ledger.Post(ctx, tx, deposit); err != nil {
					t.Fatalf("write before returning the bad result: %v", err)
				}
				return tc.id, nil
			})
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tc.code {
				t.Fatalf("bad callback result gave %v, want SQLSTATE %s", err, tc.code)
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
	for _, tc := range []struct {
		name string
		hash []byte
	}{
		{name: "nil", hash: nil},
		{name: "empty", hash: []byte{}},
		{name: "short", hash: make([]byte, 31)},
		{name: "long", hash: make([]byte, 33)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tx := testdb.Tx(t)
			seedAccounts(t, tx)
			key := claim(t, "the deposit")
			key.RequestHash = tc.hash
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
	for _, tc := range []struct {
		name     string
		id       string
		finished bool
		want     error
	}{
		{name: "missing", id: "99999999-9999-4999-8999-999999999999", want: ledger.ErrUnknownTransaction},
		{name: "malformed", id: "not-a-uuid", want: ledger.ErrBadTransactionID},
		{name: "finished", id: "99999999-9999-4999-8999-999999999999", finished: true, want: sql.ErrTxDone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := testdb.Tx(t)
			if tc.finished {
				if err := tx.Rollback(); err != nil {
					t.Fatalf("finish the transaction: %v", err)
				}
			}
			id, entry, err := ledger.EntryOf(context.Background(), tx, tc.id)
			if !errors.Is(err, tc.want) {
				t.Fatalf("lookup gave %v, want %v", err, tc.want)
			}
			if id != "" || entry.Currency != "" || entry.Description != "" || !entry.OccurredAt.IsZero() || len(entry.Legs) != 0 {
				t.Errorf("refused lookup returned %q, %+v, want no entry", id, entry)
			}
			if tc.want == ledger.ErrBadTransactionID {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
					t.Errorf("malformed ID gave %v, want wrapped SQLSTATE 22P02", err)
				}
			}
		})
	}
}

func TestMinorReadsWholeJSONAmounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want string
		bad  bool
	}{
		{name: "zero", json: "0", want: "0"},
		{name: "negative", json: "-42", want: "-42"},
		{name: "past int64", json: "9223372036854775808", want: "9223372036854775808"},
		{name: "fraction", json: "1.5", bad: true},
		{name: "text", json: `"12"`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ledger.Minor
			err := got.UnmarshalJSON([]byte(tc.json))
			if tc.bad {
				if err == nil {
					t.Fatal("UnmarshalJSON accepted a value that is not a whole number")
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalJSON: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("amount = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMinorScansDatabaseValuesWithoutFloats(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  any
		want string
		bad  bool
	}{
		{name: "null", src: nil, want: "0"},
		{name: "integer", src: int64(-17), want: "-17"},
		{name: "minimum int64", src: int64(math.MinInt64), want: "-9223372036854775808"},
		{name: "numeric string", src: "123456789012345678901", want: "123456789012345678901"},
		{name: "numeric bytes", src: []byte("-90000000000000000000"), want: "-90000000000000000000"},
		{name: "bytes beyond int64", src: []byte("-9223372036854775809"), want: "-9223372036854775809"},
		{name: "fractional string", src: "1.5", bad: true},
		{name: "float", src: float64(3), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got ledger.Minor
			err := got.Scan(tc.src)
			if tc.bad {
				if err == nil {
					t.Fatal("Scan accepted a value outside its database types")
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("amount = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestScanningMinorUnitsReplacesThePreviousAmountExactly(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  any
		want string
	}{
		{name: "SQL NULL", src: nil, want: "0"},
		{name: "minimum int64", src: int64(math.MinInt64), want: "-9223372036854775808"},
		{name: "bytes beyond int64", src: []byte("-9223372036854775809"), want: "-9223372036854775809"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			amount := ledger.MinorOf(99)
			if err := amount.Scan(tc.src); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if got := amount.String(); got != tc.want {
				t.Errorf("scanned amount = %s, want %s", got, tc.want)
			}
			encoded, err := amount.MarshalJSON()
			if err != nil {
				t.Fatalf("encode the scanned amount: %v", err)
			}
			if string(encoded) != tc.want {
				t.Errorf("encoded amount = %s, want %s", encoded, tc.want)
			}
		})
	}
}
