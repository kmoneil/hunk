package main

// A caught patch's output (§4.3, issue #89). In a deliberate-break run the
// question after "was it caught" is "by what", and the field writes commands
// whose output says, `grep "error: .*failed"` and the like; --each printed it
// under none of the caught patches, the ones it was written for. Now
// --verify-lines, given, prints every command's tail, and --json carries every
// command's output whatever the flags. Not given, the text is as it was: a
// passing suite's forty lines under every catch would bury the misses.

import (
	"bytes"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestEachVerifyLinesGivenPrintsEveryTail is the rule through the real CLI:
// sayingSuite catches the alpha patch, saying "FAIL: alpha", and lets the
// beta patch through, saying "ok: alpha".
func TestEachVerifyLinesGivenPrintsEveryTail(t *testing.T) {
	cases := []struct {
		name        string
		flags       []string
		caughtLine  bool // the caught patch's line prints
		caughtTail  bool // and its tail under it
		passingTail bool
	}{
		{"not given: the caught patch's tail is not printed", nil, true, false, true},
		{"given: printed under the caught patch's line", []string{"--verify-lines", "5"}, true, true, true},
		{"given at the default's value: printed, since it is the flag that asks", []string{"--verify-lines", "40"}, true, true, true},
		{"given with --quiet: --quiet decides the lines, so the caught one stays out", []string{"--quiet", "--verify-lines", "5"}, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, patches := eachFixture(t)
			before := snapshot(t, root)
			args := append(append([]string{"--try", sayingSuite}, c.flags...), "--each", patches[0], patches[1])
			code, out, errOut := runCLI(t, root, args, "")
			if code != exitVerifyFailed {
				t.Fatalf("exit %d, want 3: %s%s", code, out, errOut)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
			caught := "tried " + patches[0] + "  the command exited 1 ("
			if got := strings.Contains(errOut, caught); got != c.caughtLine {
				t.Errorf("the caught line printed: %v, want %v: %q", got, c.caughtLine, errOut)
			}
			// The caught tail, where it prints, is directly under its line.
			under := "\n\n$ " + sayingSuite + "\nFAIL: alpha\n(last 1 of 1 lines)\ntried " + patches[1]
			if got := strings.Contains(errOut, under); got != c.caughtTail {
				t.Errorf("the caught tail under its line: %v, want %v: %q", got, c.caughtTail, errOut)
			}
			if !c.caughtTail && strings.Contains(errOut, "\nFAIL: alpha\n") {
				t.Errorf("the caught tail printed somewhere: %q", errOut)
			}
			if got := strings.Contains(errOut, "\nok: alpha\n(last 1 of 1 lines)\n"); got != c.passingTail {
				t.Errorf("the passing tail printed: %v, want %v: %q", got, c.passingTail, errOut)
			}
		})
	}

	t.Run("--verify-lines cuts a caught tail as it cuts any", func(t *testing.T) {
		root, patches := eachFixture(t)
		_, _, errOut := runCLI(t, root, []string{
			"--try", "grep -q alpha a.txt || { printf 'one\\ntwo\\nthree\\n'; exit 1; }",
			"--verify-lines", "2", "--each", patches[0],
		}, "")
		if !strings.Contains(errOut, "\ntwo\nthree\n(last 2 of 3 lines)\n") || strings.Contains(errOut, "\none\n") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("the issue's repro, on a command that passes with no patch applied", func(t *testing.T) {
		// The issue's own command, 'echo caught-output-line; exit 1', fails on
		// the tree as it is, which #90 refuses; this is it as a suite.
		root := cliTree(t, map[string]string{"f.txt": "a\n"})
		p := writePatch(t, t.TempDir(), "p1.hunk", "@@ file f.txt\n@@ old\na\n@@ new\nb\n")
		command := "grep -qx a f.txt || { echo caught-output-line; exit 1; }"
		code, out, errOut := runCLI(t, root, []string{"--try", command, "--verify-lines", "5", "--each", p}, "")
		if code != exitOK || !strings.Contains(errOut, "\ncaught-output-line\n") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
		code, out, _ = runCLI(t, root, []string{"--try", command, "--json", "--each", p}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		if got := eachJSONOf(t, out).Each.Patches[0].Output; len(got) != 1 || got[0] != "caught-output-line" {
			t.Errorf("output = %q", got)
		}
	})

	t.Run("under --build: a patch that did not build shows the build's, a caught one the command's", func(t *testing.T) {
		root, patches := eachFixture(t)
		_, _, errOut := runCLI(t, root, []string{
			"--try", sayingSuite, "--verify-lines", "5",
			"--build", "grep -q BETA b.txt && { echo 'b.txt:1: error: unused'; exit 1; }; exit 0",
			"--each", patches[0], patches[1],
		}, "")
		if !strings.Contains(errOut, "\nFAIL: alpha\n") || !strings.Contains(errOut, "\nb.txt:1: error: unused\n") {
			t.Errorf("stderr = %q", errOut)
		}
		if n := strings.Count(errOut, "$ "); n != 2 {
			t.Errorf("%d transcripts, want 2: the caught command's and the failed build's", n)
		}
	})

	t.Run("json: the flag cuts the output, and does not decide whether it is there", func(t *testing.T) {
		root, patches := eachFixture(t)
		_, out, _ := runCLI(t, root, []string{
			"--json", "--try", "grep -q alpha a.txt || { printf 'one\\ntwo\\n'; exit 1; }",
			"--verify-lines", "1", "--each", patches[0],
		}, "")
		if got := eachJSONOf(t, out).Each.Patches[0].Output; len(got) != 1 || got[0] != "two" {
			t.Errorf("output = %q", got)
		}
	})
}

// TestEachLineTailRule is writeEachLine's rule as a table: for every way a
// patch's turn can end, whether a transcript prints under its line, with
// --verify-lines given and not.
func TestEachLineTailRule(t *testing.T) {
	tail := func(v *Verify) *Verify {
		v.Command, v.Tail, v.TotalLines = "zig build test", []string{"error: 'faults.test.reach' failed:"}, 1
		return v
	}
	cases := []struct {
		name          string
		ep            *eachPatch
		without, with bool
	}{
		{"caught", &eachPatch{v: tail(&Verify{Ran: true, Try: true, Status: 1})}, false, true},
		{"exited 0", &eachPatch{v: tail(&Verify{Ran: true, Try: true, OK: true})}, true, true},
		{"timed out", &eachPatch{v: tail(&Verify{Ran: true, Try: true, Status: exitTimedOut, TimedOut: time.Second})}, true, true},
		{"interrupted while it ran", &eachPatch{v: tail(&Verify{Ran: true, Try: true, Status: -1, Signal: syscall.SIGTERM})}, false, true},
		{"interrupted before it ran", &eachPatch{v: &Verify{Try: true, Signal: syscall.SIGTERM}}, false, false},
		{"could not start", &eachPatch{v: &Verify{Try: true}}, false, false},
		{"did not build", &eachPatch{build: tail(&Verify{Ran: true, Status: 1})}, true, true},
		{"its build interrupted", &eachPatch{build: tail(&Verify{Ran: true, Status: -1, Signal: syscall.SIGTERM})}, false, false},
	}
	for _, c := range cases {
		for _, every := range []bool{false, true} {
			want := c.without
			if every {
				want = c.with
			}
			c.ep.name = "breaks/01.hunk"
			var w bytes.Buffer
			writeEachLine(eachConfig{everyTail: every}, &w, 14, c.ep)
			if got := strings.Contains(w.String(), "\n$ "); got != want {
				t.Errorf("%s, --verify-lines given %v: transcript %v, want %v: %q", c.name, every, got, want, w.String())
			}
		}
	}
}
