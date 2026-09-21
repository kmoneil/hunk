package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// fixture builds a tree from a name->contents map and returns it with a Txn.
func fixture(t *testing.T, files map[string]string) (*Tree, string) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	for name, body := range files {
		full := filepath.Join(root, name)
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(t, os.WriteFile(full, []byte(body), 0o644))
	}
	tree, err := OpenTree(root, false)
	must(t, err)
	t.Cleanup(func() { tree.Close() })
	return tree, root
}

// snapshot records the tree under root, so "nothing was written" can be
// asserted rather than assumed: every regular file's contents and mode, every
// symlink, and every directory. §4.1 attaches a tree state to four exit codes
// and this is how those are checked.
//
// A symlink is recorded as the link, not read through. Reading through one made
// a tree with a directory link or a dangling link impossible to snapshot at
// all, and a link replaced by a regular file, which §6.5 forbids, would have
// looked unchanged.
//
// Directories were left out until 2026-09-21, which made assertUnchanged blind
// to the one thing a rollback can leave behind that is not a file. Rollback
// left a directory two creates had shared, under an exit 3 that says the tree
// is untouched, and every test that checked the tree passed.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case fi.IsDir():
			if rel != "." {
				out[rel] = "directory"
			}
			return nil
		case fi.Mode()&fs.ModeSymlink != 0:
			dest, err := os.Readlink(p)
			out[rel] = "symlink\x00" + dest
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[rel] = fi.Mode().String() + "\x00" + string(b)
		return nil
	})
	must(t, err)
	return out
}

// snapshotDiff names every entry that differs between two snapshots, in name
// order: what appeared, what went, and what changed. Nil means identical.
func snapshotDiff(before, after map[string]string) []string {
	var names []string
	for name := range before {
		names = append(names, name)
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var diffs []string
	for _, name := range names {
		b, wasThere := before[name]
		a, isThere := after[name]
		switch {
		case !isThere:
			diffs = append(diffs, name+" is gone")
		case !wasThere:
			diffs = append(diffs, name+" appeared")
		case a != b:
			diffs = append(diffs, name+" changed")
		}
	}
	return diffs
}

func assertUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	for _, d := range snapshotDiff(before, snapshot(t, root)) {
		t.Errorf("%s; the tree should be byte-identical", d)
	}
}

func TestSnapshotDiffNamesEveryDifference(t *testing.T) {
	base := map[string]string{"a.txt": "-rw-r--r--\x00a\n", "sub": "directory"}
	for _, c := range []struct {
		name  string
		after map[string]string
		want  []string
	}{
		{"identical", map[string]string{"a.txt": "-rw-r--r--\x00a\n", "sub": "directory"}, nil},
		{
			"a directory appeared",
			map[string]string{"a.txt": "-rw-r--r--\x00a\n", "sub": "directory", "new": "directory"},
			[]string{"new appeared"},
		},
		{"a directory went", map[string]string{"a.txt": "-rw-r--r--\x00a\n"}, []string{"sub is gone"}},
		{"a file changed", map[string]string{"a.txt": "-rw-r--r--\x00b\n", "sub": "directory"}, []string{"a.txt changed"}},
		{
			"all three, in name order",
			map[string]string{"a.txt": "-rw-------\x00a\n", "b.txt": "-rw-r--r--\x00b\n"},
			[]string{"a.txt changed", "b.txt appeared", "sub is gone"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := snapshotDiff(base, c.after); !slices.Equal(got, c.want) {
				t.Errorf("snapshotDiff = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSnapshotRecordsDirectories(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "a\n"})
	must(t, os.MkdirAll(filepath.Join(root, "empty", "deeper"), 0o755))
	got := snapshot(t, root)
	for _, d := range []string{"empty", filepath.Join("empty", "deeper")} {
		if got[d] != "directory" {
			t.Errorf("snapshot has %q for %s, want it recorded as a directory", got[d], d)
		}
	}
	if _, ok := got["."]; ok {
		t.Error("snapshot records the root itself, which every tree has")
	}
	if len(got) != 3 {
		t.Errorf("snapshot = %q, want a.txt and the two directories", got)
	}
}

func run(t *testing.T, tree *Tree, patch string, opt Options) (*Result, error) {
	t.Helper()
	p, err := Parse([]byte(patch), DefaultMarker)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return NewTxn(tree, opt).Run(p)
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	must(t, err)
	return string(b)
}

func TestApply(t *testing.T) {
	t.Run("one hunk", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "package a\n\nfunc f() {}\n"})
		r, err := run(t, tree, "@@ file a.go\n@@ old\nfunc f() {}\n@@ new\nfunc f(x int) {}\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "package a\n\nfunc f(x int) {}\n" {
			t.Errorf("got %q", got)
		}
		if r.Hunks != 1 || len(r.Files) != 1 || r.Files[0].Added != 1 || r.Files[0].Removed != 1 {
			t.Errorf("result = %+v", r)
		}
	})

	t.Run("many hunks across many files, one write each", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{
			"a.go": "one\ntwo\nthree\n",
			"b.go": "alpha\nbeta\n",
		})
		r, err := run(t, tree, ""+
			"@@ file a.go\n@@ old\none\n@@ new\nONE\n"+
			"@@ old\nthree\n@@ new\nTHREE\n"+
			"@@ file b.go\n@@ old\nbeta\n@@ new\nBETA\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "ONE\ntwo\nTHREE\n" {
			t.Errorf("a.go = %q", got)
		}
		if got := readFile(t, root, "b.go"); got != "alpha\nBETA\n" {
			t.Errorf("b.go = %q", got)
		}
		if r.Hunks != 3 || len(r.Files) != 2 {
			t.Errorf("result = %+v", r)
		}
	})

	// §3.5: hunks apply against the file as previous hunks have left it, so a
	// second hunk may legally match text a first one created.
	t.Run("a hunk matches text an earlier hunk created", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "start\n"})
		_, err := run(t, tree, ""+
			"@@ file a.go\n@@ old\nstart\n@@ new\nmiddle\n"+
			"@@ old\nmiddle\n@@ new\nend\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "end\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("x2 replaces both", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "P\nq\nP\n"})
		_, err := run(t, tree, "@@ file a.go\n@@ old x2\nP\n@@ new\nZ\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "Z\nq\nZ\n" {
			t.Errorf("got %q", got)
		}
	})

	// One file named several ways is one file, read once and written once
	// (§3.5). The resolved name is the key, never the string the patch wrote.
	t.Run("spellings of one path are one file", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"sub/a.go": "one\ntwo\n"})
		must(t, os.Symlink("sub/a.go", filepath.Join(root, "alias.go")))
		_, err := run(t, tree, ""+
			"@@ file sub/a.go\n@@ old\none\n@@ new\nONE\n"+
			"@@ file ./sub/../sub/a.go\n@@ old\ntwo\n@@ new\nTWO\n"+
			"@@ file alias.go\n@@ old\nONE\n@@ new\nUNO\n", Options{})
		must(t, err)
		if got := readFile(t, root, "sub/a.go"); got != "UNO\nTWO\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("the mode survives", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"s.sh": "#!/bin/sh\nfalse\n"})
		must(t, os.Chmod(filepath.Join(root, "s.sh"), 0o755))
		_, err := run(t, tree, "@@ file s.sh\n@@ old\nfalse\n@@ new\ntrue\n", Options{})
		must(t, err)
		fi, err := os.Stat(filepath.Join(root, "s.sh"))
		must(t, err)
		assertMode(t, fi.Mode(), 0o755)
	})

	t.Run("a symlinked target is written through", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"sub/real.go": "old\n"})
		must(t, os.Symlink("sub/real.go", filepath.Join(root, "link.go")))
		_, err := run(t, tree, "@@ file link.go\n@@ old\nold\n@@ new\nnew\n", Options{})
		must(t, err)
		fi, err := os.Lstat(filepath.Join(root, "link.go"))
		must(t, err)
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Error("the symlink was replaced")
		}
		if got := readFile(t, root, "sub/real.go"); got != "new\n" {
			t.Errorf("target = %q", got)
		}
	})
}

