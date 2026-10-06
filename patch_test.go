package main

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func hunkEqual(a, b Hunk) bool {
	return a.Op == b.Op && a.Path == b.Path && a.Line == b.Line && a.Count == b.Count &&
		bytes.Equal(a.Old, b.Old) && bytes.Equal(a.New, b.New) && bytes.Equal(a.Body, b.Body)
}

// rep builds an expected replace hunk. new is spelled repl because new is a
// builtin.
func rep(path string, line, count int, old, repl string) Hunk {
	return Hunk{
		Op: OpReplace, Path: path, Line: line, Count: count,
		Old: []byte(old), New: []byte(repl),
	}
}

func body(op Op, path string, line int, b string) Hunk {
	return Hunk{Op: op, Path: path, Line: line, Body: []byte(b)}
}

func TestParse(t *testing.T) {
	cases := []struct {
		name   string
		marker string // "" means DefaultMarker
		in     string
		want   []Hunk
	}{
		// --- the basic shapes ---
		{
			name: "one replace",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y")},
		},
		{
			name: "the file register persists across hunks (§3.5)",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny\n@@ old\np\n@@ new\nq\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y"), rep("a.go", 6, 1, "p", "q")},
		},
		{
			name: "a second @@ file changes it",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny\n@@ file b.go\n@@ old\np\n@@ new\nq\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y"), rep("b.go", 7, 1, "p", "q")},
		},

		// --- payload boundaries (§3.3), the subtle part ---
		{
			name: "a blank line before the next directive gives a trailing newline",
			in:   "@@ file a.go\n@@ old\nx\n\n@@ new\ny\n\n",
			want: []Hunk{rep("a.go", 2, 1, "x\n", "y\n")},
		},
		{
			name: "and the rule is the same at end of input",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny\n\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y\n")},
		},
		{
			name: "no trailing newline is ever added",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny",
			want: []Hunk{rep("a.go", 2, 1, "x", "y")},
		},
		{
			name: "two blank lines give two newlines",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny\n\n\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y\n\n")},
		},
		{
			name: "a payload of one blank line joins to nothing",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\n\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "")},
		},
		{
			name: "an empty payload deletes a line, newline included (§3.3)",
			in:   "@@ file a.go\n@@ old\n\tdebugPrint(x)\n\n@@ new\n@@ delete b.go\n",
			want: []Hunk{
				rep("a.go", 2, 1, "\tdebugPrint(x)\n", ""),
				body(OpDelete, "b.go", 6, ""),
			},
		},
		{
			name: "@@ end terminates the last payload",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny\n@@ end\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y")},
		},
		{
			name: "@@ end after a blank line keeps the newline",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny\n\n@@ end\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y\n")},
		},

		// --- directive recognition (§3.2), all three conditions ---
		{
			name: "a pasted unified diff is payload, because -1,3 is not a keyword",
			in:   "@@ file a.go\n@@ old\n@@ -1,3 +1,4 @@\n@@ new\nz\n",
			want: []Hunk{rep("a.go", 2, 1, "@@ -1,3 +1,4 @@", "z")},
		},
		{
			name: "@@@@ is payload: the character after the marker is not a space",
			in:   "@@ file a.md\n@@ old\n@@@@\n@@ new\nz\n",
			want: []Hunk{rep("a.md", 2, 1, "@@@@", "z")},
		},
		{
			name: "@@old is payload: no separator",
			in:   "@@ file a.go\n@@ old\n@@old\n@@ new\nz\n",
			want: []Hunk{rep("a.go", 2, 1, "@@old", "z")},
		},
		{
			name: "@@ oldx is payload: not a keyword",
			in:   "@@ file a.go\n@@ old\n@@ oldx\n@@ new\nz\n",
			want: []Hunk{rep("a.go", 2, 1, "@@ oldx", "z")},
		},
		{
			name: "an indented directive is payload: the marker must be at column 0",
			in:   "@@ file a.go\n@@ old\n @@ old\n@@ new\nz\n",
			want: []Hunk{rep("a.go", 2, 1, " @@ old", "z")},
		},
		{
			name: "a tab separates the marker from the keyword",
			in:   "@@ file a.go\n@@\told\nx\n@@ new\ny\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y")},
		},
		{
			name: "so does a run of spaces",
			in:   "@@ file a.go\n@@   old   x2\nx\n@@ new\ny\n",
			want: []Hunk{rep("a.go", 2, 2, "x", "y")},
		},
		{
			name:   "--marker makes @@ old ordinary payload",
			marker: "%%",
			in:     "%% file a.go\n%% old\n@@ old\n%% new\nz\n",
			want:   []Hunk{rep("a.go", 2, 1, "@@ old", "z")},
		},

		// --- counts (§3.4) ---
		{
			name: "x1 is accepted and means the same as bare",
			in:   "@@ file a.go\n@@ old x1\nx\n@@ new\ny\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y")},
		},
		{
			name: "x3 means exactly three",
			in:   "@@ file a.go\n@@ old x3\nx\n@@ new\ny\n",
			want: []Hunk{rep("a.go", 2, 3, "x", "y")},
		},

		// --- the other four ops (§6.6), and the register they set ---
		{
			name: "create",
			in:   "@@ create a.go\npackage a\n",
			want: []Hunk{body(OpCreate, "a.go", 1, "package a")},
		},
		{
			name: "create sets the register, so @@ old may follow it (§3.5)",
			in:   "@@ create a.go\nx\n@@ old\nx\n@@ new\ny\n",
			want: []Hunk{body(OpCreate, "a.go", 1, "x"), rep("a.go", 3, 1, "x", "y")},
		},
		{
			name: "append and prepend",
			in:   "@@ append a.go\nx\n@@ prepend b.go\ny\n",
			want: []Hunk{body(OpAppend, "a.go", 1, "x"), body(OpPrepend, "b.go", 3, "y")},
		},
		{
			name: "delete takes no payload and sets the register",
			in:   "@@ delete a.go\n@@ old\nx\n@@ new\ny\n",
			want: []Hunk{body(OpDelete, "a.go", 1, ""), rep("a.go", 2, 1, "x", "y")},
		},
		{
			name: "a path may contain spaces",
			in:   "@@ delete some file.go\n",
			want: []Hunk{body(OpDelete, "some file.go", 1, "")},
		},

		// --- CRLF and control characters ---
		// One trailing CR is a directive line's ending. Payload bytes are the
		// parser's to keep: whether a payload's CRs are line endings is
		// --eol's question, answered in convert.
		{
			name: "a wholly CRLF patch, every directive",
			in: "@@ file a.go\r\n@@ old x2\r\nx\r\n@@ new\r\ny\r\n" +
				"@@ create b.go\r\nB\r\n@@ append c.go\r\nC\r\n@@ prepend d.go\r\nD\r\n" +
				"@@ delete e.go\r\n@@ end\r\n",
			want: []Hunk{
				rep("a.go", 2, 2, "x\r", "y\r"),
				body(OpCreate, "b.go", 6, "B\r"),
				body(OpAppend, "c.go", 8, "C\r"),
				body(OpPrepend, "d.go", 10, "D\r"),
				body(OpDelete, "e.go", 12, ""),
			},
		},
		{
			name: "a CRLF directive in an LF patch",
			in:   "@@ file a.go\r\n@@ old\nx\n@@ new\ny\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y")},
		},
		{
			name: "@@ end with a CR ends a create's payload, which it used to join",
			in:   "@@ create c.txt\nhello\n@@ end\r\n",
			want: []Hunk{body(OpCreate, "c.txt", 1, "hello")},
		},
		{
			name: "and a replace's, which it used to write into the file",
			in:   "@@ file a.go\n@@ old\nx\n@@ new\ny\n@@ end\r\n",
			want: []Hunk{rep("a.go", 2, 1, "x", "y")},
		},
		{
			name: "@@ end with a CR and no final newline",
			in:   "@@ delete a.go\n@@ end\r",
			want: []Hunk{body(OpDelete, "a.go", 1, "")},
		},
		{
			name: "trailing spaces before the CR are trimmed as before",
			in:   "@@ delete a.go  \r\n",
			want: []Hunk{body(OpDelete, "a.go", 1, "")},
		},
		{
			name: "a CR inside a path stays, for Resolve to refuse",
			in:   "@@ delete a\rb.go\n",
			want: []Hunk{body(OpDelete, "a\rb.go", 1, "")},
		},
		{
			name: "only one CR is a line ending, so a second stays, for Resolve to refuse",
			in:   "@@ delete a.go\r\r\n",
			want: []Hunk{body(OpDelete, "a.go\r", 1, "")},
		},
		{
			name: "a tab inside a path is allowed, as it always was",
			in:   "@@ delete a\tb.go\n",
			want: []Hunk{body(OpDelete, "a\tb.go", 1, "")},
		},
		{
			name: "in a patch that mixes endings, an old that is one CR is a CR",
			in:   "@@ file a.go\n@@ old\n\r\n@@ new\nx\n",
			want: []Hunk{rep("a.go", 2, 1, "\r", "x")},
		},
		{
			name:   "--marker makes a directive with a control character stuck to it payload",
			marker: "%%",
			in:     "%% create a.txt\n@@ end\f\n",
			want:   []Hunk{body(OpCreate, "a.txt", 1, "@@ end\f")},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			marker := c.marker
			if marker == "" {
				marker = DefaultMarker
			}
			got, err := Parse([]byte(c.in), marker)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(got.Hunks) != len(c.want) {
				t.Fatalf("got %d hunks, want %d: %+v", len(got.Hunks), len(c.want), got.Hunks)
			}
			for i := range c.want {
				if !hunkEqual(got.Hunks[i], c.want[i]) {
					t.Errorf("hunk %d:\n got %s line=%d count=%d old=%q new=%q body=%q\nwant %s line=%d count=%d old=%q new=%q body=%q",
						i,
						got.Hunks[i].Op, got.Hunks[i].Line, got.Hunks[i].Count, got.Hunks[i].Old, got.Hunks[i].New, got.Hunks[i].Body,
						c.want[i].Op, c.want[i].Line, c.want[i].Count, c.want[i].Old, c.want[i].New, c.want[i].Body)
				}
				if got.Hunks[i].Path != c.want[i].Path {
					t.Errorf("hunk %d path: got %q want %q", i, got.Hunks[i].Path, c.want[i].Path)
				}
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name   string
		marker string
		in     string
		line   int    // expected ParseError.Line
		msg    string // expected substring
	}{
		{"empty patch", "", "", 0, "empty"},
		{"whitespace only is payload, not a patch", "", "   \n", 1, "expected a directive"},
		{"payload before any directive", "", "x\n@@ file a.go\n", 1, "expected a directive"},
		{"a file with no hunks", "", "@@ file a.go\n", 1, "no hunks"},

		{"@@ old with no file in effect", "", "@@ old\nx\n@@ new\ny\n", 1, "no file is in effect"},
		{"@@ old with no @@ new", "", "@@ file a.go\n@@ old\nx\n", 2, "has no"},
		{"@@ old closed by the wrong directive", "", "@@ file a.go\n@@ old\nx\n@@ delete b.go\n", 4, "expected"},
		{"@@ new with no @@ old", "", "@@ file a.go\n@@ new\ny\n", 2, "with no"},

		// An empty old matches everywhere: strings.Count(s, "") is len(s)+1,
		// and on an empty file it is 1, so it would apply.
		{"an empty old", "", "@@ file a.go\n@@ old\n@@ new\nx\n", 2, "every position"},
		{"an old of one blank line joins to nothing", "", "@@ file a.go\n@@ old\n\n@@ new\nx\n", 2, "every position"},
		// Found by fuzzing the CRLF rule before it was committed: in a CRLF
		// patch the blank line is "\r", which --eol auto reads as empty.
		{"and so does a CRLF patch's", "", "@@ file a.go\r\n@@ old\r\n\r\n@@ new\r\nx\r\n", 2, "every position"},
		{"@@ new takes no argument", "", "@@ file a.go\n@@ old\nx\n@@ new x2\ny\n", 4, "takes no argument"},

		{"@@ file with no path", "", "@@ file\n", 1, "needs a path"},
		{"@@ create with no path", "", "@@ create\nx\n", 1, "needs a path"},
		{"@@ delete with no path", "", "@@ delete\n", 1, "needs a path"},

		{"x0 is refused", "", "@@ file a.go\n@@ old x0\nx\n@@ new\ny\n", 2, "exactly zero"},
		{"a bare x is not a count", "", "@@ file a.go\n@@ old x\nx\n@@ new\ny\n", 2, "not an occurrence count"},
		{"a negative count", "", "@@ file a.go\n@@ old x-1\nx\n@@ new\ny\n", 2, "not an occurrence count"},
		{"a non-numeric count", "", "@@ file a.go\n@@ old xfoo\nx\n@@ new\ny\n", 2, "not an occurrence count"},
		{"a space inside the count", "", "@@ file a.go\n@@ old x 3\nx\n@@ new\ny\n", 2, "not an occurrence count"},
		{"a count with no x", "", "@@ file a.go\n@@ old 3\nx\n@@ new\ny\n", 2, "not an occurrence count"},
		{"an overflowing count", "", "@@ file a.go\n@@ old x99999999999999999999\nx\n@@ new\ny\n", 2, "not an occurrence count"},

		{"@@ end takes no argument", "", "@@ delete a.go\n@@ end now\n", 2, "takes no argument"},
		{"content after @@ end", "", "@@ delete a.go\n@@ end\njunk\n", 3, "terminates the patch"},
		{"content after @@ end with a CR", "", "@@ delete a.go\r\n@@ end\r\njunk\r\n", 3, "terminates the patch"},

		// A directive with a control character stuck to it used to become
		// payload in silence. Named, in a message with no raw control byte.
		{"a form feed after @@ end", "", "@@ delete a.go\n@@ end\f\n", 2, `would be the directive "@@ end" but for U+000C in it`},
		{"and inside a payload, where it was absorbed", "", "@@ create a.go\nx\n@@ end\f\n", 3, `"@@ end" but for U+000C`},
		{"an escape after @@ old", "", "@@ file a.go\n@@ old\x1b\nx\n@@ new\ny\n", 2, `"@@ old" but for U+001B`},
		{"an escape before the word", "", "@@ file a.go\n@@ \x1bold\nx\n@@ new\ny\n", 2, `"@@ old" but for U+001B`},
		{"a control character inside the marker", "", "@\x7f@ delete a.go\n", 1, `"@@ delete" but for U+007F`},
		{"a C1 control after the word", "", "@@ delete a.go\n@@ end\u0085\n", 2, `"@@ end" but for U+0085`},
		{"a vertical tab as the separator", "", "@@\x0bdelete a.go\n", 1, `"@@ delete" but for U+000B`},
		{"a second CR is not a line ending", "", "@@ delete a.go\n@@ end\r\r\n", 2, `"@@ end" but for U+000D`},
		{"a lone C1 byte after the word, which is not UTF-8", "", "@@ delete a.go\n@@ end\x9b\n", 2, `"@@ end" but for 0x9B`},
		{"the message says what to do", "", "@@ delete a.go\n@@ end\f\n", 2, "remove it, or if the line is payload text, choose another directive prefix with --marker"},

		{"an empty marker", "%%%", "", 0, "marker"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			marker := c.marker
			if marker == "" {
				marker = DefaultMarker
			}
			if c.name == "an empty marker" {
				marker = ""
			}
			_, err := Parse([]byte(c.in), marker)
			if err == nil {
				t.Fatal("want an error, got none")
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("want *ParseError, got %T: %v", err, err)
			}
			if pe.Line != c.line {
				t.Errorf("line: got %d want %d (%v)", pe.Line, c.line, err)
			}
			if !bytes.Contains([]byte(pe.Msg), []byte(c.msg)) {
				t.Errorf("message %q does not contain %q", pe.Msg, c.msg)
			}
			if name, ok := firstControl(pe.Msg); ok {
				t.Errorf("message %q prints %s raw", pe.Msg, name)
			}
		})
	}
}

