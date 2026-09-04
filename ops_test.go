package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		if fi.Mode().Perm() != createMode {
			t.Errorf("mode %v, want %v", fi.Mode().Perm(), createMode)
		}
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

// The seam (§3.3, decided 2026-09-04). A weld happens when the text on the left
// of the seam does not end in a newline: for append that is the file, for
// prepend it is the payload. Both are normalized, and the inserted byte is
// reported rather than added quietly.
func TestTheSeam(t *testing.T) {
	for _, c := range []struct {
		name      string
		start     string
		patch     string
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
			name: "append a payload with no final newline", start: "x\n",
			patch: "@@ append a.txt\ny\n", want: "x\ny",
		},
		{
			name: "append to an empty file adds no seam", start: "",
			patch: "@@ append a.txt\ny\n\n", want: "y\n",
		},
		{
			// §3.3 never gives a payload a trailing newline, so without
			// normalizing this welds every time: "yx\n".
			name: "prepend a payload with no final newline", start: "x\n",
			patch: "@@ prepend a.txt\ny\n", want: "y\nx\n", wantAdded: true,
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
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"a.txt": c.start})
			r, err := run(t, tree, c.patch, Options{})
			must(t, err)
			if got := readFile(t, root, "a.txt"); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if r.Files[0].SeamAdded != c.wantAdded {
				t.Errorf("SeamAdded = %v, want %v", r.Files[0].SeamAdded, c.wantAdded)
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
