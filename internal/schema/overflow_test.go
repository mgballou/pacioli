package schema_test

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/testdb"
)

// nearBigintMax is one posting the column will hold, and two of them are a
// balance the column would not.
const nearBigintMax = 9_000_000_000_000_000_000

func TestABalancePastBigintIsHeldAndReadBack(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	mustDefer(t, tx)

	first := postTransaction(t, tx, "First half of a very large deposit",
		leg{cash, nearBigintMax}, leg{customer, -nearBigintMax})
	postTransaction(t, tx, "Second half of a very large deposit",
		leg{cash, nearBigintMax}, leg{customer, -nearBigintMax})
	mustSettle(t, tx)

	// 1.8e19, which is past bigint and nowhere near the end of numeric.
	want := new(big.Int).Mul(big.NewInt(nearBigintMax), big.NewInt(2))
	if got := balance(t, tx, cash); got.Cmp(want) != 0 {
		t.Errorf("cash balance = %s, want %s", got, want)
	}
	if got := balance(t, tx, customer); got.Cmp(new(big.Int).Neg(want)) != 0 {
		t.Errorf("customer balance = %s, want -%s", got, want)
	}
	if got := netOf(t, tx, first); got.Sign() != 0 {
		t.Errorf("the entry that took cash past bigint nets to %s, want 0", got)
	}
}

// The balance trigger sums into a numeric, so an entry that nets past bigint is
// refused for the reason it is actually wrong — it does not balance — and the
// refusal carries the sum. Summing into a bigint raised 22003 instead, and the
// client was never told by how much it was out.
func TestAnEntryWhoseLegsSumPastBigintIsRefusedForNotBalancing(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)
	mustDefer(t, tx)

	postTransaction(t, tx, "Two debits, no credit",
		leg{cash, nearBigintMax}, leg{customer, nearBigintMax})

	_, err := tx.Exec(`SET CONSTRAINTS ALL IMMEDIATE`)
	assertCode(t, err, codeUnbalanced)

	want := new(big.Int).Mul(big.NewInt(nearBigintMax), big.NewInt(2)).String()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, want) {
		t.Errorf("the refusal said %q, and it has to name the %s it is out by", err, want)
	}
}
