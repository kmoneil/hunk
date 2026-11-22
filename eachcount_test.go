package main

// --count RE under --try --each (§4.3, issue #189). A mutation run has to
// record how hard each break was caught, the number of failing tests, and
// --each said only that the command exited nonzero. Every such run sent each
// patch's output to a file named by HUNK_PATCH and ended in a loop of grep -c.
// Now each run of the command has the lines of its whole output that match
// counted, on its line and in --json, and the summary counts the catches with
// no line matched: a break stopped by something other than a failing test.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// countSuite is the issue's command over n.txt, which holds 1 on the tree as
// it is: it prints a "--- FAIL" line for each step the number is above 1, and
// passes only at 1. So the patch that writes 3 fails two tests, and the one
// that writes 0 exits nonzero with no test failed, a break caught by something
// else. other.txt it never reads, so a patch to it is a break that passes.
const countSuite = `v=$(tr -dc 0-9 < n.txt); i=1; while [ "$i" -lt "$v" ]; do echo "    --- FAIL: T$i (0.00s)"; i=$((i+1)); done; [ "$v" = 1 ]`

// failLines is the pattern the tests count: go test's failing tests,
// subtests' indented lines included.
const failLines = `^ *--- FAIL`

// countFixture is the tree countSuite reads and one patch file per break,
// each named for what it writes: two.hunk fails one test, three.hunk two,
// zero.hunk none, and miss.hunk edits what the suite does not read.
func countFixture(t *testing.T) (root string, patch map[string]string) {
	t.Helper()
	root = cliTree(t, map[string]string{"n.txt": "1\n", "other.txt": "x\n"})
	dir := t.TempDir()
	patch = map[string]string{"miss": writePatch(t, dir, "miss.hunk", "@@ file other.txt\n@@ old\nx\n@@ new\ny\n")}
	for name, n := range map[string]string{"two": "2", "three": "3", "zero": "0"} {
		patch[name] = writePatch(t, dir, name+".hunk", "@@ file n.txt\n@@ old\n1\n@@ new\n"+n+"\n")
	}
	return root, patch
}

// eachLineFor is the per-patch line that names path, its time made (0.0s),
// or "" when no line does.
func eachLineFor(out, path string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "tried ") && strings.Contains(l, path+" ") {
			return reTenths.ReplaceAllString(l, "(0.0s)")
		}
	}
	return ""
}

// countJSON is the part of an --each report these tests read: each.count,
// and matched wherever a run of the command has one.
type countJSON struct {
	Exit  int    `json:"exit"`
	Error string `json:"error"`
	Each  *struct {
		Count   *string `json:"count"`
		Patches []struct {
			Patch   string   `json:"patch"`
			Base    bool     `json:"base"`
			Status  *int     `json:"status"`
			Matched *int     `json:"matched"`
			Output  []string `json:"output"`
			Build   *struct {
				Matched *int     `json:"matched"`
				Output  []string `json:"output"`
			} `json:"build"`
		} `json:"patches"`
		TryBaseline *struct {
			Matched *int     `json:"matched"`
			Output  []string `json:"output"`
		} `json:"try_baseline"`
		TryBaselineAfter *struct {
			Matched *int     `json:"matched"`
			Output  []string `json:"output"`
		} `json:"try_baseline_after"`
	} `json:"each"`
}

func countJSONOf(t *testing.T, out string) countJSON {
	t.Helper()
	var r countJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if r.Each == nil {
		t.Fatalf("no each object: %s", out)
	}
	return r
}

// is says what a *int from --json holds, nil being "absent".
func is(p *int) string {
	if p == nil {
		return "absent"
	}
	return strings.Repeat("|", *p) + "." // a count of bars, so 0 and absent differ
}

