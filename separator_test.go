package main

// A directive missing its space, "@@file b.go", or with a space that is not
// ASCII for one, is refused wherever it is (#96). As payload it was written
// into the file before it, and the hunks after it applied to that file too.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The rule, line by line: what it takes, and every near thing it leaves.
func TestSeparatorFault(t *testing.T) {
	const nb, ideo = "\u00a0", "\u3000"
	for _, c := range []struct {
		name, marker, line string
		ok                 bool
		word               string
		missing            bool
		space              rune
	}{
		{"file", "", "@@file b.go", true, "file", true, 0},
		{"old", "", "@@old", true, "old", true, 0},
		{"old with a count", "", "@@old x2", true, "old", true, 0},
		{"new", "", "@@new", true, "new", true, 0},
		{"create", "", "@@create c.go", true, "create", true, 0},
		{"delete", "", "@@delete c.go", true, "delete", true, 0},
		{"append", "", "@@append c.go", true, "append", true, 0},
		{"prepend", "", "@@prepend c.go", true, "prepend", true, 0},
		{"end", "", "@@end", true, "end", true, 0},
		{"a tab after the word", "", "@@file\tb.go", true, "file", true, 0},
		{"one CR is the line's ending", "", "@@end\r", true, "end", true, 0},
		{"under another marker", "%%", "%%file b.go", true, "file", true, 0},
		{"under a marker of one character", "#", "#end", true, "end", true, 0},

		{"a no-break space after the marker", "", "@@" + nb + "file b.go", true, "file", false, 0xa0},
		{"an ideographic space", "", "@@" + ideo + "file b.go", true, "file", false, 0x3000},
		{"an em space", "", "@@\u2003end", true, "end", false, 0x2003},
		{"a narrow no-break space", "", "@@\u202fend", true, "end", false, 0x202f},
		{"a line separator", "", "@@\u2028end", true, "end", false, 0x2028},
		{"after an ASCII space", "", "@@ " + nb + "file b", true, "file", false, 0xa0},
		{"before one", "", "@@" + nb + " file b", true, "file", false, 0xa0},
		{"between the word and its argument", "", "@@ old" + nb + "x2", true, "old", false, 0xa0},
		{"after @@ end", "", "@@ end" + nb, true, "end", false, 0xa0},
		{"after @@ end, then a CR", "", "@@ end" + nb + "\r", true, "end", false, 0xa0},
		{"the first of two is the one named", "", "@@" + ideo + "end" + nb, true, "end", false, 0x3000},
		{"glued, with one after the word", "", "@@file" + nb + "b.go", true, "file", true, 0xa0},
		{"glued, with one in the path, which is no separator", "", "@@file b" + nb + "c", true, "file", true, 0},
		{"one after the marker and one in the path", "", "@@" + nb + "file b" + nb + "c", true, "file", false, 0xa0},

		{"a directive", "", "@@ file b.go", false, "", false, 0},
		{"a directive with a tab", "", "@@\tfile b.go", false, "", false, 0},
		{"a directive with a run of spaces", "", "@@   end", false, "", false, 0},
		{"a directive with a no-break space in its path", "", "@@ file b" + nb + "c", false, "", false, 0},
		{"a directive whose path ends in one", "", "@@ file b" + nb, false, "", false, 0},
		{"an empty line", "", "", false, "", false, 0},
		{"the marker alone", "", "@@", false, "", false, 0},
		{"the marker and a no-break space", "", "@@" + nb, false, "", false, 0},
		{"glued to a longer word", "", "@@files b.go", false, "", false, 0},
		{"glued to a word and a name", "", "@@fileb.go", false, "", false, 0},
		{"glued to a word and a colon", "", "@@file:", false, "", false, 0},
		{"@@@@", "", "@@@@", false, "", false, 0},
		{"three @", "", "@@@ file b.go", false, "", false, 0},
		{"one @", "", "@ file b.go", false, "", false, 0},
		{"indented", "", " @@ file b.go", false, "", false, 0},
		{"indented and glued", "", " @@file b.go", false, "", false, 0},
		{"a capital", "", "@@ File b.go", false, "", false, 0},
		{"glued, with a capital", "", "@@File b.go", false, "", false, 0},
		{"a misspelt word", "", "@@ fiel b.go", false, "", false, 0},
		{"a unified diff's hunk header", "", "@@ -1,3 +1,4 @@", false, "", false, 0},
		{"a combined diff's", "", "@@@ -1,2 -1,2 +1,3 @@@", false, "", false, 0},
		{"a Ruby class variable", "", "@@count = 0", false, "", false, 0},
		{"a zero-width space is not a space", "", "@@\u200bfile b.go", false, "", false, 0},
		{"nor is a byte-order mark", "", "@@\ufefffile b.go", false, "", false, 0},
		{"NEL is a C1 control, nearDirective's", "", "@@\u0085file b.go", false, "", false, 0},
		{"a form feed is a control, nearDirective's", "", "@@\ffile b.go", false, "", false, 0},
		{"a lone 0xA0 is a byte, not a no-break space", "", "@@\xa0file b.go", false, "", false, 0},
		{"a second CR is a byte of the line, so the word is not one", "", "@@end\r\r", false, "", false, 0},
		{"another marker's line under the default", "", "%%file b.go", false, "", false, 0},
		{"the default's line under another marker", "%%", "@@file b.go", false, "", false, 0},
		{"HUNK", "", "HUNK", false, "", false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := c.marker
			if marker == "" {
				marker = DefaultMarker
			}
			bad, ok := separatorFault([]byte(c.line), marker)
			want := badSeparator{word: c.word, missing: c.missing, space: c.space}
			if ok != c.ok || bad != want {
				t.Errorf("separatorFault(%q) = %+v, %v; want %+v, %v", c.line, bad, ok, want, c.ok)
			}
		})
	}
}

