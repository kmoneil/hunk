package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// quoteCLI runs "hunk quote" in process against a real tree.
func quoteCLI(t *testing.T, root string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli(append([]string{"quote", "--root", root}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// newFile writes a --new file outside the tree, where the caller's files are.
func newFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "new.txt")
	must(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

const tenLines = "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9\nline 10\n"

// The payloads, byte for byte. Each row is a rule quoteLines's comment states,
// or a case issue #82's script decided by accident.
func TestQuoteLines(t *testing.T) {
	for _, c := range []struct {
		name        string
		file        string
		start, end  int
		repl        string
		hasNew      bool
		marker      string
		want        string
		first, last string
	}{
		{
			name: "a range with no --new is file, old and the lines, and stops",
			file: "a\nb\nc\nd\n", start: 2, end: 3,
			want:  "@@ file f.txt\n@@ old\nb\nc\n",
			first: "b", last: "c",
		},
		{
			name: "--new adds new and the replacement",
			file: "a\nb\nc\nd\n", start: 2, end: 3, repl: "B\nC\n", hasNew: true,
			want:  "@@ file f.txt\n@@ old\nb\nc\n@@ new\nB\nC\n",
			first: "b", last: "c",
		},
		{
			// Issue #82's script welded the next directive onto this one.
			name: "a replacement with no final newline comes out the same",
			file: "a\nb\nc\nd\n", start: 2, end: 3, repl: "B\nC", hasNew: true,
			want:  "@@ file f.txt\n@@ old\nb\nc\n@@ new\nB\nC\n",
			first: "b", last: "c",
		},
		{
			name: "a replacement ending in CRLF loses both bytes of it",
			file: "a\nb\nc\n", start: 2, end: 2, repl: "B\r\nC\r\n", hasNew: true,
			want:  "@@ file f.txt\n@@ old\nb\n@@ new\nB\r\nC\n",
			first: "b", last: "b",
		},
		{
			name: "only one line ending is taken from the replacement",
			file: "a\nb\nc\n", start: 2, end: 2, repl: "B\n\n", hasNew: true,
			want:  "@@ file f.txt\n@@ old\nb\n@@ new\nB\n\n",
			first: "b", last: "b",
		},
		{
			name: "a replacement of one empty line is an empty new",
			file: "a\nb\nc\n", start: 2, end: 2, repl: "\n", hasNew: true,
			want:  "@@ file f.txt\n@@ old\nb\n@@ new\n\n",
			first: "b", last: "b",
		},
		{
			// With the lines' own ending in old, so no blank line is left.
			name: "an empty replacement deletes the lines, endings and all",
			file: "a\nb\nc\nd\n", start: 2, end: 3, repl: "", hasNew: true,
			want:  "@@ file f.txt\n@@ old\nb\nc\n\n@@ new\n\n",
			first: "b", last: "c",
		},
		{
			name: "deleting through an unterminated last line has no ending to take",
			file: "a\nb\nc", start: 2, end: 3, repl: "", hasNew: true,
			want:  "@@ file f.txt\n@@ old\nb\nc\n@@ new\n\n",
			first: "b", last: "c",
		},
		{
			// The script's sed printed the line unterminated and welded
			// "@@ new" onto it.
			name: "a range ending at an unterminated last line",
			file: "a\nb\nc", start: 3, end: 3, repl: "C\n", hasNew: true,
			want:  "@@ file f.txt\n@@ old\nc\n@@ new\nC\n",
			first: "c", last: "c",
		},
		{
			// The CR before the range's last LF is that line's ending and
			// stays in the file; the ones inside the range are old's bytes.
			name: "a CRLF file keeps the ending after the range",
			file: "one\r\ntwo\r\nthree\r\nfour\r\n", start: 2, end: 3, repl: "TWO\nTHREE\n", hasNew: true,
			want:  "@@ file f.txt\n@@ old\ntwo\r\nthree\n@@ new\nTWO\nTHREE\n",
			first: "two", last: "three",
		},
		{
			name: "deleting from a CRLF file takes the CRLF",
			file: "one\r\ntwo\r\nthree\r\n", start: 2, end: 2, repl: "", hasNew: true,
			want:  "@@ file f.txt\n@@ old\ntwo\r\n\n@@ new\n\n",
			first: "two", last: "two",
		},
		{
			name: "a CR that is not before the LF is the line's text",
			file: "a\rb\r\r\nc\n", start: 1, end: 1,
			want:  "@@ file f.txt\n@@ old\na\rb\r\n",
			first: "a\rb\r", last: "a\rb\r",
		},
		{
			name: "the whole file",
			file: "a\nb\n", start: 1, end: 2,
			want:  "@@ file f.txt\n@@ old\na\nb\n",
			first: "a", last: "b",
		},
		{
			name: "the first line alone",
			file: "a\nb\n", start: 1, end: 1,
			want:  "@@ file f.txt\n@@ old\na\n",
			first: "a", last: "a",
		},
		{
			name: "empty lines inside a range are old's newlines",
			file: "a\n\n\nb\n", start: 1, end: 4,
			want:  "@@ file f.txt\n@@ old\na\n\n\nb\n",
			first: "a", last: "b",
		},
		{
			name: "two empty lines are one newline, which is not nothing",
			file: "a\n\n\nb\n", start: 2, end: 3,
			want:  "@@ file f.txt\n@@ old\n\n\n",
			first: "", last: "",
		},
		{
			name: "one empty line can be deleted, since old is then its ending",
			file: "a\n\nb\n", start: 2, end: 2, repl: "", hasNew: true,
			want:  "@@ file f.txt\n@@ old\n\n\n@@ new\n\n",
			first: "", last: "",
		},
		{
			name: "another marker heads every directive",
			file: "a\nb\n", start: 2, end: 2, repl: "B\n", hasNew: true, marker: "%%",
			want:  "%% file f.txt\n%% old\nb\n%% new\nB\n",
			first: "b", last: "b",
		},
		{
			// Rule 3 of §3.2: none of these is a directive, so none is refused.
			// "@@old" was among them until 2026-10-18, when the parser began to
			// refuse a directive missing its space (#96).
			name: "lines that only look like directives are text",
			file: "@@ -1,3 +1,4 @@\n@@olds\n @@ old\n@@ older\n@@\n", start: 1, end: 5,
			repl: "@@ -1 +1 @@\n@@newer\n", hasNew: true,
			want:  "@@ file f.txt\n@@ old\n@@ -1,3 +1,4 @@\n@@olds\n @@ old\n@@ older\n@@\n@@ new\n@@ -1 +1 @@\n@@newer\n",
			first: "@@ -1,3 +1,4 @@", last: "@@",
		},
		{
			name: "under another marker, a directive missing its space under the default is text",
			file: "@@old\n@@file b.txt\n", start: 1, end: 2, repl: "@@new\n", hasNew: true, marker: "%%",
			want:  "%% file f.txt\n%% old\n@@old\n@@file b.txt\n%% new\n@@new\n",
			first: "@@old", last: "@@file b.txt",
		},
		{
			name: "under another marker the default one is text",
			file: "@@ old\n@@ delete b.txt\n", start: 1, end: 2, repl: "@@ new\n", hasNew: true, marker: "%%",
			want:  "%% file f.txt\n%% old\n@@ old\n@@ delete b.txt\n%% new\n@@ new\n",
			first: "@@ old", last: "@@ delete b.txt",
		},
		{
			name: "bytes that are not text pass through",
			file: "a\n\x00\xff\xfe\nb\n", start: 2, end: 2,
			want:  "@@ file f.txt\n@@ old\n\x00\xff\xfe\n",
			first: "\x00\xff\xfe", last: "\x00\xff\xfe",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := c.marker
			if marker == "" {
				marker = DefaultMarker
			}
			q, r := quoteLines("f.txt", []byte(c.file), c.start, c.end, []byte(c.repl), c.hasNew, "new.txt", marker)
			if r != nil {
				t.Fatalf("refused: %s", r.msg)
			}
			if string(q.Patch) != c.want {
				t.Errorf("patch\n got %q\nwant %q", q.Patch, c.want)
			}
			if string(q.First) != c.first || string(q.Last) != c.last {
				t.Errorf("first %q, last %q; want %q and %q", q.First, q.Last, c.first, c.last)
			}
		})
	}
}

// Each refusal by its exit class, the file and line it names, and its words.
func TestQuoteLinesRefuses(t *testing.T) {
	for _, c := range []struct {
		name       string
		file       string
		start, end int
		repl       string
		hasNew     bool
		marker     string
		code       int
		want       []string
	}{
		{
			name: "END past the last line names END",
			file: "a\nb\nc\n", start: 2, end: 5, code: exitNoMatch,
			want: []string{"f.txt has 3 lines", "there is no line 5"},
		},
		{
			name: "START past it names START, the first line that is not there",
			file: "a\nb\nc\n", start: 7, end: 9, code: exitNoMatch,
			want: []string{"f.txt has 3 lines", "there is no line 7"},
		},
		{
			name: "one line is one line",
			file: "a", start: 1, end: 2, code: exitNoMatch,
			want: []string{"f.txt has 1 line,", "there is no line 2"},
		},
		{
			name: "an empty file has no line 1",
			file: "", start: 1, end: 1, code: exitNoMatch,
			want: []string{"f.txt has 0 lines", "there is no line 1"},
		},
		{
			name: "one empty line would be an empty old",
			file: "a\n\nb\n", start: 2, end: 2, code: exitNoMatch,
			want: []string{"f.txt line 2 is empty", "matches at every position"},
		},
		{
			name: "and so would one in a CRLF file",
			file: "a\r\n\r\nb\r\n", start: 2, end: 2, repl: "x\n", hasNew: true, code: exitNoMatch,
			want: []string{"f.txt line 2 is empty"},
		},
		{
			name: "a directive in the range, by its line in the file",
			file: "a\nb\n@@ new\nd\n", start: 2, end: 4, code: exitNoMatch,
			want: []string{`f.txt line 3 is "@@ new"`, "reads as a directive", "--marker"},
		},
		{
			name: "a directive on the range's first line",
			file: "@@ delete b.txt\nb\n", start: 1, end: 2, code: exitNoMatch,
			want: []string{`f.txt line 1 is "@@ delete b.txt"`},
		},
		{
			name: "a directive on its last line",
			file: "a\n@@ end\n", start: 1, end: 2, code: exitNoMatch,
			want: []string{`f.txt line 2 is "@@ end"`},
		},
		{
			name: "a tab after the marker is a directive too",
			file: "a\n@@\tfile x\n", start: 1, end: 2, code: exitNoMatch,
			want: []string{`f.txt line 2 is "@@\tfile x"`},
		},
		{
			name: "and so is one ending in a CR",
			file: "a\r\n@@ old\r\nb\r\n", start: 1, end: 3, code: exitNoMatch,
			want: []string{`f.txt line 2 is "@@ old"`},
		},
		{
			// The mirror issue #82 did not name: this one was obeyed.
			name: "a directive in the replacement, by its line there",
			file: "a\nb\n", start: 1, end: 1, repl: "intro\n@@ delete b.txt\n", hasNew: true, code: exitNoMatch,
			want: []string{`new.txt line 2 is "@@ delete b.txt"`, "reads as a directive", "--marker"},
		},
		{
			name: "a directive on the replacement's last line, with no newline after it",
			file: "a\nb\n", start: 1, end: 1, repl: "x\n@@ old", hasNew: true, code: exitNoMatch,
			want: []string{`new.txt line 2 is "@@ old"`},
		},
		{
			name: "the range is refused before the replacement",
			file: "@@ old\n", start: 1, end: 1, repl: "@@ new\n", hasNew: true, code: exitNoMatch,
			want: []string{`f.txt line 1 is "@@ old"`},
		},
		{
			name: "under another marker, that marker's directives",
			file: "a\n%% old\n", start: 1, end: 2, marker: "%%", code: exitNoMatch,
			want: []string{`f.txt line 2 is "%% old"`},
		},
		{
			// The parser refuses this line wherever it is, payload included.
			name: "a directive with a control character stuck to it",
			file: "a\n@@ end\f\n", start: 1, end: 2, code: exitNoMatch,
			want: []string{`f.txt line 2 is "@@ end\f"`, "control character"},
		},
		{
			name: "the same in the replacement",
			file: "a\n", start: 1, end: 1, repl: "@@\x0bdelete a\n", hasNew: true, code: exitNoMatch,
			want: []string{"new.txt line 1 is", "control character"},
		},
		{
			// And so does a directive missing its space (#96).
			name: "a directive missing its space",
			file: "a\n@@old\n", start: 1, end: 2, code: exitNoMatch,
			want: []string{`f.txt line 2 is "@@old"`, "missing its space", "--marker"},
		},
		{
			name: "the same in the replacement",
			file: "a\n", start: 1, end: 1, repl: "x\n@@file b.txt\n", hasNew: true, code: exitNoMatch,
			want: []string{`new.txt line 2 is "@@file b.txt"`, "missing its space"},
		},
		{
			name: "a no-break space for the separator",
			file: "a\n@@\u00a0end\n", start: 1, end: 2, code: exitNoMatch,
			want: []string{"f.txt line 2 is", "one that is not ASCII"},
		},
		{
			name: "one ending in a CR",
			file: "a\r\n@@end\r\n", start: 1, end: 2, code: exitNoMatch,
			want: []string{`f.txt line 2 is "@@end"`, "missing its space"},
		},
		{
			name: "under another marker, that marker's",
			file: "a\n%%old\n", start: 1, end: 2, marker: "%%", code: exitNoMatch,
			want: []string{`f.txt line 2 is "%%old"`, "missing its space"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := c.marker
			if marker == "" {
				marker = DefaultMarker
			}
			q, r := quoteLines("f.txt", []byte(c.file), c.start, c.end, []byte(c.repl), c.hasNew, "new.txt", marker)
			if r == nil {
				t.Fatalf("not refused; printed %q", q.Patch)
			}
			if q != nil {
				t.Errorf("a refusal came with a patch: %q", q.Patch)
			}
			if r.code != c.code {
				t.Errorf("exit %d, want %d", r.code, c.code)
			}
			for _, w := range c.want {
				if !strings.Contains(r.msg, w) {
					t.Errorf("msg = %q, want it to say %q", r.msg, w)
				}
			}
		})
	}
}

// The rows issue #82's script got wrong or right by luck, on a real tree,
// through the real apply: quote, pipe, and read the file.
func TestAQuotedRangeAppliesAsWritten(t *testing.T) {
	for _, c := range []struct {
		name  string
		file  string
		quote []string // after FILE: START END and flags; "NEW" stands for the --new file
		repl  string
		more  string // patch text after the quote's
		apply []string
		want  string
	}{
		{
			name: "a block replaced", file: tenLines,
			quote: []string{"2", "3", "--new", "NEW"}, repl: "NEW A\nNEW B\nNEW C\n",
			want: "line 1\nNEW A\nNEW B\nNEW C\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9\nline 10\n",
		},
		{
			name: "a replacement with no final newline, and a hunk after it", file: tenLines,
			quote: []string{"2", "3", "--new", "NEW"}, repl: "NEW A\nNEW B",
			more: "@@ file f.txt\n@@ old\nline 9\n@@ new\nNINE\n",
			want: "line 1\nNEW A\nNEW B\nline 4\nline 5\nline 6\nline 7\nline 8\nNINE\nline 10\n",
		},
		{
			name: "an empty replacement leaves no blank line", file: tenLines,
			quote: []string{"2", "9", "--new", "NEW"}, repl: "",
			want: "line 1\nline 10\n",
		},
		{
			name: "the last lines deleted", file: tenLines,
			quote: []string{"9", "10", "--new", "NEW"}, repl: "",
			want: "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\n",
		},
		{
			name: "an unterminated last line stays unterminated", file: "a\nb\nc",
			quote: []string{"2", "3", "--new", "NEW"}, repl: "B\nC\n",
			want: "a\nB\nC",
		},
		{
			name: "deleting through an unterminated last line", file: "a\nb\nc",
			quote: []string{"2", "3", "--new", "NEW"}, repl: "",
			want: "a\n",
		},
		{
			// The script left "THREE\n" among CRLF lines, at exit 0.
			name: "a CRLF file stays CRLF under the default --eol", file: "one\r\ntwo\r\nthree\r\nfour\r\n",
			quote: []string{"2", "3", "--new", "NEW"}, repl: "TWO\nTHREE\n",
			want: "one\r\nTWO\r\nTHREE\r\nfour\r\n",
		},
		{
			name: "a CRLF replacement in an LF file becomes LF", file: "a\nb\nc\n",
			quote: []string{"2", "2", "--new", "NEW"}, repl: "B1\r\nB2\r\n",
			want: "a\nB1\nB2\nc\n",
		},
		{
			name: "lines deleted from a CRLF file", file: "one\r\ntwo\r\nthree\r\nfour\r\n",
			quote: []string{"2", "3", "--new", "NEW"}, repl: "",
			want: "one\r\nfour\r\n",
		},
		{
			name: "no --new, and a new typed the ordinary way", file: tenLines,
			quote: []string{"4", "5"},
			more:  "@@ new\nFOUR\nFIVE\n",
			want:  "line 1\nline 2\nline 3\nFOUR\nFIVE\nline 6\nline 7\nline 8\nline 9\nline 10\n",
		},
		{
			name: "no --new, and an empty new, leaves the line's ending", file: "a\nb\nc\n",
			quote: []string{"2", "2"},
			more:  "@@ new\n",
			want:  "a\n\nc\n",
		},
		{
			name: "a file that quotes a patch, under another marker", file: "intro\n@@ old\nx\n@@ new\ny\noutro\n",
			quote: []string{"--marker", "%%", "2", "5", "--new", "NEW"}, repl: "@@ delete gone.txt\n",
			apply: []string{"--marker", "%%"},
			want:  "intro\n@@ delete gone.txt\noutro\n",
		},
		{
			// A range in a file's minority ending is the apply's to refuse
			// under auto (§6.4), and strict applies the bytes.
			name: "a range with the file's other line ending, under --eol strict", file: "one\ntwo\r\nthree\nfour\nfive\n",
			quote: []string{"2", "3", "--new", "NEW"}, repl: "X\n",
			apply: []string{"--eol", "strict"},
			want:  "one\nX\nfour\nfive\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": c.file})
			before := snapshot(t, root)
			args := []string{"f.txt"}
			for _, a := range c.quote {
				if a == "NEW" {
					a = newFile(t, c.repl)
				}
				args = append(args, a)
			}
			code, patch, errOut := quoteCLI(t, root, args...)
			if code != exitOK {
				t.Fatalf("quote: exit %d: %s", code, errOut)
			}
			if d := snapshotDiff(before, snapshot(t, root)); d != nil {
				t.Fatalf("quote changed the tree: %v", d)
			}
			code, out, errOut := runCLI(t, root, c.apply, patch+c.more)
			if code != exitOK {
				t.Fatalf("apply: exit %d: %s%s\n--- patch ---\n%s", code, out, errOut, patch+c.more)
			}
			got, err := os.ReadFile(filepath.Join(root, "f.txt"))
			must(t, err)
			if string(got) != c.want {
				t.Errorf("file\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

// What the quote cannot decide is still decided, by the apply: the exactly-once
// check is untouched, and it is what refuses these.
func TestTheApplyStillRefusesAQuotedRange(t *testing.T) {
	t.Run("a range too short to be unique", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": "a\n}\nb\n}\nc\n"})
		_, patch, _ := quoteCLI(t, root, "f.txt", "4", "4", "--new", newFile(t, "X\n"))
		code, _, errOut := runCLI(t, root, nil, patch)
		if code != exitNoMatch || !strings.Contains(errOut, "expected 1 occurrence, found 2") {
			t.Errorf("exit %d: %s", code, errOut)
		}
	})
	t.Run("a file that changed between the quote and the apply", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": tenLines})
		_, patch, _ := quoteCLI(t, root, "f.txt", "2", "3", "--new", newFile(t, "X\n"))
		must(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte(strings.Replace(tenLines, "line 3", "line three", 1)), 0o644))
		code, _, errOut := runCLI(t, root, nil, patch)
		if code != exitNoMatch || !strings.Contains(errOut, "did not match") {
			t.Errorf("exit %d: %s", code, errOut)
		}
	})
	t.Run("a range in the file's other line ending, under the default --eol", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": "one\ntwo\r\nthree\nfour\nfive\n"})
		before := snapshot(t, root)
		_, patch, _ := quoteCLI(t, root, "f.txt", "2", "3", "--new", newFile(t, "X\n"))
		code, _, errOut := runCLI(t, root, nil, patch)
		if code != exitNoMatch || !strings.Contains(errOut, "mixed line endings") {
			t.Errorf("exit %d: %s", code, errOut)
		}
		if d := snapshotDiff(before, snapshot(t, root)); d != nil {
			t.Errorf("the tree changed: %v", d)
		}
	})
}

