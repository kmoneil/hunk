package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// takenLeaves is the middle every taken CRLF's remedy shares, takenDetail
// the whole of it for one line ending hunk 1 kept, and takenHeader the
// header's line for one hunk of one copy.
const (
	takenLeaves = ", and the batch leaves the file's line ending LF where it was CRLF, so the batch's inverse would write the "
	takenDetail = "hunk 1's old and new end on that line ending's carriage return" + takenLeaves +
		"line break back as a newline alone, and the carriage return hunk 1's inverse looks for would be gone; " +
		"write this hunk and hunk 1 as one, or pass --eol strict to edit the bytes as they are"
	takenHeader = "expected 1 occurrence, found 1, and replacing it would take in a CRLF line ending kept earlier in the batch"
)

// Under --eol auto, a hunk may not take in a CRLF line ending an earlier hunk
// kept by ending its old and new on the carriage return, when the batch
// leaves the file LF where it was CRLF (#182): the inverse would write the
// line break back as a newline alone, and the carriage return the earlier
// hunk's inverse looks for would be gone. Each row is a batch against f.txt,
// built as Hunk values so that every CR is where the row puts it. A refused
// row asserts the one failure, its lines, the cause, the remedy, the report's
// header and that nothing was written; an applied row asserts the file.
func TestALineEndingAnEarlierHunkKeptIsRefusedWhenTaken(t *testing.T) {
	const (
		several = "hunk 1's old and new end on those line endings' carriage returns" + takenLeaves +
			"line breaks back as newlines alone, and the carriage returns hunk 1's inverse looks for would be gone; " +
			"write this hunk and hunk 1 as one, or pass --eol strict to edit the bytes as they are"
		two = "the old and new of hunks 1 and 2 end on those line endings' carriage returns" + takenLeaves +
			"line breaks back as newlines alone, and the carriage returns their inverses look for would be gone; " +
			"write this hunk and hunks 1 and 2 as one, or pass --eol strict to edit the bytes as they are"
	)
	byHunk2 := strings.ReplaceAll(takenDetail, "hunk 1", "hunk 2")
	re := func(pattern, tmpl string) Hunk {
		return Hunk{Op: OpRegex, Path: "f.txt", Count: 1, Old: []byte(pattern), New: []byte(tmpl), Re: regexp.MustCompile(pattern)}
	}
	for _, c := range []struct {
		name        string
		file        string
		hunks       []Hunk
		strict      bool
		want        string // the file after, for a row that applies
		hunk        int    // the refused hunk; 0 for a row that applies
		lines       []int
		found, more int
		detail      string
		header      string
	}{
		{
			name: "the shape the fuzz found: kept, then taken, and the file left with no newline",
			file: "x\r\n\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 1, "\n", "0")},
			hunk: 2, lines: []int{1}, found: 1, detail: takenDetail, header: takenHeader,
		},
		{
			name: "taken in a file the batch leaves CRLF, whose inverse writes the CRLF back",
			file: "x\r\n\ry\r\nz\r\n", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 1, "\n\ry", "0\ry")},
			want: "0\ry\r\nz\r\n",
		},
		{
			name: "both edits as one hunk, which keeps nothing",
			file: "x\r\n\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r\n", "0")},
			want: "0\r",
		},
		{
			name: "strict edits the bytes as they are",
			file: "x\r\n\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 1, "\r\n", "0")}, strict: true,
			want: "0\r",
		},
		{
			name: "taken, but never kept: old does not end on the carriage return",
			file: "x\r\n\r", hunks: []Hunk{rep("f.txt", 1, 1, "x", "y"), rep("f.txt", 2, 1, "\n", "0")},
			want: "y0\r",
		},
		{
			name: "kept, and kept again by a later hunk, which is the one named",
			file: "x\r\n\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "y\r"), rep("f.txt", 2, 1, "y\r", "\r"), rep("f.txt", 3, 1, "\n", "0")},
			hunk: 3, lines: []int{1}, found: 1, detail: byHunk2, header: takenHeader,
		},
		{
			name: "kept, and moved by text a hunk added before it",
			file: "ab\r\n\r", hunks: []Hunk{rep("f.txt", 1, 1, "b\r", "\r"), rep("f.txt", 2, 1, "a", "AAA"), rep("f.txt", 3, 1, "\n", "0")},
			hunk: 3, lines: []int{1}, found: 1, detail: takenDetail,
		},
		{
			name: "kept, and not moved by text a hunk added after it",
			file: "x\r\nb\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 1, "b", "BBB"), rep("f.txt", 3, 1, "\n", "0")},
			hunk: 3, lines: []int{1}, found: 1, detail: takenDetail,
		},
		{
			name: "two kept by one hunk and both taken",
			file: "x\r\nx\r\nq\r", hunks: []Hunk{rep("f.txt", 1, 2, "x\r", "\r"), rep("f.txt", 2, 2, "\n", "0")},
			hunk: 2, lines: []int{1, 2}, found: 2, detail: several,
			header: "expected 2 occurrences, found 2, and replacing them would take in CRLF line endings kept earlier in the batch",
		},
		{
			name: "two kept by two hunks and both taken",
			file: "x\r\ny\r\nq\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 1, "y\r", "\r"), rep("f.txt", 3, 2, "\n", "0")},
			hunk: 3, lines: []int{1, 2}, found: 2, detail: two,
		},
		{
			name: "two kept by two hunks, the later one first in the file",
			file: "x\r\ny\r\nq\r", hunks: []Hunk{rep("f.txt", 1, 1, "y\r", "\r"), rep("f.txt", 2, 1, "x\r", "\r"), rep("f.txt", 3, 2, "\n", "0")},
			hunk: 3, lines: []int{1, 2}, found: 2, detail: two,
		},
		{
			name: "one copy of two takes one in",
			file: "x\r\ny\r\nz\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 2, "\n", "0")},
			hunk: 2, lines: []int{1}, found: 1, detail: takenDetail,
			header: "expected 2 occurrences, found 2, and replacing them would take in a CRLF line ending kept earlier in the batch",
		},
		{
			name: "more taken than the report lists",
			file: strings.Repeat("x\r\n", 12) + "\r", hunks: []Hunk{rep("f.txt", 1, 12, "x\r", "\r"), rep("f.txt", 2, 12, "\n", "0")},
			hunk: 2, lines: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, found: 12, more: 2, detail: several,
		},
		{
			name: "a hunk that changes nothing keeps nothing, and its inverse changes nothing",
			file: "x\r\n\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "x\r"), rep("f.txt", 2, 1, "\n", "0")},
			want: "x0\r",
		},
		{
			name: "a delete and a create between them, which leave nothing kept",
			file: "x\r\n\r", hunks: []Hunk{
				rep("f.txt", 1, 1, "x\r", "\r"),
				{Op: OpDelete, Path: "f.txt", Line: 2},
				{Op: OpCreate, Path: "f.txt", Line: 3, Body: []byte("\r\n\r")},
				rep("f.txt", 4, 1, "\n", "0"),
			},
			want: "0\r",
		},
		{
			name: "a delete and a create after a take, which leave nothing of it",
			file: "x\r\n\r", hunks: []Hunk{
				rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 1, "\n", "0"),
				{Op: OpDelete, Path: "f.txt", Line: 3},
				{Op: OpCreate, Path: "f.txt", Line: 4, Body: []byte("a\r\nb")},
				rep("f.txt", 5, 1, "a\n", "a"),
			},
			want: "ab",
		},
		{
			name: "a regex hunk between them, after which nothing kept is followed",
			file: "x\r\nq\r", hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), re("q", "Q"), rep("f.txt", 3, 1, "\n", "0")},
			want: "0Q\r",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"f.txt": c.file})
			before := snapshot(t, root)
			opt := Options{}
			if c.strict {
				opt.EOL = EOLStrict
			}
			for i := range c.hunks {
				c.hunks[i].Line = i + 1
			}
			_, err := NewTxn(tree, opt).Run(&Patch{Hunks: c.hunks})
			if c.hunk == 0 {
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
			f, h := ve.Failures[0], c.hunks[c.hunk-1]
			if f.Hunk != c.hunk || f.Path != "f.txt" || f.PatchLine != c.hunk || f.Expected != h.Count || f.Found != h.Count || f.Refusal != "" {
				t.Errorf("failure = hunk %d %s line %d, expected %d, found %d, refusal %q; want hunk %d, line %d, %d and %d, no refusal",
					f.Hunk, f.Path, f.PatchLine, f.Expected, f.Found, f.Refusal, c.hunk, c.hunk, h.Count, h.Count)
			}
			d := f.Near
			if d == nil || d.Kind != DiagTakenCRLF {
				t.Fatalf("near = %+v, want a taken line ending", d)
			}
			if d.Cause != "taken line ending" || d.Detail != c.detail {
				t.Errorf("cause %q, detail\n%q\nwant %q,\n%q", d.Cause, d.Detail, "taken line ending", c.detail)
			}
			if !slices.Equal(d.Lines, c.lines) || d.Found != c.found || d.MoreLines != c.more {
				t.Errorf("lines %v, found %d, more %d; want %v, %d, %d", d.Lines, d.Found, d.MoreLines, c.lines, c.found, c.more)
			}
			// Every hunk before the one refused changed the file.
			if d.Shifted != c.hunk-1 {
				t.Errorf("shifted = %d, want %d", d.Shifted, c.hunk-1)
			}
			var out bytes.Buffer
			writeFailureBlocks(&out, ve.Failures)
			if c.header != "" && !strings.Contains(out.String(), "\n  "+c.header+"\n") {
				t.Errorf("report does not say %q:\n%s", c.header, out.String())
			}
			if !strings.Contains(out.String(), "\n\n  "+c.detail+"\n") {
				t.Errorf("report does not end on the detail:\n%s", out.String())
			}
		})
	}
}

