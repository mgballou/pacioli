package ledgerhttp_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The page GET /v1/accounts answers with. Neither is exported, so these are the
// copies a client would read off the endpoint and its refusal.
const (
	defaultPage = 100
	maxPage     = 1000
)

// chartOf seeds n accounts in code order, every second one in USD, so a page
// can be told from the page after it and a filter can be carried across one.
func chartOf(n int) func(*testing.T, *sql.Tx) {
	return func(t *testing.T, tx *sql.Tx) {
		t.Helper()

		if _, err := tx.Exec(
			`INSERT INTO accounts (code, name, kind, currency)
			 SELECT 'assets.page_' || lpad(i::text, 4, '0'),
			        'Branch ' || i,
			        'asset',
			        CASE WHEN i % 2 = 0 THEN 'GBP' ELSE 'USD' END
			   FROM generate_series(1, $1) AS i`, n,
		); err != nil {
			t.Fatalf("seed a chart of %d accounts: %v", n, err)
		}
	}
}

// pageCode is the code chartOf gives the i-th account.
func pageCode(i int) string { return fmt.Sprintf("assets.page_%04d", i) }

// The reproduction: the chart grows and nothing stopped the list answering with
// all of it. 200,000 accounts came back as 39 MB in one response.
func TestAChartLargerThanAPageIsAnsweredWithOnePage(t *testing.T) {
	srv := serve(t, chartOf(defaultPage+150))

	res, body := get(t, srv.URL+"/v1/accounts")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}

	codes, next := page(t, body)
	if len(codes) != defaultPage {
		t.Fatalf("%d accounts, want a page of %d", len(codes), defaultPage)
	}
	if codes[0] != pageCode(1) || codes[len(codes)-1] != pageCode(defaultPage) {
		t.Errorf("the page runs %s..%s, want %s..%s",
			codes[0], codes[len(codes)-1], pageCode(1), pageCode(defaultPage))
	}
	if want := "/v1/accounts?after=" + pageCode(defaultPage) + "&limit=" + strconv.Itoa(defaultPage); next != want {
		t.Errorf("next = %q, want %q", next, want)
	}
}

// A page is only an answer if the rest of the chart can be reached from it.
func TestFollowingNextReachesEveryAccountOnceAndStops(t *testing.T) {
	const held = 250
	srv := serve(t, chartOf(held))

	var got []string
	at, requests := "/v1/accounts?limit=40", 0
	for at != "" {
		requests++
		if requests > 20 {
			t.Fatalf("20 requests in and next still says %q; the cursor is not moving", at)
		}
		res, body := get(t, srv.URL+at)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, want 200: %s", at, res.StatusCode, body)
		}
		codes, next := page(t, body)
		got = append(got, codes...)
		at = next
	}

	want := make([]string, 0, held)
	for i := 1; i <= held; i++ {
		want = append(want, pageCode(i))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%d accounts over %d requests, want %d once each in code order",
			len(got), requests, held)
	}
	if requests != 7 {
		t.Errorf("%d requests for %d accounts at 40 a page, want 7", requests, held)
	}
}

// The last page says nothing about a page after it. A chart that divides
// exactly into pages is the sharp case: a full page can still be the last one,
// and a client that is sent on from it walks forever.
func TestTheLastPageCarriesNoNext(t *testing.T) {
	srv := serve(t, chartOf(2*defaultPage))

	_, body := get(t, srv.URL+"/v1/accounts?after="+pageCode(defaultPage))
	codes, next := page(t, body)
	if len(codes) != defaultPage {
		t.Errorf("%d accounts, want the %d left after %s", len(codes), defaultPage, pageCode(defaultPage))
	}
	if next != "" {
		t.Errorf("next = %q on a last page that is exactly full, want no next", next)
	}
}

// A page carries the question it was asked, or the second page answers a
// different one from the first.
func TestNextCarriesTheFilterAndTheSizeItWasAskedWith(t *testing.T) {
	srv := serve(t, chartOf(60))

	_, body := get(t, srv.URL+"/v1/accounts?currency=USD&limit=10")
	codes, next := page(t, body)
	if len(codes) != 10 {
		t.Fatalf("%d accounts, want a page of 10", len(codes))
	}

	asked, err := url.Parse(next)
	if err != nil {
		t.Fatalf("parse next %q: %v", next, err)
	}
	q := asked.Query()
	if q.Get("currency") != "USD" || q.Get("limit") != "10" || q.Get("after") != codes[9] {
		t.Fatalf("next = %q, want the currency, the size and the last code of this page", next)
	}

	_, body = get(t, srv.URL+next)
	codes, _ = page(t, body)
	for _, code := range codes {
		if got := currencyOf(t, srv.URL, code); got != "USD" {
			t.Errorf("%s on the second page holds %s, want the filter carried across", code, got)
		}
	}
}

