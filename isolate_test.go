package main

// --isolated's tests (#130). The live rows drive the real CLI against a real
// tree, and the command is the probe: it reads the tree by its absolute name
// while it runs, and says where it ran, so "the tree never sees a patch" is
// an assertion about what another reader saw at that moment, not only about
// the bytes once the run is over. The temporary directory is pointed at an
// empty one, so that the copy can be found while it exists and shown gone
// after.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// scratchTemp points the temporary directory at a new, empty one, and returns
// it resolved. A t.TempDir made after it is made inside it, which is why
// copiesLeft counts only the copies.
func scratchTemp(t *testing.T) string {
	t.Helper()
	dir := cliTree(t, nil)
	t.Setenv("TMPDIR", dir)
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", dir)
		t.Setenv("TEMP", dir)
	}
	return dir
}

// noTemp points the temporary directory at one that does not exist under
// root, so that a copy tried is exit 5. Windows reads TMP and TEMP, not
// TMPDIR; a test that set TMPDIR alone made its copy there after all.
func noTemp(t *testing.T, root string) {
	t.Helper()
	missing := filepath.Join(root, "no such directory")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, missing)
	}
}

// closeAndRemove closes the copy's tree and then removes it, the order
// invoke and eachRun take: Windows will not remove a directory that a
// handle, an os.Root's included, still holds open.
func closeAndRemove(c *Tree, remove func() error) error {
	_ = c.Close()
	return remove()
}

// copiesLeft is every copy --isolated made in tmp that is still there.
func copiesLeft(t *testing.T, tmp string) []string {
	t.Helper()
	entries, err := os.ReadDir(tmp)
	must(t, err)
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hunk-isolated-") {
			left = append(left, e.Name())
		}
	}
	return left
}

func assertNoCopyLeft(t *testing.T, tmp string) {
	t.Helper()
	if left := copiesLeft(t, tmp); len(left) > 0 {
		t.Errorf("the copy was not removed: %v", left)
	}
}

// theCopy is the one copy in tmp, for a test that reaches into it mid-copy.
func theCopy(t *testing.T, tmp string) string {
	t.Helper()
	left := copiesLeft(t, tmp)
	if len(left) != 1 {
		t.Fatalf("want one copy in %s, found %v", tmp, left)
	}
	return filepath.Join(tmp, left[0])
}

// reCopied is the line --isolated prints once its copy is made (#132), the
// copy's path captured. The figures are a live run's, so the shape is what is
// asserted here and the golden has the words.
var reCopied = regexp.MustCompile(`^copied the tree in \d+\.\ds: (?:1 file|[\d,]+ files), \d+(?:\.\d)? [kMGTPE]?B(?:, (?:all|none|[\d,]+) cloned)?, in (.+)\n`)

// afterCopied is stderr after the copy line, which it has to start with, and
// the path the line names, which has to be a copy under tmp. On Windows the
// path is compared by its last element: the shell the commands run under
// spells the temporary directory its own way.
func afterCopied(t *testing.T, tmp, stderr string) (path, rest string) {
	t.Helper()
	m := reCopied.FindStringSubmatch(stderr)
	switch {
	case m == nil:
		t.Errorf("stderr does not start with the copy line:\n%s", stderr)
		return "", stderr
	case runtime.GOOS == "windows" && !strings.HasPrefix(filepath.Base(m[1]), "hunk-isolated-"),
		runtime.GOOS != "windows" && !strings.HasPrefix(m[1], tmp+string(filepath.Separator)+"hunk-isolated-"):
		t.Errorf("the copy line names %s, want a copy under %s", m[1], tmp)
	}
	return m[1], stderr[len(m[0]):]
}

// sh spells a path for a command.
func sh(path string) string { return "'" + filepath.ToSlash(path) + "'" }

func logLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	must(t, err)
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// A single --try --isolated: the batch is in the copy, which the command runs
// in, while the tree holds what it held; the exit is the command's; nothing
// is left of the copy.
func TestAnIsolatedTryRunsInACopy(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		toOut  bool // the report is on stdout, as a --try that exited 0 has it
	}{
		{"the command exits 3", 3, false},
		{"the command exits 0", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			log := pidFile(t, "log")
			tmp := scratchTemp(t)
			before := snapshot(t, root)
			command := fmt.Sprintf("{ pwd -P; cat %s; cat a.txt; } > %s; exit %d",
				sh(filepath.Join(root, "a.txt")), sh(log), c.status)
			code, out, errOut := runCLI(t, root, []string{"--try", command, "--isolated"}, vPatch)
			if code != c.status {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.status, out, errOut)
			}
			// The copy line first, on stderr whatever the exit (#132).
			path, rest := afterCopied(t, tmp, errOut)
			report, other := rest, out
			if c.toOut {
				report, other = out, rest
			}
			want := fmt.Sprintf("tried 1 hunk in a copy of the tree; the command exited %d (", c.status)
			if !strings.HasPrefix(report, want) || other != "" {
				t.Errorf("stdout %q\nstderr %q\nwant the report to start %q", out, errOut, want)
			}
			got := logLines(t, log)
			if len(got) != 3 || got[1] != "one" || got[2] != "ONE" {
				t.Fatalf("the command saw %q; want the tree's a.txt unpatched and its own patched", got)
			}
			if runtime.GOOS != "windows" && (!strings.HasPrefix(got[0], tmp+string(filepath.Separator)+"hunk-isolated-") || got[0] == root || got[0] != path) {
				t.Errorf("the command ran in %s, want the copy under %s the line named, %s", got[0], tmp, path)
			}
			assertUnchanged(t, root, before)
			assertNoCopyLeft(t, tmp)
		})
	}
}

// --each --isolated: every patch is tried alone in one copy, the build and
// the runs on the tree as it is there too, and at no moment does the tree
// hold a patch.
func TestAnIsolatedEachRunsInACopy(t *testing.T) {
	root, patches := eachFixture(t)
	seen, where, built := pidFile(t, "seen"), pidFile(t, "where"), pidFile(t, "built")
	tmp := scratchTemp(t)
	before := snapshot(t, root)
	command := fmt.Sprintf("cat %s %s %s >> %s; pwd -P >> %s; %s",
		sh(filepath.Join(root, "a.txt")), sh(filepath.Join(root, "b.txt")), sh(filepath.Join(root, "c.txt")),
		sh(seen), sh(where), catchesAll)
	args := append([]string{"--try", command, "--build", "pwd -P >> " + sh(built), "--isolated", "--each"}, patches...)
	code, out, errOut := runCLI(t, root, args, "")
	if code != exitOK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.HasPrefix(out, "tried 3 patches in a copy of the tree; every command exited nonzero (") {
		t.Errorf("stdout %q", out)
	}
	// The copy line before every patch's.
	path, rest := afterCopied(t, tmp, errOut)
	if !strings.HasPrefix(rest, "tried ") || strings.Contains(rest, "copied the tree") {
		t.Errorf("stderr after the copy line: %q", rest)
	}
	// The command on the tree as it is, once per patch, and once after.
	got := logLines(t, seen)
	if len(got) != 15 {
		t.Fatalf("the command ran %d times, want 5: %q", len(got)/3, got)
	}
	for i := 0; i < len(got); i += 3 {
		if got[i] != "alpha" || got[i+1] != "beta" || got[i+2] != "gamma" {
			t.Errorf("run %d saw the tree hold %q", i/3+1, got[i:i+3])
		}
	}
	if runtime.GOOS != "windows" {
		dirs := append(logLines(t, where), logLines(t, built)...)
		if len(dirs) != 10 {
			t.Errorf("want 5 commands and 5 builds, got %q", dirs)
		}
		for _, d := range dirs {
			if d != path || !strings.HasPrefix(d, tmp+string(filepath.Separator)+"hunk-isolated-") {
				t.Errorf("ran in %q; want every command and build in the one copy under %s, the one the line named, %s", dirs, tmp, path)
				break
			}
		}
	}
	assertUnchanged(t, root, before)
	assertNoCopyLeft(t, tmp)
}

