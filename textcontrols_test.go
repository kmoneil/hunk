package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A line from the tree as the text report prints it. Until 2026-10-07 an
// escape in it reached the terminal raw, and with whitespace marked a literal →
// or · read the same as a tab or a trailing space.
func TestShown(t *testing.T) {
	for _, c := range []struct {
		name, line    string
		plain, marked string
	}{
		{"nothing to show", "abc", "abc", "abc"},
		{"an escape", "x\x1b[2Jy", "x<U+001B>[2Jy", "x<U+001B>[2Jy"},
		{"a carriage return inside a line", "one\rTWO", "one<U+000D>TWO", "one<U+000D>TWO"},
		{"a CRLF file's line ending", "abc\r", "abc\r", "abc\r"},
		{"a trailing space before it", "abc \r", "abc \r", "abc·\r"},
		{"a C1 control", "a\u009bb", "a<U+009B>b", "a<U+009B>b"},
		{"a lone byte an 8-bit terminal reads as one", "a\x9bb", "a<0x9B>b", "a<0x9B>b"},
		{"DEL", "a\x7fb", "a<U+007F>b", "a<U+007F>b"},
		{"a tab", "a\tb", "a\tb", "a→b"},
		{"a literal arrow and dot", "a→b·c", "a→b·c", "a<U+2192>b<U+00B7>c"},
		{"all four together", "a→b\tc· ", "a→b\tc· ", "a<U+2192>b→c<U+00B7>·"},
		{"trailing tab and space", "a\t ", "a\t ", "a→·"},
		{"an interior space", "a b", "a b", "a b"},
		{"an empty line", "", "", ""},
		{"a lone CR", "\r", "\r", "\r"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shown([]byte(c.line), false); got != c.plain {
				t.Errorf("shown(%q, false) = %q, want %q", c.line, got, c.plain)
			}
			if got := shown([]byte(c.line), true); got != c.marked {
				t.Errorf("shown(%q, true) = %q, want %q", c.line, got, c.marked)
			}
		})
	}
}

// The JSON escapes a C1 control as it escapes C0, and nothing else.
func TestEscapeC1(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a\u0080b\u009fc", `a\u0080b\u009fc`},
		{"\u009b2J", `\u009b2J`},
		{"no break", "no break"},
		{"café", "café"},
		{`already \u001b`, `already \u001b`},
		{"ends in \xc2", "ends in \xc2"},
		{"plain", "plain"},
	} {
		if got := string(escapeC1([]byte(c.in))); got != c.want {
			t.Errorf("escapeC1(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Every way the tree's bytes or a command's reach the text report, pinned in
// both renderings (§8.1).
func TestWhatTheTextCannotPrintIsGolden(t *testing.T) {
	near := "@@ file f.txt\n@@ old\nx\x1b[31mred\none\rTWO\na\u009bb\n@@ new\nX\n"
	for _, c := range []struct {
		golden string
		files  map[string]string
		args   []string
		patch  string
		code   int
	}{
		{"report-span-controls", map[string]string{"f.txt": "x\x1b[31mred \none\rTWO\na\u009bb\n"}, nil, near, exitNoMatch},
		{"report-span-controls-json", map[string]string{"f.txt": "x\x1b[31mred \none\rTWO\na\u009bb\n"}, []string{"--json"}, near, exitNoMatch},
		{
			"report-span-literal-markers",
			map[string]string{"f.txt": "a→b\tc· \nend\n"},
			nil,
			"@@ file f.txt\n@@ old\na→b\tc·\nend\n@@ new\nX\n", exitNoMatch,
		},
		{
			"report-verify-output-controls",
			map[string]string{"f.txt": "a\n"},
			[]string{"--verify", `printf '\033]0;title\007\033[2Jboom\n'; false`},
			"@@ file f.txt\n@@ old\na\n@@ new\nb\n", exitVerifyFailed,
		},
	} {
		t.Run(c.golden, func(t *testing.T) {
			if strings.Contains(c.golden, "verify") {
				needsBash(t) // printf's escapes are the shell's
			}
			root := cliTree(t, c.files)
			code, out, errOut := runCLI(t, root, c.args, c.patch)
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s%s", code, c.code, out, errOut)
			}
			// No slashPaths: no path is printed here, and the escapes are backslashes.
			golden(t, c.golden, strings.ReplaceAll(out+errOut, root, "/the/root"))
		})
	}
}

// A link's destination is the tree's bytes, and a refusal that quotes it shows
// a control character in it by name, in the text and in the JSON's message.
func TestARefusalNamesAControlInALinksDestination(t *testing.T) {
	root := cliTree(t, map[string]string{"sub/in.txt": "a\n"})
	must(t, os.Symlink(native("../out\x1b[31mside"), filepath.Join(root, "lk")))
	patch := "@@ file lk/x\n@@ old\na\n@@ new\nb\n"
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if strings.Contains(errOut, "\x1b") || !strings.Contains(errOut, "lk -> ../out<U+001B>[31mside") {
		t.Errorf("stderr = %q, want the escape named", errOut)
	}
	golden(t, "cli-path-refused-link-controls", slashPaths(strings.ReplaceAll(errOut, root, "/the/root")))
	_, out, _ = runCLI(t, root, []string{"--json"}, patch)
	var r struct{ Error string }
	must(t, json.Unmarshal([]byte(out), &r))
	if strings.Contains(r.Error, "\x1b") || !strings.Contains(r.Error, "<U+001B>") {
		t.Errorf("JSON error = %q, want the escape named", r.Error)
	}
}

// A report about a verify that printed control characters prints none, by any
// path that carries the command's output.
func TestAVerifysOutputReachesNoTerminalRaw(t *testing.T) {
	var b strings.Builder
	r := &Report{Exit: exitVerifyFailed, Result: &Result{Hunks: 1}, Verify: &Verify{
		Ran: true, Command: "make", Tail: []string{"\x1b[2Jclear", "bell\x07", "c1 \u009b", "crlf\r"},
		TotalLines: 4, Applied: 1, RolledBack: 1,
	}}
	r.Text(&b, &b, false)
	for _, line := range strings.Split(b.String(), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if name, ok := firstControl(line); ok {
			t.Errorf("%q prints %s raw", line, name)
		}
	}
}
