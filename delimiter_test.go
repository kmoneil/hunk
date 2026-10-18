package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every row of the card's table, and the edges of each half of the rule: a
// line that is only HUNK padded, a line that holds HUNK, a line one edit from
// it, and what stops each.
func TestLooksLikeDelimiter(t *testing.T) {
	for _, c := range []struct {
		name string
		line string
		crlf bool
		want bool
	}{
		// What the issue saw, and the shapes the recon found worse.
		{"the issue's: a stutter before the real closer", "HUHUNK", false, true},
		{"the word twice", "HUNKHUNK", false, true},
		{"an indented closer", "  HUNK", false, true},
		{"a tab-indented closer", "\tHUNK", false, true},
		{"a trailing space", "HUNK ", false, true},
		{"a trailing tab", "HUNK\t", false, true},
		{"a CR in a patch that is not CRLF is a byte of the line", "HUNK\r", false, true},
		{"a stray CR in a CRLF patch, past the one that ends it", "HUNK\r\r", true, true},
		{"a letter after", "HUNKK", false, true},
		{"the skill's other delimiter", "HUNK2", false, true},
		{"a letter before", "xHUNK", false, true},
		{"a word that holds it", "CHUNK", false, true},
		{"one letter dropped at the end", "HUN", false, true},
		{"one letter dropped at the start", "UNK", false, true},
		{"one letter dropped inside", "HNK", false, true},
		{"one letter replaced", "HUMK", false, true},
		{"the last letter replaced", "HUNL", false, true},
		{"the first letter replaced", "JUNK", false, true},
		{"two neighbours swapped", "HNUK", false, true},
		{"the first two swapped", "UHNK", false, true},
		{"the last two swapped", "HUKN", false, true},
		{"padded and one edit away", "  HUMK ", false, true},

		// What is left alone.
		{"the line HUNK itself", "HUNK", false, false},
		{"HUNK and the CR that ends a CRLF patch's line", "HUNK\r", true, false},
		{"lowercase", "hunk", false, false},
		{"capitalised", "Hunk", false, false},
		{"an identifier at a line's end", "return MAX_CHUNK", false, false},
		{"an assignment", "x = DEFAULT_CHUNK", false, false},
		{"an underscore is not a letter", "MAX_CHUNK", false, false},
		{"an unquoted heredoc opener", "hunk <<HUNK", false, false},
		{"two words", "HU HUNK", false, false},
		{"punctuation", "HUNK!", false, false},
		{"a quoted word", `"HUNK"`, false, false},
		{"a blank line", "", false, false},
		{"spaces only", "   ", false, false},
		{"two edits away", "HXXK", false, false},
		{"reversed", "KNUH", false, false},
		{"two letters swapped that are not neighbours", "NUHK", false, false},
		{"two letters long", "HU", false, false},
		{"a letter that is not ASCII", "HUNKé", false, false},
		{"full-width letters", "ＨＵＮＫ", false, false},
		{"a control character inside", "HU\x1bNK", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeDelimiter([]byte(c.line), c.crlf); got != c.want {
				t.Errorf("looksLikeDelimiter(%q, crlf %v) = %v, want %v", c.line, c.crlf, got, c.want)
			}
		})
	}
}

