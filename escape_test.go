package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The cost the refusal of a path that leaves the root has to state, since the
// flag it offers is not this path's exception.
const flagCost = "--allow-outside-root lifts that for every path in the batch, " +
	"so give a path that has to reach outside a batch of its own"

// absoluteParentTree is mktree with absolute links in parent position that
// stay inside the root, in each shape the recon of 2026-10-07 tried, and
// returns the root.
func absoluteParentTree(t *testing.T) string {
	t.Helper()
	root := mktree(t)
	must(t, os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "sub", "deep", "d.txt"), []byte("d\n"), 0o644))
	sep := string(filepath.Separator)
	// The root without its volume. On POSIX that is the root, and on Windows a
	// destination that starts at a separator and names no volume, which is on
	// the link's volume and was read as relative until 2026-10-07.
	rooted := root[len(filepath.VolumeName(root)):]
	// In order, so each link's target exists before a link to it is made.
	for _, l := range [][2]string{
		{"absdir", filepath.Join(root, "sub")},
		{"absroot", root},
		// Spliced by hand, since filepath.Join would clean the ".." away.
		{"absdot", root + sep + "sub" + sep + "deep" + sep + ".."},
		{"d", "absdir"},
		{"a/b", filepath.Join(root, "sub")},
		{"sub/abs.link", filepath.Join(root, "sub", "in.txt")},
		{"rooted", rooted + sep + "sub"},
		{"sub/rooted.link", rooted + sep + "sub" + sep + "in.txt"},
	} {
		must(t, os.Symlink(l[1], filepath.Join(root, native(l[0]))))
	}
	return root
}

// An absolute link in a parent that stays inside the root is translated, as a
// final one has been since v0.1.0. Until 2026-10-07 os.Root's refusal of it
// was returned as it stood, with advice to set --allow-outside-root, and taking
// the advice opened every path in the batch: a ../outside edit beside it
// applied at exit 0. The reason given for refusing it, path-resolution's
// TOCTOU window, went when resolveLinks began handing os.Root a name with no
// links in it.
func TestAnAbsoluteLinkInAParentIsTranslated(t *testing.T) {
	root := absoluteParentTree(t)
	for _, mode := range []struct {
		name     string
		unconfin bool
		want     func(string) string
	}{
		{"confined", false, native},
		{"unconfined", true, func(p string) string { return filepath.Join(root, native(p)) }},
	} {
		tree, err := OpenTree(root, mode.unconfin)
		must(t, err)
		t.Cleanup(func() { tree.Close() })

		for _, c := range []struct{ name, in, want string }{
			{"a link to a directory", "absdir/in.txt", "sub/in.txt"},
			{"a link to the root itself", "absroot/top.txt", "top.txt"},
			{"a link to the root, then down", "absroot/sub/in.txt", "sub/in.txt"},
			{"a .. in the destination, walked", "absdot/in.txt", "sub/in.txt"},
			{"a relative link to an absolute one", "d/in.txt", "sub/in.txt"},
			{"two levels down", "a/b/in.txt", "sub/in.txt"},
			{"a final absolute link under it", "absdir/abs.link", "sub/in.txt"},
			{"deeper under it", "absdir/deep/d.txt", "sub/deep/d.txt"},
			{"a file to create under it", "absdir/new/x.go", "sub/new/x.go"},
			{"a link that names no volume", "rooted/in.txt", "sub/in.txt"},
			{"a final link that names no volume", "absdir/rooted.link", "sub/in.txt"},
		} {
			t.Run(mode.name+", "+c.name, func(t *testing.T) {
				if mode.unconfin && strings.Contains(c.in, "rooted") && !rootedLinksResolveUnconfined() {
					t.Skip("unconfined, a destination with no volume is still read as relative; see rootedLinksResolveUnconfined")
				}
				tg, err := tree.Resolve(c.in)
				if err != nil {
					t.Fatalf("Resolve(%q): %v", c.in, err)
				}
				if want := mode.want(c.want); tg.name != want {
					t.Errorf("name = %q, want %q", tg.name, want)
				}
				if !tg.ViaSymlink() {
					t.Error("ViaSymlink = false, and a link led here")
				}
				if tg.Orig() != c.in {
					t.Errorf("Orig = %q, want it as written, %q", tg.Orig(), c.in)
				}
				// And it is the file the kernel reaches through the links, or
				// as absent.
				at := tg.name
				if !mode.unconfin {
					at = filepath.Join(root, at)
				}
				kernel, kErr := os.Stat(filepath.Join(root, native(c.in)))
				got, gErr := os.Stat(at)
				switch {
				case kErr == nil && (gErr != nil || !os.SameFile(kernel, got)):
					t.Errorf("%s is not the file the kernel reaches through %s", tg.name, c.in)
				case kErr != nil && gErr == nil:
					t.Errorf("the kernel reaches nothing through %s, and %s exists", c.in, tg.name)
				}
			})
		}
	}
}

