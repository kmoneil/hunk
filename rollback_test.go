package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Rollback runs after --verify, and the verify is a build or a test with the
// run of the tree. These tests have it do to the tree what such a command can,
// replace a file with a symbolic link, a directory with a file, and then fail,
// and assert what rollback does about it and the whole tree afterwards. Until
// 2026-10-07 every rollback step acted on a name through whatever stood there:
// it removed the verify's link and its file, and renamed regular files over its
// links, under an exit 3 that said the tree was put back.

// treeOf is a tree as these tests compare it: a file by its contents, a
// directory as "directory", a symbolic link as "-> " and where it points.
// Modes are not this card's subject, and other tests pin them.
func treeOf(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, v := range snapshot(t, root) {
		name = filepath.ToSlash(name)
		switch {
		case v == "directory":
			out[name] = v
		case strings.HasPrefix(v, "symlink\x00"):
			out[name] = "-> " + filepath.ToSlash(strings.TrimPrefix(v, "symlink\x00"))
		default:
			_, body, _ := strings.Cut(v, "\x00")
			out[name] = body
		}
	}
	return out
}

// treeDiff names what differs between two trees as treeOf gives them.
func treeDiff(want, got map[string]string) []string {
	var d []string
	for name, w := range want {
		if g, ok := got[name]; !ok {
			d = append(d, fmt.Sprintf("%s is missing, want %q", name, w))
		} else if g != w {
			d = append(d, fmt.Sprintf("%s is %q, want %q", name, g, w))
		}
	}
	for name, g := range got {
		if _, ok := want[name]; !ok {
			d = append(d, fmt.Sprintf("%s is there (%q), and should not be", name, g))
		}
	}
	sort.Strings(d)
	return d
}

// realDir is a fresh directory with symlinks in its own path resolved, as
// cliTree's root is, so a verify command can name it the way hunk sees it.
func realDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if r, err := filepath.EvalSymlinks(d); err == nil {
		d = r
	}
	return d
}

type rollbackCase struct {
	name    string
	files   map[string]string // the tree before
	outside map[string]string // a directory beside the tree that no patch names
	patch   string
	flags   []string
	verify  string // "{outside}" is replaced by the outside directory
	links   bool   // the verify makes a symbolic link with ln -s
	perms   bool   // the verify takes a permission away with chmod
	// chmodBack is what the verify made unreadable or unwritable, given back
	// before the tree is read and whatever happens.
	chmodBack []string
	exit      int
	says      []string // in the report
	not       []string // not in the report
	after     map[string]string
	// outsideAfter is the outside directory afterwards; nil means unchanged,
	// which is what every row asserts unless it says otherwise.
	outsideAfter map[string]string
}

func runRollbackCases(t *testing.T, cases []rollbackCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.links {
				needsShellSymlinks(t)
			}
			if c.perms {
				needsPOSIXPerms(t)
			}
			root := cliTree(t, c.files)
			giveBack := func() {
				for _, d := range c.chmodBack {
					_ = os.Chmod(filepath.Join(root, filepath.FromSlash(d)), 0o755)
				}
			}
			t.Cleanup(giveBack)
			outside := realDir(t)
			for name, body := range c.outside {
				full := filepath.Join(outside, filepath.FromSlash(name))
				must(t, os.MkdirAll(filepath.Dir(full), 0o755))
				must(t, os.WriteFile(full, []byte(body), 0o644))
			}
			outsideBefore := treeOf(t, outside)
			verify := strings.ReplaceAll(c.verify, "{outside}", filepath.ToSlash(outside))
			args := append(append([]string{}, c.flags...), "--verify", verify)
			code, _, errOut := runCLI(t, root, args, c.patch)
			giveBack()
			if code != c.exit {
				t.Errorf("exit %d, want %d:\n%s", code, c.exit, errOut)
			}
			for _, s := range c.says {
				if !strings.Contains(errOut, s) {
					t.Errorf("the report does not say %q:\n%s", s, errOut)
				}
			}
			for _, s := range c.not {
				if strings.Contains(errOut, s) {
					t.Errorf("the report says %q, and should not:\n%s", s, errOut)
				}
			}
			want := map[string]string{}
			for name, v := range c.after {
				want[name] = strings.ReplaceAll(v, "{outside}", filepath.ToSlash(outside))
			}
			for _, d := range treeDiff(want, treeOf(t, root)) {
				t.Errorf("the tree: %s", d)
			}
			wantOutside := c.outsideAfter
			if wantOutside == nil {
				wantOutside = outsideBefore
			}
			for _, d := range treeDiff(wantOutside, treeOf(t, outside)) {
				t.Errorf("outside the tree: %s", d)
			}
		})
	}
}

