package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The block the rows below misplace, three lines that old asks for exactly.
const block = "alpha\nbeta\ngamma"

// Every span as near as the one shown is named: every other span a
// normalization explains, or under the fallback every other with as few
// differing lines. Until 2026-10-07 the report showed the first and said
// nothing of the rest, and pasting it edited that copy at exit 0, whichever one
// the agent meant.
func TestDiagnoseNamesEverySpanAsNear(t *testing.T) {
	copies := func(n int, body string) string {
		return strings.Repeat(body+"\nsep\n", n)
	}
	for _, c := range []struct {
		name, file string
		kind       DiagKind
		line       int
		also       []int
		more, same int
	}{
		{"one copy", "start\nalpha\nbeta \ngamma\nend\n", DiagNamed, 2, nil, 0, 0},
		{
			"two copies with the same bytes", "start\nalpha\nbeta \ngamma\nmid\nalpha\nbeta \ngamma\nend\n",
			DiagNamed, 2,
			[]int{6},
			0, 1,
		},
		{
			"two copies with different causes", "start\nalpha\nbeta \ngamma\nmid\nalpha\n\tbeta\ngamma\nend\n",
			DiagNamed, 2,
			[]int{6},
			0, 0,
		},
		{
			"three, one of the others with the same bytes",
			"alpha\nbeta \ngamma\nsep\nalpha\n\tbeta\ngamma\nsep\nalpha\nbeta \ngamma\n",
			DiagNamed, 1,
			[]int{5, 9},
			0, 1,
		},
		{
			"thirteen, ten listed", copies(13, "alpha\nbeta \ngamma"), DiagNamed, 1,
			[]int{5, 9, 13, 17, 21, 25, 29, 33, 37, 41},
			2, 12,
		},
		{
			"a named span and one only nearly there", "alpha\nbeta \ngamma\nsep\nalpha\nbetx\ngamma\n",
			DiagNamed, 1, nil, 0, 0,
		},
		{
			"a closest-span tie", "start\nalpha\nbetx\ngamma\nmid\nalpha\nbety\ngamma\nend\n",
			DiagClosest, 2,
			[]int{6},
			0, 0,
		},
		{
			"a closest span and one further off", "start\nalpha\nbetx\ngamma\nmid\nalpha\nbety\ngammx\nend\n",
			DiagClosest, 2, nil, 0, 0,
		},
		{
			// Anchored through gamma, the tie's earlier span is found after
			// its later one, and the list is still in file order.
			"a tie found out of file order", "alpha\nbetx\ngamma\nsep\nalphx\nbeta\ngamma\nsep\nalpha\nbety\ngamma\n",
			DiagClosest, 1,
			[]int{5, 9},
			0, 0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Diagnose([]byte(block), []byte(c.file), DefaultContext)
			if d.Kind != c.kind || d.Line != c.line {
				t.Fatalf("kind %v at line %d, want %v at line %d", d.Kind, d.Line, c.kind, c.line)
			}
			if !slices.Equal(d.Also, c.also) || d.AlsoMore != c.more || d.AlsoSame != c.same {
				t.Errorf("also %v, %d more, %d the same; want %v, %d more, %d the same",
					d.Also, d.AlsoMore, d.AlsoSame, c.also, c.more, c.same)
			}
		})
	}
}

// A span longer than --context is cut and says so. Until 2026-10-07 it was cut
// in silence, the skill said to paste the span, and pasted it replaced the
// block's first lines and left the rest at exit 0.
func TestDiagnoseSaysWhenItCutTheSpan(t *testing.T) {
	for _, c := range []struct {
		name, old, file string
		context         int
		shown, whole    int
	}{
		{"cut", "a\nb\nc\nd\ne\nf", "a\nb \nc\nd\ne\nf\n", 3, 3, 6},
		{"exactly the context", "a\nb\nc", "a\nb \nc\n", 3, 3, 0},
		{"shorter", "a\nb", "a\nb \n", 3, 2, 0},
		{"a closest span, cut", "a\nb\nc\nd", "a\nbx\nc\nd\n", 2, 2, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Diagnose([]byte(c.old), []byte(c.file), c.context)
			if len(d.Span) != c.shown || d.SpanLines != c.whole {
				t.Errorf("%d lines shown of %d, want %d of %d", len(d.Span), d.SpanLines, c.shown, c.whole)
			}
		})
	}
}

// The new lines, in both renderings, are a contract (§8.1).
func TestTheNewNearMissLinesAreGolden(t *testing.T) {
	for _, c := range []struct {
		golden, file, old string
		args              []string
	}{
		{"report-near-also", "start\nalpha\nbeta \ngamma\nmid\nalpha\n\tbeta\ngamma\nend\n", block, nil},
		{"report-near-also-json", "start\nalpha\nbeta \ngamma\nmid\nalpha\n\tbeta\ngamma\nend\n", block, []string{"--json"}},
		{"report-near-also-same", "start\nalpha\nbeta \ngamma\nmid\nalpha\nbeta \ngamma\nend\n", block, nil},
		{"report-near-also-same-json", "start\nalpha\nbeta \ngamma\nmid\nalpha\nbeta \ngamma\nend\n", block, []string{"--json"}},
		{"report-near-closest-also", "start\nalpha\nbetx\ngamma\nmid\nalpha\nbety\ngamma\nend\n", block, nil},
		{"report-near-cut", "a\nb \nc\nd\ne\nf\n", "a\nb\nc\nd\ne\nf", []string{"--context", "3"}},
		{"report-near-cut-json", "a\nb \nc\nd\ne\nf\n", "a\nb\nc\nd\ne\nf", []string{"--context", "3", "--json"}},
	} {
		t.Run(c.golden, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": c.file})
			code, out, errOut := runCLI(t, root, c.args, "@@ file f.txt\n@@ old\n"+c.old+"\n@@ new\nX\n")
			if code != exitNoMatch {
				t.Fatalf("exit %d, want %d: %s%s", code, exitNoMatch, out, errOut)
			}
			golden(t, c.golden, out+errOut)
		})
	}
}

