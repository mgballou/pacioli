package ledgerhttp_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/testdb"
)

// The three writes below all reach the same refusal, so they name the one thing
// each of them gets wrong.
// jsonType is what the writes take, spelled out here rather than reached for
// out of the package under test.
const jsonType = "application/json"

var notJSON = []struct {
	name string
	kind string
}{
	{"nothing at all, which is what a cross-origin fetch sends by default", ""},
	{"a form, which is what a cross-origin form sends", "application/x-www-form-urlencoded"},
	{"text, which is the widest a simple request may declare", "text/plain;charset=UTF-8"},
}

func TestOpeningAnAccountNeedsAJSONContentType(t *testing.T) {
	srv := serve(t, empty)

	for _, c := range notJSON {
		t.Run(c.name, func(t *testing.T) {
			res, body := postTyped(t, srv.URL+"/v1/accounts", c.kind,
				`{"code":"assets.smuggled","name":"Smuggled","kind":"asset","currency":"GBP"}`)
			if res.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("status %d, want 415: %s", res.StatusCode, body)
			}
			if !strings.Contains(string(body), "application/json") {
				t.Errorf("the refusal does not name what it wanted: %s", body)
			}
		})
	}

	// And the chart is untouched, so every refusal came before the write.
	var list struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	_, body := get(t, srv.URL+"/v1/accounts")
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if len(list.Accounts) != 0 {
		t.Errorf("the chart holds %d accounts after three refusals, want 0", len(list.Accounts))
	}
}

func TestPostingATransactionNeedsAJSONContentType(t *testing.T) {
	srv := serve(t, seeded)

	for _, c := range notJSON {
		t.Run(c.name, func(t *testing.T) {
			res, body := postTyped(t, srv.URL+"/v1/transactions", c.kind,
				`{"currency":"GBP","description":"smuggled","postings":[]}`)
			if res.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("status %d, want 415: %s", res.StatusCode, body)
			}
		})
	}
}

// The content type is refused before the idempotency key is looked for, so the
// media type is the control on this endpoint and the key is not.
func TestTheContentTypeIsRefusedBeforeTheIdempotencyKey(t *testing.T) {
	srv := serve(t, seeded)

	res, body := postTyped(t, srv.URL+"/v1/transactions", "text/plain", `{}`)
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status %d, want 415 rather than the 400 a missing key gives: %s", res.StatusCode, body)
	}
}

// A preflight is what a browser has to send once the write is not a simple
// request, and nothing here answers one.
func TestAPreflightIsRefused(t *testing.T) {
	srv := serve(t, empty)

	for _, path := range []string{"/v1/accounts", "/v1/transactions"} {
		req, err := http.NewRequest(http.MethodOptions, srv.URL+path, nil)
		if err != nil {
			t.Fatalf("build the preflight for %s: %v", path, err)
		}
		req.Header.Set("Origin", "https://elsewhere.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "content-type")

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("preflight %s: %v", path, err)
		}
		res.Body.Close()

		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("OPTIONS %s gave %d, want 405", path, res.StatusCode)
		}
		if allow := res.Header.Get("Access-Control-Allow-Origin"); allow != "" {
			t.Errorf("OPTIONS %s allows the origin %q; nothing should", path, allow)
		}
	}
}

