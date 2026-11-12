package main

// --inverse-to's tests (#155, SPEC §4.4). The round trips are the claim: a
// batch applied with --inverse-to, and its inverse applied with exactly the
// arguments the report gave, leave the real tree byte for byte, and mode for
// mode, as it was. The rows are the cases hunk invert refuses or gets wrong,
// and the ones the writer has its own paths for.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// inverseJSON is the part of --json's report the tests read.
type inverseJSON struct {
	Exit    int    `json:"exit"`
	Error   string `json:"error"`
	Inverse *struct {
		Path    string   `json:"path"`
		Args    []string `json:"args"`
		Nothing bool     `json:"nothing_to_undo"`
	} `json:"inverse"`
}

func inverseOf(t *testing.T, out string) inverseJSON {
	t.Helper()
	var r inverseJSON
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	return r
}

// undoWith applies an inverse with the arguments the report gave, in
// process, as a shell would pass them.
func undoWith(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli(args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestInverseToUndoesTheBatch: each row's batch applied with --inverse-to,
// its inverse applied as the report says, and the tree as it began.
func TestInverseToUndoesTheBatch(t *testing.T) {
	issue := "fn a() {\n    return x;\n}\nfn b() {\n    if (y) return x;\n    return x;\n}\n"
	for _, c := range []struct {
		name  string
		files map[string]string
		patch string
		flags []string // the batch's own, beyond --json and --inverse-to
		args  []string // the flags the inverse needs beyond --root and -f
		want  string   // the inverse, where the row pins it
		// asInvert: hunk invert prints the same inverse, since it applies.
		asInvert bool
	}{
		{
			name:  "#155: a new text the file holds twice",
			files: map[string]string{"f.zig": issue},
			patch: "@@ file f.zig\n@@ old\n    if (y) return x;\n    return x;\n@@ new\n    return x;\n",
			want: "@@ file f.zig\n@@ old\nfn b() {\n    return x;\n}\n\n@@ new\n" +
				"fn b() {\n    if (y) return x;\n    return x;\n}\n\n",
		},
		{
			name:  "a replacement hunk invert can undo is undone as it would be",
			files: map[string]string{"a.txt": "alpha\nbeta\n"},
			patch: "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n",
			want:  "@@ file a.txt\n@@ old\nALPHA\n@@ new\nalpha\n", asInvert: true,
		},
		{
			name:  "@@ old x2 whose new is nowhere else keeps its count",
			files: map[string]string{"a.txt": "x\ny\nx\n"},
			patch: "@@ file a.txt\n@@ old x2\nx\n@@ new\nX\n",
			want:  "@@ file a.txt\n@@ old x2\nX\n@@ new\nx\n", asInvert: true,
		},
		{
			// Each copy alone, the second's quote was the first's line and the
			// two after it, and the first's that line and the one after: line
			// 2 quoted twice. Merged, once (#176).
			name:  "@@ old x2 whose new is there once more: copies whose quotes overlap, one hunk",
			files: map[string]string{"a.txt": "x\nX\nx\nend\n"},
			patch: "@@ file a.txt\n@@ old x2\nx\n@@ new\nX\n",
			want:  "@@ file a.txt\n@@ old\nX\nX\nX\nend\n\n@@ new\nx\nX\nx\nend\n\n",
		},
		{
			name:  "@@ old x2 whose quotes only touch: a hunk each, last first",
			files: map[string]string{"a.txt": "x\nX\na\nx\nb\n"},
			patch: "@@ file a.txt\n@@ old x2\nx\n@@ new\nX\n",
			want: "@@ file a.txt\n@@ old\na\nX\nb\n\n@@ new\na\nx\nb\n\n" +
				"@@ old\nX\nX\n\n@@ new\nx\nX\n\n",
		},
		{
			name:  "a regex's matches on one line: the line quoted once",
			files: map[string]string{"r.txt": "foo boo\n0\n"},
			patch: "@@ file r.txt\n@@ re x4\no\n@@ new\n0\n",
			want:  "@@ file r.txt\n@@ old\nf00 b00\n\n@@ new\nfoo boo\n\n",
		},
		{
			// The first match's new text is in the file once, so alone it is
			// undone as hunk invert would; the second's line quotes it first.
			name:  "a regex match undone as hunk invert would, inside the line quoted after it",
			files: map[string]string{"r.txt": "a1 a2 X2\n"},
			patch: "@@ file r.txt\n@@ re x2\na(\\d)\n@@ new\nX$1\n",
			want:  "@@ file r.txt\n@@ old\nX1 X2 X2\n\n@@ new\na1 a2 X2\n\n",
		},
		{
			name:  "two hunks on one line: the line quoted once",
			files: map[string]string{"l.txt": "left right\nL\n"},
			patch: "@@ file l.txt\n@@ old\nleft\n@@ new\nL\n@@ old\nright\n@@ new\nL\n",
			want:  "@@ file l.txt\n@@ old\nL L\n\n@@ new\nleft right\n\n",
		},
		{
			name:  "deleting a line",
			files: map[string]string{"g.txt": "a\nb\nc\nb\n"},
			patch: "@@ file g.txt\n@@ old\nc\n\n@@ new\n",
			want:  "@@ file g.txt\n@@ old\nb\nb\n\n@@ new\nb\nc\nb\n\n",
		},
		{
			name:  "deleting the first line",
			files: map[string]string{"g.txt": "a\nb\n"},
			patch: "@@ file g.txt\n@@ old\na\n\n@@ new\n",
			want:  "@@ file g.txt\n@@ old\nb\n\n@@ new\na\nb\n\n",
		},
		{
			name:  "deleting the last line, which had no newline",
			files: map[string]string{"g.txt": "a\nb"},
			patch: "@@ file g.txt\n@@ old\n\nb\n@@ new\n",
			want:  "@@ file g.txt\n@@ old\na\n@@ new\na\nb\n",
		},
		{
			name:  "deleting text inside a line",
			files: map[string]string{"g.txt": "say hello world\n"},
			patch: "@@ file g.txt\n@@ old\n world\n@@ new\n",
			want:  "@@ file g.txt\n@@ old\nsay hello\n\n@@ new\nsay hello world\n\n",
		},
		{
			name:  "emptying the file: delete, then create, which is a modify",
			files: map[string]string{"g.txt": "only\n"},
			patch: "@@ file g.txt\n@@ old\nonly\n\n@@ new\n",
			want:  "@@ delete g.txt\n@@ create g.txt\nonly\n\n",
		},
		{
			name:  "a chain: a to b, then b to c",
			files: map[string]string{"a.txt": "a\nb\n"},
			patch: "@@ file a.txt\n@@ old\na\n@@ new\nb\n@@ old\nb\nb\n@@ new\nc\n",
		},
		{
			name:  "two files",
			files: map[string]string{"a.txt": "one\n", "b.txt": "two\ntwo\n"},
			patch: "@@ file a.txt\n@@ old\none\n@@ new\nONE\n@@ file b.txt\n@@ old x2\ntwo\n@@ new\nONE\n",
		},
		{
			name:  "an append that ended the last line first",
			files: map[string]string{"k.txt": "keep"},
			patch: "@@ append k.txt\nmore\n",
			want:  "@@ file k.txt\n@@ old\nkeep\nmore\n\n@@ new\nkeep\n",
		},
		{
			name:  "an append whose text the file already holds",
			files: map[string]string{"k.txt": "more\nkeep\n"},
			patch: "@@ append k.txt\nmore\n",
			want:  "@@ file k.txt\n@@ old\nkeep\nmore\n\n@@ new\nkeep\n\n",
		},
		{
			name:  "an append to an empty file",
			files: map[string]string{"e.txt": ""},
			patch: "@@ append e.txt\nmore\n",
			want:  "@@ file e.txt\n@@ old\nmore\n\n@@ new\n",
		},
		{
			name:  "a prepend whose text the file already holds",
			files: map[string]string{"k.txt": "keep\nfirst\n"},
			patch: "@@ prepend k.txt\nfirst\n",
			want:  "@@ file k.txt\n@@ old\nfirst\nkeep\n\n@@ new\nkeep\n\n",
		},
		{
			name:  "a prepend to an empty file",
			files: map[string]string{"e.txt": ""},
			patch: "@@ prepend e.txt\nfirst\n",
			want:  "@@ file e.txt\n@@ old\nfirst\n\n@@ new\n",
		},
		{
			name:  "a create: its text taken out, then deleted",
			files: map[string]string{"a.txt": "a\n"},
			patch: "@@ create n.txt\nhello\n",
			want:  "@@ file n.txt\n@@ old\nhello\n@@ new\n@@ delete n.txt\n",
		},
		{
			name:  "a create, then edited",
			files: map[string]string{"a.txt": "a\n"},
			patch: "@@ create n.txt\nhello\n@@ file n.txt\n@@ old\nhello\n@@ new\nbye\n",
			want:  "@@ file n.txt\n@@ old\nbye\n@@ new\nhello\n@@ old\nhello\n@@ new\n@@ delete n.txt\n",
		},
		{
			name:  "an empty create",
			files: map[string]string{"a.txt": "a\n"},
			patch: "@@ create e.txt\n",
			want:  "@@ delete e.txt\n",
		},
		{
			name:  "a delete: the bytes made again",
			files: map[string]string{"d.txt": "gone\nfor good"},
			patch: "@@ delete d.txt\n",
			want:  "@@ create d.txt\ngone\nfor good\n",
		},
		{
			name:  "a delete, then a create: the file replaced whole",
			files: map[string]string{"d.txt": "old\n"},
			patch: "@@ delete d.txt\n@@ create d.txt\nnew\n",
			want:  "@@ file d.txt\n@@ old\nnew\n@@ new\n@@ delete d.txt\n@@ create d.txt\nold\n\n",
		},
		{
			name:  "a CRLF file, the new text there three times",
			files: map[string]string{"c.txt": "a\r\nb\r\nb\r\n"},
			patch: "@@ file c.txt\n@@ old\na\n@@ new\nb\n",
		},
		{
			name:  "a CRLF patch",
			files: map[string]string{"c.txt": "a\r\nb\r\n"},
			patch: "@@ file c.txt\r\n@@ old\r\na\r\n@@ new\r\nA\r\n",
		},
		{
			name:  "identical lines: widened as far as it takes",
			files: map[string]string{"x.txt": "a\n" + strings.Repeat("x\n", 300) + "b\n"},
			patch: "@@ file x.txt\n@@ old\na\n@@ new\nx\n",
		},
		{
			name:  "an append to identical lines: widened up to the top",
			files: map[string]string{"x.txt": strings.Repeat("x\n", 300)},
			patch: "@@ append x.txt\nx\n",
		},
		{
			name:  "mixed endings: --eol strict",
			files: map[string]string{"m.txt": "b\r\nb\nc\n"},
			patch: "@@ file m.txt\n@@ old\nc\n@@ new\nb\n",
			args:  []string{"--eol", "strict"},
			want:  "@@ file m.txt\n@@ old\nb\nb\n\n@@ new\nb\nc\n\n",
		},
		{
			name:  "a file holding directive lines: another marker",
			files: map[string]string{"d.md": "@@ old\nv\n@@ old\nw\n"},
			patch: "@@ file d.md\n@@ old\nw\n@@ new\nv\n",
			args:  []string{"--marker", "@@@"},
			want:  "@@@ file d.md\n@@@ old\nv\n@@ old\nv\n\n@@@ new\nv\n@@ old\nw\n\n",
		},
		{
			// A line the inverse writes back and its old does not hold: one
			// it quotes on both sides is carried over, and the parser lets
			// that be.
			name:  "a mistyped delimiter the inverse puts back: --allow-mistyped-delimiter",
			files: map[string]string{"h.txt": "a\nHUHUNK\nb\n"},
			patch: "@@ file h.txt\n@@ old\nHUHUNK\n\n@@ new\n",
			args:  []string{"--allow-mistyped-delimiter"},
			want:  "@@ file h.txt\n@@ old\nb\n\n@@ new\nHUHUNK\nb\n\n",
		},
		{
			name:  "a mistyped delimiter quoted on both sides needs nothing",
			files: map[string]string{"h.txt": "v\nHUHUNK\nw\n"},
			patch: "@@ file h.txt\n@@ old\nw\n@@ new\nv\n",
			want:  "@@ file h.txt\n@@ old\nHUHUNK\nv\n\n@@ new\nHUHUNK\nw\n\n",
		},
		{
			name:  "applied under --eol strict, undone under auto",
			files: map[string]string{"a.txt": "a\nb\n"},
			patch: "@@ file a.txt\n@@ old\na\n@@ new\nb\n",
			flags: []string{"--eol", "strict"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			before := snapshot(t, root)
			inv := filepath.Join(t.TempDir(), "undo.hunk")
			args := append([]string{"--json", "--inverse-to", inv}, c.flags...)
			code, out, errOut := runCLI(t, root, args, c.patch)
			if code != exitOK {
				t.Fatalf("apply: exit %d: %s%s", code, out, errOut)
			}
			r := inverseOf(t, out)
			if r.Inverse == nil {
				t.Fatalf("no inverse in the report: %s", out)
			}
			want := slices.Concat([]string{"--root", root}, c.args, []string{"-f", inv})
			if r.Inverse.Path != inv || !slices.Equal(r.Inverse.Args, want) {
				t.Errorf("inverse = %+v, want args %q", r.Inverse, want)
			}
			src, err := os.ReadFile(inv)
			must(t, err)
			if c.want != "" && string(src) != c.want {
				t.Errorf("the inverse:\n got %q\nwant %q", src, c.want)
			}
			if c.asInvert {
				code, printed, _ := invertCLI(t, nil, c.patch)
				if code != exitOK || printed != string(src) {
					t.Errorf("hunk invert prints %q (exit %d), --inverse-to wrote %q", printed, code, src)
				}
			}
			if len(snapshotDiff(before, snapshot(t, root))) == 0 {
				t.Fatal("the batch changed nothing")
			}
			code, out, errOut = undoWith(t, r.Inverse.Args)
			if code != exitOK {
				t.Fatalf("undo: exit %d: %s%s\ninverse:\n%s", code, out, errOut, src)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// Many copies on one line were a hunk each, each quoting the whole line
// (#176): the inverse of two thousand on a 37 KB minified line was 151 MB,
// and of the fuzzer's empty match at every byte of a 3.5 KB line, 75 MB. Now
// the line is quoted once: one hunk, whose old text is the file as the batch
// left it and whose new text the file as it was.
func TestInverseToQuotesALineOnce(t *testing.T) {
	var line strings.Builder
	for i := range 300 {
		fmt.Fprintf(&line, "var a%d=foo(%d);", i, i)
	}
	minified := line.String() + "bar(0)\n"
	for _, c := range []struct{ name, file, patch string }{
		{"a literal whose new text is there already", minified, "@@ file a.txt\n@@ old x300\nfoo(\n@@ new\nbar(\n"},
		{"a regex", minified, "@@ file a.txt\n@@ re x300\nfoo\\(\n@@ new\nbar(\n"},
		{"the fuzzer's empty match at every byte", strings.Repeat("\xab", 300) + "0\n", "@@ file a.txt\n@@ re x303\n1*\n@@ new\n\x00\x02\x00\x00\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": c.file})
			inv := filepath.Join(t.TempDir(), "undo.hunk")
			code, out, errOut := runCLI(t, root, []string{"--json", "--inverse-to", inv}, c.patch)
			if code != exitOK {
				t.Fatalf("apply: exit %d: %s%s", code, out, errOut)
			}
			now := readFile(t, root, "a.txt")
			src, err := os.ReadFile(inv)
			must(t, err)
			p, err := Parse(src, DefaultMarker)
			must(t, err)
			if len(p.Hunks) != 1 || string(p.Hunks[0].Old) != now || string(p.Hunks[0].New) != c.file {
				t.Errorf("the inverse is %d hunks, %d bytes, for a file of %d bytes and then %d:\n%.300q",
					len(p.Hunks), len(src), len(c.file), len(now), src)
			}
			if code, out, errOut := undoWith(t, inverseOf(t, out).Inverse.Args); code != exitOK {
				t.Fatalf("undo: exit %d: %s%s", code, out, errOut)
			}
			if got := readFile(t, root, "a.txt"); got != c.file {
				t.Errorf("a.txt is %.80q after the undo, was %.80q", got, c.file)
			}
		})
	}
}

// An undo merges a quote into the hunk before it where its old text overlaps
// the text that hunk put back, and on back while the hunk that makes overlaps
// the one before that (#176). Each row is a file and the steps of an undo,
// each replacing s[a:b] with back in the file as the steps before it left it,
// or at a of -1, a hunk that is not a quote, the whole file for itself. The
// hunks are what comes out, with where each one's old text starts; applied in
// order at those places, they make the file the steps made.
func TestAnUndoMergesOverlappingQuotes(t *testing.T) {
	type step struct {
		a, b int
		back string
	}
	type out struct {
		at       int
		old, new string
	}
	for _, c := range []struct {
		name  string
		s     string
		steps []step
		want  []out
	}{
		{"apart: a hunk each", "one two", []step{{4, 7, "2"}, {0, 3, "1"}}, []out{{4, "two", "2"}, {0, "one", "1"}}},
		{"touching: a hunk each", "aabb", []step{{2, 4, "B"}, {0, 2, "A"}}, []out{{2, "bb", "B"}, {0, "aa", "A"}}},
		{"over the start of what it put back", "abcdef", []step{{3, 6, "D"}, {2, 4, "X"}}, []out{{2, "cdef", "X"}}},
		{"over its end", "abcdef", []step{{1, 3, "BCDE"}, {4, 7, "x"}}, []out{{1, "bcde", "BCDx"}}},
		{"inside it", "abcdef", []step{{1, 5, "BCDE"}, {2, 3, "c"}}, []out{{1, "bcde", "BcDE"}}},
		{"past both its ends", "abcdef", []step{{2, 4, "C"}, {1, 4, "x"}}, []out{{1, "bcde", "x"}}},
		{"it reaching past both ends", "abcdef", []step{{1, 3, "BCDE"}, {2, 4, "y"}}, []out{{1, "bc", "ByE"}}},
		{"nothing put back, inside the quote", "abc", []step{{1, 2, ""}, {0, 2, "AC"}}, []out{{0, "abc", "AC"}}},
		{"nothing put back, at the quote's end: apart", "abc", []step{{1, 2, ""}, {0, 1, "A"}}, []out{{1, "b", ""}, {0, "a", "A"}}},
		{"on back through the one before", "aaaa|bbbb|cccc", []step{{10, 14, "CC"}, {5, 9, "B"}, {3, 9, "Z"}}, []out{{3, "a|bbbb|cccc", "Z"}}},
		{"not into a hunk that is not a quote", "abc", []step{{-1, 0, ""}, {0, 3, "x"}}, []out{{-1, "abc", "abc"}, {0, "abc", "x"}}},
		{"nor a hunk that is not a quote into one", "abc", []step{{0, 1, "x"}, {-1, 0, ""}}, []out{{0, "a", "x"}, {-1, "xbc", "xbc"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var u undoing
			state := []byte(c.s)
			for _, st := range c.steps {
				if st.a < 0 {
					u.push(Hunk{Op: OpReplace, Path: "f", Old: slices.Clone(state), New: slices.Clone(state), Count: 1}, -1)
					continue
				}
				u.push(Hunk{Op: OpReplace, Path: "f", Old: slices.Clone(state[st.a:st.b]), New: []byte(st.back), Count: 1}, st.a)
				state = slices.Concat(state[:st.a], []byte(st.back), state[st.b:])
			}
			var got []out
			for i, h := range u.hs {
				got = append(got, out{u.at[i], string(h.Old), string(h.New)})
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("hunks %+v, want %+v", got, c.want)
			}
			b := []byte(c.s)
			for i, h := range u.hs {
				at := u.at[i]
				if at < 0 {
					at = bytes.Index(b, h.Old)
				}
				if at < 0 || !bytes.HasPrefix(b[at:], h.Old) || h.Op != OpReplace || h.Path != "f" || h.Count != 1 {
					t.Fatalf("hunk %d, %q, is not at %d in %q", i, h.Old, at, b)
				}
				b = slices.Concat(b[:at], h.New, b[at+len(h.Old):])
			}
			if !bytes.Equal(b, state) {
				t.Errorf("the hunks make %q, the steps %q", b, state)
			}
		})
	}
}

// A directory a create made is left, empty, when the inverse deletes the
// file, as hunk invert's is: a delete removes a file.
func TestInverseToLeavesAMadeDirectory(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "a\n"})
	inv := filepath.Join(t.TempDir(), "undo.hunk")
	code, out, errOut := runCLI(t, root, []string{"--json", "--inverse-to", inv}, "@@ create n/new.txt\nhello\n")
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if code, out, errOut = undoWith(t, inverseOf(t, out).Inverse.Args); code != exitOK {
		t.Fatalf("undo: exit %d: %s%s", code, out, errOut)
	}
	entries, err := os.ReadDir(filepath.Join(root, "n"))
	if err != nil || len(entries) != 0 {
		t.Errorf("n should be left, empty: %v %v", entries, err)
	}
}

// Emptied and made again, the file keeps its mode: the inverse's delete and
// create commit as one modify.
func TestInverseToKeepsAnEmptiedFilesMode(t *testing.T) {
	needsPOSIXPerms(t)
	root := cliTree(t, map[string]string{"run.sh": "echo hi\n"})
	must(t, os.Chmod(filepath.Join(root, "run.sh"), 0o755))
	before := snapshot(t, root)
	inv := filepath.Join(t.TempDir(), "undo.hunk")
	code, out, errOut := runCLI(t, root, []string{"--json", "--inverse-to", inv}, "@@ file run.sh\n@@ old\necho hi\n\n@@ new\n")
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if code, out, errOut = undoWith(t, inverseOf(t, out).Inverse.Args); code != exitOK {
		t.Fatalf("undo: exit %d: %s%s", code, out, errOut)
	}
	assertUnchanged(t, root, before)
}

// Outside the root, as the batch was: the inverse is applied with
// --allow-outside-root too.
func TestInverseToOutsideTheRoot(t *testing.T) {
	needsSymlinks(t)
	dir := cliTree(t, map[string]string{"out.txt": "outside\n", "in/a.txt": "a\n"})
	root := filepath.Join(dir, "in")
	inv := filepath.Join(t.TempDir(), "undo.hunk")
	code, out, errOut := runCLI(t, root, []string{"--json", "--allow-outside-root", "--inverse-to", inv},
		"@@ file ../out.txt\n@@ old\noutside\n@@ new\nOUT\n")
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	r := inverseOf(t, out)
	if want := []string{"--root", root, "--allow-outside-root", "-f", inv}; !slices.Equal(r.Inverse.Args, want) {
		t.Errorf("args %q, want %q", r.Inverse.Args, want)
	}
	if code, out, errOut = undoWith(t, r.Inverse.Args); code != exitOK {
		t.Fatalf("undo: exit %d: %s%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out.txt")); string(b) != "outside\n" {
		t.Errorf("out.txt = %q", b)
	}
}

// TestInverseToStaysWithTheBatch: the inverse is on disk exactly when the
// batch is, and the report gives its line exactly then.
func TestInverseToStaysWithTheBatch(t *testing.T) {
	const patch = "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n"
	for _, c := range []struct {
		name     string
		flags    []string
		code     int
		stays    bool
		lineIn   string // "stdout", "stderr", or "" for no line
		alsoSays string
	}{
		{"no verify", nil, exitOK, true, "stdout", ""},
		{"a verify that passes", []string{"--verify", "grep -q ALPHA a.txt"}, exitOK, true, "stdout", "verify ok"},
		{"a verify that fails: rolled back, and the inverse with it", []string{"--verify", "false"}, exitVerifyFailed, false, "", "rolled back 1 file"},
		{"--keep-on-fail: the batch stays, and the inverse", []string{"--verify", "false", "--keep-on-fail"}, exitVerifyFailed, true, "stderr", "--keep-on-fail"},
		{"--quiet: the inverse, and nothing said", []string{"--quiet"}, exitOK, true, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
			inv := filepath.Join(t.TempDir(), "undo.hunk")
			code, out, errOut := runCLI(t, root, append([]string{"--inverse-to", inv}, c.flags...), patch)
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s%s", code, c.code, out, errOut)
			}
			_, err := os.Stat(inv)
			if (err == nil) != c.stays {
				t.Errorf("the inverse is there: %v, want %v", err == nil, c.stays)
			}
			line := "to undo: hunk --root " + shellQuote(root) + " -f " + shellQuote(inv) + "\n"
			for where, text := range map[string]string{"stdout": out, "stderr": errOut} {
				if has := strings.Contains(text, line); has != (where == c.lineIn) {
					t.Errorf("%s has the line: %v; stdout %q stderr %q", where, has, out, errOut)
				}
			}
			if !strings.Contains(out+errOut, c.alsoSays) {
				t.Errorf("lacks %q: %s%s", c.alsoSays, out, errOut)
			}
			if c.lineIn == "" && c.stays && out+errOut != "" {
				t.Errorf("--quiet printed %q %q", out, errOut)
			}
		})
	}

	t.Run("a verify that cannot start: rolled back, and the inverse with it", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
		inv := filepath.Join(t.TempDir(), "undo.hunk")
		broken := brokenShell(t)
		t.Setenv("PATH", os.Getenv("PATH"))
		orig := notifyInterrupts
		t.Cleanup(func() { notifyInterrupts = orig })
		notifyInterrupts = func(c chan<- os.Signal) {
			orig(c)
			must(t, os.Setenv("PATH", broken))
		}
		code, out, errOut := runCLI(t, root, []string{"--inverse-to", inv, "--verify", "true"}, patch)
		if code != exitIO || !strings.Contains(errOut, "could not run the verify command") {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if _, err := os.Stat(inv); err == nil {
			t.Error("the inverse of a batch rolled back is still there")
		}
	})

	t.Run("a signal during the verify: rolled back, and the inverse with it", func(t *testing.T) {
		needsSignals(t)
		// The grace for the command's group to go, which the sleep would
		// otherwise take whole.
		withDelays(t, 200*time.Millisecond, 200*time.Millisecond)
		root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
		inv := filepath.Join(t.TempDir(), "undo.hunk")
		var out, errOut bytes.Buffer
		var sig os.Signal
		code := invoke([]string{"--root", root, "--inverse-to", inv, "--verify", "kill -TERM $PPID; sleep 5"},
			strings.NewReader(patch), &out, &errOut, &sig)
		if code != 143 || sig != syscall.SIGTERM {
			t.Fatalf("exit %d, signal %v: %s%s", code, sig, out.String(), errOut.String())
		}
		if _, err := os.Stat(inv); err == nil {
			t.Error("the inverse of a batch rolled back is still there")
		}
	})
}

// TestInverseToRefusals: each at exit 1 with nothing written, the inverse
// included, and named.
func TestInverseToRefusals(t *testing.T) {
	const patch = "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n"
	for _, c := range []struct {
		name  string
		files map[string]string
		args  func(inv string) []string
		patch string
		setup func(t *testing.T, root, inv string)
		says  func(root, inv string) string
	}{
		{
			name: "with --try",
			args: func(inv string) []string { return []string{"--inverse-to", inv, "--try", "true"} },
			says: func(_, _ string) string {
				return "--inverse-to writes the patch that undoes a batch that stays, and --try puts every file back"
			},
		},
		{
			name: "with --try --each",
			args: func(inv string) []string {
				return []string{"--inverse-to", inv, "--try", "true", "--each", "/nonexistent/p.hunk"}
			},
			says: func(_, _ string) string {
				return "--inverse-to writes the patch that undoes a batch that stays, and --try puts every file back"
			},
		},
		{
			name: "with --dry-run",
			args: func(inv string) []string { return []string{"--inverse-to", inv, "--dry-run"} },
			says: func(_, _ string) string {
				return "--inverse-to writes the patch that undoes a batch, and --dry-run writes nothing"
			},
		},
		{
			name:  "a file already there",
			args:  func(inv string) []string { return []string{"--inverse-to", inv} },
			setup: func(t *testing.T, _, inv string) { must(t, os.WriteFile(inv, []byte("@@ file a.txt\n"), 0o644)) },
			says: func(_, inv string) string {
				return "--inverse-to " + inv + ": it already exists, and may be the inverse of an earlier batch; apply it or remove it, or name another file"
			},
		},
		{
			name: "a link already there, leading nowhere",
			args: func(inv string) []string { return []string{"--inverse-to", inv} },
			setup: func(t *testing.T, _, inv string) {
				needsSymlinks(t)
				must(t, os.Symlink(filepath.Join(filepath.Dir(inv), "nowhere"), inv))
			},
			says: func(_, inv string) string {
				return "--inverse-to " + inv + ": it already exists"
			},
		},
		{
			name:  "a file the batch creates",
			args:  func(string) []string { return []string{"--inverse-to", "PLACEHOLDER"} },
			patch: "@@ create n.txt\nhi\n",
			says: func(_, _ string) string {
				return "--inverse-to cannot write the patch that undoes this batch: it names n.txt, which the batch writes; name a file outside it; nothing was written"
			},
		},
		{
			name:  "a delete of a file whose mode a create cannot give back",
			files: map[string]string{"a.txt": "alpha\n", "run.sh": "echo hi\n"},
			args:  func(inv string) []string { return []string{"--inverse-to", inv} },
			patch: "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n@@ delete run.sh\n",
			setup: func(t *testing.T, root, _ string) {
				needsPOSIXPerms(t)
				must(t, os.Chmod(filepath.Join(root, "run.sh"), 0o755))
			},
			says: func(_, _ string) string {
				return "--inverse-to cannot undo hunk 2 (patch line 6): it deletes run.sh, whose mode is 0755, and the @@ create that would put it back makes a file 0644; nothing was written"
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := c.files
			if files == nil {
				files = map[string]string{"a.txt": "alpha\n"}
			}
			root := cliTree(t, files)
			inv := filepath.Join(t.TempDir(), "undo.hunk")
			if c.name == "a file the batch creates" {
				inv = filepath.Join(root, "n.txt")
			}
			if c.setup != nil {
				c.setup(t, root, inv)
			}
			before := snapshot(t, root)
			p := patch
			if c.patch != "" {
				p = c.patch
			}
			args := c.args(inv)
			for i, a := range args {
				if a == "PLACEHOLDER" {
					args[i] = inv
				}
			}
			invBefore, _ := os.Lstat(inv)
			code, out, errOut := runCLI(t, root, args, p)
			if code != exitUsage || out != "" || !strings.HasPrefix(errOut, "hunk: "+c.says(root, inv)) {
				t.Errorf("exit %d, stdout %q, stderr %q\nwant %q", code, out, errOut, c.says(root, inv))
			}
			assertUnchanged(t, root, before)
			if invAfter, err := os.Lstat(inv); (invBefore == nil) != errors.Is(err, os.ErrNotExist) {
				t.Errorf("the inverse's path was %v and is %v", invBefore, invAfter)
			}
		})
	}

	t.Run("under --json, the refusal is the object's error", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
		code, out, _ := runCLI(t, root, []string{"--json", "--inverse-to", filepath.Join(t.TempDir(), "u"), "--dry-run"}, patch)
		if r := inverseOf(t, out); code != exitUsage || r.Exit != exitUsage || !strings.Contains(r.Error, "--dry-run writes nothing") {
			t.Errorf("exit %d: %s", code, out)
		}
	})
}