func TestApplyRefuses(t *testing.T) {
	t.Run("no match, and nothing is written", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\ntwo\n"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ file a.go\n@@ old\nabsent\n@@ new\nx\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want *ValidationError, got %T: %v", err, err)
		}
		if len(ve.Failures) != 1 || ve.Failures[0].Expected != 1 || ve.Failures[0].Found != 0 {
			t.Errorf("failures = %+v", ve.Failures)
		}
		if ve.Failures[0].PatchLine != 2 {
			t.Errorf("patch line = %d, want 2", ve.Failures[0].PatchLine)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("too many matches", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "P\nP\nP\n"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ file a.go\n@@ old\nP\n@@ new\nZ\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("got %T: %v", err, err)
		}
		if ve.Failures[0].Found != 3 {
			t.Errorf("found = %d, want 3", ve.Failures[0].Found)
		}
		assertUnchanged(t, root, before)
	})

	// §6.4: overlapping matches are counted left to right, non-overlapping, the
	// way strings.Count does. "aa" in "aaa" is 1, and the mismatch is reported
	// rather than guessed at.
	t.Run("overlapping matches are counted the way strings.Count does", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "aaa"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ file a.txt\n@@ old x2\naa\n@@ new\nb\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("got %T: %v", err, err)
		}
		if ve.Failures[0].Expected != 2 || ve.Failures[0].Found != 1 {
			t.Errorf("expected/found = %d/%d, want 2/1", ve.Failures[0].Expected, ve.Failures[0].Found)
		}
		assertUnchanged(t, root, before)
	})

	// §8.1's cascade test, all three assertions in one case: the failing hunk
	// is reported failed, the later hunk against the same file is reported
	// skipped rather than failed, and a hunk against another file is still
	// evaluated.
	t.Run("a failure cascades within its file and not beyond it", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{
			"a.go": "one\ntwo\n",
			"b.go": "beta\n",
		})
		before := snapshot(t, root)
		_, err := run(t, tree, ""+
			"@@ file a.go\n@@ old\none\n@@ new\nONE\n"+ // 1, would apply
			"@@ old\nabsent\n@@ new\nx\n"+ // 2, fails
			"@@ old\nONE\n@@ new\nUNO\n"+ // 3, skipped: depends on 1, same file as 2
			"@@ file b.go\n@@ old\nnope\n@@ new\ny\n", // 4, other file, still evaluated
			Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("got %T: %v", err, err)
		}
		if len(ve.Failures) != 3 {
			t.Fatalf("got %d failures, want 3: %+v", len(ve.Failures), ve.Failures)
		}
		f2, f3, f4 := ve.Failures[0], ve.Failures[1], ve.Failures[2]
		if f2.Hunk != 2 || f2.Skipped() {
			t.Errorf("hunk 2 should be a failure: %+v", f2)
		}
		if f3.Hunk != 3 || f3.SkippedAfter != 2 {
			t.Errorf("hunk 3 should be skipped after 2: %+v", f3)
		}
		if f4.Hunk != 4 || f4.Skipped() || f4.Found != 0 {
			t.Errorf("hunk 4 should be evaluated and failed: %+v", f4)
		}
		if msg := ve.Error(); !strings.Contains(msg, "2 hunks did not match") ||
			!strings.Contains(msg, "1 skipped") || !strings.Contains(msg, "nothing was written") {
			t.Errorf("summary = %q", msg)
		}
		assertUnchanged(t, root, before)
	})

	// A missing file is a per-hunk refusal, not an error that aborts the load,
	// so a patch with several of them reports all of them in one round trip.
	t.Run("a missing file is a reported refusal, not an aborted load", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"a.go": "x\n"})
		_, err := run(t, tree, ""+
			"@@ file gone.go\n@@ old\nx\n@@ new\ny\n"+
			"@@ file also-gone.go\n@@ old\nx\n@@ new\ny\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want *ValidationError, got %T: %v", err, err)
		}
		if len(ve.Failures) != 2 {
			t.Fatalf("got %d failures, want both reported: %+v", len(ve.Failures), ve.Failures)
		}
		for _, f := range ve.Failures {
			if !strings.Contains(f.Refusal, "no such file") {
				t.Errorf("hunk %d refusal = %q", f.Hunk, f.Refusal)
			}
		}
	})

	// ReadFile on a directory is not fs.ErrNotExist, so without this it would
	// fall through to an I/O error. §6.1's rule is whether editing the patch
	// would fix it, and here it would.
	t.Run("a directory is exit 2 and says so", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"sub/a.go": "x\n"})
		_, err := run(t, tree, "@@ file sub\n@@ old\nx\n@@ new\ny\n", Options{})
		var pr *PathRefusal
		if !errors.As(err, &pr) {
			t.Fatalf("want *PathRefusal, got %T: %v", err, err)
		}
		if !strings.Contains(pr.Reason, "is a directory") {
			t.Errorf("reason = %q", pr.Reason)
		}
	})

	// A replace after a delete in the same batch fails against the file the
	// batch removed, and says so rather than "no such file", which would send
	// the agent looking at a disk that still has it.
	t.Run("a hunk after a delete says an earlier hunk deleted it", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "x\n"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ delete a.go\n@@ old\nx\n@@ new\ny\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want *ValidationError, got %T: %v", err, err)
		}
		if !strings.Contains(ve.Failures[0].Refusal, "earlier hunk in this batch deleted it") {
			t.Errorf("refusal = %q", ve.Failures[0].Refusal)
		}
		assertUnchanged(t, root, before)
	})
}