const (
	createA     = "@@ create a.txt\nhello\n"
	deleteD     = "@@ delete d.txt\n"
	modifyM     = "@@ file m.txt\n@@ old\nbefore\n@@ new\nafter\n"
	createInNew = "@@ create new/dir/f.txt\nhello\n"
)

// What stands at a name hunk wrote, or deleted, is asked with Lstat: a link is
// a link, whatever its destination holds, and only nothing at all is absence.
func TestRollbackAsksWhatStandsAtTheName(t *testing.T) {
	linkLeft := "which hunk did not make"
	runRollbackCases(t, []rollbackCase{
		{
			// Until 2026-10-07 its hash was read through the link, matched,
			// and the verify's link was removed at exit 3.
			name:  "a created file replaced by a link to a file holding hunk's bytes",
			files: map[string]string{"b.txt": "hello\n"},
			patch: createA, verify: "rm a.txt && ln -s b.txt a.txt; false", links: true,
			exit:  exitRollbackFailed,
			says:  []string{"a.txt was not restored", "hunk created it as a file, and it is now a symbolic link, " + linkLeft},
			after: map[string]string{"a.txt": "-> b.txt", "b.txt": "hello\n"},
		},
		{
			name:  "the same under --verify-may-format, which licenses rewritten files, not links",
			files: map[string]string{"b.txt": "hello\n"},
			patch: createA, flags: []string{"--verify-may-format"},
			verify: "rm a.txt && ln -s b.txt a.txt; false", links: true,
			exit:  exitRollbackFailed,
			says:  []string{"it is now a symbolic link, " + linkLeft},
			after: map[string]string{"a.txt": "-> b.txt", "b.txt": "hello\n"},
		},
		{
			// Until 2026-10-07 a link to nothing read as absent: "already
			// gone", exit 3, with the link still there.
			name:  "a created file replaced by a link to nothing",
			patch: createA, verify: "rm a.txt && ln -s nowhere a.txt; false", links: true,
			exit:  exitRollbackFailed,
			says:  []string{"it is now a symbolic link, " + linkLeft},
			not:   []string{"already gone"},
			after: map[string]string{"a.txt": "-> nowhere"},
		},
		{
			name:  "a created file replaced by a link to itself",
			patch: createA, verify: "rm a.txt && ln -s a.txt a.txt; false", links: true,
			exit:  exitRollbackFailed,
			says:  []string{"it is now a symbolic link, " + linkLeft},
			not:   []string{"already gone"},
			after: map[string]string{"a.txt": "-> a.txt"},
		},
		{
			name:  "a created file the verify removed is still gone, and rolled back",
			patch: createA, verify: "rm a.txt; false",
			exit:  exitVerifyFailed,
			says:  []string{"a.txt, which hunk created, was already gone"},
			after: map[string]string{},
		},
		{
			// Until 2026-10-07 a link to nothing read as absent, and the
			// restore renamed a regular file over the verify's link.
			name:  "a deleted file the verify put a link to nothing in place of",
			files: map[string]string{"d.txt": "orig\n"},
			patch: deleteD, verify: "ln -s nowhere d.txt; false", links: true,
			exit: exitRollbackFailed,
			says: []string{
				"d.txt was not restored",
				"hunk deleted it and something has since put a symbolic link there,\n  so restoring would replace that link with a file.",
			},
			not:   []string{"--verify-may-format says"},
			after: map[string]string{"d.txt": "-> nowhere"},
		},
		{
			name:  "the same under --verify-may-format: a link is never overwritten",
			files: map[string]string{"d.txt": "orig\n"},
			patch: deleteD, flags: []string{"--verify-may-format"},
			verify: "ln -s nowhere d.txt; false", links: true,
			exit:  exitRollbackFailed,
			says:  []string{"put a symbolic link there"},
			after: map[string]string{"d.txt": "-> nowhere"},
		},
		{
			name:  "a deleted file the verify put a link to a file in place of",
			files: map[string]string{"d.txt": "orig\n", "other.txt": "other\n"},
			patch: deleteD, verify: "ln -s other.txt d.txt; false", links: true,
			exit:  exitRollbackFailed,
			says:  []string{"put a symbolic link there"},
			after: map[string]string{"d.txt": "-> other.txt", "other.txt": "other\n"},
		},
		{
			// The control: a regular file where hunk deleted one is refused
			// by default, as it always was, and the flag still says how.
			name:  "a deleted file the verify recreated as a file",
			files: map[string]string{"d.txt": "orig\n"},
			patch: deleteD, verify: "echo new > d.txt; false",
			exit:  exitRollbackFailed,
			says:  []string{"hunk deleted it and something has since created it there", "--verify-may-format says"},
			after: map[string]string{"d.txt": "new\n"},
		},
		{
			// What stops the Lstat stops the write after it, and that is named.
			name:  "a deleted file under a directory the verify made unreadable",
			files: map[string]string{"sub/d.txt": "orig\n"},
			patch: "@@ delete sub/d.txt\n", verify: "chmod 000 sub; false",
			perms: true, chmodBack: []string{"sub"},
			exit:  exitRollbackFailed,
			says:  []string{"sub/d.txt was not restored", "It could not be written"},
			after: map[string]string{"sub": "directory"},
		},
		{
			name:  "a deleted file restored when nothing is there",
			files: map[string]string{"d.txt": "orig\n"},
			patch: deleteD, verify: "false",
			exit:  exitVerifyFailed,
			after: map[string]string{"d.txt": "orig\n"},
		},
		{
			// Until 2026-10-07 the hash was read through the link and
			// matched, and the restore renamed a regular file over the link.
			name:  "a modified file replaced by a link to a file holding hunk's bytes",
			files: map[string]string{"m.txt": "before\n", "other.txt": "after\n"},
			patch: modifyM, verify: "rm m.txt && ln -s other.txt m.txt; false", links: true,
			exit: exitRollbackFailed,
			says: []string{
				"m.txt was not restored", "and it is now a symbolic link, " + linkLeft,
				"so restoring would replace it with a file.",
			},
			after: map[string]string{"m.txt": "-> other.txt", "other.txt": "after\n"},
		},
		{
			name:  "the same under --verify-may-format",
			files: map[string]string{"m.txt": "before\n", "other.txt": "after\n"},
			patch: modifyM, flags: []string{"--verify-may-format"},
			verify: "rm m.txt && ln -s other.txt m.txt; false", links: true,
			exit:  exitRollbackFailed,
			says:  []string{"it is now a symbolic link"},
			after: map[string]string{"m.txt": "-> other.txt", "other.txt": "after\n"},
		},
		{
			// Until 2026-10-07 this said the file was gone from disk.
			name:  "a modified file replaced by a directory",
			files: map[string]string{"m.txt": "before\n"},
			patch: modifyM, verify: "rm m.txt && mkdir m.txt; false",
			exit:  exitRollbackFailed,
			says:  []string{"and it is now a directory, " + linkLeft},
			not:   []string{"gone from disk"},
			after: map[string]string{"m.txt": "directory"},
		},
		{
			name:  "a modified file the verify removed, under --verify-may-format, is written back",
			files: map[string]string{"m.txt": "before\n"},
			patch: modifyM, flags: []string{"--verify-may-format"}, verify: "rm m.txt; false",
			exit:  exitVerifyFailed,
			after: map[string]string{"m.txt": "before\n"},
		},
	})
}