// A file that cannot be written stops the batch with nothing applied.
func TestInverseToCannotBeWritten(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
	before := snapshot(t, root)
	inv := filepath.Join(t.TempDir(), "missing", "undo.hunk")
	code, out, errOut := runCLI(t, root, []string{"--inverse-to", inv}, "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n")
	if code != exitIO || out != "" || !strings.HasPrefix(errOut, "hunk: could not write the inverse to "+inv+": ") ||
		!strings.HasSuffix(errOut, "; nothing was written\n") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	assertUnchanged(t, root, before)
}

// A commit that fails takes the inverse with it: the batch is not on disk.
func TestInverseToCommitFails(t *testing.T) {
	needsPOSIXPerms(t)
	root := cliTree(t, map[string]string{"d/a.txt": "alpha\n"})
	must(t, os.Chmod(filepath.Join(root, "d"), 0o555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "d"), 0o755) })
	inv := filepath.Join(t.TempDir(), "undo.hunk")
	code, out, errOut := runCLI(t, root, []string{"--inverse-to", inv}, "@@ file d/a.txt\n@@ old\nalpha\n@@ new\nALPHA\n")
	if code != exitIO {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if _, err := os.Stat(inv); err == nil {
		t.Error("the inverse of a batch that was not written is still there")
	}
}

