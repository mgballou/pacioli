package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestParseControlRejectsInvalidDeclarations(t *testing.T) {
	valid := "title: Change status\nfile: value.txt\nrun: test -f value.txt\n--- before\ngreen\n--- after\nred\n"
	for _, tc := range []struct {
		name        string
		declaration string
		want        string
	}{
		{"malformed header", "not a header\n" + valid, "cannot read"},
		{"unknown header", "typo: value\n" + valid, "unknown key"},
		{"missing title", strings.Replace(valid, "title: Change status\n", "", 1), "title"},
		{"missing file", strings.Replace(valid, "file: value.txt\n", "", 1), "file"},
		{"missing command", strings.Replace(valid, "run: test -f value.txt\n", "", 1), "run"},
		{"missing before", "title: Change status\nfile: value.txt\nrun: true\n", "before"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.control")
			if err := os.WriteFile(path, []byte(tc.declaration), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := parseControl(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("parseControl error = %v, want declaration path and %q", err, tc.want)
			}
		})
	}
}

func TestControlApplyRejectsMissingOrAmbiguousText(t *testing.T) {
	for _, original := range []string{"other\n", "green\ngreen\n"} {
		t.Run(original, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "value.txt")
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			c := control{path: "example.control", file: path, before: "green", after: "red"}
			undo, err := c.apply()
			if err == nil || !strings.Contains(err.Error(), "want exactly 1") {
				t.Fatalf("apply error = %v, want ambiguous or missing text rejection", err)
			}
			if undo != nil {
				t.Fatal("rejected mutation returned an undo function")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != original {
				t.Errorf("rejected mutation changed file to %q, want %q", got, original)
			}
		})
	}
}

func TestRunControlsChecksMutationAndRestoresFiles(t *testing.T) {
	for _, tc := range []struct {
		name       string
		command    string
		before     string
		wantError  string
		wantOutput []string
	}{
		{
			name:       "green then red",
			command:    `printf 'check output\n'; printf 'check stderr\n' >&2; test "$(cat value.txt)" = green`,
			before:     "green",
			wantOutput: []string{"Change status", "green before the mutation", "red after the mutation", "check output", "check stderr"},
		},
		{
			name:       "already red",
			command:    "false",
			before:     "green",
			wantError:  "already red",
			wantOutput: []string{"RED BEFORE THE MUTATION"},
		},
		{
			name:       "stayed green",
			command:    "true",
			before:     "green",
			wantError:  "stayed green",
			wantOutput: []string{"STAYED GREEN"},
		},
		{
			name:       "stale mutation",
			command:    "true",
			before:     "absent",
			wantError:  "want exactly 1",
			wantOutput: []string{"green before the mutation"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			cmd := exec.Command("git", "init", "--quiet")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v\n%s", err, out)
			}
			// Keep fixture files out of Git's status without commits or global configuration.
			if err := os.WriteFile(filepath.Join(".git", "info", "exclude"), []byte("*\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir("negative-controls", 0o700); err != nil {
				t.Fatal(err)
			}
			declaration := "title: Change status\nfile: value.txt\nrun: " + tc.command + "\n--- before\n" + tc.before + "\n--- after\nred\n"
			if err := os.WriteFile(filepath.Join("negative-controls", "example.control"), []byte(declaration), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile("value.txt", []byte("green\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := os.CreateTemp(t.TempDir(), "report-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = report.Close() })
			err = runControls(report)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("runControls: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("runControls error = %v, want %q", err, tc.wantError)
			}
			got, err := os.ReadFile("value.txt")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "green\n" {
				t.Errorf("file after controls = %q, want original green", got)
			}
			output, err := os.ReadFile(report.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.wantOutput {
				if !strings.Contains(string(output), want) {
					t.Errorf("report missing %q: %s", want, output)
				}
			}
		})
	}
}

func TestLoadControlsReportsEmptyOrInvalidDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := loadControls(); err == nil || !strings.Contains(err.Error(), "no negative controls") {
		t.Fatalf("loadControls error = %v, want missing declarations", err)
	}
	if err := os.Mkdir("negative-controls", 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("negative-controls", "broken.control")
	if err := os.WriteFile(path, []byte("title: Incomplete\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadControls(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("loadControls error = %v, want invalid declaration path", err)
	}
}
