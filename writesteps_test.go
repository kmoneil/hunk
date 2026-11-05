package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refuseStep makes one of writeAtomic's four steps fail, as a full disk or an
// I/O error would, while the nth file is written, and passes every other call
// through: commit puts back the files before it through the same steps, and a
// seam that refused those too would test a different report. A file's write is
// its first step, so counting writes counts files.
//
// The refusal is a *fs.PathError naming the temp file, as the os package's
// errors do, so that the message is also shown not to name it. A refused write
// writes half the bytes first, which is what a full disk does; a refused close
// closes, as a real one does whatever it returns. It returns the temp file the
// refused step was given, so a test can check it was closed.
func refuseStep(t *testing.T, step string, n int) **os.File {
	t.Helper()
	w, s, m, c := fwrite, fsync, fchmod, fclose
	t.Cleanup(func() { fwrite, fsync, fchmod, fclose = w, s, m, c })
	refused := new(*os.File)
	writes := 0
	hit := func(op string, f *os.File) error {
		if op != step || writes != n {
			return nil
		}
		*refused = f
		return &fs.PathError{Op: op, Path: f.Name(), Err: errors.New("injected " + op + " failure")}
	}
	fwrite = func(f *os.File, b []byte) (int, error) {
		writes++
		if err := hit("write", f); err != nil {
			k, _ := w(f, b[:len(b)/2])
			return k, err
		}
		return w(f, b)
	}
	fsync = func(f *os.File) error {
		if err := hit("sync", f); err != nil {
			return err
		}
		return s(f)
	}
	fchmod = func(f *os.File, mode fs.FileMode) error {
		if err := hit("chmod", f); err != nil {
			return err
		}
		return m(f, mode)
	}
	fclose = func(f *os.File) error {
		if err := hit("close", f); err != nil {
			_ = c(f)
			return err
		}
		return c(f)
	}
	return refused
}

// A commit that fails at any of the four steps before the rename puts back
// what it wrote and is exit 5, naming the file and the cause, with no temp file
// left in the tree and none left open. TestACommitThatFailsPutsBackWhatItWrote
// fails it at the rename. Until 2026-11-05 no test reached these four returns,
// and "return nil" in place of "return err" after the fsync passed the suite.
func TestACommitThatFailsAtAStepPutsBackWhatItWrote(t *testing.T) {
	const (
		modify   = "@@ file keep.txt\n@@ old\nkeep\n@@ new\nKEEP\n"
		create   = "@@ create new.txt\nnew\n\n"
		first    = "@@ create first.txt\nfirst\n\n"
		del      = "@@ delete gone.txt\n"
		nothing  = "nothing was written"
		one      = "the file written before it was put back, so nothing was written"
		twoFiles = "the 2 files written before it were put back, so nothing was written"
	)
	all := []string{"write", "sync", "chmod", "close"}
	noChmod := []string{"write", "sync", "close"} // a create keeps what the open gave it
	for _, c := range []struct {
		name, patch string
		n           int    // the file that fails, counting the files written
		failed      string // "creating new.txt"
		tail        string
		steps       []string
	}{
		{"a create, alone", create, 1, "creating new.txt", nothing, noChmod},
		{"a modify, alone", modify, 1, "writing keep.txt", nothing, all},
		{"a create after a modify", modify + create, 2, "creating new.txt", one, noChmod},
		{"a modify after a create and a delete", first + del + modify, 2, "writing keep.txt", twoFiles, all},
	} {
		for _, step := range c.steps {
			t.Run(c.name+", at the "+step, func(t *testing.T) {
				root := cliTree(t, map[string]string{"keep.txt": "keep\n", "gone.txt": "gone\n"})
				must(t, os.Chmod(filepath.Join(root, "gone.txt"), 0o640))
				before := snapshot(t, root)
				refused := refuseStep(t, step, c.n)
				code, out, errOut := runCLI(t, root, nil, c.patch)
				if code != exitIO {
					t.Errorf("exit %d, want %d: %s", code, exitIO, errOut)
				}
				want := "hunk: " + c.failed + " failed: injected " + step + " failure; " + c.tail + "\n"
				if errOut != want {
					t.Errorf("stderr = %q\nwant     %q", errOut, want)
				}
				if out != "" {
					t.Errorf("stdout = %q", out)
				}
				if *refused == nil {
					t.Fatalf("the %s of file %d was never refused", step, c.n)
				}
				// A second Close says whether the first happened, on every
				// platform, as writeAtomic's cleanup relies on: Stat on a
				// closed file is ErrClosed on Unix and "the handle is
				// invalid" on Windows.
				if err := (*refused).Close(); !errors.Is(err, os.ErrClosed) {
					t.Errorf("the temp file was still open after the refusal: a second Close says %v", err)
				}
				assertUnchanged(t, root, before) // a temp file left behind is a file the tree did not have
			})
		}
	}
	// The control for the rows above: a create has no chmod, so refusing the
	// first chmod refuses nothing and the batch applies.
	t.Run("a create has no chmod to refuse", func(t *testing.T) {
		root := cliTree(t, map[string]string{"keep.txt": "keep\n"})
		refused := refuseStep(t, "chmod", 1)
		if code, _, errOut := runCLI(t, root, nil, create); code != exitOK {
			t.Errorf("exit %d, want 0: %s", code, errOut)
		}
		if *refused != nil {
			t.Error("a create's write called chmod")
		}
		if b, err := os.ReadFile(filepath.Join(root, "new.txt")); err != nil || !strings.HasPrefix(string(b), "new") {
			t.Errorf("new.txt = %q, %v", b, err)
		}
	})
}
