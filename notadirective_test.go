package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The hint a patch from stdin gets when its first line is not a directive.
const heredocHintText = "the patch came from stdin: if two heredocs share its command line, the shell feeds them " +
	"in the order their delimiters appear, so check that the one hunk reads is the patch"

// A line is shown as the message can print it: quoted, cut at 60 characters,
// and without the CR that ends a line of a CRLF patch.
func TestShownLine(t *testing.T) {
	sixty := strings.Repeat("x", 60)
	for _, c := range []struct{ name, line, want string }{
		{"an empty line", "", "a blank line"},
		{"a CR alone, which is a CRLF patch's empty line", "\r", "a blank line"},
		{"a line", "import json", `"import json"`},
		{"one trailing CR is the line's ending", "import json\r", `"import json"`},
		{"a second CR is the line's own", "import json\r\r", `"import json\r"`},
		{"whitespace only is shown, not called blank", "   ", `"   "`},
		{"a tab", "\t@@ file a.go", `"\t@@ file a.go"`},
		{"a leading space", "  @@ file a.go", `"  @@ file a.go"`},
		{"a byte-order mark", "\ufeff@@ file a.go", `"\ufeff@@ file a.go"`},
		{"an escape", "\x1b[31mred", `"\x1b[31mred"`},
		{"sixty characters, uncut", sixty, `"` + sixty + `"`},
		{"sixty-one, cut", sixty + "y", `"` + sixty + `"...`},
		{"cut by characters, not bytes", strings.Repeat("é", 61), `"` + strings.Repeat("é", 60) + `"...`},
		{"a byte that is not UTF-8 counts as one", strings.Repeat("\xff", 61), `"` + strings.Repeat(`\xff`, 60) + `"...`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := shownLine([]byte(c.line)); got != c.want {
				t.Errorf("shownLine(%q) = %s, want %s", c.line, got, c.want)
			}
		})
	}
}

// Until 2026-10-07 each of these got the same sentence, "expected a
// directive, found payload text", which showed none of them. Each is a
// different mistake, and the line is what tells them apart. The heredoc hint is
// for line 1 from stdin only: from -f nothing is chained, and a later line is
// not where a mix-up of heredocs shows.
func TestALineThatIsNotADirectiveIsShown(t *testing.T) {
	for _, c := range []struct {
		name, patch string
		file        bool // from -f rather than stdin
		line        string
		found       string
		hint        bool
	}{
		{"another heredoc's text", "import json\nprint(1)\n", false, "1", `"import json"`, true},
		{"the same from -f", "import json\nprint(1)\n", true, "1", `"import json"`, false},
		{"a blank first line", "\n@@ file f.txt\n@@ old\na\n@@ new\nb\n", false, "1", "a blank line", true},
		{"a unified diff", "--- a/f.txt\n+++ b/f.txt\n@@ -1 +1 @@\n-a\n+b\n", false, "1", `"--- a/f.txt"`, true},
		{"no space after the marker", "@@file f.txt\n", false, "1", `"@@file f.txt"`, true},
		{"a word that is not a directive", "@@ fiel f.txt\n", false, "1", `"@@ fiel f.txt"`, true},
		{"a byte-order mark", "\ufeff@@ file f.txt\n@@ old\na\n@@ new\nb\n", false, "1", `"\ufeff@@ file f.txt"`, true},
		{"an indented directive", "  @@ file f.txt\n", false, "1", `"  @@ file f.txt"`, true},
		{"a CRLF patch", "import json\r\n@@ end\r\n", false, "1", `"import json"`, true},
		{"a line after @@ file", "@@ file f.txt\na\n@@ old\na\n@@ new\nb\n", false, "2", `"a"`, false},
		{"a line after @@ delete", "@@ delete f.txt\nstray\n", false, "2", `"stray"`, false},
		{"a line after @@ delete, from -f", "@@ delete f.txt\nstray\n", true, "2", `"stray"`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "a\n"})
			before := snapshot(t, root)
			var args []string
			stdin := c.patch
			if c.file {
				p := filepath.Join(t.TempDir(), "p.hunk")
				must(t, os.WriteFile(p, []byte(c.patch), 0o644))
				args, stdin = []string{"-f", p}, ""
			}
			code, out, errOut := runCLI(t, root, args, stdin)
			if code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			want := "hunk: patch line " + c.line + ": expected a directive, found " + c.found + "; a directive is "
			if !strings.HasPrefix(errOut, want) {
				t.Errorf("stderr = %q, want it to start %q", errOut, want)
			}
			if got := strings.Contains(errOut, heredocHintText); got != c.hint {
				t.Errorf("hint = %v, want %v: %q", got, c.hint, errOut)
			}
			assertUnchanged(t, root, before)
		})
	}
}

