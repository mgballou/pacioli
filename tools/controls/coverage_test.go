package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsedControlMutatesAndRestoresFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "check.sh")
	original := "#!/bin/sh\nvalue=green\nprintf '%s' \"$value\"\n"
	if err := os.WriteFile(target, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "example.control")
	declaration := "# Replace just the value assignment.\n\ntitle: Change status\nfile: " + target + "\nrun: sh check.sh\n--- before\nvalue=green\n--- after\nvalue=red\n"
	if err := os.WriteFile(path, []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := parseControl(path)
	if err != nil {
		t.Fatal(err)
	}
	undo, err := c.apply()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(undo)
	mutated, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\nvalue=red\nprintf '%s' \"$value\"\n"
	if string(mutated) != want {
		t.Errorf("mutated file = %q, want %q", mutated, want)
	}
	undo()
	restored, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != original {
		t.Errorf("restored file = %q, want %q", restored, original)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("restored permissions = %o, want 755", info.Mode().Perm())
	}
}
