package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The warnings that follow a report, one per line, without their prefix.
func slipWarningsIn(stderr string) []string {
	var out []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if w, ok := strings.CutPrefix(line, "warning: "); ok {
			out = append(out, w)
		}
	}
	return out
}

// TestASlipIsWarnedOf (#111): a replacement whose ends disagree about a
// newline is warned of after the report, exit unchanged, by what it left:
// an empty line, a joined line, a file with no final newline. Every row is
// applied, so each remedy is the file's. The rows with no warning are the
// shapes that mean what they say, and the ones whose effect the batch's end
// undoes.
func TestASlipIsWarnedOf(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		patch string
		warn  []string
		want  map[string]string // the files afterwards
	}{
		{
			"#111: a blank line under each new text",
			map[string]string{"a.txt": "a\nb\nc\n"},
			"@@ file a.txt\n@@ old\na\n@@ new\nA\n\n@@ old\nb\n@@ new\nB\n\n",
			[]string{
				`hunk 1 (patch line 2) leaves an empty line after "A" in a.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`,
				`hunk 2 (patch line 7) leaves an empty line after "B" in a.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`,
			},
			map[string]string{"a.txt": "A\n\nB\n\nc\n"},
		},
		{
			"a CRLF file, whose newline is two bytes",
			map[string]string{"a.txt": "a\r\nb\r\n"},
			"@@ file a.txt\n@@ old\na\n@@ new\nA\n\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line after "A" in a.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "A\r\n\r\nb\r\n"},
		},
		{
			"the new text on two lines quotes its last",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\na\n@@ new\nA\nA2\n\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line after "A2" in a.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "A\nA2\n\nb\n"},
		},
		{
			"a match inside a line quotes the whole line",
			map[string]string{"a.txt": "x = 1\n"},
			"@@ file a.txt\n@@ old\n1\n@@ new\n2\n\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line after "x = 2" in a.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "x = 2\n\n"},
		},
		{
			"a new text that is one newline leaves it after a blank line",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\nb\n@@ new\n\n\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line after a blank line in a.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "a\n\n\n"},
		},
		{
			"the joined line",
			map[string]string{"a.txt": "a\nb\nc\n"},
			"@@ file a.txt\n@@ old\na\n\n@@ new\nA\n",
			[]string{`hunk 1 (patch line 2) joins the next line onto its replacement in a.txt, making "Ab": its old text ends in a newline and its new text does not; if that was not meant, put the line break back in a.txt`},
			map[string]string{"a.txt": "Ab\nc\n"},
		},
		{
			"joined, from inside a line",
			map[string]string{"a.txt": "x = 1\ny\n"},
			"@@ file a.txt\n@@ old\n1\n\n@@ new\n2\n",
			[]string{`hunk 1 (patch line 2) joins the next line onto its replacement in a.txt, making "x = 2y": its old text ends in a newline and its new text does not; if that was not meant, put the line break back in a.txt`},
			map[string]string{"a.txt": "x = 2y\n"},
		},
		{
			"joined, in a CRLF file, quoted without the CR",
			map[string]string{"a.txt": "a\r\nb\r\n"},
			"@@ file a.txt\n@@ old\na\n\n@@ new\nA\n",
			[]string{`hunk 1 (patch line 2) joins the next line onto its replacement in a.txt, making "Ab": its old text ends in a newline and its new text does not; if that was not meant, put the line break back in a.txt`},
			map[string]string{"a.txt": "Ab\r\n"},
		},
		{
			"joined, after a new text on two lines",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\na\n\n@@ new\nA\nZ\n",
			[]string{`hunk 1 (patch line 2) joins the next line onto its replacement in a.txt, making "Zb": its old text ends in a newline and its new text does not; if that was not meant, put the line break back in a.txt`},
			map[string]string{"a.txt": "A\nZb\n"},
		},
		{
			"the final newline taken away",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\nb\n\n@@ new\nB\n",
			[]string{`hunk 1 (patch line 2) leaves a.txt with no final newline: its old text ends in a newline at the end of the file and its new text does not; if that was not meant, end a.txt in a newline again`},
			map[string]string{"a.txt": "a\nB"},
		},
		{
			"@@ old x3, each",
			map[string]string{"a.txt": "x = 1\ny = 1\nz = 1\n"},
			"@@ file a.txt\n@@ old x3\n1\n@@ new\n2\n\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line after each of its 3 replacements in a.txt, the first after "x = 2": its new text ends in a newline and its old text does not; if that was not meant, remove the empty lines from a.txt`},
			map[string]string{"a.txt": "x = 2\n\ny = 2\n\nz = 2\n\n"},
		},
		{
			"@@ old x2, one at the end of a file that had no final newline",
			map[string]string{"a.txt": "a = 1\nb = 1"},
			"@@ file a.txt\n@@ old x2\n1\n@@ new\n2\n\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line after 1 of its 2 replacements in a.txt, the first after "a = 2": its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "a = 2\n\nb = 2\n"},
		},
		{
			"@@ old x3, two joined and the last at the end of the file",
			map[string]string{"a.txt": "a\nb\na\nc\na\n"},
			"@@ file a.txt\n@@ old x3\na\n\n@@ new\nA\n",
			[]string{
				`hunk 1 (patch line 2) joins the next line onto 2 of its 3 replacements in a.txt, the first making "Ab": its old text ends in a newline and its new text does not; if that was not meant, put the line breaks back in a.txt`,
				`hunk 1 (patch line 2) leaves a.txt with no final newline: its old text ends in a newline at the end of the file and its new text does not; if that was not meant, end a.txt in a newline again`,
			},
			map[string]string{"a.txt": "Ab\nAc\nA"},
		},
		{
			// The quote is read off the bytes the hunk left: the first
			// replacement is on the same line, before it.
			"@@ old x2, two on one line",
			map[string]string{"a.txt": "1 1\n"},
			"@@ file a.txt\n@@ old x2\n1\n@@ new\n2\n\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line after 1 of its 2 replacements in a.txt, the first after " 2": its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "2\n 2\n\n"},
		},
		{
			// The third is the one, and the two before it moved it 6 bytes,
			// past the start of its line.
			"@@ old x3, the quote found past the replacements before it",
			map[string]string{"a.txt": "a1 b1 c1\n"},
			"@@ file a.txt\n@@ old x3\n1\n@@ new\n222\n\n",
			[]string{`hunk 1 (patch line 2) leaves an empty line after 1 of its 3 replacements in a.txt, the first after " c222": its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt`},
			map[string]string{"a.txt": "a222\n b222\n c222\n\n"},
		},
		{
			"two files, in hunk order",
			map[string]string{"a.txt": "a\nb\n", "b.txt": "a\nb\n"},
			"@@ file b.txt\n@@ old\na\n@@ new\nA\n\n@@ file a.txt\n@@ old\na\n\n@@ new\nA\n",
			[]string{
				`hunk 1 (patch line 2) leaves an empty line after "A" in b.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from b.txt`,
				`hunk 2 (patch line 8) joins the next line onto its replacement in a.txt, making "Ab": its old text ends in a newline and its new text does not; if that was not meant, put the line break back in a.txt`,
			},
			map[string]string{"a.txt": "Ab\n", "b.txt": "A\n\nb\n"},
		},

		// No warning.
		{
			"at the end of a file with no final newline, which gains one",
			map[string]string{"a.txt": "a\nb"},
			"@@ file a.txt\n@@ old\nb\n@@ new\nB\n\n",
			nil,
			map[string]string{"a.txt": "a\nB\n"},
		},
		{
			"inside a line, which the new text breaks",
			map[string]string{"a.txt": "a b\n"},
			"@@ file a.txt\n@@ old\na\n@@ new\nA\n\n",
			nil,
			map[string]string{"a.txt": "A\n b\n"},
		},
		{
			"an empty line added on purpose: both ends have a newline",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\na\n\n@@ new\nA\n\n\n",
			nil,
			map[string]string{"a.txt": "A\n\nb\n"},
		},
		{
			"neither end has one",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\na\n@@ new\nA\n",
			nil,
			map[string]string{"a.txt": "A\nb\n"},
		},
		{
			"lines deleted, with an empty new text",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\na\n\n@@ new\n",
			nil,
			map[string]string{"a.txt": "b\n"},
		},
		{
			// The newline after the first match is the start of the second,
			// which takes it: found by FuzzASlipSaysWhatTheEditDid.
			"@@ old x2, the second match taking the newline after the first",
			map[string]string{"a.txt": "\na\na"},
			"@@ file a.txt\n@@ old x2\n\na\n@@ new\n0\n\n",
			nil,
			map[string]string{"a.txt": "0\n0\n"},
		},
		{
			"a later append gives the final newline back",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\nb\n\n@@ new\nB\n@@ append a.txt\nc\n",
			nil,
			map[string]string{"a.txt": "a\nB\nc\n"},
		},
		{
			"a later replace gives it back",
			map[string]string{"a.txt": "a\nb\n"},
			"@@ file a.txt\n@@ old\nb\n\n@@ new\nB\n@@ old\nB\n@@ new\nB\n\n",
			nil,
			map[string]string{"a.txt": "a\nB\n"},
		},
		{
			"a create the replace takes it from, whose row says it",
			nil,
			"@@ create n.txt\nhi\n\n@@ old\nhi\n\n@@ new\nbye\n",
			nil,
			map[string]string{"n.txt": "bye"},
		},
		{
			"a file the batch then deletes",
			map[string]string{"a.txt": "a\nb\n", "b.txt": "b\n"},
			"@@ file a.txt\n@@ old\na\n@@ new\nA\n\n@@ old\nb\n\n@@ new\nB\n@@ delete a.txt\n",
			nil,
			map[string]string{"b.txt": "b\n"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			code, out, errOut := runCLI(t, root, nil, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			got := slipWarningsIn(errOut)
			if strings.Join(got, "\n") != strings.Join(c.warn, "\n") {
				t.Errorf("warnings\n got %q\nwant %q", got, c.warn)
			}
			if strings.Contains(out, "warning") {
				t.Errorf("a warning went to stdout: %s", out)
			}
			for name, want := range c.want {
				if b := readFile(t, root, name); b != want {
					t.Errorf("%s = %q, want %q", name, b, want)
				}
			}
			if c.warn == nil && errOut != "" {
				t.Errorf("stderr %q", errOut)
			}
		})
	}
}

