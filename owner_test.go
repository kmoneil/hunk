package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every rewrite keeps the group the file had, when the invoker is in it. Until
// 2026-10-06 each of these left a.txt in the group a new file gets, with 0640
// now applying to that group, including the three that report the tree as put
// back. A created file is the control: it has no group to keep and takes the
// one a new file gets.
func TestEveryRewriteKeepsTheGroup(t *testing.T) {
	needsPOSIXPerms(t)
	const modify = "@@ file a.txt\n@@ old\none\n@@ new\nONE\n"
	for _, c := range []struct {
		name  string
		args  []string
		patch string
		code  int
		want  string
	}{
		{"a modify", nil, modify, exitOK, "ONE\n"},
		{"an overwrite", nil, "@@ delete a.txt\n@@ create a.txt\nTWO\n@@ end\n", exitOK, "TWO"},
		{"--try, a modify", []string{"--try", "true"}, modify, exitOK, "one\n"},
		{"--try, a delete", []string{"--try", "true"}, "@@ delete a.txt\n", exitOK, "one\n"},
		{"a failed --verify, a modify", []string{"--verify", "false"}, modify, exitVerifyFailed, "one\n"},
		{"a failed --verify, a delete", []string{"--verify", "false"}, "@@ delete a.txt\n", exitVerifyFailed, "one\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			p := filepath.Join(root, "a.txt")
			group := otherGroup(t, newFileGroup(t, root))
			must(t, os.Chown(p, -1, group))
			must(t, os.Chmod(p, 0o640))
			code, stdout, stderr := runCLI(t, root, c.args, c.patch)
			if code != c.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, c.code, stdout, stderr)
			}
			if got := readFile(t, root, "a.txt"); got != c.want {
				t.Errorf("a.txt = %q, want %q", got, c.want)
			}
			if got := groupOf(t, p); got != group {
				t.Errorf("a.txt is in group %d, want %d, the group it had", got, group)
			}
			fi, err := os.Stat(p)
			must(t, err)
			assertMode(t, fi.Mode(), 0o640)
		})
	}

	t.Run("a create, the control", func(t *testing.T) {
		root := cliTree(t, nil)
		code, stdout, stderr := runCLI(t, root, nil, "@@ create b.txt\nnew\n@@ end\n")
		if code != exitOK {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if got, want := groupOf(t, filepath.Join(root, "b.txt")), newFileGroup(t, root); got != want {
			t.Errorf("b.txt is in group %d, want %d, the group a new file there gets", got, want)
		}
	})
}

// A group the invoker is not in cannot be kept, and then the group the file
// ends up in gets the bits "other" had, never more. Reachable without privilege
// only where a new file takes its directory's group and that group is foreign:
// on macOS, a directory made under /tmp is in wheel. Elsewhere this skips, and
// TestKeepOwnerWhenChownIsRefused reaches the same branch through fchown.
func TestAGroupThatCannotBeKeptGetsWhatOthersHad(t *testing.T) {
	needsPOSIXPerms(t)
	for _, c := range []struct {
		before, after os.FileMode
	}{
		{0o640, 0o600},
		{0o664, 0o644},
		{0o604, 0o644},
		{0o660, 0o600},
		{0o600, 0o600},
	} {
		t.Run(c.before.String(), func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			p := filepath.Join(root, "a.txt")
			foreign := groupOf(t, p)
			if inGroup(t, foreign) {
				t.Skipf("a new file here is in group %d, which the invoker is in, so no group it cannot keep can be made without privilege", foreign)
			}
			// New files in root now take the invoker's group, and a.txt keeps
			// the foreign one it was made with.
			must(t, os.Chown(root, -1, os.Getegid()))
			if newFileGroup(t, root) == foreign {
				t.Skip("a new file does not take its directory's group here")
			}
			must(t, os.Chmod(p, c.before))
			code, stdout, stderr := runCLI(t, root, nil, "@@ file a.txt\n@@ old\none\n@@ new\nONE\n")
			if code != exitOK {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if got := readFile(t, root, "a.txt"); got != "ONE\n" {
				t.Errorf("a.txt = %q", got)
			}
			if got := groupOf(t, p); got != os.Getegid() {
				t.Errorf("a.txt is in group %d, want %d, the invoker's", got, os.Getegid())
			}
			fi, err := os.Stat(p)
			must(t, err)
			assertMode(t, fi.Mode(), c.after)
		})
	}
}

