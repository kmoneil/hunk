package main

import (
	"bytes"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func invertCLI(t *testing.T, args []string, stdin string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli(append([]string{"invert"}, args...), strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

// asPrinted is hunks as printPatch writes them in a patch that is CRLF or
// not: in a CRLF one, a payload's last line has its CR.
func asPrinted(hunks []Hunk, crlf bool) []Hunk {
	withCR := func(b []byte) []byte {
		if crlf && len(b) > 0 && !bytes.HasSuffix(b, cr) {
			return append(slices.Clip(b), '\r')
		}
		return b
	}
	out := make([]Hunk, len(hunks))
	for i, h := range hunks {
		h.Old, h.New, h.Body = withCR(h.Old), withCR(h.New), withCR(h.Body)
		out[i] = h
	}
	return out
}

// sameHunks compares hunks as the patch says them, without their lines.
func sameHunks(a, b []Hunk) bool {
	return slices.EqualFunc(a, b, func(x, y Hunk) bool {
		return x.Op == y.Op && x.Path == y.Path && x.Count == y.Count &&
			bytes.Equal(x.Old, y.Old) && bytes.Equal(x.New, y.New) && bytes.Equal(x.Body, y.Body)
	})
}

// TestInvertPrints (#119): the inverse, byte for byte. Each replacement's old
// and new swapped with its count kept, the hunks last first, "@@ file" when
// the file in effect changes, a create as its text taken out and a delete,
// and the patch's marker and line ending throughout. What it prints parses.
func TestInvertPrints(t *testing.T) {
	for _, c := range []struct {
		name  string
		args  []string
		patch string
		want  string
	}{
		{
			"#119: one replacement",
			nil,
			"@@ file f.txt\n@@ old\na\n@@ new\nA\n",
			"@@ file f.txt\n@@ old\nA\n@@ new\na\n",
		},
		{
			"the count is kept",
			nil,
			"@@ file f.txt\n@@ old x2\na\n@@ new\nA\n",
			"@@ file f.txt\n@@ old x2\nA\n@@ new\na\n",
		},
		{
			"last first, under one @@ file",
			nil,
			"@@ file f.txt\n@@ old\na\n@@ new\nb\n@@ old\nb\n@@ new\nc\n",
			"@@ file f.txt\n@@ old\nc\n@@ new\nb\n@@ old\nb\n@@ new\na\n",
		},
		{
			"two files",
			nil,
			"@@ file a.txt\n@@ old\n1\n@@ new\n2\n@@ file b.txt\n@@ old\n3\n@@ new\n4\n",
			"@@ file b.txt\n@@ old\n4\n@@ new\n3\n@@ file a.txt\n@@ old\n2\n@@ new\n1\n",
		},
		{
			"back and forth, each file named again",
			nil,
			"@@ file a.txt\n@@ old\n1\n@@ new\n2\n@@ file b.txt\n@@ old\n3\n@@ new\n4\n@@ file a.txt\n@@ old\n5\n@@ new\n6\n",
			"@@ file a.txt\n@@ old\n6\n@@ new\n5\n@@ file b.txt\n@@ old\n4\n@@ new\n3\n@@ file a.txt\n@@ old\n2\n@@ new\n1\n",
		},
		{
			"payloads ending in a newline keep their blank line",
			nil,
			"@@ file f.txt\n@@ old\na\n\n@@ new\nA\nB\n\n",
			"@@ file f.txt\n@@ old\nA\nB\n\n@@ new\na\n\n",
		},
		{
			"a create, as its text taken out and a delete",
			nil,
			"@@ create n.txt\nhi\n\n",
			"@@ file n.txt\n@@ old\nhi\n\n@@ new\n@@ delete n.txt\n",
		},
		{
			"an empty create is a delete",
			nil,
			"@@ create n.txt\n",
			"@@ delete n.txt\n",
		},
		{
			"a create and an edit to it",
			nil,
			"@@ create n.txt\nhi\n\n@@ old\nhi\n@@ new\nHI\n",
			"@@ file n.txt\n@@ old\nHI\n@@ new\nhi\n@@ old\nhi\n\n@@ new\n@@ delete n.txt\n",
		},
		{
			"a create between two files' edits",
			nil,
			"@@ file a.txt\n@@ old\n1\n@@ new\n2\n@@ create n.txt\nx\n@@ file a.txt\n@@ old\n3\n@@ new\n4\n",
			"@@ file a.txt\n@@ old\n4\n@@ new\n3\n@@ file n.txt\n@@ old\nx\n@@ new\n@@ delete n.txt\n@@ file a.txt\n@@ old\n2\n@@ new\n1\n",
		},
		{
			"a CRLF patch stays CRLF",
			nil,
			"@@ file f.txt\r\n@@ old\r\na\r\n@@ new\r\nA\r\n",
			"@@ file f.txt\r\n@@ old\r\nA\r\n@@ new\r\na\r\n",
		},
		{
			"a CRLF create",
			nil,
			"@@ create n.txt\r\nhi\r\n\r\n",
			"@@ file n.txt\r\n@@ old\r\nhi\r\n\r\n@@ new\r\n@@ delete n.txt\r\n",
		},
		{
			// Its last payload line has no CR, and is given one, or the
			// inverse would not be CRLF: found by FuzzInvert.
			"a CRLF patch with no final newline",
			nil,
			"@@ file f.txt\r\n@@ old\r\na\r\n@@ new\r\nA",
			"@@ file f.txt\r\n@@ old\r\nA\r\n@@ new\r\na\r\n",
		},
		{
			// The CRs of an LF patch are payload, kept as they are.
			"an LF patch with a CR in a payload",
			nil,
			"@@ file f.txt\n@@ old\na\r\n\n@@ new\na\n\n",
			"@@ file f.txt\n@@ old\na\n\n@@ new\na\r\n\n",
		},
		{
			"under --marker, with payload lines that are directives under @@",
			[]string{"--marker", "%%"},
			"%% file f.txt\n%% old\n@@ old\n%% new\n@@ new\n",
			"%% file f.txt\n%% old\n@@ new\n%% new\n@@ old\n",
		},
		{
			"a patch with no final newline",
			nil,
			"@@ file f.txt\n@@ old\na\n@@ new\nA",
			"@@ file f.txt\n@@ old\nA\n@@ new\na\n",
		},
		{
			"@@ end is not carried",
			nil,
			"@@ file f.txt\n@@ old\na\n@@ new\nA\n@@ end\n",
			"@@ file f.txt\n@@ old\nA\n@@ new\na\n",
		},
		{
			"a hunk that changes nothing is its own inverse",
			nil,
			"@@ file f.txt\n@@ old\na\n@@ new\na\n",
			"@@ file f.txt\n@@ old\na\n@@ new\na\n",
		},
		{
			"a mistyped delimiter, with the flag the apply took",
			[]string{allowMistypedFlag},
			"@@ file f.txt\n@@ old\na\n@@ new\nA\nHUHUNK\n",
			"@@ file f.txt\n@@ old\nA\nHUHUNK\n@@ new\na\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := invertCLI(t, c.args, c.patch)
			if code != exitOK || errOut != "" {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			if out != c.want {
				t.Errorf("inverse\n got %q\nwant %q", out, c.want)
			}
			marker := DefaultMarker
			if c.args != nil && c.args[0] == "--marker" {
				marker = c.args[1]
			}
			if _, err := Parse([]byte(out), marker); err != nil {
				t.Errorf("the inverse does not parse: %v", err)
			}
		})
	}

	t.Run("-f", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "p.hunk")
		must(t, os.WriteFile(p, []byte("@@ file f.txt\n@@ old\na\n@@ new\nA\n"), 0o644))
		code, out, errOut := invertCLI(t, []string{"-f", p}, "ignored")
		if code != exitOK || errOut != "" || out != "@@ file f.txt\n@@ old\nA\n@@ new\na\n" {
			t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
		}
	})
}

// TestInvertRefuses: what has no inverse is refused, every such hunk named in
// patch order, and so is a call or a patch hunk would refuse. Each refusal is
// on stderr, and its first line on stdout as an "@@ end" line under the
// patch's marker, which nothing after it can apply past.
func TestInvertRefuses(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.hunk")
	for _, c := range []struct {
		name   string
		args   []string
		patch  string
		marker string
		want   []string // in stderr, in order
	}{
		{
			"a delete", nil, "@@ delete f.txt\n", "",
			[]string{
				"hunk invert: 1 hunk has no inverse, so the patch was not inverted\n\nhunk 1  f.txt  (patch line 1)\n",
				"  a delete is not inverted: the patch does not hold the bytes to put back\n",
			},
		},
		{
			"an append", nil, "@@ append f.txt\nx\n", "",
			[]string{"hunk 1  f.txt  (patch line 1)\n  an append is not inverted: it may also have ended the file's last line"},
		},
		{
			"a prepend", nil, "@@ prepend f.txt\nx\n", "",
			[]string{"hunk 1  f.txt  (patch line 1)\n  a prepend is not inverted: the line ending it may have added is the file's"},
		},
		{
			"an empty new", nil, "@@ file f.txt\n@@ old\na\n\n@@ new\n", "",
			[]string{"hunk 1  f.txt  (patch line 2)\n  its new text is empty, so it took text out"},
		},
		{
			// A blank line in a CRLF patch is a CR, which the parser reads
			// as empty in an old.
			"an empty new in a CRLF patch", nil, "@@ file f.txt\r\n@@ old\r\na\r\n\r\n@@ new\r\n\r\n", "",
			[]string{"hunk 1  f.txt  (patch line 2)\n  its new text is empty"},
		},
		{
			"several, all named, in patch order", nil,
			"@@ delete d.txt\n@@ file f.txt\n@@ old\na\n@@ new\nA\n@@ append g.txt\nx\n@@ file h.txt\n@@ old\nh\n\n@@ new\n", "",
			[]string{
				"hunk invert: 3 hunks have no inverse, so the patch was not inverted\n",
				"hunk 1  d.txt  (patch line 1)\n  a delete",
				"hunk 3  g.txt  (patch line 7)\n  an append",
				"hunk 4  h.txt  (patch line 10)\n  its new text is empty",
			},
		},
		{
			"a file replaced wholesale, by its delete", nil, "@@ delete f.txt\n@@ create f.txt\nx\n", "",
			[]string{"1 hunk has no inverse", "hunk 1  f.txt  (patch line 1)\n  a delete"},
		},
		{
			"a path with a control character, shown", nil, "@@ file a\x1bb.txt\n@@ old\na\n@@ new\nA\n", "",
			[]string{"hunk 1  a<U+001B>b.txt  (patch line 2)\n  its path holds U+001B, a control character, which hunk refuses in a path, so the patch never applied"},
		},
		{
			// Only spaces and tabs are trimmed from a path, and the CR
			// printed back would be read as the line's ending.
			"a path that ends in a CR", nil, "@@ create 00\r \n", "",
			[]string{"hunk 1  00<U+000D>  (patch line 1)\n  its path holds U+000D"},
		},
		{"a patch that does not parse", nil, "@@ old\na\n@@ new\nb\n", "", []string{"hunk invert: patch line 1: no file is in effect"}},
		{"a break set", nil, "@@ patch one\n@@ file f.txt\n@@ old\na\n@@ new\nb\n", "", []string{"heads one patch of a break set"}},
		{"no patch", nil, "", "", []string{"hunk invert: the patch is empty"}},
		{
			"a mistyped delimiter", nil, "@@ file f.txt\n@@ old\na\n@@ new\nA\nHUHUNK\n", "",
			[]string{`patch line 6 is "HUHUNK", which looks like a mistyped heredoc delimiter`, "pass " + allowMistypedFlag},
		},
		{"an argument", []string{"p.hunk"}, "", "", []string{`unexpected argument "p.hunk"; the patch comes from stdin or -f, as for hunk`}},
		{"a flag it does not take", []string{"--dry-run"}, "", "", []string{"flag provided but not defined: -dry-run", `run "hunk invert --help"`}},
		{"-f twice", []string{"-f", "a.hunk", "-f", "b.hunk"}, "", "", []string{"already reading a.hunk; hunk invert takes one patch"}},
		{"-f that cannot be read", []string{"-f", missing}, "", "", []string{"hunk invert: open " + missing}},
		{"an empty --marker", []string{"--marker", ""}, "", "", []string{"--marker must not be empty"}},
		// A marker that cannot head a line is not the end line's: the
		// default is, as for quote.
		{"a control character in --marker", []string{"--marker", "%\x1b"}, "", "", []string{"--marker contains U+001B, a control character"}},
		{"under --marker, its end line too", []string{"--marker", "%%"}, "%% delete f.txt\n", "%%", []string{"a delete is not inverted"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := invertCLI(t, c.args, c.patch)
			if code != exitUsage {
				t.Fatalf("exit %d, want 1\nstdout %q\nstderr %q", code, out, errOut)
			}
			at := 0
			for _, w := range c.want {
				i := strings.Index(errOut[at:], w)
				if i < 0 {
					t.Fatalf("stderr does not say %q after byte %d:\n%s", w, at, errOut)
				}
				at += i + len(w)
			}
			marker := c.marker
			if marker == "" {
				marker = DefaultMarker
			}
			first, _, _ := strings.Cut(strings.TrimPrefix(errOut, "hunk invert: "), "\n")
			if want := marker + " end refused by hunk invert: " + first + "\n"; out != want {
				t.Errorf("stdout\n got %q\nwant %q", out, want)
			}
			if strings.ContainsRune(out+errOut, '\x1b') {
				t.Errorf("a control character was printed: %q %q", out, errOut)
			}
		})
	}
}

// Two ends of the call: after a flag, invert is not a subcommand, and an
// inverse that cannot be written is exit 5, as quote's is.
func TestInvertsEnds(t *testing.T) {
	t.Run("invert is a subcommand and comes first", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliTree(t, nil), []string{"invert"}, "@@ file f.txt\n@@ old\na\n@@ new\nA\n")
		if code != exitUsage || out != "" || !strings.Contains(errOut, `invert is a subcommand and comes first, its flags after it; run "hunk invert --help"`) {
			t.Errorf("exit %d, stdout %q: %s", code, out, errOut)
		}
	})
	t.Run("an inverse that cannot be written", func(t *testing.T) {
		var errOut bytes.Buffer
		code := runInvert(nil, strings.NewReader("@@ file f.txt\n@@ old\na\n@@ new\nA\n"), failingWriter{}, &errOut)
		if code != exitIO || errOut.String() != "hunk invert: the inverse could not be written: disk gone\n" {
			t.Errorf("exit %d: %q", code, errOut.String())
		}
	})
}

