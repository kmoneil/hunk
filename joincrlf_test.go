package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The details a joined CRLF is refused with, one per place a carriage return
// can meet a newline, and the one that names two.
const (
	joinBefore = "a carriage return comes just before old, and replacing old would put a newline right after it, " +
		"so under --eol auto the two would read as a CRLF line ending the patch did not write; " +
		"start old on that carriage return, or pass --eol strict to edit the bytes as they are"
	joinWithin = "new has a carriage return before a line break, and in this file a line break is a newline, " +
		"so under --eol auto the two would read as a CRLF line ending the patch did not write; " +
		"take the carriage return out of new, or pass --eol strict to edit the bytes as they are"
	joinAfter = "new ends with a carriage return, and a newline would follow it, " +
		"so under --eol auto the two would read as a CRLF line ending the patch did not write; " +
		"end new before its carriage return, or pass --eol strict to edit the bytes as they are"
	joinBeforeAndAfter = "a carriage return comes just before old, and replacing old would put a newline right after it; " +
		"new ends with a carriage return, and a newline would follow it, " +
		"so under --eol auto each carriage return and its newline would read as a CRLF line ending the patch did not write; " +
		"start old on that carriage return, end new before its carriage return, " +
		"or pass --eol strict to edit the bytes as they are"
	joinAll = "a carriage return comes just before old, and replacing old would put a newline right after it; " +
		"new has a carriage return before a line break, and in this file a line break is a newline; " +
		"new ends with a carriage return, and a newline would follow it, " +
		"so under --eol auto each carriage return and its newline would read as a CRLF line ending the patch did not write; " +
		"start old on that carriage return, take the carriage return out of new, end new before its carriage return, " +
		"or pass --eol strict to edit the bytes as they are"
)