func TestOneEditFrom(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"HUNK", "HUNK", false},
		{"HUNKK", "HUNK", true},
		{"HUN", "HUNK", true},
		{"HUNK", "HUN", true},
		{"HUMK", "HUNK", true},
		{"HNUK", "HUNK", true},
		{"HUKN", "HUNK", true},
		{"HUNKKK", "HUNK", false},
		{"HU", "HUNK", false},
		{"NUHK", "HUNK", false},
		{"HXXK", "HUNK", false},
		{"", "H", true},
		{"", "", false},
		{"ab", "ba", true},
		{"abc", "bca", false},
	} {
		if got := oneEditFrom(c.a, c.b); got != c.want {
			t.Errorf("oneEditFrom(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// osaDistance is the textbook optimal-string-alignment distance: insertions,
// deletions, substitutions and swaps of neighbours. It is the slow, obvious
// version oneEditFrom is checked against.
func osaDistance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

var reOneWord = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// The rule against a second statement of it, written the slow way: any line
// is either noted or not, never a panic, and the two agree.
func FuzzLooksLikeDelimiter(f *testing.F) {
	for _, s := range []string{"HUNK", "HUHUNK", "  HUNK", "HUNK\r", "HUN", "HNUK", "MAX_CHUNK", "", "\xff", "ab"} {
		f.Add(s, false)
		f.Add(s, true)
	}
	f.Fuzz(func(t *testing.T, line string, crlf bool) {
		got := looksLikeDelimiter([]byte(line), crlf)
		exact := line
		if crlf {
			exact = strings.TrimSuffix(line, "\r")
		}
		w := strings.Trim(line, " \t\r")
		want := exact != "HUNK" && reOneWord.MatchString(w) &&
			(strings.Contains(w, "HUNK") || osaDistance(w, "HUNK") == 1)
		if got != want {
			t.Errorf("looksLikeDelimiter(%q, crlf %v) = %v, the slow rule says %v", line, crlf, got, want)
		}
	})
}

// oneEditFrom against the distance, over any two strings: the rule only ever
// asks it about HUNK, but nothing in it is HUNK's.
func FuzzOneEditFrom(f *testing.F) {
	f.Add("HUNK", "HUNK")
	f.Add("HNUK", "HUNK")
	f.Add("", "a")
	f.Add("abc", "acb")
	f.Fuzz(func(t *testing.T, a, b string) {
		if len(a) > 64 || len(b) > 64 {
			return
		}
		if got, want := oneEditFrom(a, b), osaDistance(a, b) == 1; got != want {
			t.Errorf("oneEditFrom(%q, %q) = %v, distance %d", a, b, got, osaDistance(a, b))
		}
	})
}

// strayEqual compares the fields a test states, Text as text.
func strayEqual(a, b Stray) bool {
	return a.Line == b.Line && a.Path == b.Path && bytes.Equal(a.Text, b.Text) && a.After == b.After
}

func stray(line int, path, text string, after int) Stray {
	return Stray{Line: line, Path: path, Text: []byte(text), After: after}
}

// Where the parser looks, and what it counts after a line, for each payload
// kind and each exclusion the card decided.
func TestParseNotesStrays(t *testing.T) {
	for _, c := range []struct {
		name   string
		marker string
		in     string
		want   []Stray
	}{
		{
			name: "the issue: a stray above the real closer",
			in:   "@@ file notes.txt\n@@ old\ntwo\n@@ new\ntwo, edited\nHUHUNK\n",
			want: []Stray{stray(6, "notes.txt", "HUHUNK", 0)},
		},
		{
			name: "no real closer: the rest of the command follows it",
			in:   "@@ file notes.txt\n@@ old\ntwo\n@@ new\ntwo, edited\nHUNKK\nmake test\ncat notes.txt\n",
			want: []Stray{stray(6, "notes.txt", "HUNKK", 2)},
		},
		{
			name: "the blank line that ends a payload is not a line after it",
			in:   "@@ file a\n@@ old\nx\n@@ new\ny\nHUHUNK\n\n@@ old\np\n@@ new\nq\n",
			want: []Stray{stray(6, "a", "HUHUNK", 0)},
		},
		{
			name: "a blank line inside the payload is",
			in:   "@@ file a\n@@ old\nx\n@@ new\nHUNKK\n\nmake test\n",
			want: []Stray{stray(5, "a", "HUNKK", 2)},
		},
		{
			name: "in a create",
			in:   "@@ create a.md\n# A\n  HUNK\n",
			want: []Stray{stray(3, "a.md", "  HUNK", 0)},
		},
		{
			name: "in an append",
			in:   "@@ append a.md\nmore\nHUNK \n",
			want: []Stray{stray(3, "a.md", "HUNK ", 0)},
		},
		{
			name: "in a prepend",
			in:   "@@ prepend a.md\nHUHUNK\nfirst\n",
			want: []Stray{stray(2, "a.md", "HUHUNK", 1)},
		},
		{
			name: "in an old, which has to match the file, it is not noted",
			in:   "@@ file a\n@@ old\nHUHUNK\n@@ new\nx\n",
		},
		{
			name: "a new line its old holds is carried over",
			in:   "@@ file a\n@@ old\n  HUNK\nx\n@@ new\n  HUNK\ny\n",
		},
		{
			name: "one its old does not hold is written",
			in:   "@@ file a\n@@ old\n  HUNK\n@@ new\n  HUNK\nHUNK \n",
			want: []Stray{stray(6, "a", "HUNK ", 0)},
		},
		{
			name: "another hunk's old does not carry it",
			in:   "@@ file a\n@@ old\n  HUNK\n@@ new\nx\n@@ old\ny\n@@ new\n  HUNK\n",
			want: []Stray{stray(9, "a", "  HUNK", 0)},
		},
		{
			name: "the line HUNK itself is not one",
			in:   "@@ create a.md\nHUNK\nmore\n",
		},
		{
			name: "nor a longer line that ends in HUNK",
			in:   "@@ create a.go\nreturn MAX_CHUNK\nhunk <<HUNK\n",
		},
		{
			name: "two in one payload, the first counting the second",
			in:   "@@ create a\nHUHUNK\nHUNK\nHUNKK\nls\n",
			want: []Stray{stray(2, "a", "HUHUNK", 3), stray(4, "a", "HUNKK", 1)},
		},
		{
			name: "each names its own hunk's file, and lines count through the stream",
			in:   "@@ file a\n@@ old\nx\n@@ new\nHUHUNK\n@@ create b\nHUN\n",
			want: []Stray{stray(5, "a", "HUHUNK", 0), stray(7, "b", "HUN", 0)},
		},
		{
			name: "a CRLF patch: a line is carried over whether or not it has its CR",
			in:   "@@ file 0\r\n@@ old\r\n  HUNK\r\n@@ new\r\n  HUNK",
		},
		{
			name: "a CRLF patch: and with it",
			in:   "@@ file 0\r\n@@ old\r\n  HUNK\r\n@@ new\r\n  HUNK\r\nx\r\n",
		},
		{
			name: "a CRLF patch: HUNK and its CR is the line HUNK",
			in:   "@@ create a\r\nHUNK\r\nmore\r\n",
		},
		{
			name: "a CRLF patch: the CR is not the line's, and a blank line ends the payload",
			in:   "@@ create a\r\nHUHUNK\r\n\r\n",
			want: []Stray{stray(2, "a", "HUHUNK", 0)},
		},
		{
			name: "an LF patch: HUNK and a CR is not the line HUNK",
			in:   "@@ create a\nHUNK\r\n",
			want: []Stray{stray(2, "a", "HUNK\r", 0)},
		},
		{
			name:   "under --marker, which is not the delimiter",
			marker: "%%",
			in:     "%% create a\nHUHUNK\n",
			want:   []Stray{stray(2, "a", "HUHUNK", 0)},
		},
		{
			name: "a delete has no payload, and an empty new none to note",
			in:   "@@ delete a\n@@ file b\n@@ old\nx\n@@ new\n@@ end\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			marker := c.marker
			if marker == "" {
				marker = DefaultMarker
			}
			p, err := Parse([]byte(c.in), marker)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Strays) != len(c.want) {
				t.Fatalf("noted %d, want %d: %+v", len(p.Strays), len(c.want), p.Strays)
			}
			for i := range c.want {
				if !strayEqual(p.Strays[i], c.want[i]) {
					t.Errorf("stray %d = %+v (%q), want %+v (%q)", i, p.Strays[i], p.Strays[i].Text, c.want[i], c.want[i].Text)
				}
			}
		})
	}
}

// A patch with the issue's stray, and the same patch with an ordinary line in
// its place, so that every outcome can be checked to report byte for byte as
// it does without one, plus the warning.
const (
	strayPatch = "@@ file notes.txt\n@@ old\ntwo\n@@ new\ntwo, edited\nHUHUNK\n"
	cleanPatch = "@@ file notes.txt\n@@ old\ntwo\n@@ new\ntwo, edited\nordinary\n"

	strayApplied = "warning: patch line 6 is \"HUHUNK\", which looks like a mistyped heredoc delimiter; " +
		"it is payload for notes.txt, so if it was meant to end the heredoc, remove it from notes.txt\n"
	strayNotApplied = "warning: patch line 6 is \"HUHUNK\", which looks like a mistyped heredoc delimiter; " +
		"it is payload for notes.txt, so if it was meant to end the heredoc, take it out of the patch\n"
)

// warnCLI is runCLI under --allow-mistyped-delimiter, where a patch with stray
// lines applies and they are warned of, as every patch did before #97 made
// them a refusal. The warning tests below run through it unchanged, and their
// goldens are the v0.3.15 bytes.
func warnCLI(t *testing.T, root string, args []string, stdin string) (code int, stdout, stderr string) {
	t.Helper()
	return runCLI(t, root, append([]string{allowMistypedFlag}, args...), stdin)
}

// One case per outcome: the exit and the report are the clean patch's, and
// the warning follows the report on stderr, in the tense of whether the batch
// is on disk.
func TestAMistypedDelimiterIsWarnedOf(t *testing.T) {
	notes := map[string]string{"notes.txt": "one\ntwo\n"}
	for _, c := range []struct {
		name   string
		args   []string
		extra  string // a hunk added to both patches
		exit   int
		warn   string
		onDisk bool
	}{
		{"applied", nil, "", exitOK, strayApplied, true},
		{"a dry run", []string{"--dry-run"}, "", exitOK, strayNotApplied, false},
		{"a verify that passes", []string{"--verify", "true"}, "", exitOK, strayApplied, true},
		{"a verify that fails", []string{"--verify", "echo nope; false"}, "", exitVerifyFailed, strayNotApplied, false},
		{"--keep-on-fail", []string{"--verify", "false", "--keep-on-fail"}, "", exitVerifyFailed, strayApplied, true},
		{"--try that passes", []string{"--try", "true"}, "", exitOK, strayNotApplied, false},
		{"--try that fails", []string{"--try", "echo caught; exit 7"}, "", 7, strayNotApplied, false},
		{"a hunk that misses", nil, "@@ old\nnot there\n@@ new\nx\n", exitNoMatch, strayNotApplied, false},
		{"--marker", []string{"--marker", "%%"}, "", exitOK, strayApplied, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			patch, clean := strayPatch+c.extra, cleanPatch+c.extra
			if len(c.args) > 1 && c.args[0] == "--marker" {
				patch = strings.ReplaceAll(patch, "@@ ", "%% ")
				clean = strings.ReplaceAll(clean, "@@ ", "%% ")
			}
			wantCode, wantOut, wantErr := warnCLI(t, cliTree(t, notes), c.args, clean)
			root := cliTree(t, notes)
			code, out, errOut := warnCLI(t, root, c.args, patch)

			if code != c.exit || wantCode != c.exit {
				t.Fatalf("exit %d, the clean patch's %d, want %d\n%s%s", code, wantCode, c.exit, out, errOut)
			}
			// Two runs, so the timings are nobody's contract.
			tenths := func(s string) string { return reTenths.ReplaceAllString(s, "(0.0s)") }
			if tenths(out) != tenths(wantOut) {
				t.Errorf("stdout is not the clean patch's\n--- got ---\n%s--- clean ---\n%s", out, wantOut)
			}
			report, warning, found := strings.Cut(errOut, "warning: ")
			if !found {
				t.Fatalf("no warning on stderr:\n%s", errOut)
			}
			if tenths(report) != tenths(wantErr)+"\n" {
				t.Errorf("stderr before the warning is not the clean patch's report and a blank line\n--- got ---\n%q\n--- clean ---\n%q", report, wantErr)
			}
			if "warning: "+warning != c.warn {
				t.Errorf("warning\n got %q\nwant %q", "warning: "+warning, c.warn)
			}
			if strings.Contains(out, "warning") {
				t.Errorf("the warning went to stdout: %s", out)
			}
			if c.args != nil && strings.HasPrefix(c.args[0], "--try") && !strings.HasPrefix(out+errOut, "tried") {
				t.Errorf("a --try report has to start tried (§4.1):\n%s%s", out, errOut)
			}
			holds := strings.Contains(readFile(t, root, "notes.txt"), "HUHUNK")
			if holds != c.onDisk {
				t.Errorf("notes.txt holds the stray: %v, the warning says %v", holds, c.onDisk)
			}
		})
	}

	t.Run("under --quiet it is all that prints, with no blank line before it", func(t *testing.T) {
		code, out, errOut := warnCLI(t, cliTree(t, notes), []string{"--quiet"}, strayPatch)
		if code != exitOK || out != "" || errOut != strayApplied {
			t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
		}
	})

	t.Run("from -f, which a heredoc may have written", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "p.hunk")
		must(t, os.WriteFile(p, []byte(strayPatch), 0o644))
		code, _, errOut := warnCLI(t, cliTree(t, notes), []string{"-f", p}, "")
		if code != exitOK || !strings.HasSuffix(errOut, strayApplied) {
			t.Errorf("exit %d\nstderr %q", code, errOut)
		}
	})

	t.Run("the rest of the command, which never ran", func(t *testing.T) {
		patch := "@@ file notes.txt\n@@ old\ntwo\n@@ new\ntwo, edited\nHUNKK\nmake test\ncat notes.txt\n"
		code, out, errOut := warnCLI(t, cliTree(t, notes), nil, patch)
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		want := "warning: patch line 6 is \"HUNKK\", which looks like a mistyped heredoc delimiter; " +
			"it and the 2 lines after it are payload for notes.txt, so if it was meant to end the heredoc, " +
			"remove them from notes.txt and run any command they held, which the shell never did\n"
		if !strings.HasSuffix(errOut, want) {
			t.Errorf("stderr\n got %q\nwant %q", errOut, want)
		}
		golden(t, "cli-mistyped-delimiter-rest-of-the-command", out+errOut)
	})

	t.Run("one line after it is a line", func(t *testing.T) {
		patch := "@@ file notes.txt\n@@ old\ntwo\n@@ new\nHUNKK\nls\n"
		_, _, errOut := warnCLI(t, cliTree(t, notes), []string{"--dry-run"}, patch)
		if !strings.Contains(errOut, "it and the 1 line after it are payload for notes.txt") ||
			!strings.Contains(errOut, "take them out of the patch and run any command") {
			t.Errorf("stderr %q", errOut)
		}
	})

	t.Run("a CR that is a byte of the line is shown", func(t *testing.T) {
		_, _, errOut := warnCLI(t, cliTree(t, notes), []string{"--dry-run"}, "@@ create n.txt\nHUNK\r\n")
		if !strings.Contains(errOut, `patch line 2 is "HUNK\r", which looks like`) {
			t.Errorf("stderr %q", errOut)
		}
	})

	t.Run("a path is shown as a report shows one", func(t *testing.T) {
		_, _, errOut := warnCLI(t, cliTree(t, nil), []string{"--dry-run"}, "@@ create n\x1b[2J.txt\nHUHUNK\n")
		if strings.Contains(errOut, "\x1b") || !strings.Contains(errOut, "payload for n<U+001B>[2J.txt") {
			t.Errorf("stderr %q", errOut)
		}
	})

	t.Run("two are two lines, and a blank line comes once", func(t *testing.T) {
		patch := "@@ create a.md\nHUHUNK\n\n@@ create b.md\n  HUNK\n\n"
		_, _, errOut := warnCLI(t, cliTree(t, nil), nil, patch)
		lines := strings.Split(strings.TrimSuffix(errOut, "\n"), "\n")
		if len(lines) != 3 || lines[0] != "" ||
			!strings.HasPrefix(lines[1], `warning: patch line 2 is "HUHUNK"`) || !strings.Contains(lines[1], "payload for a.md") ||
			!strings.HasPrefix(lines[2], `warning: patch line 5 is "  HUNK"`) || !strings.Contains(lines[2], "payload for b.md") {
			t.Errorf("stderr %q", errOut)
		}
	})

	t.Run("the issue's repro, golden", func(t *testing.T) {
		_, out, errOut := warnCLI(t, cliTree(t, notes), nil, strayPatch)
		golden(t, "cli-mistyped-delimiter", out+errOut)
	})

	t.Run("a verify that fails, golden", func(t *testing.T) {
		code, _, errOut := warnCLI(t, cliTree(t, notes), []string{"--verify", "echo FAIL; false"}, strayPatch)
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		golden(t, "cli-mistyped-delimiter-verify-failed", errOut)
	})
}