// A refused inverse piped into hunk stops it at exit 1, writing nothing,
// where an empty stdout would have applied nothing at exit 0.
func TestARefusedInverseStopsTheApply(t *testing.T) {
	root := cliTree(t, map[string]string{"f.txt": "a\n"})
	before := snapshot(t, root)
	_, inverse, _ := invertCLI(t, nil, "@@ delete f.txt\n")
	code, _, errOut := runCLI(t, root, nil, inverse)
	if code != exitUsage || !strings.Contains(errOut, "refused by hunk invert: 1 hunk has no inverse") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if d := snapshotDiff(before, snapshot(t, root)); len(d) > 0 {
		t.Errorf("the tree changed: %v", d)
	}
}

// TestAnInverseUndoesThePatch: on a real tree, a patch and then its inverse
// leave every byte and mode as they were. Where the file has moved on since,
// the inverse is refused at 2 and writes nothing; and the two limits --help
// states are what happens.
func TestAnInverseUndoesThePatch(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		args  []string
		patch string
	}{
		{"#119's repro", map[string]string{"f.txt": "a\nb\n"}, nil, "@@ file f.txt\n@@ old\na\n@@ new\nA\n"},
		{"a hunk that matches the one before it", map[string]string{"f.txt": "a\nb\n"}, nil, "@@ file f.txt\n@@ old\na\n@@ new\nb2\n@@ old\nb2\n@@ new\nc\n"},
		{"@@ old x3", map[string]string{"f.txt": "x = 1\ny = 1\nz = 1\n"}, nil, "@@ file f.txt\n@@ old x3\n1\n@@ new\n22\n"},
		{
			"two files and a create",
			map[string]string{"a.go": "package a\n", "b.go": "package b\n\nfunc B() {}\n"},
			nil,
			"@@ file a.go\n@@ old\npackage a\n@@ new\npackage a // A\n@@ file b.go\n@@ old\nfunc B() {}\n@@ new\nfunc B() { C() }\n@@ create c.go\npackage b\n\nfunc C() {}\n\n",
		},
		{"a create and an edit to it", nil, nil, "@@ create n.txt\nhi\n\n@@ old\nhi\n@@ new\nHI\n"},
		{"lines added, with the blank line", map[string]string{"f.txt": "a\nb\n"}, nil, "@@ file f.txt\n@@ old\na\n\n@@ new\na\na2\na3\n\n"},
		{"a CRLF file under an LF patch", map[string]string{"f.txt": "a\r\nb\r\n"}, nil, "@@ file f.txt\n@@ old\na\nb\n@@ new\nA\nB\n"},
		{"a CRLF patch", map[string]string{"f.txt": "a\nb\n"}, nil, "@@ file f.txt\r\n@@ old\r\na\r\n@@ new\r\nA\r\n@@ create n.txt\r\nhi\r\n\r\n"},
		{"a CRLF patch with no final newline", map[string]string{"f.txt": "a\nb\n"}, nil, "@@ file f.txt\r\n@@ old\r\nb\r\n@@ new\r\nB\r\n@@ old\r\na\r\n@@ new\r\nA"},
		{"--eol strict", map[string]string{"f.txt": "a\r\nb\r\n"}, []string{"--eol", "strict"}, "@@ file f.txt\n@@ old\na\r\n\n@@ new\na\n\n"},
		{"--marker", map[string]string{"f.txt": "@@ old\n"}, []string{"--marker", "%%"}, "%% file f.txt\n%% old\n@@ old\n%% new\n@@ new\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			before := snapshot(t, root)
			if code, out, errOut := runCLI(t, root, c.args, c.patch); code != exitOK {
				t.Fatalf("the patch: exit %d: %s%s", code, out, errOut)
			}
			var marker []string
			if c.args != nil && c.args[0] == "--marker" {
				marker = c.args
			}
			code, inverse, errOut := invertCLI(t, marker, c.patch)
			if code != exitOK {
				t.Fatalf("invert: exit %d: %s", code, errOut)
			}
			if code, out, errOut := runCLI(t, root, c.args, inverse); code != exitOK {
				t.Fatalf("the inverse: exit %d: %s%s\n%s", code, out, errOut, inverse)
			}
			if d := snapshotDiff(before, snapshot(t, root)); len(d) > 0 {
				t.Errorf("the tree is not as it was: %v", d)
			}
		})
	}

	// Refused at 2, nothing written: the inverse's text is not in the file
	// as many times as the patch wrote it.
	for _, c := range []struct {
		name   string
		files  map[string]string
		patch  string
		change func(root string)
		want   string
	}{
		{
			"the new text changed since",
			map[string]string{"f.txt": "a\nb\n"},
			"@@ file f.txt\n@@ old\na\n@@ new\nA\n",
			func(root string) { must(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("B\nb\n"), 0o644)) },
			"expected 1 occurrence, found 0",
		},
		{
			"the new text was in the file already",
			map[string]string{"f.txt": "a\nA\n"},
			"@@ file f.txt\n@@ old\na\n@@ new\nA\n",
			func(string) {},
			"expected 1 occurrence, found 2",
		},
		{
			"a created file whose text changed since is not deleted",
			nil,
			"@@ create n.txt\nhi\n\n",
			func(root string) { must(t, os.WriteFile(filepath.Join(root, "n.txt"), []byte("hello\n"), 0o644)) },
			"expected 1 occurrence, found 0",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			if code, _, errOut := runCLI(t, root, nil, c.patch); code != exitOK {
				t.Fatalf("the patch: exit %d: %s", code, errOut)
			}
			c.change(root)
			moved := snapshot(t, root)
			_, inverse, _ := invertCLI(t, nil, c.patch)
			code, _, errOut := runCLI(t, root, nil, inverse)
			if code != exitNoMatch || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d, want 2 and %q:\n%s", code, c.want, errOut)
			}
			if d := snapshotDiff(moved, snapshot(t, root)); len(d) > 0 {
				t.Errorf("a refused inverse wrote: %v", d)
			}
		})
	}

	// The limits --help states, as they happen.
	t.Run("text added to a created file is deleted with it", func(t *testing.T) {
		root := cliTree(t, nil)
		patch := "@@ create n.txt\nhi\n\n"
		runCLI(t, root, nil, patch)
		must(t, os.WriteFile(filepath.Join(root, "n.txt"), []byte("hi\nadded since\n"), 0o644))
		_, inverse, _ := invertCLI(t, nil, patch)
		if code, _, errOut := runCLI(t, root, nil, inverse); code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if _, err := os.Stat(filepath.Join(root, "n.txt")); !os.IsNotExist(err) {
			t.Errorf("n.txt: %v", err)
		}
	})
	t.Run("a patch that changed the file's ending gets its text back in the other", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": "a\r\nb"})
		patch := "@@ file f.txt\n@@ old\na\nb\n@@ new\nab\n"
		runCLI(t, root, nil, patch)
		_, inverse, _ := invertCLI(t, nil, patch)
		if code, _, errOut := runCLI(t, root, nil, inverse); code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if got := readFile(t, root, "f.txt"); got != "a\nb" {
			t.Errorf("f.txt = %q", got)
		}
	})
	t.Run("a directory a create made is left, empty", func(t *testing.T) {
		root := cliTree(t, nil)
		patch := "@@ create sub/n.txt\nhi\n\n"
		runCLI(t, root, nil, patch)
		_, inverse, _ := invertCLI(t, nil, patch)
		if code, _, errOut := runCLI(t, root, nil, inverse); code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		entries, err := os.ReadDir(filepath.Join(root, "sub"))
		if err != nil || len(entries) != 0 {
			t.Errorf("sub: %v, %v", entries, err)
		}
	})
}

