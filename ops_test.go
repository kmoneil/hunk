package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func absent(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s should not exist", name)
	}
}

func TestCreateDeleteAppendPrepend(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "x\n"})
		r, err := run(t, tree, "@@ create new.txt\nhello\n\n", Options{})
		must(t, err)
		if got := readFile(t, root, "new.txt"); got != "hello\n" {
			t.Errorf("got %q", got)
		}
		if r.Files[0].Op != "create" || r.Files[0].Added != 1 || r.Files[0].Removed != 0 {
			t.Errorf("result = %+v", r.Files[0])
		}
	})

	// §6.6 does not say what mode a created file gets. 0644 before umask is the
	// only sane default; a created shell script wants 0755 and the format has
	// no way to ask.
	t.Run("a created file is 0644", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "x\n"})
		must(t, run2(t, tree, "@@ create s.sh\n#!/bin/sh\n\n"))
		fi, err := os.Stat(filepath.Join(root, "s.sh"))
		must(t, err)
		assertMode(t, fi.Mode(), createMode)
	})

	t.Run("create makes missing directories", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "x\n"})
		must(t, run2(t, tree, "@@ create a/b/c/deep.txt\nhi\n\n"))
		if got := readFile(t, root, "a/b/c/deep.txt"); got != "hi\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("delete", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "one\ntwo\n", "b.txt": "keep\n"})
		r, err := run(t, tree, "@@ delete a.txt\n", Options{})
		must(t, err)
		absent(t, root, "a.txt")
		if readFile(t, root, "b.txt") != "keep\n" {
			t.Error("b.txt was touched")
		}
		if r.Files[0].Op != "delete" || r.Files[0].Removed != 2 || r.Files[0].Added != 0 {
			t.Errorf("result = %+v", r.Files[0])
		}
	})

	t.Run("append and prepend", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "middle\n"})
		must(t, run2(t, tree, "@@ append a.txt\nlast\n\n@@ prepend a.txt\nfirst\n\n"))
		if got := readFile(t, root, "a.txt"); got != "first\nmiddle\nlast\n" {
			t.Errorf("got %q", got)
		}
	})

	// §3.5: a @@ old hunk may follow a create and apply to the new content.
	t.Run("a replace against a file the batch just created", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "x\n"})
		must(t, run2(t, tree, "@@ create new.txt\nalpha\n\n@@ old\nalpha\n@@ new\nBETA\n"))
		if got := readFile(t, root, "new.txt"); got != "BETA\n" {
			t.Errorf("got %q", got)
		}
	})
}

func run2(t *testing.T, tree *Tree, patch string) error {
	t.Helper()
	_, err := run(t, tree, patch, Options{})
	return err
}

