package ledger_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/testdb"
)

// The kinds account_kind holds, as the schema declares them. A refusal that
// names a kind has to list these, and this is the only copy of them in Go.
var kinds = []string{"asset", "liability", "equity", "revenue", "expense"}

// says fails unless the message carries every one of want. Every test below asks
// the same thing: a reader is told what they gave, not only which rule it broke.
func says(t *testing.T, err error, sentinel error, want ...string) {
	t.Helper()

	if err == nil {
		t.Fatal("no error at all")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error is %v, want it to match %v", err, sentinel)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal does not say %q:\n  %v", w, err)
		}
	}
	t.Logf("%v", err)
}

func TestAnAccountThatAlreadyExistsNamesTheCodeAndWhatHoldsIt(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)

	first := ledger.Account{Code: savings, Name: "Savings at bank", Kind: "asset", Currency: "GBP"}
	if err := ledger.Open(ctx, tx, first); err != nil {
		t.Fatalf("open: %v", err)
	}

	err := ledger.Open(ctx, tx, ledger.Account{
		Code: savings, Name: "Someone else's savings", Kind: "asset", Currency: "GBP",
	})
	says(t, err, ledger.ErrAccountExists, savings, `"Savings at bank" (asset, GBP)`)
}

func TestAnUnknownKindOnOpenNamesTheKindAndTheKindsThereAre(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)

	err := ledger.Open(ctx, tx, ledger.Account{
		Code: savings, Name: "Savings at bank", Kind: "assets", Currency: "GBP",
	})
	says(t, err, ledger.ErrUnknownKind, append([]string{`"assets"`}, kinds...)...)

	var unknown *ledger.UnknownKindError
	if !errors.As(err, &unknown) {
		t.Fatalf("the refusal is not an *UnknownKindError, so a caller has to read its text: %v", err)
	}
	if unknown.Kind != "assets" || len(unknown.Valid) != len(kinds) {
		t.Errorf("the typed refusal is %+v, want the kind given and all %d kinds", unknown, len(kinds))
	}
}

func TestAnUnknownKindOnAFilterNamesTheKindAndTheKindsThereAre(t *testing.T) {
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := ledger.Balances(context.Background(), tx, ledger.AccountFilter{Kind: "liabilty"})
	says(t, err, ledger.ErrUnknownKind, append([]string{`"liabilty"`}, kinds...)...)
}

func TestAnAccountTheSchemaRefusesNamesEveryValueItWasGiven(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)

	err := ledger.Open(ctx, tx, ledger.Account{
		Code: savings, Name: "Savings", Kind: "asset", Currency: "pounds",
	})
	says(t, err, ledger.ErrRejected, savings, "Savings", "asset", "pounds")
}

func TestAnUnbalancedEntryNamesHowFarOutItIs(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Fifty pence short",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4450},
		},
	})
	says(t, err, ledger.ErrUnbalanced, "2 postings", "GBP", "50 minor units")
}

func TestAnEntryWithNoPostingsSaysThatRatherThanNettingToZero(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := ledger.Post(ctx, tx, ledger.Entry{Currency: "GBP", Description: "Nothing at all"})
	says(t, err, ledger.ErrUnbalanced, "no postings")
}

func TestAnUnknownAccountNamesTheCodeThatWasGiven(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := ledger.BalanceOf(ctx, tx, "assets.csah")
	says(t, err, ledger.ErrUnknownAccount, `"assets.csah"`)

	_, err = ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "To an account nobody opened",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: "liabilities.csutomer", AmountMinor: -4500},
		},
	})
	says(t, err, ledger.ErrUnknownAccount, "leg 1", "liabilities.csutomer", "-4500")
}

func TestACurrencyMismatchNamesBothSides(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Sterling into a dollar account",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: cashUSD, AmountMinor: -4500},
		},
	})
	says(t, err, ledger.ErrCurrencyMismatch, cashUSD, "holds USD", "in GBP")

	var leg *ledger.LegError
	if !errors.As(err, &leg) || leg.Holds != "USD" {
		t.Errorf("the refusal carries %+v, want the currency the account holds", leg)
	}
}

func TestALegTheSchemaRefusesNamesTheLegAndWhatItCarried(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "A leg that moves nothing",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 0},
			{Account: customer, AmountMinor: 0},
		},
	})
	says(t, err, ledger.ErrRejected, "leg 0", cash, "0 minor units")
}

func TestATransactionTheSchemaRefusesNamesWhatItWasGiven(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := ledger.Post(ctx, tx, ledger.Entry{
		Currency:    "GBP",
		Description: "   ",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4500},
		},
	})
	says(t, err, ledger.ErrRejected, `description "   "`, "2 postings", "GBP")
}

func TestAKeyUsedForAnotherRequestNamesTheKey(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	key := claim(t, "the deposit")
	if _, err := ledger.Once(ctx, tx, key, writing(ctx, tx, deposit)); err != nil {
		t.Fatalf("the first post: %v", err)
	}

	other := ledger.Claim{Key: key.Key, RequestHash: hashOf("a different request entirely")}
	_, err := ledger.Once(ctx, tx, other, writing(ctx, tx, deposit))
	says(t, err, ledger.ErrKeyReused, key.Key)
}

func TestAKeyTheLedgerWillNotHoldNamesItAndTheShapeItWanted(t *testing.T) {
	ctx := context.Background()
	tx := testdb.Tx(t)
	seedAccounts(t, tx)

	_, err := ledger.Once(ctx, tx,
		ledger.Claim{Key: "1", RequestHash: hashOf("short key")},
		writing(ctx, tx, deposit))
	says(t, err, ledger.ErrBadKey, `"1"`, "is 1 character", ledger.KeyShape)
}
