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
			return errors.Join(classifyOpen(err, a), fmt.Errorf("rollback to savepoint: %w", rbErr))
		}
		return classifyOpen(err, a)
	}

	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+openSavepoint); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

// classifyOpen turns the server's refusal into one of this package's values,
// with the PgError wrapped in so nothing it said is lost.
func classifyOpen(err error, a Account) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("open account %q: %w", a.Code, err)
	}

	switch pgErr.Code {
	// The only unique index a client can collide with is the one on code.
	case "23505":
		return fmt.Errorf("%w: %q: %w", ErrAccountExists, a.Code, pgErr)

	// The enum cast, the only one on this statement.
	case "22P02":
		return fmt.Errorf("%w %q: %w", ErrUnknownKind, a.Kind, pgErr)
	}
	return fmt.Errorf("%w: %w", ErrRejected, pgErr)
}
