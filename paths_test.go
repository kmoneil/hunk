package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mktree builds a fixture tree and returns its resolved root. Everything here
// runs against a real filesystem: symlinks, modes and renames are the whole
// subject, and a mock of os.Rename proves nothing about os.Rename.
func mktree(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if r, err := filepath.EvalSymlinks(d); err == nil {
		d = r
	}
	must(t, os.MkdirAll(filepath.Join(d, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(d, "top.txt"), []byte("top\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(d, "sub", "in.txt"), []byte("in\n"), 0o644))
	return d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveConfined(t *testing.T) {
	root := mktree(t)
	outside := t.TempDir()
	if r, err := filepath.EvalSymlinks(outside); err == nil {
		outside = r
	}
	must(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("no\n"), 0o644))

	// Relative symlink inside the root. os.Root follows these on its own.
	must(t, os.Symlink("sub/in.txt", filepath.Join(root, "rel.link")))
	// Absolute symlink inside the root. os.Root refuses these outright even
	// though the target is inside, which is the gap this file covers.
	must(t, os.Symlink(filepath.Join(root, "sub", "in.txt"), filepath.Join(root, "abs.link")))
	// Two hops.
	must(t, os.Symlink("rel.link", filepath.Join(root, "chain.link")))
	// Escapes, one of each spelling.
	must(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape-abs.link")))
	must(t, os.Symlink("../"+filepath.Base(outside)+"/secret.txt", filepath.Join(root, "escape-rel.link")))
	// A loop.
	must(t, os.Symlink("loop-b.link", filepath.Join(root, "loop-a.link")))
	must(t, os.Symlink("loop-a.link", filepath.Join(root, "loop-b.link")))

	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()

	ok := []struct {
		name     string
		in       string
		wantName string
		wantLink bool
	}{
		{"a plain relative path", "top.txt", "top.txt", false},
		{"a nested relative path", "sub/in.txt", "sub/in.txt", false},
		{"an interior .. that stays inside", "sub/../top.txt", "top.txt", false},
		{"a path that does not exist yet", "new/deep/file.go", "new/deep/file.go", false},
		{"an absolute path under the root", filepath.Join(root, "top.txt"), "top.txt", false},
		{"a relative symlink is written through", "rel.link", "sub/in.txt", true},
		{"an absolute in-root symlink is written through", "abs.link", "sub/in.txt", true},
		{"a chain of symlinks", "chain.link", "sub/in.txt", true},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			tg, err := tree.Resolve(c.in)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.in, err)
			}
			if tg.name != c.wantName {
				t.Errorf("name = %q, want %q", tg.name, c.wantName)
			}
			if tg.ViaSymlink() != c.wantLink {
				t.Errorf("ViaSymlink = %v, want %v", tg.ViaSymlink(), c.wantLink)
			}
			if tg.Orig() != c.in {
				t.Errorf("Orig = %q, want %q", tg.Orig(), c.in)
			}
		})
	}

	bad := []struct {
		name string
		in   string
		msg  string
	}{
		{"an empty path", "", "empty"},
		{"a path climbing out", "../secret.txt", "climbs out"},
		{"a path climbing out from a subdirectory", "sub/../../secret.txt", "climbs out"},
		{"an absolute path outside the root", filepath.Join(outside, "secret.txt"), "only under the root"},
		{"an absolute symlink out of the root", "escape-abs.link", "symlink out of the root"},
		{"a relative symlink out of the root", "escape-rel.link", "symlink out of the root"},
		{"a symlink loop", "loop-a.link", "too many symlinks"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := tree.Resolve(c.in)
			if err == nil {
				t.Fatalf("Resolve(%q) was allowed", c.in)
			}
			var pr *PathRefusal
			if !errors.As(err, &pr) {
				t.Fatalf("want *PathRefusal, got %T: %v", err, err)
			}
			if !strings.Contains(pr.Reason, c.msg) {
				t.Errorf("reason %q does not contain %q", pr.Reason, c.msg)
			}
			// §6.5's message contract: the refusal names the root, so an agent
			// can tell what it was measured against.
			if !strings.Contains(err.Error(), tree.Root()) {
				t.Errorf("message does not name the root: %s", err)
			}
		})
	}
}

