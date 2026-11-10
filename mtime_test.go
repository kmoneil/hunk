package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every file hunk writes is dated past every earlier version of it (#162).
// Python takes a .pyc for current while the source's whole-second mtime and
// size are the ones it compiled, so a same-size file put back within the
// second the patch was written ran the patch's bytecode from a tree that held
// the original: after a --try, after an --each (whose after-run then failed
// with no patch applied), and after a --verify that rolled back.

// writtenAt is a FileInfo whose ModTime is at, for fstat to hand stamp, so a
// test says when the temp file was written rather than the clock.
type writtenAt struct {
	fs.FileInfo
	at time.Time
}

func (w writtenAt) ModTime() time.Time { return w.at }

// writeTimeIs makes every temp file read as written at at.
func writeTimeIs(t *testing.T, at time.Time) {
	t.Helper()
	real := fstat
	t.Cleanup(func() { fstat = real })
	fstat = func(f *os.File) (fs.FileInfo, error) {
		fi, err := real(f)
		if err != nil {
			return nil, err
		}
		return writtenAt{fi, at}, nil
	}
}

func dateFile(t *testing.T, path string, at time.Time) {
	t.Helper()
	must(t, os.Chtimes(path, time.Time{}, at))
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	must(t, err)
	return fi.ModTime()
}

// stamp's rule, one source of an earlier second at a time and then together.
// base is a fixed second, and every time is relative to it, so no row depends
// on the clock.
func TestStampDatesAWritePastEveryEarlierVersion(t *testing.T) {
	const base = int64(1_800_000_000)
	at := time.Unix(base, 400_000_000) // the temp file, written 0.4s into base
	none := int64(-1)
	for _, c := range []struct {
		name             string
		like, file, last int64 // the second each is dated, or none
		want             int64
		moved            bool
	}{
		{"nothing earlier keeps the temp's own time", none, none, none, base, false},
		{"an older file at the name keeps it", none, base - 5, none, base, false},
		{"a file at the name in the second before keeps it", none, base - 1, none, base, false},
		{"the file at the name in the same second moves it to the next", none, base, none, base + 1, true},
		{"a file at the name dated ahead of the clock moves it past that", none, base + 7, none, base + 8, true},
		{"like in the same second with nothing at the name, as a delete's restore", base, none, none, base + 1, true},
		{"like older keeps it", base - 1, none, none, base, false},
		{"the second this tree gave the name last, as a create made again", none, none, base, base + 1, true},
		{"a last second older keeps it", none, none, base - 1, base, false},
		{"like the latest of the three", base + 3, base + 1, base + 2, base + 4, true},
		{"the file at the name the latest of the three", base + 2, base + 3, base + 1, base + 4, true},
		{"the last second the latest of the three", base + 1, base + 2, base + 3, base + 4, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{}
			if c.file != none {
				files["f.txt"] = "x\n"
			}
			tree, root := fixture(t, files)
			if c.file != none {
				dateFile(t, filepath.Join(root, "f.txt"), time.Unix(c.file, 900_000_000))
			}
			var like fs.FileInfo
			if c.like != none {
				like = writtenAt{nil, time.Unix(c.like, 0)}
			}
			if c.last != none {
				tree.stamps = map[string]int64{"f.txt": c.last}
			}
			got, moved := tree.stamp("f.txt", at, like)
			want := at
			if c.moved {
				want = time.Unix(c.want, 0)
			}
			if !got.Equal(want) || moved != c.moved {
				t.Errorf("stamp = %v, moved %v; want %v, moved %v", got, moved, want, c.moved)
			}
			if tree.stamps["f.txt"] != c.want {
				t.Errorf("the second recorded for f.txt is %d, want %d", tree.stamps["f.txt"], c.want)
			}
		})
	}
}

