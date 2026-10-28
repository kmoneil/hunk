package main

// --each's tests (§4.3). The live rows drive the real CLI against a real tree
// and real patch files, with the command's own behaviour as the probe: a
// marker file says whether a command ran at all, which is how "nothing was
// tried" is an assertion rather than a sentence. The goldens are rendered
// from constructed outcomes, as the try and verify reports' are, because a
// live run's seconds are nobody's contract.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// eachFixture is three files and one patch for each, the patch files written
// where --each reads them: the caller's filesystem, as -f is, not the tree.
func eachFixture(t *testing.T) (root string, patches []string) {
	t.Helper()
	root = cliTree(t, map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n", "c.txt": "gamma\n"})
	dir := t.TempDir()
	return root, []string{
		writePatch(t, dir, "01-alpha.hunk", "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n"),
		writePatch(t, dir, "02-beta.hunk", "@@ file b.txt\n@@ old\nbeta\n@@ new\nBETA\n"),
		writePatch(t, dir, "03-gamma.hunk", "@@ file c.txt\n@@ old\ngamma\n@@ new\nGAMMA\n"),
	}
}

// catchesAll is a suite for eachFixture: it passes on the tree as it is and
// fails under each of the three patches, each of which capitalises its file's
// word. Since #90 the command runs once on the tree as it is first and a
// command that fails there refuses the run, so every test's command is a
// suite that passes before it is a probe; `exit 1`, which these tests used
// until then, is refused now, which is the point.
const catchesAll = "grep -q alpha a.txt && grep -q beta b.txt && grep -q gamma c.txt"

// sayingSuite tests a.txt alone and says what it found: "FAIL: alpha" and
// exit 1 under the alpha patch, "ok: alpha" and exit 0 on the tree as it is
// and under the others. So a caught patch has output to show (#89), and the
// beta patch is a break the suite lets through.
const sayingSuite = "grep -q alpha a.txt || { echo 'FAIL: alpha'; exit 1; }; echo 'ok: alpha'"

// counted is command with a line appended to mark first, so markerLines says
// how often it ran, its run on the tree as it is included.
func counted(mark, command string) string {
	return fmt.Sprintf("echo ran >> '%s'; %s", mark, command)
}

func writePatch(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	must(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

// markerLines counts how often a command ran, by the lines it appended to a
// pidFile-spelled marker: the proof that "nothing was tried" is an assertion.
func markerLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	if os.IsNotExist(err) {
		return 0
	}
	must(t, err)
	return strings.Count(string(b), "\n")
}

// TestEachTriesEachPatchAlone is the happy paths: every command's status is a
// result, the tree ends as it began, and the exit is the run's.
func TestEachTriesEachPatchAlone(t *testing.T) {
	t.Run("every command exits nonzero: exit 0, summary on stdout", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--try", counted(mark, catchesAll), "--each", patches[0], patches[1], patches[2]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		if n := markerLines(t, mark); n != 5 {
			t.Errorf("the command ran %d times, want 5: on the tree as it is, once a patch, and on the tree again after the last (#110)", n)
		}
		if !strings.Contains(out, "tried 3 patches and put back every file; every command exited nonzero") {
			t.Errorf("stdout = %q", out)
		}
		for _, p := range patches {
			if !strings.Contains(errOut, "tried "+p) {
				t.Errorf("stderr has no line for %s: %q", p, errOut)
			}
		}
		if !strings.Contains(errOut, "the command exited 1") {
			t.Errorf("stderr = %q", errOut)
		}
		// §4.3: under --try the exit would have been the command's 1. Here it
		// is the run's 0, which is the whole point.
	})

	t.Run("a command exits 0: exit 3, its output shown, the caught one's not", func(t *testing.T) {
		root, patches := eachFixture(t)
		before := snapshot(t, root)
		// Output on both patches, so the transcript rule shows: the caught
		// one's is not printed without --verify-lines (#89).
		code, out, errOut := runCLI(t, root,
			[]string{"--try", sayingSuite, "--each", patches[0], patches[1]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s%s", code, out, errOut)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		if out != "" {
			t.Errorf("stdout = %q, want empty: the report is a failure's", out)
		}
		if !strings.Contains(errOut, "tried 2 patches and put back every file; 1 exited nonzero, 1 exited 0") {
			t.Errorf("stderr = %q", errOut)
		}
		if !strings.Contains(errOut, "$ "+sayingSuite) || !strings.Contains(errOut, "ok: alpha") {
			t.Errorf("the passing command's output is not shown: %q", errOut)
		}
		if n := strings.Count(errOut, "$ "); n != 1 || strings.Contains(errOut, "\nFAIL: alpha\n") {
			t.Errorf("%d transcripts, want 1, for the command that exited 0 alone: %q", n, errOut)
		}
	})

	t.Run("a command times out: exit 3, the line says so", func(t *testing.T) {
		root, patches := eachFixture(t)
		// Two seconds, not a hundred milliseconds: the run on the tree as it
		// is has to finish inside it, on Windows' sh too.
		code, _, errOut := runCLI(t, root,
			[]string{"--try", "if grep -q ALPHA a.txt; then sleep 30; fi", "--timeout", "2s", "--each", patches[0]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s", code, errOut)
		}
		if !strings.Contains(errOut, "the command timed out after 2s") {
			t.Errorf("stderr = %q", errOut)
		}
		if !strings.Contains(errOut, "tried 1 patch and put back every file; 1 timed out") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("the lines come in the order the patches were given", func(t *testing.T) {
		root, patches := eachFixture(t)
		_, _, errOut := runCLI(t, root,
			[]string{"--try", catchesAll, "--each", patches[2], patches[0]}, "")
		if i, j := strings.Index(errOut, patches[2]), strings.Index(errOut, patches[0]); i < 0 || j < 0 || i > j {
			t.Errorf("lines out of order: %q", errOut)
		}
	})

	t.Run("each patch is tried alone: the command never sees two edits", func(t *testing.T) {
		root, patches := eachFixture(t)
		// The command passes when a.txt and b.txt agree, as on the tree as it
		// is and if both edits were ever on disk at once, and fails when one
		// edit is there alone.
		code, out, errOut := runCLI(t, root,
			[]string{"--try", "grep -q alpha a.txt; a=$?; grep -q beta b.txt; b=$?; [ $a = $b ]", "--each", patches[0], patches[1]}, "")
		if code != exitOK {
			t.Fatalf("exit %d, want 0 (a patch leaked into another's try): %s%s", code, out, errOut)
		}
	})

	t.Run("every report line starts tried, §4.1's word", func(t *testing.T) {
		root, patches := eachFixture(t)
		_, out, errOut := runCLI(t, root,
			[]string{"--try", catchesAll, "--each", patches[0], patches[1]}, "")
		for _, l := range strings.Split(strings.TrimRight(errOut+out, "\n"), "\n") {
			if !strings.HasPrefix(l, "tried ") {
				t.Errorf("a report line does not start tried: %q", l)
			}
		}
	})
}

// TestEachPreflightRefusesTheLot: a patch that cannot be read or does not
// apply refuses the whole run before any command runs. The marker file is the
// assertion that nothing was tried.
func TestEachPreflightRefusesTheLot(t *testing.T) {
	cases := []struct {
		name    string
		patch   string // body of the bad second patch; badFile overrides
		badFile string // a path that cannot be read, instead of a body
		code    int
		header  string
		detail  string
	}{
		{
			name:   "a patch that does not parse is exit 1",
			patch:  "@@ file a.txt\n@@ old\nalpha\n",
			code:   exitUsage,
			header: "1 of 2 patches could not be read; nothing was tried",
			detail: `"@@ old" has no "@@ new"`,
		},
		{
			name:   "and so does one with a directive missing its space (#96)",
			patch:  "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n@@file b.txt\n",
			code:   exitUsage,
			header: "1 of 2 patches could not be read; nothing was tried",
			detail: `patch line 6: the line would be the directive "@@ file" but for the space missing after "@@"`,
		},
		{
			name:    "a patch file that does not exist is exit 1",
			badFile: "missing.hunk",
			code:    exitUsage,
			header:  "1 of 2 patches could not be read; nothing was tried",
			detail:  "missing.hunk",
		},
		{
			name:   "a patch that does not match is exit 2, with the near miss",
			patch:  "@@ file a.txt\n@@ old\nnothing like it\n@@ new\nX\n",
			code:   exitNoMatch,
			header: "1 of 2 patches would not apply; nothing was tried",
			detail: "no anchor found",
		},
		{
			name:   "a path refusal is exit 2 under the patch's name",
			patch:  "@@ file ../out.txt\n@@ old\nx\n@@ new\ny\n",
			code:   exitNoMatch,
			header: "1 of 2 patches would not apply; nothing was tried",
			detail: "climbs out",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, patches := eachFixture(t)
			bad := c.badFile
			if bad == "" {
				bad = writePatch(t, t.TempDir(), "bad.hunk", c.patch)
			}
			mark := pidFile(t, "ran")
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root,
				[]string{"--try", fmt.Sprintf("echo ran >> '%s'", mark), "--each", patches[0], bad}, "")
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s%s", code, c.code, out, errOut)
			}
			if n := markerLines(t, mark); n != 0 {
				t.Errorf("the command ran %d times before the refusal", n)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
			if !strings.Contains(errOut, c.header) {
				t.Errorf("stderr lacks %q: %q", c.header, errOut)
			}
			if !strings.Contains(errOut, c.detail) {
				t.Errorf("stderr lacks %q: %q", c.detail, errOut)
			}
			if !strings.Contains(errOut, bad+":") {
				t.Errorf("the failing patch is not named: %q", errOut)
			}
		})
	}

	t.Run("every bad patch is reported, not only the first", func(t *testing.T) {
		root, _ := eachFixture(t)
		dir := t.TempDir()
		bad1 := writePatch(t, dir, "bad1.hunk", "@@ file a.txt\n@@ old\nno such text\n@@ new\nX\n")
		bad2 := writePatch(t, dir, "bad2.hunk", "@@ file b.txt\n@@ old\nnot there either\n@@ new\nY\n")
		code, _, errOut := runCLI(t, root, []string{"--try", "exit 1", "--each", bad1, bad2}, "")
		if code != exitNoMatch {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if !strings.Contains(errOut, "2 patches would not apply") {
			t.Errorf("header does not count both: %q", errOut)
		}
		if !strings.Contains(errOut, bad1+":") || !strings.Contains(errOut, bad2+":") {
			t.Errorf("a bad patch is unreported: %q", errOut)
		}
	})

	t.Run("read errors are reported before matching, alone", func(t *testing.T) {
		// Stage order: a run with a typo and a no-match reports the typo at
		// exit 1; the no-match is found on the resend, as a single run would.
		root, _ := eachFixture(t)
		dir := t.TempDir()
		typo := writePatch(t, dir, "typo.hunk", "@@ file a.txt\n@@ old\nalpha\n")
		nomatch := writePatch(t, dir, "nomatch.hunk", "@@ file a.txt\n@@ old\nmissing\n@@ new\nX\n")
		code, _, errOut := runCLI(t, root, []string{"--try", "exit 1", "--each", typo, nomatch}, "")
		if code != exitUsage {
			t.Fatalf("exit %d, want 1: %s", code, errOut)
		}
		if strings.Contains(errOut, "would not apply") {
			t.Errorf("matching ran despite a read failure: %q", errOut)
		}
	})

	t.Run("an unreadable target is exit 5", func(t *testing.T) {
		needsPOSIXPerms(t)
		root, patches := eachFixture(t)
		must(t, os.Chmod(filepath.Join(root, "a.txt"), 0o000))
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "a.txt"), 0o644) })
		code, _, errOut := runCLI(t, root, []string{"--try", "exit 1", "--each", patches[0], patches[1]}, "")
		if code != exitIO {
			t.Fatalf("exit %d, want 5: %s", code, errOut)
		}
		if !strings.Contains(errOut, "1 of 2 patches could not be checked; nothing was tried") {
			t.Errorf("the header does not track exit 5: %q", errOut)
		}
		if !strings.Contains(errOut, patches[0]+":") {
			t.Errorf("the unreadable patch is not named: %q", errOut)
		}
	})
}