func TestARequestPastItsDeadlineIsRefusedAsJSON(t *testing.T) {
	const (
		budget = 100 * time.Millisecond
		// Longer than the budget by enough that a deadline which does not fire
		// is a failure and not a hang.
		work = 3 * time.Second
	)

	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(work):
			w.WriteHeader(http.StatusOK)
		}
	})

	srv := httptest.NewServer(ledgerhttp.Deadline(budget, slow))
	t.Cleanup(srv.Close)

	started := time.Now()
	res, body := get(t, srv.URL+"/v1/accounts")
	took := time.Since(started)

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type %q, want application/json — a refusal is answered in the same format as an answer", ct)
	}

	var refusal struct {
		Error    string `json:"error"`
		Expected string `json:"expected"`
		See      string `json:"see"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	if refusal.Error == "" || refusal.See == "" {
		t.Errorf("the refusal says %+v, want a reason and somewhere to read the rule", refusal)
	}
	if !strings.Contains(refusal.Expected, budget.String()) {
		t.Errorf("expected = %q, want the budget %s in it", refusal.Expected, budget)
	}
	if took >= work {
		t.Errorf("the refusal took %s, want about the budget of %s", took, budget)
	}
}

// The budget is on the request's context, not only on the socket: a handler
// waiting on the database has to be cancelled, or it goes on holding one
// connection out of a pool of sixteen after the client has been answered.
func TestADeadlineCancelsTheRequestContext(t *testing.T) {
	cancelled := make(chan error, 1)
	slow := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			cancelled <- r.Context().Err()
		case <-time.After(3 * time.Second):
			cancelled <- nil
		}
	})

	srv := httptest.NewServer(ledgerhttp.Deadline(50*time.Millisecond, slow))
	t.Cleanup(srv.Close)

	get(t, srv.URL+"/v1/accounts")

	select {
	case err := <-cancelled:
		if err == nil {
			t.Error("the handler's context ended with no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler is still running after the client was answered; the deadline closed the socket and left the work behind it")
	}
}

func TestAnAnswerInsideItsDeadlineIsUntouched(t *testing.T) {
	quick := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Set-By-The-Handler", "yes")
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, `{"ok":true}`)
	})

	srv := httptest.NewServer(ledgerhttp.Deadline(10*time.Second, quick))
	t.Cleanup(srv.Close)

	res, body := get(t, srv.URL+"/anything")
	if res.StatusCode != http.StatusTeapot {
		t.Errorf("status %d, want 418 — the deadline changed an answer it should have passed through", res.StatusCode)
	}
	if res.Header.Get("X-Set-By-The-Handler") != "yes" {
		t.Error("the header the handler set did not survive the deadline")
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body %q, want the handler's own", body)
	}
}

// A request that never got as far as being refused is not an internal error. Deadline usually
// answers it first and this body is thrown away, but a handler that finishes a
// moment before its own budget hands it to the client, and under four hundred
// concurrent writes that happened.
func TestARequestWhoseContextEndedIsNotA500(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{
		{"the budget ran out", context.DeadlineExceeded},
		{"the client went away", context.Canceled},
		{"the pooled connection a cancellation left unusable", driver.ErrBadConn},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			h := ledgerhttp.Handler(storeFunc(func(context.Context, func(*sql.Tx) error) error {
				return fmt.Errorf("begin the write: %w", c.err)
			}), log.New(&logged, "", 0))
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			for _, call := range []struct {
				what string
				do   func() (*http.Response, []byte)
			}{
				{"GET /v1/accounts", func() (*http.Response, []byte) { return get(t, srv.URL+"/v1/accounts") }},
				{"POST /v1/accounts", func() (*http.Response, []byte) {
					return postTyped(t, srv.URL+"/v1/accounts", jsonType,
						`{"code":"assets.cash","name":"Cash","kind":"asset","currency":"GBP"}`)
				}},
				{"POST /v1/transactions", func() (*http.Response, []byte) {
					return postTyped(t, srv.URL+"/v1/transactions", jsonType,
						`{"currency":"GBP","description":"x","postings":[]}`)
				}},
			} {
				res, body := call.do()
				if res.StatusCode != http.StatusServiceUnavailable {
					t.Errorf("%s gave %d, want 503: %s", call.what, res.StatusCode, body)
				}
			}
			if logged.Len() != 0 {
				t.Errorf("a cancelled request was logged as a cause behind a 500: %s", logged.String())
			}
		})
	}
}

// The isolation level a read runs at is what makes Pool.Read's one-snapshot
// claim true, and nothing else does.
func TestPoolLendsAReadAtRepeatableRead(t *testing.T) {
	if got := isolationOf(t, ledgerhttp.Pool{DB: testdb.Open(t)}.Read); got != "repeatable read" {
		t.Errorf("a read runs at %q, want repeatable read", got)
	}
}

// A write runs at read committed, and says so: the idempotency replay reads a
// row another transaction committed after this one began, which a snapshot
// taken once for the whole transaction cannot see.
func TestPoolLendsAWriteAtReadCommitted(t *testing.T) {
	if got := isolationOf(t, ledgerhttp.Pool{DB: testdb.Open(t)}.Write); got != "read committed" {
		t.Errorf("a write runs at %q, want read committed", got)
	}
}

// isolationOf asks the server what level the transaction lend gave it.
func isolationOf(t *testing.T, lend func(context.Context, func(*sql.Tx) error) error) string {
	t.Helper()

	var level string
	if err := lend(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT current_setting('transaction_isolation')`).Scan(&level)
	}); err != nil {
		t.Fatalf("read the isolation level: %v", err)
	}
	return level
}

// postTyped sends a body under a content type the caller chose, and sends the
// idempotency key too, so the only thing wrong with the request is the type.
func postTyped(t *testing.T, url, contentType, body string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the request for %s: %v", url, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else {
		// Go fills this in unless it is asked not to.
		req.Header["Content-Type"] = nil
	}
	req.Header.Set("Idempotency-Key", "posture-aaaaaaaaaaaaaaaaaaaa")

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
