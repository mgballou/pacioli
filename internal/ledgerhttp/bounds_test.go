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

// The bounds internal/ledgerhttp holds a write to: the legs of an entry, and
// the most of a body each endpoint reads. None is exported, so these are the
// copies a client would read off the refusals.
const (
	maxPostings        = 1000
	maxAccountBody     = 4 << 10
	maxTransactionBody = 256 << 10
)

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
// 100 kB one did. Two ceilings stop it now, and neither refusal carries the
// code or a URL built from it.
func TestAnOverLongAccountCodeIsRefusedWithTheRuleAndTheLength(t *testing.T) {
	srv := serve(t, empty)

	// A code the body still carries, so the ledger is the one that refuses it.
	t.Run("three thousand characters", func(t *testing.T) {
		long := "a" + strings.Repeat("b", 3_000)
		res, body := postPlain(t, srv.URL+"/v1/accounts", fmt.Sprintf(
			`{"code": %q, "name": "A code of three kilobytes", "kind": "asset", "currency": "GBP"}`, long))

		assertBounded(t, res, body, "code", ledger.CodeShape, len(long))
		if len(body) > 4096 {
			t.Errorf("the refusal is %d bytes, and it was given a %d-character code", len(body), len(long))
		}
	})

	// The hundred kilobytes it was, which this endpoint no longer reads.
	t.Run("a hundred kilobytes", func(t *testing.T) {
		long := "a" + strings.Repeat("b", 100_000)
		res, body := postPlain(t, srv.URL+"/v1/accounts", fmt.Sprintf(
			`{"code": %q, "name": "A code of a hundred kilobytes", "kind": "asset", "currency": "GBP"}`, long))

		if res.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d, want 413: %s", res.StatusCode, body)
		}
		if len(body) > 4096 {
			t.Errorf("the refusal is %d bytes, and it was given a %d-character code", len(body), len(long))
		}
		if strings.Contains(string(body), long[:64]) {
			t.Error("the refusal carries the code it would not read")
		}
	})
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
// None of the four opens now, at either of the two lengths it takes to be
// refused: a body carrying a name of a quarter of a megabyte is past what this
// endpoint reads, and a name the body does carry is refused by MaxName.
func TestFourAccountsCannotMakeAMegabyteOfAReport(t *testing.T) {
	srv := serve(t, empty)

	codes := []string{"assets.one", "assets.two", "assets.three", "assets.four"}
	for _, round := range []struct {
		name   string
		each   int
		status int
	}{
		{"a quarter of a megabyte of name each", 250_000, http.StatusRequestEntityTooLarge},
		{"three kilobytes of name each", 3_000, http.StatusUnprocessableEntity},
	} {
		t.Run(round.name, func(t *testing.T) {
			for i, code := range codes {
				res, body := postPlain(t, srv.URL+"/v1/accounts", fmt.Sprintf(
					`{"code": %q, "name": %q, "kind": "asset", "currency": "GBP"}`,
					code, strings.Repeat("n", round.each)))
				if res.StatusCode != round.status {
					t.Fatalf("account %d: status %d, want %d: %d bytes",
						i+1, res.StatusCode, round.status, len(body))
				}
				if len(body) > 4096 {
					t.Errorf("account %d: the refusal is %d bytes, and it was given a %d-character name",
						i+1, len(body), round.each)
				}
			}
		})
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

// The body a write carries is bounded too, and the number is the endpoint's
// own: a request the endpoint cannot answer for is refused before it is held.
// One body of a megabyte, of names no endpoint defines, was read whole, walked
// whole and answered with a list as long as it. DESIGN.md 25.
func TestEachEndpointReadsOnlyTheBodyItsOwnBoundsAllow(t *testing.T) {
	for _, e := range []struct {
		name    string
		path    string
		ceiling int
		body    func(int) string
		send    func(*testing.T, string, string) (*http.Response, []byte)
	}{
		{"an account", "/v1/accounts", maxAccountBody, accountBodyOf, postPlain},
		{"a transaction", "/v1/transactions", maxTransactionBody, transactionBodyOf, post},
	} {
		t.Run(e.name, func(t *testing.T) {
			srv := serve(t, seeded)

			// At the ceiling the body is read, so the ledger is what refuses it.
			res, body := e.send(t, srv.URL+e.path, e.body(e.ceiling))
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("a body of exactly %d bytes gave %d, want the 422 of a body that was read: %s",
					e.ceiling, res.StatusCode, body)
			}

			// One byte past it, and nothing reads it.
			res, body = e.send(t, srv.URL+e.path, e.body(e.ceiling+1))
			if res.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("a body of %d bytes gave %d, want 413: %s", e.ceiling+1, res.StatusCode, body)
			}

			var got refusal
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("unmarshal %q: %v", body, err)
			}
			if !strings.Contains(got.Expected, strconv.Itoa(e.ceiling)) {
				t.Errorf("expected %q, want it to name the ceiling of %d", got.Expected, e.ceiling)
			}
		})
	}
}