// Whether a take is refused is a question about the file the batch leaves,
// so it is asked only of a file every hunk applied to. One a later hunk
// failed on is reported for that hunk alone. Another file's failure does not
// hide it, and the two are reported in hunk order. And in an LF file a copy
// that would take a kept CRLF in is a misread, refused before it can be
// taken.
func TestATakenCRLFIsAskedOfAFileTheBatchFinished(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		hunks []Hunk
		want  []int      // the failures' hunks, in order
		kinds []DiagKind // and their kinds
	}{
		{
			name:  "a later hunk on the same file does not match",
			files: map[string]string{"f.txt": "x\r\n\r"},
			hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 1, "\n", "0"), rep("f.txt", 3, 1, "not here", "N")},
			want:  []int{3}, kinds: []DiagKind{DiagNoAnchor},
		},
		{
			name:  "an earlier hunk on another file does not match",
			files: map[string]string{"f.txt": "x\r\n\r", "g.txt": "g\n"},
			hunks: []Hunk{rep("g.txt", 1, 1, "not here", "N"), rep("f.txt", 2, 1, "x\r", "\r"), rep("f.txt", 3, 1, "\n", "0")},
			want:  []int{1, 3}, kinds: []DiagKind{DiagNoAnchor, DiagTakenCRLF},
		},
		{
			name:  "a later hunk on another file does not match, and the take is reported first",
			files: map[string]string{"f.txt": "x\r\n\r", "g.txt": "g\n"},
			hunks: []Hunk{rep("f.txt", 1, 1, "x\r", "\r"), rep("f.txt", 2, 1, "\n", "0"), rep("g.txt", 3, 1, "not here", "N")},
			want:  []int{2, 3}, kinds: []DiagKind{DiagTakenCRLF, DiagNoAnchor},
		},
		{
			name:  "an LF file, where the copy that would take it in is a misread",
			files: map[string]string{"f.txt": "a\r\nb\nc\n"},
			hunks: []Hunk{rep("f.txt", 1, 1, "a\r", "A\r"), rep("f.txt", 2, 1, "\r\r\n", "\n")},
			want:  []int{2}, kinds: []DiagKind{DiagMisreadCRLF},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, c.files)
			before := snapshot(t, root)
			_, err := NewTxn(tree, Options{}).Run(&Patch{Hunks: c.hunks})
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			var got []int
			var kinds []DiagKind
			for _, f := range ve.Failures {
				got = append(got, f.Hunk)
				if f.Near != nil {
					kinds = append(kinds, f.Near.Kind)
				}
			}
			if !slices.Equal(got, c.want) || !slices.Equal(kinds, c.kinds) {
				t.Errorf("failures at hunks %v, kinds %v; want %v, %v", got, kinds, c.want, c.kinds)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// §7.2 diagnoses ten failing hunks a batch and reports the rest bare. A taken
// line ending is reported whole past the cap, as a split and a join are.
func TestATakenCRLFIsDiagnosedPastTheCap(t *testing.T) {
	files := map[string]string{"f.txt": "x\r\n\r"}
	var hunks []Hunk
	for i := range diagnoseLimit {
		name := fmt.Sprintf("miss%02d.txt", i)
		files[name] = "here\n"
		hunks = append(hunks, rep(name, i+1, 1, "not here", "x"))
	}
	hunks = append(hunks, rep("f.txt", 98, 1, "x\r", "\r"), rep("f.txt", 99, 1, "\n", "0"))
	tree, root := fixture(t, files)
	before := snapshot(t, root)
	_, err := NewTxn(tree, Options{}).Run(&Patch{Hunks: hunks})
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Failures) != diagnoseLimit+1 {
		t.Fatalf("err = %v, want %d failures", err, diagnoseLimit+1)
	}
	if last := ve.Failures[diagnoseLimit]; last.Near == nil || last.Near.Kind != DiagTakenCRLF {
		t.Errorf("the take past the cap = %+v, want its diagnosis", last)
	}
	assertUnchanged(t, root, before)
}