// The same links through a whole batch, confined: each op edits, makes or
// removes the file the link leads to and nothing else, and the batch is still
// confined, so a ../outside edit beside one is refused with nothing written.
func TestABatchThroughAnAbsoluteLinkInAParent(t *testing.T) {
	for _, c := range []struct {
		name, patch string
		changed     []string // nil: refused, exit 2, nothing written
	}{
		{
			name:    "a modify",
			patch:   "@@ file absdir/in.txt\n@@ old\nin\n@@ new\nIN\n",
			changed: []string{"sub/in.txt changed"},
		},
		{
			name:    "a create",
			patch:   "@@ create absdir/new.txt\nnew\n\n",
			changed: []string{"sub/new.txt appeared"},
		},
		{
			name:    "a create in a directory it makes",
			patch:   "@@ create absdir/made/new.txt\nnew\n\n",
			changed: []string{"sub/made appeared", "sub/made/new.txt appeared"},
		},
		{name: "a delete", patch: "@@ delete absdir/in.txt\n", changed: []string{"sub/in.txt is gone"}},
		{name: "an append", patch: "@@ append d/in.txt\nmore\n\n", changed: []string{"sub/in.txt changed"}},
		{
			name: "one beside a path that climbs out of the root",
			patch: "@@ file absdir/in.txt\n@@ old\nin\n@@ new\nIN\n" +
				"@@ file ../outside.txt\n@@ old\noutside\n@@ new\nOUTSIDE\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := absoluteParentTree(t)
			outside := filepath.Join(filepath.Dir(root), "outside.txt")
			must(t, os.WriteFile(outside, []byte("outside\n"), 0o644))
			tree, err := OpenTree(root, false)
			must(t, err)
			defer tree.Close()
			before := snapshot(t, root)

			_, err = run(t, tree, c.patch, Options{})
			if c.changed == nil {
				if code := ExitCode(err); code != exitNoMatch {
					t.Errorf("exit %d (%v), want %d", code, err, exitNoMatch)
				}
				assertUnchanged(t, root, before)
				if got := readFile(t, filepath.Dir(root), "outside.txt"); got != "outside\n" {
					t.Errorf("outside.txt = %q, written through a confined batch", got)
				}
				return
			}
			must(t, err)
			got := snapshotDiff(before, snapshot(t, root))
			if strings.Join(got, "; ") != filepath.FromSlash(strings.Join(c.changed, "; ")) {
				t.Errorf("the tree changed in %q, want %q", got, c.changed)
			}
		})
	}
}

