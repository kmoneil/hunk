package main

// --isolated-link's tests (#140). The issue: under --isolated the copy's ".."
// is the temporary directory, so a command that reads a sibling of the tree by
// a relative path, a Zig path dependency on ../typo.zig-0.1.0, found nothing
// and failed on the tree as it is. The live rows drive the real CLI against a
// real tree with real siblings, and the command is the probe: it reads what it
// would beside the tree, and says where it ran.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// linkFixture is a tree, app, whose parent and grandparent hold what a command
// reads beside it, the shape of a repository with a sibling checkout:
//
//	top/shared/x.txt
//	top/mid/lib/data.txt, lib/sub/s.txt, VERSION
//	top/mid/app/a.txt          the root
//
// It returns the root and top, so that a test can snapshot everything around
// the root as well as the root.
func linkFixture(t *testing.T) (root, top string) {
	t.Helper()
	top = cliTree(t, map[string]string{
		"shared/x.txt":      "shared\n",
		"mid/lib/data.txt":  "from lib\n",
		"mid/lib/sub/s.txt": "sub\n",
		"mid/VERSION":       "1.2\n",
		"mid/app/a.txt":     "one\n",
	})
	return filepath.Join(top, "mid", "app"), top
}

const linkPatch = "@@ file a.txt\n@@ old\none\n@@ new\nONE\n"

// A path --isolated-link names is beside the copy where the command climbs to
// it, and is the real one; what is not named is not there; the root's own
// name, climbed to and back, is the copy, which holds the patch. Nothing
// around the tree is changed, the links are removed with the copy, and what
// they point at is not.
func TestAnIsolatedLinkIsBesideTheCopy(t *testing.T) {
	needsSymlinks(t)
	for _, c := range []struct {
		name    string
		links   []string
		command string
		saw     string // what the command printed
		nest    string // the copy, under the directory MkdirTemp made
	}{
		{"a sibling directory", []string{"../lib"}, "cat ../lib/data.txt", "from lib\n", "app"},
		{"a sibling file", []string{"../VERSION"}, "cat ../VERSION", "1.2\n", "app"},
		{
			"inside a sibling, and nothing else of it",
			[]string{"../lib/sub"},
			"cat ../lib/sub/s.txt; test -e ../lib/data.txt || echo no data.txt", "sub\nno data.txt\n", "app",
		},
		{
			"two levels up, beside one level up",
			[]string{"../lib", "../../shared"},
			"cat ../lib/data.txt ../../shared/x.txt", "from lib\nshared\n", "mid/app",
		},
		{
			"the deeper first",
			[]string{"../../shared", "../lib"},
			"cat ../../shared/x.txt ../lib/data.txt", "shared\nfrom lib\n", "mid/app",
		},
		{"spelled with a trailing slash", []string{"../lib/"}, "cat ../lib/data.txt", "from lib\n", "app"},
		{"spelled the long way round", []string{"./x/../../lib"}, "cat ../lib/data.txt", "from lib\n", "app"},
		{"the root's own name is the copy", []string{"../lib"}, "cat ../app/a.txt", "ONE\n", "app"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, top := linkFixture(t)
			tmp := scratchTemp(t)
			before := snapshot(t, top)
			log, where := pidFile(t, "log"), pidFile(t, "where")
			args := []string{"--isolated"}
			for _, l := range c.links {
				args = append(args, "--isolated-link", l)
			}
			command := fmt.Sprintf("{ %s; } > %s; pwd -P > %s", c.command, sh(log), sh(where))
			code, out, errOut := runCLI(t, root, append(args, "--try", command), linkPatch)
			if code != exitOK {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			path, rest := afterCopied(t, tmp, errOut)
			if rest != "" || !strings.HasPrefix(out, "tried 1 hunk in a copy of the tree; the command exited 0 (") {
				t.Errorf("stdout %q\nstderr after the copy line %q", out, rest)
			}
			if got, err := os.ReadFile(filepath.FromSlash(log)); err != nil || string(got) != c.saw {
				t.Errorf("the command saw %q (%v), want %q", got, err, c.saw)
			}
			if runtime.GOOS != "windows" {
				ran := logLines(t, where)[0]
				made := filepath.Dir(path)
				for range strings.Count(c.nest, "/") {
					made = filepath.Dir(made)
				}
				if ran != path || filepath.Dir(made) != tmp || path != filepath.Join(made, filepath.FromSlash(c.nest)) {
					t.Errorf("the command ran in %s and the line named %s; want both to be the copy, %s under the directory made", ran, path, c.nest)
				}
			}
			assertUnchanged(t, top, before)
			assertNoCopyLeft(t, tmp)
		})
	}
}

