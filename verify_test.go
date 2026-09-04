package main

import (
	"bytes"
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
		const fmtThenFail = "sed -i 's/ONE/One/' a.txt; false"
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