// Patch.CRLF is "every line the patch terminates ends in CRLF", and only that
// lets --eol auto read a payload's CRs as line endings. A patch that mixes
// endings keeps its bytes.
func TestParseSaysWhetherThePatchIsCRLF(t *testing.T) {
	for _, c := range []struct {
		name, in string
		want     bool
	}{
		{"every line CRLF", "@@ create a\r\nx\r\n", true},
		{"an unterminated last line does not count", "@@ create a\r\nx", true},
		{"a CRLF line among LF ones", "@@ create a\r\nx\n", false},
		{"LF", "@@ create a\nx\n", false},
		{"no newline at all", "@@ delete a", false},
		{"a CR alone at the end of a line still makes it CRLF", "@@ create a\r\nx\r\r\n", true},
		{"a CR inside a line does not", "@@ create a\nx\ry\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := Parse([]byte(c.in), DefaultMarker)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if p.CRLF != c.want {
				t.Errorf("CRLF = %v, want %v", p.CRLF, c.want)
			}
		})
	}
}

// showControls is what a message prints in place of text it is refusing.
func TestShowControls(t *testing.T) {
	for in, want := range map[string]string{
		"plain.go":         "plain.go",
		"x\x1b[31mred.txt": "x<U+001B>[31mred.txt",
		"a\rb":             "a<U+000D>b",
		"\x00":             "<U+0000>",
		"a\u009bb\u0085":   "a<U+009B>b<U+0085>",
		"a\x7f":            "a<U+007F>",
		"a\tb":             "a\tb",
		"café":             "café",
		"bad\xff":          "bad\xff",
		"a\x9bb":           "a<0x9B>b",
		"\x80":             "<0x80>",
	} {
		if got := showControls(in); got != want {
			t.Errorf("showControls(%q) = %q, want %q", in, got, want)
		}
	}
}

