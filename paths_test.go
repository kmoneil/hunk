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
			{"a .. after a link inside a destination, resolved physically", "phys.link", "sub/in.txt", true},
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
		// it reaches, so it is refused.
		t.Run(mode.name+", a .. after a directory that does not exist", func(t *testing.T) {
			_, err := tree.Resolve("nowhere.link")
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

	// The absolute directory link: refused confined, by os.Root and in its words
	// (TestAbsoluteSymlinkInAnIntermediateComponent), and resolved unconfined.
	loose, err := OpenTree(root, true)
	must(t, err)
	t.Cleanup(func() { loose.Close() })
	tg, err := loose.Resolve("absd/in.txt")
	must(t, err)
	if want := filepath.Join(root, "sub", "in.txt"); tg.name != want {
		t.Errorf("unconfined absd/in.txt = %q, want %q", tg.name, want)
	}

	// Confined, a link in a parent that climbs out of the root is refused by
	// os.Root, in its words, before the walk could clamp it at the top.
	confined, err := OpenTree(root, false)
	must(t, err)
	t.Cleanup(func() { confined.Close() })
	_, err = confined.Resolve("up/x")
	var pr *PathRefusal
	if !errors.As(err, &pr) || !strings.Contains(pr.Reason, "it leaves the root") {
		t.Errorf("confined up/x = %v, want os.Root's refusal of an escape", err)
	}

	// Unconfined, a ".." above the top of the path stays at the top, as it does
	// for the kernel. Confined, os.Root refuses the same link as an escape. Where
	// the platform will not walk such a link at all, the path comes back as
	// written, for load to report (see climbingPastTheTopResolves).
	vol := filepath.VolumeName(root)
	climb := filepath.Join(strings.Repeat(".."+string(filepath.Separator), 64), root[len(vol):], "sub")
	must(t, os.Symlink(climb, filepath.Join(root, "climb.link")))
	tg, err = loose.Resolve("climb.link/in.txt")
	must(t, err)
	want := filepath.Join(root, "sub", "in.txt")
	if !climbingPastTheTopResolves() {
		want = filepath.Join(root, "climb.link", "in.txt")
	}
	if tg.name != want {
		t.Errorf("unconfined climb.link/in.txt = %q, want %q", tg.name, want)
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
				for _, want := range []string{"the path contains a NUL byte", tree.Root()} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%q does not say %q", err.Error(), want)
					}
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
		if made[i] != native(want[i]) {
			t.Errorf("made[%d] = %q, want %q", i, made[i], native(want[i]))
		}
	}

	// A directory that already existed is not reported as created, so rollback
	// cannot remove something this tool did not make.
	tg2, err := tree.Resolve("sub/deeper/f.txt")
	must(t, err)
	made2, err := tree.MkdirAll(tg2)
	must(t, err)
	if len(made2) != 1 || made2[0] != native("sub/deeper") {
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
	assertMode(t, fi.Mode(), 0o644)
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
		"d/in.txt", "d/new/x.go", "l/x", "sub/../d/in.txt", "rel.link/x",
		"SUB/IN.TXT", "Sub/New/X.go", "D/in.txt", "\u00e9.txt", "e\u0301.txt",
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
		// The name is the file's one name (§3.5): wherever it can be used at
		// all, as a file that exists or one a create would make, no component
		// of it is still a link. A name that fails for another reason, such as
		// a path through a file, is load's to report and is not checked here.
		if _, err := os.Lstat(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return
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

// An absolute symlink in an *intermediate* component is refused, by os.Root in
// the Lstat resolveLinks makes before it walks, and not translated as a final
// one is. That is a deliberate scope line (§6.5 says "a target that is itself a
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
		t.Fatal("allowed; if os.Root has relaxed, linkDestination can translate this case too")
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