// TestEachCountsTheLinesThatMatch is the rule through the real CLI: each
// line's count, the summary's clause, the exit, and the tree put back.
func TestEachCountsTheLinesThatMatch(t *testing.T) {
	cases := []struct {
		name    string
		breaks  []string // countFixture's patches, in the order given
		flags   []string // before --each; --count failLines unless noCount
		noCount bool
		code    int
		lines   map[string]string // a patch's line, its time made (0.0s); "" for none
		summary string            // the last line, its time cut off
		stdout  bool              // the summary is on stdout
	}{
		{
			name:   "the issue's repro: a count on each line, and no clause when every catch matched",
			breaks: []string{"two", "three"}, code: exitOK, stdout: true,
			lines: map[string]string{
				"two":   "the command exited 1 (0.0s), 1 line matched",
				"three": "the command exited 1 (0.0s), 2 lines matched",
			},
			summary: "tried 2 patches and put back every file; every command exited nonzero",
		},
		{
			name:   "a catch with no line matched says 0 and is counted in the summary; the exit is unchanged",
			breaks: []string{"two", "zero"}, code: exitOK, stdout: true,
			lines: map[string]string{
				"two":  "the command exited 1 (0.0s), 1 line matched",
				"zero": "the command exited 1 (0.0s), 0 lines matched",
			},
			summary: "tried 2 patches and put back every file; every command exited nonzero, 1 with no line matched",
		},
		{
			name:   "a break that passed has its count and is not among the catches with none",
			breaks: []string{"two", "miss", "zero"}, code: exitVerifyFailed,
			lines: map[string]string{
				"two":  "the command exited 1 (0.0s), 1 line matched",
				"miss": "the command exited 0 (0.0s), 0 lines matched",
				"zero": "the command exited 1 (0.0s), 0 lines matched",
			},
			summary: "tried 3 patches and put back every file; 2 exited nonzero, 1 with no line matched, 1 exited 0",
		},
		{
			name:   "every catch with none",
			breaks: []string{"zero"}, code: exitOK, stdout: true,
			lines:   map[string]string{"zero": "the command exited 1 (0.0s), 0 lines matched"},
			summary: "tried 1 patch and put back every file; every command exited nonzero, 1 with no line matched",
		},
		{
			name:   "without --count no line counts, and the summary is as before",
			breaks: []string{"two", "zero"}, noCount: true, code: exitOK, stdout: true,
			lines: map[string]string{
				"two":  "the command exited 1 (0.0s)",
				"zero": "the command exited 1 (0.0s)",
			},
			summary: "tried 2 patches and put back every file; every command exited nonzero",
		},
		{
			name:   "--quiet on a clean run prints nothing, as without --count",
			breaks: []string{"two", "zero"}, flags: []string{"--quiet"}, code: exitOK,
			lines: map[string]string{"two": "", "zero": ""},
		},
		{
			name:   "--quiet keeps a missed break's line, with its count",
			breaks: []string{"two", "miss"}, flags: []string{"--quiet"}, code: exitVerifyFailed,
			lines: map[string]string{
				"two":  "",
				"miss": "the command exited 0 (0.0s), 0 lines matched",
			},
			summary: "tried 2 patches and put back every file; 1 exited nonzero, 1 exited 0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, patch := countFixture(t)
			before := snapshot(t, root)
			args := append([]string{"--try", countSuite}, c.flags...)
			if !c.noCount {
				args = append(args, "--count", failLines)
			}
			args = append(args, "--each")
			for _, b := range c.breaks {
				args = append(args, patch[b])
			}
			code, out, errOut := runCLI(t, root, args, "")
			if code != c.code {
				t.Fatalf("exit %d, want %d:\n%s%s", code, c.code, errOut, out)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
			for b, want := range c.lines {
				got := eachLineFor(errOut, patch[b])
				if want != "" {
					want = "tried " + patch[b] + "  " + want
				}
				// Lines are padded to the longest name; the test reads the
				// name, then the rest after its padding.
				if got != "" {
					name, rest, _ := strings.Cut(got[len("tried "):], " ")
					got = "tried " + name + "  " + strings.TrimLeft(rest, " ")
				}
				if got != want {
					t.Errorf("%s's line:\n got %q\nwant %q", b, got, want)
				}
			}
			summaryIn := errOut
			if c.stdout {
				summaryIn = out
			}
			switch {
			case c.summary == "" && out+errOut != "":
				t.Errorf("printed %q%q, want nothing", errOut, out)
			case c.summary != "" && !strings.Contains(summaryIn, "\n"+c.summary+" (") && !strings.HasPrefix(summaryIn, c.summary+" ("):
				t.Errorf("summary: want %q in %q", c.summary, summaryIn)
			}
			if c.noCount && strings.Contains(out+errOut, "matched") {
				t.Errorf("a run without --count says matched: %q", errOut+out)
			}
		})
	}
}

