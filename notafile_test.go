package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A named pipe, a socket and a device are not files anybody edits, and load
// refuses each, per hunk, beside the batch's other failures. Until 2026-10-06 a
// named pipe with no writer held every operation in load's open for ever, one
// with a writer was read, edited and replaced by a regular file, a socket was
// exit 5 with a syscall, and an unconfined link to /dev/zero was read until the
// process ran out of memory.

// withoutHanging runs fn and fails the test, rather than the suite, if fn has
// not returned in five seconds. On the deadline it calls release until fn
// returns, so nothing the test started outlives it: for a pipe with no writer,
// opening it for writing lets a blocked open return, and closing that gives
// the read its end of file. fn must not call t's Fatal methods.
func withoutHanging(t *testing.T, release func(), fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
	}
	for {
		release()
		select {
		case <-done:
			t.Fatal("blocked until the test released the named pipe; that is the hang this guards against")
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// openForWriting is the release for a pipe nobody writes to. With no reader it
// fails at once (O_NONBLOCK), which is harmless: then nothing is blocked on it.
func openForWriting(pipe string) func() {
	return func() {
		if w, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	}
}

// isPipe reports whether name is still a named pipe, not followed.
func isPipe(t *testing.T, name string) bool {
	t.Helper()
	fi, err := os.Lstat(name)
	must(t, err)
	return fi.Mode()&fs.ModeNamedPipe != 0
}

// refused asserts that err is a validation failure that refuses hunk n, at
// path and patch line, saying want.
func refused(t *testing.T, err error, n int, path string, line int, want string) {
	t.Helper()
	if code := ExitCode(err); code != exitNoMatch {
		t.Fatalf("exit %d (%v), want %d", code, err, exitNoMatch)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %T %v, want a *ValidationError", err, err)
	}
	for _, f := range ve.Failures {
		if f.Hunk != n {
			continue
		}
		if f.Path != path || f.PatchLine != line || !strings.Contains(f.Refusal, want) {
			t.Errorf("hunk %d = %+v, want path %q, patch line %d, refusal saying %q", n, f, path, line, want)
		}
		return
	}
	t.Errorf("no failure for hunk %d in %+v", n, ve.Failures)
}

func TestANamedPipeIsNotAFile(t *testing.T) {
	const pipeRefusal = "it is a named pipe, not a file"
	for _, c := range []struct {
		name, patch string
		dryRun      bool
		pipe        string // where the pipe is made, under the root
		link        bool   // d -> sub, so the patch reaches sub/p through a directory link
		hunk, line  int
		path        string
	}{
		{name: "a replace", patch: "@@ file p\n@@ old\nx\n@@ new\ny\n", pipe: "p", hunk: 1, line: 2, path: "p"},
		{name: "an append", patch: "@@ append p\nz\n", pipe: "p", hunk: 1, line: 1, path: "p"},
		{name: "a prepend", patch: "@@ prepend p\nz\n", pipe: "p", hunk: 1, line: 1, path: "p"},
		{name: "a delete", patch: "@@ delete p\n", pipe: "p", hunk: 1, line: 1, path: "p"},
		// A create over it would rename a regular file onto the pipe.
		{name: "a create", patch: "@@ create p\nz\n", pipe: "p", hunk: 1, line: 1, path: "p"},
		{name: "a delete then a create", patch: "@@ delete p\n@@ create p\nz\n", pipe: "p", hunk: 1, line: 1, path: "p"},
		{name: "a dry run", patch: "@@ append p\nz\n", dryRun: true, pipe: "p", hunk: 1, line: 1, path: "p"},
		{name: "through a directory link", patch: "@@ append d/p\nz\n", pipe: "sub/p", link: true, hunk: 1, line: 1, path: "d/p"},
		// Reported beside the pipe. Until 2026-10-06 the open of the pipe
		// never returned, so the miss in a.txt was never reported at all.
		{
			name:  "a miss elsewhere in the batch",
			patch: "@@ file a.txt\n@@ old\nnope\n@@ new\nx\n@@ append p\nz\n",
			pipe:  "p", hunk: 2, line: 6, path: "p",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"a.txt": "one\n"})
			if c.link {
				must(t, os.Mkdir(filepath.Join(root, "sub"), 0o755))
				must(t, os.Symlink("sub", filepath.Join(root, "d")))
			}
			pipe := filepath.Join(root, filepath.FromSlash(c.pipe))
			makeFIFO(t, pipe)
			before := snapshot(t, root)
			p, err := Parse([]byte(c.patch), DefaultMarker)
			must(t, err)

			withoutHanging(t, openForWriting(pipe), func() {
				if c.dryRun {
					_, err = NewTxn(tree, Options{}).Preview(p)
				} else {
					_, err = NewTxn(tree, Options{}).Run(p)
				}
			})
			refused(t, err, c.hunk, c.path, c.line, pipeRefusal)
			if c.hunk == 2 {
				var ve *ValidationError
				if errors.As(err, &ve) && (len(ve.Failures) != 2 || ve.Failures[0].Path != "a.txt" || ve.Failures[0].Found != 0) {
					t.Errorf("failures = %+v, want the miss in a.txt reported first", ve.Failures)
				}
			}
			if !isPipe(t, pipe) {
				t.Errorf("%s is no longer a named pipe", c.pipe)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// A pipe somebody is writing to. Until 2026-10-06 load read the writer's text,
// the edit applied, and commit renamed a regular file over the pipe, at exit 0:
// the shape §6.5 forbids for a symlink. Now nothing is read: the bytes are
// still in the pipe for whoever was meant to read them.
func TestANamedPipeWithAWriterIsNeitherReadNorReplaced(t *testing.T) {
	tree, root := fixture(t, nil)
	pipe := filepath.Join(root, "p")
	makeFIFO(t, pipe)
	// Read and write, so the open does not wait for a reader and the test is
	// both ends: a writer for hunk to meet, and a reader to see what is left.
	rw, err := os.OpenFile(pipe, os.O_RDWR, 0)
	must(t, err)
	var once sync.Once
	closeRW := func() { once.Do(func() { _ = rw.Close() }) }
	t.Cleanup(closeRW)
	_, err = rw.Write([]byte("x\n"))
	must(t, err)
	p, err := Parse([]byte("@@ file p\n@@ old\nx\n@@ new\ny\n"), DefaultMarker)
	must(t, err)

	// Old code read "x\n" and then waited for an end of file that only
	// closing the test's end gives, and its Check opened the pipe again.
	withoutHanging(t, func() { closeRW(); openForWriting(pipe)() }, func() {
		_, err = NewTxn(tree, Options{}).Run(p)
	})
	refused(t, err, 1, "p", 2, "it is a named pipe, not a file")
	if !isPipe(t, pipe) {
		t.Fatal("p was replaced by a regular file")
	}
	// A read deadline is not available on a pipe everywhere (macOS keeps it
	// out of the poller), so the read back is guarded the same way: if the
	// pipe is empty, because something consumed the writer's bytes, writing
	// one byte from this end lets the read return, and the test fails.
	buf := make([]byte, 16)
	var n int
	withoutHanging(t, func() { _, _ = rw.Write([]byte("!")) }, func() {
		n, err = rw.Read(buf)
	})
	if err != nil || string(buf[:n]) != "x\n" {
		t.Errorf("read back %q, %v; want the writer's \"x\\n\" still unread", buf[:n], err)
	}
}

// A socket fails the open itself. Until 2026-10-06 that was exit 5 with the
// syscall's words; the fix is in the patch, so it is exit 2 with a sentence.
func TestASocketIsNotAFile(t *testing.T) {
	needsUnixSocketFiles(t)
	for _, patch := range []string{
		"@@ file s\n@@ old\nx\n@@ new\ny\n",
		"@@ append s\nz\n",
		"@@ delete s\n",
		"@@ create s\nz\n",
	} {
		t.Run(strings.Fields(patch)[1], func(t *testing.T) {
			tree, root := fixture(t, nil)
			// Bound by a relative name from inside the root: a socket's path
			// is limited to 104 bytes on macOS, and a runner's temp directory
			// alone can come near that.
			t.Chdir(root)
			l, err := net.Listen("unix", "s")
			must(t, err)
			t.Cleanup(func() { _ = l.Close() })
			before := snapshot(t, root)
			_, err = run(t, tree, patch, Options{})
			line := 1
			if strings.HasPrefix(patch, "@@ file") {
				line = 2
			}
			refused(t, err, 1, "s", line, "it is a socket, not a file")
			assertUnchanged(t, root, before)
		})
	}
}

// A device is reached only through a link, since making a node needs root.
// Confined, a link to one is already refused as out of the root. Unconfined,
// /dev/zero was read without end until 2026-10-06, so these run hunk as its own
// process, killed if it has not finished: a hang here is a failure, not a
// stuck suite, and an unbounded read cannot take the suite's memory with it.
func TestADeviceIsNotAFile(t *testing.T) {
	needsDeviceFiles(t)
	exe, err := os.Executable()
	must(t, err)
	for _, dev := range []string{"/dev/zero", "/dev/null"} {
		for _, patch := range []string{"@@ append dev.link\nz\n", "@@ file dev.link\n@@ old\nx\n@@ new\ny\n"} {
			t.Run(dev+" "+strings.Fields(patch)[1], func(t *testing.T) {
				root := cliTree(t, nil)
				must(t, os.Symlink(dev, filepath.Join(root, "dev.link")))

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, exe, "--root", root, "--allow-outside-root")
				cmd.Env = append(os.Environ(), "HUNK_TEST_AS_HUNK=1")
				cmd.Stdin = strings.NewReader(patch)
				out, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("still running after 5s, killed: %s reached through a link is being read", dev)
				}
				var ee *exec.ExitError
				if !errors.As(err, &ee) || ee.ExitCode() != exitNoMatch {
					t.Fatalf("err %v, want exit %d: %s", err, exitNoMatch, out)
				}
				if !strings.Contains(string(out), "it is a device, not a file") {
					t.Errorf("output does not say it is a device:\n%s", out)
				}

				code, _, errOut := runCLI(t, root, nil, patch)
				if code != exitNoMatch || !strings.Contains(errOut, "it is a symlink out of the root") {
					t.Errorf("confined: exit %d, %q; want the link refused as out of the root", code, errOut)
				}
			})
		}
	}
}

// A regular file replaced by a pipe between load and Check is a second writer,
// exit 6, by identity. Until 2026-10-06 Check's own open of the pipe waited.
func TestCheckDoesNotWaitOnAPipePutUnderneath(t *testing.T) {
	tree, root := fixture(t, map[string]string{"a.go": "one\n"})
	p, err := Parse([]byte("@@ file a.go\n@@ old\none\n@@ new\nONE\n"), DefaultMarker)
	must(t, err)
	x := NewTxn(tree, Options{})
	must(t, x.Load(p))
	if f := x.Validate(p); len(f) != 0 {
		t.Fatalf("validate: %+v", f)
	}
	name := filepath.Join(root, "a.go")
	must(t, os.Remove(name))
	makeFIFO(t, name)

	withoutHanging(t, openForWriting(name), func() { err = x.Check() })
	var ce *ChangedError
	if !errors.As(err, &ce) || ExitCode(err) != exitChanged {
		t.Fatalf("Check = %T %v, want a *ChangedError, exit %d", err, err, exitChanged)
	}
	if !isPipe(t, name) {
		t.Error("a.go is no longer the pipe put there")
	}
}

// The page an agent reads, in both renderings: the refusal beside a miss in
// another file, which the hang used to hide.
func TestANamedPipeIsGolden(t *testing.T) {
	patch := "@@ file a.txt\n@@ old\nnope\n@@ new\nx\n\n@@ append p\nz\n"
	tree := func() string {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		makeFIFO(t, filepath.Join(root, "p"))
		return root
	}

	root := tree()
	var code int
	var out, errOut string
	withoutHanging(t, openForWriting(filepath.Join(root, "p")), func() {
		code, out, errOut = runCLI(t, root, nil, patch)
	})
	if code != exitNoMatch || out != "" {
		t.Fatalf("exit %d, stdout %q; want %d and the report on stderr", code, out, exitNoMatch)
	}
	golden(t, "cli-not-a-file", slashPaths(strings.ReplaceAll(errOut, root, "/the/root")))

	root = tree()
	withoutHanging(t, openForWriting(filepath.Join(root, "p")), func() {
		code, out, errOut = runCLI(t, root, []string{"--json"}, patch)
	})
	if code != exitNoMatch || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want %d and nothing on stderr", code, errOut, exitNoMatch)
	}
	quoted, err := json.Marshal(root)
	must(t, err)
	golden(t, "cli-not-a-file-json", strings.ReplaceAll(out, strings.Trim(string(quoted), `"`), "/the/root"))
}

// notAFile names exactly the three kinds, and nothing else, so a regular file,
// a directory and a symlink, and Windows' irregular reparse points, are not
// refused by it.
func TestNotAFileNamesThreeKinds(t *testing.T) {
	for _, c := range []struct {
		mode fs.FileMode
		want string
	}{
		{0o644, ""},
		{fs.ModeDir | 0o755, ""},
		{fs.ModeSymlink | 0o777, ""},
		{fs.ModeIrregular | 0o644, ""},
		{fs.ModeNamedPipe | 0o644, "a named pipe"},
		{fs.ModeSocket | 0o755, "a socket"},
		{fs.ModeDevice | 0o666, "a device"},
		{fs.ModeDevice | fs.ModeCharDevice | 0o666, "a device"},
	} {
		if got := notAFile(modeInfo(c.mode)); got != c.want {
			t.Errorf("notAFile(%v) = %q, want %q", c.mode, got, c.want)
		}
	}
}

// modeInfo is an fs.FileInfo with only a mode, for the one test of a pure
// function over modes that no filesystem here can produce all of.
type modeInfo fs.FileMode

func (m modeInfo) Name() string       { return "x" }
func (m modeInfo) Size() int64        { return 0 }
func (m modeInfo) Mode() fs.FileMode  { return fs.FileMode(m) }
func (m modeInfo) ModTime() time.Time { return time.Time{} }
func (m modeInfo) IsDir() bool        { return fs.FileMode(m).IsDir() }
func (m modeInfo) Sys() any           { return nil }