// A root given through a link is run in resolved, so its ".." is the resolved
// root's parent, and that is where a PATH climbs to: the link's own parent
// holds a lib of its own, which the command would never have read.
func TestAnIsolatedLinkClimbsFromTheRootResolved(t *testing.T) {
	needsSymlinks(t)
	root, top := linkFixture(t)
	elsewhere := cliTree(t, map[string]string{"lib/data.txt": "the link's neighbour\n"})
	alias := filepath.Join(elsewhere, "alias")
	must(t, os.Symlink(root, alias))
	tmp := scratchTemp(t)
	before := snapshot(t, top)
	log := pidFile(t, "log")
	code, out, errOut := runCLI(t, alias, []string{
		"--isolated", "--isolated-link", "../lib", "--try", "cat ../lib/data.txt > " + sh(log),
	}, linkPatch)
	if code != exitOK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if got := logLines(t, log); !slices.Equal(got, []string{"from lib"}) {
		t.Errorf("the command read %q, want the resolved root's sibling", got)
	}
	assertUnchanged(t, top, before)
	assertNoCopyLeft(t, tmp)
}

// --each takes the links as one --try does: the issue's own shape, a break set
// whose command reads a sibling, goes through with the link and is refused
// without it, the refusal saying why (below).
func TestAnIsolatedEachReadsALinkedSibling(t *testing.T) {
	needsSymlinks(t)
	root, top := linkFixture(t)
	patch := writePatch(t, t.TempDir(), "flip.hunk", linkPatch)
	tmp := scratchTemp(t)
	before := snapshot(t, top)
	log := pidFile(t, "log")
	code, out, errOut := runCLI(t, root, []string{
		"--isolated", "--isolated-link", "../lib", "--build", "test -f ../lib/data.txt",
		"--try", "cat ../lib/data.txt >> " + sh(log) + " && grep -q one a.txt", "--each", patch,
	}, "")
	// The command reads the sibling on the tree as it is, with the patch,
	// which it catches, and after it.
	if code != exitOK || !strings.HasPrefix(out, "tried 1 patch in a copy of the tree; every command exited nonzero (") {
		t.Errorf("exit %d\n%s%s", code, out, errOut)
	}
	if _, rest := afterCopied(t, tmp, errOut); !strings.HasPrefix(rest, "tried "+patch+"  the command exited 1 (") {
		t.Errorf("stderr after the copy line %q", rest)
	}
	if got := logLines(t, log); !slices.Equal(got, []string{"from lib", "from lib", "from lib"}) {
		t.Errorf("the command read %q, want the sibling three times", got)
	}
	assertUnchanged(t, top, before)
	assertNoCopyLeft(t, tmp)
}