// §6.1 step 4. The guard against a second writer between load and commit, which
// needs the phases driven separately because Run closes the window itself.
func TestCheckCatchesAWriterUnderneath(t *testing.T) {
	for _, c := range []struct {
		name string
		mess func(t *testing.T, root string)
	}{
		{"rewritten", func(t *testing.T, root string) {
			must(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("somebody else\n"), 0o644))
		}},
		{"removed", func(t *testing.T, root string) {
			must(t, os.Remove(filepath.Join(root, "a.go")))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"a.go": "one\n"})
			p, err := Parse([]byte("@@ file a.go\n@@ old\none\n@@ new\nONE\n"), DefaultMarker)
			must(t, err)
			x := NewTxn(tree, Options{})
			must(t, x.Load(p))
			if f := x.Validate(p); len(f) != 0 {
				t.Fatalf("validate: %+v", f)
			}

			c.mess(t, root)
			before := snapshot(t, root)

			err = x.Check()
			var ce *ChangedError
			if !errors.As(err, &ce) {
				t.Fatalf("want *ChangedError, got %T: %v", err, err)
			}
			if !strings.Contains(ce.Error(), "nothing was written") {
				t.Errorf("message = %q", ce.Error())
			}
			assertUnchanged(t, root, before)
		})
	}
}

// A commit that fails part-way puts back what it wrote, decided 2026-09-21.
// What a filesystem refuses, such as a name too long for it or a full disk, is
// not knowable before asking it, so the refusal comes at the rename, after
// every file before it was written. Until then those files stayed written,
// deletes included, and the message named a temp file.
//
// A 300-byte name forces it portably: every filesystem the suite runs on caps
// a name at 255, and under a directory the batch creates, load cannot see it.
func TestACommitThatFailsPutsBackWhatItWrote(t *testing.T) {
	long := strings.Repeat("n", 300)
	for _, c := range []struct {
		name, patch, says string
	}{
		{
			"a create into a directory it makes", "@@ create newdir/" + long + "\nx\n\n",
			"; nothing was written",
		},
		{
			"two directories deep", "@@ create a/b/" + long + "\nx\n\n",
			"; nothing was written",
		},
		{
			// A directory's name, not the file's: MkdirAll makes a and a/b and
			// then fails, and what it made has to come back deepest first.
			// Found by fuzzing this card's change.
			"a directory name too long, under two it makes", "@@ create a/b/" + long + "/x.go\nx\n\n",
			"; nothing was written",
		},
		{
			"after a create", "@@ create first.txt\nf\n\n@@ create newdir/" + long + "\nx\n\n",
			"; the file written before it was put back, so nothing was written",
		},
		{
			"after a modify and a delete", "@@ file keep.txt\n@@ old\nkeep\n@@ new\nKEEP\n@@ delete gone.txt\n" +
				"@@ create newdir/" + long + "\nx\n\n",
			"; the 2 files written before it were put back, so nothing was written",
		},
		{
			// Commit stops at the failure, so what comes after was never written.
			"before a modify", "@@ create newdir/" + long + "\nx\n\n@@ file keep.txt\n@@ old\nkeep\n@@ new\nKEEP\n",
			"; nothing was written",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"keep.txt": "keep\n", "gone.txt": "gone\n"})
			must(t, os.Chmod(filepath.Join(root, "gone.txt"), 0o640))
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, nil, c.patch)
			if code != exitIO {
				t.Fatalf("exit %d, want 5: %s", code, errOut)
			}
			// The path the patch wrote for the create that failed.
			failed := c.patch[strings.LastIndex(c.patch, "@@ create ")+len("@@ create "):]
			failed = failed[:strings.Index(failed, "\n")]
			if !strings.HasPrefix(errOut, "hunk: creating "+failed+" failed: ") {
				t.Errorf("does not name %s as the file that failed:\n%s", failed, errOut)
			}
			if !strings.HasSuffix(errOut, c.says+"\n") {
				t.Errorf("want it to end %q:\n%s", c.says, errOut)
			}
			if strings.Contains(errOut, ".hunk-") {
				t.Errorf("names the temp file:\n%s", errOut)
			}
			assertUnchanged(t, root, before) // directories and modes included
		})
	}

	t.Run("--json carries the same sentence", func(t *testing.T) {
		root := cliTree(t, map[string]string{"keep.txt": "keep\n"})
		_, js, _ := runCLI(t, root, []string{"--json"}, "@@ create first.txt\nf\n\n@@ create newdir/"+long+"\nx\n\n")
		var v struct {
			Exit  int
			Error string
		}
		must(t, json.Unmarshal([]byte(js), &v))
		if v.Exit != exitIO || !strings.HasSuffix(v.Error, "the file written before it was put back, so nothing was written") {
			t.Errorf("%s", js)
		}
	})
}

