package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
			if tg.name != native(c.wantName) {
				t.Errorf("name = %q, want %q", tg.name, native(c.wantName))
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
		t.Error("os.Root now allows an absolute in-root symlink; linkDestination's translation of one may be able to go")
	}

	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()
	tg, err := tree.Resolve("abs.link")
	if err != nil {
		t.Fatalf("hunk must still allow it (§6.5): %v", err)
	}
	if tg.name != native("sub/in.txt") {
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

// Every link on a path is resolved, in any component, so one file has one name
// whatever spelling reached it (§3.5). Until 2026-09-17 only the final
// component's was: with d -> sub, sub/in.txt and d/in.txt were two names, and a
// batch using both loaded the file twice, wrote it twice, and reported both
// while the second write discarded the first.
func TestResolveGivesAFileOneName(t *testing.T) {
	root := mktree(t)
	must(t, os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "other"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "other", "f.txt"), []byte("o\n"), 0o644))
	// In order, so each link's target exists before a link to it is made.
	for _, l := range [][2]string{
		{"d", "sub"},
		{"e", "d"},
		{"sub/alias.link", "in.txt"},
		{"sub/inner", "../other"},
		{"x", "sub/deep"},
		// x/.. is sub, physically. Cleaning the text would make it the root.
		{"phys.link", "x/../in.txt"},
		{"l", "internal"},
		{"nowhere.link", "missing/../top.txt"},
		{"dot.link", "./sub"},
		{"here.link", "."},
		{"dotdot.link", "sub/./../top.txt"},
		{"up", ".."},
	} {
		must(t, os.Symlink(filepath.FromSlash(l[1]), filepath.Join(root, filepath.FromSlash(l[0]))))
	}
	must(t, os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "absd")))

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

		for _, c := range []struct {
			name, in, want string
			via            bool
		}{
			{"no link at all", "sub/in.txt", "sub/in.txt", false},
			{"a path that does not exist yet", "new/deep/file.go", "new/deep/file.go", false},
			{"a directory link", "d/in.txt", "sub/in.txt", true},
			{"a chain of directory links", "e/in.txt", "sub/in.txt", true},
			{"a final link under a directory link", "d/alias.link", "sub/in.txt", true},
			{"a link whose destination climbs with ..", "sub/inner/f.txt", "other/f.txt", true},
			{"a .. after a link inside a destination, as the platform reads it", "phys.link", dotDotAnswer("sub/in.txt", "in.txt"), true},
			{"a new file under a directory link", "d/new/x.go", "sub/new/x.go", true},
			{"a dangling directory link, written through", "l/new.go", "internal/new.go", true},
			{"a path below a dangling directory link", "l/a/b.go", "internal/a/b.go", true},
			{"a . inside a destination", "dot.link/in.txt", "sub/in.txt", true},
			{"a link to the root itself", "here.link", ".", true},
			{"a . then a .. inside a destination", "dotdot.link", "top.txt", true},
		} {
			t.Run(mode.name+", "+c.name, func(t *testing.T) {
				tg, err := tree.Resolve(c.in)
				if err != nil {
					t.Fatalf("Resolve(%q): %v", c.in, err)
				}
				if want := mode.want(c.want); tg.name != want {
					t.Errorf("name = %q, want %q", tg.name, want)
				}
				if tg.ViaSymlink() != c.via {
					t.Errorf("ViaSymlink = %v, want %v", tg.ViaSymlink(), c.via)
				}
				if tg.Orig() != c.in {
					t.Errorf("Orig = %q, want it as written, %q", tg.Orig(), c.in)
				}
			})
		}

		// A .. inside a link's destination, after a directory that does not
		// exist, has no physical answer: the kernel stops at the missing
		// directory. Written through, it would make a path that no spelling of
		// it reaches, so it is refused. Windows reads the .. as text and never
		// looks for the missing directory, so there it is top.txt.
		t.Run(mode.name+", a .. after a directory that does not exist", func(t *testing.T) {
			tg, err := tree.Resolve("nowhere.link")
			if !dotDotIsWalked() {
				if want := mode.want("top.txt"); err != nil || tg.name != want {
					t.Errorf("Resolve = %q, %v; want %q", tg.name, err, want)
				}
				return
			}
			var pr *PathRefusal
			if !errors.As(err, &pr) {
				t.Fatalf("Resolve = %v, want a *PathRefusal", err)
			}
			for _, want := range []string{"climbs out of a directory that does not exist", tree.Root()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%q does not say %q", err.Error(), want)
				}
			}
		})
	}

	// The absolute directory link: resolved in both modes. Confined it was
	// refused until 2026-10-07, by os.Root and in its words; it is translated
	// now, as a final one is (TestAnAbsoluteLinkInAParentIsTranslated).
	loose, err := OpenTree(root, true)
	must(t, err)
	t.Cleanup(func() { loose.Close() })
	tg, err := loose.Resolve("absd/in.txt")
	must(t, err)
	if want := filepath.Join(root, "sub", "in.txt"); tg.name != want {
		t.Errorf("unconfined absd/in.txt = %q, want %q", tg.name, want)
	}
	confined, err := OpenTree(root, false)
	must(t, err)
	t.Cleanup(func() { confined.Close() })
	tg, err = confined.Resolve("absd/in.txt")
	must(t, err)
	if want := native("sub/in.txt"); tg.name != want {
		t.Errorf("confined absd/in.txt = %q, want %q", tg.name, want)
	}

	// Confined, a link in a parent that climbs out of the root is refused,
	// naming it, before the walk could clamp it at the top.
	_, err = confined.Resolve("up/x")
	var pr *PathRefusal
	if !errors.As(err, &pr) || !strings.HasPrefix(pr.Reason, "the symlink up -> .. leads out of the root") {
		t.Errorf("confined up/x = %v, want the refusal of a link that leads out", err)
	}

	// Unconfined, a ".." above the top of the path stays at the top, as it does
	// for the kernel. Confined, os.Root refuses the same link as an escape. On
	// Windows this came back as written until 2026-10-07, since the walk could
	// not get past the climb; read as text, as Windows reads it, it climbs to
	// the top and stays there like anywhere else.
	vol := filepath.VolumeName(root)
	climb := filepath.Join(strings.Repeat(".."+string(filepath.Separator), 64), root[len(vol):], "sub")
	must(t, os.Symlink(climb, filepath.Join(root, "climb.link")))
	tg, err = loose.Resolve("climb.link/in.txt")
	must(t, err)
	if want := filepath.Join(root, "sub", "in.txt"); tg.name != want {
		t.Errorf("unconfined climb.link/in.txt = %q, want %q", tg.name, want)
	}
}