// The refusal by a run on the tree as it is says, under --isolated, that the
// run was in a copy, whose ".." is not the tree's: the issue took a rerun to
// see why. The build's refusal and the command's both; and not without
// --isolated, where it would be false.
func TestARefusalInACopySaysItWasInACopy(t *testing.T) {
	const clause = "; it ran in a copy of the tree, where a relative path out of --root, such as ../lib, finds nothing unless --isolated-link names it\n"
	for _, c := range []struct {
		name     string
		args     []string
		says     string
		isolated bool
	}{
		{
			"the command, in a copy",
			[]string{"--isolated", "--try", "cat ../lib/data.txt"},
			"hunk: the command exited 1 with no patch applied, so every patch would read as caught; nothing was tried" + clause, true,
		},
		{
			"the build, in a copy",
			[]string{"--isolated", "--build", "test -f ../lib/data.txt", "--try", "true"},
			"hunk: the build exited 1 with no patch applied, so every patch would read as not built; nothing was tried" + clause, true,
		},
		{
			"the command, timed out in a copy",
			[]string{"--isolated", "--timeout", "1ms", "--try", "sleep 5"},
			"hunk: the command timed out after 1ms with no patch applied, so every patch would read as timed out; nothing was tried" + clause, true,
		},
		{
			"the command, in the tree",
			[]string{"--try", "cat ../nothing/data.txt"},
			"hunk: the command exited 1 with no patch applied, so every patch would read as caught; nothing was tried\n", false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, top := linkFixture(t)
			patch := writePatch(t, t.TempDir(), "flip.hunk", linkPatch)
			tmp := scratchTemp(t)
			before := snapshot(t, top)
			code, out, errOut := runCLI(t, root, append(c.args, "--each", patch), "")
			rest := errOut
			if c.isolated {
				_, rest = afterCopied(t, tmp, errOut)
			}
			if code != exitUsage || out != "" || !strings.HasPrefix(rest, c.says) {
				t.Errorf("exit %d\nstdout %q\nstderr %q\nwant stderr to start %q", code, out, rest, c.says)
			}
			assertUnchanged(t, top, before)
			assertNoCopyLeft(t, tmp)
		})
	}
}

