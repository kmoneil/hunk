package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// CRLF patches and control characters, end to end. A CRLF patch is what
// PowerShell sends down a pipe, what an editor set to CRLF saves, and what git
// checks out with autocrlf; until 2026-10-06 hunk read the CR as part of each
// line, so "@@ end\r" was text written into a file and "@@ create b.txt\r"
// created a name ending in a carriage return, both at exit 0.

// contents is the tree as names and bytes, without the modes snapshot records,
// for rows whose subject is what the files say.
func contents(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, v := range snapshot(t, root) {
		if _, b, ok := strings.Cut(v, "\x00"); ok && v != "directory" {
			v = b
		}
		out[name] = v
	}
	return out
}

func assertContents(t *testing.T, root string, want map[string]string) {
	t.Helper()
	got := contents(t, root)
	for name, w := range want {
		if g, ok := got[name]; !ok {
			t.Errorf("%q is missing", name)
		} else if g != w {
			t.Errorf("%q = %q, want %q", name, g, w)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%q appeared", name)
		}
	}
}

// Every directive, with one trailing CR, does what it does without one.
func TestACRLFDirectiveIsADirective(t *testing.T) {
	for _, c := range []struct {
		name, patch string
		files       map[string]string
		want        map[string]string
	}{
		{
			name:  "a patch with every line CRLF",
			patch: "@@ file a.go\r\n@@ old\r\nalpha\r\n@@ new\r\nALPHA\r\n",
			want:  map[string]string{"a.go": "ALPHA\nbeta\n"},
		},
		{
			name:  "a CRLF @@ file in an LF patch, which looked for a.go plus a CR",
			patch: "@@ file a.go\r\n@@ old\nalpha\n@@ new\nALPHA\n",
			want:  map[string]string{"a.go": "ALPHA\nbeta\n"},
		},
		{
			name:  "a CRLF @@ create, which made a name ending in a CR",
			patch: "@@ create b.txt\r\nhello\n\n",
			want:  map[string]string{"a.go": "alpha\nbeta\n", "b.txt": "hello\n"},
		},
		{
			name:  "@@ end with a CR after a create, which was written into the file",
			patch: "@@ create c.txt\nhello\n@@ end\r\n",
			want:  map[string]string{"a.go": "alpha\nbeta\n", "c.txt": "hello"},
		},
		{
			name:  "@@ end with a CR after a replace, which added a line to the file",
			patch: "@@ file a.go\n@@ old\nalpha\n@@ new\nALPHA\n@@ end\r\n",
			want:  map[string]string{"a.go": "ALPHA\nbeta\n"},
		},
		{
			name:  "@@ old with a CR",
			patch: "@@ file a.go\n@@ old\r\nalpha\n@@ new\nALPHA\n",
			want:  map[string]string{"a.go": "ALPHA\nbeta\n"},
		},
		{
			name:  "@@ new with a CR, which was swallowed into old",
			patch: "@@ file a.go\n@@ old\nalpha\n@@ new\r\nALPHA\n",
			want:  map[string]string{"a.go": "ALPHA\nbeta\n"},
		},
		{
			name:  "@@ old x2 with a CR, which was not a count",
			patch: "@@ file a.go\n@@ old x2\r\nalpha\n@@ new\nZ\n",
			files: map[string]string{"a.go": "alpha\nalpha\n"},
			want:  map[string]string{"a.go": "Z\nZ\n"},
		},
		{
			name:  "@@ delete with a CR, which was no such file",
			patch: "@@ delete a.go\r\n",
			want:  map[string]string{},
		},
		{
			name:  "@@ append and @@ prepend with a CR",
			patch: "@@ append a.go\r\nomega\n@@ prepend a.go\r\nzero\n",
			want:  map[string]string{"a.go": "zero\nalpha\nbeta\nomega\n"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := c.files
			if files == nil {
				files = map[string]string{"a.go": "alpha\nbeta\n"}
			}
			root := cliTree(t, files)
			code, _, errOut := runCLI(t, root, nil, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d, want 0: %s", code, errOut)
			}
			assertContents(t, root, c.want)
		})
	}
}