// The report names one line as a line and several as lines, and says how many
// it did not list, as it does for a split, a join and a misread.
func TestATakenCRLFRendersItsLines(t *testing.T) {
	for _, c := range []struct {
		lines []int
		more  int
		want  string
	}{
		{[]int{4}, 0, "  at line 4\n"},
		{[]int{1, 3}, 0, "  at lines 1, 3\n"},
		{[]int{1, 2}, 5, "  at lines 1, 2, and 5 more\n"},
	} {
		d := &Diagnosis{Kind: DiagTakenCRLF, Lines: c.lines, MoreLines: c.more, Detail: "why"}
		if got := d.Render("  "); !strings.HasPrefix(got, c.want) || !strings.HasSuffix(got, "\n\n  why\n") {
			t.Errorf("Render(%v, %d more) = %q, want it to open %q and end on the detail", c.lines, c.more, got, c.want)
		}
	}
}

// trackKept on its own, for what the table cannot reach through Validate: an
// empty old, which the parser refuses, and the two ways a copy ending on a
// carriage return keeps nothing, at the end of the file and before a byte
// that is no newline.
func TestTrackKeptOnItsOwn(t *testing.T) {
	for _, c := range []struct {
		name, cur, old, repl string
	}{
		{"an empty old", "a\r\n", "", "\r"},
		{"nothing kept and nothing to keep", "a\r\nb\r\n", "a", "A"},
		{"a carriage return that ends the file", "a\r\nb\r", "b\r", "B\r"},
		{"a carriage return before a letter", "a\rb\r\n", "a\r", "A\r"},
		{"a new that does not end on it, which Validate refuses as a split", "a\r\n", "a\r", "A"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &file{cur: []byte(c.cur)}
			f.trackKept(1, []byte(c.old), []byte(c.repl), 1)
			if len(f.kept) != 0 || len(f.taken) != 0 {
				t.Errorf("kept %+v, taken %+v; want neither", f.kept, f.taken)
			}
		})
	}
}

