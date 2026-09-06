package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mgballou/pacioli/internal/docs"
)

func TestTheRefusalOfAnUnknownCommandNamesItAndTheCommandsThereAre(t *testing.T) {
	var stdout, stderr strings.Builder

	err := run(context.Background(), []string{"srve"}, &stdout, &stderr, func(string) string { return "" })
	if !errors.Is(err, errUsage) {
		t.Fatalf("run gave %v, want a usage error", err)
	}
	for _, want := range append([]string{`"srve"`, docs.Home}, commands...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n  %v", want, err)
		}
	}
	t.Logf("%v", err)
}

// Every refusal in this program points at `pacioli --help`, so it has to answer.
func TestTheHelpEveryRefusalPointsAtIsACommand(t *testing.T) {
	for _, arg := range []string{"help", "-h", "-help", "--help"} {
		var stdout, stderr strings.Builder
		if err := run(context.Background(), []string{arg}, &stdout, &stderr, func(string) string { return "" }); err != nil {
			t.Errorf("pacioli %s gave %v, want the usage and no error", arg, err)
		}
		if !strings.Contains(stdout.String(), "pacioli serve") {
			t.Errorf("pacioli %s wrote %q to stdout, want the usage", arg, stdout.String())
		}
		if stderr.String() != "" {
			t.Errorf("pacioli %s wrote %q to stderr, want nothing", arg, stderr.String())
		}
	}
}

// commands is written out rather than read off the switch, so this holds the
// two against each other. The context is already done, so serve reaches for the
// database, gives up and returns rather than running.
func TestTheCommandsListedAreTheCommandsThatRun(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()

	for _, name := range commands {
		var stdout, stderr strings.Builder
		err := run(done, []string{name}, &stdout, &stderr, func(string) string { return "" })
		if errors.Is(err, errUsage) {
			t.Errorf("%q is listed as a command and is refused as one: %v", name, err)
		}
	}
}

func TestTheRefusalOfAnArgumentServeDoesNotTakeNamesItAndTheFlags(t *testing.T) {
	_, err := parseServe([]string{"127.0.0.1:1"}, io.Discard, func(string) string { return "" })
	if !errors.Is(err, errUsage) {
		t.Fatalf("parseServe gave %v, want a usage error", err)
	}
	for _, want := range []string{`"127.0.0.1:1"`, "-addr", "-dsn", "pacioli serve -h"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n  %v", want, err)
		}
	}
	t.Logf("%v", err)
}

func TestTheRefusalOfAnAddressAlreadyTakenNamesItAndHowToChangeIt(t *testing.T) {
	// The DSN is never reached: the listen fails first, on an address no
	// machine will bind.
	err := serve(context.Background(), []string{"-addr", "256.256.256.256:1"}, io.Discard,
		func(string) string { return "" })
	if err == nil {
		t.Fatal("serve bound an address that does not exist")
	}
	for _, want := range []string{"256.256.256.256:1", "-addr", addrEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n  %v", want, err)
		}
	}
	t.Logf("%v", err)
}
