package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Makefile is the developer's half of the gate, and these are the claims it
// makes that nothing else would notice it stop making. They run make itself on
// a real filesystem rather than reading the recipe, with one exception that
// says why.

// runMake runs a target of this repository's Makefile with extra variables and
// environment. MAKEFLAGS and MAKELEVEL are dropped, because `make check` runs
// this suite, and an outer make's jobserver and flags are not this test's.
func runMake(t *testing.T, env []string, args ...string) (code int, out string) {
	t.Helper()
	needsMake(t)
	cmd := exec.Command("make", append([]string{"--no-print-directory"}, args...)...)
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "MAKEFLAGS", "MAKELEVEL", "MFLAGS":
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, env...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, b.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), b.String()
	default:
		t.Fatalf("make %v: %v", args, err)
		return 0, ""
	}
}

// fakeGo writes a stand-in for the go command that `make size` can be pointed
// at with GO=. It handles `build -o PATH .`, appends PATH to a log, and then
// does what mode says: writes the bytes it is given, makes a directory where
// the binary should be, or fails.
func fakeGo(t *testing.T) (prog, log string) {
	t.Helper()
	dir := t.TempDir()
	prog, log = filepath.Join(dir, "go"), filepath.Join(dir, "log")
	must(t, os.WriteFile(prog, []byte(`#!/bin/sh
echo "$3" >> "$FAKEGO_LOG"
case "$FAKEGO_MODE" in
fail) exit 1 ;;
dir) mkdir -p "$3" ;;
*) printf '%s' "$FAKEGO_BYTES" > "$3" ;;
esac
`), 0o755))
	return prog, log
}

// `make size` builds into a directory of its own, removes it however the recipe
// ends, and fails when any step does. Until 2026-10-06 it built to
// /tmp/hunk-size, a fixed name in a directory every user can write, and a size
// it could not read was printed as empty and passed: the "dir" row exited 0.
func TestMakeSize(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		bytes  string
		budget string
		code   bool // non-zero exit wanted
		want   string
		absent string
	}{
		{name: "under budget", bytes: "12345", budget: "5", want: "5 bytes, budget 5"},
		{name: "over budget", bytes: "123456", budget: "5", code: true, want: "binary is 6 bytes, over the 5 budget"},
		{name: "build fails", mode: "fail", budget: "5", code: true, absent: "bytes"},
		{name: "build leaves a directory", mode: "dir", budget: "5", code: true, absent: "budget 5"},
	}
	prog, log := fakeGo(t)
	var seen []string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			must(t, os.WriteFile(log, nil, 0o644))
			code, out := runMake(t,
				[]string{"TMPDIR=" + tmp, "FAKEGO_LOG=" + log, "FAKEGO_MODE=" + tt.mode, "FAKEGO_BYTES=" + tt.bytes},
				"size", "GO="+prog, "SIZE_BUDGET_BYTES="+tt.budget)
			flat := strings.Join(strings.Fields(out), " ")
			if (code != 0) != tt.code {
				t.Errorf("exit %d, want non-zero %v; output:\n%s", code, tt.code, out)
			}
			if tt.want != "" && !strings.Contains(flat, tt.want) {
				t.Errorf("output %q does not say %q", flat, tt.want)
			}
			if tt.absent != "" && strings.Contains(flat, tt.absent) {
				t.Errorf("output %q says %q, which a failed step must not reach", flat, tt.absent)
			}

			b, err := os.ReadFile(log)
			must(t, err)
			built := strings.TrimSpace(string(b))
			// A fresh directory under TMPDIR, never TMPDIR itself and never a
			// fixed name somebody else could have put something at first.
			if filepath.Dir(filepath.Dir(built)) != tmp {
				t.Errorf("built to %q, want a fresh directory under %q", built, tmp)
			}
			seen = append(seen, built)
			left, err := os.ReadDir(tmp)
			must(t, err)
			if len(left) != 0 {
				t.Errorf("left %d entries in TMPDIR, the first %q", len(left), left[0].Name())
			}
		})
	}
	// Every row got its own TMPDIR, so this compares the directory under it.
	names := map[string]bool{}
	for _, s := range seen {
		names[filepath.Base(filepath.Dir(s))] = true
	}
	if len(names) < 2 && len(seen) > 1 {
		t.Errorf("every run built into a directory named %v; want a fresh name each time", names)
	}
}

// No recipe names a fixed path under /tmp. TestMakeSize proves the one target
// that had one; this is the rule for the next target, and it reads the text
// because the claim is about every recipe, including ones no test runs.
func TestMakefileNamesNoFixedTempPath(t *testing.T) {
	b, err := os.ReadFile("Makefile")
	must(t, err)
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "\t") && strings.Contains(line, "/tmp/") {
			t.Errorf("Makefile:%d: a recipe names a fixed path under /tmp; use mktemp -d: %s", i+1, strings.TrimSpace(line))
		}
	}
}