// --isolated with a flag it cannot honour, or without the one it needs, is
// refused before anything is read: nothing copied, nothing run.
func TestIsolatedIsRefusedWhereItWouldNotHold(t *testing.T) {
	dir := t.TempDir()
	patch := writePatch(t, dir, "p.hunk", vPatch)
	for _, c := range []struct {
		name string
		args []string
		say  string
	}{
		{
			"without --try",
			[]string{"--isolated"},
			"--isolated runs --try in a copy of the tree and does nothing without --try CMD",
		},
		{
			"with --verify",
			[]string{"--verify", "RUN", "--isolated"},
			"--isolated runs --try in a copy of the tree, and a --verify keeps the batch that passes, in the tree itself",
		},
		{
			"with --allow-outside-root",
			[]string{"--try", "RUN", "--isolated", "--allow-outside-root"},
			"--isolated and --allow-outside-root cannot both be set; a path outside the root is not in the copy, and would be edited where it is",
		},
		{
			"with --verify-may-format",
			[]string{"--try", "RUN", "--isolated", "--verify-may-format"},
			"--isolated and --verify-may-format cannot both be set; nothing but the command writes in the copy, so whatever it wrote there is undone without asking",
		},
		{
			"under --each, with --verify-may-format",
			[]string{"--try", "RUN", "--isolated", "--verify-may-format", "--each", patch},
			"--isolated and --verify-may-format cannot both be set; nothing but the command writes in the copy, so whatever it wrote there is undone without asking",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			mark := pidFile(t, "ran")
			tmp := scratchTemp(t)
			before := snapshot(t, root)
			args := slices.Clone(c.args)
			for i, a := range args {
				if a == "RUN" {
					args[i] = counted(mark, "true")
				}
			}
			code, out, errOut := runCLI(t, root, args, vPatch)
			if code != exitUsage || out != "" || errOut != "hunk: "+c.say+"\n" {
				t.Errorf("exit %d\nstdout %q\nstderr %q\nwant stderr %q", code, out, errOut, "hunk: "+c.say+"\n")
			}
			if n := markerLines(t, mark); n > 0 {
				t.Errorf("the command ran %d times", n)
			}
			assertUnchanged(t, root, before)
			assertNoCopyLeft(t, tmp)
		})
	}
}

// The preflight is the tree's own: a patch that would not apply is refused in
// the same bytes with --isolated as without, before any copy is attempted. The
// temporary directory does not exist, so a copy tried first would be exit 5.
func TestAnIsolatedRunIsRefusedAsTheTreeWouldBe(t *testing.T) {
	const miss = "@@ file a.txt\n@@ old\nnot there\n@@ new\nx\n"
	dir := t.TempDir()
	good := writePatch(t, dir, "01-good.hunk", vPatch)
	bad := writePatch(t, dir, "02-bad.hunk", miss)
	for _, c := range []struct {
		name  string
		args  []string
		patch string
	}{
		{"a --try", []string{"--try", "true"}, miss},
		{"an --each", []string{"--try", "true", "--each", good, bad}, ""},
		{"an --each, in JSON", []string{"--json", "--try", "true", "--each", good, bad}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			plain, plainOut, plainErr := runCLI(t, root, c.args, c.patch)
			noTemp(t, root)
			args := append([]string{"--isolated"}, c.args...)
			code, out, errOut := runCLI(t, root, args, c.patch)
			if code != exitNoMatch || code != plain || out != plainOut || errOut != plainErr {
				t.Errorf("--isolated: exit %d\n%s%s\nwithout it: exit %d\n%s%s", code, out, errOut, plain, plainOut, plainErr)
			}
		})
	}
	t.Run("a dry run copies nothing either", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		noTemp(t, root)
		code, out, errOut := runCLI(t, root, []string{"--dry-run", "--try", "true", "--isolated"}, vPatch)
		if code != exitOK || !strings.Contains(out, "(dry run: nothing written, command not run)") {
			t.Errorf("exit %d\n%s%s", code, out, errOut)
		}
	})
}

// A copy that cannot be made is exit 5 with nothing run, in text and JSON.
func TestAnIsolatedRunWhoseCopyCannotBeMade(t *testing.T) {
	dir := t.TempDir()
	patch := writePatch(t, dir, "p.hunk", vPatch)
	for _, c := range []struct {
		name  string
		args  []string
		patch string
	}{
		{"a --try", []string{"--isolated", "--try", "RUN"}, vPatch},
		{"an --each", []string{"--isolated", "--try", "RUN", "--each", patch}, ""},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, json %v", c.name, asJSON), func(t *testing.T) {
				root := cliTree(t, map[string]string{"a.txt": "one\n"})
				mark := pidFile(t, "ran")
				noTemp(t, root)
				before := snapshot(t, root)
				args := slices.Clone(c.args)
				args[2] = counted(mark, "true")
				if asJSON {
					args = append([]string{"--json"}, args...)
				}
				code, out, errOut := runCLI(t, root, args, c.patch)
				msg := errOut
				if asJSON {
					var r struct {
						Exit     int       `json:"exit"`
						Error    string    `json:"error"`
						Isolated bool      `json:"isolated"`
						Copy     *copyJSON `json:"copy"`
					}
					if err := json.Unmarshal([]byte(out), &r); err != nil {
						t.Fatalf("stdout not JSON: %v\n%s", err, out)
					}
					if r.Exit != exitIO || r.Isolated || r.Copy != nil {
						t.Errorf("json %+v: want exit 5, and no copy to call it isolated or to describe", r)
					}
					msg = "hunk: " + r.Error + "\n"
				}
				if code != exitIO || !strings.HasPrefix(msg, "hunk: could not copy the tree for --isolated: ") ||
					!strings.HasSuffix(msg, "; nothing was run, and the tree is untouched\n") {
					t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
				}
				if n := markerLines(t, mark); n > 0 {
					t.Errorf("the command ran %d times", n)
				}
				assertUnchanged(t, root, before)
			})
		}
	}
}