// A directory hunk made is removed only while it is the directory hunk made,
// and one left is named, at exit 3: the batch's own changes are undone.
func TestRollbackRemovesOnlyTheDirectoriesItMade(t *testing.T) {
	runRollbackCases(t, []rollbackCase{
		{
			// Until 2026-10-07 the verify's file was unlinked as though it
			// were hunk's directory, against the report's own words.
			name:  "a made directory replaced by a file",
			patch: createInNew, verify: "rm -rf new/dir && echo keep > new/dir; false",
			exit: exitRollbackFailed,
			says: []string{
				"new/dir/f.txt was not restored.\n  new/dir, the directory hunk created it in, is now a file,\n  so hunk did not look for it there.",
				"new/dir, which hunk made as a directory, is now a file, so hunk left it.",
			},
			after: map[string]string{"new": "directory", "new/dir": "keep\n"},
		},
		{
			name:  "a made directory replaced by a link to a directory in the tree",
			files: map[string]string{"elsewhere/keep.txt": "keep\n"},
			patch: createInNew, verify: "rm -rf new/dir && ln -s ../elsewhere new/dir; false", links: true,
			// Through the link the name was absent, and read as already gone,
			// until the path was checked on 2026-10-07: the directory hunk
			// created the file in is not the one there now.
			exit: exitRollbackFailed,
			says: []string{
				"new/dir, the directory hunk created it in, is now a symbolic link,",
				"new/dir, which hunk made as a directory, is now a symbolic link, so hunk left it.",
			},
			not: []string{"already gone"},
			after: map[string]string{
				"new": "directory", "new/dir": "-> ../elsewhere",
				"elsewhere": "directory", "elsewhere/keep.txt": "keep\n",
			},
		},
		{
			// Made beside the original before that is removed, so the two
			// cannot share an inode number by reuse.
			name:  "a made directory replaced by another, empty, directory",
			patch: createInNew, verify: "mv new/dir new/old && mkdir new/dir && rm -rf new/old; false",
			exit: exitRollbackFailed,
			says: []string{
				"new/dir, the directory hunk created it in, is now another directory,",
				"new/dir, which hunk made as a directory, is now another directory, so hunk left it.",
			},
			not:   []string{"already gone"},
			after: map[string]string{"new": "directory", "new/dir": "directory"},
		},
		{
			// It used to be left silently.
			name:  "a made directory the verify wrote into",
			patch: "@@ create new/f.txt\nhello\n", verify: "echo x > new/other.txt; false",
			exit:  exitVerifyFailed,
			says:  []string{"new, which hunk made as a directory, is not empty, so hunk left it."},
			after: map[string]string{"new": "directory", "new/other.txt": "x\n"},
		},
		{
			// Until 2026-10-07 the gone directory stopped the unwinding, and
			// new/, which hunk made and which was empty, stayed under exit 3.
			name:  "a made directory the verify removed, with the file in it",
			patch: createInNew, verify: "rm -rf new/dir; false",
			exit:  exitVerifyFailed,
			says:  []string{"new/dir/f.txt, which hunk created, was already gone"},
			not:   []string{"so hunk left it"},
			after: map[string]string{},
		},
		{
			name:  "a made directory the verify made unsearchable",
			patch: createInNew, verify: "chmod 000 new; false",
			perms: true, chmodBack: []string{"new"},
			exit: exitRollbackFailed,
			says: []string{
				"new/dir, the directory hunk created it in, could not be checked (permission denied),",
				"new/dir, which hunk made as a directory, could not be checked (permission denied), so hunk left it.",
			},
			after: map[string]string{"new": "directory", "new/dir": "directory", "new/dir/f.txt": "hello"},
		},
		{
			// The file is removed, so the batch's own change is undone; the
			// directory it was in could not be, and says why.
			name:  "a made directory whose parent the verify made unwritable",
			patch: createInNew, verify: "chmod 555 new; false",
			perms: true, chmodBack: []string{"new"},
			exit: exitVerifyFailed,
			says: []string{
				"new/dir, which hunk made as a directory, could not be removed (permission denied), so hunk left it.",
			},
			after: map[string]string{"new": "directory", "new/dir": "directory"},
		},
		{
			name:  "made directories removed when nothing touched them",
			patch: createInNew, verify: "false",
			exit:  exitVerifyFailed,
			not:   []string{"so hunk left it", "already gone"},
			after: map[string]string{},
		},
	})
}

