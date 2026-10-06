package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A --verify or --try command that does not end, or a hunk that is signalled
// while one runs. Until 2026-10-06 either left the batch applied: a signal
// killed hunk before any rollback could run, --try's edit included, and the
// command ran on, orphaned; nothing bounded a hung command at all; and a
// background child holding the output pipe held hunk until it died.
//
// Every row here runs real commands through sh and, where it says so, sends
// real signals. Each asserts the tree afterwards and, where sh's process IDs
// are real (pidsFromShAreReal), which processes are left.

// withDelays shortens verifyGrace and verifyWaitDelay for one test, so a row
// that has to wait for one does not wait five seconds.
func withDelays(t *testing.T, grace, wait time.Duration) {
	t.Helper()
	g, w := verifyGrace, verifyWaitDelay
	verifyGrace, verifyWaitDelay = grace, wait
	t.Cleanup(func() { verifyGrace, verifyWaitDelay = g, w })
}

// pidFile is a path outside the tree for a command to write a process ID to,
// spelled for sh, so that the tree's own snapshot stays the tree.
func pidFile(t *testing.T, name string) string {
	t.Helper()
	return filepath.ToSlash(filepath.Join(t.TempDir(), name))
}

// pidFrom waits for a command to write a process ID to path, which is how a
// test knows the command is running, and so that the handler is in.
func pidFrom(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(filepath.FromSlash(path))
		if err == nil && strings.HasSuffix(string(b), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			must(t, err)
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared: the command did not start", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func killPID(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// assertEnded fails if pid is still running two seconds on: a process nobody
// has reaped yet still answers signal 0, so it is given a moment.
func assertEnded(t *testing.T, pid int, what string) {
	t.Helper()
	if !pidsFromShAreReal() {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			killPID(pid)
			t.Errorf("%s (pid %d) is still running; it should have been ended", what, pid)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertLeftRunning fails if pid is gone, and then ends it: a command that
// exits normally may have started it on purpose.
func assertLeftRunning(t *testing.T, pid int, what string) {
	t.Helper()
	if !pidsFromShAreReal() {
		return
	}
	if !alive(pid) {
		t.Errorf("%s (pid %d) was ended; a command that exits normally keeps what it started", what, pid)
	}
	killPID(pid)
}

func TestTimeoutFlag(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		code int
		say  string
	}{
		{"seconds", []string{"--verify", "true", "--timeout", "90s"}, exitOK, ""},
		{"minutes", []string{"--verify", "true", "--timeout", "5m"}, exitOK, ""},
		{"a compound", []string{"--try", "true", "--timeout", "1m30s"}, exitOK, ""},
		{"milliseconds", []string{"--verify", "true", "--timeout", "800ms"}, exitOK, ""},
		{"with --dry-run, which runs nothing", []string{"--dry-run", "--verify", "true", "--timeout", "1s"}, exitOK, ""},
		{
			"no unit",
			[]string{"--verify", "true", "--timeout", "90"},
			exitUsage,
			`--timeout must be a duration with a unit, such as 90s or 5m, not "90"`,
		},
		{
			"not a duration",
			[]string{"--verify", "true", "--timeout", "soon"},
			exitUsage,
			`--timeout must be a duration with a unit, such as 90s or 5m, not "soon"`,
		},
		{"zero", []string{"--verify", "true", "--timeout", "0s"}, exitUsage, `--timeout must be more than zero, not "0s"`},
		{"negative", []string{"--try", "true", "--timeout", "-5s"}, exitUsage, `--timeout must be more than zero, not "-5s"`},
		{"without a command", []string{"--timeout", "5s"}, exitUsage, "--timeout does nothing without --verify or --try"},
		{"no value", []string{"--verify", "true", "--timeout"}, exitUsage, "flag needs an argument: -timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, c.args, vPatch)
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s", code, c.code, errOut)
			}
			if c.code == exitOK {
				return
			}
			if !strings.Contains(errOut, c.say) {
				t.Errorf("stderr = %q, want it to say %q", errOut, c.say)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// --timeout ends the command and everything it started, and the batch goes
// back: exit 3 under --verify, 124 under --try as timeout(1) exits, and 3 with
// the batch kept under --keep-on-fail, as for any failed verify.
func TestATimeoutEndsTheCommand(t *testing.T) {
	withDelays(t, 2*time.Second, 300*time.Millisecond)
	for _, c := range []struct {
		name   string
		flags  []string
		code   int
		say    string
		file   string // a.txt afterwards
		posix  bool   // rests on signal dispositions; see needsSignals
		cmd    func(pid, bg string) string
		bgGone bool // the background process must be ended too
	}{
		{
			name: "--verify is exit 3, rolled back", flags: []string{"--verify"}, code: exitVerifyFailed,
			say: "hunk: applied 1 hunk, verify timed out after 300ms, rolled back 1 file\n", file: "one\n",
			cmd: func(pid, _ string) string { return fmt.Sprintf("echo $$ > '%s'; exec "+longSleep(), pid) },
		},
		{
			name: "--try is 124, put back", flags: []string{"--try"}, code: exitTimedOut,
			say: "hunk: tried 1 hunk and put back 1 file; the command timed out after 300ms, exit 124\n", file: "one\n",
			cmd: func(pid, _ string) string { return fmt.Sprintf("echo $$ > '%s'; exec "+longSleep(), pid) },
		},
		{
			name: "--keep-on-fail keeps it and is still 3", flags: []string{"--keep-on-fail", "--verify"}, code: exitVerifyFailed,
			say:  "hunk: applied 1 hunk, verify timed out after 300ms, changes left in place for inspection (--keep-on-fail)\n",
			file: "ONE\n",
			cmd:  func(pid, _ string) string { return fmt.Sprintf("echo $$ > '%s'; exec "+longSleep(), pid) },
		},
		{
			name: "what the command started goes with it", flags: []string{"--verify"}, code: exitVerifyFailed,
			say: "verify timed out after 300ms, rolled back 1 file\n", file: "one\n", bgGone: true,
			cmd: func(pid, bg string) string {
				return fmt.Sprintf(longSleep()+" & echo $! > '%s'; echo $$ > '%s'; wait", bg, pid)
			},
		},
		{
			// sh and sleep both ignore SIGTERM, which a child inherits, so only
			// the SIGKILL after the grace ends them.
			name: "a group that ignores SIGTERM is killed after the grace", flags: []string{"--verify"},
			code: exitVerifyFailed, say: "verify timed out after 300ms, rolled back 1 file\n", file: "one\n",
			posix: true, bgGone: true,
			cmd: func(pid, bg string) string {
				return fmt.Sprintf("trap '' TERM; "+longSleep()+" & echo $! > '%s'; echo $$ > '%s'; wait", bg, pid)
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.posix {
				needsSignals(t)
			}
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			pid, bg := pidFile(t, "pid"), pidFile(t, "bg")
			args := append(append([]string{}, c.flags...), c.cmd(pid, bg), "--timeout", "300ms")
			start := time.Now()
			code, _, errOut := runCLI(t, root, args, vPatch)
			took := time.Since(start)
			if code != c.code {
				t.Errorf("exit %d, want %d: %s", code, c.code, errOut)
			}
			if !strings.Contains(errOut, c.say) {
				t.Errorf("stderr = %q, want it to say %q", errOut, c.say)
			}
			if got := readFile(t, root, "a.txt"); got != c.file {
				t.Errorf("a.txt = %q, want %q", got, c.file)
			}
			if took > 6*time.Second {
				t.Errorf("took %v; the timeout is 300ms and the grace 2s", took)
			}
			if c.posix && took < verifyGrace {
				t.Errorf("took %v, less than the grace; SIGKILL came before the group had its %v", took, verifyGrace)
			}
			assertEnded(t, pidFrom(t, pid), "the command")
			if c.bgGone {
				assertEnded(t, pidFrom(t, bg), "the process the command started")
			}
		})
	}
}

// sh exiting is the end of the command, even when something it started still
// holds its output: hunk reads on for verifyWaitDelay, then stops and says so,
// and leaves that process running. Exec's own WaitDelay reports the cut only
// for a command that succeeded, which is why the failing row is here.
func TestAHeldPipeIsCut(t *testing.T) {
	withDelays(t, 2*time.Second, 300*time.Millisecond)
	const cut = "stopped reading 300ms after the command exited: something it started still holds its output open"
	for _, c := range []struct {
		name   string
		flags  []string
		tail   string // what the command does after starting the background sleep
		code   int
		stream string
		say    string
		file   string
		held   bool // the background process holds the pipe
	}{
		{
			"a verify that passes",
			[]string{"--verify"},
			"echo ok", exitOK, "out",
			"1 file, 1 hunk, +1 -1, verify ok (", "ONE\n", true,
		},
		{
			"a verify that fails",
			[]string{"--verify"},
			"echo FAIL; false", exitVerifyFailed, "err",
			"FAIL\n(last 1 of 1 lines)\n(" + cut + ")\n", "one\n", true,
		},
		{
			"a --try",
			[]string{"--try"},
			"echo tried", exitOK, "out",
			"tried 1 hunk and put back 1 file; the command exited 0", "one\n", true,
		},
		{
			"a background process with its output elsewhere",
			[]string{"--verify"},
			"echo ok", exitOK, "out",
			"verify ok (", "ONE\n", false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			bg := pidFile(t, "bg")
			redirect := ""
			if !c.held {
				redirect = " >/dev/null 2>&1"
			}
			command := fmt.Sprintf("%s%s & echo $! > '%s'; %s", longSleep(), redirect, bg, c.tail)
			start := time.Now()
			code, out, errOut := runCLI(t, root, append(append([]string{}, c.flags...), command), vPatch)
			took := time.Since(start)
			if code != c.code {
				t.Errorf("exit %d, want %d: %s%s", code, c.code, out, errOut)
			}
			report := out
			if c.stream == "err" {
				report = errOut
			}
			if !strings.Contains(report, c.say) {
				t.Errorf("report = %q, want it to say %q", report, c.say)
			}
			// The note is written only when the delay ran out, so its absence is
			// what says nothing was waited for; a clock would also measure how
			// long sh took to start, which on a Windows runner is not small.
			if got := strings.Contains(out+errOut, cut); got != c.held {
				t.Errorf("says the output was cut: %v, want %v\n%s%s", got, c.held, out, errOut)
			}
			if took > 5*time.Second {
				t.Errorf("took %v; hunk waited on the pipe rather than for %v after sh exited", took, verifyWaitDelay)
			}
			if got := readFile(t, root, "a.txt"); got != c.file {
				t.Errorf("a.txt = %q, want %q", got, c.file)
			}
			assertLeftRunning(t, pidFrom(t, bg), "the background sleep")
		})
	}

	t.Run("in JSON", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		bg := pidFile(t, "bg")
		code, out, _ := runCLI(t, root, []string{"--json", "--verify", fmt.Sprintf(longSleep()+" & echo $! > '%s'", bg)}, vPatch)
		var got struct {
			Exit   int
			Verify struct {
				OK        bool
				OutputCut bool `json:"output_cut"`
			}
		}
		must(t, json.Unmarshal([]byte(out), &got))
		if code != exitOK || got.Exit != exitOK || !got.Verify.OK || !got.Verify.OutputCut {
			t.Errorf("exit %d, json %+v; want 0, ok and output_cut\n%s", code, got, out)
		}
		assertLeftRunning(t, pidFrom(t, bg), "the background sleep")
	})
}

// signalSelf sends sig to this test process, where hunk's handler, installed by
// cli for the duration of a verify, receives it.
func signalSelf(t *testing.T, sig os.Signal) {
	t.Helper()
	p, err := os.FindProcess(os.Getpid())
	must(t, err)
	must(t, p.Signal(sig))
}

// A signal while the command runs ends it and the batch goes back, through cli
// in this process with a real signal to it. cli returns what a shell would see,
// 128 plus the signal, and never re-raises it, which main does and
// TestHunkDiesOfTheSignal asserts in a process of its own.
func TestASignalPutsTheBatchBack(t *testing.T) {
	needsSignals(t)
	// A non-interactive sh starts a background job with SIGINT ignored, so on
	// the SIGINT rows the background sleep outlives the signal and the grace
	// ends it, which is the path those rows take. One second is grace enough.
	withDelays(t, time.Second, 300*time.Millisecond)
	for _, c := range []struct {
		name  string
		sig   syscall.Signal
		flags []string
		code  int
		say   string
		file  string
	}{
		{
			"SIGTERM during a verify", syscall.SIGTERM,
			[]string{"--verify"},
			143,
			"hunk: applied 1 hunk, interrupted by SIGTERM during the verify, rolled back 1 file\n", "one\n",
		},
		{
			"SIGINT during a verify", syscall.SIGINT,
			[]string{"--verify"},
			130,
			"interrupted by SIGINT during the verify, rolled back 1 file\n", "one\n",
		},
		{
			"SIGHUP during a verify", syscall.SIGHUP,
			[]string{"--verify"},
			129,
			"interrupted by SIGHUP during the verify, rolled back 1 file\n", "one\n",
		},
		{
			"SIGTERM during a --try", syscall.SIGTERM,
			[]string{"--try"},
			143,
			"hunk: tried 1 hunk and put back 1 file; interrupted by SIGTERM, the command was ended (", "one\n",
		},
		{
			"SIGINT with --keep-on-fail keeps the batch", syscall.SIGINT,
			[]string{"--keep-on-fail", "--verify"},
			130,
			"interrupted by SIGINT during the verify, changes left in place for inspection (--keep-on-fail)\n", "ONE\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			pid, bg := pidFile(t, "pid"), pidFile(t, "bg")
			command := fmt.Sprintf(longSleep()+" & echo $! > '%s'; echo $$ > '%s'; wait", bg, pid)
			type result struct {
				code        int
				out, errOut string
				raise       os.Signal
			}
			done := make(chan result, 1)
			go func() {
				var out, errOut bytes.Buffer
				var raise os.Signal
				args := append(append([]string{"--root", root}, c.flags...), command)
				code := invoke(args, strings.NewReader(vPatch), &out, &errOut, &raise)
				done <- result{code, out.String(), errOut.String(), raise}
			}()
			shPID := pidFrom(t, pid)
			signalSelf(t, c.sig)
			var r result
			select {
			case r = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("cli did not return after the signal")
			}
			if r.code != c.code {
				t.Errorf("exit %d, want %d: %s%s", r.code, c.code, r.out, r.errOut)
			}
			// What main is handed to die of, once the batch is back.
			if r.raise != c.sig {
				t.Errorf("hands main %v to re-raise, want %v", r.raise, c.sig)
			}
			if !strings.Contains(r.errOut, c.say) {
				t.Errorf("stderr = %q, want it to say %q", r.errOut, c.say)
			}
			if got := readFile(t, root, "a.txt"); got != c.file {
				t.Errorf("a.txt = %q, want %q", got, c.file)
			}
			assertEnded(t, shPID, "sh")
			assertEnded(t, pidFrom(t, bg), "the process the command started")
		})
	}

	t.Run("a file that cannot go back is exit 4", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		pid := pidFile(t, "pid")
		command := fmt.Sprintf("printf 'x\\n' > a.txt; echo $$ > '%s'; exec "+longSleep(), pid)
		done := make(chan [3]any, 1)
		go func() {
			var out, errOut bytes.Buffer
			var raise os.Signal
			code := invoke([]string{"--root", root, "--verify", command}, strings.NewReader(vPatch), &out, &errOut, &raise)
			done <- [3]any{code, errOut.String(), raise}
		}()
		shPID := pidFrom(t, pid)
		signalSelf(t, syscall.SIGTERM)
		r := <-done
		code, errOut := r[0].(int), r[1].(string)
		if code != exitRollbackFailed {
			t.Errorf("exit %d, want 4: %s", code, errOut)
		}
		// An inconsistent tree is said with 4, not with the signal.
		if r[2] != nil {
			t.Errorf("hands main %v to re-raise; with a file left alone it should exit 4", r[2])
		}
		for _, want := range []string{"interrupted by SIGTERM during the verify, rolled back 0 files, 1 file left alone", "a.txt was not restored."} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr = %q, want it to say %q", errOut, want)
			}
		}
		if got := readFile(t, root, "a.txt"); got != "x\n" {
			t.Errorf("a.txt = %q, want the command's x, left alone", got)
		}
		assertEnded(t, shPID, "the command")
	})
}

