package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/schema"
)

const (
	// Loopback, so the binary does not put the ledger on the network. The
	// container listens on 0.0.0.0, and the Dockerfile says so where it sets it.
	defaultAddr = "127.0.0.1:8080"

	// defaultDSN is compose.test.yaml's, written out because internal/testdb
	// imports "testing". A test holds the two against each other.
	defaultDSN = "postgres://ledger:ledger@127.0.0.1:55432/ledger_test?sslmode=disable"

	addrEnv = "LEDGER_ADDR"
	dsnEnv  = "LEDGER_DSN"

	// connectGrace bounds the first connection to the database.
	connectGrace = 10 * time.Second

	// drainGrace bounds the wait for in-flight requests after the signal.
	drainGrace = 15 * time.Second
)

// The deadlines the listener is held to. graces carries them to the flags.
const (
	readHeaderGrace = 10 * time.Second
	readGrace       = 30 * time.Second
	requestGrace    = 15 * time.Second
	acquireGrace    = 2 * time.Second
	writeGrace      = 45 * time.Second
	idleGrace       = 120 * time.Second
)

// The ceilings the database keeps on its own, sent as startup parameters so they
// hold from a connection's first statement. A connection string that sets one of
// these keeps its own value.
var databaseGraces = map[string]string{
	"statement_timeout":                   "20s",
	"lock_timeout":                        "10s",
	"idle_in_transaction_session_timeout": "30s",
}

// deadlines is what the server is held to, once flags and environment are settled.
type deadlines struct {
	readHeader time.Duration
	read       time.Duration
	request    time.Duration
	acquire    time.Duration
	write      time.Duration
	idle       time.Duration
}

// A grace is one deadline a deployer can move: the flag that sets it, what it
// bounds, its default, and the field it settles into.
type grace struct {
	flag string
	why  string
	def  time.Duration
	into *time.Duration
}

// env is the environment variable a grace is also settable from.
func (g grace) env() string {
	return "LEDGER_" + strings.ToUpper(strings.ReplaceAll(g.flag, "-", "_"))
}

// graces is every deadline serve takes, in the order `serve -h` prints them.
func graces(d *deadlines) []grace {
	return []grace{
		{"read-header-timeout", "close a connection that opens and then sends no headers", readHeaderGrace, &d.readHeader},
		{"read-timeout", "close a connection that has not finished sending its request", readGrace, &d.read},
		{"request-timeout", "answer 503 and cancel the database work behind a request that runs past this", requestGrace, &d.request},
		{"acquire-timeout", "answer 503 to a request that has waited this long for one of the pool's sixteen connections", acquireGrace, &d.acquire},
		{"write-timeout", "close a connection whose response cannot be handed over", writeGrace, &d.write},
		{"idle-timeout", "close a kept-alive connection that is carrying nothing", idleGrace, &d.idle},
	}
}

// defaultDeadlines is what the graces settle to when nothing sets them.
func defaultDeadlines() deadlines {
	var d deadlines
	for _, g := range graces(&d) {
		*g.into = g.def
	}
	return d
}

// config is what serve was asked for, once flags and environment are settled.
type config struct {
	addr      string
	dsn       string
	deadlines deadlines
}

// parseServe reads the flags, then the environment, then the defaults.
func parseServe(args []string, stderr io.Writer, getenv func(string) string) (config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: pacioli serve [flags]\n\n")
		fs.PrintDefaults()
	}

	// Empty rather than the default, so given and defaulted are different states.
	addr := fs.String("addr", "", "listen address, host:port (default "+defaultAddr+", or $"+addrEnv+")")
	dsn := fs.String("dsn", "", "postgres connection string (default compose.test.yaml's, or $"+dsnEnv+")")

	// Zero for the same reason, and net/http reads zero as no deadline at all.
	var settled deadlines
	gs := graces(&settled)
	given := make([]*time.Duration, len(gs))
	for i, g := range gs {
		given[i] = fs.Duration(g.flag, 0, g.why+" (default "+g.def.String()+", or $"+g.env()+")")
	}

	if err := fs.Parse(args); err != nil {
		return config{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("%w: serve takes no arguments, got %q. It takes the flags %s; run `pacioli serve -h`",
			errUsage, fs.Arg(0), strings.Join(serveFlags(gs), ", "))
	}

	for i, g := range gs {
		d, err := firstDuration(*given[i], getenv(g.env()), g.def)
		if err != nil {
			return config{}, fmt.Errorf("%w: -%s: %w", errUsage, g.flag, err)
		}
		*g.into = d
	}

	return config{
		addr:      firstSet(*addr, getenv(addrEnv), defaultAddr),
		dsn:       firstSet(*dsn, getenv(dsnEnv), defaultDSN),
		deadlines: settled,
	}, nil
}

