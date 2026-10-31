package main

// A payload line that is "@@" glued to a word that is no directive,
// "@@include other.txt", is refused where a payload writes it (#138). It was
// written into the file at exit 0, in the field for a directive hunk does not
// have, a payload taken from a file.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// The rule, line by line: what it takes, the word it names, and every near
// thing it leaves to another rule or to the text.
func TestUnknownWord(t *testing.T) {
	const nb = "\u00a0"
	for _, c := range []struct {
		name, marker, line string
		ok                 bool
		word               string
	}{
		{"the issue's", "", "@@include other.txt", true, "include"},
		{"a capital", "", "@@Include x", true, "Include"},
		{"all capitals, alone", "", "@@INCLUDE", true, "INCLUDE"},
		{"one letter", "", "@@x", true, "x"},
		{"a Ruby class variable", "", "@@count = 0", true, "count"},
		{"a T-SQL global", "", "@@ROWCOUNT", true, "ROWCOUNT"},
		{"Razor's escaped at-rule", "", "@@media screen {", true, "media"},
		{"JavaScript's symbol in prose", "", "@@replace collect loop", true, "replace"},
		{"glued to a longer directive word", "", "@@files b.go", true, "files"},
		{"glued to a word and a name", "", "@@fileb.go", true, "fileb.go"},
		{"glued to a word and a colon", "", "@@file:", true, "file:"},
		{"the word ends at a tab", "", "@@foo.bar(x)\ty", true, "foo.bar(x)"},
		{"at a no-break space", "", "@@include" + nb + "y", true, "include"},
		{"at an ideographic space", "", "@@include\u3000y", true, "include"},
		{"one CR is the line's ending", "", "@@include x\r", true, "include"},
		{"a second CR is a byte of the word", "", "@@old\r\r", true, "old\r"},
		{"so is a control character", "", "@@inc\x1blude", true, "inc\x1blude"},
		{"a placeholder needs both markers", "", "@@NAME@ x", true, "NAME@"},
		{"a name, then the marker, then more name, is closed", "", "@@A@@B", false, ""},
		{"an explicit default marker", "@@", "@@include x", true, "include"},

		{"a directive", "", "@@ file b.go", false, ""},
		{"with a space and an unknown word: luaconf.h's", "", "@@ LUAI_MAXSTACK limits", false, ""},
		{"a directive missing its space is separatorFault's", "", "@@file b.go", false, ""},
		{"and so is one with a no-break space after the word", "", "@@file" + nb + "b.go", false, ""},
		{"and @@end with a CR", "", "@@end\r", false, ""},
		{"a control character in a directive word is nearDirective's", "", "@@ en\x7fd", false, ""},
		{"and glued, it is a byte of an unknown word", "", "@@en\x7fd", true, "en\x7fd"},
		{"a placeholder", "", "@@UNIT_TEST_BODY@@", false, ""},
		{"a placeholder with text after it", "", "@@TEMPLATE_KEYS@@ and after", false, ""},
		{"a placeholder with digits", "", "@@a1_b2@@", false, ""},
		{"the marker alone", "", "@@", false, ""},
		{"and a CR", "", "@@\r", false, ""},
		{"an underscore", "", "@@_foo", false, ""},
		{"a digit", "", "@@1abc", false, ""},
		{"a unified diff's hunk header", "", "@@ -1,3 +1,4 @@", false, ""},
		{"glued to a dash", "", "@@-1,2", false, ""},
		{"a combined diff's", "", "@@@ -1 +1 @@@", false, ""},
		{"@@@@", "", "@@@@", false, ""},
		{"a letter that is not ASCII", "", "@@\u00e9t\u00e9", false, ""},
		{"a no-break space after the marker", "", "@@" + nb + "include", false, ""},
		{"indented", "", " @@include", false, ""},
		{"one @", "", "@include", false, ""},
		{"an empty line", "", "", false, ""},
		{"text", "", "include", false, ""},
		{"under another marker, the default's", "%%", "@@include x", false, ""},
		{"and its own: PostScript's", "%%", "%%EOF", false, ""},
		{"and IPython's", "%%", "%%time", false, ""},
		{"a marker of one character", "#", "#include <stdio.h>", false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := c.marker
			if marker == "" {
				marker = DefaultMarker
			}
			word, ok := unknownWord([]byte(c.line), marker)
			if ok != c.ok || word != c.word {
				t.Errorf("unknownWord(%q, %q) = %q, %v; want %q, %v", c.line, marker, word, ok, c.word, c.ok)
			}
		})
	}
}

