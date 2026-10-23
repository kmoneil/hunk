package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file that holds its original bytes again, after the command hunk ran, is
// put back like any other (#106). Until 2026-10-20 rollback compared what was
// at the name only with what hunk wrote, so a lint's --fix that wrote a file
// back as it was, or a checkout that recreated a file the batch deleted, left
// the tree exactly as it began at exit 4, "not restored", and under --each it
// stopped the run. Only bytes that are neither hunk's nor the original are
// somebody's work, and those are still left.

type originalCase struct {
	name  string
	files map[string]string
	modes map[string]fs.FileMode // given after the files are written
	patch string
	args  []string // --verify or --try, and its command
	perms bool     // a mode is asserted, or taken away with chmod
	links bool     // the command makes a symbolic link with ln -s
	// fifo is the name the command makes a named pipe at. Rollback must not
	// read it: the read would wait for a writer that never comes.
	fifo string
	// chmodBack is what the command made unreadable, given back before the
	// tree is read and whatever happens.
	chmodBack []string
	exit      int
	says      []string // in the report
	not       []string // not in the report
	// after is the tree afterwards as treeOf gives it, a named pipe as "named
	// pipe". Nil means the tree is as it began, modes included.
	after map[string]string
}

const (
	modifyOne   = "@@ file a.txt\n@@ old\none\n@@ new\ntwo\n"
	deleteOne   = "@@ delete a.txt\n"
	backAsItWas = "printf 'one\\n' > a.txt"
)

func TestAFileHoldingItsOriginalBytesIsPutBack(t *testing.T) {
	one := map[string]string{"a.txt": "one\n"}
	runOriginalCases(t, []originalCase{
		{
			name:  "#106 as filed: the command writes a modified file back as it was",
			files: one, patch: modifyOne, args: []string{"--try", backAsItWas},
			exit: exitOK,
			says: []string{"tried 1 hunk and put back 1 file; the command exited 0"},
			not:  []string{"was not restored", "left alone"},
		},
		{
			name:  "the same under a --verify that then fails",
			files: one, patch: modifyOne, args: []string{"--verify", backAsItWas + "; false"},
			exit: exitVerifyFailed,
			says: []string{"verify failed, rolled back 1 file\n"},
			not:  []string{"was not restored", "left alone"},
		},
		{
			name:  "a deleted file the command recreated as it was",
			files: one, patch: deleteOne, args: []string{"--try", backAsItWas},
			exit: exitOK,
			says: []string{"tried 1 hunk and put back 1 file; the command exited 0"},
			not:  []string{"was not restored", "left alone"},
		},
		{
			name:  "the same under a --verify that then fails",
			files: one, patch: deleteOne, args: []string{"--verify", backAsItWas + "; false"},
			exit: exitVerifyFailed,
			says: []string{"verify failed, rolled back 1 file\n"},
			not:  []string{"was not restored"},
		},
		{
			// Put back is the file as it was, mode and all (§6.6), so the
			// restore still writes: a recreated file has the umask's mode.
			name:  "a deleted executable recreated as it was gets its mode back",
			files: one, modes: map[string]fs.FileMode{"a.txt": 0o755}, perms: true,
			patch: deleteOne, args: []string{"--try", backAsItWas},
			exit: exitOK,
			says: []string{"put back 1 file;"},
		},
		{
			name:  "a modified file written back as it was under another mode gets its mode back",
			files: one, perms: true,
			patch: modifyOne, args: []string{"--try", "printf 'one\\n' > t && chmod 600 t && mv t a.txt"},
			exit: exitOK,
			says: []string{"put back 1 file;"},
		},
		{
			name:  "--verify-may-format agrees, as it always did",
			files: one, patch: modifyOne, args: []string{"--try", backAsItWas, "--verify-may-format"},
			exit: exitOK,
			says: []string{"put back 1 file;"},
		},
		{
			name:  "of two files, the one written back as it was is put back and the rewritten one left",
			files: map[string]string{"a.txt": "one\n", "b.txt": "bee\n"},
			patch: modifyOne + "@@ file b.txt\n@@ old\nbee\n@@ new\nBEE\n",
			args:  []string{"--try", backAsItWas + "; printf 'theirs\\n' > b.txt"},
			exit:  exitRollbackFailed,
			says:  []string{"put back 1 file, 1 file left alone", "\nb.txt was not restored."},
			not:   []string{"a.txt was not restored"},
			after: map[string]string{"a.txt": "one\n", "b.txt": "theirs\n"},
		},
	})
}

