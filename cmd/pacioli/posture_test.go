package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mgballou/pacioli/internal/testdb"
)

// The four connection deadlines. Zero is what each of them was, and to net/http
// zero is no deadline at all, so a test that only checked they were settable
// would pass on the state this fixed.
func TestTheServerCarriesEveryConnectionDeadline(t *testing.T) {
	d := deadlines{
		readHeader: 1 * time.Second,
		read:       2 * time.Second,
		request:    3 * time.Second,
		write:      4 * time.Second,
		idle:       5 * time.Second,
	}
	mounted := sentinel{http.NotFoundHandler()}
	srv := newServer(mounted, d, quiet())

	for _, c := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, d.readHeader},
		{"ReadTimeout", srv.ReadTimeout, d.read},
		{"WriteTimeout", srv.WriteTimeout, d.write},
		{"IdleTimeout", srv.IdleTimeout, d.idle},
	} {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
		if c.got == 0 {
			t.Errorf("%s is zero, which net/http reads as no deadline at all", c.name)
		}
	}

	// The request budget is not a field on the server; it is the handler, so
	// the handler that was passed in cannot be the one that is mounted.
	if _, bare := srv.Handler.(sentinel); bare {
		t.Error("the handler is mounted as it was given, so nothing puts the request budget on a request's context")
	}
}

// sentinel is a handler that can be recognised again after it is mounted.
type sentinel struct{ http.Handler }

// The budget has to bite on a real socket, not only in the struct: a handler
// waiting on the database is what the 503 exists for.
func TestARequestPastItsBudgetIsRefusedOverASocket(t *testing.T) {
	const (
		budget = 200 * time.Millisecond
		work   = 5 * time.Second
	)

	entered := make(chan context.Context, 1)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Context()
		select {
		case <-r.Context().Done():
		case <-time.After(work):
			w.WriteHeader(http.StatusOK)
		}
	})

	d := defaultDeadlines()
	d.request = budget

	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() { served <- serveOn(ctx, ln, slow, d, quiet()) }()

	started := time.Now()
	res, err := http.Get("http://" + ln.Addr().String() + "/v1/accounts")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	took := time.Since(started)

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503: %s", res.StatusCode, body)
	}
	if took >= work {
		t.Errorf("the answer took %s, want about the budget of %s", took, budget)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type %q, want application/json", ct)
	}

	// And the work behind it was cancelled, so its database connection went
	// back to a pool of sixteen rather than being held by a client that has
	// already been answered.
	select {
	case handlerCtx := <-entered:
		select {
		case <-handlerCtx.Done():
		case <-time.After(work):
			t.Error("the handler is still running after the client was refused")
		}
	default:
		t.Fatal("the handler was never entered")
	}

	cancel()
	if err := <-served; err != nil {
		t.Errorf("serveOn: %v", err)
	}
}

func TestEveryDeadlineIsSettableByFlagAndEnvironment(t *testing.T) {
	var probe deadlines
	for _, g := range graces(&probe) {
		t.Run(g.flag, func(t *testing.T) {
			byFlag, err := parseServe([]string{"-" + g.flag, "7s"}, io.Discard, none)
			if err != nil {
				t.Fatalf("parseServe -%s: %v", g.flag, err)
			}
			byEnv, err := parseServe(nil, io.Discard, one(g.env(), "11s"))
			if err != nil {
				t.Fatalf("parseServe $%s: %v", g.env(), err)
			}
			beats, err := parseServe([]string{"-" + g.flag, "7s"}, io.Discard, one(g.env(), "11s"))
			if err != nil {
				t.Fatalf("parseServe -%s with $%s: %v", g.flag, g.env(), err)
			}

			// The field this grace settles into, read back off each config.
			at := func(c config) time.Duration {
				d := c.deadlines
				for _, h := range graces(&d) {
					if h.flag == g.flag {
						return *h.into
					}
				}
				t.Fatalf("no grace named %s", g.flag)
				return 0
			}

			if got := at(byFlag); got != 7*time.Second {
				t.Errorf("-%s 7s gave %s", g.flag, got)
			}
			if got := at(byEnv); got != 11*time.Second {
				t.Errorf("$%s=11s gave %s", g.env(), got)
			}
			if got := at(beats); got != 7*time.Second {
				t.Errorf("-%s 7s with $%s=11s gave %s, want the flag to win", g.flag, g.env(), got)
			}
		})
	}
}

