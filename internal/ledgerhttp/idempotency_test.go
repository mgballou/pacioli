package ledgerhttp_test

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/testdb"
)

const usedTwice = "the-key-a-client-retries-under"

func TestTheSameKeyTwiceIsAnsweredWithTheFirstResultAndWritesNothing(t *testing.T) {
	srv := serve(t, seeded)

	first, firstBody := postKeyed(t, srv.URL+"/v1/transactions", usedTwice, refund)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("the first status is %d, want 201: %s", first.StatusCode, firstBody)
	}
	if got := first.Header.Get(replayedHeader); got != "" {
		t.Errorf("the first answer carries %s: %q", replayedHeader, got)
	}
	after := balanceOver(t, srv, cash)

	second, secondBody := postKeyed(t, srv.URL+"/v1/transactions", usedTwice, refund)

	if second.StatusCode != http.StatusCreated {
		t.Fatalf("the second status is %d, want 201: %s", second.StatusCode, secondBody)
	}
	if got := second.Header.Get(replayedHeader); got != "true" {
		t.Errorf("%s = %q, want true — nothing says this was a replay", replayedHeader, got)
	}

	if string(secondBody) != string(firstBody) {
		t.Errorf("the replay is a different answer:\n%s\nwant:\n%s", secondBody, firstBody)
	}

	if got := balanceOver(t, srv, cash); got != after {
		t.Errorf("%s = %d after the retry, want %d — the refund was posted twice", cash, got, after)
	}
}

func TestARequestWithNoIdempotencyKeyIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := postWith(t, srv.URL+"/v1/transactions", refund, func(*http.Request) {})
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — the missing key was not refused at the door: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Parameter != keyHeader {
		t.Errorf("body = %+v, want the refusal to name %s", got, keyHeader)
	}
	if bal := balanceOver(t, srv, cash); bal != 4500 {
		t.Errorf("%s = %d, so the refused request posted anyway", cash, bal)
	}
}

func TestTheSameKeyWithADifferentRequestIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	if res, body := postKeyed(t, srv.URL+"/v1/transactions", usedTwice, refund); res.StatusCode != http.StatusCreated {
		t.Fatalf("the first status is %d, want 201: %s", res.StatusCode, body)
	}

	res, body := postKeyed(t, srv.URL+"/v1/transactions", usedTwice, `{
	  "currency": "GBP",
	  "description": "A different refund entirely",
	  "postings": [
	    {"account": "assets.cash", "amount_minor": -100},
	    {"account": "liabilities.customer", "amount_minor": 100}
	  ]
	}`)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter, Value string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Parameter != keyHeader || got.Value != usedTwice {
		t.Errorf("body = %+v, want the refusal to name the header and the key", got)
	}

	if bal := balanceOver(t, srv, cash); bal != 4000 {
		t.Errorf("%s = %d, want 4000 — one refund and no more", cash, bal)
	}
}

func TestReformattingTheSameRequestIsStillTheSameRequest(t *testing.T) {
	srv := serve(t, seeded)

	first, firstBody := postKeyed(t, srv.URL+"/v1/transactions", usedTwice, refund)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("the first status is %d, want 201: %s", first.StatusCode, firstBody)
	}

	reflowed := `{"postings":[{"amount_minor":-500,"account":"assets.cash"},` +
		`{"amount_minor":500,"account":"liabilities.customer"}],` +
		`"description":"Refund, in full","currency":"GBP"}`

	res, body := postKeyed(t, srv.URL+"/v1/transactions", usedTwice, reflowed)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201 — the same request reformatted read as a different one: %s",
			res.StatusCode, body)
	}
	if got := res.Header.Get(replayedHeader); got != "true" {
		t.Errorf("%s = %q, want true", replayedHeader, got)
	}
}

func TestAKeyTheLedgerWillNotHoldIsRefused(t *testing.T) {
	srv := serve(t, seeded)

	res, body := postKeyed(t, srv.URL+"/v1/transactions", "1", refund)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", res.StatusCode, body)
	}

	var got struct{ Error, Parameter, Value string }
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if got.Parameter != keyHeader || got.Value != "1" {
		t.Errorf("body = %+v, want the refusal to name the header and the key", got)
	}
	if bal := balanceOver(t, srv, cash); bal != 4500 {
		t.Errorf("%s = %d, so the refused request posted anyway", cash, bal)
	}
}