// A payload's CRs are line endings under --eol auto when the whole patch is
// CRLF, and bytes otherwise. --eol strict keeps every byte, and a patch that
// mixes endings keeps its bytes too, so a CR somebody meant survives.
func TestACRLFPayloadUnderEachEOL(t *testing.T) {
	const (
		lfFile   = "alpha\nbeta\n"
		crlfFile = "alpha\r\nbeta\r\n"
		// every line CRLF
		whole1 = "@@ file f.txt\r\n@@ old\r\nalpha\r\n@@ new\r\nALPHA\r\n@@ end\r\n"
		whole2 = "@@ file f.txt\r\n@@ old\r\nalpha\r\nbeta\r\n@@ new\r\nALPHA\r\nBETA\r\n@@ end\r\n"
		// with the blank line that gives each payload its newline
		whole2nl = "@@ file f.txt\r\n@@ old\r\nalpha\r\nbeta\r\n\r\n@@ new\r\nALPHA\r\nBETA\r\n\r\n@@ end\r\n"
		// directives LF, payload lines CRLF: mixed, so the bytes are kept
		mixed1   = "@@ file f.txt\n@@ old\nalpha\r\n@@ new\nALPHA\r\n@@ end\n"
		mixed2   = "@@ file f.txt\n@@ old\nalpha\r\nbeta\r\n@@ new\nALPHA\r\nBETA\r\n@@ end\n"
		mixed2nl = "@@ file f.txt\n@@ old\nalpha\r\nbeta\r\n\n@@ new\nALPHA\r\nBETA\r\n\n@@ end\n"
	)
	for _, c := range []struct {
		name, file, patch, eol string
		code                   int
		want                   string // the file afterwards; "" with code 2 means untouched
	}{
		// An LF file. This is what changed: a CRLF patch was refused here.
		{"LF file, CRLF patch, one line, auto", lfFile, whole1, "auto", 0, "ALPHA\nbeta\n"},
		{"LF file, CRLF patch, two lines, auto", lfFile, whole2, "auto", 0, "ALPHA\nBETA\n"},
		{"LF file, CRLF patch, two lines and a blank, auto", lfFile, whole2nl, "auto", 0, "ALPHA\nBETA\n"},
		{"LF file, CRLF patch, one line, strict", lfFile, whole1, "strict", exitNoMatch, ""},
		{"LF file, CRLF patch, two lines, strict", lfFile, whole2, "strict", exitNoMatch, ""},
		{"LF file, mixed patch, one line, auto", lfFile, mixed1, "auto", exitNoMatch, ""},
		{"LF file, mixed patch, two lines, auto", lfFile, mixed2, "auto", exitNoMatch, ""},
		// toEOL folds a CR before an LF, so this applied already, and its
		// replacement takes the file's ending.
		{"LF file, mixed patch, two lines and a blank, auto", lfFile, mixed2nl, "auto", 0, "ALPHA\nBETA\n"},
		{"LF file, mixed patch, one line, strict", lfFile, mixed1, "strict", exitNoMatch, ""},
		{"LF file, mixed patch, two lines and a blank, strict", lfFile, mixed2nl, "strict", exitNoMatch, ""},

		// A CRLF file: applied before and after, to the same bytes.
		{"CRLF file, CRLF patch, one line, auto", crlfFile, whole1, "auto", 0, "ALPHA\r\nbeta\r\n"},
		{"CRLF file, CRLF patch, two lines, auto", crlfFile, whole2, "auto", 0, "ALPHA\r\nBETA\r\n"},
		{"CRLF file, CRLF patch, two lines and a blank, auto", crlfFile, whole2nl, "auto", 0, "ALPHA\r\nBETA\r\n"},
		{"CRLF file, CRLF patch, one line, strict", crlfFile, whole1, "strict", 0, "ALPHA\r\nbeta\r\n"},
		{"CRLF file, CRLF patch, two lines, strict", crlfFile, whole2, "strict", 0, "ALPHA\r\nBETA\r\n"},
		{"CRLF file, mixed patch, one line, auto", crlfFile, mixed1, "auto", 0, "ALPHA\r\nbeta\r\n"},
		{"CRLF file, mixed patch, two lines, auto", crlfFile, mixed2, "auto", 0, "ALPHA\r\nBETA\r\n"},
		{"CRLF file, mixed patch, two lines, strict", crlfFile, mixed2, "strict", 0, "ALPHA\r\nBETA\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": c.file})
			code, _, errOut := runCLI(t, root, []string{"--eol", c.eol}, c.patch)
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s", code, c.code, errOut)
			}
			want := c.want
			if want == "" {
				want = c.file
			}
			if got := readFile(t, root, "f.txt"); got != want {
				t.Errorf("f.txt = %q, want %q", got, want)
			}
		})
	}
}