// The one way the undo can fall short is the way rollback can: a file commit
// wrote that something else changed before commit could put it back, or a put
// back that fails itself. Neither can be arranged between two renames from
// outside, so the report is asserted on the error the transaction returns.
func TestACommitThatCannotPutBackSaysWhere(t *testing.T) {
	err := &CommitError{
		Path: "newdir/x.go", Op: "create", Err: errors.New("no space left on device"), Restored: 1,
		NotRestored: []NotRestored{{Path: "a.go", Reason: "It could not be written: no space left on device"}},
	}
	want := "creating newdir/x.go failed: no space left on device; 1 of the 2 files written before it could not be put back"
	if err.Error() != want {
		t.Errorf("got  %q\nwant %q", err.Error(), want)
	}
	rep := NewReport(nil, err, nil, false, 2)
	if rep.Exit != exitIO {
		t.Errorf("exit %d, want 5", rep.Exit)
	}
	var out, errOut bytes.Buffer
	rep.Text(&out, &errOut, false)
	for _, line := range []string{
		"hunk: " + want,
		"a.go was not restored.",
		"  It could not be written: no space left on device",
	} {
		if !strings.Contains(errOut.String(), line+"\n") {
			t.Errorf("missing %q:\n%s", line, errOut.String())
		}
	}
	var js bytes.Buffer
	must(t, rep.JSON(&js))
	var v struct {
		NotRestored []struct{ Path, Reason string } `json:"not_restored"`
	}
	must(t, json.Unmarshal(js.Bytes(), &v))
	if len(v.NotRestored) != 1 || v.NotRestored[0].Path != "a.go" {
		t.Errorf("--json does not carry what was not put back: %s", js.String())
	}
	// The cause, without the operation and the paths: a rename names the temp
	// file, a remove the target, and an error of hunk's own names neither.
	boom := errors.New("boom")
	for _, err := range []error{
		&os.LinkError{Op: "rename", Old: ".hunk-x.tmp", New: "x", Err: boom},
		&fs.PathError{Op: "remove", Path: "x", Err: boom},
		boom,
	} {
		if got := cause(err); got != boom {
			t.Errorf("cause(%v) = %v, want %v", err, got, boom)
		}
	}
}

// The check phase is asserted at the phase level above, by calling Check
// directly, because Run closes the window itself and nothing can write between
// its phases from outside. So nothing asserted that Run calls it: deleting the
// call failed no test, and the concurrency test below cannot see it either,
// since two runs started together almost never meet the check anyway. A
// mutation check found the gap on 2026-09-21. The order of Run's phases is a
// claim the compiler does not check, so this does.
func TestRunChecksBetweenValidatingAndCommitting(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "apply.go", nil, 0)
	must(t, err)
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "Run" && fd.Recv != nil {
			run = fd
		}
	}
	if run == nil {
		t.Fatal("apply.go has no Txn.Run")
	}
	recv := run.Recv.List[0].Names[0].Name
	var calls []string
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
					calls = append(calls, sel.Sel.Name)
				}
			}
		}
		return true
	})
	if want := []string{"Load", "Validate", "Check", "Commit", "result"}; !slices.Equal(calls, want) {
		t.Errorf("Run calls %v on the transaction, want %v: §6.1's phases, in order, each once", calls, want)
	}

	// And a phase that fails stops Run: each of these is called as
	// "if err := x.Phase(); err != nil { return nil, err }". A call whose error
	// is dropped keeps the order above and refuses nothing.
	stops := map[string]bool{}
	for _, st := range run.Body.List {
		ifs, ok := st.(*ast.IfStmt)
		if !ok || ifs.Init == nil || len(ifs.Body.List) == 0 {
			continue
		}
		as, ok := ifs.Init.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			continue
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		ret, ok := ifs.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			continue
		}
		errName := as.Lhs[0].(*ast.Ident).Name
		if last, ok := ret.Results[len(ret.Results)-1].(*ast.Ident); ok && last.Name == errName {
			stops[sel.Sel.Name] = true
		}
	}
	for _, phase := range []string{"Load", "Check", "Commit"} {
		if !stops[phase] {
			t.Errorf("Run does not return when %s fails", phase)
		}
	}
}