// --json says the run was in a copy, and a single --try's put-back count is
// none, since nothing was put back; without --isolated the key is absent.
func TestIsolatedInJSON(t *testing.T) {
	type report struct {
		Exit     int       `json:"exit"`
		Isolated bool      `json:"isolated"`
		Copy     *copyJSON `json:"copy"`
		Try      *struct {
			Status     int `json:"status"`
			RolledBack int `json:"rolled_back"`
		} `json:"try"`
		Each *struct {
			Patches []struct{ Status int } `json:"patches"`
		} `json:"each"`
	}
	read := func(t *testing.T, out string) (report, bool) {
		t.Helper()
		var r report
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		return r, strings.Contains(out, `"isolated"`) || strings.Contains(out, `"copy"`)
	}
	t.Run("a --try", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		tmp := scratchTemp(t)
		code, out, _ := runCLI(t, root, []string{"--json", "--isolated", "--try", "grep -q ONE a.txt; exit 7"}, vPatch)
		r, _ := read(t, out)
		if code != 7 || r.Exit != 7 || !r.Isolated || r.Try == nil || r.Try.Status != 7 || r.Try.RolledBack != 0 {
			t.Errorf("exit %d, %s", code, out)
		}
		assertCopyJSON(t, tmp, r.Copy, 1, 4)
		assertNoCopyLeft(t, tmp)
		code, out, _ = runCLI(t, root, []string{"--json", "--try", "exit 7"}, vPatch)
		if _, has := read(t, out); code != 7 || has {
			t.Errorf("without --isolated: exit %d, %s", code, out)
		}
	})
	t.Run("an --each", func(t *testing.T) {
		root, patches := eachFixture(t)
		tmp := scratchTemp(t)
		code, out, errOut := runCLI(t, root, append([]string{"--json", "--isolated", "--try", catchesAll, "--each"}, patches...), "")
		r, _ := read(t, out)
		if code != exitOK || !r.Isolated || r.Each == nil || len(r.Each.Patches) != 3 {
			t.Errorf("exit %d, %s", code, out)
		}
		// The three files the patches are for, and the stderr line, which
		// streams under --json as each patch's does.
		assertCopyJSON(t, tmp, r.Copy, 3, int64(len("alpha\nbeta\ngamma\n")))
		afterCopied(t, tmp, errOut)
		assertNoCopyLeft(t, tmp)
		code, out, _ = runCLI(t, root, append([]string{"--json", "--try", catchesAll, "--each"}, patches...), "")
		if _, has := read(t, out); code != exitOK || has {
			t.Errorf("without --isolated: exit %d, %s", code, out)
		}
	})
}

// A command that rewrites a patched file, as a formatter does, has it put back
// in the copy whatever it wrote, since nothing else writes there, and the run
// goes on. In the tree itself the same command is exit 4, the run stopped and
// the file left as the command left it, which is the contrast.
func TestTheCopyIsPutBackWhateverTheCommandWrote(t *testing.T) {
	formats := "if grep -q ALPHA a.txt; then printf 'formatted\\n' > a.txt; fi; " + catchesAll
	t.Run("in a copy", func(t *testing.T) {
		root, patches := eachFixture(t)
		tmp := scratchTemp(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, append([]string{"--isolated", "--try", formats, "--each"}, patches...), "")
		if code != exitOK || !strings.HasPrefix(out, "tried 3 patches in a copy of the tree; every command exited nonzero (") {
			t.Errorf("exit %d\n%s%s", code, out, errOut)
		}
		assertUnchanged(t, root, before)
		assertNoCopyLeft(t, tmp)
	})
	t.Run("in the tree", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, append([]string{"--try", formats, "--each"}, patches...), "")
		if code != exitRollbackFailed || readFile(t, root, "a.txt") != "formatted\n" {
			t.Errorf("exit %d, a.txt %q\n%s%s", code, readFile(t, root, "a.txt"), out, errOut)
		}
	})
}

// A copy that cannot be put back between patches stops the run, as in the
// tree, but at 5: the file is in the copy, which is removed, and the tree was
// never written, which is what 4 would deny.
func TestACopyNotPutBackStopsTheRunAtFive(t *testing.T) {
	breaks := "if grep -q ALPHA a.txt; then rm a.txt && mkdir a.txt; fi; " + catchesAll
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json %v", asJSON), func(t *testing.T) {
			root, patches := eachFixture(t)
			tmp := scratchTemp(t)
			before := snapshot(t, root)
			args := append([]string{"--isolated", "--try", breaks, "--each"}, patches...)
			if asJSON {
				args = append([]string{"--json"}, args...)
			}
			code, out, errOut := runCLI(t, root, args, "")
			if code != exitIO {
				t.Fatalf("exit %d, want 5\n%s%s", code, out, errOut)
			}
			if asJSON {
				var r struct {
					Exit     int    `json:"exit"`
					Isolated bool   `json:"isolated"`
					Error    string `json:"error"`
				}
				must(t, json.Unmarshal([]byte(out), &r))
				if r.Exit != exitIO || !r.Isolated || r.Error != "stopped at "+patches[0]+": 1 file was not restored in the copy" {
					t.Errorf("json %+v", r)
				}
			} else {
				// The patch's line and its block say where the file is, as
				// the summary does: in the copy, not the tree.
				for _, want := range []string{
					"tried " + patches[0] + "  the command exited ",
					"; 1 file left alone in the copy\n",
					"\na.txt was not restored in the copy.\n",
					"  The copy is removed at the end, and the tree's own file was never written.\n",
				} {
					if !strings.Contains(errOut, want) {
						t.Errorf("stderr %q\nwant it to hold %q", errOut, want)
					}
				}
				if want := fmt.Sprintf("tried 0 of 3 patches; stopped at %s: 1 file was not restored in the copy; 2 patches not tried\n", patches[0]); !strings.HasSuffix(errOut, want) {
					t.Errorf("stderr %q\nwant it to end %q", errOut, want)
				}
			}
			assertUnchanged(t, root, before)
			assertNoCopyLeft(t, tmp)
		})
	}
}

// An absolute path under the root names the same file in the copy, by either
// spelling of the root, and the report names it as the patch wrote it.
func TestAnAbsolutePathReachesTheCopy(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	link := filepath.Join(cliTree(t, nil), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("no symlink to the root: %v", err)
	}
	for _, c := range []struct{ name, rootFlag, path string }{
		{"the root resolved", root, filepath.Join(root, "a.txt")},
		{"the root as given, through a link", link, filepath.Join(link, "a.txt")},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := scratchTemp(t)
			before := snapshot(t, root)
			patch := "@@ file " + c.path + "\n@@ old\none\n@@ new\nONE\n"
			code, out, errOut := runCLI(t, c.rootFlag, []string{"--json", "--isolated", "--try", "grep -q ONE a.txt"}, patch)
			var r struct {
				Exit  int `json:"exit"`
				Files []struct {
					Path string `json:"path"`
				} `json:"files"`
			}
			if err := json.Unmarshal([]byte(out), &r); err != nil {
				t.Fatalf("not JSON: %v\n%s%s", err, out, errOut)
			}
			if code != exitOK || len(r.Files) != 1 || r.Files[0].Path != c.path {
				t.Errorf("exit %d: %s%s", code, out, errOut)
			}
			assertUnchanged(t, root, before)
			assertNoCopyLeft(t, tmp)
		})
	}
	t.Run("under --each", func(t *testing.T) {
		tmp := scratchTemp(t)
		patch := writePatch(t, t.TempDir(), "abs.hunk", "@@ file "+filepath.Join(root, "a.txt")+"\n@@ old\none\n@@ new\nONE\n")
		code, out, errOut := runCLI(t, root, []string{"--isolated", "--try", "grep -q one a.txt", "--each", patch}, "")
		if code != exitOK {
			t.Errorf("exit %d\n%s%s", code, out, errOut)
		}
		assertNoCopyLeft(t, tmp)
	})
}

