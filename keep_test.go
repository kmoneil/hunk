package main

// A patch from stdin that --verify applied and rolled back is kept in a file in
// the temporary directory, and the report's last line names it (#195). The
// rows here are each outcome that keeps one, each that does not, and each way
// keeping can fail. runCLI checks every kept file the rest of the suite makes.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	keptLine = regexp.MustCompile(`the patch is kept in (.+), to change and resend with -f\n`)
	keptJSON = regexp.MustCompile(`"kept_patch": ("(?:[^"\\]|\\.)*")`)
	keptName = regexp.MustCompile(`^hunk-patch-\d+\.hunk$`)
)

// keptPlaceholder stands for a kept patch's path in what runCLI returns.
const keptPlaceholder = "$TMPDIR/hunk-patch-N.hunk"

// hideKept checks every patch a report says it kept, and puts keptPlaceholder
// where its name was, which is random, so that a golden or a comparison of two
// runs can hold the line. Each must be a file in the temporary directory,
// named as keepPatch names one, holding exactly what was on stdin.
func hideKept(t *testing.T, stdin, s string) string {
	t.Helper()
	s = keptLine.ReplaceAllStringFunc(s, func(m string) string {
		checkKept(t, keptLine.FindStringSubmatch(m)[1], stdin)
		return "the patch is kept in " + keptPlaceholder + ", to change and resend with -f\n"
	})
	return keptJSON.ReplaceAllStringFunc(s, func(m string) string {
		var path string
		if err := json.Unmarshal([]byte(keptJSON.FindStringSubmatch(m)[1]), &path); err != nil {
			t.Errorf("kept_patch is not a JSON string: %s: %v", m, err)
			return m
		}
		checkKept(t, path, stdin)
		return `"kept_patch": "` + keptPlaceholder + `"`
	})
}

// checkKept is a kept patch as it has to be: in the temporary directory, named
// hunk-patch-N.hunk, its owner's alone where there are modes, and the patch's
// bytes exactly.
func checkKept(t *testing.T, path, patch string) {
	t.Helper()
	if filepath.Dir(path) != filepath.Clean(os.TempDir()) || !keptName.MatchString(filepath.Base(path)) {
		t.Errorf("kept in %s, which is not a hunk-patch-N.hunk in %s", path, os.TempDir())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Errorf("the report says the patch is kept in %s: %v", path, err)
		return
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("%s is %04o, want 0600: a patch can carry what the tree does not show", path, info.Mode().Perm())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != patch {
		t.Errorf("%s holds %q (%v), and stdin was %q", path, got, err, patch)
	}
}

// keepCLI is runCLI with the kept path as printed rather than hidden.
func keepCLI(t *testing.T, root string, args []string, stdin string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli(append([]string{"--root", root}, args...), strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

// ownTempDir points the temporary directory at a new empty one for the rest
// of the test, and returns it. The tree is made first, since t.TempDir makes
// its directories in whatever the temporary directory is at its first call.
func ownTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tmp")
	must(t, os.Mkdir(dir, 0o700))
	setTempDir(t, dir)
	return dir
}

// setTempDir is the temporary directory as each platform's os.TempDir reads
// it: TMPDIR, and on Windows TMP, then TEMP.
func setTempDir(t *testing.T, dir string) {
	t.Helper()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, dir)
	}
}