// The patch the outcome tests apply: #111's shape, one hunk.
const slipPatch = "@@ file a.txt\n@@ old\na\n@@ new\nA\n\n"

const (
	slipApplied    = `warning: hunk 1 (patch line 2) leaves an empty line after "A" in a.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from a.txt` + "\n"
	slipNotApplied = `warning: hunk 1 (patch line 2) leaves an empty line after "A" in a.txt: its new text ends in a newline and its old text does not; if that was not meant, take out the blank line under its new text` + "\n"
)

// TestASlipsRemedyFollowsTheOutcome: the warning is the same whatever the
// command made of the batch, its remedy the file's when the batch is on
// disk and the patch's when it is not, and it follows the report, which is
// otherwise the same, after a blank line. A batch refused at exit 2 matched
// nothing and is not warned of.
func TestASlipsRemedyFollowsTheOutcome(t *testing.T) {
	files := map[string]string{"a.txt": "a\nb\n"}
	for _, c := range []struct {
		name string
		args []string
		exit int
		warn string // "" for none
	}{
		{"applied", nil, exitOK, slipApplied},
		{"a dry run", []string{"--dry-run"}, exitOK, slipNotApplied},
		{"a verify that passes", []string{"--verify", "true"}, exitOK, slipApplied},
		{"a verify that fails", []string{"--verify", "echo nope; false"}, exitVerifyFailed, slipNotApplied},
		{"--keep-on-fail", []string{"--verify", "false", "--keep-on-fail"}, exitVerifyFailed, slipApplied},
		{"--try that passes", []string{"--try", "true"}, exitOK, slipNotApplied},
		{"--try that fails", []string{"--try", "echo caught; exit 7"}, 7, slipNotApplied},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, cleanOut, cleanErr := runCLI(t, cliTree(t, files), c.args, "@@ file a.txt\n@@ old\na\n@@ new\nA\n")
			root := cliTree(t, files)
			code, out, errOut := runCLI(t, root, c.args, slipPatch)
			if code != c.exit {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.exit, out, errOut)
			}
			tenths := func(s string) string { return reTenths.ReplaceAllString(s, "(0.0s)") }
			// The patch that agrees adds no line, so the counts are the same.
			if tenths(out) != tenths(cleanOut) {
				t.Errorf("stdout is not the clean patch's\n--- got ---\n%s--- clean ---\n%s", out, cleanOut)
			}
			report, warning, found := strings.Cut(errOut, "warning: ")
			if !found {
				t.Fatalf("no warning on stderr:\n%s", errOut)
			}
			gap := "\n"
			if cleanOut+cleanErr == "" {
				gap = ""
			}
			if tenths(report) != tenths(cleanErr)+gap {
				t.Errorf("stderr before the warning is not the clean patch's report and a blank line\n--- got ---\n%q\n--- clean ---\n%q", report, cleanErr)
			}
			if "warning: "+warning != c.warn {
				t.Errorf("warning\n got %q\nwant %q", "warning: "+warning, c.warn)
			}
			if c.args != nil && c.args[0] == "--try" && !strings.HasPrefix(out+errOut, "tried") {
				t.Errorf("a --try report has to start tried (§4.1):\n%s%s", out, errOut)
			}
			onDisk := readFile(t, root, "a.txt") == "A\n\nb\n"
			if onDisk != (c.warn == slipApplied) {
				t.Errorf("the empty line is on disk: %v, the warning says %v", onDisk, !onDisk)
			}
		})
	}

	t.Run("a batch refused at exit 2 is not warned of", func(t *testing.T) {
		code, _, errOut := runCLI(t, cliTree(t, files), nil, slipPatch+"@@ old\nnot there\n@@ new\nx\n")
		if code != exitNoMatch || strings.Contains(errOut, "warning") {
			t.Errorf("exit %d\nstderr %q", code, errOut)
		}
	})

	t.Run("under --quiet it is all that prints, with no blank line before it", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliTree(t, files), []string{"--quiet"}, slipPatch)
		if code != exitOK || out != "" || errOut != slipApplied {
			t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
		}
	})

	t.Run("before a stray, both after the report", func(t *testing.T) {
		code, _, errOut := runCLI(t, cliTree(t, files), []string{allowMistypedFlag}, "@@ file a.txt\n@@ old\na\n@@ new\nA\nHUHUNK\n\n")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		w := slipWarningsIn(errOut)
		if len(w) != 2 || !strings.HasPrefix(w[0], `hunk 1 (patch line 2) leaves an empty line after "HUHUNK" in a.txt`) ||
			!strings.HasPrefix(w[1], `patch line 6 is "HUHUNK"`) {
			t.Errorf("warnings %q", w)
		}
		// The report is on stdout, so the blank line opens stderr.
		if !strings.HasPrefix(errOut, "\nwarning: hunk 1") || strings.Contains(errOut, "\n\nwarning: patch line") {
			t.Errorf("one blank line, before the first warning:\n%s", errOut)
		}
	})

	t.Run("under --marker", func(t *testing.T) {
		code, _, errOut := runCLI(t, cliTree(t, files), []string{"--marker", "%%"}, strings.ReplaceAll(slipPatch, "@@ ", "%% "))
		if code != exitOK || !strings.HasSuffix(errOut, slipApplied) {
			t.Errorf("exit %d\nstderr %q", code, errOut)
		}
	})
}