// The lock: an --isolated run shares the tree while it copies, as a dry run
// does, and lets it go once the copy is made.
func TestAnIsolatedRunLetsTheTreeGo(t *testing.T) {
	needsTreeLock(t)
	t.Run("it shares the tree while it copies", func(t *testing.T) {
		const p = "@@ file f.txt\n@@ old\na\n@@ new\nA\n"
		for _, c := range []struct {
			name      string
			exclusive bool
			code      int
		}{
			{"beside a dry run", false, exitOK},
			{"not beside a run that writes", true, exitChanged},
		} {
			t.Run(c.name, func(t *testing.T) {
				root := cliTree(t, map[string]string{"f.txt": "a\n"})
				_, err := lockAt(t, root, c.exclusive)
				must(t, err)
				if code, out, errOut := runCLI(t, root, []string{"--isolated", "--try", "grep -q A f.txt"}, p); code != c.code {
					t.Errorf("exit %d, want %d\n%s%s", code, c.code, out, errOut)
				}
			})
		}
	})

	// A real hunk process holds its command open in the copy; meanwhile the
	// tree takes an edit, of the very file the holder patched; and the holder
	// ends as it would have, its patch never in the tree.
	patches := t.TempDir()
	x := writePatch(t, patches, "x.hunk", "@@ file f.txt\n@@ old\na\n@@ new\nA\n")
	for _, h := range []struct {
		name  string
		args  func(hold string) []string
		patch string
	}{
		{"a --try", func(hold string) []string { return []string{"--try", hold + "; grep -q A f.txt"} }, "@@ file f.txt\n@@ old\na\n@@ new\nA\n"},
		// The command on the tree as it is holds, before the patch, which is
		// tried against the copy as it was made once the edit has landed.
		{"an --each", func(hold string) []string {
			return []string{"--try", hold + "; ! grep -q A f.txt", "--each", x}
		}, ""},
	} {
		t.Run("the tree is free while "+h.name+" runs in its copy", func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "a\nb\n"})
			tmp := scratchTemp(t)
			started, release := pidFile(t, "started"), pidFile(t, "go")
			hold := fmt.Sprintf(": > %s; while [ ! -e %s ]; do sleep 0.02; done", sh(started), sh(release))
			exe, err := os.Executable()
			must(t, err)
			cmd := exec.Command(exe, append([]string{"--root", root, "--isolated"}, h.args(hold)...)...)
			cmd.Env = childEnv("HUNK_TEST_AS_HUNK=1")
			cmd.Stdin = strings.NewReader(h.patch)
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			must(t, cmd.Start())
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if _, err := os.Stat(filepath.FromSlash(started)); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the holder's command never started:\n%s", out.String())
				}
			}
			if got := readFile(t, root, "f.txt"); got != "a\nb\n" {
				t.Errorf("while the holder runs, f.txt = %q: the tree holds a patch", got)
			}
			if code, o, e := runCLI(t, root, nil, "@@ file f.txt\n@@ old\nb\n@@ new\nB\n"); code != exitOK {
				t.Errorf("an edit while the holder runs: exit %d\n%s%s", code, o, e)
			}
			must(t, os.WriteFile(filepath.FromSlash(release), nil, 0o644))
			if err := cmd.Wait(); err != nil {
				t.Errorf("the holder: %v\n%s", err, out.String())
			}
			if got := readFile(t, root, "f.txt"); got != "a\nB\n" {
				t.Errorf("once the holder ended, f.txt = %q, want the edit and not the holder's patch\n%s", got, out.String())
			}
			assertNoCopyLeft(t, tmp)
		})
	}
}