// Every outcome that keeps the patch: --verify applied it, and it did not stay.
// The line is the report's last, under --quiet too, and --json carries the path
// with nothing said on stderr.
func TestAPatchFromStdinIsKept(t *testing.T) {
	for _, c := range []struct {
		name  string
		args  []string
		patch string
		exit  int
		path  func(t *testing.T) string // PATH, where the row needs one
		json  bool
		after string // a.txt afterwards
	}{
		{name: "a verify that fails", args: []string{"--verify", "false"}, patch: vPatch, exit: exitVerifyFailed},
		{name: "-f -, which is stdin", args: []string{"-f", "-", "--verify", "false"}, patch: vPatch, exit: exitVerifyFailed},
		{name: "--quiet, since a failure is never silent", args: []string{"--quiet", "--verify", "false"}, patch: vPatch, exit: exitVerifyFailed},
		{
			name: "a verify --timeout ended", args: []string{"--timeout", "300ms", "--verify", "exec " + longSleep()},
			patch: vPatch, exit: exitVerifyFailed,
		},
		{
			// The verify rewrote a.txt after hunk did, so it is left: exit 4.
			name: "a rollback that could not finish", args: []string{"--verify", "printf 'x\\n' > a.txt; false"},
			patch: vPatch, exit: exitRollbackFailed, after: "x\n",
		},
		{
			name: "a verify that could not start", args: []string{"--verify", "true"},
			patch: vPatch, exit: exitIO, path: brokenShell,
		},
		{
			name: "every byte as it came: CRLF", args: []string{"--verify", "false"},
			patch: "@@ file a.txt\r\n@@ old\r\none\r\n@@ new\r\nONE\r\n", exit: exitVerifyFailed,
		},
		{
			name: "every byte as it came: no final newline", args: []string{"--verify", "false"},
			patch: "@@ file a.txt\n@@ old\none\n@@ new\nONE", exit: exitVerifyFailed,
		},
		{
			name: "every byte as it came: not UTF-8, and a control", args: []string{"--verify", "false"},
			patch: "@@ file a.txt\n@@ old\none\n@@ new\nO\xffN\x01E\n", exit: exitVerifyFailed,
		},
		{name: "--json", args: []string{"--json", "--verify", "false"}, patch: vPatch, exit: exitVerifyFailed, json: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			if c.path != nil {
				t.Setenv("PATH", c.path(t))
			}
			code, out, errOut := keepCLI(t, root, c.args, c.patch)
			if code != c.exit {
				t.Fatalf("exit %d, want %d: %s%s", code, c.exit, out, errOut)
			}
			if want := cmp.Or(c.after, "one\n"); readFile(t, root, "a.txt") != want {
				t.Errorf("a.txt = %q, want %q", readFile(t, root, "a.txt"), want)
			}
			if c.json {
				var got struct {
					KeptPatch string `json:"kept_patch"`
				}
				must(t, json.Unmarshal([]byte(out), &got))
				checkKept(t, got.KeptPatch, c.patch)
				if errOut != "" {
					t.Errorf("stderr = %q, want nothing beside the JSON", errOut)
				}
				return
			}
			m := keptLine.FindAllStringSubmatchIndex(errOut, -1)
			if len(m) != 1 || m[0][1] != len(errOut) {
				t.Fatalf("want the kept line once, and last:\n%s", errOut)
			}
			if !strings.HasSuffix(errOut[:m[0][0]], "\n\n") {
				t.Errorf("want a blank line before the kept line:\n%s", errOut)
			}
			checkKept(t, errOut[m[0][2]:m[0][3]], c.patch)
		})
	}
}

// A signal while the verify runs puts the batch back, and keeps the patch.
func TestASignalDuringTheVerifyKeepsThePatch(t *testing.T) {
	needsSignals(t)
	withDelays(t, 200*time.Millisecond, 200*time.Millisecond)
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	var out, errOut bytes.Buffer
	var sig os.Signal
	code := invoke([]string{"--root", root, "--verify", "kill -TERM $PPID; sleep 5"},
		strings.NewReader(vPatch), &out, &errOut, &sig)
	if code != 143 || sig != syscall.SIGTERM {
		t.Fatalf("exit %d, signal %v: %s%s", code, sig, out.String(), errOut.String())
	}
	m := keptLine.FindStringSubmatch(errOut.String())
	if m == nil || !strings.HasSuffix(errOut.String(), m[0]) {
		t.Fatalf("want the kept line last:\n%s", errOut.String())
	}
	checkKept(t, m[1], vPatch)
	if got := readFile(t, root, "a.txt"); got != "one\n" {
		t.Errorf("a.txt = %q, want it back", got)
	}
}

