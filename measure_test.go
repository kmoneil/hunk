package main

// --each --measure's tests (#154, §4.3). The live rows drive the real CLI
// against a real tree, with a command that logs what it was given and what
// it found, one line per run, so the order of the runs, the tree each ran
// on and the environment each got are one read of the log. The goldens are
// rendered from constructed outcomes, as --each's are, since a live run's
// seconds are nobody's contract.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// measureLog is a command that appends one line per run to log: HUNK_PATCH,
// HUNK_ROUND and the three words of eachFixture's files as it found them;
// then it prints a.txt, which is its output tail.
func measureLog(log string) string {
	return fmt.Sprintf(`set -- $(cat a.txt b.txt c.txt); printf '%%s %%s %%s %%s %%s\n' "${HUNK_PATCH-unset}" "${HUNK_ROUND-unset}" "$1" "$2" "$3" >> '%s'; cat a.txt`, log)
}

// measureLines is what measureLog wrote, a line a run.
func measureLines(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(log))
	if os.IsNotExist(err) {
		return nil
	}
	must(t, err)
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

// measureJSONReport is what --json printed under --measure, the fields the
// tests read.
type measureJSONReport struct {
	OK       bool   `json:"ok"`
	Exit     int    `json:"exit"`
	Error    string `json:"error"`
	Isolated bool   `json:"isolated"`
	Each     *struct {
		Measure     bool            `json:"measure"`
		Rounds      int             `json:"rounds"`
		TryBaseline json.RawMessage `json:"try_baseline"`
		Baseline    json.RawMessage `json:"baseline"`
		Patches     []struct {
			Patch  string          `json:"patch"`
			Base   bool            `json:"base"`
			Round  int             `json:"round"`
			Ran    bool            `json:"ran"`
			Status *int            `json:"status"`
			Output []string        `json:"output"`
			Build  json.RawMessage `json:"build"`
		} `json:"patches"`
	} `json:"each"`
	Warnings []struct {
		Kind  string `json:"kind"`
		Patch string `json:"patch"`
	} `json:"warnings"`
}

// TestMeasureRunsBaseThenEachPatchInEveryRound: base is the tree as it is,
// each patch runs alone on it, and rounds interleave them, every file back
// at the end and every run its own line with its tail.
func TestMeasureRunsBaseThenEachPatchInEveryRound(t *testing.T) {
	t.Run("one round", func(t *testing.T) {
		root, patches := eachFixture(t)
		log := pidFile(t, "log")
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--try", measureLog(log), "--measure", "--each", patches[0], patches[1]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		assertUnchanged(t, root, before)
		sameLines(t, "the runs", measureLines(t, log), []string{
			"base 1 alpha beta gamma",
			patches[0] + " 1 ALPHA beta gamma",
			patches[1] + " 1 alpha BETA gamma",
		})
		if !strings.HasPrefix(out, "tried base and 2 patches and put back every file; every command exited 0 (") {
			t.Errorf("stdout = %q", out)
		}
		// Every run's tail prints without --verify-lines: the output is the
		// measurement. No round column with one round.
		for _, want := range []string{"tried base", "\nALPHA\n", "\nalpha\n", "the command exited 0"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
		// The temp dir's name has "round" in it; the column is "  round N  ".
		if strings.Contains(errOut, "  round 1  ") {
			t.Errorf("one round has no round column: %q", errOut)
		}
	})

	t.Run("three rounds, interleaved", func(t *testing.T) {
		root, patches := eachFixture(t)
		log := pidFile(t, "log")
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--try", measureLog(log), "--measure", "--rounds", "3", "--each", patches[0], patches[1]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		assertUnchanged(t, root, before)
		var want []string
		for r := 1; r <= 3; r++ {
			want = append(want,
				fmt.Sprintf("base %d alpha beta gamma", r),
				fmt.Sprintf("%s %d ALPHA beta gamma", patches[0], r),
				fmt.Sprintf("%s %d alpha BETA gamma", patches[1], r))
		}
		sameLines(t, "the runs", measureLines(t, log), want)
		if !strings.HasPrefix(out, "tried base and 2 patches in 3 rounds and put back every file; every command exited 0 (") {
			t.Errorf("stdout = %q", out)
		}
		width := len(patches[0])
		for r := 1; r <= 3; r++ {
			for _, name := range []string{"base", patches[0], patches[1]} {
				line := fmt.Sprintf("tried %-*s  round %d  the command exited 0 (", width, name, r)
				if !strings.Contains(errOut, line) {
					t.Errorf("stderr lacks %q: %q", line, errOut)
				}
			}
		}
	})

	t.Run("a round column as wide as the last round", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root,
			[]string{"--quiet", "--try", "true", "--measure", "--rounds", "10", "--each", patches[0]}, "")
		if code != exitOK || out != "" || errOut != "" {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		code, _, errOut = runCLI(t, root,
			[]string{"--try", "true", "--measure", "--rounds", "10", "--each", patches[0]}, "")
		width := len(patches[0])
		for _, line := range []string{
			fmt.Sprintf("tried %-*s  round 1   the command", width, "base"),
			fmt.Sprintf("tried %-*s  round 10  the command", width, patches[0]),
		} {
			if code != exitOK || !strings.Contains(errOut, line) {
				t.Errorf("exit %d, stderr lacks %q: %q", code, line, errOut)
			}
		}
	})
}

