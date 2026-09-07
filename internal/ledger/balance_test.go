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
		Currency: "GBP", AmountMinor: ledger.MinorOf(4500), Postings: 1,
	}
	if !sameBalance(got, want) {
		t.Errorf("balance = %+v, want %+v", got, want)
	}
}

func TestBalanceSeesWorkTheCallerHasNotCommitted(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	post(t, tx, "GBP", ledger.Leg{Account: cash, AmountMinor: 4500}, ledger.Leg{Account: customer, AmountMinor: -4500})
	if got := amount(t, tx, cash); !got.Equal(ledger.MinorOf(4500)) {
		t.Fatalf("%s = %s after one uncommitted post, want 4500", cash, got)
	}

	post(t, tx, "GBP", ledger.Leg{Account: cash, AmountMinor: 1000}, ledger.Leg{Account: customer, AmountMinor: -1000})
	if got := amount(t, tx, cash); !got.Equal(ledger.MinorOf(5500)) {
		t.Errorf("%s = %s after two uncommitted posts, want 5500", cash, got)
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

	if got := amount(t, tx, cash); !got.Equal(ledger.MinorOf(4500)) {
		t.Errorf("%s = %s after the refusal, want 4500", cash, got)
	}
}

func TestBalanceOfAnUntouchedAccountIsZero(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.BalanceOf(context.Background(), tx, fees)
	if err != nil {
		t.Fatalf("balance of %s: %v", fees, err)
	}
	if !got.AmountMinor.IsZero() || got.Postings != 0 {
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

	got, err := ledger.Balances(context.Background(), tx, ledger.AccountFilter{})
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

func codes(bs []ledger.Balance) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Account)
	}
	return out
}

func TestAFilterNarrowsToOneCurrency(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.Balances(context.Background(), tx, ledger.AccountFilter{Currency: "USD"})
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	want := []string{cashUSD, customerUSD, feesUSD}
	if !slices.Equal(codes(got), want) {
		t.Errorf("codes = %v, want %v", codes(got), want)
	}
}

func TestAFilterNarrowsToOneKind(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.Balances(context.Background(), tx, ledger.AccountFilter{Kind: "asset"})
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	want := []string{cash, cashUSD}
	if !slices.Equal(codes(got), want) {
		t.Errorf("codes = %v, want %v", codes(got), want)
	}
}

func TestBothFiltersNarrowTogether(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.Balances(context.Background(), tx,
		ledger.AccountFilter{Currency: "USD", Kind: "asset"})
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	if want := []string{cashUSD}; !slices.Equal(codes(got), want) {
		t.Errorf("codes = %v, want %v", codes(got), want)
	}
}

func TestAListAnswersWithAtMostTheLimitItWasGiven(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	whole := listed(t, tx, ledger.AccountFilter{})
	got := listed(t, tx, ledger.AccountFilter{Limit: 3})
	if want := whole[:3]; !slices.Equal(got, want) {
		t.Errorf("codes = %v, want the first three of %v", got, whole)
	}
}

func TestALimitLargerThanTheChartIsTheWholeChart(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	whole := listed(t, tx, ledger.AccountFilter{})
	if got := listed(t, tx, ledger.AccountFilter{Limit: len(whole) + 100}); !slices.Equal(got, whole) {
		t.Errorf("codes = %v, want %v", got, whole)
	}
}

func TestAListStartsPastTheCodeItWasGiven(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	whole := listed(t, tx, ledger.AccountFilter{})
	got := listed(t, tx, ledger.AccountFilter{After: whole[2]})
	if want := whole[3:]; !slices.Equal(got, want) {
		t.Errorf("codes = %v, want everything past %s: %v", got, whole[2], want)
	}
}

func TestACodeNoAccountHoldsIsEmptyAndNotAnError(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	if got := listed(t, tx, ledger.AccountFilter{After: "zzz.nothing.holds.this"}); len(got) != 0 {
		t.Errorf("codes = %v, want none", got)
	}
}

