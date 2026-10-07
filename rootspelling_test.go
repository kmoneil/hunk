package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// spellingTree makes the shape of macOS's /tmp anywhere: a directory real, the
// root, and alias -> real beside it, so a path written through alias names the
// root by another spelling. up -> the directory both are in reaches the root a
// third way, as up/real. real2 is a sibling whose name extends the root's, and
// real/loop -> . is a link inside the root back to it. It returns the
// directory holding them, resolved.
func spellingTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	for name, body := range map[string]string{
		"real/f.txt": "a\n", "real/sub/in.txt": "in\n", "real/.git/config": "[core]\n", "real2/f.txt": "b\n",
	} {
		full := filepath.Join(base, native(name))
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(t, os.WriteFile(full, []byte(body), 0o644))
	}
	for _, l := range [][2]string{
		{"alias", filepath.Join(base, "real")},
		{"up", base},
		{"real/loop", "."},
	} {
		must(t, os.Symlink(l[1], filepath.Join(base, native(l[0]))))
	}
	return base
}

// An absolute path is under the root when it names the root's directory by any
// spelling, not only when its text begins with the root's. Until 2026-10-07 it
// was text against the root resolved, so on macOS a path through /tmp, which
// is /private/tmp, or through any mktemp -d directory, which is under /var and
// so /private/var, was refused as outside (#62).
func TestAnAbsolutePathThroughAnotherSpellingIsUnderTheRoot(t *testing.T) {
	base := spellingTree(t)
	real, alias := filepath.Join(base, "real"), filepath.Join(base, "alias")
	for _, opened := range []string{real, alias} {
		tree, err := OpenTree(opened, false)
		must(t, err)
		t.Cleanup(func() { tree.Close() })
		name := "opened as " + filepath.Base(opened)
		for _, c := range []struct {
			name, in, want string
			via            bool
		}{
			{"the root's own text", filepath.Join(real, "f.txt"), "f.txt", false},
			{"through a link to the root", filepath.Join(alias, "f.txt"), "f.txt", false},
			{"deeper through it", filepath.Join(alias, "sub", "in.txt"), native("sub/in.txt"), false},
			{"a file to create through it", filepath.Join(alias, "new", "x.go"), native("new/x.go"), false},
			{"the root itself through it", alias, ".", false},
			{"through a link to the root's parent", filepath.Join(base, "up", "real", "f.txt"), "f.txt", false},
			{
				// The first prefix that is the root is the one taken, so loop is
				// walked as the link it is.
				"through a link to the root inside the root",
				filepath.Join(alias, "loop", "f.txt"), "f.txt", true,
			},
		} {
			t.Run(name+", "+c.name, func(t *testing.T) {
				tg, err := tree.Resolve(c.in)
				if err != nil {
					t.Fatalf("Resolve(%q): %v", c.in, err)
				}
				if tg.name != c.want {
					t.Errorf("name = %q, want %q", tg.name, c.want)
				}
				if tg.ViaSymlink() != c.via {
					t.Errorf("ViaSymlink = %v, want %v", tg.ViaSymlink(), c.via)
				}
				if tg.Orig() != c.in {
					t.Errorf("Orig = %q, want it as written", tg.Orig())
				}
			})
		}

		for _, c := range []struct{ name, in, reason string }{
			{"a sibling whose name extends the root's", filepath.Join(base, "real2", "f.txt"), "an absolute path is allowed only under the root"},
			{"the directory above the root", filepath.Join(base, "up", "f.txt"), "an absolute path is allowed only under the root"},
			{"a path under nothing that exists", filepath.Join(base, "missing", "real", "f.txt"), "an absolute path is allowed only under the root"},
			{".git through a link to the root", filepath.Join(alias, ".git", "config"), "it is .git or inside it"},
		} {
			t.Run(name+", refused: "+c.name, func(t *testing.T) {
				_, err := tree.Resolve(c.in)
				var pr *PathRefusal
				if !errors.As(err, &pr) {
					t.Fatalf("Resolve(%q) = %v, want a *PathRefusal", c.in, err)
				}
				if !strings.HasPrefix(pr.Reason, c.reason) {
					t.Errorf("Reason = %q, want %q", pr.Reason, c.reason)
				}
				if !strings.HasSuffix(err.Error(), "; the root is "+tree.Shown()) {
					t.Errorf("%q does not end naming the root as %q", err, tree.Shown())
				}
			})
		}
	}

	// In another case, where the filesystem folds it.
	if foldsCase(t) {
		tree, err := OpenTree(real, false)
		must(t, err)
		defer tree.Close()
		in := filepath.Join(base, "REAL", "f.txt")
		tg, err := tree.Resolve(in)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", in, err)
		}
		if tg.name != "f.txt" {
			t.Errorf("name = %q, want f.txt", tg.name)
		}
	}
}