// Check asks with Lstat too. A link put at a path the batch creates, between
// load and the check, is a second writer: exit 6, nothing written, and the
// link still there. Until 2026-10-07 a link to nothing read as absent, and
// commit renamed a regular file over it.
func TestCheckSeesALinkWhereTheBatchCreates(t *testing.T) {
	for _, c := range []struct{ name, dest string }{
		{"a link to nothing", "nowhere"},
		{"a link to itself", "a.txt"},
		{"a link to a file", "b.txt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"b.txt": "b\n"})
			p, err := Parse([]byte(createA), DefaultMarker)
			must(t, err)
			x := NewTxn(tree, Options{})
			must(t, x.Load(p))
			if f := x.Validate(p); len(f) != 0 {
				t.Fatalf("validate: %+v", f)
			}
			must(t, os.Symlink(c.dest, filepath.Join(root, "a.txt")))
			before := snapshot(t, root)
			err = x.Check()
			var ce *ChangedError
			if !errors.As(err, &ce) || ce.Path != "a.txt" {
				t.Fatalf("Check = %v, want a *ChangedError for a.txt", err)
			}
			if ExitCode(err) != exitChanged {
				t.Errorf("exit %d, want %d", ExitCode(err), exitChanged)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// fixedSeconds pins how long the verify took, the one field of a real run's
// JSON that is different every time, so the rest of it can be a golden.
func fixedSeconds(js string) string {
	return regexp.MustCompile(`"seconds": [0-9.e+-]+`).ReplaceAllString(js, `"seconds": 0`)
}

// The directories rollback left are in the JSON beside the files, in the same
// words, and the two renderings are goldened.
func TestRollbackLeavesAreReported(t *testing.T) {
	const verify = "echo x > new/other.txt; printf 'FAIL\\n'; false"
	patch := "@@ create new/f.txt\nhello\n"

	t.Run("text golden", func(t *testing.T) {
		_, _, errOut := runCLI(t, cliTree(t, nil), []string{"--verify", verify}, patch)
		golden(t, "cli-rollback-left-a-directory", errOut)
	})
	t.Run("json golden, and the shape", func(t *testing.T) {
		code, js, _ := runCLI(t, cliTree(t, nil), []string{"--json", "--verify", verify}, patch)
		if code != exitVerifyFailed {
			t.Errorf("exit %d, want %d", code, exitVerifyFailed)
		}
		golden(t, "cli-rollback-left-a-directory-json", fixedSeconds(js))
		var v struct {
			Verify struct {
				RolledBack int `json:"rolled_back"`
				Left       []struct {
					Path  string `json:"path"`
					State string `json:"state"`
				} `json:"dirs_left"`
			}
		}
		must(t, json.Unmarshal([]byte(js), &v))
		if v.Verify.RolledBack != 1 || len(v.Verify.Left) != 1 ||
			v.Verify.Left[0].Path != "new" || v.Verify.Left[0].State != "is not empty" {
			t.Errorf("verify = %+v", v.Verify)
		}
	})
	t.Run("--try reports them too", func(t *testing.T) {
		code, js, _ := runCLI(t, cliTree(t, nil), []string{"--json", "--try", "echo x > new/other.txt"}, patch)
		if code != exitOK {
			t.Errorf("exit %d, want the command's own 0", code)
		}
		var v struct {
			Try struct {
				Left []struct{ Path, State string } `json:"dirs_left"`
			}
		}
		must(t, json.Unmarshal([]byte(js), &v))
		if len(v.Try.Left) != 1 || v.Try.Left[0].Path != "new" {
			t.Errorf("try = %+v", v.Try)
		}
	})
	t.Run("a link rollback did not put there, golden", func(t *testing.T) {
		needsShellSymlinks(t)
		root := cliTree(t, map[string]string{"d.txt": "orig\n"})
		_, _, errOut := runCLI(t, root, []string{"--verify", "ln -s nowhere d.txt; printf 'FAIL\\n'; false"}, deleteD)
		golden(t, "cli-rollback-left-a-link", errOut)
		code, js, _ := runCLI(t, cliTree(t, map[string]string{"d.txt": "orig\n"}),
			[]string{"--json", "--verify", "ln -s nowhere d.txt; false"}, deleteD)
		if code != exitRollbackFailed {
			t.Errorf("exit %d, want %d", code, exitRollbackFailed)
		}
		golden(t, "cli-rollback-left-a-link-json", fixedSeconds(js))
	})
}

// irregular is a FileInfo for the one kind of entry no POSIX filesystem gives
// this suite: Windows reports some reparse points as irregular.
type irregular struct{ fs.FileInfo }

func (irregular) Mode() fs.FileMode { return fs.ModeIrregular }

// kindOf is how a message names what stands where hunk expected its own file
// or directory.
func TestKindOf(t *testing.T) {
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644))
	must(t, os.Mkdir(filepath.Join(root, "d"), 0o755))
	must(t, os.Symlink("f", filepath.Join(root, "l")))
	for name, want := range map[string]string{"f": "a file", "d": "a directory", "l": "a symbolic link"} {
		fi, err := os.Lstat(filepath.Join(root, name))
		must(t, err)
		if got := kindOf(fi); got != want {
			t.Errorf("kindOf(%s) = %q, want %q", name, got, want)
		}
	}
	if got := kindOf(irregular{}); got != "something else" {
		t.Errorf("kindOf(irregular) = %q, want %q", got, "something else")
	}
	t.Run("a named pipe", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "p")
		makeFIFO(t, p)
		fi, err := os.Lstat(p)
		must(t, err)
		if got := kindOf(fi); got != "a named pipe" {
			t.Errorf("kindOf(fifo) = %q, want %q", got, "a named pipe")
		}
	})
}