// leavingTree is mktree with a sibling directory outside, holding outside.txt,
// a decoy root/outside/outside.txt where a walk that dropped a ".." at the top
// of the root would land, and links that lead out of the root in each shape.
// It returns the root.
func leavingTree(t *testing.T) string {
	t.Helper()
	root := mktree(t)
	out := filepath.Join(filepath.Dir(root), "outside")
	must(t, os.MkdirAll(out, 0o755))
	must(t, os.WriteFile(filepath.Join(out, "outside.txt"), []byte("outside\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(root, "outside"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "outside", "outside.txt"), []byte("decoy\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))
	sep := string(filepath.Separator)
	for _, l := range [][2]string{
		{"absout", out},
		{"relout", native("../outside")},
		{"chain", "relout"},
		{"a2", "sub"},
		{"sub/b", native("../../outside")},
		{"up", ".."},
		// Under the root as written, and out of it once the ".." is walked.
		{"absup", root + sep + ".."},
		{"x", native("sub/deep")},
		{"dd", native("x/../../..")},
	} {
		must(t, os.Symlink(l[1], filepath.Join(root, native(l[0]))))
	}
	return root
}

// A path that leaves the root is refused naming the link it leaves through,
// and the flag that would let it is offered with its cost. Until 2026-10-07
// every one of these got one sentence, which named no link, said an absolute
// link in a parent "is refused even when its target is inside" about links
// that were neither absolute nor inside, and offered --allow-outside-root with
// no word of what else it opens.
func TestAPathThatLeavesTheRootNamesTheLink(t *testing.T) {
	root := leavingTree(t)
	parent := filepath.Dir(root)
	out := filepath.Join(parent, "outside")
	const leads, cannot = "leads out of the root", "cannot be followed (path escapes from parent)"
	for _, c := range []struct {
		name, in   string
		link       string // the link the refusal names, with its destination as Readlink gives it
		says       string
		unconfined string // where it resolves with the flag
		holds      bool   // the unconfined answer holds where a ".." after a link is cleaned as text
	}{
		{"an absolute link out", "absout/outside.txt", "absout", leads, filepath.Join(out, "outside.txt"), true},
		{"a relative link out", "relout/outside.txt", "relout", leads, filepath.Join(out, "outside.txt"), true},
		{
			"a relative link to a link out, which is the one named", "chain/outside.txt",
			"relout", leads, filepath.Join(out, "outside.txt"), true,
		},
		{
			"a link out below a link that stays in", "a2/b/outside.txt",
			native("sub/b"), leads, filepath.Join(out, "outside.txt"), true,
		},
		{"a link to the root's parent", "up/x", "up", leads, filepath.Join(parent, "x"), true},
		{
			"an absolute link under the root as written and above it walked", "absup/outside/outside.txt",
			"absup", leads, filepath.Join(out, "outside.txt"), true,
		},
		{
			// With the refusal held, a link with a ".." after it is not
			// followed, so the refusal names x, whose ".." os.Root on Windows
			// would clean as text before following anything. Unconfined,
			// Windows cleans it too, and the answer is the grandparent's.
			"a link with a .. after it, while os.Root refuses the path", "dd/f",
			"x", cannot, filepath.Join(parent, "f"), false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, err := OpenTree(root, false)
			must(t, err)
			defer tree.Close()
			tg, err := tree.Resolve(c.in)
			var pr *PathRefusal
			if !errors.As(err, &pr) {
				t.Fatalf("Resolve(%q) = %q, %v; want a *PathRefusal", c.in, tg.name, err)
			}
			if pr.Path != c.in {
				t.Errorf("Path = %q, want it as written, %q", pr.Path, c.in)
			}
			dest, rlErr := os.Readlink(filepath.Join(root, c.link))
			must(t, rlErr)
			want := "the symlink " + c.link + " -> " + dest + " " + c.says
			if c.says == leads {
				want += "; " + flagCost
			}
			if !strings.HasPrefix(pr.Reason, want) {
				t.Errorf("Reason = %q, want it to start %q", pr.Reason, want)
			}
			if pr.Resolved != "" {
				t.Errorf("Resolved = %q; the link and its destination say where it led", pr.Resolved)
			}
			if !strings.HasSuffix(err.Error(), "; the root is "+tree.Root()) {
				t.Errorf("%q does not end by naming the root", err)
			}

			if !c.holds && !dotDotIsWalked() {
				return
			}
			loose, err := OpenTree(root, true)
			must(t, err)
			defer loose.Close()
			tg, err = loose.Resolve(c.in)
			if err != nil {
				t.Fatalf("unconfined Resolve(%q): %v", c.in, err)
			}
			if tg.name != c.unconfined {
				t.Errorf("unconfined name = %q, want %q", tg.name, c.unconfined)
			}
		})
	}
}