// The boundaries: what is not the original is still somebody's, and what
// cannot be read, or is not a file, is never taken for it.
func TestAFileNotHoldingItsOriginalBytesIsStillLeft(t *testing.T) {
	one := map[string]string{"a.txt": "one\n"}
	runOriginalCases(t, []originalCase{
		{
			name:  "a modified file rewritten to a third version",
			files: one, patch: modifyOne, args: []string{"--try", "printf 'three\\n' > a.txt"},
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "Something rewrote it after hunk did", "the file is now "},
			after: map[string]string{"a.txt": "three\n"},
		},
		{
			name:  "the original less its final newline is not the original",
			files: one, patch: modifyOne, args: []string{"--try", "printf 'one' > a.txt"},
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "Something rewrote it after hunk did"},
			after: map[string]string{"a.txt": "one"},
		},
		{
			name:  "a deleted file recreated with other bytes",
			files: one, patch: deleteOne, args: []string{"--try", "printf 'theirs\\n' > a.txt"},
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "hunk deleted it and something has since created it there"},
			after: map[string]string{"a.txt": "theirs\n"},
		},
		{
			// It may hold the original, but nothing read it to say so.
			name:  "a deleted file recreated as it was and made unreadable",
			files: one, perms: true, chmodBack: []string{"a.txt"},
			patch: deleteOne, args: []string{"--try", backAsItWas + " && chmod 000 a.txt"},
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "hunk deleted it and something has since created it there"},
			after: map[string]string{"a.txt": "one\n"},
		},
		{
			name:  "a link to the original bytes where a file was deleted is a link",
			files: map[string]string{"a.txt": "one\n", "keep.txt": "one\n"}, links: true,
			patch: deleteOne, args: []string{"--try", "ln -s keep.txt a.txt"},
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "put a symbolic link there"},
			after: map[string]string{"a.txt": "-> keep.txt", "keep.txt": "one\n"},
		},
		{
			name:  "a link to the original bytes where a file was modified is a link",
			files: map[string]string{"a.txt": "one\n", "keep.txt": "one\n"}, links: true,
			patch: modifyOne, args: []string{"--try", "rm a.txt && ln -s keep.txt a.txt"},
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "it is now a symbolic link"},
			after: map[string]string{"a.txt": "-> keep.txt", "keep.txt": "one\n"},
		},
		{
			name:  "a directory where a file was deleted",
			files: one, patch: deleteOne, args: []string{"--try", "mkdir a.txt"},
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "something has since created it there"},
			after: map[string]string{"a.txt": "directory"},
		},
		{
			name:  "a named pipe where a file was deleted is not read",
			files: one, patch: deleteOne, args: []string{"--try", "mkfifo a.txt"}, fifo: "a.txt",
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "something has since created it there"},
			after: map[string]string{"a.txt": "named pipe"},
		},
	})
}

func runOriginalCases(t *testing.T, cases []originalCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.perms {
				needsPOSIXPerms(t)
			}
			if c.links {
				needsShellSymlinks(t)
			}
			if c.fifo != "" {
				needsNamedPipes(t)
			}
			root := cliTree(t, c.files)
			for name, m := range c.modes {
				must(t, os.Chmod(filepath.Join(root, filepath.FromSlash(name)), m))
			}
			giveBack := func() {
				for _, name := range c.chmodBack {
					_ = os.Chmod(filepath.Join(root, filepath.FromSlash(name)), 0o644)
				}
			}
			t.Cleanup(giveBack)
			before := snapshot(t, root)
			var code int
			var out, errOut string
			run := func() { code, out, errOut = runCLI(t, root, c.args, c.patch) }
			if c.fifo != "" {
				withoutHanging(t, openForWriting(filepath.Join(root, c.fifo)), run)
			} else {
				run()
			}
			giveBack()
			report := out + errOut
			if code != c.exit {
				t.Errorf("exit %d, want %d:\n%s", code, c.exit, report)
			}
			for _, s := range c.says {
				if !strings.Contains(report, s) {
					t.Errorf("the report does not say %q:\n%s", s, report)
				}
			}
			for _, s := range c.not {
				if strings.Contains(report, s) {
					t.Errorf("the report says %q, and should not:\n%s", s, report)
				}
			}
			if c.after == nil {
				assertUnchanged(t, root, before)
				return
			}
			got := treeOf(t, root)
			if c.fifo != "" && isPipe(t, filepath.Join(root, c.fifo)) {
				got[c.fifo] = "named pipe"
			}
			for _, d := range treeDiff(c.after, got) {
				t.Errorf("the tree: %s", d)
			}
		})
	}
}

// --each stops at a file not put back, so until #106 a command that wrote a
// file back as it was cost every patch after it.
func TestEachCarriesOnPastAFileWrittenBackAsItWas(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	dir := t.TempDir()
	patches := []string{
		writePatch(t, dir, "01.hunk", modifyOne),
		writePatch(t, dir, "02.hunk", "@@ file a.txt\n@@ old\none\n@@ new\nthree\n"),
	}
	before := snapshot(t, root)
	mark := pidFile(t, "ran")
	// A suite with a --fix: it passes on the original, and under either patch
	// writes the original back and fails.
	suite := "grep -qx one a.txt || { " + backAsItWas + "; exit 1; }"
	code, out, errOut := runCLI(t, root, []string{"--try", counted(mark, suite), "--each", patches[0], patches[1]}, "")
	if code != exitOK {
		t.Fatalf("exit %d, want 0, every patch caught:\n%s%s", code, out, errOut)
	}
	if n := markerLines(t, mark); n != 4 {
		t.Errorf("the command ran %d times, want 4: on the tree as it is, under each patch, and on the tree again after the last (#110)", n)
	}
	for _, s := range []string{"was not restored", "stopped at", "not tried"} {
		if strings.Contains(out+errOut, s) {
			t.Errorf("the report says %q, and should not:\n%s%s", s, out, errOut)
		}
	}
	assertUnchanged(t, root, before)
}

// --json counts it as rolled back and lists nothing not restored.
func TestTheJSONCountsAFileWrittenBackAsItWas(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	code, out, errOut := runCLI(t, root, []string{"--json", "--try", backAsItWas}, modifyOne)
	if code != exitOK {
		t.Fatalf("exit %d, want 0:\n%s%s", code, out, errOut)
	}
	var r struct {
		Exit int `json:"exit"`
		Try  struct {
			RolledBack  int               `json:"rolled_back"`
			NotRestored []json.RawMessage `json:"not_restored"`
		} `json:"try"`
	}
	must(t, json.Unmarshal([]byte(out), &r))
	if r.Exit != exitOK || r.Try.RolledBack != 1 || len(r.Try.NotRestored) != 0 {
		t.Errorf("exit %d, rolled_back %d, not_restored %d; want 0, 1, 0:\n%s",
			r.Exit, r.Try.RolledBack, len(r.Try.NotRestored), out)
	}
}