// The card's ordering table. The rule underneath is that the batch has one
// final state per path, and commit writes that state rather than the sequence
// that produced it (§6.6).
func TestOrderingAcrossOpsOnOnePath(t *testing.T) {
	for _, c := range []struct {
		name    string
		start   map[string]string
		patch   string
		wantErr string // a substring of the refusal, or "" for success
		check   func(t *testing.T, root string)
	}{
		{
			name:  "delete then create is one modify",
			start: map[string]string{"a.txt": "old\n"},
			patch: "@@ delete a.txt\n@@ create a.txt\nbrand new\n\n",
			check: func(t *testing.T, root string) {
				if got := readFile(t, root, "a.txt"); got != "brand new\n" {
					t.Errorf("got %q", got)
				}
			},
		},
		{
			name:  "create then delete touches nothing at all",
			start: map[string]string{"a.txt": "x\n"},
			patch: "@@ create new.txt\nhi\n\n@@ delete new.txt\n",
			check: func(t *testing.T, root string) { absent(t, root, "new.txt") },
		},
		{
			name:  "a replace then a delete leaves the file deleted",
			start: map[string]string{"a.txt": "one\n"},
			patch: "@@ file a.txt\n@@ old\none\n@@ new\nONE\n@@ delete a.txt\n",
			check: func(t *testing.T, root string) { absent(t, root, "a.txt") },
		},
		{
			name:  "create then append",
			start: map[string]string{"a.txt": "x\n"},
			patch: "@@ create new.txt\nfirst\n\n@@ append new.txt\nsecond\n\n",
			check: func(t *testing.T, root string) {
				if got := readFile(t, root, "new.txt"); got != "first\nsecond\n" {
					t.Errorf("got %q", got)
				}
			},
		},
		{
			name:    "create over a file that exists",
			start:   map[string]string{"a.txt": "x\n"},
			patch:   "@@ create a.txt\nnew\n\n",
			wantErr: "already exists",
		},
		{
			name:    "a replace after a delete",
			start:   map[string]string{"a.txt": "one\n"},
			patch:   "@@ delete a.txt\n@@ old\none\n@@ new\nONE\n",
			wantErr: "earlier hunk in this batch deleted it",
		},
		{
			name:    "delete twice",
			start:   map[string]string{"a.txt": "one\n"},
			patch:   "@@ delete a.txt\n@@ delete a.txt\n",
			wantErr: "earlier hunk in this batch deleted it",
		},
		{
			name:    "delete a file that is not there",
			start:   map[string]string{"a.txt": "x\n"},
			patch:   "@@ delete gone.txt\n",
			wantErr: "no such file",
		},
		{
			name:    "append to a file that is not there",
			start:   map[string]string{"a.txt": "x\n"},
			patch:   "@@ append gone.txt\nhi\n\n",
			wantErr: "no such file",
		},
		{
			name:    "prepend to a file that is not there",
			start:   map[string]string{"a.txt": "x\n"},
			patch:   "@@ prepend gone.txt\nhi\n\n",
			wantErr: "no such file",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, c.start)
			before := snapshot(t, root)
			err := run2(t, tree, c.patch)
			if c.wantErr == "" {
				must(t, err)
				c.check(t, root)
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want a refusal, got %T: %v", err, err)
			}
			found := false
			for _, f := range ve.Failures {
				if strings.Contains(f.Refusal, c.wantErr) {
					found = true
				}
			}
			if !found {
				t.Errorf("no refusal mentioning %q: %+v", c.wantErr, ve.Failures)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The two-path half of the table above, and the nightly fuzz sweep's first find
// (issue #11). A batch that needed one path to be a file and a directory passed
// validation, because both paths were absent at load, and commit wrote the
// first, failed on the second, and left the first behind: exit 5, and a syscall
// naming a temp file the agent never wrote.
//
// The rule reads the final state, as commit does (§6.6). The rows that apply
// reach the same pairs of names and work, and a check that walked the sequence
// instead would refuse them.
func TestOneBatchCannotMakeAPathBothAFileAndADirectory(t *testing.T) {
	type failure struct {
		hunk, line int
		path       string
		says       string // a substring of the refusal; "" for a failure that is not one
		skipped    int    // SkippedAfter
	}
	for _, c := range []struct {
		name       string
		patch      string
		want       []failure // every failure, in order; nil means the batch applies
		files      []string  // when it applies: regular files it leaves
		dryRun     bool
		unconfined bool
	}{
		{
			name:  "a create inside a file an earlier hunk creates",
			patch: "@@ create internal\n@@ create internal/0\n",
			want:  []failure{{2, 2, "internal/0", "it is inside internal, which hunk 1 creates as a file", 0}},
		},
		{
			name:  "a file created where an earlier hunk needs a directory",
			patch: "@@ create internal/0\n@@ create internal\n",
			want:  []failure{{2, 2, "internal", "hunk 1 creates internal/0 inside it, so it cannot also be a file", 0}},
		},
		{
			name:  "a create two levels inside a file",
			patch: "@@ create x\n@@ create x/y/z\n",
			want:  []failure{{2, 2, "x/y/z", "it is inside x, which hunk 1 creates as a file", 0}},
		},
		{
			name:  "a file created two levels above an earlier create",
			patch: "@@ create x/y/z\n@@ create x\n",
			want:  []failure{{2, 2, "x", "hunk 1 creates x/y/z inside it, so it cannot also be a file", 0}},
		},
		{
			name:  "the same directory spelled two ways",
			patch: "@@ create internal\n@@ create ./internal/0\n",
			want:  []failure{{2, 2, "./internal/0", "it is inside internal, which hunk 1 creates as a file", 0}},
		},
		{
			name:  "named by the hunk that created it, not the last to touch it",
			patch: "@@ create internal\nalpha\n\n@@ append internal\nbeta\n\n@@ create internal/0\nz\n",
			want:  []failure{{3, 7, "internal/0", "it is inside internal, which hunk 1 creates as a file", 0}},
		},
		{
			name:  "every create inside one file is refused",
			patch: "@@ create a\n@@ create a/b\n@@ create a/c\n",
			want: []failure{
				{2, 2, "a/b", "it is inside a, which hunk 1 creates as a file", 0},
				{3, 3, "a/c", "it is inside a, which hunk 1 creates as a file", 0},
			},
		},
		{
			name:  "a hunk in several conflicts names the earliest",
			patch: "@@ create a/b/c\n@@ create a/b\n@@ create a\n",
			want: []failure{
				{2, 2, "a/b", "hunk 1 creates a/b/c inside it, so it cannot also be a file", 0},
				{3, 3, "a", "hunk 1 creates a/b/c inside it, so it cannot also be a file", 0},
			},
		},
		{
			name:  "reported in hunk order among the other failures",
			patch: "@@ create internal\n@@ create internal/0\n@@ file a.txt\n@@ old\nabsent\n@@ new\ny\n",
			want: []failure{
				{2, 2, "internal/0", "it is inside internal, which hunk 1 creates as a file", 0},
				{3, 4, "a.txt", "", 0},
			},
		},
		{
			// The delete that removes the conflict is skipped, so reporting
			// one would send the agent after a phantom (§3.5).
			name:  "a file whose own hunk failed takes no part",
			patch: "@@ create x\nhello\n\n@@ old\nabsent\n@@ new\ny\n@@ delete x\n@@ create x/y\n",
			want:  []failure{{2, 4, "x", "", 0}, {3, 8, "x", "", 2}},
		},
		{
			name:   "a dry run refuses it too",
			patch:  "@@ create internal\n@@ create internal/0\n",
			want:   []failure{{2, 2, "internal/0", "it is inside internal, which hunk 1 creates as a file", 0}},
			dryRun: true,
		},
		{
			name:       "an unconfined tree refuses it too",
			patch:      "@@ create internal\n@@ create internal/0\n",
			want:       []failure{{2, 2, "internal/0", "it is inside internal, which hunk 1 creates as a file", 0}},
			unconfined: true,
		},
		{
			name:  "a sibling whose name only starts the same",
			patch: "@@ create internal\n@@ create internal2/0\n",
			files: []string{"internal", "internal2/0"},
		},
		{
			name:  "two creates into one new directory",
			patch: "@@ create d/a\n@@ create d/b\n",
			files: []string{"d/a", "d/b"},
		},
		{
			name:  "a file created and deleted, then a directory in its place",
			patch: "@@ create x\n@@ delete x\n@@ create x/y\n",
			files: []string{"x/y"},
		},
		{
			name:  "a directory's only create deleted, then a file in its place",
			patch: "@@ create x/y\n@@ delete x/y\n@@ create x\n",
			files: []string{"x"},
		},
		{
			name:  "a file over a directory a later hunk empties",
			patch: "@@ create x/y\n@@ create x\n@@ delete x/y\n",
			files: []string{"x"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if r, err := filepath.EvalSymlinks(root); err == nil {
				root = r
			}
			must(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("x\n"), 0o644))
			tree, err := OpenTree(root, c.unconfined)
			must(t, err)
			t.Cleanup(func() { tree.Close() })
			p, err := Parse([]byte(c.patch), DefaultMarker)
			must(t, err)

			before := snapshot(t, root)
			if c.dryRun {
				_, err = NewTxn(tree, Options{}).Preview(p)
			} else {
				_, err = NewTxn(tree, Options{}).Run(p)
			}
			if c.want == nil {
				must(t, err)
				for _, name := range c.files {
					if fi, err := os.Stat(filepath.Join(root, name)); err != nil || !fi.Mode().IsRegular() {
						t.Errorf("%s should be a file: %v", name, err)
					}
				}
				return
			}

			if got := ExitCode(err); got != exitNoMatch {
				t.Fatalf("exit %d, want %d: %v", got, exitNoMatch, err)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %T: %v", err, err)
			}
			if len(ve.Failures) != len(c.want) {
				t.Fatalf("%d failures, want %d: %+v", len(ve.Failures), len(c.want), ve.Failures)
			}
			for i, w := range c.want {
				f := ve.Failures[i]
				if f.Hunk != w.hunk || f.PatchLine != w.line || f.Path != w.path || f.SkippedAfter != w.skipped {
					t.Errorf("failure %d is hunk %d, line %d, %q, skipped after %d; want hunk %d, line %d, %q, skipped after %d",
						i, f.Hunk, f.PatchLine, f.Path, f.SkippedAfter, w.hunk, w.line, w.path, w.skipped)
				}
				switch {
				case w.says == "" && f.Refusal != "":
					t.Errorf("hunk %d was refused and should not have been: %s", f.Hunk, f.Refusal)
				case w.says != "" && !strings.Contains(f.Refusal, w.says):
					t.Errorf("hunk %d: refusal = %q, want it to say %q", f.Hunk, f.Refusal, w.says)
				case w.says != "" && !strings.Contains(f.Refusal, root):
					t.Errorf("hunk %d: refusal = %q, want it to name the root %q", f.Hunk, f.Refusal, root)
				}
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The page an agent reads, with both directions of the refusal on it. It is the
// first per-hunk refusal pinned end to end; the root is substituted as in
// TestAPathRefusalIsGolden.
func TestAFileAndADirectoryAtOnePathIsGolden(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "x\n"})
	patch := "@@ create internal\n@@ create internal/0\n@@ create cmd/main.go\n@@ create cmd\n"
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch {
		t.Fatalf("exit %d, want %d: %s", code, exitNoMatch, errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want the refusal on stderr and nothing else", out)
	}
	golden(t, "cli-a-file-and-a-directory", slashPaths(strings.ReplaceAll(errOut, root, "/the/root")))
}

// The mirror of the test above, for a file already on disk rather than one the
// batch creates. Until 2026-09-21 every op on a path through a file aborted
// load as exit 5, printing "statat a.go/x: not a directory" and hiding every
// other failure in the batch. Now it is refused per hunk, beside them, naming
// the file. A delete of that file in the same batch does not make room: one
// batch cannot turn a file into a directory, and the refusal says so.
func TestAPathThroughAFileOnDiskIsRefused(t *testing.T) {
	type failure struct {
		hunk, line int
		path       string
		says       string // a substring of the refusal; "" for a failure that is not one
		skipped    int    // SkippedAfter
	}
	const inside = "it is inside a.go, which is a file, not a directory"
	const noRoom = "deleting it in the same batch does not make room, because one batch cannot turn a file into a directory"
	const nothingInside = "no such file; a.go is a file, so nothing can be inside it"
	for _, c := range []struct {
		name       string
		patch      string
		want       []failure // every failure, in order; nil means the batch applies
		files      []string  // when it applies: regular files it leaves
		dryRun     bool
		unconfined bool
	}{
		{
			name:  "a create inside a file",
			patch: "@@ create a.go/x\nhi\n",
			want:  []failure{{1, 1, "a.go/x", inside, 0}},
		},
		{
			name:  "a create two levels inside a file",
			patch: "@@ create a.go/deeper/x\nhi\n",
			want:  []failure{{1, 1, "a.go/deeper/x", inside, 0}},
		},
		{
			name:  "a create inside a link to a file",
			patch: "@@ create link.go/x\nhi\n",
			want:  []failure{{1, 1, "link.go/x", "it is inside link.go, which is a file, not a directory", 0}},
		},
		{
			name:  "a replace inside a file",
			patch: "@@ file a.go/x\n@@ old\na\n@@ new\nb\n",
			want:  []failure{{1, 2, "a.go/x", nothingInside, 0}},
		},
		{
			name:  "an append inside a file",
			patch: "@@ append a.go/x\nhi\n",
			want:  []failure{{1, 1, "a.go/x", nothingInside, 0}},
		},
		{
			name:  "a delete inside a file",
			patch: "@@ delete a.go/x\n",
			want:  []failure{{1, 1, "a.go/x", nothingInside, 0}},
		},
		{
			name:  "a create inside a file an earlier hunk deletes",
			patch: "@@ delete a.go\n@@ create a.go/x\nhi\n",
			want:  []failure{{2, 2, "a.go/x", noRoom, 0}},
		},
		{
			name:  "a create inside a file a later hunk deletes",
			patch: "@@ create a.go/x\nhi\n@@ delete a.go\n",
			want:  []failure{{1, 1, "a.go/x", noRoom, 0}},
		},
		{
			name:  "a modify of the file, and a create inside it",
			patch: "@@ file a.go\n@@ old\npackage a\n@@ new\npackage b\n@@ create a.go/x\nhi\n",
			want:  []failure{{2, 6, "a.go/x", inside, 0}},
		},
		{
			// §5.2: one round trip fixes all of them. Load-aborting, this
			// batch reported only the syscall.
			name:  "reported beside the batch's other failures",
			patch: "@@ create a.go/x\nhi\n@@ file sub/keep.txt\n@@ old\nabsent\n@@ new\ny\n",
			want:  []failure{{1, 1, "a.go/x", inside, 0}, {2, 4, "sub/keep.txt", "", 0}},
		},
		{
			// As a missing file's are: each hunk's refusal is true on its
			// own, so none is a phantom to skip (§3.5).
			name:  "every hunk on the path is refused",
			patch: "@@ create a.go/x\nhi\n\n@@ append a.go/x\nmore\n",
			want:  []failure{{1, 1, "a.go/x", inside, 0}, {2, 4, "a.go/x", nothingInside, 0}},
		},
		{
			name:   "a dry run refuses it too",
			patch:  "@@ create a.go/x\nhi\n",
			want:   []failure{{1, 1, "a.go/x", inside, 0}},
			dryRun: true,
		},
		{
			name:       "an unconfined tree refuses it too",
			patch:      "@@ create a.go/x\nhi\n",
			want:       []failure{{1, 1, "a.go/x", inside, 0}},
			unconfined: true,
		},
		{
			name:  "a create beside the file",
			patch: "@@ create sub/new.txt\nhi\n",
			files: []string{"sub/new.txt"},
		},
		{
			name:  "the file deleted and created again at its own path",
			patch: "@@ delete a.go\n@@ create a.go\npackage c\n",
			files: []string{"a.go"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			if r, err := filepath.EvalSymlinks(root); err == nil {
				root = r
			}
			must(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644))
			must(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
			must(t, os.WriteFile(filepath.Join(root, "sub", "keep.txt"), []byte("x\n"), 0o644))
			must(t, os.Symlink("a.go", filepath.Join(root, "link.go")))
			tree, err := OpenTree(root, c.unconfined)
			must(t, err)
			t.Cleanup(func() { tree.Close() })
			p, err := Parse([]byte(c.patch), DefaultMarker)
			must(t, err)

			before := snapshot(t, root)
			if c.dryRun {
				_, err = NewTxn(tree, Options{}).Preview(p)
			} else {
				_, err = NewTxn(tree, Options{}).Run(p)
			}
			if c.want == nil {
				must(t, err)
				for _, name := range c.files {
					if fi, err := os.Stat(filepath.Join(root, name)); err != nil || !fi.Mode().IsRegular() {
						t.Errorf("%s should be a file: %v", name, err)
					}
				}
				return
			}

			if got := ExitCode(err); got != exitNoMatch {
				t.Fatalf("exit %d, want %d: %v", got, exitNoMatch, err)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %T: %v", err, err)
			}
			if len(ve.Failures) != len(c.want) {
				t.Fatalf("%d failures, want %d: %+v", len(ve.Failures), len(c.want), ve.Failures)
			}
			for i, w := range c.want {
				f := ve.Failures[i]
				if f.Hunk != w.hunk || f.PatchLine != w.line || f.Path != w.path || f.SkippedAfter != w.skipped {
					t.Errorf("failure %d is hunk %d, line %d, %q, skipped after %d; want hunk %d, line %d, %q, skipped after %d",
						i, f.Hunk, f.PatchLine, f.Path, f.SkippedAfter, w.hunk, w.line, w.path, w.skipped)
				}
				switch {
				case w.says == "" && f.Refusal != "":
					t.Errorf("hunk %d was refused and should not have been: %s", f.Hunk, f.Refusal)
				case w.says != "" && !strings.Contains(f.Refusal, w.says):
					t.Errorf("hunk %d: refusal = %q, want it to say %q", f.Hunk, f.Refusal, w.says)
				case w.says != "" && !strings.Contains(f.Refusal, root):
					t.Errorf("hunk %d: refusal = %q, want it to name the root %q", f.Hunk, f.Refusal, root)
				}
				// The clause belongs only to a create under a file the batch
				// deletes.
				if w.says != noRoom && strings.Contains(f.Refusal, "does not make room") {
					t.Errorf("hunk %d: refusal = %q, which says the batch deletes a file it does not", f.Hunk, f.Refusal)
				}
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The page an agent reads for a path through a file on disk: all three
// sentences, and an ordinary miss reported beside them in the same round.
func TestAPathThroughAFileIsGolden(t *testing.T) {
	root := cliTree(t, map[string]string{"a.go": "package a\n", "b.go": "package b\n", "sub/keep.txt": "x\n"})
	patch := "@@ create a.go/x\nhi\n\n" +
		"@@ delete b.go\n@@ create b.go/x\nhi\n\n" +
		"@@ append a.go/y\nmore\n\n" +
		"@@ file sub/keep.txt\n@@ old\nabsent\n@@ new\ny\n"
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch {
		t.Fatalf("exit %d, want %d: %s", code, exitNoMatch, errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want the refusal on stderr and nothing else", out)
	}
	golden(t, "cli-a-path-through-a-file", slashPaths(strings.ReplaceAll(errOut, root, "/the/root")))
}

// A file is read once and written once (§3.5), and that has to mean the file,
// not the spelling. Until 2026-09-17 a directory link made one file two: with
// d -> sub, a batch naming sub/b.go and d/b.go loaded it twice and wrote it
// twice, and every operation had a shape of it. Replaces lost an edit and said
// both applied, a delete came back with an edit in it, creates lost a payload,
// and a file and a path inside it hid their conflict and half-wrote the tree.
// Each row is one of those, probed against v0.2.1 before it was written.
func TestOneFileUnderTwoNamesIsOneFile(t *testing.T) {
	const b = "alpha\nbeta\n"
	for _, c := range []struct {
		name       string
		files      map[string]string // besides sub/b.go and a.txt
		links      [][2]string       // besides d -> sub; made in order, ROOT standing for the root
		hardLinks  [][2]string       // new name, existing file
		patch      string
		unconfined bool
		dryRun     bool

		after    map[string]string // when it applies: every regular file afterwards
		reported int               // and how many files the report lists

		hunk, line int // when it is refused: the one failure
		path, says string
	}{
		{
			name:     "a replace under each spelling",
			patch:    "@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file d/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
			after:    map[string]string{"sub/b.go": "ALPHA\nBETA\n"},
			reported: 1,
		},
		{
			name:     "a replace matching what the other spelling's replace made",
			patch:    "@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file d/b.go\n@@ old\nALPHA\n@@ new\nOMEGA\n",
			after:    map[string]string{"sub/b.go": "OMEGA\nbeta\n"},
			reported: 1,
		},
		{
			name:  "a create under each spelling",
			patch: "@@ create sub/n.go\none\n\n@@ create d/n.go\ntwo\n\n",
			hunk:  2, line: 4, path: "d/n.go", says: "it already exists",
		},
		{
			name:     "a create, then a replace under the other spelling",
			patch:    "@@ create d/n.go\nalpha\n\n@@ file sub/n.go\n@@ old\nalpha\n@@ new\nbeta\n",
			after:    map[string]string{"sub/b.go": b, "sub/n.go": "beta\n"},
			reported: 1,
		},
		{
			name:  "a delete, then a replace under the other spelling",
			patch: "@@ delete sub/b.go\n@@ file d/b.go\n@@ old\nalpha\n@@ new\nALPHA\n",
			hunk:  2, line: 3, path: "d/b.go", says: "an earlier hunk in this batch deleted it",
		},
		{
			name:  "a delete under each spelling",
			patch: "@@ delete sub/b.go\n@@ delete d/b.go\n",
			hunk:  2, line: 2, path: "d/b.go", says: "an earlier hunk in this batch deleted it",
		},
		{
			name:     "a final link under a directory link",
			links:    [][2]string{{"sub/alias.go", "b.go"}},
			patch:    "@@ file d/alias.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file sub/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
			after:    map[string]string{"sub/b.go": "ALPHA\nBETA\n"},
			reported: 1,
		},
		{
			name:     "a chain of directory links",
			links:    [][2]string{{"e", "d"}},
			patch:    "@@ file e/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file sub/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
			after:    map[string]string{"sub/b.go": "ALPHA\nBETA\n"},
			reported: 1,
		},
		{
			name:     "an append under the other spelling",
			patch:    "@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ append d/b.go\ngamma\n\n",
			after:    map[string]string{"sub/b.go": "ALPHA\nbeta\ngamma\n"},
			reported: 1,
		},
		{
			name:  "creates under a directory neither spelling has yet",
			patch: "@@ create d/new/x.go\none\n\n@@ create sub/new/x.go\ntwo\n\n",
			hunk:  2, line: 4, path: "sub/new/x.go", says: "it already exists",
		},
		{
			name:     "a link whose destination climbs with ..",
			files:    map[string]string{"other/f.go": "o\n"},
			links:    [][2]string{{"sub/inner", "../other"}},
			patch:    "@@ file sub/inner/f.go\n@@ old\no\n@@ new\nO1\n@@ file other/f.go\n@@ old\nO1\n@@ new\nO2\n",
			after:    map[string]string{"sub/b.go": b, "other/f.go": "O2\n"},
			reported: 1,
		},
		{
			name:  "a file and a path inside it, hidden by a link",
			patch: "@@ create d/x\n@@ create sub/x/y\n",
			hunk:  2, line: 2, path: "sub/x/y", says: "it is inside d/x, which hunk 1 creates as a file",
		},
		{
			name:  "the same, the other way round",
			patch: "@@ create sub/x/y\n@@ create d/x\n",
			hunk:  2, line: 2, path: "d/x", says: "hunk 1 creates sub/x/y inside it, so it cannot also be a file",
		},
		{
			name:     "a create through a dangling directory link is written through",
			links:    [][2]string{{"l", "internal"}},
			patch:    "@@ create l/0\nx\n\n",
			after:    map[string]string{"sub/b.go": b, "internal/0": "x\n"},
			reported: 1,
		},
		{
			name:     "an earlier create, then one through a dangling directory link",
			links:    [][2]string{{"l", "internal"}},
			patch:    "@@ create first.txt\nx\n\n@@ create l/0\ny\n\n",
			after:    map[string]string{"sub/b.go": b, "first.txt": "x\n", "internal/0": "y\n"},
			reported: 2,
		},
		{
			name:     "a create in the link's destination, then one through the link",
			links:    [][2]string{{"l", "internal"}},
			patch:    "@@ create internal/x\nx\n\n@@ create l/y\ny\n\n",
			after:    map[string]string{"sub/b.go": b, "internal/x": "x\n", "internal/y": "y\n"},
			reported: 2,
		},
		{
			name:     "the same two creates in the order commit used to fail",
			links:    [][2]string{{"l", "internal"}},
			patch:    "@@ create l/y\ny\n\n@@ create internal/x\nx\n\n",
			after:    map[string]string{"sub/b.go": b, "internal/x": "x\n", "internal/y": "y\n"},
			reported: 2,
		},
		{
			name:  "a dangling link's destination created as a file, then a path through the link",
			links: [][2]string{{"l", "internal"}},
			patch: "@@ create internal\n@@ create l/0\n",
			hunk:  2, line: 2, path: "l/0", says: "it is inside internal, which hunk 1 creates as a file",
		},
		{
			name:       "unconfined, through a relative link",
			patch:      "@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file d/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
			unconfined: true,
			after:      map[string]string{"sub/b.go": "ALPHA\nBETA\n"},
			reported:   1,
		},
		{
			name:       "unconfined, through an absolute link",
			links:      [][2]string{{"dabs", "ROOT/sub"}},
			patch:      "@@ file dabs/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file sub/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
			unconfined: true,
			after:      map[string]string{"sub/b.go": "ALPHA\nBETA\n"},
			reported:   1,
		},
		{
			name:     "a dry run reports one file",
			patch:    "@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file d/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
			dryRun:   true,
			after:    map[string]string{"sub/b.go": b},
			reported: 1,
		},
		{
			// Not an alias. Rename separates a hard link in any batch, so each
			// name keeps its own edit; unchanged, and pinned so that changing
			// it is a decision.
			name:      "a hard link is two files",
			hardLinks: [][2]string{{"hard.go", "sub/b.go"}},
			patch:     "@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file hard.go\n@@ old\nbeta\n@@ new\nBETA\n",
			after:     map[string]string{"sub/b.go": "ALPHA\nbeta\n", "hard.go": "alpha\nBETA\n"},
			reported:  2,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"sub/b.go": b, "a.txt": "x\n"}
			maps.Copy(files, c.files)
			root := t.TempDir()
			if r, err := filepath.EvalSymlinks(root); err == nil {
				root = r
			}
			for name, body := range files {
				full := filepath.Join(root, filepath.FromSlash(name))
				must(t, os.MkdirAll(filepath.Dir(full), 0o755))
				must(t, os.WriteFile(full, []byte(body), 0o644))
			}
			links := append([][2]string{{"d", "sub"}}, c.links...)
			for _, l := range links {
				dest := filepath.FromSlash(strings.Replace(l[1], "ROOT", filepath.ToSlash(root), 1))
				must(t, os.Symlink(dest, filepath.Join(root, filepath.FromSlash(l[0]))))
			}
			for _, h := range c.hardLinks {
				must(t, os.Link(filepath.Join(root, filepath.FromSlash(h[1])), filepath.Join(root, filepath.FromSlash(h[0]))))
			}
			tree, err := OpenTree(root, c.unconfined)
			must(t, err)
			t.Cleanup(func() { tree.Close() })
			p, err := Parse([]byte(c.patch), DefaultMarker)
			must(t, err)

			before := snapshot(t, root)
			var r *Result
			if c.dryRun {
				r, err = NewTxn(tree, Options{}).Preview(p)
			} else {
				r, err = NewTxn(tree, Options{}).Run(p)
			}

			if c.says == "" {
				must(t, err)
				if len(r.Files) != c.reported {
					t.Errorf("the report lists %d files, want %d: %+v", len(r.Files), c.reported, r.Files)
				}
				want := map[string]string{"a.txt": "x\n"}
				maps.Copy(want, c.after)
				if got := regularFiles(t, root); !maps.Equal(got, want) {
					t.Errorf("files afterwards:\n got %q\nwant %q", got, want)
				}
				for _, l := range links {
					if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(l[0]))); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
						t.Errorf("%s is no longer a symlink: %v", l[0], err)
					}
				}
				return
			}

			if got := ExitCode(err); got != exitNoMatch {
				t.Fatalf("exit %d, want %d: %v", got, exitNoMatch, err)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %T: %v", err, err)
			}
			if len(ve.Failures) != 1 {
				t.Fatalf("%d failures, want 1: %+v", len(ve.Failures), ve.Failures)
			}
			f := ve.Failures[0]
			if f.Hunk != c.hunk || f.PatchLine != c.line || f.Path != c.path {
				t.Errorf("failure is hunk %d, line %d, %q; want hunk %d, line %d, %q", f.Hunk, f.PatchLine, f.Path, c.hunk, c.line, c.path)
			}
			for _, want := range []string{c.says, root} {
				if !strings.Contains(f.Refusal, want) {
					t.Errorf("refusal = %q, want it to say %q", f.Refusal, want)
				}
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The batch half of TestASpellingIsTheOneTheDirectoryStores: every shape probed
// against v0.2.2 on this machine's case-insensitive /workspace, where two
// spellings of one name were two entries. A replace under each case lost an
// edit under exit 0, creates lost a payload, deletes and a hidden conflict
// half-wrote the tree, and a delete then a create was refused as if the delete
// had not happened. Each row has an outcome for a filesystem that folds and one
// for a filesystem that does not, and runs the one that applies.
func TestOneFileUnderTwoSpellingsIsOneFile(t *testing.T) {
	type outcome struct {
		after    map[string]string // applied: every regular file afterwards, by the name the directory lists
		reported int
		check    func(t *testing.T, files map[string]string) // applied, instead of after

		hunk, line int // refused: the one failure
		path, says string
	}
	const b = "alpha\nbeta\n"
	nfc, nfd := "é.go", "é.go"
	folds, normalizes := foldsCase(t), foldsNormalization(t)
	for _, c := range []struct {
		name            string
		files           map[string]string // besides a.go
		dirs            []string
		byNormalization bool // folds is normalization, not case
		patch           string
		folded, plain   outcome
	}{
		{
			name:   "a replace under each case",
			patch:  "@@ file a.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file A.go\n@@ old\nbeta\n@@ new\nBETA\n",
			folded: outcome{after: map[string]string{"a.go": "ALPHA\nBETA\n"}, reported: 1},
			plain:  outcome{hunk: 2, line: 7, path: "A.go", says: "no such file"},
		},
		{
			name:   "one replace in the wrong case keeps the stored name",
			patch:  "@@ file A.go\n@@ old\nalpha\n@@ new\nALPHA\n",
			folded: outcome{after: map[string]string{"a.go": "ALPHA\nbeta\n"}, reported: 1},
			plain:  outcome{hunk: 1, line: 2, path: "A.go", says: "no such file"},
		},
		{
			name:   "a create under each case",
			patch:  "@@ create n.go\none\n\n@@ create N.go\ntwo\n\n",
			folded: outcome{hunk: 2, line: 4, path: "N.go", says: "it already exists"},
			plain:  outcome{after: map[string]string{"a.go": b, "n.go": "one\n", "N.go": "two\n"}, reported: 2},
		},
		{
			name:   "a file and a path inside it, in another case",
			patch:  "@@ create x\n@@ create X/y\n",
			folded: outcome{hunk: 2, line: 2, path: "X/y", says: "it is inside x, which hunk 1 creates as a file"},
			plain:  outcome{after: map[string]string{"a.go": b, "x": "", "X/y": ""}, reported: 2},
		},
		{
			name:   "a delete under each case",
			patch:  "@@ delete a.go\n@@ delete A.go\n",
			folded: outcome{hunk: 2, line: 2, path: "A.go", says: "an earlier hunk in this batch deleted it"},
			plain:  outcome{hunk: 2, line: 2, path: "A.go", says: "no such file"},
		},
		{
			name:   "a directory under each case",
			files:  map[string]string{"sub/b.go": b},
			patch:  "@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file SUB/b.go\n@@ old\nbeta\n@@ new\nBETA\n",
			folded: outcome{after: map[string]string{"a.go": b, "sub/b.go": "ALPHA\nBETA\n"}, reported: 1},
			plain:  outcome{hunk: 2, line: 7, path: "SUB/b.go", says: "no such file"},
		},
		{
			name:            "an existing name under each normalization",
			files:           map[string]string{nfc: b},
			byNormalization: true,
			patch:           "@@ file " + nfc + "\n@@ old\nalpha\n@@ new\nALPHA\n@@ file " + nfd + "\n@@ old\nbeta\n@@ new\nBETA\n",
			folded:          outcome{after: map[string]string{"a.go": b, nfc: "ALPHA\nBETA\n"}, reported: 1},
			plain:           outcome{hunk: 2, line: 7, path: nfd, says: "no such file"},
		},
		{
			name:   "creates under a new directory in each case",
			patch:  "@@ create NEW/x.go\none\n\n@@ create new/x.go\ntwo\n\n",
			folded: outcome{hunk: 2, line: 4, path: "new/x.go", says: "it already exists"},
			plain:  outcome{after: map[string]string{"a.go": b, "NEW/x.go": "one\n", "new/x.go": "two\n"}, reported: 2},
		},
		{
			name:   "a delete, then a create in another case",
			patch:  "@@ delete a.go\n@@ create A.go\nnew\n\n",
			folded: outcome{after: map[string]string{"a.go": "new\n"}, reported: 1},
			plain:  outcome{after: map[string]string{"A.go": "new\n"}, reported: 2},
		},
		{
			// The stated limit (Kevin, 2026-09-17): telling two normalizations
			// of a name that does not exist apart needs Unicode tables, which
			// go.mod excludes. They are two creates everywhere, so where the
			// filesystem folds normalization the second replaces the first.
			name:            "a new name under each normalization is two creates",
			byNormalization: true,
			patch:           "@@ create " + nfc + "\none\n\n@@ create " + nfd + "\ntwo\n\n",
			folded: outcome{reported: 2, check: func(t *testing.T, files map[string]string) {
				if len(files) != 2 || files["a.go"] != b {
					t.Errorf("want a.go and one other file, got %q", files)
				}
				for name, body := range files {
					if name != "a.go" && body != "two\n" {
						t.Errorf("%q holds %q; the limit is that the second payload wins", name, body)
					}
				}
			}},
			plain: outcome{after: map[string]string{"a.go": b, nfc: "one\n", nfd: "two\n"}, reported: 2},
		},
		{
			// Nothing in 2026 has a letter, and neither has its name, so it
			// cannot say whether it folds, and it is assumed to.
			name:   "creates under a directory nothing can probe",
			dirs:   []string{"2026"},
			patch:  "@@ create 2026/n.go\none\n\n@@ create 2026/N.go\ntwo\n\n",
			folded: outcome{hunk: 2, line: 4, path: "2026/N.go", says: "it already exists"},
			plain:  outcome{hunk: 2, line: 4, path: "2026/N.go", says: "it already exists"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := c.plain
			if (c.byNormalization && normalizes) || (!c.byNormalization && folds) {
				want = c.folded
			}
			root := t.TempDir()
			if r, err := filepath.EvalSymlinks(root); err == nil {
				root = r
			}
			files := map[string]string{"a.go": b}
			maps.Copy(files, c.files)
			for name, body := range files {
				full := filepath.Join(root, filepath.FromSlash(name))
				must(t, os.MkdirAll(filepath.Dir(full), 0o755))
				must(t, os.WriteFile(full, []byte(body), 0o644))
			}
			for _, d := range c.dirs {
				must(t, os.MkdirAll(filepath.Join(root, d), 0o755))
			}
			tree, err := OpenTree(root, false)
			must(t, err)
			t.Cleanup(func() { tree.Close() })
			p, err := Parse([]byte(c.patch), DefaultMarker)
			must(t, err)

			before := snapshot(t, root)
			r, err := NewTxn(tree, Options{}).Run(p)
			if want.says == "" {
				if err != nil {
					t.Fatalf("refused (folds case: %v, normalization: %v): %v", folds, normalizes, err)
				}
				if len(r.Files) != want.reported {
					t.Errorf("the report lists %d files, want %d: %+v", len(r.Files), want.reported, r.Files)
				}
				got := regularFiles(t, root)
				if want.check != nil {
					want.check(t, got)
				} else if !maps.Equal(got, want.after) {
					t.Errorf("files afterwards:\n got %q\nwant %q", got, want.after)
				}
				return
			}

			if got := ExitCode(err); got != exitNoMatch {
				t.Fatalf("exit %d, want %d (folds case: %v, normalization: %v): %v", got, exitNoMatch, folds, normalizes, err)
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want *ValidationError, got %T: %v", err, err)
			}
			if len(ve.Failures) != 1 {
				t.Fatalf("%d failures, want 1: %+v", len(ve.Failures), ve.Failures)
			}
			f := ve.Failures[0]
			if f.Hunk != want.hunk || f.PatchLine != want.line || f.Path != want.path {
				t.Errorf("failure is hunk %d, line %d, %q; want hunk %d, line %d, %q", f.Hunk, f.PatchLine, f.Path, want.hunk, want.line, want.path)
			}
			for _, s := range []string{want.says, root} {
				if !strings.Contains(f.Refusal, s) {
					t.Errorf("refusal = %q, want it to say %q", f.Refusal, s)
				}
			}
			assertUnchanged(t, root, before)
		})
	}
}

// regularFiles is every regular file under root and its contents, by
// slash-separated relative name. Links and directories are not in it, so an
// extra file, a duplicate or a leftover temp file is a difference.
func regularFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	must(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		body, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = string(body)
		return err
	}))
	return out
}

// With one entry per spelling, a failed verify rolled the file back twice: the
// first entry found the second entry's bytes and refused, blaming a second
// writer, and the second restored it. Exit 4, "the only one that leaves the tree
// in a state you have to look at", about a tree that was fine.
func TestRollbackOfOneFileUnderTwoNames(t *testing.T) {
	for _, c := range []struct{ name, patch string }{
		{"two replaces", "@@ file sub/b.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ file d/b.go\n@@ old\nbeta\n@@ new\nBETA\n"},
		{"a create and a replace", "@@ create sub/n.go\none\n\n@@ file d/n.go\n@@ old\none\n@@ new\ntwo\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"sub/b.go": "alpha\nbeta\n"})
			must(t, os.Symlink("sub", filepath.Join(root, "d")))
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, []string{"--verify", "false"}, c.patch)
			if code != exitVerifyFailed {
				t.Fatalf("exit %d, want %d:\n%s", code, exitVerifyFailed, errOut)
			}
			if !strings.Contains(errOut, "rolled back 1 file\n") {
				t.Errorf("stderr does not say one file was rolled back:\n%s", errOut)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The seam (§3.3, decided 2026-09-04). A weld happens when the text on the left
// of the seam does not end in a newline: for append that is the file, for
// prepend it is the payload. Both are normalized, and the inserted byte is
// reported rather than added quietly.
func TestTheSeam(t *testing.T) {
	for _, c := range []struct {
		name      string
		start     string
		patch     string
		opt       Options
		want      string
		wantAdded bool
	}{
		{
			name: "append to a file that ends in a newline", start: "x\n",
			patch: "@@ append a.txt\ny\n\n", want: "x\ny\n",
		},
		{
			// Without normalizing, this is "xy\n": the payload welded onto the
			// file's last line, reported as success.
			name: "append to a file with no final newline", start: "x",
			patch: "@@ append a.txt\ny\n\n", want: "x\ny\n", wantAdded: true,
		},
		{
			// §3.3 never gives a payload a trailing newline, and this one is
			// the new end of the file, so until 2026-09-21 this was "x\ny", a
			// file with no final newline. Every append in the field was spelled
			// this way. Append adds whole lines now, and says nothing about it:
			// the caller can predict it from the patch alone.
			name: "append a payload with no final newline", start: "x\n",
			patch: "@@ append a.txt\ny\n", want: "x\ny\n",
		},
		{
			name: "append several lines, the last with no newline", start: "x\n",
			patch: "@@ append a.txt\ny\nz\n", want: "x\ny\nz\n",
		},
		{
			// Both bytes, and one report: the one at the seam.
			name: "append a payload with none to a file with none", start: "x",
			patch: "@@ append a.txt\ny\n", want: "x\ny\n", wantAdded: true,
		},
		{
			name: "append a payload with none to an empty file", start: "",
			patch: "@@ append a.txt\ny\n", want: "y\n",
		},
		{
			// An empty payload has no last line to end.
			name: "append nothing to a file that ends in a newline", start: "x\n",
			patch: "@@ append a.txt\n", want: "x\n",
		},
		{
			name: "append nothing to a file with no final newline", start: "x",
			patch: "@@ append a.txt\n", want: "x\n", wantAdded: true,
		},
		{
			name: "append nothing to an empty file", start: "",
			patch: "@@ append a.txt\n", want: "",
		},
		{
			name: "append to an empty file adds no seam", start: "",
			patch: "@@ append a.txt\ny\n\n", want: "y\n",
		},
		{
			// §3.3 never gives a payload a trailing newline, so without
			// normalizing this welds every time: "yx\n". The newline is the
			// payload's own, so it is not reported, since 2026-09-22.
			name: "prepend a payload with no final newline", start: "x\n",
			patch: "@@ prepend a.txt\ny\n", want: "y\nx\n",
		},
		{
			// The file's end is not prepend's seam, so it is left alone.
			name: "prepend a payload with none to a file with none", start: "x",
			patch: "@@ prepend a.txt\ny\n", want: "y\nx",
		},
		{
			name: "prepend a payload that ends in a newline", start: "x\n",
			patch: "@@ prepend a.txt\ny\n\n", want: "y\nx\n",
		},
		{
			name: "prepend to a file with no final newline", start: "x",
			patch: "@@ prepend a.txt\ny\n\n", want: "y\nx",
		},
		{
			// The seam byte is the file's ending, not always LF.
			name: "the seam byte on a CRLF file", start: "a\r\nx",
			patch: "@@ append a.txt\ny\n\n", want: "a\r\nx\r\ny\r\n", wantAdded: true,
		},
		{
			name: "the payload's newline on a CRLF file", start: "a\r\n",
			patch: "@@ append a.txt\ny\n", want: "a\r\ny\r\n",
		},
		{
			name: "both on a CRLF file", start: "a\r\nx",
			patch: "@@ append a.txt\ny\n", want: "a\r\nx\r\ny\r\n", wantAdded: true,
		},
		{
			// The file already ends in a newline, a bare one. Asking whether
			// it ended in the file's own ending, CRLF, added a second newline
			// here until 2026-09-21: "c\n\r\n", a blank line nobody wrote,
			// reported as a final newline added to a file that had one.
			name: "a CRLF file whose last line ends in a bare LF", start: "a\r\nb\r\nc\n",
			patch: "@@ append a.txt\ny\n\n", want: "a\r\nb\r\nc\ny\r\n",
		},
		{
			// --eol strict does not convert the payload, so its LF reaches a
			// CRLF file as written, and it is still a newline.
			name: "append under --eol strict, a payload that ends in LF", start: "a\r\n",
			patch: "@@ append a.txt\ny\n\n", opt: Options{EOL: EOLStrict}, want: "a\r\ny\n",
		},
		{
			name: "append under --eol strict, a payload with none", start: "a\r\n",
			patch: "@@ append a.txt\ny\n", opt: Options{EOL: EOLStrict}, want: "a\r\ny\r\n",
		},
		{
			name: "prepend under --eol strict, a payload that ends in LF", start: "a\r\n",
			patch: "@@ prepend a.txt\ny\n\n", opt: Options{EOL: EOLStrict}, want: "y\na\r\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"a.txt": c.start})
			r, err := run(t, tree, c.patch, c.opt)
			must(t, err)
			if got := readFile(t, root, "a.txt"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			// A batch that leaves the bytes as they were lists no file, like a
			// replace whose new text equals its old (an-empty-append-rewrites-the-file).
			if unchanged := c.start == c.want; unchanged != (len(r.Files) == 0) {
				t.Errorf("unchanged %v, but %d files listed", unchanged, len(r.Files))
			}
			if added := len(r.Files) == 1 && r.Files[0].SeamAdded; added != c.wantAdded {
				t.Errorf("SeamAdded = %v, want %v", added, c.wantAdded)
			}
		})
	}

	// The byte is reported, which is the whole reason normalizing it was
	// acceptable: it turns a byte the tool wrote and the patch did not contain
	// from something found later in a diff into something said at the time.
	t.Run("the added byte is reported", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x"})
		code, out, errOut := runCLI(t, root, nil, "@@ append a.txt\ny\n\n")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if !strings.Contains(out, "(added a final newline)") {
			t.Errorf("out = %q", out)
		}
		golden(t, "cli-seam-added", out)
	})

	t.Run("and not reported when none was added", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x\n"})
		_, out, _ := runCLI(t, root, nil, "@@ append a.txt\ny\n\n")
		if strings.Contains(out, "final newline") {
			t.Errorf("out = %q", out)
		}
	})

	// The newline append gives the end of its payload is not reported. Every
	// append in the field needed it, so a suffix for it would sit on every
	// append row and say nothing the patch did not, and sharing the words with
	// the seam byte would hide the one thing the suffix does say: that the
	// file had no final newline.
	t.Run("nor the newline append gives its payload", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x\n"})
		_, out, _ := runCLI(t, root, nil, "@@ append a.txt\ny\n")
		if out != "M a.txt +1 -0\n1 file, 1 hunk, +1 -0\n" {
			t.Errorf("out = %q", out)
		}
		_, js, _ := runCLI(t, cliTree(t, map[string]string{"a.txt": "x\n"}), []string{"--json"}, "@@ append a.txt\ny\n")
		if strings.Contains(js, "newline") {
			t.Errorf("--json: %s", js)
		}
	})
}

// An append or prepend that changes no byte is not a write, as a replace whose
// new text equals its old is not. Until 2026-09-22 it counted anyway: the file
// was listed as "M +0 -0" and rewritten, with a new inode and a new mtime,
// which invalidates a build cache for nothing.
func TestAnAppendThatChangesNothingIsNotAWrite(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, start, patch, out, want string
	}{
		{"an empty append to a file that ends in a newline", "x\n", "@@ append t.txt\n", "0 files, 1 hunk, +0 -0\n", "x\n"},
		{"an empty prepend", "x\n", "@@ prepend t.txt\n", "0 files, 1 hunk, +0 -0\n", "x\n"},
		{"an empty append to an empty file", "", "@@ append t.txt\n", "0 files, 1 hunk, +0 -0\n", ""},
		{
			// The seam byte is a change, so this one is a write.
			"an empty append to a file with no final newline", "x", "@@ append t.txt\n",
			"M t.txt +0 -0  (added a final newline)\n1 file, 1 hunk, +0 -0\n", "x\n",
		},
		{
			// Listed once, for the replace.
			"after a real change", "x\n", "@@ file t.txt\n@@ old\nx\n@@ new\ny\n@@ append t.txt\n",
			"M t.txt +1 -1\n1 file, 2 hunks, +1 -1\n", "y\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"t.txt": c.start})
			path := filepath.Join(root, "t.txt")
			must(t, os.Chtimes(path, old, old))
			before, err := os.Stat(path)
			must(t, err)
			code, out, errOut := runCLI(t, root, nil, c.patch)
			if code != exitOK || out != c.out {
				t.Fatalf("exit %d, out %q, want %q: %s", code, out, c.out, errOut)
			}
			if got := readFile(t, root, "t.txt"); got != c.want {
				t.Errorf("t.txt = %q, want %q", got, c.want)
			}
			after, err := os.Stat(path)
			must(t, err)
			written := !os.SameFile(before, after) || !after.ModTime().Equal(old)
			if want := c.start != c.want; written != want {
				t.Errorf("written = %v, want %v", written, want)
			}
		})
	}
}

// §3.3's create writes exactly the bytes it was given, and since 2026-09-21 the
// report says when the file it made does not end in a newline. 45 of the
// field's 52 creates were spelled that way, a payload running straight into the
// terminator, so this speaks up in the common case, and it is the case a
// formatter run as a check refuses. It is read off the bytes the batch ends
// with, not off the create's payload: a later hunk can give the newline back or
// take it away, and a delete then create commits as a modify.
func TestACreatedFileWithNoFinalNewlineSaysSo(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		patch string
		row   string // n.txt's line of the report
		want  string // n.txt afterwards
	}{
		{"a payload that runs into the terminator", nil, "@@ create n.txt\nhi\n", "A n.txt +1 -0  (no final newline)", "hi"},
		{"a payload with the blank line", nil, "@@ create n.txt\nhi\n\n", "A n.txt +1 -0", "hi\n"},
		{"an empty file has no last line to end", nil, "@@ create n.txt\n", "A n.txt +0 -0", ""},
		{"CRLF, ended", nil, "@@ create n.txt\na\r\nb\r\n\n", "A n.txt +2 -0", "a\r\nb\r\n"},
		{"CRLF, not ended", nil, "@@ create n.txt\na\r\nb\n", "A n.txt +2 -0  (no final newline)", "a\r\nb"},
		{"a bare CR is not a newline", nil, "@@ create n.txt\nhi\r\n", "A n.txt +1 -0  (no final newline)", "hi\r"},
		{
			// The append meets a file with no final newline, which is the seam
			// byte and is said, and then ends its own payload.
			"a later append ends it", nil, "@@ create n.txt\nhi\n@@ append n.txt\nthere\n",
			"A n.txt +2 -0  (added a final newline)", "hi\nthere\n",
		},
		{
			"a later replace takes it away", nil, "@@ create n.txt\nhi\n\n@@ old\nhi\n\n@@ new\nbye\n",
			"A n.txt +2 -1  (no final newline)", "bye",
		},
		{
			// The newline prepend gives its payload is not reported, so only
			// the file's end is said. Until 2026-09-22 this row printed both
			// suffixes, "(added a final newline)  (no final newline)".
			"a later prepend, and only the file's end is said", nil, "@@ create n.txt\na\n@@ prepend n.txt\np\n",
			"A n.txt +2 -0  (no final newline)", "p\na",
		},
		{
			// The case where it matters most: the file had a final newline.
			"an overwrite, which commits as a modify",
			map[string]string{"n.txt": "old\n"},
			"@@ delete n.txt\n@@ create n.txt\nnew\n", "M n.txt +1 -1  (no final newline)", "new",
		},
		{
			"an overwrite with the blank line",
			map[string]string{"n.txt": "old\n"},
			"@@ delete n.txt\n@@ create n.txt\nnew\n\n", "M n.txt +1 -1", "new\n",
		},
		{
			// Only a create is reported. A replace spells both sides, and
			// §3.3's blank-line rule is how it spells the newline.
			"a modify of a file that has none",
			map[string]string{"n.txt": "a\nb"},
			"@@ file n.txt\n@@ old\na\n@@ new\nA\n", "M n.txt +1 -1", "A\nb",
		},
		{
			"a modify that takes it away",
			map[string]string{"n.txt": "a\n"},
			"@@ file n.txt\n@@ old\na\n\n@@ new\nA\n", "M n.txt +1 -1", "A",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			code, out, errOut := runCLI(t, root, nil, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if row, _, _ := strings.Cut(out, "\n"); row != c.row {
				t.Errorf("row = %q, want %q", row, c.row)
			}
			if got := readFile(t, root, "n.txt"); got != c.want {
				t.Errorf("n.txt = %q, want %q", got, c.want)
			}

			// --json carries it as a field, on a tree of its own.
			_, js, _ := runCLI(t, cliTree(t, c.files), []string{"--json"}, c.patch)
			var v struct{ Files []map[string]any }
			must(t, json.Unmarshal([]byte(js), &v))
			if len(v.Files) != 1 {
				t.Fatalf("--json: %s", js)
			}
			if got, want := v.Files[0]["no_final_newline"] == true, strings.Contains(c.row, "(no final newline)"); got != want {
				t.Errorf("no_final_newline = %v, want %v: %s", got, want, js)
			}
		})
	}

	t.Run("a dry run says it about the file it would make", func(t *testing.T) {
		root := cliTree(t, nil)
		_, out, _ := runCLI(t, root, []string{"--dry-run"}, "@@ create n.txt\nhi\n")
		if want := "A n.txt +1 -0  (no final newline)\n1 file, 1 hunk, +1 -0 (dry run: nothing written)\n"; out != want {
			t.Errorf("out = %q, want %q", out, want)
		}
		absent(t, root, "n.txt")
	})

	// Two creates, one of each, so the suffix is seen against the padding.
	t.Run("golden", func(t *testing.T) {
		patch := "@@ create testdata/empty.json\n{}\n@@ create testdata/one.json\n{\"a\": 1}\n\n"
		_, out, _ := runCLI(t, cliTree(t, nil), nil, patch)
		golden(t, "cli-no-final-newline", out)
		_, js, _ := runCLI(t, cliTree(t, nil), []string{"--json"}, patch)
		golden(t, "cli-no-final-newline-json", js)
	})
}

// §3.3's append, for any file and any payload. The file's bytes are kept as a
// prefix. The file's ending goes in at the seam exactly when the file has text
// and its last byte is not \n. What follows is the payload as §6.4 translates it
// for this file, and then the file's ending if the payload has text and does
// not end in \n. Until 2026-09-21 the last part was false for the default
// spelling, and a file ending in a bare LF among CRLFs broke the second.
//
// The comparison is exact. Its first draft compared line endings as LF, and
// fuzzing it found that normalizing welds a payload's trailing CR to the
// ending added after it, on a CRLF file: "\r" became "\r\r\n", which is right,
// and read as "\r\n" against a wanted "\n".
func FuzzAppendAddsWholeLines(f *testing.F) {
	for _, c := range [][2]string{
		{"x\n", "y\n"},
		{"x", "y\n"},
		{"x\n", "y\n\n"},
		{"", "y\n"},
		{"x", ""},
		{"", ""},
		{"a\r\nx", "y\n"},
		{"a\r\n", "y\nz\n"},
		{"a\r\nb\r\nc\n", "y\n\n"},
		{"a\n", "y\r"},
	} {
		f.Add([]byte(c[0]), c[1])
	}
	f.Fuzz(func(t *testing.T, start []byte, payload string) {
		patch := "@@ append a.txt\n" + payload
		p, err := Parse([]byte(patch), DefaultMarker)
		if err != nil || len(p.Hunks) != 1 {
			return // the payload held a directive, which is FuzzParse's ground
		}
		tree, root := fixture(t, map[string]string{"a.txt": string(start)})
		r, err := NewTxn(tree, Options{}).Run(p)
		if err != nil {
			t.Fatalf("an append to a file that exists failed: %v", err)
		}
		got := []byte(readFile(t, root, "a.txt"))

		eol := []byte(dominantEOL(start))
		rest, ok := bytes.CutPrefix(got, start)
		if !ok {
			t.Fatalf("the file's bytes were not kept:\nstart %q\ngot   %q", start, got)
		}
		seam := len(start) > 0 && start[len(start)-1] != '\n'
		if seam {
			if rest, ok = bytes.CutPrefix(rest, eol); !ok {
				t.Fatalf("no %q at the seam of %q: %q", eol, start, got)
			}
		}
		reported := len(r.Files) == 1 && r.Files[0].SeamAdded
		if reported != seam {
			t.Errorf("SeamAdded = %v for %q, want %v", reported, start, seam)
		}

		want := toEOL(p.Hunks[0].Body, string(eol))
		if len(want) > 0 && want[len(want)-1] != '\n' {
			want = append(want, eol...)
		}
		if !bytes.Equal(rest, want) {
			t.Errorf("after the file came %q, want %q\nstart %q, payload %q", rest, want, start, payload)
		}
	})
}

// §6.1 step 4 for a path that was absent at load: there is no hash to compare,
// so the guard is that it is still absent. A second writer creating it is
// exactly the case exit 6 exists for.
func TestCheckCatchesAFileCreatedUnderneath(t *testing.T) {
	tree, root := fixture(t, map[string]string{"a.txt": "x\n"})
	p, err := Parse([]byte("@@ create new.txt\nmine\n\n"), DefaultMarker)
	must(t, err)
	x := NewTxn(tree, Options{})
	must(t, x.Load(p))
	if f := x.Validate(p); len(f) != 0 {
		t.Fatalf("validate: %+v", f)
	}

	must(t, os.WriteFile(filepath.Join(root, "new.txt"), []byte("somebody else\n"), 0o644))
	var ce *ChangedError
	if err := x.Check(); !errors.As(err, &ce) {
		t.Fatalf("want *ChangedError, got %T: %v", err, err)
	}
	if got := readFile(t, root, "new.txt"); got != "somebody else\n" {
		t.Errorf("the other writer's file was overwritten: %q", got)
	}
}

func TestRollbackOfTheNewOps(t *testing.T) {
	t.Run("a create is removed, with the directories it made", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, []string{"--verify", "false"},
			"@@ create deep/new/made.txt\nmade\n\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		absent(t, root, "deep/new/made.txt")
		absent(t, root, "deep")
		assertUnchanged(t, root, before)
	})

	t.Run("a directory that already existed is not removed", func(t *testing.T) {
		root := cliTree(t, map[string]string{"sub/keep.txt": "x\n"})
		code, _, _ := runCLI(t, root, []string{"--verify", "false"},
			"@@ create sub/made.txt\nmade\n\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		if _, err := os.Stat(filepath.Join(root, "sub")); err != nil {
			t.Error("a directory hunk did not create was removed")
		}
	})

	t.Run("a delete is restored, bytes and mode", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "precious\n"})
		must(t, os.Chmod(filepath.Join(root, "a.txt"), 0o640))
		before := snapshot(t, root)
		code, _, _ := runCLI(t, root, []string{"--verify", "false"}, "@@ delete a.txt\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		assertUnchanged(t, root, before)
	})

	// The default never discards another writer's work, including at a path
	// hunk created or deleted.
	t.Run("a created file something else rewrote is left alone", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x\n"})
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "printf 'theirs\\n' > new.txt; false"},
			"@@ create new.txt\nmine\n\n")
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s", code, errOut)
		}
		if got := readFile(t, root, "new.txt"); got != "theirs\n" {
			t.Errorf("their work was discarded: %q", got)
		}
	})

	t.Run("a deleted file something else recreated is left alone", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "mine\n"})
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "printf 'theirs\\n' > a.txt; false"}, "@@ delete a.txt\n")
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s", code, errOut)
		}
		if got := readFile(t, root, "a.txt"); got != "theirs\n" {
			t.Errorf("their file was overwritten: %q", got)
		}
	})
}