// invertHelpFlags is the flags "hunk invert --help" lists.
func invertHelpFlags(t *testing.T) []string {
	t.Helper()
	_, section, ok := strings.Cut(invertUsage, "\nFLAGS\n")
	if !ok {
		t.Fatal("hunk invert --help has no FLAGS section")
	}
	section, _, _ = strings.Cut(section, "\nREFUSALS\n")
	var flags []string
	for _, m := range reHelpFlag.FindAllStringSubmatch(section, -1) {
		flags = append(flags, m[1])
	}
	return flags
}

func TestInvertHelp(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		code, out, errOut := invertCLI(t, []string{arg}, "")
		if code != exitOK || out != invertUsage || errOut != "" {
			t.Errorf("%s: exit %d, stderr %q, or not the usage", arg, code, errOut)
		}
	}
	golden(t, "cli-invert-help", invertUsage)

	// The flags it documents are the flags it takes, both ways.
	var defined []string
	newInvertFlags().fs.VisitAll(func(f *flag.Flag) {
		switch {
		case f.Name == "help" || f.Name == "h":
		case len(f.Name) == 1:
			defined = append(defined, "-"+f.Name)
		default:
			defined = append(defined, "--"+f.Name)
		}
	})
	listed := invertHelpFlags(t)
	slices.Sort(defined)
	slices.Sort(listed)
	if !slices.Equal(defined, listed) {
		t.Errorf("hunk invert takes %q and its help lists %q", defined, listed)
	}

	flat := strings.Join(strings.Fields(invertUsage), " ")
	for _, want := range []string{
		"WHAT HAS NO INVERSE", "WHAT IT CANNOT CHECK", "last first", "deleted with it", "left in place, empty",
		"@@ delete", "@@ append", "@@ prepend", "an empty new", "--dry-run",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("hunk invert --help does not say %q", want)
		}
	}
	for _, code := range []string{"1", "5"} {
		if !strings.Contains(invertUsage, "\n  "+code+"  ") {
			t.Errorf("hunk invert --help does not list exit %s", code)
		}
	}
}

