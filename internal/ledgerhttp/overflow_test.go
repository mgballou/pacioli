package ledgerhttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// nearBigintMax is one leg the ledger will hold. Two of them are a balance no
// int64 would, and the whole of this file is the reproduction that finding
// walked through, run against the endpoints rather than against Go.
const nearBigintMax = "9000000000000000000"

// twiceNear and thriceNear are 1.8e19 and 2.7e19: the balance after two such
// entries, and the debits total after the reversal that repairs them.
const (
	twiceNear  = "18000000000000000000"
	thriceNear = "27000000000000000000"
)

func bigEntry(description, debit, credit string, amount string) string {
	return fmt.Sprintf(`{
	  "currency": "GBP",
	  "description": %q,
	  "postings": [
	    {"account": %q, "amount_minor": %s},
	    {"account": %q, "amount_minor": -%s}
	  ]
	}`, description, debit, amount, credit, amount)
}

// wideBalance is what a client sees at GET /v1/accounts/{code}. json.Number,
// not int64: balance_minor is a sum of postings and nothing caps it at the
// width of one.
type wideBalance struct {
	Account      string      `json:"account"`
	BalanceMinor json.Number `json:"balance_minor"`
	Postings     int64       `json:"postings"`
}

type wideTrialRow struct {
	Currency     string      `json:"currency"`
	DebitsMinor  json.Number `json:"debits_minor"`
	CreditsMinor json.Number `json:"credits_minor"`
	NetMinor     json.Number `json:"net_minor"`
	Balanced     bool        `json:"balanced"`
}

// The reproduction: two legal 201s of 9e18, then all three read endpoints. Each
// one answered 500 from then on, and the account list and the trial balance
// were the two an operator would reach for to find out which account was out of
// range.
func TestTwoEntriesPastInt64LeaveEveryReadEndpointServing(t *testing.T) {
	srv := serve(t, empty)

	openAccounts(t, srv.URL)
	for i, description := range []string{"first half", "second half"} {
		res, body := post(t, srv.URL+"/v1/transactions", bigEntry(description, cash, customer, nearBigintMax))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("entry %d: status %d, want 201: %s", i+1, res.StatusCode, body)
		}
	}

	one := readBalance(t, srv.URL, cash)
	if one.BalanceMinor.String() != twiceNear {
		t.Errorf("GET /v1/accounts/%s gave %s, want %s", cash, one.BalanceMinor, twiceNear)
	}

	var list struct {
		Accounts []wideBalance `json:"accounts"`
	}
	getInto(t, srv.URL+"/v1/accounts", http.StatusOK, &list)
	if got := balanceIn(list.Accounts, customer); got != "-"+twiceNear {
		t.Errorf("GET /v1/accounts gave %s for %s, want -%s", got, customer, twiceNear)
	}

	row := readTrial(t, srv.URL)
	if row.DebitsMinor.String() != twiceNear || row.CreditsMinor.String() != twiceNear || !row.Balanced {
		t.Errorf("GET /v1/trial-balance gave %+v, want %s either side and balanced", row, twiceNear)
	}
}

// The half of the finding that had no repair: a reversing entry recovered
// /v1/accounts and could never recover /v1/trial-balance, because `debits` is a
// filtered sum that only ever grows and the book is append-only. Both reports
// answer here, before and after.
func TestAReversingEntryKeepsTheTrialBalanceServing(t *testing.T) {
	srv := serve(t, empty)

	openAccounts(t, srv.URL)
	for _, description := range []string{"first half", "second half"} {
		res, body := post(t, srv.URL+"/v1/transactions", bigEntry(description, cash, customer, nearBigintMax))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("status %d, want 201: %s", res.StatusCode, body)
		}
	}

	before := readTrial(t, srv.URL)
	if before.DebitsMinor.String() != twiceNear {
		t.Fatalf("before the reversal debits are %s, want %s", before.DebitsMinor, twiceNear)
	}

	res, body := post(t, srv.URL+"/v1/transactions", bigEntry("reversal", customer, cash, nearBigintMax))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("the reversal: status %d, want 201: %s", res.StatusCode, body)
	}

	after := readTrial(t, srv.URL)
	if after.DebitsMinor.String() != thriceNear || after.CreditsMinor.String() != thriceNear {
		t.Errorf("after the reversal the trial balance is %+v, want %s either side", after, thriceNear)
	}
	if !after.Balanced || after.NetMinor.String() != "0" {
		t.Errorf("after the reversal GBP nets to %s, want 0", after.NetMinor)
	}
	if got := readBalance(t, srv.URL, cash).BalanceMinor.String(); got != nearBigintMax {
		t.Errorf("%s = %s after the reversal, want %s", cash, got, nearBigintMax)
	}
}