// --json carries each in warnings, whatever the outcome, and prints nothing on
// stderr; it says nothing when there is nothing to say.
func TestAMistypedDelimiterInJSON(t *testing.T) {
	notes := map[string]string{"notes.txt": "one\ntwo\n"}
	type report struct {
		Exit     int
		Warnings []map[string]any
	}
	for _, c := range []struct {
		name string
		args []string
		exit int
		fix  string
	}{
		{"applied", []string{"--json"}, exitOK, "remove it from notes.txt"},
		{"--json wins over --quiet", []string{"--json", "--quiet"}, exitOK, "remove it from notes.txt"},
		{"a dry run", []string{"--json", "--dry-run"}, exitOK, "take it out of the patch"},
		{"--try", []string{"--json", "--try", "exit 3"}, 3, "take it out of the patch"},
		{"a verify that fails", []string{"--json", "--verify", "false"}, exitVerifyFailed, "take it out of the patch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := warnCLI(t, cliTree(t, notes), c.args, strayPatch)
			if code != c.exit || errOut != "" {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			var r report
			must(t, json.Unmarshal([]byte(out), &r))
			if len(r.Warnings) != 1 {
				t.Fatalf("warnings: %s", out)
			}
			w := r.Warnings[0]
			if w["patch_line"] != 6.0 || w["path"] != "notes.txt" || w["text"] != "HUHUNK" || w["lines_after"] != 0.0 {
				t.Errorf("warning %v", w)
			}
			if _, ok := w["patch"]; ok {
				t.Errorf("a single run's warning names no patch file: %v", w)
			}
			if msg, _ := w["message"].(string); !strings.HasSuffix(msg, c.fix) || strings.HasPrefix(msg, "warning") {
				t.Errorf("message %q, want it to end %q", msg, c.fix)
			}
		})
	}

	t.Run("golden", func(t *testing.T) {
		_, out, _ := warnCLI(t, cliTree(t, notes), []string{"--json"}, strayPatch)
		golden(t, "cli-mistyped-delimiter-json", out)
	})

	t.Run("lines_after is there when it is 0, and counts", func(t *testing.T) {
		_, out, _ := warnCLI(t, cliTree(t, notes), []string{"--json", "--dry-run"}, "@@ create n.txt\nHUNKK\nls\n")
		if !strings.Contains(out, `"lines_after": 1,`) {
			t.Errorf("%s", out)
		}
	})

	t.Run("no warnings key when there is none", func(t *testing.T) {
		_, out, _ := warnCLI(t, cliTree(t, notes), []string{"--json"}, cleanPatch)
		if strings.Contains(out, "warnings") {
			t.Errorf("%s", out)
		}
	})
}