// §2 goal 8: "An agent that has never seen the tool uses it correctly on its
// second call." That only holds if what `hunk format` prints is what the parser
// accepts, so the help text's own example is parsed here. A grammar change the
// example does not survive fails the build, which is the point.
func TestFormatExampleParses(t *testing.T) {
	p, err := Parse([]byte(formatExamplePatch), DefaultMarker)
	if err != nil {
		t.Fatalf("the example in `hunk format` does not parse: %v", err)
	}
	// §5.1's success output for this patch reads "3 files, 4 hunks".
	if len(p.Hunks) != 4 {
		t.Errorf("got %d hunks, want 4", len(p.Hunks))
	}
	files := map[string]bool{}
	for _, h := range p.Hunks {
		files[h.Path] = true
	}
	if len(files) != 3 {
		t.Errorf("got %d files, want 3: %v", len(files), files)
	}
	// The create must end in a newline. The example teaches by being copied,
	// and a Go file without one is a thing gofmt immediately undoes.
	last := p.Hunks[len(p.Hunks)-1]
	if last.Op != OpCreate {
		t.Fatalf("last hunk is %s, want create", last.Op)
	}
	if !bytes.HasSuffix(last.Body, []byte("\n")) {
		t.Errorf("the created file does not end in a newline: %q", last.Body)
	}
	if !bytes.Contains([]byte(formatDoc), []byte(formatExamplePatch)) {
		t.Error("`hunk format` does not print the example it is tested against")
	}
}