// Through a whole batch, confined: exit 2, the refusal on stderr, and nothing
// written inside the root or outside it. The decoy is root/outside/outside.txt,
// which is where relout/outside.txt lands if the ".." that leaves the root is
// dropped at its top, as an unconfined walk drops one at the top of the disk.
func TestAPathThatLeavesTheRootWritesNothing(t *testing.T) {
	for _, path := range []string{"relout/outside.txt", "absout/outside.txt", "absup/outside/outside.txt"} {
		t.Run(path, func(t *testing.T) {
			root := leavingTree(t)
			before, beforeOut := snapshot(t, root), snapshot(t, filepath.Join(filepath.Dir(root), "outside"))
			code, out, errOut := runCLI(t, root, nil, "@@ file "+path+"\n@@ old\noutside\n@@ new\nEDIT\n")
			if code != exitNoMatch {
				t.Errorf("exit %d, want %d: %s", code, exitNoMatch, errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want the refusal on stderr and nothing else", out)
			}
			if !strings.Contains(errOut, "leads out of the root; "+flagCost) {
				t.Errorf("stderr = %q, want the refusal of a path that leaves", errOut)
			}
			assertUnchanged(t, root, before)
			assertUnchanged(t, filepath.Join(filepath.Dir(root), "outside"), beforeOut)
			if got := readFile(t, root, "outside/outside.txt"); got != "decoy\n" {
				t.Errorf("the decoy = %q", got)
			}
		})
	}
}

// The refusal is new wording, so it is a contract (§8.1), in both renderings,
// and the first golden this refusal has had: until 2026-10-07 it was asserted
// by a substring only. The root is substituted as in TestAPathRefusalIsGolden,
// and so are the separators, since the destination is printed as Readlink
// gave it.
func TestAPathThatLeavesTheRootIsGolden(t *testing.T) {
	root := cliTree(t, map[string]string{"sub/in.txt": "x\n"})
	must(t, os.Symlink(native("../outside"), filepath.Join(root, "absdir")))
	before := snapshot(t, root)
	patch := "@@ file absdir/outside.txt\n@@ old\nx\n@@ new\ny\n"

	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch {
		t.Fatalf("exit %d, want %d: %s", code, exitNoMatch, errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want the refusal on stderr and nothing else", out)
	}
	golden(t, "cli-path-refused-leaves-the-root", slashPaths(strings.ReplaceAll(errOut, root, "/the/root")))

	code, out, errOut = runCLI(t, root, []string{"--json"}, patch)
	if code != exitNoMatch || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want %d and nothing on stderr", code, errOut, exitNoMatch)
	}
	escaped, err := json.Marshal(root)
	must(t, err)
	out = strings.ReplaceAll(out, strings.Trim(string(escaped), `"`), "/the/root")
	golden(t, "cli-path-refused-leaves-the-root-json", strings.ReplaceAll(out, `\\`, "/"))
	assertUnchanged(t, root, before)
}

// The report of an edit through an absolute link in a parent names the path
// as the patch wrote it, as every report does, in both renderings.
func TestAnEditThroughAnAbsoluteLinkInAParentIsGolden(t *testing.T) {
	tree := func() string {
		root := cliTree(t, map[string]string{"sub/in.txt": "x\n"})
		must(t, os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "absdir")))
		return root
	}
	patch := "@@ file absdir/in.txt\n@@ old\nx\n@@ new\ny\n"
	root := tree()
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitOK {
		t.Fatalf("exit %d, want 0: %s", code, errOut)
	}
	golden(t, "cli-an-absolute-link-in-a-parent", out)
	if got := readFile(t, root, "sub/in.txt"); got != "y\n" {
		t.Errorf("sub/in.txt = %q, want the edit", got)
	}
	code, out, errOut = runCLI(t, tree(), []string{"--json"}, patch)
	if code != exitOK {
		t.Fatalf("exit %d, want 0: %s", code, errOut)
	}
	golden(t, "cli-an-absolute-link-in-a-parent-json", out)
}

