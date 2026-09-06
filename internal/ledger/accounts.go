package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrAccountExists means the code is already held.
var ErrAccountExists = errors.New("an account already holds that code")

// An Account is the chart-of-accounts row a client asks for. The id and the
// creation time are the database's.
type Account struct {
	Code     string // e.g. "assets.cash" — lower case, dotted, unique
	Name     string // what it is called on a report
	Kind     string // one of the five account_kind values
	Currency string // three capitals, and the only currency the account holds
}

// openSavepoint wraps the insert so a refusal leaves the caller's transaction
// usable.
const openSavepoint = "ledger_open"

// Open adds an account to the chart. A refused open leaves nothing behind and
// leaves tx usable: every refusal below is a raised exception, and without the
// savepoint one mistyped kind would abort the caller's whole transaction.
func Open(ctx context.Context, tx *sql.Tx, a Account) error {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+openSavepoint); err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}

	// The kind is cast to the enum rather than checked against a list here,
	// so the values live in one place.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO accounts (code, name, kind, currency) VALUES ($1, $2, $3::account_kind, $4)`,
		a.Code, a.Name, a.Kind, a.Currency,
	)
	if err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+openSavepoint); rbErr != nil {
			return errors.Join(classifyOpen(err, a, refusal{}), fmt.Errorf("rollback to savepoint: %w", rbErr))
		}
		// The savepoint is back, so the chart can be read again: a collision
		// can say what already holds the code, and a mistyped kind can list
		// the kinds there are.
		return classifyOpen(err, a, secondLook(ctx, tx, err, a))
	}

	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+openSavepoint); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

// A refusal is what a second look at the chart adds to a refused open: what
// already holds the code that collided, and the kinds the schema has. Both are
// empty when the read could not be made, and a message then says less rather
// than saying something untrue.
type refusal struct {
	holder string
	kinds  []string
}

// secondLook reads only what the refusal at hand can use, and only after the
// savepoint has put the transaction back.
func secondLook(ctx context.Context, tx *sql.Tx, err error, a Account) refusal {
	switch code(err) {
	case "23505":
		return refusal{holder: holderOf(ctx, tx, a.Code)}
	case "22P02":
		kinds, kindsErr := accountKinds(ctx, tx)
		if kindsErr != nil {
			return refusal{}
		}
		return refusal{kinds: kinds}
	}
	return refusal{}
}

// holderOf describes the account already holding a code. Everything it reports
// is served to anyone by GET /v1/accounts/{code}, so a collision is told nothing
// it could not have asked for.
func holderOf(ctx context.Context, tx *sql.Tx, accountCode string) string {
	var name, kind, currency string
	if err := tx.QueryRowContext(ctx,
		`SELECT name, kind::text, currency FROM accounts WHERE code = $1`, accountCode,
	).Scan(&name, &kind, &currency); err != nil {
		return ""
	}
	return fmt.Sprintf("%q (%s, %s)", shown(name), kind, currency)
}

// classifyOpen turns the server's refusal into one of this package's values,
// carrying the values the caller gave and, where there is one, the set it should
// have chosen from. The PgError is wrapped in so nothing the server said is lost.
func classifyOpen(err error, a Account, more refusal) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("open account %q: %w", shown(a.Code), err)
	}

	switch pgErr.Code {
	// The only unique index a client can collide with is the one on code.
	case "23505":
		if more.holder == "" {
			return fmt.Errorf("%w: %q is taken: %w", ErrAccountExists, shown(a.Code), pgErr)
		}
		return fmt.Errorf("%w: %q is held by %s: %w", ErrAccountExists, shown(a.Code), more.holder, pgErr)

	// The enum cast, the only one on this statement.
	case "22P02":
		return &UnknownKindError{Kind: a.Kind, Valid: more.kinds, Err: pgErr}
	}
	return fmt.Errorf("%w: code %q, name %q, kind %q, currency %q: %w",
		ErrRejected, shown(a.Code), shown(a.Name), shown(a.Kind), shown(a.Currency), pgErr)
}