// --help, the README and the skill each name the subcommand, and the two an
// agent reads before using it say what it refuses and what it cannot check.
func TestTheDocsNameInvert(t *testing.T) {
	var out bytes.Buffer
	if code := cli([]string{"--help"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != exitOK {
		t.Fatalf("--help: exit %d", code)
	}
	if !strings.Contains(out.String(), "hunk invert") {
		t.Error("hunk --help never says \"hunk invert\"")
	}
	for _, p := range []string{"README.md", filepath.Join(".agents", "skills", "hunk", "SKILL.md")} {
		b, err := os.ReadFile(p)
		must(t, err)
		flat := strings.Join(strings.Fields(string(b)), " ")
		_, after, ok := strings.Cut(flat, "hunk invert")
		if !ok {
			t.Errorf("%s never says \"hunk invert\"", p)
			continue
		}
		for _, want := range []string{"last first", "@@ delete", "--dry-run", "deleted with it"} {
			if !strings.Contains(after, want) {
				t.Errorf("%s names hunk invert and says nothing after it of %q", p, want)
			}
		}
	}
}

// invert.go reads no tree and writes nothing: the patch comes through
// readPatch, in main.go, and the inverse goes to stdout. This holds the claim
// --help makes for the calls no test makes.
func TestInvertReadsNoTree(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "invert.go", nil, 0)
	if err != nil {
		t.Fatalf("parse invert.go: %v", err)
	}
	forbidden := map[string]bool{"OpenTree": true, "NewTxn": true, "Tree": true, "Txn": true}
	idents := 0
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && x.Name == "os" {
				t.Errorf("invert.go uses os.%s; it reads its patch through readPatch and nothing else", n.Sel.Name)
			}
		case *ast.Ident:
			idents++
			if forbidden[n.Name] {
				t.Errorf("invert.go names %s; hunk invert reads no tree", n.Name)
			}
		}
		return true
	})
	if idents == 0 {
		t.Fatal("read no identifiers from invert.go; every check above would pass")
	}
}