// TestEachCountReadsTheWholeOutput: the count is of every line the command
// printed, not of the --verify-lines tail, and the tail is still cut.
func TestEachCountReadsTheWholeOutput(t *testing.T) {
	// Four failing tests, then two lines that are not; the tail of one is the
	// last of those.
	command := `grep -qx 1 n.txt && exit 0; for i in 1 2 3 4; do echo "--- FAIL: T$i"; done; echo FAIL; echo 'exit status 1'; exit 1`

	t.Run("text", func(t *testing.T) {
		root, patch := countFixture(t)
		_, _, errOut := runCLI(t, root, []string{"--try", command, "--count", failLines, "--verify-lines", "1", "--each", patch["two"]}, "")
		if got := eachLineFor(errOut, patch["two"]); !strings.HasSuffix(got, "the command exited 1 (0.0s), 4 lines matched") {
			t.Errorf("line = %q", got)
		}
		if !strings.Contains(errOut, "\nexit status 1\n(last 1 of 6 lines)\n") || strings.Contains(errOut, "--- FAIL: T4\n") {
			t.Errorf("the tail is not cut to 1 of 6 lines: %q", errOut)
		}
	})

	t.Run("json", func(t *testing.T) {
		root, patch := countFixture(t)
		code, out, _ := runCLI(t, root, []string{"--json", "--try", command, "--count", failLines, "--verify-lines", "2", "--each", patch["two"]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		p := countJSONOf(t, out).Each.Patches[0]
		if is(p.Matched) != is(intp(4)) || !slices.Equal(p.Output, []string{"FAIL", "exit status 1"}) {
			t.Errorf("matched %s output %q, want 4 and the last two lines", is(p.Matched), p.Output)
		}
	})

	t.Run("the default tail is cut too: forty of fifty", func(t *testing.T) {
		root, patch := countFixture(t)
		many := `grep -qx 1 n.txt && exit 0; i=0; while [ $i -lt 50 ]; do echo "--- FAIL: T$i"; i=$((i+1)); done; exit 1`
		code, out, _ := runCLI(t, root, []string{"--json", "--try", many, "--count", failLines, "--each", patch["two"]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		p := countJSONOf(t, out).Each.Patches[0]
		if is(p.Matched) != is(intp(50)) || len(p.Output) != 40 || p.Output[39] != "--- FAIL: T49" {
			t.Errorf("matched %s, output %q", is(p.Matched), p.Output)
		}
	})
}

// TestEachCountEveryRun is each kind of run: a timed-out command is counted,
// a build never is, a CRLF line matches a $, and under --measure every run
// is, base's included, with no summary clause.
func TestEachCountEveryRun(t *testing.T) {
	t.Run("a command that timed out is counted: what it printed before", func(t *testing.T) {
		root, patch := countFixture(t)
		// Two seconds, for the run on the tree as it is on Windows' sh.
		command := `grep -qx 1 n.txt && exit 0; echo '--- FAIL: T1'; sleep 30`
		code, out, errOut := runCLI(t, root, []string{"--try", command, "--count", failLines, "--timeout", "2s", "--each", patch["two"]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s%s", code, errOut, out)
		}
		if got := eachLineFor(errOut, patch["two"]); !strings.HasSuffix(got, "the command timed out after 2s, 1 line matched") {
			t.Errorf("line = %q", got)
		}
	})

	t.Run("a build's output is never counted, and its patch's line has no count", func(t *testing.T) {
		root, patch := countFixture(t)
		build := `grep -qx 0 n.txt && { echo '--- FAIL: a compile error that looks like a test'; exit 1; }; exit 0`
		args := []string{"--try", countSuite, "--count", failLines, "--build", build, "--each", patch["two"], patch["zero"]}
		code, out, errOut := runCLI(t, root, args, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s%s", code, errOut, out)
		}
		if got := eachLineFor(errOut, patch["zero"]); !strings.HasSuffix(got, "did not build: the build exited 1 (0.0s)") {
			t.Errorf("zero's line = %q", got)
		}
		if !strings.Contains(errOut, "1 exited nonzero, 1 did not build (") {
			t.Errorf("summary: %q", errOut)
		}
		_, out, _ = runCLI(t, root, append([]string{"--json"}, args...), "")
		r := countJSONOf(t, out)
		z := r.Each.Patches[1]
		if z.Matched != nil || z.Build == nil || z.Build.Matched != nil || len(z.Build.Output) != 1 {
			t.Errorf("zero: matched %s, build %+v", is(z.Matched), z.Build)
		}
		if is(r.Each.Patches[0].Matched) != is(intp(1)) || is(r.Each.Patches[0].Build.Matched) != "absent" {
			t.Errorf("two: matched %s, build's %s", is(r.Each.Patches[0].Matched), is(r.Each.Patches[0].Build.Matched))
		}
	})

	t.Run("a line a Windows tool printed matches a pattern ending in $", func(t *testing.T) {
		root, patch := countFixture(t)
		command := `grep -qx 1 n.txt && exit 0; printf '%s\r\n' '--- FAIL: T1' '--- FAIL: T2' FAIL; exit 1`
		_, _, errOut := runCLI(t, root, []string{"--try", command, "--count", `^--- FAIL: T[0-9]$`, "--each", patch["two"]}, "")
		if got := eachLineFor(errOut, patch["two"]); !strings.HasSuffix(got, ", 2 lines matched") {
			t.Errorf("line = %q", got)
		}
	})

	t.Run("--measure: every run is counted, base's too, and nothing is a catch", func(t *testing.T) {
		root, patch := countFixture(t)
		args := []string{"--try", countSuite, "--measure", "--count", failLines, "--each", patch["three"], patch["zero"]}
		code, out, errOut := runCLI(t, root, args, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, errOut, out)
		}
		for name, want := range map[string]string{
			"base":         "the command exited 0 (0.0s), 0 lines matched",
			patch["three"]: "the command exited 1 (0.0s), 2 lines matched",
			patch["zero"]:  "the command exited 1 (0.0s), 0 lines matched",
		} {
			if got := eachLineFor(errOut, name); !strings.HasSuffix(got, want) {
				t.Errorf("%s's line = %q, want it to end %q", name, got, want)
			}
		}
		if !strings.Contains(out, "; 1 exited 0, 2 exited nonzero (") || strings.Contains(out, "with no line matched") {
			t.Errorf("summary: %q", out)
		}
		_, out, _ = runCLI(t, root, append([]string{"--json"}, args...), "")
		r := countJSONOf(t, out)
		var got []string
		for _, p := range r.Each.Patches {
			got = append(got, filepath.Base(p.Patch)+"="+is(p.Matched))
		}
		if want := []string{"base=.", "three.hunk=||.", "zero.hunk=."}; !slices.Equal(got, want) {
			t.Errorf("matched per run = %q, want %q", got, want)
		}
	})

	t.Run("--dry-run takes it and runs nothing", func(t *testing.T) {
		root, patch := countFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--dry-run", "--try", countSuite, "--count", failLines, "--each", patch["two"]}, "")
		if code != exitOK || !strings.Contains(out, "(dry run: nothing written, command not run)") || strings.Contains(out+errOut, "matched") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
		_, out, _ = runCLI(t, root, []string{"--json", "--dry-run", "--try", countSuite, "--count", failLines, "--each", patch["two"]}, "")
		r := countJSONOf(t, out)
		if r.Each.Count == nil || *r.Each.Count != failLines || r.Each.Patches[0].Matched != nil {
			t.Errorf("count %v, matched %s: %s", r.Each.Count, is(r.Each.Patches[0].Matched), out)
		}
	})
}

func intp(n int) *int { return &n }

// TestEachCountJSON: each.count is the pattern, and every run of the command
// has matched, 0 included, the runs on the tree as it is too; without
// --count none of it is there.
func TestEachCountJSON(t *testing.T) {
	t.Run("given", func(t *testing.T) {
		root, patch := countFixture(t)
		code, out, _ := runCLI(t, root, []string{"--json", "--try", countSuite, "--count", failLines, "--each", patch["three"], patch["zero"]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := countJSONOf(t, out)
		if r.Each.Count == nil || *r.Each.Count != failLines {
			t.Errorf("each.count = %v", r.Each.Count)
		}
		if got := is(r.Each.Patches[0].Matched) + is(r.Each.Patches[1].Matched); got != "||.." {
			t.Errorf("matched = %s, want 2 then 0", got)
		}
		// The runs on the tree as it is, before the first patch and, since
		// one was caught, after the last.
		if r.Each.TryBaseline == nil || is(r.Each.TryBaseline.Matched) != "." {
			t.Errorf("try_baseline = %+v", r.Each.TryBaseline)
		}
		if r.Each.TryBaselineAfter == nil || is(r.Each.TryBaselineAfter.Matched) != "." {
			t.Errorf("try_baseline_after = %+v", r.Each.TryBaselineAfter)
		}
		if !strings.Contains(out, `"matched": 0`) {
			t.Errorf("a count of 0 is left out: %s", out)
		}
	})

	t.Run("not given", func(t *testing.T) {
		root, patch := countFixture(t)
		code, out, _ := runCLI(t, root, []string{"--json", "--try", countSuite, "--each", patch["three"]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		if strings.Contains(out, `"matched"`) || strings.Contains(out, `"count"`) {
			t.Errorf("a run without --count carries a count: %s", out)
		}
	})
}

// TestEachCountCutsTheRunsOnTheTreeAsItIs: the runs with no patch applied keep
// the whole output to count it, so they have to be cut afterwards too, or a
// refusal there would print every line and --json carry them all.
func TestEachCountCutsTheRunsOnTheTreeAsItIs(t *testing.T) {
	fifty := `i=0; while [ $i -lt 50 ]; do echo "--- FAIL: T$i"; i=$((i+1)); done; exit 1`

	t.Run("before the first patch", func(t *testing.T) {
		root, patch := countFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--try", fifty, "--count", failLines, "--each", patch["two"]}, "")
		if code != exitUsage {
			t.Fatalf("exit %d: %s%s", code, errOut, out)
		}
		if !strings.Contains(errOut, "the command exited 1 with no patch applied") || !cutToForty(errOut) {
			t.Errorf("stderr = %q", errOut)
		}
		_, out, _ = runCLI(t, root, []string{"--json", "--try", fifty, "--count", failLines, "--each", patch["two"]}, "")
		b := countJSONOf(t, out).Each.TryBaseline
		if b == nil || is(b.Matched) != is(intp(50)) || len(b.Output) != 40 {
			t.Errorf("try_baseline = %+v", b)
		}
	})

	t.Run("after the last patch", func(t *testing.T) {
		root, patch := countFixture(t)
		// The third run is the one after the last patch: it fails, printing
		// fifty, where the first two were the suite.
		mark := pidFile(t, "runs")
		command := `echo x >> '` + mark + `'; [ "$(wc -l < '` + mark + `' | tr -dc 0-9)" -ge 3 ] && { ` + fifty + `; }; ` + countSuite
		args := []string{"--try", command, "--count", failLines, "--each", patch["two"]}
		code, out, errOut := runCLI(t, root, args, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s%s", code, errOut, out)
		}
		if !strings.Contains(errOut, "after the last patch, the command exited 1") || !cutToForty(errOut) {
			t.Errorf("stderr = %q", errOut)
		}
		must(t, os.Remove(filepath.FromSlash(mark)))
		_, out, _ = runCLI(t, root, append([]string{"--json"}, args...), "")
		b := countJSONOf(t, out).Each.TryBaselineAfter
		if b == nil || is(b.Matched) != is(intp(50)) || len(b.Output) != 40 {
			t.Errorf("try_baseline_after = %+v", b)
		}
	})
}

// cutToForty says the transcript of a run that printed "--- FAIL: T0" to
// "--- FAIL: T49" is its last forty lines: T10 on, and not T9.
func cutToForty(out string) bool {
	return strings.Contains(out, "\n--- FAIL: T10\n") && strings.Contains(out, "\n--- FAIL: T49\n(last 40 of 50 lines)\n") &&
		!strings.Contains(out, "\n--- FAIL: T9\n")
}

// TestEachCountRefusals: every refusal at exit 1, the whole message, the
// tree untouched and the command never run.
func TestEachCountRefusals(t *testing.T) {
	cases := []struct {
		name string
		args func(cmd, p string) []string
		msg  string // all of stderr
	}{
		{
			"without --each", func(cmd, p string) []string { return []string{"--try", cmd, "--count", failLines, "-f", p} },
			"hunk: --count counts the matching lines of each patch's output under --try CMD --each, and does nothing without --each\n",
		},
		{
			"with --verify", func(cmd, p string) []string { return []string{"--verify", cmd, "--count", failLines, "-f", p} },
			"hunk: --count counts the matching lines of each patch's output under --try CMD --each, and does nothing without --each\n",
		},
		{
			"without --try", func(_, p string) []string { return []string{"--count", failLines, "-f", p} },
			"hunk: --count counts the matching lines of each patch's output under --try CMD --each, and does nothing without --each\n",
		},
		{
			"an empty pattern", func(cmd, p string) []string { return []string{"--try", cmd, "--count", "", "--each", p} },
			"hunk: --count was given an empty pattern; if it came from a variable, that variable is empty\n",
		},
		{
			"an empty pattern after one", func(cmd, p string) []string {
				return []string{"--try", cmd, "--count", failLines, "--count", "", "--each", p}
			},
			"hunk: --count was given an empty pattern; if it came from a variable, that variable is empty\n",
		},
		{
			"a pattern Go's regexp does not read", func(cmd, p string) []string { return []string{"--try", cmd, "--count", "FAIL(", "--each", p} },
			"hunk: --count \"FAIL(\" is not a pattern Go's regexp reads: error parsing regexp: missing closing ): `FAIL(`\n",
		},
		{
			"two patterns", func(cmd, p string) []string {
				return []string{"--try", cmd, "--count", failLines, "--count", "^panic:", "--each", p}
			},
			"hunk: invalid value \"^panic:\" for flag -count: already counting lines that match \"^ *--- FAIL\"; " +
				"--count takes one pattern, so to count the lines either matches give \"^ *--- FAIL|^panic:\"\n" +
				"Run \"hunk --help\" for the flags.\n",
		},
		{
			"one pattern twice", func(cmd, p string) []string {
				return []string{"--try", cmd, "--count", failLines, "--count", failLines, "--each", p}
			},
			"hunk: invalid value \"^ *--- FAIL\" for flag -count: already counting lines that match \"^ *--- FAIL\"; give --count once\n" +
				"Run \"hunk --help\" for the flags.\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, patch := countFixture(t)
			before := snapshot(t, root)
			mark := pidFile(t, "ran")
			args := c.args(counted(mark, countSuite), patch["two"])
			code, out, errOut := runCLI(t, root, args, "")
			if code != exitUsage {
				t.Errorf("exit %d, want 1", code)
			}
			if errOut != c.msg || out != "" {
				t.Errorf("stderr:\n got %q\nwant %q\nstdout %q", errOut, c.msg, out)
			}
			if n := markerLines(t, mark); n != 0 {
				t.Errorf("the command ran %d times", n)
			}
			assertUnchanged(t, root, before)

			// Under --json the refusal is the object, with the same words.
			code, out, errOut = runCLI(t, root, append([]string{"--json"}, args...), "")
			var r countJSON
			if err := json.Unmarshal([]byte(out), &r); err != nil || code != exitUsage || r.Exit != exitUsage || errOut != "" {
				t.Fatalf("--json: exit %d, %v, stdout %q, stderr %q", code, err, out, errOut)
			}
			if first, _, _ := strings.Cut(strings.TrimPrefix(c.msg, "hunk: "), "\n"); !strings.HasSuffix(r.Error, first) {
				t.Errorf("--json error %q, want it to end %q", r.Error, first)
			}
		})
	}
}

// TestPatternFlag is --count's flag value on its own: one pattern, a second
// refused, an empty one noted wherever it stood.
func TestPatternFlag(t *testing.T) {
	for _, c := range []struct {
		name    string
		sets    []string
		err     string // of the last Set, "" for none
		pattern string
		empty   bool
	}{
		{"one", []string{"a"}, "", "a", false},
		{"two", []string{"a", "b"}, `already counting lines that match "a"; --count takes one pattern, so to count the lines either matches give "a|b"`, "a", false},
		{"the same twice", []string{"a", "a"}, `already counting lines that match "a"; give --count once`, "a", false},
		{"empty, then one", []string{"", "a"}, "", "a", true},
		{"one, then empty", []string{"a", ""}, "", "a", true},
		{"empty alone", []string{""}, "", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var p patternFlag
			var err error
			for _, v := range c.sets {
				err = p.Set(v)
			}
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.err || p.pattern != c.pattern || p.String() != c.pattern || p.givenEmpty() != c.empty || p.set != (c.pattern != "") {
				t.Errorf("err %q pattern %q empty %v set %v; want %q %q %v", got, p.pattern, p.givenEmpty(), p.set, c.err, c.pattern, c.empty)
			}
		})
	}
}

// TestMatchingLines is the count itself: lines, not matches, each without
// one CRLF's carriage return.
func TestMatchingLines(t *testing.T) {
	for _, c := range []struct {
		name, pattern string
		lines         []string
		want          int
	}{
		{"no lines", failLines, nil, 0},
		{"none match", failLines, []string{"ok", "PASS"}, 0},
		{"go test's failures, a subtest's indented", failLines, []string{"--- FAIL: TestA (0.00s)", "    --- FAIL: TestA/x (0.00s)", "FAIL", "FAIL\tpkg\t0.1s"}, 2},
		{"a line that matches twice is one", "FAIL", []string{"FAIL FAIL", "ok"}, 1},
		{"unanchored, anywhere in the line", "panic", []string{"goroutine 1: panic: boom", "x"}, 1},
		{"a carriage return before the newline is not part of the line", `FAIL$`, []string{"FAIL\r", "FAIL"}, 2},
		{"only one carriage return is", `FAIL$`, []string{"FAIL\r\r"}, 0},
		{"a carriage return within a line stays", `^a$`, []string{"a\rb"}, 0},
		{"a pattern that names a carriage return matches none at the end", `\r$`, []string{"FAIL\r"}, 0},
		{"empty lines, by ^$", `^$`, []string{"", "a", ""}, 2},
		{"bytes that are not UTF-8", `^x`, []string{"x\xff\xfe", "\xffx"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := matchingLines(regexp.MustCompile(c.pattern), c.lines); got != c.want {
				t.Errorf("matchingLines(%q, %q) = %d, want %d", c.pattern, c.lines, got, c.want)
			}
		})
	}
}

// TestEachReadCountsAndCuts is read on each kind of run: counted where the
// command ran to an end of its own or the timeout's, never where a signal
// ended it or it never ran, and cut to the tail either way, after counting.
func TestEachReadCountsAndCuts(t *testing.T) {
	lines := func() []string { return []string{"--- FAIL: A", "--- FAIL: B", "FAIL"} }
	re := regexp.MustCompile(failLines)
	for _, c := range []struct {
		name    string
		cfg     eachConfig
		v       *Verify
		matched string // is() of v.Matched afterwards
		tail    int
		figures bool
	}{
		{"ran", eachConfig{count: re, verifyLines: 1}, &Verify{Ran: true, Status: 1, Tail: lines()}, "||.", 1, false},
		{"timed out: counted", eachConfig{count: re, verifyLines: 2}, &Verify{Ran: true, Status: 124, TimedOut: time.Second, Tail: lines()}, "||.", 2, false},
		{"a signal ended it: not counted, cut", eachConfig{count: re, verifyLines: 1}, &Verify{Ran: true, Signal: os.Interrupt, Tail: lines()}, "absent", 1, false},
		{"never ran", eachConfig{count: re, verifyLines: 1}, &Verify{Ran: false}, "absent", 0, false},
		{"no --count", eachConfig{verifyLines: 1}, &Verify{Ran: true, Tail: lines()}, "absent", 1, false},
		{"--measure and --count: both read", eachConfig{count: re, measure: true, verifyLines: 40}, &Verify{Ran: true, Tail: []string{"--- FAIL: A (0.25s)"}}, "|.", 1, true},
		{"--measure, timed out: counted, numbers not read", eachConfig{count: re, measure: true, verifyLines: 40}, &Verify{Ran: true, TimedOut: time.Second, Tail: []string{"--- FAIL: A (0.25s)"}}, "|.", 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			figures := c.cfg.read(c.v)
			if got := is(c.v.Matched); got != c.matched {
				t.Errorf("matched %s, want %s", got, c.matched)
			}
			if len(c.v.Tail) != c.tail {
				t.Errorf("tail %q, want %d lines", c.v.Tail, c.tail)
			}
			if (len(figures) > 0) != c.figures {
				t.Errorf("figures %v, want some: %v", figures, c.figures)
			}
		})
	}
	if (eachConfig{}).read(nil) != nil {
		t.Error("read(nil) read something")
	}
	for _, c := range []struct {
		cfg  eachConfig
		want int
	}{
		{eachConfig{verifyLines: 7}, 7},
		{eachConfig{verifyLines: 7, count: re}, 0},
		{eachConfig{verifyLines: 7, measure: true}, 0},
	} {
		if got := c.cfg.commandLines(); got != c.want {
			t.Errorf("commandLines(%+v) = %d, want %d", c.cfg, got, c.want)
		}
	}
}

// TestEachCommandRunsKeepEveryLine is the claim read rests on, which the
// compiler does not check: every run of the --try command under --each keeps
// commandLines() of its output, so that --count and --measure read all of it.
// A run that passed cfg.verifyLines would count the tail alone, and only the
// tests of that run would notice.
func TestEachCommandRunsKeepEveryLine(t *testing.T) {
	src, err := os.ReadFile("each.go")
	must(t, err)
	calls := regexp.MustCompile(`(?:runOnTheTree|runTry)\([^\n]*cfg\.command, ([^,]+),`).FindAllStringSubmatch(string(src), -1)
	// Four when this was written: the run before the first patch and the one
	// after the last, base's under --measure, and each patch's.
	if len(calls) < 4 {
		t.Fatalf("found %d runs of the command in each.go; the pattern is out of date", len(calls))
	}
	for _, m := range calls {
		if m[1] != "cfg.commandLines()" {
			t.Errorf("%s keeps %s lines, not cfg.commandLines()", m[0], m[1])
		}
	}
}

// TestEachCountReport is the text and the object as goldens, built from runs
// of every kind, so that a reword is a deliberate change.
func TestEachCountReport(t *testing.T) {
	cfg := eachConfig{command: "go test ./...", count: regexp.MustCompile(failLines), verifyLines: 40}
	ran := func(status int, secs float64, matched int, tail ...string) *Verify {
		return &Verify{Ran: true, Try: true, Command: cfg.command, Status: status, Seconds: secs, Applied: 1, RolledBack: 1, Matched: intp(matched), TotalLines: len(tail), Tail: tail}
	}
	patches := func() []*eachPatch {
		hung := ran(exitTimedOut, 90, 1, "--- FAIL: TestCache (0.00s)", "=== RUN   TestLoop")
		hung.TimedOut = 90 * time.Second
		return []*eachPatch{
			{name: "breaks.hunk:m01-off-by-one", v: ran(1, 4.2, 3, "--- FAIL: TestSplit (0.00s)", "FAIL")},
			{name: "breaks.hunk:m02-nil-map", v: ran(2, 0.9, 0, "panic: assignment to entry in nil map", "FAIL\texample.com/x\t0.9s")},
			{name: "breaks.hunk:m03-swap", v: ran(0, 4.0, 0, "ok  \texample.com/x\t4.0s")},
			{name: "breaks.hunk:m04-loop", v: hung},
		}
	}
	o := eachOutcome{tried: 4, zeros: 1, timeouts: 1, unmatched: 1, stopAt: -1, seconds: 99.1}

	t.Run("text", func(t *testing.T) {
		var out, errOut bytes.Buffer
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
		golden(t, "report-each-count", errOut.String()+out.String())
	})

	t.Run("json", func(t *testing.T) {
		var out, errOut bytes.Buffer
		cfg := cfg
		cfg.asJSON = true
		base := &Verify{Ran: true, OK: true, Try: true, Command: cfg.command, Seconds: 4.1, Matched: intp(0), TotalLines: 1, Tail: []string{"ok  \texample.com/x\t4.1s"}}
		if code := eachJSON(cfg, patches(), eachBaselines{command: base}, exitVerifyFailed, "", &out, &errOut); code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		golden(t, "report-each-count-json", out.String())
	})
}

// FuzzCountLines: any pattern and any output give a count or a refusal,
// never a panic, and the count is of lines: no more than there are, at least
// one for a pattern that quotes a line of them, and read's the same as the
// count's however the output is cut.
func FuzzCountLines(f *testing.F) {
	f.Add(failLines, "--- FAIL: A\n    --- FAIL: A/b\nFAIL\n", 2)
	f.Add(`FAIL$`, "FAIL\r\nok\r\n", 1)
	f.Add(`^$`, "\n\n\n", 40)
	f.Add(`(`, "x", 1)
	f.Add(`\xff`, "\xff\xfe\n", 1)
	f.Fuzz(func(t *testing.T, pattern, output string, tail int) {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return
		}
		// The lines as RunVerify splits them.
		lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
		n := matchingLines(re, lines)
		if n < 0 || n > len(lines) {
			t.Fatalf("%d lines matched of %d", n, len(lines))
		}
		// A pattern is UTF-8, so a line that is not cannot be quoted as one.
		if len(lines) > 0 && utf8.ValidString(lines[0]) {
			first := strings.TrimSuffix(lines[0], "\r")
			if matchingLines(regexp.MustCompile(regexp.QuoteMeta(first)), lines) < 1 {
				t.Fatalf("a pattern quoting line 1, %q, matched no line", first)
			}
		}
		tail = max(tail%64, 1)
		v := &Verify{Ran: true, Tail: slices.Clone(lines)}
		eachConfig{count: re, verifyLines: tail}.read(v)
		if v.Matched == nil || *v.Matched != n || len(v.Tail) > tail {
			t.Fatalf("read: matched %s, %d lines kept of %d, want %d and at most %d", is(v.Matched), len(v.Tail), len(lines), n, tail)
		}
	})
}