// Every outcome that keeps nothing, in a temporary directory of its own that
// has to stay empty: a batch that stays or was never applied, a patch that is
// a file already, and --try, whose batch is meant to go.
func TestNothingIsKept(t *testing.T) {
	const miss = "@@ file a.txt\n@@ old\nnot there\n@@ new\nX\n"
	for _, c := range []struct {
		name     string
		args     []string
		patch    string
		fromFile bool // the patch is given with -f FILE, not on stdin
		exit     int
		perms    bool // rests on a chmod that refuses a write
	}{
		{name: "a verify that passes", args: []string{"--verify", "true"}, patch: vPatch, exit: exitOK},
		{name: "no verify", patch: vPatch, exit: exitOK},
		{name: "a patch from a file", args: []string{"--verify", "false"}, patch: vPatch, fromFile: true, exit: exitVerifyFailed},
		{name: "--keep-on-fail, whose batch stays", args: []string{"--keep-on-fail", "--verify", "false"}, patch: vPatch, exit: exitVerifyFailed},
		{name: "a --try that fails", args: []string{"--try", "false"}, patch: vPatch, exit: 1},
		{name: "a --try that passes", args: []string{"--try", "true"}, patch: vPatch, exit: exitOK},
		{name: "--dry-run", args: []string{"--dry-run", "--verify", "false"}, patch: vPatch, exit: exitOK},
		{name: "a hunk that did not match", args: []string{"--verify", "false"}, patch: miss, exit: exitNoMatch},
		{name: "a patch that does not parse", args: []string{"--verify", "false"}, patch: "not a patch\n", exit: exitUsage},
		{
			name: "a commit that fails", args: []string{"--verify", "false"},
			patch: "@@ file d/b.txt\n@@ old\ntwo\n@@ new\nTWO\n", exit: exitIO, perms: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.perms {
				needsPOSIXPerms(t)
			}
			root := cliTree(t, map[string]string{"a.txt": "one\n", "d/b.txt": "two\n"})
			if c.perms {
				must(t, os.Chmod(filepath.Join(root, "d"), 0o555))
				t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "d"), 0o755) })
			}
			args, stdin := c.args, c.patch
			if c.fromFile {
				p := filepath.Join(t.TempDir(), "p.hunk")
				must(t, os.WriteFile(p, []byte(c.patch), 0o644))
				args, stdin = append([]string{"-f", p}, args...), ""
			}
			dir := ownTempDir(t)
			code, out, errOut := keepCLI(t, root, args, stdin)
			if code != c.exit {
				t.Fatalf("exit %d, want %d: %s%s", code, c.exit, out, errOut)
			}
			if strings.Contains(out+errOut, "kept") {
				t.Errorf("says it kept something:\n%s%s", out, errOut)
			}
			if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
				t.Errorf("the temporary directory holds %v (%v), want nothing", left, err)
			}
		})
	}
}