// Every directory on a file's path is checked before rollback acts on it, and
// a file is left, and named at exit 4, when one is not the directory commit
// went through. Until 2026-10-07 rollback walked the name again through
// whatever the verify left, and a directory swapped for a link sent the
// original bytes, a restored file or a removal to wherever the link led: in
// the tree under default flags, and outside it under --allow-outside-root.
func TestRollbackChecksTheDirectoriesOnThePath(t *testing.T) {
	const (
		modifySub = "@@ file sub/a.txt\n@@ old\nbefore\n@@ new\nafter\n"
		deleteSub = "@@ delete sub/d.txt\n"
		wroteIn   = "sub, the directory hunk wrote it in, is now a symbolic link,\n  so hunk wrote nothing there."
		deletedIn = "sub, the directory hunk deleted it from, is now a symbolic link,\n  so hunk wrote nothing there."
		createdIn = "new/dir, the directory hunk created it in, is now a symbolic link,\n  so hunk did not look for it there."
	)
	swap := "mv sub sub.real && ln -s other sub"
	runRollbackCases(t, []rollbackCase{
		{
			// The original went into the new sub/a.txt, and sub.real/a.txt,
			// which is the file hunk wrote, kept the edit.
			name:  "a parent replaced by another directory",
			files: map[string]string{"sub/a.txt": "before\n"},
			patch: modifySub, verify: "mv sub sub.real && mkdir sub && cp sub.real/a.txt sub/; false",
			exit: exitRollbackFailed,
			says: []string{"sub/a.txt was not restored", "sub, the directory hunk wrote it in, is now another directory"},
			after: map[string]string{
				"sub": "directory", "sub/a.txt": "after\n",
				"sub.real": "directory", "sub.real/a.txt": "after\n",
			},
		},
		{
			// other/a.txt was overwritten with sub/a.txt's original.
			name:  "a parent replaced by a link, to a file holding hunk's bytes",
			files: map[string]string{"sub/a.txt": "before\n", "other/a.txt": "after\n"},
			patch: modifySub, verify: swap + "; false", links: true,
			exit: exitRollbackFailed,
			says: []string{wroteIn},
			after: map[string]string{
				"sub": "-> other", "sub.real": "directory", "sub.real/a.txt": "after\n",
				"other": "directory", "other/a.txt": "after\n",
			},
		},
		{
			name:  "the same under --verify-may-format, to a file of somebody else's",
			files: map[string]string{"sub/a.txt": "before\n", "other/a.txt": "precious\n"},
			patch: modifySub, flags: []string{"--verify-may-format"}, verify: swap + "; false", links: true,
			exit: exitRollbackFailed,
			says: []string{wroteIn},
			after: map[string]string{
				"sub": "-> other", "sub.real": "directory", "sub.real/a.txt": "after\n",
				"other": "directory", "other/a.txt": "precious\n",
			},
		},
		{
			// d.txt was restored into other/, a directory no patch named.
			name:  "a deleted file's parent replaced by a link",
			files: map[string]string{"sub/d.txt": "orig\n", "other/keep.txt": "keep\n"},
			patch: deleteSub, verify: swap + "; false", links: true,
			exit: exitRollbackFailed,
			says: []string{"sub/d.txt was not restored", deletedIn},
			after: map[string]string{
				"sub": "-> other", "sub.real": "directory",
				"other": "directory", "other/keep.txt": "keep\n",
			},
		},
		{
			// It was written outside the tree.
			name:  "a deleted file's parent replaced by a link out of the tree, unconfined",
			files: map[string]string{"sub/d.txt": "orig\n"},
			patch: deleteSub, flags: []string{"--allow-outside-root"},
			verify: "mv sub sub.real && ln -s {outside} sub; false", links: true,
			outside: map[string]string{"keep.txt": "keep\n"},
			exit:    exitRollbackFailed,
			says:    []string{deletedIn},
			after:   map[string]string{"sub": "-> {outside}", "sub.real": "directory"},
		},
		{
			// os.Root refused this one already; now the walk does, first.
			name:  "the same, confined",
			files: map[string]string{"sub/d.txt": "orig\n"},
			patch: deleteSub, verify: "mv sub sub.real && ln -s {outside} sub; false", links: true,
			outside: map[string]string{"keep.txt": "keep\n"},
			exit:    exitRollbackFailed,
			says:    []string{deletedIn},
			not:     []string{"escapes"},
			after:   map[string]string{"sub": "-> {outside}", "sub.real": "directory"},
		},
		{
			// elsewhere/f.txt, which no patch named, was deleted at exit 3.
			name:  "a created file's made directory replaced by a link in the tree, to hunk's bytes",
			files: map[string]string{"elsewhere/f.txt": "hello"},
			patch: createInNew, verify: "rm -rf new/dir && ln -s ../elsewhere new/dir; false", links: true,
			exit: exitRollbackFailed,
			says: []string{createdIn},
			after: map[string]string{
				"new": "directory", "new/dir": "-> ../elsewhere",
				"elsewhere": "directory", "elsewhere/f.txt": "hello",
			},
		},
		{
			name:  "the same under --verify-may-format, to somebody else's file",
			files: map[string]string{"elsewhere/f.txt": "precious"},
			patch: createInNew, flags: []string{"--verify-may-format"},
			verify: "rm -rf new/dir && ln -s ../elsewhere new/dir; false", links: true,
			exit: exitRollbackFailed,
			says: []string{createdIn},
			after: map[string]string{
				"new": "directory", "new/dir": "-> ../elsewhere",
				"elsewhere": "directory", "elsewhere/f.txt": "precious",
			},
		},
		{
			// outside/f.txt was deleted, outside the tree.
			name:  "a created file's made directory replaced by a link out of the tree, unconfined",
			patch: createInNew, flags: []string{"--allow-outside-root"},
			verify: "rm -rf new/dir && ln -s {outside} new/dir; false", links: true,
			outside: map[string]string{"f.txt": "hello"},
			exit:    exitRollbackFailed,
			says:    []string{createdIn},
			after:   map[string]string{"new": "directory", "new/dir": "-> {outside}"},
		},
		{
			name:  "the same under --verify-may-format",
			patch: createInNew, flags: []string{"--allow-outside-root", "--verify-may-format"},
			verify: "rm -rf new/dir && ln -s {outside} new/dir; false", links: true,
			outside: map[string]string{"f.txt": "precious"},
			exit:    exitRollbackFailed,
			says:    []string{createdIn},
			after:   map[string]string{"new": "directory", "new/dir": "-> {outside}"},
		},
		{
			// a/b is the very directory commit wrote in, moved: only the walk
			// from the top sees that a, above it, is now a link. A check of the
			// immediate parent alone restores the file through the link.
			name:   "a directory two levels up replaced by a link to where its child went",
			files:  map[string]string{"a/b/c.txt": "before\n"},
			patch:  "@@ file a/b/c.txt\n@@ old\nbefore\n@@ new\nafter\n",
			verify: "mkdir elsewhere && mv a/b elsewhere/b && rmdir a && ln -s elsewhere a; false", links: true,
			exit: exitRollbackFailed,
			says: []string{"a/b/c.txt was not restored", "a, the directory hunk wrote it in, is now a symbolic link"},
			after: map[string]string{
				"a": "-> elsewhere", "elsewhere": "directory", "elsewhere/b": "directory",
				"elsewhere/b/c.txt": "after\n",
			},
		},
		{
			name:  "a parent the verify removed",
			files: map[string]string{"sub/a.txt": "before\n"},
			patch: modifySub, verify: "rm -rf sub; false",
			exit:  exitRollbackFailed,
			says:  []string{"sub, the directory hunk wrote it in, is gone,\n  so hunk wrote nothing there."},
			after: map[string]string{},
		},
		{
			name:  "a parent untouched, under a parent untouched",
			files: map[string]string{"a/b/c.txt": "before\n"},
			patch: "@@ file a/b/c.txt\n@@ old\nbefore\n@@ new\nafter\n", verify: "false",
			exit:  exitVerifyFailed,
			after: map[string]string{"a": "directory", "a/b": "directory", "a/b/c.txt": "before\n"},
		},
	})
}