// §8.1's concurrency test, as §6.2 can keep it, decided 2026-09-21. Two hunk
// processes started together on the same files, each replacing the same line in
// every file with its own tag.
//
// The check above catches a writer that finished between a run's read and its
// write. It does not catch one writing at the same time, and two processes
// started together nearly always both pass it before either renames, after
// which their renames interleave file by file. So neither "exactly one exits
// 0" nor "every file came from the same process" is a property this tool has,
// and a test asserting either would fail at random. What holds on every run is
// asserted, and the rest is counted and logged.
func TestTwoProcessesOnTheSameFiles(t *testing.T) {
	exe, err := os.Executable()
	must(t, err)
	const nFiles, trials = 4, 40
	var names []string
	start := map[string]string{}
	for i := range nFiles {
		n := fmt.Sprintf("f%d.txt", i)
		names = append(names, n)
		start[n] = "base\n"
	}
	patch := func(tag string) string {
		var b strings.Builder
		for _, n := range names {
			fmt.Fprintf(&b, "@@ file %s\n@@ old\nbase\n@@ new\n%s\n", n, tag)
		}
		return b.String()
	}
	launch := func(root, tag string) *exec.Cmd {
		cmd := exec.Command(exe, "--root", root, "--quiet")
		cmd.Env = append(os.Environ(), "HUNK_TEST_AS_HUNK=1")
		cmd.Stdin = strings.NewReader(patch(tag))
		must(t, cmd.Start())
		return cmd
	}
	exitOf := func(err error) int {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		must(t, err)
		return exitOK
	}

	seen := map[string]int{}
	for trial := range trials {
		root := cliTree(t, start)
		a, b := launch(root, "A"), launch(root, "B")
		ea, eb := exitOf(a.Wait()), exitOf(b.Wait())

		// Every file is one process's whole version. Never a mix, and never
		// the original, since at least one process always writes everything.
		whose := map[string]int{}
		for _, n := range names {
			got := readFile(t, root, n)
			if got != "A\n" && got != "B\n" {
				t.Fatalf("trial %d: %s is %q, which is neither process's version (exits %d, %d)", trial, n, got, ea, eb)
			}
			whose[strings.TrimSpace(got)]++
		}
		// 2: it read the other's result, so its old was gone. 6: the other
		// wrote between its read and its check.
		for _, e := range []int{ea, eb} {
			if e != exitOK && e != exitNoMatch && e != exitChanged {
				t.Fatalf("trial %d: exits %d and %d; each should be 0, 2 or 6", trial, ea, eb)
			}
		}
		switch {
		case ea != exitOK && eb != exitOK:
			t.Fatalf("trial %d: both refused (%d, %d), but a refusal needs a write to be refused for", trial, ea, eb)
		case ea != exitOK || eb != exitOK:
			// A refused process wrote nothing, so the tree is the other's.
			winner := "A"
			if ea != exitOK {
				winner = "B"
			}
			if whose[winner] != nFiles {
				t.Fatalf("trial %d: exits %d and %d, but the tree is %v, not all %s", trial, ea, eb, whose, winner)
			}
			seen["one refused"]++
		case whose["A"] == nFiles || whose["B"] == nFiles:
			seen["both wrote, one everywhere"]++
		default:
			seen["both wrote, a blend"]++
		}
	}
	t.Logf("%d pairs of %d-file batches: %v", trials, nFiles, seen)
}