// The two ceilings are different numbers, because the two endpoints take
// different bodies. A body the transaction endpoint reads without comment is
// past what an account is.
func TestTheBodyCeilingIsTheEndpointsOwn(t *testing.T) {
	srv := serve(t, seeded)

	body := accountBodyOf(maxAccountBody + 1)
	res, got := postPlain(t, srv.URL+"/v1/accounts", body)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d bytes to /v1/accounts gave %d, want 413: %s", len(body), res.StatusCode, got)
	}

	// The same length of body, at the endpoint whose own bound is larger.
	res, got = post(t, srv.URL+"/v1/transactions", transactionBodyOf(maxAccountBody+1))
	if res.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatalf("%d bytes to /v1/transactions gave 413, so both endpoints hold one ceiling: %s", len(body), got)
	}
}

// And the ceiling admits the largest entry the endpoint takes, which is what a
// body limit chosen without one had no way to promise.
func TestTheLargestEntryTheEndpointTakesIsInsideTheBodyCeiling(t *testing.T) {
	srv := serve(t, empty)

	// Two codes at the schema's own ceiling, so every leg is as long as a leg
	// can be.
	debit := "a" + strings.Repeat("d", ledger.MaxCode-1)
	credit := "b" + strings.Repeat("c", ledger.MaxCode-1)
	for _, code := range []string{debit, credit} {
		res, body := postPlain(t, srv.URL+"/v1/accounts", fmt.Sprintf(
			`{"code": %q, "name": "At the ceiling", "kind": "asset", "currency": "GBP"}`, code))
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("open %s: status %d: %s", code, res.StatusCode, body)
		}
	}

	// maxPostings legs, a full-width amount on every one, and a description at
	// its own bound: the largest entry this endpoint accepts.
	legs := make([]string, 0, maxPostings)
	for i := range maxPostings {
		account, amount := debit, "9223372036854775807"
		if i%2 == 1 {
			account, amount = credit, "-9223372036854775807"
		}
		legs = append(legs, fmt.Sprintf(`{"account": %q, "amount_minor": %s}`, account, amount))
	}
	entry := fmt.Sprintf(`{"currency": "GBP", "description": %q, "postings": [%s]}`,
		strings.Repeat("d", ledger.MaxDescription), strings.Join(legs, ","))

	if len(entry) > maxTransactionBody {
		t.Fatalf("the largest entry this endpoint takes is %d bytes and the ceiling is %d",
			len(entry), maxTransactionBody)
	}

	res, body := post(t, srv.URL+"/v1/transactions", entry)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201 — the ceiling refuses an entry the endpoint accepts: %s",
			res.StatusCode, body)
	}
}

// A body carrying more legs than the endpoint takes is refused for the count,
// and the legs past the count are not read. Every one of them was: a 5,500-leg
// body was walked in full to be told it may carry a thousand. DESIGN.md 25.
func TestTheLegsPastTheBoundAreNotRead(t *testing.T) {
	srv := serve(t, seeded)

	legs := make([]string, 0, maxPostings+1)
	for i := range maxPostings {
		account, amount := cash, "100"
		if i%2 == 1 {
			account, amount = customer, "-100"
		}
		legs = append(legs, fmt.Sprintf(`{"account": %q, "amount_minor": %s}`, account, amount))
	}
	// One leg past the bound, and nothing about it can be read. Reaching it is
	// the failure.
	legs = append(legs, `{"account": 7, "amount_minor": "not a number", "elsewhere": true}`)

	res, body := post(t, srv.URL+"/v1/transactions", fmt.Sprintf(
		`{"currency": "GBP", "description": "One leg past the bound, and unreadable", "postings": [%s]}`,
		strings.Join(legs, ",")))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want the 422 of a count — the leg past the bound was read: %s",
			res.StatusCode, body)
	}

	var got refusal
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Parameter != "postings" || got.Value != strconv.Itoa(maxPostings+1) {
		t.Errorf("body = %+v, want the count of %d refused against the bound", got, maxPostings+1)
	}
}

// accountBodyOf is a json object of exactly n bytes for POST /v1/accounts, the
// padding going in the one field with room for it.
func accountBodyOf(n int) string {
	const shape = `{"code": "assets.pad", "name": %q, "kind": "asset", "currency": "GBP"}`
	return fmt.Sprintf(shape, strings.Repeat("n", n-len(fmt.Sprintf(shape, ""))))
}

// transactionBodyOf is the same for POST /v1/transactions.
func transactionBodyOf(n int) string {
	shape := `{"currency": "GBP", "description": %q, "postings": [` +
		fmt.Sprintf(`{"account": %q, "amount_minor": 100}, {"account": %q, "amount_minor": -100}`, cash, customer) +
		`]}`
	return fmt.Sprintf(shape, strings.Repeat("d", n-len(fmt.Sprintf(shape, ""))))
}