// Every rewrite keeps setuid, setgid and sticky as it keeps the permission
// bits, decided 2026-10-07: §6.1's "chmod to the original mode". Until then
// load recorded Perm(), which in Go is the nine permission bits alone, so
// every row here lost all three, including the four that promise the file back
// with the same bytes and mode. The invoker owns a.txt and is in its group, so
// each bit kept is one it could set with chmod anyway.
func TestEveryRewriteKeepsTheSpecialBits(t *testing.T) {
	needsPOSIXPerms(t)
	const modify = "@@ file a.txt\n@@ old\none\n@@ new\nONE\n"
	const overwrite = "@@ delete a.txt\n@@ create a.txt\nTWO\n@@ end\n"
	shapes := []struct {
		name  string
		args  []string
		patch string
		code  int
		want  string
	}{
		{"a modify", nil, modify, exitOK, "ONE\n"},
		{"an overwrite", nil, overwrite, exitOK, "TWO"},
		{"--try, a modify", []string{"--try", "true"}, modify, exitOK, "one\n"},
		{"--try, a delete", []string{"--try", "true"}, "@@ delete a.txt\n", exitOK, "one\n"},
		{"a failed --verify, a modify", []string{"--verify", "false"}, modify, exitVerifyFailed, "one\n"},
		{"a failed --verify, a delete", []string{"--verify", "false"}, "@@ delete a.txt\n", exitVerifyFailed, "one\n"},
		{"a failed --verify, an overwrite", []string{"--verify", "false"}, overwrite, exitVerifyFailed, "one\n"},
	}
	for _, mode := range []fs.FileMode{
		0o755 | fs.ModeSetuid,
		0o755 | fs.ModeSetgid,
		0o755 | fs.ModeSetuid | fs.ModeSetgid,
		0o644 | fs.ModeSticky,
		0o644 | fs.ModeSetgid,
		0o755 | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky,
	} {
		for _, c := range shapes {
			t.Run(mode.String()+", "+c.name, func(t *testing.T) {
				root := cliTree(t, map[string]string{"a.txt": "one\n"})
				p := filepath.Join(root, "a.txt")
				withSpecialMode(t, p, mode)
				code, stdout, stderr := runCLI(t, root, c.args, c.patch)
				if code != c.code {
					t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, c.code, stdout, stderr)
				}
				if got := readFile(t, root, "a.txt"); got != c.want {
					t.Errorf("a.txt = %q, want %q", got, c.want)
				}
				fi, err := os.Stat(p)
				must(t, err)
				assertMode(t, fi.Mode(), mode)
			})
		}
	}
}

// keepOwner keeps setuid only while the file keeps its owner, and setgid only
// while it keeps its group, decided 2026-10-07. A rewrite of another user's
// file comes out the invoker's, and setuid on it would run as the invoker; a
// group that cannot be kept leaves the file in another, and setgid would run
// as that. Sticky means the same whoever owns the file, and is kept. So every
// bit hunk sets is one the invoker could set with chmod on the file as it ends
// up.
//
// The owner and group that cannot be kept are a fake like's, since giving a
// real file another owner needs privilege, and fchown refuses any but the
// invoker's own, as it does without privilege, so that root gets the same
// answers. The directory is in the invoker's group, so the rewrite lands
// where a setgid would be honoured if hunk asked for one.
func TestASpecialBitIsKeptOnlyWithWhatItNames(t *testing.T) {
	needsPOSIXPerms(t)
	me, mine := os.Geteuid(), os.Getegid()
	notMine := mine + 1
	for inGroup(t, notMine) {
		notMine++
	}
	const all = 0o750 | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky
	for _, c := range []struct {
		name      string
		uid, gid  int
		want      fs.FileMode
		wantCalls int
	}{
		{"the owner and the group kept", me, mine, all, 1},
		{"the owner not kept", me + 1, mine, 0o750 | fs.ModeSetgid | fs.ModeSticky, 2},
		{"the group not kept", me, notMine, 0o700 | fs.ModeSetuid | fs.ModeSticky, 2},
		{"neither kept", me + 1, notMine, 0o700 | fs.ModeSticky, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := mktree(t)
			p := filepath.Join(root, "top.txt")
			withSpecialMode(t, p, all)
			fi, err := os.Stat(p)
			must(t, err)
			like := ownedBy(t, fi, c.uid, c.gid)

			calls := 0
			orig := fchown
			t.Cleanup(func() { fchown = orig })
			fchown = func(f *os.File, uid, gid int) error {
				calls++
				if (uid != -1 && uid != me) || gid != mine {
					return fs.ErrPermission
				}
				return f.Chown(uid, gid)
			}

			tree, err := OpenTree(root, false)
			must(t, err)
			defer tree.Close()
			tg, err := tree.Resolve("top.txt")
			must(t, err)
			must(t, tree.WriteAtomic(tg, []byte("rewritten\n"), all, like))

			fi, err = os.Stat(p)
			must(t, err)
			assertMode(t, fi.Mode(), c.want)
			if calls != c.wantCalls {
				t.Errorf("fchown called %d times, want %d", calls, c.wantCalls)
			}
		})
	}
}

// Perm() is how the special bits were lost: in Go it is the nine permission
// bits alone, and load recorded it and the check compared it, so setuid, setgid
// and sticky were dropped by every rewrite and invisible to the check until
// 2026-10-07. A file's mode is recorded, compared and written as keptMode now,
// and nothing outside the tests calls Perm() at all, so a new use has to be
// argued for here rather than slip in.
func TestNoCodeKeepsOnlyThePermissionBits(t *testing.T) {
	names, err := filepath.Glob("*.go")
	must(t, err)
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		must(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Perm" {
					t.Errorf("%s calls Perm(), which drops setuid, setgid and sticky; use keptMode", fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
}