func TestEOL(t *testing.T) {
	t.Run("an LF payload edits a CRLF file and it stays CRLF", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\r\ntwo\r\nthree\r\n"})
		_, err := run(t, tree, "@@ file a.go\n@@ old\ntwo\n@@ new\nTWO\nEXTRA\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "one\r\nTWO\r\nEXTRA\r\nthree\r\n" {
			t.Errorf("got %q", got)
		}
	})

	// A payload's own line endings are the CRLFs inside it, and a patch is
	// split on \n, so a payload line that ends CRLF is written as a line ending
	// in \r. The blank line before the next directive is what gives the last
	// payload line its ending at all (§3.3). Getting this wrong is how the
	// first draft of these two tests tested nothing.
	crlfPayload := func(lines ...string) string {
		return strings.Join(lines, "\r\n") + "\r\n\n"
	}

	// A heredoc from an agent's shell is always LF, so a CRLF payload means
	// somebody pasted CRLF text. Converting LF to CRLF on it would give
	// "\r\r\n"; probed during recon, and this is what keeps it fixed.
	t.Run("a CRLF payload on a CRLF file gains no extra carriage return", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\r\ntwo\r\n"})
		patch := "@@ file a.go\n@@ old\n" + crlfPayload("two") + "@@ new\n" + crlfPayload("TWO")
		_, err := run(t, tree, patch, Options{})
		must(t, err)
		got := readFile(t, root, "a.go")
		if strings.Contains(got, "\r\r") {
			t.Errorf("doubled carriage return: %q", got)
		}
		if got != "one\r\nTWO\r\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a CRLF payload edits an LF file and is converted down", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\ntwo\n"})
		patch := "@@ file a.go\n@@ old\n" + crlfPayload("two") + "@@ new\n" + crlfPayload("TWO")
		_, err := run(t, tree, patch, Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "one\nTWO\n" {
			t.Errorf("got %q", got)
		}
	})

	// A bare CR that is not a line ending is content, and byte-exact matching
	// leaves it alone in both directions.
	t.Run("a lone carriage return is content, not a line ending", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "a\rb\n"})
		_, err := run(t, tree, "@@ file a.txt\n@@ old\na\rb\n@@ new\nc\rd\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.txt"); got != "c\rd\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("strict does not translate, so an LF payload misses a CRLF file", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\r\ntwo\r\n"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ file a.go\n@@ old\ntwo\n\n@@ new\nTWO\n\n", Options{EOL: EOLStrict})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("strict should have missed; got %T: %v", err, err)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("dominantEOL", func(t *testing.T) {
		for _, c := range []struct {
			in   string
			want string
		}{
			{"", "\n"},
			{"no newline at all", "\n"},
			{"a\nb\n", "\n"},
			{"a\r\nb\r\n", "\r\n"},
			{"a\r\nb\r\nc\n", "\r\n"},
			{"a\r\nb\nc\n", "\n"},
			{"a\r\nb\n", "\n"}, // a tie goes to LF
		} {
			if got := dominantEOL([]byte(c.in)); got != c.want {
				t.Errorf("dominantEOL(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	})

	t.Run("toEOL is idempotent in both directions", func(t *testing.T) {
		for _, in := range []string{"a\nb", "a\r\nb", "a\r\nb\nc", ""} {
			for _, eol := range []string{"\n", "\r\n"} {
				once := toEOL([]byte(in), eol)
				twice := toEOL(once, eol)
				if string(once) != string(twice) {
					t.Errorf("toEOL(%q, %q) = %q, applying twice = %q", in, eol, once, twice)
				}
			}
		}
	})
}

// §5.1's example is self-consistent under exactly this rule: added is the
// number of lines in new, removed the number in old, summed per file. Its
// "3 files, 4 hunks, +8 -3" is 2+2+4 and 1+2+0.
func TestLineCountAndDiffstat(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"\n", 1},
		{"\n\n", 2},
	} {
		if got := lineCount([]byte(c.in)); got != c.want {
			t.Errorf("lineCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	tree, _ := fixture(t, map[string]string{"a.go": "one\ntwo\nthree\n"})
	r, err := run(t, tree, "@@ file a.go\n@@ old\none\n@@ new\nONE\nEXTRA\n", Options{})
	must(t, err)
	if r.Files[0].Added != 2 || r.Files[0].Removed != 1 {
		t.Errorf("diffstat = +%d -%d, want +2 -1", r.Files[0].Added, r.Files[0].Removed)
	}
}

// §8.1's property test, with the two conditions the recon found it was missing.
// A randomly extracted span is not necessarily unique, so the hunk carries the
// count it actually occurs at; and the replacement is drawn from an alphabet
// disjoint from the file's, so inverting it cannot match more than was written.
func TestApplyThenInvertIsIdentity(t *testing.T) {
	const fileAlpha = "abc \n\t{}"
	const replAlpha = "XYZ"
	rng := rand.New(rand.NewPCG(1, 2))

	pick := func(alpha string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(alpha[rng.IntN(len(alpha))])
		}
		return b.String()
	}

	for i := 0; i < 400; i++ {
		body := pick(fileAlpha, 1+rng.IntN(120))
		if len(body) < 2 {
			continue
		}
		start := rng.IntN(len(body) - 1)
		end := start + 1 + rng.IntN(len(body)-start)
		span := body[start:end]
		repl := pick(replAlpha, 1+rng.IntN(4))
		n := strings.Count(body, span)
		if n == 0 {
			t.Fatalf("extracted span %q does not occur in %q", span, body)
		}

		tree, root := fixture(t, map[string]string{"f.txt": body})
		// A payload is the lines between two directives joined by \n, so
		// emitting the span followed by a newline encodes it exactly.
		fwd := "@@ file f.txt\n@@ old x" + itoa(n) + "\n" + span + "\n@@ new\n" + repl + "\n"
		if _, err := run(t, tree, fwd, Options{}); err != nil {
			t.Fatalf("apply %q -> %q in %q: %v", span, repl, body, err)
		}
		back := "@@ file f.txt\n@@ old x" + itoa(n) + "\n" + repl + "\n@@ new\n" + span + "\n"
		if _, err := run(t, tree, back, Options{}); err != nil {
			t.Fatalf("invert %q -> %q in %q: %v", repl, span, body, err)
		}
		if got := readFile(t, root, "f.txt"); got != body {
			t.Fatalf("round trip changed the file:\n span %q\n repl %q\n want %q\n got  %q",
				span, repl, body, got)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

// A hunk that matched but changed nothing must not rewrite the file: moving an
// mtime invalidates a build cache, and the whole point of the tool is to make
// the next step cheaper.
func TestANoOpHunkDoesNotTouchTheFile(t *testing.T) {
	tree, root := fixture(t, map[string]string{"a.go": "one\ntwo\n"})
	full := filepath.Join(root, "a.go")
	fi0, err := os.Stat(full)
	must(t, err)

	r, err := run(t, tree, "@@ file a.go\n@@ old\none\n@@ new\none\n", Options{})
	must(t, err)
	if len(r.Files) != 0 {
		t.Errorf("a no-op hunk was reported as a change: %+v", r.Files)
	}
	if r.Hunks != 1 {
		t.Errorf("hunks = %d, want 1", r.Hunks)
	}
	fi1, err := os.Stat(full)
	must(t, err)
	if !fi1.ModTime().Equal(fi0.ModTime()) {
		t.Error("the file was rewritten")
	}
	if got := readFile(t, root, "a.go"); got != "one\ntwo\n" {
		t.Errorf("contents changed: %q", got)
	}

	// And a real change beside a no-op still lands, with the no-op absent from
	// the diffstat.
	r, err = run(t, tree, "@@ file a.go\n@@ old\none\n@@ new\none\n@@ old\ntwo\n@@ new\nTWO\n", Options{})
	must(t, err)
	if len(r.Files) != 1 || r.Files[0].Added != 1 || r.Files[0].Removed != 1 {
		t.Errorf("diffstat = %+v, want one file at +1 -1", r.Files)
	}
	if got := readFile(t, root, "a.go"); got != "one\nTWO\n" {
		t.Errorf("got %q", got)
	}
}

func TestLoadErrorPaths(t *testing.T) {
	t.Run("a path that escapes the root is refused before anything is read", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"a.go": "x\n"})
		_, err := run(t, tree, "@@ file ../outside.go\n@@ old\nx\n@@ new\ny\n", Options{})
		var pr *PathRefusal
		if !errors.As(err, &pr) {
			t.Fatalf("want *PathRefusal, got %T: %v", err, err)
		}
		if !strings.Contains(pr.Reason, "climbs out") {
			t.Errorf("reason = %q", pr.Reason)
		}
	})

	// Stat succeeds (the directory is traversable) and the read fails. That is
	// an I/O error, exit 5, not a validation failure: editing the patch would
	// not help, a chmod would.
	t.Run("an unreadable file is an I/O error, not a refusal", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "x\n"})
		must(t, os.Chmod(filepath.Join(root, "a.go"), 0o000))
		t.Cleanup(func() { os.Chmod(filepath.Join(root, "a.go"), 0o644) })

		_, err := run(t, tree, "@@ file a.go\n@@ old\nx\n@@ new\ny\n", Options{})
		if err == nil {
			t.Fatal("read an unreadable file")
		}
		var pr *PathRefusal
		if errors.As(err, &pr) {
			t.Errorf("a permission problem was reported as a path refusal: %s", pr.Reason)
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("err = %v, want a permission error", err)
		}
	})

	t.Run("an unstattable parent is an I/O error too", func(t *testing.T) {
		needsPOSIXPerms(t)
		tree, root := fixture(t, map[string]string{"locked/a.go": "x\n"})
		must(t, os.Chmod(filepath.Join(root, "locked"), 0o000))
		t.Cleanup(func() { os.Chmod(filepath.Join(root, "locked"), 0o755) })

		_, err := run(t, tree, "@@ file locked/a.go\n@@ old\nx\n@@ new\ny\n", Options{})
		if err == nil {
			t.Fatal("statted through a locked directory")
		}
		var pr *PathRefusal
		if errors.As(err, &pr) {
			t.Errorf("reported as a path refusal: %s", pr.Reason)
		}
	})
}