// Two creates can share a directory the first of them made. Until 2026-09-21
// rollback tried each create's directories beside its own file, in commit
// order, so a shared one was tried while it still held the other file and
// never again: exit 3, and the directory stayed, on any filesystem. Two rows
// tell reverse commit order from forward, the shallow create first and the two
// new directories inside one: with every file already gone, forward order
// still tries a directory before one a later create made inside it.
func TestRollbackRemovesEveryDirectoryItMade(t *testing.T) {
	for _, c := range []struct{ name, patch string }{
		{
			"two creates in one new directory",
			"@@ create new/x.go\none\n\n@@ create new/y.go\ntwo\n\n",
		},
		{
			"two new directories inside one new directory",
			"@@ create new/a/x.go\none\n\n@@ create new/b/y.go\ntwo\n\n",
		},
		{
			"a deep create, then a shallow one at its top",
			"@@ create a/b/c/x.go\none\n\n@@ create a/y.go\ntwo\n\n",
		},
		{
			"a shallow create, then a deep one beneath it",
			"@@ create a/y.go\ntwo\n\n@@ create a/b/c/x.go\none\n\n",
		},
		{
			"two creates in a new directory inside one that existed",
			"@@ create sub/new/x.go\none\n\n@@ create sub/new/y.go\ntwo\n\n",
		},
		{
			"a modify between two creates",
			"@@ create new/x.go\none\n\n@@ file keep.txt\n@@ old\nkeep\n@@ new\nKEEP\n@@ create new/y.go\ntwo\n\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"keep.txt": "keep\n", "sub/keep.txt": "keep\n"})
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, []string{"--verify", "false"}, c.patch)
			if code != exitVerifyFailed {
				t.Fatalf("exit %d, want 3: %s", code, errOut)
			}
			if !strings.Contains(errOut, "verify failed, rolled back") {
				t.Errorf("err = %q", errOut)
			}
			assertUnchanged(t, root, before)
		})
	}

	// §6.6 removes a directory hunk made only if it is empty afterward, so
	// what the verify wrote into one keeps it.
	t.Run("a directory the verify wrote into stays, with what it wrote", func(t *testing.T) {
		root := cliTree(t, map[string]string{"keep.txt": "keep\n"})
		code, _, errOut := runCLI(t, root, []string{"--verify", "printf 'built\\n' > new/out.txt; false"},
			"@@ create new/x.go\none\n\n@@ create new/y.go\ntwo\n\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s", code, errOut)
		}
		absent(t, root, "new/x.go")
		absent(t, root, "new/y.go")
		if got := readFile(t, root, "new/out.txt"); got != "built\n" {
			t.Errorf("the verify's own file = %q", got)
		}
	})

	// The directory hunk made for a created file the verify deleted is empty,
	// and §6.6 has no exception for it. Until 2026-09-21 it stayed, and until
	// later the same day the file itself was exit 4:
	// TestACreatedFileTheVerifyDeletedIsRolledBack has that.
	t.Run("a created file the verify deleted leaves no directory", func(t *testing.T) {
		root := cliTree(t, map[string]string{"keep.txt": "keep\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, []string{"--verify", "rm new/x.go; false"},
			"@@ create new/x.go\none\n\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s", code, errOut)
		}
		assertUnchanged(t, root, before)
	})
}