// The words, rendered from a constructed outcome, in text and in --json.
// §8.1: a reworded line is a contract change.
func TestARefusalInACopyReads(t *testing.T) {
	c := &Verify{
		Ran: true, Try: true, Command: "zig build test", Status: 1, Seconds: 0.4,
		TotalLines: 1, Tail: []string{`error: failed to open "../typo.zig-0.1.0": FileNotFound`},
	}
	cfg := eachConfig{command: c.Command, inCopy: true}
	patches := []*eachPatch{{name: "breaks/01-shadow-reach.hunk"}, {name: "breaks/02-texture.hunk"}}
	var out, errOut bytes.Buffer
	code := eachBaselineRefused(cfg, patches, eachBaselines{command: c}, c, cfg.inACopy(baselineFailure("the command", c, "caught")), exitUsage, &out, &errOut)
	if code != exitUsage || out.Len() > 0 {
		t.Errorf("exit %d, stdout %q", code, out.String())
	}
	golden(t, "report-each-isolated-baseline", errOut.String())

	out.Reset()
	cfg.asJSON = true
	eachBaselineRefused(cfg, patches, eachBaselines{command: c}, c, cfg.inACopy(baselineFailure("the command", c, "caught")), exitUsage, &out, &errOut)
	var r struct {
		Error    string `json:"error"`
		Isolated bool   `json:"isolated"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || !r.Isolated || !strings.HasSuffix(r.Error, "finds nothing unless --isolated-link names it") {
		t.Errorf("--json: %v\n%s", err, out.String())
	}
}

// A path --isolated-link cannot link is refused, exit 1, before anything is
// read: nothing copied, nothing run, nothing around the tree changed. Every
// refusal by its words, since a wrong refusal and a missing one are both
// defects.
func TestAnIsolatedLinkIsRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, root, top string)
		args  []string
		say   func(root, top string) string
	}{
		{
			"without --isolated", nil,
			[]string{"--isolated-link", "../lib", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link links a path beside the copy --isolated makes, and does nothing without --isolated"
			},
		},
		{
			"an empty path", nil,
			[]string{"--isolated", "--isolated-link", "", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link was given an empty path; if it came from a variable, that variable is empty"
			},
		},
		{
			"an absolute path", nil,
			[]string{"--isolated", "--isolated-link", "ABS", "--try", "RUN"},
			func(_, top string) string {
				return "--isolated-link " + filepath.Join(top, "mid", "lib") +
					" is absolute, and the command reaches it by that name from the copy too; it takes a path out of --root, such as ../lib"
			},
		},
		{
			"a path in the tree", nil,
			[]string{"--isolated", "--isolated-link", "a.txt", "--try", "RUN"},
			func(_, _ string) string { return "--isolated-link a.txt is in the tree, and so in the copy already" },
		},
		{
			"a path in the tree that is not there", nil,
			[]string{"--isolated", "--isolated-link", "nothing.txt", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link nothing.txt is in the tree, and so in the copy already"
			},
		},
		{
			"a path that climbs out and back in", nil,
			[]string{"--isolated", "--isolated-link", "../app/a.txt", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link ../app/a.txt is in the tree, and so in the copy already"
			},
		},
		{
			"the tree", nil,
			[]string{"--isolated", "--isolated-link", "../app", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link ../app is the tree itself, whose place the copy takes"
			},
		},
		{
			"the tree, through a link", func(t *testing.T, root, top string) {
				needsSymlinks(t)
				must(t, os.Symlink(root, filepath.Join(top, "mid", "alias")))
			},
			[]string{"--isolated", "--isolated-link", "../alias", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link ../alias is the tree itself, whose place the copy takes"
			},
		},
		{
			"in the tree, through a link", func(t *testing.T, root, top string) {
				needsSymlinks(t)
				must(t, os.Symlink(root, filepath.Join(top, "mid", "alias")))
			},
			[]string{"--isolated", "--isolated-link", "../alias/a.txt", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link ../alias/a.txt is in the tree, and so in the copy already"
			},
		},
		{
			"the parent", nil,
			[]string{"--isolated", "--isolated-link", "..", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link .. holds the tree, and a link to it would reach the tree itself, not the copy"
			},
		},
		{
			"the grandparent", nil,
			[]string{"--isolated", "--isolated-link", "../..", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link ../.. holds the tree, and a link to it would reach the tree itself, not the copy"
			},
		},
		{
			"what holds the tree, through a link", func(t *testing.T, root, top string) {
				needsSymlinks(t)
				must(t, os.Symlink(top, filepath.Join(top, "mid", "up")))
			},
			[]string{"--isolated", "--isolated-link", "../up", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link ../up holds the tree, and a link to it would reach the tree itself, not the copy"
			},
		},
		{
			"above the top of the filesystem", nil,
			[]string{"--isolated", "--isolated-link", "CLIMB", "--try", "RUN"},
			func(root, _ string) string {
				return "--isolated-link " + climbAbove(root) + " climbs above the top of the filesystem from " + root
			},
		},
		{
			"nothing there", nil,
			[]string{"--isolated", "--isolated-link", "../nothing", "--try", "RUN"},
			func(_, top string) string {
				return "--isolated-link ../nothing is " + filepath.Join(top, "mid", "nothing") + ", which does not exist"
			},
		},
		{
			"through a file", nil,
			[]string{"--isolated", "--isolated-link", "../VERSION/x", "--try", "RUN"},
			func(_, top string) string {
				return "--isolated-link ../VERSION/x is " + filepath.Join(top, "mid", "VERSION", "x") + ", which does not exist"
			},
		},
		{
			"where it cannot look", func(t *testing.T, _, top string) {
				needsPOSIXPerms(t)
				// Readable, so that the snapshot lists it, and not searchable,
				// so that nothing in it can be looked up.
				locked := filepath.Join(top, "mid", "locked")
				must(t, os.Mkdir(locked, 0o444))
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			},
			[]string{"--isolated", "--isolated-link", "../locked/x", "--try", "RUN"},
			func(_, top string) string {
				return "--isolated-link ../locked/x is " + filepath.Join(top, "mid", "locked", "x") +
					", which cannot be reached: lstat " + filepath.Join(top, "mid", "locked", "x") + ": permission denied"
			},
		},
		{
			"named twice", nil,
			[]string{"--isolated", "--isolated-link", "../lib", "--isolated-link", "../lib", "--try", "RUN"},
			func(_, _ string) string { return "--isolated-link ../lib is listed twice" },
		},
		{
			"one path, two spellings", nil,
			[]string{"--isolated", "--isolated-link", "../lib", "--isolated-link", "../lib/sub/..", "--try", "RUN"},
			func(_, _ string) string { return "--isolated-link ../lib and ../lib/sub/.. are one path" },
		},
		{
			"inside one named before", nil,
			[]string{"--isolated", "--isolated-link", "../lib", "--isolated-link", "../lib/sub", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link ../lib/sub is inside ../lib, which is linked too; name one of them"
			},
		},
		{
			"holding one named before", nil,
			[]string{"--isolated", "--isolated-link", "../lib/sub", "--isolated-link", "../lib", "--try", "RUN"},
			func(_, _ string) string {
				return "--isolated-link ../lib/sub is inside ../lib, which is linked too; name one of them"
			},
		},
		{
			"refused after one it took", nil,
			[]string{"--isolated", "--isolated-link", "../VERSION", "--isolated-link", "../nothing", "--try", "RUN"},
			func(_, top string) string {
				return "--isolated-link ../nothing is " + filepath.Join(top, "mid", "nothing") + ", which does not exist"
			},
		},
		{
			"under --dry-run", nil,
			[]string{"--dry-run", "--isolated", "--isolated-link", "../nothing", "--try", "RUN"},
			func(_, top string) string {
				return "--isolated-link ../nothing is " + filepath.Join(top, "mid", "nothing") + ", which does not exist"
			},
		},
		{
			"under --each", nil,
			[]string{"--isolated", "--isolated-link", "..", "--try", "RUN", "--each", "PATCH"},
			func(_, _ string) string {
				return "--isolated-link .. holds the tree, and a link to it would reach the tree itself, not the copy"
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, top := linkFixture(t)
			if c.setup != nil {
				c.setup(t, root, top)
			}
			mark := pidFile(t, "ran")
			tmp := scratchTemp(t)
			before := snapshot(t, top)
			args := slices.Clone(c.args)
			for i, a := range args {
				switch a {
				case "RUN":
					args[i] = counted(mark, "true")
				case "ABS":
					args[i] = filepath.Join(top, "mid", "lib")
				case "CLIMB":
					args[i] = climbAbove(root)
				case "PATCH":
					args[i] = writePatch(t, t.TempDir(), "flip.hunk", linkPatch)
				}
			}
			code, out, errOut := runCLI(t, root, args, linkPatch)
			want := "hunk: " + c.say(root, top)
			if code != exitUsage || out != "" || !strings.HasPrefix(errOut, want) || !strings.HasSuffix(errOut, "\n") || strings.Count(errOut, "\n") != 1 {
				t.Errorf("exit %d\nstdout %q\nstderr %q\nwant stderr to start %q", code, out, errOut, want)
			}
			if n := markerLines(t, mark); n > 0 {
				t.Errorf("the command ran %d times", n)
			}
			assertUnchanged(t, top, before)
			assertNoCopyLeft(t, tmp)
		})
	}

	t.Run("in --json", func(t *testing.T) {
		root, _ := linkFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--json", "--isolated", "--isolated-link", "..", "--try", "true"}, linkPatch)
		var r struct {
			OK    bool   `json:"ok"`
			Exit  int    `json:"exit"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &r); err != nil || code != exitUsage || r.OK || r.Exit != exitUsage || errOut != "" ||
			r.Error != "--isolated-link .. holds the tree, and a link to it would reach the tree itself, not the copy" {
			t.Errorf("exit %d, %v\nstdout %s\nstderr %q", code, err, out, errOut)
		}
	})
}

