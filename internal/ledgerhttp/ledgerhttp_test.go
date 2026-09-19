package ledgerhttp_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/testdb"
)

const (
	cash     = "assets.cash"
	cashUSD  = "assets.cash_usd"
	customer = "liabilities.customer"
	fees     = "revenue.fees"
	ops      = "expenses.ops"
)

func TestABalanceIsServedAsJSON(t *testing.T) {
	srv := serve(t, seeded)

	res, body := get(t, srv.URL+"/v1/accounts/"+cash)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type %q, want application/json", ct)
	}
	if got, want := res.Header.Get("Content-Length"), strconv.Itoa(len(body)); got != want {
		t.Errorf("content-length %q, want %q — the body was encoded before the header was written", got, want)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	want := map[string]any{
		"account": cash, "name": "Cash at bank", "kind": "asset",
		"currency": "GBP", "balance_minor": float64(4500), "postings": float64(1),
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
}

func TestAnUnknownAccountIs404(t *testing.T) {
	srv := serve(t, seeded)

	res, body := get(t, srv.URL+"/v1/accounts/assets.csah")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", res.StatusCode, body)
	}

	var got struct{ Error, Code string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "no such account" || got.Code != "assets.csah" {
		t.Errorf("body = %+v, want the refusal and the code that caused it", got)
	}
}

func TestAnUntouchedAccountIsZeroAndNotAbsent(t *testing.T) {
	srv := serve(t, seeded)

	res, body := get(t, srv.URL+"/v1/accounts/"+ops)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}
	var got struct {
		BalanceMinor int64 `json:"balance_minor"`
		Postings     int64 `json:"postings"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.BalanceMinor != 0 || got.Postings != 0 {
		t.Errorf("body = %+v, want 0 over 0 postings", got)
	}
}

func TestAWriteMethodIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, err := http.Post(srv.URL+"/v1/accounts/"+cash, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", res.StatusCode)
	}
	if allow := res.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow: %q, want it to name GET", allow)
	}
}

func TestTheAccountListIsEveryAccountInCodeOrder(t *testing.T) {
	srv := serve(t, seededWithUSD)

	res, body := get(t, srv.URL+"/v1/accounts")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}
	want := []string{cash, cashUSD, ops, customer, fees}
	if got := listed(t, body); !slices.Equal(got, want) {
		t.Errorf("codes = %v, want %v", got, want)
	}
}

func TestTheListNarrowsToACurrency(t *testing.T) {
	srv := serve(t, seededWithUSD)

	_, body := get(t, srv.URL+"/v1/accounts?currency=USD")
	if got, want := listed(t, body), []string{cashUSD}; !slices.Equal(got, want) {
		t.Errorf("codes = %v, want %v", got, want)
	}
}

func TestTheListNarrowsToAKind(t *testing.T) {
	srv := serve(t, seededWithUSD)

	_, body := get(t, srv.URL+"/v1/accounts?kind=asset")
	if got, want := listed(t, body), []string{cash, cashUSD}; !slices.Equal(got, want) {
		t.Errorf("codes = %v, want %v", got, want)
	}
}

func TestBothFiltersNarrowTogether(t *testing.T) {
	srv := serve(t, seededWithUSD)

	_, body := get(t, srv.URL+"/v1/accounts?currency=USD&kind=asset")
	if got, want := listed(t, body), []string{cashUSD}; !slices.Equal(got, want) {
		t.Errorf("codes = %v, want %v", got, want)
	}
}

func TestACurrencyNoAccountHoldsIsAnEmptyList(t *testing.T) {
	srv := serve(t, seededWithUSD)

	res, body := get(t, srv.URL+"/v1/accounts?currency=ZWL")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"accounts": []`)) {
		t.Errorf("body = %s, want an empty array", body)
	}
}

func TestAMistypedKindIsRefusedAndNotAnsweredEmpty(t *testing.T) {
	srv := serve(t, seededWithUSD)

	res, body := get(t, srv.URL+"/v1/accounts?kind=liabilty")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter, Value string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "no such account kind" || got.Parameter != "kind" || got.Value != "liabilty" {
		t.Errorf("body = %+v, want the refusal, the parameter and what it carried", got)
	}
}

func TestAnUnknownQueryParameterIsRefused(t *testing.T) {
	srv := serve(t, seededWithUSD)

	res, body := get(t, srv.URL+"/v1/accounts?curency=GBP")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — the typo was accepted and %d accounts came back",
			res.StatusCode, len(listed(t, body)))
	}

	var got struct{ Error, Parameter string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "no such query parameter" || got.Parameter != "curency" {
		t.Errorf("body = %+v, want the refusal and the parameter that caused it", got)
	}
}

func TestTwoUnknownParametersAreRefusedInAStableOrder(t *testing.T) {
	srv := serve(t, seededWithUSD)

	for i := range 8 {
		_, body := get(t, srv.URL+"/v1/accounts?zzz=1&aaa=2")
		var got struct{ Parameter string }
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal %q: %v", body, err)
		}
		if got.Parameter != "aaa" {
			t.Fatalf("request %d named %q, want aaa every time", i, got.Parameter)
		}
	}
}

func TestAQueryStringThatCannotBeReadIsRefused(t *testing.T) {
	srv := serve(t, seededWithUSD)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/accounts", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// Set past url.Parse, which would refuse to build this on the way out.
	req.URL.RawQuery = "%zz=1"

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", res.StatusCode)
	}
}