// hunkList is prose: one hunk, two, and three or more.
func TestHunkList(t *testing.T) {
	for _, c := range []struct {
		ns   []int
		want string
	}{
		{[]int{1}, "hunk 1"},
		{[]int{1, 3}, "hunks 1 and 3"},
		{[]int{1, 2, 3}, "hunks 1, 2 and 3"},
	} {
		if got := hunkList(c.ns); got != c.want {
			t.Errorf("hunkList(%v) = %q, want %q", c.ns, got, c.want)
		}
	}
}

// The page an agent reads, in text and JSON, for the nightly's find (#182)
// as a patch: its first payload line is "x\r", a CRLF line pasted into an LF
// heredoc. Until 2026-11-19 the patch, hunk invert of it and the inverse all
// exited 0, and the file ended "\nx\r". --eol strict applies the same patch to
// the bytes as they are.
func TestATakenCRLFIsGolden(t *testing.T) {
	const file, patch = "x\r\n\r", "@@ file f.txt\n@@ old\nx\r\n@@ new\n\r\n@@ old\n\n\n@@ new\n0\n"
	root := cliTree(t, map[string]string{"f.txt": file})
	before := snapshot(t, root)
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch || out != "" {
		t.Fatalf("exit %d, stdout %q; want %d and the report on stderr: %s", code, out, exitNoMatch, errOut)
	}
	golden(t, "report-taken-crlf", errOut)

	code, js, errOut := runCLI(t, root, []string{"--json"}, patch)
	if code != exitNoMatch || errOut != "" {
		t.Fatalf("exit %d, stderr %q; want %d and nothing on stderr", code, errOut, exitNoMatch)
	}
	var report struct {
		Failures []struct {
			Hunk     int
			Lines    []int
			NearMiss struct{ Cause, Detail string } `json:"near_miss"`
		}
	}
	must(t, json.Unmarshal([]byte(js), &report))
	if len(report.Failures) != 1 || report.Failures[0].Hunk != 2 || report.Failures[0].NearMiss.Cause != "taken line ending" ||
		report.Failures[0].NearMiss.Detail != takenDetail || !slices.Equal(report.Failures[0].Lines, []int{1}) {
		t.Errorf("JSON failures = %+v, want hunk 2 at line 1 whose cause is a taken line ending", report.Failures)
	}
	golden(t, "report-taken-crlf-json", js)
	assertUnchanged(t, root, before)

	code, out, errOut = runCLI(t, root, []string{"--eol", "strict"}, patch)
	if code != exitOK {
		t.Fatalf("under --eol strict: exit %d: %s%s", code, out, errOut)
	}
	if got := readFile(t, root, "f.txt"); got != "\r0\r" {
		t.Errorf("under --eol strict f.txt = %q, want the newline byte replaced where it is", got)
	}
}

