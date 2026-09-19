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
	"time"

	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/schema"

	// Registers "pgx" as a database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	// defaultAddr is loopback, so running the binary does not put the
	// database on the network.
	defaultAddr = "127.0.0.1:8080"

	// defaultDSN is the compose database. It is written out rather than taken
	// from internal/testdb, which imports "testing"; a test holds the two
	// strings against each other so the copy cannot drift.
	defaultDSN = "postgres://ledger:ledger@127.0.0.1:55432/ledger_test?sslmode=disable"

	addrEnv = "LEDGER_ADDR"
	dsnEnv  = "LEDGER_DSN"

	// connectGrace bounds the first connection, so a database that is starting
	// rather than absent does not hang the process.
	connectGrace = 10 * time.Second

	// drainGrace bounds the wait for in-flight requests after the signal.
	drainGrace = 15 * time.Second

	// readHeaderGrace closes a connection that opens and then says nothing.
	// The zero value is no timeout at all.
	readHeaderGrace = 10 * time.Second
)

// config is what serve was asked for, once flags and environment are settled.
type config struct {
	addr string
	dsn  string
}

// parseServe reads the flags, falling back to the environment and then to the
// defaults above.
func parseServe(args []string, stderr io.Writer, getenv func(string) string) (config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: pacioli serve [flags]\n\n")
		fs.PrintDefaults()
	}

	// Empty rather than the default, so a flag that was given and a flag that
	// holds the default are different states.
	addr := fs.String("addr", "", "listen address, host:port (default "+defaultAddr+", or $"+addrEnv+")")
	dsn := fs.String("dsn", "", "postgres connection string (default compose.test.yaml's, or $"+dsnEnv+")")

	if err := fs.Parse(args); err != nil {
		return config{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("%w: serve takes no arguments, got %q", errUsage, fs.Arg(0))
	}

	return config{
		addr: firstSet(*addr, getenv(addrEnv), defaultAddr),
		dsn:  firstSet(*dsn, getenv(dsnEnv), defaultDSN),
	}, nil
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// serve opens the database, mounts the read surface on it and runs until ctx is
// done.
func serve(ctx context.Context, args []string, stderr io.Writer, getenv func(string) string) error {
	cfg, err := parseServe(args, stderr, getenv)
	if err != nil {
		return err
	}

	// One logger for the lifecycle lines and the causes behind a 500.
	// Requests and their bodies are not logged.
	lg := log.New(stderr, "", log.LstdFlags)

	db, err := open(ctx, cfg.dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	// The compose database is a tmpfs and starts empty, so without this the
	// first request to a fresh container is a 500 about a missing table.
	applied, err := schema.Apply(ctx, db)
	if err != nil {
		return fmt.Errorf("apply the schema: %w", err)
	}
	if applied {
		lg.Print("schema applied from internal/schema/0001_ledger.sql")
	} else {
		lg.Print("schema already present")
	}

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.addr, err)
	}

	return serveOn(ctx, ln, ledgerhttp.Handler(ledgerhttp.Pool{DB: db}, lg), lg)
}

// open connects and proves the connection, because sql.Open does neither.
func open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open the database: %w", err)
	}

	// The default is unlimited, which can exhaust Postgres's own connection
	// limit under load.
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, connectGrace)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("reach the database at %s: %w\n\nStart it with: make db-up", redacted(dsn), err)
	}
	return db, nil
}

// redacted is the connection string with its password taken out. A DSN that will
// not parse is reported as a placeholder rather than as itself.
func redacted(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "the configured database"
	}
	return u.Redacted()
}

// serveOn serves h on ln until ctx is done, then drains and returns. It takes a
// listener so a test can serve on a port the kernel picked.
func serveOn(ctx context.Context, ln net.Listener, h http.Handler, lg *log.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ErrorLog:          lg,
		ReadHeaderTimeout: readHeaderGrace,
	}

	// Buffered, so the goroutine can finish with nothing left to read it.
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	lg.Printf("listening on %s", ln.Addr())

	select {
	case err := <-serveErr:
		// Serve stopped on its own, so it failed; the quiet way out is Shutdown.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	lg.Print("signal received, finishing the requests already in flight")
	if err := drain(ctx, srv); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	lg.Print("stopped")
	return nil
}

// drain closes the listener and waits for the requests already accepted, for up
// to drainGrace. WithoutCancel because ctx is already cancelled, and a deadline
// derived from it would expire at once and cut a request off mid-answer.
func drain(ctx context.Context, srv *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainGrace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