func listed(t *testing.T, body []byte) []string {
	t.Helper()

	var got struct {
		Accounts []struct {
			Account string `json:"account"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	out := make([]string, 0, len(got.Accounts))
	for _, a := range got.Accounts {
		out = append(out, a.Account)
	}
	return out
}

func TestTheTrialBalanceIsServedPerCurrency(t *testing.T) {
	srv := serve(t, seeded)

	res, body := get(t, srv.URL+"/v1/trial-balance")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}

	var got struct {
		Trial []trialRow `json:"trial"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if len(got.Trial) != 1 {
		t.Fatalf("%d rows, want 1: %+v", len(got.Trial), got.Trial)
	}
	row := got.Trial[0]
	if row.Currency != "GBP" || row.DebitsMinor != 4500 || row.CreditsMinor != 4500 || row.NetMinor != 0 || !row.Balanced {
		t.Errorf("row = %+v, want GBP 4500 either side and balanced", row)
	}
	if row.Accounts != 4 || row.Postings != 3 {
		t.Errorf("row = %+v, want 4 accounts over 3 postings", row)
	}
}

func TestAnEmptyChartIsAnEmptyListAndNotNull(t *testing.T) {
	srv := serve(t, func(*testing.T, *sql.Tx) {})

	_, body := get(t, srv.URL+"/v1/trial-balance")
	if !bytes.Contains(body, []byte(`"trial": []`)) {
		t.Errorf("body = %s, want an empty array", body)
	}
}

func TestAFaultIsA500AndTellsTheClientNothing(t *testing.T) {
	boom := errors.New("the database fell over")
	var logged bytes.Buffer

	h := ledgerhttp.Handler(readerFunc(func(context.Context, func(*sql.Tx) error) error {
		return boom
	}), log.New(&logged, "", 0))
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, body := get(t, srv.URL+"/v1/accounts/"+cash)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", res.StatusCode, body)
	}
	if strings.Contains(string(body), boom.Error()) {
		t.Errorf("body = %s, and it repeats the cause back to the client", body)
	}
	if !strings.Contains(logged.String(), boom.Error()) {
		t.Errorf("log = %q, want the cause the client was not told", logged.String())
	}
}

func TestPoolLendsAReadOnlyTransaction(t *testing.T) {
	p := ledgerhttp.Pool{DB: testdb.Open(t)}

	err := p.Read(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO accounts (code, name, kind, currency)
			 VALUES ('assets.read_only_control', 'Written by a read', 'asset', 'GBP')`)
		return err
	})
	if err == nil {
		t.Fatal("the insert was allowed; the transaction a request reads on can write")
	}
	if !strings.Contains(err.Error(), "read-only transaction") {
		t.Errorf("insert refused with %v, want Postgres refusing a read-only transaction", err)
	}
}

func TestPoolFinishesTheTransactionItLent(t *testing.T) {
	p := ledgerhttp.Pool{DB: testdb.Open(t)}

	var lent *sql.Tx
	if err := p.Read(context.Background(), func(tx *sql.Tx) error {
		lent = tx
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := lent.Rollback(); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("rollback of the lent transaction = %v, want %v", err, sql.ErrTxDone)
	}
}

// readerFunc lends the test's own transaction to every read and never commits: postings are append-only, so a committed row would move every other test's counts.
type readerFunc func(ctx context.Context, f func(*sql.Tx) error) error

func (r readerFunc) Read(ctx context.Context, f func(*sql.Tx) error) error { return r(ctx, f) }

// serve uses one transaction and never two: every seed writes the same account codes and code is UNIQUE, so a second transaction would block until the test times out.
func serve(t *testing.T, seed func(*testing.T, *sql.Tx)) *httptest.Server {
	t.Helper()

	tx := testdb.Tx(t)
	seed(t, tx)

	srv := httptest.NewServer(ledgerhttp.Handler(readerFunc(
		func(_ context.Context, f func(*sql.Tx) error) error { return f(tx) },
	), log.New(io.Discard, "", 0)))
	t.Cleanup(srv.Close)
	return srv
}

func seeded(t *testing.T, tx *sql.Tx) {
	t.Helper()

	if _, err := tx.Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES
		   ($1, 'Cash at bank',      'asset',     'GBP'),
		   ($2, 'Customer balances', 'liability', 'GBP'),
		   ($3, 'Fee income',        'revenue',   'GBP'),
		   ($4, 'Operating costs',   'expense',   'GBP')`,
		cash, customer, fees, ops,
	); err != nil {
		t.Fatalf("seed the chart: %v", err)
	}

	if _, err := ledger.Post(context.Background(), tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Customer deposit, less fee",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4350},
			{Account: fees, AmountMinor: -150},
		},
	}); err != nil {
		t.Fatalf("post the deposit: %v", err)
	}
}

func seededWithUSD(t *testing.T, tx *sql.Tx) {
	t.Helper()

	seeded(t, tx)
	if _, err := tx.Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES ($1, 'Cash at bank (USD)', 'asset', 'USD')`,
		cashUSD,
	); err != nil {
		t.Fatalf("seed the USD account: %v", err)
	}
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()

	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return res, body
}

type trialRow struct {
	Currency     string `json:"currency"`
	DebitsMinor  int64  `json:"debits_minor"`
	CreditsMinor int64  `json:"credits_minor"`
	NetMinor     int64  `json:"net_minor"`
	Balanced     bool   `json:"balanced"`
	Accounts     int64  `json:"accounts"`
	Postings     int64  `json:"postings"`
}
