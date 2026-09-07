package ledgerhttp_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// refusal is every field a refusal can carry. Each test below reads the whole
// thing, because what a client is owed is the value it sent as much as the rule.
type refusal struct {
	Error      string   `json:"error"`
	Code       string   `json:"code"`
	Parameter  string   `json:"parameter"`
	Value      string   `json:"value"`
	Expected   string   `json:"expected"`
	Characters int      `json:"characters"`
	Valid      []string `json:"valid"`
	See        string   `json:"see"`
}

// kinds is the closed set account_kind holds. A refusal about a kind has to
// hand back all of it.
var kinds = []string{"asset", "equity", "expense", "liability", "revenue"}

func read(t *testing.T, body []byte) refusal {
	t.Helper()

	var got refusal
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.See == "" {
		t.Errorf("the refusal sends the reader nowhere:\n%s", body)
	}
	t.Logf("%s", body)
	return got
}

func TestTheKindRefusalOnAnOpenCarriesTheKindAndTheKindsThereAre(t *testing.T) {
	srv := serve(t, empty)

	res, body := postPlain(t, srv.URL+"/v1/accounts", `{
	  "code": "assets.savings", "name": "Savings", "kind": "assets", "currency": "GBP"}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Parameter != "kind" || got.Value != "assets" {
		t.Errorf("refusal = %+v, want the kind that was given", got)
	}
	sorted := slices.Sorted(slices.Values(got.Valid))
	if !slices.Equal(sorted, kinds) {
		t.Errorf("the refusal lists %v, want every kind: %v", got.Valid, kinds)
	}
}

func TestTheKindRefusalOnAListCarriesTheKindAndTheKindsThereAre(t *testing.T) {
	srv := serve(t, seeded)

	res, body := get(t, srv.URL+"/v1/accounts?kind=liabilty")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Parameter != "kind" || got.Value != "liabilty" {
		t.Errorf("refusal = %+v, want the kind that was given", got)
	}
	if sorted := slices.Sorted(slices.Values(got.Valid)); !slices.Equal(sorted, kinds) {
		t.Errorf("the refusal lists %v, want every kind: %v", got.Valid, kinds)
	}
}

func TestTheQueryParameterRefusalCarriesTheParametersThereAre(t *testing.T) {
	srv := serve(t, seeded)

	res, body := get(t, srv.URL+"/v1/accounts?curency=GBP")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Parameter != "curency" {
		t.Errorf("refusal = %+v, want the parameter that was given", got)
	}
	if !slices.Equal(got.Valid, []string{"currency", "kind"}) {
		t.Errorf("the refusal lists %v, want currency and kind", got.Valid)
	}
}

func TestTheUnknownFieldRefusalCarriesTheFieldsThereAre(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP", "descriptoin": "Refund", "postings": []}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Parameter != "descriptoin" {
		t.Errorf("refusal = %+v, want the field that was given", got)
	}
	for _, want := range []string{"account", "amount_minor", "currency", "description", "occurred_at", "postings"} {
		if !slices.Contains(got.Valid, want) {
			t.Errorf("the refusal lists %v, and %q is missing", got.Valid, want)
		}
	}
}

func TestTheWrongTypeRefusalSaysWhatTypeItWanted(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP", "description": "Refund",
	  "postings": [{"account": "assets.cash", "amount_minor": "-500"}]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Parameter != "postings.amount_minor" || got.Value != "string" || got.Expected != "number" {
		t.Errorf("refusal = %+v, want the field, what it held and what it wanted", got)
	}
}

func TestTheUnreadableBodyRefusalSaysWhereItStopped(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{"currency": "GBP",,}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	if got := read(t, body); !strings.HasPrefix(got.Value, "byte ") {
		t.Errorf("refusal = %+v, want where the reading stopped", got)
	}
}

func TestTheCodeCollisionRefusalPointsAtWhatHoldsTheCode(t *testing.T) {
	srv := serve(t, empty)

	if res, body := postPlain(t, srv.URL+"/v1/accounts", savingsRequest); res.StatusCode != http.StatusCreated {
		t.Fatalf("the first open gave %d: %s", res.StatusCode, body)
	}
	res, body := postPlain(t, srv.URL+"/v1/accounts", savingsRequest)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Value != savings {
		t.Errorf("refusal = %+v, want the code that was given", got)
	}
	if !strings.Contains(got.See, "/v1/accounts/"+savings) {
		t.Errorf("the refusal sends the reader to %q, want the account that holds the code", got.See)
	}
}

func TestTheSchemaRefusalOnAnOpenNamesTheFieldAndTheValue(t *testing.T) {
	srv := serve(t, empty)

	for _, c := range []struct{ name, body, parameter, value string }{
		{"a code that is not a dotted lower-case path", `{
		  "code": "Assets.Savings", "name": "Savings", "kind": "asset", "currency": "GBP"}`, "code", "Assets.Savings"},
		{"a blank name", `{
		  "code": "assets.savings", "name": "   ", "kind": "asset", "currency": "GBP"}`, "name", "   "},
		{"a currency that is not three capitals", `{
		  "code": "assets.savings", "name": "Savings", "kind": "asset", "currency": "pounds"}`, "currency", "pounds"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, body := postPlain(t, srv.URL+"/v1/accounts", c.body)
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
			}
			got := read(t, body)
			if got.Parameter != c.parameter || got.Value != c.value {
				t.Errorf("refusal = %+v, want %q and the value that was sent for it", got, c.parameter)
			}
			// The server's own words name the constraint, and they stay in the log.
			if strings.Contains(string(body), "constraint") || strings.Contains(string(body), "SQLSTATE") {
				t.Errorf("body = %s, and it hands the client the schema", body)
			}
		})
	}
}

func TestTheZeroLegRefusalNamesTheLegAndTheAmount(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP", "description": "A leg that moves nothing",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": 0},
	    {"account": "liabilities.customer", "amount_minor": 0}]}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	if got := read(t, body); got.Parameter != "postings[0]" || got.Value != "0" {
		t.Errorf("refusal = %+v, want the leg and what it tried to move", got)
	}
}

func TestTheCurrencyMismatchRefusalCarriesBothSides(t *testing.T) {
	srv := serve(t, seededWithUSD)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP", "description": "Sterling into a dollar account",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": -500},
	    {"account": "assets.cash_usd", "amount_minor": 500}]}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Value != "GBP" || !slices.Equal(got.Valid, []string{"USD"}) {
		t.Errorf("refusal = %+v, want the currency sent and the one the account holds", got)
	}
}

func TestTheKeyRefusalsCarryTheKeyAndTheShapeAKeyTakes(t *testing.T) {
	srv := serve(t, seeded)

	res, body := postKeyed(t, srv.URL+"/v1/transactions", "1", refund)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}
	got := read(t, body)
	if got.Value != "1" || got.Expected == "" {
		t.Errorf("refusal = %+v, want the key that was sent and the shape a key takes", got)
	}

	res, body = postWith(t, srv.URL+"/v1/transactions", refund, func(*http.Request) {})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}
	if got := read(t, body); got.Expected == "" {
		t.Errorf("refusal = %+v, want the shape the missing key should have taken", got)
	}
}

func TestTheUnbalancedRefusalSaysWhatWouldHaveBeenTaken(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP", "description": "Refund, mistyped",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": -500},
	    {"account": "liabilities.customer", "amount_minor": 5000}]}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct {
		NetMinor int64  `json:"net_minor"`
		Expected string `json:"expected"`
		See      string `json:"see"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.NetMinor != 4500 || got.Expected == "" || got.See == "" {
		t.Errorf("refusal = %+v, want how far out it was and what would have been taken", got)
	}
	t.Logf("%s", body)
}

func TestAnUnknownAccountRefusalSendsTheReaderSomewhere(t *testing.T) {
	srv := serve(t, seeded)

	res, body := get(t, srv.URL+"/v1/accounts/assets.csah")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", res.StatusCode, body)
	}
	if got := read(t, body); got.Code != "assets.csah" {
		t.Errorf("refusal = %+v, want the code that was asked for", got)
	}
}
