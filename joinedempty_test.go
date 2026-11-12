package main

import (
	"strings"
	"testing"
)

// The file of #169: three functions, an empty line between each.
const threeFns = "fn a() {}\n\nfn b() {}\n\nfn c() {}\n"

// The same in Python's shape, two empty lines between each.
const threeDefs = "def a():\n    pass\n\n\ndef b():\n    pass\n\n\ndef c():\n    pass\n"

// TestJoinedEmptyLinesAreWarnedOf (#169): text taken out from between two
// empty lines, its new text empty or only line breaks, lets them touch, and a
// warning after the report says how many empty lines are in a row now, exit
// unchanged. Every row is applied, so each remedy is the file's. The rows with
// no warning are the shapes that join nothing.
func TestJoinedEmptyLinesAreWarnedOf(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		patch string
		warn  []string
		want  map[string]string // the files afterwards
	}{
		{
			"#169: a function taken out from between two others",
			map[string]string{"x.zig": threeFns},
			"@@ file x.zig\n@@ old\nfn b() {}\n\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 2 empty lines in a row where "fn b() {}" was in x.zig: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty line from x.zig`},
			map[string]string{"x.zig": "fn a() {}\n\n\nfn c() {}\n"},
		},
		{
			"#169's other round: a new of line breaks makes it three",
			map[string]string{"x.zig": threeFns},
			"@@ file x.zig\n@@ old\nfn b() {}\n\n@@ new\n\n\n",
			[]string{`hunk 1 (patch line 2) leaves 3 empty lines in a row where "fn b() {}" was in x.zig: its new text is only line breaks, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty lines from x.zig`},
			map[string]string{"x.zig": "fn a() {}\n\n\n\nfn c() {}\n"},
		},
		{
			"an emptied line between two empty lines: one warning for both",
			map[string]string{"x.zig": threeFns},
			"@@ file x.zig\n@@ old\nfn b() {}\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 3 empty lines in a row where "fn b() {}" was in x.zig: its new text is empty, its old text stops before the line break, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty lines from x.zig`},
			map[string]string{"x.zig": "fn a() {}\n\n\n\nfn c() {}\n"},
		},
		{
			"an emptied line with a new of line breaks",
			map[string]string{"x.zig": threeFns},
			"@@ file x.zig\n@@ old\nfn b() {}\n@@ new\n\n\n",
			[]string{`hunk 1 (patch line 2) leaves 4 empty lines in a row where "fn b() {}" was in x.zig: its new text is only line breaks, its old text stops before the line break, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty lines from x.zig`},
			map[string]string{"x.zig": "fn a() {}\n\n\n\n\nfn c() {}\n"},
		},
		{
			"two empty lines on each side, as Python has them",
			map[string]string{"p.py": threeDefs},
			"@@ file p.py\n@@ old\ndef b():\n    pass\n\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 4 empty lines in a row where "def b():" was in p.py: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty lines from p.py`},
			map[string]string{"p.py": "def a():\n    pass\n\n\n\n\ndef c():\n    pass\n"},
		},
		{
			"a CRLF file, whose empty line is a CRLF",
			map[string]string{"a.txt": "a\r\n\r\nb\r\n\r\nc\r\n"},
			"@@ file a.txt\n@@ old\nb\n\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 2 empty lines in a row where "b" was in a.txt: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty line from a.txt`},
			map[string]string{"a.txt": "a\r\n\r\n\r\nc\r\n"},
		},
		{
			"an emptied line in a CRLF file, whose break is two bytes",
			map[string]string{"a.txt": "a\r\n\r\nb\r\n\r\nc\r\n"},
			"@@ file a.txt\n@@ old\nb\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 3 empty lines in a row where "b" was in a.txt: its new text is empty, its old text stops before the line break, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty lines from a.txt`},
			map[string]string{"a.txt": "a\r\n\r\n\r\n\r\nc\r\n"},
		},
		{
			"@@ old x2, each between empty lines",
			map[string]string{"a.txt": "a\n\nX\n\nb\n\nX\n\nc\n"},
			"@@ file a.txt\n@@ old x2\nX\n\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 2 empty lines in a row where each of its 2 replacements was in a.txt, the first where "X" was: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty lines from a.txt`},
			map[string]string{"a.txt": "a\n\n\nb\n\n\nc\n"},
		},
		{
			"two copies side by side, which join the empty lines only together",
			map[string]string{"a.txt": "a\n\nX\nX\n\nc\n"},
			"@@ file a.txt\n@@ old x2\nX\n\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 2 empty lines in a row where 1 of its 2 replacements was in a.txt, the first where "X" was: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty line from a.txt`},
			map[string]string{"a.txt": "a\n\n\nc\n"},
		},
		{
			"@@ re, whose matches are judged the same way",
			map[string]string{"a.txt": "a\n\nX1\n\nc\n"},
			"@@ file a.txt\n@@ re\nX[0-9]\n\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 2 empty lines in a row where "X1" was in a.txt: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty line from a.txt`},
			map[string]string{"a.txt": "a\n\n\nc\n"},
		},
		{
			"a last line with no newline after it, below the empty one",
			map[string]string{"a.txt": "a\n\nX\n\nc"},
			"@@ file a.txt\n@@ old\nX\n\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 2 empty lines in a row where "X" was in a.txt: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty line from a.txt`},
			map[string]string{"a.txt": "a\n\n\nc"},
		},
		{
			// Two empty lines in a row, but one of them is the break the
			// emptied line left: nothing was joined, and #150's warning says so.
			"an emptied line with an empty line below only, which is #150's warning",
			map[string]string{"a.txt": "a\nb\n\nc\n"},
			"@@ file a.txt\n@@ old\nb\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line where "b" was in a.txt: its new text is empty and its old text stops before the line break; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "a\n\n\nc\n"},
		},
		{
			"an emptied line with an empty line above only, the same",
			map[string]string{"a.txt": "a\n\nb\nc\n"},
			"@@ file a.txt\n@@ old\nb\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line where "b" was in a.txt: its new text is empty and its old text stops before the line break; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "a\n\n\nc\n"},
		},
		{
			"an empty line above only",
			map[string]string{"a.txt": "a\n\nb\nc\n"},
			"@@ file a.txt\n@@ old\nb\n\n@@ new\n",
			nil,
			map[string]string{"a.txt": "a\n\nc\n"},
		},
		{
			"an empty line below only",
			map[string]string{"a.txt": "a\nb\n\nc\n"},
			"@@ file a.txt\n@@ old\nb\n\n@@ new\n",
			nil,
			map[string]string{"a.txt": "a\n\nc\n"},
		},
		{
			"the remedy: the empty line below taken too",
			map[string]string{"x.zig": threeFns},
			"@@ file x.zig\n@@ old\nfn b() {}\n\n\n@@ new\n",
			nil,
			map[string]string{"x.zig": "fn a() {}\n\nfn c() {}\n"},
		},
		{
			// The empty line above is old's own, and fn b's line break stays,
			// which was #150's emptied line until the two runs were counted
			// with old's empty lines in them.
			"an old that starts on the empty line above and stops before its break",
			map[string]string{"x.zig": threeFns},
			"@@ file x.zig\n@@ old\n\nfn b() {}\n@@ new\n",
			[]string{`hunk 1 (patch line 2) leaves 2 empty lines in a row where "fn b() {}" was in x.zig: its new text is empty, its old text stops before the line break, and the empty lines above and below its old text now touch; if that was not meant, remove the extra empty line from x.zig`},
			map[string]string{"x.zig": "fn a() {}\n\n\nfn c() {}\n"},
		},
		{
			"an old that takes the empty line above and its own break",
			map[string]string{"x.zig": threeFns},
			"@@ file x.zig\n@@ old\n\nfn b() {}\n\n@@ new\n",
			nil,
			map[string]string{"x.zig": "fn a() {}\n\nfn c() {}\n"},
		},
		{
			"a block at the top of the file",
			map[string]string{"a.txt": "X\n\nb\n"},
			"@@ file a.txt\n@@ old\nX\n\n@@ new\n",
			nil,
			map[string]string{"a.txt": "\nb\n"},
		},
		{
			"text taken out inside a line",
			map[string]string{"a.txt": "a\n\nxXy\n\nc\n"},
			"@@ file a.txt\n@@ old\nX\n@@ new\n",
			nil,
			map[string]string{"a.txt": "a\n\nxy\n\nc\n"},
		},
		{
			"an emptied line's text, with more after it on its line",
			map[string]string{"a.txt": "a\n\nXy\n\nc\n"},
			"@@ file a.txt\n@@ old\nX\n@@ new\n",
			nil,
			map[string]string{"a.txt": "a\n\ny\n\nc\n"},
		},
		{
			"a new with text in it",
			map[string]string{"x.zig": threeFns},
			"@@ file x.zig\n@@ old\nfn b() {}\n\n@@ new\nfn d() {}\n\n",
			nil,
			map[string]string{"x.zig": "fn a() {}\n\nfn d() {}\n\nfn c() {}\n"},
		},
		{
			"empty lines taken out of a run, with the line of text above them kept",
			map[string]string{"a.txt": "a\n\n\n\nb\n"},
			"@@ file a.txt\n@@ old\na\n\n\n\n@@ new\na\n\n",
			nil,
			map[string]string{"a.txt": "a\n\nb\n"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			code, out, errOut := runCLI(t, root, nil, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			got := slipWarningsIn(errOut)
			if strings.Join(got, "\n") != strings.Join(c.warn, "\n") {
				t.Errorf("warnings\n got %q\nwant %q", got, c.warn)
			}
			for name, want := range c.want {
				if b := readFile(t, root, name); b != want {
					t.Errorf("%s = %q, want %q", name, b, want)
				}
			}
			if c.warn == nil && errOut != "" {
				t.Errorf("stderr %q", errOut)
			}
		})
	}
}

