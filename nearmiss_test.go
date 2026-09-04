package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §8.1: "The exact bytes of every failure message, because those messages are
// the product. A reworded diagnostic is a contract change." -update exists to
// make a deliberate change easy to apply, never to make a surprising diff go
// away: read the diff before you run it.
var update = flag.Bool("update", false, "rewrite the golden reports in testdata/")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".txt")
	if *update {
		must(t, os.MkdirAll("testdata", 0o755))
		must(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test -update to create it, then read it)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s changed. That is a contract change, not a diff to regenerate.\n--- want ---\n%s\n--- got ---\n%s",
			path, want, got)
	}
}

// diagnose runs the real path: a patch that fails against a real tree, so the
// goldens cover what an agent would actually see rather than a hand-built
// Diagnosis.
func diagnose(t *testing.T, fileBody, oldText string, opt Options) *Diagnosis {
	t.Helper()
	tree, _ := fixture(t, map[string]string{"f.txt": fileBody})
	patch := "@@ file f.txt\n@@ old\n" + oldText + "\n@@ new\nREPLACEMENT\n"
	_, err := run(t, tree, patch, opt)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want a validation failure, got %v", err)
	}
	if ve.Failures[0].Near == nil {
		t.Fatal("no diagnosis")
	}
	return ve.Failures[0].Near
}

func TestDiagnoseGoldens(t *testing.T) {
	cases := []struct {
		name string
		file string
		old  string
		opt  Options
	}{
		{
			// §7's own first example: a tab that is spaces.
			name: "indentation-tab-vs-spaces",
			file: "func f() {\n    return nil\n}\n",
			old:  "func f() {\n\treturn nil\n}",
		},
		{
			name: "trailing-whitespace",
			file: "alpha   \nbeta\n",
			old:  "alpha\nbeta",
		},
		{
			// A curly quote copied out of rendered Markdown, §7's third example.
			name: "curly-quote",
			file: "msg := “hello”\n",
			old:  "msg := \"hello\"",
		},
		{
			name: "no-break-space",
			file: "a\u00a0b\n",
			old:  "a b",
		},
		{
			name: "en-dash",
			file: "range 1–10\n",
			old:  "range 1-10",
		},
		{
			// The floor (§7.1 step 4): nothing explains it, so the closest span
			// and the first differing line are printed anyway.
			name: "typo-no-normalization-explains-it",
			file: "func (s *Scope) Project() (string, error) {\n\tif s == nil {\n\t\treturn s.project, nil\n\t}\n",
			old:  "func (s *Scope) Project() (string, error) {\n\tif s == nil {\n\t\treturn s.projet, nil\n\t}",
		},
		{
			name: "no-anchor",
			file: "alpha\nbeta\n",
			old:  "nothing like this is here",
		},
		{
			// --eol auto translates this away, so reaching it means strict.
			name: "line-endings-under-strict",
			file: "alpha\r\nbeta\r\n",
			old:  "alpha\nbeta",
			opt:  Options{EOL: EOLStrict},
		},
		{
			// Under auto the payload was converted to the file's majority
			// ending, so an LF region in a mostly-CRLF file cannot match. Same
			// normalization as the row above, different thing to say about it.
			name: "mixed-line-endings",
			file: "a\r\nb\r\nc\r\ndelta\nepsilon\n",
			old:  "delta\nepsilon",
		},
		{
			name: "whitespace-collapsed",
			file: "a\t \tb\n",
			old:  "a b",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := diagnose(t, c.file, c.old, c.opt)
			golden(t, c.name, d.Render("  "))
		})
	}

	t.Run("too-many-matches", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"f.txt": "P\nq\nP\nr\nP\n"})
		_, err := run(t, tree, "@@ file f.txt\n@@ old x2\nP\n@@ new\nZ\n", Options{})
		ve := err.(*ValidationError)
		golden(t, "too-many-matches", ve.Failures[0].Near.Render("  "))
	})

	t.Run("too-many-matches-capped", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"f.txt": strings.Repeat("P\n", 14)})
		_, err := run(t, tree, "@@ file f.txt\n@@ old\nP\n@@ new\nZ\n", Options{})
		ve := err.(*ValidationError)
		golden(t, "too-many-matches-capped", ve.Failures[0].Near.Render("  "))
	})

	// Nothing was written, so an agent that opens the file sees different line
	// numbers than the ones searched. The report has to say so.
	t.Run("line-numbers-shifted-by-an-earlier-hunk", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"f.txt": "one\ntwo\n    three\n"})
		_, err := run(t, tree, ""+
			"@@ file f.txt\n@@ old\none\n@@ new\nONE\nEXTRA\n"+
			"@@ old\n\tthree\n@@ new\nTHREE\n", Options{})
		ve := err.(*ValidationError)
		golden(t, "shifted-by-earlier-hunk", ve.Failures[0].Near.Render("  "))
	})
}