// A code no account holds is an open set, so it is an empty answer and never a
// refusal. DESIGN.md 8.
func TestACodeNoAccountHoldsIsAnEmptyPage(t *testing.T) {
	srv := serve(t, chartOf(10))

	res, body := get(t, srv.URL+"/v1/accounts?after=zzz.nothing.holds.this")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}
	codes, next := page(t, body)
	if len(codes) != 0 || next != "" {
		t.Errorf("%d accounts and next %q, want an empty page", len(codes), next)
	}
}

// The refusal every other bound in this repo answers in: the value that was
// sent, and the ceiling it went past.
func TestAPageSizePastTheCeilingIsRefusedWithTheValueAndTheCeiling(t *testing.T) {
	srv := serve(t, chartOf(10))

	res, body := get(t, srv.URL+"/v1/accounts?limit="+strconv.Itoa(maxPage+1))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	got := read(t, body)
	if got.Parameter != "limit" {
		t.Errorf("parameter %q, want limit", got.Parameter)
	}
	if got.Value != strconv.Itoa(maxPage+1) {
		t.Errorf("value %q, want the %d that was sent", got.Value, maxPage+1)
	}
	if !strings.Contains(got.Expected, strconv.Itoa(maxPage)) {
		t.Errorf("expected %q, want it to name the ceiling of %d", got.Expected, maxPage)
	}
}

// Everything else a page size can be that this endpoint will not answer with.
func TestAPageSizeTheEndpointWillNotAnswerWithIsRefused(t *testing.T) {
	srv := serve(t, chartOf(10))

	for _, asked := range []string{"0", "-1", "fifty", "1.5", "1e3", " 10", "9223372036854775808"} {
		res, body := get(t, srv.URL+"/v1/accounts?limit="+url.QueryEscape(asked))
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("limit=%q: status %d, want 400: %s", asked, res.StatusCode, body)
			continue
		}
		if got := read(t, body); got.Value != asked || got.Expected == "" {
			t.Errorf("limit=%q: refusal = %+v, want the value it sent and the rule", asked, got)
		}
	}
}

// The ceiling is the largest page it answers with, not the first it refuses.
func TestTheLargestPageTheCeilingAllowsIsServed(t *testing.T) {
	srv := serve(t, chartOf(maxPage+1))

	res, body := get(t, srv.URL+"/v1/accounts?limit="+strconv.Itoa(maxPage))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}
	codes, next := page(t, body)
	if len(codes) != maxPage {
		t.Fatalf("%d accounts, want the %d the ceiling allows", len(codes), maxPage)
	}
	if next == "" {
		t.Errorf("next is empty with one account still to come, want the address of it")
	}
}

// One row per currency, and the schema holds a currency to three capitals, so
// the trial balance has a ceiling of its own and takes no page. DESIGN.md 23.
func TestTheTrialBalanceOverAPagedChartIsStillWholeAndBalanced(t *testing.T) {
	srv := serve(t, chartOf(defaultPage+150))

	_, body := get(t, srv.URL+"/v1/trial-balance")
	var got struct {
		Trial []trialRow `json:"trial"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if len(got.Trial) != 2 {
		t.Fatalf("%d rows, want one for each of GBP and USD", len(got.Trial))
	}
	for _, row := range got.Trial {
		if row.Accounts != 125 || !row.Balanced {
			t.Errorf("row = %+v, want 125 accounts and a book that balances", row)
		}
	}
}

// page reads the codes a list answered with and the address of the page after it.
func page(t *testing.T, body []byte) ([]string, string) {
	t.Helper()

	var got struct {
		Accounts []struct {
			Account string `json:"account"`
		} `json:"accounts"`
		Next string `json:"next"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	out := make([]string, 0, len(got.Accounts))
	for _, a := range got.Accounts {
		out = append(out, a.Account)
	}
	return out, got.Next
}

// currencyOf reads one account's currency back off its own endpoint.
func currencyOf(t *testing.T, base, code string) string {
	t.Helper()

	var got struct {
		Currency string `json:"currency"`
	}
	getInto(t, base+"/v1/accounts/"+code, http.StatusOK, &got)
	return got.Currency
}
