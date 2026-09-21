package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const vPatch = "@@ file a.txt\n@@ old\none\n@@ new\nONE\n"

// §8.1's rollback tests, all four, plus the case §6.3 says the default breaks.
func TestVerifyAndRollback(t *testing.T) {
	t.Run("a passing verify leaves the changes and exits 0", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, out, errOut := runCLI(t, root, []string{"--verify", "grep -q ONE a.txt"}, vPatch)
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if got := readFile(t, root, "a.txt"); got != "ONE\n" {
			t.Errorf("got %q", got)
		}
		if !strings.Contains(out, "verify ok") {
			t.Errorf("out = %q", out)
		}
	})

	t.Run("a failing verify rolls back and every file is byte-identical", func(t *testing.T) {
		root := cliTree(t, map[string]string{
			"a.txt": "one\n",
			"b.txt": "two\n",
		})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, []string{"--verify", "false"},
			vPatch+"@@ file b.txt\n@@ old\ntwo\n@@ new\nTWO\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s", code, errOut)
		}
		assertUnchanged(t, root, before)
		if !strings.Contains(errOut, "applied 2 hunks, verify failed, rolled back 2 files") {
			t.Errorf("err = %q", errOut)
		}
	})

	// §6.3: something rewrote the file after hunk did, and hunk cannot tell a
	// formatter from a second agent. The default refuses to revert it.
	t.Run("a file touched underneath is left alone, at exit 4", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "printf 'somebody else\\n' > a.txt; false"}, vPatch)
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s", code, errOut)
		}
		if got := readFile(t, root, "a.txt"); got != "somebody else\n" {
			t.Errorf("the other writer's work was discarded: %q", got)
		}
		for _, want := range []string{"was not restored", "Something rewrote it after hunk did", "--verify-may-format"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("err missing %q:\n%s", want, errOut)
			}
		}
	})

	t.Run("--verify-may-format restores it anyway, at exit 3", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "printf 'reformatted\\n' > a.txt; false", "--verify-may-format"}, vPatch)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s", code, errOut)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("--keep-on-fail leaves the changes and still exits 3", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--verify", "false", "--keep-on-fail"}, vPatch)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3", code)
		}
		if got := readFile(t, root, "a.txt"); got != "ONE\n" {
			t.Errorf("the changes were rolled back: %q", got)
		}
		if !strings.Contains(errOut, "left in place") {
			t.Errorf("err = %q", errOut)
		}
	})

	// §6.3's motivating case, verbatim in shape: "make fmt && go test". The
	// formatter rewrites the file, then the tests fail. Under the default this
	// is exit 4, which is exactly what §11.2 is a decision about.
	t.Run("a formatting verify that then fails", func(t *testing.T) {
		// A plain redirect rather than `sed -i`, which is not portable and was
		// wrong here in the direction that looks like a pass. GNU sed takes an
		// optional attached suffix, BSD sed a separate required one, so on
		// macOS `sed -i 's/ONE/One/' a.txt` reads the script as the suffix and
		// "a.txt" as the script, fails, and rewrites nothing. The verify then
		// failed without having formatted, rollback restored correctly, and the
		// test saw exit 3 where it wanted 4. It looked like the tool not
		// detecting a rewrite; it was the fixture never making one.
		const fmtThenFail = "printf 'One\\n' > a.txt; false"
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, _ := runCLI(t, root, []string{"--verify", fmtThenFail}, vPatch)
		if code != exitRollbackFailed {
			t.Errorf("exit %d, want 4: the default does not restore a file the verify rewrote", code)
		}

		root = cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", fmtThenFail, "--verify-may-format"}, vPatch)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s", code, errOut)
		}
		assertUnchanged(t, root, before)
	})

	// A file that is gone cannot be hashed, so it counts as changed. Restoring
	// it could not discard anybody's work, but hunk cannot tell a formatter's
	// cleanup from a deliberate removal, which is why the default refuses.
	t.Run("a file the verify deleted is not restored by default", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--verify", "rm a.txt; false"}, vPatch)
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s", code, errOut)
		}
		if !strings.Contains(errOut, "gone from disk") {
			t.Errorf("err = %q", errOut)
		}
		if _, err := os.Stat(filepath.Join(root, "a.txt")); err == nil {
			t.Error("the file was restored")
		}
	})

	t.Run("and --verify-may-format brings it back", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "rm a.txt; false", "--verify-may-format"}, vPatch)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s", code, errOut)
		}
		assertUnchanged(t, root, before)
	})
}