// The other three ops read a CRLF patch's payload the same way. A create has no
// file to convert to, so under auto its file is CRLF, as the patch was, and no
// longer ends in the lone CR the blank line's rule used to leave.
func TestACRLFPatchCreatesAppendsAndPrepends(t *testing.T) {
	for _, c := range []struct {
		name, patch, eol, want string
	}{
		{"create, CRLF patch, auto", "@@ create n.txt\r\nhello\r\nworld\r\n\r\n@@ end\r\n", "auto", "hello\r\nworld\r\n"},
		{"create, CRLF patch, strict keeps every byte", "@@ create n.txt\r\nhello\r\nworld\r\n\r\n@@ end\r\n", "strict", "hello\r\nworld\r\n\r"},
		{"create, CRLF patch, no blank line, auto", "@@ create n.txt\r\nhello\r\n@@ end\r\n", "auto", "hello"},
		{"create, mixed patch, auto keeps the bytes", "@@ create n.txt\nhello\r\nworld\r\n\n@@ end\n", "auto", "hello\r\nworld\r\n"},
		{"create, mixed patch, strict", "@@ create n.txt\nhello\r\nworld\r\n\n@@ end\n", "strict", "hello\r\nworld\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, nil)
			code, _, errOut := runCLI(t, root, []string{"--eol", c.eol}, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d, want 0: %s", code, errOut)
			}
			if got := readFile(t, root, "n.txt"); got != c.want {
				t.Errorf("n.txt = %q, want %q", got, c.want)
			}
		})
	}
	for _, c := range []struct {
		name, file, patch, eol, want string
	}{
		// The append used to leave "more\r\n" in an LF file: a mixed file.
		{"append, CRLF patch, LF file, auto", "a\n", "@@ append f.txt\r\nmore\r\n@@ end\r\n", "auto", "a\nmore\n"},
		{"prepend, CRLF patch, LF file, auto", "a\n", "@@ prepend f.txt\r\nfirst\r\n@@ end\r\n", "auto", "first\na\n"},
		{"append, CRLF patch, CRLF file, auto", "a\r\n", "@@ append f.txt\r\nmore\r\n@@ end\r\n", "auto", "a\r\nmore\r\n"},
		{"append, CRLF patch, LF file, strict", "a\n", "@@ append f.txt\r\nmore\r\n@@ end\r\n", "strict", "a\nmore\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": c.file})
			code, _, errOut := runCLI(t, root, []string{"--eol", c.eol}, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d, want 0: %s", code, errOut)
			}
			if got := readFile(t, root, "f.txt"); got != c.want {
				t.Errorf("f.txt = %q, want %q", got, c.want)
			}
		})
	}
}

// An old of one blank line is refused because it matches everywhere, and on
// an empty file it inserted at offset 0. In a CRLF patch that line is "\r";
// read as --eol auto reads it, it is the same empty old.
func TestACRLFPatchCannotGiveAnEmptyOld(t *testing.T) {
	root := cliTree(t, map[string]string{"e.txt": ""})
	before := snapshot(t, root)
	code, _, errOut := runCLI(t, root, nil, "@@ file e.txt\r\n@@ old\r\n\r\n@@ new\r\nINSERTED\r\n")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d: %s", code, exitUsage, errOut)
	}
	if !strings.Contains(errOut, "patch line 2: @@ old has an empty payload") {
		t.Errorf("stderr = %q", errOut)
	}
	assertUnchanged(t, root, before)
}

// The near-miss hint for a payload whose CRs the file does not have said
// "--eol auto translates that for you" under --eol auto, which had not.
func TestTheLineEndingsHintIsTrueUnderAuto(t *testing.T) {
	for _, eol := range []string{"auto", "strict"} {
		t.Run(eol, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "alpha\nbeta\n"})
			code, _, errOut := runCLI(t, root, []string{"--eol", eol}, "@@ file f.txt\n@@ old\nalpha\r\n@@ new\nX\n")
			if code != exitNoMatch {
				t.Fatalf("exit %d, want %d", code, exitNoMatch)
			}
			for _, want := range []string{
				"the text is there with different line endings, at line 1:",
				"the patch's lines end in CRLF and the file's in LF; write the patch with LF line endings",
			} {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr does not say %q:\n%s", want, errOut)
				}
			}
			if strings.Contains(errOut, "translates that for you") {
				t.Errorf("stderr still says auto translates it:\n%s", errOut)
			}
		})
	}
}