// --try goes through the same rollback, and its exit is 4 when a file could
// not be put back, whatever its command said.
func TestTryChecksTheDirectoriesOnThePath(t *testing.T) {
	needsShellSymlinks(t)
	for _, c := range []struct {
		name  string
		files map[string]string
		patch string
		flags []string
	}{
		{
			"a modify",
			map[string]string{"sub/a.txt": "before\n", "other/a.txt": "after\n"},
			"@@ file sub/a.txt\n@@ old\nbefore\n@@ new\nafter\n", nil,
		},
		{
			"a modify under --verify-may-format",
			map[string]string{"sub/a.txt": "before\n", "other/a.txt": "precious\n"},
			"@@ file sub/a.txt\n@@ old\nbefore\n@@ new\nafter\n",
			[]string{"--verify-may-format"},
		},
		{
			"a delete",
			map[string]string{"sub/d.txt": "orig\n", "other/keep.txt": "keep\n"},
			"@@ delete sub/d.txt\n", nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			otherBefore := treeOf(t, filepath.Join(root, "other"))
			args := append(append([]string{}, c.flags...), "--try", "mv sub sub.real && ln -s other sub")
			code, _, errOut := runCLI(t, root, args, c.patch)
			if code != exitRollbackFailed {
				t.Errorf("exit %d, want %d:\n%s", code, exitRollbackFailed, errOut)
			}
			if !strings.Contains(errOut, ", the directory hunk ") || !strings.Contains(errOut, "is now a symbolic link") {
				t.Errorf("the report does not name the swapped directory:\n%s", errOut)
			}
			for _, d := range treeDiff(otherBefore, treeOf(t, filepath.Join(root, "other"))) {
				t.Errorf("other/, which no patch named: %s", d)
			}
		})
	}
}