func TestVerifyMechanics(t *testing.T) {
	t.Run("the command runs in --root", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n", "marker.txt": "here\n"})
		code, _, errOut := runCLI(t, root, []string{"--verify", "test -f marker.txt"}, vPatch)
		if code != exitOK {
			t.Fatalf("exit %d, so the verify did not run in --root: %s", code, errOut)
		}
	})

	t.Run("a command that cannot start is exit 5, not a failed verify", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		// A shell that does not exist. Exit 3 would say the tests failed, and
		// nothing was verified at all.
		v, err := RunVerify("true", root, 40)
		if err != nil || !v.OK {
			t.Fatalf("a working command: %v %+v", err, v)
		}
		if _, err := RunVerify("true", filepath.Join(root, "nonexistent-dir"), 40); err == nil {
			t.Error("a verify in a directory that does not exist should not report as a failed verify")
		}
	})

	t.Run("--verify-lines caps the tail and says what it elided", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		_, _, errOut := runCLI(t, root,
			[]string{"--verify", "seq 1 50; false", "--verify-lines", "3"}, vPatch)
		if !strings.Contains(errOut, "(last 3 of 50 lines)") {
			t.Errorf("err = %q", errOut)
		}
		if strings.Contains(errOut, "\n47\n") {
			t.Errorf("printed more than the tail:\n%s", errOut)
		}
		if !strings.Contains(errOut, "50") {
			t.Errorf("the tail is not the last lines:\n%s", errOut)
		}
	})

	t.Run("a verify producing no output prints no elision count", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		_, _, errOut := runCLI(t, root, []string{"--verify", "exit 1"}, vPatch)
		if strings.Contains(errOut, "of 0 lines") {
			t.Errorf("err = %q", errOut)
		}
	})

	t.Run("the duration is measured", func(t *testing.T) {
		v, err := RunVerify("sleep 0.05", t.TempDir(), 40)
		must(t, err)
		if v.Seconds < 0.04 {
			t.Errorf("seconds = %v", v.Seconds)
		}
	})
}

func TestVerifyFlagInteractions(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	for _, c := range []struct {
		name string
		args []string
		msg  string
	}{
		{"--keep-on-fail with --verify-may-format", []string{"--verify", "false", "--keep-on-fail", "--verify-may-format"}, "cannot both be set"},
		{"--verify-may-format alone", []string{"--verify-may-format"}, "does nothing without --verify"},
		{"--keep-on-fail alone", []string{"--keep-on-fail"}, "does nothing without --verify"},
		{"--verify-lines below one", []string{"--verify", "true", "--verify-lines", "0"}, "at least 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, c.args, vPatch)
			if code != exitUsage {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(errOut, c.msg) {
				t.Errorf("err = %q, want %q", errOut, c.msg)
			}
			assertUnchanged(t, root, before)
		})
	}

	// §4: --dry-run writes nothing and runs no verify. Saying so beats letting
	// the caller assume it passed.
	t.Run("--dry-run runs no verify and says so", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, out, _ := runCLI(t, root, []string{"--dry-run", "--verify", "exit 7"}, vPatch)
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, "verify not run") {
			t.Errorf("out = %q", out)
		}
		assertUnchanged(t, root, before)
	})
}

func TestVerifyGoldens(t *testing.T) {
	t.Run("exit 3 through the real CLI", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		_, _, errOut := runCLI(t, root,
			[]string{"--verify", "printf 'FAIL one\\nFAIL two\\n'; false"}, vPatch)
		golden(t, "cli-verify-failed", errOut)
	})

	t.Run("exit 4 through the real CLI", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		_, _, errOut := runCLI(t, root,
			[]string{"--verify", "printf 'x\\n' > a.txt; printf 'FAIL\\n'; false"}, vPatch)
		// The hashes are content-derived and stable, so this golden is too.
		golden(t, "cli-rollback-incomplete", errOut)
	})
}