// serveFlags is the closed set a refusal lists.
func serveFlags(gs []grace) []string {
	out := []string{"-addr", "-dsn"}
	for _, g := range gs {
		out = append(out, "-"+g.flag)
	}
	slices.Sort(out)
	return out
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstDuration settles one deadline: the flag, then the environment, then the default.
func firstDuration(flagged time.Duration, env string, def time.Duration) (time.Duration, error) {
	if flagged != 0 {
		return positive(flagged)
	}
	if env != "" {
		d, err := time.ParseDuration(env)
		if err != nil {
			return 0, fmt.Errorf("read %q as a duration: %w", env, err)
		}
		return positive(d)
	}
	return def, nil
}

// positive refuses a deadline of zero, which to net/http is no deadline at all.
func positive(d time.Duration) (time.Duration, error) {
	if d <= 0 {
		return 0, fmt.Errorf("a deadline of %s is no deadline at all, which is what these flags exist to end. Give a positive duration", d)
	}
	return d, nil
}

// serve opens the database, mounts the read surface on it and runs until ctx is done.
func serve(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string) error {
	cfg, err := parseServe(args, stderr, getenv)
	if err != nil {
		return err
	}

	// One logger for the lifecycle lines and the causes behind a 500.
	lg := log.New(stderr, "", log.LstdFlags)

	db, err := open(ctx, cfg.dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	// The compose database is a tmpfs and starts empty.
	applied, err := schema.Apply(ctx, db)
	if err != nil {
		return fmt.Errorf("apply the schema to %s: %w", redacted(cfg.dsn), err)
	}
	if applied {
		lg.Print("schema applied from internal/schema/0001_ledger.sql")
	} else {
		lg.Print("schema already present")
	}

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w\n\nAsk for another with -addr, or $%s", cfg.addr, err, addrEnv)
	}

	return serveOn(ctx, ln, ledgerhttp.Handler(store(db, cfg.deadlines), lg), cfg.deadlines, lg)
}

// store is the pool, and the ceiling on the wait for one of its connections.
func store(db *sql.DB, d deadlines) ledgerhttp.Pool {
	return ledgerhttp.Pool{DB: db, Acquire: d.acquire}
}

// open connects and proves the connection, because sql.Open does neither.
func open(ctx context.Context, dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("read %s as a connection string: %w", redacted(dsn), err)
	}
	for name, value := range databaseGraces {
		if _, given := cfg.RuntimeParams[name]; !given {
			cfg.RuntimeParams[name] = value
		}
	}

	db := stdlib.OpenDB(*cfg)

	// The default is unlimited, which can exhaust Postgres's own connection limit.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, connectGrace)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("reach the database at %s: %w\n\nStart it with: make db-up, or point somewhere else with -dsn or $%s",
			redacted(dsn), err, dsnEnv)
	}
	return db, nil
}

// redacted is the connection string with its password taken out.
func redacted(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "the configured database"
	}
	return u.Redacted()
}

// serveOn serves h on ln until ctx is done, then drains and returns.
func serveOn(ctx context.Context, ln net.Listener, h http.Handler, d deadlines, lg *log.Logger) error {
	srv := newServer(h, d, lg)

	// Buffered, so the goroutine can finish with nothing left to read it.
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	lg.Printf("listening on %s", ln.Addr())

	select {
	case err := <-serveErr:
		// Serve stopped on its own, so it failed; the quiet way out is Shutdown.
		return fmt.Errorf("serve on %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}

	lg.Print("signal received, finishing the requests already in flight")
	if err := drain(ctx, srv); err != nil {
		return fmt.Errorf("shut down after %s: %w", drainGrace, err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve on %s: %w", ln.Addr(), err)
	}
	lg.Print("stopped")
	return nil
}

// newServer is the listener's whole posture in one place. The four deadlines here
// bound the socket; ledgerhttp.Deadline is what bounds the work behind it.
func newServer(h http.Handler, d deadlines, lg *log.Logger) *http.Server {
	return &http.Server{
		Handler:           ledgerhttp.Deadline(d.request, h),
		ErrorLog:          lg,
		ReadHeaderTimeout: d.readHeader,
		ReadTimeout:       d.read,
		WriteTimeout:      d.write,
		IdleTimeout:       d.idle,
	}
}

// drain closes the listener and waits for the requests already accepted, for up to
// drainGrace. WithoutCancel, so a deadline off a cancelled ctx does not expire at once.
func drain(ctx context.Context, srv *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainGrace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
