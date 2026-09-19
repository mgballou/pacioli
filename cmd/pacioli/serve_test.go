package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/testdb"
)

const (
	cash     = "assets.cash"
	customer = "liabilities.customer"
)

func TestServeFinishesAnInFlightRequestAfterTheSignal(t *testing.T) {
	ln := listen(t)

	entered, released := make(chan struct{}), make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-released
		fmt.Fprint(w, "finished")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serveOn(ctx, ln, handler, quiet()) }()

	type answer struct {
		body string
		err  error
	}
	answers := make(chan answer, 1)
	go func() {
		res, err := http.Get("http://" + ln.Addr().String() + "/anything")
		if err != nil {
			answers <- answer{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		answers <- answer{body: string(body), err: err}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was never reached")
	}

	cancel()
	waitUntilRefused(t, ln.Addr().String())
	close(released)

	select {
	case got := <-answers:
		if got.err != nil {
			t.Fatalf("the request that was already in flight did not finish: %v", got.err)
		}
		if got.body != "finished" {
			t.Errorf("the in-flight request answered %q, want %q", got.body, "finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request that was already in flight never came back")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serveOn returned %v, want nil — a drain that reports a failure exits on top of the request it was draining", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn never returned after the signal")
	}
}

func TestServeMountsTheLedgerReadSurface(t *testing.T) {
	tx := testdb.Tx(t)
	seed(t, tx)

	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- serveOn(ctx, ln, ledgerhttp.Handler(txReader{tx}, quiet()), quiet())
	}()

	base := "http://" + ln.Addr().String()

	var one struct {
		Account      string `json:"account"`
		BalanceMinor int64  `json:"balance_minor"`
	}
	getJSON(t, base+"/v1/accounts/"+cash, http.StatusOK, &one)
	if one.Account != cash || one.BalanceMinor != 4500 {
		t.Errorf("GET /v1/accounts/%s gave %+v, want %s at 4500", cash, one, cash)
	}

	var list struct {
		Accounts []struct {
			Account string `json:"account"`
		} `json:"accounts"`
	}
	getJSON(t, base+"/v1/accounts", http.StatusOK, &list)
	if len(list.Accounts) != 2 {
		t.Errorf("GET /v1/accounts returned %d accounts, want 2", len(list.Accounts))
	}

	var trial struct {
		Trial []struct {
			Currency string `json:"currency"`
			Balanced bool   `json:"balanced"`
		} `json:"trial"`
	}
	getJSON(t, base+"/v1/trial-balance", http.StatusOK, &trial)
	if len(trial.Trial) != 1 || trial.Trial[0].Currency != "GBP" || !trial.Trial[0].Balanced {
		t.Errorf("GET /v1/trial-balance gave %+v, want one balanced GBP row", trial.Trial)
	}

	var refusal struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	getJSON(t, base+"/v1/accounts/assets.csah", http.StatusNotFound, &refusal)
	if refusal.Error != "no such account" || refusal.Code != "assets.csah" {
		t.Errorf("the refusal was %+v, want the ledger's own", refusal)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serveOn returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn never returned after the signal")
	}
}

func TestTheListenAddressComesFromTheFlagThenTheEnvironment(t *testing.T) {
	env := func(pairs map[string]string) func(string) string {
		return func(k string) string { return pairs[k] }
	}

	for _, c := range []struct {
		name string
		args []string
		env  map[string]string
		want config
	}{
		{
			name: "neither: the defaults, and they are loopback and compose",
			want: config{addr: defaultAddr, dsn: defaultDSN},
		},
		{
			name: "the environment alone",
			env:  map[string]string{addrEnv: "127.0.0.1:9999", dsnEnv: "postgres://elsewhere/x_test"},
			want: config{addr: "127.0.0.1:9999", dsn: "postgres://elsewhere/x_test"},
		},
		{
			name: "the flag beats the environment",
			args: []string{"-addr", "127.0.0.1:1234", "-dsn", "postgres://flagged/x_test"},
			env:  map[string]string{addrEnv: "127.0.0.1:9999", dsnEnv: "postgres://elsewhere/x_test"},
			want: config{addr: "127.0.0.1:1234", dsn: "postgres://flagged/x_test"},
		},
		{
			name: "one flag does not take the other's environment away",
			args: []string{"-addr", "127.0.0.1:1234"},
			env:  map[string]string{dsnEnv: "postgres://elsewhere/x_test"},
			want: config{addr: "127.0.0.1:1234", dsn: "postgres://elsewhere/x_test"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseServe(c.args, io.Discard, env(c.env))
			if err != nil {
				t.Fatalf("parseServe: %v", err)
			}
			if got != c.want {
				t.Errorf("parseServe gave %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestServeRefusesArgumentsItDoesNotUnderstand(t *testing.T) {
	for _, args := range [][]string{
		{"-adr", "127.0.0.1:1"},
		{"127.0.0.1:1"},
	} {
		if _, err := parseServe(args, io.Discard, func(string) string { return "" }); !errors.Is(err, errUsage) {
			t.Errorf("parseServe(%q) gave %v, want a usage error", args, err)
		}
	}
}

func TestAnUnknownCommandIsARefusalAndNotAVersion(t *testing.T) {
	var stdout, stderr strings.Builder
	err := run(context.Background(), []string{"srve"}, &stdout, &stderr, func(string) string { return "" })
	if !errors.Is(err, errUsage) {
		t.Errorf("run gave %v, want a usage error", err)
	}
	if stdout.String() != "" {
		t.Errorf("run wrote %q to stdout, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "pacioli serve") {
		t.Errorf("the usage on stderr does not mention serve:\n%s", stderr.String())
	}
}

func TestTheServerDefaultsToTheDatabaseTheTestsUse(t *testing.T) {
	if defaultDSN != testdb.DefaultDSN {
		t.Errorf("the binary defaults to\n  %s\nand the tests use\n  %s", defaultDSN, testdb.DefaultDSN)
	}
}

// txReader never commits: postings are append-only, so a committed test row would move every other test's counts.
type txReader struct{ tx *sql.Tx }

func (r txReader) Read(_ context.Context, f func(*sql.Tx) error) error { return f(r.tx) }

func seed(t *testing.T, tx *sql.Tx) {
	t.Helper()

	if _, err := tx.Exec(
		`INSERT INTO accounts (code, name, kind, currency) VALUES
		   ($1, 'Cash at bank',      'asset',     'GBP'),
		   ($2, 'Customer balances', 'liability', 'GBP')`,
		cash, customer,
	); err != nil {
		t.Fatalf("seed the chart: %v", err)
	}

	if _, err := ledger.Post(context.Background(), tx, ledger.Entry{
		Currency:    "GBP",
		Description: "Customer deposit",
		Legs: []ledger.Leg{
			{Account: cash, AmountMinor: 4500},
			{Account: customer, AmountMinor: -4500},
		},
	}); err != nil {
		t.Fatalf("post the deposit: %v", err)
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// quiet discards rather than writing to t.Log, because serveOn logs from a goroutine that can outlive the test.
func quiet() *log.Logger { return log.New(io.Discard, "", 0) }

func waitUntilRefused(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the listener was still accepting connections five seconds after the signal")
}

func getJSON(t *testing.T, url string, wantStatus int, into any) {
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
	if res.StatusCode != wantStatus {
		t.Fatalf("GET %s gave %d, want %d\n%s", url, res.StatusCode, wantStatus, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("GET %s answered as %q, want application/json — is the ledger's surface mounted?", url, ct)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("GET %s returned %v\n%s", url, err, body)
	}
}
