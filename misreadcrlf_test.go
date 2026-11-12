package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// misreadDetail is the remedy a misread CRLF is refused with.
const misreadDetail = "old has a carriage return before a line break, and in this file a line break is a newline, " +
	"so under --eol auto the two are the bytes of a CRLF line ending, and match one as if it were text; " +
	"pass --eol strict to match old's bytes as they are"

// Under --eol auto, in a file whose line ending is LF, an old with a carriage
// return before a line break may not match (#170): converted, the two are a
// CRLF's bytes, and what they match is a CRLF line ending, which old does not
// say. Each row is one hunk against one file, built as Hunk values so that
// every CR is where the row puts it; a refused row asserts the failure, the
// lines, the cause, the report's header and that nothing was written, and an
// applied row asserts the file.
func TestAnOldThatMisreadsACRLFIsRefused(t *testing.T) {
	twelve := strings.Repeat("a\r\n", 12) + strings.Repeat("b\n", 13)
	for _, c := range []struct {
		name          string
		file          string
		old, new      string
		count         int
		strict        bool
		want          string // the file after, for a row that applies
		lines         []int  // the refusal's lines; nil for a row that applies
		more          int
		header        string
		shiftedByHunk bool // a first hunk changes the file before this one
	}{
		{
			name: "the shape the fuzz found: carriage returns before a line break, matched one late",
			file: "p\r\r\r\nq\nr\n", old: "\r\r\r\n", new: "X\n", count: 1,
			lines:  []int{1},
			header: "expected 1 occurrence, found 1, and old matched a CRLF line ending there as a carriage return and a newline",
		},
		{
			name: "no carriage return before the copy: old's meets a CRLF of the file's",
			file: "x\r\ny\nz\n", old: "\r\r\n", new: "\n", count: 1,
			lines: []int{1},
		},
		{
			name: "a CRLF line old writes as a carriage return and a line break, which applied until #170",
			file: "a\r\nb\nc\nd\n", old: "a\r\r\nb", new: "A\r\r\nB", count: 1,
			lines: []int{1},
		},
		{
			name: "the same with another in new, which was refused as a join until #170",
			file: "a\r\nb\nc\nd\n", old: "a\r\r\nb", new: "A\r\r\nB\r\r\nC", count: 1,
			lines: []int{1},
		},
		{
			name: "a copy that spans lines, named at its first CRLF",
			file: "a\nb\r\nc\r\nd\ne\nf\ng\n", old: "a\nb\r\r\nc\r\r\n", new: "x\n", count: 1,
			lines: []int{2},
		},
		{
			name: "every copy",
			file: "x\r\ny\r\nz\nw\nv\n", old: "\r\r\n", new: "\n", count: 2,
			lines:  []int{1, 2},
			header: "expected 2 occurrences, found 2, and old matched a CRLF line ending in each as a carriage return and a newline",
		},
		{
			name: "more copies than the report lists",
			file: twelve, old: "a\r\r\n", new: "A\n", count: 12,
			lines: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, more: 2,
		},
		{
			name: "the lines of a file an earlier hunk changed",
			file: "x\r\nb\nc\n", old: "\r\r\n", new: "\n", count: 1,
			lines: []int{1}, shiftedByHunk: true,
		},
		{
			name: "a CRLF file, where old's carriage return before a line break reads as the file does",
			file: "p\r\r\nq\r\nr\r\n", old: "\r\r\n", new: "X\n", count: 1,
			want: "pX\r\nq\r\nr\r\n",
		},
		{
			name: "a carriage return in old that is not before a line break",
			file: "a\rb\nc\nd\n", old: "a\rb", new: "A\rB", count: 1,
			want: "A\rB\nc\nd\n",
		},
		{
			name: "strict matches old's bytes as they are",
			file: "p\r\r\r\nq\nr\n", old: "\r\r\r\n", new: "X\n", count: 1, strict: true,
			want: "pX\nq\nr\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"f.txt": c.file})
			before := snapshot(t, root)
			opt := Options{}
			if c.strict {
				opt.EOL = EOLStrict
			}
			var hunks []Hunk
			if c.shiftedByHunk {
				// Changes a line without changing its ending or the copies.
				hunks = append(hunks, Hunk{Op: OpReplace, Path: "f.txt", Line: 1, Count: 1, Old: []byte("b\n"), New: []byte("B\n")})
			}
			n := len(hunks) + 1
			hunks = append(hunks, Hunk{
				Op: OpReplace, Path: "f.txt", Line: 3, Count: c.count,
				Old: []byte(c.old), New: []byte(c.new),
			})
			_, err := NewTxn(tree, opt).Run(&Patch{Hunks: hunks})
			if c.lines == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got := readFile(t, root, "f.txt"); got != c.want {
					t.Errorf("f.txt = %q, want %q", got, c.want)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			assertUnchanged(t, root, before)
			if len(ve.Failures) != 1 {
				t.Fatalf("failures = %+v, want one", ve.Failures)
			}
			f := ve.Failures[0]
			if f.Hunk != n || f.Expected != c.count || f.Found != c.count || f.Refusal != "" {
				t.Errorf("failure = hunk %d, expected %d, found %d, refusal %q; want hunk %d, %d and %d, no refusal",
					f.Hunk, f.Expected, f.Found, f.Refusal, n, c.count, c.count)
			}
			d := f.Near
			if d == nil || d.Kind != DiagMisreadCRLF {
				t.Fatalf("near = %+v, want a misread line ending", d)
			}
			if d.Cause != "misread line ending" || d.Detail != misreadDetail {
				t.Errorf("cause %q, detail\n%q\nwant %q,\n%q", d.Cause, d.Detail, "misread line ending", misreadDetail)
			}
			if !slices.Equal(d.Lines, c.lines) || d.Found != c.count || d.MoreLines != c.more {
				t.Errorf("lines %v, found %d, more %d; want %v, %d, %d", d.Lines, d.Found, d.MoreLines, c.lines, c.count, c.more)
			}
			if want := len(hunks) - 1; d.Shifted != want {
				t.Errorf("shifted = %d, want %d", d.Shifted, want)
			}
			var out bytes.Buffer
			writeFailureBlocks(&out, ve.Failures)
			if c.header != "" && !strings.Contains(out.String(), "\n  "+c.header+"\n") {
				t.Errorf("report does not say %q:\n%s", c.header, out.String())
			}
			if !strings.Contains(out.String(), "\n\n  "+misreadDetail+"\n") {
				t.Errorf("report does not end on the detail:\n%s", out.String())
			}
			if note := "line numbers count against the file as the hunk before this one would have left it"; strings.Contains(out.String(), note) != c.shiftedByHunk {
				t.Errorf("shifted note printed = %v, want %v:\n%s", !c.shiftedByHunk, c.shiftedByHunk, out.String())
			}
		})
	}
}