// isDotGit is git's rule for a component that names .git, every spelling of it
// on every platform: case, NTFS's trailing dots and spaces, stream suffix and
// short name, and HFS+'s ignorable code points. The names beside it that are
// part of the working tree have to stay editable, and so does anything that is
// only one rule's spelling away from .git under no filesystem at all.
func TestIsDotGit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{".git", true},
		{".GIT", true},
		{".Git", true},
		{".git.", true},
		{".git ", true},
		{".git. . ", true},
		{".git:stream", true},
		{".git::$INDEX_ALLOCATION", true},
		{".git. :x", true},
		{"git~1", true},
		{"GIT~1", true},
		{"git~1.", true},
		{"git~1:x", true},
		{".g\u200cit", true},
		{"\u200d.git", true},
		{".gi\u202et", true},
		{".git\ufeff", true},
		{".G\u206aIT", true},
		{".git\u200f", true},

		{"", false},
		{"git", false},
		{".gi", false},
		{".gitignore", false},
		{".github", false},
		{".gitattributes", false},
		{".gitmodules", false},
		{".git.x", false},
		{".git~1", false},
		{"x.git", false},
		{"repo.git", false},
		{"..git", false},
		{".g it", false},
		{"git~2", false},
		{"git~1x", false},
		{"g\u200cit~1", false},
		{".g\u200bit", false}, // ZERO WIDTH SPACE is not on HFS+'s list
		{".g\u0131t", false},  // dotless i does not fold to i
		{".g\u0130t", false},  // nor does dotted capital I
		{".git\u200c.", false},
	} {
		if got := isDotGit(c.in); got != c.want {
			t.Errorf("isDotGit(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestInsideDotGit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{native(".git/config"), true},
		{native("a/.git"), true},
		{native("a/.GIT/b/c"), true},
		{filepath.Join(string(filepath.Separator)+"abs", "root", ".git", "HEAD"), true},
		{native(".gitignore"), false},
		{native("sub/.github/workflows/ci.yml"), false},
		{native("repo.git/config"), false},
		{"", false},
	} {
		if got := insideDotGit(c.in); got != c.want {
			t.Errorf("insideDotGit(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A delete cannot go through a link the patch named, and can go through one in a
// directory above it, so Resolve has to tell the two apart. ViaSymlink cannot:
// it is set by both. named is set by a link standing last on the path, however
// it was reached, and by nothing else.
func TestResolveSaysWhetherTheNamedEntryIsALink(t *testing.T) {
	root := mktree(t)
	for _, l := range [][2]string{
		{"rel.link", "sub/in.txt"},
		{"chain.link", "rel.link"},
		{"d", "sub"},
		{"sub/alias.link", "in.txt"},
		{"through.link", "d/in.txt"},
		{"dangling.link", "missing.txt"},
		{"l", "internal"},
	} {
		must(t, os.Symlink(filepath.FromSlash(l[1]), filepath.Join(root, filepath.FromSlash(l[0]))))
	}
	must(t, os.Symlink(filepath.Join(root, "sub", "in.txt"), filepath.Join(root, "abs.link")))

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

		for _, c := range []struct {
			name, in, want string
			via, named     bool
		}{
			{"no link at all", "sub/in.txt", "sub/in.txt", false, false},
			{"a path that does not exist yet", "new/x.go", "new/x.go", false, false},
			{"a relative link", "rel.link", "sub/in.txt", true, true},
			{"an absolute in-root link", "abs.link", "sub/in.txt", true, true},
			{"a chain of links", "chain.link", "sub/in.txt", true, true},
			{"a dangling link", "dangling.link", "missing.txt", true, true},
			{"a directory link above a file", "d/in.txt", "sub/in.txt", true, false},
			{"a dangling directory link above a new file", "l/new.go", "internal/new.go", true, false},
			{"a link reached through a directory link", "d/alias.link", "sub/in.txt", true, true},
			{"a link whose destination runs through a directory link", "through.link", "sub/in.txt", true, true},
		} {
			t.Run(mode.name+", "+c.name, func(t *testing.T) {
				tg, err := tree.Resolve(c.in)
				if err != nil {
					t.Fatalf("Resolve(%q): %v", c.in, err)
				}
				if want := mode.want(c.want); tg.name != want {
					t.Errorf("name = %q, want %q", tg.name, want)
				}
				if tg.ViaSymlink() != c.via || tg.named != c.named {
					t.Errorf("ViaSymlink = %v, named = %v; want %v, %v", tg.ViaSymlink(), tg.named, c.via, c.named)
				}
				// Load respells every target before it is used, and a respelled
				// target that forgot the link would delete through it again.
				if got := tree.Spellings().Respell(tg); got.named != c.named {
					t.Errorf("respelled, named = %v, want %v", got.named, c.named)
				}
			})
		}
	}
}

// On a filesystem that folds case or Unicode normalization, two spellings of one
// name are one file, and until 2026-09-17 they were two entries in the load
// index: A.go and a.go in one batch lost an edit under exit 0. A name that
// exists is now spelled the way its directory stores it, and a name the batch
// creates the way the batch first spelled it, wherever its directory folds.
//
// Every row has two right answers, and the filesystem the test runs on says
// which applies: Linux does not fold, macOS and Windows do, and so does this
// machine's /workspace, which is how it was found.
func TestASpellingIsTheOneTheDirectoryStores(t *testing.T) {
	folds, normalizes := foldsCase(t), foldsNormalization(t)
	nfc, nfd := "é.txt", "é.txt"
	root := mktree(t)
	must(t, os.WriteFile(filepath.Join(root, nfc), []byte("e\n"), 0o644))
	// Sorts before nfc, so a lookup that took the first name not in ASCII,
	// rather than the one that is the same file, would pick it.
	must(t, os.WriteFile(filepath.Join(root, "\u00e0.txt"), []byte("a\n"), 0o644))
	must(t, os.Link(filepath.Join(root, "top.txt"), filepath.Join(root, "hard.txt")))
	must(t, os.MkdirAll(filepath.Join(root, "2026"), 0o755))
	for _, n := range []string{"1", "2"} {
		must(t, os.MkdirAll(filepath.Join(root, "digits", n), 0o755))
	}

	pick := func(when bool, yes, no string) string {
		if when {
			return yes
		}
		return no
	}
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
		respell := func(t *testing.T, s *Spellings, in string) string {
			t.Helper()
			tg, err := tree.Resolve(in)
			must(t, err)
			return s.Respell(tg).name
		}

		for _, c := range []struct{ name, in, want string }{
			{"the stored spelling", "top.txt", "top.txt"},
			{"a file in another case", "TOP.TXT", pick(folds, "top.txt", "TOP.TXT")},
			{"a directory in another case", "SUB/in.txt", pick(folds, "sub/in.txt", "SUB/in.txt")},
			{"every component in another case", "Sub/IN.txt", pick(folds, "sub/in.txt", "Sub/IN.txt")},
			{"the other normalization", nfd, pick(normalizes, nfc, nfd)},
			{"a hard link keeps its own name", "hard.txt", "hard.txt"},
			{"a name that does not exist is left as written", "new/Deep.go", "new/Deep.go"},
			{"a name with no letter in it", "2026", "2026"},
		} {
			t.Run(mode.name+", "+c.name, func(t *testing.T) {
				if got, want := respell(t, tree.Spellings(), c.in), mode.want(c.want); got != want {
					t.Errorf("Respell(%q) = %q, want %q (folds case: %v, normalization: %v)", c.in, got, want, folds, normalizes)
				}
			})
		}

		// Names the batch creates, spelled more than once in one batch.
		for _, c := range []struct {
			name string
			in   []string
			want []string
		}{
			{"a new name in two cases", []string{"n.go", "N.go"}, []string{"n.go", pick(folds, "n.go", "N.go")}},
			{"a new directory in two cases", []string{"NEW/x.go", "new/x.go"}, []string{"NEW/x.go", pick(folds, "NEW/x.go", "new/x.go")}},
			{"a new name meeting its own spelling", []string{"n.go", "n.go"}, []string{"n.go", "n.go"}},
			// Nothing in 2026 has a letter, and neither does its name, so it
			// cannot say whether it folds. It is assumed to (Kevin, 2026-09-17):
			// a refusal is recoverable and a lost file is not.
			{"new names under a directory nothing can probe", []string{"2026/n.go", "2026/N.go"}, []string{"2026/n.go", "2026/n.go"}},
			// Nothing in digits has a letter, but its own name does, so it is
			// asked from its parent.
			{"a directory that answers with its own name", []string{"digits/n.go", "digits/N.go"}, []string{"digits/n.go", pick(folds, "digits/n.go", "digits/N.go")}},
			{"a directory asked once and answering twice", []string{"n.go", "N.go", "m.go", "M.go"}, []string{"n.go", pick(folds, "n.go", "N.go"), "m.go", pick(folds, "m.go", "M.go")}},
			{"a new name that is not UTF-8 is its own", []string{"\xffn.go", "\xffN.go"}, []string{"\xffn.go", "\xffN.go"}},
			// Inside a directory the batch creates, the directory that exists above
			// it decides, not the new one, which cannot be asked anything.
			{"a new name in two cases inside a new directory", []string{"NEW/x.go", "NEW/X.go"}, []string{"NEW/x.go", pick(folds, "NEW/x.go", "NEW/X.go")}},
		} {
			t.Run(mode.name+", "+c.name, func(t *testing.T) {
				s := tree.Spellings()
				for i, in := range c.in {
					if got, want := respell(t, s, in), mode.want(c.want[i]); got != want {
						t.Errorf("Respell(%q) = %q, want %q (folds case: %v)", in, got, want, folds)
					}
				}
			})
		}
	}

	// A directory that can be searched and not read cannot say how it stores a
	// name, so the name stays as written. On a filesystem that does not fold
	// the name is simply not there, and stays as written for that reason.
	//
	// Unconfined, because os.Root opens each directory it walks through, so a
	// confined tree cannot reach a name in a directory it cannot read at all,
	// and leaves it as written before a directory read is ever tried. Plain os
	// looks a name up with search permission alone, which is the case here.
	t.Run("a directory that cannot be read", func(t *testing.T) {
		needsPOSIXPerms(t)
		locked := filepath.Join(root, "sub")
		must(t, os.Chmod(locked, 0o311))
		t.Cleanup(func() { os.Chmod(locked, 0o755) })
		tree, err := OpenTree(root, true)
		must(t, err)
		defer tree.Close()
		tg, err := tree.Resolve("sub/IN.TXT")
		must(t, err)
		if got, want := tree.Spellings().Respell(tg).name, filepath.Join(root, "sub", "IN.TXT"); got != want {
			t.Errorf("Respell = %q, want it as written, %q", got, want)
		}
	})
}

// No file name can hold a NUL byte, and one in a patch half-wrote the tree:
// under a directory the batch creates, load's stat stopped at the missing
// directory, and the name was first refused at the rename, after the files
// before it were written. Fuzzing FuzzApplyIsAllOrNothing found it on
// 2026-09-17. Where load could reach the byte, it was exit 5 in a syscall's
// words. It is a path refusal wherever it is, confined or not.
func TestANulByteInAPathIsRefused(t *testing.T) {
	root := mktree(t)
	for _, mode := range []struct {
		name         string
		allowOutside bool
	}{{"confined", false}, {"unconfined", true}} {
		tree, err := OpenTree(root, mode.allowOutside)
		must(t, err)
		t.Cleanup(func() { tree.Close() })
		for _, c := range []struct{ name, in string }{
			{"under a directory that does not exist", "new/\x00"},
			{"as a directory under one that does not exist", "new/\x00/deep.go"},
			{"under a directory that exists", "sub/\x00"},
			{"inside a name", "a\x00b.go"},
			{"alone", "\x00"},
		} {
			t.Run(mode.name+", "+c.name, func(t *testing.T) {
				_, err := tree.Resolve(c.in)
				var pr *PathRefusal
				if !errors.As(err, &pr) {
					t.Fatalf("Resolve(%q) = %v, want a *PathRefusal", c.in, err)
				}
				if got := ExitCode(err); got != exitNoMatch {
					t.Errorf("exit %d, want %d", got, exitNoMatch)
				}
				if pr.Path != c.in {
					t.Errorf("path = %q, want it as written, %q", pr.Path, c.in)
				}
				for _, want := range []string{"the path contains a NUL byte", "<U+0000>", tree.Root()} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%q does not say %q", err.Error(), want)
					}
				}
				// Until 2026-10-06 the message printed the path, NUL and all.
				if strings.Contains(err.Error(), "\x00") {
					t.Errorf("%q prints the NUL raw", err.Error())
				}
			})
		}
	}

	// The find, end to end. The fuzz snapshot records files, so this is what
	// says the directory is not left behind either, and that a dry run agrees.
	patch := "@@ create 0\n@@ create 1/\x00"
	for _, args := range [][]string{nil, {"--dry-run"}} {
		t.Run("the find, with args "+strings.Join(args, " "), func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "x\n"})
			code, _, errOut := runCLI(t, root, args, patch)
			if code != exitNoMatch {
				t.Fatalf("exit %d, want %d: %s", code, exitNoMatch, errOut)
			}
			if !strings.Contains(errOut, "the path contains a NUL byte") {
				t.Errorf("stderr = %q", errOut)
			}
			for _, name := range []string{"0", "1"} {
				if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s is on disk: %v", name, err)
				}
			}
		})
	}
}

