package ledgerhttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/testdb"
)

const refund = `{
  "currency": "GBP",
  "description": "Refund, in full",
  "postings": [
    {"account": "assets.cash", "amount_minor": -500},
    {"account": "liabilities.customer", "amount_minor": 500}
  ]
}`

func TestABalancedTransactionIsAcceptedAndTheBalancesMove(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", refund)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type %q, want application/json", ct)
	}

	var got struct {
		Transaction string `json:"transaction"`
		Currency    string `json:"currency"`
		Postings    []struct {
			Account     string `json:"account"`
			AmountMinor int64  `json:"amount_minor"`
		} `json:"postings"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Transaction == "" {
		t.Error("the answer carries no transaction id")
	}
	if got.Currency != "GBP" || len(got.Postings) != 2 {
		t.Errorf("body = %+v, want the GBP entry that was posted", got)
	}

	if bal := balanceOver(t, srv, cash); bal != 4000 {
		t.Errorf("%s = %d after the refund, want 4000", cash, bal)
	}
	if bal := balanceOver(t, srv, customer); bal != -3850 {
		t.Errorf("%s = %d after the refund, want -3850", customer, bal)
	}
}

func TestAnUnbalancedTransactionIsRefusedAndNothingMoves(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "description": "Refund, mistyped",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": -500},
	    {"account": "liabilities.customer", "amount_minor": 5000}
	  ]
	}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct {
		Error    string `json:"error"`
		NetMinor int64  `json:"net_minor"`
		Postings int    `json:"postings"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "the transaction does not balance" || got.NetMinor != 4500 || got.Postings != 2 {
		t.Errorf("body = %+v, want the refusal, 4500 out over 2 postings", got)
	}
	if strings.Contains(string(body), "SQLSTATE") || strings.Contains(string(body), "ERROR:") {
		t.Errorf("body = %s, and it hands the client Postgres's own words", body)
	}

	if bal := balanceOver(t, srv, cash); bal != 4500 {
		t.Errorf("%s = %d after the refusal, want 4500 — the refused entry left a leg behind", cash, bal)
	}
}

func TestTheNetInTheRefusalIsTheNetTheServerRefused(t *testing.T) {
	srv, tx := serveTx(t, seeded)

	_, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "description": "Refund, mistyped",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": -500},
	    {"account": "liabilities.customer", "amount_minor": 5000}
	  ]
	}`)
	var got struct {
		NetMinor int64 `json:"net_minor"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}

	_, err := ledger.Post(context.Background(), tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Refund, mistyped",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: -500},
			{Account: customer, AmountMinor: 5000},
		},
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("post gave %v, want a refusal from the server", err)
	}
	if want := fmt.Sprintf("net to %d", got.NetMinor); !strings.Contains(pgErr.Message, want) {
		t.Errorf("the answer said %d out; the server said %q", got.NetMinor, pgErr.Message)
	}
}

func TestATransactionWithNoPostingsIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions",
		`{"currency": "GBP", "description": "Nothing at all", "postings": []}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct {
		Error    string `json:"error"`
		NetMinor int64  `json:"net_minor"`
		Postings int    `json:"postings"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Postings != 0 || got.NetMinor != 0 {
		t.Errorf("body = %+v, want nothing over no postings", got)
	}
}

func TestALegNamingAnUnknownAccountIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "description": "Refund to an account nobody opened",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": -500},
	    {"account": "liabilities.csutomer", "amount_minor": 500}
	  ]
	}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct{ Error, Code, Parameter string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "no such account" || got.Code != "liabilities.csutomer" || got.Parameter != "postings[1]" {
		t.Errorf("body = %+v, want the refusal, the code, and which leg named it", got)
	}

	res, _ = get(t, srv.URL+"/v1/accounts/liabilities.csutomer")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("the account answers %d, so the refusal opened it", res.StatusCode)
	}
}

func TestALegInAnotherCurrencyIsRefused(t *testing.T) {
	srv := serve(t, seededWithUSD)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "description": "Sterling into a dollar account",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": -500},
	    {"account": "assets.cash_usd", "amount_minor": 500}
	  ]
	}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct{ Error, Code, Parameter, Value string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Code != cashUSD || got.Parameter != "postings[1]" || got.Value != "GBP" {
		t.Errorf("body = %+v, want the leg, its account and the currency it was asked to hold", got)
	}
}

func TestALegThatMovesNothingIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "description": "A leg that moves nothing",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": 0},
	    {"account": "liabilities.customer", "amount_minor": 0}
	  ]
	}`)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "the ledger refused the transaction" || got.Parameter != "postings[0]" {
		t.Errorf("body = %+v, want the refusal and the leg that caused it", got)
	}
}

func TestABodyThatIsNotJSONIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `not json at all`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}
}

func TestAFieldThisEndpointDoesNotDefineIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "descriptoin": "Refund, in full",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": -500},
	    {"account": "liabilities.customer", "amount_minor": 500}
	  ]
	}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — the misspelt field was accepted: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "no such field" || got.Parameter != "descriptoin" {
		t.Errorf("body = %+v, want the refusal and the field that caused it", got)
	}
}

func TestAFieldThisEndpointDoesNotDefineInsideAPostingIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "description": "Refund, in full",
	  "postings": [{"account": "assets.cash", "amount": -500}]
	}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	var got struct{ Parameter string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Parameter != "amount" {
		t.Errorf("the refusal named %q, want amount", got.Parameter)
	}
}

func TestAFieldOfTheWrongTypeIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", `{
	  "currency": "GBP",
	  "description": "Refund, in full",
	  "postings": [{"account": "assets.cash", "amount_minor": "-500"}]
	}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter, Value string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Parameter != "postings.amount_minor" || got.Value != "string" {
		t.Errorf("body = %+v, want the field and what it held", got)
	}
}

func TestASecondJSONValueInOneBodyIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", refund+refund)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — the second entry was thrown away silently: %s", res.StatusCode, body)
	}
	if bal := balanceOver(t, srv, cash); bal != 4500 {
		t.Errorf("%s = %d, so the first of the two was posted anyway", cash, bal)
	}
}

func TestABodyLargerThanTheEndpointAcceptsIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	huge := `{"currency": "GBP", "description": "` + strings.Repeat("x", 1<<20) + `"}`
	res, body := post(t, srv.URL+"/v1/transactions", huge)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413: %s", res.StatusCode, body)
	}
}

func TestAReadMethodOnTransactionsIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, _ := get(t, srv.URL+"/v1/transactions")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", res.StatusCode)
	}
	if allow := res.Header.Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("Allow: %q, want it to name POST", allow)
	}
}