// FuzzInvert: whatever the input, hunk invert ends with a value, never a
// panic. At 0 it printed exactly the hunks invertPatch made, as a patch that
// parses back to them with the input's line ending, and for a patch of
// replacements alone, inverting that gives the patch back. At 1 its stdout is
// one "@@ end" line, which the parser refuses.
func FuzzInvert(f *testing.F) {
	for _, s := range []string{
		"@@ file f.txt\n@@ old\na\n@@ new\nA\n",
		"@@ file f.txt\n@@ old x2\na\n\n@@ new\nA\nB\n\n@@ old\nA\n@@ new\n",
		"@@ create n.txt\nhi\n\n@@ old\nhi\n@@ new\nHI\n@@ file a.txt\n@@ old\n1\n@@ new\n2\n",
		"@@ create n.txt\r\nhi\r\n\r\n@@ file f.txt\r\n@@ old\r\na\r\n@@ new\r\n\r\nb\r\n",
		"@@ file f.txt\n@@ old\na\r\n\n@@ new\na\n\n",
		"@@ delete f.txt\n@@ append f.txt\nx\n@@ prepend f.txt\n",
		"@@ file f.txt\n@@ old\na\n@@ new\nA\nHUHUNK\n",
		"@@ file f.txt\n@@ old\na\n@@ new\nA",
		"@@ create n.txt\n",
		"",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		var out, errOut bytes.Buffer
		code := runInvert([]string{allowMistypedFlag}, bytes.NewReader(src), &out, &errOut)
		switch code {
		case exitOK:
			p, err := Parse(src, DefaultMarker)
			if err != nil {
				t.Fatalf("inverted a patch that does not parse: %v\n%q", err, src)
			}
			want, _ := invertPatch(p)
			q, err := Parse(out.Bytes(), DefaultMarker)
			if err != nil {
				t.Fatalf("the inverse does not parse: %v\nsrc %q\ninverse %q", err, src, out.Bytes())
			}
			if !sameHunks(q.Hunks, asPrinted(want, p.CRLF)) || q.CRLF != p.CRLF || errOut.Len() > 0 {
				t.Fatalf("printed %+v (crlf %v), want %+v (crlf %v)\nsrc %q\ninverse %q", q.Hunks, q.CRLF, want, p.CRLF, src, out.Bytes())
			}
			if !slices.ContainsFunc(p.Hunks, func(h Hunk) bool { return h.Op != OpReplace }) {
				back, faults := invertPatch(q)
				if faults != nil || !sameHunks(back, asPrinted(p.Hunks, p.CRLF)) {
					t.Fatalf("inverted twice is not the patch: %+v\nsrc %q", back, src)
				}
			}
		case exitUsage:
			s := out.String()
			if !strings.HasPrefix(s, DefaultMarker+" end refused by hunk invert: ") || strings.Count(s, "\n") != 1 || !strings.HasSuffix(s, "\n") {
				t.Fatalf("stdout is not one end line: %q", s)
			}
			if _, err := Parse(out.Bytes(), DefaultMarker); err == nil {
				t.Fatalf("the end line parses: %q", s)
			}
		default:
			t.Fatalf("exit %d\nsrc %q", code, src)
		}
	})
}

