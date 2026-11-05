package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// When an earlier hunk in the batch changed the file, a refusal's line numbers
// count against the file as it left it, not the one on disk, and the report
// says so. Until 2026-11-05 four reports returned before the note: too many,
// overlap, mixed endings and no room, so "at lines 1, 4" could name copies at
// lines 1 and 3 on disk with nothing said. Too many said it in --json neither.

const shiftNote = "line numbers count against the file as the hunk before this one would have left it,\n" +
	"  not as it is on disk: nothing was written.\n"

// shifted is each report, with the hunk that shifts the file and without it.
// The shift adds a line above every copy, so each line number moves by one.
var shifted = []struct {
	name, golden string
	file         string
	shift        string // a hunk that adds a line above the copies
	failing      string
	lines        string // as the shifted report prints them
	json         string // where --json carries the shift
}{
	{
		"too many", "shifted-too-many", "x\nA\nx\n",
		"@@ old\nA\n@@ new\nB\nC\n", "@@ old\nx\n@@ new\ny\n",
		"at lines 1, 4\n", "failure",
	},
	{
		"overlap", "shifted-overlap", "A\na\na\na\n",
		"@@ old\nA\n@@ new\nB\nC\n", "@@ old x2\na\na\n@@ new\nz\n",
		"at lines 3, 4\n", "near_miss",
	},
	{
		"mixed endings", "shifted-mixed-endings", "A\nk\nl\nk\r\nl\r\nk\r\n",
		"@@ old\nA\n@@ new\nB\nC\n", "@@ old\nk\nl\n@@ new\nz\n",
		"at lines 3 (LF), 5 (CRLF)\n", "near_miss",
	},
	{
		"no room", "shifted-no-room", "a\nb\n",
		"@@ old\na\n@@ new\nX\na\n", "@@ old\np\nq\nr\na\nb\n@@ new\nz\n",
		"line 4 of your old is at line 2,", "near_miss",
	},
}

func TestAShiftedRefusalSaysItsLinesAreShifted(t *testing.T) {
	for _, c := range shifted {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": c.file})
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, nil, "@@ file f.txt\n"+c.shift+c.failing)
			if code != exitNoMatch || out != "" {
				t.Errorf("exit %d, stdout %q; want %d and nothing", code, out, exitNoMatch)
			}
			if !strings.Contains(errOut, c.lines) {
				t.Errorf("does not say %q:\n%s", c.lines, errOut)
			}
			if !strings.HasSuffix(errOut, "  "+shiftNote) {
				t.Errorf("does not end with the note on shifted lines:\n%s", errOut)
			}
			golden(t, c.golden, errOut)
			assertUnchanged(t, root, before)
		})
		// The control: with no hunk before it nothing shifted, the numbers are
		// the disk's, and the note would be false.
		t.Run(c.name+", nothing before it", func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": c.file})
			code, _, errOut := runCLI(t, root, nil, "@@ file f.txt\n"+c.failing)
			if code != exitNoMatch {
				t.Errorf("exit %d, want %d", code, exitNoMatch)
			}
			if strings.Contains(errOut, "line numbers count against") {
				t.Errorf("says the lines are shifted when nothing came before:\n%s", errOut)
			}
		})
	}
}

// Two hunks before it, in the plural, through a report that returned early.
func TestAShiftedTooManyCountsTheHunksBeforeIt(t *testing.T) {
	root := cliTree(t, map[string]string{"f.txt": "x\nA\nx\nD\n"})
	_, _, errOut := runCLI(t, root, nil,
		"@@ file f.txt\n@@ old\nA\n@@ new\nB\nC\n@@ old\nD\n@@ new\nE\n@@ old\nx\n@@ new\ny\n")
	if !strings.Contains(errOut, "as the 2 hunks before this one would have left it") {
		t.Errorf("want the plural:\n%s", errOut)
	}
}

// --json carries everything the text does (§5.1): the number of hunks that
// shifted the file, in near_miss where a failure has one, and beside lines for
// too many, which has none. Absent where nothing shifted.
func TestAShiftedRefusalCarriesTheShiftInJSON(t *testing.T) {
	type failure struct {
		Lines    []int `json:"lines"`
		Shifted  int   `json:"shifted_by_hunks"`
		NearMiss *struct {
			Shifted int `json:"shifted_by_hunks"`
		} `json:"near_miss"`
	}
	read := func(t *testing.T, out string) failure {
		t.Helper()
		var r struct{ Failures []failure }
		if err := json.Unmarshal([]byte(out), &r); err != nil || len(r.Failures) != 1 {
			t.Fatalf("want one failure: %v\n%s", err, out)
		}
		return r.Failures[0]
	}
	for _, c := range shifted {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": c.file})
			_, out, _ := runCLI(t, root, []string{"--json"}, "@@ file f.txt\n"+c.shift+c.failing)
			f := read(t, out)
			inNear := 0
			if f.NearMiss != nil {
				inNear = f.NearMiss.Shifted
			}
			switch c.json {
			case "failure":
				if f.Shifted != 1 || f.NearMiss != nil {
					t.Errorf("want shifted_by_hunks 1 beside lines and no near_miss:\n%s", out)
				}
			case "near_miss":
				if inNear != 1 || f.Shifted != 0 {
					t.Errorf("want near_miss.shifted_by_hunks 1 and none beside lines:\n%s", out)
				}
			}
			_, out, _ = runCLI(t, cliTree(t, map[string]string{"f.txt": c.file}), []string{"--json"}, "@@ file f.txt\n"+c.failing)
			if strings.Contains(out, "shifted_by_hunks") {
				t.Errorf("carries a shift when nothing came before:\n%s", out)
			}
		})
	}
}

// Every kind of diagnosis but no anchor, which prints no line, carries the note
// when it is shifted. The kinds are read from nearmiss.go's const block, so a
// kind added there is held to it without a row: the four that returned early
// were each added after the note was written.
func TestEveryDiagnosisKindButNoAnchorCarriesTheShift(t *testing.T) {
	kinds := diagKindsDeclared(t)
	if len(kinds) < 9 {
		t.Fatalf("read %d kinds from nearmiss.go: %v; the reader is out of date", len(kinds), kinds)
	}
	for i, name := range kinds {
		t.Run(name, func(t *testing.T) {
			d := &Diagnosis{
				Kind: DiagKind(i), Shifted: 1, Line: 3, DiffLine: 1, Column: 1, Found: 2,
				Lines: []int{3, 7}, Span: [][]byte{[]byte("a")}, Yours: []byte("a"), Theirs: []byte("b"),
				Detail: "detail", Headline: "headline",
			}
			got := d.Render("  ")
			if has := strings.Contains(got, "line numbers count against"); has != (name != "DiagNoAnchor") {
				t.Errorf("note present %v, want %v:\n%s", has, name != "DiagNoAnchor", got)
			}
		})
	}
}

// diagKindsDeclared is the DiagKind constants in nearmiss.go, in their iota order.
func diagKindsDeclared(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "nearmiss.go", nil, 0)
	must(t, err)
	var kinds []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		typed := false
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); ok {
				typed = id.Name == "DiagKind"
			}
			if typed {
				for _, n := range vs.Names {
					kinds = append(kinds, n.Name)
				}
			}
		}
	}
	return kinds
}