// The date stamp chooses is the one the file has at its name, confined and
// under --allow-outside-root, whose writes take another branch to the
// filesystem. The temp file reads as written in the second the file already
// has, so the write has to move.
func TestAWriteInTheSecondTheFileHasIsDatedTheNext(t *testing.T) {
	for _, flags := range [][]string{nil, {"--allow-outside-root"}} {
		t.Run(fmt.Sprintf("flags %q", flags), func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "alpha\n"})
			now := time.Unix(1_800_000_000, 250_000_000)
			dateFile(t, filepath.Join(root, "f.txt"), now)
			writeTimeIs(t, now.Add(500*time.Millisecond))
			code, _, errOut := runCLI(t, root, flags, "@@ file f.txt\n@@ old\nalpha\n@@ new\nALPHA\n")
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if got, want := mtimeOf(t, filepath.Join(root, "f.txt")), time.Unix(1_800_000_001, 0); !got.Equal(want) {
				t.Errorf("f.txt is dated %v, want %v", got, want)
			}
			if got := readFile(t, root, "f.txt"); got != "ALPHA\n" {
				t.Errorf("f.txt = %q", got)
			}
		})
	}
}

// A date that cannot be set fails the write like any other step: exit 5,
// nothing written, no temp file left, and the message names the file, not the
// temp file the error came from.
func TestAWriteWhoseDateIsRefusedWritesNothing(t *testing.T) {
	root := cliTree(t, map[string]string{"f.txt": "alpha\n"})
	now := time.Unix(1_800_000_000, 0)
	dateFile(t, filepath.Join(root, "f.txt"), now)
	writeTimeIs(t, now)
	real := chtimes
	t.Cleanup(func() { chtimes = real })
	tried := ""
	chtimes = func(_ *Tree, name string, _ time.Time) error {
		tried = name
		return &fs.PathError{Op: "chtimes", Path: name, Err: errors.New("injected chtimes failure")}
	}
	before := snapshot(t, root)
	code, out, errOut := runCLI(t, root, nil, "@@ file f.txt\n@@ old\nalpha\n@@ new\nALPHA\n")
	if code != exitIO {
		t.Errorf("exit %d, want %d: %s", code, exitIO, errOut)
	}
	if want := "hunk: writing f.txt failed: injected chtimes failure; nothing was written\n"; errOut != want {
		t.Errorf("stderr = %q\nwant     %q", errOut, want)
	}
	if out != "" {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(tried, ".hunk-") {
		t.Errorf("chtimes was given %q, want the temp file", tried)
	}
	assertUnchanged(t, root, before)
	if got := mtimeOf(t, filepath.Join(root, "f.txt")); !got.Equal(now) {
		t.Errorf("f.txt is dated %v, want %v as it was", got, now)
	}
}

// copyEachVersion is a command that copies name, date and all, into dir each
// time it runs, and versionsSeen reads the copies back oldest first: what the
// command saw, in the order it saw it.
func copyEachVersion(name, dir string) string {
	return fmt.Sprintf(`cp -p %s "$(mktemp '%s/v.XXXXXX')"`, name, filepath.ToSlash(dir))
}

type fileVersion struct {
	text string
	at   time.Time
}

func versionsSeen(t *testing.T, dir string) []fileVersion {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	var vs []fileVersion
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		must(t, err)
		vs = append(vs, fileVersion{string(b), mtimeOf(t, p)})
	}
	slices.SortFunc(vs, func(a, b fileVersion) int { return a.at.Compare(b.at) })
	return vs
}

// assertEachPastTheLast checks that the versions read texts, in order, and
// that each is dated in a later whole second than the one before.
func assertEachPastTheLast(t *testing.T, vs []fileVersion, texts ...string) {
	t.Helper()
	var got []string
	for _, v := range vs {
		got = append(got, v.text)
	}
	if !slices.Equal(got, texts) {
		t.Fatalf("the command saw %q, want %q", got, texts)
	}
	for i := 1; i < len(vs); i++ {
		if vs[i].at.Unix() <= vs[i-1].at.Unix() {
			t.Errorf("version %d (%q) is dated %v, not in a second after version %d (%q) at %v",
				i, vs[i].text, vs[i].at, i-1, vs[i-1].text, vs[i-1].at)
		}
	}
}