// Nobody means a control character in a file name. Until 2026-10-06 one in a
// create's path made the file, at exit 0, and the report printed the name raw:
// an escape sequence coloured the terminal, and a CR left by a CRLF patch named
// a file that looked like the one meant and was not. It is a path refusal
// beside NUL, confined or not, and the message names the character instead of
// printing it, while Path stays exactly as written.
func TestAControlCharacterInAPathIsRefused(t *testing.T) {
	root := mktree(t)
	for _, mode := range []struct {
		name         string
		allowOutside bool
	}{{"confined", false}, {"unconfined", true}} {
		tree, err := OpenTree(root, mode.allowOutside)
		must(t, err)
		t.Cleanup(func() { tree.Close() })
		for _, c := range []struct{ name, in, char string }{
			{"an escape sequence", "x\x1b[31mred.txt", "U+001B"},
			{"a vertical tab", "a\x0bb.go", "U+000B"},
			{"a bell", "a\x07.go", "U+0007"},
			{"DEL", "a\x7f.go", "U+007F"},
			{"NEL, a C1 control", "a\u0085b.go", "U+0085"},
			{"CSI, the C1 escape introducer", "a\u009b31m.go", "U+009B"},
			{"CSI as a lone byte, which is not UTF-8", "a\x9b31m.go", "0x9B"},
			{"a CR inside a name", "a\rb.go", "U+000D"},
			{"a CR at the end, where a second CR is left", "a.go\r", "U+000D"},
			{"in a directory", "sub\x1b/in.txt", "U+001B"},
			{"first in the path", "\x1bx", "U+001B"},
			{"under a directory that does not exist", "new/\x1b", "U+001B"},
			{"the first of two is named", "a\x07\x1b.go", "U+0007"},
		} {
			t.Run(mode.name+", "+c.name, func(t *testing.T) {
				_, err := tree.Resolve(c.in)
				var pr *PathRefusal
				if !errors.As(err, &pr) {
					t.Fatalf("Resolve(%q) = %v, want a *PathRefusal", c.in, err)
				}
				if got := ExitCode(err); got != exitNoMatch {
					t.Errorf("exit %d, want %d", got, exitNoMatch)
				}
				if pr.Path != c.in {
					t.Errorf("path = %q, want it as written, %q", pr.Path, c.in)
				}
				for _, want := range []string{
					"the path contains " + c.char + ", a control character, which hunk does not allow in a file name; remove it from the patch",
					"<" + c.char + ">", tree.Root(),
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%q does not say %q", err.Error(), want)
					}
				}
				if name, ok := firstControl(err.Error()); ok {
					t.Errorf("%q prints %s raw", err.Error(), name)
				}
			})
		}
		// A tab is allowed in a path, as it always was: it is not refused as a
		// control character, whatever the platform makes of the name.
		t.Run(mode.name+", a tab", func(t *testing.T) {
			if _, err := tree.Resolve("a\tb.go"); err != nil && strings.Contains(err.Error(), "control character") {
				t.Errorf("a tab was refused as a control character: %v", err)
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
		must(t, tree.WriteAtomic(tg, []byte("rewritten\n"), 0o640, nil))

		got, err := os.ReadFile(filepath.Join(root, "top.txt"))
		must(t, err)
		if string(got) != "rewritten\n" {
			t.Errorf("contents %q", got)
		}
		fi, err := os.Stat(filepath.Join(root, "top.txt"))
		must(t, err)
		assertMode(t, fi.Mode(), 0o640)
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
		must(t, tree.WriteAtomic(tg, []byte("through\n"), 0o644, nil))

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
		must(t, tree.WriteAtomic(tg, []byte("x\n"), 0o644, nil))

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
		if err := tree.WriteAtomic(Target{}, []byte("x"), 0o644, nil); err == nil {
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
		if err := tree.WriteAtomic(tg, []byte("x"), 0o644, nil); err == nil {
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
		if made[i].Name != native(want[i]) {
			t.Errorf("made[%d] = %q, want %q", i, made[i].Name, native(want[i]))
		}
		// Each is described as the directory it made, which rollback compares
		// with what stands at the name later.
		fi, err := os.Lstat(filepath.Join(root, want[i]))
		must(t, err)
		if made[i].Info == nil || !made[i].Info.IsDir() || !os.SameFile(made[i].Info, fi) {
			t.Errorf("made[%d].Info does not describe %s", i, want[i])
		}
	}

	// A directory that already existed is not reported as created, so rollback
	// cannot remove something this tool did not make.
	tg2, err := tree.Resolve("sub/deeper/f.txt")
	must(t, err)
	made2, err := tree.MkdirAll(tg2)
	must(t, err)
	if len(made2) != 1 || made2[0].Name != native("sub/deeper") {
		t.Errorf("made %v, want just sub/deeper", made2)
	}

	must(t, tree.WriteAtomic(tg, []byte("x\n"), 0o644, nil))
	must(t, tree.Remove(tg))
	if _, err := os.Stat(filepath.Join(root, "a/b/c/f.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("Remove did not remove")
	}
	for _, d := range made {
		must(t, tree.RemoveDir(d.Name))
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
	assertMode(t, fi.Mode(), 0o644)
	b, err := tree.ReadFile(tg)
	must(t, err)
	if string(b) != "in\n" {
		t.Errorf("read %q", b)
	}
}

// FileAbove names the file a path runs through, asking the directories rather
// than reading the error, which Linux and macOS spell ENOTDIR and Windows
// spells "does not exist". Every row runs confined and unconfined, and the
// file is named as the patch spelled it in both.
func TestFileAboveNamesTheFileInTheWay(t *testing.T) {
	for _, unconfined := range []bool{false, true} {
		root := mktree(t)
		must(t, os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))
		must(t, os.Symlink("top.txt", filepath.Join(root, "link.txt")))
		must(t, os.Symlink("sub", filepath.Join(root, "d")))
		tree, err := OpenTree(root, unconfined)
		must(t, err)
		t.Cleanup(func() { tree.Close() })

		for _, c := range []struct {
			path, want string // want "" means no file is in the way
		}{
			{"top.txt/x", "top.txt"},
			{"top.txt/deeper/x", "top.txt"},
			{"./top.txt/x", "top.txt"},
			{"sub/in.txt/x", "sub/in.txt"},
			{"link.txt/x", "link.txt"},
			{"d/in.txt/x", "d/in.txt"},
			{"sub/deep/in.txt/x", ""},
			{"sub/new.txt", ""},
			{"new/deeper/x.txt", ""},
			{"new.txt", ""},
			{"top.txt", ""},
		} {
			t.Run(fmt.Sprintf("unconfined=%v/%s", unconfined, c.path), func(t *testing.T) {
				tg, err := tree.Resolve(c.path)
				must(t, err)
				shown, name, ok := tree.FileAbove(tg)
				if shown != c.want || ok != (c.want != "") {
					t.Fatalf("FileAbove(%q) = %q, %v; want %q, %v", c.path, shown, ok, c.want, c.want != "")
				}
				if !ok {
					return
				}
				// The name is the tree's, and is one a stat of the file finds.
				fi, err := tree.stat(name)
				if err != nil || fi.IsDir() {
					t.Errorf("FileAbove's name %q is not the file in the way: %v", name, err)
				}
			})
		}
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
		"d/in.txt", "d/new/x.go", "l/x", "sub/../d/in.txt", "rel.link/x",
		"SUB/IN.TXT", "Sub/New/X.go", "D/in.txt", "\u00e9.txt", "e\u0301.txt",
		".git/config", ".GIT/config", "g/config", "cfg.link", "sub/../.git/HEAD", ".git", "sub/.git",
		"g/" + strings.Repeat("0", 256),
		"absd/in.txt", "absd/new/x.go", "absd/abs.link", "absg/config", "absout/passwd", "up/x",
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
	os.Symlink("sub", filepath.Join(root, "d"))
	os.Symlink("internal", filepath.Join(root, "l"))
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644)
	os.Symlink(".git", filepath.Join(root, "g"))
	os.Symlink(filepath.Join(".git", "config"), filepath.Join(root, "cfg.link"))
	// Absolute directory links, which os.Root refuses in a parent wherever
	// they lead: inside, to .git, and out of the root.
	os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "absd"))
	os.Symlink(filepath.Join(root, "sub", "in.txt"), filepath.Join(root, "sub", "abs.link"))
	os.Symlink(filepath.Join(root, ".git"), filepath.Join(root, "absg"))
	os.Symlink(filepath.Dir(filepath.Dir(root)), filepath.Join(root, "absout"))
	os.Symlink("..", filepath.Join(root, "up"))
	// A link to the root beside it, as /tmp is to /private/tmp, so an absolute
	// path through it names the root by another spelling (#62).
	alias := filepath.Join(filepath.Dir(root), "alias")
	os.Symlink(root, alias)
	for _, s := range []string{"sub/in.txt", ".git/config", "../x", "", "absd/in.txt", "up/x", "d/../../x"} {
		f.Add(alias + "/" + s)
	}
	gitDir, err := os.Stat(filepath.Join(root, ".git"))
	if err != nil {
		f.Fatal(err)
	}
	folds := foldsCase(f) || foldsNormalization(f)

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
			// A refusal names a control character; it never prints one.
			if name, ok := firstControl(pr.Error()); ok {
				t.Fatalf("Resolve(%q): %q prints %s raw", p, pr.Error(), name)
			}
			return
		}
		if tg.name == "" {
			t.Fatalf("Resolve(%q) allowed an empty target", p)
		}
		// Nobody means a control character in a file name, and no path holding
		// one resolves (2026-10-06).
		if name, ok := firstControl(p); ok {
			t.Fatalf("Resolve(%q) = %q, a path holding %s", p, tg.name, name)
		}
		abs := filepath.Join(tree.Root(), tg.name)
		if !filepath.IsAbs(tg.name) && abs != tree.Root() &&
			!strings.HasPrefix(abs, tree.Root()+string(filepath.Separator)) {
			t.Fatalf("Resolve(%q) = %q, which is %q, outside the root %q", p, tg.name, abs, tree.Root())
		}
		if filepath.IsAbs(tg.name) {
			t.Fatalf("Resolve(%q) returned an absolute name %q from a confined tree", p, tg.name)
		}
		// The name is the file's one name (§3.5): wherever it can be used at
		// all, as a file that exists or one a create would make, no component
		// of it is still a link. A name that fails for another reason, such as
		// a path through a file, is load's to report and is not checked here.
		if _, err := os.Lstat(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return
		}
		// Nothing usable it hands back is .git or inside it, asked of the
		// kernel rather than of the rule Resolve applies: no part of the
		// target that exists is the .git directory, however the name spells
		// it. A name nothing can use comes back as it stands and is not
		// checked: g/ and a component too long for the filesystem is the
		// first thing fuzzing found, and load stops it at exit 5 having
		// written nothing.
		for dir := abs; ; dir = filepath.Dir(dir) {
			if fi, err := os.Stat(dir); err == nil && os.SameFile(fi, gitDir) {
				t.Fatalf("Resolve(%q) = %q, which is inside .git", p, tg.name)
			}
			if dir == tree.Root() || dir == filepath.Dir(dir) {
				break
			}
		}
		for dir := tg.name; dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			if fi, err := os.Lstat(filepath.Join(tree.Root(), dir)); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				t.Fatalf("Resolve(%q) = %q, and %q in it is still a link", p, tg.name, dir)
			}
		}
		// Respell changes a spelling and never a file: the name it returns is
		// the same file as the one it was given, or is as absent, and where the
		// filesystem folds neither case nor normalization it is the same name.
		rs := tree.Spellings().Respell(tg)
		if !folds && rs.name != tg.name {
			t.Fatalf("Respell(%q) = %q on a filesystem that does not fold", tg.name, rs.name)
		}
		was, wasErr := os.Lstat(abs)
		now, nowErr := os.Lstat(filepath.Join(tree.Root(), rs.name))
		switch {
		case wasErr == nil && (nowErr != nil || !os.SameFile(was, now)):
			t.Fatalf("Respell(%q) = %q, which is not the same file", tg.name, rs.name)
		case errors.Is(wasErr, fs.ErrNotExist) && nowErr == nil:
			t.Fatalf("Respell(%q) = %q, which exists where the name it was given does not", tg.name, rs.name)
		}
	})
}