// contentsOnly is a snapshot with each file's mode dropped, for a comparison
// that is about what is in the tree rather than how it is permitted.
func contentsOnly(s map[string]string) map[string]string {
	out := map[string]string{}
	for name, v := range s {
		if _, body, ok := strings.Cut(v, "\x00"); ok && v != "directory" {
			v = body
		}
		out[name] = v
	}
	return out
}

// Rolling a create back means the path is absent. A created file the verify
// already removed is absent, so it is rolled back, under either flag, and the
// exit is 3 if nothing else failed, decided 2026-09-21. Until then it was exit
// 4, "rollback was incomplete", on a tree exactly as it was before, and the
// message called a missing file "something else". Something other than hunk
// did remove it, which is worth knowing, so one line says so.
func TestACreatedFileTheVerifyDeletedIsRolledBack(t *testing.T) {
	const gone = "new/x.go, which hunk created, was already gone: something else removed it."
	create := "@@ create new/x.go\none\n\n"
	for _, c := range []struct {
		name   string
		args   []string
		patch  string
		exit   int
		header string // the report's first line, after "hunk: applied "
		says   string // and a line it must carry, or "" for none
		after  map[string]string
	}{
		{
			"by default",
			[]string{"--verify", "rm new/x.go; false"},
			create,
			exitVerifyFailed, "1 hunk, verify failed, rolled back 1 file", gone, nil,
		},
		{
			// It used to try the removal and print the syscall's ENOENT.
			"under --verify-may-format",
			[]string{"--verify", "rm new/x.go; false", "--verify-may-format"},
			create,
			exitVerifyFailed, "1 hunk, verify failed, rolled back 1 file", gone, nil,
		},
		{
			"the directory hunk made went with it",
			[]string{"--verify", "rm -r new; false"},
			create,
			exitVerifyFailed, "1 hunk, verify failed, rolled back 1 file", gone, nil,
		},
		{
			"one of two creates",
			[]string{"--verify", "rm new/x.go; false"},
			"@@ create new/x.go\none\n\n@@ create new/y.go\ntwo\n\n",
			exitVerifyFailed, "2 hunks, verify failed, rolled back 2 files", gone, nil,
		},
		{
			// Gone is not a pass for the rest of the batch. The modify the
			// verify rewrote is still left alone, and still exit 4.
			"beside a modify something rewrote",
			[]string{"--verify", "rm new/x.go; echo changed > keep.txt; false"},
			create + "@@ file keep.txt\n@@ old\nkeep\n@@ new\nKEEP\n",
			exitRollbackFailed, "2 hunks, verify failed, rolled back 1 file, 1 file left alone", gone,
			map[string]string{"keep.txt": "changed\n"},
		},
		{
			// A created file that is there and is not what hunk wrote is the
			// case exit 4 is for, and it is unchanged.
			"a created file something rewrote",
			[]string{"--verify", "echo other > new/x.go; false"},
			create,
			exitRollbackFailed, "1 hunk, verify failed, rolled back 0 files, 1 file left alone",
			"it is now something else",
			map[string]string{"new": "directory", "new/x.go": "other\n"},
		},
		{
			// Not a file, and not gone either.
			"a directory where the file was",
			[]string{"--verify", "rm new/x.go; mkdir new/x.go; false"},
			create,
			exitRollbackFailed, "1 hunk, verify failed, rolled back 0 files, 1 file left alone",
			"it is now something else",
			map[string]string{"new": "directory", "new/x.go": "directory"},
		},
		{
			// Nothing is rolled back, so nothing is found gone.
			"--keep-on-fail looks at nothing",
			[]string{"--verify", "rm new/x.go; false", "--keep-on-fail"},
			create,
			exitVerifyFailed, "1 hunk, verify failed, changes left in place for inspection (--keep-on-fail)", "",
			map[string]string{"new": "directory"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"keep.txt": "keep\n"})
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, c.args, c.patch)
			if code != c.exit {
				t.Fatalf("exit %d, want %d: %s", code, c.exit, errOut)
			}
			if head, _, _ := strings.Cut(errOut, "\n"); head != "hunk: applied "+c.header {
				t.Errorf("header = %q, want %q", head, "hunk: applied "+c.header)
			}
			if c.says != "" && !strings.Contains(errOut, c.says) {
				t.Errorf("the report does not say %q:\n%s", c.says, errOut)
			}
			if c.says != gone && strings.Contains(errOut, "already gone") {
				t.Errorf("said a file was gone that was not:\n%s", errOut)
			}
			if strings.Contains(errOut, "could not be removed") {
				t.Errorf("a missing file is not a failed removal:\n%s", errOut)
			}

			// The tree: as it was, apart from what the verify left there.
			// Compared by kind and content, since modes are not this rule's.
			want := maps.Clone(before)
			for name, v := range c.after {
				want[filepath.FromSlash(name)] = v
			}
			for _, d := range snapshotDiff(contentsOnly(want), contentsOnly(snapshot(t, root))) {
				t.Errorf("%s", d)
			}
		})
	}

	t.Run("golden", func(t *testing.T) {
		_, _, errOut := runCLI(t, cliTree(t, map[string]string{"keep.txt": "keep\n"}),
			[]string{"--verify", "rm new/x.go; printf 'FAIL\\n'; false"}, create)
		golden(t, "cli-verify-failed-created-gone", errOut)
	})

	t.Run("--json carries it beside the count", func(t *testing.T) {
		_, js, _ := runCLI(t, cliTree(t, map[string]string{"keep.txt": "keep\n"}),
			[]string{"--json", "--verify", "rm new/x.go; false"}, create)
		var v struct {
			Exit   int
			Verify struct {
				RolledBack  int               `json:"rolled_back"`
				Gone        []string          `json:"already_gone"`
				NotRestored []json.RawMessage `json:"not_restored"`
			}
		}
		must(t, json.Unmarshal([]byte(js), &v))
		if v.Exit != exitVerifyFailed || v.Verify.RolledBack != 1 || len(v.Verify.NotRestored) != 0 ||
			len(v.Verify.Gone) != 1 || v.Verify.Gone[0] != "new/x.go" {
			t.Errorf("%s", js)
		}
	})
}