// A signal: during the copy, while the batch is written into it, and while
// the command runs in it. Each ends hunk by the signal, with the tree as it
// was and the copy gone.
func TestASignalToAnIsolatedRun(t *testing.T) {
	needsSignals(t)
	// signalAt delivers SIGTERM as the handler goes in for the nth time: the
	// first is before the copy, the second (a single --try's) at the commit.
	signalAt := func(t *testing.T, n int) {
		orig := notifyInterrupts
		t.Cleanup(func() { notifyInterrupts = orig })
		calls := 0
		notifyInterrupts = func(c chan<- os.Signal) {
			orig(c)
			if calls++; calls != n {
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
	}
	dir := t.TempDir()
	patch := writePatch(t, dir, "p.hunk", vPatch)
	for _, c := range []struct {
		name  string
		at    int
		args  []string
		patch string
		say   string // all of stderr, or its last line under --each
		raise bool   // invoke is given somewhere to put the signal
	}{
		{
			"during the copy for a --try", 1,
			[]string{"--try", "RUN"},
			vPatch,
			"hunk: interrupted by SIGTERM while copying the tree for --isolated; nothing was run, and the tree is untouched\n", true,
		},
		{
			"during the copy, with nowhere to put the signal", 1,
			[]string{"--try", "RUN"},
			vPatch,
			"hunk: interrupted by SIGTERM while copying the tree for --isolated; nothing was run, and the tree is untouched\n", false,
		},
		{
			"while the batch is written into the copy", 2,
			[]string{"--try", "RUN"},
			vPatch,
			"hunk: interrupted by SIGTERM before the --try command ran; the batch was in a copy of the tree, and the tree is untouched\n", true,
		},
		{
			"during the copy for an --each", 1,
			[]string{"--try", "RUN", "--each", patch},
			"",
			"tried 0 of 1 patch; interrupted by SIGTERM; 1 patch not tried\n", true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			mark := pidFile(t, "ran")
			tmp := scratchTemp(t)
			before := snapshot(t, root)
			signalAt(t, c.at)
			args := append([]string{"--root", root, "--isolated"}, c.args...)
			args[4] = counted(mark, "true")
			var out, errOut bytes.Buffer
			var raise os.Signal
			where := &raise
			if !c.raise {
				where = nil
			}
			code := invoke(args, strings.NewReader(c.patch), &out, &errOut, where)
			if code != 143 || (c.raise && raise != syscall.SIGTERM) {
				t.Errorf("exit %d, raise %v\n%s%s", code, raise, out.String(), errOut.String())
			}
			// A signal at the second handler is after the copy was made, and
			// so after its line; at the first, there is no copy to name.
			got := errOut.String()
			if c.at > 1 {
				_, got = afterCopied(t, tmp, got)
			}
			if !strings.HasSuffix(got, c.say) || (c.patch != "" && got != c.say) {
				t.Errorf("stderr %q\nwant %q", got, c.say)
			}
			if n := markerLines(t, mark); n > 0 {
				t.Errorf("the command ran %d times", n)
			}
			assertUnchanged(t, root, before)
			assertNoCopyLeft(t, tmp)
		})
	}

	t.Run("while the command runs in the copy", func(t *testing.T) {
		withDelays(t, time.Second, 300*time.Millisecond)
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		pid := pidFile(t, "pid")
		tmp := scratchTemp(t)
		before := snapshot(t, root)
		command := fmt.Sprintf("echo $$ > %s; %s", sh(pid), longSleep())
		done := make(chan int, 1)
		var out, errOut bytes.Buffer
		var raise os.Signal
		go func() {
			done <- invoke([]string{"--root", root, "--isolated", "--try", command}, strings.NewReader(vPatch), &out, &errOut, &raise)
		}()
		pidFrom(t, pid)
		signalSelf(t, syscall.SIGTERM)
		select {
		case code := <-done:
			_, rest := afterCopied(t, tmp, errOut.String())
			if code != 143 || raise != syscall.SIGTERM ||
				!strings.HasPrefix(rest, "tried 1 hunk in a copy of the tree; interrupted by SIGTERM, the command was ended (") {
				t.Errorf("exit %d, raise %v\n%s%s", code, raise, out.String(), errOut.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatal("invoke did not return after the signal")
		}
		assertUnchanged(t, root, before)
		assertNoCopyLeft(t, tmp)
	})
}

// A --try --timeout ended in the copy exits 124, as one in the tree does.
func TestAnIsolatedTryThatTimesOut(t *testing.T) {
	withDelays(t, time.Second, 300*time.Millisecond)
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	tmp := scratchTemp(t)
	before := snapshot(t, root)
	code, _, errOut := runCLI(t, root, []string{"--isolated", "--timeout", "300ms", "--try", longSleep()}, vPatch)
	_, rest := afterCopied(t, tmp, errOut)
	if code != exitTimedOut || !strings.HasPrefix(rest, "tried 1 hunk in a copy of the tree; the command timed out after 300ms, exit 124\n") {
		t.Errorf("exit %d: %q", code, errOut)
	}
	assertUnchanged(t, root, before)
	assertNoCopyLeft(t, tmp)
}

// A --try command that cannot start in the copy is exit 5, and says the tree
// is untouched rather than that a batch was put back, since none was.
func TestAnIsolatedTryWhoseCommandCannotStart(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	tmp := scratchTemp(t)
	before := snapshot(t, root)
	t.Setenv("PATH", brokenShell(t))
	code, _, errOut := runCLI(t, root, []string{"--isolated", "--try", "true"}, vPatch)
	_, rest := afterCopied(t, tmp, errOut)
	if code != exitIO || !strings.HasPrefix(rest, "hunk: could not run the --try command: ") ||
		!strings.HasSuffix(rest, "; it was to run in a copy of the tree, and the tree is untouched\n") {
		t.Errorf("exit %d: %q", code, errOut)
	}
	assertUnchanged(t, root, before)
	assertNoCopyLeft(t, tmp)
}

// The reports, rendered from constructed outcomes, since a live run's seconds
// are nobody's contract. §8.1: a reworded line is a contract change.
func TestIsolatedReportsRead(t *testing.T) {
	for _, c := range []struct {
		golden string
		v      *Verify
		exit   int
	}{
		{"report-try-isolated", &Verify{
			Ran: true, Try: true, Isolated: true, Applied: 2, Status: 1, Seconds: 2.0, Command: "zig build test",
			TotalLines: 1, Tail: []string{"error: 'faults.test.a shadow ray stops at the first hit' failed"},
		}, 1},
		{"report-try-isolated-timed-out", &Verify{
			Ran: true, Try: true, Isolated: true, Applied: 1, Status: exitTimedOut, TimedOut: 90 * time.Second, Command: "zig build test",
		}, exitTimedOut},
		{"report-try-isolated-interrupted", &Verify{
			Ran: true, Try: true, Isolated: true, Applied: 1, Status: 143, Signal: syscall.SIGTERM, Seconds: 0.4, Command: "zig build test",
		}, 143},
	} {
		t.Run(c.golden, func(t *testing.T) {
			rep := NewReport(&Result{Hunks: c.v.Applied}, nil, c.v, false, c.v.Applied)
			var out, errOut bytes.Buffer
			rep.Text(&out, &errOut, false)
			if rep.Exit != c.exit || out.Len() > 0 {
				t.Errorf("exit %d, want %d; stdout %q", rep.Exit, c.exit, out.String())
			}
			golden(t, c.golden, errOut.String())
		})
	}

	caught := func(name string) *eachPatch {
		return &eachPatch{name: name, v: &Verify{Ran: true, Try: true, Isolated: true, Applied: 1, Status: 1, Seconds: 41.2, Command: "zig build test"}}
	}
	for _, c := range []struct {
		golden  string
		patches []*eachPatch
		o       eachOutcome
		exit    int
	}{
		{
			"report-each-isolated",
			[]*eachPatch{caught("breaks/01-shadow-reach.hunk"), caught("breaks/02-texture.hunk")},
			eachOutcome{tried: 2, stopAt: -1, seconds: 83.1, inCopy: true},
			exitOK,
		},
		{"report-each-isolated-stopped", []*eachPatch{
			{name: "breaks/01-shadow-reach.hunk", v: &Verify{
				Ran: true, Try: true, Isolated: true, Applied: 1, Status: 1, Seconds: 41.2, Command: "zig build test",
				NotRestored: []NotRestored{{Path: "src/reach.zig", Reason: "hunk wrote 1a2b3c4d there, and it is now a directory, which hunk did not make,\nso restoring would replace it with a file."}},
			}},
			{name: "breaks/02-texture.hunk"},
		}, eachOutcome{tried: 0, stopAt: 0, seconds: 41.3, inCopy: true}, exitIO},
	} {
		t.Run(c.golden, func(t *testing.T) {
			var out, errOut bytes.Buffer
			width := eachNameWidth(c.patches)
			for _, ep := range c.patches {
				if ep.v != nil {
					writeEachLine(eachConfig{}, &errOut, width, ep)
				}
			}
			code := eachExit(c.patches, c.o)
			writeEachEnd(eachConfig{}, c.patches, c.o, code, &out, &errOut)
			if code != c.exit {
				t.Errorf("exit %d, want %d", code, c.exit)
			}
			golden(t, c.golden, errOut.String()+out.String())
		})
	}
}

// What a copy holds: every file's bytes, mode and time, every directory's,
// links as written except one to an absolute name under the root, and nothing
// that is not a file, a directory or a link. Against a real filesystem.
func TestTheCopyIsTheTree(t *testing.T) {
	needsPOSIXPerms(t)
	outside := cliTree(t, map[string]string{"o.txt": "outside\n"})
	root := cliTree(t, map[string]string{
		"f.txt": "f\n", "x.sh": "#!/bin/sh\n", "secret": "s\n", "suid": "u\n",
		"sub/deep/d.txt": "d\n", "ro/r.txt": "r\n", ".git/HEAD": "ref: refs/heads/main\n",
	})
	given := filepath.Join(cliTree(t, nil), "given")
	must(t, os.Symlink(root, given))
	must(t, os.Mkdir(filepath.Join(root, "empty"), 0o755))
	links := map[string]string{
		"rel":      "sub/deep/d.txt",
		"absin":    filepath.Join(root, "f.txt"),
		"absgiven": filepath.Join(given, "sub", "deep", "d.txt"),
		"absroot":  root,
		"absout":   filepath.Join(outside, "o.txt"),
		"dirout":   outside,
		"dangling": "nothing here",
	}
	for name, dest := range links {
		must(t, os.Symlink(dest, filepath.Join(root, name)))
	}
	makeFIFO(t, filepath.Join(root, "fifo"))
	t.Chdir(root)
	l, err := net.Listen("unix", "sock")
	must(t, err)
	t.Cleanup(func() { _ = l.Close() })
	modes := map[string]fs.FileMode{
		"x.sh": 0o755, "secret": 0o600, "suid": 0o4755, "ro": 0o555, "empty": 0o700, "sub/deep": 0o750,
	}
	for name, mode := range modes {
		must(t, os.Chmod(filepath.Join(root, name), mode))
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "ro"), 0o755) })
	when := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	var names []string
	must(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		names = append(names, p)
		return err
	}))
	for i := len(names) - 1; i >= 0; i-- {
		if fi, err := os.Lstat(names[i]); err == nil && (fi.Mode().IsRegular() || fi.IsDir()) {
			must(t, os.Chtimes(names[i], time.Time{}, when))
		}
	}

	tmp := scratchTemp(t)
	tree, err := OpenTree(given, false)
	must(t, err)
	t.Cleanup(func() { _ = tree.Close() })
	c, made, remove, err := isolate(tree, func() os.Signal { return nil })
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	dst := c.Root()
	// Seven regular files and 41 bytes; links, directories, the pipe and the
	// socket are not files. Where hunk clones, a clone each: the temporary
	// directory is on the tree's volume, the setuid file and the one in a
	// 0555 directory among them.
	want := copyMade{Dir: dst, Files: 7, Bytes: 41, Clones: clones, Seconds: made.Seconds}
	if clones {
		want.Cloned = 7
	}
	if made != want || made.Seconds <= 0 {
		t.Errorf("made %+v, want %+v", made, want)
	}

	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the copy's own directory: %v, %v; want 0700 whatever the root's", fi.Mode(), err)
	}
	var wantLines, got []string
	describe := func(base string, into *[]string) {
		must(t, filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			must(t, err)
			rel, _ := filepath.Rel(base, p)
			if rel == "." {
				return nil
			}
			fi, err := os.Lstat(p)
			must(t, err)
			line := fmt.Sprintf("%s %v", rel, fi.Mode()&(keptMode|fs.ModeType))
			switch {
			case fi.Mode()&fs.ModeSymlink != 0:
				dest, err := os.Readlink(p)
				must(t, err)
				line += " -> " + strings.Replace(dest, dst, "<copy>", 1)
			case fi.Mode().IsRegular():
				b, err := os.ReadFile(p)
				must(t, err)
				line += fmt.Sprintf(" %q %s", b, fi.ModTime().UTC().Format(time.RFC3339Nano))
			case fi.IsDir():
				line += " " + fi.ModTime().UTC().Format(time.RFC3339Nano)
			}
			*into = append(*into, line)
			return nil
		}))
	}
	describe(root, &wantLines)
	describe(dst, &got)
	// The source as the copy should hold it: no pipe, no socket, and the two
	// links to absolute names under the root pointed into the copy.
	wantLines = slices.DeleteFunc(wantLines, func(s string) bool {
		return strings.HasPrefix(s, "fifo ") || strings.HasPrefix(s, "sock ")
	})
	for i, s := range wantLines {
		for _, name := range []string{"absin", "absgiven", "absroot"} {
			if strings.HasPrefix(s, name+" ") {
				rest, _ := strings.CutPrefix(links[name], map[bool]string{true: given, false: root}[name == "absgiven"])
				wantLines[i] = s[:strings.Index(s, " -> ")+4] + "<copy>" + rest
			}
		}
	}
	if !slices.Equal(got, wantLines) {
		t.Errorf("the copy holds\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantLines, "\n"))
	}
	if b, err := os.ReadFile(filepath.Join(dst, "absgiven")); err != nil || string(b) != "d\n" {
		t.Errorf("absgiven in the copy reads %q, %v", b, err)
	}

	must(t, closeAndRemove(c, remove))
	assertNoCopyLeft(t, tmp)
}