// os.Root refuses an absolute symlink in an *intermediate* component too, even
// when it points inside the root, and resolveLinks holds that refusal while it
// walks to the link and translates it. Until 2026-10-07 the refusal was
// returned as it stood, with advice to set --allow-outside-root. Pinned as
// TestAbsoluteInRootSymlinkIsTheStdlibGap pins the final case, because the
// day the stdlib relaxes it, this test says so.
func TestAnAbsoluteLinkInAParentIsTheStdlibGap(t *testing.T) {
	root := mktree(t)
	must(t, os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "absdir")))

	r, err := os.OpenRoot(root)
	must(t, err)
	defer r.Close()
	if _, err := r.Lstat(filepath.Join("absdir", "in.txt")); !escapes(err) {
		t.Errorf("os.Root's answer is %v; if it now allows an absolute in-root link in a parent, "+
			"the refusal resolveLinks holds may be able to go", err)
	}

	tree, err := OpenTree(root, false)
	must(t, err)
	defer tree.Close()
	tg, err := tree.Resolve("absdir/in.txt")
	if err != nil {
		t.Fatalf("hunk must allow it, confined (§6.5): %v", err)
	}
	if tg.name != native("sub/in.txt") {
		t.Errorf("resolved to %q, want sub/in.txt", tg.name)
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
	if err := tree.WriteAtomic(tg, []byte("x\n"), 0o644, nil); err == nil {
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
	must(t, tree.WriteAtomic(tg, []byte("unconfined\n"), 0o600, nil))

	fi, err := tree.Stat(tg)
	must(t, err)
	assertMode(t, fi.Mode(), 0o600)
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
	must(t, tree.WriteAtomic(otg, []byte("new\n"), 0o644, nil))
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
		must(t, tree.RemoveDir(d.Name))
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
// the relative-symlink branch of linkDestination. Both are code that only
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
	if err := tree.WriteAtomic(dtg, []byte("x\n"), 0o644, nil); err == nil {
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
	needsPOSIXPerms(t)
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
	if tg.name != native("locked/f.txt") {
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
		if tg.name != native("sub/in.txt") {
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

// A refusal reaches a reader two ways, and both have to say where the tool
// looked. §5.2 prints the path on a line of its own and the reason under it, so
// the report takes Detail; a refusal that aborts the load has no such line, so
// the top-level error takes Error, which is Detail under the path.
//
// Raised by a field report: an agent whose shell had moved into a subdirectory
// got "no such file; only @@ create makes one" and spent two round trips on its
// patch, because Validate stored Reason, and Reason on its own knows nothing
// about a root.
func TestPathRefusalSaysWhereItLooked(t *testing.T) {
	for _, c := range []struct {
		name string
		ref  PathRefusal
		want string
	}{
		{
			name: "the reason and the root",
			ref:  PathRefusal{Path: "a.go", Root: "/r", Reason: "no such file"},
			want: "no such file; the root is /r",
		},
		{
			name: "a resolved path that differs is named as well",
			ref: PathRefusal{
				Path: "link.go", Root: "/r", Resolved: "/elsewhere/a.go",
				Reason: "it is a symlink out of the root",
			},
			want: "it is a symlink out of the root (it resolves to /elsewhere/a.go); the root is /r",
		},
		{
			name: "a resolved path equal to the written one is not repeated",
			ref:  PathRefusal{Path: "a.go", Root: "/r", Resolved: "a.go", Reason: "no such file"},
			want: "no such file; the root is /r",
		},
		{
			// The same case on Windows, where filepath.Clean hands back a
			// native path: the clause would otherwise name the caller's own
			// path back at it with the slashes turned round. Trivially true on
			// POSIX, where native() is identity, and the end-to-end pin is
			// testdata/cli-path-refused.txt on the Windows job, which is what
			// caught it.
			name: "a resolved path differing only in separator is not repeated",
			ref: PathRefusal{
				Path: "../outside.txt", Root: "/r", Resolved: native("../outside.txt"),
				Reason: "the path climbs out of the root",
			},
			want: "the path climbs out of the root; the root is /r",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ref.Detail(); got != c.want {
				t.Errorf("Detail() = %q, want %q", got, c.want)
			}
			if got, want := c.ref.Error(), c.ref.Path+": "+c.want; got != want {
				t.Errorf("Error() = %q, want %q", got, want)
			}
		})
	}
}

// The page an agent reads when a path refusal aborts the load, pinned end to
// end. Nothing did until now: the tests above cover Detail() and Error(), and
// the measurement in scripts/corpus could not classify the printed form at all,
// so a refusal reachable by writing one wrong path counted as "unclassified" in
// §12's exit table.
//
// One golden per branch of the message rather than per reason. The reasons are
// interchangeable text; the branch that names where the path led is a different
// shape, and the cheapest way to reach it needs no symlink: a path that cleans
// to something other than what the patch wrote.
//
// The root is an absolute temporary directory and is in the message by design,
// so it is substituted for a placeholder. Everything else is compared byte for
// byte.
func TestAPathRefusalIsGolden(t *testing.T) {
	for _, c := range []struct{ name, golden, path string }{
		{"a path that climbs out of the root", "cli-path-refused", "../outside.txt"},
		{
			"one that resolves somewhere else first",
			"cli-path-refused-resolves", "sub/../../outside.txt",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"sub/in.txt": "x\n"})
			code, out, errOut := runCLI(t, root, nil, "@@ file "+c.path+"\n@@ old\nx\n@@ new\ny\n")
			if code != exitNoMatch {
				t.Fatalf("exit %d, want %d: %s", code, exitNoMatch, errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want the refusal on stderr and nothing else", out)
			}
			golden(t, c.golden, slashPaths(strings.ReplaceAll(errOut, root, "/the/root")))
		})
	}
}

// keepOwner's order, through fchown. TestEveryRewriteKeepsTheGroup and
// TestAGroupThatCannotBeKeptGetsWhatOthersHad reach it with real groups; the
// second can only run where a foreign group can be made without privilege,
// which no CI runner offers, and an owner that is not the invoker's needs root.
// So each refusal is made here, and what is asserted is still the file on disk.
func TestKeepOwnerWhenChownIsRefused(t *testing.T) {
	needsPOSIXPerms(t)
	refuse := func(owner, group bool) func(*os.File, int, int) error {
		return func(f *os.File, uid, gid int) error {
			if (uid != -1 && owner) || (uid == -1 && group) {
				return fs.ErrPermission
			}
			return f.Chown(uid, gid)
		}
	}
	for _, c := range []struct {
		name         string
		owner, group bool // whether each chown is refused
		keepGroup    bool // whether the file starts in a group a new file would not get
		like         bool // false for a file that was not there
		wantMode     fs.FileMode
		wantGroup    string // "had" or "new"
		wantCalls    int
	}{
		{"nothing refused", false, false, true, true, 0o640, "had", 1},
		{"the owner refused, the group not", true, false, true, true, 0o640, "had", 2},
		{"both refused, a group that differs", true, true, true, true, 0o600, "new", 2},
		{"both refused, the group already right", true, true, false, true, 0o640, "new", 2},
		{"a new file has nothing to keep", true, true, true, false, 0o640, "new", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := mktree(t)
			p := filepath.Join(root, "top.txt")
			fresh := newFileGroup(t, root)
			had := fresh
			if c.keepGroup {
				had = otherGroup(t, fresh)
				must(t, os.Chown(p, -1, had))
			}
			must(t, os.Chmod(p, 0o640))
			var like fs.FileInfo
			if c.like {
				fi, err := os.Stat(p)
				must(t, err)
				like = fi
			}
			calls := 0
			orig := fchown
			t.Cleanup(func() { fchown = orig })
			fchown = func(f *os.File, uid, gid int) error {
				calls++
				return refuse(c.owner, c.group)(f, uid, gid)
			}

			tree, err := OpenTree(root, false)
			must(t, err)
			defer tree.Close()
			tg, err := tree.Resolve("top.txt")
			must(t, err)
			must(t, tree.WriteAtomic(tg, []byte("rewritten\n"), 0o640, like))

			want := map[string]int{"had": had, "new": fresh}[c.wantGroup]
			if got := groupOf(t, p); got != want {
				t.Errorf("group %d, want %d (%s)", got, want, c.wantGroup)
			}
			fi, err := os.Stat(p)
			must(t, err)
			assertMode(t, fi.Mode(), c.wantMode)
			if calls != c.wantCalls {
				t.Errorf("fchown called %d times, want %d", calls, c.wantCalls)
			}
		})
	}
}

// dotDotTree adds the fixture for links whose destinations hold a "..". The
// kernel reaches sub/c.txt through x, and c.txt is the decoy that cleaning the
// text instead lands on. b is a chain of nine directory links, one more than
// os.Root follows, and k a chain of eight. root+"2" is a sibling of the root
// whose name extends the root's. s is a directory with a one-letter name, for
// the links that climb in and out of one more times than os.Root will step.
func dotDotTree(tb testing.TB, root string) {
	tb.Helper()
	do := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatal(err)
		}
	}
	do(os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))
	do(os.MkdirAll(filepath.Join(root, "s"), 0o755))
	do(os.MkdirAll(root+"2", 0o755))
	for name, body := range map[string]string{
		"top.txt": "top\n", "c.txt": "lexical\n", "sub/c.txt": "physical\n", "sub/f.txt": "f\n",
	} {
		do(os.WriteFile(filepath.Join(root, native(name)), []byte(body), 0o644))
	}
	do(os.WriteFile(filepath.Join(root+"2", "c.txt"), []byte("sibling\n"), 0o644))
	// In order, so each link's target exists before a link to it is made, which
	// is how Windows tells a directory link from a file link.
	links := [][2]string{{"x", "sub/deep"}, {"d", "sub"}, {"alias", "c.txt"}, {"b8", "sub/deep"}, {"k7", "sub/deep"}}
	for i := 7; i >= 1; i-- {
		links = append(links, [2]string{"b" + strconv.Itoa(i), "b" + strconv.Itoa(i+1)})
	}
	for i := 6; i >= 1; i-- {
		links = append(links, [2]string{"k" + strconv.Itoa(i), "k" + strconv.Itoa(i+1)})
	}
	links = append(links, [2]string{"b", "b1"}, [2]string{"k", "k1"})
	for _, l := range links {
		do(os.Symlink(native(l[1]), filepath.Join(root, l[0])))
	}
}