// The whole point of the flags is that a deadline exists, so the one value that
// would take it away is refused rather than kept.
func TestADeadlineOfZeroOrLessIsRefused(t *testing.T) {
	var probe deadlines
	for _, g := range graces(&probe) {
		if _, err := parseServe([]string{"-" + g.flag, "-1s"}, io.Discard, none); !errors.Is(err, errUsage) {
			t.Errorf("-%s -1s gave %v, want a usage error", g.flag, err)
		}
		for _, given := range []string{"0s", "-1s", "forever"} {
			if _, err := parseServe(nil, io.Discard, one(g.env(), given)); !errors.Is(err, errUsage) {
				t.Errorf("$%s=%s gave %v, want a usage error", g.env(), given, err)
			}
		}
	}
}

// A flag of 0 cannot be told from a flag that was not given, so it takes the
// default rather than becoming no deadline.
func TestADeadlineFlagOfZeroFallsBackToTheDefault(t *testing.T) {
	got, err := parseServe([]string{"-request-timeout", "0s"}, io.Discard, none)
	if err != nil {
		t.Fatalf("parseServe: %v", err)
	}
	if got.deadlines.request != requestGrace {
		t.Errorf("-request-timeout 0s gave %s, want the default %s", got.deadlines.request, requestGrace)
	}
}

// The database keeps its own ceilings, so a query the server has stopped
// waiting for stops running even if the cancellation never arrives.
func TestTheConnectionCarriesTheDatabaseCeilings(t *testing.T) {
	db, err := open(context.Background(), testdb.DSN())
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for name, want := range databaseGraces {
		var got string
		if err := db.QueryRow(`SELECT current_setting($1)`, name).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !sameDuration(t, got, want) {
			t.Errorf("%s = %q, want %s", name, got, want)
		}
		if got == "0" {
			t.Errorf("%s is 0, which Postgres reads as no ceiling at all", name)
		}
	}
}

// A deployer moves these on the connection string, so a connection string that
// names one keeps its own value.
func TestAConnectionStringKeepsItsOwnCeiling(t *testing.T) {
	dsn, err := url.Parse(testdb.DSN())
	if err != nil {
		t.Fatalf("parse %s: %v", testdb.DSN(), err)
	}
	q := dsn.Query()
	q.Set("statement_timeout", "3456ms")
	dsn.RawQuery = q.Encode()

	db, err := open(context.Background(), dsn.String())
	if err != nil {
		t.Fatalf("open with a statement_timeout of its own: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var got string
	if err := db.QueryRow(`SELECT current_setting('statement_timeout')`).Scan(&got); err != nil {
		t.Fatalf("read statement_timeout: %v", err)
	}
	if !sameDuration(t, got, "3456ms") {
		t.Errorf("statement_timeout = %q, want the connection string's 3456ms", got)
	}
}

// sameDuration compares what Postgres reports a timeout as against what was
// asked for; the server normalises "20s" to "20s" and "3456ms" to "3456ms", but
// the units it picks are its own.
func sameDuration(t *testing.T, got, want string) bool {
	t.Helper()

	parse := func(s string) time.Duration {
		if !strings.ContainsAny(s, "smh") {
			s += "ms" // a bare number is milliseconds to Postgres
		}
		d, err := time.ParseDuration(strings.ReplaceAll(s, " ", ""))
		if err != nil {
			t.Fatalf("read %q as a duration: %v", s, err)
		}
		return d
	}
	return parse(got) == parse(want)
}

// none and one stand in for the environment.
func none(string) string { return "" }

func one(name, value string) func(string) string {
	return func(k string) string {
		if k == name {
			return value
		}
		return ""
	}
}