// The lines the rule leaves alone, together in one batch that applies: no
// warning, in the text or in --json.
func TestWhatIsNotAMistypedDelimiter(t *testing.T) {
	files := map[string]string{"a.md": "  HUNK\nx\n", "b.go": "package b\n"}
	patch := "@@ file a.md\n" +
		"@@ old\n  HUNK\nx\n@@ new\n  HUNK\ny\n" + // carried over from its old
		"@@ create c.md\nHUNK\n\n" + // the line HUNK itself
		"@@ append b.go\nfunc f() int { return MAX_CHUNK }\nconst MAX_CHUNK = 1\nhunk <<HUNK\n" + // HUNK ends longer lines
		"@@ create d.md\nhunk\nHunk\n" // not the word's case
	root := cliTree(t, files)
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitOK || errOut != "" || strings.Contains(out, "warning") {
		t.Fatalf("exit %d\nstdout %s\nstderr %q", code, out, errOut)
	}
	_, js, _ := runCLI(t, cliTree(t, files), []string{"--json"}, patch)
	if strings.Contains(js, "warnings") {
		t.Errorf("%s", js)
	}

	// And an old holding one does not warn, whether it matches or not.
	code, _, errOut = runCLI(t, cliTree(t, map[string]string{"a.md": "HUHUNK\n"}), nil, "@@ file a.md\n@@ old\nHUHUNK\n@@ new\nok\n")
	if code != exitOK || errOut != "" {
		t.Errorf("an old that matches: exit %d, stderr %q", code, errOut)
	}
	code, _, errOut = runCLI(t, cliTree(t, map[string]string{"a.md": "x\n"}), nil, "@@ file a.md\n@@ old\nHUHUNK\n@@ new\nok\n")
	if code != exitNoMatch || strings.Contains(errOut, "warning") {
		t.Errorf("an old that misses: exit %d, stderr %q", code, errOut)
	}
}

