package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// TestLeadingSlip (#192): the start of a change, judged on its own. The line
// above joined on where old begins with a line break, a non-empty new does
// not, and the byte before the replacement in what the hunk left is not a
// newline; an empty line before it where new begins with one, old does not,
// a line starts there, and new is more than line breaks. Rows the CLI table
// cannot reach are here: a CR before the match, which --eol strict can leave,
// and a new that begins with a CRLF, whose quote is past both bytes.
func TestLeadingSlip(t *testing.T) {
	for _, c := range []struct {
		name            string
		next            string
		e               int
		old, repl, text string
		kind            SlipKind
	}{
		{"joined onto a line of text", "aB\n", 1, "\nb", "B", "aB", SlipJoinedAbove},
		{"an old that begins with a CRLF", "aB\r\n", 1, "\r\nb", "B", "aB", SlipJoinedAbove},
		{"a CR before the match, which is not a line break", "a\rB\n", 2, "\nb", "B", "a\rB", SlipJoinedAbove},
		{"a new that begins with a CR alone, which is not one", "a\rB\n", 1, "\nb", "\rB", "a\rB", SlipJoinedAbove},
		{"the replacement is the file's last text", "aB", 1, "\nb", "B", "aB", SlipJoinedAbove},
		{"an empty line before", "a\n\nB\n", 2, "b", "\nB", "B", SlipEmptyLineBefore},
		{"an empty line before, from a new that begins with a CRLF", "a\r\n\r\nB\r\n", 3, "b", "\r\nB", "B", SlipEmptyLineBefore},
		{"an empty line before, at the file's start", "\nB\n", 0, "b", "\nB", "B", SlipEmptyLineBefore},
		{"an empty line before a second one", "a\n\n\nB\n", 2, "b", "\n\nB", "", SlipEmptyLineBefore},
		{"a new of a line break and a CR has text", "a\n\n\r", 2, "b", "\n\r", "", SlipEmptyLineBefore},

		{"the line above is empty, so old's break was its own", "a\nB\n", 2, "\nb", "B", "", 0},
		{"the file's first line was empty", "B\n", 0, "\nb", "B", "", 0},
		{"an empty new", "a\n", 1, "\nb", "", "", 0},
		{"both begin with a line break", "a\nB\n", 1, "\nb", "\nB", "", 0},
		{"both begin with one, in different endings", "a\r\nB\n", 1, "\nb", "\r\nB", "", 0},
		{"neither does", "aB\n", 1, "b", "B", "", 0},
		{"a new that begins with a line break inside a line", "a \nB\n", 2, "b", "\nB", "", 0},
		{"a new of nothing but a line break", "a\n\n\n", 2, "b", "\n", "", 0},
		{"a new of nothing but CRLFs", "a\r\n\r\n\r\n\r\n", 3, "b", "\r\n\r\n", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.repl != "" && !strings.HasPrefix(c.next[c.e:], c.repl) {
				t.Fatalf("bad row: next[%d:] = %q does not start with %q", c.e, c.next[c.e:], c.repl)
			}
			kind, text := leadingSlip([]byte(c.next), c.e, []byte(c.old), []byte(c.repl))
			if kind != c.kind || string(text) != c.text {
				t.Errorf("leadingSlip = %d %q, want %d %q", kind, text, c.kind, c.text)
			}
			if kind == 0 && text != nil {
				t.Errorf("no slip, and a quote %q", text)
			}
		})
	}
}

