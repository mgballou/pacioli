package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// The two ways a claim can be refused.
var (
	// ErrKeyReused means the key has been used before, for a different
	// request. Nothing is written.
	ErrKeyReused = errors.New("the idempotency key was used for a different request")

	// ErrBadKey means the ledger will not hold the key as it was given.
	// The shape is a CHECK in the schema, and a request_hash of the wrong
	// length lands here too.
	ErrBadKey = errors.New("the ledger will not hold that idempotency key")
)

// A Claim is a client's assertion that a write should happen at most once: the
// key it chose, and a fingerprint of the request it sent that key with. What
// the fingerprint covers is the calling surface's to decide.
type Claim struct {
	Key         string
	RequestHash []byte
}

// A Record is what the ledger holds under a key. Replayed false means this call
// created Transaction; true means an earlier call did and this one wrote nothing.
type Record struct {
	Key         string
	Transaction string
	Replayed    bool
}

// onceSavepoint wraps the reservation and the work, so a duplicate or a refusal
// can be undone without taking the caller's transaction with it.
const onceSavepoint = "ledger_once"

// settledConstraint names the deferred trigger that insists a committed key
// carries a result. Once settles this one and leaves the rest alone.
const settledConstraint = "idempotency_keys_must_be_settled"

// Once runs write under the client's claim, at most once ever. It takes the
// write as a function so reserving the key and doing the work cannot come apart.
//
// On a key already used, write is never called and the Record carries the first
// call's transaction. On a key used for a different request, nothing is written
// and the error is ErrKeyReused. The caller's transaction is left usable either
// way.
func Once(ctx context.Context, tx *sql.Tx, c Claim, write func() (string, error)) (Record, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+onceSavepoint); err != nil {
		return Record{}, fmt.Errorf("savepoint: %w", err)
	}

	// An earlier SET CONSTRAINTS could have made the trigger immediate, and the
	// reservation is by definition a row with no result yet.
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS `+settledConstraint+` DEFERRED`); err != nil {
		return Record{}, fmt.Errorf("defer the settled check: %w", err)
	}

	// The reservation. A duplicate blocks on the primary key here until the
	// transaction holding the key finishes, and finds out afterwards.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, request_hash) VALUES ($1, $2)`,
		c.Key, c.RequestHash,
	)
	if err != nil {
		if rbErr := rollbackToOnce(ctx, tx); rbErr != nil {
			return Record{}, errors.Join(err, rbErr)
		}
		// The primary key is the only unique index on the table, so 23505 alone
		// says which constraint refused it.
		if code(err) == "23505" {
			rec, err := replay(ctx, tx, c)
			if relErr := releaseOnce(ctx, tx); relErr != nil {
				return Record{}, errors.Join(err, relErr)
			}
			return rec, err
		}
		return Record{}, badKey(err)
	}

	id, err := write()
	if err != nil {
		if rbErr := rollbackToOnce(ctx, tx); rbErr != nil {
			return Record{}, errors.Join(err, rbErr)
		}
		// The key went back with it, so the client can correct the body and
		// retry under the same key.
		return Record{}, err
	}

	// Settling the row is what makes it an answer; the schema will not let an
	// unsettled key commit. The IS NULL narrows it to the reservation this
	// call made.
	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET transaction_id = $2::uuid
		  WHERE key = $1 AND transaction_id IS NULL`,
		c.Key, id,
	); err != nil {
		if rbErr := rollbackToOnce(ctx, tx); rbErr != nil {
			return Record{}, errors.Join(err, rbErr)
		}
		return Record{}, fmt.Errorf("record the result under the key: %w", err)
	}

	// Ask now, so an unsettled key fails on this call rather than at COMMIT.
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS `+settledConstraint+` IMMEDIATE`); err != nil {
		if rbErr := rollbackToOnce(ctx, tx); rbErr != nil {
			return Record{}, errors.Join(err, rbErr)
		}
		return Record{}, fmt.Errorf("settle the key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS `+settledConstraint+` DEFERRED`); err != nil {
		return Record{}, fmt.Errorf("re-defer the settled check: %w", err)
	}

	if err := releaseOnce(ctx, tx); err != nil {
		return Record{}, err
	}
	return Record{Key: c.Key, Transaction: id}, nil
}

// replay reads what the key already holds. It runs only after the reservation
// was refused, which means the transaction that took the key has committed.
func replay(ctx context.Context, tx *sql.Tx, c Claim) (Record, error) {
	var (
		id   sql.NullString
		hash []byte
	)
	err := tx.QueryRowContext(ctx,
		`SELECT transaction_id::text, request_hash FROM idempotency_keys WHERE key = $1`,
		c.Key,
	).Scan(&id, &hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Unreachable while nothing deletes these rows. Expiry, when it lands,
		// has to answer for this race.
		return Record{}, fmt.Errorf("the key %q was taken and is already gone", c.Key)
	case err != nil:
		return Record{}, fmt.Errorf("read the stored result: %w", err)
	}

	// Not constant time: reaching here means the client already knows the key.
	if !bytes.Equal(hash, c.RequestHash) {
		return Record{}, fmt.Errorf("%w: %s", ErrKeyReused, c.Key)
	}

	if !id.Valid {
		// The schema refuses to commit a key with no result, so this means a
		// database that has lost that trigger.
		return Record{}, fmt.Errorf("the key %q is stored with no transaction", c.Key)
	}
	return Record{Key: c.Key, Transaction: id.String, Replayed: true}, nil
}

// EntryOf reads back the entry a transaction holds, legs in posting order, so a
// replay is answered out of the ledger rather than from a stored copy.
func EntryOf(ctx context.Context, tx *sql.Tx, id string) (Entry, error) {
	var e Entry
	err := tx.QueryRowContext(ctx,
		`SELECT currency, description, occurred_at FROM transactions WHERE id = $1::uuid`,
		id,
	).Scan(&e.Currency, &e.Description, &e.OccurredAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Entry{}, fmt.Errorf("no transaction %s", id)
		}
		return Entry{}, fmt.Errorf("read transaction %s: %w", id, err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT a.code, p.amount_minor
		   FROM postings p
		   JOIN accounts a ON a.id = p.account_id
		  WHERE p.transaction_id = $1::uuid
		  ORDER BY p.id`,
		id,
	)
	if err != nil {
		return Entry{}, fmt.Errorf("read the legs of %s: %w", id, err)
	}
	defer rows.Close()

	for rows.Next() {
		var leg Leg
		if err := rows.Scan(&leg.Account, &leg.AmountMinor); err != nil {
			return Entry{}, fmt.Errorf("scan a leg of %s: %w", id, err)
		}
		e.Legs = append(e.Legs, leg)
	}
	if err := rows.Err(); err != nil {
		return Entry{}, fmt.Errorf("read the legs of %s: %w", id, err)
	}
	return e, nil
}

func rollbackToOnce(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+onceSavepoint); err != nil {
		return fmt.Errorf("rollback to savepoint: %w", err)
	}
	return nil
}

func releaseOnce(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+onceSavepoint); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

// code reports the SQLSTATE the server answered with, or "".
func code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// badKey names the refusal by the statement that raised it. The reservation
// touches one table and carries one client value, so anything refused there is
// the claim being wrong.
func badKey(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("reserve the idempotency key: %w", err)
	}
	return fmt.Errorf("%w: %w", ErrBadKey, pgErr)
}
