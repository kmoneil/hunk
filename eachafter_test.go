package main

// The runs on the tree as it is after the last patch (§4.3, issue #110). The
// runs before the first patch say the command could pass then, and until
// #110 nothing said it still could at the end: a linter upgraded mid-run,
// failing on a file no patch touched, made every later patch a catch, and the
// summary called them all caught at exit 0. The rows are what the end can
// find, the command failing, the command hanging, the build failing, the tree
// still passing, and where it must not run: no catch to doubt, or a run that
// stopped.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// brokeAfter is a suite for eachFixture whose surroundings break as it runs:
// it passes on the tree as it is the first time, leaves broke behind, and from
// then on fails whatever the tree holds, saying why, as the issue's linter
// did. So every patch reads as caught, and none of them by the suite.
func brokeAfter(broke string) string {
	return fmt.Sprintf("if [ -e '%s' ]; then echo 'lint: a new rule fires'; exit 1; fi; touch '%s'; %s", broke, broke, catchesAll)
}

// fromRun is catchesAll, counted into mark, that does then first from its nth
// run on: 1 is the run on the tree as it is, 2 the first patch's, and so on.
func fromRun(mark string, n int, then string) string {
	return counted(mark, fmt.Sprintf(`[ "$(grep -c . '%s')" -ge %d ] && { %s; }; %s`, mark, n, then, catchesAll))
}

// summaryLine is the line of out that begins with the run's summary, or "".
func summaryLine(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "tried ") && strings.Contains(l, " and put back every file; ") {
			return l
		}
	}
	return ""
}