// climbAbove is a path that climbs from root past the top of the filesystem,
// one ".." more than root has directories.
func climbAbove(root string) string {
	n := len(splitPath(root[len(filepath.VolumeName(root)):]))
	return strings.Repeat("../", n+1) + "x"
}

// Where the filesystem folds case, as APFS and NTFS do by default, a spelling
// of the tree that differs from the root's only in case is the tree, by
// identity, though not by name. A link to it would have the command read the
// tree, which holds no patch, and every patch would read as missed.
func TestAnIsolatedLinkKnowsTheTreeByIdentity(t *testing.T) {
	root, top := linkFixture(t)
	if fi, err := os.Stat(filepath.Join(top, "mid", "APP")); err != nil || !sameAsRoot(t, root, fi) {
		t.Skip("this filesystem does not fold case, so no other spelling names the tree")
	}
	for _, c := range []struct{ path, say string }{
		{"../APP", "--isolated-link ../APP is the tree itself, whose place the copy takes"},
		{"../APP/a.txt", "--isolated-link ../APP/a.txt is in the tree, and so in the copy already"},
		{"../../MID", "--isolated-link ../../MID holds the tree, and a link to it would reach the tree itself, not the copy"},
	} {
		t.Run(c.path, func(t *testing.T) {
			tmp := scratchTemp(t)
			code, out, errOut := runCLI(t, root, []string{"--isolated", "--isolated-link", c.path, "--try", "true"}, linkPatch)
			if code != exitUsage || out != "" || errOut != "hunk: "+c.say+"\n" {
				t.Errorf("exit %d\nstdout %q\nstderr %q\nwant %q", code, out, errOut, c.say)
			}
			assertNoCopyLeft(t, tmp)
		})
	}
}