// FuzzAnInverseUndoesThePatch is §8.1's property, for a patch of two
// replacements on a real file: applied and then inverted, the file is byte
// for byte what it was, or the inverse is refused at 2 and writes nothing.
// Never a third file. Under --eol auto and strict, each inverse applied as
// its patch was.
//
// With one exception, which --help states and the fuzzer found: under --eol
// auto a payload is written in the ending the file has most of when it is
// applied, so a patch that changed which ending that is gets its text back
// in the other one ("a\r\nb" joined to "ab" comes back "a\nb"). Then the
// file is what it was but for its CRs.
func FuzzAnInverseUndoesThePatch(f *testing.F) {
	for _, c := range [][4]string{
		{"a\nb\n", "a", "A", "b"},
		{"a\nb\n", "a", "b2", "b2"},
		{"x = 1\ny = 1\n", "1", "11", "x"},
		{"a\nA\n", "a", "A", "b"},
		{"a\r\nb\r\n", "a\n", "A\n", "b"},
		{"aaa", "a", "aa", "aaa"},
	} {
		f.Add([]byte(c[0]), []byte(c[1]), []byte(c[2]), []byte(c[3]), []byte("Z"), false)
		f.Add([]byte(c[0]), []byte(c[1]), []byte(c[2]), []byte(c[3]), []byte("Z"), true)
	}
	// #122. The nightly's find: a bare "\r" for old, which met the CR of every
	// CRLF, so the inverse took another copy of "\r\n\n" at exit 0. And the
	// same with no bare CR, the ending flipped by a join: the inverse's "\nR"
	// landed on the newline of the CRLF its patch wrote. c is written in CRLF
	// because the count here is of raw bytes. Both are refused now, the first
	// when it is applied and the second when it is inverted.
	f.Add([]byte("\r\n\x00\r\n\n"), []byte("\n\x00"), []byte("\r\n\n"), []byte("\r"), []byte("\n0"), false)
	f.Add([]byte("a\r\nv\r\nw\r\nx\r\ny\r\nP\nQ\nm\nn\n"), []byte("a"), []byte("P\nQ"),
		[]byte("Q\r\nv\r\nw\r\nx\r\ny"), []byte("\nR"), false)
	// #126, the next night's find: d is a carriage return and then a line
	// break, which went into the file, LF by a tie, as "\r\n". That read as a
	// CRLF, turned the majority to CRLF, and the inverse's "\r\r\n" then
	// matched the file's own first line at the right count. Refused now when
	// the patch is applied, as joining a carriage return to a newline.
	f.Add([]byte("\r\r\n\x80J\n"), []byte("J"), []byte("0"), []byte("0"), []byte("\r\r\n"), false)
	f.Fuzz(func(t *testing.T, start, a, b, c, d []byte, strict bool) {
		n1 := bytes.Count(start, a)
		if len(a) == 0 || n1 == 0 {
			return
		}
		n2 := bytes.Count(bytes.Replace(start, a, b, n1), c)
		if len(c) == 0 || n2 == 0 {
			return
		}
		opt := Options{}
		if strict {
			opt.EOL = EOLStrict
		}
		p := &Patch{Hunks: []Hunk{
			{Op: OpReplace, Path: "a.txt", Line: 1, Count: n1, Old: a, New: b},
			{Op: OpReplace, Path: "a.txt", Line: 2, Count: n2, Old: c, New: d},
		}}
		tree, root := fixture(t, map[string]string{"a.txt": string(start)})
		if _, err := NewTxn(tree, opt).Run(p); err != nil {
			return // the patch itself refused, as under --eol auto it can be
		}
		after, err := os.ReadFile(filepath.Join(root, "a.txt"))
		must(t, err)
		inv, faults := invertPatch(p)
		if faults != nil {
			if len(b) > 0 && len(d) > 0 {
				t.Fatalf("refused %+v with no empty new", faults)
			}
			return
		}
		_, err = NewTxn(tree, opt).Run(&Patch{Hunks: inv})
		now, rerr := os.ReadFile(filepath.Join(root, "a.txt"))
		must(t, rerr)
		var ve *ValidationError
		flipped := !strict && dominantEOL(after) != dominantEOL(start)
		noCR := func(b []byte) []byte { return bytes.ReplaceAll(b, cr, nil) }
		switch {
		case err == nil && flipped && !bytes.Equal(noCR(now), noCR(start)):
			t.Fatalf("the ending flipped, and the inverse left %q, not %q but for CRs\nafter the patch %q\na %q b %q c %q d %q", now, start, after, a, b, c, d)
		case err == nil && !flipped && !bytes.Equal(now, start):
			t.Fatalf("the inverse applied and left %q, not %q\nafter the patch %q\na %q b %q c %q d %q strict %v", now, start, after, a, b, c, d, strict)
		case err != nil && !errors.As(err, &ve):
			t.Fatalf("the inverse failed other than by refusal: %v", err)
		case err != nil && !bytes.Equal(now, after):
			t.Fatalf("a refused inverse wrote %q over %q", now, after)
		}
	})
}

// The goldens: --help, and a refusal that names three hunks, as a terminal
// shows it.
func TestInvertGoldens(t *testing.T) {
	var both bytes.Buffer
	cli([]string{"invert"}, strings.NewReader("@@ delete d.txt\n@@ file f.txt\n@@ old\na\n@@ new\nA\n@@ append g.txt\nx\n@@ file h.txt\n@@ old\nh\n\n@@ new\n"), &both, &both)
	golden(t, "cli-invert-refusal", both.String())
}