// The verify runs a subprocess, and TestNoNetworkDependency is a gate this is
// the first thing in the tool that could have broken.
func TestVerifyDoesNotNeedTheNetwork(t *testing.T) {
	var b bytes.Buffer
	if code := cli([]string{"--help"}, strings.NewReader(""), &b, &b); code != exitOK {
		t.Fatal("help")
	}
	// The real assertion is TestNoNetworkDependency; this records why it
	// matters here.
	if !strings.Contains(b.String(), "--verify") {
		t.Error("--help lost --verify")
	}
}

func TestVerifyCoverageEdges(t *testing.T) {
	// Rollback skips a file no hunk changed. Reachable because a hunk whose new
	// text equals its old changed nothing, so its file is loaded and untouched.
	t.Run("rollback skips a file nothing changed", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n", "b.txt": "two\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, []string{"--verify", "false"},
			"@@ file a.txt\n@@ old\none\n@@ new\none\n"+
				"@@ file b.txt\n@@ old\ntwo\n@@ new\nTWO\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		// §5.3's first line counts hunks, not files, and counts only the hunks
		// that changed something: the no-op hunk matched and did nothing, so
		// there is nothing for the rollback to undo on its account.
		if !strings.Contains(errOut, "applied 1 hunk, verify failed, rolled back 1 file") {
			t.Errorf("the no-op hunk and its file should not be counted: %q", errOut)
		}
		assertUnchanged(t, root, before)
	})

	// Exit 4 also covers a file rollback could not write back.
	t.Run("a file that cannot be restored is named", func(t *testing.T) {
		needsPOSIXPerms(t)
		root := cliTree(t, map[string]string{"sub/a.txt": "one\n"})
		sub := filepath.Join(root, "sub")
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "chmod 555 sub; false"},
			"@@ file sub/a.txt\n@@ old\none\n@@ new\nONE\n")
		os.Chmod(sub, 0o755)
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s", code, errOut)
		}
		if !strings.Contains(errOut, "could not be written") {
			t.Errorf("err = %q", errOut)
		}
	})

	// A shell that cannot be found means nothing was verified at all, which is
	// exit 5 and not exit 3. Reporting it as a failed verify would be a lie
	// about the tree.
	t.Run("no shell on PATH is exit 5", func(t *testing.T) {
		t.Setenv("PATH", "")
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--verify", "true"}, vPatch)
		if code != exitIO {
			t.Fatalf("exit %d, want 5: %s", code, errOut)
		}
		if !strings.Contains(errOut, "could not run the verify command") {
			t.Errorf("err = %q", errOut)
		}
		// And it did not claim the verify failed.
		if strings.Contains(errOut, "verify failed") {
			t.Errorf("reported as a failed verify: %q", errOut)
		}
	})

	t.Run("--dry-run reports a load failure", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--dry-run"},
			"@@ file gone.txt\n@@ old\nx\n@@ new\ny\n")
		if code != exitNoMatch {
			t.Errorf("exit %d, want 2", code)
		}
		if !strings.Contains(errOut, "no such file") {
			t.Errorf("err = %q", errOut)
		}
	})
}

