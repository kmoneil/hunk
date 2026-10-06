package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The workflows' claims that no compiler checks, in the habit of
// TestReleasePublishesOnlyOnATagPush and with the same limits: a few regular
// expressions over YAML written two spaces a level, each checked against what
// the files are known to hold, so that a reader that found nothing would fail
// rather than pass.

// A runBlock is the script of one `run:` key, as Actions sees it before it
// substitutes any ${{ }} expression into it.
type runBlock struct {
	file string
	line int
	text string
}

var runKey = regexp.MustCompile(`^(\s*)(- )?run:\s?(.*)$`)

// runBlocks returns every `run:` script in src. Comments inside a script are
// kept: Actions substitutes an expression into a shell comment as readily as
// into a command, so a comment is not somewhere one is safe.
func runBlocks(file, src string) []runBlock {
	lines := strings.Split(src, "\n")
	var out []runBlock
	for i := 0; i < len(lines); i++ {
		m := runKey.FindStringSubmatch(lines[i])
		if m == nil || strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			continue
		}
		key := len(m[1]) + len(m[2])
		rest := strings.TrimSpace(m[3])
		if rest != "" && rest != "|" && rest != ">" {
			out = append(out, runBlock{file, i + 1, rest})
			continue
		}
		var b strings.Builder
		j := i + 1
		for ; j < len(lines); j++ {
			l := lines[j]
			if strings.TrimSpace(l) != "" && len(l)-len(strings.TrimLeft(l, " ")) <= key {
				break
			}
			b.WriteString(l + "\n")
		}
		out = append(out, runBlock{file, i + 1, b.String()})
		i = j - 1
	}
	return out
}

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// No script interpolates an expression a dispatcher or an event author can
// write. Actions pastes the value into the script's text before the shell
// reads it, so `10m; anything` as a fuzztime ran `anything`. Until 2026-10-06
// fuzz-nightly.yml did exactly that with inputs.fuzztime. A value goes
// through env: and the script reads it as a variable.
func TestNoWorkflowScriptInterpolatesAnInputOrAnEvent(t *testing.T) {
	untrusted := regexp.MustCompile(`\$\{\{[^}]*\b(inputs|github\.event)\.`)
	files, err := filepath.Glob(filepath.Join(".github", "workflows", "*.yml"))
	must(t, err)
	if len(files) < 4 {
		t.Fatalf("found %d workflows, want at least ci, fuzz-nightly, install and release", len(files))
	}
	sawFuzz := false
	for _, f := range files {
		name := filepath.Base(f)
		blocks := runBlocks(name, readWorkflow(t, name))
		if len(blocks) == 0 {
			t.Errorf("%s: found no run: scripts, so this test read nothing", name)
		}
		for _, b := range blocks {
			if m := untrusted.FindString(b.text); m != "" {
				t.Errorf("%s:%d: a run: script interpolates %s...; pass it through env: and read the variable",
					b.file, b.line, m)
			}
			if name == "fuzz-nightly.yml" && strings.Contains(b.text, "make fuzz") {
				sawFuzz = true
			}
		}
	}
	if !sawFuzz {
		t.Error("fuzz-nightly.yml's fuzz step was not found among its run: scripts")
	}
}

// fuzzStep is the script fuzz-nightly.yml runs to fuzz one target.
func fuzzStep(t *testing.T) string {
	t.Helper()
	for _, b := range runBlocks("fuzz-nightly.yml", readWorkflow(t, "fuzz-nightly.yml")) {
		if strings.Contains(b.text, "make fuzz") {
			return b.text
		}
	}
	t.Fatal("fuzz-nightly.yml has no run: script that calls make fuzz")
	return ""
}

