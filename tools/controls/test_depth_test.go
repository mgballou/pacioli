package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mgballou/pacioli/internal/ledger"
	"github.com/mgballou/pacioli/internal/schema"
	"github.com/mgballou/pacioli/internal/testdb"
)

func TestControlsRequireGreenBeforeRedAndRestoreTheSource(t *testing.T) {
	for _, c := range []struct {
		name, command, want string
	}{
		{"caught mutation", `test "$(cat probe.txt)" = original`, ""},
		{"already failing", "false", "already red"},
		{"insensitive test", "true", "stayed green"},
	} {
		t.Run(c.name, func(t *testing.T) {
			controlFixture(t, controlText(c.command))
			log, err := os.CreateTemp(t.TempDir(), "controls-log")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			err = runControls(log)
			if c.want == "" {
				if err != nil {
					t.Fatalf("control: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("control = %v, want %q", err, c.want)
			}
			raw, err := os.ReadFile("probe.txt")
			if err != nil || string(raw) != "original\n" {
				t.Fatalf("source after control = %q, %v; want original", raw, err)
			}
			output, err := os.ReadFile(log.Name())
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" && (!strings.Contains(string(output), "green before the mutation") ||
				!strings.Contains(string(output), "red after the mutation")) {
				t.Errorf("control did not report both checks: %s", output)
			}
		})
	}
}

func TestControlsRefuseAnUnsafeOrUnreadableRun(t *testing.T) {
	for _, c := range []struct {
		name, declaration, source, want string
	}{
		{"dirty checkout", controlText("true"), "dirty\n", "working tree is dirty"},
		{"no declarations", "", "", "no negative controls declared"},
		{"missing title", "file: probe.txt\nrun: true\n--- before\noriginal\n", "", "no `title:`"},
		{"missing file", "title: probe\nrun: true\n--- before\noriginal\n", "", "no `file:`"},
		{"missing command", "title: probe\nfile: probe.txt\n--- before\noriginal\n", "", "no `run:`"},
		{"missing mutation", "title: probe\nfile: probe.txt\nrun: true\n", "", "no `--- before` section"},
		{"unknown field", "surprise: value\n", "", "unknown key"},
		{"unreadable field", "unreadable\n", "", "cannot read"},
		{"missing target", strings.ReplaceAll(controlText("true"), "probe.txt", "absent.txt"), "", "no such file"},
		{"stale mutation", strings.ReplaceAll(controlText("true"), "original", "absent"), "", "appears 0 times"},
		{"ambiguous mutation", strings.ReplaceAll(controlText("true"), "original", "i"), "", "appears 2 times"},
	} {
		t.Run(c.name, func(t *testing.T) {
			controlFixture(t, c.declaration)
			if c.source != "" {
				writeFixture(t, "probe.txt", c.source)
			}
			log, err := os.CreateTemp(t.TempDir(), "controls-log")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			if err := runControls(log); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("control = %v, want %q", err, c.want)
			}
		})
	}
}

func TestTheTestDatabaseAddressUsesTheOverrideOrTheComposeDefault(t *testing.T) {
	for _, dsn := range []string{"", "postgres://localhost/address_override_test?sslmode=disable"} {
		t.Run(dsn, func(t *testing.T) {
			t.Setenv(testdb.DSNEnv, dsn)
			want := dsn
			if want == "" {
				want = testdb.DefaultDSN
			}
			if got := testdb.DSN(); got != want {
				t.Errorf("DSN = %q, want %q", got, want)
			}
		})
	}
}

func TestADisposableDatabaseAllowsCommittedWorkAndIsDroppedAtCleanup(t *testing.T) {
	name := fmt.Sprintf("controls_depth_%d_test", os.Getpid())
	code := "assets.depth"
	ctx := context.Background()
	shared := testdb.Open(t)
	var disposable *sql.DB
	t.Run("commit in its own database", func(t *testing.T) {
		db := testdb.Disposable(t, name)
		disposable = db
		var got string
		if err := db.QueryRow(`SELECT current_database()`).Scan(&got); err != nil || got != name {
			t.Fatalf("database = %q, %v; want %q", got, err, name)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := ledger.Open(ctx, tx, ledger.Account{Code: code, Name: "Depth", Kind: "asset", Currency: "GBP"}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if b, err := ledger.BalanceOf(ctx, tx, code); err != nil || !b.AmountMinor.IsZero() {
			t.Fatalf("committed account = %+v, %v", b, err)
		}
	})
	if err := disposable.Ping(); err == nil {
		t.Error("disposable pool still accepts work after cleanup")
	}
	var exists bool
	if err := shared.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil || exists {
		t.Fatalf("disposable database remains = %t, %v", exists, err)
	}
	if _, err := ledger.BalanceOf(ctx, testdb.Tx(t), code); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Fatalf("shared database holds the disposable account: %v", err)
	}
}

func TestSchemaInstallationRollsBackAFailureAndCanBeRetried(t *testing.T) {
	db := testdb.Disposable(t, fmt.Sprintf("schema_depth_%d_test", os.Getpid()))
	if _, err := db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public; CREATE TYPE account_kind AS ENUM ('occupied')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	applied, err := schema.Apply(ctx, db)
	var pgErr *pgconn.PgError
	if applied || !errors.As(err, &pgErr) || pgErr.Code != "42710" || !strings.Contains(err.Error(), "apply 0001_ledger.sql") {
		t.Fatalf("conflicting schema = %t, %v; want duplicate-object refusal", applied, err)
	}
	var partial bool
	if err := db.QueryRow(`SELECT to_regprocedure('public.is_blank(text)') IS NOT NULL`).Scan(&partial); err != nil || partial {
		t.Fatalf("failed installation left a function = %t, %v", partial, err)
	}
	if _, err := db.Exec(`DROP TYPE account_kind`); err != nil {
		t.Fatal(err)
	}
	if applied, err := schema.Apply(ctx, db); err != nil || !applied {
		t.Fatalf("retry = %t, %v; want installed", applied, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ledger.Open(ctx, tx, ledger.Account{Code: "assets.preserved", Name: "Preserved", Kind: "asset", Currency: "GBP"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if applied, err := schema.Apply(ctx, db); err != nil || applied {
		t.Fatalf("second installation = %t, %v; want unchanged", applied, err)
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := ledger.BalanceOf(ctx, tx, "assets.preserved"); err != nil || b.Name != "Preserved" {
		t.Fatalf("account after repeated installation = %+v, %v", b, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if applied, err := schema.Apply(ctx, db); applied || err == nil || !strings.Contains(err.Error(), "begin:") {
		t.Fatalf("closed database = %t, %v; want begin refusal", applied, err)
	}
}

func controlText(command string) string {
	return "# Fixture control\n\ntitle: a changed source is caught\nfile: probe.txt\nrun: " + command +
		"\n--- before\noriginal\n--- after\nmutated\n"
}

func controlFixture(t *testing.T, declaration string) {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.Mkdir("negative-controls", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, "probe.txt", "original\n")
	paths := []string{"probe.txt"}
	if declaration != "" {
		path := filepath.Join("negative-controls", "001-probe.control")
		writeFixture(t, path, declaration)
		paths = append(paths, path)
	}
	fixtureGit(t, "init", "--quiet")
	fixtureGit(t, append([]string{"add", "--"}, paths...)...)
	fixtureGit(t, append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid",
		"-c", "core.hooksPath=/dev/null", "commit", "--quiet", "--no-gpg-sign", "-m", "Create control fixture", "--"}, paths...)...)
}

func fixtureGit(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
