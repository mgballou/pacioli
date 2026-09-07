package ledgerhttp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
)

// maxPostings is the bound internal/ledgerhttp holds an entry to. It is not
// exported, so this is the copy a client would read off the refusal.
const maxPostings = 1000

// entryOf builds an entry of n legs that cancel in pairs, all on two accounts.
func entryOf(n int) string {
	legs := make([]string, 0, n)
	for i := range n {
		account, amount := cash, "100"
		if i%2 == 1 {
			account, amount = customer, "-100"
		}
		legs = append(legs, fmt.Sprintf(`{"account": %q, "amount_minor": %s}`, account, amount))
	}
	return fmt.Sprintf(`{"currency": "GBP", "description": "An entry of %d legs", "postings": [%s]}`,
		n, strings.Join(legs, ","))
}

// The 1 MB body was the only ceiling on a leg count, and it let one request
// carry 20,900 legs and take 21 seconds.
func TestMorePostingsThanTheBoundAreRefusedWithTheBound(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", entryOf(maxPostings+1))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got refusal
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Parameter != "postings" {
		t.Errorf("parameter %q, want postings", got.Parameter)
	}
	if got.Value != strconv.Itoa(maxPostings+1) {
		t.Errorf("value %q, want the %d postings that were sent", got.Value, maxPostings+1)
	}
	if !strings.Contains(got.Expected, strconv.Itoa(maxPostings)) {
		t.Errorf("expected %q, want it to name the bound of %d", got.Expected, maxPostings)
	}
	if got.See == "" {
		t.Errorf("body = %+v, want where the rule is written down", got)
	}
}

// The bound is the last entry it takes, not the first it refuses, and the
// balance check still runs on it.
func TestTheLargestEntryTheBoundAllowsIsTaken(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", entryOf(maxPostings))
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", res.StatusCode, body)
	}

	// 500 debits of 100 on top of the 4500 the seed left there.
	if bal := balanceOver(t, srv, cash); bal != 4500+500*100 {
		t.Errorf("%s = %d after a %d-leg entry, want %d", cash, bal, maxPostings, 4500+500*100)
	}
}

