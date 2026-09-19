package ledgerhttp_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const savings = "assets.savings"

type account struct {
	Account      string `json:"account"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
	Postings     int64  `json:"postings"`
}

const savingsRequest = `{
  "code": "assets.savings",
  "name": "Savings at bank",
  "kind": "asset",
  "currency": "GBP"
}`

func TestAnAccountIsOpenedOverHTTPAndIsThenReadable(t *testing.T) {
	srv := serve(t, empty)

	res, body := postPlain(t, srv.URL+"/v1/accounts", savingsRequest)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", res.StatusCode, body)
	}
	if got, want := res.Header.Get("Location"), "/v1/accounts/"+savings; got != want {
		t.Errorf("Location %q, want %q", got, want)
	}

	var got account
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	want := account{Account: savings, Name: "Savings at bank", Kind: "asset", Currency: "GBP"}
	if got != want {
		t.Errorf("body = %+v, want %+v — zero over zero postings", got, want)
	}

	res, body = get(t, srv.URL+"/v1/accounts/"+savings)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d reading it back, want 200: %s", res.StatusCode, body)
	}
	var read account
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if read != got {
		t.Errorf("the read surface says %+v and the open said %+v", read, got)
	}
}

func TestAChartOpenedOverHTTPTakesATransactionOverHTTP(t *testing.T) {
	srv := serve(t, empty)

	for _, body := range []string{savingsRequest, `{
	  "code": "equity.opening",
	  "name": "Opening balances",
	  "kind": "equity",
	  "currency": "GBP"
	}`} {
		if res, got := postPlain(t, srv.URL+"/v1/accounts", body); res.StatusCode != http.StatusCreated {
			t.Fatalf("status %d opening an account, want 201: %s", res.StatusCode, got)
		}
	}

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "description": "Opening balance",
	  "postings": [
	    {"account": "assets.savings", "amount_minor": 10000},
	    {"account": "equity.opening", "amount_minor": -10000}
	  ]
	}`)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d posting against the new chart, want 201: %s", res.StatusCode, body)
	}

	_, body = get(t, srv.URL+"/v1/accounts/"+savings)
	var read account
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if read.BalanceMinor != 10000 || read.Postings != 1 {
		t.Errorf("%s = %d over %d postings, want 10000 over 1", savings, read.BalanceMinor, read.Postings)
	}
}

func TestOpeningACodeTwiceIsAConflict(t *testing.T) {
	srv := serve(t, empty)

	if res, body := postPlain(t, srv.URL+"/v1/accounts", savingsRequest); res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d on the first open, want 201: %s", res.StatusCode, body)
	}

	res, body := postPlain(t, srv.URL+"/v1/accounts", `{
	  "code": "assets.savings",
	  "name": "Someone else's savings",
	  "kind": "asset",
	  "currency": "GBP"
	}`)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status %d on the second open, want 409: %s", res.StatusCode, body)
	}

	var refusal struct{ Error, Parameter, Value string }
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if refusal.Parameter != "code" || refusal.Value != savings {
		t.Errorf("refusal = %+v, want the field and the code that caused it", refusal)
	}
	if strings.Contains(string(body), "SQLSTATE") || strings.Contains(string(body), "ERROR:") {
		t.Errorf("body = %s, and it hands the client Postgres's own words", body)
	}

	_, body = get(t, srv.URL+"/v1/accounts/"+savings)
	var read account
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if read.Name != "Savings at bank" {
		t.Errorf("name = %q, want the first account's", read.Name)
	}
}

func TestAnAccountKindOutsideTheEnumIs422(t *testing.T) {
	srv := serve(t, empty)

	res, body := postPlain(t, srv.URL+"/v1/accounts", `{
	  "code": "assets.savings",
	  "name": "Savings at bank",
	  "kind": "assets",
	  "currency": "GBP"
	}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var refusal struct{ Error, Parameter, Value string }
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if refusal.Error != "no such account kind" || refusal.Parameter != "kind" || refusal.Value != "assets" {
		t.Errorf("refusal = %+v, want the kind that caused it", refusal)
	}
}

func TestAnAccountTheSchemaRefusesIs422(t *testing.T) {
	srv := serve(t, empty)

	for _, c := range []struct{ name, body string }{
		{"a code that is not a dotted lower-case path", `{
		  "code": "Assets.Savings", "name": "Savings", "kind": "asset", "currency": "GBP"}`},
		{"a blank name", `{
		  "code": "assets.savings", "name": "   ", "kind": "asset", "currency": "GBP"}`},
		{"a currency that is not three capitals", `{
		  "code": "assets.savings", "name": "Savings", "kind": "asset", "currency": "pounds"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, body := postPlain(t, srv.URL+"/v1/accounts", c.body)
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
			}
			if strings.Contains(string(body), "SQLSTATE") || strings.Contains(string(body), "constraint") {
				t.Errorf("body = %s, and it hands the client the schema", body)
			}
		})
	}
}

func TestAFieldTheAccountEndpointDoesNotDefineIsRefused(t *testing.T) {
	srv := serve(t, empty)

	res, body := postPlain(t, srv.URL+"/v1/accounts", `{
	  "code": "assets.savings",
	  "name": "Savings at bank",
	  "kind": "asset",
	  "curency": "GBP"
	}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	var refusal struct{ Error, Parameter string }
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if refusal.Error != "no such field" || refusal.Parameter != "curency" {
		t.Errorf("refusal = %+v, want the field that caused it", refusal)
	}
}

func TestARefusedOpenLeavesTheChartAloneAndTheServerAnswering(t *testing.T) {
	srv := serve(t, empty)

	if res, _ := postPlain(t, srv.URL+"/v1/accounts", `{
	  "code": "assets.savings", "name": "Savings", "kind": "assets", "currency": "GBP"}`,
	); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", res.StatusCode)
	}

	res, body := get(t, srv.URL+"/v1/accounts")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d listing the chart, want 200: %s", res.StatusCode, body)
	}
	var list struct {
		Accounts []account `json:"accounts"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if len(list.Accounts) != 0 {
		t.Errorf("the chart holds %d accounts after the refusal, want 0", len(list.Accounts))
	}
}

func empty(*testing.T, *sql.Tx) {}

func postPlain(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()

	return postWith(t, url, body, func(*http.Request) {})
}