// The hint is for this one failure, at line 1, from stdin. Every other failure
// at line 1 has a cause the hint does not explain.
func TestHeredocHint(t *testing.T) {
	_, notDirective := Parse([]byte("import json\n"), DefaultMarker)
	_, atLine2 := Parse([]byte("@@ delete f.txt\nstray\n"), DefaultMarker)
	_, control := Parse([]byte("@@ end\f\n"), DefaultMarker)
	_, noPath := Parse([]byte("@@ file\n"), DefaultMarker)
	for _, c := range []struct {
		name      string
		err       error
		fromStdin bool
		want      bool
	}{
		{"not a directive at line 1, from stdin", notDirective, true, true},
		{"the same from -f", notDirective, false, false},
		{"not a directive at line 2", atLine2, true, false},
		{"a control character at line 1", control, true, false},
		{"a directive with no path at line 1", noPath, true, false},
		{"the same, wrapped", errors.Join(errors.New("parse"), notDirective), true, true},
		{"no error", nil, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := heredocHint(c.err, c.fromStdin) != ""; got != c.want {
				t.Errorf("heredocHint(%v, %v) gives a hint: %v, want %v", c.err, c.fromStdin, got, c.want)
			}
		})
	}
	// Through the CLI too: a control character at line 1 from stdin is exit 1
	// with its own message and no hint.
	code, _, errOut := runCLI(t, cliTree(t, nil), nil, "@@ end\f\n")
	if code != exitUsage || strings.Contains(errOut, heredocHintText) {
		t.Errorf("exit %d, stderr %q; want %d and no hint", code, errOut, exitUsage)
	}
	// And it is still a ParseError, which is what makes it exit 1.
	var pe *ParseError
	if !errors.As(notDirective, &pe) || ExitCode(notDirective) != exitUsage {
		t.Errorf("%T is not a *ParseError to errors.As, or exits %d", notDirective, ExitCode(notDirective))
	}
}

// The mistake the issue is about, made the way it is made: two heredocs on one
// command line, their bodies written in the other order, run by a real shell.
// hunk reads the first body and shows its first line, with the hint. In the
// order of the delimiters, the same command applies.
func TestTwoHeredocsInTheWrongOrder(t *testing.T) {
	needsBash(t)
	exe, err := os.Executable()
	must(t, err)
	for _, c := range []struct {
		name, script string
		code         int
		says         string
	}{
		{
			"the other body first",
			"\"$HUNK\" --dry-run <<'HUNK' && cat <<'PY'\nimport json\nPY\n@@ file f.txt\n@@ old\na\n@@ new\nb\nHUNK\n",
			exitUsage, `patch line 1: expected a directive, found "import json"`,
		},
		{
			"in the order of the delimiters",
			"\"$HUNK\" --dry-run <<'HUNK' && cat <<'PY'\n@@ file f.txt\n@@ old\na\n@@ new\nb\nHUNK\nimport json\nPY\n",
			exitOK, "M f.txt +1 -1",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "a\n"})
			cmd := exec.Command("bash", "-c", c.script)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HUNK_TEST_AS_HUNK=1", "HUNK="+exe)
			out, err := cmd.CombinedOutput()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != c.code {
				t.Errorf("exit %d, want %d: %s", code, c.code, out)
			}
			if !strings.Contains(string(out), c.says) {
				t.Errorf("output does not say %q:\n%s", c.says, out)
			}
			if got := strings.Contains(string(out), heredocHintText); got != (c.code == exitUsage) {
				t.Errorf("hint = %v:\n%s", got, out)
			}
		})
	}
}

// The new wording is a contract (§8.1). One golden per shape: from stdin with
// the hint, in JSON, from -f without it, at line 2, and a byte-order mark,
// which is the one the old sentence hid completely.
func TestALineThatIsNotADirectiveIsGolden(t *testing.T) {
	for _, c := range []struct {
		golden, patch string
		args          []string
		file          bool
	}{
		{"cli-parse-not-a-directive", "import json\nprint(1)\n", nil, false},
		{"cli-parse-not-a-directive-json", "import json\nprint(1)\n", []string{"--json"}, false},
		{"cli-parse-not-a-directive-from-a-file", "import json\nprint(1)\n", nil, true},
		{"cli-parse-not-a-directive-line-2", "@@ delete f.txt\nstray\n", nil, false},
		{"cli-parse-not-a-directive-bom", "\ufeff@@ file f.txt\n@@ old\na\n@@ new\nb\n", nil, false},
	} {
		t.Run(c.golden, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "a\n"})
			args, stdin := c.args, c.patch
			if c.file {
				p := filepath.Join(t.TempDir(), "p.hunk")
				must(t, os.WriteFile(p, []byte(c.patch), 0o644))
				args, stdin = append(args, "-f", p), ""
			}
			code, out, errOut := runCLI(t, root, args, stdin)
			if code != exitUsage {
				t.Fatalf("exit %d, want %d: %s", code, exitUsage, errOut)
			}
			golden(t, c.golden, out+errOut)
		})
	}
}
