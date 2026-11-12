package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// @@ re's tests (#161). The parse rows are every refusal the parser makes of
// one and the forms it takes; the apply rows run real batches against real
// files, with regexp.ReplaceAll as the oracle where it is one; the report
// rows are goldened, since a refusal's wording is the product; and the fuzz
// target holds the lot to the oracle, the diffstat to the file's own change
// in lines, and --inverse-to to putting the file back.

func TestParseRegex(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		for _, c := range []struct {
			name, src, marker string
			count             int
			old, new          string
		}{
			{"one", "@@ file a\n@@ re\nfoo\n@@ new\nbar\n", "@@", 1, "foo", "bar"},
			{"a count", "@@ file a\n@@ re x3\nfoo\n@@ new\nbar\n", "@@", 3, "foo", "bar"},
			{"an empty new", "@@ file a\n@@ re\nfoo\n@@ new\n", "@@", 1, "foo", ""},
			{"a blank line ends the pattern in a line break", "@@ file a\n@@ re\nfoo\n\n@@ new\nx\n", "@@", 1, "foo\n", "x"},
			{
				"every reference Expand reads, and a $ that starts none",
				"@@ file a\n@@ re\n(?P<name>a)(b)\n@@ new\n$0 $1 ${2} $name ${name} $$HOME $ $) ${1\n",
				"@@", 1, "(?P<name>a)(b)", "$0 $1 ${2} $name ${name} $$HOME $ $) ${1",
			},
			{"under --marker", "%% file a\n%% re x2\n@@ x\n%% new\ny\n", "%%", 2, "@@ x", "y"},
		} {
			t.Run(c.name, func(t *testing.T) {
				p, err := Parse([]byte(c.src), c.marker)
				if err != nil {
					t.Fatalf("Parse: %v", err)
				}
				if len(p.Hunks) != 1 {
					t.Fatalf("%d hunks", len(p.Hunks))
				}
				h := p.Hunks[0]
				if h.Op != OpRegex || h.Path != "a" || h.Line != 2 || h.Count != c.count ||
					string(h.Old) != c.old || string(h.New) != c.new || h.Re == nil || h.Re.String() != c.old {
					t.Errorf("hunk = %+v", h)
				}
			})
		}
	})
	t.Run("refused", func(t *testing.T) {
		for _, c := range []struct {
			name, src string
			line      int
			want      string
		}{
			{"no file in effect", "@@ re\nfoo\n@@ new\nx\n", 1, `no file is in effect; put "@@ file <path>" before this`},
			{
				"a count that is none", "@@ file a\n@@ re x\nfoo\n@@ new\nx\n", 2,
				`"x" is not an occurrence count; write "@@ re" for exactly one occurrence or "@@ re x3" for exactly three`,
			},
			{"a count of zero", "@@ file a\n@@ re x0\nfoo\n@@ new\nx\n", 2, `"@@ re x0" asks for exactly zero occurrences`},
			{"no new", "@@ file a\n@@ re\nfoo\n", 2, `"@@ re" has no "@@ new"; every pattern needs the text to put in place of each match`},
			{
				"an empty pattern", "@@ file a\n@@ re\n@@ new\nx\n", 2,
				`"@@ re" has an empty pattern, which would match at every position in the file rather than at one`,
			},
			{
				"an empty pattern ended by another directive", "@@ file a\n@@ re\n@@ old\nx\n@@ new\ny\n", 2,
				`"@@ re" has an empty pattern, ended by "@@ old" at line 3, a directive`,
			},
			{
				"another directive where new should be", "@@ file a\n@@ re\nfoo\n@@ old\nx\n@@ new\ny\n", 4,
				`expected "@@ new" to close the "@@ re" at line 2, found "@@ old"`,
			},
			{
				"a count on new", "@@ file a\n@@ re\nfoo\n@@ new x2\ny\n", 4,
				`"@@ new" takes no argument; the occurrence count goes on "@@ re"`,
			},
			{
				"a pattern Go cannot read", "@@ file a\n@@ re\nnew(Name\n@@ new\ny\n", 2,
				"the pattern of this \"@@ re\" is not one Go's regexp reads: error parsing regexp: missing closing ): `new(Name`",
			},
			{
				"a group past the pattern's", "@@ file a\n@@ re\n(a)\n@@ new\nx $2\n", 5,
				`$2 in the "@@ new" of the "@@ re" at line 2 names no group of its pattern, which has 1 group, so each match would put nothing there; write $$ for a $ that is text, and ${1}x for group 1 followed by x`,
			},
			{"a shell variable", "@@ file a\n@@ re\necho a\n@@ new\necho $HOME\n", 5, `$HOME in the "@@ new"`},
			{"$1x reads as a group named 1x", "@@ file a\n@@ re\n(a)\n@@ new\n$1x\n", 5, `$1x in the "@@ new"`},
			{"a name the pattern does not have", "@@ file a\n@@ re\n(?P<k>a)\n@@ new\n${v}\n", 5, `${v} in the "@@ new"`},
			{"a leading zero is a name", "@@ file a\n@@ re\n(a)\n@@ new\n$01\n", 5, `$01 in the "@@ new"`},
			{"the line of the reference", "@@ file a\n@@ re\n(a)(b)\n@@ new\n$1\n$2 $3\n", 6, `$3 in the "@@ new" of the "@@ re" at line 2 names no group of its pattern, which has 2 groups`},
			{
				"a line in new that starts like a directive and is none", "@@ file a\n@@ re\na\n@@ new\n@@include x\n", 5,
				`"@@include x" starts like a directive, but "include" is not one, so the line would be payload for a`,
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				_, err := Parse([]byte(c.src), "@@")
				var pe *ParseError
				if !errors.As(err, &pe) {
					t.Fatalf("Parse = %v, want a ParseError", err)
				}
				if pe.Line != c.line || !strings.Contains(pe.Error(), c.want) {
					t.Errorf("line %d: %v\nwant line %d: %s", pe.Line, pe, c.line, c.want)
				}
			})
		}
	})
}