// A misread is asked only of a count that holds. An old that matches nowhere
// gets the near miss's report, and one that matches more often than claimed
// the count's: neither copy is edited, so neither is misread.
func TestAMisreadIsAskedOnlyOfACountThatHolds(t *testing.T) {
	for _, c := range []struct {
		name, file, old string
		found           int
		kind            DiagKind
	}{
		{"found nowhere, which the near miss puts down to whitespace", "p\r\r\r\nq\nr\n", "p\r\r\r\n", 0, DiagNamed},
		{"found more often than claimed", "x\r\ny\r\nz\nw\nv\n", "\r\r\n", 2, DiagTooMany},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"f.txt": c.file})
			before := snapshot(t, root)
			_, err := NewTxn(tree, Options{}).Run(&Patch{Hunks: []Hunk{
				{Op: OpReplace, Path: "f.txt", Line: 1, Count: 1, Old: []byte(c.old), New: []byte("X\n")},
			}})
			var ve *ValidationError
			if !errors.As(err, &ve) || len(ve.Failures) != 1 {
				t.Fatalf("err = %v, want one refusal", err)
			}
			if f := ve.Failures[0]; f.Found != c.found || f.Near == nil || f.Near.Kind != c.kind {
				t.Errorf("found %d, near %+v; want found %d and kind %v", f.Found, f.Near, c.found, c.kind)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// A copy old misreads is reported as the misread even when its replacement
// would also split a CRLF or join one: Validate asks DiagnoseMisreadCRLF
// first, since the copy is not the text the replacement would be judged on.
func TestAMisreadIsReportedBeforeASplitOrAJoin(t *testing.T) {
	for _, c := range []struct {
		name, file, old, new string
		other                func(file, old, repl []byte) *Diagnosis
	}{
		{
			"a split", "a\r\nX\r\nb\nc\nd\n", "\nX\r\r\n", "Y",
			func(file, old, repl []byte) *Diagnosis { return DiagnoseSplitCRLF(file, old, repl, 1) },
		},
		{
			"a join", "a\r\nb\nc\nd\n", "a\r\r\nb", "A\r\r\nB\r\r\nC",
			func(file, old, repl []byte) *Diagnosis { return DiagnoseJoinedCRLF(file, old, repl, 1, "\n") },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"f.txt": c.file})
			before := snapshot(t, root)
			_, err := NewTxn(tree, Options{}).Run(&Patch{Hunks: []Hunk{
				{Op: OpReplace, Path: "f.txt", Line: 1, Count: 1, Old: []byte(c.old), New: []byte(c.new)},
			}})
			var ve *ValidationError
			if !errors.As(err, &ve) || len(ve.Failures) != 1 {
				t.Fatalf("err = %v, want one refusal", err)
			}
			if d := ve.Failures[0].Near; d == nil || d.Kind != DiagMisreadCRLF {
				t.Errorf("near = %+v, want the misread", d)
			}
			if c.other([]byte(c.file), toEOL([]byte(c.old), "\n"), toEOL([]byte(c.new), "\n")) == nil {
				t.Errorf("the copy is not %s too, or this test proves nothing about the order", c.name)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The report names one line as a line and several as lines, and says how many
// it did not list, as it does for a split and a join.
func TestAMisreadCRLFRendersItsLines(t *testing.T) {
	for _, c := range []struct {
		lines []int
		more  int
		want  string
	}{
		{[]int{4}, 0, "  at line 4\n"},
		{[]int{1, 3}, 0, "  at lines 1, 3\n"},
		{[]int{1, 2}, 5, "  at lines 1, 2, and 5 more\n"},
	} {
		d := &Diagnosis{Kind: DiagMisreadCRLF, Lines: c.lines, MoreLines: c.more, Detail: "why"}
		if got := d.Render("  "); !strings.HasPrefix(got, c.want) || !strings.HasSuffix(got, "\n\n  why\n") {
			t.Errorf("Render(%v, %d more) = %q, want it to open %q and end on the detail", c.lines, c.more, got, c.want)
		}
	}
}

// DiagnoseMisreadCRLF on its own, for what Validate never hands it: a count
// beyond the copies there are, which looks at those there are, one short of
// them, which looks at the first n as bytes.Replace would edit them, and an
// old with no copy. And what it is handed and passes: a CRLF file, and an old
// with no carriage return before a newline.
func TestDiagnoseMisreadCRLFOnItsOwn(t *testing.T) {
	file := []byte("x\r\ny\r\nz\nw\nv\n")
	d := DiagnoseMisreadCRLF(file, []byte("\r\n"), 5, "\n")
	if d == nil || d.Found != 2 || !slices.Equal(d.Lines, []int{1, 2}) {
		t.Errorf("DiagnoseMisreadCRLF past the copies = %+v, want the two there are", d)
	}
	d = DiagnoseMisreadCRLF(file, []byte("\r\n"), 1, "\n")
	if d == nil || d.Found != 1 || !slices.Equal(d.Lines, []int{1}) {
		t.Errorf("DiagnoseMisreadCRLF short of the copies = %+v, want the first", d)
	}
	if d := DiagnoseMisreadCRLF(file, []byte("q\r\n"), 1, "\n"); d != nil {
		t.Errorf("DiagnoseMisreadCRLF with no copy = %+v, want nil", d)
	}
	if d := DiagnoseMisreadCRLF(file, []byte("\r\n"), 2, "\r\n"); d != nil {
		t.Errorf("DiagnoseMisreadCRLF in a CRLF file = %+v, want nil", d)
	}
	if d := DiagnoseMisreadCRLF(file, []byte("z\n"), 1, "\n"); d != nil {
		t.Errorf("DiagnoseMisreadCRLF with no CRLF in old = %+v, want nil", d)
	}
}

// §7.2 diagnoses ten failing hunks a batch and reports the rest bare. A
// misread is reported whole past the cap, as a split and a join are.
func TestAMisreadIsDiagnosedPastTheCap(t *testing.T) {
	files := map[string]string{"misread.txt": "x\r\ny\nz\n"}
	var hunks []Hunk
	for i := range diagnoseLimit {
		name := fmt.Sprintf("miss%02d.txt", i)
		files[name] = "here\n"
		hunks = append(hunks, Hunk{Op: OpReplace, Path: name, Line: i + 1, Count: 1, Old: []byte("not here"), New: []byte("x")})
	}
	hunks = append(hunks, Hunk{Op: OpReplace, Path: "misread.txt", Line: 99, Count: 1, Old: []byte("\r\r\n"), New: []byte("\n")})
	tree, root := fixture(t, files)
	before := snapshot(t, root)
	_, err := NewTxn(tree, Options{}).Run(&Patch{Hunks: hunks})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Failures) != diagnoseLimit+1 {
		t.Fatalf("err = %v, want %d failures", err, diagnoseLimit+1)
	}
	if last := ve.Failures[diagnoseLimit]; last.Near == nil || last.Near.Kind != DiagMisreadCRLF {
		t.Errorf("the misread past the cap = %+v, want its diagnosis", last)
	}
	assertUnchanged(t, root, before)
}

// The page an agent reads, in text and JSON, for the shape the fuzz found
// without an inverse: two carriage returns and a line break as old, in an LF
// file, which matched one carriage return late. --eol strict applies the same
// patch where old's bytes are.
func TestAMisreadCRLFIsGolden(t *testing.T) {
	const file, patch = "p\r\r\r\nq\nr\n", "@@ file f.txt\n@@ old\n\r\r\r\n\n@@ new\nX\n\n"
	root := cliTree(t, map[string]string{"f.txt": file})
	before := snapshot(t, root)
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch || out != "" {
		t.Fatalf("exit %d, stdout %q; want %d and the report on stderr: %s", code, out, exitNoMatch, errOut)
	}
	golden(t, "report-misread-crlf", errOut)

	code, js, errOut := runCLI(t, root, []string{"--json"}, patch)
	if code != exitNoMatch || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want %d and nothing on stderr", code, errOut, exitNoMatch)
	}
	var report struct {
		Failures []struct {
			Lines    []int
			NearMiss struct{ Cause, Detail string } `json:"near_miss"`
		}
	}
	must(t, json.Unmarshal([]byte(js), &report))
	if len(report.Failures) != 1 || report.Failures[0].NearMiss.Cause != "misread line ending" ||
		report.Failures[0].NearMiss.Detail != misreadDetail || !slices.Equal(report.Failures[0].Lines, []int{1}) {
		t.Errorf("JSON failures = %+v, want one at line 1 whose cause is a misread line ending", report.Failures)
	}
	golden(t, "report-misread-crlf-json", js)
	assertUnchanged(t, root, before)

	code, out, errOut = runCLI(t, root, []string{"--eol", "strict"}, patch)
	if code != exitOK {
		t.Fatalf("under --eol strict: exit %d: %s%s", code, out, errOut)
	}
	if got := readFile(t, root, "f.txt"); got != "pX\nq\nr\n" {
		t.Errorf("under --eol strict f.txt = %q, want old's bytes replaced where they are", got)
	}
}

// A patch whose every line ends in CRLF loses one carriage return per payload
// line first, as its terminator. One more before it is text, and is a misread
// in an LF file all the same.
func TestAMisreadInACRLFPatch(t *testing.T) {
	const file, patch = "a\nb\r\nc\nd\n", "@@ file f.txt\r\n@@ old\r\nb\r\r\r\n\r\n@@ new\r\nB\r\n\r\n"
	root := cliTree(t, map[string]string{"f.txt": file})
	before := snapshot(t, root)
	code, _, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch || !strings.Contains(errOut, "\n  expected 1 occurrence, found 1, and old matched a CRLF line ending there as a carriage return and a newline\n  at line 2\n") {
		t.Errorf("exit %d, want %d and the misread at line 2:\n%s", code, exitNoMatch, errOut)
	}
	assertUnchanged(t, root, before)
}

// The nightly's find for #170, through the CLI. The patch writes two carriage
// returns and a line break into a CRLF file, which makes its endings a tie
// that reads as LF, and until 2026-11-12 the patch, hunk invert of it and the
// inverse all exited 0, the inverse leaving "\r\n\rQ\nv\nw\nx\ny0Pa\n". Now the
// patch still applies, and its inverse is refused with nothing written.
func TestTheInverseThatMisreadIsRefused(t *testing.T) {
	const start = "a\r\nv\r\nw\r\nx\r\ny0P\nQ\n"
	const patch = "@@ file f.txt\n@@ old\na\n@@ new\n\nQ\n@@ old\nQ\r\nv\r\nw\r\nx\r\n\n@@ new\n\r\r\r\n\n"
	root := cliTree(t, map[string]string{"f.txt": start})
	if code, out, errOut := runCLI(t, root, nil, patch); code != exitOK {
		t.Fatalf("the patch: exit %d: %s%s", code, out, errOut)
	}
	after := readFile(t, root, "f.txt")
	if after != "\r\n\r\r\r\ny0P\nQ\n" {
		t.Fatalf("after the patch f.txt = %q", after)
	}
	var inv, invErr bytes.Buffer
	if code := cli([]string{"invert"}, strings.NewReader(patch), &inv, &invErr); code != exitOK {
		t.Fatalf("hunk invert: exit %d: %s", code, invErr.String())
	}
	code, out, errOut := runCLI(t, root, nil, inv.String())
	if code != exitNoMatch {
		t.Fatalf("the inverse: exit %d, want %d: %s%s", code, exitNoMatch, out, errOut)
	}
	if !strings.Contains(errOut, "hunk 1  f.txt  (patch line 2)\n  expected 1 occurrence, found 1, and old matched a CRLF line ending there as a carriage return and a newline\n  at line 2\n") {
		t.Errorf("the refusal does not name hunk 1's misread:\n%s", errOut)
	}
	if got := readFile(t, root, "f.txt"); got != after {
		t.Errorf("f.txt = %q, want it as the patch left it, %q", got, after)
	}
}