// A control character in a path is one refusal for every op, before anything
// is read, in text and JSON and under --dry-run, and neither output prints it.
func TestAControlCharacterInAPathIsRefusedForEveryOp(t *testing.T) {
	const name = "x\x1b[31mred.txt"
	for _, patch := range []string{
		"@@ file " + name + "\n@@ old\nx\n@@ new\ny\n",
		"@@ create " + name + "\nhi\n",
		"@@ delete " + name + "\n",
		"@@ append " + name + "\nhi\n",
		"@@ prepend " + name + "\nhi\n",
		// after a hunk that would have applied, which is still not written
		"@@ create ok.txt\nhi\n@@ create " + name + "\nhi\n",
	} {
		for _, args := range [][]string{nil, {"--dry-run"}, {"--json"}} {
			t.Run(strings.SplitN(patch, "\n", 2)[0]+" "+strings.Join(args, " "), func(t *testing.T) {
				root := cliTree(t, map[string]string{"a.go": "x\n"})
				before := snapshot(t, root)
				code, out, errOut := runCLI(t, root, args, patch)
				if code != exitNoMatch {
					t.Fatalf("exit %d, want %d: %s %s", code, exitNoMatch, out, errOut)
				}
				assertUnchanged(t, root, before)
				report := errOut
				if len(args) > 0 && args[0] == "--json" {
					report = out
					var got struct{ Error string }
					must(t, json.Unmarshal([]byte(out), &got))
					if !strings.Contains(got.Error, "x<U+001B>[31mred.txt: the path contains U+001B") {
						t.Errorf("JSON error = %q", got.Error)
					}
				}
				if !strings.Contains(report, "U+001B, a control character") {
					t.Errorf("report does not name the character:\n%q", report)
				}
				// A report is lines, so its own newlines are not the question.
				if name, ok := firstControl(strings.ReplaceAll(out+errOut, "\n", "")); ok {
					t.Errorf("output prints %s raw:\n%q", name, out+errOut)
				}
			})
		}
	}
}

// Each message this card added or changed is a contract (§8.1), in both
// renderings. The root is substituted as in TestAPathRefusalIsGolden.
func TestTheControlCharacterMessagesAreGolden(t *testing.T) {
	for _, c := range []struct {
		golden, patch string
		files         map[string]string
		code          int
	}{
		{"cli-path-control-character", "@@ create x\x1b[31mred.txt\nhi\n", nil, exitNoMatch},
		{"cli-path-refused-nul", "@@ create a\x00b.txt\nhi\n", nil, exitNoMatch},
		{"cli-parse-control-character", "@@ create a.txt\nhi\n@@ end\f\n", nil, exitUsage},
		{
			"cli-crlf-payload-on-an-lf-file", "@@ file f.txt\n@@ old\nalpha\r\n@@ new\nX\n",
			map[string]string{"f.txt": "alpha\nbeta\n"},
			exitNoMatch,
		},
	} {
		for _, asJSON := range []bool{false, true} {
			name := c.golden
			var args []string
			if asJSON {
				name += "-json"
				args = []string{"--json"}
			}
			t.Run(name, func(t *testing.T) {
				root := cliTree(t, c.files)
				code, out, errOut := runCLI(t, root, args, c.patch)
				if code != c.code {
					t.Fatalf("exit %d, want %d: %s %s", code, c.code, out, errOut)
				}
				if !asJSON {
					golden(t, name, slashPaths(strings.ReplaceAll(errOut, root, "/the/root")))
					return
				}
				// In JSON a backslash is escaped, so the root is found in its
				// escaped form, and nothing else is folded: the quotes in a
				// parse error are escaped with backslashes too.
				escaped, err := json.Marshal(root)
				must(t, err)
				golden(t, name, strings.ReplaceAll(out, strings.Trim(string(escaped), `"`), "/the/root"))
			})
		}
	}
}