func TestMissingGroup(t *testing.T) {
	re := regexp.MustCompile(`(?P<w>a)(b)`)
	for _, c := range []struct {
		tmpl, ref string
		at        int
	}{
		{"", "", -1},
		{"$0 $1 $2 $w ${w} ${2}", "", -1},
		{"$3", "$3", 0},
		{"x $x", "$x", 2},
		{"$$x", "", -1},
		{"$$$x", "$x", 2},
		{"$01", "$01", 0},
		{"${1", "", -1},
		{"$ $) $", "", -1},
		{"$1x", "$1x", 0},
		{"${1}x", "", -1},
		{"$é", "$é", 0},
		{"$99999999999", "$99999999999", 0},
		{"${w}_${nope}", "${nope}", 5},
	} {
		ref, at, bad := missingGroup(re, []byte(c.tmpl))
		if bad != (c.at >= 0) || ref != c.ref || (bad && at != c.at) {
			t.Errorf("missingGroup(%q) = %q at %d, %v; want %q at %d", c.tmpl, ref, at, bad, c.ref, c.at)
		}
	}
}

// regexHunk is an @@ re patch on a.txt.
func regexHunk(count, pattern, tmpl string) string {
	return "@@ file a.txt\n@@ re" + count + "\n" + pattern + "\n@@ new\n" + tmpl + "\n"
}