// listed is the codes f matches, in the order Balances answers in.
func listed(t *testing.T, tx *sql.Tx, f ledger.AccountFilter) []string {
	t.Helper()

	got, err := ledger.Balances(context.Background(), tx, f)
	if err != nil {
		t.Fatalf("balances (%+v): %v", f, err)
	}
	return codes(got)
}

func TestACurrencyNoAccountHoldsIsEmptyAndNotAnError(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.Balances(context.Background(), tx, ledger.AccountFilter{Currency: "ZWL"})
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("balances = %+v, want none", got)
	}
}

func TestAKindNoAccountCanHoldIsAnError(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	got, err := ledger.Balances(context.Background(), tx, ledger.AccountFilter{Kind: "liabilty"})
	if !errors.Is(err, ledger.ErrUnknownKind) {
		t.Fatalf("balances = %+v, err = %v, want %v", got, err, ledger.ErrUnknownKind)
	}
	t.Logf("%v", err)
}

func TestARefusedFilterLeavesTheTransactionUsable(t *testing.T) {
	tx := testdb.Tx(t)
	seedWiderChart(t, tx)

	if _, err := ledger.Balances(context.Background(), tx, ledger.AccountFilter{Kind: "liabilty"}); err == nil {
		t.Fatal("the filter was accepted")
	}
	if _, err := ledger.Balances(context.Background(), tx, ledger.AccountFilter{}); err != nil {
		t.Errorf("the transaction did not survive the refusal: %v", err)
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
		{Currency: "GBP", DebitsMinor: ledger.MinorOf(4500), CreditsMinor: ledger.MinorOf(4500), Accounts: 4, Postings: 2},
		{Currency: "USD", DebitsMinor: ledger.MinorOf(3000), CreditsMinor: ledger.MinorOf(3000), Accounts: 3, Postings: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("%d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !sameTrial(got[i], want[i]) {
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
				t.Fatalf("after entry %d (seed %d) the %s ledger nets to %s: %+v",
					n, propertySeed, row.Currency, row.NetMinor, row)
			}
			if !row.DebitsMinor.Equal(row.CreditsMinor) {
				t.Fatalf("after entry %d (seed %d) %s has %s debits and %s credits",
					n, propertySeed, row.Currency, row.DebitsMinor, row.CreditsMinor)
			}
		}
	}

	balances, err := ledger.Balances(ctx, tx, ledger.AccountFilter{})
	if err != nil {
		t.Fatalf("balances: %v", err)
	}
	for _, b := range balances {
		if want := ledger.MinorOf(wantAmount[b.Account]); !b.AmountMinor.Equal(want) {
			t.Errorf("%s = %s, Go made it %s", b.Account, b.AmountMinor, want)
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

func amount(t *testing.T, tx *sql.Tx, code string) ledger.Minor {
	t.Helper()

	b, err := ledger.BalanceOf(context.Background(), tx, code)
	if err != nil {
		t.Fatalf("balance of %s: %v", code, err)
	}
	return b.AmountMinor
}

// A ledger.Minor holds a slice, so neither a Balance nor a Trial is comparable
// with == any more, and each field is named here instead.
func sameBalance(a, b ledger.Balance) bool {
	return a.Account == b.Account &&
		a.Name == b.Name &&
		a.Kind == b.Kind &&
		a.Currency == b.Currency &&
		a.Postings == b.Postings &&
		a.AmountMinor.Equal(b.AmountMinor)
}

func sameTrial(a, b ledger.Trial) bool {
	return a.Currency == b.Currency &&
		a.Accounts == b.Accounts &&
		a.Postings == b.Postings &&
		a.DebitsMinor.Equal(b.DebitsMinor) &&
		a.CreditsMinor.Equal(b.CreditsMinor) &&
		a.NetMinor.Equal(b.NetMinor)
}
