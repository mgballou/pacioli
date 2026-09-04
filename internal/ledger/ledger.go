// Package ledger writes transactions to the ledger and derives its balances.
// The constraints live in the schema; this package writes, asks the server, and
// turns the refusal into an error a caller can switch on.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The reasons an entry can be refused. Each wraps the server's own *pgconn.PgError.
var (
	// ErrUnbalanced means the legs do not sum to zero, or there are none.
	ErrUnbalanced = errors.New("transaction does not balance")

	// ErrUnknownAccount means a leg named a code no account holds.
	ErrUnknownAccount = errors.New("no such account")

	// ErrCurrencyMismatch means a leg's account holds another currency.
	ErrCurrencyMismatch = errors.New("account currency is not the transaction currency")

	// ErrRejected is every other refusal. Unwrap to *pgconn.PgError for the
	// SQLSTATE and the message.
	ErrRejected = errors.New("ledger rejected the entry")
)

// A Leg is one side of a transaction: an account code and a signed amount in
// minor units. Debit is positive, credit negative, and the sides must cancel.
type Leg struct {
	Account     string
	AmountMinor int64
}

// An Entry is a transaction and the legs that make it up.
type Entry struct {
	Currency    string
	Description string
	Legs        []Leg

	// OccurredAt is when the money moved, which is not always when the row was
	// written. Zero means now.
	OccurredAt time.Time
}

// balanceConstraints names the two deferred constraint triggers. Post settles
// these and leaves the rest alone.
const balanceConstraints = "transactions_must_balance, postings_must_balance"

// savepoint is reused: a second SAVEPOINT of the same name hides the first.
const savepoint = "ledger_post"

// Post writes an entry and returns the id of the transaction it created. It
// takes a *sql.Tx because the legs of an entry are only ever true together.
//
// Post settles the balance check before it returns, so a refusal lands on this
// call rather than at COMMIT, and runs inside a savepoint, so a refused entry
// leaves tx usable.
func Post(ctx context.Context, tx *sql.Tx, e Entry) (string, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+savepoint); err != nil {
		return "", fmt.Errorf("savepoint: %w", err)
	}

	id, err := post(ctx, tx, e)
	if err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+savepoint); rbErr != nil {
			return "", errors.Join(err, fmt.Errorf("rollback to savepoint: %w", rbErr))
		}
		return "", err
	}

	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+savepoint); err != nil {
		return "", fmt.Errorf("release savepoint: %w", err)
	}
	return id, nil
}

func post(ctx context.Context, tx *sql.Tx, e Entry) (string, error) {
	// An earlier SET CONSTRAINTS could have made the triggers immediate, and
	// then the first leg of every entry would be refused on its own.
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS `+balanceConstraints+` DEFERRED`); err != nil {
		return "", fmt.Errorf("defer the balance check: %w", err)
	}

	occurredAt := sql.NullTime{Time: e.OccurredAt, Valid: !e.OccurredAt.IsZero()}

	var id string
	err := tx.QueryRowContext(ctx,
		`INSERT INTO transactions (currency, description, occurred_at)
		 VALUES ($1, $2, coalesce($3::timestamptz, now()))
		 RETURNING id`,
		e.Currency, e.Description, occurredAt,
	).Scan(&id)
	if err != nil {
		return "", classify(err, -1, Leg{})
	}

	for i, leg := range e.Legs {
		// A LEFT JOIN rather than a WHERE, so a code no account holds arrives
		// as a NULL account_id and is refused rather than silently dropped.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO postings (transaction_id, account_id, currency, amount_minor)
			 SELECT $1, a.id, a.currency, $3
			   FROM (SELECT $2::text AS code) req
			   LEFT JOIN accounts a ON a.code = req.code`,
			id, leg.Account, leg.AmountMinor,
		); err != nil {
			return "", classify(err, i, leg)
		}
	}

	// Ask now, not at COMMIT: deferred means the legs may arrive across statements.
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS `+balanceConstraints+` IMMEDIATE`); err != nil {
		return "", classify(err, -1, Leg{})
	}
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS `+balanceConstraints+` DEFERRED`); err != nil {
		return "", fmt.Errorf("re-defer the balance check: %w", err)
	}
	return id, nil
}

// classify turns the server's refusal into one of the errors above.
func classify(err error, legIndex int, leg Leg) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch {
	case pgErr.Code == "LB001":
		return fmt.Errorf("%w: %w", ErrUnbalanced, pgErr)

	case pgErr.Code == "23502" && pgErr.ColumnName == "account_id":
		return fmt.Errorf("leg %d: %w %q: %w", legIndex, ErrUnknownAccount, leg.Account, pgErr)

	case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "transaction_id"):
		return fmt.Errorf("leg %d (%s): %w: %w", legIndex, leg.Account, ErrCurrencyMismatch, pgErr)
	}

	if legIndex >= 0 {
		return fmt.Errorf("leg %d (%s): %w: %w", legIndex, leg.Account, ErrRejected, pgErr)
	}
	return fmt.Errorf("%w: %w", ErrRejected, pgErr)
}