// dotDotLinks are the links the rows below name, by the destination each
// holds. One that starts with "/" is under the root; {root} is the root, and
// {base} its last component.
//
// nt.link and ntc.link step through s 130 times, which is past os.Root's 255
// steps, and it is the steps os.Root counts, not the bytes. They used sub,
// until the first CI run: unconfined, the destination is spliced into an
// absolute path, and under a runner's /private/var/folders/... temp directory
// 130 "sub/../" took it past macOS's 1024-byte limit, which is a different
// refusal from the one the rows pin. With s it is about 650 bytes plus the root.
var dotDotLinks = map[string]string{
	"abs.link":    "/x/../c.txt",
	"absup.link":  "/x/../../c.txt",
	"absout.link": "/../{base}2/c.txt",
	"sib.link":    "{root}2/c.txt",
	"el.link":     "b/../c.txt",
	"el8.link":    "k/../c.txt",
	"nd.link":     "top.txt/../c.txt",
	"self.link":   "self.link/../c.txt",
	"nt.link":     strings.Repeat("s/../", 130) + "x/../c.txt",
	"ntc.link":    strings.Repeat("s/../", 130) + "c.txt",
	"ndd.link":    "top.txt/../d/f.txt",
	"nd2.link":    "top.txt/../alias",
	"up.link":     "x/../../c.txt",
	"out.link":    "x/../../../{base}2/c.txt",
	"above.link":  "{parent}",
}