// TestEachChecksTheTreeAgainAfterTheLastPatch: after the last patch the build,
// where there is one, and the command run on the tree as it is once more,
// when a patch was caught; a run that fails there is exit 3, says why above
// the summary with the failed run's output, and says so in the summary.
func TestEachChecksTheTreeAgainAfterTheLastPatch(t *testing.T) {
	const doubt = ", and it passed before the first: something besides the patches makes it fail, so any catch may be that"
	type run struct {
		args       []string
		ran, built string // the command's and the build's markers, built "" without --build
		failed     string // the command whose output the story shows, "" for none
	}
	cases := []struct {
		name       string
		setup      func(t *testing.T, patches []string) run
		code       int
		ran, built int    // how often the command and the build ran
		story      string // the line above the summary, after "hunk: ", "" for none
		shown      string // a line of the failed run's output the story shows
		verdict    string // the summary up to its seconds
	}{
		{
			name: "the issue's repro: the suite starts failing mid-run, and every later patch reads as caught",
			setup: func(t *testing.T, patches []string) run {
				ran, broke := pidFile(t, "ran"), pidFile(t, "broke")
				cmd := counted(ran, brokeAfter(broke))
				return run{args: []string{"--try", cmd, "--each", patches[0], patches[1]}, ran: ran, failed: cmd}
			},
			code: exitVerifyFailed, ran: 4,
			story:   "after the last patch, the command exited 1 with no patch applied" + doubt,
			shown:   "lint: a new rule fires",
			verdict: "tried 2 patches and put back every file; every command exited nonzero, but the command now fails with no patch applied (",
		},
		{
			name: "the suite hangs after the last patch: timed out, and the same doubt",
			setup: func(t *testing.T, patches []string) run {
				ran := pidFile(t, "ran")
				cmd := fromRun(ran, 3, "echo hanging; sleep 30")
				// Two seconds, not less: the run before the first patch and the
				// patch's own have to finish inside it, on Windows' sh too.
				return run{args: []string{"--try", cmd, "--timeout", "2s", "--each", patches[0]}, ran: ran, failed: cmd}
			},
			code: exitVerifyFailed, ran: 3,
			story:   "after the last patch, the command timed out after 2s with no patch applied" + doubt,
			shown:   "hanging",
			verdict: "tried 1 patch and put back every file; every command exited nonzero, but the command now fails with no patch applied (",
		},
		{
			name: "the build fails after the last patch: it ran first, and the command does not run again",
			setup: func(t *testing.T, patches []string) run {
				ran, built, broke := pidFile(t, "ran"), pidFile(t, "built"), pidFile(t, "broke")
				build := counted(built, fmt.Sprintf("[ -e '%s' ] && { echo 'error: a warning is an error now'; exit 2; }; exit 0", broke))
				cmd := counted(ran, fmt.Sprintf("grep -q alpha a.txt || touch '%s'; %s", broke, catchesAll))
				return run{args: []string{"--try", cmd, "--build", build, "--each", patches[0]}, ran: ran, built: built, failed: build}
			},
			code: exitVerifyFailed, ran: 2, built: 3,
			story: "after the last patch, the build exited 2 with no patch applied, and it passed before the first: " +
				"something besides the patches makes it fail, so the command could not run again to confirm the catches",
			shown:   "error: a warning is an error now",
			verdict: "tried 1 patch and put back every file; every command exited nonzero, but the build now fails with no patch applied (",
		},
		{
			name: "the tree still passes after the last patch: exit 0, and nothing more is said",
			setup: func(t *testing.T, patches []string) run {
				ran, built := pidFile(t, "ran"), pidFile(t, "built")
				return run{args: []string{"--try", counted(ran, catchesAll), "--build", counted(built, "true"), "--each", patches[0], patches[1]}, ran: ran, built: built}
			},
			code: exitOK, ran: 4, built: 4,
			verdict: "tried 2 patches and put back every file; every command exited nonzero (",
		},
		{
			name: "a catch beside a break that passed: the run after the last still happens, and passes",
			setup: func(t *testing.T, patches []string) run {
				ran := pidFile(t, "ran")
				return run{args: []string{"--try", counted(ran, sayingSuite), "--each", patches[0], patches[1]}, ran: ran}
			},
			code: exitVerifyFailed, ran: 4,
			verdict: "tried 2 patches and put back every file; 1 exited nonzero, 1 exited 0 (",
		},
		{
			name: "no patch caught: nothing to doubt, so no run after the last",
			setup: func(t *testing.T, patches []string) run {
				ran, built := pidFile(t, "ran"), pidFile(t, "built")
				return run{args: []string{"--try", counted(ran, "grep -q . a.txt"), "--build", counted(built, "true"), "--each", patches[0], patches[1]}, ran: ran, built: built}
			},
			code: exitVerifyFailed, ran: 3, built: 3,
			verdict: "tried 2 patches and put back every file; 2 exited 0 (",
		},
		{
			name: "no patch caught because none built: no run after the last",
			setup: func(t *testing.T, patches []string) run {
				ran, built := pidFile(t, "ran"), pidFile(t, "built")
				return run{args: []string{"--try", counted(ran, catchesAll), "--build", counted(built, catchesAll), "--each", patches[0]}, ran: ran, built: built}
			},
			code: exitVerifyFailed, ran: 1, built: 2,
			verdict: "tried 1 patch and put back every file; 1 did not build (",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, patches := eachFixture(t)
			r := c.setup(t, patches)
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, r.args, "")
			if code != c.code {
				t.Fatalf("exit %d, want %d:\n%s%s", code, c.code, out, errOut)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
			if n := markerLines(t, r.ran); n != c.ran {
				t.Errorf("the command ran %d times, want %d", n, c.ran)
			}
			if r.built != "" {
				if n := markerLines(t, r.built); n != c.built {
					t.Errorf("the build ran %d times, want %d", n, c.built)
				}
			}
			summaryIn := errOut
			if c.code == exitOK {
				summaryIn = out
			}
			if l := summaryLine(summaryIn); !strings.HasPrefix(l, c.verdict) {
				t.Errorf("the summary = %q, want it to begin %q:\n%s%s", l, c.verdict, out, errOut)
			}
			if c.story == "" {
				if strings.Contains(errOut, "after the last patch") {
					t.Errorf("a story with nothing to tell: %q", errOut)
				}
				return
			}
			if !strings.Contains(errOut, "\nhunk: "+c.story+"\n") {
				t.Errorf("stderr lacks the line %q:\n%s", "hunk: "+c.story, errOut)
			}
			if !strings.Contains(errOut, "\n$ "+r.failed+"\n") || !strings.Contains(errOut, "\n"+c.shown+"\n") {
				t.Errorf("the failed run and its output %q are not shown:\n%s", c.shown, errOut)
			}
			story, summary := strings.Index(errOut, "hunk: "+c.story), strings.Index(errOut, summaryLine(errOut))
			if story > summary {
				t.Errorf("the story comes after the summary it explains:\n%s", errOut)
			}
		})
	}

	t.Run("a run that stopped after a catch: no run after the last", func(t *testing.T) {
		root, patches := eachFixture(t)
		ran := pidFile(t, "ran")
		// The first patch is caught and put back. Under the second the suite
		// writes over the file the patch made, which is a file hunk will not
		// put back: the run stops there with a catch counted, and the tree is
		// not one to run anything on. Until a mutant survived it, this test
		// stopped at the first patch, where nothing was caught, and could not
		// tell whether a stop was what kept the check from running.
		cmd := counted(ran, "grep -q beta b.txt || { echo other > b.txt; exit 1; }; "+catchesAll)
		code, out, errOut := runCLI(t, root, []string{"--try", cmd, "--each", patches[0], patches[1]}, "")
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4:\n%s%s", code, out, errOut)
		}
		if l := eachLine(errOut, patches[0]); !strings.HasPrefix(l, "the command exited 1 ") {
			t.Errorf("the first patch was not caught: %q", l)
		}
		if n := markerLines(t, ran); n != 3 {
			t.Errorf("the command ran %d times, want 3: on the tree as it is and under each patch, and not after a stop", n)
		}
		if strings.Contains(errOut, "after the last patch") || strings.Contains(errOut, ", but ") {
			t.Errorf("a stopped run spoke of the run after the last patch:\n%s", errOut)
		}
	})

	t.Run("--quiet still prints the story and the summary: they are a failure", func(t *testing.T) {
		root, patches := eachFixture(t)
		broke := pidFile(t, "broke")
		code, out, errOut := runCLI(t, root, []string{"--quiet", "--try", brokeAfter(broke), "--each", patches[0], patches[1]}, "")
		if code != exitVerifyFailed || out != "" {
			t.Fatalf("exit %d, stdout %q, want 3 and nothing: %s", code, out, errOut)
		}
		if !strings.Contains(errOut, "hunk: after the last patch, the command exited 1 with no patch applied"+doubt+"\n") ||
			!strings.HasPrefix(summaryLine(errOut), "tried 2 patches and put back every file; every command exited nonzero, but the command now fails") {
			t.Errorf("stderr = %q", errOut)
		}
		if eachLine(errOut, patches[0]) != "" {
			t.Errorf("--quiet printed a caught patch's line: %q", errOut)
		}
	})
}