// A root that could not be described has no identity to match, as OpenTree
// allows, and is then known by name: the tree, a path in it, and what holds
// it, which is known by the identity of the directories above it, not the
// root's.
func TestReachesByNameWhereTheRootHasNoIdentity(t *testing.T) {
	root, top := linkFixture(t)
	tree := &Tree{root: root, given: root}
	for _, c := range []struct{ path, want string }{
		{root, "is"},
		{filepath.Join(root, "a.txt"), "in"},
		{filepath.Join(top, "mid"), "holds"},
		{top, "holds"},
		{filepath.Join(top, "mid", "lib"), ""},
		{filepath.Join(top, "mid", "gone"), ""},
	} {
		if got := tree.reaches(c.path); got != c.want {
			t.Errorf("reaches(%s) = %q, want %q", c.path, got, c.want)
		}
	}
}

func sameAsRoot(t *testing.T, root string, fi fs.FileInfo) bool {
	t.Helper()
	rfi, err := os.Stat(root)
	must(t, err)
	return os.SameFile(rfi, fi)
}

// A link that cannot be made, as on Windows without the privilege, ends the
// run at exit 5 before the copy, with nothing run and nothing left. One --try
// and --each both.
func TestALinkThatCannotBeMadeEndsTheRun(t *testing.T) {
	refused := errors.New("A required privilege is not held by the client.")
	for _, each := range []bool{false, true} {
		t.Run(fmt.Sprintf("each=%v", each), func(t *testing.T) {
			old := makeLink
			makeLink = func(oldname, newname string) error {
				return &os.LinkError{Op: "symlink", Old: oldname, New: newname, Err: refused}
			}
			t.Cleanup(func() { makeLink = old })
			root, top := linkFixture(t)
			mark := pidFile(t, "ran")
			tmp := scratchTemp(t)
			before := snapshot(t, top)
			args := []string{"--isolated", "--isolated-link", "../VERSION", "--isolated-link", "../lib", "--try", counted(mark, "true")}
			stdin := linkPatch
			if each {
				args, stdin = append(args, "--each", writePatch(t, t.TempDir(), "flip.hunk", linkPatch)), ""
			}
			code, out, errOut := runCLI(t, root, args, stdin)
			want := "hunk: could not copy the tree for --isolated: --isolated-link ../VERSION: symlink " +
				filepath.Join(top, "mid", "VERSION") + " "
			if code != exitIO || !strings.HasPrefix(errOut, want) ||
				!strings.HasSuffix(errOut, ": A required privilege is not held by the client.; nothing was run, and the tree is untouched\n") {
				t.Errorf("exit %d\nstdout %q\nstderr %q\nwant stderr to start %q", code, out, errOut, want)
			}
			if n := markerLines(t, mark); n > 0 {
				t.Errorf("the command ran %d times", n)
			}
			assertUnchanged(t, top, before)
			assertNoCopyLeft(t, tmp)
		})
	}
}

// Removing the copy removes the links and nothing they point at, when the
// command left a directory in the copy that takes a second pass to remove:
// that pass makes every directory it walks writable, and must not walk into
// a link.
func TestRemovingTheCopyLeavesWhatItLinked(t *testing.T) {
	needsSymlinks(t)
	needsPOSIXPerms(t)
	root, top := linkFixture(t)
	lib := filepath.Join(top, "mid", "lib")
	must(t, os.Chmod(filepath.Join(lib, "sub"), 0o555))
	must(t, os.Chmod(lib, 0o555))
	t.Cleanup(func() { _ = os.Chmod(lib, 0o755); _ = os.Chmod(filepath.Join(lib, "sub"), 0o755) })
	tmp := scratchTemp(t)
	before := snapshot(t, top)
	code, out, errOut := runCLI(t, root, []string{
		"--isolated", "--isolated-link", "../lib", "--try", "mkdir -p ro/x && chmod 555 ro && cat ../lib/data.txt",
	}, linkPatch)
	if code != exitOK || strings.Contains(errOut, "warning") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	assertUnchanged(t, top, before)
	for _, d := range []string{lib, filepath.Join(lib, "sub")} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o555 {
			t.Errorf("%s: %v, %v; want it left 0555", d, fi.Mode(), err)
		}
	}
	assertNoCopyLeft(t, tmp)
}

