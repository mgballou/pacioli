// Command controls applies every mutation declared in negative-controls/ and
// insists each one is green before the mutation and red after it.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if err := runControls(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "controls: %v\n", err)
		os.Exit(1)
	}
}

// control is one declared mutation: a named edit to a file, and the command
// that has to fail once it is applied.
type control struct {
	path   string
	title  string
	file   string
	run    string
	before string
	after  string
}

func parseControl(path string) (control, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return control{}, err
	}
	c := control{path: path}
	section := ""
	var before, after []string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case line == "--- before":
			section = "before"
			continue
		case line == "--- after":
			section = "after"
			continue
		}
		switch section {
		case "before":
			before = append(before, line)
		case "after":
			after = append(after, line)
		default:
			if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
				continue
			}
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				return control{}, fmt.Errorf("%s: cannot read %q as `key: value`", path, line)
			}
			switch strings.TrimSpace(k) {
			case "title":
				c.title = strings.TrimSpace(v)
			case "file":
				c.file = strings.TrimSpace(v)
			case "run":
				c.run = strings.TrimSpace(v)
			default:
				return control{}, fmt.Errorf("%s: unknown key %q", path, k)
			}
		}
	}
	c.before, c.after = strings.TrimRight(strings.Join(before, "\n"), "\n"), strings.TrimRight(strings.Join(after, "\n"), "\n")
	switch {
	case c.title == "":
		return control{}, fmt.Errorf("%s: no title", path)
	case c.file == "":
		return control{}, fmt.Errorf("%s: no file", path)
	case c.run == "":
		return control{}, fmt.Errorf("%s: no run", path)
	case c.before == "":
		return control{}, fmt.Errorf("%s: no `--- before` section", path)
	}
	return c, nil
}

func loadControls() ([]control, error) {
	paths, err := filepath.Glob("negative-controls/*.control")
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, errors.New("no negative controls declared in negative-controls/ — every branch owes at least one, or a green run on it proves nothing")
	}
	var out []control
	for _, p := range paths {
		c, err := parseControl(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// apply makes the mutation and returns the undo. The `before` text has to appear
// exactly once, so a control cannot rot into a no-op when the code moves.
func (c control) apply() (func(), error) {
	original, err := os.ReadFile(c.file)
	if err != nil {
		return nil, err
	}
	if n := bytes.Count(original, []byte(c.before)); n != 1 {
		return nil, fmt.Errorf("%s: its `before` text appears %d times in %s, want exactly 1 — the control has rotted", c.path, n, c.file)
	}
	info, err := os.Stat(c.file)
	if err != nil {
		return nil, err
	}
	mutated := bytes.Replace(original, []byte(c.before), []byte(c.after), 1)
	if err := os.WriteFile(c.file, mutated, info.Mode()); err != nil {
		return nil, err
	}
	return func() { _ = os.WriteFile(c.file, original, info.Mode()) }, nil
}

// runControls applies every declared control in turn and insists the command it
// names passes before the mutation and fails after. A control that leaves the
// suite green names a test that cannot fail; one already red proves nothing.
func runControls(w *os.File) error {
	if out, err := exec.Command("git", "status", "--porcelain").Output(); err != nil {
		return fmt.Errorf("git status: %w", err)
	} else if len(bytes.TrimSpace(out)) != 0 {
		return errors.New("the working tree is dirty; negative controls edit tracked files and restore them, so commit or stash first")
	}
	controls, err := loadControls()
	if err != nil {
		return err
	}
	var bad []string
	for _, c := range controls {
		fmt.Fprintf(w, "CONTROL  %s\n", c.path)
		fmt.Fprintf(w, "         %s\n", c.title)
		fmt.Fprintf(w, "         %s: %s\n", c.file, oneLine(c.before))
		fmt.Fprintf(w, "             becomes %s\n", oneLine(c.after))
		fmt.Fprintf(w, "         $ %s\n\n", c.run)

		if err := c.exec(nil); err != nil {
			fmt.Fprintf(w, "         RED BEFORE THE MUTATION — this proves nothing: %v\n\n", err)
			bad = append(bad, c.path+" (already red)")
			continue
		}
		fmt.Fprintf(w, "         green before the mutation\n\n")

		undo, err := c.apply()
		if err != nil {
			return err
		}
		runErr := c.exec(w)
		undo()

		fmt.Fprintln(w)
		if runErr == nil {
			fmt.Fprintf(w, "         STAYED GREEN — nothing here can fail\n\n")
			bad = append(bad, c.path+" (stayed green)")
		} else {
			fmt.Fprintf(w, "         red after the mutation\n\n")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("these controls did not go green then red, so what they mutate is not under test: %s", strings.Join(bad, ", "))
	}
	return nil
}

// exec runs the control's command, sending output to w when w is not nil.
func (c control) exec(w *os.File) error {
	cmd := exec.Command("sh", "-c", c.run)
	if w != nil {
		cmd.Stdout, cmd.Stderr = w, w
	}
	return cmd.Run()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	if len(s) > 96 {
		s = s[:93] + "..."
	}
	return s
}