// The cut line's advice works: rerun with the --context it names, and the span
// it then prints is the whole block, which pasted replaces all of it.
func TestTheCutSpansAdviceGetsTheWholeBlock(t *testing.T) {
	root := cliTree(t, map[string]string{"f.txt": "a\nb \nc\nd\ne\nf\n"})
	patch := "@@ file f.txt\n@@ old\na\nb\nc\nd\ne\nf\n@@ new\nX\n"
	_, _, report := runCLI(t, root, []string{"--context", "3"}, patch)
	if !strings.Contains(report, "so rerun with --context 6 before pasting") {
		t.Fatalf("the report does not name the --context that shows it all:\n%s", report)
	}
	_, _, report = runCLI(t, root, []string{"--context", "6"}, patch)
	if strings.Contains(report, "not shown") {
		t.Fatalf("still cut at --context 6:\n%s", report)
	}
	d := Diagnose([]byte("a\nb\nc\nd\ne\nf"), []byte(readFile(t, root, "f.txt")), 6)
	pasted := fmt.Sprintf("@@ file f.txt\n@@ old\n%s\n@@ new\nX\n", joinLines(d.Span))
	if code, out, errOut := runCLI(t, root, nil, pasted); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if got := readFile(t, root, "f.txt"); got != "X\n" {
		t.Errorf("f.txt = %q, want the whole block replaced", got)
	}
	_ = os.Remove(filepath.Join(root, "f.txt"))
}

// Copies are compared whole even when the span shown is cut: two copies that
// differ only below the cut are not the same bytes, and two that are the same
// are, whatever --context hid.
func TestCopiesAreComparedWholeWhenTheSpanIsCut(t *testing.T) {
	for _, c := range []struct {
		name, file string
		same       int
	}{
		{"the same below the cut", "a\nb \nc\nd\nsep\na\nb \nc\nd\n", 1},
		{"different only below the cut", "a\nb \nc\nd\nsep\na\nb \nc\n\td\n", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Diagnose([]byte("a\nb\nc\nd"), []byte(c.file), 2)
			if d.SpanLines != 4 || !slices.Equal(d.Also, []int{6}) || d.AlsoSame != c.same {
				t.Errorf("%d of %d lines, also %v, %d the same; want 2 of 4, [6], %d",
					len(d.Span), d.SpanLines, d.Also, d.AlsoSame, c.same)
			}
		})
	}
}

// Every form the two new lines take, rendered, with the line each must end in.
func TestTheNewLinesInEveryForm(t *testing.T) {
	for _, c := range []struct{ name, old, file, last string }{
		{
			"one line not shown", "a\nb\nc", "a\nb \nc\n",
			"... 1 more line not shown (--context 2): old needs all 3, so rerun with --context 3 before pasting",
		},
		{
			"several, listed and counted, all the same bytes", block, strings.Repeat("alpha\nbeta \ngamma\nsep\n", 13),
			"also nearly there at lines 5, 9, 13, 17, 21, 25, 29, 33, 37, 41, and 2 more; the span above is line 1's, " +
				"and all of them have the same bytes, so pasted alone it matches more than once: add context",
		},
		{
			"two others, one the same", block, "alpha\nbeta \ngamma\nsep\nalpha\n\tbeta\ngamma\nsep\nalpha\nbeta \ngamma\n",
			"also nearly there at lines 5, 9; the span above is line 1's, " +
				"and one of them has the same bytes, so pasted alone it matches more than once: add context",
		},
		{
			"three others, two the same", block,
			"alpha\nbeta \ngamma\nsep\nalpha\n\tbeta\ngamma\nsep\nalpha\nbeta \ngamma\nsep\nalpha\nbeta \ngamma\n",
			"also nearly there at lines 5, 9, 13; the span above is line 1's, " +
				"and 2 of them have the same bytes, so pasted alone it matches more than once: add context",
		},
		{
			"two others, none the same", block, "alpha\nbeta \ngamma\nsep\nalpha\n\tbeta\ngamma\nsep\nalpha\n beta\ngamma\n",
			"also nearly there at lines 5, 9; the span above is line 1's, and pasted it edits that copy only",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			context := DefaultContext
			if c.name == "one line not shown" {
				context = 2
			}
			out := strings.TrimRight(Diagnose([]byte(c.old), []byte(c.file), context).Render(""), "\n")
			lines := strings.Split(out, "\n")
			got := strings.TrimSpace(lines[len(lines)-1])
			if c.name == "one line not shown" {
				got = strings.TrimSpace(lines[len(lines)-2])
			}
			if got != c.last {
				t.Errorf("got\n%s\nwant\n%s", got, c.last)
			}
		})
	}
}