// The issue's case, without Python: three same-size breaks of one file tried
// in well under a second, the file dated now when the run starts. The command
// sees the original, each break and the original again, and each is dated in
// a later second than the one before, as is the file left at the end.
func TestEveryVersionAnEachRunWritesIsDatedPastTheLast(t *testing.T) {
	root := cliTree(t, map[string]string{"f.txt": "alpha\n"})
	dateFile(t, filepath.Join(root, "f.txt"), time.Now())
	pdir, seen := t.TempDir(), t.TempDir()
	var args []string
	for i, b := range []string{"ALPHA", "alphA", "Alpha"} {
		args = append(args, writePatch(t, pdir, fmt.Sprintf("%d.hunk", i), "@@ file f.txt\n@@ old\nalpha\n@@ new\n"+b+"\n"))
	}
	args = append([]string{"--try", copyEachVersion("f.txt", seen) + "; grep -q alpha f.txt", "--each"}, args...)
	code, _, errOut := runCLI(t, root, args, "")
	if code != exitOK {
		t.Fatalf("exit %d, want every break caught: %s", code, errOut)
	}
	vs := versionsSeen(t, seen)
	assertEachPastTheLast(t, vs, "alpha\n", "ALPHA\n", "alphA\n", "Alpha\n", "alpha\n")
	if last := mtimeOf(t, filepath.Join(root, "f.txt")); last.Unix() != vs[len(vs)-1].at.Unix() {
		t.Errorf("f.txt is dated %v at the end, not the %v the after-run saw", last, vs[len(vs)-1].at)
	}
}

// A create rolled back and made again leaves nothing at the name in between,
// so only the second the tree gave it last keeps the two apart.
func TestAFileCreatedTwiceInARunIsDatedPastItsFirstVersion(t *testing.T) {
	root := cliTree(t, map[string]string{"keep.txt": "keep\n"})
	pdir, seen := t.TempDir(), t.TempDir()
	one := writePatch(t, pdir, "1.hunk", "@@ create n.txt\none\n")
	two := writePatch(t, pdir, "2.hunk", "@@ create n.txt\ntwo\n")
	command := "if [ -f n.txt ]; then " + copyEachVersion("n.txt", seen) + "; exit 1; fi"
	code, _, errOut := runCLI(t, root, []string{"--try", command, "--each", one, two}, "")
	if code != exitOK {
		t.Fatalf("exit %d, want both caught: %s", code, errOut)
	}
	assertEachPastTheLast(t, versionsSeen(t, seen), "one", "two")
}