// What ends a copy, and what is left out of one: every failure is exit 5 with
// nothing left of the copy, a signal is its own, and anything that went
// between being listed and being read is skipped.
func TestWhatEndsACopy(t *testing.T) {
	needsPOSIXPerms(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	for _, c := range []struct {
		name  string
		files map[string]string
		setup func(t *testing.T, root string)
		// before, given the copy and the source, runs as the walk asks about
		// its nth entry, counting the root as the first.
		n      int
		before func(t *testing.T, dst, root string) os.Signal
		err    string // a substring of the copyError, "" for a copy that succeeds
		sig    bool   // the copy is interrupted
		left   []string
	}{
		{
			name: "an unreadable file", files: map[string]string{"a.txt": "a\n"},
			setup: func(t *testing.T, root string) { must(t, os.Chmod(filepath.Join(root, "a.txt"), 0)) },
			err:   "permission denied",
		},
		{
			name: "an unreadable directory", files: map[string]string{"d/a.txt": "a\n"},
			setup: func(t *testing.T, root string) {
				must(t, os.Chmod(filepath.Join(root, "d"), 0))
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "d"), 0o755) })
			},
			err: "permission denied",
		},
		{
			name: "a directory that cannot be made", files: map[string]string{"d/a.txt": "a\n"},
			n: 2, before: readOnly, err: "permission denied",
		},
		{
			name: "a file that cannot be made", files: map[string]string{"a.txt": "a\n"},
			n: 2, before: readOnly, err: "permission denied",
		},
		{
			name: "a link that cannot be made", files: map[string]string{},
			setup: func(t *testing.T, root string) { must(t, os.Symlink("x", filepath.Join(root, "l"))) },
			n:     2, before: readOnly, err: "permission denied",
		},
		{
			name: "a copy whose directory goes", files: map[string]string{},
			n: 1, before: func(t *testing.T, dst, root string) os.Signal { must(t, os.Remove(dst)); return nil },
			err: "no such file or directory",
		},
		{
			name: "a signal", files: map[string]string{"a.txt": "a\n", "b.txt": "b\n"},
			n: 2, before: func(t *testing.T, dst, root string) os.Signal { return syscall.SIGINT }, sig: true,
		},
		{
			name: "a file that goes", files: map[string]string{"a.txt": "a\n", "b.txt": "b\n"},
			n: 2, before: removeSource("a.txt"), left: []string{"b.txt"},
		},
		{
			name: "a directory that goes", files: map[string]string{"a/x.txt": "x\n", "b.txt": "b\n"},
			n: 2, before: removeSource("a"), left: []string{"b.txt"},
		},
		{
			name: "a link that goes", files: map[string]string{"b.txt": "b\n"},
			setup: func(t *testing.T, root string) { must(t, os.Symlink("b.txt", filepath.Join(root, "a"))) },
			n:     2, before: removeSource("a"), left: []string{"b.txt"},
		},
		{
			name: "a file that is a pipe by the time it is read", files: map[string]string{"a.txt": "a\n", "b.txt": "b\n"},
			n: 2, before: func(t *testing.T, dst, root string) os.Signal {
				must(t, os.Remove(filepath.Join(root, "a.txt")))
				makeFIFO(t, filepath.Join(root, "a.txt"))
				return nil
			}, left: []string{"b.txt"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			if c.setup != nil {
				c.setup(t, root)
			}
			tmp := scratchTemp(t)
			tree, err := OpenTree(root, false)
			must(t, err)
			t.Cleanup(func() { _ = tree.Close() })
			asked := 0
			cp, _, remove, err := isolate(tree, func() os.Signal {
				if asked++; asked == c.n && c.before != nil {
					return c.before(t, theCopy(t, tmp), root)
				}
				return nil
			})
			var ce *copyError
			var ci *copyInterrupted
			switch {
			case c.sig:
				if !errors.As(err, &ci) || ci.sig != syscall.SIGINT || ExitCode(err) != exitIO {
					t.Errorf("err = %v", err)
				}
			case c.err != "":
				if !errors.As(err, &ce) || !strings.Contains(err.Error(), c.err) || ExitCode(err) != exitIO {
					t.Errorf("err = %v, want a copyError saying %q", err, c.err)
				}
			default:
				must(t, err)
				t.Cleanup(func() { _ = cp.Close() })
				entries, err := os.ReadDir(cp.Root())
				must(t, err)
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				if !slices.Equal(names, c.left) {
					t.Errorf("the copy holds %q, want %q", names, c.left)
				}
				must(t, closeAndRemove(cp, remove))
			}
			assertNoCopyLeft(t, tmp)
		})
	}
}

// readOnly takes the write permission off the copy's own directory, so that
// the next entry cannot be made in it.
func readOnly(t *testing.T, dst, _ string) os.Signal {
	must(t, os.Chmod(dst, 0o500))
	return nil
}

// removeSource takes name out of the tree just before the copy reaches it.
func removeSource(name string) func(t *testing.T, dst, root string) os.Signal {
	return func(t *testing.T, _, root string) os.Signal {
		must(t, os.RemoveAll(filepath.Join(root, name)))
		return nil
	}
}