// os.Root refuses an absolute symlink even when its target is inside the root.
// That is stricter than §6.5, which draws no such distinction, so this file
// resolves the final component itself. Pinned here because the day the stdlib
// relaxes it, this test says so.
func TestAbsoluteInRootSymlinkIsTheStdlibGap(t *testing.T) {
	root := mktree(t)
	must(t, os.Symlink(filepath.Join(root, "sub", "in.txt"), filepath.Join(root, "abs.link")))

	r, err := os.OpenRoot(root)
	must(t, err)
	defer r.Close()
	if _, err := r.Open("abs.link"); err == nil {
		t.Error("os.Root now allows an absolute in-root symlink; followFinalLink may be able to go")
	}

	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()
	tg, err := tree.Resolve("abs.link")
	if err != nil {
		t.Fatalf("hunk must still allow it (§6.5): %v", err)
	}
	if tg.name != "sub/in.txt" {
		t.Errorf("resolved to %q, want sub/in.txt", tg.name)
	}
}

func TestResolveUnconfined(t *testing.T) {
	root := mktree(t)
	outside := t.TempDir()
	if r, err := filepath.EvalSymlinks(outside); err == nil {
		outside = r
	}
	must(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("yes\n"), 0o644))
	must(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "out.link")))

	tree, err := OpenTree(root, true)
	must(t, err)
	defer tree.Close()

	for _, c := range []struct{ name, in, want string }{
		{"a relative path becomes absolute", "top.txt", filepath.Join(root, "top.txt")},
		{"climbing out is allowed", "../x.txt", filepath.Join(filepath.Dir(root), "x.txt")},
		{"an absolute path outside is allowed", filepath.Join(outside, "secret.txt"), filepath.Join(outside, "secret.txt")},
		{"a symlink out of the root is followed", "out.link", filepath.Join(outside, "secret.txt")},
	} {
		t.Run(c.name, func(t *testing.T) {
			tg, err := tree.Resolve(c.in)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.in, err)
			}
			if tg.name != c.want {
				t.Errorf("name = %q, want %q", tg.name, c.want)
			}
		})
	}
}

// A root that is itself a symlink must not make every path under it look like
// an escape.
func TestSymlinkedRoot(t *testing.T) {
	real := mktree(t)
	link := filepath.Join(t.TempDir(), "as-link")
	must(t, os.Symlink(real, link))

	tree, err := OpenTree(link, false)
	must(t, err)
	defer tree.Close()
	if tree.Root() != real {
		t.Errorf("root = %q, want the resolved %q", tree.Root(), real)
	}
	if _, err := tree.Resolve("sub/in.txt"); err != nil {
		t.Errorf("a path under a symlinked root was refused: %v", err)
	}
}

func TestWriteAtomic(t *testing.T) {
	t.Run("replaces contents and keeps the mode", func(t *testing.T) {
		root := mktree(t)
		must(t, os.Chmod(filepath.Join(root, "top.txt"), 0o640))
		tree, err := OpenTree(root, false)
		must(t, err)
		defer tree.Close()

		tg, err := tree.Resolve("top.txt")
		must(t, err)
		must(t, tree.WriteAtomic(tg, []byte("rewritten\n"), 0o640))

		got, err := os.ReadFile(filepath.Join(root, "top.txt"))
		must(t, err)
		if string(got) != "rewritten\n" {
			t.Errorf("contents %q", got)
		}
		fi, err := os.Stat(filepath.Join(root, "top.txt"))
		must(t, err)
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("mode %v, want 0640", fi.Mode().Perm())
		}
	})

	// The case §6.5 is about, and the one a plain temp-and-rename gets wrong:
	// os.Rename and os.Root.Rename both replace the link with a regular file.
	t.Run("writes through a symlink and the link survives", func(t *testing.T) {
		root := mktree(t)
		must(t, os.Symlink("sub/in.txt", filepath.Join(root, "rel.link")))
		tree, err := OpenTree(root, false)
		must(t, err)
		defer tree.Close()

		tg, err := tree.Resolve("rel.link")
		must(t, err)
		must(t, tree.WriteAtomic(tg, []byte("through\n"), 0o644))

		fi, err := os.Lstat(filepath.Join(root, "rel.link"))
		must(t, err)
		if fi.Mode()&fs.ModeSymlink == 0 {
			t.Error("the symlink was replaced with a regular file; §6.5 forbids that")
		}
		got, err := os.ReadFile(filepath.Join(root, "sub", "in.txt"))
		must(t, err)
		if string(got) != "through\n" {
			t.Errorf("the link's target says %q, want the new bytes", got)
		}
	})

	t.Run("leaves no temp files behind", func(t *testing.T) {
		root := mktree(t)
		tree, err := OpenTree(root, false)
		must(t, err)
		defer tree.Close()
		tg, err := tree.Resolve("top.txt")
		must(t, err)
		must(t, tree.WriteAtomic(tg, []byte("x\n"), 0o644))

		ents, err := os.ReadDir(root)
		must(t, err)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".hunk-") {
				t.Errorf("temp file left behind: %s", e.Name())
			}
		}
	})

	t.Run("an unresolved target is refused", func(t *testing.T) {
		root := mktree(t)
		tree, err := OpenTree(root, false)
		must(t, err)
		defer tree.Close()
		if err := tree.WriteAtomic(Target{}, []byte("x"), 0o644); err == nil {
			t.Error("a zero Target was written")
		}
	})

	t.Run("a temp file in a missing directory fails without leaving one", func(t *testing.T) {
		root := mktree(t)
		tree, err := OpenTree(root, false)
		must(t, err)
		defer tree.Close()
		tg, err := tree.Resolve("nodir/f.txt")
		must(t, err)
		if err := tree.WriteAtomic(tg, []byte("x"), 0o644); err == nil {
			t.Error("wrote into a directory that does not exist")
		}
	})
}