// A batch the verify fails, with the file dated now when it starts: the
// verify sees the patch dated past the original, and the original is put back
// dated past the patch. A delete's restore has only the file as load read it
// to go past, and a formatter's rewrite, dated ahead of the clock here, is the
// file at the name that the restore replaces.
func TestARollbackIsDatedPastWhatItReplaces(t *testing.T) {
	t.Run("a modify", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": "alpha\n"})
		orig := time.Now()
		dateFile(t, filepath.Join(root, "f.txt"), orig)
		seen := t.TempDir()
		code, _, errOut := runCLI(t, root, []string{"--verify", copyEachVersion("f.txt", seen) + "; exit 1"},
			"@@ file f.txt\n@@ old\nalpha\n@@ new\nALPHA\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want %d: %s", code, exitVerifyFailed, errOut)
		}
		vs := append([]fileVersion{{"alpha\n", orig}}, versionsSeen(t, seen)...)
		vs = append(vs, fileVersion{readFile(t, root, "f.txt"), mtimeOf(t, filepath.Join(root, "f.txt"))})
		assertEachPastTheLast(t, vs, "alpha\n", "ALPHA\n", "alpha\n")
	})
	t.Run("a delete", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": "alpha\n"})
		orig := time.Now()
		dateFile(t, filepath.Join(root, "f.txt"), orig)
		code, _, errOut := runCLI(t, root, []string{"--verify", "exit 1"}, "@@ delete f.txt\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want %d: %s", code, exitVerifyFailed, errOut)
		}
		vs := []fileVersion{{"alpha\n", orig}, {readFile(t, root, "f.txt"), mtimeOf(t, filepath.Join(root, "f.txt"))}}
		assertEachPastTheLast(t, vs, "alpha\n", "alpha\n")
	})
	t.Run("a formatter's rewrite dated ahead", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": "alpha\n"})
		ahead := time.Now().Add(5 * time.Second)
		formatted := filepath.Join(t.TempDir(), "formatted.txt")
		must(t, os.WriteFile(formatted, []byte("ALPHA\n"), 0o644))
		dateFile(t, formatted, ahead)
		verify := fmt.Sprintf("cp -p '%s' f.txt; exit 1", filepath.ToSlash(formatted))
		code, _, errOut := runCLI(t, root, []string{"--verify", verify, "--verify-may-format"},
			"@@ file f.txt\n@@ old\nalpha\n@@ new\nAlpha\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want %d: %s", code, exitVerifyFailed, errOut)
		}
		vs := []fileVersion{{"ALPHA\n", ahead}, {readFile(t, root, "f.txt"), mtimeOf(t, filepath.Join(root, "f.txt"))}}
		assertEachPastTheLast(t, vs, "ALPHA\n", "alpha\n")
	})
}

// The issue's repro, with Python itself: a same-size break tried five times
// in a row, each run within a second of the last. Each run catches it, and
// the tree imports the original afterwards. Before #162 the first run's
// put-back shared the patch's second and size, the after-run ran the patch's
// bytecode, and every run after refused the clean tree. A rollback after a
// failed verify is the same case without --try.
func TestPythonRunsTheFilePutBack(t *testing.T) {
	if err := exec.Command("python3", "-c", "pass").Run(); err != nil {
		t.Skipf("no python3 that runs: %v", err)
	}
	importsAAAA := `python3 -c "import m, sys; sys.exit(0 if m.X == 'aaaa' else 1)"`
	imported := func(t *testing.T, root string) string {
		t.Helper()
		cmd := exec.Command("python3", "-c", "import m; print(m.X)")
		cmd.Dir = root
		out, err := cmd.Output()
		must(t, err)
		return strings.TrimSpace(string(out))
	}
	patch := "@@ file m.py\n@@ old\nX = \"aaaa\"\n@@ new\nX = \"bbbb\"\n"
	t.Run("--each", func(t *testing.T) {
		root := cliTree(t, map[string]string{"m.py": "X = \"aaaa\"\n"})
		breaks := writePatch(t, t.TempDir(), "breaks.hunk", "@@ patch same-size\n"+patch)
		for i := 1; i <= 5; i++ {
			if code, _, errOut := runCLI(t, root, []string{"--try", importsAAAA, "--each", breaks}, ""); code != exitOK {
				t.Fatalf("run %d: exit %d, want the break caught: %s", i, code, errOut)
			}
		}
		if got := imported(t, root); got != "aaaa" {
			t.Errorf("after the runs the tree imports X = %q, want aaaa", got)
		}
	})
	t.Run("--verify", func(t *testing.T) {
		root := cliTree(t, map[string]string{"m.py": "X = \"aaaa\"\n"})
		if got := imported(t, root); got != "aaaa" {
			t.Fatalf("before the run the tree imports X = %q", got)
		}
		if code, _, errOut := runCLI(t, root, []string{"--verify", importsAAAA}, patch); code != exitVerifyFailed {
			t.Fatalf("exit %d, want %d: %s", code, exitVerifyFailed, errOut)
		}
		if got := imported(t, root); got != "aaaa" {
			t.Errorf("after the rollback the tree imports X = %q, want aaaa", got)
		}
	})
}