// TestMeasureScoresNoStatus: under --measure a command's status is a result
// and never a catch, the tree as it is is no gate, and nothing runs after the
// last patch; only a run that timed out or did not build makes the exit 3.
func TestMeasureScoresNoStatus(t *testing.T) {
	t.Run("nonzero exits are results: exit 0, and their tails print", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		code, out, errOut := runCLI(t, root,
			[]string{"--try", counted(mark, sayingSuite), "--measure", "--each", patches[0], patches[1]}, "")
		if code != exitOK {
			t.Fatalf("exit %d, want 0: %s%s", code, out, errOut)
		}
		if !strings.Contains(out, "; 2 exited 0, 1 exited nonzero (") {
			t.Errorf("stdout = %q", out)
		}
		if !strings.Contains(errOut, "FAIL: alpha") {
			t.Errorf("the run that exited 1 should have its tail printed: %q", errOut)
		}
		// Three runs, and no fourth after the last patch (#110 is off).
		if n := markerLines(t, mark); n != 3 {
			t.Errorf("the command ran %d times, want 3", n)
		}
	})

	t.Run("a base that fails is an entry, not a gate", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		code, out, errOut := runCLI(t, root,
			[]string{"--try", counted(mark, "echo measured; exit 7"), "--measure", "--rounds", "2", "--each", patches[0]}, "")
		if code != exitOK {
			t.Fatalf("exit %d, want 0: %s%s", code, out, errOut)
		}
		if n := markerLines(t, mark); n != 4 {
			t.Errorf("the command ran %d times, want 4", n)
		}
		if strings.Contains(errOut, "with no patch applied") || !strings.Contains(out, "; 4 exited nonzero (") {
			t.Errorf("stdout %q, stderr %q", out, errOut)
		}
	})

	t.Run("a run that timed out: exit 3", func(t *testing.T) {
		root, patches := eachFixture(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--try", "grep -q ALPHA a.txt && sleep 5; exit 0", "--timeout", "300ms", "--measure", "--each", patches[0], patches[1]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s%s", code, out, errOut)
		}
		assertUnchanged(t, root, before)
		if !strings.Contains(eachLine(errOut, patches[0]), "the command timed out after 300ms") ||
			!strings.Contains(errOut, "tried base and 2 patches and put back every file; 2 exited 0, 1 timed out (") {
			t.Errorf("stdout %q, stderr %q", out, errOut)
		}
	})

	t.Run("a command piped into a filter is let through", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		code, out, errOut := runCLI(t, root,
			[]string{"--try", counted(mark, "cat a.txt | tail -1"), "--measure", "--each", patches[0]}, "")
		if code != exitOK || markerLines(t, mark) != 2 || !strings.Contains(errOut, "ALPHA") {
			t.Errorf("exit %d: %s%s", code, out, errOut)
		}
	})

	t.Run("in a copy of the tree", func(t *testing.T) {
		root, patches := eachFixture(t)
		log := pidFile(t, "log")
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--try", measureLog(log), "--isolated", "--measure", "--each", patches[0]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		assertUnchanged(t, root, before)
		sameLines(t, "the runs", measureLines(t, log), []string{"base 1 alpha beta gamma", patches[0] + " 1 ALPHA beta gamma"})
		if !strings.HasPrefix(out, "tried base and 1 patch in a copy of the tree; every command exited 0 (") {
			t.Errorf("stdout = %q", out)
		}
	})
}