// --try (§4.1, decided 2026-09-21): apply, run the command, put the batch back
// whatever it says, and exit with its status. It replaces the shape §1 was
// measured on, a backup, an edit, a run and a restore, which a field session
// wrote three times in a day. Each outcome asserts the exit, the stream the
// report went to, and the tree, which is back in every row but the one where
// the command rewrote the file itself.
func TestTryPutsTheTreeBackWhateverTheCommandSays(t *testing.T) {
	for _, c := range []struct {
		name   string
		args   []string
		patch  string
		exit   int
		stdout string // a line the stdout report carries, or "" for none
		stderr string // likewise for stderr
		after  string // a.txt afterwards
	}{
		{
			"the command passes",
			[]string{"--try", "grep -c ONE a.txt"},
			vPatch,
			exitOK, "tried 1 hunk and put back 1 file; the command exited 0", "", "one\n",
		},
		{
			// The command's output is the point, so it is printed on success too.
			"its output is printed on success",
			[]string{"--try", "echo measured 424"},
			vPatch,
			exitOK, "measured 424", "", "one\n",
		},
		{
			"the command fails",
			[]string{"--try", "grep -c ONE a.txt; exit 3"},
			vPatch,
			3, "", "hunk: tried 1 hunk and put back 1 file; the command exited 3", "one\n",
		},
		{
			// The command's 2, told apart from hunk's own by the report.
			"the command exits 2",
			[]string{"--try", "exit 2"},
			vPatch,
			exitNoMatch, "", "hunk: tried 1 hunk", "one\n",
		},
		{
			// hunk's own 2: the command never ran, so it wrote no marker.
			"the patch does not match",
			[]string{"--try", "touch ran"},
			"@@ file a.txt\n@@ old\nabsent\n@@ new\nx\n",
			exitNoMatch, "", "did not match", "one\n",
		},
		{
			"the command rewrites the file",
			[]string{"--try", "echo other > a.txt"},
			vPatch,
			exitRollbackFailed, "", "put back 0 files, 1 file left alone; the command exited 0", "other\n",
		},
		{
			"and --verify-may-format puts it back anyway",
			[]string{"--try", "echo other > a.txt", "--verify-may-format"},
			vPatch,
			exitOK, "tried 1 hunk and put back 1 file", "", "one\n",
		},
		{
			"--dry-run runs nothing",
			[]string{"--try", "touch ran", "--dry-run"},
			vPatch,
			exitOK, "(dry run: nothing written, command not run)", "", "one\n",
		},
		{
			"--quiet silences a success",
			[]string{"--try", "echo hidden", "--quiet"},
			vPatch,
			exitOK, "", "", "one\n",
		},
		{
			"--verify-lines caps the tail",
			[]string{"--try", "printf '1\\n2\\n3\\n'; exit 1", "--verify-lines", "2"},
			vPatch,
			1, "", "(last 2 of 3 lines)", "one\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			code, out, errOut := runCLI(t, root, c.args, c.patch)
			if code != c.exit {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.exit, out, errOut)
			}
			for _, s := range []struct{ got, want, stream string }{{out, c.stdout, "stdout"}, {errOut, c.stderr, "stderr"}} {
				switch {
				case s.want == "" && s.got != "":
					t.Errorf("%s = %q, want nothing", s.stream, s.got)
				case s.want != "" && !strings.Contains(s.got, s.want):
					t.Errorf("%s = %q, want it to carry %q", s.stream, s.got, s.want)
				}
			}
			if got := readFile(t, root, "a.txt"); got != c.after {
				t.Errorf("a.txt = %q, want %q", got, c.after)
			}
			if _, err := os.Stat(filepath.Join(root, "ran")); err == nil {
				t.Error("the command ran")
			}
		})
	}

	// sh itself killed has no exit code, so the status is the shell's 128 plus
	// the signal. Windows has no signal for kill -9 $$ to send.
	t.Run("a killed command is 128 plus the signal", func(t *testing.T) {
		needsSignals(t)
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--try", "kill -9 $$"}, vPatch)
		if code != 137 || !strings.Contains(errOut, "the command exited 137") {
			t.Errorf("exit %d: %s", code, errOut)
		}
		if got := readFile(t, root, "a.txt"); got != "one\n" {
			t.Errorf("a.txt = %q", got)
		}
	})

	t.Run("a command that cannot start is 5, with the batch put back", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		t.Setenv("PATH", t.TempDir()) // no sh to be found
		code, _, errOut := runCLI(t, root, []string{"--try", "true"}, vPatch)
		if code != exitIO || !strings.Contains(errOut, "could not run the --try command") ||
			!strings.HasSuffix(errOut, "; the batch was put back\n") {
			t.Errorf("exit %d: %q", code, errOut)
		}
		if got := readFile(t, root, "a.txt"); got != "one\n" {
			t.Errorf("a.txt = %q", got)
		}
	})

	t.Run("--json carries the command's status and the put-back", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, js, _ := runCLI(t, root, []string{"--json", "--try", "echo out; exit 3"}, vPatch)
		var v struct {
			OK   bool
			Exit int
			Try  struct {
				Ran        bool
				Status     int
				Output     []string
				RolledBack int `json:"rolled_back"`
			}
		}
		must(t, json.Unmarshal([]byte(js), &v))
		if code != 3 || v.Exit != 3 || v.OK || !v.Try.Ran || v.Try.Status != 3 ||
			v.Try.RolledBack != 1 || len(v.Try.Output) != 1 || v.Try.Output[0] != "out" {
			t.Errorf("exit %d: %s", code, js)
		}
	})
}