// dirsOn describes the directories on a path as they are, and stops at the
// first it cannot: one that is not there.
func TestDirsOn(t *testing.T) {
	root := mktree(t)
	for _, unconfined := range []bool{false, true} {
		tree, err := OpenTree(root, unconfined)
		must(t, err)
		t.Cleanup(func() { tree.Close() })
		tg, err := tree.Resolve("sub/in.txt")
		must(t, err)
		dirs := tree.dirsOn(tg)
		if len(dirs) == 0 || filepath.Base(dirs[len(dirs)-1].name) != "sub" {
			t.Fatalf("unconfined=%v: dirsOn(sub/in.txt) = %v, want it to end at sub", unconfined, dirs)
		}
		fi, err := os.Lstat(filepath.Join(root, "sub"))
		must(t, err)
		if !os.SameFile(dirs[len(dirs)-1].info, fi) {
			t.Errorf("unconfined=%v: sub is described as something else", unconfined)
		}
		if !unconfined && len(dirs) != 1 {
			t.Errorf("confined, dirsOn(sub/in.txt) = %v, want only sub, below the root", dirs)
		}

		top, err := tree.Resolve("top.txt")
		must(t, err)
		if d := tree.dirsOn(top); !unconfined && len(d) != 0 {
			t.Errorf("confined, a file at the top of the root has directories %v", d)
		}

		missing, err := tree.Resolve("nope/deeper/x.txt")
		must(t, err)
		d := tree.dirsOn(missing)
		for _, sd := range d {
			if strings.Contains(sd.name, "nope") {
				t.Errorf("unconfined=%v: dirsOn described %s, which is not there", unconfined, sd.name)
			}
		}
	}
}

// The refusal for a swapped directory is new wording, so it is goldened in
// both renderings.
func TestTheSwappedDirectoryRefusalIsGolden(t *testing.T) {
	needsShellSymlinks(t)
	files := map[string]string{"sub/d.txt": "orig\n", "other/keep.txt": "keep\n"}
	const swap = "mv sub sub.real && ln -s other sub; printf 'FAIL\\n'; false"
	_, _, errOut := runCLI(t, cliTree(t, files), []string{"--verify", swap}, "@@ delete sub/d.txt\n")
	golden(t, "cli-rollback-a-directory-was-replaced", errOut)
	code, js, _ := runCLI(t, cliTree(t, files), []string{"--json", "--verify", swap}, "@@ delete sub/d.txt\n")
	if code != exitRollbackFailed {
		t.Errorf("exit %d, want %d", code, exitRollbackFailed)
	}
	golden(t, "cli-rollback-a-directory-was-replaced-json", fixedSeconds(js))
}
