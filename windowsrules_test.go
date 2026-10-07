package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// missingAbove stops at the top of the volume as well as at the top of the
// tree. Until 2026-10-07 it stopped only at ".", "/" and "", and on Windows
// filepath.Dir("E:\\") is "E:\\", so a create on a volume that is not there
// never stopped. Asked here with an exists that says no to everything, which is
// what a missing volume says, so the stop is tested where "/" always exists.
func TestMissingAboveStopsAtTheTop(t *testing.T) {
	top := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
	never := func(string) bool { return false }
	for _, c := range []struct {
		name   string
		dir    string
		exists func(string) bool
		want   []string
	}{
		{
			"nothing exists, from the top of a volume", filepath.Join(top, "nope", "x"), never,
			[]string{filepath.Join(top, "nope", "x"), filepath.Join(top, "nope")},
		},
		{"nothing exists, relative", filepath.Join("a", "b"), never, []string{filepath.Join("a", "b"), "a"}},
		{"the top of the tree", ".", never, nil},
		{"the top of the volume", top, never, nil},
		{
			"one directory exists", filepath.Join(top, "nope", "x"), func(d string) bool { return d == filepath.Join(top, "nope") },
			[]string{filepath.Join(top, "nope", "x")},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan []string, 1)
			go func() { done <- missingAbove(c.dir, c.exists) }()
			select {
			case got := <-done:
				if !slices.Equal(got, c.want) {
					t.Errorf("missingAbove(%q) = %q, want %q", c.dir, got, c.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("missingAbove(%q) has not stopped after 2s", c.dir)
			}
		})
	}
}