func TestMkdirAllAndRemove(t *testing.T) {
	root := mktree(t)
	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()

	tg, err := tree.Resolve("a/b/c/f.txt")
	must(t, err)
	made, err := tree.MkdirAll(tg)
	must(t, err)
	// Deepest first, which is removal order (§6.6).
	want := []string{"a/b/c", "a/b", "a"}
	if len(made) != len(want) {
		t.Fatalf("made %v, want %v", made, want)
	}
	for i := range want {
		if made[i] != want[i] {
			t.Errorf("made[%d] = %q, want %q", i, made[i], want[i])
		}
	}

	// A directory that already existed is not reported as created, so rollback
	// cannot remove something this tool did not make.
	tg2, err := tree.Resolve("sub/deeper/f.txt")
	must(t, err)
	made2, err := tree.MkdirAll(tg2)
	must(t, err)
	if len(made2) != 1 || made2[0] != "sub/deeper" {
		t.Errorf("made %v, want just sub/deeper", made2)
	}

	must(t, tree.WriteAtomic(tg, []byte("x\n"), 0o644))
	must(t, tree.Remove(tg))
	if _, err := os.Stat(filepath.Join(root, "a/b/c/f.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("Remove did not remove")
	}
	for _, d := range made {
		must(t, tree.RemoveDir(d))
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("RemoveDir did not unwind")
	}
}

func TestStatAndReadFile(t *testing.T) {
	root := mktree(t)
	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()
	tg, err := tree.Resolve("sub/in.txt")
	must(t, err)
	fi, err := tree.Stat(tg)
	must(t, err)
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	b, err := tree.ReadFile(tg)
	must(t, err)
	if string(b) != "in\n" {
		t.Errorf("read %q", b)
	}
}

func TestOpenTreeRejectsAMissingRoot(t *testing.T) {
	if _, err := OpenTree(filepath.Join(t.TempDir(), "nope"), false); err == nil {
		t.Error("opened a root that does not exist")
	}
}

func TestPathRefusalMessage(t *testing.T) {
	e := &PathRefusal{Path: "a.go", Root: "/r", Resolved: "/elsewhere/a.go", Reason: "it left"}
	got := e.Error()
	for _, want := range []string{"a.go", "it left", "/elsewhere/a.go", "/r"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing %q", got, want)
		}
	}
	// No "resolves to" clause when there is nothing to add.
	plain := (&PathRefusal{Path: "a.go", Root: "/r", Resolved: "a.go", Reason: "no"}).Error()
	if strings.Contains(plain, "resolves to") {
		t.Errorf("redundant clause: %q", plain)
	}
}

// Paths come out of a payload the tool did not write, so this is the input
// most worth fuzzing. The property is the security one: a confined tree never
// hands back a target outside its root.
func FuzzResolveStaysInsideTheRoot(f *testing.F) {
	for _, s := range []string{
		"a.go", "sub/in.txt", "../escape", "/etc/passwd", "", "a/../../b",
		"./x", "sub/./../top.txt", strings.Repeat("../", 40) + "etc/passwd",
	} {
		f.Add(s)
	}
	root := f.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "in.txt"), []byte("in\n"), 0o644)
	os.Symlink("sub/in.txt", filepath.Join(root, "rel.link"))
	os.Symlink("/etc/passwd", filepath.Join(root, "bad.link"))

	tree, err := OpenTree(root, false)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { tree.Close() })

	f.Fuzz(func(t *testing.T, p string) {
		tg, err := tree.Resolve(p)
		if err != nil {
			var pr *PathRefusal
			if !errors.As(err, &pr) {
				t.Fatalf("non-PathRefusal %T: %v", err, err)
			}
			return
		}
		if tg.name == "" {
			t.Fatalf("Resolve(%q) allowed an empty target", p)
		}
		abs := filepath.Join(tree.Root(), tg.name)
		if !filepath.IsAbs(tg.name) && abs != tree.Root() &&
			!strings.HasPrefix(abs, tree.Root()+string(filepath.Separator)) {
			t.Fatalf("Resolve(%q) = %q, which is %q, outside the root %q", p, tg.name, abs, tree.Root())
		}
		if filepath.IsAbs(tg.name) {
			t.Fatalf("Resolve(%q) returned an absolute name %q from a confined tree", p, tg.name)
		}
	})
}

