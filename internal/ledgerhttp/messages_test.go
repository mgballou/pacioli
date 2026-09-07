package ledgerhttp_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
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
	Fields     []field  `json:"fields"`
	See        string   `json:"see"`
}

// field is one entry of the list a refusal carries when a body got more than
// one field wrong.
type field struct {
	Parameter string   `json:"parameter"`
	Value     string   `json:"value"`
	Expected  string   `json:"expected"`
	Valid     []string `json:"valid"`
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
	// The set an entry takes, and not the set a posting takes as well: the
	// decoder reads an object at a time, so it knows which object the name
	// was not in.
	if !slices.Equal(got.Valid, []string{"currency", "description", "occurred_at", "postings"}) {
		t.Errorf("the refusal lists %v, want what an entry takes", got.Valid)
	}

	res, body = post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP", "description": "Refund",
	  "postings": [{"account": "assets.cash", "amont_minor": -500}]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got = read(t, body)
	if got.Parameter != "postings[0].amont_minor" {
		t.Errorf("refusal = %+v, want the leg and the field that was given", got)
	}
	if !slices.Equal(got.Valid, []string{"account", "amount_minor"}) {
		t.Errorf("the refusal lists %v, want what a posting takes", got.Valid)
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
	if got.Parameter != "postings[0].amount_minor" || got.Value != `"-500"` || got.Expected != ledger.AmountShape {
		t.Errorf("refusal = %+v, want the field, what it held and what it wanted", got)
	}
}

// 100.5 is a number, and "expected: number" told a client its own value was what
// it was asked for. The rule is minor units as a whole number, and the refusal
// now says that, what was sent, and the amount that works.
func TestTheDecimalAmountRefusalNamesMinorUnitsAndNotJustANumber(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP", "description": "A pound and a half",
	  "postings": [{"account": "assets.cash", "amount_minor": 100.5}]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Parameter != "postings[0].amount_minor" {
		t.Errorf("refusal = %+v, want the field that was sent", got)
	}
	if got.Value != "100.5" {
		t.Errorf("refusal value = %q, want the number that was sent", got.Value)
	}
	if got.Expected == "number" {
		t.Errorf("refusal = %+v, and 100.5 is a number — the rule is minor units", got)
	}
	for _, want := range []string{"minor units", "10050"} {
		if !strings.Contains(got.Expected, want) {
			t.Errorf("expected = %q, and it does not say %q", got.Expected, want)
		}
	}
}

// The same decoder answers both endpoints, so an integer field on either says
// what a whole number is rather than "number".
func TestAWholeNumberIsNotCalledANumber(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP", "description": "More than a bigint holds",
	  "postings": [{"account": "assets.cash", "amount_minor": 9223372036854775808}]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Value != "9223372036854775808" {
		t.Errorf("refusal value = %q, want the number that was sent", got.Value)
	}
	if !strings.Contains(got.Expected, "9223372036854775807") {
		t.Errorf("expected = %q, and it does not say the largest amount a posting holds", got.Expected)
	}
}

// btrim() strips spaces and nothing else, so a description of one newline was a
// description. Both refusals hand back the value and the rule it broke.
func TestTheBlankRefusalNamesTheFieldAndWhatBlankMeans(t *testing.T) {
	t.Run("description", func(t *testing.T) {
		srv := serve(t, seeded)

		res, body := post(t, srv.URL+"/v1/transactions", `{
		  "currency": "GBP", "description": "\n",
		  "postings": [
		    {"account": "assets.cash", "amount_minor": -500},
		    {"account": "liabilities.customer", "amount_minor": 500}]}`)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
		}

		got := read(t, body)
		if got.Parameter != "description" || got.Value != "\n" {
			t.Errorf("refusal = %+v, want the description that was sent", got)
		}
		if got.Expected != ledger.DescriptionShape {
			t.Errorf("expected = %q, want %q", got.Expected, ledger.DescriptionShape)
		}
	})

	t.Run("name", func(t *testing.T) {
		srv := serve(t, empty)

		res, body := postPlain(t, srv.URL+"/v1/accounts", `{
		  "code": "assets.savings", "name": "\t", "kind": "asset", "currency": "GBP"}`)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
		}

		got := read(t, body)
		if got.Parameter != "name" || got.Value != "\t" {
			t.Errorf("refusal = %+v, want the name that was sent", got)
		}
		if got.Expected != ledger.NameShape {
			t.Errorf("expected = %q, want %q", got.Expected, ledger.NameShape)
		}
	})
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