func TestARefusedEntryDoesNotBurnItsKey(t *testing.T) {
	srv := serve(t, seeded)

	res, body := postKeyed(t, srv.URL+"/v1/transactions", usedTwice, `{
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

	res, body = postKeyed(t, srv.URL+"/v1/transactions", usedTwice, refund)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201 — the refusal burned the key: %s", res.StatusCode, body)
	}
	if got := res.Header.Get(replayedHeader); got != "" {
		t.Errorf("the corrected entry came back as a replay (%s: %q)", replayedHeader, got)
	}
	if bal := balanceOver(t, srv, cash); bal != 4000 {
		t.Errorf("%s = %d, want 4000", cash, bal)
	}
}

const raceRequests = 16

func TestManyRequestsWithOneKeyAtOnceWriteOnce(t *testing.T) {
	db := testdb.Disposable(t, "ledger_race_test")
	seedOverPool(t, db)

	srv := httptest.NewServer(ledgerhttp.Handler(ledgerhttp.Pool{DB: db}, log.New(io.Discard, "", 0)))
	t.Cleanup(srv.Close)

	answers := make([]answer, raceRequests)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range raceRequests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			answers[i] = send(srv.URL+"/v1/transactions", usedTwice, deposit)
		}()
	}
	close(start)
	wg.Wait()

	created, replayed := 0, 0
	for i, a := range answers {
		if a.err != nil {
			t.Fatalf("request %d: %v", i, a.err)
		}
		if a.status != http.StatusCreated {
			t.Fatalf("request %d answered %d, want 201: %s", i, a.status, a.body)
		}
		if a.replayed {
			replayed++
		} else {
			created++
		}
		if a.body != answers[0].body {
			t.Errorf("request %d got a different answer from request 0:\n%s\nwant:\n%s", i, a.body, answers[0].body)
		}
	}
	if created != 1 || replayed != raceRequests-1 {
		t.Errorf("%d created and %d replayed, want 1 and %d — more than one of them wrote",
			created, replayed, raceRequests-1)
	}
	t.Logf("%d requests, one key, released together: %d created, %d replayed", raceRequests, created, replayed)

	transactions := count(t, db, `SELECT count(*) FROM transactions`)
	postings := count(t, db, `SELECT count(*) FROM postings`)
	if transactions != 1 || postings != 2 {
		t.Errorf("the ledger holds %d transactions over %d postings, want 1 over 2", transactions, postings)
	}
	t.Logf("the ledger holds %d transaction over %d postings", transactions, postings)

	moved := balanceOver(t, srv, cash)
	if moved != 4500 {
		t.Errorf("%s = %d, want 4500 — one deposit, not %d", cash, moved, raceRequests)
	}
	t.Logf("%s = %d, which is one deposit and not %d", cash, moved, raceRequests)
}

const deposit = `{
  "currency": "GBP",
  "description": "Customer deposit",
  "postings": [
    {"account": "assets.cash", "amount_minor": 4500},
    {"account": "liabilities.customer", "amount_minor": -4500}
  ]
}`

func send(url, key, body string) answer {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return answer{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(keyHeader, key)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{err: err}
	}
	defer res.Body.Close()

	got, err := io.ReadAll(res.Body)
	if err != nil {
		return answer{err: err}
	}
	return answer{
		status:   res.StatusCode,
		replayed: res.Header.Get(replayedHeader) == "true",
		body:     string(got),
	}
}

type answer struct {
	status   int
	replayed bool
	body     string
	err      error
}

// seedOverPool commits the chart, because each request under test runs on its own transaction and cannot see one still open.
func seedOverPool(t *testing.T, db *sql.DB) {
	t.Helper()

	if _, err := db.Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES
		   ($1, 'Cash at bank',      'asset',     'GBP'),
		   ($2, 'Customer balances', 'liability', 'GBP')`,
		cash, customer,
	); err != nil {
		t.Fatalf("seed the chart: %v", err)
	}
}

func count(t *testing.T, db *sql.DB, query string) int64 {
	t.Helper()

	var n int64
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}