// The parser never touches the disk, which is what lets §6.1 promise "any
// syntax error, exit 1, nothing read" structurally rather than by discipline.
// Asserted on the import list rather than trusted, in the habit of the other
// gates here.
func TestParserHasNoFileAccess(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "patch.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse patch.go: %v", err)
	}
	// unicode/utf8 joined on 2026-10-06, to tell a lone C1 byte from a byte of
	// another character; it decodes bytes in memory and opens nothing.
	allowed := map[string]bool{"bytes": true, "fmt": true, "strconv": true, "strings": true, "unicode/utf8": true}
	for _, im := range f.Imports {
		path, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", im.Path.Value, err)
		}
		if !allowed[path] {
			t.Errorf("patch.go imports %q; the parser reads no files and needs no I/O", path)
		}
	}
}

// The skill is a file in this repository and directiveWords is code in it, so
// "the skill documents every operation" is checkable rather than a habit. It
// was not one: create, delete, append and prepend shipped on 2026-09-04 and
// SKILL.md named none of them until 2026-09-06, when a field report said "there
// is no @@ delete", which is what the skill had told it.
//
// "end" is skipped, and named here rather than filtered quietly: it terminates
// a patch, every example in the skill is a heredoc whose own terminator does
// that job, and leaving it undocumented is a decision rather than an omission.
func TestTheSkillDocumentsEveryDirective(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".agents", "skills", "hunk", "SKILL.md"))
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	for word := range directiveWords {
		if word == "end" {
			continue
		}
		d := DefaultMarker + " " + word
		if !strings.Contains(string(b), d) {
			t.Errorf("SKILL.md never says %q; an operation an agent cannot see is one it will not use", d)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add("@@ file a.go\n@@ old\nx\n@@ new\ny\n")
	f.Add(formatExamplePatch)
	f.Add("@@ old x0\n")
	f.Add("@@ end\n")
	f.Add("@@ -1,3 +1,4 @@\n")
	f.Add("@@\t\tcreate  \n")
	f.Add("")
	f.Add("@@ create c.txt\r\nhello\r\n@@ end\r\n")
	f.Add("@@ delete a.go\n@@ end\f\n")
	f.Add("@@\x0bdelete a\n")
	f.Add("@@ file a\n@@ old x2\nx\n\n@@ new\n\n@@ end\n")

	f.Fuzz(func(t *testing.T, s string) {
		// Total: every input is either a Patch or a *ParseError, never a panic.
		p, err := Parse([]byte(s), DefaultMarker)
		if err != nil {
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("non-ParseError %T: %v", err, err)
			}
			// A parse error names a control character; it never prints one.
			if name, ok := firstControl(pe.Error()); ok {
				t.Fatalf("%q prints %s raw", pe.Error(), name)
			}
		} else {
			if len(p.Hunks) == 0 {
				t.Fatal("a successful parse with no hunks")
			}
			for _, h := range p.Hunks {
				if h.Path == "" {
					t.Errorf("hunk at line %d has no path", h.Line)
				}
				if h.Op == OpReplace && h.Count < 1 {
					t.Errorf("replace at line %d has count %d", h.Line, h.Count)
				}
				if h.Op == OpReplace && len(h.Old) == 0 {
					t.Errorf("replace at line %d has an empty old, which matches everywhere", h.Line)
				}
				// Nor empty as --eol auto reads a CRLF patch's old.
				if h.Op == OpReplace && p.CRLF && len(stripCR(h.Old)) == 0 {
					t.Errorf("replace at line %d has an old that --eol auto reads as empty: %q", h.Line, h.Old)
				}
			}
		}

		// A CRLF patch is the same patch. Every \n made \r\n changes no
		// directive, path, count or line, and no error's line, and leaves each
		// payload line one CR longer, which is exactly what --eol auto takes
		// away again. So "@@ end" with a CR ends a payload as "@@ end" does.
		// Asked of inputs with no CR of their own, whose lines would end
		// differently once converted.
		if strings.Contains(s, "\r") {
			return
		}
		q, qerr := Parse([]byte(strings.ReplaceAll(s, "\n", "\r\n")), DefaultMarker)
		if (err == nil) != (qerr == nil) {
			t.Fatalf("LF: %v\nCRLF: %v", err, qerr)
		}
		if err != nil {
			var pe, qe *ParseError
			errors.As(err, &pe)
			errors.As(qerr, &qe)
			if pe.Line != qe.Line {
				t.Fatalf("LF fails at line %d, CRLF at %d: %v / %v", pe.Line, qe.Line, err, qerr)
			}
			return
		}
		if strings.Contains(s, "\n") && !q.CRLF {
			t.Errorf("a patch with every \\n made \\r\\n is not CRLF")
		}
		if len(q.Hunks) != len(p.Hunks) {
			t.Fatalf("LF has %d hunks, CRLF %d", len(p.Hunks), len(q.Hunks))
		}
		for i, a := range p.Hunks {
			b := q.Hunks[i]
			if a.Op != b.Op || a.Path != b.Path || a.Line != b.Line || a.Count != b.Count {
				t.Errorf("hunk %d: LF %s %q line %d x%d, CRLF %s %q line %d x%d",
					i+1, a.Op, a.Path, a.Line, a.Count, b.Op, b.Path, b.Line, b.Count)
			}
			for _, pair := range [][2][]byte{{a.Old, b.Old}, {a.New, b.New}, {a.Body, b.Body}} {
				if !bytes.Equal(stripCR(pair[1]), pair[0]) {
					t.Errorf("hunk %d: LF payload %q, CRLF payload %q", i+1, pair[0], pair[1])
				}
			}
		}
	})
}

func TestOpString(t *testing.T) {
	for op, want := range map[Op]string{
		OpReplace: "replace", OpCreate: "create", OpAppend: "append",
		OpPrepend: "prepend", OpDelete: "delete",
	} {
		if got := op.String(); got != want {
			t.Errorf("Op(%d).String() = %q, want %q", uint8(op), got, want)
		}
	}
	if got := Op(200).String(); got != "Op(200)" {
		t.Errorf("unknown op printed %q", got)
	}
}

// Line 0 means the error is about the patch as a whole, and the message must
// not then claim a line that does not exist.
func TestParseErrorMessage(t *testing.T) {
	if got := (&ParseError{Line: 9, Msg: "no"}).Error(); got != "patch line 9: no" {
		t.Errorf("got %q", got)
	}
	if got := (&ParseError{Line: 0, Msg: "the patch is empty"}).Error(); got != "the patch is empty" {
		t.Errorf("got %q", got)
	}
}