// slipJSON is the part of --json a slip test reads.
type slipJSON struct {
	OK       bool `json:"ok"`
	Exit     int  `json:"exit"`
	Warnings []struct {
		Kind       string  `json:"kind"`
		Patch      *string `json:"patch"`
		Hunk       int     `json:"hunk"`
		PatchLine  int     `json:"patch_line"`
		Path       string  `json:"path"`
		Text       string  `json:"text"`
		LinesAfter *int    `json:"lines_after"`
		Message    string  `json:"message"`
	} `json:"warnings"`
}

// TestASlipInJSON: --json carries each slip among the warnings, before the
// strays, with its kind, its hunk and patch line, its file, the line it
// left and the text report's sentence, and no lines_after, which is a
// stray's.
func TestASlipInJSON(t *testing.T) {
	files := map[string]string{"a.txt": "a\nb\nc\nd\n"}
	patch := "@@ file a.txt\n@@ old\na\n@@ new\nA\n\n@@ old\nb\n\n@@ new\nB\n@@ old\nd\n\n@@ new\nD\nHUHUNK\n"
	for _, c := range []struct {
		name   string
		args   []string
		onDisk bool
	}{
		{"applied", nil, true},
		{"a dry run", []string{"--dry-run"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, files)
			code, out, errOut := runCLI(t, root, append([]string{"--json", allowMistypedFlag}, c.args...), patch)
			if code != exitOK || errOut != "" {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			var r slipJSON
			must(t, json.Unmarshal([]byte(out), &r))
			if !r.OK || len(r.Warnings) != 4 {
				t.Fatalf("ok %v, %d warnings: %s", r.OK, len(r.Warnings), out)
			}
			for i, want := range []struct {
				kind       string
				hunk, line int
				text       string
			}{
				{"empty_line", 1, 2, "A"},
				{"joined_line", 2, 7, "Bc"},
				{"no_final_newline", 3, 12, "HUHUNK"},
				{"mistyped_delimiter", 0, 17, "HUHUNK"},
			} {
				w := r.Warnings[i]
				if w.Kind != want.kind || w.Hunk != want.hunk || w.PatchLine != want.line || w.Path != "a.txt" || w.Text != want.text || w.Patch != nil {
					t.Errorf("warning %d = %+v, want %+v", i+1, w, want)
				}
				if (w.LinesAfter != nil) != (want.kind == "mistyped_delimiter") {
					t.Errorf("warning %d's lines_after = %v", i+1, w.LinesAfter)
				}
			}
			// The messages are the text report's, as the outcome has them.
			_, _, text := runCLI(t, cliTree(t, files), append([]string{allowMistypedFlag}, c.args...), patch)
			if got := slipWarningsIn(text); len(got) != 4 {
				t.Fatalf("text warnings %q", got)
			} else {
				for i, w := range r.Warnings {
					if w.Message != got[i] {
						t.Errorf("warning %d's message\n got %q\nwant %q", i+1, w.Message, got[i])
					}
				}
			}
			fix := "take out the blank line under its new text"
			if c.onDisk {
				fix = "remove the empty line from a.txt"
			}
			if !strings.HasSuffix(r.Warnings[0].Message, fix) {
				t.Errorf("message %q, want it to end %q", r.Warnings[0].Message, fix)
			}
		})
	}

	t.Run("no warnings, no key", func(t *testing.T) {
		_, out, _ := runCLI(t, cliTree(t, files), []string{"--json"}, "@@ file a.txt\n@@ old\na\n@@ new\nA\n")
		if strings.Contains(out, "warnings") {
			t.Errorf("%s", out)
		}
	})
}