// A temporary directory inside the root is not copied into itself, which
// would go on until the disk was full: the copy holds the rest, and the
// directory the copy is in, empty.
func TestACopyIsNotCopiedIntoItself(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "a\n", "tmp/other.txt": "o\n"})
	tmp := filepath.Join(root, "tmp")
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	tree, err := OpenTree(root, false)
	must(t, err)
	t.Cleanup(func() { _ = tree.Close() })
	c, _, remove, err := isolate(tree, func() os.Signal { return nil })
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	var got []string
	must(t, filepath.WalkDir(c.Root(), func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(c.Root(), p)
		got = append(got, filepath.ToSlash(rel))
		return err
	}))
	if want := []string{".", "a.txt", "tmp", "tmp/other.txt"}; !slices.Equal(got, want) {
		t.Errorf("the copy holds %q, want %q", got, want)
	}
	must(t, closeAndRemove(c, remove))
	assertNoCopyLeft(t, tmp)
}

// A copy answers for the tree it was made from: it names that tree's root in
// a refusal, and an absolute path under that root, by either spelling, is the
// same name under the copy, still written as the patch wrote it.
func TestACopyAnswersForItsTree(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "a\n", "sub/b.txt": "b\n"})
	given := filepath.Join(cliTree(t, nil), "given")
	if err := os.Symlink(root, given); err != nil {
		t.Skipf("no symlink to the root: %v", err)
	}
	scratchTemp(t)
	tree, err := OpenTree(given, false)
	must(t, err)
	t.Cleanup(func() { _ = tree.Close() })
	c, _, remove, err := isolate(tree, func() os.Signal { return nil })
	must(t, err)
	t.Cleanup(func() { _ = c.Close(); _ = remove() })

	if c.Shown() != tree.Shown() || c.Root() == tree.Root() {
		t.Errorf("the copy shows %q at %q; the tree shows %q at %q", c.Shown(), c.Root(), tree.Shown(), tree.Root())
	}
	for _, p := range []string{
		filepath.Join(root, "sub", "b.txt"),
		filepath.Join(given, "sub", "b.txt"),
		filepath.Join("sub", "b.txt"),
	} {
		tg, err := c.Resolve(p)
		if err != nil || tg.name != filepath.Join("sub", "b.txt") || tg.Orig() != p {
			t.Errorf("Resolve(%q) = %+v, %v", p, tg, err)
		}
	}
	for _, c2 := range []struct{ path, reason string }{
		{filepath.Join(filepath.Dir(root), "elsewhere.txt"), "an absolute path is allowed only under the root"},
		{filepath.Join(root, ".git", "config"), "it is .git or inside it"},
	} {
		_, err := c.Resolve(c2.path)
		var pr *PathRefusal
		if !errors.As(err, &pr) || pr.Path != c2.path || pr.Root != tree.Shown() || !strings.Contains(pr.Reason, c2.reason) {
			t.Errorf("Resolve(%q): %v; want a refusal of the path as written, naming %q", c2.path, err, tree.Shown())
		}
	}
}