// The card's done-when, and the product claim in one test: §7 exists so that a
// failure costs zero extra round trips, which is only true if the bytes it
// prints can be pasted straight back into the patch and applied.
func TestThePrintedSpanCanBePastedBack(t *testing.T) {
	cases := []struct{ name, file, old string }{
		{"a tab where the file has spaces", "func f() {\n    return nil\n}\n", "func f() {\n\treturn nil\n}"},
		{"a trailing space", "alpha   \nbeta\n", "alpha\nbeta"},
		{"a curly quote out of rendered markdown", "msg := “hello”\n", "msg := \"hello\""},
		// Escaped, not literal: a no-break space typed into a source file is invisible,
		// and the first draft of this case had a plain space on both sides and passed
		// by matching itself.
		{"a no-break space", "a\u00a0b\n", "a b"},
		{"an em dash", "a — b\n", "a - b"},
		// §5.2's own fallback example: a multi-line old whose first line
		// anchors and whose third line has the typo.
		{"a typo, where no normalization explains it",
			"func f() {\n\tif s == nil {\n\t\treturn s.project, nil\n\t}\n",
			"func f() {\n\tif s == nil {\n\t\treturn s.projet, nil\n\t}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := diagnose(t, c.file, c.old, Options{})
			if len(d.Span) == 0 {
				t.Fatalf("no span printed; there is nothing to paste:\n%s", d.Render("  "))
			}
			pasted := string(bytesJoinLines(d.Span))

			// A fresh tree, and the printed span used verbatim as the new old.
			tree, root := fixture(t, map[string]string{"f.txt": c.file})
			patch := "@@ file f.txt\n@@ old\n" + pasted + "\n@@ new\nAPPLIED\n"
			if _, err := run(t, tree, patch, Options{}); err != nil {
				t.Fatalf("the span the report printed does not apply:\n span %q\n err  %v\n report:\n%s",
					pasted, err, d.Render("  "))
			}
			if got := readFile(t, root, "f.txt"); !strings.Contains(got, "APPLIED") {
				t.Errorf("file = %q", got)
			}
		})
	}
}

func bytesJoinLines(lines [][]byte) []byte {
	var out []byte
	for i, l := range lines {
		if i > 0 {
			out = append(out, '\n')
		}
		out = append(out, l...)
	}
	return out
}