// TestASlipUnderEach: --each preflights every patch, and a patch's slips are
// warned of under its name after the run, before its strays, with the
// patch's remedy, since every patch is put back. A break set's hunks are
// numbered within their patch and named by the line of the file that holds
// them.
func TestASlipUnderEach(t *testing.T) {
	files := map[string]string{"a.txt": "a\nb\n"}
	// Passes on the tree as it is, and every patch breaks it.
	const try = "grep -qx a a.txt && grep -qx b a.txt"
	dir := t.TempDir()
	p1 := filepath.Join(dir, "p1.hunk")
	must(t, os.WriteFile(p1, []byte(slipPatch), 0o644))
	set := filepath.Join(dir, "set.hunk")
	must(t, os.WriteFile(set, []byte("@@ patch one\n@@ file a.txt\n@@ old\nb\n@@ new\nB\n@@ patch two\n@@ file a.txt\n@@ old\na\n\n@@ new\nA\n"), 0o644))

	code, out, errOut := runCLI(t, cliTree(t, files), []string{"--try", try, "--each", p1, set}, "")
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	want := []string{
		p1 + ` hunk 1 (line 2) leaves an empty line after "A" in a.txt: its new text ends in a newline and its old text does not; if that was not meant, take out the blank line under its new text`,
		set + `:two hunk 1 (line 9) joins the next line onto its replacement in a.txt, making "Ab": its old text ends in a newline and its new text does not; if that was not meant, leave a blank line under its new text, as under its old text`,
	}
	if got := slipWarningsIn(errOut); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(errOut, "(0.0s)\n\nwarning: ") && !strings.Contains(errOut, "s)\n\nwarning: ") {
		t.Errorf("no blank line between the summary and the warnings:\n%s", errOut)
	}

	t.Run("json", func(t *testing.T) {
		code, out, _ := runCLI(t, cliTree(t, files), []string{"--json", "--try", try, "--each", p1, set}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		var r slipJSON
		must(t, json.Unmarshal([]byte(out), &r))
		var raw struct {
			Warnings []map[string]any `json:"warnings"`
		}
		must(t, json.Unmarshal([]byte(out), &raw))
		if len(r.Warnings) != 2 || len(raw.Warnings) != 2 {
			t.Fatalf("warnings: %s", out)
		}
		for i, w := range r.Warnings {
			name := []string{p1, set + ":two"}[i]
			if raw.Warnings[i]["patch"] != name || w.Message != want[i] || w.Hunk != 1 {
				t.Errorf("warning %d = %v", i+1, raw.Warnings[i])
			}
		}
	})

	t.Run("a refused run still names the patch that matched", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.hunk")
		must(t, os.WriteFile(bad, []byte("@@ file a.txt\n@@ old\nnot there\n@@ new\nx\n"), 0o644))
		code, _, errOut := runCLI(t, cliTree(t, files), []string{"--try", try, "--each", p1, bad}, "")
		if code != exitNoMatch || !strings.HasSuffix(errOut, "\nwarning: "+want[0]+"\n") {
			t.Errorf("exit %d\nstderr %s", code, errOut)
		}
	})
}