func TestRegexApply(t *testing.T) {
	for _, c := range []struct {
		name, file, patch, want string
		added, removed          int
		strict                  bool
		slip                    SlipKind
	}{
		{
			"the issue's example",
			"package a\n\nfunc f() {\n\tx := oldName(a)\n\ty := oldName(bb)\n\treturn oldName(c1)\n}\n",
			regexHunk(" x3", `\boldName\((\w+)\)`, "newName($1, ctx)"),
			"package a\n\nfunc f() {\n\tx := newName(a, ctx)\n\ty := newName(bb, ctx)\n\treturn newName(c1, ctx)\n}\n",
			3, 3, false, -1,
		},
		{"$0 is the match", "a 1 b 22 c 333\n", regexHunk(" x3", `\d+`, "<$0>"), "a <1> b <22> c <333>\n", 1, 1, false, -1},
		{"named groups", "k=v\nx=y\n", regexHunk(" x2", `(?P<k>\w+)=(?P<v>\w+)`, "${v}=${k}"), "v=k\ny=x\n", 2, 2, false, -1},
		{"$$ is a $", "cost 5\n", regexHunk("", `(\d+)`, "$$$1"), "cost $5\n", 1, 1, false, -1},
		{"a $ that starts no reference is text", "a\n", regexHunk("", "a", "$ $) ${1"), "$ $) ${1\n", 1, 1, false, -1},
		{"a group that took no part puts nothing", "b\n", regexHunk("", "(a)?b", "[$1]"), "[]\n", 1, 1, false, -1},
		{"a match of nothing at each line's start", "a\nb\nc", regexHunk(" x3", `(?m)^`, "// "), "// a\n// b\n// c", 3, 3, false, -1},
		{"matches deleted with their lines", "a\n// x\nb\n// y\n", regexHunk(" x2", "// \\w\n", ""), "a\nb\n", 0, 2, false, -1},
		{"a pattern that ends in a line break", "a\nb\nc\n", regexHunk("", "a\nb\n", "ab\n"), "ab\nc\n", 1, 2, false, -1},
		{"under auto a line break typed in the pattern and in new is the file's", "a\r\nb\r\n", regexHunk("", "a\nb", "x\ny"), "x\r\ny\r\n", 2, 2, false, -1},
		{"a group carries the file's own CRLF", "<a\r\nb>\r\n", regexHunk("", `(?s)<(.*)>`, "[$1]"), "[a\r\nb]\r\n", 2, 2, false, -1},
		{"twice, in an LF file, joining nothing", "x\r\ny\nz\n", regexHunk("", `(x\r\n)`, "$1$1"), "x\r\nx\r\ny\nz\n", 1, 0, false, -1},
		{"two groups that copy a CRLF's halves side by side join nothing", "a\r\nb\r\n", regexHunk("", `(a\r)(\nb)`, "$1$2!"), "a\r\nb!\r\n", 1, 1, false, -1},
		{"under strict the pattern is its bytes", "a\nb\n", regexHunk("", "a\nb", "x"), "x\n", 1, 2, true, -1},
		// Two hunks on one line each count it, as two commits would.
		{"after a literal hunk on the same file", "aaa\n", "@@ file a.txt\n@@ old\naaa\n@@ new\nbbb\n" + regexHunk("", "b+", "c"), "c\n", 2, 2, false, -1},
		{"a replacement that ends in a line break where the match did not", "a\nc\n", regexHunk("", "a", "b\n"), "b\n\nc\n", 2, 1, false, SlipEmptyLine},
		{"one that joins the next line on", "a\nc\n", regexHunk("", "a\n", "b"), "bc\n", 1, 2, false, SlipJoinedLine},
		{"an empty one that leaves a line's break", "x\ny\n", regexHunk("", "x", ""), "\ny\n", 1, 1, false, SlipEmptiedLine},
		{"one that changes nothing", "a\n", regexHunk("", "a", "a"), "a\n", 0, 0, false, SlipNoChange},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"a.txt": c.file})
			opt := Options{}
			if c.strict {
				opt.EOL = EOLStrict
			}
			r, err := run(t, tree, c.patch, opt)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := readFile(t, root, "a.txt"); got != c.want {
				t.Errorf("a.txt = %q, want %q", got, c.want)
			}
			if c.slip == SlipNoChange {
				if len(r.Files) != 0 {
					t.Errorf("files = %+v, want none listed", r.Files)
				}
			} else if len(r.Files) != 1 || r.Files[0].Added != c.added || r.Files[0].Removed != c.removed {
				t.Errorf("files = %+v, want +%d -%d", r.Files, c.added, c.removed)
			}
			switch {
			case c.slip < 0 && len(r.Slips) > 0:
				t.Errorf("slips = %+v, want none", r.Slips)
			case c.slip >= 0 && (len(r.Slips) != 1 || r.Slips[0].Kind != c.slip):
				t.Errorf("slips = %+v, want one of kind %d", r.Slips, c.slip)
			}
		})
	}
}

func TestRegexRefusals(t *testing.T) {
	for _, c := range []struct {
		name, file, patch string
		flags             []string
	}{
		{
			"regex-count", "package a\n\nfunc f() {\n\tx := oldName(a)\n\ty := oldName(bb)\n\treturn oldName(c1)\n}\n",
			regexHunk(" x2", `\boldName\((\w+)\)`, "newName($1, ctx)"), nil,
		},
		{"regex-none", "a\n", regexHunk("", "nothingHere", "x"), nil},
		{"regex-more-than-ten", strings.Repeat("ab\n", 12), regexHunk("", "b", "c"), nil},
		{"regex-of-nothing", "a\nb\nc\n", regexHunk(" x2", `(?m)^`, "> "), nil},
		{"regex-runs-on", "a\nb\nc\n", regexHunk(" x2", "(?s)a.*c", "x"), nil},
		{"regex-shifted", "a\nb\n", "@@ file a.txt\n@@ old\na\n@@ new\nA\n" + regexHunk(" x2", "b", "c"), nil},
		{"regex-mixed-endings", "a\nb\nc\na\r\nb\r\n", regexHunk("", "a\nb", "x"), nil},
		{"regex-mixed-endings-crlf", "a\r\nb\r\nc\r\na\nb\n", regexHunk("", "a\nb", "x"), nil},
		{"regex-split-crlf-at-its-newline", "a\r\nb\r\n", regexHunk("", `\nb`, "B"), nil},
		{"regex-split-crlf", "a=1\r\nb=2\r\n", regexHunk("", "a=.*", "a=9"), nil},
		{"regex-joined-crlf", "a\rb\n", regexHunk("", "b", "\nb"), nil},
		{"regex-joined-crlf-template-and-group", "a\nb\n", regexHunk("", `a(\n)b`, "a\r${1}b"), nil},
		{"regex-count-json", "b b\n", regexHunk(" x3", "b", "c"), []string{"--json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": c.file})
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, c.flags, c.patch)
			if code != exitNoMatch {
				t.Fatalf("exit %d, want %d: %s%s", code, exitNoMatch, out, errOut)
			}
			assertUnchanged(t, root, before)
			golden(t, c.name, out+errOut)
		})
	}
}