// The limit the help states, held here so that nobody reads the suite as
// saying otherwise: a stale number quotes another block, and it applies.
func TestAStaleLineNumberIsNotCaught(t *testing.T) {
	root := cliTree(t, map[string]string{"f.txt": "added 1\nadded 2\n" + tenLines})
	// The caller means "line 5" and "line 6", as they were numbered before
	// two lines went in above them.
	code, patch, errOut := quoteCLI(t, root, "f.txt", "5", "6", "--new", newFile(t, "NEW\n"))
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// The report is where it shows.
	if want := `quoted f.txt 5-6, 2 lines: "line 3" to "line 4"` + "\n"; errOut != want {
		t.Errorf("report = %q, want %q", errOut, want)
	}
	if code, _, errOut := runCLI(t, root, nil, patch); code != exitOK {
		t.Fatalf("apply: exit %d: %s", code, errOut)
	}
	got, err := os.ReadFile(filepath.Join(root, "f.txt"))
	must(t, err)
	if !strings.Contains(string(got), "line 2\nNEW\nline 5\n") {
		t.Errorf("file = %q", got)
	}
}

// A refusal's two lines, checked together: stderr says it, and stdout carries
// the same sentence as a line no patch may hold.
func refusedAs(t *testing.T, stdout, stderr, marker string) string {
	t.Helper()
	msg, ok := strings.CutPrefix(stderr, "hunk quote: ")
	if !ok || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("stderr = %q, want one line starting \"hunk quote: \"", stderr)
	}
	if want := marker + " end refused by hunk quote: " + msg; stdout != want {
		t.Errorf("stdout = %q, want the refusal as %q", stdout, want)
	}
	return strings.TrimSuffix(msg, "\n")
}