func makeDotDotLinks(t *testing.T, root string) {
	t.Helper()
	for name, dest := range dotDotLinks {
		if strings.HasPrefix(dest, "/") {
			dest = root + dest
		}
		dest = strings.NewReplacer(
			"{root}", root, "{base}", filepath.Base(root), "{parent}", filepath.Dir(root),
		).Replace(dest)
		must(t, os.Symlink(native(dest), filepath.Join(root, name)))
	}
}

// A ".." in a link's destination means whatever walking it finds, which is
// what the kernel does and what resolveLinks says it does. Until 2026-10-06 two
// branches cleaned it as text instead: an absolute final link, through
// filepath.Rel, and the fallback for a path os.Root cannot walk. Either named a
// file the kernel does not reach, so a modify, an append or a delete of the
// link acted on c.txt where every other reader sees sub/c.txt, or nothing at
// all. A third, the text check on a final relative link, refused links that
// stay inside once walked.
//
// The whole suite passed against the fix unchanged, so nothing pinned the old
// answers and nothing would have noticed them coming back. These rows do.
func TestALinkLeadsWhereTheKernelWalksIt(t *testing.T) {
	root := mktree(t)
	dotDotTree(t, root)
	makeDotDotLinks(t, root)

	const (
		loop    = "too many levels of symbolic links"
		notDir  = "not a directory"
		tooLong = "file name too long"
		cannot  = "cannot be followed"
		outOfIt = "it is a symlink out of the root"
	)
	// A want with no reason resolves, to name: {root} is the root, {parent}
	// the directory above it.
	type want struct{ name, reason, why string }
	// Where a row's answer holds. A row that pins a refusal of a walk os.Root
	// cannot make, or that rests on a ".." being walked rather than cleaned as
	// text, holds on POSIX: os.Root is not the same code on Windows, and the
	// first CI run there walked every one of them. See dotDotIsWalked.
	const (
		anywhere = iota
		onPOSIX
	)
	for _, c := range []struct {
		name, link string
		confined   want
		unconfined want
		holds      int
	}{
		{
			"an absolute link with a .. after a directory link", "abs.link",
			want{name: "sub/c.txt"},
			want{name: "{root}/sub/c.txt"},
			onPOSIX,
		},
		{
			"an absolute link whose text climbs out and whose walk stays in", "absup.link",
			want{name: "c.txt"},
			want{name: "{root}/c.txt"},
			onPOSIX,
		},
		{
			"an absolute link that climbs out and into a sibling", "absout.link",
			want{reason: outOfIt},
			want{name: "{root}2/c.txt"},
			anywhere,
		},
		{
			"an absolute link to a sibling whose name extends the root's", "sib.link",
			want{reason: outOfIt},
			want{name: "{root}2/c.txt"},
			anywhere,
		},
		{
			"a .. after nine directory links, one more than os.Root follows", "el.link",
			want{reason: cannot, why: loop},
			want{name: "{root}/sub/c.txt"},
			onPOSIX,
		},
		{
			"a .. after eight directory links", "el8.link",
			want{name: "sub/c.txt"},
			want{name: "{root}/sub/c.txt"},
			onPOSIX,
		},
		{
			"a .. after a file", "nd.link",
			want{reason: cannot, why: notDir},
			want{reason: cannot, why: notDir},
			onPOSIX,
		},
		{
			"a .. after the link itself, which loops", "self.link",
			want{reason: cannot, why: loop},
			want{reason: cannot, why: loop},
			onPOSIX,
		},
		{
			"a .. past more steps than os.Root takes", "nt.link",
			want{reason: cannot, why: tooLong},
			want{name: "{root}/sub/c.txt"},
			onPOSIX,
		},
		{
			"the same, where the text and the walk agree", "ntc.link",
			want{reason: cannot, why: tooLong},
			want{name: "{root}/c.txt"},
			onPOSIX,
		},
		{
			"a .. after a file, then a directory link", "ndd.link",
			want{reason: cannot, why: notDir},
			want{reason: cannot, why: notDir},
			onPOSIX,
		},
		{
			"a .. after a file, then a link to a file", "nd2.link",
			want{reason: cannot, why: notDir},
			want{reason: cannot, why: notDir},
			onPOSIX,
		},
		{
			"a relative link whose text climbs out and whose walk stays in", "up.link",
			want{name: "c.txt"},
			want{name: "{root}/c.txt"},
			onPOSIX,
		},
		{
			"a relative link that climbs out through a directory link", "out.link",
			want{reason: outOfIt},
			want{name: "{root}2/c.txt"},
			anywhere,
		},
		{
			"an absolute link to a directory above the root", "above.link",
			want{reason: outOfIt},
			want{name: "{parent}"},
			anywhere,
		},
	} {
		for _, mode := range []struct {
			name     string
			unconfin bool
		}{{"confined", false}, {"unconfined", true}} {
			t.Run(mode.name+", "+c.name, func(t *testing.T) {
				w := c.confined
				if mode.unconfin {
					w = c.unconfined
				}
				if !dotDotIsWalked() && c.holds == onPOSIX {
					t.Skip("this platform may clean a .. as text before anything walks it; see dotDotIsWalked")
				}
				tree, err := OpenTree(root, mode.unconfin)
				must(t, err)
				defer tree.Close()
				tg, err := tree.Resolve(c.link)
				if w.reason == "" {
					if err != nil {
						t.Fatalf("Resolve(%q): %v", c.link, err)
					}
					named := strings.NewReplacer("{root}", root, "{parent}", filepath.Dir(root)).Replace(w.name)
					if want := native(named); tg.name != want {
						t.Errorf("name = %q, want %q", tg.name, want)
					}
					// And it is the file the kernel reaches through the link,
					// where the kernel walks a .. too.
					if dotDotIsWalked() {
						at := filepath.Join(root, tg.name)
						if mode.unconfin {
							at = tg.name
						}
						got, err := os.Stat(at)
						must(t, err)
						kernel, err := os.Stat(filepath.Join(root, c.link))
						must(t, err)
						if !os.SameFile(got, kernel) {
							t.Errorf("%s is not the file the kernel reaches through %s", tg.name, c.link)
						}
					}
					return
				}
				var pr *PathRefusal
				if !errors.As(err, &pr) {
					t.Fatalf("Resolve(%q) = %q, %v; want a *PathRefusal saying %q", c.link, tg.name, err, w.reason)
				}
				if pr.Path != c.link {
					t.Errorf("Path = %q, want the link as written, %q", pr.Path, c.link)
				}
				for _, s := range []string{w.reason, w.why, tree.Root()} {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("%q does not say %q", err.Error(), s)
					}
				}
				if w.reason == cannot && !strings.Contains(pr.Reason, c.link+" -> ") {
					t.Errorf("%q does not name the link it could not follow", pr.Reason)
				}
			})
		}
	}
}

