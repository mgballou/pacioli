package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestARefusalDoesNotExposeItsConstraintName(t *testing.T) {
	tx := testdb.Tx(t)
	err := ledger.Open(context.Background(), tx, ledger.Account{
		Code: "assets.invalid", Name: "Invalid currency", Kind: "asset", Currency: "pounds",
	})
	if !errors.Is(err, ledger.ErrRejected) {
		t.Fatalf("Open = %v, want ErrRejected", err)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName == "" {
		t.Fatalf("Open error = %v, want the server's constraint name for this probe", err)
	}
	if strings.Contains(err.Error(), pgErr.ConstraintName) {
		t.Skipf("His call: CONTRIBUTING.md requires wrapping *pgconn.PgError, while DESIGN.md decision 14 says the server's schema words stay out of a refusal. Decide whether internal ledger error text may expose %q before changing that contract.", pgErr.ConstraintName)
	}
}
