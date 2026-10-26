package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// Under --eol auto a replacement may not part a CRLF's carriage return from its
// newline (#122). Each row is one hunk against one file, built as Hunk values
// so that every CR is where the row puts it; a refused row asserts the failure,
// the lines, the cause, the report's header and that nothing was written, and
// an applied row asserts the file.
func TestAReplacementThatSplitsACRLFIsRefused(t *testing.T) {
	const (
		begins = "old begins on the newline of a CRLF and new does not begin with one, " +
			"which would leave its carriage return on its own; start old after the newline, " +
			"or pass --eol strict to edit the bytes as they are"
		ends = "old ends on the carriage return of a CRLF and new does not end with one, " +
			"which would part it from its newline; end old before the carriage return, " +
			"or pass --eol strict to edit the bytes as they are"
		both = "old begins on the newline of one CRLF and ends on the carriage return of another, " +
			"and new neither begins with a newline nor ends with a carriage return, which would part them; " +
			"start old after the newline and end it before the carriage return, " +
			"or pass --eol strict to edit the bytes as they are"
	)
	twelve := strings.Repeat("a\r\n", 12)
	for _, c := range []struct {
		name          string
		file          string
		old, new      string
		count         int
		strict        bool
		want          string // the file after, for a row that applies
		lines         []int  // the refusal's lines; nil for a row that applies
		split, more   int
		detail        string
		header        string
		shiftedByHunk bool // a first hunk changes the file before this one
	}{
		{
			name: "a join onto a line that ends in CRLF, in an LF file",
			file: "x\r\nfoo\nb\nc\n", old: "\nfoo", new: " foo", count: 1,
			lines: []int{1}, split: 1, detail: begins,
			header: "expected 1 occurrence, found 1, and replacing it would split a CRLF line ending",
		},
		{
			name: "the same copy, with new keeping the newline",
			file: "x\r\nfoo\nb\nc\n", old: "\nfoo", new: "\nbar", count: 1,
			want: "x\r\nbar\nb\nc\n",
		},
		{
			name: "an old that ends on a carriage return, in a CRLF file",
			file: "a\r\nb\r\n", old: "a\r", new: "A", count: 1,
			lines: []int{1}, split: 1, detail: ends,
		},
		{
			name: "the same copy, with new ending on one too, as a mixed patch's lone CR does",
			file: "a\r\nb\r\n", old: "a\r", new: "A\r", count: 1,
			want: "A\r\nb\r\n",
		},
		{
			name: "a bare carriage return against every CRLF, as the fuzz found",
			file: "a\r\nb\r\n", old: "\r", new: "", count: 2,
			lines: []int{1, 2}, split: 2, detail: ends,
			header: "expected 2 occurrences, found 2, and replacing them would split CRLF line endings",
		},
		{
			name: "both edges at once",
			file: "p\r\nX\r\nq\nr\ns\n", old: "\nX\r", new: "Y", count: 1,
			lines: []int{1}, split: 1, detail: both,
		},
		{
			name: "an empty new where old begins on the newline",
			file: "x\r\nfoo\nb\nc\n", old: "\nfoo", new: "", count: 1,
			lines: []int{1}, split: 1, detail: begins,
		},
		{
			name: "an empty new where old ends on the carriage return",
			file: "a\r\nb\r\n", old: "a\r", new: "", count: 1,
			lines: []int{1}, split: 1, detail: ends,
		},
		{
			name: "one copy of three",
			file: "a\r\nk\nb\nk\nc\nk\nd\ne\n", old: "\nk", new: "K", count: 3,
			lines: []int{1}, split: 1, detail: begins,
			header: "expected 3 occurrences, found 3, and replacing 1 of them would split a CRLF line ending",
		},
		{
			name: "two copies of three",
			file: "a\r\nk\nb\r\nk\nc\nk\nd\ne\nf\n", old: "\nk", new: "K", count: 3,
			lines: []int{1, 3}, split: 2, detail: begins,
			header: "expected 3 occurrences, found 3, and replacing 2 of them would split CRLF line endings",
		},
		{
			name: "more copies than the report lists",
			file: twelve, old: "\r", new: "", count: 12,
			lines: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, split: 12, more: 2, detail: ends,
		},
		{
			name: "the lines of a file an earlier hunk changed",
			file: "x\r\nfoo\nb\nc\n", old: "\nfoo", new: " foo", count: 1,
			lines: []int{1}, split: 1, detail: begins, shiftedByHunk: true,
		},
		{
			name: "a replacement that changes nothing",
			file: "a\r\nb\r\n", old: "\r", new: "\r", count: 2,
			want: "a\r\nb\r\n",
		},
		{
			name: "a copy at the start of the file",
			file: "\nfoo\nb\n", old: "\nfoo", new: "foo", count: 1,
			want: "foo\nb\n",
		},
		{
			name: "a carriage return that ends the file",
			file: "a\r\nb\r", old: "b\r", new: "B", count: 1,
			want: "a\r\nB",
		},
		{
			name: "a file with no CRLF",
			file: "x\nfoo\n", old: "\nfoo", new: " foo", count: 1,
			want: "x foo\n",
		},
		{
			name: "strict edits the bytes as they are",
			file: "x\r\nfoo\nb\nc\n", old: "\nfoo", new: " foo", count: 1, strict: true,
			want: "x\r foo\nb\nc\n",
		},
		{
			name: "strict takes the carriage returns out",
			file: "a\r\nb\r\n", old: "\r", new: "", count: 2, strict: true,
			want: "a\nb\n",
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
				// Changes a line without changing its ending or the count of
				// the copies after it.
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
			if d == nil || d.Kind != DiagSplitCRLF {
				t.Fatalf("near = %+v, want a split line ending", d)
			}
			if d.Cause != "split line ending" || d.Detail != c.detail {
				t.Errorf("cause %q, detail %q; want %q, %q", d.Cause, d.Detail, "split line ending", c.detail)
			}
			if !slices.Equal(d.Lines, c.lines) || d.Found != c.split || d.MoreLines != c.more {
				t.Errorf("lines %v, split %d, more %d; want %v, %d, %d", d.Lines, d.Found, d.MoreLines, c.lines, c.split, c.more)
			}
			if want := len(hunks) - 1; d.Shifted != want {
				t.Errorf("shifted = %d, want %d", d.Shifted, want)
			}
			var out bytes.Buffer
			writeFailureBlocks(&out, ve.Failures)
			if c.header != "" && !strings.Contains(out.String(), "\n  "+c.header+"\n") {
				t.Errorf("report does not say %q:\n%s", c.header, out.String())
			}
			if !strings.Contains(out.String(), "\n\n  "+c.detail+"\n") {
				t.Errorf("report does not end on the detail:\n%s", out.String())
			}
			if note := "line numbers count against the file as the hunk before this one would have left it"; strings.Contains(out.String(), note) != c.shiftedByHunk {
				t.Errorf("shifted note printed = %v, want %v:\n%s", !c.shiftedByHunk, c.shiftedByHunk, out.String())
			}
		})
	}
}