// An escape is os.Root's error, known by the text of the innermost error
// alone. The first row pins that os.Root still says it, which is the guard
// against Go rewording an error it does not export.
func TestAnEscapeIsKnownByItsInnerError(t *testing.T) {
	r, err := os.OpenRoot(t.TempDir())
	must(t, err)
	defer r.Close()
	_, rootSays := r.Lstat(filepath.Join("..", "x"))
	if rootSays == nil {
		t.Fatal("os.Root let a path climb out of it")
	}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"os.Root's refusal of a path that climbs out", rootSays, true},
		{"the same, wrapped again", fmt.Errorf("load: %w", rootSays), true},
		{"the bare text", errors.New("path escapes from parent"), true},
		{
			"a path that holds the text, with another error",
			&fs.PathError{Op: "statat", Path: "escapes from parent/x.txt", Err: syscall.ENOTDIR}, false,
		},
		{
			"a path that holds all of it",
			&fs.PathError{Op: "openat", Path: "a path escapes from parent here/x", Err: syscall.ELOOP}, false,
		},
		{"more than the text", errors.New("path escapes from parent, twice"), false},
		{"nothing", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := escapes(c.err); got != c.want {
				t.Errorf("escapes(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// A path that only names the text os.Root's escape error holds is not an
// escape. Until 2026-10-07 the whole error was searched, path included, so
// each of these was refused as one: a file used as a directory said the path
// leaves the root, under --allow-outside-root it advised setting
// --allow-outside-root, and a loop, exit 5, became exit 2.
func TestAPathNamedLikeTheEscapeIsNotOne(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, root string)
		patch string
		flags []string
		exit  int
		says  string
	}{
		{
			name:  "a file used as a directory",
			setup: func(t *testing.T, root string) { mustWrite(t, root, "escapes from parent", "x") },
			patch: "@@ create escapes from parent/x.txt\nx\n\n",
			exit:  exitNoMatch, says: "it is inside escapes from parent, which is a file, not a directory",
		},
		{
			name:  "the same under --allow-outside-root",
			setup: func(t *testing.T, root string) { mustWrite(t, root, "escapes from parent", "x") },
			patch: "@@ create escapes from parent/x.txt\nx\n\n",
			flags: []string{"--allow-outside-root"},
			exit:  exitNoMatch, says: "it is inside escapes from parent, which is a file, not a directory",
		},
		{
			name:  "a replace through a file whose name holds all of it",
			setup: func(t *testing.T, root string) { mustWrite(t, root, "a path escapes from parent here", "x") },
			patch: "@@ file a path escapes from parent here/x.txt\n@@ old\nx\n@@ new\ny\n",
			exit:  exitNoMatch, says: "no such file; a path escapes from parent here is a file, so nothing can be inside it",
		},
		{
			name: "a loop",
			setup: func(t *testing.T, root string) {
				must(t, os.Symlink("escapes from parent", filepath.Join(root, "escapes from parent")))
			},
			patch: "@@ create escapes from parent/x.txt\nx\n\n",
			exit:  exitIO,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, nil)
			c.setup(t, root)
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, c.flags, c.patch)
			if code != c.exit {
				t.Errorf("exit %d, want %d: %s", code, c.exit, errOut)
			}
			if !strings.Contains(errOut, c.says) {
				t.Errorf("stderr = %q, want %q", errOut, c.says)
			}
			for _, not := range []string{"leaves the root", "leads out of the root", "--allow-outside-root"} {
				if strings.Contains(errOut, not) {
					t.Errorf("stderr = %q, which says %q", errOut, not)
				}
			}
			assertUnchanged(t, root, before)
		})
	}
}

func mustWrite(t *testing.T, root, name, body string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o644))
}