// FuzzASlipSaysWhatTheEditDid: whatever the file and the payloads, the
// warnings are a value, never a panic, and each one's claim is true of the
// bytes the replacement left: an empty line after the line it quotes, a
// line it quotes, or a last line it quotes with no newline after it. None
// is warned of where the ends agree, and none counts more replacements than
// the hunk made.
func FuzzASlipSaysWhatTheEditDid(f *testing.F) {
	for _, c := range [][3]string{
		{"a\nb\nc\n", "a", "A\n"},
		{"a\nb\nc\n", "a\n", "A"},
		{"a\nb\n", "b\n", "B"},
		{"a\nb", "b", "B\n"},
		{"a\r\nb\r\n", "a", "A\r\n"},
		{"a\r\nb\r\n", "a\r\n", "A"},
		{"a\r\nb\r\n", "a", "A\n"},
		{"x = 1\ny = 1\n", "1", "2\n"},
		{"1 1\n", "1", "2\n"},
		{"a1 b1 c1\n", "1", "222\n"},
		{"\na\na", "\na", "0\n"},
		{"a b\n", "a", "A\n"},
		{"a\nb\n", "b", "\n"},
		{"a\nb\na\n", "a\n", "A"},
		{"a\na\n", "a\n", "A\r"},
	} {
		f.Add([]byte(c[0]), []byte(c[1]), []byte(c[2]))
	}
	f.Fuzz(func(t *testing.T, start, old, repl []byte) {
		n := bytes.Count(start, old)
		if len(old) == 0 || n == 0 {
			return
		}
		tree, _ := fixture(t, map[string]string{"a.txt": string(start)})
		p := &Patch{Hunks: []Hunk{{Op: OpReplace, Path: "a.txt", Line: 1, Count: n, Old: old, New: repl}}}
		r, err := NewTxn(tree, Options{EOL: EOLStrict}).Preview(p)
		if err != nil {
			return // copies that overlap, which are refused (§3.4)
		}
		got := bytes.Replace(start, old, repl, n)
		var lines []string
		for l := range strings.SplitSeq(string(got), "\n") {
			lines = append(lines, strings.TrimSuffix(l, "\r"))
		}
		// The last is what follows the final newline, which is a line only
		// when it is not empty.
		real := len(lines) - 1
		if lines[real] != "" {
			real++
		}
		agree := bytes.HasSuffix(old, lf) == bytes.HasSuffix(repl, lf) || len(repl) == 0
		if agree && len(r.Slips) > 0 {
			t.Fatalf("ends that agree were warned of: %+v\nstart %q old %q new %q", r.Slips, start, old, repl)
		}
		kinds := map[SlipKind]bool{}
		for _, s := range r.Slips {
			if s.Hunk != 1 || s.Line != 1 || s.Path != "a.txt" || s.Of != n || s.Count < 1 || s.Count > n || kinds[s.Kind] {
				t.Fatalf("slip %+v\nstart %q old %q new %q", s, start, old, repl)
			}
			kinds[s.Kind] = true
			text, found := string(s.Text), false
			switch s.Kind {
			case SlipEmptyLine:
				for i := 0; i+1 < real; i++ {
					found = found || (lines[i] == text && lines[i+1] == "")
				}
			case SlipJoinedLine:
				for i := range real {
					found = found || lines[i] == text
				}
			case SlipNoFinalNewline:
				found = !bytes.HasSuffix(got, lf) && lines[len(lines)-1] == text
			}
			if !found {
				t.Errorf("slip %v quoting %q is not in what the edit left, %q\nstart %q old %q new %q", s.Kind, text, got, start, old, repl)
			}
		}
	})
}