// The report names one line as a line and several as lines, and says how many
// it did not list.
func TestASplitCRLFRendersItsLines(t *testing.T) {
	for _, c := range []struct {
		lines []int
		more  int
		want  string
	}{
		{[]int{4}, 0, "  at line 4\n"},
		{[]int{1, 3}, 0, "  at lines 1, 3\n"},
		{[]int{1, 2}, 5, "  at lines 1, 2, and 5 more\n"},
	} {
		d := &Diagnosis{Kind: DiagSplitCRLF, Lines: c.lines, MoreLines: c.more, Detail: "why"}
		if got := d.Render("  "); !strings.HasPrefix(got, c.want) || !strings.HasSuffix(got, "\n\n  why\n") {
			t.Errorf("Render(%v, %d more) = %q, want it to open %q and end on the detail", c.lines, c.more, got, c.want)
		}
	}
}

// DiagnoseSplitCRLF on its own, for what Validate never hands it: an empty
// old, and a count beyond the copies there are, which looks at those there
// are.
func TestDiagnoseSplitCRLFOnItsOwn(t *testing.T) {
	if d := DiagnoseSplitCRLF([]byte("a\r\nb"), nil, nil, 1); d != nil {
		t.Errorf("DiagnoseSplitCRLF with no old = %+v, want nil", d)
	}
	d := DiagnoseSplitCRLF([]byte("a\r\nb\r\n"), []byte("\r"), nil, 5)
	if d == nil || d.Found != 2 || !slices.Equal(d.Lines, []int{1, 2}) {
		t.Errorf("DiagnoseSplitCRLF past the copies = %+v, want the two there are", d)
	}
}

