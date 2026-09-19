package ledgerhttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestDemoReadSurface(t *testing.T) {
	tx := testdb.Tx(t)
	seeded(t, tx)

	srv := httptest.NewServer(ledgerhttp.Handler(readerFunc(
		func(_ context.Context, f func(*sql.Tx) error) error { return f(tx) },
	), log.New(io.Discard, "", 0)))
	defer srv.Close()

	fmt.Println("\nCHART   four GBP accounts, one deposit of 45.00 less a 1.50 fee")

	exchange(t, srv, http.MethodGet, "/v1/accounts/assets.cash", http.StatusOK)
	exchange(t, srv, http.MethodGet, "/v1/accounts/assets.csah", http.StatusNotFound)
	exchange(t, srv, http.MethodPost, "/v1/accounts/assets.cash", http.StatusMethodNotAllowed)
	body := exchange(t, srv, http.MethodGet, "/v1/trial-balance", http.StatusOK)

	var got struct {
		Trial []trialRow `json:"trial"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal the trial balance: %v", err)
	}
	if len(got.Trial) != 1 {
		t.Fatalf("%d trial rows, want 1", len(got.Trial))
	}
	if row := got.Trial[0]; !row.Balanced || row.DebitsMinor != row.CreditsMinor {
		t.Errorf("%s does not balance: %+v", row.Currency, row)
	}
}

func TestDemoAccountList(t *testing.T) {
	tx := testdb.Tx(t)
	seededWithUSD(t, tx)

	srv := httptest.NewServer(ledgerhttp.Handler(readerFunc(
		func(_ context.Context, f func(*sql.Tx) error) error { return f(tx) },
	), log.New(io.Discard, "", 0)))
	defer srv.Close()

	fmt.Println("\nCHART   four GBP accounts, plus one USD account nothing has been posted to")

	all := exchange(t, srv, http.MethodGet, "/v1/accounts", http.StatusOK)
	exchange(t, srv, http.MethodGet, "/v1/accounts?kind=liabilty", http.StatusBadRequest)
	exchange(t, srv, http.MethodGet, "/v1/accounts?curency=GBP", http.StatusBadRequest)

	fmt.Println("\nAFTER   both filters at once, on the transaction the two refusals were answered on")
	one := exchange(t, srv, http.MethodGet, "/v1/accounts?currency=USD&kind=asset", http.StatusOK)

	if got, want := listed(t, all), []string{cash, cashUSD, ops, customer, fees}; !slices.Equal(got, want) {
		t.Errorf("the list is %v, want %v in code order", got, want)
	}
	if got, want := listed(t, one), []string{cashUSD}; !slices.Equal(got, want) {
		t.Errorf("the narrowed list is %v, want %v", got, want)
	}
}

func exchange(t *testing.T, srv *httptest.Server, method, path string, want int) []byte {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}

	line := "  " + res.Status + "   " + res.Header.Get("Content-Type")
	if allow := res.Header.Get("Allow"); allow != "" {
		line += "   Allow: " + allow
	}

	fmt.Printf("\n%s %s\n%s\n", method, path, line)
	for _, l := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		fmt.Println("  " + l)
	}

	if res.StatusCode != want {
		t.Errorf("%s %s answered %d, and the transcript above says %d", method, path, res.StatusCode, want)
	}
	return body
}