// A command that cannot start, after which a file cannot be put back because
// something changed it: the message must not claim the batch was put back.
// Nothing through the CLI can change a file between the apply and the put
// back, so the phases are driven here.
func TestTryThatCannotStartDoesNotClaimAPutBack(t *testing.T) {
	tree, root := fixture(t, map[string]string{"a.txt": "one\n"})
	txn := NewTxn(tree, Options{})
	p, err := Parse([]byte(vPatch), DefaultMarker)
	must(t, err)
	_, err = txn.Run(p)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("somebody else\n"), 0o644))
	t.Setenv("PATH", t.TempDir())

	v, err := runTry(txn, tree, "true", 40, false)
	if err == nil || !strings.HasSuffix(err.Error(), "; 1 file could not be put back") {
		t.Fatalf("err = %v", err)
	}
	rep := NewReport(nil, err, v, false, 1)
	if rep.Exit != exitRollbackFailed {
		t.Errorf("exit %d, want 4", rep.Exit)
	}
	var out, errOut bytes.Buffer
	rep.Text(&out, &errOut, false)
	if !strings.Contains(errOut.String(), "a.txt was not restored.") {
		t.Errorf("does not name the file:\n%s", errOut.String())
	}
	if got := readFile(t, root, "a.txt"); got != "somebody else\n" {
		t.Errorf("a.txt = %q; the other writer's work was overwritten", got)
	}
}

func TestTryFlagInteractions(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	for _, c := range []struct {
		name string
		args []string
		msg  string
	}{
		{"--try with --verify", []string{"--try", "true", "--verify", "true"}, "--try and --verify cannot both be set"},
		{"--verify-may-format with neither", []string{"--verify-may-format"}, "does nothing without --verify or --try"},
		{"--keep-on-fail with --try", []string{"--try", "true", "--keep-on-fail"}, "--keep-on-fail does nothing without --verify"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, c.args, vPatch)
			if code != exitUsage || !strings.Contains(errOut, c.msg) {
				t.Errorf("exit %d, err %q, want 1 and %q", code, errOut, c.msg)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The three report shapes, from fixed timings so the goldens are stable.
func TestTryGoldens(t *testing.T) {
	v := func(status int, notRestored ...NotRestored) *Verify {
		return &Verify{
			Ran: true, Try: true, Status: status, OK: status == 0, Command: "zig build test",
			Seconds: 4.2, Tail: []string{"All 12 tests passed.", "FBA_USED 1320"}, TotalLines: 2,
			Applied: 2, RolledBack: 1 - len(notRestored), NotRestored: notRestored,
		}
	}
	res := &Result{Files: []FileResult{{Path: "src/faults.zig", Op: "modify", Added: 1}}, Hunks: 2}
	for _, c := range []struct {
		name string
		v    *Verify
	}{
		{"report-try-passed", v(0)},
		{"report-try-failed", v(1)},
		{"report-try-left-alone", v(0, NotRestored{Path: "src/faults.zig", Reason: "Something rewrote it after hunk did."})},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := NewReport(res, nil, c.v, false, 2)
			var out, errOut, js bytes.Buffer
			rep.Text(&out, &errOut, false)
			golden(t, c.name, out.String()+errOut.String())
			must(t, rep.JSON(&js))
			golden(t, c.name+"-json", js.String())
		})
	}
}