// §5.1's first column. A and D have existed in opLetter since the report card
// and nothing has ever produced them.
func TestDiffstatLetters(t *testing.T) {
	root := cliTree(t, map[string]string{"gone.txt": "bye\n", "keep.txt": "one\n"})
	_, out, errOut := runCLI(t, root, nil,
		"@@ create made.txt\nnew\n\n@@ delete gone.txt\n@@ file keep.txt\n@@ old\none\n@@ new\nONE\n")
	if !strings.HasPrefix(out, "A made.txt") {
		t.Errorf("out = %q %q", out, errOut)
	}
	for _, want := range []string{"A made.txt", "D gone.txt", "M keep.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	golden(t, "cli-all-three-ops", out)
}

// The project's acceptance test, inherited from the-cli when §3.6's example
// turned out to need an op that did not exist. The example is taken from the
// string `hunk format` prints, not retyped, so it cannot drift from what the
// tool teaches.
func TestSpecWorkedExampleAppliesInFull(t *testing.T) {
	root := cliTree(t, map[string]string{
		"internal/cli/root.go": "import (\n\t\"github.com/kmoneil/jr/internal/registry\"\n)\n\n" +
			"func main() {\n\tbind.mustHaveBoundEveryGlobal()\n}\n",
		"internal/registry/globals.go": "const (\n\tGlobalProject\n\tGlobalOther\n\tGlobalProject\n)\n",
	})
	code, out, errOut := runCLI(t, root, nil, formatExamplePatch)
	if code != exitOK {
		t.Fatalf("the example `hunk format` prints does not apply: exit %d\n%s%s", code, out, errOut)
	}
	golden(t, "cli-spec-example-full", out)

	// §5.1's own rows for the two files that example actually edits.
	for _, want := range []string{
		"M internal/registry/globals.go +2 -2",
		"A internal/cli/scope.go        +4 -0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing §5.1's row %q:\n%s", want, out)
		}
	}
	created := readFile(t, root, "internal/cli/scope.go")
	if !strings.HasSuffix(created, "\n") {
		t.Errorf("the created Go file has no final newline: %q", created)
	}
	if !bytes.Contains([]byte(readFile(t, root, "internal/registry/globals.go")),
		[]byte("GlobalProject, GlobalScope")) {
		t.Error("the x2 hunk did not apply")
	}
}