// FuzzATakenCRLFIsRefusedAndOnlyThen: two replacements under --eol auto. A
// batch refused as taking a kept CRLF is one the oracle says takes one, and
// a batch that applied takes none. The oracle shares nothing with trackKept:
// it builds the file after the first hunk with each byte marked if it is a
// carriage return the hunk's old and new both end on where the file had a
// newline after it, then asks whether a copy of the second hunk holds a mark
// and the byte after it, and whether the batch leaves the file LF where it
// was CRLF. A take is recorded only in a CRLF file, which is the claim
// takencrlf.go makes and the compiler does not check.
func FuzzATakenCRLFIsRefusedAndOnlyThen(f *testing.F) {
	for _, s := range []struct {
		file, a, b, c, d string
		n1, n2           uint8
	}{
		{"x\r\n\r", "x\r", "\r", "\n", "0", 0, 0},
		{"\xe7\r\n\r", "\xe7\r", "\r", "\n", "0", 0, 0},
		{"x\r\n\ry\r\nz\r\n", "x\r", "\r", "\n\ry", "0\ry", 0, 0},
		{"x\r\nx\r\nq\r", "x\r", "\r", "\n", "0", 1, 1},
		{"x\r\ny\r\nz\r", "x\r", "\r", "\n", "0", 0, 1},
		{"ab\r\n\r", "b\r", "\r", "a\r\n", "0", 0, 0},
		{"a\r\nb\nc\n", "a\r", "A\r", "\r\r\n", "\n", 0, 0},
		{"x\r\n\r", "x\r", "x\r", "\n", "0", 0, 0},
	} {
		f.Add(s.file, s.a, s.b, s.c, s.d, s.n1, s.n2)
	}
	f.Fuzz(func(t *testing.T, file, a, b, c, d string, c1, c2 uint8) {
		if a == "" || c == "" {
			t.Skip()
		}
		n1, n2 := int(c1%3)+1, int(c2%3)+1
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(file), 0o644); err != nil {
			t.Skip()
		}
		tree, err := OpenTree(root, false)
		if err != nil {
			t.Fatal(err)
		}
		defer tree.Close()
		p := &Patch{Hunks: []Hunk{
			{Op: OpReplace, Path: "f.txt", Line: 1, Count: n1, Old: []byte(a), New: []byte(b)},
			{Op: OpReplace, Path: "f.txt", Line: 2, Count: n2, Old: []byte(c), New: []byte(d)},
		}}
		x := NewTxn(tree, Options{})
		if err := x.Load(p); err != nil {
			t.Fatal(err)
		}
		failures := x.Validate(p)

		eol := endingOf(file)
		conv := func(s string) string {
			s = strings.ReplaceAll(s, "\r\n", "\n")
			if eol == "\n" {
				return s
			}
			return strings.ReplaceAll(s, "\n", "\r\n")
		}
		takes := takesAKeptCRLF(file, conv(a), conv(b), n1, conv(c), conv(d), n2)
		for _, fl := range failures {
			if fl.Near == nil || fl.Near.Kind != DiagTakenCRLF {
				continue
			}
			if !takes {
				t.Fatalf("refused %q -> %q x%d, %q -> %q x%d in %q as taking a kept CRLF, which it does not",
					a, b, n1, c, d, n2, file)
			}
			if fl.Hunk != 2 || !strings.HasPrefix(fl.Near.Detail, "hunk 1's old and new end on th") {
				t.Fatalf("refused hunk %d, %q; want hunk 2, naming hunk 1", fl.Hunk, fl.Near.Detail)
			}
		}
		if len(failures) == 0 && takes {
			t.Fatalf("applied %q -> %q x%d, %q -> %q x%d in %q, taking a kept CRLF into a file left LF: %q",
				a, b, n1, c, d, n2, file, x.hunkFile[0].cur)
		}
		if len(x.hunkFile[0].taken) > 0 && eol != "\r\n" {
			t.Fatalf("a take recorded in an LF file: %q -> %q, %q -> %q in %q", a, b, c, d, file)
		}
	})
}