// A signal that lands while the batch is being written: the commit finishes,
// the command never starts, and the batch goes back. notifyInterrupts is
// wrapped so the signal arrives at the moment the handler goes in, which is
// after the check and before the first write.
func TestASignalWhileTheBatchIsWritten(t *testing.T) {
	needsSignals(t)
	orig := notifyInterrupts
	t.Cleanup(func() { notifyInterrupts = orig })
	notifyInterrupts = func(c chan<- os.Signal) {
		orig(c)
		signalSelf(t, syscall.SIGTERM)
		for deadline := time.Now().Add(5 * time.Second); len(c) == 0; {
			if time.Now().After(deadline) {
				t.Error("the signal never reached the handler")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	for _, c := range []struct {
		name, flag string
		say        string
	}{
		{"before a verify", "--verify", "hunk: interrupted by SIGTERM before the --verify command ran; the batch was put back\n"},
		{"before a --try", "--try", "hunk: interrupted by SIGTERM before the --try command ran; the batch was put back\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			ran := pidFile(t, "ran")
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, []string{c.flag, fmt.Sprintf("echo ran > '%s'", ran)}, vPatch)
			if code != 143 {
				t.Errorf("exit %d, want 143: %s%s", code, out, errOut)
			}
			if errOut != c.say {
				t.Errorf("stderr = %q, want %q", errOut, c.say)
			}
			if _, err := os.Stat(filepath.FromSlash(ran)); err == nil {
				t.Error("the command ran after the signal")
			}
			assertUnchanged(t, root, before)
		})
	}

	t.Run("in JSON", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, out, _ := runCLI(t, root, []string{"--json", "--verify", "true"}, vPatch)
		var got struct {
			Exit   int
			Error  string
			Verify struct {
				Ran         bool
				RolledBack  int    `json:"rolled_back"`
				Interrupted string `json:"interrupted_by"`
			}
		}
		must(t, json.Unmarshal([]byte(out), &got))
		if code != 143 || got.Exit != 143 || got.Verify.Ran || got.Verify.RolledBack != 1 || got.Verify.Interrupted != "SIGTERM" ||
			!strings.Contains(got.Error, "before the --verify command ran") {
			t.Errorf("exit %d, json %+v\n%s", code, got, out)
		}
	})

	t.Run("a load that fails is not interrupted", func(t *testing.T) {
		// The handler goes in after the check, so a batch that fails before
		// then never meets it: this patch does not match, and the signal the
		// wrapper would send is never sent.
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--verify", "true"}, "@@ file a.txt\n@@ old\nnot there\n@@ new\nx\n")
		if code != exitNoMatch || strings.Contains(errOut, "interrupted") {
			t.Errorf("exit %d: %s", code, errOut)
		}
	})
}