// One check for a thousand legs is still a check.
func TestAnEntryAtTheBoundThatDoesNotBalanceIsStillRefused(t *testing.T) {
	srv := serve(t, seeded)

	// The last credit is a penny short of cancelling the debit before it.
	entry := strings.Replace(entryOf(maxPostings), `"amount_minor": -100}]`, `"amount_minor": -99}]`, 1)
	res, body := post(t, srv.URL+"/v1/transactions", entry)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct {
		Error    string      `json:"error"`
		NetMinor json.Number `json:"net_minor"`
		Postings int         `json:"postings"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "the transaction does not balance" {
		t.Errorf("error = %q, want the entry refused for the reason it is wrong", got.Error)
	}
	if got.NetMinor.String() != "1" || got.Postings != maxPostings {
		t.Errorf("body = %+v, want 1 over %d postings", got, maxPostings)
	}
}

// A description ran to the 1 MB body, and the refusal handed the whole of it
// back. Both halves are fixed here: the ledger will not hold it, and the
// refusal says the rule, a trimmed value and how long the value was.
func TestAnOverLongDescriptionIsRefusedWithTheRuleAndTheLength(t *testing.T) {
	srv := serve(t, seeded)

	long := strings.Repeat("d", ledger.MaxDescription+1)
	res, body := post(t, srv.URL+"/v1/transactions", fmt.Sprintf(`{
	  "currency": "GBP",
	  "description": %q,
	  "postings": [
	    {"account": %q, "amount_minor": -500},
	    {"account": %q, "amount_minor": 500}
	  ]
	}`, long, cash, customer))

	got := assertBounded(t, res, body, "description", ledger.DescriptionShape, len(long))
	if strings.Contains(got.Value, long) {
		t.Errorf("the refusal handed the whole %d-character description back", len(long))
	}
	if len(body) > 4096 {
		t.Errorf("the refusal is %d bytes; a refusal about length must not repeat the length", len(body))
	}
}

func TestAnOverLongAccountNameIsRefusedWithTheRuleAndTheLength(t *testing.T) {
	srv := serve(t, empty)

	long := strings.Repeat("n", ledger.MaxName+1)
	res, body := postPlain(t, srv.URL+"/v1/accounts", fmt.Sprintf(
		`{"code": %q, "name": %q, "kind": "asset", "currency": "GBP"}`, savings, long))

	assertBounded(t, res, body, "name", ledger.NameShape, len(long))
}

// The code is the sharp one: it comes back out as a URL path segment, and a
// 100 kB one did. The refusal names the rule and the length, and carries
// neither the code nor a URL built from it.
func TestAnOverLongAccountCodeIsRefusedWithTheRuleAndTheLength(t *testing.T) {
	srv := serve(t, empty)

	long := "a" + strings.Repeat("b", 100_000)
	res, body := postPlain(t, srv.URL+"/v1/accounts", fmt.Sprintf(
		`{"code": %q, "name": "A code of a hundred kilobytes", "kind": "asset", "currency": "GBP"}`, long))

	assertBounded(t, res, body, "code", ledger.CodeShape, len(long))
	if len(body) > 4096 {
		t.Errorf("the refusal is %d bytes, and it was given a %d-character code", len(body), len(long))
	}
}

// And the bound itself: one character past it is refused like any other.
func TestACodeOneCharacterPastTheBoundIsRefused(t *testing.T) {
	srv := serve(t, empty)

	long := "a" + strings.Repeat("b", ledger.MaxCode)
	res, body := postPlain(t, srv.URL+"/v1/accounts", fmt.Sprintf(
		`{"code": %q, "name": "Too long by one", "kind": "asset", "currency": "GBP"}`, long))

	assertBounded(t, res, body, "code", ledger.CodeShape, len(long))
}

// The reproduction: four accounts answered GET /v1/accounts with 1,000,655
// bytes, because nothing bounded a name and the report reads them all back.
// None of the four opens now, and the report cannot be made large.
func TestFourAccountsCannotMakeAMegabyteOfAReport(t *testing.T) {
	srv := serve(t, empty)

	const each = 250_000
	for i, code := range []string{"assets.one", "assets.two", "assets.three", "assets.four"} {
		res, body := postPlain(t, srv.URL+"/v1/accounts", fmt.Sprintf(
			`{"code": %q, "name": %q, "kind": "asset", "currency": "GBP"}`,
			code, strings.Repeat("n", each)))
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("account %d: status %d, want 422: %d bytes", i+1, res.StatusCode, len(body))
		}
		if len(body) > 4096 {
			t.Errorf("account %d: the refusal is %d bytes, and it was given a %d-character name",
				i+1, len(body), each)
		}
	}

	res, body := get(t, srv.URL+"/v1/accounts")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}
	if len(body) > 4096 {
		t.Errorf("GET /v1/accounts is %d bytes after four refused opens, want a report of no accounts", len(body))
	}
}

// assertBounded checks the one shape every length refusal answers in: 422, the
// field, the rule in words, and the characters the value actually ran to.
func assertBounded(t *testing.T, res *http.Response, body []byte, field, shape string, characters int) refusal {
	t.Helper()

	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}
	var got refusal
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Parameter != field {
		t.Errorf("parameter %q, want %s", got.Parameter, field)
	}
	if got.Expected != shape {
		t.Errorf("expected %q, want the rule in words: %q", got.Expected, shape)
	}
	if got.Characters != characters {
		t.Errorf("characters %d, want the %d the value ran to", got.Characters, characters)
	}
	if got.See == "" {
		t.Errorf("body = %+v, want where the rule is written down", got)
	}
	return got
}