// An absolute symlink in an *intermediate* component is refused, by os.Root
// rather than by followFinalLink, which resolves the final component only.
// That is a deliberate scope line (§6.5 says "a target that is itself a
// symlink"), so the refusal has to carry a message an agent can act on rather
// than os.Root's "path escapes from parent", which names neither the root nor
// what happened.
func TestAbsoluteSymlinkInAnIntermediateComponent(t *testing.T) {
	root := mktree(t)
	must(t, os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "absdir")))

	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()

	_, err = tree.Resolve("absdir/in.txt")
	if err == nil {
		t.Fatal("allowed; if os.Root has relaxed, followFinalLink can cover this case too")
	}
	var pr *PathRefusal
	if !errors.As(err, &pr) {
		t.Fatalf("want *PathRefusal, got %T: %v", err, err)
	}
	if !strings.Contains(pr.Reason, "--allow-outside-root") {
		t.Errorf("the refusal does not point at the way out: %s", pr.Reason)
	}
	if !strings.Contains(err.Error(), tree.Root()) {
		t.Errorf("the refusal does not name the root: %s", err)
	}

	// And --allow-outside-root does lift it, as the message promises.
	loose, err := OpenTree(root, true)
	must(t, err)
	defer loose.Close()
	if _, err := loose.Resolve("absdir/in.txt"); err != nil {
		t.Errorf("--allow-outside-root did not lift it: %v", err)
	}
}

// A failed rename must not leave the temp file behind. Renaming onto an
// existing directory is the cheapest way to make the last step fail after the
// temp exists.
func TestWriteAtomicCleansUpAfterAFailedRename(t *testing.T) {
	root := mktree(t)
	must(t, os.MkdirAll(filepath.Join(root, "adir"), 0o755))
	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()

	tg, err := tree.Resolve("adir")
	must(t, err)
	if err := tree.WriteAtomic(tg, []byte("x\n"), 0o644); err == nil {
		t.Fatal("renamed a file over a directory")
	}
	ents, err := os.ReadDir(root)
	must(t, err)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".hunk-") {
			t.Errorf("temp file left behind after a failed rename: %s", e.Name())
		}
	}
}

// Every method has an unconfined branch, and --allow-outside-root is the only
// thing that runs it. An untested branch here is a path that escapes the root
// by design and has never been executed.
func TestUnconfinedWriteCycle(t *testing.T) {
	root := mktree(t)
	tree, err := OpenTree(root, true)
	must(t, err)
	defer tree.Close()

	tg, err := tree.Resolve("x/y/f.txt")
	must(t, err)
	made, err := tree.MkdirAll(tg)
	must(t, err)
	if len(made) != 2 {
		t.Fatalf("made %v, want two directories", made)
	}
	must(t, tree.WriteAtomic(tg, []byte("unconfined\n"), 0o600))

	fi, err := tree.Stat(tg)
	must(t, err)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	b, err := tree.ReadFile(tg)
	must(t, err)
	if string(b) != "unconfined\n" {
		t.Errorf("read %q", b)
	}

	// And it writes through a symlink out of the root, which is the whole
	// point of the flag.
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "o.txt"), []byte("old\n"), 0o644))
	must(t, os.Symlink(filepath.Join(outside, "o.txt"), filepath.Join(root, "out.link")))
	otg, err := tree.Resolve("out.link")
	must(t, err)
	must(t, tree.WriteAtomic(otg, []byte("new\n"), 0o644))
	got, err := os.ReadFile(filepath.Join(outside, "o.txt"))
	must(t, err)
	if string(got) != "new\n" {
		t.Errorf("target says %q", got)
	}
	li, err := os.Lstat(filepath.Join(root, "out.link"))
	must(t, err)
	if li.Mode()&fs.ModeSymlink == 0 {
		t.Error("the symlink was replaced even unconfined")
	}

	must(t, tree.Remove(tg))
	for _, d := range made {
		must(t, tree.RemoveDir(d))
	}
	if _, err := os.Stat(filepath.Join(root, "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("RemoveDir did not unwind unconfined")
	}
}

