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
	"unicode/utf8"

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

	// ErrRejected is every refusal this package does not name on its own. In
	// practice that is the schema's shape rules — a code that is not a dotted
	// lower-case path, a blank name or description, a currency that is not
	// three capitals, a posting of zero — and the triggers that keep
	// transactions and postings append-only. Unwrap to *pgconn.PgError for the
	// SQLSTATE and the server's own words; the wraps below carry the values
	// that were given.
	ErrRejected = errors.New("a rule in the schema refused it")
)

// A LegError says which leg of an entry the server refused, and what that leg
// carried. It is typed so a caller can name the leg, the amount and the currency
// the account holds without reading this package's error text.
type LegError struct {
	Index       int    // where the leg sat in Entry.Legs
	Account     string // the code that leg named
	AmountMinor int64  // what that leg tried to move
	Err         error  // the refusal, wrapping the server's own *pgconn.PgError

	// Holds is the currency the account actually holds, filled in only for
	// ErrCurrencyMismatch, where the two sides are the whole of the refusal.
	Holds string
}

func (e *LegError) Error() string {
	return fmt.Sprintf("leg %d (%s, %d minor units): %s", e.Index, e.Account, e.AmountMinor, e.Err)
}

// Unwrap keeps errors.Is working through a LegError.
func (e *LegError) Unwrap() error { return e.Err }

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

// net returns what the legs sum to and how many there were, so a refusal can say
// how far out an entry was rather than leaving a person to add it up.
func (e Entry) net() (int64, int) {
	var sum int64
	for _, leg := range e.Legs {
		sum += leg.AmountMinor
	}
	return sum, len(e.Legs)
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
// leaves tx usable. Once is the entry point for a caller whose client retries.
func Post(ctx context.Context, tx *sql.Tx, e Entry) (string, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+savepoint); err != nil {
		return "", fmt.Errorf("savepoint: %w", err)
	}

	id, err := post(ctx, tx, e)
	if err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+savepoint); rbErr != nil {
			return "", errors.Join(err, fmt.Errorf("rollback to savepoint: %w", rbErr))
		}
		// The savepoint is back, so the ledger can be read again and a refusal
		// that only a second look can explain can have one.
		return "", explain(ctx, tx, err, e)
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
		return "", classify(err, e, -1, Leg{})
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
			return "", classify(err, e, i, leg)
		}
	}

	// Ask now, not at COMMIT: deferred means the legs may arrive across statements.
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS `+balanceConstraints+` IMMEDIATE`); err != nil {
		return "", classify(err, e, -1, Leg{})
	}
	if _, err := tx.ExecContext(ctx, `SET CONSTRAINTS `+balanceConstraints+` DEFERRED`); err != nil {
		return "", fmt.Errorf("re-defer the balance check: %w", err)
	}
	return id, nil
}

// classify turns the server's refusal into one of the errors above, carrying
// back whatever of the entry the refusal was about.
func classify(err error, e Entry, legIndex int, leg Leg) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch {
	case pgErr.Code == "LB001":
		return unbalanced(e, pgErr)

	case pgErr.Code == "23502" && pgErr.ColumnName == "account_id":
		return legErr(legIndex, leg, ErrUnknownAccount, pgErr)

	case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "transaction_id"):
		return legErr(legIndex, leg, ErrCurrencyMismatch, pgErr)
	}

	if legIndex >= 0 {
		return legErr(legIndex, leg, ErrRejected, pgErr)
	}
	return fmt.Errorf("%w: currency %q, description %q, over %s: %w",
		ErrRejected, shown(e.Currency), shown(e.Description), postings(len(e.Legs)), pgErr)
}

// unbalanced says how far out the entry was, which is the number a person
// otherwise works out by hand from the legs they sent.
func unbalanced(e Entry, pgErr *pgconn.PgError) error {
	sum, legs := e.net()
	if legs == 0 {
		return fmt.Errorf("%w: the entry carried no postings, and an entry needs at least two that cancel: %w",
			ErrUnbalanced, pgErr)
	}
	return fmt.Errorf("%w: %s in %s net to %d minor units, want 0: %w",
		ErrUnbalanced, postings(legs), shown(e.Currency), sum, pgErr)
}

// legErr pairs a refusal with the leg that caused it and what that leg carried.
func legErr(index int, leg Leg, reason error, pgErr *pgconn.PgError) error {
	return &LegError{
		Index:       index,
		Account:     leg.Account,
		AmountMinor: leg.AmountMinor,
		Err:         fmt.Errorf("%w: %w", reason, pgErr),
	}
}

// explain adds what only a second look at the ledger can say. It runs after the
// savepoint is back, so the transaction can be read again; a read that answers
// nothing leaves the refusal exactly as it was.
func explain(ctx context.Context, tx *sql.Tx, err error, e Entry) error {
	var leg *LegError
	if !errors.As(err, &leg) || !errors.Is(err, ErrCurrencyMismatch) {
		return err
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	var held string
	if qErr := tx.QueryRowContext(ctx,
		`SELECT currency FROM accounts WHERE code = $1`, leg.Account,
	).Scan(&held); qErr != nil {
		return err
	}

	return &LegError{
		Index:       leg.Index,
		Account:     leg.Account,
		AmountMinor: leg.AmountMinor,
		Holds:       held,
		Err: fmt.Errorf("%w: %s holds %s and the entry is in %s: %w",
			ErrCurrencyMismatch, leg.Account, held, shown(e.Currency), pgErr),
	}
}

// postings is "1 posting" or "3 postings", so a count reads as a sentence.
func postings(n int) string {
	return count(n, "posting")
}

// count is "1 posting" or "3 postings", so a number in a message never reads as
// "1 characters".
func count(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// shown trims a value a client chose to something a log line can hold. Every
// value in these messages came in over the wire, and none of it is bounded.
func shown(s string) string {
	const most = 80
	if utf8.RuneCountInString(s) <= most {
		return s
	}
	return string([]rune(s)[:most]) + "\u2026"
}