// Every way a call is refused, each by exit, by words, and by a tree that is
// as it was. The sentences are goldened together in quote-refusals.
func TestQuoteRefusals(t *testing.T) {
	type refusal struct {
		name string
		args []string // "ROOT" stands for the tree's root
		code int
		want []string
		// local marks a sentence the platform words, which is asserted here
		// and left out of the golden.
		local bool
	}
	cases := []refusal{
		{"no arguments", nil, exitUsage, []string{"quote takes FILE START END", "got 0 arguments", `"hunk quote --help"`}, false},
		{"one argument", []string{"f.txt"}, exitUsage, []string{"got 1 argument;"}, false},
		{"two arguments", []string{"f.txt", "1"}, exitUsage, []string{"got 2 arguments"}, false},
		{"four arguments", []string{"f.txt", "1", "2", "3"}, exitUsage, []string{"got 4 arguments"}, false},
		{"START is not a number", []string{"f.txt", "x", "2"}, exitUsage, []string{"line numbers counted from 1", `"x" is not one`}, false},
		{"END is not a number", []string{"f.txt", "1", "2a"}, exitUsage, []string{`"2a" is not one`}, false},
		{"START is 0", []string{"f.txt", "0", "2"}, exitUsage, []string{`"0" is not one`}, false},
		{"a sign", []string{"f.txt", "+1", "2"}, exitUsage, []string{`"+1" is not one`}, false},
		{"a fraction", []string{"f.txt", "1", "2.5"}, exitUsage, []string{`"2.5" is not one`}, false},
		{"an empty number", []string{"f.txt", "", "2"}, exitUsage, []string{`"" is not one`}, false},
		{"a number no int holds", []string{"f.txt", "1", "99999999999999999999"}, exitUsage, []string{`"99999999999999999999" is not one`}, false},
		{"a number with a control character in it", []string{"f.txt", "1\x1b[2J", "2"}, exitUsage, []string{`"1\x1b[2J" is not one`}, false},
		{"START after END", []string{"f.txt", "6", "3"}, exitUsage, []string{"START is 6 and END is 3", "runs backwards"}, false},
		{"a path a directive line would trim", []string{"f.txt ", "1", "2"}, exitUsage, []string{`"f.txt " begins or ends with a space`, "no patch can name it"}, false},
		{"a path with a leading tab", []string{"\tf.txt", "1", "2"}, exitUsage, []string{`"\tf.txt" begins or ends`}, false},
		{"an unknown flag", []string{"f.txt", "1", "2", "--nwe", "x"}, exitUsage, []string{"flag provided but not defined: -nwe", `"hunk quote --help"`}, false},
		{"a negative number is read as a flag", []string{"f.txt", "-3", "5"}, exitUsage, []string{"flag provided but not defined: -3"}, false},
		{"a flag with an escape in its name", []string{"f.txt", "1", "2", "--x\x1b[2J"}, exitUsage, []string{"flag provided but not defined: -x<U+001B>[2J"}, false},
		{"a --new whose name holds an escape", []string{"f.txt", "1", "2", "--new", "gone\x1b.txt"}, exitUsage, []string{"--new: ", "gone<U+001B>.txt"}, true},
		{"an empty --marker", []string{"--marker", "", "f.txt", "1", "2"}, exitUsage, []string{"--marker must not be empty"}, false},
		{"a --marker with a newline in it", []string{"--marker", "@@\n", "f.txt", "1", "2"}, exitUsage, []string{"--marker contains U+000A", "control character"}, false},
		// Under a directory that is not there either, which is the case every
		// platform words the same way (notExistPhrase).
		{"a --new that is not there", []string{"f.txt", "1", "2", "--new", filepath.Join("ROOT", "absent", "new.txt")}, exitUsage, []string{"--new: ", notExistPhrase()}, true},
		{"a --new that is a directory", []string{"f.txt", "1", "2", "--new", "ROOT"}, exitUsage, []string{"--new: "}, true},
		{"a FILE that is not there", []string{"absent.txt", "1", "2"}, exitNoMatch, []string{"absent.txt: no such file", "the root is"}, false},
		{"a FILE that is a directory", []string{"sub", "1", "2"}, exitNoMatch, []string{"sub: it is a directory, not a file"}, false},
		{"a FILE outside the root", []string{"../out.txt", "1", "2"}, exitNoMatch, []string{"../out.txt: the path climbs out of the root"}, false},
		{"a FILE inside .git", []string{".git/config", "1", "2"}, exitNoMatch, []string{".git/config: it is .git or inside it"}, false},
		{"a FILE inside a file", []string{"f.txt/x", "1", "2"}, exitNoMatch, []string{"f.txt/x: no such file; f.txt is a file"}, false},
		{"a FILE with a control character in its name", []string{"f\x1b.txt", "1", "2"}, exitNoMatch, []string{"f<U+001B>.txt: the path contains U+001B"}, false},
		{"an empty FILE", []string{"", "1", "2"}, exitUsage, []string{"FILE is empty"}, false},
		{"END past the last line", []string{"f.txt", "9", "40"}, exitNoMatch, []string{"f.txt has 10 lines", "there is no line 40"}, false},
		{"one empty line", []string{"gap.txt", "2", "2"}, exitNoMatch, []string{"gap.txt line 2 is empty"}, false},
		{"a directive in the range", []string{"doc.md", "1", "3"}, exitNoMatch, []string{`doc.md line 2 is "@@ old"`, "--marker"}, false},
		{"a root that is not there", []string{"--root", filepath.Join("ROOT", "nowhere", "deeper"), "f.txt", "1", "2"}, exitIO, []string{notExistPhrase()}, true},
	}
	var sentences strings.Builder
	goldened, ran := 0, 0
	for _, c := range cases {
		if !c.local {
			goldened++
		}
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{
				"f.txt": tenLines, "sub/in.txt": "x\n", ".git/config": "[core]\n",
				"gap.txt": "a\n\nb\n", "doc.md": "intro\n@@ old\nx\n",
			})
			before := snapshot(t, root)
			args := make([]string, len(c.args))
			for i, a := range c.args {
				args[i] = strings.ReplaceAll(a, "ROOT", root)
			}
			code, out, errOut := quoteCLI(t, root, args...)
			if code != c.code {
				t.Errorf("exit %d, want %d", code, c.code)
			}
			msg := refusedAs(t, out, errOut, DefaultMarker)
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("msg = %q, want it to say %q", msg, w)
				}
			}
			if name, ok := firstControl(strings.TrimSuffix(errOut, "\n") + strings.TrimSuffix(out, "\n")); ok {
				t.Errorf("the refusal prints %s raw: %q", name, errOut)
			}
			if d := snapshotDiff(before, snapshot(t, root)); d != nil {
				t.Errorf("the tree changed: %v", d)
			}
			if !c.local {
				ran++
				fmt.Fprintf(&sentences, "%s\n    %s\n", c.name, strings.ReplaceAll(msg, root, "/the/root"))
			}
		})
	}
	// Every sentence or none: a run narrowed with -run has only some of them.
	if ran == goldened {
		golden(t, "quote-refusals", sentences.String())
	}
}