// Every line that is the default marker and an ASCII letter is exactly one
// of: a directive missing its space or with a space that is not ASCII
// (#96), a placeholder, or an unknown word. Nothing falls between the rules,
// and no line is two of them, so the order Parse asks them in does not show.
// None is a directive with a control character in it, nearDirective's, which
// unknownWord does not ask. A line that is not the default marker and a
// letter is none of this rule's.
func FuzzUnknownWord(f *testing.F) {
	for _, s := range []string{
		"@@include other.txt", "@@file b.go", "@@UNIT_TEST_BODY@@", "@@old\r\r", "@@ en\x7fd", "@@inc\x1blude",
		"@@NAME@ x", "@@file\u00a0b", "@@\u00e9", "@@", "@@x\r", "@@end\x0c", "@@e\x85nd",
	} {
		f.Add(s, DefaultMarker)
	}
	f.Add("%%EOF", "%%")
	f.Add("@@include", "%%")
	f.Fuzz(func(t *testing.T, line, marker string) {
		word, ok := unknownWord([]byte(line), marker)
		bare := strings.TrimSuffix(line, "\r")
		rest, glued := strings.CutPrefix(bare, DefaultMarker)
		glued = glued && marker == DefaultMarker && rest != "" && isASCIILetter(rest[0])
		if !glued {
			if ok {
				t.Fatalf("unknownWord(%q, %q) took a line that is not the default marker and a letter: %q", line, marker, word)
			}
			return
		}
		_, sep := separatorFault([]byte(line), marker)
		_, near := nearDirective([]byte(bare), marker)
		name := 0
		for name < len(rest) && isNameByte(rest[name]) {
			name++
		}
		placeholder := strings.HasPrefix(rest[name:], marker)
		if near {
			t.Fatalf("%q: nearDirective takes a line with a letter glued to the marker", line)
		}
		taken := 0
		for _, b := range []bool{sep, placeholder, ok} {
			if b {
				taken++
			}
		}
		if taken != 1 {
			t.Fatalf("%q: separatorFault %v, placeholder %v, unknownWord %v", line, sep, placeholder, ok)
		}
		if !ok {
			return
		}
		if _, _, d := directive([]byte(line), marker); d {
			t.Fatalf("%q is a directive", line)
		}
		if word == "" || !strings.HasPrefix(rest, word) || directiveWords[word] {
			t.Fatalf("%q: the word %q is not the start of the line after the marker, or is a directive word", line, word)
		}
		// The word is the run up to a separator, and what follows it, if
		// anything, is one.
		if after := rest[len(word):]; after != "" {
			if _, w := sepWidth(after); w == 0 {
				t.Fatalf("%q: the word %q stops at %q, which is no separator", line, word, after)
			}
		}
		for i := range word {
			if _, w := sepWidth(word[i:]); w > 0 {
				t.Fatalf("%q: the word %q holds a separator", line, word)
			}
		}
	})
}

// sepWidth is the width of the separator s starts with, as separatorFault
// reads one, or 0: a space, a tab, or a space that is not ASCII and not a
// control character, which NEL is.
func sepWidth(s string) (rune, int) {
	r, w := utf8.DecodeRuneInString(s)
	if r == ' ' || r == '\t' || r >= utf8.RuneSelf && unicode.IsSpace(r) && !isControl(r) {
		return r, w
	}
	return r, 0
}

