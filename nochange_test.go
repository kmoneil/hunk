package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two sentences for a hunk that changed nothing (#118), without the
// "warning: " prefix.
func sameText(hunk, line int, path string) string {
	return fmt.Sprintf("hunk %d (patch line %d) changes nothing in %s: its old and new text are the same; if a change was meant, write it under @@ new", hunk, line, path)
}

func onlyEndings(hunk, line int, path string) string {
	return fmt.Sprintf("hunk %d (patch line %d) changes nothing in %s: its old and new text differ only in line endings, and --eol auto writes the file's; to change a line's ending, pass --eol strict", hunk, line, path)
}

// TestAHunkThatChangesNothingIsWarnedOf (#118): a replace that matched and
// changed nothing applies at exit 0, as before, with the report it had, and
// is warned of after it. Two causes, said apart: old and new the same, and
// old and new that differ only in line endings, which --eol auto writes as
// the file's. The rows with no warning are an empty append or prepend,
// decided on 2026-10-26, and the same line-ending change under --eol strict,
// which makes it.
func TestAHunkThatChangesNothingIsWarnedOf(t *testing.T) {
	for _, c := range []struct {
		name   string
		files  map[string]string
		args   []string
		patch  string
		warn   []string
		report string            // stdout
		want   map[string]string // the files afterwards; "" for gone
	}{
		{
			"#118: old and new the same",
			map[string]string{"f.txt": "a\nb\n"},
			nil,
			"@@ file f.txt\n@@ old\na\n@@ new\na\n",
			[]string{sameText(1, 2, "f.txt")},
			"0 files, 1 hunk, +0 -0\n",
			map[string]string{"f.txt": "a\nb\n"},
		},
		{
			"among hunks that changed the file, whose row hid it",
			map[string]string{"f.txt": "a\nb\nc\n"},
			nil,
			"@@ file f.txt\n@@ old\na\n@@ new\nA\n@@ old\nb\n@@ new\nb\n@@ old\nc\n@@ new\nC\n",
			[]string{sameText(2, 6, "f.txt")},
			"M f.txt +2 -2\n1 file, 3 hunks, +2 -2\n",
			map[string]string{"f.txt": "A\nb\nC\n"},
		},
		{
			"@@ old x2",
			map[string]string{"a.txt": "x\ny\nx\n"},
			nil,
			"@@ file a.txt\n@@ old x2\nx\n@@ new\nx\n",
			[]string{sameText(1, 2, "a.txt")},
			"0 files, 1 hunk, +0 -0\n",
			map[string]string{"a.txt": "x\ny\nx\n"},
		},
		{
			"old on two lines, ending in a newline",
			map[string]string{"a.txt": "a\nb\nc\n"},
			nil,
			"@@ file a.txt\n@@ old\na\nb\n\n@@ new\na\nb\n\n",
			[]string{sameText(1, 2, "a.txt")},
			"0 files, 1 hunk, +0 -0\n",
			map[string]string{"a.txt": "a\nb\nc\n"},
		},
		{
			"after a create in the same batch",
			nil, nil,
			"@@ create n.txt\nhi\n\n@@ old\nhi\n@@ new\nhi\n",
			[]string{sameText(2, 4, "n.txt")},
			"A n.txt +1 -0\n1 file, 2 hunks, +1 -0\n",
			map[string]string{"n.txt": "hi\n"},
		},
		{
			// What it was written to do was not done, whatever came after.
			"in a file the batch then deletes",
			map[string]string{"a.txt": "a\n"},
			nil,
			"@@ file a.txt\n@@ old\na\n@@ new\na\n@@ delete a.txt\n",
			[]string{sameText(1, 2, "a.txt")},
			"D a.txt +0 -1\n1 file, 2 hunks, +0 -1\n",
			map[string]string{"a.txt": ""},
		},
		{
			"two files, in hunk order, with a slip",
			map[string]string{"a.txt": "a\nb\n", "b.txt": "a\nb\n"},
			nil,
			"@@ file b.txt\n@@ old\na\n@@ new\nA\n\n@@ old\nb\n@@ new\nb\n@@ file a.txt\n@@ old\na\n@@ new\na\n",
			[]string{
				`hunk 1 (patch line 2) leaves an empty line after "A" in b.txt: its new text ends in a newline and its old text does not; if that was not meant, remove the empty line from b.txt`,
				sameText(2, 7, "b.txt"),
				sameText(3, 12, "a.txt"),
			},
			"M b.txt +2 -1\n1 file, 3 hunks, +2 -1\n",
			map[string]string{"a.txt": "a\nb\n", "b.txt": "A\n\nb\n"},
		},
		{
			"a CRLF file, the line's ending changed to LF",
			map[string]string{"c.txt": "a\r\nb\r\n"},
			nil,
			"@@ file c.txt\n@@ old\na\r\n\n@@ new\na\n\n",
			[]string{onlyEndings(1, 2, "c.txt")},
			"0 files, 1 hunk, +0 -0\n",
			map[string]string{"c.txt": "a\r\nb\r\n"},
		},
		{
			"an LF file, the line's ending changed to CRLF",
			map[string]string{"a.txt": "a\nb\n"},
			nil,
			"@@ file a.txt\n@@ old\na\n\n@@ new\na\r\n\n",
			[]string{onlyEndings(1, 2, "a.txt")},
			"0 files, 1 hunk, +0 -0\n",
			map[string]string{"a.txt": "a\nb\n"},
		},
		{
			// Every line CRLF, as PowerShell pipes it: the CRs are the
			// patch's, and old and new are the same.
			"a CRLF patch",
			map[string]string{"a.txt": "a\nb\n"},
			nil,
			"@@ file a.txt\r\n@@ old\r\na\r\n@@ new\r\na\r\n",
			[]string{sameText(1, 2, "a.txt")},
			"0 files, 1 hunk, +0 -0\n",
			map[string]string{"a.txt": "a\nb\n"},
		},

		// No warning.
		{
			"--eol strict changes the line's ending",
			map[string]string{"c.txt": "a\r\nb\r\n"},
			[]string{"--eol", "strict"},
			"@@ file c.txt\n@@ old\na\r\n\n@@ new\na\n\n",
			nil,
			"M c.txt +1 -1\n1 file, 1 hunk, +1 -1\n",
			map[string]string{"c.txt": "a\nb\r\n"},
		},
		{
			"an empty append to a file that ends in a newline",
			map[string]string{"a.txt": "a\n"},
			nil,
			"@@ append a.txt\n",
			nil,
			"0 files, 1 hunk, +0 -0\n",
			map[string]string{"a.txt": "a\n"},
		},
		{
			"an empty prepend",
			map[string]string{"a.txt": "a\n"},
			nil,
			"@@ prepend a.txt\n",
			nil,
			"0 files, 1 hunk, +0 -0\n",
			map[string]string{"a.txt": "a\n"},
		},
		{
			"a hunk that changes a byte",
			map[string]string{"a.txt": "a\n"},
			nil,
			"@@ file a.txt\n@@ old\na\n@@ new\nb\n",
			nil,
			"M a.txt +1 -1\n1 file, 1 hunk, +1 -1\n",
			map[string]string{"a.txt": "b\n"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			code, out, errOut := runCLI(t, root, c.args, c.patch)
			if code != exitOK {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			got := slipWarningsIn(errOut)
			if strings.Join(got, "\n") != strings.Join(c.warn, "\n") {
				t.Errorf("warnings\n got %q\nwant %q", got, c.warn)
			}
			if out != c.report {
				t.Errorf("report\n got %q\nwant %q", out, c.report)
			}
			if c.warn == nil && errOut != "" {
				t.Errorf("stderr %q", errOut)
			}
			for name, want := range c.want {
				_, err := os.Stat(filepath.Join(root, name))
				if want == "" {
					if !os.IsNotExist(err) {
						t.Errorf("%s is still there: %v", name, err)
					}
					continue
				}
				if b := readFile(t, root, name); b != want {
					t.Errorf("%s = %q, want %q", name, b, want)
				}
			}
		})
	}
}

// The patch the outcome tests apply: a change, and #118's leftover after it.
const noChangePatch = "@@ file a.txt\n@@ old\na\n@@ new\nA\n@@ old\nb\n@@ new\nb\n"

// TestANoChangesRemedyIsThePatchs: the warning is the same sentence whatever
// the command made of the batch, since nothing was written for the hunk and
// what was meant has to go in the patch either way. It is the last thing on
// stderr, after a blank line. A batch refused at exit 2 is not warned of.
func TestANoChangesRemedyIsThePatchs(t *testing.T) {
	files := map[string]string{"a.txt": "a\nb\n"}
	warn := "warning: " + sameText(2, 6, "a.txt") + "\n"
	for _, c := range []struct {
		name string
		args []string
		exit int
	}{
		{"applied", nil, exitOK},
		{"a dry run", []string{"--dry-run"}, exitOK},
		{"a verify that passes", []string{"--verify", "true"}, exitOK},
		{"a verify that fails", []string{"--verify", "echo nope; false"}, exitVerifyFailed},
		{"--keep-on-fail", []string{"--verify", "false", "--keep-on-fail"}, exitVerifyFailed},
		{"--try that passes", []string{"--try", "true"}, exitOK},
		{"--try that fails", []string{"--try", "echo caught; exit 7"}, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := runCLI(t, cliTree(t, files), c.args, noChangePatch)
			if code != c.exit {
				t.Fatalf("exit %d, want %d\n%s%s", code, c.exit, out, errOut)
			}
			if w := slipWarningsIn(errOut); len(w) != 1 {
				t.Fatalf("warnings %q", w)
			}
			if !strings.HasSuffix(errOut, "\n"+warn) || strings.HasSuffix(errOut, "\n\n\n"+warn) {
				t.Errorf("the warning is not last on stderr after one blank line:\n%s", errOut)
			}
			if strings.Contains(out, "warning") {
				t.Errorf("a warning went to stdout: %s", out)
			}
			if c.args != nil && c.args[0] == "--try" && !strings.HasPrefix(out+errOut, "tried") {
				t.Errorf("a --try report has to start tried (§4.1):\n%s%s", out, errOut)
			}
		})
	}

	t.Run("a batch refused at exit 2 is not warned of", func(t *testing.T) {
		code, _, errOut := runCLI(t, cliTree(t, files), nil, noChangePatch+"@@ old\nnot there\n@@ new\nx\n")
		if code != exitNoMatch || strings.Contains(errOut, "warning") {
			t.Errorf("exit %d\nstderr %q", code, errOut)
		}
	})

	t.Run("under --quiet it is all that prints, with no blank line before it", func(t *testing.T) {
		code, out, errOut := runCLI(t, cliTree(t, files), []string{"--quiet"}, noChangePatch)
		if code != exitOK || out != "" || errOut != warn {
			t.Errorf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
		}
	})

	t.Run("before a stray", func(t *testing.T) {
		code, _, errOut := runCLI(t, cliTree(t, files), []string{allowMistypedFlag}, noChangePatch+"@@ old\nA\n@@ new\nA\nHUHUNK\n")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		w := slipWarningsIn(errOut)
		if len(w) != 2 || w[0] != sameText(2, 6, "a.txt") || !strings.HasPrefix(w[1], `patch line 14 is "HUHUNK"`) {
			t.Errorf("warnings %q", w)
		}
	})
}

// TestANoChangeInJSON: --json carries each hunk that changed nothing among
// the warnings, as "no_change" whichever the cause, with its hunk, patch
// line and file, its old text's first line, and the text report's sentence.
func TestANoChangeInJSON(t *testing.T) {
	files := map[string]string{"a.txt": "a\nb\nc\n", "c.txt": "x\r\ny\r\n"}
	patch := "@@ file a.txt\n@@ old\na\nb\n@@ new\na\nb\n@@ file c.txt\n@@ old\nx\r\n\n@@ new\nx\n\n"
	for _, args := range [][]string{nil, {"--dry-run"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			code, out, errOut := runCLI(t, cliTree(t, files), append([]string{"--json"}, args...), patch)
			if code != exitOK || errOut != "" {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			var r slipJSON
			must(t, json.Unmarshal([]byte(out), &r))
			if !r.OK || len(r.Warnings) != 2 {
				t.Fatalf("ok %v, %d warnings: %s", r.OK, len(r.Warnings), out)
			}
			_, _, text := runCLI(t, cliTree(t, files), args, patch)
			msgs := slipWarningsIn(text)
			for i, want := range []struct {
				hunk, line int
				path, text string
			}{
				{1, 2, "a.txt", "a"},
				{2, 9, "c.txt", "x"},
			} {
				w := r.Warnings[i]
				if w.Kind != "no_change" || w.Hunk != want.hunk || w.PatchLine != want.line || w.Path != want.path ||
					w.Text != want.text || w.Patch != nil || w.LinesAfter != nil {
					t.Errorf("warning %d = %+v, want %+v", i+1, w, want)
				}
				if len(msgs) != 2 || w.Message != msgs[i] {
					t.Errorf("warning %d's message %q, the text report's %q", i+1, w.Message, msgs)
				}
			}
			if r.Warnings[1].Message != onlyEndings(2, 9, "c.txt") {
				t.Errorf("message %q", r.Warnings[1].Message)
			}
		})
	}
}

// TestANoChangeUnderEach: --each preflights every patch, and a hunk in one
// that changed nothing is warned of under the patch's name after the run.
func TestANoChangeUnderEach(t *testing.T) {
	files := map[string]string{"a.txt": "a\nb\n"}
	p := filepath.Join(t.TempDir(), "p.hunk")
	must(t, os.WriteFile(p, []byte(noChangePatch), 0o644))
	want := p + " hunk 2 (line 6) changes nothing in a.txt: its old and new text are the same; if a change was meant, write it under @@ new"

	code, out, errOut := runCLI(t, cliTree(t, files), []string{"--try", "grep -qx a a.txt", "--each", p}, "")
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if got := slipWarningsIn(errOut); len(got) != 1 || got[0] != want {
		t.Errorf("warnings\n got %q\nwant %q", got, want)
	}

	code, out, _ = runCLI(t, cliTree(t, files), []string{"--json", "--try", "grep -qx a a.txt", "--each", p}, "")
	if code != exitOK {
		t.Fatalf("--json: exit %d: %s", code, out)
	}
	var r slipJSON
	must(t, json.Unmarshal([]byte(out), &r))
	if len(r.Warnings) != 1 || r.Warnings[0].Kind != "no_change" || r.Warnings[0].Patch == nil ||
		*r.Warnings[0].Patch != p || r.Warnings[0].Message != want {
		t.Errorf("warnings: %s", out)
	}
}

// FuzzANoChangeIsWhatTheHunkDid: whatever the file, the payloads and --eol,
// the warnings are a value, never a panic, and a hunk is warned of as
// changing nothing exactly when the batch left its file as it was. Its
// cause is "the same" exactly when the patch's bytes are, and "only line
// endings" never under --eol strict, and only where the two agree once each
// CRLF is an LF.
func FuzzANoChangeIsWhatTheHunkDid(f *testing.F) {
	for _, c := range []struct {
		start, old, repl string
		strict           bool
	}{
		{"a\nb\n", "a", "a", false},
		{"a\nb\n", "a", "A", false},
		{"a\r\nb\r\n", "a\r\n", "a\n", false},
		{"a\r\nb\r\n", "a\r\n", "a\n", true},
		{"a\nb\n", "a\n", "a\r\n", false},
		{"a\nb\n", "a\n", "a\r\r\n", false},
		{"x\nx\n", "x", "x", true},
		{"a\rb\n", "a\r", "a", false},
	} {
		f.Add([]byte(c.start), []byte(c.old), []byte(c.repl), c.strict)
	}
	f.Fuzz(func(t *testing.T, start, old, repl []byte, strict bool) {
		n := bytes.Count(start, old)
		if len(old) == 0 || n == 0 {
			return
		}
		opt := Options{EOL: EOLAuto}
		if strict {
			opt.EOL = EOLStrict
		}
		tree, _ := fixture(t, map[string]string{"a.txt": string(start)})
		p := &Patch{Hunks: []Hunk{{Op: OpReplace, Path: "a.txt", Line: 1, Count: n, Old: old, New: repl}}}
		r, err := NewTxn(tree, opt).Preview(p)
		if err != nil {
			return // under --eol auto the count can differ, and overlap is refused
		}
		var none []Slip
		for _, s := range r.Slips {
			if s.Kind == SlipNoChange || s.Kind == SlipLineEndings {
				none = append(none, s)
			}
		}
		changed := len(r.Files) > 0
		if len(none) > 1 || changed == (len(none) == 1) {
			t.Fatalf("changed %v, warned of %+v\nstart %q old %q new %q strict %v", changed, none, start, old, repl, strict)
		}
		if len(none) == 0 {
			return
		}
		s := none[0]
		first, _, _ := bytes.Cut(old, lf)
		if s.Hunk != 1 || s.Line != 1 || s.Path != "a.txt" || s.Count != n || s.Of != n ||
			string(s.Text) != string(bytes.TrimSuffix(first, cr)) {
			t.Fatalf("%+v\nstart %q old %q new %q", s, start, old, repl)
		}
		if (s.Kind == SlipNoChange) != bytes.Equal(old, repl) {
			t.Fatalf("kind %v, old %q new %q", s.Kind, old, repl)
		}
		lfs := func(b []byte) []byte { return bytes.ReplaceAll(b, crlf, lf) }
		if s.Kind == SlipLineEndings && (strict || !bytes.Equal(lfs(old), lfs(repl))) {
			t.Fatalf("line endings blamed, strict %v, old %q new %q", strict, old, repl)
		}
	})
}

// The goldens: one batch with both causes and a change, as text and as
// --json, and --each.
func TestANoChangeGoldens(t *testing.T) {
	files := map[string]string{"f.txt": "a\nb\nc\n", "c.txt": "a\r\nb\r\n"}
	patch := "@@ file f.txt\n@@ old\na\n@@ new\nA\n@@ old\nb\n@@ new\nb\n" +
		"@@ file c.txt\n@@ old\na\r\n\n@@ new\na\n\n"
	t.Run("text", func(t *testing.T) {
		_, out, errOut := runCLI(t, cliTree(t, files), nil, patch)
		golden(t, "cli-no-change", out+errOut)
	})
	t.Run("json", func(t *testing.T) {
		_, out, _ := runCLI(t, cliTree(t, files), []string{"--json"}, patch)
		golden(t, "cli-no-change-json", out)
	})
	t.Run("each", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "p.hunk")
		must(t, os.WriteFile(p, []byte(noChangePatch), 0o644))
		var both bytes.Buffer
		root := cliTree(t, map[string]string{"a.txt": "a\nb\n"})
		if code := cli([]string{"--root", root, "--try", "grep -qx a a.txt", "--each", p}, strings.NewReader(""), &both, &both); code != exitOK {
			t.Fatalf("exit %d: %s", code, both.String())
		}
		golden(t, "cli-each-no-change", reTenths.ReplaceAllString(strings.ReplaceAll(both.String(), filepath.Dir(p)+string(filepath.Separator), ""), "(0.0s)"))
	})
}