// The fuzz step checks its time budget before it hands it to make, and fails
// the job naming the value when it is not a whole number of seconds, minutes or
// hours. Run here as the workflow holds it, under bash as Actions runs it on
// Linux, with make replaced by a stub that records what it was asked, since a
// dispatch cannot be made from a test.
func TestTheFuzzTimeIsCheckedBeforeMake(t *testing.T) {
	needsBash(t)
	script := fuzzStep(t)
	dir := t.TempDir()
	stub := filepath.Join(dir, "bin")
	must(t, os.Mkdir(stub, 0o755))
	must(t, os.WriteFile(filepath.Join(stub, "make"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$MAKE_LOG\"\n"), 0o755))
	file := filepath.Join(dir, "step.sh")
	must(t, os.WriteFile(file, []byte(script), 0o644))
	marker := filepath.Join(dir, "ran")

	for _, c := range []struct {
		name, fuzztime string
		ok             bool
	}{
		{"minutes", "10m", true},
		{"seconds", "20s", true},
		{"hours", "2h", true},
		{"empty", "", false},
		{"no unit", "10", false},
		{"a unit Go does not take here", "1d", false},
		{"a leading space", " 10m", false},
		{"a command after it", "10m; touch " + marker, false},
		{"a command on a second line", "10m\ntouch " + marker, false},
		{"a substitution", "$(touch " + marker + ")", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := filepath.Join(dir, "make.log")
			_ = os.Remove(log)
			cmd := exec.Command("bash", "-e", file)
			cmd.Dir = dir
			cmd.Env = childEnv("PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"),
				"MAKE_LOG="+log, "TARGET=FuzzSomething", "FUZZTIME="+c.fuzztime)
			out, err := cmd.CombinedOutput()
			called, _ := os.ReadFile(log)
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Fatalf("the value ran as a command:\n%s", out)
			}
			if c.ok {
				if err != nil {
					t.Fatalf("exit %v for %q:\n%s", err, c.fuzztime, out)
				}
				if want := "fuzz FUZZTARGET=FuzzSomething FUZZTIME=" + c.fuzztime + "\n"; string(called) != want {
					t.Errorf("make was given %q, want %q", called, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("%q was accepted:\n%s", c.fuzztime, out)
			}
			if len(called) != 0 {
				t.Errorf("make ran with %q for a refused value", called)
			}
			if !strings.Contains(string(out), "fuzztime must be a whole number of seconds, minutes or hours") {
				t.Errorf("the failure does not say what a fuzztime must be:\n%s", out)
			}
		})
	}
}

// A release builds from nothing it did not make. setup-go caches by default,
// and since its v6 the key is go.mod, which this repository has, so until
// 2026-10-06 the release's gate and its build both restored a Go cache that
// another run on main had written. Every setup-go in release.yml says
// cache: false.
func TestTheReleaseBuildsWithoutACache(t *testing.T) {
	lines := strings.Split(readWorkflow(t, "release.yml"), "\n")
	found := 0
	for i, l := range lines {
		if !strings.Contains(l, "uses: actions/setup-go@") || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		found++
		dash := strings.Index(l, "- ")
		off := false
		for _, s := range lines[i+1:] {
			if strings.TrimSpace(s) != "" && len(s)-len(strings.TrimLeft(s, " ")) <= dash {
				break
			}
			if strings.TrimSpace(s) == "cache: false" {
				off = true
			}
		}
		if !off {
			t.Errorf("release.yml:%d: setup-go without cache: false", i+1)
		}
	}
	if found < 2 {
		t.Errorf("found %d setup-go steps in release.yml, want the gate's and the build's", found)
	}
}

// Scoop's installer is fetched from a commit, its bytes are checked, and only
// then is it run. get.scoop.sh answers with the installer repository's master
// branch, which moves, and until 2026-10-06 install.yml piped whatever that was
// straight into Invoke-Expression.
func TestTheScoopInstallerIsPinned(t *testing.T) {
	src := readWorkflow(t, "install.yml")
	for i, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if strings.Contains(l, "get.scoop.sh") {
			t.Errorf("install.yml:%d: fetches get.scoop.sh, a moving branch: %s", i+1, strings.TrimSpace(l))
		}
		if strings.Contains(l, "Invoke-Expression") && (strings.Contains(l, "Invoke-RestMethod") || strings.Contains(l, "irm ")) {
			t.Errorf("install.yml:%d: runs a download without reading it first: %s", i+1, strings.TrimSpace(l))
		}
	}
	pinned := regexp.MustCompile(`https://raw\.githubusercontent\.com/ScoopInstaller/Install/[0-9a-f]{40}/install\.ps1`)
	if n := len(pinned.FindAllString(src, -1)); n != 1 {
		t.Errorf("install.yml names the installer at a commit %d times, want once", n)
	}
	if !regexp.MustCompile(`(?m)^\s+SCOOP_INSTALLER_SHA256: [0-9a-f]{64}$`).MatchString(src) {
		t.Error("install.yml pins no SHA-256 for the installer")
	}
	var run string
	for _, b := range runBlocks("install.yml", src) {
		if strings.Contains(b.text, "-RunAsAdmin") {
			run = b.text
		}
	}
	if run == "" {
		t.Fatal("install.yml has no run: script that runs the Scoop installer")
	}
	check, start := strings.Index(run, "Get-FileHash"), strings.Index(run, "-RunAsAdmin")
	if check < 0 || check > start || !strings.Contains(run[check:start], "throw") {
		t.Errorf("the installer is run before its SHA-256 is compared and a mismatch thrown:\n%s", run)
	}
}