// A pattern that compiles as written can fail to once --eol auto has made
// its line breaks a CRLF file's: three thousand of them, repeated a thousand
// times, is past RE2's size as CRLFs and not as LFs. The hunk is refused, not
// matched with a pattern other than the one written.
func TestARegexTooLargeAsCRLFIsRefused(t *testing.T) {
	pattern := "(?:" + strings.Repeat("\n", 3000) + "){1000}"
	if _, err := regexp.Compile(pattern); err != nil {
		t.Fatalf("the pattern does not compile as written either: %v", err)
	}
	root := cliTree(t, map[string]string{"a.txt": "a\r\nb\r\n"})
	before := snapshot(t, root)
	code, out, errOut := runCLI(t, root, nil, regexHunk("", pattern, "x"))
	if code != exitNoMatch {
		t.Fatalf("exit %d, want %d: %s%s", code, exitNoMatch, out, errOut)
	}
	if !strings.Contains(errOut, "hunk 1  a.txt  (patch line 2)\n  its pattern, with its line breaks made the file's for --eol auto, "+
		"is not one Go's regexp reads: error parsing regexp: expression too large") ||
		!strings.Contains(errOut, "write the line breaks as \\r?\\n, or pass --eol strict") {
		t.Errorf("stderr = %.400q", errOut)
	}
	assertUnchanged(t, root, before)
}

// A regex hunk is undone by --inverse-to, a match at a time, matches whose
// quotes overlap in one hunk, and the undo put back the file's bytes.
func TestRegexInverseTo(t *testing.T) {
	for _, c := range []struct{ name, file, patch string }{
		{"the issue's example", "x := oldName(a)\ny := oldName(bb)\n", regexHunk(" x2", `\boldName\((\w+)\)`, "newName($1, ctx)")},
		{"matches that grow and shrink on one line", "1 22 333 4444\n", regexHunk(" x4", `\d+`, "<$0$0>") + regexHunk(" x4", `<(\d)\d*>`, "$1")},
		{"lines deleted", "a\n// x\nb\n// y\n", regexHunk(" x2", "// \\w\n", "")},
		{"a CRLF file", "a=1\r\nb=2\r\n", regexHunk(" x2", `(\w)=(\d)`, "$2=$1")},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": c.file})
			undo := filepath.Join(t.TempDir(), "undo.hunk")
			if code, _, errOut := runCLI(t, root, []string{"--inverse-to", undo}, c.patch); code != exitOK {
				t.Fatalf("apply: exit %d: %s", code, errOut)
			}
			if code, _, errOut := runCLI(t, root, []string{"-f", undo}, ""); code != exitOK {
				b, _ := os.ReadFile(undo)
				t.Fatalf("undo: exit %d: %s\n%s", code, errOut, b)
			}
			if got := readFile(t, root, "a.txt"); got != c.file {
				t.Errorf("a.txt = %q after the undo, was %q", got, c.file)
			}
		})
	}
}

