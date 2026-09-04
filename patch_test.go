package main

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

func hunkEqual(a, b Hunk) bool {
	return a.Op == b.Op && a.Path == b.Path && a.Line == b.Line && a.Count == b.Count &&
		bytes.Equal(a.Old, b.Old) && bytes.Equal(a.New, b.New) && bytes.Equal(a.Body, b.Body)
}

// rep builds an expected replace hunk. new is spelled repl because new is a
// builtin.
func rep(path string, line, count int, old, repl string) Hunk {
	return Hunk{Op: OpReplace, Path: path, Line: line, Count: count,
		Old: []byte(old), New: []byte(repl)}
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
		})
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
	allowed := map[string]bool{"bytes": true, "fmt": true, "strconv": true, "strings": true}
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

func FuzzParse(f *testing.F) {
	f.Add("@@ file a.go\n@@ old\nx\n@@ new\ny\n")
	f.Add(formatExamplePatch)
	f.Add("@@ old x0\n")
	f.Add("@@ end\n")
	f.Add("@@ -1,3 +1,4 @@\n")
	f.Add("@@\t\tcreate  \n")
	f.Add("")

	f.Fuzz(func(t *testing.T, s string) {
		// Total: every input is either a Patch or a *ParseError, never a panic.
		p, err := Parse([]byte(s), DefaultMarker)
		if err != nil {
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("non-ParseError %T: %v", err, err)
			}
			return
		}
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