func TestAFaultOnTheWritePathIsA500(t *testing.T) {
	boom := errors.New("the database fell over")
	var logged strings.Builder

	h := ledgerhttp.Handler(storeFunc(func(context.Context, func(*sql.Tx) error) error {
		return boom
	}), log.New(&logged, "", 0))
	srv := httptest.NewServer(h)
	defer srv.Close()

	res, body := post(t, srv.URL+"/v1/transactions", refund)
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

func TestPoolCommitsAWriteThatSucceeded(t *testing.T) {
	p := ledgerhttp.Pool{DB: testdb.Open(t)}

	xid := ""
	if err := p.Write(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT pg_current_xact_id()::text`).Scan(&xid)
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := xactStatus(t, xid); got != "committed" {
		t.Errorf("the transaction Pool lent is %q, want committed — a write that answers 201 and is not there", got)
	}
}

func TestPoolRollsBackAWriteThatFailed(t *testing.T) {
	p := ledgerhttp.Pool{DB: testdb.Open(t)}

	refused := errors.New("the ledger refused it")
	xid := ""
	err := p.Write(context.Background(), func(tx *sql.Tx) error {
		if err := tx.QueryRow(`SELECT pg_current_xact_id()::text`).Scan(&xid); err != nil {
			return err
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("write gave %v, want the error the work returned", err)
	}
	if got := xactStatus(t, xid); got != "aborted" {
		t.Errorf("the transaction Pool lent is %q, want aborted — a refused entry that is in the ledger anyway", got)
	}
}

func xactStatus(t *testing.T, xid string) string {
	t.Helper()

	var status string
	if err := testdb.Open(t).QueryRow(`SELECT pg_xact_status($1::xid8)`, xid).Scan(&status); err != nil {
		t.Fatalf("pg_xact_status(%s): %v", xid, err)
	}
	return status
}

// The write used to hand back an id and nothing would resolve it: a caller
// stored the id, came back, and found there was no endpoint to come back to.
// These four say the id is an address.

func TestAPostedTransactionIsServedBackUnderTheIdItWasGiven(t *testing.T) {
	srv := serve(t, seeded)

	res, body := post(t, srv.URL+"/v1/transactions", refund)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", res.StatusCode, body)
	}
	written := entry(t, body)
	if got, want := res.Header.Get("Location"), "/v1/transactions/"+written.Transaction; got != want {
		t.Errorf("Location %q, want %q", got, want)
	}

	res, body = get(t, srv.URL+"/v1/transactions/"+written.Transaction)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}

	read := entry(t, body)
	if read.Transaction != written.Transaction || read.Currency != "GBP" || read.Description != "Refund, in full" {
		t.Errorf("read back %+v, want the entry that was written", read)
	}
	if len(read.Postings) != 2 {
		t.Fatalf("read back %d postings, want 2: %s", len(read.Postings), body)
	}
	// Posting order, which is the order they were written in.
	if read.Postings[0].Account != cash || read.Postings[0].AmountMinor != -500 {
		t.Errorf("first leg = %+v, want %s at -500", read.Postings[0], cash)
	}
	if read.Postings[1].Account != customer || read.Postings[1].AmountMinor != 500 {
		t.Errorf("second leg = %+v, want %s at 500", read.Postings[1], customer)
	}
}

// Postgres takes a uuid in more spellings than it writes one in, so the answer
// carries the ledger's id rather than the caller's version of it.
func TestAnIdInAnotherSpellingReadsBackAsTheLedgerHoldsIt(t *testing.T) {
	srv := serve(t, seeded)

	_, body := post(t, srv.URL+"/v1/transactions", refund)
	written := entry(t, body)

	res, body := get(t, srv.URL+"/v1/transactions/"+strings.ToUpper(written.Transaction))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", res.StatusCode, body)
	}
	if got := entry(t, body).Transaction; got != written.Transaction {
		t.Errorf("read back %q, want the id the ledger holds, %q", got, written.Transaction)
	}
}

func TestAnUnknownTransactionIs404(t *testing.T) {
	srv := serve(t, seeded)

	const absent = "00000000-0000-0000-0000-000000000000"
	res, body := get(t, srv.URL+"/v1/transactions/"+absent)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter, Value string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Error != "no such transaction" || got.Parameter != "id" || got.Value != absent {
		t.Errorf("body = %+v, want the refusal and the id that was asked for", got)
	}
}

// 400 and not 404: the path segment could not name a transaction, so nothing was
// asked of the ledger. DESIGN.md 10.
func TestAnIdThatIsNotAUuidIs400AndSaysTheShapeAnIdTakes(t *testing.T) {
	srv := serve(t, seeded)

	res, body := get(t, srv.URL+"/v1/transactions/not-a-uuid")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter, Value, Expected, See string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Value != "not-a-uuid" || got.Expected != ledger.IDShape || got.See == "" {
		t.Errorf("body = %+v, want the id that was sent and the shape an id takes", got)
	}
	if strings.Contains(string(body), "SQLSTATE") || strings.Contains(string(body), "uuid_in") {
		t.Errorf("body = %s, and it hands the client Postgres's own words", body)
	}
}

// A write method on the new path answers 405 and not 404, so a client that
// mistakes it for the write is told which method it wanted.
func TestAWriteToOneTransactionIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, err := http.Post(srv.URL+"/v1/transactions/"+strings.Repeat("0", 8)+"-0000-0000-0000-000000000000",
		"application/json", strings.NewReader("{}"))
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

// entry reads a transaction body, which is what both the write and the read of
// one transaction answer with.
func entry(t *testing.T, body []byte) transactionBody {
	t.Helper()

	var got transactionBody
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return got
}

type transactionBody struct {
	Transaction string `json:"transaction"`
	Currency    string `json:"currency"`
	Description string `json:"description"`
	Postings    []struct {
		Account     string `json:"account"`
		AmountMinor int64  `json:"amount_minor"`
	} `json:"postings"`
}

// post mints a key prefixed with this package, so its seeds and internal/ledger's cannot wait on each other's open reservation.
func post(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()

	return postKeyed(t, url, mintKey(t), body)
}

func postKeyed(t *testing.T, url, key, body string) (*http.Response, []byte) {
	t.Helper()

	return postWith(t, url, body, func(r *http.Request) { r.Header.Set(keyHeader, key) })
}

func postWith(t *testing.T, url, body string, with func(*http.Request)) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the request for %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	with(req)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer res.Body.Close()

	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return res, got
}

const (
	keyHeader      = "Idempotency-Key"
	replayedHeader = "Idempotent-Replayed"
)

var keysMinted atomic.Int64

func mintKey(t *testing.T) string {
	t.Helper()

	return fmt.Sprintf("ledgerhttp-%s-%d", t.Name(), keysMinted.Add(1))
}

func balanceOver(t *testing.T, srv *httptest.Server, code string) int64 {
	t.Helper()

	res, body := get(t, srv.URL+"/v1/accounts/"+code)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s gave %d: %s", code, res.StatusCode, body)
	}
	var got struct {
		BalanceMinor int64 `json:"balance_minor"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return got.BalanceMinor
}