// startHunk runs this test binary as hunk, in a process of its own, so that a
// signal can kill it the way a harness would.
func startHunk(t *testing.T, root string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	exe, err := os.Executable()
	must(t, err)
	cmd := exec.Command(exe, append([]string{"--root", root}, args...)...)
	cmd.Env = childEnv("HUNK_TEST_AS_HUNK=1")
	cmd.Stdin = strings.NewReader(vPatch)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	must(t, cmd.Start())
	return cmd, &out
}

// What a parent of hunk sees. main re-raises the signal once the batch is back,
// so a harness or a shell sees a death by that signal, as it did before hunk
// caught it; and SIGKILL, which nothing can catch, is the floor.
func TestHunkDiesOfTheSignal(t *testing.T) {
	needsSignals(t)
	for _, c := range []struct {
		name   string
		sig    syscall.Signal
		flag   string
		pre    string // run by the command before it waits
		dies   bool   // hunk ends by the signal, rather than with an exit code
		exit   int    // when it does not
		file   string
		say    string
		orphan bool // the command outlives hunk
	}{
		{
			name: "SIGTERM during a verify", sig: syscall.SIGTERM, flag: "--verify", dies: true, file: "one\n",
			say: "interrupted by SIGTERM during the verify, rolled back 1 file",
		},
		{
			name: "SIGINT during a verify, as Ctrl-C sends", sig: syscall.SIGINT, flag: "--verify", dies: true, file: "one\n",
			say: "interrupted by SIGINT during the verify, rolled back 1 file",
		},
		{
			name: "SIGHUP during a verify", sig: syscall.SIGHUP, flag: "--verify", dies: true, file: "one\n",
			say: "interrupted by SIGHUP during the verify, rolled back 1 file",
		},
		{
			name: "SIGTERM during a --try", sig: syscall.SIGTERM, flag: "--try", dies: true, file: "one\n",
			say: "tried 1 hunk and put back 1 file; interrupted by SIGTERM, the command was ended",
		},
		{
			name: "a file that cannot go back is exit 4, not a signal", sig: syscall.SIGTERM, flag: "--verify",
			pre: "printf 'x\\n' > a.txt; ", exit: exitRollbackFailed, file: "x\n", say: "1 file left alone",
		},
		{
			name: "SIGKILL cannot be caught, and leaves the batch applied", sig: syscall.SIGKILL, flag: "--verify",
			dies: true, file: "ONE\n", orphan: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			pid := pidFile(t, "pid")
			cmd, out := startHunk(t, root, c.flag, fmt.Sprintf("%secho $$ > '%s'; exec "+longSleep(), c.pre, pid))
			shPID := pidFrom(t, pid)
			must(t, cmd.Process.Signal(c.sig))
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			var err error
			select {
			case err = <-waited:
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
				killPID(shPID)
				t.Fatal("hunk did not end after the signal")
			}
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("hunk exited 0 (%v) after %v:\n%s", err, c.sig, out)
			}
			ws := ee.Sys().(syscall.WaitStatus)
			switch {
			case c.dies && (!ws.Signaled() || ws.Signal() != c.sig):
				t.Errorf("hunk ended with %v, want death by %v:\n%s", ws, c.sig, out)
			case !c.dies && (ws.Signaled() || ws.ExitStatus() != c.exit):
				t.Errorf("hunk ended with %v, want exit %d:\n%s", ws, c.exit, out)
			}
			if !strings.Contains(out.String(), c.say) {
				t.Errorf("output = %q, want it to say %q", out, c.say)
			}
			if got := readFile(t, root, "a.txt"); got != c.file {
				t.Errorf("a.txt = %q, want %q", got, c.file)
			}
			if c.orphan {
				assertLeftRunning(t, shPID, "the command, which SIGKILL to hunk cannot reach")
			} else {
				assertEnded(t, shPID, "the command")
			}
		})
	}
}