// A name that reaches a file the batch creates another way, in another case,
// passes the name check and meets the check before the commit, which refuses
// the inverse as the second writer it then is, and takes it back out.
func TestInverseToInAnotherCaseMeetsTheCheck(t *testing.T) {
	if !foldsCase(t) {
		t.Skip("this filesystem tells N.TXT from n.txt")
	}
	root := cliTree(t, map[string]string{"a.txt": "a\n"})
	inv := filepath.Join(root, "N.TXT")
	code, out, errOut := runCLI(t, root, []string{"--inverse-to", inv}, "@@ create n.txt\nhi\n")
	if code != exitChanged {
		t.Fatalf("exit %d, want 6: %s%s", code, out, errOut)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 1 {
		t.Errorf("the tree holds %v, want a.txt alone", entries)
	}
}

// TestInverseToQuietSaysWhatTheInverseNeeds: --quiet prints nothing for an
// inverse that applies with -f alone, and the line for one that needs more.
func TestInverseToQuietSaysWhatTheInverseNeeds(t *testing.T) {
	root := cliTree(t, map[string]string{"m.txt": "b\r\nb\nc\n"})
	inv := filepath.Join(t.TempDir(), "undo.hunk")
	code, out, errOut := runCLI(t, root, []string{"--quiet", "--inverse-to", inv}, "@@ file m.txt\n@@ old\nc\n@@ new\nb\n")
	want := "to undo: hunk --root " + shellQuote(root) + " --eol strict -f " + shellQuote(inv) + "\n"
	if code != exitOK || out != "" || errOut != want {
		t.Errorf("exit %d, stdout %q, stderr %q, want %q", code, out, errOut, want)
	}
}

// A batch that changes nothing has nothing to undo, and a patch with no hunks
// is refused, so no file is written and the report says so.
func TestInverseToNothingToUndo(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "a\n"})
	inv := filepath.Join(t.TempDir(), "undo.hunk")
	patch := "@@ file a.txt\n@@ old\na\n@@ new\na\n"
	code, out, _ := runCLI(t, root, []string{"--inverse-to", inv}, patch)
	if code != exitOK || !strings.Contains(out, "nothing to undo: the batch changed nothing, so no inverse was written to "+inv+"\n") {
		t.Errorf("exit %d: %q", code, out)
	}
	if _, err := os.Stat(inv); err == nil {
		t.Error("an inverse was written")
	}
	code, out, _ = runCLI(t, root, []string{"--json", "--inverse-to", inv}, patch)
	if r := inverseOf(t, out); code != exitOK || r.Inverse == nil || !r.Inverse.Nothing || r.Inverse.Args != nil {
		t.Errorf("exit %d: %s", code, out)
	}
}