// "tuesday" is not a date, and the body carrying it is good json throughout.
// The endpoint answered "the request body is not json this endpoint can read",
// which was false about the body and said nothing about the field. occurred_at
// is a time.Time, and encoding/json hands a type that reads itself back as a
// plain error with no field on it, so the whole body wore the fault.
func TestABadTimestampNamesTheFieldAndTheShapeATimestampTakes(t *testing.T) {
	srv := serve(t, seeded)

	for _, c := range []struct{ name, sent, value string }{
		{"a day of the week", `"tuesday"`, `"tuesday"`},
		{"a date with no time on it", `"2026-09-07"`, `"2026-09-07"`},
		{"a number", `17`, "17"},
		{"a boolean", `true`, "true"},
		{"an object", `{}`, "object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, body := post(t, srv.URL+"/v1/transactions", `{
			  "currency": "GBP", "description": "Refund", "occurred_at": `+c.sent+`,
			  "postings": [
			    {"account": "assets.cash", "amount_minor": -500},
			    {"account": "liabilities.customer", "amount_minor": 500}]}`)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
			}

			got := read(t, body)
			if strings.Contains(got.Error, "not json") {
				t.Errorf("refusal = %+v, and the body it is describing is json", got)
			}
			if got.Parameter != "occurred_at" || got.Value != c.value {
				t.Errorf("refusal = %+v, want the field and what it held", got)
			}
			if got.Expected != ledger.TimeShape {
				t.Errorf("expected = %q, want %q", got.Expected, ledger.TimeShape)
			}
		})
	}
}

// A body that is json and is not an object wore the same false sentence. It is
// the whole body that is wrong here, so the refusal names no field.
func TestAJsonBodyThatIsNotAnObjectIsNotCalledUnreadable(t *testing.T) {
	for _, c := range []struct{ name, sent, value string }{
		{"an array", `[1, 2, 3]`, "array"},
		{"a string", `"hello"`, `"hello"`},
		{"a number", `42`, "42"},
		{"null", `null`, "null"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := serve(t, seeded)

			res, body := post(t, srv.URL+"/v1/transactions", c.sent)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
			}
			got := read(t, body)
			if strings.Contains(got.Error, "not json") {
				t.Errorf("refusal = %+v, and the body it is describing is json", got)
			}
			if got.Value != c.value || got.Expected == "" {
				t.Errorf("refusal = %+v, want what the body held and what it should have", got)
			}

			res, body = postPlain(t, srv.URL+"/v1/accounts", c.sent)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("the open gave %d, want 400: %s", res.StatusCode, body)
			}
			if got := read(t, body); got.Value != c.value {
				t.Errorf("the open answered %+v, want what the body held", got)
			}
		})
	}
}

// encoding/json stops at the first field it cannot read, so a body with three
// mistakes in it took three round trips to mend. Every one is named, and the
// first is still where it has always been, so a client reading parameter alone
// reads what it always did.
func TestABodyWithSeveralBadFieldsNamesEveryOne(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": 7, "description": [], "occurred_at": "tuesday",
	  "postings": [{"account": "assets.cash", "amount_minor": 100.5}]}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got := read(t, body)
	want := []string{"currency", "description", "postings[0].amount_minor", "occurred_at"}
	var named []string
	for _, f := range got.Fields {
		named = append(named, f.Parameter)
		if f.Value == "" || f.Expected == "" {
			t.Errorf("field %+v, want what it held and what it wanted", f)
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(named)), slices.Sorted(slices.Values(want))) {
		t.Errorf("the refusal names %v, want every field that was wrong: %v", named, want)
	}
	if got.Parameter != got.Fields[0].Parameter || got.Value != got.Fields[0].Value {
		t.Errorf("refusal = %+v, want the first of the list at the top of the body", got)
	}
}

// The fix for the four sentences above is not a trade of one wrong answer for
// another: a body that is genuinely not json is still told so.
func TestABodyThatIsNotJsonIsStillToldSo(t *testing.T) {
	srv := serve(t, seeded)

	for _, c := range []struct{ name, sent, want string }{
		{"prose", `not json at all`, "not json"},
		{"a body that stops early", `{"currency": "GBP"`, "not json"},
		{"nothing at all", ``, "empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, body := post(t, srv.URL+"/v1/transactions", c.sent)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
			}
			got := read(t, body)
			if !strings.Contains(got.Error, c.want) {
				t.Errorf("refusal = %+v, want it to say %q", got, c.want)
			}
			// Where the reading stopped, or what it was short of. A client
			// cannot work either out from a body it thought was json.
			if got.Value == "" && got.Expected == "" {
				t.Errorf("refusal = %+v, want where the reading stopped or what it wanted", got)
			}
		})
	}
}