// TestEachInterruptedDuringTheRunAfterTheLastPatch: a signal while the
// command runs on the tree as it is after the last patch ends it and the run,
// and hunk dies of the signal, the summary, or --json's error, saying the
// check was cut short.
func TestEachInterruptedDuringTheRunAfterTheLastPatch(t *testing.T) {
	needsSignals(t)
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json %v", asJSON), func(t *testing.T) {
			root, patches := eachFixture(t)
			ran, started := pidFile(t, "ran"), pidFile(t, "started")
			go func() {
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
					if _, err := os.Stat(started); err == nil {
						signalSelf(t, syscall.SIGTERM)
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
			args := []string{"--root", root, "--try", fromRun(ran, 3, "echo go > '"+started+"'; sleep 30"), "--each", patches[0]}
			if asJSON {
				args = append([]string{"--json"}, args...)
			}
			before := snapshot(t, root)
			var out, errOut bytes.Buffer
			var sig os.Signal
			code := invoke(args, strings.NewReader(""), &out, &errOut, &sig)
			if code != 143 {
				t.Fatalf("exit %d, want 143: %s%s", code, out.String(), errOut.String())
			}
			if sig != syscall.SIGTERM {
				t.Errorf("main would not re-raise SIGTERM: %v", sig)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
			if asJSON {
				var r eachAfterJSONReport
				if err := json.Unmarshal(out.Bytes(), &r); err != nil {
					t.Fatalf("unmarshal: %v\n%s", err, out.String())
				}
				if r.OK || r.Exit != 143 || r.Error != "the check with no patch applied after the last patch was interrupted by SIGTERM" {
					t.Errorf("ok %v exit %d error %q", r.OK, r.Exit, r.Error)
				}
				return
			}
			if l := eachLine(errOut.String(), patches[0]); !strings.HasPrefix(l, "the command exited 1 ") {
				t.Errorf("the patch's line = %q: %q", l, errOut.String())
			}
			want := "tried 1 patch and put back every file; every command exited nonzero, but the check with no patch applied was interrupted by SIGTERM ("
			if l := summaryLine(errOut.String()); !strings.HasPrefix(l, want) {
				t.Errorf("the summary = %q, want it to begin %q", l, want)
			}
		})
	}
}