// The ordinary apply records nothing for an inverse it was not asked for:
// --inverse-to costs a batch without it no memory and no scan.
func TestTheApplyRecordsNoEditsUnasked(t *testing.T) {
	tree, _ := fixture(t, map[string]string{"a.txt": "a\nb\n"})
	x := NewTxn(tree, Options{})
	p := &Patch{Hunks: []Hunk{
		{Op: OpReplace, Path: "a.txt", Line: 1, Count: 1, Old: []byte("a"), New: []byte("A")},
		{Op: OpAppend, Path: "a.txt", Line: 2, Body: []byte("c")},
	}}
	if _, err := x.Run(p); err != nil {
		t.Fatal(err)
	}
	if x.edits != nil || x.inverse != nil {
		t.Errorf("edits %v, inverse %v", x.edits, x.inverse)
	}
}

func TestWiden(t *testing.T) {
	for _, c := range []struct {
		name     string
		s        string
		lo, hi   int
		up, down bool
		want     string
	}{
		{"a unique line", "a\nb\nc\n", 2, 3, true, true, "b\n"},
		{"inside a line, the whole line", "say hello\nx\n", 4, 9, true, true, "say hello\n"},
		{"a repeated line, one each way", "x\nb\nx\nc\n", 4, 5, true, true, "b\nx\nc\n"},
		{"a place, the line after it", "a\nb\n", 2, 2, true, true, "b\n"},
		{"the end of the file, the line before it", "a\nb\n", 4, 4, true, true, "b\n"},
		{"up only", "x\nx\nx\n", 4, 6, true, false, "x\nx\nx\n"},
		{"down only", "x\nx\ny\n", 0, 2, false, true, "x\nx\n"},
		{"down only, as far as it goes", "x\nx\n", 0, 2, false, true, "x\nx\n"},
		{"no final newline", "a\nb", 2, 3, true, true, "b"},
		{"CRLF lines stay whole", "a\r\nb\r\na\r\n", 6, 7, true, true, "b\r\na\r\n"},
		{"overlapping copies are not once", "a\na\na\nz\n", 4, 6, true, false, "a\na\na\n"},
		// Up only, from a line that is not the last, stops at the top once
		// or not: no caller asks it, since append's quote ends at the end of
		// the file, and without the stop it would never end.
		{"as far as up goes, not once", "x\nx\nx\nx\n", 2, 4, true, false, "x\nx\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, b := widen([]byte(c.s), c.lo, c.hi, c.up, c.down)
			if got := c.s[a:b]; got != c.want {
				t.Errorf("widen = %q, want %q", got, c.want)
			}
		})
	}
	// Doubling then halving finds the fewest lines: a run of identical lines
	// with one different line at a known distance.
	t.Run("the fewest lines, found by halving", func(t *testing.T) {
		s := []byte("z\n" + strings.Repeat("x\n", 37) + "y\n" + strings.Repeat("x\n", 50))
		q := 2 + 37*2 + 2 + 20*2 // the 21st x after y
		a, b := widen(s, q, q+2, true, false)
		if got := string(s[a:b]); got != "y\n"+strings.Repeat("x\n", 21) {
			t.Errorf("widen = %q", got)
		}
	})
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"undo.hunk":   "undo.hunk",
		"/tmp/a-b_c":  "/tmp/a-b_c",
		"--eol":       "--eol",
		"@@@":         "@@@",
		"two words":   "'two words'",
		"it's":        `'it'\''s'`,
		"":            "''",
		"$HOME":       "'$HOME'",
		"a\nb":        "'a\nb'",
		`C:\x\y.hunk`: `'C:\x\y.hunk'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// FuzzInverseToUndoesTheBatch: two replacements and an end op on a real
// tree, the batch's inverse written and then applied with the flags it
// asked for. A batch that applies has an inverse that puts the file back
// exactly; the only refusal allowed is a file that holds directives under
// every marker tried.
func FuzzInverseToUndoesTheBatch(f *testing.F) {
	for _, c := range [][5]string{
		{"fn a() {\n    return x;\n}\nfn b() {\n    if (y) return x;\n    return x;\n}\n", "    if (y) return x;\n    return x;\n", "    return x;\n", "a", "a"},
		{"a\nb\nc\nb\n", "c\n", "", "b", "B"},
		{"x\nX\nx\n", "x", "X", "X", "x"},
		{"a\r\nb\r\n", "a\n", "A\n", "b", ""},
		{"b\r\nb\nc\n", "c", "b", "b", "c"},
		{"@@ old\nv\n@@ old\nw\n", "w", "v", "v", "w"},
		{"keep", "keep", "kept", "kept", "k"},
		{"a a a a\nb\n", "a", "b", "b b", "c"},
		{"", "", "", "", ""},
	} {
		f.Add([]byte(c[0]), []byte(c[1]), []byte(c[2]), []byte(c[3]), []byte(c[4]), byte(0), false)
		f.Add([]byte(c[0]), []byte(c[1]), []byte(c[2]), []byte(c[3]), []byte(c[4]), byte(1), true)
		f.Add([]byte(c[0]), []byte(c[1]), []byte(c[2]), []byte(c[3]), []byte(c[4]), byte(2), false)
		f.Add([]byte(c[0]), []byte(c[1]), []byte(c[2]), []byte(c[3]), []byte(c[4]), byte(3), false)
	}
	f.Fuzz(func(t *testing.T, start, a, b, c, d []byte, end byte, strict bool) {
		opt := Options{}
		if strict {
			opt.EOL = EOLStrict
		}
		var hunks []Hunk
		if n := bytes.Count(start, a); len(a) > 0 && n > 0 {
			hunks = append(hunks, Hunk{Op: OpReplace, Path: "a.txt", Line: 1, Count: n, Old: a, New: b})
			if m := bytes.Count(bytes.Replace(start, a, b, n), c); len(c) > 0 && m > 0 {
				hunks = append(hunks, Hunk{Op: OpReplace, Path: "a.txt", Line: 2, Count: m, Old: c, New: d})
			}
		}
		switch end % 4 {
		case 1:
			hunks = append(hunks, Hunk{Op: OpAppend, Path: "a.txt", Line: 3, Body: d})
		case 2:
			hunks = append(hunks, Hunk{Op: OpPrepend, Path: "a.txt", Line: 3, Body: d})
		case 3:
			hunks = append(hunks, Hunk{Op: OpDelete, Path: "a.txt", Line: 3})
		}
		if len(hunks) == 0 {
			return
		}
		tree, root := fixture(t, map[string]string{"a.txt": string(start)})
		inv := filepath.Join(t.TempDir(), "undo.hunk")
		x := NewTxn(tree, opt)
		x.inverseTo, x.inverseMarker = inv, DefaultMarker
		_, err := x.Run(&Patch{Hunks: hunks})
		var ie *InverseError
		switch {
		case errors.As(err, &ie):
			// Or a delete, where the umask made the fixture's file another
			// mode than a create gives.
			if !strings.Contains(ie.Why, "read as directives under every marker") && !strings.Contains(ie.Why, "whose mode is") {
				t.Fatalf("refused: %v", err)
			}
			return
		case err != nil:
			return // the batch itself was refused
		case x.inverse.Nothing:
			if now, _ := os.ReadFile(filepath.Join(root, "a.txt")); !bytes.Equal(now, start) {
				t.Fatalf("nothing to undo, but a.txt is %q, was %q", now, start)
			}
			return
		}
		src, err := os.ReadFile(inv)
		must(t, err)
		marker, uopt := DefaultMarker, Options{}
		for i, arg := range x.inverse.Args {
			switch arg {
			case "--marker":
				marker = x.inverse.Args[i+1]
			case "strict":
				uopt.EOL = EOLStrict
			}
		}
		p, err := Parse(src, marker)
		if err != nil {
			t.Fatalf("the inverse does not parse: %v\n%q", err, src)
		}
		if _, err := NewTxn(tree, uopt).Run(p); err != nil {
			t.Fatalf("the inverse does not apply: %v\n%q", err, src)
		}
		now, err := os.ReadFile(filepath.Join(root, "a.txt"))
		if err != nil || !bytes.Equal(now, start) {
			t.Fatalf("a.txt is %q (%v), was %q\ninverse %q", now, err, start, src)
		}
	})
}

// TestInverseToGoldens are the issue's inverse and report, and the refusals
// whose words are the contract.
func TestInverseToGoldens(t *testing.T) {
	t.Run("the report", func(t *testing.T) {
		res := &Result{Hunks: 1, Files: []FileResult{{Path: "f.zig", Op: "modify", Removed: 1}}}
		rep := NewReport(res, nil, nil, false, 1)
		rep.Inverse = &Inverse{Path: "undo.hunk", Args: []string{"-f", "undo.hunk"}}
		var out, errOut bytes.Buffer
		rep.Text(&out, &errOut, false)
		golden(t, "report-inverse-to", out.String()+errOut.String())
		var j bytes.Buffer
		must(t, rep.JSON(&j))
		golden(t, "report-inverse-to-json", j.String())
	})

	// inDir runs f with the working directory a temporary one, so that the
	// inverse's name is relative and the same every run, and returns what
	// it gives: the golden is read after, from the package's own directory.
	inDir := func(t *testing.T, f func(t *testing.T) string) string {
		var got string
		t.Run("in a directory of its own", func(t *testing.T) {
			t.Chdir(t.TempDir())
			got = f(t)
		})
		return got
	}

	t.Run("the issue's inverse", func(t *testing.T) {
		src := inDir(t, func(t *testing.T) string {
			root := cliTree(t, map[string]string{"f.zig": "fn a() {\n    return x;\n}\nfn b() {\n    if (y) return x;\n    return x;\n}\n"})
			code, _, errOut := runCLI(t, root, []string{"--inverse-to", "undo.hunk"},
				"@@ file f.zig\n@@ old\n    if (y) return x;\n    return x;\n@@ new\n    return x;\n")
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			src, err := os.ReadFile("undo.hunk")
			must(t, err)
			return string(src)
		})
		golden(t, "inverse-to-issue-155", src)
	})

	for _, c := range []struct{ name, golden, patch string }{
		{"already there", "cli-inverse-to-exists", "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n"},
		{"with --try", "cli-inverse-to-try", "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			errOut := inDir(t, func(t *testing.T) string {
				root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
				args := []string{"--inverse-to", "undo.hunk"}
				if c.golden == "cli-inverse-to-exists" {
					must(t, os.WriteFile("undo.hunk", []byte("x"), 0o644))
				} else {
					args = append(args, "--try", "true")
				}
				code, _, errOut := runCLI(t, root, args, c.patch)
				if code != exitUsage {
					t.Fatalf("exit %d: %s", code, errOut)
				}
				return errOut
			})
			golden(t, c.golden, errOut)
		})
	}

	t.Run("a mode a create cannot give back", func(t *testing.T) {
		needsPOSIXPerms(t)
		root := cliTree(t, map[string]string{"run.sh": "echo hi\n"})
		must(t, os.Chmod(filepath.Join(root, "run.sh"), 0o755))
		code, _, errOut := runCLI(t, root, []string{"--inverse-to", filepath.Join(t.TempDir(), "u")}, "@@ delete run.sh\n")
		if code != exitUsage {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		golden(t, "cli-inverse-to-mode", errOut)
	})
}

// The --help entry, the README and the skill name the flag, and hunk
// invert's help points at it for what invert cannot undo.
func TestTheDocsNameInverseTo(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	must(t, err)
	skill, err := os.ReadFile(filepath.Join(".agents", "skills", "hunk", "SKILL.md"))
	must(t, err)
	for doc, text := range map[string]string{
		"--help": usage, "hunk invert --help": invertUsage, "README.md": string(readme), "SKILL.md": string(skill),
	} {
		if !strings.Contains(text, "--inverse-to") {
			t.Errorf("%s does not name --inverse-to", doc)
		}
	}
}

// A file that holds a directive under every marker the inverse tries cannot
// have one written: refused, exit 1, nothing written.
func TestInverseToRunsOutOfMarkers(t *testing.T) {
	var block strings.Builder
	for i := range markerTries {
		block.WriteString(DefaultMarker + strings.Repeat("@", i) + " old\n")
	}
	// The block twice, so the last change's quote has to climb through all
	// of it to the line above it.
	root := cliTree(t, map[string]string{"d.md": block.String() + "v\n" + block.String() + "w\n"})
	before := snapshot(t, root)
	inv := filepath.Join(t.TempDir(), "undo.hunk")
	code, out, errOut := runCLI(t, root, []string{"--inverse-to", inv}, "@@ file d.md\n@@ old\nw\n@@ new\nv\n")
	want := "hunk: --inverse-to cannot write the patch that undoes this batch: the files hold lines that read as directives under every marker from @@ to @@@@@@@@@; nothing was written\n"
	if code != exitUsage || out != "" || errOut != want {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	assertUnchanged(t, root, before)
	if _, err := os.Stat(inv); err == nil {
		t.Error("an inverse was written")
	}
}

// fullDiskFile is a file whose writes fail, as on a full disk.
type fullDiskFile struct{ f *os.File }

func (w fullDiskFile) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }
func (w fullDiskFile) Close() error              { return w.f.Close() }

// A write that fails after the file is made takes the file back out, and
// the batch with nothing applied.
func TestInverseToWriteFailsPartWay(t *testing.T) {
	orig := openInverse
	t.Cleanup(func() { openInverse = orig })
	openInverse = func(name string) (io.WriteCloser, error) {
		f, err := orig(name)
		if err != nil {
			return nil, err
		}
		return fullDiskFile{f.(*os.File)}, nil
	}
	root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
	before := snapshot(t, root)
	inv := filepath.Join(t.TempDir(), "undo.hunk")
	code, _, errOut := runCLI(t, root, []string{"--inverse-to", inv}, "@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n")
	if want := "hunk: could not write the inverse to " + inv + ": no space left on device; nothing was written\n"; code != exitIO || errOut != want {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	assertUnchanged(t, root, before)
	if _, err := os.Stat(inv); err == nil {
		t.Error("the part-written inverse is still there")
	}
}

// An inverse that cannot be removed when the batch does not stay is said,
// since it would read as an undo still to do.
func TestInverseToCannotBeRemoved(t *testing.T) {
	needsPOSIXPerms(t)
	root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
	dir := t.TempDir()
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	inv := filepath.Join(dir, "undo.hunk")
	code, _, errOut := runCLI(t, root, []string{"--inverse-to", inv, "--verify", "chmod 555 '" + dir + "'; false"},
		"@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n")
	if code != exitVerifyFailed || !strings.HasPrefix(errOut, "hunk: the batch did not stay, and its inverse could not be removed: ") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

// The replay is what refuses an inverse that does not put the files back:
// unreachable from a batch, so fed one here.
func TestTheReplayRefusesAWrongInverse(t *testing.T) {
	tree, _ := fixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	x := NewTxn(tree, Options{})
	x.inverseTo, x.inverseMarker = filepath.Join(t.TempDir(), "u"), DefaultMarker
	p := &Patch{Hunks: []Hunk{{Op: OpReplace, Path: "a.txt", Line: 1, Count: 1, Old: []byte("a"), New: []byte("A")}}}
	must(t, x.Load(p))
	if f := x.Validate(p); len(f) > 0 {
		t.Fatal(f)
	}
	for _, c := range []struct {
		name  string
		hunks []Hunk
	}{
		{"it changes the file to something else", []Hunk{{Op: OpReplace, Path: "a.txt", Count: 1, Old: []byte("A"), New: []byte("c")}}},
		{"it deletes the file", []Hunk{{Op: OpDelete, Path: "a.txt"}}},
		{"it does not apply", []Hunk{{Op: OpReplace, Path: "a.txt", Count: 1, Old: []byte("zzz"), New: []byte("a")}}},
		{"it reaches a file the batch did not", []Hunk{
			{Op: OpReplace, Path: "a.txt", Count: 1, Old: []byte("A"), New: []byte("a")},
			{Op: OpReplace, Path: "b.txt", Count: 1, Old: []byte("b"), New: []byte("b")},
		}},
		{"it does not load", []Hunk{{Op: OpReplace, Path: "../outside.txt", Count: 1, Old: []byte("A"), New: []byte("a")}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := x.inversePatch(c.hunks)
			var ie *InverseError
			if !errors.As(err, &ie) || !strings.Contains(ie.Why, "does not put every file back as it was, which is a bug in hunk") {
				t.Errorf("err = %v", err)
			}
		})
	}
	t.Run("and passes the right one", func(t *testing.T) {
		if _, args, err := x.inversePatch([]Hunk{{Op: OpReplace, Path: "a.txt", Count: 1, Old: []byte("A"), New: []byte("a")}}); err != nil || len(args) != 0 {
			t.Errorf("args %q, err %v", args, err)
		}
	})
}