// The new report shapes, text and JSON, from constructed results so that no
// duration or path moves them.
func TestStopGoldens(t *testing.T) {
	res := &Result{Files: []FileResult{{Path: "src/faults.zig", Op: "modify", Added: 1}}, Hunks: 2}
	base := func(try bool) *Verify {
		return &Verify{
			Ran: true, Try: try, Command: "zig build test", Seconds: 4.2,
			Tail: []string{"Build Summary: 3/5 steps succeeded", "test: waiting"}, TotalLines: 2,
			Applied: 2, RolledBack: 1,
		}
	}
	for _, c := range []struct {
		name string
		edit func(v *Verify)
		try  bool
		exit int
	}{
		{"report-verify-timed-out", func(v *Verify) { v.TimedOut, v.Status = 2*time.Minute, 143 }, false, exitVerifyFailed},
		{"report-try-timed-out", func(v *Verify) { v.TimedOut, v.Status = 90*time.Second, exitTimedOut }, true, exitTimedOut},
		{"report-verify-interrupted", func(v *Verify) { v.Signal, v.Status = syscall.SIGTERM, 143 }, false, 143},
		{"report-try-interrupted", func(v *Verify) { v.Signal, v.Status = syscall.SIGINT, 130 }, true, 130},
		{"report-verify-interrupted-left-alone", func(v *Verify) {
			v.Signal, v.Status, v.RolledBack = syscall.SIGTERM, 143, 0
			v.NotRestored = []NotRestored{{Path: "src/faults.zig", Reason: "Something rewrote it after hunk did."}}
		}, false, exitRollbackFailed},
		{"report-verify-ok-output-cut", func(v *Verify) {
			v.OK, v.Cut, v.Applied, v.RolledBack, v.Tail, v.TotalLines = true, true, 0, 0, nil, 0
		}, false, exitOK},
		{"report-verify-failed-output-cut", func(v *Verify) { v.Status, v.Cut = 1, true }, false, exitVerifyFailed},
		{"report-try-output-cut", func(v *Verify) { v.OK, v.Cut = true, true }, true, exitOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := base(c.try)
			c.edit(v)
			rep := NewReport(res, nil, v, false, 2)
			if rep.Exit != c.exit {
				t.Errorf("exit %d, want %d", rep.Exit, c.exit)
			}
			var out, errOut, js bytes.Buffer
			rep.Text(&out, &errOut, false)
			golden(t, c.name, out.String()+errOut.String())
			must(t, rep.JSON(&js))
			golden(t, c.name+"-json", js.String())
		})
	}
}

