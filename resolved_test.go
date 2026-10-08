package main

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A report names a file as the patch wrote it, and, where that path led to a
// file under another name, through a symlink or in a spelling its directory
// stores another way, the name the file has: "(it resolves to X)" in text and
// "resolved" in JSON. Decided 2026-10-08, from issue #43. Until then every
// report named the path alone, which is a name git cannot act on: with in.txt
// -> sub/real.txt, "git diff in.txt" is empty and "git checkout in.txt"
// restores nothing, and exit 4 sends its reader to version control by it.

// jsonResolved is what a --json report says about each file and each file
// rollback could not put back: the path, and resolved if the key is there.
type jsonResolved struct {
	Files []struct {
		Path     string  `json:"path"`
		Resolved *string `json:"resolved"`
	} `json:"files"`
	Verify *struct {
		NotRestored []struct {
			Path     string  `json:"path"`
			Resolved *string `json:"resolved"`
		} `json:"not_restored"`
	} `json:"verify"`
}

func decodeResolved(t *testing.T, out string) jsonResolved {
	t.Helper()
	var r jsonResolved
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("--json: %v\n%s", err, out)
	}
	return r
}

// resolvedOrAbsent is a "resolved" key as a row of a table states it: "" for
// a key that must be absent, since omitempty is the promise for a path that
// names its file by its own name.
func resolvedOrAbsent(p *string) string {
	if p == nil {
		return ""
	}
	if *p == "" {
		return "<present and empty>"
	}
	return *p
}

// rows is the success report without its total, which is the part a file
// row's note changes.
func rows(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return strings.Join(lines[:len(lines)-1], "\n")
}

// resolvedTree makes the tree every confined row runs in: two files and the
// links to them a row asks for, each destination as given, with {root}
// standing for the root.
func resolvedTree(t *testing.T, links map[string]string) string {
	t.Helper()
	root := cliTree(t, map[string]string{
		"real.txt": "one\n", "sub/a.txt": "one\n", "sub/real.txt": "one\n", "sub/deep/keep": "",
	})
	for link, dest := range links {
		must(t, os.Symlink(native(strings.ReplaceAll(dest, "{root}", root)), filepath.Join(root, native(link))))
	}
	return root
}

