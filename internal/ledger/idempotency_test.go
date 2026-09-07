package ledger_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

var deposit = ledger.Entry{
	Currency:    "GBP",
	Description: "Customer deposit",
	Legs: []ledger.Leg{
		{Account: cash, AmountMinor: 4500},
		{Account: customer, AmountMinor: -4500},
	},
}

func TestTheSameKeyTwiceWritesOnceAndGivesBackTheFirstTransaction(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	first, err := ledger.Once(ctx, tx, claim(t, "the deposit"), writing(ctx, tx, deposit))
	if err != nil {
		t.Fatalf("the first post: %v", err)
	}
	if first.Replayed {
		t.Error("the first post came back as a replay")
	}

	calls := 0
	second, err := ledger.Once(ctx, tx, claim(t, "the deposit"), func() (string, error) {
		calls++
		return ledger.Post(ctx, tx, deposit)
	})
	if err != nil {
		t.Fatalf("the second post: %v", err)
	}
	if calls != 0 {
		t.Errorf("the write ran %d times on a key already used, want 0", calls)
	}
	if !second.Replayed {
		t.Error("the second post is not marked as a replay")
	}
	if second.Transaction != first.Transaction {
		t.Errorf("the replay names %s, want the first transaction %s", second.Transaction, first.Transaction)
	}

	if got := balance(t, tx, cash); got != 4500 {
		t.Errorf("%s = %d, want 4500 — the deposit was posted twice", cash, got)
	}
	if got := transactionCount(t, tx); got != 1 {
		t.Errorf("%d transactions, want 1", got)
	}
}

func TestTheSameKeyWithADifferentRequestIsRefused(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	if _, err := ledger.Once(ctx, tx, claim(t, "the deposit"), writing(ctx, tx, deposit)); err != nil {
		t.Fatalf("the first post: %v", err)
	}

	other := deposit
	other.Description = "A different deposit entirely"
	_, err := ledger.Once(ctx, tx, claim(t, "something else"), writing(ctx, tx, other))
	if !errors.Is(err, ledger.ErrKeyReused) {
		t.Fatalf("second post gave %v, want ErrKeyReused", err)
	}
	if got := transactionCount(t, tx); got != 1 {
		t.Errorf("%d transactions, want 1 — the second request was posted anyway", got)
	}
}

func TestARefusedEntryDoesNotBurnItsKey(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	unbalanced := deposit
	unbalanced.Legs = []ledger.Leg{{Account: cash, AmountMinor: 4500}, {Account: customer, AmountMinor: -4000}}

	_, err := ledger.Once(ctx, tx, claim(t, "the deposit"), writing(ctx, tx, unbalanced))
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("post gave %v, want ErrUnbalanced", err)
	}

	rec, err := ledger.Once(ctx, tx, claim(t, "the deposit"), writing(ctx, tx, deposit))
	if err != nil {
		t.Fatalf("post under the same key after a refusal: %v", err)
	}
	if rec.Replayed {
		t.Error("the corrected entry came back as a replay; the refusal burned the key")
	}
	if got := balance(t, tx, cash); got != 4500 {
		t.Errorf("%s = %d, want 4500", cash, got)
	}
}

func TestTwoKeysPostTwice(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	first, err := ledger.Once(ctx, tx, claim(t, "the deposit"), writing(ctx, tx, deposit))
	if err != nil {
		t.Fatalf("the first post: %v", err)
	}
	second, err := ledger.Once(ctx, tx, otherClaim(t, "the deposit"), writing(ctx, tx, deposit))
	if err != nil {
		t.Fatalf("the second post: %v", err)
	}

	if second.Replayed || second.Transaction == first.Transaction {
		t.Errorf("the second key gave back %+v, want a transaction of its own", second)
	}
	if got := balance(t, tx, cash); got != 9000 {
		t.Errorf("%s = %d, want 9000 — two deposits under two keys", cash, got)
	}
}