// The failure paths the new ops added. Each is a real filesystem refusal
// reached with a read-only directory or a file where a directory belongs, not
// an injected error.
func TestNewOpFailurePaths(t *testing.T) {
	t.Run("a delete that cannot be performed", func(t *testing.T) {
		needsPOSIXPerms(t)
		root := cliTree(t, map[string]string{"sub/a.txt": "x\n"})
		sub := filepath.Join(root, "sub")
		must(t, os.Chmod(sub, 0o555))
		t.Cleanup(func() { os.Chmod(sub, 0o755) })
		code, _, errOut := runCLI(t, root, nil, "@@ delete sub/a.txt\n")
		if code == exitOK {
			t.Fatalf("deleted from a read-only directory: %s", errOut)
		}
		if readFile(t, root, "sub/a.txt") != "x\n" {
			t.Error("the file went away anyway")
		}
	})

	// Distinct from the case above: this one gets past load and validate and
	// fails in commit, which is the only phase that makes directories.
	t.Run("a create whose directory cannot be made", func(t *testing.T) {
		needsPOSIXPerms(t)
		root := cliTree(t, map[string]string{"sub/keep.txt": "x\n"})
		sub := filepath.Join(root, "sub")
		must(t, os.Chmod(sub, 0o555))
		t.Cleanup(func() { os.Chmod(sub, 0o755) })
		code, _, errOut := runCLI(t, root, nil, "@@ create sub/made/x.txt\nhi\n\n")
		if code == exitOK {
			t.Fatalf("made a directory in a read-only parent: %s", errOut)
		}
		absent(t, root, "sub/made")
	})

	t.Run("a create whose parent is a regular file", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x\n"})
		code, _, errOut := runCLI(t, root, nil, "@@ create a.txt/b/c.txt\nhi\n\n")
		if code == exitOK {
			t.Fatalf("created under a regular file: %s", errOut)
		}
	})

	t.Run("a create that rollback cannot remove", func(t *testing.T) {
		needsPOSIXPerms(t)
		root := cliTree(t, map[string]string{"sub/keep.txt": "x\n"})
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "chmod 555 sub; false"}, "@@ create sub/made.txt\nmade\n\n")
		os.Chmod(filepath.Join(root, "sub"), 0o755)
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s", code, errOut)
		}
		if !strings.Contains(errOut, "could not be removed") {
			t.Errorf("err = %q", errOut)
		}
	})

	// A directory hunk made that the verify then put something else into is not
	// hunk's to remove, and the unwind stops at it rather than failing.
	t.Run("rollback stops unwinding at a directory that is not empty", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x\n"})
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "printf 'theirs\\n' > deep/new/other.txt; false"},
			"@@ create deep/new/made.txt\nmade\n\n")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d, want 3: %s", code, errOut)
		}
		absent(t, root, "deep/new/made.txt")
		if got := readFile(t, root, "deep/new/other.txt"); got != "theirs\n" {
			t.Errorf("the other file was removed with the directory: %q", got)
		}
	})

	t.Run("a delete that rollback cannot restore", func(t *testing.T) {
		needsPOSIXPerms(t)
		root := cliTree(t, map[string]string{"sub/a.txt": "x\n"})
		code, _, errOut := runCLI(t, root,
			[]string{"--verify", "chmod 555 sub; false"}, "@@ delete sub/a.txt\n")
		os.Chmod(filepath.Join(root, "sub"), 0o755)
		if code != exitRollbackFailed {
			t.Fatalf("exit %d, want 4: %s", code, errOut)
		}
		if !strings.Contains(errOut, "could not be written") {
			t.Errorf("err = %q", errOut)
		}
	})

	t.Run("--dry-run surfaces a load failure", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x\n"})
		code, _, errOut := runCLI(t, root, []string{"--dry-run"},
			"@@ create ../outside.txt\nhi\n\n")
		if code != exitNoMatch {
			t.Errorf("exit %d, want 2", code)
		}
		if !strings.Contains(errOut, "climbs out") {
			t.Errorf("err = %q", errOut)
		}
	})
}