// The two refusals that need the filesystem to refuse something.
func TestQuoteRefusesWhatTheTreeWillNotGive(t *testing.T) {
	t.Run("a FILE that cannot be read is exit 5", func(t *testing.T) {
		needsPOSIXPerms(t)
		if os.Geteuid() == 0 {
			t.Skip("root reads a file whatever its mode")
		}
		root := cliTree(t, map[string]string{"f.txt": tenLines})
		must(t, os.Chmod(filepath.Join(root, "f.txt"), 0o000))
		code, out, errOut := quoteCLI(t, root, "f.txt", "1", "2")
		if code != exitIO {
			t.Errorf("exit %d, want 5", code)
		}
		if msg := refusedAs(t, out, errOut, DefaultMarker); !strings.Contains(msg, "permission denied") {
			t.Errorf("msg = %q", msg)
		}
	})
	t.Run("a patch that cannot be written is exit 5, on stderr alone", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": tenLines})
		var errOut bytes.Buffer
		code := cli([]string{"quote", "--root", root, "f.txt", "1", "2"}, strings.NewReader(""), failingWriter{}, &errOut)
		if code != exitIO {
			t.Errorf("exit %d, want 5", code)
		}
		if want := "hunk quote: the patch could not be written: disk gone\n"; errOut.String() != want {
			t.Errorf("stderr = %q, want %q", errOut.String(), want)
		}
	})
}