// The skill, the README and "hunk format" each name the mistyped closer
// beside the rule for the exact one, and say the report warns of it: a
// warning nobody has been told to read is one that gets skimmed.
func TestTheDocsSayAMistypedCloserIsWarnedOf(t *testing.T) {
	const reach = 1200
	for name, doc := range formatDocuments(t) {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(doc, "HUHUNK") {
				t.Fatal("never names a mistyped closer such as HUHUNK")
			}
			// Any mention will do, so long as one sits beside the rule.
			for at, rest := 0, doc; ; {
				i := strings.Index(rest, "HUHUNK")
				if i < 0 {
					t.Errorf("no mention of HUHUNK has %q, %q and %q within %d bytes of it",
						"exactly", "delimiter", "warning", reach)
					return
				}
				at += i
				near := doc[max(0, at-reach):min(len(doc), at+reach)]
				if strings.Contains(near, "exactly") && strings.Contains(near, "delimiter") && strings.Contains(near, "warning") {
					return
				}
				at += len("HUHUNK")
				rest = doc[at:]
			}
		})
	}
}

var reTenths = regexp.MustCompile(`\(\d+\.\ds\)`)

// --each warns of each patch's lines under the patch's name, after the whole
// run, on every end the run has, and in --json beside the failures' patch.
func TestEachWarnsOfAMistypedDelimiter(t *testing.T) {
	notes := map[string]string{"notes.txt": "one\ntwo\n"}
	setup := func(t *testing.T) (root, p1, p2 string) {
		root = cliTree(t, notes)
		dir := t.TempDir()
		p1, p2 = filepath.Join(dir, "p1.hunk"), filepath.Join(dir, "p2.hunk")
		must(t, os.WriteFile(p1, []byte(strayPatch), 0o644))
		must(t, os.WriteFile(p2, []byte("@@ file notes.txt\n@@ old\none\n@@ new\nONE\n"), 0o644))
		return root, p1, p2
	}
	const try = `grep -q "^two$" notes.txt`
	warnFor := func(p1 string) string {
		return "warning: " + p1 + " line 6 is \"HUHUNK\", which looks like a mistyped heredoc delimiter; " +
			"it is payload for notes.txt, so if it was meant to end the heredoc, take it out of the patch\n"
	}

	t.Run("a run", func(t *testing.T) {
		root, p1, p2 := setup(t)
		before := snapshot(t, root)
		code, out, errOut := warnCLI(t, root, []string{"--try", try, "--each", p1, p2}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d\n%s%s", code, errOut, out)
		}
		if !strings.HasSuffix(errOut, "\n"+warnFor(p1)) {
			t.Errorf("stderr does not end in a blank line and the warning:\n%s", errOut)
		}
		if d := snapshotDiff(before, snapshot(t, root)); len(d) > 0 {
			t.Errorf("the tree changed: %v", d)
		}
		golden(t, "cli-each-mistyped-delimiter", reTenths.ReplaceAllString(strings.ReplaceAll(errOut+out, filepath.Dir(p1)+string(filepath.Separator), ""), "(0.0s)"))
	})

	t.Run("a clean run under --quiet prints the warning alone", func(t *testing.T) {
		root, p1, _ := setup(t)
		code, out, errOut := warnCLI(t, root, []string{"--quiet", "--try", try, "--each", p1}, "")
		if code != exitOK || out != "" || errOut != warnFor(p1) {
			t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
		}
	})

	t.Run("a dry run", func(t *testing.T) {
		root, p1, p2 := setup(t)
		code, out, errOut := warnCLI(t, root, []string{"--dry-run", "--try", try, "--each", p1, p2}, "")
		if code != exitOK || !strings.HasPrefix(out, p1) || errOut != "\n"+warnFor(p1) {
			t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
		}
	})

	t.Run("a run refused because another patch does not parse", func(t *testing.T) {
		root, p1, p2 := setup(t)
		must(t, os.WriteFile(p2, []byte("not a patch\n"), 0o644))
		code, _, errOut := warnCLI(t, root, []string{"--try", try, "--each", p1, p2}, "")
		if code != exitUsage || !strings.HasPrefix(errOut, "hunk: 1 of 2 patches could not be read") ||
			!strings.HasSuffix(errOut, "\n"+warnFor(p1)) {
			t.Errorf("exit %d\nstderr %s", code, errOut)
		}
	})

	t.Run("a run refused because the command fails with no patch", func(t *testing.T) {
		root, p1, _ := setup(t)
		code, _, errOut := warnCLI(t, root, []string{"--try", "false", "--each", p1}, "")
		if code != exitUsage || !strings.HasSuffix(errOut, "\n"+warnFor(p1)) {
			t.Errorf("exit %d\nstderr %s", code, errOut)
		}
	})

	t.Run("--json", func(t *testing.T) {
		root, p1, p2 := setup(t)
		code, out, _ := warnCLI(t, root, []string{"--json", "--try", try, "--each", p1, p2}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d", code)
		}
		var r struct{ Warnings []map[string]any }
		must(t, json.Unmarshal([]byte(out), &r))
		if len(r.Warnings) != 1 {
			t.Fatalf("%s", out)
		}
		w := r.Warnings[0]
		if w["patch"] != p1 || w["patch_line"] != 6.0 || w["path"] != "notes.txt" ||
			w["message"] != strings.TrimSuffix(strings.TrimPrefix(warnFor(p1), "warning: "), "\n") {
			t.Errorf("warning %v", w)
		}
	})
}