// TestEachFlagRefusals: every wrong spelling is exit 1 before anything runs.
func TestEachFlagRefusals(t *testing.T) {
	root, patches := eachFixture(t)
	cases := []struct {
		name string
		args []string
		say  string
	}{
		{
			"without --try",
			[]string{"--each", patches[0]},
			"--each tries each patch alone and does nothing without --try CMD",
		},
		{
			"with --verify instead",
			[]string{"--verify", "true", "--each", patches[0]},
			"--each needs --try, not --verify",
		},
		{
			"with --verify and --try both",
			[]string{"--verify", "true", "--try", "true", "--each", patches[0]},
			"--try and --verify cannot both be set",
		},
		{
			"with -f",
			[]string{"--try", "true", "-f", patches[0], "--each", patches[1]},
			"--each takes its patches as arguments, not from -f",
		},
		{
			"with no patches",
			[]string{"--try", "true", "--each"},
			"--each needs at least one patch file",
		},
		{
			"a flag after the files",
			[]string{"--try", "true", "--each", patches[0], "--timeout", "5s"},
			`argument "--timeout" begins with "-": flags come before --each's patch files`,
		},
		{
			"a patch listed twice",
			[]string{"--try", "true", "--each", patches[0], patches[0]},
			"is listed twice",
		},
		{
			"-f twice, without --each",
			[]string{"-f", patches[0], "-f", patches[1]},
			"already reading " + patches[0] + "; one -f is one patch. To try several patches one at a time, use --try CMD --each",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, c.args, "")
			if code != exitUsage {
				t.Fatalf("exit %d, want 1: %s%s", code, out, errOut)
			}
			if !strings.Contains(errOut, c.say) {
				t.Errorf("stderr lacks %q: %q", c.say, errOut)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
		})
	}
}