// A refusal has to stop the batch the quote was written for, wherever its line
// lands in the patch, because the apply at the end of a brace group cannot see
// a quote fail. Until this was built, a quoter that refused and printed nothing
// left hunks 1 and 3 to apply at exit 0.
func TestARefusedQuoteStopsTheBatch(t *testing.T) {
	const first = "@@ file f.txt\n@@ old\nline 2\n@@ new\nTWO\n"
	const last = "@@ file f.txt\n@@ old\nline 9\n@@ new\nNINE\n"
	for _, c := range []struct {
		name          string
		before, after string
		line          string
	}{
		{"first in the patch", "", last, "1"},
		{"between two hunks", first, last, "6"},
		{"last in the patch", first, "", "6"},
		{"alone", "", "", "1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": tenLines})
			before := snapshot(t, root)
			code, refused, _ := quoteCLI(t, root, "f.txt", "40", "50", "--new", newFile(t, "X\n"))
			if code != exitNoMatch {
				t.Fatalf("quote: exit %d", code)
			}
			code, out, errOut := runCLI(t, root, nil, c.before+refused+c.after)
			if code != exitUsage || out != "" {
				t.Errorf("apply: exit %d, stdout %q; want 1 and nothing applied", code, out)
			}
			want := "hunk: patch line " + c.line + `: "@@ end" takes no argument; found: ` +
				"refused by hunk quote: f.txt has 10 lines, so there is no line 40\n"
			if errOut != want {
				t.Errorf("apply said\n %q\nwant\n %q", errOut, want)
			}
			if d := snapshotDiff(before, snapshot(t, root)); d != nil {
				t.Errorf("the tree changed: %v", d)
			}
			if c.name == "between two hunks" {
				golden(t, "cli-quote-refusal-reaches-the-apply", errOut)
				code, js, _ := runCLI(t, root, []string{"--json"}, c.before+refused+c.after)
				if code != exitUsage {
					t.Errorf("--json: exit %d", code)
				}
				golden(t, "cli-quote-refusal-reaches-the-apply-json", js)
			}
		})
	}

	// Under the apply's marker, or it is a line of text in the hunk before it.
	t.Run("the refusal is written under the quote's marker", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": tenLines})
		before := snapshot(t, root)
		code, refused, errOut := quoteCLI(t, root, "--marker", "%%", "f.txt", "40", "50")
		if code != exitNoMatch {
			t.Fatalf("quote: exit %d", code)
		}
		refusedAs(t, refused, errOut, "%%")
		patch := strings.ReplaceAll(first, "@@", "%%") + refused
		if code, _, errOut := runCLI(t, root, []string{"--marker", "%%"}, patch); code != exitUsage ||
			!strings.Contains(errOut, `"%% end" takes no argument; found: refused by hunk quote`) {
			t.Errorf("apply: exit %d: %s", code, errOut)
		}
		if d := snapshotDiff(before, snapshot(t, root)); d != nil {
			t.Errorf("the tree changed: %v", d)
		}
	})
}

// The marker a refusal is written under is known even when the flags do not
// parse, because the apply reads by it and a refusal under another is a line
// of text. One gap is left and it is a row here: --marker itself misspelled.
func TestMarkerArg(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"none given", []string{"f.txt", "1", "2"}, "@@"},
		{"no arguments", nil, "@@"},
		{"two dashes and a value", []string{"--marker", "%%", "f.txt"}, "%%"},
		{"one dash", []string{"-marker", "%%"}, "%%"},
		{"an equals sign", []string{"--marker=%%"}, "%%"},
		{"an equals sign in the value", []string{"--marker==="}, "=="},
		{"after a flag that does not parse", []string{"--bogus", "f.txt", "--marker", "##"}, "##"},
		{"after the range", []string{"f.txt", "1", "2", "--marker", "##"}, "##"},
		{"the last one wins, as the flag package has it", []string{"--marker", "%%", "--marker", "##"}, "##"},
		{"nothing after it", []string{"f.txt", "--marker"}, "@@"},
		{"after -- it is an argument", []string{"--", "--marker", "%%"}, "@@"},
		{"a flag that only starts like it", []string{"--markers", "%%"}, "@@"},
		{"misspelled, which is the gap", []string{"--markr", "%%", "f.txt", "1", "2"}, "@@"},
		{"an argument that is not a flag", []string{"marker", "%%"}, "@@"},
		{"an empty one cannot head a line", []string{"--marker", ""}, "@@"},
		{"an empty one after an equals sign", []string{"--marker="}, "@@"},
		{"nor can one with a control character", []string{"--marker", "%%\n"}, "@@"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := markerArg(c.args); got != c.want {
				t.Errorf("markerArg(%q) = %q, want %q", c.args, got, c.want)
			}
		})
	}

	t.Run("so a call whose flags do not parse still refuses under its marker", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": tenLines})
		for _, args := range [][]string{
			{"--marker", "%%", "f.txt", "1", "2", "--bogus"},
			{"--bogus", "--marker=%%", "f.txt", "1", "2"},
		} {
			code, out, errOut := quoteCLI(t, root, args...)
			if code != exitUsage {
				t.Errorf("%q: exit %d", args, code)
			}
			refusedAs(t, out, errOut, "%%")
		}
	})
}

func TestLineNumber(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"1", 1, true},
		{"7", 7, true},
		{"188", 188, true},
		{"007", 7, true},
		{"0", 0, false},
		{"00", 0, false},
		{"", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"1.0", 0, false},
		{"1e3", 0, false},
		{" 1", 0, false},
		{"1 ", 0, false},
		{"0x10", 0, false},
		{"1_000", 0, false},
		{"١", 0, false},
		{"99999999999999999999", 0, false},
	} {
		got, r := lineNumber(c.in)
		if (r == nil) != c.ok || got != c.want {
			t.Errorf("lineNumber(%q) = %d, refused %v; want %d, ok %v", c.in, got, r != nil, c.want, c.ok)
		}
		if r != nil && (r.code != exitUsage || !strings.Contains(r.msg, fmt.Sprintf("%q is not one", c.in))) {
			t.Errorf("lineNumber(%q) refused with exit %d: %s", c.in, r.code, r.msg)
		}
	}
}

