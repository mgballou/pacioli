// Package ledger writes transactions to the ledger and derives its balances.
// The constraints live in the schema; this package writes, asks the server, and
// turns the refusal into an error a caller can switch on.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
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

	// ErrRejected is every refusal this package does not name on its own;
	// unwrap to *pgconn.PgError for the SQLSTATE and the server's own words.
	ErrRejected = errors.New("a rule in the schema refused it")
)

// The two ways a transaction id can name nothing.
var (
	// ErrUnknownTransaction means no transaction holds that id.
	ErrUnknownTransaction = errors.New("no such transaction")

	// ErrBadTransactionID means the id is not the shape an id takes, so no
	// transaction can hold it.
	ErrBadTransactionID = errors.New("that is not a transaction id")
)

// IDShape puts the transactions primary key, the uuid in internal/schema, into
// words a client can act on.
const IDShape = "the uuid POST /v1/transactions answered with"

// A LegError says which leg of an entry the server refused, and what that leg
// carried.
type LegError struct {
	Index       int    // where the leg sat in Entry.Legs
	Account     string // the code that leg named
	AmountMinor int64  // what that leg tried to move
	Err         error  // the refusal, wrapping the server's own *pgconn.PgError

	// Holds is the currency the account actually holds, filled in only for
	// ErrCurrencyMismatch.
	Holds string
}

func (e *LegError) Error() string {
	return fmt.Sprintf("leg %d (%s, %d minor units): %s", e.Index, e.Account, e.AmountMinor, e.Err)
}

// Unwrap keeps errors.Is working through a LegError.
func (e *LegError) Unwrap() error { return e.Err }

// NotBlank is what the schema means by blank, in the words a refusal uses. It is
// said once here because is_blank and has_control_character are asked once each
// in internal/schema, of every field that holds a person's words.
const NotBlank = "at least one that is not a space, a tab or a line break, and no control characters"

// AmountShape puts postings.amount_minor, the bigint column in internal/schema,
// into words a client can act on. It is the rule a bare "number" cannot say:
// json has one kind of number and this ledger takes only whole ones.
var AmountShape = fmt.Sprintf(
	"a whole number of minor units, so 100.50 is 10050, from %d to %d", int64(math.MinInt64), int64(math.MaxInt64))

// DescriptionShape puts transactions_description_check, the CHECK in
// internal/schema, into words a client can act on. MaxDescription is the length
// those words allow, counted in characters and not bytes.
const MaxDescription = 500

var DescriptionShape = fmt.Sprintf("1 to %d characters, %s", MaxDescription, NotBlank)

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

// net returns what the legs sum to and how many there were. The sum is a Minor,
// not an int64: two legs the schema will each hold can still sum past one.
func (e Entry) net() (Minor, int) {
	var sum Minor
	for _, leg := range e.Legs {
		sum = sum.Add(MinorOf(leg.AmountMinor))
	}
	return sum, len(e.Legs)
}

// balanceConstraints names the two deferred triggers Post settles, and no others.
const balanceConstraints = "transactions_must_balance, balance_checks_must_balance"

// savepoint is reused: a second SAVEPOINT of the same name hides the first.
const savepoint = "ledger_post"

// Post writes an entry and returns the id of the transaction it created. A
// refused entry leaves tx usable, and Once is the entry point for a caller
// whose client retries.
func Post(ctx context.Context, tx *sql.Tx, e Entry) (string, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+savepoint); err != nil {
		return "", fmt.Errorf("savepoint: %w", err)
	}

	id, err := post(ctx, tx, e)
	if err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+savepoint); rbErr != nil {
			return "", errors.Join(err, fmt.Errorf("rollback to savepoint: %w", rbErr))
		}
		// The savepoint is back, so explain can read the ledger again.
		return "", explain(ctx, tx, err, e)
	}

	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+savepoint); err != nil {
		return "", fmt.Errorf("release savepoint: %w", err)
	}
	return id, nil
}

func post(ctx context.Context, tx *sql.Tx, e Entry) (string, error) {
	// An earlier SET CONSTRAINTS could have left these immediate, and then the
	// first leg of every entry would be refused on its own.
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

// EntryOf reads back the entry a transaction holds, legs in posting order. It
// is what serves GET /v1/transactions/{id}, and what a replayed write answers
// from. The id it returns is the one the book holds: Postgres takes a uuid in
// more spellings than it writes one in, and the answer should carry the ledger's.
//
// The id is a client's, so both ways it can name nothing are named: a well
// formed uuid the book does not hold, and a string that is not a uuid at all.
// The cast is what tells them apart, and Postgres refuses it with 22P02.
func EntryOf(ctx context.Context, tx *sql.Tx, id string) (string, Entry, error) {
	var (
		held string
		e    Entry
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id::text, currency, description, occurred_at FROM transactions WHERE id = $1::uuid`,
		id,
	).Scan(&held, &e.Currency, &e.Description, &e.OccurredAt)
	if err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return "", Entry{}, fmt.Errorf("%w: %q", ErrUnknownTransaction, shown(id))
		case code(err) == "22P02":
			return "", Entry{}, fmt.Errorf("%w: %q is not %s: %w", ErrBadTransactionID, shown(id), IDShape, err)
		}
		return "", Entry{}, fmt.Errorf("read transaction %s: %w", id, err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT a.code, p.amount_minor
		   FROM postings p
		   JOIN accounts a ON a.id = p.account_id
		  WHERE p.transaction_id = $1::uuid
		  ORDER BY p.id`,
		held,
	)
	if err != nil {
		return "", Entry{}, fmt.Errorf("read the legs of %s: %w", held, err)
	}
	defer rows.Close()

	for rows.Next() {
		var leg Leg
		if err := rows.Scan(&leg.Account, &leg.AmountMinor); err != nil {
			return "", Entry{}, fmt.Errorf("scan a leg of %s: %w", held, err)
		}
		e.Legs = append(e.Legs, leg)
	}
	if err := rows.Err(); err != nil {
		return "", Entry{}, fmt.Errorf("read the legs of %s: %w", held, err)
	}
	return held, e, nil
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

// unbalanced says how far out the entry was.
func unbalanced(e Entry, pgErr *pgconn.PgError) error {
	sum, legs := e.net()
	if legs == 0 {
		return fmt.Errorf("%w: the entry carried no postings, and an entry needs at least two that cancel: %w",
			ErrUnbalanced, pgErr)
	}
	return fmt.Errorf("%w: %s in %s net to %s minor units, want 0: %w",
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

// explain adds what only a second look at the ledger can say. A read that
// answers nothing leaves the refusal exactly as it was.
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

// postings is "1 posting" or "3 postings".
func postings(n int) string {
	return count(n, "posting")
}

// count pluralizes thing, so a message never reads "1 characters".
func count(n int, thing string) string {
	if n == 1 {
		return "1 " + thing
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// shown trims a value a client chose to something a log line can hold.
func shown(s string) string {
	const most = 80
	if utf8.RuneCountInString(s) <= most {
		return s
	}
	return string([]rune(s)[:most]) + "\u2026"
}
