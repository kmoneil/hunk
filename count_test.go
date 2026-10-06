package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// replacePatch is a one-hunk patch against f.txt. A payload is the lines
// between two directives joined by \n, so emitting the text followed by a
// newline encodes it exactly, a trailing newline included.
func replacePatch(old, repl string, count int) string {
	x := ""
	if count > 0 {
		x = " x" + itoa(count)
	}
	return "@@ file f.txt\n@@ old" + x + "\n" + old + "\n@@ new\n" + repl + "\n"
}

// Two files with one copy of a two-line old in each line ending: CRLF the
// majority in the first, LF in the second. The copies are on lines 3 and 6.
const (
	crlfMajority = "a\r\nb\r\nkey = 1\r\nnext\r\nc\r\nkey = 1\nnext\nd\r\n"
	lfMajority   = "a\nb\nkey = 1\nnext\nc\nkey = 1\r\nnext\r\nd\n"
)

// A bare @@ old, or one with xN, whose count cannot see every copy of its text
// is refused with exit 2, and the copies it can see are left alone. Until
// 2026-10-06 the count was left to right and non-overlapping, and under --eol
// auto it saw only the dominant line ending, and the hunk applied at exit 0 to
// whichever copy the count happened to see. The rows around the refusals are
// the shapes that must still apply.
func TestACountThatMissesACopyIsRefused(t *testing.T) {
	for _, c := range []struct {
		name    string
		file    string
		old     string
		count   int // 0 is a bare @@ old
		repl    string
		strict  bool
		want    string // the file afterwards; "" means refused and untouched
		kind    DiagKind
		found   int
		lines   []int
		endings []string
		more    int
	}{
		// Overlap.
		{name: "a bare old over aaa", file: "aaa", old: "aa", kind: DiagOverlap, found: 2, lines: []int{1, 1}},
		{name: "x2 over aaa", file: "aaa", old: "aa", count: 2, kind: DiagOverlap, found: 2, lines: []int{1, 1}},
		{
			name: "two identical lines in a run of three", file: "x\nx\nx\n", old: "x\nx\n",
			kind: DiagOverlap, found: 2, lines: []int{1, 2},
		},
		{
			name: "the same with x2", file: "x\nx\nx\n", old: "x\nx\n", count: 2,
			kind: DiagOverlap, found: 2, lines: []int{1, 2},
		},
		{
			name: "x2 that a non-overlapping count would satisfy", file: "x\nx\nx\nx\n", old: "x\nx\n", count: 2,
			kind: DiagOverlap, found: 3, lines: []int{1, 2, 3},
		},
		{
			name: "past the listing cap", file: strings.Repeat("x\n", 13), old: "x\nx\n",
			kind: DiagOverlap, found: 12, lines: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, more: 2,
		},
		{name: "strict refuses an overlap too", file: "aaa", old: "aa", strict: true, kind: DiagOverlap, found: 2, lines: []int{1, 1}},
		{name: "copies side by side do not overlap", file: "aXa", old: "a", count: 2, repl: "b", want: "bXb"},
		{name: "repeated two-line copies that do not overlap", file: "x\ny\nx\ny\n", old: "x\ny\n", count: 2, repl: "z\n", want: "z\nz\n"},
		{name: "an old that could overlap itself, once", file: "abab", old: "aba", repl: "Q", want: "Qb"},

		// Line endings, under --eol auto.
		{
			name: "CRLF majority, a copy in each ending", file: crlfMajority, old: "key = 1\nnext",
			kind: DiagMixedEndings, found: 2, lines: []int{3, 6}, endings: []string{"CRLF", "LF"},
		},
		{
			name: "LF majority, a copy in each ending", file: lfMajority, old: "key = 1\nnext",
			kind: DiagMixedEndings, found: 2, lines: []int{3, 6}, endings: []string{"LF", "CRLF"},
		},
		{
			name: "x2 across both endings", file: crlfMajority, old: "key = 1\nnext", count: 2,
			kind: DiagMixedEndings, found: 2, lines: []int{3, 6}, endings: []string{"CRLF", "LF"},
		},
		{
			name: "a copy whose own breaks are mixed", file: "p\r\nq\r\nr\r\nx\r\np\r\nq\nr\r\ny\r\n", old: "p\nq\nr",
			kind: DiagMixedEndings, found: 2, lines: []int{1, 5}, endings: []string{"CRLF", "mixed"},
		},
		{
			name: "strict matches bytes as given and is unchanged", file: crlfMajority, old: "key = 1\nnext", strict: true, repl: "K\nN",
			want: "a\r\nb\r\nkey = 1\r\nnext\r\nc\r\nK\nN\nd\r\n",
		},
		{
			name: "a multi-line old in a mixed file, only in its dominant ending", file: crlfMajority, old: "b\nkey = 1",
			repl: "B\nK", want: "a\r\nB\r\nK\r\nnext\r\nc\r\nkey = 1\nnext\nd\r\n",
		},
		{
			name: "a one-line old in a mixed file, once", file: crlfMajority, old: "c", repl: "C",
			want: "a\r\nb\r\nkey = 1\r\nnext\r\nC\r\nkey = 1\nnext\nd\r\n",
		},
		{
			name: "a one-line old in both endings is counted in both, as before", file: crlfMajority, old: "key = 1",
			kind: DiagTooMany, found: 2, lines: []int{3, 6},
		},
		{
			name: "and x2 replaces both", file: crlfMajority, old: "key = 1", count: 2, repl: "K",
			want: "a\r\nb\r\nK\r\nnext\r\nc\r\nK\nnext\nd\r\n",
		},
		{
			name: "a pure CRLF file is unchanged", file: "a\r\nkey = 1\r\nnext\r\nb\r\n", old: "key = 1\nnext", repl: "K\nN",
			want: "a\r\nK\r\nN\r\nb\r\n",
		},
		{
			name: "a copy only in the minority ending keeps its diagnosis", file: "a\r\nb\r\nc\r\nkey = 1\nnext\nd\r\n", old: "key = 1\nnext",
			kind: DiagNamed, found: 0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"f.txt": c.file})
			before := snapshot(t, root)
			repl := c.repl
			if repl == "" {
				repl = "Y" // refused rows: the replacement never lands
			}
			opt := Options{}
			if c.strict {
				opt.EOL = EOLStrict
			}
			_, err := run(t, tree, replacePatch(c.old, repl, c.count), opt)
			if c.want != "" {
				if err != nil {
					t.Fatalf("refused, want it applied: %v", err)
				}
				if got := readFile(t, root, "f.txt"); got != c.want {
					t.Errorf("file = %q, want %q", got, c.want)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("got %T %v, want a validation failure", err, err)
			}
			f := ve.Failures[0]
			expected := max(c.count, 1)
			if f.Hunk != 1 || f.Expected != expected || f.Found != c.found {
				t.Errorf("hunk %d, expected %d, found %d; want hunk 1, expected %d, found %d",
					f.Hunk, f.Expected, f.Found, expected, c.found)
			}
			if f.Near == nil || f.Near.Kind != c.kind {
				t.Fatalf("near = %+v, want kind %v", f.Near, c.kind)
			}
			if c.kind == DiagNamed && f.Near.Cause != "line endings" {
				t.Errorf("cause = %q, want the existing line endings diagnosis", f.Near.Cause)
			}
			if c.kind != DiagNamed {
				if !slices.Equal(f.Near.Lines, c.lines) || !slices.Equal(f.Near.Endings, c.endings) || f.Near.MoreLines != c.more {
					t.Errorf("lines %v endings %v more %d; want %v %v %d",
						f.Near.Lines, f.Near.Endings, f.Near.MoreLines, c.lines, c.endings, c.more)
				}
			}
			if c.kind == DiagOverlap || c.kind == DiagMixedEndings {
				r := f.Near.Render("  ")
				if !strings.Contains(r, "at lines "+itoa(c.lines[0])) || strings.Contains(r, "or say") {
					t.Errorf("render:\n%s", r)
				}
				if c.more > 0 && !strings.Contains(r, ", and "+itoa(c.more)+" more") {
					t.Errorf("render does not say how many more:\n%s", r)
				}
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The refusals as an agent meets them: exit 2, the hunk and its patch line,
// the true count and every copy's line, the remedy, and no suggestion of an
// xN, which for these copies is either a guess or cannot apply.
func TestTheRefusalOfAMissedCopyThroughTheCLI(t *testing.T) {
	for _, c := range []struct {
		name, file, old string
		text            []string
		cause           string
		lines           []int
		endings         []string
	}{
		{
			name: "overlap", file: "x\nx\nx\n", old: "x\nx\n",
			text: []string{
				"hunk 1  f.txt  (patch line 2)",
				"expected 1 occurrence, found 2 that overlap\n  at lines 1, 2\n",
				"overlapping copies share bytes, so one cannot be replaced without the other",
			},
			cause: "overlap", lines: []int{1, 2},
		},
		{
			name: "mixed line endings", file: crlfMajority, old: "key = 1\nnext",
			text: []string{
				"hunk 1  f.txt  (patch line 2)",
				"expected 1 occurrence, found 2 across both line endings\n  at lines 3 (CRLF), 6 (LF)\n",
				"--eol auto counted only the copies in its dominant one",
			},
			cause: "mixed line endings", lines: []int{3, 6}, endings: []string{"CRLF", "LF"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": c.file})
			patch := replacePatch(c.old, "Y", 0)
			code, out, errOut := runCLI(t, root, nil, patch)
			if code != exitNoMatch || out != "" {
				t.Fatalf("exit %d, stdout %q; want %d and the report on stderr", code, out, exitNoMatch)
			}
			for _, s := range c.text {
				if !strings.Contains(errOut, s) {
					t.Errorf("report does not say %q:\n%s", s, errOut)
				}
			}
			if strings.Contains(errOut, "or say") || strings.Contains(errOut, "x2") {
				t.Errorf("report suggests a count:\n%s", errOut)
			}
			if got := readFile(t, root, "f.txt"); got != c.file {
				t.Errorf("the file changed: %q", got)
			}

			code, out, _ = runCLI(t, root, []string{"--json"}, patch)
			var r struct {
				Exit     int `json:"exit"`
				Failures []struct {
					Found       *int     `json:"found"`
					Lines       []int    `json:"lines"`
					LineEndings []string `json:"line_endings"`
					NearMiss    struct {
						Cause  string `json:"cause"`
						Detail string `json:"detail"`
					} `json:"near_miss"`
				} `json:"failures"`
			}
			must(t, json.Unmarshal([]byte(out), &r))
			f := r.Failures[0]
			if code != exitNoMatch || r.Exit != exitNoMatch || f.Found == nil || *f.Found != 2 ||
				!slices.Equal(f.Lines, c.lines) || !slices.Equal(f.LineEndings, c.endings) ||
				f.NearMiss.Cause != c.cause || f.NearMiss.Detail == "" {
				t.Errorf("json: exit %d, %s", code, out)
			}
		})
	}
}

// copiesPast is on every replace hunk's path, so its cost is the claim: at most
// n+1 searches however the copies overlap. A scan that forgot to stop
// allocates nothing more and answers the same, so the bytes the linearity
// guard measures cannot see it, and this counts the searches instead.
func TestCopiesPastStopsAtTheFirstCopyPastN(t *testing.T) {
	for _, c := range []struct {
		name     string
		text     string
		pat      string
		n        int
		past     bool
		searches int
	}{
		{"a run of one byte, the overlapping worst case", strings.Repeat("a", 100000), strings.Repeat("a", 10), 10000, true, 10001},
		{"copies that do not overlap", strings.Repeat("ab", 1000), "ab", 1000, false, 1001},
		{"fewer copies than n", "abc", "b", 3, false, 2},
		{"no copy", "abc", "z", 1, false, 1},
		{"an empty pattern", "abc", "", 1, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			past, searches := countCopies([]byte(c.text), []byte(c.pat), c.n)
			if past != c.past || searches != c.searches {
				t.Errorf("past %v after %d searches, want %v after %d", past, searches, c.past, c.searches)
			}
			if past != copiesPast([]byte(c.text), []byte(c.pat), c.n) {
				t.Error("copiesPast disagrees with countCopies")
			}
		})
	}
}

// matchStarts is held to the obvious answer, every offset tried.
func TestMatchStartsCountsEveryCopy(t *testing.T) {
	for _, c := range []struct {
		text, pat string
		n         int
		starts    []int
	}{
		{"aaa", "aa", 2, []int{0, 1}},
		{"abab", "aba", 1, []int{0}},
		{"abababa", "aba", 3, []int{0, 2, 4}},
		{"aabaabaa", "aabaa", 2, []int{0, 3}},
		{"", "a", 0, nil},
		{"a", "", 0, nil},
		{"abc", "abcd", 0, nil},
	} {
		n, starts := matchStarts([]byte(c.text), []byte(c.pat), tooManyLimit)
		if n != c.n || !slices.Equal(starts, c.starts) {
			t.Errorf("matchStarts(%q, %q) = %d %v, want %d %v", c.text, c.pat, n, starts, c.n, c.starts)
		}
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 2000 {
		text := randomOver(rng, "ab\n", rng.IntN(40))
		pat := randomOver(rng, "ab\n", 1+rng.IntN(4))
		n, starts := matchStarts([]byte(text), []byte(pat), 3)
		var want []int
		for i := 0; i+len(pat) <= len(text); i++ {
			if text[i:i+len(pat)] == pat {
				want = append(want, i)
			}
		}
		if n != len(want) || !slices.Equal(starts, want[:min(3, len(want))]) {
			t.Fatalf("matchStarts(%q, %q) = %d %v, want %d %v", text, pat, n, starts, len(want), want)
		}
	}
}

func randomOver(rng *rand.Rand, alpha string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alpha[rng.IntN(len(alpha))]
	}
	return string(b)
}

// The walk that labels each copy keeps the file and its folded copy in step.
// A lone CR just before a CRLF is the shape most likely to throw it out: the
// fold turns "\r\r\n" into "\r\n", and a walk that took the first CR for the
// CRLF would read the next copy one byte early.
func TestDiagnoseMixedEndingsKeepsTheFileAndTheFoldInStep(t *testing.T) {
	d := DiagnoseMixedEndings([]byte("k\nv"), []byte("z\r\r\nk\nv\r\nk\r\nv\r\n"))
	if d.Found != 2 || !slices.Equal(d.Lines, []int{2, 4}) || !slices.Equal(d.Endings, []string{"LF", "CRLF"}) {
		t.Errorf("found %d at %v with %v, want 2 at [2 4] with [LF CRLF]", d.Found, d.Lines, d.Endings)
	}
}

func TestMixedEndings(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"a\r\nb\n", true},
		{"a\r\nb\r\n", false},
		{"a\nb\n", false},
		{"", false},
		{"a\rb\n", false},
	} {
		if got := mixedEndings([]byte(c.in)); got != c.want {
			t.Errorf("mixedEndings(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// hasLoneCR reports a CR not followed by LF, which neither line ending has.
// The rule is about CRLF against LF, and a lone CR makes folding CRLF to LF
// say different things about one file depending on where the fold starts.
func hasLoneCR(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' && (i+1 == len(s) || s[i+1] != '\n') {
			return true
		}
	}
	return false
}

// The counted copies are exactly the copies replaced. For any file and old,
// either the hunk is refused, or an exhaustive scan finds exactly as many
// copies as the hunk claimed, overlapping ones and (under --eol auto) ones in
// either line ending included, and the file is those copies replaced. And a
// refusal for a missed copy is one an exhaustive scan agrees with. The scans
// here try every offset and fold the endings themselves; they share nothing
// with the code under test.
func FuzzTheCountedCopiesAreTheReplacedOnes(f *testing.F) {
	for _, s := range []struct {
		file, old string
		count     uint8
		strict    bool
	}{
		{"aaa", "aa", 0, false},
		{"x\nx\nx\n", "x\nx\n", 1, false},
		{crlfMajority, "key = 1\nnext", 0, false},
		{lfMajority, "key = 1\nnext", 1, false},
		{crlfMajority, "key = 1\nnext", 0, true},
		{"p\r\nq\r\nr\r\nx\r\np\r\nq\nr\r\ny\r\n", "p\nq\nr", 0, false},
		{"aXa", "a", 1, false},
		{"one\r\ntwo\n", "one\ntwo", 0, false},
	} {
		f.Add(s.file, s.old, "R\n", s.count, s.strict)
	}
	f.Fuzz(func(t *testing.T, file, old, repl string, count uint8, strict bool) {
		if old == "" || hasLoneCR(file) || hasLoneCR(old) || hasLoneCR(repl) {
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
		opt := Options{}
		if strict {
			opt.EOL = EOLStrict
		}
		p := &Patch{Hunks: []Hunk{{Op: OpReplace, Path: "f.txt", Line: 1, Count: n, Old: []byte(old), New: []byte(repl)}}}
		x := NewTxn(tree, opt)
		if err := x.Load(p); err != nil {
			t.Fatal(err)
		}
		failures := x.Validate(p)

		eol := "\n"
		if !strict && strings.Count(file, "\r\n") > strings.Count(file, "\n")-strings.Count(file, "\r\n") {
			eol = "\r\n"
		}
		fold := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
		conv := func(s string) string {
			if strict {
				return s
			}
			if eol == "\n" {
				return fold(s)
			}
			return strings.ReplaceAll(fold(s), "\n", "\r\n")
		}
		plain := strings.Count(file, conv(old))
		every := overlapping(file, conv(old))
		if !strict {
			every = overlapping(fold(file), fold(old))
		}

		if len(failures) == 0 {
			if every != n {
				t.Fatalf("applied x%d to %q in %q, where every copy counted is %d", n, old, file, every)
			}
			got := x.hunkFile[0].cur
			if want := strings.Replace(file, conv(old), conv(repl), n); string(got) != want {
				t.Fatalf("applied %q -> %q in %q: got %q, want %q", old, repl, file, got, want)
			}
			return
		}
		d := failures[0].Near
		switch {
		case d == nil:
			t.Fatalf("a single failing hunk has no diagnosis: %+v", failures[0])
		case d.Kind == DiagOverlap:
			if overlapping(file, conv(old)) <= plain {
				t.Fatalf("refused %q in %q as an overlap, but no copies overlap", old, file)
			}
		case d.Kind == DiagMixedEndings:
			if strict || every <= plain {
				t.Fatalf("refused %q in %q for its endings, but every copy was counted (%d of %d)", old, file, plain, every)
			}
		default:
			if plain == n {
				t.Fatalf("refused %q in %q as %v with the count it claimed", old, file, d.Kind)
			}
		}
		if failures[0].Found != plain && d.Kind != DiagOverlap && d.Kind != DiagMixedEndings {
			t.Fatalf("found %d, but the count is %d", failures[0].Found, plain)
		}
		if b, _ := os.ReadFile(filepath.Join(root, "f.txt")); !bytes.Equal(b, []byte(file)) {
			t.Fatal("validation wrote the file")
		}
	})
}