// The report is the reporter's own check made part of the tool: every range's
// two ends, on stderr, where the patch is not.
func TestQuoteReport(t *testing.T) {
	long := strings.Repeat("x", 70)
	root := cliTree(t, map[string]string{
		"f.txt":    tenLines,
		"crlf.txt": "one\r\ntwo\r\nthree\r\n",
		"odd.txt":  "\tindented\n\nesc\x1b[2J\n" + long + "\nlast",
		"a b.txt":  "x\ny\n",
	})
	var report strings.Builder
	rows := []struct {
		name string
		args []string
		want string
	}{
		{"a range", []string{"f.txt", "2", "9"}, `quoted f.txt 2-9, 8 lines: "line 2" to "line 9"`},
		{"one line has one end", []string{"f.txt", "7", "7"}, `quoted f.txt 7, 1 line: "line 7"`},
		{"two lines", []string{"f.txt", "1", "2"}, `quoted f.txt 1-2, 2 lines: "line 1" to "line 2"`},
		{"a CRLF file's ends are shown without the CR", []string{"crlf.txt", "1", "3"}, `quoted crlf.txt 1-3, 3 lines: "one" to "three"`},
		{"a tab is shown, and a blank line named", []string{"odd.txt", "1", "2"}, `quoted odd.txt 1-2, 2 lines: "\tindented" to a blank line`},
		{"an escape is never printed raw", []string{"odd.txt", "2", "3"}, `quoted odd.txt 2-3, 2 lines: a blank line to "esc\x1b[2J"`},
		{"a long line is cut", []string{"odd.txt", "4", "5"}, `quoted odd.txt 4-5, 2 lines: "` + strings.Repeat("x", 60) + `"... to "last"`},
		{"a path with a space", []string{"a b.txt", "1", "2"}, `quoted a b.txt 1-2, 2 lines: "x" to "y"`},
		{"the replacement is not part of it", []string{"f.txt", "2", "3", "--new", newFile(t, "A\nB\nC\n")}, `quoted f.txt 2-3, 2 lines: "line 2" to "line 3"`},
	}
	ran := 0
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := quoteCLI(t, root, c.args...)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if errOut != c.want+"\n" {
				t.Errorf("report\n got %q\nwant %q", errOut, c.want+"\n")
			}
			if strings.Contains(out, "quoted ") {
				t.Errorf("the report is in the patch: %q", out)
			}
			if name, ok := firstControl(strings.TrimSuffix(errOut, "\n")); ok {
				t.Errorf("the report prints %s raw", name)
			}
			ran++
			fmt.Fprintf(&report, "%s", errOut)
		})
	}
	if ran == len(rows) {
		golden(t, "quote-report", report.String())
	}
}

// --new after the range is the natural spelling and the flag package stops at
// the first argument that is not a flag, so every placement is tried.
func TestQuoteTakesItsFlagsAnywhere(t *testing.T) {
	root := cliTree(t, map[string]string{"f.txt": "a\nb\nc\n", "sub/g.txt": "x\ny\n"})
	repl := newFile(t, "B\n")
	const want = "@@ file f.txt\n@@ old\nb\n@@ new\nB\n"
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"before", []string{"--new", repl, "f.txt", "2", "2"}, want},
		{"after", []string{"f.txt", "2", "2", "--new", repl}, want},
		{"between", []string{"f.txt", "--new", repl, "2", "2"}, want},
		{"with an equals sign", []string{"f.txt", "2", "2", "--new=" + repl}, want},
		{"split around the range", []string{"--marker", "%%", "f.txt", "2", "2", "--new", repl}, strings.ReplaceAll(want, "@@", "%%")},
		{"the marker last", []string{"f.txt", "2", "2", "--new", repl, "--marker=%%"}, strings.ReplaceAll(want, "@@", "%%")},
		{"a path under the root", []string{"sub/g.txt", "1", "1"}, "@@ file sub/g.txt\n@@ old\nx\n"},
		{"an absolute path under the root", []string{filepath.Join(root, "f.txt"), "1", "1"}, "@@ file " + filepath.Join(root, "f.txt") + "\n@@ old\na\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := quoteCLI(t, root, c.args...)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if out != c.want {
				t.Errorf("patch\n got %q\nwant %q", out, c.want)
			}
		})
	}

	t.Run("no --root means the working directory", func(t *testing.T) {
		t.Chdir(root)
		var out, errOut bytes.Buffer
		if code := cli([]string{"quote", "f.txt", "1", "1"}, strings.NewReader(""), &out, &errOut); code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		if out.String() != "@@ file f.txt\n@@ old\na\n" {
			t.Errorf("patch = %q", out.String())
		}
	})
	t.Run("--new is relative to the working directory, not to --root", func(t *testing.T) {
		dir := filepath.Dir(repl)
		t.Chdir(dir)
		code, out, errOut := quoteCLI(t, root, "f.txt", "2", "2", "--new", "new.txt")
		if code != exitOK || out != want {
			t.Errorf("exit %d, patch %q: %s", code, out, errOut)
		}
	})
	t.Run("--allow-outside-root reads a FILE outside it, as the apply would write it", func(t *testing.T) {
		outer := cliTree(t, map[string]string{"out.txt": "o\np\n", "in/x.txt": "x\n"})
		inner := filepath.Join(outer, "in")
		if code, _, _ := quoteCLI(t, inner, "../out.txt", "1", "1"); code != exitNoMatch {
			t.Errorf("confined: exit %d, want 2", code)
		}
		code, out, errOut := quoteCLI(t, inner, "--allow-outside-root", "../out.txt", "1", "1")
		if code != exitOK || out != "@@ file ../out.txt\n@@ old\no\n" {
			t.Errorf("exit %d, patch %q: %s", code, out, errOut)
		}
	})
	t.Run("quote is a subcommand and comes first", func(t *testing.T) {
		code, out, errOut := runCLI(t, root, []string{"quote", "f.txt", "1", "1"}, "")
		if code != exitUsage || out != "" || !strings.Contains(errOut, "quote is a subcommand and comes first") {
			t.Errorf("exit %d, stdout %q: %s", code, out, errOut)
		}
	})
}

// quoteHelpFlags reads the flags "hunk quote --help" has entries for, as
// helpFlags reads --help's.
func quoteHelpFlags(t *testing.T) []string {
	t.Helper()
	_, section, ok := strings.Cut(quoteUsage, "\nFLAGS\n")
	if !ok {
		t.Fatal("hunk quote --help has no FLAGS section")
	}
	section, _, _ = strings.Cut(section, "\nREFUSALS\n")
	var flags []string
	for _, m := range reHelpFlag.FindAllStringSubmatch(section, -1) {
		flags = append(flags, m[1])
	}
	return flags
}

func TestQuoteHelp(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		var out, errOut bytes.Buffer
		if code := cli([]string{"quote", arg}, strings.NewReader(""), &out, &errOut); code != exitOK {
			t.Fatalf("%s: exit %d", arg, code)
		}
		if out.String() != quoteUsage || errOut.String() != "" {
			t.Errorf("%s printed something other than the usage, or wrote to stderr: %q", arg, errOut.String())
		}
	}
	golden(t, "cli-quote-help", quoteUsage)

	// The flags it documents are the flags it takes, both ways. A flag the set
	// defines and the page leaves out is one nobody will use, and one the page
	// offers and the set lacks is a usage error it taught.
	var defined []string
	newQuoteFlags().fs.VisitAll(func(f *flag.Flag) {
		if f.Name != "help" && f.Name != "h" {
			defined = append(defined, "--"+f.Name)
		}
	})
	listed := quoteHelpFlags(t)
	slices.Sort(defined)
	slices.Sort(listed)
	if !slices.Equal(defined, listed) {
		t.Errorf("hunk quote takes %q and its help lists %q", defined, listed)
	}

	// The limit is stated, because a tool that overstates its guarantees is
	// worse than one that has none (§6.2).
	flat := strings.Join(strings.Fields(quoteUsage), " ")
	for _, want := range []string{"WHAT IT CANNOT CHECK", "gone stale", "--dry-run first", "An empty FILE deletes the lines"} {
		if !strings.Contains(flat, want) {
			t.Errorf("hunk quote --help does not say %q", want)
		}
	}
	// Every exit it can end with has a row.
	for _, code := range []string{"1", "2", "5"} {
		if !strings.Contains(quoteUsage, "\n  "+code+"  ") {
			t.Errorf("hunk quote --help does not list exit %s", code)
		}
	}
}