// The three sentences, whole.
func TestSeparatorMessage(t *testing.T) {
	for _, c := range []struct {
		name, marker string
		bad          badSeparator
		want         string
	}{
		{
			"missing", "@@",
			badSeparator{word: "file", missing: true},
			`the line would be the directive "@@ file" but for the space missing after "@@"; put one in, or if the line is payload text, choose another directive prefix with --marker`,
		},
		{
			"not ASCII", "@@",
			badSeparator{word: "end", space: 0xa0},
			`the line would be the directive "@@ end" but for U+00A0 in it, a space that is not ASCII; make it an ASCII space, or if the line is payload text, choose another directive prefix with --marker`,
		},
		{
			"both", "%%",
			badSeparator{word: "old", missing: true, space: 0x3000},
			`the line would be the directive "%% old" but for U+3000, a space that is not ASCII, and the space missing after "%%"; make that an ASCII space and put one after "%%", or if the line is payload text, choose another directive prefix with --marker`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := separatorMessage(c.bad, c.marker); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

// separatorSpaced is the rule restated over the whole line, as an oracle: the
// line is not a directive, and would be one with every space that is not
// ASCII made a space and a space put after the marker where none is.
// separatorFault reads only the separators; the two agree because a path is
// no part of whether a line is a directive. directive takes off the one CR
// that ends a line, and nothing here takes off another: fuzzing found this
// oracle doing so, "file" and two CRs under an empty marker.
func separatorSpaced(line, marker string) (string, bool) {
	l := line
	if _, _, ok := directive([]byte(l), marker); ok || !strings.HasPrefix(strings.TrimSuffix(l, "\r"), marker) {
		return "", false
	}
	var b strings.Builder
	rest := l[len(marker):]
	for i := 0; i < len(rest); {
		r, w := utf8.DecodeRuneInString(rest[i:])
		if r >= utf8.RuneSelf && unicode.IsSpace(r) && !isControl(r) {
			b.WriteByte(' ')
		} else {
			b.WriteString(rest[i : i+w])
		}
		i += w
	}
	spaced := b.String()
	if spaced != "" && spaced[0] != ' ' && spaced[0] != '\t' {
		spaced = " " + spaced
	}
	word, _, ok := directive([]byte(marker+spaced), marker)
	return word, ok
}

func FuzzSeparatorFault(f *testing.F) {
	for _, s := range []string{
		"@@file b.go", "@@old x2", "@@end\r", "@@\u00a0file b", "@@ end\u00a0", "@@file\u00a0b",
		"@@file b\u00a0c", "@@files", "@@ file b", "@@", "@@\u0085end", "@@\xa0end", "@@\u2028old",
	} {
		f.Add(s, DefaultMarker)
	}
	f.Add("%%file b", "%%")
	f.Add("file b", "")
	f.Fuzz(func(t *testing.T, line, marker string) {
		bad, ok := separatorFault([]byte(line), marker)
		word, want := separatorSpaced(line, marker)
		if ok != want || bad.word != word {
			t.Fatalf("separatorFault(%q, %q) = %+v, %v; the line spaced out is %q, %v", line, marker, bad, ok, word, want)
		}
		if !ok {
			if bad != (badSeparator{}) {
				t.Errorf("refused nothing, but said %+v", bad)
			}
			return
		}
		if !bad.missing && bad.space == 0 {
			t.Errorf("%q: taken with neither fault", line)
		}
		// The two rules never take the same line, so which the parser asks
		// first is not a choice that shows: a control character is no
		// separator here, and nearDirective's reading leaves every other
		// byte as it is.
		if word, near := nearDirective([]byte(strings.TrimSuffix(line, "\r")), marker); near {
			t.Errorf("%q: nearDirective takes it too, as %q", line, word)
		}
		if bad.space != 0 && (!strings.ContainsRune(line, bad.space) || !unicode.IsSpace(bad.space) || bad.space < utf8.RuneSelf) {
			t.Errorf("%q: names %U, which is not a space in it that is not ASCII", line, bad.space)
		}
		if rest := line[len(marker):]; bad.missing && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			t.Errorf("%q: says the space is missing after %q, and it is not", line, marker)
		}
	})
}

const (
	sepA = "package a\n\nimport \"fmt\"\n\nfunc A() { fmt.Println(\"a\") }\n"
	sepB = "package b\n\nimport \"fmt\"\n\nfunc B() { fmt.Println(\"b\") }\n"
)

// Through the CLI: exit 1, the line named, nothing read or written, whatever
// the flags, and the same patch with the space put in does what it was meant
// to. The first three rows are the recon's: the issue's repro, a hunk after
// the line applied to the file before it at exit 0, and one that missed there
// and was reported against that file.
func TestADirectiveMissingItsSpaceIsRefused(t *testing.T) {
	for _, c := range []struct {
		name      string
		patch     string
		args      []string
		file      bool // from -f rather than stdin
		bad, good string
		line      string
		directive string
		goodCode  int
	}{
		{
			name:  "the issue's repro",
			patch: "@@ file a.go\n@@ old\npackage a\n@@ new\npackage aa\n@@file b.go\n@@ old\npackage b\n@@ new\npackage bb\n",
			bad:   "@@file b.go", good: "@@ file b.go", line: "6", directive: "@@ file",
		},
		{
			name:  "a hunk after it that the file before it also holds",
			patch: "@@ file a.go\n@@ old\npackage a\n@@ new\npackage aa\n@@file b.go\n@@ old\nimport \"fmt\"\n@@ new\nimport \"os\"\n",
			bad:   "@@file b.go", good: "@@ file b.go", line: "6", directive: "@@ file",
		},
		{
			name:  "one it does not",
			patch: "@@ file a.go\n@@ old\npackage a\n@@ new\npackage aa\n@@file b.go\n@@ old\nfunc B() { fmt.Println(\"b\") }\n@@ new\nfunc B() {}\n",
			bad:   "@@file b.go", good: "@@ file b.go", line: "6", directive: "@@ file",
		},
		{
			name:  "at line 1, with no heredoc hint",
			patch: "@@file a.go\n@@ old\npackage a\n@@ new\npackage aa\n",
			bad:   "@@file a.go", good: "@@ file a.go", line: "1", directive: "@@ file",
		},
		{
			name:  "from -f",
			patch: "@@ create c.go\npackage c\n@@end\n",
			file:  true, bad: "@@end", good: "@@ end", line: "3", directive: "@@ end",
		},
		{
			name:  "a create that never happened",
			patch: "@@ file a.go\n@@ old\npackage a\n@@ new\npackage aa\n@@create c.go\npackage c\n\n@@ file b.go\n@@ old\npackage b\n@@ new\npackage bb\n",
			bad:   "@@create c.go", good: "@@ create c.go", line: "6", directive: "@@ create",
		},
		{
			name:  "under --dry-run",
			patch: "@@ create c.go\nx\n@@delete b.go\n",
			args:  []string{"--dry-run"}, bad: "@@delete b.go", good: "@@ delete b.go", line: "3", directive: "@@ delete",
		},
		{
			name:  "under --verify, which never runs",
			patch: "@@ append a.go\n// x\n@@prepend b.go\n// y\n",
			args:  []string{"--verify", "touch ran"}, bad: "@@prepend", good: "@@ prepend", line: "3", directive: "@@ prepend",
		},
		{
			name:  "under --try, which never runs",
			patch: "@@ prepend a.go\n// x\n@@append b.go\n// y\n",
			args:  []string{"--try", "touch ran"}, bad: "@@append", good: "@@ append", line: "3", directive: "@@ append",
		},
		{
			name:  "under another marker, that marker's",
			patch: "%% create c.go\n@@file b.go\n%%end\n",
			args:  []string{"--marker", "%%"}, bad: "%%end", good: "%% end", line: "3", directive: "%% end",
		},
		{
			name:  "a no-break space for the separator",
			patch: "@@ file a.go\n@@ old\npackage a\n@@ new\npackage aa\n@@\u00a0file b.go\n@@ old\npackage b\n@@ new\npackage bb\n",
			bad:   "@@\u00a0file", good: "@@ file", line: "6", directive: "@@ file",
		},
		{
			name:  "a no-break space after the word",
			patch: "@@ file a.go\n@@ old\npackage a\n@@ new\npackage aa\n@@ old\u00a0x2\nfmt\n@@ new\nfmt2\n",
			bad:   "@@ old\u00a0x2", good: "@@ old x2", line: "6", directive: "@@ old",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := func(patch string) (code int, out, errOut, root string, before map[string]string) {
				root = cliTree(t, map[string]string{"a.go": sepA, "b.go": sepB})
				before = snapshot(t, root)
				args, stdin := c.args, patch
				if c.file {
					p := filepath.Join(t.TempDir(), "p.hunk")
					must(t, os.WriteFile(p, []byte(patch), 0o644))
					args, stdin = append([]string{"-f", p}, args...), ""
				}
				code, out, errOut = runCLI(t, root, args, stdin)
				return code, out, errOut, root, before
			}

			code, out, errOut, root, before := run(c.patch)
			if code != exitUsage {
				t.Errorf("exit %d, want %d: %s%s", code, exitUsage, out, errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			want := "hunk: patch line " + c.line + ": the line would be the directive \"" + c.directive + "\" but for "
			if !strings.HasPrefix(errOut, want) || strings.Count(errOut, "\n") != 1 {
				t.Errorf("stderr = %q, want one line starting %q", errOut, want)
			}
			if strings.Contains(errOut, heredocHintText) {
				t.Errorf("a glued directive is a patch, not another heredoc's text, and got the hint: %q", errOut)
			}
			assertUnchanged(t, root, before)

			// The control: with the space put in, the patch is what was meant.
			if !strings.Contains(c.patch, c.bad) {
				t.Fatalf("the patch does not hold %q", c.bad)
			}
			code, out, errOut, _, _ = run(strings.Replace(c.patch, c.bad, c.good, 1))
			if code != c.goodCode {
				t.Errorf("with the space put in: exit %d, want %d: %s%s", code, c.goodCode, out, errOut)
			}
		})
	}
}

// The wrong-file edit the recon found, read back from the tree: with the
// space put in, b.go is edited and a.go is not; without it, neither.
func TestTheHunksAfterAGluedDirectiveDoNotLandOnTheFileBeforeIt(t *testing.T) {
	const patch = "@@ file a.go\n@@ old\nfunc A() { fmt.Println(\"a\") }\n@@ new\nfunc A() { fmt.Println(\"A\") }\n" +
		"@@file b.go\n@@ old\nimport \"fmt\"\n@@ new\nimport \"os\"\n"
	root := cliTree(t, map[string]string{"a.go": sepA, "b.go": sepB})
	if code, out, errOut := runCLI(t, root, nil, patch); code != exitUsage {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	for name, want := range map[string]string{"a.go": sepA, "b.go": sepB} {
		if b, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want it untouched", name, b, err)
		}
	}
	if code, out, errOut := runCLI(t, root, nil, strings.Replace(patch, "@@file", "@@ file", 1)); code != exitOK {
		t.Fatalf("with the space: exit %d: %s%s", code, out, errOut)
	}
	a, _ := os.ReadFile(filepath.Join(root, "a.go"))
	b, _ := os.ReadFile(filepath.Join(root, "b.go"))
	if !strings.Contains(string(a), "import \"fmt\"") || !strings.Contains(string(b), "import \"os\"") {
		t.Errorf("a.go = %q\nb.go = %q", a, b)
	}
}

// hunk quote refuses to quote such a line, from the file or from --new, as it
// refuses a directive, and under another marker it quotes it as text, and
// that patch applies.
func TestQuoteRefusesADirectiveMissingItsSpace(t *testing.T) {
	root := cliTree(t, map[string]string{"f.txt": "a\n@@file b.txt\nc\n"})
	code, out, errOut := quoteCLI(t, root, "f.txt", "1", "3")
	// The refusal goes into the patch as its @@ end line, so the hunk at the
	// end of a pipe applies nothing.
	if code != exitNoMatch || !strings.HasPrefix(out, "@@ end refused by hunk quote: f.txt line 2 is") ||
		!strings.Contains(errOut, `f.txt line 2 is "@@file b.txt", which a patch refuses as a directive missing its space`) ||
		!strings.Contains(errOut, "--marker") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	code, out, errOut = quoteCLI(t, root, "f.txt", "1", "1", "--new", newFile(t, "x\n@@\u00a0end\n"))
	if code != exitNoMatch || !strings.Contains(errOut, "new.txt line 2 is") || !strings.Contains(errOut, "one that is not ASCII") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	code, patch, errOut := quoteCLI(t, root, "--marker", "%%", "f.txt", "2", "2", "--new", newFile(t, "@@ file b.txt\n"))
	if code != exitOK {
		t.Fatalf("under %%%%: exit %d: %s", code, errOut)
	}
	if code, out, errOut := runCLI(t, root, []string{"--marker", "%%"}, patch); code != exitOK {
		t.Fatalf("applying %q: exit %d: %s%s", patch, code, out, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "f.txt")); string(b) != "a\n@@ file b.txt\nc\n" {
		t.Errorf("f.txt = %q", b)
	}
}

// Every document says that the line is refused, beside the rule it is a
// near miss of, and names --marker as the way to write one as text.
func TestTheDocsSayADirectiveMissingItsSpaceIsRefused(t *testing.T) {
	const reach = 600
	for name, doc := range formatDocuments(t) {
		t.Run(name, func(t *testing.T) {
			i := strings.Index(doc, "@@file")
			if i < 0 {
				t.Fatal(`never names a directive missing its space, such as "@@file"`)
			}
			near := doc[max(0, i-reach):min(len(doc), i+reach)]
			for _, w := range []string{"exit 1", "--marker", "no-break space"} {
				if !strings.Contains(near, w) {
					t.Errorf("%q is not within %d bytes of the first \"@@file\"", w, reach)
				}
			}
		})
	}
}