// A link Lstat finds and Readlink cannot read is refused, naming the link and
// the system's reason, in both modes.
func TestALinkThatCannotBeReadIsRefused(t *testing.T) {
	root := mktree(t)
	if !unreadableLink(t, filepath.Join(root, "shut.link")) {
		t.Skip("this platform keeps no mode on a symlink to stop readlink; see unreadableLink")
	}
	for _, unconfin := range []bool{false, true} {
		t.Run(fmt.Sprintf("unconfined=%v", unconfin), func(t *testing.T) {
			tree, err := OpenTree(root, unconfin)
			must(t, err)
			defer tree.Close()
			_, err = tree.Resolve("shut.link")
			var pr *PathRefusal
			if !errors.As(err, &pr) {
				t.Fatalf("Resolve = %v, want a *PathRefusal", err)
			}
			for _, s := range []string{"the symlink could not be read", "permission denied", "shut.link", tree.Root()} {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("%q does not say %q", err.Error(), s)
				}
			}
		})
	}
}

// The same links through a whole batch, confined, with the tree read
// afterwards: a refusal is exit 2 with nothing written, and a link that
// resolves edits the file the kernel reaches, and only that file.
func TestABatchThroughALinkEditsTheFileTheKernelReaches(t *testing.T) {
	// Every row is one TestALinkLeadsWhereTheKernelWalksIt holds on POSIX only:
	// a refusal of a walk os.Root on Windows makes, or a ".." Windows may clean.
	if !dotDotIsWalked() {
		t.Skip("os.Root on Windows walks what these rows pin as refused; see dotDotIsWalked")
	}
	for _, c := range []struct {
		name, patch string
		changed     string // "": refused, and the tree is unchanged
		file, body  string
	}{
		{name: "a delete through a link that loops", patch: "@@ delete self.link\n"},
		{name: "a delete through a .. after a file", patch: "@@ delete nd.link\n"},
		{
			name:  "a modify past nine directory links",
			patch: "@@ file el.link\n@@ old\nlexical\n@@ new\nEDIT\n",
		},
		{name: "an append past nine directory links", patch: "@@ append el.link\nmore\n@@ end\n"},
		{
			name:  "a modify past more steps than os.Root takes",
			patch: "@@ file nt.link\n@@ old\nlexical\n@@ new\nEDIT\n",
		},
		{
			// It used to replace alias, a link, with a regular file (§6.5).
			name:  "a modify through a .. after a file, to a link",
			patch: "@@ file nd2.link\n@@ old\nlexical\n@@ new\nEDIT\n",
		},
		{
			// It used to load sub/f.txt under two names and lose the first edit.
			name: "two names for one file, one through a .. after a file",
			patch: "@@ file ndd.link\n@@ old\nf\n@@ new\nONE\n" +
				"@@ file sub/f.txt\n@@ old\nf\n@@ new\nf\nTWO\n",
		},
		{
			name:    "a modify through an absolute link with a .. after a directory link",
			patch:   "@@ file abs.link\n@@ old\nphysical\n@@ new\nEDIT\n",
			changed: "sub/c.txt changed", file: "sub/c.txt", body: "EDIT\n",
		},
		{
			name:    "a modify through a relative link whose text climbs out",
			patch:   "@@ file up.link\n@@ old\nlexical\n@@ new\nEDIT\n",
			changed: "c.txt changed", file: "c.txt", body: "EDIT\n",
		},
		{
			name:    "a modify through an absolute link whose text climbs out",
			patch:   "@@ file absup.link\n@@ old\nlexical\n@@ new\nEDIT\n",
			changed: "c.txt changed", file: "c.txt", body: "EDIT\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, nil)
			dotDotTree(t, root)
			makeDotDotLinks(t, root)
			before := snapshot(t, root)
			_, err := run(t, tree, c.patch, Options{})
			if c.changed == "" {
				if code := ExitCode(err); code != exitNoMatch {
					t.Errorf("exit %d (%v), want %d", code, err, exitNoMatch)
				}
				var pr *PathRefusal
				if !errors.As(err, &pr) || !strings.Contains(pr.Reason, "cannot be followed") {
					t.Errorf("err = %v, want the refusal of a .. it cannot walk", err)
				}
				assertUnchanged(t, root, before)
				return
			}
			must(t, err)
			if got := snapshotDiff(before, snapshot(t, root)); len(got) != 1 || got[0] != c.changed {
				t.Errorf("the tree changed in %q, want only %q", got, c.changed)
			}
			if got := readFile(t, root, c.file); got != c.body {
				t.Errorf("%s = %q, want %q", c.file, got, c.body)
			}
		})
	}
}