// The leading slips' goldens (#192), on the issue's shape and its mirror,
// beside the issue's patch where the empty line is there, which has none,
// and under @@ old x2: applied, a dry run, whose remedies are the patch's,
// and --json, whose kinds are #111's joined_line and empty_line.
func TestALeadingSlipGoldens(t *testing.T) {
	files := map[string]string{
		"f.md":  "- a\n- b\nEnd.\n",
		"g.md":  "- a\n- b\n\nEnd.\n",
		"e.txt": "a\nb\n",
		"x.txt": "a\nb\na\nb\n",
	}
	patch := "@@ file f.md\n@@ old\n\nEnd.\n@@ new\n- c\n\nEnd.\n" +
		"@@ file g.md\n@@ old\n\nEnd.\n@@ new\n- c\n\nEnd.\n" +
		"@@ file e.txt\n@@ old\nb\n@@ new\n\nB\n" +
		"@@ file x.txt\n@@ old x2\n\nb\n@@ new\nB\n"
	t.Run("text", func(t *testing.T) {
		root := cliTree(t, files)
		_, out, errOut := runCLI(t, root, nil, patch)
		golden(t, "cli-slip-leading", out+errOut)
		for name, want := range map[string]string{
			"f.md":  "- a\n- b- c\n\nEnd.\n",
			"g.md":  "- a\n- b\n- c\n\nEnd.\n",
			"e.txt": "a\n\nB\n",
			"x.txt": "aB\naB\n",
		} {
			if got := readFile(t, root, name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
	})
	t.Run("dry run", func(t *testing.T) {
		_, out, errOut := runCLI(t, cliTree(t, files), []string{"--dry-run"}, patch)
		golden(t, "cli-slip-leading-dry-run", out+errOut)
	})
	t.Run("json", func(t *testing.T) {
		_, out, _ := runCLI(t, cliTree(t, files), []string{"--json"}, patch)
		golden(t, "cli-slip-leading-json", out)
		var r slipJSON
		must(t, json.Unmarshal([]byte(out), &r))
		if len(r.Warnings) != 3 {
			t.Fatalf("%d warnings: %s", len(r.Warnings), out)
		}
		for i, want := range []struct {
			kind       string
			hunk, line int
			path, text string
		}{
			{"joined_line", 1, 2, "f.md", "- b- c"},
			{"empty_line", 3, 18, "e.txt", "B"},
			{"joined_line", 4, 24, "x.txt", "aB"},
		} {
			w := r.Warnings[i]
			if w.Kind != want.kind || w.Hunk != want.hunk || w.PatchLine != want.line || w.Path != want.path || w.Text != want.text || w.LinesAfter != nil {
				t.Errorf("warning %d = %+v, want %+v", i+1, w, want)
			}
		}
	})
}

// TestEverySlipKindIsSaidApart: jsonKind and slipMessage each end in a
// default case, the file with no final newline, so a kind added without a
// case of its own would be warned of as that one, which the compiler does not
// see. Every Slip constant slip.go declares is listed here, with the --json
// kind its doc names, and each says something no other kind says, applied
// and not.
func TestEverySlipKindIsSaidApart(t *testing.T) {
	kinds := map[string]struct {
		kind SlipKind
		json string
	}{
		"SlipEmptyLine":        {SlipEmptyLine, "empty_line"},
		"SlipJoinedLine":       {SlipJoinedLine, "joined_line"},
		"SlipNoFinalNewline":   {SlipNoFinalNewline, "no_final_newline"},
		"SlipNoChange":         {SlipNoChange, "no_change"},
		"SlipLineEndings":      {SlipLineEndings, "no_change"},
		"SlipEmptiedLine":      {SlipEmptiedLine, "empty_line"},
		"SlipJoinedEmptyLines": {SlipJoinedEmptyLines, "empty_line"},
		"SlipTakenEmptyLine":   {SlipTakenEmptyLine, "taken_empty_line"},
		"SlipJoinedAbove":      {SlipJoinedAbove, "joined_line"},
		"SlipEmptyLineBefore":  {SlipEmptyLineBefore, "empty_line"},
	}
	f, err := parser.ParseFile(token.NewFileSet(), "slip.go", nil, 0)
	must(t, err)
	var declared []string
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
			for _, spec := range g.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if strings.HasPrefix(name.Name, "Slip") {
						declared = append(declared, name.Name)
					}
				}
			}
		}
	}
	var listed []string
	for name := range kinds {
		listed = append(listed, name)
	}
	slices.Sort(declared)
	slices.Sort(listed)
	if !slices.Equal(declared, listed) {
		t.Fatalf("slip.go declares %v; this test lists %v", declared, listed)
	}
	said := map[string]string{}
	for name, k := range kinds {
		if got := k.kind.jsonKind(); got != k.json {
			t.Errorf("%s's --json kind is %q, want %q", name, got, k.json)
		}
		s := Slip{Kind: k.kind, Hunk: 1, Line: 2, Path: "a.txt", Count: 1, Of: 1, Text: []byte("x"), Run: 2, Kept: 1}
		for _, onDisk := range []bool{true, false} {
			m := slipMessage("", s, onDisk)
			if other, ok := said[m]; ok && other != name {
				t.Errorf("%s and %s say the same:\n%s", name, other, m)
			}
			said[m] = name
		}
	}
}