// §7.2 diagnoses ten failing hunks a batch and reports the rest bare. A split
// is reported whole past the cap: it prints no span, and bare it would read
// "expected 1 occurrence, found 1".
func TestASplitIsDiagnosedPastTheCap(t *testing.T) {
	files := map[string]string{"split.txt": "x\r\nfoo\nb\nc\n"}
	var hunks []Hunk
	for i := range diagnoseLimit {
		name := fmt.Sprintf("miss%02d.txt", i)
		files[name] = "here\n"
		hunks = append(hunks, Hunk{Op: OpReplace, Path: name, Line: i + 1, Count: 1, Old: []byte("not here"), New: []byte("x")})
	}
	hunks = append(hunks, Hunk{Op: OpReplace, Path: "split.txt", Line: 99, Count: 1, Old: []byte("\nfoo"), New: []byte(" foo")})
	tree, root := fixture(t, files)
	before := snapshot(t, root)
	_, err := NewTxn(tree, Options{}).Run(&Patch{Hunks: hunks})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Failures) != diagnoseLimit+1 {
		t.Fatalf("err = %v, want %d failures", err, diagnoseLimit+1)
	}
	for _, f := range ve.Failures[:diagnoseLimit] {
		if f.Near == nil {
			t.Fatalf("hunk %d has no diagnosis inside the cap", f.Hunk)
		}
	}
	if last := ve.Failures[diagnoseLimit]; last.Near == nil || last.Near.Kind != DiagSplitCRLF {
		t.Errorf("the split past the cap = %+v, want its diagnosis", last)
	}
	assertUnchanged(t, root, before)
}

// The page an agent reads, in text and JSON: a join onto a line that ends in
// CRLF, in a file whose other lines end in LF. Neither names the root, so
// nothing is substituted. --eol strict applies the same patch.
func TestASplitCRLFIsGolden(t *testing.T) {
	const file, patch = "x\r\nfoo\nb\nc\n", "@@ file f.txt\n@@ old\n\nfoo\n@@ new\n foo\n"
	root := cliTree(t, map[string]string{"f.txt": file})
	before := snapshot(t, root)
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch || out != "" {
		t.Fatalf("exit %d, stdout %q; want %d and the report on stderr: %s", code, out, exitNoMatch, errOut)
	}
	golden(t, "report-split-crlf", errOut)

	code, js, errOut := runCLI(t, root, []string{"--json"}, patch)
	if code != exitNoMatch || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want %d and nothing on stderr", code, errOut, exitNoMatch)
	}
	var report struct {
		Failures []struct {
			NearMiss struct{ Cause string } `json:"near_miss"`
		}
	}
	must(t, json.Unmarshal([]byte(js), &report))
	if len(report.Failures) != 1 || report.Failures[0].NearMiss.Cause != "split line ending" {
		t.Errorf("JSON failures = %+v, want one whose cause is a split line ending", report.Failures)
	}
	golden(t, "report-split-crlf-json", js)
	assertUnchanged(t, root, before)

	code, out, errOut = runCLI(t, root, []string{"--eol", "strict"}, patch)
	if code != exitOK {
		t.Fatalf("under --eol strict: exit %d: %s%s", code, out, errOut)
	}
	if got := readFile(t, root, "f.txt"); got != "x\r foo\nb\nc\n" {
		t.Errorf("under --eol strict f.txt = %q, want the bytes as the patch gave them", got)
	}
}

// The inverse that #122 led to, with no bare carriage return anywhere. The
// patch moves the file's majority from CRLF to LF, so its inverse reads "\nR"
// as LF and finds it on the newline of the CRLF the patch wrote. Until
// 2026-10-26 that left the CR on its own, which broke up "P\nQ" where the patch
// had put it, and the next hunk took the file's other copy of it, at exit 0.
// Now the inverse is refused and the file is as the patch left it.
func TestAnInverseThatWouldSplitACRLFIsRefused(t *testing.T) {
	const start, patch = "a\r\nv\r\nw\r\nx\r\ny\r\nP\nQ\nm\nn\n",
		"@@ file f.txt\n@@ old\na\n@@ new\nP\nQ\n@@ old\nQ\nv\nw\nx\ny\n@@ new\n\nR\n"
	root := cliTree(t, map[string]string{"f.txt": start})
	if code, out, errOut := runCLI(t, root, nil, patch); code != exitOK {
		t.Fatalf("the patch: exit %d: %s%s", code, out, errOut)
	}
	after := readFile(t, root, "f.txt")
	if after != "P\r\n\r\nR\r\nP\nQ\nm\nn\n" {
		t.Fatalf("after the patch f.txt = %q", after)
	}
	var inv bytes.Buffer
	if code := cli([]string{"invert"}, strings.NewReader(patch), &inv, os.Stderr); code != exitOK {
		t.Fatalf("invert exit %d", code)
	}
	code, out, errOut := runCLI(t, root, nil, inv.String())
	if code != exitNoMatch {
		t.Fatalf("the inverse: exit %d, want %d: %s%s", code, exitNoMatch, out, errOut)
	}
	if !strings.Contains(errOut, "replacing it would split a CRLF line ending\n  at line 2\n") {
		t.Errorf("the inverse's refusal does not name the split:\n%s", errOut)
	}
	if got := readFile(t, root, "f.txt"); got != after {
		t.Errorf("f.txt = %q, want it as the patch left it, %q", got, after)
	}
}