// TestEachCannotStartAfterTheLastPatch: a shell that cannot start the build
// or the command on the tree as it is after the last patch is exit 5, as the
// same failure before the first patch is, the summary, or --json's error,
// naming which. The patch's suite swaps the sh on PATH for one that cannot
// run, so the run after the last patch is the first to meet it.
func TestEachCannotStartAfterTheLastPatch(t *testing.T) {
	needsShellSwap(t)
	sh, err := exec.LookPath("sh")
	must(t, err)
	for _, c := range []struct {
		name   string
		build  bool
		asJSON bool
		who    string
	}{
		{"the command", false, false, "the --try command"},
		{"the build, which runs first", true, false, "the --build command"},
		{"json, the command", false, true, "the --try command"},
		{"json, the build", true, true, "the --build command"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, patches := eachFixture(t)
			dir := t.TempDir()
			must(t, os.Symlink(sh, filepath.Join(dir, "sh")))
			must(t, os.WriteFile(filepath.Join(dir, "sh.broken"), []byte("not a program\n"), 0o755))
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			swap := fmt.Sprintf("grep -q alpha a.txt || mv -f '%s' '%s'; %s",
				filepath.Join(dir, "sh.broken"), filepath.Join(dir, "sh"), catchesAll)
			args := []string{"--try", swap}
			if c.build {
				args = append(args, "--build", "true")
			}
			if c.asJSON {
				args = append(args, "--json")
			}
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, append(args, "--each", patches[0]), "")
			if code != exitIO {
				t.Fatalf("exit %d, want 5:\n%s%s", code, out, errOut)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
			why := "could not run " + c.who + " with no patch applied after the last patch: "
			if c.asJSON {
				var r eachAfterJSONReport
				if err := json.Unmarshal([]byte(out), &r); err != nil {
					t.Fatalf("unmarshal: %v\n%s", err, out)
				}
				if r.OK || r.Exit != exitIO || !strings.HasPrefix(r.Error, why) {
					t.Errorf("ok %v exit %d error %q", r.OK, r.Exit, r.Error)
				}
				return
			}
			if !strings.Contains(errOut, "\nhunk: "+why) {
				t.Errorf("stderr lacks why:\n%s", errOut)
			}
			want := "tried 1 patch and put back every file; every command exited nonzero, but " + c.who + " could not start with no patch applied ("
			if l := summaryLine(errOut); !strings.HasPrefix(l, want) {
				t.Errorf("the summary = %q, want it to begin %q", l, want)
			}
		})
	}
}