// TestJoinedEmptyLinesRemedies: before the batch is on disk the remedy is the
// patch's, and the patch it describes applies with no warning and leaves the
// file with the empty lines either side had, at most.
func TestJoinedEmptyLinesRemedies(t *testing.T) {
	for _, c := range []struct {
		name, file, patch, warn, remedy, want string
	}{
		{
			"#169's shape", threeFns,
			"@@ file f\n@@ old\nfn b() {}\n\n@@ new\n",
			`hunk 1 (patch line 2) leaves 2 empty lines in a row where "fn b() {}" was in f: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, leave another blank line under its old text, so an empty line below goes with it`,
			"@@ file f\n@@ old\nfn b() {}\n\n\n@@ new\n",
			"fn a() {}\n\nfn c() {}\n",
		},
		{
			"a new of line breaks", threeFns,
			"@@ file f\n@@ old\nfn b() {}\n\n@@ new\n\n\n",
			`hunk 1 (patch line 2) leaves 3 empty lines in a row where "fn b() {}" was in f: its new text is only line breaks, and the empty lines above and below its old text now touch; if that was not meant, take the blank lines out from under its new text, and leave another blank line under its old text, so an empty line below goes with it`,
			"@@ file f\n@@ old\nfn b() {}\n\n\n@@ new\n",
			"fn a() {}\n\nfn c() {}\n",
		},
		{
			"an emptied line", threeFns,
			"@@ file f\n@@ old\nfn b() {}\n@@ new\n",
			`hunk 1 (patch line 2) leaves 3 empty lines in a row where "fn b() {}" was in f: its new text is empty, its old text stops before the line break, and the empty lines above and below its old text now touch; if that was not meant, leave 2 blank lines under its old text, so its line break and an empty line below go with it`,
			"@@ file f\n@@ old\nfn b() {}\n\n\n@@ new\n",
			"fn a() {}\n\nfn c() {}\n",
		},
		{
			"Python's two empty lines", threeDefs,
			"@@ file f\n@@ old\ndef b():\n    pass\n\n@@ new\n",
			`hunk 1 (patch line 2) leaves 4 empty lines in a row where "def b():" was in f: its new text is empty, and the empty lines above and below its old text now touch; if that was not meant, leave 2 more blank lines under its old text, so 2 empty lines below go with it`,
			"@@ file f\n@@ old\ndef b():\n    pass\n\n\n\n@@ new\n",
			"def a():\n    pass\n\n\ndef c():\n    pass\n",
		},
		{
			"an old that starts on the empty line above, emptied: its break is all that is left", threeFns,
			"@@ file f\n@@ old\n\nfn b() {}\n@@ new\n",
			`hunk 1 (patch line 2) leaves 2 empty lines in a row where "fn b() {}" was in f: its new text is empty, its old text stops before the line break, and the empty lines above and below its old text now touch; if that was not meant, leave a blank line under its old text, so its line break goes with it`,
			"@@ file f\n@@ old\n\nfn b() {}\n\n@@ new\n",
			"fn a() {}\n\nfn c() {}\n",
		},
		{
			"the empty line below taken, but a new of line breaks: they are all that is left", threeFns,
			"@@ file f\n@@ old\nfn b() {}\n\n\n@@ new\n\n\n",
			`hunk 1 (patch line 2) leaves 2 empty lines in a row where "fn b() {}" was in f: its new text is only line breaks, and the empty lines above and below its old text now touch; if that was not meant, take the blank lines out from under its new text`,
			"@@ file f\n@@ old\nfn b() {}\n\n\n@@ new\n",
			"fn a() {}\n\nfn c() {}\n",
		},
		{
			"Python's, emptied", threeDefs,
			"@@ file f\n@@ old\ndef b():\n    pass\n@@ new\n",
			`hunk 1 (patch line 2) leaves 5 empty lines in a row where "def b():" was in f: its new text is empty, its old text stops before the line break, and the empty lines above and below its old text now touch; if that was not meant, leave 3 blank lines under its old text, so its line break and 2 empty lines below go with it`,
			"@@ file f\n@@ old\ndef b():\n    pass\n\n\n\n@@ new\n",
			"def a():\n    pass\n\n\ndef c():\n    pass\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f": c.file})
			code, out, errOut := runCLI(t, root, []string{"--dry-run"}, c.patch)
			if code != exitOK {
				t.Fatalf("dry run: exit %d: %s%s", code, out, errOut)
			}
			if got := slipWarningsIn(errOut); len(got) != 1 || got[0] != c.warn {
				t.Errorf("warnings\n got %q\nwant [%q]", got, c.warn)
			}
			code, out, errOut = runCLI(t, root, nil, c.remedy)
			if code != exitOK || errOut != "" {
				t.Fatalf("the remedy: exit %d, stderr %q: %s", code, errOut, out)
			}
			if got := readFile(t, root, "f"); got != c.want {
				t.Errorf("after the remedy f = %q, want %q", got, c.want)
			}
		})
	}
}

