package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

// nearBigintMax is one leg the schema will hold, and two of them are a balance
// no int64 would.
const nearBigintMax = 9_000_000_000_000_000_000

// minorTimes is n × nearBigintMax, which is what these totals are made of.
func minorTimes(n int64) ledger.Minor {
	var m ledger.Minor
	for range n {
		m = m.Add(ledger.MinorOf(nearBigintMax))
	}
	return m
}

func TestABalancePastInt64ReadsBack(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: nearBigintMax},
		ledger.Leg{Account: customer, AmountMinor: -nearBigintMax})
	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: nearBigintMax},
		ledger.Leg{Account: customer, AmountMinor: -nearBigintMax})

	want := minorTimes(2)
	b, err := ledger.BalanceOf(ctx, tx, cash)
	if err != nil {
		t.Fatalf("balance of %s past int64: %v", cash, err)
	}
	if !b.AmountMinor.Equal(want) {
		t.Errorf("%s = %s, want %s", cash, b.AmountMinor, want)
	}

	// The list reads the same view, so it is the same scan and a separate path.
	rows, err := ledger.Balances(ctx, tx, ledger.AccountFilter{Currency: "GBP"})
	if err != nil {
		t.Fatalf("list balances past int64: %v", err)
	}
	if !amountIn(rows, cash).Equal(want) {
		t.Errorf("%s in the list = %s, want %s", cash, amountIn(rows, cash), want)
	}
	if !amountIn(rows, customer).Equal(want.Neg()) {
		t.Errorf("%s in the list = %s, want -%s", customer, amountIn(rows, customer), want)
	}
}

func TestATrialBalancePastInt64ReadsBack(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: nearBigintMax},
		ledger.Leg{Account: customer, AmountMinor: -nearBigintMax})
	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: nearBigintMax},
		ledger.Leg{Account: customer, AmountMinor: -nearBigintMax})

	row := trialFor(t, ctx, tx, "GBP")
	if want := minorTimes(2); !row.DebitsMinor.Equal(want) || !row.CreditsMinor.Equal(want) {
		t.Errorf("GBP = %s debits and %s credits, want %s either side",
			row.DebitsMinor, row.CreditsMinor, want)
	}
	if !row.Balanced() {
		t.Errorf("GBP nets to %s, want 0", row.NetMinor)
	}
}

// The finding this fixes was not that the total went out of range. It was that
// a reversing entry — the ledger's only repair, because the book is append-only
// — could not undo it: `debits` is a filtered sum that only ever grows, so the
// reversal adds to both sides and takes the report further out. Nothing here
// needs repairing, and the reversal is checked to prove it.
func TestAReversingEntryLeavesTheTrialBalanceReadable(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: nearBigintMax},
		ledger.Leg{Account: customer, AmountMinor: -nearBigintMax})
	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: nearBigintMax},
		ledger.Leg{Account: customer, AmountMinor: -nearBigintMax})

	before := trialFor(t, ctx, tx, "GBP")
	if want := minorTimes(2); !before.DebitsMinor.Equal(want) {
		t.Fatalf("before the reversal GBP has %s debits, want %s", before.DebitsMinor, want)
	}

	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: -nearBigintMax},
		ledger.Leg{Account: customer, AmountMinor: nearBigintMax})

	after := trialFor(t, ctx, tx, "GBP")
	if want := minorTimes(3); !after.DebitsMinor.Equal(want) || !after.CreditsMinor.Equal(want) {
		t.Errorf("after the reversal GBP = %s debits and %s credits, want %s either side",
			after.DebitsMinor, after.CreditsMinor, want)
	}
	if !after.Balanced() {
		t.Errorf("after the reversal GBP nets to %s, want 0", after.NetMinor)
	}

	// And the account the reversal was written for is back where it started.
	b, err := ledger.BalanceOf(ctx, tx, cash)
	if err != nil {
		t.Fatalf("balance of %s after the reversal: %v", cash, err)
	}
	if want := minorTimes(1); !b.AmountMinor.Equal(want) {
		t.Errorf("%s = %s after the reversal, want %s", cash, b.AmountMinor, want)
	}
}

// The other end of the same arithmetic: a stored total going out of int64 above,
// a computed one here. Decision 14 says a refusal that is arithmetic answers
// with the arithmetic, so this one owes the client the sum it is out by.
func TestAnEntryThatNetsPastInt64IsUnbalancedAndSaysByHowMuch(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	_, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Two debits and no credit",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: nearBigintMax},
			{Account: customer, AmountMinor: nearBigintMax},
		},
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("post = %v, want %v", err, ledger.ErrUnbalanced)
	}
	if want := minorTimes(2).String(); !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal said %q, and it has to say it is %s out", err, want)
	}

	// Refused, and the savepoint put the ledger back: the next entry still lands.
	post(t, tx, "GBP",
		ledger.Leg{Account: cash, AmountMinor: 4500},
		ledger.Leg{Account: customer, AmountMinor: -4500})
	if got := amount(t, tx, cash); !got.Equal(ledger.MinorOf(4500)) {
		t.Errorf("%s = %s after the refusal and one good entry, want 4500", cash, got)
	}
}

// A Minor is the type; these are the two boundaries it has to cross without an
// int64 in the way.
func TestAMinorCrossesJSONAndAnUnwidenedRow(t *testing.T) {
	past := new(big.Int).Mul(big.NewInt(nearBigintMax), big.NewInt(3)).String()

	var m ledger.Minor
	if err := m.Scan(past); err != nil {
		t.Fatalf("scan %s: %v", past, err)
	}
	if m.String() != past {
		t.Errorf("scanned %s, want %s", m, past)
	}

	out, err := m.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal %s: %v", m, err)
	}
	if string(out) != past {
		t.Errorf("marshalled to %s, want the bare number %s", out, past)
	}

	var back ledger.Minor
	if err := back.UnmarshalJSON(out); err != nil {
		t.Fatalf("unmarshal %s: %v", out, err)
	}
	if !back.Equal(m) {
		t.Errorf("round trip gave %s, want %s", back, m)
	}

	if err := m.Scan("1.5"); err == nil {
		t.Error("scanned 1.5 as minor units; a fraction of a minor unit is not one")
	}
	if err := m.Scan(3.0); err == nil {
		t.Error("scanned a float64 as minor units; no float belongs in the money path")
	}
}

func amountIn(rows []ledger.Balance, code string) ledger.Minor {
	for _, b := range rows {
		if b.Account == code {
			return b.AmountMinor
		}
	}
	return ledger.Minor{}
}

func trialFor(t *testing.T, ctx context.Context, tx *sql.Tx, currency string) ledger.Trial {
	t.Helper()

	rows, err := ledger.TrialBalance(ctx, tx)
	if err != nil {
		t.Fatalf("trial balance: %v", err)
	}
	for _, row := range rows {
		if row.Currency == currency {
			return row
		}
	}
	t.Fatalf("no %s row in the trial balance: %+v", currency, rows)
	return ledger.Trial{}
}