// §7.2: capped at --context lines per hunk and 10 hunks per batch. "A
// diagnostic that blows the context window is a worse failure than no
// diagnostic."
func TestCostBounds(t *testing.T) {
	t.Run("the span is capped at --context", func(t *testing.T) {
		var file, old strings.Builder
		for i := 0; i < 30; i++ {
			file.WriteString("    line\n")
			old.WriteString("\tline\n")
		}
		tree, _ := fixture(t, map[string]string{"f.txt": file.String()})
		patch := "@@ file f.txt\n@@ old\n" + old.String() + "\n@@ new\nX\n"
		_, err := run(t, tree, patch, Options{Context: 5})
		ve, ok := err.(*ValidationError)
		if !ok {
			t.Fatalf("got %v", err)
		}
		if n := len(ve.Failures[0].Near.Span); n != 5 {
			t.Errorf("span is %d lines, want the --context cap of 5", n)
		}
	})

	t.Run("the default context is 20", func(t *testing.T) {
		if (Options{}).context() != DefaultContext || DefaultContext != 20 {
			t.Errorf("default context = %d, want 20 (§4, §11)", (Options{}).context())
		}
	})

	// The cap is per batch and the cascade is per file, so this needs one
	// failing hunk in each of many files. A first draft put fourteen failing
	// hunks in one file, and thirteen of them were skipped rather than
	// diagnosed: the test could not have detected the cap being removed, and a
	// mutation check is what said so.
	t.Run("at most ten hunks are diagnosed, and the rest are still reported", func(t *testing.T) {
		const n = 14
		files := map[string]string{}
		var patch strings.Builder
		for i := 0; i < n; i++ {
			name := "f" + itoa(i) + ".txt"
			files[name] = "    line\n"
			patch.WriteString("@@ file " + name + "\n@@ old\n\tline\n@@ new\nX\n")
		}
		tree, _ := fixture(t, files)
		_, err := run(t, tree, patch.String(), Options{})
		ve, ok := err.(*ValidationError)
		if !ok {
			t.Fatalf("got %v", err)
		}
		// Every failure is reported; §5.2 requires that. Only the near-miss
		// block is dropped past the cap.
		if len(ve.Failures) != n {
			t.Fatalf("got %d failures, want %d", len(ve.Failures), n)
		}
		diagnosed := 0
		for _, f := range ve.Failures {
			if f.Near != nil {
				diagnosed++
			}
		}
		if diagnosed != diagnoseLimit {
			t.Errorf("%d hunks diagnosed, want exactly the cap of %d", diagnosed, diagnoseLimit)
		}
	})

	t.Run("a too-many list is capped at ten with a count", func(t *testing.T) {
		d := DiagnoseTooMany([]byte("P"), []byte(strings.Repeat("P\n", 14)))
		if len(d.Lines) != tooManyLimit || d.MoreLines != 4 || d.Found != 14 {
			t.Errorf("lines=%d more=%d found=%d", len(d.Lines), d.MoreLines, d.Found)
		}
	})
}

// §7.1 step 1 anchors on the first non-blank line of old, which may not be
// old's first line. The span has to start where old would start, not at the
// matched line, or it compares the wrong text.
func TestAnchorAlignment(t *testing.T) {
	// gamma sits on file line 4, and old's first non-blank line is its third.
	// The span therefore has to begin at file line 2, not at line 4. Starting
	// at the match, as §7.1 step 2 literally says, compares old's blank lines
	// against the wrong file lines and the cause is never found.
	file := "alpha\n\n\n    gamma\ndelta\n"
	d := diagnose(t, file, "\n\n\tgamma\ndelta", Options{})
	if d.Kind != DiagNamed || d.Cause != "indentation" {
		t.Fatalf("kind=%v cause=%q\n%s", d.Kind, d.Cause, d.Render("  "))
	}
	if d.Line != 2 {
		t.Errorf("span starts at line %d, want 2 (two lines above the anchor at 4)", d.Line)
	}
}

// A tie needs a rule or a golden has no deterministic answer. Earliest wins.
func TestClosestSpanTieGoesToTheEarliest(t *testing.T) {
	file := "aaa\nxxx\nbbb\naaa\nyyy\nbbb\n"
	d := diagnose(t, file, "aaa\nzzz\nbbb", Options{})
	if d.Kind != DiagClosest {
		t.Fatalf("kind = %v\n%s", d.Kind, d.Render("  "))
	}
	if d.Line != 1 {
		t.Errorf("span starts at line %d, want the earliest of the tied candidates, 1", d.Line)
	}
}