// endingOf is the oracle's own dominant line ending: CRLF when CRLFs
// outnumber the newlines no carriage return comes before.
func endingOf(s string) string {
	if n := strings.Count(s, "\r\n"); n > strings.Count(s, "\n")-n {
		return "\r\n"
	}
	return "\n"
}

// takesAKeptCRLF is the oracle: whether replacing the first n1 copies of a
// with b and then the first n2 copies of c with d, each a changing
// replacement, has a copy of c hold a carriage return a and b both end on
// where file had a newline after it, and the newline after that, and leaves
// the file in the other line ending. The payloads are converted already.
func takesAKeptCRLF(file, a, b string, n1 int, c, d string, n2 int) bool {
	var after []byte
	var mark []bool
	off := 0
	for range n1 {
		j := strings.Index(file[off:], a)
		if j < 0 {
			return false
		}
		s := off + j
		after = append(after, file[off:s]...)
		mark = append(mark, make([]bool, s-off)...)
		m := make([]bool, len(b))
		if e := s + len(a); strings.HasSuffix(a, "\r") && strings.HasSuffix(b, "\r") && e < len(file) && file[e] == '\n' {
			m[len(b)-1] = true
		}
		after = append(after, b...)
		mark = append(mark, m...)
		off = s + len(a)
	}
	after = append(after, file[off:]...)
	mark = append(mark, make([]bool, len(file)-off)...)
	if string(after) == file {
		return false
	}
	var final []byte
	taken := false
	off = 0
	for range n2 {
		j := bytes.Index(after[off:], []byte(c))
		if j < 0 {
			return false
		}
		s := off + j
		for q := s; q+1 < s+len(c); q++ {
			taken = taken || mark[q]
		}
		final = append(final, after[off:s]...)
		final = append(final, d...)
		off = s + len(c)
	}
	final = append(final, after[off:]...)
	if bytes.Equal(final, after) {
		return false
	}
	return taken && endingOf(string(final)) != endingOf(file)
}