// §6.4's open question, answered: a file that does not exist yet has no
// dominant line ending to convert a payload to, so the payload is written as
// given and the new file's ending is sampled from it. Everything that follows
// in the batch uses that ending.
//
// The patches below spell their payload bytes out. A payload line ending CRLF
// is written as a line ending in \r, and the newline after it is the patch's
// line separator, not part of the payload. Three separate expectations in this
// file were wrong before that was written down instead of wrapped in a helper.
func TestACreatedFileKeepsItsOwnLineEndings(t *testing.T) {
	for _, c := range []struct {
		name  string
		patch string
		want  string
	}{
		{
			// payload "a\r\nb\r\n": two CRLF lines, the blank line giving the
			// second one its ending.
			name:  "a CRLF payload creates a CRLF file",
			patch: "@@ create new.txt\na\r\nb\r\n\n",
			want:  "a\r\nb\r\n",
		},
		{
			// payload "a\r\nb": CRLF-dominant and ending in no newline, so the
			// append's seam fires and must use \r\n rather than \n.
			name:  "the seam byte follows the created file's ending",
			patch: "@@ create new.txt\na\r\nb\n@@ append new.txt\nc\n\n",
			want:  "a\r\nb\r\nc\r\n",
		},
		{
			// The appended payload is converted to the created file's ending.
			name:  "an append is converted to the created file's ending",
			patch: "@@ create new.txt\na\r\nb\r\n\n@@ append new.txt\nc\n\n",
			want:  "a\r\nb\r\nc\r\n",
		},
		{
			name:  "an LF payload creates an LF file",
			patch: "@@ create new.txt\na\nb\n@@ append new.txt\nc\n\n",
			want:  "a\nb\nc\n",
		},
		{
			name:  "a payload with no newline at all falls back to LF",
			patch: "@@ create new.txt\nsolo\n@@ append new.txt\nnext\n\n",
			want:  "solo\nnext\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"seed.txt": "x\n"})
			must(t, run2(t, tree, c.patch))
			if got := readFile(t, root, "new.txt"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