// The goldens: one batch with every kind, applied; the same in --json; and
// --each.
func TestASlipGoldens(t *testing.T) {
	files := map[string]string{"e.txt": "a\nb\nc\n", "j.txt": "a\nb\nc\n", "z.txt": "a\nb\n", "m.txt": "x = 1\ny = 1\nz = 1\n"}
	patch := "@@ file e.txt\n@@ old\na\n@@ new\nA\n\n@@ old\nb\n@@ new\nB\n\n" +
		"@@ file j.txt\n@@ old\na\n\n@@ new\nA\n" +
		"@@ file z.txt\n@@ old\nb\n\n@@ new\nB\n" +
		"@@ file m.txt\n@@ old x3\n1\n@@ new\n2\n\n"
	t.Run("text", func(t *testing.T) {
		_, out, errOut := runCLI(t, cliTree(t, files), nil, patch)
		golden(t, "cli-slip", out+errOut)
	})
	t.Run("dry run", func(t *testing.T) {
		_, out, errOut := runCLI(t, cliTree(t, files), []string{"--dry-run"}, patch)
		golden(t, "cli-slip-dry-run", out+errOut)
	})
	t.Run("json", func(t *testing.T) {
		_, out, _ := runCLI(t, cliTree(t, files), []string{"--json"}, patch)
		golden(t, "cli-slip-json", out)
	})
	t.Run("each", func(t *testing.T) {
		p1 := filepath.Join(t.TempDir(), "p1.hunk")
		must(t, os.WriteFile(p1, []byte(slipPatch), 0o644))
		// One buffer for both, so the golden is in the order a terminal
		// shows: the summary on stdout, then the warning on stderr.
		var both bytes.Buffer
		root := cliTree(t, map[string]string{"a.txt": "a\nb\n"})
		if code := cli([]string{"--root", root, "--try", "grep -qx a a.txt", "--each", p1}, strings.NewReader(""), &both, &both); code != exitOK {
			t.Fatalf("exit %d: %s", code, both.String())
		}
		golden(t, "cli-each-slip", reTenths.ReplaceAllString(strings.ReplaceAll(both.String(), filepath.Dir(p1)+string(filepath.Separator), ""), "(0.0s)"))
	})
}