// A copy the command left hard to remove, as Go leaves its module cache, is
// removed all the same; and one that cannot be is a warning, not an exit.
func TestTheCopyIsRemoved(t *testing.T) {
	needsPOSIXPerms(t)
	dir := cliTree(t, map[string]string{"ro/a.txt": "a\n", "none/b.txt": "b\n", "c.txt": "c\n"})
	must(t, os.Chmod(filepath.Join(dir, "ro"), 0o555))
	must(t, os.Chmod(filepath.Join(dir, "none"), 0))
	if err := removeCopy(dir); err != nil {
		t.Fatalf("removeCopy: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("still there: %v", err)
	}

	var w bytes.Buffer
	removeOrWarn(func() error { return &fs.PathError{Op: "unlinkat", Path: "/tmp/hunk-isolated-1/x", Err: syscall.EPERM} }, &w)
	removeOrWarn(func() error { return nil }, &w)
	if want := "warning: the copy of the tree --isolated made could not all be removed: unlinkat /tmp/hunk-isolated-1/x: operation not permitted\n"; w.String() != want {
		t.Errorf("warning %q, want %q", w.String(), want)
	}
}

// intoCopy is fed link destinations, which are whatever the tree holds. It is
// total, and what it returns is the destination as written or a name under
// the copy.
func FuzzIntoCopy(f *testing.F) {
	for _, s := range []string{"", "a", "/", "/r", "/r/a", "/r/../x", "/g/a/../../b", "/rx", "../r/a", "/r/", "//r/a", "/r/./a"} {
		f.Add(s)
	}
	tree := &Tree{root: "/r", given: "/g"}
	f.Fuzz(func(t *testing.T, dest string) {
		got := intoCopy(tree, "/copy", dest)
		if got != dest && got != "/copy" && !strings.HasPrefix(got, "/copy"+string(filepath.Separator)) {
			t.Errorf("intoCopy(%q) = %q", dest, got)
		}
	})
}

// copyJSON is --json's "copy" as a caller reads it, cloned absent where hunk
// makes no clones.
type copyJSON struct {
	Path    string  `json:"path"`
	Files   int     `json:"files"`
	Bytes   int64   `json:"bytes"`
	Cloned  *int    `json:"cloned"`
	Seconds float64 `json:"seconds"`
}

// assertCopyJSON is a "copy" that names a copy under tmp holding files files
// of bytes bytes, every one cloned where hunk clones and cloned absent where
// it does not.
func assertCopyJSON(t *testing.T, tmp string, c *copyJSON, files int, bytes int64) {
	t.Helper()
	switch {
	case c == nil:
		t.Error(`no "copy" in the report`)
	case c.Files != files || c.Bytes != bytes || c.Seconds <= 0:
		t.Errorf("copy %+v, want %d files of %d bytes, and the time it took", c, files, bytes)
	case !strings.HasPrefix(filepath.Base(c.Path), "hunk-isolated-") ||
		runtime.GOOS != "windows" && filepath.Dir(c.Path) != tmp:
		t.Errorf("copy at %s, want one under %s", c.Path, tmp)
	case clones && (c.Cloned == nil || *c.Cloned != files):
		t.Errorf("cloned %v, want %d", c.Cloned, files)
	case !clones && c.Cloned != nil:
		t.Errorf("cloned %d on a system that makes no clones", *c.Cloned)
	}
}

// The line an --isolated run prints once its copy is made (#132), in every
// shape it takes: each file cloned, none, some, a system that makes no clones,
// one file, an empty tree, and a temporary directory with a line break in its
// name. §8.1: a reworded line is a contract change.
func TestTheCopyLineReads(t *testing.T) {
	const dir = "/tmp/hunk-isolated-2917155247"
	var w bytes.Buffer
	for _, m := range []copyMade{
		{Dir: dir, Files: 34345, Bytes: 61_234_567_890, Cloned: 34345, Clones: true, Seconds: 9.14},
		{Dir: dir, Files: 34345, Bytes: 61_234_567_890, Clones: true, Seconds: 41.04},
		{Dir: dir, Files: 34345, Bytes: 61_234_567_890, Cloned: 30001, Clones: true, Seconds: 12.3},
		{Dir: dir, Files: 34345, Bytes: 61_234_567_890, Seconds: 41.04},
		{Dir: dir, Files: 1, Bytes: 4, Cloned: 1, Clones: true, Seconds: 0.01},
		{Dir: dir, Clones: true},
		{Dir: "/tmp/odd\nname/hunk-isolated-1", Files: 2, Bytes: 1500, Cloned: 2, Clones: true, Seconds: 0.2},
	} {
		writeCopied(&w, m)
	}
	golden(t, "isolated-copied", w.String())
}

// A size and a count as the copy line prints them, at each edge: the units
// change where a size would print as 1000.0, and the thousands are marked
// from the right.
func TestSizesAndCountsRead(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1.0 kB"},
		{1500, "1.5 kB"},
		{999_949, "999.9 kB"},
		{999_950, "1.0 MB"},
		{61_234_567_890, "61.2 GB"},
		{999_949_000_000, "999.9 GB"},
		{999_951_000_000, "1.0 TB"},
		{2_500_000_000_000_000, "2.5 PB"},
		{math.MaxInt64, "9.2 EB"},
	} {
		if got := byteSize(c.n); got != c.want {
			t.Errorf("byteSize(%d) = %q, want %q", c.n, got, c.want)
		}
	}
	for _, c := range []struct {
		n    int
		want string
	}{
		{0, "0"}, {7, "7"}, {999, "999"}, {1000, "1,000"}, {34345, "34,345"}, {100000, "100,000"}, {1234567, "1,234,567"},
	} {
		if got := grouped(c.n); got != c.want {
			t.Errorf("grouped(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// --quiet leaves the copy line out with the rest of a clean run's report: a
// --try that passed, and an --each whose every patch was caught, print
// nothing.
func TestQuietLeavesTheCopyLineOut(t *testing.T) {
	root, patches := eachFixture(t)
	tmp := scratchTemp(t)
	for _, args := range [][]string{
		{"--quiet", "--isolated", "--try", "true", "-f", patches[0]},
		append([]string{"--quiet", "--isolated", "--try", catchesAll, "--each"}, patches...),
	} {
		code, out, errOut := runCLI(t, root, args, "")
		if code != exitOK || out != "" || errOut != "" {
			t.Errorf("%q: exit %d\nstdout %q\nstderr %q", args, code, out, errOut)
		}
	}
	assertNoCopyLeft(t, tmp)
}

// A clone is taken only when it is the file (#132). One that fails, one that
// made nothing, one of the wrong size, a directory, and a link to the tree's
// own file, which a Stat would have passed and which would have sent the
// command's writes into the tree, are each taken away and the file copied
// instead. One that is the file counts as a clone. A wrong thing that cannot
// be taken away ends the copy at exit 5 rather than staying in it.
func TestACloneIsTakenOnlyWhenItIsTheFile(t *testing.T) {
	const body = "abc\n"
	for _, c := range []struct {
		name   string
		clone  func(root string) func(*os.File, string) error
		cloned int
		err    bool
	}{
		{"fails", func(string) func(*os.File, string) error {
			return func(*os.File, string) error { return syscall.ENOTSUP }
		}, 0, false},
		{"makes nothing", func(string) func(*os.File, string) error {
			return func(*os.File, string) error { return nil }
		}, 0, false},
		{"makes the wrong size", func(string) func(*os.File, string) error {
			return func(_ *os.File, to string) error { return os.WriteFile(to, []byte("x"), 0o644) }
		}, 0, false},
		{"makes a directory", func(string) func(*os.File, string) error {
			return func(_ *os.File, to string) error { return os.Mkdir(to, 0o755) }
		}, 0, false},
		{"makes a link to the tree's file", func(root string) func(*os.File, string) error {
			return func(_ *os.File, to string) error { return os.Symlink(filepath.Join(root, "a.txt"), to) }
		}, 0, false},
		// A link's size is its destination's length, so this one is the
		// file's size, and only its not being a regular file refuses it.
		{"makes a link the file's size", func(string) func(*os.File, string) error {
			return func(_ *os.File, to string) error { return os.Symlink("wxyz", to) }
		}, 0, false},
		{"makes the file", func(string) func(*os.File, string) error {
			return func(in *os.File, to string) error {
				b, err := io.ReadAll(io.NewSectionReader(in, 0, 1<<20))
				if err != nil {
					return err
				}
				return os.WriteFile(to, b, 0o644)
			}
		}, 1, false},
		{"makes what cannot be taken away", func(string) func(*os.File, string) error {
			return func(_ *os.File, to string) error { return os.MkdirAll(filepath.Join(to, "x"), 0o755) }
		}, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": body})
			tmp := scratchTemp(t)
			orig := cloneFile
			cloneFile = c.clone(root)
			t.Cleanup(func() { cloneFile = orig })
			tree, err := OpenTree(root, false)
			must(t, err)
			t.Cleanup(func() { _ = tree.Close() })
			cp, made, remove, err := isolate(tree, func() os.Signal { return nil })
			if c.err {
				if !errors.Is(err, fs.ErrExist) || ExitCode(err) != exitIO {
					t.Errorf("err = %v, want exit 5 for the file that exists", err)
				}
				assertNoCopyLeft(t, tmp)
				return
			}
			must(t, err)
			t.Cleanup(func() { _ = closeAndRemove(cp, remove) })
			fi, err := os.Lstat(filepath.Join(cp.Root(), "a.txt"))
			must(t, err)
			b, err := os.ReadFile(filepath.Join(cp.Root(), "a.txt"))
			must(t, err)
			if !fi.Mode().IsRegular() || string(b) != body {
				t.Errorf("the copy holds %v %q, want a file of %q", fi.Mode(), b, body)
			}
			if made.Files != 1 || made.Bytes != int64(len(body)) || made.Cloned != c.cloned {
				t.Errorf("made %+v, want 1 file of %d bytes, %d cloned", made, len(body), c.cloned)
			}
		})
	}
}

// --json's copy says cloned only where hunk clones: absent is "this system
// makes no clones", which is not the same as making none, and no copy is no
// key at all.
func TestTheCopyInJSON(t *testing.T) {
	for _, c := range []struct {
		m    *copyMade
		want string
	}{
		{nil, "null"},
		{&copyMade{Dir: "/t/c", Files: 2, Bytes: 9, Seconds: 0.5}, `{"path":"/t/c","files":2,"bytes":9,"seconds":0.5}`},
		{&copyMade{Dir: "/t/c", Files: 2, Bytes: 9, Clones: true, Seconds: 0.5}, `{"path":"/t/c","files":2,"bytes":9,"cloned":0,"seconds":0.5}`},
		{&copyMade{Dir: "/t/c", Files: 2, Bytes: 9, Cloned: 2, Clones: true, Seconds: 0.5}, `{"path":"/t/c","files":2,"bytes":9,"cloned":2,"seconds":0.5}`},
	} {
		b, err := json.Marshal(jsonCopyOf(c.m))
		if must(t, err); string(b) != c.want {
			t.Errorf("%+v: %s, want %s", c.m, b, c.want)
		}
	}
}

// A write to the copy, clone or not, is the copy's alone: the tree's file
// keeps its bytes. Where hunk clones, this is the copy-on-write the issue
// counted on, against the real filesystem, with a file big enough to span
// many blocks.
func TestAWriteToTheCopyLeavesTheTreeAlone(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", 1<<16)
	root := cliTree(t, map[string]string{"big.bin": big})
	scratchTemp(t)
	tree, err := OpenTree(root, false)
	must(t, err)
	t.Cleanup(func() { _ = tree.Close() })
	cp, made, remove, err := isolate(tree, func() os.Signal { return nil })
	must(t, err)
	t.Cleanup(func() { _ = closeAndRemove(cp, remove) })
	if want := map[bool]int{true: 1, false: 0}[clones]; made.Cloned != want {
		t.Errorf("cloned %d, want %d", made.Cloned, want)
	}
	f, err := os.OpenFile(filepath.Join(cp.Root(), "big.bin"), os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteAt([]byte("WRITTEN IN THE COPY"), int64(len(big)/2))
	must(t, errors.Join(err, f.Close()))
	if b, err := os.ReadFile(filepath.Join(root, "big.bin")); err != nil || string(b) != big {
		t.Errorf("the tree's file changed with the copy's: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(cp.Root(), "big.bin")); err != nil || !strings.Contains(string(b), "WRITTEN IN THE COPY") {
		t.Errorf("the copy's file does not hold the write: %v", err)
	}
}
