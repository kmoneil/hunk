package main

// The command's run on the tree as it is (§4.3, issue #90). A command that
// fails with no patch applied fails under every patch, and --each would score
// every patch caught at exit 0, the strongest result it has, from a run that
// tested nothing. The rows are the shapes the field produced: an assertion
// that is false before any patch (the issue's repro), a filter that matches no
// test (pytest's 5, and unittest's since Python 3.12), an exit taken from
// zsh's pipestatus under sh, a cd from the wrong directory, and a suite that
// hangs. The marker says the command ran exactly once, which is what "nothing
// was tried" means now.

import (
	"bytes"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestEachRefusesACommandThatFailsWithNoPatch: exit 1, the command run once,
// its output shown, no patch tried, the tree as it was.
func TestEachRefusesACommandThatFailsWithNoPatch(t *testing.T) {
	const caught = "so every patch would read as caught; nothing was tried\n"
	cases := []struct {
		name    string
		command string
		extra   []string
		say     string // the refusal line's start, after "hunk: "
		end     string // the refusal line's end
		shown   string // a line of the command's output the refusal shows, or ""
	}{
		{
			name:    "an assertion false on the tree as it is: the issue's repro",
			command: "grep -q ALPHA a.txt || { echo AssertionError; exit 1; }",
			say:     "the command exited 1 with no patch applied, ",
			end:     caught,
			shown:   "AssertionError",
		},
		{
			name:    "a filter that matches no test: pytest's 5",
			command: "echo 'no tests ran in 0.01s'; exit 5",
			say:     "the command exited 5 with no patch applied, ",
			end:     caught,
			shown:   "no tests ran in 0.01s",
		},
		{
			// 1 where sh is bash, which expands the unset array to nothing
			// and exits with grep's status; 2 where it is dash, which refuses
			// the substitution. Either was a catch for every patch.
			name:    "an exit taken from zsh's pipestatus, which sh does not have",
			command: `echo ok | grep -m2 "error:\|failed"; exit ${pipestatus[1]}`,
			say:     "the command exited ",
			end:     caught,
		},
		{
			name:    "a cd from the wrong directory",
			command: "cd scripts/corpus && grep -q alpha a.txt",
			say:     "the command exited ",
			end:     caught,
		},
		{
			name:    "a suite that hangs with no patch applied",
			command: "echo collecting; sleep 30",
			extra:   []string{"--timeout", "1s"},
			say:     "the command timed out after 1s with no patch applied, ",
			end:     "so every patch would read as timed out; nothing was tried\n",
			shown:   "collecting",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, patches := eachFixture(t)
			mark := pidFile(t, "ran")
			command := counted(mark, c.command)
			before := snapshot(t, root)
			args := append([]string{"--try", command}, c.extra...)
			code, out, errOut := runCLI(t, root, append(args, "--each", patches[0], patches[1]), "")
			if code != exitUsage {
				t.Fatalf("exit %d, want 1: %s%s", code, out, errOut)
			}
			if n := markerLines(t, mark); n != 1 {
				t.Errorf("the command ran %d times, want 1: on the tree as it is, and under no patch", n)
			}
			if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
				t.Errorf("tree changed: %v", diff)
			}
			first, _, _ := strings.Cut(errOut, "\n")
			if !strings.HasPrefix(first, "hunk: "+c.say) || !strings.HasSuffix(first+"\n", c.end) {
				t.Errorf("the refusal = %q, want %q ... %q", first, "hunk: "+c.say, c.end)
			}
			if !strings.Contains(errOut, "\n$ "+command+"\n") {
				t.Errorf("the command is not shown: %q", errOut)
			}
			if c.shown != "" && !strings.Contains(errOut, "\n"+c.shown+"\n") {
				t.Errorf("its output %q is not shown: %q", c.shown, errOut)
			}
			if strings.Contains(errOut, "tried ") || out != "" {
				t.Errorf("a refused run reported a try: stdout %q, stderr %q", out, errOut)
			}
		})
	}

	t.Run("--quiet still prints the refusal: it is a failure", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--quiet", "--try", "exit 5", "--each", patches[0]}, "")
		if code != exitUsage || out != "" || !strings.HasPrefix(errOut, "hunk: the command exited 5 with no patch applied") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
}