// Through the CLI: exit 1, the line named, nothing read or written, whatever
// the flags and wherever the patch comes from; and the control, the same
// patch under another marker, writes the line as text.
func TestAnUnknownWordIsRefused(t *testing.T) {
	for _, c := range []struct {
		name  string
		patch string
		args  []string
		file  bool
		line  string
		word  string
	}{
		{
			name:  "the issue's repro",
			patch: "@@ file f.txt\n@@ old\na\n@@ new\na\n@@include other.txt\n",
			line:  "6", word: "include",
		},
		{
			name:  "from -f",
			patch: "@@ create c.zig\n@@include /tmp/lib-tests.zig\n",
			file:  true, line: "2", word: "include",
		},
		{
			name:  "under --dry-run",
			patch: "@@ append f.txt\n@@count = 0\n",
			args:  []string{"--dry-run"}, line: "2", word: "count",
		},
		{
			name:  "under --verify, which never runs",
			patch: "@@ prepend f.txt\n@@ROWCOUNT\n",
			args:  []string{"--verify", "touch ran"}, line: "2", word: "ROWCOUNT",
		},
		{
			name:  "under --try, which never runs",
			patch: "@@ file f.txt\n@@ old\nb\n@@ new\n@@media screen {\n",
			args:  []string{"--try", "touch ran"}, line: "5", word: "media",
		},
		{
			name:  "a later hunk that would have applied is not",
			patch: "@@ file f.txt\n@@ old\na\n@@ new\nA\n@@x\n@@ old\nb\n@@ new\nB\n",
			line:  "6", word: "x",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := func(patch string, args []string) (code int, out, errOut, root string, before map[string]string) {
				root = cliTree(t, map[string]string{"f.txt": "a\nb\n"})
				before = snapshot(t, root)
				stdin := patch
				if c.file {
					p := filepath.Join(t.TempDir(), "p.hunk")
					must(t, os.WriteFile(p, []byte(patch), 0o644))
					args, stdin = append([]string{"-f", p}, args...), ""
				}
				code, out, errOut = runCLI(t, root, args, stdin)
				return code, out, errOut, root, before
			}

			code, out, errOut, root, before := run(c.patch, c.args)
			if code != exitUsage {
				t.Errorf("exit %d, want %d: %s%s", code, exitUsage, out, errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			want := "hunk: patch line " + c.line + ": "
			if !strings.HasPrefix(errOut, want) || !strings.Contains(errOut, `starts like a directive, but "`+c.word+`" is not one`) ||
				!strings.Contains(errOut, "hunk quote --new FILE") || strings.Count(errOut, "\n") != 1 {
				t.Errorf("stderr = %q, want one line starting %q naming %q", errOut, want, c.word)
			}
			if strings.Contains(errOut, heredocHintText) {
				t.Errorf("got the heredoc hint: %q", errOut)
			}
			assertUnchanged(t, root, before)

			// The control: under another marker the line is text, and the
			// patch applies with it.
			other := strings.NewReplacer("@@ ", "%% ").Replace(c.patch)
			code, out, errOut, root, _ = run(other, append([]string{"--marker", "%%"}, c.args...))
			if code != exitOK {
				t.Fatalf("under --marker %%%%: exit %d: %s%s", code, out, errOut)
			}
			// --dry-run writes nothing and --try puts the tree back.
			if flags := strings.Join(c.args, " "); strings.Contains(flags, "--dry-run") || strings.Contains(flags, "--try") {
				return
			}
			b, err := os.ReadFile(filepath.Join(root, "f.txt"))
			if c.file {
				b, err = os.ReadFile(filepath.Join(root, "c.zig"))
			}
			must(t, err)
			if !strings.Contains(string(b), "@@"+c.word) {
				t.Errorf("under --marker %%%%, the line is not in the file: %q", b)
			}
		})
	}
}

// --json says it as every parse error is said, and --each and hunk invert
// refuse it as the apply does.
func TestAnUnknownWordIsRefusedOnEveryPath(t *testing.T) {
	const patch = "@@ file f.txt\n@@ old\na\n@@ new\na\n@@include other.txt\n"
	const msg = `patch line 6: "@@include other.txt" starts like a directive, but "include" is not one`

	t.Run("--json", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": "a\nb\n"})
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--json"}, patch)
		if code != exitUsage || errOut != "" {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		var got struct {
			OK    bool   `json:"ok"`
			Exit  int    `json:"exit"`
			Error string `json:"error"`
		}
		must(t, json.Unmarshal([]byte(out), &got))
		if got.OK || got.Exit != exitUsage || !strings.HasPrefix(got.Error, msg) {
			t.Errorf("got %+v, want exit %d and an error starting %q", got, exitUsage, msg)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("--each", func(t *testing.T) {
		root := cliTree(t, map[string]string{"f.txt": "a\nb\n"})
		dir := t.TempDir()
		bad, good := filepath.Join(dir, "bad.hunk"), filepath.Join(dir, "good.hunk")
		must(t, os.WriteFile(bad, []byte(patch), 0o644))
		must(t, os.WriteFile(good, []byte("@@ file f.txt\n@@ old\nb\n@@ new\nB\n"), 0o644))
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", "true", "--each", bad, good}, "")
		if code == exitOK {
			t.Errorf("exit 0 with a patch that does not parse: %s%s", out, errOut)
		}
		if all := out + errOut; !strings.Contains(all, "bad.hunk") || !strings.Contains(all, `"include" is not one`) {
			t.Errorf("the refusal is not named by its patch:\n%s%s", out, errOut)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("invert", func(t *testing.T) {
		// A refusal is written into the patch it prints as well, as an
		// "@@ end" that no apply gets past.
		code, out, errOut := invertCLI(t, nil, patch)
		if code != exitUsage || !strings.HasPrefix(out, "@@ end refused by hunk invert: "+msg) || !strings.HasPrefix(errOut, "hunk invert: "+msg) {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit %d and %q in both", code, out, errOut, exitUsage, msg)
		}
	})
}

// The message is a contract (§8.1), in both renderings: the issue's repro.
func TestTheUnknownWordMessageIsGolden(t *testing.T) {
	const patch = "@@ file f.txt\n@@ old\na\n@@ new\na\n@@include other.txt\n"
	for _, asJSON := range []bool{false, true} {
		name := "cli-parse-unknown-word"
		var args []string
		if asJSON {
			name += "-json"
			args = []string{"--json"}
		}
		t.Run(name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "a\nb\n"})
			code, out, errOut := runCLI(t, root, args, patch)
			if code != exitUsage {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			if asJSON {
				golden(t, name, out)
				return
			}
			golden(t, name, errOut)
		})
	}
}