// The second half of the same fault, at the other end: a computed total out of
// range rather than a stored one. It answered a 422 with no parameter, no value
// and no arithmetic, which is the one refusal in the repo that broke decision
// 14. It is an unbalanced entry, and it owes the client the sum it is out by.
func TestAnEntryThatNetsPastInt64IsRefusedWithItsArithmetic(t *testing.T) {
	srv := serve(t, empty)

	openAccounts(t, srv.URL)
	res, body := post(t, srv.URL+"/v1/transactions", fmt.Sprintf(`{
	  "currency": "GBP",
	  "description": "two debits and no credit",
	  "postings": [
	    {"account": %q, "amount_minor": %s},
	    {"account": %q, "amount_minor": %s}
	  ]
	}`, cash, nearBigintMax, customer, nearBigintMax))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct {
		Error    string      `json:"error"`
		NetMinor json.Number `json:"net_minor"`
		Postings int         `json:"postings"`
		Expected string      `json:"expected"`
		See      string      `json:"see"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "the transaction does not balance" {
		t.Errorf("error = %q, want the entry refused for the reason it is wrong", got.Error)
	}
	if got.NetMinor.String() != twiceNear || got.Postings != 2 {
		t.Errorf("body = %+v, want %s over 2 postings", got, twiceNear)
	}
	if got.Expected == "" || got.See == "" {
		t.Errorf("body = %+v, want the rule in words and where it is written down", got)
	}
}

func readBalance(t *testing.T, base, code string) wideBalance {
	t.Helper()

	var got wideBalance
	getInto(t, base+"/v1/accounts/"+code, http.StatusOK, &got)
	return got
}

func readTrial(t *testing.T, base string) wideTrialRow {
	t.Helper()

	var got struct {
		Trial []wideTrialRow `json:"trial"`
	}
	getInto(t, base+"/v1/trial-balance", http.StatusOK, &got)
	if len(got.Trial) != 1 {
		t.Fatalf("%d trial rows, want 1: %+v", len(got.Trial), got.Trial)
	}
	return got.Trial[0]
}

func getInto(t *testing.T, url string, want int, into any) {
	t.Helper()

	res, body := get(t, url)
	if res.StatusCode != want {
		t.Fatalf("GET %s: status %d, want %d: %s", url, res.StatusCode, want, body)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
}

func balanceIn(rows []wideBalance, code string) string {
	for _, b := range rows {
		if b.Account == code {
			return b.BalanceMinor.String()
		}
	}
	return ""
}

// openAccounts opens the two accounts these entries move money between, over
// the API, so nothing in the reproduction reaches past the endpoints.
func openAccounts(t *testing.T, base string) {
	t.Helper()

	for _, a := range []struct{ code, name, kind string }{
		{cash, "Cash at bank", "asset"},
		{customer, "Customer balances", "liability"},
	} {
		res, body := postPlain(t, base+"/v1/accounts", fmt.Sprintf(
			`{"code": %q, "name": %q, "kind": %q, "currency": "GBP"}`, a.code, a.name, a.kind))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("open %s: status %d, want 201: %s", a.code, res.StatusCode, body)
		}
	}
}