func TestNormalizations(t *testing.T) {
	for _, c := range []struct {
		name  string
		fn    func([]byte) []byte
		in    string
		equal string
	}{
		{"stripTrailing leaves a carriage return alone", stripTrailing, "a  \nb\t", "a\nb"},
		{"stripCR removes a trailing one per line", stripCR, "a\r\nb\r", "a\nb"},
		{"flattenIndent touches leading whitespace only", flattenIndent, "\t\ta b\t c", " a b\t c"},
		{"collapseSpace flattens every run", collapseSpace, "a\t \t\nb", "a b"},
		// Written with escapes on purpose: a literal no-break space in a test is
		// a character nobody can count, which is the whole reason this
		// normalization exists.
		{"foldLookalikes folds the whole table", foldLookalikes,
			"\u00a0\u2018\u2019\u201c\u201d\u2013\u2014\u2212\u2007\u202f",
			" ''\"\"---  "},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(c.fn([]byte(c.in))); got != c.equal {
				t.Errorf("got %q, want %q", got, c.equal)
			}
		})
	}
}

func TestDiagnoseDegenerateInputs(t *testing.T) {
	for _, c := range []struct {
		name string
		old  string
		file string
		kind DiagKind
	}{
		{"an empty old", "", "abc\n", DiagNoAnchor},
		{"an all-blank old", "\n\n", "abc\n", DiagNoAnchor},
		{"an empty file", "abc", "", DiagNoAnchor},
		{"nothing in common", "zzz", "abc\n", DiagNoAnchor},
		{"a span running past the end of the file", "abc\ndef\nghi", "abc\n", DiagClosest},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := Diagnose([]byte(c.old), []byte(c.file), DefaultContext)
			if d.Kind != c.kind {
				t.Errorf("kind = %v, want %v\n%s", d.Kind, c.kind, d.Render("  "))
			}
			if s := d.Render("  "); s == "" {
				t.Error("rendered nothing at all")
			}
		})
	}
}

// The diagnosis runs on agent-written text against arbitrary file contents, and
// it runs on the failure path, where a panic would replace a useful report with
// a crash.
func FuzzDiagnose(f *testing.F) {
	f.Add("func f() {\n\treturn nil\n}", "func f() {\n    return nil\n}\n")
	f.Add("", "")
	f.Add("\n\n\n", "a\r\nb\r\n")
	f.Add("a b", "a b\n")
	f.Add("aaa", "aaaaaa")

	f.Fuzz(func(t *testing.T, old, file string) {
		d := Diagnose([]byte(old), []byte(file), 20)
		if d == nil {
			t.Fatal("nil diagnosis")
		}
		if s := d.Render("  "); s == "" {
			t.Fatalf("empty render for kind %v", d.Kind)
		}
		if len(d.Span) > 20 {
			t.Errorf("span of %d lines exceeds the context cap", len(d.Span))
		}
		// A reported span must actually be in the file, or the report is
		// inventing bytes for the agent to paste.
		if d.Line > 0 {
			lines := splitLines([]byte(file))
			if d.Line-1+len(d.Span) > len(lines) {
				t.Fatalf("span at line %d runs %d lines past a %d-line file", d.Line, len(d.Span), len(lines))
			}
			for i, l := range d.Span {
				if string(lines[d.Line-1+i]) != string(l) {
					t.Fatalf("span line %d is %q, but the file has %q", i, l, lines[d.Line-1+i])
				}
			}
		}
		tm := DiagnoseTooMany([]byte(old), []byte(file))
		if len(tm.Lines) > tooManyLimit {
			t.Errorf("%d listed lines exceeds the cap", len(tm.Lines))
		}
	})
}

