package docs_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mgballou/pacioli/internal/ledgerhttp"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestRunbookBalanceCheckCommands(t *testing.T) {
	commands, _ := runbookCommands(t)
	var balanceCommands []string
	for _, command := range commands {
		if strings.Contains(command, "localhost:18080") {
			balanceCommands = append(balanceCommands, command)
		}
	}
	if len(balanceCommands) != 7 {
		t.Fatalf("found %d runnable balance-check commands, want 7; update this test when the documented flow changes", len(balanceCommands))
	}

	db := testdb.Disposable(t, "runbook_commands_test")
	server := httptest.NewServer(ledgerhttp.Handler(ledgerhttp.Pool{DB: db}, nil))
	t.Cleanup(server.Close)
	address := strings.TrimPrefix(server.URL, "http://")

	wants := [][]string{
		{`"account": "assets.cash"`, `"balance_minor": 0`},
		{`"account": "revenue.fees"`, `"balance_minor": 0`},
		{`"description": "Runbook check"`, `"amount_minor": 4500`},
		{`"balanced": true`, `"postings": 2`},
		{`"error": "an account already holds that code"`, `"value": "assets.cash"`},
		{"HTTP/1.1 201 Created", "Idempotent-Replayed: true"},
		{`"balanced": true`, `"postings": 2`},
	}

	for i, command := range balanceCommands {
		t.Run(strings.TrimSpace(strings.SplitN(command, " ", 2)[0])+"-"+string(rune('1'+i)), func(t *testing.T) {
			command = strings.ReplaceAll(command, "localhost:18080", address)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-eu", "-c", command)
			cmd.Dir = repoRoot(t)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("run command from docs/RUNBOOK.md:\n%s\nerror: %v\nstdout:\n%s\nstderr:\n%s", balanceCommands[i], err, stdout.String(), stderr.String())
			}
			for _, want := range wants[i] {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("command output does not contain %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestEveryRunbookCommandIsRunOrListedAsNonTestable(t *testing.T) {
	commands, text := runbookCommands(t)
	appendixAt := strings.Index(text, "## Commands outside the test")
	if appendixAt < 0 {
		t.Fatal("docs/RUNBOOK.md must classify commands outside the test")
	}
	if !strings.Contains(text, "reclaim policy") || !strings.Contains(text, "Retain") {
		t.Fatal("docs/RUNBOOK.md must explain that PV deletion depends on its reclaim policy")
	}
	appendix := text[appendixAt:]

	for _, command := range commands {
		switch {
		case strings.Contains(command, "localhost:18080"):
			// These curl commands are extracted and run in TestRunbookBalanceCheckCommands.
		case strings.HasPrefix(command, "orbctl "):
			mustListReason(t, appendix, "OrbStack settings")
		case strings.HasPrefix(command, "docker "):
			mustListReason(t, appendix, "Docker daemon")
		case strings.HasPrefix(command, "kind "):
			mustListReason(t, appendix, "kind")
		case strings.HasPrefix(command, "kubectl "):
			mustListReason(t, appendix, "Kubernetes context and cluster")
		case strings.HasPrefix(command, "curl -sSv localhost:8080") || strings.HasPrefix(command, "curl -sS 127.0.0.1:8080"):
			mustListReason(t, appendix, "loopback addresses")
		default:
			t.Errorf("fenced runbook command is neither tested nor classified: %s", command)
		}
	}
}

func runbookCommands(t *testing.T) ([]string, string) {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "RUNBOOK.md"))
	if err != nil {
		t.Fatalf("read docs/RUNBOOK.md: %v", err)
	}

	var commands []string
	var inFence bool
	var continuing bool
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			continuing = false
			return
		}
		commands = append(commands, strings.TrimSpace(current.String()))
		current.Reset()
		continuing = false
	}
	appendCommandLine := func(line string) {
		line = strings.TrimSpace(line)
		continuing = strings.HasSuffix(line, `\`)
		if continuing {
			line = strings.TrimSpace(strings.TrimSuffix(line, `\`))
		}
		if current.Len() > 0 {
			if shellQuoteOpen(current.String()) {
				current.WriteByte('\n')
			} else {
				current.WriteByte(' ')
			}
		}
		current.WriteString(line)
	}
	for _, line := range strings.Split(string(text), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			flush()
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		if strings.HasPrefix(trimmed, "$ ") {
			flush()
			appendCommandLine(strings.TrimPrefix(trimmed, "$ "))
			continue
		}
		if continuing {
			appendCommandLine(line)
			continue
		}
		if current.Len() > 0 && shellQuoteOpen(current.String()) {
			appendCommandLine(line)
			continue
		}
		flush()
	}
	flush()
	return commands, string(text)
}

func shellQuoteOpen(command string) bool {
	var quote rune
	escaped := false
	for _, r := range command {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
		}
	}
	return quote != 0
}

func mustListReason(t *testing.T, appendix, reason string) {
	t.Helper()
	if !strings.Contains(appendix, reason) {
		t.Errorf("non-testable command requires a reason mentioning %q", reason)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate runbook_test.go")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	if err != nil {
		t.Fatalf("find repository root: %v", err)
	}
	return root
}