func TestInvertRefusesRegex(t *testing.T) {
	code, out, errOut := invertCLI(t, nil, regexHunk("", "a", "b"))
	if code != exitUsage || !strings.Contains(errOut, "a regex replacement is not inverted: what each match was is in the file it was applied to") ||
		!strings.Contains(errOut, "--inverse-to FILE") || !strings.Contains(out, "@@ end refused by hunk invert") {
		t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
}

// FuzzRegexHunk: any pattern that compiles, any template that names only its
// groups, against any file it matches, under either --eol. Under strict it
// applies, and the file is regexp.ReplaceAll's; under auto it applies or is
// refused, never a panic. Applied, the diffstat's added less removed is the
// file's own change in lines, and --inverse-to's undo puts the file back,
// quoting no byte of the file twice, before or after (#176).
func FuzzRegexHunk(f *testing.F) {
	for _, s := range [][3]string{
		{"x := oldName(a)\ny := oldName(bb)\n", `\boldName\((\w+)\)`, "newName($1, ctx)"},
		{"a\nb\nc", `(?m)^`, "// "},
		{"a\r\nb\r\n", "a\nb", "x\ny"},
		{"a=1\r\nb=2\r\n", "a=.*", "a=9"},
		{"<a\r\nb>\r\n", `(?s)<(.*)>`, "[$1][$1]"},
		{"1 22 333\n", `\d*`, "<$0>"},
		{"a\rb\n", "b", "\nb"},
		{"x\n", "x", ""},
		// #176, the nightly's find: an empty match at every byte of a line,
		// each its own hunk quoting the whole line, made a 75 MB inverse that
		// took the fuzz worker past its minute to read back.
		{strings.Repeat("\xab", 3527) + "000000000000\n", "1*", "\x00\x02\x00\x00"},
		// A file the matches empty is made again as it was before the last
		// one, and the quotes after start from that: up to three times.
		{"0217", ".", ""},
	} {
		f.Add(s[0], s[1], s[2], false)
		f.Add(s[0], s[1], s[2], true)
	}
	f.Fuzz(func(t *testing.T, file, pattern, tmpl string, strict bool) {
		re, err := regexp.Compile(pattern)
		if err != nil || pattern == "" {
			return
		}
		if _, _, bad := missingGroup(re, []byte(tmpl)); bad {
			return
		}
		n := len(re.FindAllIndex([]byte(file), -1))
		if n == 0 {
			return
		}
		opt := Options{}
		if strict {
			opt.EOL = EOLStrict
		}
		tree, root := fixture(t, map[string]string{"a.txt": file})
		inv := filepath.Join(t.TempDir(), "undo.hunk")
		x := NewTxn(tree, opt)
		x.inverseTo, x.inverseMarker = inv, DefaultMarker
		h := Hunk{Op: OpRegex, Path: "a.txt", Line: 1, Count: n, Old: []byte(pattern), New: []byte(tmpl), Re: re}
		r, err := x.Run(&Patch{Hunks: []Hunk{h}})
		var ie *InverseError
		var ve *ValidationError
		switch {
		case errors.As(err, &ie):
			return // an undo whose lines read as directives under every marker
		case errors.As(err, &ve) && !strict:
			return // a CRLF or mixed-endings refusal, which only auto makes
		case err != nil:
			t.Fatalf("refused: %v", err)
		}
		now, err := os.ReadFile(filepath.Join(root, "a.txt"))
		must(t, err)
		if want := re.ReplaceAll([]byte(file), []byte(tmpl)); strict && !bytes.Equal(now, want) {
			t.Fatalf("a.txt = %q, ReplaceAll gives %q", now, want)
		}
		if len(r.Files) == 1 {
			if d := r.Files[0].Added - r.Files[0].Removed; d != lineCount(now)-lineCount([]byte(file)) {
				t.Fatalf("+%d -%d, but the file went from %d lines to %d", r.Files[0].Added, r.Files[0].Removed,
					lineCount([]byte(file)), lineCount(now))
			}
		}
		if x.inverse.Nothing {
			return
		}
		src, err := os.ReadFile(inv)
		must(t, err)
		marker, uopt := DefaultMarker, Options{}
		for i, arg := range x.inverse.Args {
			switch arg {
			case "--marker":
				marker = x.inverse.Args[i+1]
			case "strict":
				uopt.EOL = EOLStrict
			}
		}
		p, err := Parse(src, marker)
		if err != nil {
			t.Fatalf("the inverse does not parse: %v\n%q", err, src)
		}
		// No byte of the file, before or after, is quoted twice. A file the
		// matches emptied is made again as it was before the last of them,
		// a part of the file, and the quotes after it quote that once and the
		// file once: three times at most.
		quoted, limit := 0, len(file)+len(now)
		if len(now) == 0 {
			limit = 3 * len(file)
		}
		for _, h := range p.Hunks {
			quoted += len(h.Old) + len(h.New) + len(h.Body)
		}
		if quoted > limit {
			t.Fatalf("the inverse quotes %d bytes of a file of %d and then %d\n%.2000q", quoted, len(file), len(now), src)
		}
		if _, err := NewTxn(tree, uopt).Run(p); err != nil {
			t.Fatalf("the inverse does not apply: %v\n%q", err, src)
		}
		if back, _ := os.ReadFile(filepath.Join(root, "a.txt")); !bytes.Equal(back, []byte(file)) {
			t.Fatalf("a.txt is %q after the undo, was %q\ninverse %q", back, file, src)
		}
	})
}