// The detail sentences are half of what an agent reads, so they get direct
// tests rather than only whatever the goldens happen to reach.
func TestDetailSentences(t *testing.T) {
	t.Run("describeIndent", func(t *testing.T) {
		for in, want := range map[string]string{
			"\tx":     "a tab",
			"\t\tx":   "2 tabs",
			"    x":   "4 spaces",
			" x":      "a space",
			"\t  x":   "a tab and 2 spaces",
			"x":       "no indentation",
			"\t\t\tx": "3 tabs",
		} {
			if got := describeIndent([]byte(in)); got != want {
				t.Errorf("describeIndent(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("describeTrailing", func(t *testing.T) {
		for in, want := range map[string]string{
			"x":       "has none",
			"x ":      "ends in a space",
			"x   ":    "ends in 3 spaces",
			"x\t":     "ends in a tab",
			"x\t\t":   "ends in 2 tabs",
			"x \t":    "ends in a tab and a space",
			"x  \t\t": "ends in 2 tabs and 2 spaces",
		} {
			if got := describeTrailing([]byte(in)); got != want {
				t.Errorf("describeTrailing(%q) = %q, want %q", in, got, want)
			}
		}
	})

	// Every entry in the lookalike table has a name, or the report says
	// "a lookalike character" about something it could have named.
	t.Run("every lookalike is named", func(t *testing.T) {
		for r := range lookalikes {
			if n := runeName(r); n == "a lookalike character" {
				t.Errorf("U+%04X has no name", r)
			}
		}
		if runeName('q') != "a lookalike character" {
			t.Errorf("an unlisted rune should fall back")
		}
	})

	t.Run("plural picks its article", func(t *testing.T) {
		for _, c := range []struct {
			n          int
			word, want string
		}{
			{1, "tab", "a tab"},
			{2, "tab", "2 tabs"},
			{1, "occurrence", "an occurrence"},
			{0, "space", "0 spaces"},
		} {
			if got := plural(c.n, c.word); got != c.want {
				t.Errorf("plural(%d, %q) = %q, want %q", c.n, c.word, got, c.want)
			}
		}
	})

	// --eol auto translates CRLF away, so an LF file reaching this row means
	// the caller is on strict with an LF file and a CRLF payload.
	t.Run("detailEOL names which side is which", func(t *testing.T) {
		for in, want := range map[string]string{
			"a\r\nb\r\n":       "the file is CRLF",
			"a\nb\n":           "the file is LF",
			"a\r\nb\nc\r\nd\n": "mixed line endings",
		} {
			if got := detailEOL(nil, nil, []byte(in)); !strings.Contains(got, want) {
				t.Errorf("detailEOL(%q) = %q, want it to mention %q", in, got, want)
			}
		}
	})
}

func TestRenderingHelpers(t *testing.T) {
	t.Run("visible marks tabs everywhere and trailing space only", func(t *testing.T) {
		for in, want := range map[string]string{
			"a\tb":   "a→b",
			"a\tb  ": "a→b··",
			"a b":    "a b",
			"\ta\t":  "→a→",
			"":       "",
			"   ":    "···",
		} {
			if got := visible([]byte(in)); got != want {
				t.Errorf("visible(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("differsOnlyInWhitespace", func(t *testing.T) {
		for _, c := range []struct {
			a, b string
			want bool
		}{
			{"a b", "a b", false}, // identical is not a difference
			{"a b", "a\tb", true}, // whitespace only
			{"a  b", "a b", true}, // run length only
			{"a b", "a c", false}, // a real difference
			{"\ta", "a", true},    // leading
		} {
			if got := differsOnlyInWhitespace([]byte(c.a), []byte(c.b)); got != c.want {
				t.Errorf("differsOnlyInWhitespace(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
			}
		}
	})

	t.Run("differingLines counts a length mismatch too", func(t *testing.T) {
		got := differingLines([][]byte{[]byte("a"), []byte("b"), []byte("c")}, [][]byte{[]byte("a")})
		if got != 4 {
			t.Errorf("got %d, want 4 (two missing lines, counted on both sides)", got)
		}
	})

	t.Run("firstDifference reports nothing when the lines are equal", func(t *testing.T) {
		l, _, _, c := firstDifference([][]byte{[]byte("a")}, [][]byte{[]byte("a")})
		if l != 0 || c != 0 {
			t.Errorf("line=%d col=%d, want 0/0", l, c)
		}
	})

	// explain returns nil for a span that already equals old: the miss is
	// somewhere else and naming a cause here would be inventing one.
	t.Run("explain declines an exact match", func(t *testing.T) {
		same := [][]byte{[]byte("a")}
		if d := explain(same, same, []byte("a\n")); d != nil {
			t.Errorf("got %+v, want nil", d)
		}
		if d := explain(same, [][]byte{[]byte("a"), []byte("b")}, nil); d != nil {
			t.Errorf("a length mismatch should not be explained: %+v", d)
		}
	})
}

// §7.1 step 1's second try: when the first non-blank line matches nothing, the
// longest line is tried instead.
func TestAnchorFallsBackToTheLongestLine(t *testing.T) {
	file := "zzz\nthe long distinctive line that anchors\n    tail\n"
	old := "qqq\nthe long distinctive line that anchors\n\ttail"
	d := diagnose(t, file, old, Options{})
	if d.Kind == DiagNoAnchor {
		t.Fatalf("gave up instead of trying the longest line:\n%s", d.Render("  "))
	}
	if d.Line != 1 {
		t.Errorf("span starts at line %d, want 1", d.Line)
	}
}

// A candidate so near the top of the file that the span would start above line
// one is skipped rather than clamped, because a clamped span compares the wrong
// lines and would report a cause that is not the real one.
func TestCandidateTooCloseToTheTopIsSkipped(t *testing.T) {
	file := "    gamma\ndelta\nfiller\n\n\n    gamma\ndelta\n"
	d := diagnose(t, file, "\n\n\tgamma\ndelta", Options{})
	if d.Kind != DiagNamed {
		t.Fatalf("kind = %v\n%s", d.Kind, d.Render("  "))
	}
	if d.Line != 4 {
		t.Errorf("span starts at line %d, want 4: the first candidate is too near the top", d.Line)
	}
}

// Every candidate too near the top for its span to start above line one, and
// nothing to fall back to. The report has to say the text is not there rather
// than clamp the span and name a cause it did not find.
func TestEveryCandidateTooCloseToTheTop(t *testing.T) {
	d := Diagnose([]byte("\n\ngamma\ndelta"), []byte("gamma\nzzz\n"), DefaultContext)
	if d.Kind != DiagNoAnchor {
		t.Errorf("kind = %v, want DiagNoAnchor\n%s", d.Kind, d.Render("  "))
	}
}

func TestShiftedWordingForSeveralHunks(t *testing.T) {
	tree, _ := fixture(t, map[string]string{"f.txt": "one\ntwo\n    three\n"})
	_, err := run(t, tree, ""+
		"@@ file f.txt\n@@ old\none\n@@ new\nONE\n"+
		"@@ old\ntwo\n@@ new\nTWO\nEXTRA\n"+
		"@@ old\n\tthree\n@@ new\nTHREE\n", Options{})
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("got %v", err)
	}
	got := ve.Failures[0].Near.Render("  ")
	if !strings.Contains(got, "the 2 hunks before this one") {
		t.Errorf("wanted the plural form:\n%s", got)
	}
}

// The detail sentence names which line differs, so it has to skip the lines
// that match. A report that always says "line 1" is wrong whenever the anchor
// line is not the one that differs, which is the common shape.
func TestDetailNamesTheLineThatDiffers(t *testing.T) {
	lines := func(ss ...string) [][]byte {
		out := make([][]byte, len(ss))
		for i, s := range ss {
			out[i] = []byte(s)
		}
		return out
	}

	t.Run("trailing whitespace on a later line", func(t *testing.T) {
		got := detailTrailing(
			lines("same", "same", "third"),
			lines("same", "same", "third   "), nil)
		if !strings.Contains(got, "on line 3") || !strings.Contains(got, "3 spaces") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("indentation on a later line", func(t *testing.T) {
		got := detailIndent(
			lines("same", "\tthird"),
			lines("same", "    third"), nil)
		if !strings.Contains(got, "a tab") || !strings.Contains(got, "4 spaces") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a lookalike on a later line", func(t *testing.T) {
		got := detailUnicode(
			lines("same", "a-b"),
			lines("same", "a–b"), nil)
		if !strings.Contains(got, "en dash") {
			t.Errorf("got %q", got)
		}
	})

	// detailUnicode is reached only when the lookalike row explained the miss,
	// but it declines rather than guesses if the first divergence is not one.
	t.Run("a divergence that is not a lookalike gets no sentence", func(t *testing.T) {
		if got := detailUnicode(lines("abc"), lines("axc"), nil); got != "" {
			t.Errorf("got %q, want no sentence", got)
		}
	})
}