// A link inside the root whose absolute destination is spelled through another
// spelling of the root leads inside it, standing last or in a parent. Until
// 2026-10-07 both were a symlink out of the root (row A3b of the escape card).
func TestALinkSpelledThroughAnotherSpellingOfTheRootStaysInside(t *testing.T) {
	base := spellingTree(t)
	real, alias := filepath.Join(base, "real"), filepath.Join(base, "alias")
	for _, l := range [][2]string{
		{"real/final.link", filepath.Join(alias, "sub", "in.txt")},
		{"real/dir.link", filepath.Join(alias, "sub")},
		{"real/out.link", filepath.Join(base, "up", "real2")},
	} {
		must(t, os.Symlink(l[1], filepath.Join(base, native(l[0]))))
	}
	tree, err := OpenTree(real, false)
	must(t, err)
	defer tree.Close()
	for _, c := range []struct{ in, want string }{
		{"final.link", native("sub/in.txt")},
		{native("dir.link/in.txt"), native("sub/in.txt")},
	} {
		t.Run(c.in, func(t *testing.T) {
			tg, err := tree.Resolve(c.in)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.in, err)
			}
			if tg.name != c.want {
				t.Errorf("name = %q, want %q", tg.name, c.want)
			}
			kernel, err := os.Stat(filepath.Join(real, c.in))
			must(t, err)
			got, err := os.Stat(filepath.Join(real, tg.name))
			must(t, err)
			if !os.SameFile(kernel, got) {
				t.Errorf("%s is not the file the kernel reaches through %s", tg.name, c.in)
			}
		})
	}
	t.Run("one spelled through the root's parent to a sibling", func(t *testing.T) {
		_, err := tree.Resolve(native("out.link/f.txt"))
		var pr *PathRefusal
		if !errors.As(err, &pr) || !strings.HasPrefix(pr.Reason, "the symlink out.link -> ") {
			t.Errorf("Resolve = %v, want the refusal of a link that leads out", err)
		}
	})
}

// A refusal names the root as the caller gave it, and the resolved form after
// it when the two differ: a caller in /tmp/x read "the root is /private/tmp/x"
// until 2026-10-07, which looks like being in the wrong directory.
func TestShownNamesTheRootAsGiven(t *testing.T) {
	base := spellingTree(t)
	real, alias := filepath.Join(base, "real"), filepath.Join(base, "alias")
	for _, c := range []struct{ opened, want string }{
		{real, real},
		{alias, alias + ", which is " + real},
	} {
		tree, err := OpenTree(c.opened, false)
		must(t, err)
		if got := tree.Shown(); got != c.want {
			t.Errorf("OpenTree(%q).Shown() = %q, want %q", c.opened, got, c.want)
		}
		if tree.Root() != real {
			t.Errorf("Root() = %q, want the resolved %q", tree.Root(), real)
		}
		tree.Close()
	}
}

// The new clause is a contract (§8.1). The paths are substituted: the root as
// given for /the/given, and resolved for /the/root.
func TestARootGivenThroughALinkIsGolden(t *testing.T) {
	base := spellingTree(t)
	real, alias := filepath.Join(base, "real"), filepath.Join(base, "alias")
	patch := "@@ file ../x.txt\n@@ old\na\n@@ new\nb\n"
	shown := func(s string) string {
		return slashPaths(strings.ReplaceAll(strings.ReplaceAll(s, alias, "/the/given"), real, "/the/root"))
	}
	code, out, errOut := runCLI(t, alias, nil, patch)
	if code != exitNoMatch || out != "" {
		t.Fatalf("exit %d, stdout %q; want %d and nothing", code, out, exitNoMatch)
	}
	golden(t, "cli-path-refused-root-given-through-a-link", shown(errOut))

	code, out, _ = runCLI(t, alias, []string{"--json"}, patch)
	if code != exitNoMatch {
		t.Fatalf("exit %d, want %d", code, exitNoMatch)
	}
	for _, p := range []string{alias, real} {
		escaped, err := json.Marshal(p)
		must(t, err)
		out = strings.ReplaceAll(out, strings.Trim(string(escaped), `"`), p)
	}
	golden(t, "cli-path-refused-root-given-through-a-link-json", shown(strings.ReplaceAll(out, `\\`, `\`)))
}

// Through the whole tool, as the issue ran it: hunk started in the link to the
// root, with the shell's PWD saying so, edits a file named absolutely through
// the link, and does so with PWD unset too, which Go's working directory reads
// as the resolved root. Run as its own process, since the working directory is
// the subject.
func TestAnAbsolutePathThroughTheWorkingDirectorysSpelling(t *testing.T) {
	exe, err := os.Executable()
	must(t, err)
	for _, pwd := range []bool{true, false} {
		t.Run(map[bool]string{true: "PWD set", false: "PWD unset"}[pwd], func(t *testing.T) {
			base := spellingTree(t)
			alias := filepath.Join(base, "alias")
			cmd := exec.Command(exe)
			cmd.Dir = alias
			env := []string{"HUNK_TEST_AS_HUNK=1"}
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "PWD=") {
					env = append(env, kv)
				}
			}
			if pwd {
				env = append(env, "PWD="+alias)
			}
			cmd.Env = env
			cmd.Stdin = strings.NewReader("@@ file " + filepath.Join(alias, "f.txt") + "\n@@ old\na\n@@ new\nb\n")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if got := readFile(t, base, native("real/f.txt")); got != "b\n" {
				t.Errorf("real/f.txt = %q, want the edit", got)
			}
		})
	}
}