func TestCleanDots(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"x", "..", "c.txt"}, []string{"c.txt"}},
		{[]string{"a", "x", "..", "..", "c.txt"}, []string{"c.txt"}},
		{[]string{"x", "..", "..", "c.txt"}, []string{"..", "c.txt"}},
		{[]string{"..", "..", "c.txt"}, []string{"..", "..", "c.txt"}},
		{[]string{"a", ".", "b"}, []string{"a", "b"}},
		{[]string{"a", ".."}, []string{}},
		{nil, []string{}},
	} {
		if got := cleanDots(c.in); !slices.Equal(got, c.want) {
			t.Errorf("cleanDots(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Windows reads a ".." in a link's destination as text, cancelling the
// component before it without following it, and hunk does the same there since
// 2026-10-07: the windows-latest probe found it editing a file other than the
// one Windows opens in four of these rows, confined. The rule is asked for on
// every platform, so it is tested where the suite runs; on Windows, where it is
// the platform's own, every answer is also checked against the file the kernel
// opens through the link, or its finding nothing.
func TestADotDotInALinkReadsAsTheKernelReadsIt(t *testing.T) {
	root := mktree(t)
	dotDotTree(t, root)
	makeDotDotLinks(t, root)
	// back.link's .. cancels sub, which the walk had passed, and lands on
	// alias, a link to c.txt: the walk has to start again to follow it.
	for _, l := range [][2]string{{"pd.link", "x/.."}, {"pu.link", "x/../.."}, {"pdd.link", "d/../sub"}, {"sub/back.link", "../alias"}} {
		must(t, os.Symlink(native(l[1]), filepath.Join(root, native(l[0]))))
	}
	parent := filepath.Dir(root)
	place := strings.NewReplacer("{root}", root, "{parent}", parent,
		"{grandparent}", filepath.Dir(parent), "{base}", filepath.Base(root))
	const out = "it is a symlink out of the root"
	for _, c := range []struct {
		path                 string
		confined, unconfined string // a name, or a refusal's reason beginning "!"
	}{
		{"el.link", "c.txt", "{root}/c.txt"},
		{"el8.link", "c.txt", "{root}/c.txt"},
		{"nt.link", "c.txt", "{root}/c.txt"},
		{"ntc.link", "c.txt", "{root}/c.txt"},
		{"self.link", "c.txt", "{root}/c.txt"},
		{"nd.link", "c.txt", "{root}/c.txt"},
		{"nd2.link", "c.txt", "{root}/c.txt"},
		{"ndd.link", "sub/f.txt", "{root}/sub/f.txt"},
		{"abs.link", "c.txt", "{root}/c.txt"},
		{"up.link", "!" + out, "{parent}/c.txt"},
		{"out.link", "!" + out, "{grandparent}/{base}2/c.txt"},
		{"pd.link/c.txt", "c.txt", "{root}/c.txt"},
		{"pu.link/c.txt", "!the symlink pu.link -> ", "{parent}/c.txt"},
		{"pdd.link/c.txt", "sub/c.txt", "{root}/sub/c.txt"},
		{"sub/back.link", "c.txt", "{root}/c.txt"},
	} {
		for _, unconfin := range []bool{false, true} {
			want := c.confined
			if unconfin {
				want = c.unconfined
			}
			t.Run(map[bool]string{false: "confined, ", true: "unconfined, "}[unconfin]+c.path, func(t *testing.T) {
				tree, err := OpenTree(root, unconfin)
				must(t, err)
				defer tree.Close()
				native := tree.dotsAsText
				tree.dotsAsText = true
				tg, err := tree.Resolve(c.path)
				if reason, refused := strings.CutPrefix(want, "!"); refused {
					var pr *PathRefusal
					if !errors.As(err, &pr) || !strings.HasPrefix(pr.Reason, reason) {
						t.Errorf("Resolve(%q) = %q, %v; want a refusal beginning %q", c.path, tg.name, err, reason)
					}
					return
				}
				if err != nil {
					t.Fatalf("Resolve(%q): %v", c.path, err)
				}
				if w := filepath.FromSlash(place.Replace(want)); tg.name != w {
					t.Errorf("name = %q, want %q", tg.name, w)
				}
				if !native {
					return // the kernel here walks a .., which is not the rule asked for
				}
				at := tg.name
				if !filepath.IsAbs(at) {
					at = filepath.Join(root, at)
				}
				kernel, kErr := os.Stat(filepath.Join(root, filepath.FromSlash(c.path)))
				got, gErr := os.Stat(at)
				switch {
				case kErr == nil && (gErr != nil || !os.SameFile(kernel, got)):
					t.Errorf("%s is not the file the kernel opens through %s", at, c.path)
				case kErr != nil && gErr == nil:
					t.Errorf("the kernel opens nothing through %s (%v), and %s exists", c.path, kErr, at)
				}
			})
		}
	}
}

// An 8.3 alias is respelled to the name its directory lists. Windows makes
// one by default on its system volume and lists only the long name, so until
// 2026-10-07 the two spellings of one file were two entries in a batch, and the
// windows-latest probe saw the first of two edits lost at exit 0. No other
// platform makes an alias, so stored is asked here as Respell would ask it,
// with a name the directory answers to and does not list; on Windows,
// TestAShortNameIsTheLongName asks the real thing.
func TestStoredFindsAnAliasByIdentity(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a long name.txt"), []byte("x\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "other.txt"), []byte("y\n"), 0o644))
	long, err := os.Lstat(filepath.Join(dir, "a long name.txt"))
	must(t, err)
	// A hard link whose own name has a "~": its name, in any case, is kept,
	// though "a.txt" sorts first and is the same file.
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("z\n"), 0o644))
	must(t, os.Link(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b~1.txt")))
	linked, err := os.Lstat(filepath.Join(dir, "b~1.txt"))
	must(t, err)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.txt")
	must(t, os.WriteFile(elsewhere, []byte("w\n"), 0o644))
	away, err := os.Lstat(elsewhere)
	must(t, err)

	tree, err := OpenTree(dir, true)
	must(t, err)
	defer tree.Close()
	for _, c := range []struct {
		name, c string
		fi      os.FileInfo
		want    string
	}{
		{"an alias the directory does not list", "ALONGN~1.TXT", long, "a long name.txt"},
		{"the same in lower case", "alongn~1.txt", long, "a long name.txt"},
		{"a listed name with a ~ is its own", "b~1.txt", linked, "b~1.txt"},
		{"another case of it is still its own, not its hard link's", "B~1.TXT", linked, "b~1.txt"},
		{"a name with a ~ that is no file listed here stays as written", "NOPE~1.TXT", away, "NOPE~1.TXT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := tree.Spellings()
			dirName, err := tree.toName(dir)
			must(t, err)
			if got := s.stored(dirName, c.c, c.fi); got != c.want {
				t.Errorf("stored(%q) = %q, want %q", c.c, got, c.want)
			}
		})
	}
	// Without a ~, a name the directory does not list is not looked for by
	// identity: on a filesystem that does not fold, it stays as written.
	s := tree.Spellings()
	dirName, err := tree.toName(dir)
	must(t, err)
	if got := s.stored(dirName, "elsewhere.txt", long); got != "elsewhere.txt" {
		t.Errorf("stored(elsewhere.txt) = %q, want it as written", got)
	}
}