// TestMeasureBuild: under --measure the build runs before every run's
// command, base's included, since a patch's build leaves what it built; one
// that fails is that run's line and makes the exit 3, never a refusal.
func TestMeasureBuild(t *testing.T) {
	t.Run("the build before every run, with the run's environment", func(t *testing.T) {
		root, patches := eachFixture(t)
		log := pidFile(t, "log")
		say := func(what string) string {
			return fmt.Sprintf(`printf '%s %%s %%s\n' "$HUNK_PATCH" "$HUNK_ROUND" >> '%s'`, what, log)
		}
		code, out, errOut := runCLI(t, root,
			[]string{"--try", say("run"), "--build", say("build"), "--measure", "--rounds", "2", "--each", patches[0]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		var want []string
		for r := 1; r <= 2; r++ {
			for _, name := range []string{"base", patches[0]} {
				want = append(want, fmt.Sprintf("build %s %d", name, r), fmt.Sprintf("run %s %d", name, r))
			}
		}
		sameLines(t, "the build and the command, in order", measureLines(t, log), want)
	})

	t.Run("base does not build: its line, its command not run, exit 3", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		code, out, errOut := runCLI(t, root,
			[]string{"--try", counted(mark, "true"), "--build", "echo broken; grep -q ALPHA a.txt", "--measure", "--each", patches[0]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s%s", code, out, errOut)
		}
		if n := markerLines(t, mark); n != 1 {
			t.Errorf("the command ran %d times, want 1: under the patch, not on base", n)
		}
		if !strings.Contains(eachLine(errOut, "base"), "did not build: the build exited 1") ||
			!strings.Contains(errOut, "broken") ||
			!strings.Contains(errOut, "; 1 exited 0, 1 did not build (") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("a patch does not build: exit 3, put back", func(t *testing.T) {
		root, patches := eachFixture(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--try", "true", "--build", "! grep -q ALPHA a.txt", "--measure", "--each", patches[0], patches[1]}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s%s", code, out, errOut)
		}
		assertUnchanged(t, root, before)
		if !strings.Contains(eachLine(errOut, patches[0]), "did not build") || !strings.Contains(errOut, "; 2 exited 0, 1 did not build (") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("a build piped into a filter is still refused", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root,
			[]string{"--try", counted(mark, "true"), "--build", "false | tail -1", "--measure", "--each", patches[0]}, "")
		if code != exitUsage || !strings.Contains(errOut, "--build cannot see the build piped into tail fail") {
			t.Errorf("exit %d, stderr %q", code, errOut)
		}
		if n := markerLines(t, mark); n != 0 {
			t.Errorf("the command ran %d times, want none", n)
		}
		assertUnchanged(t, root, before)
	})
}

// TestMeasureEnvironment: HUNK_PATCH and HUNK_ROUND are each run's own, over
// whatever the caller's environment had, and only under --measure.
func TestMeasureEnvironment(t *testing.T) {
	t.Run("over an inherited value", func(t *testing.T) {
		t.Setenv("HUNK_PATCH", "inherited")
		t.Setenv("HUNK_ROUND", "inherited")
		root, patches := eachFixture(t)
		log := pidFile(t, "log")
		code, out, errOut := runCLI(t, root, []string{"--try", measureLog(log), "--measure", "--each", patches[0]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		sameLines(t, "the runs", measureLines(t, log), []string{"base 1 alpha beta gamma", patches[0] + " 1 ALPHA beta gamma"})
	})

	t.Run("a break set's entries by FILE:NAME", func(t *testing.T) {
		root, _ := eachFixture(t)
		set := writePatch(t, t.TempDir(), "set.hunk",
			"@@ patch up\n@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n@@ patch down\n@@ file b.txt\n@@ old\nbeta\n@@ new\nBETA\n")
		log := pidFile(t, "log")
		code, out, errOut := runCLI(t, root, []string{"--try", measureLog(log), "--measure", "--each", set}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		sameLines(t, "the runs", measureLines(t, log), []string{
			"base 1 alpha beta gamma", set + ":up 1 ALPHA beta gamma", set + ":down 1 alpha BETA gamma",
		})
	})

	t.Run("not set outside --measure", func(t *testing.T) {
		t.Setenv("HUNK_PATCH", "")
		t.Setenv("HUNK_ROUND", "")
		must(t, os.Unsetenv("HUNK_PATCH"))
		must(t, os.Unsetenv("HUNK_ROUND"))
		root, patches := eachFixture(t)
		log := pidFile(t, "log")
		code, out, errOut := runCLI(t, root, []string{"--try", measureLog(log) + "; grep -q alpha a.txt", "--each", patches[0]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		for _, l := range measureLines(t, log) {
			if !strings.HasPrefix(l, "unset unset ") {
				t.Errorf("a run outside --measure got %q", l)
			}
		}
	})

	// The documents name the variables the code sets, every one of them.
	t.Run("the variables are documented", func(t *testing.T) {
		names := map[string]bool{}
		for _, kv := range (&eachPatch{name: "p", round: 1}).env() {
			name, _, _ := strings.Cut(kv, "=")
			names[name] = true
		}
		if len(names) != 2 {
			t.Fatalf("env() sets %v", names)
		}
		skill, err := os.ReadFile(filepath.Join(".agents", "skills", "hunk", "SKILL.md"))
		must(t, err)
		readme, err := os.ReadFile("README.md")
		must(t, err)
		for name := range names {
			for doc, text := range map[string]string{"--help": usage, "README.md": string(readme), "SKILL.md": string(skill)} {
				if !strings.Contains(text, name) {
					t.Errorf("%s does not name %s", doc, name)
				}
			}
		}
	})
}

// TestMeasureQuiet: a run that ended by itself is no news under --measure,
// whatever it exited; one that timed out still prints.
func TestMeasureQuiet(t *testing.T) {
	t.Run("every run ended by itself: nothing", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--quiet", "--try", sayingSuite, "--measure", "--each", patches[0], patches[1]}, "")
		if code != exitOK || out != "" || errOut != "" {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})

	t.Run("a run timed out: its line and the summary", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root,
			[]string{"--quiet", "--try", "grep -q ALPHA a.txt && sleep 5; exit 0", "--timeout", "300ms", "--measure", "--each", patches[0], patches[1]}, "")
		if code != exitVerifyFailed || out != "" {
			t.Fatalf("exit %d, stdout %q", code, out)
		}
		if strings.Count(errOut, "\ntried ") != 1 || !strings.HasPrefix(errOut, "tried "+patches[0]) ||
			!strings.Contains(errOut, "tried base and 2 patches and put back every file; 2 exited 0, 1 timed out") {
			t.Errorf("stderr = %q", errOut)
		}
	})
}

// TestMeasureJSON: the run's entries one per run, with their rounds and base
// marked, every run's output, no gate runs, and a patch's warnings once.
func TestMeasureJSON(t *testing.T) {
	root, patches := eachFixture(t)
	same := writePatch(t, t.TempDir(), "same.hunk", "@@ file a.txt\n@@ old\nalpha\n@@ new\nalpha\n")
	code, out, errOut := runCLI(t, root,
		[]string{"--json", "--try", sayingSuite, "--measure", "--rounds", "2", "--each", patches[0], same}, "")
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	var r measureJSONReport
	must(t, json.Unmarshal([]byte(out), &r))
	if !r.OK || r.Exit != exitOK || r.Each == nil || !r.Each.Measure || r.Each.Rounds != 2 {
		t.Fatalf("%s", out)
	}
	if r.Each.TryBaseline != nil || r.Each.Baseline != nil {
		t.Errorf("no gate ran, so no baseline: %s", out)
	}
	type entry struct {
		patch  string
		base   bool
		round  int
		status int
		out    string
	}
	var want []entry
	for round := 1; round <= 2; round++ {
		want = append(want,
			entry{"base", true, round, 0, "ok: alpha"},
			entry{patches[0], false, round, 1, "FAIL: alpha"},
			entry{same, false, round, 0, "ok: alpha"})
	}
	if len(r.Each.Patches) != len(want) {
		t.Fatalf("%d entries, want %d: %s", len(r.Each.Patches), len(want), out)
	}
	for i, w := range want {
		p := r.Each.Patches[i]
		if p.Patch != w.patch || p.Base != w.base || p.Round != w.round || !p.Ran || p.Status == nil || *p.Status != w.status ||
			len(p.Output) != 1 || p.Output[0] != w.out {
			t.Errorf("entry %d = %+v, want %+v", i, p, w)
		}
	}
	// same.hunk changes nothing: warned of once, not once a round.
	if len(r.Warnings) != 1 || r.Warnings[0].Kind != "no_change" || r.Warnings[0].Patch != same {
		t.Errorf("warnings = %+v", r.Warnings)
	}
	t.Run("the text warns once too", func(t *testing.T) {
		_, _, errOut := runCLI(t, root, []string{"--try", "true", "--measure", "--rounds", "2", "--each", same}, "")
		if n := strings.Count(errOut, "warning: "); n != 1 {
			t.Errorf("%d warnings: %q", n, errOut)
		}
	})
	// A mistyped delimiter is the patch's, which every round's entry
	// carries, where a slip is the preflight's, which only the first does:
	// the mutant that warned of each round's found this row missing.
	t.Run("a mistyped delimiter once", func(t *testing.T) {
		stray := writePatch(t, t.TempDir(), "stray.hunk", "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\nHUHUNK\n")
		code, out, errOut := runCLI(t, root,
			[]string{"--json", "--allow-mistyped-delimiter", "--try", "true", "--measure", "--rounds", "3", "--each", stray}, "")
		var r measureJSONReport
		must(t, json.Unmarshal([]byte(out), &r))
		if code != exitOK || len(r.Warnings) != 1 || r.Warnings[0].Kind != "mistyped_delimiter" || r.Warnings[0].Patch != stray {
			t.Errorf("exit %d, warnings %+v: %s", code, r.Warnings, errOut)
		}
	})
}

// TestMeasureBeforeTheFirstRun: the preflight and --dry-run are --each's,
// with the patches as entries and nothing run.
func TestMeasureBeforeTheFirstRun(t *testing.T) {
	t.Run("--dry-run is the preflight alone", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		code, out, errOut := runCLI(t, root, []string{"--dry-run", "--try", counted(mark, "true"), "--measure", "--rounds", "3", "--each", patches[0], patches[1]}, "")
		if code != exitOK || !strings.Contains(out, "2 patches would apply") || markerLines(t, mark) != 0 {
			t.Errorf("exit %d, ran %d: %s%s", code, markerLines(t, mark), out, errOut)
		}
	})

	t.Run("a patch that would not apply refuses the lot", func(t *testing.T) {
		root, patches := eachFixture(t)
		bad := writePatch(t, t.TempDir(), "bad.hunk", "@@ file a.txt\n@@ old\nomega\n@@ new\nOMEGA\n")
		mark := pidFile(t, "ran")
		code, out, errOut := runCLI(t, root, []string{"--json", "--try", counted(mark, "true"), "--measure", "--each", patches[0], bad}, "")
		if code != exitNoMatch || markerLines(t, mark) != 0 {
			t.Fatalf("exit %d, ran %d: %s%s", code, markerLines(t, mark), out, errOut)
		}
		var r measureJSONReport
		must(t, json.Unmarshal([]byte(out), &r))
		// Per patch, not per run: no run began.
		if r.Each == nil || len(r.Each.Patches) != 2 || r.Each.Patches[0].Round != 0 || r.Each.Patches[0].Base {
			t.Errorf("%s", out)
		}
	})
}

// TestMeasureFlagRefusals: each refusal at exit 1, named, before anything
// runs or is read.
func TestMeasureFlagRefusals(t *testing.T) {
	root, patches := eachFixture(t)
	for _, c := range []struct {
		name string
		args func(cmd string) []string
		says string
	}{
		{
			"--measure without --each", func(cmd string) []string { return []string{"--try", cmd, "--measure", "-f", patches[0]} },
			"--measure reports every run of --try CMD --each, and does nothing without --each",
		},
		{
			"--measure without --try", func(string) []string { return []string{"--measure", "-f", patches[0]} },
			"--measure reports every run of --try CMD --each, and does nothing without --each",
		},
		{
			"--rounds without --measure", func(cmd string) []string { return []string{"--try", cmd, "--rounds", "2", "--each", patches[0]} },
			"--rounds repeats a --measure run, and does nothing without --measure",
		},
		{
			"--rounds 1 given without --measure", func(cmd string) []string { return []string{"--try", cmd, "--rounds", "1", "--each", patches[0]} },
			"--rounds repeats a --measure run, and does nothing without --measure",
		},
		{
			"--rounds 0", func(cmd string) []string {
				return []string{"--try", cmd, "--measure", "--rounds", "0", "--each", patches[0]}
			},
			"--rounds must be at least 1, not 0",
		},
		{
			"--rounds -3", func(cmd string) []string {
				return []string{"--try", cmd, "--measure", "--rounds", "-3", "--each", patches[0]}
			},
			"--rounds must be at least 1, not -3",
		},
		{
			"a patch named base", func(cmd string) []string { return []string{"--try", cmd, "--measure", "--each", patches[0], "base"} },
			"a patch file named base would read as --measure's run with no patch applied, which is named base; name it ./base",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			mark := pidFile(t, "ran")
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, c.args(counted(mark, "true")), "")
			if code != exitUsage || out != "" || errOut != "hunk: "+c.says+"\n" {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
			if n := markerLines(t, mark); n != 0 {
				t.Errorf("the command ran %d times", n)
			}
			assertUnchanged(t, root, before)
		})
	}

	t.Run("under --json, the refusal is the object's error", func(t *testing.T) {
		code, out, _ := runCLI(t, root, []string{"--json", "--try", "true", "--rounds", "2", "--each", patches[0]}, "")
		var r measureJSONReport
		must(t, json.Unmarshal([]byte(out), &r))
		if code != exitUsage || r.Exit != exitUsage || !strings.Contains(r.Error, "--rounds repeats a --measure run") {
			t.Errorf("exit %d: %s", code, out)
		}
	})

	// The name is refused, not the file: ./base is the same file, and
	// outside --measure base is a name like any other.
	t.Run("./base under --measure, and base without it", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		writePatch(t, dir, "base", "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n")
		log := pidFile(t, "log")
		code, out, errOut := runCLI(t, root, []string{"--try", measureLog(log), "--measure", "--each", "./base"}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		sameLines(t, "the runs", measureLines(t, log), []string{"base 1 alpha beta gamma", "./base 1 ALPHA beta gamma"})
		code, out, errOut = runCLI(t, root, []string{"--try", catchesAll, "--each", "base"}, "")
		if code != exitOK || !strings.Contains(out, "tried 1 patch and put back every file") {
			t.Errorf("exit %d: %s%s", code, out, errOut)
		}
	})
}

// TestMeasureStops: the stops are --each's, said of runs: a file not put
// back, the tree changed under the run, a command that cannot start, a
// signal. The summary counts the runs not reached and the "not tried:" line,
// which would repeat every name a round, is left out.
func TestMeasureStops(t *testing.T) {
	t.Run("a file not restored: exit 4", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		code, out, errOut := runCLI(t, root, []string{
			"--try", counted(mark, "grep -q alpha a.txt || { rm a.txt && mkdir a.txt; }"),
			"--measure", "--rounds", "2", "--each", patches[0], patches[1],
		}, "")
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s%s", code, out, errOut)
		}
		if n := markerLines(t, mark); n != 2 {
			t.Errorf("the command ran %d times, want 2", n)
		}
		want := "tried 1 of 6 runs; stopped at " + patches[0] + " in round 1: 1 file was not restored; 4 runs not tried\n"
		if !strings.HasSuffix(errOut, want) || strings.Contains(errOut, "not tried: ") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("the tree changed under the run: exit 2, named with its round", func(t *testing.T) {
		root, patches := eachFixture(t)
		// base writes a.txt in round 2, which nothing puts back: base has
		// no patch to put back. The patch then no longer applies.
		cmd := `[ "$HUNK_PATCH" = base ] && [ "$HUNK_ROUND" = 2 ] && echo changed > a.txt; true`
		code, out, errOut := runCLI(t, root, []string{"--try", cmd, "--measure", "--rounds", "2", "--each", patches[0]}, "")
		if code != exitNoMatch {
			t.Fatalf("exit %d, want 2: %s%s", code, out, errOut)
		}
		for _, want := range []string{
			"hunk: " + patches[0] + " in round 2 no longer applies; the preflight matched every patch",
			"tried 3 of 4 runs; stopped at " + patches[0] + " in round 2\n",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
		_, out, _ = runCLI(t, cliTree(t, map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n", "c.txt": "gamma\n"}),
			[]string{"--json", "--try", cmd, "--measure", "--rounds", "2", "--each", patches[0]}, "")
		var r measureJSONReport
		must(t, json.Unmarshal([]byte(out), &r))
		if !strings.HasPrefix(r.Error, patches[0]+" in round 2 no longer applies") {
			t.Errorf("error = %q", r.Error)
		}
	})

	t.Run("a command that cannot start: exit 5", func(t *testing.T) {
		root, patches := eachFixture(t)
		breakShellAtTheFirstCommit(t)
		code, out, errOut := runCLI(t, root, []string{"--try", "true", "--measure", "--rounds", "2", "--each", patches[0]}, "")
		if code != exitIO {
			t.Fatalf("exit %d, want 5: %s%s", code, out, errOut)
		}
		for _, want := range []string{
			"hunk: at " + patches[0] + " in round 1: could not run the --try command",
			"tried 1 of 4 runs; stopped at " + patches[0] + " in round 1; 2 runs not tried\n",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
	})

	t.Run("a signal during base: the run ends by it", func(t *testing.T) {
		needsSignals(t)
		root, patches := eachFixture(t)
		flag := pidFile(t, "go")
		go func() {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				if _, err := os.Stat(filepath.FromSlash(flag)); err == nil {
					signalSelf(t, syscall.SIGTERM)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		var out, errOut bytes.Buffer
		var sig os.Signal
		code := invoke([]string{
			"--root", root, "--try", fmt.Sprintf("echo go > '%s'; sleep 5", flag), "--measure", "--each", patches[0],
		}, strings.NewReader(""), &out, &errOut, &sig)
		if code != 143 || sig != syscall.SIGTERM {
			t.Fatalf("exit %d, signal %v: %s%s", code, sig, out.String(), errOut.String())
		}
		for _, want := range []string{
			"tried base",
			"interrupted by SIGTERM, the command was ended",
			"tried 0 of 2 runs; interrupted by SIGTERM during base; 1 run not tried\n",
		} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("stderr lacks %q: %q", want, errOut.String())
			}
		}
	})
}

// TestTryBase is base's turn alone, where its CLI tests cannot reach: a
// build or a command that cannot start, and a signal between the two.
func TestTryBase(t *testing.T) {
	tree := func(t *testing.T) *Tree {
		t.Helper()
		tr, err := OpenTree(t.TempDir(), false)
		must(t, err)
		t.Cleanup(func() { _ = tr.Close() })
		return tr
	}
	for _, c := range []struct {
		name, build, says string
	}{
		{"the build cannot start", "true", "could not run the --build command: "},
		{"the command cannot start", "", "could not run the --try command: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := tree(t)
			t.Setenv("PATH", brokenShell(t))
			ep := &eachPatch{name: measureBase, base: true, round: 1, rounds: 1}
			err := tryBase(eachConfig{command: "true", build: c.build, verifyLines: 40}, ep, tr, nil, Stop{})
			if err == nil || !strings.HasPrefix(err.Error(), c.says) || !strings.HasSuffix(err.Error(), "; no patch was applied") {
				t.Fatalf("err = %v", err)
			}
			if v := ep.outcome(); v == nil || v.Ran {
				t.Errorf("the run that could not start: %+v", v)
			}
		})
	}

	t.Run("a signal after the build: the command does not run", func(t *testing.T) {
		tr := tree(t)
		mark := pidFile(t, "ran")
		sigs := make(chan os.Signal, 1)
		sigs <- syscall.SIGTERM
		ep := &eachPatch{name: measureBase, base: true, round: 1, rounds: 1}
		// The build runs with no signals of its own to see, as one does
		// that exited before the signal came.
		cfg := eachConfig{command: counted(mark, "true"), build: "true", verifyLines: 40}
		if err := tryBase(cfg, ep, tr, sigs, Stop{}); err != nil {
			t.Fatal(err)
		}
		if ep.build == nil || !ep.build.OK || ep.v == nil || ep.v.Ran || ep.v.Signal != syscall.SIGTERM || markerLines(t, mark) != 0 {
			t.Errorf("build %+v, command %+v, ran %d", ep.build, ep.v, markerLines(t, mark))
		}
		var b bytes.Buffer
		writeEachLine(cfg, &b, 4, ep)
		if b.String() != "tried base  interrupted by SIGTERM before the command ran\n" {
			t.Errorf("line = %q", b.String())
		}
	})
}

// TestMeasureReportGoldens are --measure's reports, rendered from
// constructed runs as --each's are.
func TestMeasureReportGoldens(t *testing.T) {
	run := func(name string, round, rounds, status int, tail ...string) *eachPatch {
		ep := &eachPatch{
			name: name, base: name == measureBase, round: round, rounds: rounds,
			v: &Verify{
				Ran: true, Try: true, Command: "go test -run '^$' -bench Parse .", Status: status, OK: status == 0,
				Seconds: 3.1, Tail: tail, TotalLines: len(tail),
			},
		}
		if !ep.base {
			// Base has no patch, so nothing to put back.
			ep.v.Applied, ep.v.RolledBack = 1, 1
		}
		return ep
	}
	bench := func(ns string) []string {
		return []string{"BenchmarkParse-10   \t  412345\t      " + ns + " ns/op", "ok  \texample.com/parse\t3.012s"}
	}
	stream := func(cfg eachConfig, runs []*eachPatch, o eachOutcome) (string, int) {
		var out, errOut bytes.Buffer
		width := eachNameWidth(runs)
		for _, ep := range runs {
			if ep.outcome() != nil {
				writeEachLine(cfg, &errOut, width, ep)
			}
		}
		o.measure = true
		code := eachExit(runs, o)
		writeEachEnd(cfg, runs, o, code, &out, &errOut)
		return errOut.String() + out.String(), code
	}
	cfg := func(rounds int) eachConfig {
		return eachConfig{command: "go test -run '^$' -bench Parse .", measure: true, rounds: rounds}
	}

	t.Run("two rounds", func(t *testing.T) {
		runs := []*eachPatch{
			run("base", 1, 2, 0, bench("2911")...), run("v1.hunk", 1, 2, 0, bench("2604")...),
			run("v2.hunk", 1, 2, 1, "--- FAIL: TestParse (0.00s)", "FAIL"),
			run("base", 2, 2, 0, bench("2897")...), run("v1.hunk", 2, 2, 0, bench("2611")...),
			run("v2.hunk", 2, 2, 1, "--- FAIL: TestParse (0.00s)", "FAIL"),
		}
		got, code := stream(cfg(2), runs, eachOutcome{tried: 6, zeros: 4, stopAt: -1, seconds: 18.6})
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-measure", got)

		var out, errOut bytes.Buffer
		if code := eachJSON(cfg(2), runs, eachBaselines{}, exitOK, "", &out, &errOut); code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		golden(t, "report-each-measure-json", out.String())
	})

	t.Run("a measurement missing", func(t *testing.T) {
		c := cfg(1)
		c.build = "go vet ./..."
		timedOut := run("v1.hunk", 1, 1, exitTimedOut)
		timedOut.v.TimedOut, timedOut.v.Seconds = 90*time.Second, 90
		notBuilt := &eachPatch{name: "v2.hunk", round: 1, rounds: 1, build: &Verify{
			Ran: true, Command: c.build, Status: 1, Seconds: 1.2, RolledBack: 1,
			Tail: []string{"vet: ./parse.go:40:2: declared and not used: n"}, TotalLines: 1,
		}}
		got, code := stream(c, []*eachPatch{run("base", 1, 1, 0, bench("2911")...), timedOut, notBuilt},
			eachOutcome{tried: 3, zeros: 1, timeouts: 1, notBuilt: 1, stopAt: -1, seconds: 94.4})
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-measure-missing", got)
	})

	t.Run("stopped", func(t *testing.T) {
		left := run("v1.hunk", 2, 2, 0, bench("2604")...)
		left.v.NotRestored = []NotRestored{{Path: "parse.go", Reason: "it changed after hunk wrote it, and it is not hunk's to overwrite"}}
		runs := []*eachPatch{
			run("base", 1, 2, 0, bench("2911")...), run("v1.hunk", 1, 2, 0, bench("2604")...),
			run("base", 2, 2, 0, bench("2897")...), left,
		}
		got, code := stream(cfg(2), runs, eachOutcome{tried: 3, zeros: 3, stopAt: 3, seconds: 12.4})
		if code != exitRollbackFailed {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-measure-stopped", got)
	})

	t.Run("stopped at base: the command could not start", func(t *testing.T) {
		base := &eachPatch{name: "base", base: true, round: 1, rounds: 1, v: &Verify{Command: "bench"}}
		runs := []*eachPatch{base, {name: "v1.hunk", round: 1, rounds: 1}}
		got, code := stream(cfg(1), runs, eachOutcome{
			stopAt: 0, stopErr: fmt.Errorf("could not run the --try command: %w; no patch was applied", os.ErrNotExist),
		})
		if code != exitIO {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-measure-cannot-start", got)
	})
}