// --help, the README and the skill each say the subcommand exists and what it
// cannot check. A feature an agent cannot see is one it will rebuild with sed,
// which is how this one came to be asked for.
func TestTheDocsNameQuote(t *testing.T) {
	var out bytes.Buffer
	if code := cli([]string{"--help"}, strings.NewReader(""), &out, &bytes.Buffer{}); code != exitOK {
		t.Fatalf("--help: exit %d", code)
	}
	docs := map[string]string{"hunk --help": out.String()}
	for _, p := range []string{"README.md", filepath.Join(".agents", "skills", "hunk", "SKILL.md")} {
		b, err := os.ReadFile(p)
		must(t, err)
		docs[p] = string(b)
	}
	for name, doc := range docs {
		if !strings.Contains(doc, "hunk quote") {
			t.Errorf("%s never says \"hunk quote\"", name)
		}
	}
	// The two an agent reads before using it also carry the limit and the
	// dry run that goes with it.
	for _, p := range []string{"README.md", filepath.Join(".agents", "skills", "hunk", "SKILL.md")} {
		flat := strings.Join(strings.Fields(docs[p]), " ")
		for _, want := range []string{"stale", "--dry-run", "--new", "--marker"} {
			_, after, _ := strings.Cut(flat, "hunk quote")
			if !strings.Contains(after, want) {
				t.Errorf("%s names hunk quote and says nothing after it of %q", p, want)
			}
		}
	}
}

// quote.go reads the tree and never writes it. Every test above snapshots the
// tree around the call; this holds the claim for the calls no test makes.
func TestQuoteNeverWritesTheTree(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "quote.go", nil, 0)
	if err != nil {
		t.Fatalf("parse quote.go: %v", err)
	}
	// The whole of what it may ask the operating system for, by name.
	osAllowed := map[string]bool{"ReadFile": true}
	// And what would write, on a Tree, a Txn or the os package.
	writers := map[string]bool{
		"WriteAtomic": true, "CreateAtomic": true, "Remove": true, "RemoveDir": true, "MkdirAll": true,
		"Run": true, "Commit": true, "Rollback": true, "WriteFile": true, "Create": true,
		"OpenFile": true, "Rename": true, "Mkdir": true, "RemoveAll": true, "Chmod": true,
	}
	calls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		calls++
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "os" && !osAllowed[sel.Sel.Name] {
			t.Errorf("quote.go uses os.%s; it may read --new's file and nothing else", sel.Sel.Name)
		}
		if writers[sel.Sel.Name] {
			t.Errorf("quote.go calls %s, which writes; hunk quote writes nothing to the tree", sel.Sel.Name)
		}
		return true
	})
	if calls == 0 {
		t.Fatal("read no selectors from quote.go; every check above would pass")
	}
}

// quotedAsWholeLines is what replacing lines start..end of file with repl
// leaves, worked out the long way round: by cutting both into lines and
// joining them, with none of quoteLines's offsets.
func quotedAsWholeLines(file []byte, start, end int, repl []byte) []byte {
	lines := bytes.SplitAfter(file, lf)
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	var out []byte
	for _, l := range lines[:start-1] {
		out = append(out, l...)
	}
	if len(repl) > 0 {
		// The replacement's lines, the last of them ended as the range's last
		// line was.
		last := lines[end-1]
		ending := ""
		switch {
		case bytes.HasSuffix(last, crlf):
			ending = "\r\n"
		case bytes.HasSuffix(last, lf):
			ending = "\n"
		}
		body := repl
		switch {
		case bytes.HasSuffix(body, crlf):
			body = body[:len(body)-2]
		case bytes.HasSuffix(body, lf):
			body = body[:len(body)-1]
		}
		out = append(append(out, body...), ending...)
	}
	for _, l := range lines[end:] {
		out = append(out, l...)
	}
	return out
}

// The property the subcommand is for. For any file, range, replacement and
// marker, quote either refuses, or prints a patch the parser reads as exactly
// one replace on that file, whose old is at the range and whose new, put in
// its place, leaves the file with those lines swapped for the replacement's
// and every other byte where it was. Input here is whatever a tree holds, so
// it is fuzzed: no panic, and no refusal that prints a control character raw.
func FuzzAQuoteReadsBackAsTheRange(f *testing.F) {
	f.Add([]byte(tenLines), 2, 3, []byte("NEW A\nNEW B\n"), true, "@@")
	f.Add([]byte("a\nb\nc"), 2, 3, []byte(""), true, "@@")
	f.Add([]byte("one\r\ntwo\r\nthree\r\n"), 2, 2, []byte("TWO"), true, "@@")
	f.Add([]byte("a\n@@ old\nb\n"), 1, 3, []byte("x\n"), true, "@@")
	f.Add([]byte("a\n@@ old\nb\n"), 1, 3, []byte("@@ delete b.txt\n"), true, "%%")
	f.Add([]byte("a\n\nb\n"), 2, 2, []byte(nil), false, "@@")
	f.Add([]byte("a\n@@ end\f\nb\n"), 1, 3, []byte("\n"), true, "@@")
	f.Add([]byte(""), 1, 1, []byte("x"), true, "@@")
	f.Add([]byte("\n\n\n"), 1, 3, []byte("\r\n"), true, " ")
	f.Add([]byte("a\rb\r\r\n"), 1, 1, []byte("x\r"), true, "@@ file")
	// Three the seeds above could not tell from a broken quote: a
	// delete of lines that have endings, a directive in the replacement, and
	// a range one line past the end.
	f.Add([]byte(tenLines), 2, 3, []byte(""), true, "@@")
	f.Add([]byte(tenLines), 2, 3, []byte("intro\n@@ delete b.txt\n"), true, "@@")
	f.Add([]byte("a\nb\n"), 1, 3, []byte("x\n"), true, "@@")

	f.Fuzz(func(t *testing.T, file []byte, start, end int, repl []byte, hasNew bool, marker string) {
		// What runQuote has refused before quoteLines is asked.
		if _, bad := firstControl(marker); bad || marker == "" || start < 1 || start > end {
			return
		}
		if !hasNew {
			repl = nil
		}
		q, r := quoteLines("f.txt", file, start, end, repl, hasNew, "new.txt", marker)
		if r != nil {
			if q != nil {
				t.Fatalf("a refusal came with a patch")
			}
			if r.code != exitNoMatch {
				t.Fatalf("refused with exit %d: %s", r.code, r.msg)
			}
			if name, ok := firstControl(showControls(r.msg)); ok {
				t.Fatalf("the refusal prints %s raw: %q", name, r.msg)
			}
			// A range that is there, of text without the marker's first byte
			// anywhere in it, has nothing to refuse but an empty old: no
			// line of it can start with the marker, whatever is taken out.
			// Unless that byte is a space, which is what the parser tries in
			// place of a control character when it looks for a directive with
			// one stuck to it: under the marker " \xa1" the line
			// "\a\xa1\x98old" is such a directive, and holds no space. The
			// first ninety seconds of fuzzing found that in twenty-two, and it
			// was this sentence that was wrong, not the refusal.
			plain := marker[0] != ' ' && bytes.IndexByte(file, marker[0]) < 0 && bytes.IndexByte(repl, marker[0]) < 0
			if end <= lineCount(file) && plain && !strings.Contains(r.msg, "is empty") {
				t.Fatalf("refused a range that is there, with no marker in it: %s", r.msg)
			}
			return
		}

		// With no --new the patch is the head of a hunk, and the caller types
		// the rest.
		patch, want := q.Patch, repl
		if !hasNew {
			patch = append(append([]byte{}, patch...), (marker + " new\nTYPED\n")...)
			want = []byte("TYPED\n")
		}
		p, err := Parse(patch, marker)
		if err != nil {
			t.Fatalf("the patch does not parse: %v\n%q", err, patch)
		}
		if len(p.Hunks) != 1 {
			t.Fatalf("the patch has %d hunks, want 1\n%q", len(p.Hunks), patch)
		}
		h := p.Hunks[0]
		if h.Op != OpReplace || h.Path != "f.txt" || h.Count != 1 {
			t.Fatalf("hunk is %s on %q x%d\n%q", h.Op, h.Path, h.Count, patch)
		}
		// Where the range starts, counted the long way.
		from := 0
		for _, l := range bytes.SplitAfter(file, lf)[:start-1] {
			from += len(l)
		}
		if !bytes.HasPrefix(file[from:], h.Old) {
			t.Fatalf("old is not the text at line %d\n old %q\nfile %q", start, h.Old, file[from:])
		}
		got := append(append(append([]byte{}, file[:from]...), h.New...), file[from+len(h.Old):]...)
		if exp := quotedAsWholeLines(file, start, end, want); !bytes.Equal(got, exp) {
			t.Fatalf("replacing old with new leaves\n %q\nand whole lines swapped leave\n %q\n--- patch ---\n%q", got, exp, patch)
		}
		if lineCount(q.First) > 1 || lineCount(q.Last) > 1 || !bytes.HasPrefix(file[from:], q.First) {
			t.Fatalf("the report's ends are not lines of the range: %q, %q", q.First, q.Last)
		}
	})
}