// eachAfterJSONReport is what --json printed, the fields these tests read.
type eachAfterJSONReport struct {
	OK    bool   `json:"ok"`
	Exit  int    `json:"exit"`
	Error string `json:"error"`
	Each  struct {
		Baseline         *jsonBuildSeen `json:"baseline"`
		TryBaseline      *jsonBuildSeen `json:"try_baseline"`
		BaselineAfter    *jsonBuildSeen `json:"baseline_after"`
		TryBaselineAfter *jsonBuildSeen `json:"try_baseline_after"`
	} `json:"each"`
}

// TestEachAfterTheLastPatchJSON: the runs after the last patch are
// baseline_after and try_baseline_after, there only where they ran, and one
// that did not pass is the run's error.
func TestEachAfterTheLastPatchJSON(t *testing.T) {
	decode := func(t *testing.T, out string) eachAfterJSONReport {
		t.Helper()
		var r eachAfterJSONReport
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, out)
		}
		return r
	}

	t.Run("the command fails after the last patch", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--json", "--try", brokeAfter(pidFile(t, "broke")), "--each", patches[0], patches[1]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		r := decode(t, out)
		want := "after the last patch, the command exited 1 with no patch applied, and it passed before the first: something besides the patches makes it fail, so any catch may be that"
		if r.OK || r.Exit != exitVerifyFailed || r.Error != want {
			t.Errorf("ok %v exit %d error %q", r.OK, r.Exit, r.Error)
		}
		if b := r.Each.TryBaseline; b == nil || !b.OK {
			t.Errorf("try_baseline = %+v", b)
		}
		a := r.Each.TryBaselineAfter
		if a == nil || !a.Ran || a.OK || a.Status != 1 || len(a.Output) != 1 || a.Output[0] != "lint: a new rule fires" {
			t.Errorf("try_baseline_after = %+v", a)
		}
		if r.Each.BaselineAfter != nil || strings.Contains(out, `"baseline_after"`) {
			t.Errorf("a run without --build has the build's run after the last patch: %s", out)
		}
	})

	t.Run("the command hangs after the last patch: 124 and its timeout", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{
			"--json", "--try", fromRun(pidFile(t, "ran"), 3, "sleep 30"), "--timeout", "2s", "--each", patches[0],
		}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, out)
		}
		if a := decode(t, out).Each.TryBaselineAfter; a == nil || a.OK || a.Status != exitTimedOut || a.TimedOut != 2 {
			t.Errorf("try_baseline_after = %+v", a)
		}
	})

	t.Run("the build fails after the last patch: its run and output, the command's absent", func(t *testing.T) {
		root, patches := eachFixture(t)
		broke := pidFile(t, "broke")
		code, out, _ := runCLI(t, root, []string{
			"--json",
			"--try", fmt.Sprintf("grep -q alpha a.txt || touch '%s'; %s", broke, catchesAll),
			"--build", fmt.Sprintf("[ -e '%s' ] && { echo 'error: a warning is an error now'; exit 2; }; exit 0", broke),
			"--each", patches[0],
		}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := decode(t, out)
		if !strings.HasPrefix(r.Error, "after the last patch, the build exited 2 with no patch applied") {
			t.Errorf("error %q", r.Error)
		}
		b := r.Each.BaselineAfter
		if b == nil || b.OK || b.Status != 2 || len(b.Output) != 1 || b.Output[0] != "error: a warning is an error now" {
			t.Errorf("baseline_after = %+v", b)
		}
		if r.Each.TryBaselineAfter != nil {
			t.Errorf("the command ran after a build that failed: %+v", r.Each.TryBaselineAfter)
		}
	})

	t.Run("the tree passes after the last patch: both runs there, no error", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{"--json", "--try", catchesAll, "--build", "true", "--each", patches[0]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := decode(t, out)
		if !r.OK || r.Error != "" {
			t.Errorf("ok %v error %q", r.OK, r.Error)
		}
		if b, a := r.Each.BaselineAfter, r.Each.TryBaselineAfter; b == nil || !b.OK || a == nil || !a.Ran || !a.OK || a.Status != 0 {
			t.Errorf("baseline_after %+v, try_baseline_after %+v", b, a)
		}
	})

	t.Run("no patch caught: neither run, and neither key", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{"--json", "--try", "grep -q . a.txt", "--build", "true", "--each", patches[0]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, out)
		}
		if strings.Contains(out, `"baseline_after"`) || strings.Contains(out, `"try_baseline_after"`) {
			t.Errorf("runs after the last patch with nothing caught: %s", out)
		}
	})
}