// MkdirAll must fail rather than pretend when a component of the path is a
// regular file.
func TestMkdirAllOverAFile(t *testing.T) {
	for _, allowOutside := range []bool{false, true} {
		root := mktree(t)
		tree, err := OpenTree(root, allowOutside)
		must(t, err)
		tg, err := tree.Resolve("sub/in.txt/deeper/f.txt")
		must(t, err)
		if _, err := tree.MkdirAll(tg); err == nil {
			t.Errorf("allowOutside=%v: made a directory under a regular file", allowOutside)
		}
		tree.Close()
	}
}

// The unconfined mirror of TestWriteAtomicCleansUpAfterAFailedRename, and of
// the relative-symlink branch of relinkTarget. Both are code that only
// --allow-outside-root reaches, and the RemoveDir bug this file found was
// exactly that shape.
func TestUnconfinedFailurePaths(t *testing.T) {
	root := mktree(t)
	must(t, os.MkdirAll(filepath.Join(root, "adir"), 0o755))
	must(t, os.Symlink("sub/in.txt", filepath.Join(root, "rel.link")))
	tree, err := OpenTree(root, true)
	must(t, err)
	defer tree.Close()

	tg, err := tree.Resolve("rel.link")
	must(t, err)
	if want := filepath.Join(root, "sub", "in.txt"); tg.name != want {
		t.Errorf("a relative symlink resolved to %q, want %q", tg.name, want)
	}

	dtg, err := tree.Resolve("adir")
	must(t, err)
	if err := tree.WriteAtomic(dtg, []byte("x\n"), 0o644); err == nil {
		t.Fatal("renamed a file over a directory")
	}
	ents, err := os.ReadDir(root)
	must(t, err)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".hunk-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// A directory the process cannot read is not an escape, and must not be
// reported as one. The distinction matters because the two refusals send an
// agent to different places: one is a patch to fix, the other is a chmod.
func TestUnreadableDirectoryIsNotAnEscape(t *testing.T) {
	root := mktree(t)
	locked := filepath.Join(root, "locked")
	must(t, os.Mkdir(locked, 0o755))
	must(t, os.WriteFile(filepath.Join(locked, "f.txt"), []byte("x\n"), 0o644))
	must(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()

	// Resolve does not refuse it: whether the file can be opened is §6.1's
	// call at load, where the failure is an I/O error (exit 5) rather than a
	// path refusal (exit 2).
	tg, err := tree.Resolve("locked/f.txt")
	if err != nil {
		var pr *PathRefusal
		if errors.As(err, &pr) {
			t.Fatalf("a permission problem was reported as a path refusal: %s", pr.Reason)
		}
		t.Fatalf("Resolve: %v", err)
	}
	if tg.name != "locked/f.txt" {
		t.Errorf("name = %q", tg.name)
	}
	if _, err := tree.ReadFile(tg); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("ReadFile error = %v, want a permission error", err)
	}
}

// The cycle test above asserts the message and not the bound: raise
// maxLinkHops to a million and it still passes, just slowly. So the bound gets
// its own test, from both sides, because an unbounded chain is a hang and a
// hang in an agent's tool call is worse than any error.
func TestSymlinkChainDepthBound(t *testing.T) {
	chain := func(t *testing.T, n int) (*Tree, string) {
		t.Helper()
		root := mktree(t)
		// end -> link0 -> link1 -> ... -> link(n-1) -> sub/in.txt
		prev := "sub/in.txt"
		for i := 0; i < n; i++ {
			name := "l" + strconv.Itoa(i) + ".link"
			must(t, os.Symlink(prev, filepath.Join(root, name)))
			prev = name
		}
		tree, err := OpenTree(root, false)
		must(t, err)
		t.Cleanup(func() { tree.Close() })
		return tree, prev
	}

	t.Run("a chain shorter than the bound resolves", func(t *testing.T) {
		tree, head := chain(t, maxLinkHops-12)
		tg, err := tree.Resolve(head)
		if err != nil {
			t.Fatalf("a chain of %d links was refused: %v", maxLinkHops-12, err)
		}
		if tg.name != "sub/in.txt" {
			t.Errorf("resolved to %q, want sub/in.txt", tg.name)
		}
	})

	t.Run("a chain longer than the bound is refused", func(t *testing.T) {
		tree, head := chain(t, maxLinkHops+8)
		_, err := tree.Resolve(head)
		if err == nil {
			t.Fatalf("a chain of %d links resolved; the bound is not effective", maxLinkHops+8)
		}
		var pr *PathRefusal
		if !errors.As(err, &pr) || !strings.Contains(pr.Reason, "too many symlinks") {
			t.Errorf("refusal = %v", err)
		}
	})
}