// The goldens for #169, on the issue's file and on Python's with a new of
// line breaks: applied, a dry run, whose remedies are the patch's, and
// --json, whose kind is #111's empty_line.
func TestJoinedEmptyLinesGoldens(t *testing.T) {
	files := map[string]string{"x.zig": threeFns, "p.py": threeDefs}
	patch := "@@ file x.zig\n@@ old\nfn b() {}\n\n@@ new\n" +
		"@@ file p.py\n@@ old\ndef b():\n    pass\n\n@@ new\n\n\n"
	t.Run("text", func(t *testing.T) {
		_, out, errOut := runCLI(t, cliTree(t, files), nil, patch)
		golden(t, "cli-slip-joined-empty", out+errOut)
	})
	t.Run("dry run", func(t *testing.T) {
		_, out, errOut := runCLI(t, cliTree(t, files), []string{"--dry-run"}, patch)
		golden(t, "cli-slip-joined-empty-dry-run", out+errOut)
	})
	t.Run("json", func(t *testing.T) {
		_, out, _ := runCLI(t, cliTree(t, files), []string{"--json"}, patch)
		golden(t, "cli-slip-joined-empty-json", out)
	})
}

// The helpers on their own, for the edges the rows reach only in passing: a
// file's first line, a lone carriage return, a last line with no newline, and
// an old with no text in it.
func TestJoinedEmptyLinesHelpers(t *testing.T) {
	for _, c := range []struct {
		b         string
		at, above int
	}{
		{"\n\nx", 2, 2},
		{"a\n\n\nx", 4, 2},
		{"a\n\r\nx", 4, 1},
		{"a\n\r\r\nx", 5, 0},
		{"x", 0, 0},
	} {
		if got := emptyAbove([]byte(c.b), c.at); got != c.above {
			t.Errorf("emptyAbove(%q, %d) = %d, want %d", c.b, c.at, got, c.above)
		}
	}
	for _, c := range []struct {
		b         string
		at, below int
	}{
		{"\n\r\nx\n", 0, 2},
		{"\n", 0, 1},
		{"", 0, 0},
		{"\r", 0, 0},
		{" \n", 0, 0},
	} {
		if got := emptyBelow([]byte(c.b), c.at); got != c.below {
			t.Errorf("emptyBelow(%q, %d) = %d, want %d", c.b, c.at, got, c.below)
		}
	}
	for _, c := range []struct {
		b    string
		want int
	}{
		{"\n\r\nx\r\ny\n", 3},
		{"\n\r\n", -1},
		{"  \n", 0},
		{"\n\r\r", 1},
	} {
		if got := firstTextLine([]byte(c.b)); got != c.want {
			t.Errorf("firstTextLine(%q) = %d, want %d", c.b, got, c.want)
		}
	}
	cur := []byte("a\n\n\r\n\nc\n")
	if _, ok := joinsEmptyLines(cur, []byte("a\n\n\nc\n"), 3, 5, 3, nil); ok {
		t.Error("an old of empty lines joined two runs")
	}
}