func TestASuccessRowSaysWhereItsPathLed(t *testing.T) {
	const modify = "@@ old\none\n@@ new\ntwo\n"
	viaDots := dotDotAnswer("sub/real.txt", "real.txt")
	for _, c := range []struct {
		name  string
		links map[string]string
		patch string
		flags []string
		want  string   // the rows
		json  []string // each file's "resolved", "" for absent
	}{
		{
			name: "a final link", links: map[string]string{"link.txt": "real.txt"},
			patch: "@@ file link.txt\n" + modify,
			want:  "M link.txt +1 -1  (it resolves to real.txt)", json: []string{"real.txt"},
		},
		{
			name: "a chain of links", links: map[string]string{"link.txt": "real.txt", "chain.txt": "link.txt"},
			patch: "@@ file chain.txt\n" + modify,
			want:  "M chain.txt +1 -1  (it resolves to real.txt)", json: []string{"real.txt"},
		},
		{
			name: "an absolute final link", links: map[string]string{"abs.txt": "{root}/real.txt"},
			patch: "@@ file abs.txt\n" + modify,
			want:  "M abs.txt +1 -1  (it resolves to real.txt)", json: []string{"real.txt"},
		},
		{
			name: "a directory link", links: map[string]string{"d": "sub"},
			patch: "@@ file d/a.txt\n" + modify,
			want:  "M d/a.txt +1 -1  (it resolves to sub/a.txt)", json: []string{"sub/a.txt"},
		},
		{
			name: "a delete through a directory link", links: map[string]string{"d": "sub"},
			patch: "@@ delete d/a.txt\n",
			want:  "D d/a.txt +0 -1  (it resolves to sub/a.txt)", json: []string{"sub/a.txt"},
		},
		{
			name: "an append through a link", links: map[string]string{"link.txt": "real.txt"},
			patch: "@@ append link.txt\ntwo\n",
			want:  "M link.txt +1 -0  (it resolves to real.txt)", json: []string{"real.txt"},
		},
		{
			// The note is about the path, so it comes before the one about
			// the bytes.
			name: "a create through a directory link, before the newline's note", links: map[string]string{"d": "sub"},
			patch: "@@ create d/new.txt\nx\n",
			want:  "A d/new.txt +1 -0  (it resolves to sub/new.txt)  (no final newline)", json: []string{"sub/new.txt"},
		},
		{
			name: "a create through a dangling link", links: map[string]string{"dangling.txt": "missing.txt"},
			patch: "@@ create dangling.txt\nx\n\n",
			want:  "A dangling.txt +1 -0  (it resolves to missing.txt)", json: []string{"missing.txt"},
		},
		{
			name:  "a link whose destination climbs with ..",
			links: map[string]string{"x": "sub/deep", "viadots.txt": "x/../real.txt"},
			patch: "@@ file viadots.txt\n" + modify,
			want:  "M viadots.txt +1 -1  (it resolves to " + viaDots + ")", json: []string{viaDots},
		},
		{
			// The patch named real.txt, and until 2026-10-08 the report did
			// not, nor said that the two hunks were one file.
			name: "the link named first, then the file", links: map[string]string{"link.txt": "real.txt"},
			patch: "@@ file link.txt\n" + modify + "@@ file real.txt\n@@ old\ntwo\n@@ new\nthree\n",
			want:  "M link.txt +2 -2  (it resolves to real.txt)", json: []string{"real.txt"},
		},
		{
			name: "the file named first, then the link", links: map[string]string{"link.txt": "real.txt"},
			patch: "@@ file real.txt\n" + modify + "@@ file link.txt\n@@ old\ntwo\n@@ new\nthree\n",
			want:  "M real.txt +2 -2", json: []string{""},
		},
		{
			name: "the column still lines up", links: map[string]string{"link.txt": "real.txt", "chain.txt": "link.txt"},
			patch: "@@ file chain.txt\n" + modify + "@@ file sub/a.txt\n" + modify,
			want:  "M chain.txt +1 -1  (it resolves to real.txt)\nM sub/a.txt +1 -1", json: []string{"real.txt", ""},
		},
		{
			name: "a dry run says it too", links: map[string]string{"link.txt": "real.txt"},
			patch: "@@ file link.txt\n" + modify, flags: []string{"--dry-run"},
			want: "M link.txt +1 -1  (it resolves to real.txt)", json: []string{"real.txt"},
		},
		// The controls: a path that names its file by the file's own name says
		// nothing new, however it is spelled.
		{name: "a plain path", patch: "@@ file real.txt\n" + modify, want: "M real.txt +1 -1", json: []string{""}},
		{name: "a leading ./", patch: "@@ file ./real.txt\n" + modify, want: "M ./real.txt +1 -1", json: []string{""}},
		{name: "a doubled slash", patch: "@@ file sub//a.txt\n" + modify, want: "M sub//a.txt +1 -1", json: []string{""}},
		{name: "a .. with no link", patch: "@@ file sub/../real.txt\n" + modify, want: "M sub/../real.txt +1 -1", json: []string{""}},
		{
			name: "a .. back out of a directory link", links: map[string]string{"d": "sub"},
			patch: "@@ file d/../real.txt\n" + modify, want: "M d/../real.txt +1 -1", json: []string{""},
		},
		{
			name: "a link the batch does not use", links: map[string]string{"link.txt": "real.txt"},
			patch: "@@ file real.txt\n" + modify, want: "M real.txt +1 -1", json: []string{""},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, resolvedTree(t, c.links), c.flags, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d, want 0: %s", code, errOut)
			}
			if got := rows(out); got != c.want {
				t.Errorf("rows:\n got %q\nwant %q", got, c.want)
			}
			code, out, errOut = runCLI(t, resolvedTree(t, c.links), append([]string{"--json"}, c.flags...), c.patch)
			if code != exitOK {
				t.Fatalf("--json: exit %d, want 0: %s", code, errOut)
			}
			r := decodeResolved(t, out)
			if len(r.Files) != len(c.json) {
				t.Fatalf("--json: %d files, want %d\n%s", len(r.Files), len(c.json), out)
			}
			for i, want := range c.json {
				if got := resolvedOrAbsent(r.Files[i].Resolved); got != want {
					t.Errorf("--json: files[%d].resolved = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// An absolute path names its file by its own name when it is under the root,
// whichever spelling of the root it uses. The root here is reached through a
// link, as /tmp is on macOS, so the path written and the file's name differ in
// the root's part and nowhere else.
func TestAnAbsolutePathUnderTheRootSaysNothingNew(t *testing.T) {
	real := resolvedTree(t, nil)
	alias := filepath.Join(t.TempDir(), "alias")
	must(t, os.Symlink(real, alias))
	for _, c := range []struct{ name, root, path string }{
		{"the root as resolved", real, filepath.Join(real, "real.txt")},
		{"the root through a link, given that way", alias, filepath.Join(alias, "real.txt")},
		{"the root through a link, the path resolved", alias, filepath.Join(real, "real.txt")},
		{"the root resolved, the path through the link", real, filepath.Join(alias, "real.txt")},
	} {
		for _, unconfined := range []bool{false, true} {
			name := c.name
			var flags []string
			if unconfined {
				name, flags = name+", unconfined", []string{"--allow-outside-root"}
			}
			t.Run(name, func(t *testing.T) {
				must(t, os.WriteFile(filepath.Join(real, "real.txt"), []byte("one\n"), 0o644))
				code, out, errOut := runCLI(t, c.root, flags, "@@ file "+c.path+"\n@@ old\none\n@@ new\ntwo\n")
				if code != exitOK {
					t.Fatalf("exit %d, want 0: %s", code, errOut)
				}
				if strings.Contains(out, "resolves to") {
					t.Errorf("a path under the root said where it led:\n%s", out)
				}
			})
		}
	}
}

// Unconfined, a name is shown relative to the root when it is under it, as a
// patch would write it, and absolute when it is not, which is the write the
// review that raised this wanted seen: one that left the root.
func TestAnUnconfinedRowSaysWhereItLed(t *testing.T) {
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	tree := func() {
		must(t, os.RemoveAll(root))
		must(t, os.RemoveAll(outside))
		must(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
		must(t, os.MkdirAll(outside, 0o755))
		mustWrite(t, root, native("sub/real.txt"), "one\n")
		mustWrite(t, outside, "o.txt", "one\n")
		must(t, os.Symlink(native("sub/real.txt"), filepath.Join(root, "in.txt")))
		must(t, os.Symlink(native("../outside/o.txt"), filepath.Join(root, "out.txt")))
	}
	const modify = "@@ old\none\n@@ new\ntwo\n"
	outsideShown := filepath.ToSlash(filepath.Join(outside, "o.txt"))
	for _, c := range []struct{ name, path, want string }{
		{"a link out of the root", "out.txt", "M out.txt +1 -1  (it resolves to " + outsideShown + ")"},
		{"a link inside it", "in.txt", "M in.txt +1 -1  (it resolves to sub/real.txt)"},
		{"a path written outside it", "../outside/o.txt", "M ../outside/o.txt +1 -1"},
		{"a path written inside it", "sub/real.txt", "M sub/real.txt +1 -1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree()
			code, out, errOut := runCLI(t, root, []string{"--allow-outside-root"}, "@@ file "+c.path+"\n"+modify)
			if code != exitOK {
				t.Fatalf("exit %d, want 0: %s", code, errOut)
			}
			if got := rows(out); got != c.want {
				t.Errorf("rows:\n got %q\nwant %q", got, c.want)
			}
		})
	}
	// Respell spells every component of an unconfined name as its directory
	// stores it, the root's own among them, so with --root given in another
	// case every path led somewhere until the root's stored spelling was
	// asked for too.
	t.Run("the root given in another case", func(t *testing.T) {
		if !foldsCase(t) {
			t.Skip("the temporary directory does not fold case")
		}
		tree()
		other := filepath.Join(base, "ROOT")
		code, out, errOut := runCLI(t, other, []string{"--allow-outside-root"}, "@@ file sub/real.txt\n"+modify)
		if code != exitOK {
			t.Fatalf("exit %d, want 0: %s", code, errOut)
		}
		if got, want := rows(out), "M sub/real.txt +1 -1"; got != want {
			t.Errorf("rows:\n got %q\nwant %q", got, want)
		}
	})
}

// A spelling the directory stores another way leads to a file under another
// name too, and git cannot act on it either: with core.ignorecase,
// "git diff sub/S.TXT" is empty for a change to Sub/s.txt.
func TestASuccessRowSaysWhereARespellingLed(t *testing.T) {
	const modify = "@@ old\none\n@@ new\ntwo\n"
	for _, c := range []struct {
		name, patch, want, json string
		folds                   func(testing.TB) bool
	}{
		{"another case", "@@ file sub/S.TXT\n" + modify, "M sub/S.TXT +1 -1  (it resolves to Sub/s.txt)", "Sub/s.txt", foldsCase},
		{"a create under a directory in another case", "@@ create sub/new.txt\nx\n\n", "A sub/new.txt +1 -0  (it resolves to Sub/new.txt)", "Sub/new.txt", foldsCase},
		{"another normalization", "@@ file e\u0301.txt\n" + modify, "M e\u0301.txt +1 -1  (it resolves to \u00e9.txt)", "\u00e9.txt", foldsNormalization},
		// The controls. A new name takes the batch's first spelling, so a
		// second one leads where the first did and the row is the first's.
		{"the stored spelling", "@@ file Sub/s.txt\n" + modify, "M Sub/s.txt +1 -1", "", nil},
		{"a new name, then another case of it", "@@ create Sub/New.txt\nx\n\n@@ append sub/NEW.txt\ny\n", "A Sub/New.txt +2 -0", "", foldsCase},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.folds != nil && !c.folds(t) {
				t.Skip("the temporary directory does not fold this")
			}
			files := map[string]string{"Sub/s.txt": "one\n", "\u00e9.txt": "one\n"}
			code, out, errOut := runCLI(t, cliTree(t, files), nil, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d, want 0: %s", code, errOut)
			}
			if got := rows(out); got != c.want {
				t.Errorf("rows:\n got %q\nwant %q", got, c.want)
			}
			code, out, errOut = runCLI(t, cliTree(t, files), []string{"--json"}, c.patch)
			if code != exitOK {
				t.Fatalf("--json: exit %d, want 0: %s", code, errOut)
			}
			if got := resolvedOrAbsent(decodeResolved(t, out).Files[0].Resolved); got != c.json {
				t.Errorf("--json: resolved = %q, want %q", got, c.json)
			}
		})
	}
}

// Exit 4 tells its reader the original bytes "are in version control", so it
// has to name the file version control knows. Until 2026-10-08 it named the
// link, and "git checkout link.txt" exits 0 having restored nothing.
func TestAFileNotRestoredSaysWhereItsPathLed(t *testing.T) {
	const verify = "echo theirs > real.txt; echo theirs > Sub/s.txt; false"
	for _, c := range []struct {
		name, patch, says, json string
		folds                   func(testing.TB) bool
	}{
		{"through a link", "@@ file link.txt\n@@ old\none\n@@ new\ntwo\n", "link.txt (it resolves to real.txt) was not restored.", "real.txt", nil},
		{"through a respelling", "@@ file sub/S.TXT\n@@ old\none\n@@ new\ntwo\n", "sub/S.TXT (it resolves to Sub/s.txt) was not restored.", "Sub/s.txt", foldsCase},
		{"the control, by its own name", "@@ file real.txt\n@@ old\none\n@@ new\ntwo\n", "\nreal.txt was not restored.", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.folds != nil && !c.folds(t) {
				t.Skip("the temporary directory does not fold case")
			}
			tree := func() string {
				root := cliTree(t, map[string]string{"real.txt": "one\n", "Sub/s.txt": "one\n"})
				must(t, os.Symlink("real.txt", filepath.Join(root, "link.txt")))
				return root
			}
			code, _, errOut := runCLI(t, tree(), []string{"--verify", verify}, c.patch)
			if code != exitRollbackFailed {
				t.Fatalf("exit %d, want 4: %s", code, errOut)
			}
			if !strings.Contains(errOut, c.says) {
				t.Errorf("the report does not say %q:\n%s", c.says, errOut)
			}
			code, out, _ := runCLI(t, tree(), []string{"--verify", verify, "--json"}, c.patch)
			if code != exitRollbackFailed {
				t.Fatalf("--json: exit %d, want 4", code)
			}
			r := decodeResolved(t, out)
			if r.Verify == nil || len(r.Verify.NotRestored) != 1 {
				t.Fatalf("--json: want one not_restored:\n%s", out)
			}
			if got := resolvedOrAbsent(r.Verify.NotRestored[0].Resolved); got != c.json {
				t.Errorf("--json: not_restored[0].resolved = %q, want %q", got, c.json)
			}
			if got := resolvedOrAbsent(r.Files[0].Resolved); got != c.json {
				t.Errorf("--json: files[0].resolved = %q, want %q", got, c.json)
			}
		})
	}
}

// Exit 5's sentence names the file whose write failed, and its list the files
// it could not put back; exit 6's names the file that changed. Each says where
// the path led, in the same words.
func TestACommitThatFailsSaysWhereItsPathLed(t *testing.T) {
	long := strings.Repeat("n", 300)
	root := resolvedTree(t, map[string]string{"d": "sub"})
	code, _, errOut := runCLI(t, root, nil, "@@ create d/newdir/"+long+"\nx\n\n")
	if code != exitIO {
		t.Fatalf("exit %d, want 5: %s", code, errOut)
	}
	want := "creating d/newdir/" + long + " (it resolves to sub/newdir/" + long + ") failed: "
	if !strings.Contains(errOut, want) {
		t.Errorf("the report does not say %q:\n%s", want, errOut)
	}
	// The control: a path by its own name has no clause.
	code, _, errOut = runCLI(t, root, nil, "@@ create sub/newdir/"+long+"\nx\n\n")
	if code != exitIO {
		t.Fatalf("exit %d, want 5: %s", code, errOut)
	}
	if strings.Contains(errOut, "resolves to") {
		t.Errorf("a path by its own name said where it led:\n%s", errOut)
	}

	// The list, rendered from the error, as a commit's rollback fills it.
	rep := NewReport(nil, &CommitError{
		Path: "b.txt", Op: "modify", Err: errors.New("no space left on device"),
		NotRestored: []NotRestored{
			{Path: "link.txt", Resolved: "real.txt", Reason: "Something rewrote it."},
			{Path: "a.txt", Reason: "Something rewrote it."},
		},
	}, nil, false, 2)
	var out, text strings.Builder
	rep.Text(&out, &text, false)
	for _, says := range []string{"\nlink.txt (it resolves to real.txt) was not restored.\n", "\na.txt was not restored.\n"} {
		if !strings.Contains(text.String(), says) {
			t.Errorf("the report does not say %q:\n%s", says, text.String())
		}
	}
	var js strings.Builder
	must(t, rep.JSON(&js))
	var r struct {
		NotRestored []map[string]string `json:"not_restored"`
	}
	must(t, json.Unmarshal([]byte(js.String()), &r))
	if len(r.NotRestored) != 2 || r.NotRestored[0]["resolved"] != "real.txt" {
		t.Errorf("--json: not_restored = %v, want the first resolved to real.txt", r.NotRestored)
	}
	if _, ok := r.NotRestored[1]["resolved"]; ok {
		t.Errorf("--json: not_restored[1] carries resolved for a path by its own name: %v", r.NotRestored[1])
	}
}

func TestAChangedFileSaysWhereItsPathLed(t *testing.T) {
	for _, c := range []struct{ name, path, want string }{
		{"through a link", "link.txt", "link.txt (it resolves to real.txt) changed on disk between being read and being written; nothing was written"},
		{"the control, by its own name", "real.txt", "real.txt changed on disk between being read and being written; nothing was written"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"real.txt": "one\n"})
			must(t, os.Symlink("real.txt", filepath.Join(root, "link.txt")))
			p, err := Parse([]byte("@@ file "+c.path+"\n@@ old\none\n@@ new\ntwo\n"), DefaultMarker)
			must(t, err)
			x := NewTxn(tree, Options{})
			must(t, x.Load(p))
			if f := x.Validate(p); len(f) != 0 {
				t.Fatalf("validate: %+v", f)
			}
			must(t, os.WriteFile(filepath.Join(root, "real.txt"), []byte("theirs\n"), 0o644))
			err = x.Check()
			var ce *ChangedError
			if !errors.As(err, &ce) {
				t.Fatalf("want *ChangedError, got %T: %v", err, err)
			}
			if ce.Error() != c.want {
				t.Errorf("message:\n got %q\nwant %q", ce.Error(), c.want)
			}
		})
	}
}

