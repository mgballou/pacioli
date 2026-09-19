package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBinaryPrintsTheCommitItWasBuiltFrom(t *testing.T) {
	root := repoRoot(t)
	want := git(t, root, "describe", "--tags", "--always", "--dirty")

	dir := t.TempDir()
	build := exec.Command("make", "build", "BIN_DIR="+dir)
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("make build: %v\n%s", err, out)
	}

	var stdout, stderr bytes.Buffer
	bin := exec.Command(filepath.Join(dir, "pacioli"))
	bin.Stdout, bin.Stderr = &stdout, &stderr
	if err := bin.Run(); err != nil {
		t.Fatalf("running the binary: %v\n%s", err, stderr.String())
	}

	got := stdout.String()
	if got != want+"\n" {
		t.Errorf("the binary printed %q, want %q — the stamp does not name the commit it was built from", got, want+"\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("the binary wrote %q to stderr, want nothing", stderr.String())
	}

	printed := strings.TrimSuffix(strings.TrimSpace(got), "-dirty")
	if printed == "" {
		t.Fatal("the binary printed nothing to resolve")
	}
	named, err := gitErr(root, "rev-parse", "--verify", printed+"^{commit}")
	if err != nil {
		t.Fatalf("the binary printed %q, which names no commit in this repository: %v", printed, err)
	}
	if head := git(t, root, "rev-parse", "HEAD"); named != head {
		t.Errorf("the binary printed %q, which is commit %s, but it was built from %s", printed, named, head)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := gitErr(".", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("this test needs a git checkout to know what commit it is on: %v", err)
	}
	return out
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitErr(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func gitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
