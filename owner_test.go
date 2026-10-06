package main

import (
	"os"
	"path/filepath"
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