// Commit is the one phase that can fail after everything has been validated.
// A read-only directory reaches it without any interleaving: load reads,
// validate matches, check re-reads, and the temp file cannot be created.
func TestCommitFailureSurfaces(t *testing.T) {
	needsPOSIXPerms(t)
	tree, root := fixture(t, map[string]string{"sub/a.go": "x\n"})
	must(t, os.Chmod(filepath.Join(root, "sub"), 0o555))
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "sub"), 0o755) })

	_, err := run(t, tree, "@@ file sub/a.go\n@@ old\nx\n@@ new\ny\n", Options{})
	if err == nil {
		t.Fatal("wrote into a read-only directory")
	}
	if got := readFile(t, root, "sub/a.go"); got != "x\n" {
		t.Errorf("contents changed to %q", got)
	}
}

// Run's Check branch. Driving the phases separately is the only way to get a
// writer in between, which is the point of the phase being separate.
func TestRunPropagatesACheckFailure(t *testing.T) {
	tree, root := fixture(t, map[string]string{"a.go": "one\n"})
	p, err := Parse([]byte("@@ file a.go\n@@ old\none\n@@ new\nONE\n"), DefaultMarker)
	must(t, err)
	x := NewTxn(tree, Options{})
	must(t, x.Load(p))
	x.Validate(p)
	must(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("else\n"), 0o644))

	if err := x.Check(); err == nil {
		t.Fatal("check passed over a changed file")
	}
	if err := x.Commit(); err != nil {
		t.Fatalf("commit after a failed check: %v", err)
	}
	// Run refuses the same patch on a second Txn, having re-read the file.
	if _, err := run(t, tree, "@@ file a.go\n@@ old\none\n@@ new\nONE\n", Options{}); err == nil {
		t.Error("the stale patch applied")
	}
}

func TestErrorMessages(t *testing.T) {
	ce := &ChangedError{Path: "b.go"}
	for _, want := range []string{"b.go", "between being read and being written", "nothing was written"} {
		if !strings.Contains(ce.Error(), want) {
			t.Errorf("%q missing %q", ce.Error(), want)
		}
	}
	ve := &ValidationError{Failures: []Failure{{Hunk: 1}, {Hunk: 2, SkippedAfter: 1}}}
	if got := ve.Error(); !strings.Contains(got, "1 hunks did not match") || !strings.Contains(got, "1 skipped") {
		t.Errorf("summary = %q", got)
	}
	if got := (&ValidationError{Failures: []Failure{{Hunk: 1}}}).Error(); strings.Contains(got, "skipped") {
		t.Errorf("no skips should mean no skip clause: %q", got)
	}
}

// §2 goal 3 is "All or nothing": after a failure the tree is byte-identical to
// before. That is the strongest invariant in the design, the patch is
// agent-written text, and the two together make this the fuzz target worth
// having.
func FuzzApplyIsAllOrNothing(f *testing.F) {
	for _, s := range []string{
		"@@ file a.go\n@@ old\none\n@@ new\nONE\n",
		"@@ file a.go\n@@ old x2\nx\n@@ new\ny\n",
		"@@ file a.go\n@@ old\nabsent\n@@ new\nx\n@@ old\none\n@@ new\nX\n",
		"@@ file sub/b.go\n@@ old\nbeta\n@@ new\n\n",
		"@@ file ../escape\n@@ old\nx\n@@ new\ny\n",
		"@@ delete a.go\n",
		formatExamplePatch,
		// The sweep's first find (issue #11), byte for byte: a file, then a
		// file inside it. Both are absent at load, so both used to validate.
		"@@ create internal\n@@ create internal/0",
		// Found by fuzzing the fix for it: a NUL byte under a directory the
		// batch creates, which load's stat never reached.
		"@@ create 0\n@@ create 1/\x00",
		// A path through a file already on disk, which aborted load as exit 5
		// until 2026-09-21 and is refused per hunk now, with and without the
		// batch deleting the file.
		"@@ create a.go/0\n",
		"@@ delete a.go\n@@ create a.go/0\n",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, patch string) {
		root := t.TempDir()
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
			t.Skip()
		}
		os.WriteFile(filepath.Join(root, "a.go"), []byte("one\ntwo\nx\nx\n"), 0o644)
		os.WriteFile(filepath.Join(root, "sub", "b.go"), []byte("alpha\r\nbeta\r\n"), 0o600)

		p, err := Parse([]byte(patch), DefaultMarker)
		if err != nil {
			return
		}
		tree, err := OpenTree(root, false)
		if err != nil {
			t.Fatal(err)
		}
		defer tree.Close()

		before := snapshot(t, root)
		// A dry run first, on the same tree, since it writes nothing.
		preview, previewErr := NewTxn(tree, Options{}).Preview(p)
		assertUnchanged(t, root, before)
		r, err := NewTxn(tree, Options{}).Run(p)

		// A dry run is a promise, as far as validation can make one. Where it
		// refuses, the apply refuses the same way. Where it succeeds, the apply
		// succeeds with the same diffstat, or fails in commit at exit 5, which
		// is what a filesystem refusing a name looks like: nothing before the
		// rename can know.
		switch {
		case previewErr != nil && ExitCode(err) != ExitCode(previewErr):
			t.Errorf("the dry run exits %d and the apply %d", ExitCode(previewErr), ExitCode(err))
		case previewErr == nil && err == nil && !slices.Equal(preview.Files, r.Files):
			t.Errorf("the dry run reports %+v and the apply %+v", preview.Files, r.Files)
		case previewErr == nil && err != nil && ExitCode(err) != exitIO:
			t.Errorf("the dry run succeeds and the apply exits %d: %v", ExitCode(err), err)
		}

		if err != nil {
			// Every failure path leaves the tree exactly as it was, directories
			// included. A commit that fails part-way used to be the exception
			// §6.2 admitted, and the patch alone can make one: a file and then
			// a file inside it did, and a NUL byte under a directory the batch
			// creates, until each was refused before commit (issue #11). A name
			// too long for the filesystem still fails there. Since 2026-09-21
			// commit puts back what it wrote first, so exit 5 has no exemption
			// here any more.
			assertUnchanged(t, root, before)
			return
		}
		if r == nil {
			t.Fatal("success with no result")
		}
		if r.Hunks != len(p.Hunks) {
			t.Errorf("result reports %d hunks, patch had %d", r.Hunks, len(p.Hunks))
		}
		// A success reports every file it changed and no file it did not.
		after := snapshot(t, root)
		reported := map[string]bool{}
		for _, fr := range r.Files {
			reported[fr.Path] = true
		}
		for name := range before {
			if before[name] != after[name] && len(reported) == 0 {
				t.Errorf("%s changed but the result reported no files", name)
			}
		}
	})
}