// With the temporary directory under the root and a link beside the copy, the
// directory the copy is made in, not the copy, is what the walk leaves out:
// leaving out only the copy, the walk copies the directory it is in, the copy
// included, into the copy.
func TestANestedCopyIsNotCopiedIntoItself(t *testing.T) {
	needsSymlinks(t)
	root, _ := linkFixture(t)
	must(t, os.MkdirAll(filepath.Join(root, "tmp"), 0o755))
	tmp := filepath.Join(root, "tmp")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, tmp)
	}
	tree, err := OpenTree(root, false)
	must(t, err)
	t.Cleanup(func() { _ = tree.Close() })
	links, err := planLinks(tree, []string{"../lib"})
	must(t, err)
	c, made, remove, err := isolate(tree, links, func() os.Signal { return nil })
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	var got []string
	must(t, filepath.WalkDir(c.Root(), func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(c.Root(), p)
		got = append(got, filepath.ToSlash(rel))
		return err
	}))
	if want := []string{".", "a.txt", "tmp"}; !slices.Equal(got, want) {
		t.Errorf("the copy holds %q, want %q", got, want)
	}
	if filepath.Base(made.Dir) != "app" || made.Files != 1 || c.Root() != made.Dir {
		t.Errorf("made %+v, the copy at %s", made, c.Root())
	}
	if dest, err := os.Readlink(filepath.Join(filepath.Dir(made.Dir), "lib")); err != nil || dest != filepath.Join(filepath.Dir(root), "lib") {
		t.Errorf("the link beside the copy leads to %q, %v", dest, err)
	}
	must(t, closeAndRemove(c, remove))
	assertNoCopyLeft(t, tmp)
}

// planLink is fed whatever a caller typed after --isolated-link. It is total:
// every path is a refusal of its own type or a link that climbs out of the
// root, names somewhere neither in nor holding the root, and is the root
// joined with what was given.
func FuzzPlanLink(f *testing.F) {
	for _, s := range []string{
		"", ".", "..", "../", "../..", "../lib", "../lib/", "../lib/sub", "../app", "../app/a.txt", "a.txt",
		"/etc", "//x", "../../shared", "./../lib", "x/../../lib", "../VERSION/x", "../\x00", "..\\lib",
		"../../../../../../../../../../../../../../../../x", "C:\\x", "../nothing",
	} {
		f.Add(s)
	}
	top := f.TempDir()
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	for name, body := range map[string]string{"shared/x.txt": "s", "mid/lib/data.txt": "d", "mid/lib/sub/s.txt": "s", "mid/VERSION": "v", "mid/app/a.txt": "a"} {
		p := filepath.Join(top, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			f.Fatal(err)
		}
	}
	tree, err := OpenTree(filepath.Join(top, "mid", "app"), false)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = tree.Close() })
	f.Fuzz(func(t *testing.T, g string) {
		l, err := planLink(tree, g)
		var lr *linkRefusal
		switch {
		case err != nil && !errors.As(err, &lr):
			t.Fatalf("planLink(%q): an error not a refusal: %v", g, err)
		case err != nil && !strings.HasPrefix(err.Error(), "--isolated-link "):
			t.Errorf("planLink(%q): %v", g, err)
		case err != nil:
		case l.climbs < 1 || len(l.rest) < 1 || l.given != g || l.target != filepath.Join(tree.root, g):
			t.Errorf("planLink(%q) = %+v", g, l)
		default:
			if _, in := below(tree.root, l.target); in {
				t.Errorf("planLink(%q) took %s, in the tree", g, l.target)
			}
			if _, holds := below(l.target, tree.root); holds {
				t.Errorf("planLink(%q) took %s, which holds the tree", g, l.target)
			}
		}
	})
}