func TestAKeyTheLedgerWillNotHoldIsRefused(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	for _, key := range []string{"", "short", "sixteen chars but with spaces in"} {
		_, err := ledger.Once(ctx, tx,
			ledger.Claim{Key: key, RequestHash: hashOf("the deposit")},
			writing(ctx, tx, deposit))
		if !errors.Is(err, ledger.ErrBadKey) {
			t.Errorf("key %q gave %v, want ErrBadKey", key, err)
		}
	}
	if got := transactionCount(t, tx); got != 0 {
		t.Errorf("%d transactions, want 0", got)
	}
}

func TestADuplicateLeavesTheTransactionUsable(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	if _, err := ledger.Once(ctx, tx, claim(t, "the deposit"), writing(ctx, tx, deposit)); err != nil {
		t.Fatalf("the first post: %v", err)
	}
	if _, err := ledger.Once(ctx, tx, claim(t, "the deposit"), writing(ctx, tx, deposit)); err != nil {
		t.Fatalf("the replay: %v", err)
	}

	if got := balance(t, tx, cash); got != 4500 {
		t.Errorf("%s = %d, want 4500 — and the read after the duplicate worked at all", cash, got)
	}
}

func TestAnEntryReadsBackAsItWasWritten(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	three := ledger.Entry{
		Currency:    "GBP",
		Description: "Customer deposit, less fee",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4350},
			{Account: fees, AmountMinor: -150},
		},
	}
	rec, err := ledger.Once(ctx, tx, claim(t, "the deposit"), writing(ctx, tx, three))
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	held, got, err := ledger.EntryOf(ctx, tx, rec.Transaction)
	if err != nil {
		t.Fatalf("read it back: %v", err)
	}
	if held != rec.Transaction {
		t.Errorf("read back id %q, want %q", held, rec.Transaction)
	}
	if got.Currency != three.Currency || got.Description != three.Description {
		t.Errorf("read back %+v, want the entry that was posted", got)
	}
	if len(got.Legs) != len(three.Legs) {
		t.Fatalf("%d legs, want %d", len(got.Legs), len(three.Legs))
	}
	for i, leg := range got.Legs {
		if leg != three.Legs[i] {
			t.Errorf("leg %d = %+v, want %+v — the order the client sent is not the order it reads back",
				i, leg, three.Legs[i])
		}
	}
}

func TestASecondTransactionWaitsForTheKeyRatherThanTakingIt(t *testing.T) {
	ctx := context.Background()
	db := testdb.Open(t)
	key := "race-" + t.Name()

	first := begin(t, db)
	defer func() { _ = first.Rollback() }()
	reserveKey(t, first, key)

	second := begin(t, db)
	defer func() { _ = second.Rollback() }()

	blocked := make(chan error, 1)
	go func() {
		_, err := second.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, sha256('body'))`, key)
		blocked <- err
	}()

	select {
	case err := <-blocked:
		t.Fatalf("the second reservation answered %v while the first still held the key", err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := first.Rollback(); err != nil {
		t.Fatalf("roll the first back: %v", err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("the second reservation was refused a key nobody holds: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second reservation never woke up after the first rolled back")
	}
}

func writing(ctx context.Context, tx *sql.Tx, e ledger.Entry) func() (string, error) {
	return func() (string, error) { return ledger.Post(ctx, tx, e) }
}

// The key carries the test name so two packages writing at once cannot wait on each other's uncommitted reservation.
func claim(t *testing.T, body string) ledger.Claim {
	t.Helper()

	return ledger.Claim{Key: "ledger-" + t.Name(), RequestHash: hashOf(body)}
}

func otherClaim(t *testing.T, body string) ledger.Claim {
	t.Helper()

	c := claim(t, body)
	c.Key += "-again"
	return c
}

func hashOf(body string) []byte {
	sum := sha256.Sum256([]byte(body))
	return sum[:]
}

func begin(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

func reserveKey(t *testing.T, tx *sql.Tx, key string) {
	t.Helper()

	if _, err := tx.Exec(
		`INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, sha256('body'))`, key,
	); err != nil {
		t.Fatalf("reserve %s: %v", key, err)
	}
}
