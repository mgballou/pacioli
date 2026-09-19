package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

const (
	ops         = "expenses.ops"
	customerUSD = "liabilities.customer_usd"
	feesUSD     = "revenue.fees_usd"
)

var chart = map[string][]string{
	"GBP": {cash, customer, fees, ops},
	"USD": {cashUSD, customerUSD, feesUSD},
}

var currencies = []string{"GBP", "USD"}

func TestBalanceOfReadsBackWhatPostWrote(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	post(t, tx, "GBP", ledger.Leg{Account: cash, AmountMinor: 4500}, ledger.Leg{Account: customer, AmountMinor: -4500})

	got, err := ledger.BalanceOf(ctx, tx, cash)
	if err != nil {
		t.Fatalf("balance of %s: %v", cash, err)
	}
	want := ledger.Balance{
		Account: cash, Name: "Cash at bank", Kind: "asset",
		Currency: "GBP", AmountMinor: 4500, Postings: 1,
	}
	if got != want {
		t.Errorf("balance = %+v, want %+v", got, want)
	}
}

func TestBalanceSeesWorkTheCallerHasNotCommitted(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	post(t, tx, "GBP", ledger.Leg{Account: cash, AmountMinor: 4500}, ledger.Leg{Account: customer, AmountMinor: -4500})
	if got := amount(t, tx, cash); got != 4500 {
		t.Fatalf("%s = %d after one uncommitted post, want 4500", cash, got)
	}

	post(t, tx, "GBP", ledger.Leg{Account: cash, AmountMinor: 1000}, ledger.Leg{Account: customer, AmountMinor: -1000})
	if got := amount(t, tx, cash); got != 5500 {
		t.Errorf("%s = %d after two uncommitted posts, want 5500", cash, got)
	}
}

func TestARefusedEntryDoesNotMoveABalance(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	post(t, tx, "GBP", ledger.Leg{Account: cash, AmountMinor: 4500}, ledger.Leg{Account: customer, AmountMinor: -4500})

	if _, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency: "GBP", Description: "Fifty pence short",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4450},
		},
	}); !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("post: %v, want %v", err, ledger.ErrUnbalanced)
	}

	if got := amount(t, tx, cash); got != 4500 {
		t.Errorf("%s = %d after the refusal, want 4500", cash, got)
	}
}

func TestBalanceOfAnUntouchedAccountIsZero(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.BalanceOf(context.Background(), tx, fees)
	if err != nil {
		t.Fatalf("balance of %s: %v", fees, err)
	}
	if got.AmountMinor != 0 || got.Postings != 0 {
		t.Errorf("balance = %+v, want 0 over 0 postings", got)
	}
}

func TestBalanceOfACodeNoAccountHoldsIsAnError(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.BalanceOf(context.Background(), tx, "liabilities.custmoer")
	if !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("balance = %+v, err = %v, want %v", got, err, ledger.ErrUnknownAccount)
	}
	t.Logf("%v", err)
}

func TestBalancesListsEveryAccountInCodeOrder(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.Balances(context.Background(), tx)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}

	want := []string{cash, cashUSD, ops, customer, customerUSD, fees, feesUSD}
	slices.Sort(want)
	if len(got) != len(want) {
		t.Fatalf("%d balances, want %d: %+v", len(got), len(want), got)
	}
	for i, b := range got {
		if b.Account != want[i] {
			t.Errorf("balance %d is %s, want %s", i, b.Account, want[i])
		}
	}
}