// TestEachCommandBaselineOrder: the build's run on the tree as it is comes
// first, then the command's, then each patch's build and command; and a
// command that fails there under --build is the command's refusal, not the
// build's.
func TestEachCommandBaselineOrder(t *testing.T) {
	t.Run("build, command, then each patch", func(t *testing.T) {
		root, patches := eachFixture(t)
		log := pidFile(t, "log")
		code, out, errOut := runCLI(t, root, []string{
			"--try", "echo command >> '" + log + "'; " + catchesAll,
			"--build", "echo build >> '" + log + "'",
			"--each", patches[0],
		}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		b, err := os.ReadFile(log)
		must(t, err)
		if string(b) != "build\ncommand\nbuild\ncommand\n" {
			t.Errorf("the runs went %q, want the build and the command on the tree as it is, then under the patch", b)
		}
	})

	t.Run("a command that fails there under --build: the command's refusal, the build run once", func(t *testing.T) {
		root, patches := eachFixture(t)
		built := pidFile(t, "built")
		code, _, errOut := runCLI(t, root, []string{
			"--try", "exit 5", "--build", "echo built >> '" + built + "'",
			"--each", patches[0], patches[1],
		}, "")
		if code != exitUsage {
			t.Fatalf("exit %d, want 1: %s", code, errOut)
		}
		if !strings.HasPrefix(errOut, "hunk: the command exited 5 with no patch applied, so every patch would read as caught") {
			t.Errorf("stderr = %q", errOut)
		}
		if n := markerLines(t, built); n != 1 {
			t.Errorf("the build ran %d times, want 1: on the tree as it is alone", n)
		}
	})
}

// TestEachCommandBaselineCannotStart: a shell that cannot run the command on
// the tree as it is stops the run at 5 before the first patch, as the build's
// does.
func TestEachCommandBaselineCannotStart(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		root, patches := eachFixture(t)
		t.Setenv("PATH", brokenShell(t))
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", "true", "--each", patches[0], patches[1]}, "")
		if code != exitIO {
			t.Fatalf("exit %d, want 5: %s%s", code, out, errOut)
		}
		if !strings.HasPrefix(errOut, "hunk: could not run the --try command: ") ||
			!strings.HasSuffix(errOut, "; nothing was tried\n") || strings.Contains(errOut, "tried ") {
			t.Errorf("stderr = %q", errOut)
		}
		if diff := snapshotDiff(before, snapshot(t, root)); len(diff) > 0 {
			t.Errorf("tree changed: %v", diff)
		}
	})

	t.Run("json", func(t *testing.T) {
		root, patches := eachFixture(t)
		t.Setenv("PATH", brokenShell(t))
		code, out, _ := runCLI(t, root, []string{"--json", "--try", "true", "--each", patches[0]}, "")
		if code != exitIO {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachJSONOf(t, out)
		if r.Exit != exitIO || !strings.Contains(r.Error, "could not run the --try command") || r.Each.TryBaseline != nil {
			t.Errorf("exit %d error %q try_baseline %+v", r.Exit, r.Error, r.Each.TryBaseline)
		}
	})
}

// TestEachInterruptedDuringTheCommandBaseline: a signal while the command
// runs on the tree as it is ends it and the run, nothing tried, and hunk dies
// of the signal. The handler has to be in before that run, with or without
// --build: the command runs in its own process group, and would outlive a
// hunk the signal killed.
func TestEachInterruptedDuringTheCommandBaseline(t *testing.T) {
	needsSignals(t)
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
		"--root", root, "--try", "grep -q ALPHA a.txt && exit 1; echo go > '" + started + "'; sleep 30",
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
		"not tried: " + patches[0] + ", " + patches[1],
		"tried 0 of 2 patches; interrupted by SIGTERM; 2 patches not tried",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q: %q", want, errOut.String())
		}
	}
	if strings.Contains(errOut.String(), "tried "+patches[0]) {
		t.Errorf("a patch was tried after the signal: %q", errOut.String())
	}
}

// TestEachCommandBaselineJSON: the command's run on the tree as it is is
// try_baseline, with its output whatever the end; a refused run says why at
// the top and tries nothing.
func TestEachCommandBaselineJSON(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{
			"--json", "--try", "echo 'no tests ran'; exit 5", "--each", patches[0], patches[1],
		}, "")
		if code != exitUsage {
			t.Fatalf("exit %d: %s", code, out)
		}
		if errOut != "" {
			t.Errorf("a refused --json run wrote to stderr: %q", errOut)
		}
		r := eachJSONOf(t, out)
		if r.OK || r.Exit != exitUsage ||
			r.Error != "the command exited 5 with no patch applied, so every patch would read as caught; nothing was tried" {
			t.Errorf("ok %v exit %d error %q", r.OK, r.Exit, r.Error)
		}
		b := r.Each.TryBaseline
		if b == nil || !b.Ran || b.OK || b.Status != 5 || len(b.Output) != 1 || b.Output[0] != "no tests ran" {
			t.Errorf("try_baseline = %+v", b)
		}
		for _, p := range r.Each.Patches {
			if p.Ran || p.Status != nil {
				t.Errorf("a patch was tried: %+v", p)
			}
		}
		if strings.Contains(out, `"baseline"`) {
			t.Errorf("a run without --build has the build's baseline: %s", out)
		}
	})

	t.Run("timed out: 124 and its timeout", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{
			"--json", "--try", "sleep 30", "--timeout", "1s", "--each", patches[0],
		}, "")
		if code != exitUsage {
			t.Fatalf("exit %d: %s", code, out)
		}
		b := eachJSONOf(t, out).Each.TryBaseline
		if b == nil || b.OK || b.Status != exitTimedOut || b.TimedOut != 1 {
			t.Errorf("try_baseline = %+v", b)
		}
	})

	t.Run("with --build, both runs on the tree as it is", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, _ := runCLI(t, root, []string{
			"--json", "--try", catchesAll, "--build", "true", "--each", patches[0],
		}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		r := eachBuildJSONOf(t, out)
		if r.Each.Baseline == nil || !r.Each.Baseline.OK {
			t.Errorf("baseline = %+v", r.Each.Baseline)
		}
		if b := eachJSONOf(t, out).Each.TryBaseline; b == nil || !b.Ran || !b.OK || b.Status != 0 {
			t.Errorf("try_baseline = %+v", b)
		}
	})
}