// The example the README and the skill print, run as an agent runs it: sh, a
// brace group, and a real "hunk quote" process for each range, with only the
// hunk at the end of the pipe run in process. The block of output each
// document prints under it is what comes out, so neither can drift from the
// tool, and the second half is the shape the refusal line exists for: the
// group's own exit status is cat's, 0, whatever a quote inside it did.
func TestTheQuoteExampleRunsAsPrinted(t *testing.T) {
	exe, err := os.Executable()
	must(t, err)
	source := func(lines int) string {
		var b strings.Builder
		for n := 1; n <= lines; n++ {
			switch n {
			case 40:
				b.WriteString("func parse(src []byte) (*Tree, error) {\n")
			case 210:
				b.WriteString("func walk(t *Tree) error {\n")
			case 95, 262:
				b.WriteString("}\n")
			default:
				fmt.Fprintf(&b, "\t// line %d\n", n)
			}
		}
		return b.String()
	}
	const replacement = "func parse(src []byte) (*Tree, error) {\n\tif len(src) == 0 {\n\t\treturn nil, errEmpty\n\t}\n\treturn scan(src)\n}\n"
	tree := func(lines int) map[string]string {
		return map[string]string{
			"src/parse.go": source(lines),
			"src/main.go":  "func main() {\n\tparse(src)\n}\n",
			"new/parse.go": replacement,
		}
	}

	for _, doc := range []string{"README.md", filepath.Join(".agents", "skills", "hunk", "SKILL.md")} {
		b, err := os.ReadFile(doc)
		must(t, err)
		_, rest, ok := strings.Cut(string(b), "\n{\n  hunk quote ")
		if !ok {
			t.Fatalf("%s no longer shows a batch built with hunk quote, or its opening changed", doc)
		}
		group, after, ok := strings.Cut(rest, "\n} | hunk --dry-run\n")
		if !ok {
			t.Fatalf("%s: the example is not piped into hunk --dry-run", doc)
		}
		_, printed, ok := strings.Cut(after, "```\nquoted ")
		if !ok {
			t.Fatalf("%s: the example prints no output under it", doc)
		}
		printed, _, _ = strings.Cut("quoted "+printed, "```")
		// hunk is the test binary, as a function, so the example's own lines
		// run unchanged.
		script := "hunk() { \"$HUNK\" \"$@\"; }\n{\n  hunk quote " + group + "\n}\n"
		generate := func(t *testing.T, root string) (stream, quoted string) {
			t.Helper()
			cmd := exec.Command("sh", "-c", script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HUNK_TEST_AS_HUNK=1", "HUNK="+exe)
			var out, errOut bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &errOut
			if err := cmd.Run(); err != nil {
				t.Fatalf("the example does not run under sh: %v\n%s\n--- script ---\n%s", err, errOut.String(), script)
			}
			return out.String(), errOut.String()
		}

		t.Run(doc+": it prints what the document says, and applies", func(t *testing.T) {
			root := cliTree(t, tree(300))
			before := snapshot(t, root)
			stream, quoted := generate(t, root)
			code, out, errOut := runCLI(t, root, []string{"--dry-run"}, stream)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if got := quoted + out; got != printed {
				t.Errorf("the example prints\n%s\nand %s says it prints\n%s", got, doc, printed)
			}
			if d := snapshotDiff(before, snapshot(t, root)); d != nil {
				t.Fatalf("a dry run changed the tree: %v", d)
			}
			if code, _, errOut := runCLI(t, root, nil, stream); code != exitOK {
				t.Fatalf("applying it: exit %d: %s", code, errOut)
			}
			want := strings.Replace(source(300), source(95)[len(source(39)):], replacement, 1)
			want = strings.Replace(want, source(262)[len(source(209)):], "func walk(t *Tree) error {\n\treturn walkFrom(t.Root)\n}\n", 1)
			if got := readFile(t, root, "src/parse.go"); got != want {
				t.Errorf("src/parse.go is not the file with those two blocks swapped:\n%s", got)
			}
			if got := readFile(t, root, "src/main.go"); got != "func main() {\n\tparse(src, defaults)\n}\n" {
				t.Errorf("src/main.go = %q", got)
			}
		})

		t.Run(doc+": a quote in the group that refuses stops the whole batch", func(t *testing.T) {
			// The file ends before the second range, so the second quote
			// refuses, and the hunks either side of it are whole.
			root := cliTree(t, tree(180))
			before := snapshot(t, root)
			stream, quoted := generate(t, root)
			if !strings.Contains(quoted, "hunk quote: src/parse.go has 180 lines, so there is no line 210") {
				t.Errorf("the quote did not refuse on stderr: %q", quoted)
			}
			code, out, errOut := runCLI(t, root, nil, stream)
			if code != exitUsage || out != "" {
				t.Errorf("exit %d, stdout %q; want 1 and nothing applied", code, out)
			}
			if d := snapshotDiff(before, snapshot(t, root)); d != nil {
				t.Errorf("the tree changed: %v", d)
			}
			// The README prints this line as the example of it.
			if readme, err := os.ReadFile("README.md"); err != nil || !strings.Contains(string(readme), "```\n"+errOut+"```") {
				t.Errorf("README.md does not print the refusal this batch gets:\n%s", errOut)
			}
		})
	}
}