func TestTrialBalanceIsPerCurrencyAndNeverAcross(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	post(t, tx, "GBP", ledger.Leg{Account: cash, AmountMinor: 4500}, ledger.Leg{Account: customer, AmountMinor: -4500})
	post(t, tx, "USD", ledger.Leg{Account: cashUSD, AmountMinor: 3000}, ledger.Leg{Account: customerUSD, AmountMinor: -3000})

	got, err := ledger.TrialBalance(ctx, tx)
	if err != nil {
		t.Fatalf("trial balance: %v", err)
	}

	want := []ledger.Trial{
		{Currency: "GBP", DebitsMinor: 4500, CreditsMinor: 4500, NetMinor: 0, Accounts: 4, Postings: 2},
		{Currency: "USD", DebitsMinor: 3000, CreditsMinor: 3000, NetMinor: 0, Accounts: 3, Postings: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

const (
	propertySeed         = 0x1ed6e11ab
	propertyTransactions = 250
)

func TestRandomValidTransactionSetsAlwaysBalance(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	rng := rand.New(rand.NewPCG(propertySeed, propertySeed))
	wantAmount := map[string]int64{}
	wantPostings := map[string]int64{}

	for n := range propertyTransactions {
		e := randomEntry(rng, n)
		if _, err := ledger.Post(ctx, tx, e); err != nil {
			t.Fatalf("entry %d %+v: %v", n, e, err)
		}
		for _, leg := range e.Legs {
			wantAmount[leg.Account] += leg.AmountMinor
			wantPostings[leg.Account]++
		}

		trial, err := ledger.TrialBalance(ctx, tx)
		if err != nil {
			t.Fatalf("trial balance after entry %d: %v", n, err)
		}
		for _, row := range trial {
			if !row.Balanced() {
				t.Fatalf("after entry %d (seed %d) the %s ledger nets to %d: %+v",
					n, propertySeed, row.Currency, row.NetMinor, row)
			}
			if row.DebitsMinor != row.CreditsMinor {
				t.Fatalf("after entry %d (seed %d) %s has %d debits and %d credits",
					n, propertySeed, row.Currency, row.DebitsMinor, row.CreditsMinor)
			}
		}
	}

	balances, err := ledger.Balances(ctx, tx)
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	for _, b := range balances {
		if b.AmountMinor != wantAmount[b.Account] {
			t.Errorf("%s = %d, Go made it %d", b.Account, b.AmountMinor, wantAmount[b.Account])
		}
		if b.Postings != wantPostings[b.Account] {
			t.Errorf("%s has %d postings, Go generated %d", b.Account, b.Postings, wantPostings[b.Account])
		}
	}
	t.Logf("%d transactions, seed %d, every intermediate state balanced", propertyTransactions, propertySeed)
}

func randomEntry(rng *rand.Rand, n int) ledger.Entry {
	currency := currencies[rng.IntN(len(currencies))]

	accounts := append([]string(nil), chart[currency]...)
	rng.Shuffle(len(accounts), func(i, j int) { accounts[i], accounts[j] = accounts[j], accounts[i] })

	for {
		legs := 2 + rng.IntN(len(accounts)-1)

		e := ledger.Entry{
			Currency:    currency,
			Description: fmt.Sprintf("random entry %d, %d legs", n, legs),
		}
		var net int64
		for i := range legs - 1 {
			amount := int64(1 + rng.IntN(1_000_000))
			if rng.IntN(2) == 0 {
				amount = -amount
			}
			net += amount
			e.Legs = append(e.Legs, ledger.Leg{Account: accounts[i], AmountMinor: amount})
		}

		// The schema refuses a zero posting, so draw again.
		if net == 0 {
			continue
		}
		e.Legs = append(e.Legs, ledger.Leg{Account: accounts[legs-1], AmountMinor: -net})
		return e
	}
}

func seedWiderChart(t *testing.T, tx *sql.Tx) {
	t.Helper()

	seedAccounts(t, tx)
	if _, err := tx.Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES
		   ($1, 'Operating costs',          'expense',   'GBP'),
		   ($2, 'Customer balances (USD)',  'liability', 'USD'),
		   ($3, 'Fee income (USD)',         'revenue',   'USD')`,
		ops, customerUSD, feesUSD,
	); err != nil {
		t.Fatalf("seed the rest of the chart: %v", err)
	}
}

func post(t *testing.T, tx *sql.Tx, currency string, legs ...ledger.Leg) {
	t.Helper()

	if _, err := ledger.Post(context.Background(), tx, ledger.Entry{
		Currency:    currency,
		Description: "Test entry",
		Legs:        legs,
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
}

func amount(t *testing.T, tx *sql.Tx, code string) int64 {
	t.Helper()

	b, err := ledger.BalanceOf(context.Background(), tx, code)
	if err != nil {
		t.Fatalf("balance of %s: %v", code, err)
	}
	return b.AmountMinor
}