// A resolved name is the tree's, and a link's destination can put a control
// character in it, which text output shows by name (§6.5, decided
// 2026-10-07). Checking that found two places that had printed such a name
// raw since before this card: the directory a rollback left, and a directory
// a not-restored reason names.
func TestAResolvedNameShowsAControlCharacterByName(t *testing.T) {
	needsPOSIXNames(t)
	const esc = "e\x1bv"
	tree := func(t *testing.T) string {
		root := cliTree(t, map[string]string{esc + ".txt": "one\n", esc + "/a.txt": "one\n"})
		must(t, os.Symlink(esc+".txt", filepath.Join(root, "link.txt")))
		must(t, os.Symlink(esc, filepath.Join(root, "d")))
		return root
	}
	for _, c := range []struct {
		name, patch, verify string
		links               bool
		says                []string
	}{
		{
			name: "a success row", patch: "@@ file link.txt\n@@ old\none\n@@ new\ntwo\n",
			says: []string{"M link.txt +1 -1  (it resolves to e<U+001B>v.txt)"},
		},
		{
			name: "a directory rollback left", patch: "@@ create d/new/x.txt\nx\n\n",
			verify: "touch d/new/keep; false",
			says:   []string{"e<U+001B>v/new, which hunk made as a directory, is not empty, so hunk left it."},
		},
		{
			name: "a not-restored entry and its reason", patch: "@@ file d/a.txt\n@@ old\none\n@@ new\ntwo\n",
			verify: `n=$(printf 'e\033v'); mv "$n" moved && ln -s moved "$n"; false`, links: true,
			says: []string{
				"d/a.txt (it resolves to e<U+001B>v/a.txt) was not restored.",
				"  e<U+001B>v, the directory hunk wrote it in, is now a symbolic link,",
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.links {
				needsShellSymlinks(t)
			}
			var flags []string
			if c.verify != "" {
				flags = []string{"--verify", c.verify}
			}
			_, out, errOut := runCLI(t, tree(t), flags, c.patch)
			all := out + errOut
			if strings.Contains(all, "\x1b") {
				t.Errorf("the report printed ESC raw:\n%q", all)
			}
			for _, s := range c.says {
				if !strings.Contains(all, s) {
					t.Errorf("the report does not say %q:\n%s", s, all)
				}
			}
		})
	}
	// JSON keeps the name as it is, escaped by the encoder, as it keeps a
	// path as written.
	_, out, _ := runCLI(t, tree(t), []string{"--json"}, "@@ file link.txt\n@@ old\none\n@@ new\ntwo\n")
	if got := resolvedOrAbsent(decodeResolved(t, out).Files[0].Resolved); got != esc+".txt" {
		t.Errorf("--json: resolved = %q, want %q", got, esc+".txt")
	}
}

// Every report built from a file that names it as the patch wrote it carries
// where it led too. The compiler cannot say so: a new NotRestored written
// with Path and no Resolved compiles, and drops the clause from that one
// message. So every composite literal of these four types in apply.go that
// sets Path is read, and has to set Resolved.
func TestEveryFileReportCarriesWhereItLed(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "apply.go", nil, 0)
	must(t, err)
	seen := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		id, ok := lit.Type.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "NotRestored", "CommitError", "ChangedError", "FileResult":
		default:
			return true
		}
		keys := map[string]bool{}
		for _, e := range lit.Elts {
			if kv, ok := e.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok {
					keys[k.Name] = true
				}
			}
		}
		if keys["Path"] {
			seen[id.Name]++
			if !keys["Resolved"] {
				t.Errorf("%s: a %s names Path and not Resolved", fset.Position(lit.Pos()), id.Name)
			}
		}
		return true
	})
	// The census, so that a rename of a type cannot pass this by finding none.
	for _, name := range []string{"NotRestored", "CommitError", "ChangedError", "FileResult"} {
		if seen[name] == 0 {
			t.Errorf("no %s literal with a Path found in apply.go; the test is reading the wrong thing", name)
		}
	}
}