// TestEachAfterReportGoldens: the text and the object for a run whose tree
// fails after the last patch, rendered from constructed outcomes with fixed
// seconds, as TestEachReportGoldens renders the rest.
func TestEachAfterReportGoldens(t *testing.T) {
	caught := func(seconds float64) *Verify {
		return &Verify{Ran: true, Try: true, Command: "zig build test", Status: 1, Seconds: seconds, Applied: 1, RolledBack: 1}
	}
	passing := &Verify{Ran: true, OK: true, Try: true, Command: "zig build test", Seconds: 140.2, TotalLines: 1, Tail: []string{"All 212 tests passed."}}
	failing := &Verify{
		Ran: true, Try: true, Command: "zig build test", Status: 1, Seconds: 4.1, TotalLines: 2,
		Tail: []string{"src/net.zig:40:5: error: lint: 'std.debug.print' in library code", "error: the following command failed with 1 compilation errors:"},
	}
	patches := func() []*eachPatch {
		return []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", v: caught(139.8)},
			{name: "breaks/02-texture-not-hashed.hunk", v: caught(4.0)},
			{name: "breaks/03-loose.hunk", v: caught(4.2)},
		}
	}
	o := eachOutcome{tried: 3, stopAt: -1, seconds: 292.3, bases: eachBaselines{command: passing, commandAfter: failing}}

	t.Run("text", func(t *testing.T) {
		var out, errOut bytes.Buffer
		cfg := eachConfig{command: "zig build test"}
		ps := patches()
		width := eachNameWidth(ps)
		for _, ep := range ps {
			writeEachLine(cfg, &errOut, width, ep)
		}
		code := eachExit(ps, o)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		writeEachEnd(cfg, ps, o, code, &out, &errOut)
		golden(t, "report-each-after", errOut.String()+out.String())
	})

	t.Run("text, the build", func(t *testing.T) {
		var out, errOut bytes.Buffer
		build := &Verify{
			Ran: true, Command: "zig build", Status: 2, Seconds: 3.2, TotalLines: 1,
			Tail: []string{"build.zig.zon:7:20: error: hash mismatch: manifest declares 1220ab, found 1220cd"},
		}
		ob := o
		ob.bases = eachBaselines{build: &Verify{Ran: true, OK: true, Command: "zig build"}, command: passing, buildAfter: build}
		cfg := eachConfig{command: "zig build test", build: "zig build"}
		code := eachExit(patches(), ob)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		writeEachEnd(cfg, patches(), ob, code, &out, &errOut)
		golden(t, "report-each-after-build", errOut.String()+out.String())
	})

	t.Run("json", func(t *testing.T) {
		var out, errOut bytes.Buffer
		cfg := eachConfig{command: "zig build test", asJSON: true}
		ps := patches()
		code := eachJSON(cfg, ps, o.bases, eachExit(ps, o), eachStopReason(ps, o), &out, &errOut)
		if code != exitVerifyFailed || errOut.Len() > 0 {
			t.Fatalf("exit %d, stderr %q", code, errOut.String())
		}
		golden(t, "report-each-after-json", out.String())
	})
}