// A patch that cannot be kept leaves the exit as the rollback's, and says why
// where the line would have been: last in the text, on stderr beside --json.
// A file the write failed on is taken away, since part of a patch would apply
// part of a batch.
func TestAPatchThatCannotBeKept(t *testing.T) {
	t.Run("no temporary directory", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		missing := filepath.Join(t.TempDir(), "missing")
		setTempDir(t, missing)
		code, out, errOut := keepCLI(t, root, []string{"--verify", "false"}, vPatch)
		if code != exitVerifyFailed || out != "" {
			t.Fatalf("exit %d, stdout %q", code, out)
		}
		// Last, after the verify's output and a blank line, and one line.
		at := strings.LastIndex(errOut, "\n\nthe patch could not be kept in "+missing+": ")
		if at < 0 || !strings.HasSuffix(errOut[:at], "\n$ false") || strings.Count(errOut[at+2:], "\n") != 1 {
			t.Errorf("want the line after the verify's output, and last:\n%s", errOut)
		}

		code, out, errOut = keepCLI(t, root, []string{"--json", "--verify", "false"}, vPatch)
		if code != exitVerifyFailed || strings.Contains(out, "kept_patch") {
			t.Errorf("exit %d, stdout %s", code, out)
		}
		if want := "hunk: the patch could not be kept in " + missing + ": "; !strings.HasPrefix(errOut, want) ||
			strings.Count(errOut, "\n") != 1 {
			t.Errorf("stderr = %q, want one line starting %q", errOut, want)
		}
	})

	boom := errors.New("no space left on device")
	for _, c := range []struct {
		name               string
		writeErr, closeErr error
	}{
		{"a write that fails", boom, nil},
		{"a close that fails", nil, boom},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			name := filepath.Join(t.TempDir(), "hunk-patch-1.hunk")
			orig := createKept
			t.Cleanup(func() { createKept = orig })
			createKept = func() (keptFile, error) {
				f, err := os.Create(name)
				return &failingFile{File: f, writeErr: c.writeErr, closeErr: c.closeErr}, err
			}
			code, out, errOut := keepCLI(t, root, []string{"--verify", "false"}, vPatch)
			if code != exitVerifyFailed || out != "" {
				t.Fatalf("exit %d, stdout %q", code, out)
			}
			if want := "\n\nthe patch could not be kept in " + name + ": no space left on device\n"; !strings.HasSuffix(errOut, want) {
				t.Errorf("stderr = %q, want it to end %q", errOut, want)
			}
			if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the file the patch did not fit in is still there: %v", err)
			}
		})
	}
}

// failingFile is a real file whose write or close fails as asked, after
// doing what it was asked to, so that what is left on disk is real.
type failingFile struct {
	*os.File
	writeErr, closeErr error
}

func (f *failingFile) Write(b []byte) (int, error) {
	n, err := f.File.Write(b)
	return n, cmp.Or(f.writeErr, err)
}

func (f *failingFile) Close() error {
	err := f.File.Close()
	return cmp.Or(f.closeErr, err)
}

// The issue's case, end to end: a batch that missed a caller is rolled back,
// and the kept patch with the missed hunk after it is the next batch, nothing
// typed twice. A batch is a concatenation, which is what makes that work.
func TestAKeptPatchTakesTheHunkItMissed(t *testing.T) {
	root := cliTree(t, map[string]string{
		"lib.txt":    "func area(w int) int\n",
		"caller.txt": "x := area(3)\n",
	})
	before := snapshot(t, root)
	// The verify: every call has the definition's arity.
	verify := `grep -q 'area(w, h int)' lib.txt && grep -q 'area(3, 4)' caller.txt`
	first := "@@ file lib.txt\n@@ old\nfunc area(w int) int\n@@ new\nfunc area(w, h int) int\n"
	code, _, errOut := keepCLI(t, root, []string{"--verify", verify}, first)
	if code != exitVerifyFailed {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	assertUnchanged(t, root, before)
	m := keptLine.FindStringSubmatch(errOut)
	if m == nil {
		t.Fatalf("nothing kept:\n%s", errOut)
	}
	kept, err := os.ReadFile(m[1])
	must(t, err)
	more := "@@ file caller.txt\n@@ old\nx := area(3)\n@@ new\nx := area(3, 4)\n"
	code, out, errOut := runCLI(t, root, []string{"--verify", verify}, string(kept)+more)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if got := readFile(t, root, "lib.txt") + readFile(t, root, "caller.txt"); got != "func area(w, h int) int\nx := area(3, 4)\n" {
		t.Errorf("the tree is %q", got)
	}
}

// FuzzKeepPatch: whatever came on stdin, keepPatch keeps exactly it, or says
// why not; it reads nothing in the bytes, so none can stop it.
func FuzzKeepPatch(f *testing.F) {
	for _, s := range []string{"", vPatch, "@@ file a.txt\r\n", "\x00\xff", strings.Repeat("x", 1<<16)} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		path, err := keepPatch(src)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(path) }()
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, src) {
			t.Fatalf("kept %q (%v), want %q", got, err, src)
		}
	})
}