func TestShortDurationAndSignalName(t *testing.T) {
	for d, want := range map[time.Duration]string{
		300 * time.Millisecond: "300ms", 90 * time.Second: "1m30s", 2 * time.Minute: "2m",
		time.Hour: "1h", 90 * time.Minute: "1h30m", time.Hour + time.Second: "1h0m1s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
	for sig, want := range map[os.Signal]string{
		syscall.SIGINT: "SIGINT", syscall.SIGTERM: "SIGTERM", syscall.SIGHUP: "SIGHUP", syscall.SIGKILL: syscall.SIGKILL.String(),
	} {
		if got := signalName(sig); got != want {
			t.Errorf("signalName(%v) = %q, want %q", sig, got, want)
		}
	}
	if got := signalExit(syscall.SIGINT); got != 130 {
		t.Errorf("signalExit(SIGINT) = %d, want 130", got)
	}
	if got := signalExit(fakeSignal{}); got != 128+int(syscall.SIGTERM) {
		t.Errorf("signalExit of a signal with no number = %d, want SIGTERM's", got)
	}
}

// fakeSignal is an os.Signal that is not a syscall.Signal, which the standard
// library never delivers but the interface allows.
type fakeSignal struct{}

func (fakeSignal) String() string { return "fake" }
func (fakeSignal) Signal()        {}