// The refusal is new wording, so it is a contract (§8.1), in both renderings.
// The root is substituted as in TestAPathRefusalIsGolden, and so are the
// separators, since the link's destination is printed as Readlink gave it.
func TestALinkThatCannotBeFollowedIsGolden(t *testing.T) {
	if !dotDotIsWalked() {
		t.Skip("os.Root on Windows walks self.link's .. rather than refusing it; see dotDotIsWalked")
	}
	root := cliTree(t, map[string]string{"c.txt": "lexical\n"})
	must(t, os.Symlink(native("self.link/../c.txt"), filepath.Join(root, "self.link")))
	before := snapshot(t, root)

	code, out, errOut := runCLI(t, root, nil, "@@ delete self.link\n")
	if code != exitNoMatch {
		t.Fatalf("exit %d, want %d: %s", code, exitNoMatch, errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want the refusal on stderr and nothing else", out)
	}
	golden(t, "cli-path-refused-link-cannot-be-followed", slashPaths(strings.ReplaceAll(errOut, root, "/the/root")))

	code, out, errOut = runCLI(t, root, []string{"--json"}, "@@ delete self.link\n")
	if code != exitNoMatch || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want %d and nothing on stderr", code, errOut, exitNoMatch)
	}
	// In JSON a backslash is escaped, so the root is found in its escaped form
	// and an escaped separator folds to a slash.
	escaped, err := json.Marshal(root)
	must(t, err)
	out = strings.ReplaceAll(out, strings.Trim(string(escaped), `"`), "/the/root")
	golden(t, "cli-path-refused-link-cannot-be-followed-json", strings.ReplaceAll(out, `\\`, "/"))
	assertUnchanged(t, root, before)
}

// asksForADirectory reports whether a destination can only lead to a
// directory, by its text: it ends in a separator or a ".", or is empty.
func asksForADirectory(dest string) bool {
	return dest == "" || dest == "." || strings.HasSuffix(dest, "/") ||
		strings.HasSuffix(dest, string(filepath.Separator)) || strings.HasSuffix(dest, "/.")
}

// A link's destination is the half of a path the repository writes and the
// patch does not, so it is fuzzed here with the patch's half fixed. The
// property is the one resolveLinks claims and nothing asserted until
// 2026-10-06: it walks a path as the kernel does. Where Resolve answers, its
// name is the file the kernel reaches through the link, or is as absent as the
// kernel finds it. A refusal is always allowed, since refusing where the
// kernel would succeed is what not having a second walker costs; a refusal
// that is not a PathRefusal is not.
func FuzzALinkLeadsWhereTheKernelWalks(f *testing.F) {
	root := f.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		f.Fatal(err)
	}
	dotDotTree(f, root)
	// In name order, so a seed's number names the same link on every run.
	for _, name := range slices.Sorted(maps.Keys(dotDotLinks)) {
		dest := dotDotLinks[name]
		if strings.HasPrefix(dest, "/") {
			dest = root + dest
		}
		f.Add(strings.NewReplacer(
			"{root}", root, "{base}", filepath.Base(root), "{parent}", filepath.Dir(root),
		).Replace(dest))
	}
	for _, dest := range []string{
		"fz.link", "fz.link/../c.txt", "x/..", "x/../..", "d/../top.txt", "c.txt/", "sub/./c.txt",
		"alias/../c.txt", "missing/../c.txt", "../" + filepath.Base(root) + "/c.txt", "/", "",
		// Directories, for the link in a parent: inside and absolute, which
		// os.Root refuses and the walk translates, and out of the root.
		"sub", "x", "../" + filepath.Base(root) + "/sub", root, root + "/sub", root + "/x/..",
		root + "/sub/deep/../", root + "/../" + filepath.Base(root) + "/sub", filepath.Dir(root),
	} {
		f.Add(dest)
	}
	var trees []*Tree
	for _, unconfin := range []bool{false, true} {
		tree, err := OpenTree(root, unconfin)
		if err != nil {
			f.Fatal(err)
		}
		f.Cleanup(func() { tree.Close() })
		trees = append(trees, tree)
	}
	link := filepath.Join(root, "fz.link")

	f.Fuzz(func(t *testing.T, dest string) {
		_ = os.Remove(link)
		if os.Symlink(dest, link) != nil {
			return // a destination no filesystem stores, such as one with a NUL
		}
		// The link standing last, and standing in a parent, which until
		// 2026-10-07 os.Root's refusal of an absolute destination ended
		// before the walk reached it.
		for _, spelled := range []string{"fz.link", "fz.link/c.txt"} {
			for _, tree := range trees {
				tg, err := tree.Resolve(spelled)
				if err != nil {
					var pr *PathRefusal
					if !errors.As(err, &pr) {
						t.Fatalf("%s, fz.link -> %q: a %T, not a refusal: %v", spelled, dest, err, err)
					}
					continue
				}
				if !dotDotIsWalked() {
					continue
				}
				at := tg.name
				if !filepath.IsAbs(at) {
					at = filepath.Join(root, at)
				}
				kernel, kErr := os.Stat(filepath.Join(root, native(spelled)))
				got, gErr := os.Stat(at)
				// Two shapes this property found on its first run that hold no
				// "..", and are left for their own card rather than given a new
				// refusal here: a destination ending in a separator or a ".",
				// which the kernel follows only to a directory, and an empty one,
				// which macOS stores and nothing follows. Resolve names the file
				// the text names, or the root. Skipped by shape, so the day they
				// are refused this is the line to delete.
				if kErr != nil && asksForADirectory(dest) {
					continue
				}
				switch {
				case kErr == nil && gErr != nil:
					t.Fatalf("%s, fz.link -> %q: the kernel reaches a file, and Resolve named %q, where there is none: %v",
						spelled, dest, tg.name, gErr)
				case kErr == nil && !os.SameFile(kernel, got):
					t.Fatalf("%s, fz.link -> %q: Resolve named %q, which is not the file the kernel reaches", spelled, dest, tg.name)
				case kErr != nil && gErr == nil:
					t.Fatalf("%s, fz.link -> %q: the kernel reaches nothing (%v), and Resolve named %q, which exists",
						spelled, dest, kErr, tg.name)
				}
			}
		}
	})
}
