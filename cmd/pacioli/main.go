// Command pacioli serves a double-entry ledger over HTTP.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// version is set at link time by the Makefile.
var version = "dev"

// errUsage separates bad arguments from failed work; they exit differently.
var errUsage = errors.New("usage")

func main() {
	// SIGTERM from a container runtime, SIGINT from a terminal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "pacioli: %v\n", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// run dispatches the subcommand. getenv is a parameter so tests can supply one.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	if len(args) == 0 {
		fmt.Fprintln(stdout, version)
		return nil
	}

	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "serve":
		return serve(ctx, args[1:], stderr, getenv)
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("%w: no such command %q", errUsage, args[0])
	}
}

const usage = `pacioli — a double-entry ledger

  pacioli            print the version this binary was built from
  pacioli version    the same, said out loud
  pacioli serve      serve the ledger's read surface over HTTP

  pacioli serve -h   the flags serve takes
`