// TestEachStopsWhenAFileIsNotRestored is the rule the field's loops got
// wrong: the most careful one read an exit-4 report's "the command exited 0"
// as a missed mutant and carried on against the broken tree.
func TestEachStopsWhenAFileIsNotRestored(t *testing.T) {
	root, patches := eachFixture(t)
	mark := pidFile(t, "ran")
	code, out, errOut := runCLI(t, root,
		[]string{
			"--try", counted(mark, "grep -q alpha a.txt || { rm a.txt && mkdir a.txt; }"),
			"--each", patches[0], patches[1], patches[2],
		}, "")
	if code != exitRollbackFailed {
		t.Fatalf("exit %d, want 4: %s%s", code, out, errOut)
	}
	if n := markerLines(t, mark); n != 2 {
		t.Errorf("the command ran %d times, want 2: on the tree as it is, then the patch that stopped the run", n)
	}
	if fi, err := os.Stat(filepath.Join(root, "a.txt")); err != nil || !fi.IsDir() {
		t.Errorf("the directory the command left should still be there: %v", err)
	}
	for _, want := range []string{
		"1 file left alone",
		"a.txt was not restored.",
		"not tried: " + patches[1] + ", " + patches[2],
		"tried 0 of 3 patches; stopped at " + patches[0] + ": 1 file was not restored; 2 patches not tried",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %q", want, errOut)
		}
	}
}

// TestEachStopsWhenTheTreeChangesUnderTheRun: the preflight's answer can go
// stale when the command writes files no patch named, and then a later patch
// is refused mid-run. Carrying on would score every later patch against a
// tree nobody validated.
func TestEachStopsWhenTheTreeChangesUnderTheRun(t *testing.T) {
	root, patches := eachFixture(t)
	code, out, errOut := runCLI(t, root,
		[]string{"--try", "printf 'junk\\n' > b.txt", "--each", patches[0], patches[1], patches[2]}, "")
	if code != exitNoMatch {
		t.Fatalf("exit %d, want 2: %s%s", code, out, errOut)
	}
	for _, want := range []string{
		patches[1] + " no longer applies; the preflight matched every patch, so the tree has changed since the run began: by the command, or by another writer",
		"expected 1 occurrence, found 0",
		"not tried: " + patches[2],
		"tried 1 of 3 patches; stopped at " + patches[1] + "; 1 patch not tried",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %q", want, errOut)
		}
	}
	// a.txt and c.txt are as they were; b.txt is as the command left it,
	// which is the truth the report just told.
	for name, want := range map[string]string{"a.txt": "alpha\n", "b.txt": "junk\n", "c.txt": "gamma\n"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		must(t, err)
		if string(b) != want {
			t.Errorf("%s = %q, want %q", name, b, want)
		}
	}
}

// TestEachQuiet: --quiet keeps the actionable lines and nothing else.
func TestEachQuiet(t *testing.T) {
	t.Run("a clean run prints nothing", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root,
			[]string{"--quiet", "--try", catchesAll, "--each", patches[0], patches[1]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if out != "" || errOut != "" {
			t.Errorf("quiet success printed: stdout %q, stderr %q", out, errOut)
		}
	})
	t.Run("a command that exited 0 still prints, and the summary with it", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root,
			[]string{"--quiet", "--try", sayingSuite, "--each", patches[0], patches[1]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if strings.Contains(errOut, "tried "+patches[0]) {
			t.Errorf("quiet printed the caught patch's line: %q", errOut)
		}
		if !strings.Contains(errOut, "tried "+patches[1]) || !strings.Contains(errOut, "1 exited 0") {
			t.Errorf("quiet dropped the actionable lines: %q", errOut)
		}
	})
}

