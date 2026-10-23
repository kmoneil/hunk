package main

// --build's tests (§4.3, issue #86). The fixture is --each's: three files and
// a patch for each. The build is a shell command that fails only when a given
// patch is on disk, standing in for a compiler that refuses a mutant, and the
// markers say how often the build and the command ran: "its command was not
// run" and "nothing was tried" are counts, not sentences.

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

// failsOn is a build that refuses one patch, as a compiler refuses an unused
// parameter: it fails, with a compiler's line, when word is in file, appends
// to mark either way, and passes on the tree as it is.
func failsOn(mark, file, word string) string {
	return fmt.Sprintf("echo built >> '%s'; grep -q %s %s && { echo '%s:1:18: error: unused %s'; exit 1; }; exit 0",
		mark, word, file, file, word)
}

// eachLine is what a patch's line says after its padded name, or "" when no
// line names it: the padding is the longest name's, which is not the test's
// business.
func eachLine(errOut, name string) string {
	for _, l := range strings.Split(errOut, "\n") {
		if rest, ok := strings.CutPrefix(l, "tried "+name+" "); ok {
			return strings.TrimLeft(rest, " ")
		}
	}
	return ""
}

// TestEachBuild is the happy paths and the line the issue asked for.
func TestEachBuild(t *testing.T) {
	t.Run("every patch builds: the build runs once first and once a patch, exit 0", func(t *testing.T) {
		root, patches := eachFixture(t)
		built, ran := pidFile(t, "built"), pidFile(t, "ran")
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{
			"--try", counted(ran, catchesAll),
			"--build", fmt.Sprintf("echo built >> '%s'", built),
			"--each", patches[0], patches[1], patches[2],
		}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if n := markerLines(t, built); n != 5 {
			t.Errorf("the build ran %d times, want 5: on the tree as it is, once a patch, and on the tree again after the last (#110)", n)
		}
		if n := markerLines(t, ran); n != 5 {
			t.Errorf("the command ran %d times, want 5: on the tree as it is, once a patch, and on the tree again after the last (#110)", n)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		if !strings.Contains(out, "tried 3 patches and put back every file; every command exited nonzero") {
			t.Errorf("stdout = %q", out)
		}
		if strings.Contains(errOut, "$ echo built") {
			t.Errorf("a build that passed printed its output: %q", errOut)
		}
	})

	t.Run("a patch that does not build: its line, its compile error, its command not run, exit 3", func(t *testing.T) {
		root, patches := eachFixture(t)
		built, ran := pidFile(t, "built"), pidFile(t, "ran")
		build := failsOn(built, "b.txt", "BETA")
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{
			"--try", counted(ran, catchesAll),
			"--build", build,
			"--each", patches[0], patches[1], patches[2],
		}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s%s", code, out, errOut)
		}
		if n := markerLines(t, ran); n != 4 {
			t.Errorf("the command ran %d times, want 4: on the tree as it is, under the two that built, never for the patch that did not, and on the tree again after the last (#110)", n)
		}
		if n := markerLines(t, built); n != 5 {
			t.Errorf("the build ran %d times, want 5", n)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: the patch that did not build was not put back: %v", diff)
		}
		if out != "" {
			t.Errorf("stdout = %q, want empty: the report is a failure's", out)
		}
		if l := eachLine(errOut, patches[1]); !strings.HasPrefix(l, "did not build: the build exited 1 (") {
			t.Errorf("the line = %q: %q", l, errOut)
		}
		for _, want := range []string{
			"$ " + build,
			"b.txt:1:18: error: unused BETA",
			"tried 3 patches and put back every file; 2 exited nonzero, 1 did not build",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
		if n := strings.Count(errOut, "$ "); n != 1 {
			t.Errorf("%d transcripts, want 1, the failed build's alone", n)
		}
		if !strings.Contains(errOut, "tried "+patches[0]+"  the command exited 1") {
			t.Errorf("a patch that built lost its command's line: %q", errOut)
		}
	})

	t.Run("a build that times out did not build", func(t *testing.T) {
		root, patches := eachFixture(t)
		ran := pidFile(t, "ran")
		// Two seconds, not a hundred milliseconds: the baseline and the first
		// patch's build have to finish inside it, on Windows' sh too.
		code, out, errOut := runCLI(t, root, []string{
			"--try", counted(ran, catchesAll),
			"--build", "grep -q BETA b.txt && sleep 30; exit 0",
			"--timeout", "2s",
			"--each", patches[0], patches[1],
		}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s%s", code, out, errOut)
		}
		if n := markerLines(t, ran); n != 3 {
			t.Errorf("the command ran %d times, want 3: on the tree as it is, under the patch that built, and on the tree again after the last (#110)", n)
		}
		if l := eachLine(errOut, patches[1]); l != "did not build: the build timed out after 2s" {
			t.Errorf("the line = %q: %q", l, errOut)
		}
		for _, want := range []string{
			"tried 2 patches and put back every file; 1 exited nonzero, 1 did not build",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
		if strings.Contains(errOut, "1 timed out") {
			t.Errorf("a build's timeout was counted as the command's: %q", errOut)
		}
	})

	t.Run("the build sees the tree, then each patch alone, as the command does", func(t *testing.T) {
		root, patches := eachFixture(t)
		buildSaw, commandSaw := pidFile(t, "build-saw"), pidFile(t, "command-saw")
		code, out, errOut := runCLI(t, root, []string{
			"--try", fmt.Sprintf("cat a.txt b.txt >> '%s'; %s", commandSaw, catchesAll),
			"--build", fmt.Sprintf("cat a.txt b.txt >> '%s'", buildSaw),
			"--each", patches[0], patches[1],
		}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		for path, want := range map[string]string{
			// The tree as it is, then the first patch, then the second, the
			// first put back in between: the command as the build, since #90.
			// Then the tree as it is again, the second put back (#110).
			buildSaw:   "alpha\nbeta\nALPHA\nbeta\nalpha\nBETA\nalpha\nbeta\n",
			commandSaw: "alpha\nbeta\nALPHA\nbeta\nalpha\nBETA\nalpha\nbeta\n",
		} {
			b, err := os.ReadFile(filepath.FromSlash(path))
			must(t, err)
			if string(b) != want {
				t.Errorf("%s saw %q, want %q", filepath.Base(path), b, want)
			}
		}
	})

	t.Run("every line starts tried, the did-not-build one too", func(t *testing.T) {
		root, patches := eachFixture(t)
		_, _, errOut := runCLI(t, root, []string{
			"--try", catchesAll, "--build", "grep -q BETA b.txt && exit 1; exit 0",
			"--each", patches[0], patches[1],
		}, "")
		// The build printed nothing, so the transcript is the command and
		// the line count; every other line is the report's.
		for _, l := range strings.Split(strings.TrimRight(errOut, "\n"), "\n") {
			if l == "" || strings.HasPrefix(l, "$ ") || strings.HasPrefix(l, "(last ") {
				continue
			}
			if !strings.HasPrefix(l, "tried ") {
				t.Errorf("a report line does not start tried: %q", l)
			}
		}
	})
}

// TestEachBuildBaseline: a build that cannot pass on the tree as it is
// refuses the run before the first patch, nothing tried.
func TestEachBuildBaseline(t *testing.T) {
	cases := []struct {
		name  string
		build string
		extra []string
		say   string
		shown string // a line of the build's output the refusal shows
	}{
		{
			name:  "a build that fails: exit 1, its output shown",
			build: "echo \"error: no step named 'tst'\"; exit 2",
			say:   "hunk: the build exited 2 with no patch applied, so every patch would read as not built; nothing was tried",
			shown: "error: no step named 'tst'",
		},
		{
			name:  "a build that times out: exit 1",
			build: "echo compiling; sleep 30",
			extra: []string{"--timeout", "1s"},
			say:   "hunk: the build timed out after 1s with no patch applied, so every patch would read as not built; nothing was tried",
			shown: "compiling",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, patches := eachFixture(t)
			ran := pidFile(t, "ran")
			before := snapshot(t, root)
			args := append([]string{"--try", fmt.Sprintf("echo ran >> '%s'", ran), "--build", c.build}, c.extra...)
			args = append(args, "--each", patches[0], patches[1])
			code, out, errOut := runCLI(t, root, args, "")
			if code != exitUsage {
				t.Fatalf("exit %d, want 1: %s%s", code, out, errOut)
			}
			if n := markerLines(t, ran); n != 0 {
				t.Errorf("the command ran %d times after the baseline failed", n)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
			for _, want := range []string{c.say, "$ " + c.build, c.shown} {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr lacks %q: %q", want, errOut)
				}
			}
			if strings.Contains(errOut, "tried ") || out != "" {
				t.Errorf("a refused run reported a try: stdout %q, stderr %q", out, errOut)
			}
		})
	}

	t.Run("the preflight comes first: a typo'd patch is reported, the build never runs", func(t *testing.T) {
		root, patches := eachFixture(t)
		built := pidFile(t, "built")
		typo := writePatch(t, t.TempDir(), "typo.hunk", "@@ file a.txt\n@@ old\nalpha\n")
		code, _, errOut := runCLI(t, root, []string{
			"--try", catchesAll, "--build", fmt.Sprintf("echo built >> '%s'", built),
			"--each", patches[0], typo,
		}, "")
		if code != exitUsage || !strings.Contains(errOut, "1 of 2 patches could not be read") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if n := markerLines(t, built); n != 0 {
			t.Errorf("the build ran %d times before a patch the preflight refused", n)
		}
	})

	t.Run("a build that cannot start: exit 5, nothing tried", func(t *testing.T) {
		root, patches := eachFixture(t)
		t.Setenv("PATH", brokenShell(t))
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", "true", "--build", "true", "--each", patches[0]}, "")
		if code != exitIO {
			t.Fatalf("exit %d, want 5: %s%s", code, out, errOut)
		}
		if !strings.Contains(errOut, "hunk: could not run the --build command: ") ||
			!strings.HasSuffix(errOut, "; nothing was tried\n") {
			t.Errorf("stderr = %q", errOut)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
	})

	t.Run("json", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{
			"--json", "--try", "true", "--build", "echo nope; exit 2", "--each", patches[0], patches[1],
		}, "")
		if code != exitUsage {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachBuildJSONOf(t, out)
		if r.OK || r.Exit != exitUsage || !strings.Contains(r.Error, "the build exited 2 with no patch applied") {
			t.Errorf("ok %v exit %d error %q", r.OK, r.Exit, r.Error)
		}
		b := r.Each.Baseline
		if b == nil || !b.Ran || b.OK || b.Status != 2 || len(b.Output) != 1 || b.Output[0] != "nope" {
			t.Errorf("baseline = %+v", b)
		}
		if r.Each.Build != "echo nope; exit 2" {
			t.Errorf("build = %q", r.Each.Build)
		}
		for _, p := range r.Each.Patches {
			if p.Ran || p.Build != nil {
				t.Errorf("a patch was tried: %+v", p)
			}
		}
	})

	t.Run("json, a build that cannot start", func(t *testing.T) {
		root, patches := eachFixture(t)
		t.Setenv("PATH", brokenShell(t))
		code, out, _ := runCLI(t, root, []string{"--json", "--try", "true", "--build", "true", "--each", patches[0]}, "")
		if code != exitIO {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachBuildJSONOf(t, out)
		if !strings.Contains(r.Error, "could not run the --build command") || r.Each.Baseline != nil {
			t.Errorf("error %q baseline %+v", r.Error, r.Each.Baseline)
		}
	})
}

// TestEachBuildRefusals: --build anywhere but beside --try --each is exit 1
// before anything runs.
func TestEachBuildRefusals(t *testing.T) {
	root, patches := eachFixture(t)
	const say = "--build runs before each patch's command under --try --each, and does nothing without --each"
	cases := []struct {
		name string
		args []string
		say  string
	}{
		{"with --try alone", []string{"--try", "true", "--build", "true", "-f", patches[0]}, say},
		{"with --verify", []string{"--verify", "true", "--build", "true", "-f", patches[0]}, say},
		{"on its own", []string{"--build", "true", "-f", patches[0]}, say},
		{"with --each but no --try: --each's refusal", []string{"--build", "true", "--each", patches[0]}, "--each tries each patch alone and does nothing without --try CMD"},
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

// TestEachBuildStops: the stop rules hold for the build as for the command.
func TestEachBuildStops(t *testing.T) {
	t.Run("a failed build that rewrote a patched file: exit 4, the rest not tried", func(t *testing.T) {
		root, patches := eachFixture(t)
		ran := pidFile(t, "ran")
		code, out, errOut := runCLI(t, root, []string{
			"--try", fmt.Sprintf("echo ran >> '%s'", ran),
			"--build", "grep -q ALPHA a.txt && { printf 'generated\\n' > a.txt; exit 1; }; exit 0",
			"--each", patches[0], patches[1], patches[2],
		}, "")
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s%s", code, out, errOut)
		}
		if n := markerLines(t, ran); n != 1 {
			t.Errorf("the command ran %d times, want 1: on the tree as it is alone", n)
		}
		for _, want := range []string{
			"tried " + patches[0] + "  did not build: the build exited 1 (",
			"; 1 file left alone",
			"a.txt was not restored.",
			"not tried: " + patches[1] + ", " + patches[2],
			"tried 0 of 3 patches; stopped at " + patches[0] + ": 1 file was not restored; 2 patches not tried",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
		b, err := os.ReadFile(filepath.Join(root, "a.txt"))
		must(t, err)
		if string(b) != "generated\n" {
			t.Errorf("a.txt = %q, want the build's bytes left alone", b)
		}
	})

	t.Run("a build that cannot start mid-run: exit 5, put back, the rest not tried", func(t *testing.T) {
		// The shell breaks between the baseline, which needs it, and the
		// first patch's build: the handler's second installation, at the
		// first patch's commit, is the seam.
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
		root, patches := eachFixture(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", "true", "--build", "true", "--each", patches[0], patches[1]}, "")
		if code != exitIO {
			t.Fatalf("exit %d, want 5: %s%s", code, out, errOut)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		for _, want := range []string{
			"tried " + patches[0] + "  the build could not start",
			"hunk: at " + patches[0] + ": could not run the --build command",
			"; the batch was put back",
			"not tried: " + patches[1],
			"tried 0 of 2 patches; stopped at " + patches[0] + "; 1 patch not tried",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
	})
}

// TestEachBuildInterrupted: a signal during the build, or between it and the
// command, ends the run as one during the command does.
func TestEachBuildInterrupted(t *testing.T) {
	needsSignals(t)
	// signalWhen sends SIGTERM to this process once path exists, after delay.
	signalWhen := func(t *testing.T, path string, delay time.Duration) {
		go func() {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				if _, err := os.Stat(filepath.FromSlash(path)); err == nil {
					time.Sleep(delay)
					signalSelf(t, syscall.SIGTERM)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	// run is one interrupted run: exit 143, SIGTERM for main to re-raise,
	// and the stderr to read.
	run := func(t *testing.T, root string, args []string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		var sig os.Signal
		code := invoke(append([]string{"--root", root}, args...), strings.NewReader(""), &out, &errOut, &sig)
		if code != 143 {
			t.Fatalf("exit %d, want 143: %s%s", code, out.String(), errOut.String())
		}
		if sig != syscall.SIGTERM {
			t.Errorf("main would not re-raise SIGTERM: %v", sig)
		}
		return errOut.String()
	}

	t.Run("during a patch's build", func(t *testing.T) {
		root, patches := eachFixture(t)
		started, ran := pidFile(t, "started"), pidFile(t, "ran")
		signalWhen(t, started, 0)
		before := snapshot(t, root)
		errOut := run(t, root, []string{
			"--try", fmt.Sprintf("echo ran >> '%s'", ran),
			"--build", fmt.Sprintf("grep -q ALPHA a.txt || exit 0; echo go > '%s'; sleep 30", started),
			"--each", patches[0], patches[1],
		})
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		if n := markerLines(t, ran); n != 1 {
			t.Errorf("the command ran %d times, want 1: on the tree as it is alone", n)
		}
		for _, want := range []string{
			"tried " + patches[0] + "  interrupted by SIGTERM, the build was ended",
			"not tried: " + patches[1],
			"tried 0 of 2 patches; interrupted by SIGTERM during " + patches[0] + "; 1 patch not tried",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
	})

	t.Run("during the build on the tree as it is", func(t *testing.T) {
		root, patches := eachFixture(t)
		started := pidFile(t, "started")
		signalWhen(t, started, 0)
		before := snapshot(t, root)
		errOut := run(t, root, []string{
			"--try", catchesAll, "--build", fmt.Sprintf("echo go > '%s'; sleep 30", started),
			"--each", patches[0], patches[1],
		})
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		for _, want := range []string{
			"not tried: " + patches[0] + ", " + patches[1],
			"tried 0 of 2 patches; interrupted by SIGTERM; 2 patches not tried",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q: %q", want, errOut)
			}
		}
	})

	t.Run("after the build exited 0, before the command", func(t *testing.T) {
		// The build exits while something it started holds its output, so
		// hunk is still reading when the signal lands, and the build's own
		// end has already been taken.
		root, patches := eachFixture(t)
		flag, ran := pidFile(t, "go"), pidFile(t, "ran")
		signalWhen(t, flag, 500*time.Millisecond)
		before := snapshot(t, root)
		errOut := run(t, root, []string{
			"--try", fmt.Sprintf("echo ran >> '%s'", ran),
			"--build", fmt.Sprintf("grep -q ALPHA a.txt || exit 0; echo go > '%s'; sleep 3 & exit 0", flag),
			"--each", patches[0], patches[1],
		})
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
		if n := markerLines(t, ran); n != 1 {
			t.Errorf("the command ran %d times, want 1: on the tree as it is, and not after the signal", n)
		}
		if !strings.Contains(errOut, "tried "+patches[0]+"  interrupted by SIGTERM before the command ran") {
			t.Errorf("stderr = %q", errOut)
		}
	})
}

// TestEachBuildQuietAndDryRun: --quiet keeps a patch that did not build, and
// --dry-run runs neither the build nor the command.
func TestEachBuildQuietAndDryRun(t *testing.T) {
	t.Run("quiet keeps the patch that did not build", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{
			"--quiet", "--try", catchesAll, "--build", "grep -q BETA b.txt && { echo 'error: unused'; exit 1; }; exit 0",
			"--each", patches[0], patches[1],
		}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if strings.Contains(errOut, "tried "+patches[0]) {
			t.Errorf("quiet printed the caught patch's line: %q", errOut)
		}
		if l := eachLine(errOut, patches[1]); !strings.HasPrefix(l, "did not build") {
			t.Errorf("quiet dropped the patch that did not build: %q", errOut)
		}
		for _, want := range []string{"error: unused", "1 did not build"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("quiet dropped %q: %q", want, errOut)
			}
		}
	})

	t.Run("quiet and clean prints nothing", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--quiet", "--try", catchesAll, "--build", "true", "--each", patches[0]}, "")
		if code != exitOK || out != "" || errOut != "" {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})

	t.Run("dry run runs neither, and says so", func(t *testing.T) {
		root, patches := eachFixture(t)
		built, ran := pidFile(t, "built"), pidFile(t, "ran")
		code, out, errOut := runCLI(t, root, []string{
			"--dry-run", "--try", fmt.Sprintf("echo ran >> '%s'", ran),
			"--build", fmt.Sprintf("echo built >> '%s'; exit 1", built),
			"--each", patches[0],
		}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if markerLines(t, built) != 0 || markerLines(t, ran) != 0 {
			t.Errorf("a dry run ran something: build %d, command %d", markerLines(t, built), markerLines(t, ran))
		}
		if !strings.Contains(out, "1 patch would apply, 1 hunk, +1 -1 (dry run: nothing written, build and command not run)") {
			t.Errorf("stdout = %q", out)
		}
	})
}

// eachBuildJSONReport is what --json printed under --build, the fields the
// tests read.
type eachBuildJSONReport struct {
	OK    bool   `json:"ok"`
	Exit  int    `json:"exit"`
	Error string `json:"error"`
	Each  struct {
		Command  string         `json:"command"`
		Build    string         `json:"build"`
		Baseline *jsonBuildSeen `json:"baseline"`
		Patches  []struct {
			Patch       string         `json:"patch"`
			Ran         bool           `json:"ran"`
			Status      *int           `json:"status"`
			Build       *jsonBuildSeen `json:"build"`
			RolledBack  int            `json:"rolled_back"`
			Output      []string       `json:"output"`
			NotRestored []struct {
				Path string `json:"path"`
			} `json:"not_restored"`
		} `json:"patches"`
	} `json:"each"`
}

type jsonBuildSeen struct {
	Ran      bool     `json:"ran"`
	OK       bool     `json:"ok"`
	Status   int      `json:"status"`
	Seconds  float64  `json:"seconds"`
	Output   []string `json:"output"`
	TimedOut float64  `json:"timed_out_after"`
}

func eachBuildJSONOf(t *testing.T, out string) eachBuildJSONReport {
	t.Helper()
	var r eachBuildJSONReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	return r
}

// TestEachBuildJSON: each patch carries its build, and a patch that did not
// build has ran false, its build's output, and the build's own putting back.
func TestEachBuildJSON(t *testing.T) {
	t.Run("a run", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{
			"--json", "--try", catchesAll, "--build", "grep -q BETA b.txt && { echo 'b.txt:1: error: unused'; exit 1; }; exit 0",
			"--each", patches[0], patches[1],
		}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachBuildJSONOf(t, out)
		if r.OK || r.Exit != exitVerifyFailed || r.Each.Baseline == nil || !r.Each.Baseline.OK {
			t.Fatalf("ok %v exit %d baseline %+v", r.OK, r.Exit, r.Each.Baseline)
		}
		p0, p1 := r.Each.Patches[0], r.Each.Patches[1]
		if !p0.Ran || p0.Status == nil || *p0.Status != 1 || p0.Build == nil || !p0.Build.OK || len(p0.Build.Output) != 0 {
			t.Errorf("built patch = %+v build %+v", p0, p0.Build)
		}
		if p1.Ran || p1.Status != nil || p1.RolledBack != 1 || p1.Build == nil || p1.Build.OK || p1.Build.Status != 1 ||
			len(p1.Build.Output) != 1 || p1.Build.Output[0] != "b.txt:1: error: unused" {
			t.Errorf("unbuilt patch = %+v build %+v", p1, p1.Build)
		}
	})

	t.Run("a build that timed out is 124 with its timeout", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{
			"--json", "--try", catchesAll, "--build", "grep -q ALPHA a.txt && sleep 30; exit 0", "--timeout", "2s",
			"--each", patches[0],
		}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, out)
		}
		b := eachBuildJSONOf(t, out).Each.Patches[0].Build
		if b == nil || b.OK || b.Status != exitTimedOut || b.TimedOut == 0 {
			t.Errorf("build = %+v", b)
		}
	})

	t.Run("without --build there is no build in it", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{"--json", "--try", catchesAll, "--each", patches[0]}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		if strings.Contains(out, `"build"`) || strings.Contains(out, `"baseline"`) {
			t.Errorf("a run without --build mentions one: %s", out)
		}
	})
}

// TestEachBuildReportGoldens renders the new shapes from constructed
// outcomes with fixed seconds, as TestEachReportGoldens does.
func TestEachBuildReportGoldens(t *testing.T) {
	cfg := eachConfig{command: "zig build test", build: "zig build"}
	built := func() *Verify {
		return &Verify{Ran: true, OK: true, Command: "zig build", Seconds: 1.4, Applied: 1}
	}
	tried := func(status int, edit ...func(*Verify)) *Verify {
		v := &Verify{
			Ran: true, Try: true, Command: "zig build test", Status: status, OK: status == 0,
			Seconds: 41.2, Applied: 1, RolledBack: 1,
		}
		for _, e := range edit {
			e(v)
		}
		return v
	}
	unbuilt := &Verify{
		Ran: true, Command: "zig build", Status: 1, Seconds: 1.6, Applied: 1, RolledBack: 1, TotalLines: 3,
		Tail: []string{
			"src/faults.zig:88:17: error: unused function parameter",
			"fn reach(self: *Faults, depth: u32) u32 {",
			"                        ^~~~~",
		},
	}
	hung := &Verify{
		Ran: true, Command: "zig build", Status: exitTimedOut, Seconds: 170.0, Applied: 1, RolledBack: 1,
		TimedOut: 170 * time.Second, TotalLines: 1, Tail: []string{"[3/9] compile src/cache.zig"},
	}
	patches := func() []*eachPatch {
		return []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", build: built(), v: tried(1)},
			{name: "breaks/04-depth-unused.hunk", build: unbuilt},
			{name: "breaks/06-texture-not-hashed.hunk", build: built(), v: tried(0, func(v *Verify) {
				v.Tail, v.TotalLines = []string{"All 212 tests passed."}, 1
			})},
			{name: "breaks/11-cache-loops.hunk", build: hung},
		}
	}

	t.Run("not built", func(t *testing.T) {
		var out, errOut bytes.Buffer
		ps := patches()
		width := eachNameWidth(ps)
		for _, ep := range ps {
			writeEachLine(cfg, &errOut, width, ep)
		}
		o := eachOutcome{tried: 4, zeros: 1, notBuilt: 2, stopAt: -1, seconds: 256.0}
		code := eachExit(ps, o)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		writeEachEnd(cfg, ps, o, code, &out, &errOut)
		golden(t, "report-each-not-built", errOut.String()+out.String())
	})

	t.Run("baseline", func(t *testing.T) {
		var out, errOut bytes.Buffer
		b := &Verify{
			Ran: true, Command: "zig build", Status: 2, Seconds: 0.4, TotalLines: 2,
			Tail: []string{"error: no step named 'biuld'", "  access the help menu with 'zig build -h'"},
		}
		code := eachBaselineRefused(cfg, patches(), eachBaselines{build: b}, b, baselineFailure("the build", b, "not built"), exitUsage, &out, &errOut)
		if code != exitUsage {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-baseline", errOut.String()+out.String())
	})

	t.Run("json", func(t *testing.T) {
		var out, errOut bytes.Buffer
		cfg := cfg
		cfg.asJSON = true
		tested := &Verify{Ran: true, OK: true, Try: true, Command: "zig build test", Seconds: 40.8, TotalLines: 1, Tail: []string{"All 212 tests passed."}}
		code := eachJSON(cfg, patches(), eachBaselines{build: built(), command: tested}, exitVerifyFailed, "", &out, &errOut)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		golden(t, "report-each-build-json", out.String())
	})
}

// TestEachVerdictCountsEveryKind is the summary's middle as a table, the
// one place the four counts meet.
func TestEachVerdictCountsEveryKind(t *testing.T) {
	cases := []struct {
		tried, zeros, timeouts, notBuilt int
		want                             string
	}{
		{3, 0, 0, 0, "every command exited nonzero"},
		{3, 0, 0, 1, "2 exited nonzero, 1 did not build"},
		{2, 0, 0, 2, "2 did not build"},
		{4, 1, 1, 1, "1 exited nonzero, 1 exited 0, 1 timed out, 1 did not build"},
		{1, 1, 0, 0, "1 exited 0"},
	}
	for _, c := range cases {
		if got := eachVerdict(c.tried, c.zeros, c.timeouts, c.notBuilt); got != c.want {
			t.Errorf("eachVerdict(%d, %d, %d, %d) = %q, want %q", c.tried, c.zeros, c.timeouts, c.notBuilt, got, c.want)
		}
	}
}