// Two spellings of one file are one file (§3.5), for any patch. The property is
// metamorphic: a patch naming paths through the directory link d -> sub, and
// the same patch with each of them spelled through sub instead, have to do
// exactly the same thing to the tree. Until 2026-09-17 they did not, and the
// commonest difference was an edit lost under exit 0.
func FuzzASpellingThroughALinkIsTheSameFile(f *testing.F) {
	for _, s := range []string{
		"@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file d/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
		"@@ create sub/n.go\none\n\n@@ create d/n.go\ntwo\n\n",
		"@@ create d/n.go\nalpha\n\n@@ file sub/n.go\n@@ old\nalpha\n@@ new\nbeta\n",
		"@@ delete sub/b.go\n@@ file d/b.go\n@@ old\nalpha\n@@ new\nALPHA\n",
		"@@ delete sub/b.go\n@@ delete d/b.go\n",
		"@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ append d/b.go\ngamma\n\n",
		"@@ create d/x\n@@ create sub/x/y\n",
		"@@ file d/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, patch string) {
		p, err := Parse([]byte(patch), DefaultMarker)
		if err != nil {
			return
		}
		respelled := respellThroughSub(patch)
		q, err := Parse([]byte(respelled), DefaultMarker)
		if err != nil {
			t.Fatalf("respelling made the patch unparseable: %v\n%q", err, respelled)
		}
		linkExit, linkTree := applyToLinkedTree(t, p)
		subExit, subTree := applyToLinkedTree(t, q)
		if linkExit != subExit {
			t.Fatalf("exit %d through the link, %d through sub\n%q\n%q", linkExit, subExit, patch, respelled)
		}
		if !maps.Equal(linkTree, subTree) {
			t.Fatalf("the trees differ\nthrough the link: %q\nthrough sub:      %q\npatch: %q", linkTree, subTree, patch)
		}
	})
}

// respellThroughSub names every path a directive names under d/ under sub/
// instead, and leaves every other byte alone. It asks the parser's own test
// which lines are directives, so no payload line is ever touched.
func respellThroughSub(patch string) string {
	lines := strings.Split(patch, "\n")
	for i, line := range lines {
		word, arg, ok := directive([]byte(line), DefaultMarker)
		if !ok || !strings.HasPrefix(arg, "d/") {
			continue
		}
		switch word {
		case "file", "create", "delete", "append", "prepend":
			lines[i] = DefaultMarker + " " + word + " sub/" + strings.TrimPrefix(arg, "d/")
		}
	}
	return strings.Join(lines, "\n")
}

// applyToLinkedTree applies p to a fresh tree of sub/b.go, a.go and the
// directory link d -> sub, and returns the exit and the whole tree afterwards:
// files with their contents and modes, links, and directories.
func applyToLinkedTree(t *testing.T, p *Patch) (int, map[string]string) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	must(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "sub", "b.go"), []byte("alpha\nbeta\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("one\n"), 0o644))
	must(t, os.Symlink("sub", filepath.Join(root, "d")))
	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()

	_, runErr := NewTxn(tree, Options{}).Run(p)
	return ExitCode(runErr), snapshot(t, root)
}

// An "@@ old x2" that rewrites two lines changed two lines. Counting per hunk
// would say +1 -1 and understate it; §5.1's globals.go row is +2 -2 for exactly
// this shape.
func TestDiffstatCountsPerOccurrence(t *testing.T) {
	tree, _ := fixture(t, map[string]string{"a.go": "P\nq\nP\nr\nP\n"})
	r, err := run(t, tree, "@@ file a.go\n@@ old x3\nP\n@@ new\nZ\nY\n", Options{})
	must(t, err)
	if r.Files[0].Added != 6 || r.Files[0].Removed != 3 {
		t.Errorf("diffstat = +%d -%d, want +6 -3 (three occurrences, one line each becoming two)",
			r.Files[0].Added, r.Files[0].Removed)
	}
}

// §6.1 step 2's three refusals are the ones an agent reaches by writing an
// ordinary patch, and until 2026-09-06 they were the only three that reached it
// without the root, because Validate stored err.Reason and dropped what the
// PathRefusal knew. The two that abort the load never lost it.
//
// The fourth per-hunk refusal came from the nightly fuzz sweep on 2026-09-17,
// and is listed so that it cannot be the one that forgets.
func TestEveryRefusalNamesTheRootItLookedIn(t *testing.T) {
	for _, c := range []struct{ name, patch, want string }{
		{"a missing file", "@@ file gone.go\n@@ old\nx\n@@ new\ny\n", "no such file"},
		{"a create over a file that is there", "@@ create a.go\nz\n\n", "already exists"},
		{"a replace after a delete", "@@ delete a.go\n@@ old\nx\n@@ new\ny\n", "earlier hunk in this batch deleted it"},
		{"a create inside a file the batch creates", "@@ create new\n@@ create new/x\n", "which hunk 1 creates as a file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"a.go": "x\n"})
			_, err := run(t, tree, c.patch, Options{})
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %T: %v", err, err)
			}
			got := ve.Failures[len(ve.Failures)-1].Refusal
			if !strings.Contains(got, c.want) {
				t.Errorf("refusal = %q, want it to say %q", got, c.want)
			}
			if !strings.Contains(got, root) {
				t.Errorf("refusal = %q, want it to name the root %q", got, root)
			}
		})
	}
}