// TestEachDryRunIsThePreflightAlone: nothing written, no command run.
func TestEachDryRunIsThePreflightAlone(t *testing.T) {
	root, patches := eachFixture(t)
	mark := pidFile(t, "ran")
	before := snapshot(t, root)
	code, out, errOut := runCLI(t, root,
		[]string{"--dry-run", "--try", fmt.Sprintf("echo ran >> '%s'", mark), "--each", patches[0], patches[1]}, "")
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if n := markerLines(t, mark); n != 0 {
		t.Errorf("--dry-run ran the command %d times", n)
	}
	if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
		t.Errorf("tree changed: %v", diff)
	}
	for _, want := range []string{
		patches[0] + "  1 hunk, +1 -1",
		"2 patches would apply, 2 hunks, +2 -2 (dry run: nothing written, command not run)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q: %q", want, out)
		}
	}
	if errOut != "" {
		t.Errorf("a dry run wrote to stderr: %q", errOut)
	}

	t.Run("quiet", func(t *testing.T) {
		code, out, errOut := runCLI(t, root,
			[]string{"--quiet", "--dry-run", "--try", "true", "--each", patches[0]}, "")
		if code != exitOK || out != "" || errOut != "" {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
}

// eachJSONReport unmarshals what --json printed, the fields the tests read.
type eachJSONReport struct {
	OK     bool `json:"ok"`
	Exit   int  `json:"exit"`
	DryRun bool `json:"dry_run"`
	Each   *struct {
		Command string `json:"command"`
		Patches []struct {
			Patch       string   `json:"patch"`
			Ran         bool     `json:"ran"`
			Status      *int     `json:"status"`
			Seconds     float64  `json:"seconds"`
			Hunks       int      `json:"hunks"`
			Added       *int     `json:"added"`
			Removed     *int     `json:"removed"`
			Output      []string `json:"output"`
			RolledBack  int      `json:"rolled_back"`
			TimedOut    float64  `json:"timed_out_after"`
			NotRestored []struct {
				Path string `json:"path"`
			} `json:"not_restored"`
			Error string `json:"error"`
		} `json:"patches"`
		TryBaseline *jsonBuildSeen `json:"try_baseline"`
	} `json:"each"`
	Failures []struct {
		Patch string `json:"patch"`
		Hunk  int    `json:"hunk"`
		Path  string `json:"path"`
	} `json:"failures"`
	Error string `json:"error"`
}

func eachJSONOf(t *testing.T, out string) eachJSONReport {
	t.Helper()
	var r eachJSONReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	return r
}

// TestEachJSON: the object carries the whole run, alone on stdout, while the
// lines stream on stderr.
func TestEachJSON(t *testing.T) {
	t.Run("a run", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root,
			[]string{"--json", "--try", sayingSuite, "--each", patches[0], patches[1]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		r := eachJSONOf(t, out)
		if r.OK || r.Exit != exitVerifyFailed || r.Each == nil {
			t.Fatalf("ok %v exit %d each %v", r.OK, r.Exit, r.Each)
		}
		if r.Each.Command != sayingSuite {
			t.Errorf("command = %q", r.Each.Command)
		}
		if len(r.Each.Patches) != 2 {
			t.Fatalf("%d patches", len(r.Each.Patches))
		}
		p0, p1 := r.Each.Patches[0], r.Each.Patches[1]
		// The caught patch carries its output, flag or no flag (#89).
		if !p0.Ran || p0.Status == nil || *p0.Status != 1 || p0.RolledBack != 1 || len(p0.Output) != 1 || p0.Output[0] != "FAIL: alpha" {
			t.Errorf("caught patch = %+v", p0)
		}
		if !p1.Ran || p1.Status == nil || *p1.Status != 0 || len(p1.Output) != 1 || p1.Output[0] != "ok: alpha" {
			t.Errorf("passing patch = %+v", p1)
		}
		if b := r.Each.TryBaseline; b == nil || !b.Ran || !b.OK || b.Status != 0 || len(b.Output) != 1 || b.Output[0] != "ok: alpha" {
			t.Errorf("try_baseline = %+v", b)
		}
		if !strings.Contains(errOut, "tried "+patches[0]) {
			t.Errorf("the lines did not stream under --json: %q", errOut)
		}
	})

	t.Run("a preflight refusal carries each failure's patch", func(t *testing.T) {
		root, patches := eachFixture(t)
		bad := writePatch(t, t.TempDir(), "bad.hunk", "@@ file a.txt\n@@ old\nmissing\n@@ new\nX\n")
		code, out, _ := runCLI(t, root, []string{"--json", "--try", "true", "--each", patches[1], bad}, "")
		if code != exitNoMatch {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachJSONOf(t, out)
		if len(r.Failures) != 1 || r.Failures[0].Patch != bad || r.Failures[0].Path != "a.txt" {
			t.Errorf("failures = %+v", r.Failures)
		}
		if len(r.Each.Patches) != 2 || r.Each.Patches[0].Ran || r.Each.Patches[1].Ran {
			t.Errorf("patches = %+v", r.Each.Patches)
		}
	})

	t.Run("a parse refusal is the patch's own error", func(t *testing.T) {
		root, patches := eachFixture(t)
		typo := writePatch(t, t.TempDir(), "typo.hunk", "@@ file a.txt\n@@ old\nalpha\n")
		code, out, _ := runCLI(t, root, []string{"--json", "--try", "true", "--each", patches[0], typo}, "")
		if code != exitUsage {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachJSONOf(t, out)
		if len(r.Each.Patches) != 2 || !strings.Contains(r.Each.Patches[1].Error, `"@@ old" has no "@@ new"`) {
			t.Errorf("patches = %+v", r.Each.Patches)
		}
		if r.Each.Patches[0].Error != "" {
			t.Errorf("the good patch has an error: %+v", r.Each.Patches[0])
		}
	})

	t.Run("a stop leaves the rest ran false, and says why at the top", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root,
			[]string{"--json", "--try", "grep -q alpha a.txt || { rm a.txt && mkdir a.txt; }", "--each", patches[0], patches[1]}, "")
		if code != exitRollbackFailed {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachJSONOf(t, out)
		if r.Exit != exitRollbackFailed {
			t.Errorf("exit = %d", r.Exit)
		}
		p0, p1 := r.Each.Patches[0], r.Each.Patches[1]
		if len(p0.NotRestored) != 1 || p0.NotRestored[0].Path != "a.txt" {
			t.Errorf("stopping patch = %+v", p0)
		}
		if p1.Ran || p1.Status != nil {
			t.Errorf("untried patch = %+v", p1)
		}
		if !strings.Contains(r.Error, "1 file was not restored") {
			t.Errorf("error = %q", r.Error)
		}
	})

	t.Run("a dry run carries the diffstat", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root,
			[]string{"--json", "--dry-run", "--try", "true", "--each", patches[0]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachJSONOf(t, out)
		p := r.Each.Patches[0]
		if !r.OK || !r.DryRun || p.Hunks != 1 || p.Added == nil || *p.Added != 1 || *p.Removed != 1 || p.Ran {
			t.Errorf("dry run = ok %v dry %v %+v", r.OK, r.DryRun, p)
		}
	})

	t.Run("a timeout is the patch's, not the run's", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root,
			[]string{"--json", "--try", "if grep -q ALPHA a.txt; then sleep 30; fi", "--timeout", "2s", "--each", patches[0]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3, never 124: %s", code, out)
		}
		p := eachJSONOf(t, out).Each.Patches[0]
		if p.Status == nil || *p.Status != exitTimedOut || p.TimedOut == 0 {
			t.Errorf("timed-out patch = %+v", p)
		}
	})
}

// TestEachInterrupted: a signal ends the command, puts the batch back, stops
// the run, and the exit is 128 plus the signal, as one --try's is.
func TestEachInterrupted(t *testing.T) {
	needsSignals(t)
	t.Run("during a command", func(t *testing.T) {
		root, patches := eachFixture(t)
		started := pidFile(t, "started")
		go func() {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				if _, err := os.Stat(started); err == nil {
					signalSelf(t, syscall.SIGTERM)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		before := snapshot(t, root)
		var out, errOut bytes.Buffer
		var sig os.Signal
		code := invoke([]string{
			"--root", root, "--try", fmt.Sprintf("grep -q ALPHA a.txt || exit 0; echo go > '%s'; sleep 30", started),
			"--each", patches[0], patches[1],
		}, strings.NewReader(""), &out, &errOut, &sig)
		if code != 143 {
			t.Fatalf("exit %d, want 143: %s%s", code, out.String(), errOut.String())
		}
		if sig != syscall.SIGTERM {
			t.Errorf("main would not re-raise SIGTERM: %v", sig)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		for _, want := range []string{
			"interrupted by SIGTERM, the command was ended",
			"not tried: " + patches[1],
			"tried 0 of 2 patches; interrupted by SIGTERM during " + patches[0] + "; 1 patch not tried",
		} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("stderr lacks %q: %q", want, errOut.String())
			}
		}
	})

	t.Run("between the commit and the command", func(t *testing.T) {
		// The handler's second installation is the first patch's commit; the
		// first is before the run on the tree as it is.
		orig := notifyInterrupts
		t.Cleanup(func() { notifyInterrupts = orig })
		calls := 0
		notifyInterrupts = func(c chan<- os.Signal) {
			orig(c)
			if calls++; calls != 2 {
				return
			}
			signalSelf(t, syscall.SIGTERM)
			for deadline := time.Now().Add(5 * time.Second); len(c) == 0; {
				if time.Now().After(deadline) {
					t.Error("the signal never reached the handler")
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
		root, patches := eachFixture(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", catchesAll, "--each", patches[0], patches[1]}, "")
		if code != 143 {
			t.Fatalf("exit %d, want 143: %s%s", code, out, errOut)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		if !strings.Contains(errOut, "interrupted by SIGTERM before the command ran") {
			t.Errorf("stderr = %q", errOut)
		}
	})
}

// The goldens are rendered from constructed outcomes with fixed seconds, as
// the try and verify report goldens are: a live run's timing is nobody's
// contract. Each scenario is the full stream a caller sees, stdout appended
// after the streamed stderr, the order render() uses.
func TestEachReportGoldens(t *testing.T) {
	v := func(status int, edit ...func(*Verify)) *Verify {
		out := &Verify{
			Ran: true, Try: true, Command: "zig build test", Status: status, OK: status == 0,
			Seconds: 41.2, Applied: 1, RolledBack: 1,
		}
		for _, e := range edit {
			e(out)
		}
		return out
	}
	// caughtSays is a caught patch's output: printed only with --verify-lines
	// given (#89), and in --json always.
	caughtSays := func(v *Verify) {
		v.Tail, v.TotalLines = []string{
			"error: 'faults.test.a shadow ray stops at the first hit' failed:",
			"Build Summary: 3/5 steps succeeded (1 failed); 211/212 tests passed (1 failed)",
		}, 14
	}
	// passing is the command's run on the tree as it is, before any patch.
	passing := &Verify{
		Ran: true, OK: true, Try: true, Command: "zig build test", Seconds: 40.8,
		TotalLines: 1, Tail: []string{"All 212 tests passed."},
	}
	stream := func(cfg eachConfig, patches []*eachPatch, o eachOutcome) (string, int) {
		var out, errOut bytes.Buffer
		width := eachNameWidth(patches)
		for _, ep := range patches {
			if ep.v != nil {
				writeEachLine(cfg, &errOut, width, ep)
			}
		}
		code := eachExit(patches, o)
		writeEachEnd(cfg, patches, o, code, &out, &errOut)
		return errOut.String() + out.String(), code
	}

	t.Run("clean", func(t *testing.T) {
		got, code := stream(eachConfig{command: "zig build test"}, []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", v: v(1)},
			{name: "breaks/02-texture.hunk", v: v(2)},
		}, eachOutcome{tried: 2, stopAt: -1, seconds: 83.1})
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-clean", got)
	})

	mixed := func() []*eachPatch {
		return []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", v: v(1, caughtSays)},
			{name: "breaks/06-texture-not-hashed.hunk", v: v(0, func(v *Verify) {
				v.Tail, v.TotalLines = []string{"All 212 tests passed."}, 1
			})},
			{name: "breaks/19-loose.hunk", v: v(exitTimedOut, func(v *Verify) {
				v.TimedOut, v.Seconds = 170*time.Second, 170.0
			})},
		}
	}

	t.Run("mixed", func(t *testing.T) {
		// The caught patch has output, and without --verify-lines given it
		// does not print: this golden is unchanged by #89, on purpose.
		got, code := stream(eachConfig{command: "zig build test"}, mixed(),
			eachOutcome{tried: 3, zeros: 1, timeouts: 1, stopAt: -1, seconds: 249.2})
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-mixed", got)
	})

	t.Run("every tail, with --verify-lines given", func(t *testing.T) {
		got, code := stream(eachConfig{command: "zig build test", everyTail: true}, mixed(),
			eachOutcome{tried: 3, zeros: 1, timeouts: 1, stopAt: -1, seconds: 249.2})
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-every-tail", got)
	})

	t.Run("refused: the command fails with no patch applied", func(t *testing.T) {
		var out, errOut bytes.Buffer
		c := &Verify{
			Ran: true, Try: true, Command: "pytest -q -k GoogleLoger tests/", Status: 5, Seconds: 0.6,
			TotalLines: 1, Tail: []string{"5 deselected in 0.31s"},
		}
		code := eachBaselineRefused(eachConfig{command: c.Command}, mixed(), eachBaselines{command: c}, c,
			baselineFailure("the command", c, "caught"), exitUsage, &out, &errOut)
		if code != exitUsage {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-command-baseline", errOut.String()+out.String())
	})

	t.Run("json, refused: the command fails with no patch applied", func(t *testing.T) {
		var out, errOut bytes.Buffer
		c := &Verify{
			Ran: true, Try: true, Command: "pytest -q -k GoogleLoger tests/", Status: 5, Seconds: 0.6,
			TotalLines: 1, Tail: []string{"5 deselected in 0.31s"},
		}
		patches := []*eachPatch{{name: "breaks/01-keep-in-payload.hunk"}, {name: "breaks/02-handler-wins.hunk"}}
		code := eachBaselineRefused(eachConfig{command: c.Command, asJSON: true}, patches, eachBaselines{command: c}, c,
			baselineFailure("the command", c, "caught"), exitUsage, &out, &errOut)
		if code != exitUsage {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-command-baseline-json", out.String())
	})

	t.Run("stopped, a file not restored", func(t *testing.T) {
		patches := []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", v: v(0, func(v *Verify) {
				v.RolledBack = 0
				v.NotRestored = []NotRestored{{Path: "src/faults.zig", Reason: "it changed after hunk wrote it, so restoring would discard somebody else's work; --verify-may-format says the verify command is expected to rewrite files"}}
			})},
			{name: "breaks/02-texture.hunk"},
			{name: "breaks/19-loose.hunk"},
		}
		got, code := stream(eachConfig{command: "zig build test"}, patches,
			eachOutcome{tried: 0, stopAt: 0, seconds: 41.2})
		if code != exitRollbackFailed {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-stopped", got)
	})

	t.Run("stopped, the tree changed under the run", func(t *testing.T) {
		found := 0
		patches := []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", v: v(1)},
			{name: "breaks/02-texture.hunk", failures: []Failure{
				{Hunk: 1, Path: "src/faults.zig", PatchLine: 2, Expected: 1, Found: found},
			}},
			{name: "breaks/19-loose.hunk"},
		}
		got, code := stream(eachConfig{command: "zig build test"}, patches,
			eachOutcome{tried: 1, stopAt: 1, stopErr: &ValidationError{Failures: patches[1].failures}, seconds: 44.0})
		if code != exitNoMatch {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-drift", got)
	})

	t.Run("interrupted during a command", func(t *testing.T) {
		patches := []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", v: v(143, func(v *Verify) { v.Signal = syscall.SIGTERM })},
			{name: "breaks/02-texture.hunk"},
		}
		got, code := stream(eachConfig{command: "zig build test"}, patches,
			eachOutcome{tried: 0, stopAt: 0, sig: syscall.SIGTERM, seconds: 3.0})
		if code != 143 {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-interrupted", got)
	})

	t.Run("refused: a patch could not be read", func(t *testing.T) {
		var out, errOut bytes.Buffer
		patches := []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk"},
			{name: "breaks/19-typo.hunk", loadErr: fmt.Errorf(`patch line 2: "@@ old" has no "@@ new"; every old needs the text to put in its place, even if that text is empty`)},
		}
		code := eachRefused(eachConfig{command: "zig build test"}, patches, "could not be read", exitUsage, &out, &errOut)
		if code != exitUsage {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-refused", errOut.String()+out.String())
	})

	t.Run("refused: a patch would not apply", func(t *testing.T) {
		var out, errOut bytes.Buffer
		found := 0
		patches := []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk"},
			{name: "breaks/06-texture-not-hashed.hunk", failures: []Failure{
				{Hunk: 1, Path: "src/faults.zig", PatchLine: 2, Expected: 1, Found: found},
			}},
		}
		code := eachRefused(eachConfig{command: "zig build test"}, patches, "would not apply", exitNoMatch, &out, &errOut)
		if code != exitNoMatch {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-would-not-apply", errOut.String()+out.String())
	})

	t.Run("dry run", func(t *testing.T) {
		var out bytes.Buffer
		patches := []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", res: &Result{Hunks: 1, Files: []FileResult{{Path: "src/faults.zig", Op: "modify", Added: 1, Removed: 1}}}},
			{name: "breaks/06-texture-not-hashed.hunk", res: &Result{Hunks: 2, Files: []FileResult{{Path: "src/metadata.zig", Op: "modify", Added: 3, Removed: 2}}}},
		}
		code := eachDryRun(eachConfig{command: "zig build test", dryRun: true}, patches, &out)
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-dry-run", out.String())
	})

	t.Run("json", func(t *testing.T) {
		// The caught patch's output is there, and the command's run on the
		// tree as it is: both #89's and #90's, read on 2026-10-16.
		var out, errOut bytes.Buffer
		code := eachJSON(eachConfig{command: "zig build test", asJSON: true}, mixed(), eachBaselines{command: passing}, exitVerifyFailed, "", &out, &errOut)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-json", out.String())
	})

	t.Run("json, refused", func(t *testing.T) {
		var out, errOut bytes.Buffer
		found := 0
		patches := []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk"},
			{name: "breaks/06-texture-not-hashed.hunk", failures: []Failure{
				{Hunk: 1, Path: "src/faults.zig", PatchLine: 2, Expected: 1, Found: found},
			}},
			{name: "breaks/19-typo.hunk", loadErr: fmt.Errorf(`patch line 2: "@@ old" has no "@@ new"`)},
		}
		code := eachJSON(eachConfig{command: "zig build test", asJSON: true}, patches, eachBaselines{}, exitUsage, "", &out, &errOut)
		if code != exitUsage {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-refused-json", out.String())
	})
}

// FuzzEachDryRunRefusesOrCounts asserts totality over what an agent hands
// --each: any tree content and any two patch files either refuse, at 1, 2 or
// 5, or preview, at 0, and the tree is untouched either way, since the
// preflight is a dry run and the command never runs under --dry-run.
func FuzzEachDryRunRefusesOrCounts(f *testing.F) {
	f.Add([]byte("alpha\n"), []byte("@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n"), []byte("@@ file a.txt\n@@ old\nalpha\n@@ new\nX\nY\n"))
	f.Add([]byte("alpha\n"), []byte("@@ file a.txt\n@@ old\nalpha\n"), []byte("not a patch"))
	f.Add([]byte("alpha\n"), []byte("@@ file ../out.txt\n@@ old\nx\n@@ new\ny\n"), []byte("@@ delete a.txt\n"))
	f.Add([]byte{0}, []byte("@@ create b.txt\n@@ new\nx\n"), []byte("@@ file .git/config\n@@ old\nx\n@@ new\ny\n"))
	f.Fuzz(func(t *testing.T, content, p1, p2 []byte) {
		root := t.TempDir()
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		must(t, os.WriteFile(filepath.Join(root, "a.txt"), content, 0o644))
		dir := t.TempDir()
		one := filepath.Join(dir, "one.hunk")
		two := filepath.Join(dir, "two.hunk")
		must(t, os.WriteFile(one, p1, 0o644))
		must(t, os.WriteFile(two, p2, 0o644))
		before := snapshot(t, root)
		var out, errOut bytes.Buffer
		code := cli([]string{"--root", root, "--dry-run", "--try", "true", "--each", one, two},
			strings.NewReader(""), &out, &errOut)
		switch code {
		case exitOK, exitUsage, exitNoMatch, exitIO:
		default:
			t.Fatalf("exit %d is outside --dry-run --each's set\n%s%s", code, out.String(), errOut.String())
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Fatalf("a dry run changed the tree: %v", diff)
		}
		if code == exitOK && !strings.Contains(out.String(), "2 patches would apply") {
			t.Fatalf("exit 0 without the summary: %q", out.String())
		}
	})
}

// breakShellAtTheFirstCommit breaks the shell between the run on the tree as
// it is, which needs it, and the first patch's command: the handler's second
// installation, at the first patch's commit, is the seam.
func breakShellAtTheFirstCommit(t *testing.T) {
	t.Helper()
	broken := brokenShell(t)
	t.Setenv("PATH", os.Getenv("PATH"))
	orig := notifyInterrupts
	t.Cleanup(func() { notifyInterrupts = orig })
	calls := 0
	notifyInterrupts = func(c chan<- os.Signal) {
		orig(c)
		if calls++; calls == 2 {
			must(t, os.Setenv("PATH", broken))
		}
	}
}

// TestEachStopsWhenTheCommandCannotStart: a shell that disappears mid-run
// stops the run at 5 with the batch put back; later patches would only fail
// the same way.
func TestEachStopsWhenTheCommandCannotStart(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		root, patches := eachFixture(t)
		breakShellAtTheFirstCommit(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", "true", "--each", patches[0], patches[1]}, "")
		if code != exitIO {
			t.Fatalf("exit %d, want 5: %s%s", code, out, errOut)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		for _, want := range []string{
			"tried " + patches[0] + "  the command could not start",
			"hunk: at " + patches[0] + ": could not run the --try command",
			"; the batch was put back",
			"not tried: " + patches[1],
			"tried 0 of 2 patches; stopped at " + patches[0] + "; 1 patch not tried",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
	})

	t.Run("json says why at the top", func(t *testing.T) {
		root, patches := eachFixture(t)
		breakShellAtTheFirstCommit(t)
		code, out, _ := runCLI(t, root, []string{"--json", "--try", "true", "--each", patches[0], patches[1]}, "")
		if code != exitIO {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachJSONOf(t, out)
		if !strings.Contains(r.Error, "at "+patches[0]+": could not run the --try command") {
			t.Errorf("error = %q", r.Error)
		}
		if p := r.Each.Patches[0]; p.Ran {
			t.Errorf("a command that could not start is ran true: %+v", p)
		}
	})
}

// A mid-run drift under --json carries the stop at the top and the failures
// with the patch's name, which is eachStopReason's other half.
func TestEachJSONSaysWhyADriftStopped(t *testing.T) {
	root, patches := eachFixture(t)
	code, out, _ := runCLI(t, root,
		[]string{"--json", "--try", "printf 'junk\\n' > b.txt", "--each", patches[0], patches[1]}, "")
	if code != exitNoMatch {
		t.Fatalf("exit %d: %s", code, out)
	}
	r := eachJSONOf(t, out)
	if !strings.Contains(r.Error, patches[1]+" no longer applies; the preflight matched every patch") {
		t.Errorf("error = %q", r.Error)
	}
	if len(r.Failures) != 1 || r.Failures[0].Patch != patches[1] {
		t.Errorf("failures = %+v", r.Failures)
	}
}

// TestEachInterruptedBetweenPatches: a signal that lands after one patch is
// back and before the next begins stops the run with the tree whole. The
// window is real: the command exits while something it started still holds
// its output, and the signal arrives while hunk drains it.
func TestEachInterruptedBetweenPatches(t *testing.T) {
	needsSignals(t)
	root, patches := eachFixture(t)
	flag := pidFile(t, "go")
	go func() {
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if _, err := os.Stat(filepath.FromSlash(flag)); err == nil {
				time.Sleep(500 * time.Millisecond)
				signalSelf(t, syscall.SIGTERM)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	var out, errOut bytes.Buffer
	var sig os.Signal
	code := invoke([]string{
		"--root", root, "--try", fmt.Sprintf("grep -q alpha a.txt && exit 0; echo go > '%s'; sleep 3 & exit 1", flag),
		"--each", patches[0], patches[1],
	}, strings.NewReader(""), &out, &errOut, &sig)
	if code != 143 {
		t.Fatalf("exit %d, want 143: %s%s", code, out.String(), errOut.String())
	}
	if sig != syscall.SIGTERM {
		t.Errorf("main would not re-raise SIGTERM: %v", sig)
	}
	for _, want := range []string{
		"tried " + patches[0],
		"the command exited 1",
		"not tried: " + patches[1],
		"tried 1 of 2 patches; interrupted by SIGTERM; 1 patch not tried",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q: %q", want, errOut.String())
		}
	}
}

// A commit that failed part-way mid-run names what it could not put back,
// through the same block exit 4 uses. The error is constructed, as the
// rollback-incomplete report golden's is, because a commit that half-fails on
// purpose needs a planted race this renderer does not.
func TestEachEndWritesACommitFailure(t *testing.T) {
	patches := []*eachPatch{
		{name: "breaks/01.hunk", v: &Verify{Ran: true, Try: true, Status: 1, Seconds: 1.0, Applied: 1, RolledBack: 1}},
		{name: "breaks/02.hunk"},
	}
	stopErr := &CommitError{
		Err:         errors.New("open b.txt: permission denied"),
		NotRestored: []NotRestored{{Path: "b.txt", Reason: "it could not be opened to write the original bytes back"}},
	}
	var out, errOut bytes.Buffer
	writeEachEnd(eachConfig{command: "true"}, patches,
		eachOutcome{tried: 1, stopAt: 1, stopErr: stopErr, seconds: 2.0}, exitIO, &out, &errOut)
	got := errOut.String()
	for _, want := range []string{
		"hunk: at breaks/02.hunk: ",
		"b.txt was not restored.",
		"tried 1 of 2 patches; stopped at breaks/02.hunk",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q: %q", want, got)
		}
	}
}

// TestEachStopClauseCountsFiles is the stop clause's plural, which no live
// row reaches: one failed try that leaves two files changed. It was the one
// statement of each.go no test ran, at 88.9% of eachStopClause, from #84's
// merge until 2026-10-15, though the card said 100%.
//
// Under --isolated the files are in the copy (#130), and the clause says so:
// the run stopped, and the tree itself was never written.
func TestEachStopClauseCountsFiles(t *testing.T) {
	for _, c := range []struct {
		n      int
		inCopy bool
		want   string
	}{
		{1, false, "stopped at breaks/01.hunk: 1 file was not restored"},
		{2, false, "stopped at breaks/01.hunk: 2 files were not restored"},
		{1, true, "stopped at breaks/01.hunk: 1 file was not restored in the copy"},
		{2, true, "stopped at breaks/01.hunk: 2 files were not restored in the copy"},
	} {
		nr := make([]NotRestored, c.n)
		for i := range nr {
			nr[i] = NotRestored{Path: fmt.Sprintf("f%d.txt", i), Reason: "changed"}
		}
		patches := []*eachPatch{{name: "breaks/01.hunk", v: &Verify{Ran: true, Try: true, NotRestored: nr}}}
		if got := eachStopClause(patches, eachOutcome{stopAt: 0, inCopy: c.inCopy}); got != c.want {
			t.Errorf("%d files, in a copy %v: %q, want %q", c.n, c.inCopy, got, c.want)
		}
	}
}

// The --json report can itself fail to write, and the message must not claim
// a tree state the run did not end in.
func TestEachJSONReportCannotBeWritten(t *testing.T) {
	patches := []*eachPatch{{name: "breaks/01.hunk", v: &Verify{Ran: true, Try: true, Status: 0, Seconds: 1.0, Applied: 1}}}
	for _, c := range []struct {
		name string
		code int
		say  string
	}{
		{"with the tree whole", exitVerifyFailed, "hunk: the --json report could not be written: disk gone\n"},
		{"with a file not restored", exitRollbackFailed, "hunk: a file was not restored, and the --json report could not be written: disk gone\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var errOut bytes.Buffer
			code := eachJSON(eachConfig{command: "true", asJSON: true}, patches, eachBaselines{}, c.code, "", failingWriter{}, &errOut)
			if code != exitIO {
				t.Errorf("exit %d, want 5", code)
			}
			if errOut.String() != c.say {
				t.Errorf("stderr = %q, want %q", errOut.String(), c.say)
			}
		})
	}
}