// Under --eol auto a replacement may not put a carriage return just before a
// newline that did not follow it (#126), which would read back as a CRLF line
// ending the patch did not write. Each row is one hunk against one file, built
// as Hunk values so that every CR is where the row puts it; a refused row
// asserts the failure, the lines, the cause, the report's header and that
// nothing was written, and an applied row asserts the file.
func TestAReplacementThatJoinsACRLFIsRefused(t *testing.T) {
	twelve := strings.Repeat("a\rX\n", 12)
	for _, c := range []struct {
		name          string
		file          string
		old, new      string
		count         int
		strict        bool
		want          string // the file after, for a row that applies
		lines         []int  // the refusal's lines; nil for a row that applies
		joined, more  int
		detail        string
		header        string
		shiftedByHunk bool // a first hunk changes the file before this one
	}{
		{
			name: "a carriage return new puts before a line break, in an LF file, as the fuzz found",
			file: "a\nX\nb\nc\n", old: "X", new: "Y\r\r\nZ", count: 1,
			lines: []int{2}, joined: 1, detail: joinWithin,
			header: "expected 1 occurrence, found 1, and replacing it would join a carriage return to a newline",
		},
		{
			name: "the same in a CRLF file, where the line break is a CRLF of its own",
			file: "a\r\nX\r\nb\r\n", old: "X", new: "Y\r\r\nZ", count: 1,
			want: "a\r\nY\r\r\nZ\r\nb\r\n",
		},
		{
			name: "new keeping a CRLF its old matched, in an LF file",
			file: "a\r\nb\nc\nd\n", old: "a\r\r\nb", new: "A\r\r\nB", count: 1,
			want: "A\r\nB\nc\nd\n",
		},
		{
			name: "new with more of them than old matched",
			file: "a\r\nb\nc\nd\n", old: "a\r\r\nb", new: "A\r\r\nB\r\r\nC", count: 1,
			lines: []int{1}, joined: 1, detail: joinWithin,
		},
		{
			name: "new ending on a carriage return before the file's newline",
			file: "aX\nb\nc\n", old: "X", new: "Y\r", count: 1,
			lines: []int{1}, joined: 1, detail: joinAfter,
		},
		{
			name: "the same in a CRLF file, before a newline on its own",
			file: "aX\nb\r\nc\r\n", old: "X", new: "Y\r", count: 1,
			lines: []int{1}, joined: 1, detail: joinAfter,
		},
		{
			name: "new ending on a carriage return before the file's own carriage return",
			file: "aX\r\nb\r\n", old: "X", new: "Y\r", count: 1,
			want: "aY\r\r\nb\r\n",
		},
		{
			name: "old and new both ending on the carriage return of a CRLF, which keeps it",
			file: "a\r\nb\r\n", old: "a\r", new: "A\r", count: 1,
			want: "A\r\nb\r\n",
		},
		{
			name: "new opening on a newline after the file's carriage return",
			file: "a\rX\nb\nc\n", old: "X", new: "\nY", count: 1,
			lines: []int{1}, joined: 1, detail: joinBefore,
		},
		{
			name: "old and new both opening on the newline of a CRLF, which keeps it",
			file: "x\r\nfoo\nb\nc\n", old: "\nfoo", new: "\nbar", count: 1,
			want: "x\r\nbar\nb\nc\n",
		},
		{
			name: "the remedy: old started on the carriage return",
			file: "a\rX\nb\nc\n", old: "\rX", new: "\nY", count: 1,
			want: "a\nY\nb\nc\n",
		},
		{
			name: "an empty new that closes a carriage return up to a newline",
			file: "a\rX\nb\nc\n", old: "X", new: "", count: 1,
			lines: []int{1}, joined: 1, detail: joinBefore,
		},
		{
			name: "an empty new that takes lines out between them, named at the carriage return",
			file: "a\rX\nY\nb\nc\nd\n", old: "X\nY", new: "", count: 1,
			lines: []int{1}, joined: 1, detail: joinBefore,
		},
		{
			name: "an empty new over two touching copies, which close up only together",
			file: "a\rXX\nb\nc\n", old: "X", new: "", count: 2,
			lines: []int{1}, joined: 1, detail: joinBefore,
			header: "expected 2 occurrences, found 2, and replacing 1 of them would join a carriage return to a newline",
		},
		{
			name: "an empty new over two copies apart, neither of which closes one up",
			file: "a\rXbX\nc\nd\n", old: "X", new: "", count: 2,
			want: "a\rb\nc\nd\n",
		},
		{
			name: "two touching copies whose new ends on a carriage return and opens on a newline",
			file: "aXXb\nc\nd\n", old: "X", new: "\nq\r", count: 2,
			lines: []int{1}, joined: 1, detail: joinAfter,
			header: "expected 2 occurrences, found 2, and replacing 1 of them would join a carriage return to a newline",
		},
		{
			name: "two touching copies opening on a newline, whose new ends on a carriage return",
			file: "a\nX\nXb\nc\n", old: "\nX", new: "Y\r", count: 2,
			want: "aY\rY\rb\nc\n",
		},
		{
			name: "two touching copies that met on a CRLF, which new keeps",
			file: "p\r\nX\r\nX\r\nq\nr\ns\nt\n", old: "\nX\r", new: "\nY\r", count: 2,
			want: "p\r\nY\r\nY\r\nq\nr\ns\nt\n",
		},
		{
			name: "a carriage return on both sides of new",
			file: "a\rX\nb\nc\n", old: "X", new: "\nY\r", count: 1,
			lines: []int{1}, joined: 1, detail: joinBeforeAndAfter,
		},
		{
			name: "both sides of a copy that spans a line, named at the first",
			file: "a\rX\nY\nb\nc\nd\n", old: "X\nY", new: "\nZ\r", count: 1,
			lines: []int{1}, joined: 1, detail: joinBeforeAndAfter,
		},
		{
			name: "all three at once",
			file: "a\rX\nb\nc\nd\n", old: "X", new: "\nY\r\r\nZ\r", count: 1,
			lines: []int{1}, joined: 1, detail: joinAll,
		},
		{
			name: "every copy",
			file: "aX\nbX\nc\nd\n", old: "X", new: "Y\r", count: 2,
			lines: []int{1, 2}, joined: 2, detail: joinAfter,
			header: "expected 2 occurrences, found 2, and replacing them would join carriage returns to newlines",
		},
		{
			name: "two copies of three",
			file: "aX\nbX\ncXd\ne\nf\n", old: "X", new: "Y\r", count: 3,
			lines: []int{1, 2}, joined: 2, detail: joinAfter,
			header: "expected 3 occurrences, found 3, and replacing 2 of them would join carriage returns to newlines",
		},
		{
			name: "more copies than the report lists",
			file: twelve, old: "X", new: "", count: 12,
			lines: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, joined: 12, more: 2, detail: joinBefore,
		},
		{
			name: "the lines of a file an earlier hunk changed",
			file: "aX\nb\nc\n", old: "X", new: "Y\r", count: 1,
			lines: []int{1}, joined: 1, detail: joinAfter, shiftedByHunk: true,
		},
		{
			name: "a copy at the start of the file",
			file: "Xa\nb\n", old: "X", new: "\nY", count: 1,
			want: "\nYa\nb\n",
		},
		{
			name: "a carriage return that ends the file",
			file: "a\nbX", old: "X", new: "Y\r", count: 1,
			want: "a\nbY\r",
		},
		{
			name: "an empty new at the end of the file",
			file: "a\nb\rX", old: "X", new: "", count: 1,
			want: "a\nb\r",
		},
		{
			name: "strict writes the carriage return before the line break as given",
			file: "a\nX\nb\nc\n", old: "X", new: "Y\r\r\nZ", count: 1, strict: true,
			want: "a\nY\r\r\nZ\nb\nc\n",
		},
		{
			name: "strict closes the carriage return up to the newline",
			file: "a\rX\nb\nc\n", old: "X", new: "", count: 1, strict: true,
			want: "a\r\nb\nc\n",
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
			if d == nil || d.Kind != DiagJoinedCRLF {
				t.Fatalf("near = %+v, want a joined line ending", d)
			}
			if d.Cause != "joined line ending" || d.Detail != c.detail {
				t.Errorf("cause %q, detail\n%q\nwant %q,\n%q", d.Cause, d.Detail, "joined line ending", c.detail)
			}
			if !slices.Equal(d.Lines, c.lines) || d.Found != c.joined || d.MoreLines != c.more {
				t.Errorf("lines %v, joined %d, more %d; want %v, %d, %d", d.Lines, d.Found, d.MoreLines, c.lines, c.joined, c.more)
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

// A copy that would both split a CRLF and join one is reported as the split:
// Validate asks DiagnoseSplitCRLF first, and one refusal says what to do.
func TestASplitIsReportedBeforeAJoin(t *testing.T) {
	tree, root := fixture(t, map[string]string{"f.txt": "a\rX\r\nb\nc\n"})
	before := snapshot(t, root)
	_, err := NewTxn(tree, Options{}).Run(&Patch{Hunks: []Hunk{
		{Op: OpReplace, Path: "f.txt", Line: 1, Count: 1, Old: []byte("X\r"), New: nil},
	}})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Failures) != 1 {
		t.Fatalf("err = %v, want one refusal", err)
	}
	if d := ve.Failures[0].Near; d == nil || d.Kind != DiagSplitCRLF {
		t.Errorf("near = %+v, want the split", d)
	}
	if DiagnoseJoinedCRLF([]byte("a\rX\r\nb\nc\n"), []byte("X\r"), nil, 1, "\n") == nil {
		t.Error("the copy joins one too, or this test proves nothing about the order")
	}
	assertUnchanged(t, root, before)
}

// The report names one line as a line and several as lines, and says how many
// it did not list, as it does for a split.
func TestAJoinedCRLFRendersItsLines(t *testing.T) {
	for _, c := range []struct {
		lines []int
		more  int
		want  string
	}{
		{[]int{4}, 0, "  at line 4\n"},
		{[]int{1, 3}, 0, "  at lines 1, 3\n"},
		{[]int{1, 2}, 5, "  at lines 1, 2, and 5 more\n"},
	} {
		d := &Diagnosis{Kind: DiagJoinedCRLF, Lines: c.lines, MoreLines: c.more, Detail: "why"}
		if got := d.Render("  "); !strings.HasPrefix(got, c.want) || !strings.HasSuffix(got, "\n\n  why\n") {
			t.Errorf("Render(%v, %d more) = %q, want it to open %q and end on the detail", c.lines, c.more, got, c.want)
		}
	}
}

// DiagnoseJoinedCRLF on its own, for what Validate never hands it: an empty
// old, a count beyond the copies there are, which looks at those there are,
// and a CRLF file, where a carriage return before a line break in new is
// written before a CRLF and joins nothing.
func TestDiagnoseJoinedCRLFOnItsOwn(t *testing.T) {
	if d := DiagnoseJoinedCRLF([]byte("a\rX\n"), nil, nil, 1, "\n"); d != nil {
		t.Errorf("DiagnoseJoinedCRLF with no old = %+v, want nil", d)
	}
	d := DiagnoseJoinedCRLF([]byte("aX\nbX\n"), []byte("X"), []byte("Y\r"), 5, "\n")
	if d == nil || d.Found != 2 || !slices.Equal(d.Lines, []int{1, 2}) {
		t.Errorf("DiagnoseJoinedCRLF past the copies = %+v, want the two there are", d)
	}
	if d := DiagnoseJoinedCRLF([]byte("a\r\nX\r\n"), []byte("X"), []byte("Y\r\r\nZ"), 1, "\r\n"); d != nil {
		t.Errorf("DiagnoseJoinedCRLF in a CRLF file = %+v, want nil", d)
	}
	// Two touching copies taken out together leave "a\rb". The newline after
	// the first is the second's, and goes with it. Validate never asks this,
	// since the first copy splits a CRLF.
	if d := DiagnoseJoinedCRLF([]byte("a\r\nX\nXb"), []byte("\nX"), nil, 2, "\n"); d != nil {
		t.Errorf("DiagnoseJoinedCRLF over a run of copies = %+v, want nil", d)
	}
}

// §7.2 diagnoses ten failing hunks a batch and reports the rest bare. A join
// is reported whole past the cap, as a split is.
func TestAJoinIsDiagnosedPastTheCap(t *testing.T) {
	files := map[string]string{"join.txt": "aX\nb\nc\n"}
	var hunks []Hunk
	for i := range diagnoseLimit {
		name := fmt.Sprintf("miss%02d.txt", i)
		files[name] = "here\n"
		hunks = append(hunks, Hunk{Op: OpReplace, Path: name, Line: i + 1, Count: 1, Old: []byte("not here"), New: []byte("x")})
	}
	hunks = append(hunks, Hunk{Op: OpReplace, Path: "join.txt", Line: 99, Count: 1, Old: []byte("X"), New: []byte("Y\r")})
	tree, root := fixture(t, files)
	before := snapshot(t, root)
	_, err := NewTxn(tree, Options{}).Run(&Patch{Hunks: hunks})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Failures) != diagnoseLimit+1 {
		t.Fatalf("err = %v, want %d failures", err, diagnoseLimit+1)
	}
	if last := ve.Failures[diagnoseLimit]; last.Near == nil || last.Near.Kind != DiagJoinedCRLF {
		t.Errorf("the join past the cap = %+v, want its diagnosis", last)
	}
	assertUnchanged(t, root, before)
}

// The page an agent reads, in text and JSON, for the shape the fuzz found: a
// carriage return before a line break in new, in an LF file. --eol strict
// applies the same patch, and writes the payload's bytes.
func TestAJoinedCRLFIsGolden(t *testing.T) {
	const file, patch = "a\nX\nb\nc\n", "@@ file f.txt\n@@ old\nX\n@@ new\nY\r\r\nZ\n"
	root := cliTree(t, map[string]string{"f.txt": file})
	before := snapshot(t, root)
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch || out != "" {
		t.Fatalf("exit %d, stdout %q; want %d and the report on stderr: %s", code, out, exitNoMatch, errOut)
	}
	golden(t, "report-joined-crlf", errOut)

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
	if len(report.Failures) != 1 || report.Failures[0].NearMiss.Cause != "joined line ending" {
		t.Errorf("JSON failures = %+v, want one whose cause is a joined line ending", report.Failures)
	}
	golden(t, "report-joined-crlf-json", js)
	assertUnchanged(t, root, before)

	code, out, errOut = runCLI(t, root, []string{"--eol", "strict"}, patch)
	if code != exitOK {
		t.Fatalf("under --eol strict: exit %d: %s%s", code, out, errOut)
	}
	if got := readFile(t, root, "f.txt"); got != "a\nY\r\r\nZ\nb\nc\n" {
		t.Errorf("under --eol strict f.txt = %q, want the bytes as the patch gave them", got)
	}
}

// The nightly's find for #126, through the CLI. Until 2026-10-27 the patch,
// hunk invert of it and the inverse all exited 0, and the inverse rewrote the
// file's first line rather than the one the patch wrote: "\r\r\n\x80J\n" came
// back "J\x80\r\n\n". Now the patch is refused, and there is nothing to undo.
func TestThePatchThatFlippedItsInverseIsRefused(t *testing.T) {
	const start, patch = "\r\r\n\x80J\n", "@@ file f.txt\n@@ old\nJ\n@@ new\n0\n@@ old\n0\n@@ new\n\r\r\n\n"
	root := cliTree(t, map[string]string{"f.txt": start})
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch {
		t.Fatalf("the patch: exit %d, want %d: %s%s", code, exitNoMatch, out, errOut)
	}
	if !strings.Contains(errOut, "hunk 2  f.txt  (patch line 6)\n  expected 1 occurrence, found 1, and replacing it would join a carriage return to a newline\n  at line 2\n") {
		t.Errorf("the refusal does not name hunk 2's join:\n%s", errOut)
	}
	if got := readFile(t, root, "f.txt"); got != start {
		t.Errorf("f.txt = %q, want it untouched, %q", got, start)
	}
}

// joinsACRLF is an exhaustive scan for what DiagnoseJoinedCRLF finds, sharing
// nothing with it but the rule. It builds the replaced text byte by byte, each
// byte tagged with where it came from, and asks of every CRLF in the result
// that is not inside one copy of new whether the file had a CRLF at the same
// place: the CR stands just before file offset q, the LF at q, and file[q-1:q+1]
// is a CRLF. One inside new is counted, as the rule counts it: in an LF file,
// more CRLFs in new than in old.
func joinsACRLF(file, old, repl string, n int, eol string) bool {
	type from struct{ copy, at int } // copy -1 is the file's byte at; else new's byte at, in that copy
	var out []byte
	var src []from
	var starts []int
	off := 0
	for len(starts) < n {
		i := strings.Index(file[off:], old)
		if i < 0 {
			break
		}
		s := off + i
		for j := off; j < s; j++ {
			out, src = append(out, file[j]), append(src, from{-1, j})
		}
		for j := range len(repl) {
			out, src = append(out, repl[j]), append(src, from{len(starts), j})
		}
		starts = append(starts, s)
		off = s + len(old)
	}
	for j := off; j < len(file); j++ {
		out, src = append(out, file[j]), append(src, from{-1, j})
	}
	after := func(b from) int { // the file offset a byte stands just before
		if b.copy < 0 {
			return b.at + 1
		}
		return starts[b.copy] + len(old)
	}
	at := func(b from) int { // the file offset a byte stands at
		if b.copy < 0 {
			return b.at
		}
		return starts[b.copy]
	}
	for i := 0; i+1 < len(out); i++ {
		if out[i] != '\r' || out[i+1] != '\n' {
			continue
		}
		a, b := src[i], src[i+1]
		if a.copy >= 0 && a.copy == b.copy {
			continue
		}
		q := after(a)
		if q != at(b) || q == 0 || q >= len(file) || file[q-1] != '\r' || file[q] != '\n' {
			return true
		}
	}
	return eol == "\n" && len(starts) > 0 && strings.Count(repl, "\r\n") > strings.Count(old, "\r\n")
}

// A replacement under --eol auto applies only if it joins no carriage return
// to a newline, and is refused as a join only if it does, by joinsACRLF's
// count. Bare carriage returns are the point here, so unlike the counted-copies
// target nothing is skipped for one.
func FuzzAJoinIsRefusedAndOnlyThen(f *testing.F) {
	for _, s := range []struct {
		file, old, repl string
		count           uint8
	}{
		{"a\nX\nb\nc\n", "X", "Y\r\r\nZ", 0},
		{"aX\nb\nc\n", "X", "Y\r", 0},
		{"a\rX\nb\nc\n", "X", "\nY", 0},
		{"a\rX\nb\nc\n", "X", "", 0},
		{"a\rXX\nb\nc\n", "X", "", 1},
		{"aXXb\nc\nd\n", "X", "\nq\r", 1},
		{"p\r\nX\r\nX\r\nq\nr\ns\nt\n", "\nX\r", "\nY\r", 1},
		{"a\r\nb\nc\nd\n", "a\r\r\nb", "A\r\r\nB", 0},
		{"a\r\nX\r\nb\r\n", "X", "Y\r\r\nZ", 0},
		{"\r\r\n\x800\n", "0", "\r\r\n", 0},
	} {
		f.Add(s.file, s.old, s.repl, s.count)
	}
	f.Fuzz(func(t *testing.T, file, old, repl string, count uint8) {
		if old == "" {
			t.Skip()
		}
		n := int(count%3) + 1
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(file), 0o644); err != nil {
			t.Skip()
		}
		tree, err := OpenTree(root, false)
		if err != nil {
			t.Fatal(err)
		}
		defer tree.Close()
		p := &Patch{Hunks: []Hunk{{Op: OpReplace, Path: "f.txt", Line: 1, Count: n, Old: []byte(old), New: []byte(repl)}}}
		x := NewTxn(tree, Options{})
		if err := x.Load(p); err != nil {
			t.Fatal(err)
		}
		failures := x.Validate(p)

		eol := "\n"
		if strings.Count(file, "\r\n") > strings.Count(file, "\n")-strings.Count(file, "\r\n") {
			eol = "\r\n"
		}
		conv := func(s string) string {
			s = strings.ReplaceAll(s, "\r\n", "\n")
			if eol == "\n" {
				return s
			}
			return strings.ReplaceAll(s, "\n", "\r\n")
		}
		joins := joinsACRLF(file, conv(old), conv(repl), n, eol)
		if len(failures) == 0 {
			if joins {
				t.Fatalf("applied %q -> %q x%d in %q, joining a carriage return to a newline: %q",
					old, repl, n, file, x.hunkFile[0].cur)
			}
			return
		}
		if d := failures[0].Near; d != nil && d.Kind == DiagJoinedCRLF && !joins {
			t.Fatalf("refused %q -> %q x%d in %q as a join, which it is not", old, repl, n, file)
		}
	})
}
