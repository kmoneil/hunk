package main

// A patch holding a line that looks like a mistyped heredoc delimiter is
// refused, exit 1, before anything is read (#97), every such line named.
// Under --allow-mistyped-delimiter it applies and they are warned of, as every
// patch was in v0.3.15 and v0.3.16; delimiter_test.go tests that path through
// warnCLI, against its goldens.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The refusal of strayPatch, whole.
const strayRefused = "hunk: patch line 6 is \"HUHUNK\", which looks like a mistyped heredoc delimiter; " +
	"it would be payload for notes.txt, so nothing was written. If it was meant to end the heredoc, " +
	"take it out of the patch; if it is text, pass --allow-mistyped-delimiter\n"

// Every mode refuses the same way, with nothing read, written or run, and the
// same call under the flag ends as it did before the refusal existed.
func TestAMistypedDelimiterIsRefused(t *testing.T) {
	notes := map[string]string{"notes.txt": "one\ntwo\n"}
	marked := "%% file notes.txt\n%% old\ntwo\n%% new\ntwo, edited\nHUHUNK\n"
	missing := "@@ file notes.txt\n@@ old\nnot there\n@@ new\ntwo, edited\nHUHUNK\n"
	for _, c := range []struct {
		name    string
		args    []string
		patch   string // strayPatch if empty
		file    bool   // from -f rather than stdin
		allowed int    // the exit under the flag
	}{
		{name: "applied", allowed: exitOK},
		{name: "from -f, which a heredoc may have written", file: true, allowed: exitOK},
		{name: "a dry run", args: []string{"--dry-run"}, allowed: exitOK},
		{name: "a verify, which never runs", args: []string{"--verify", "touch ran"}, allowed: exitOK},
		{name: "a verify that would fail", args: []string{"--verify", "touch ran; false"}, allowed: exitVerifyFailed},
		{name: "--keep-on-fail", args: []string{"--verify", "touch ran; false", "--keep-on-fail"}, allowed: exitVerifyFailed},
		{name: "--try, which never runs", args: []string{"--try", "touch ran"}, allowed: exitOK},
		{name: "--quiet, which silences no refusal", args: []string{"--quiet"}, allowed: exitOK},
		{name: "a hunk that would miss, refused first", patch: missing, allowed: exitNoMatch},
		{name: "--marker, which is no way out", args: []string{"--marker", "%%"}, patch: marked, allowed: exitOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			patch := c.patch
			if patch == "" {
				patch = strayPatch
			}
			run := func(args []string) (int, string, string, string, map[string]string) {
				root := cliTree(t, notes)
				before := snapshot(t, root)
				stdin := patch
				if c.file {
					p := filepath.Join(t.TempDir(), "p.hunk")
					must(t, os.WriteFile(p, []byte(patch), 0o644))
					args, stdin = append([]string{"-f", p}, args...), ""
				}
				code, out, errOut := runCLI(t, root, args, stdin)
				return code, out, errOut, root, before
			}

			code, out, errOut, root, before := run(c.args)
			if code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			if errOut != strayRefused {
				t.Errorf("stderr\n got %q\nwant %q", errOut, strayRefused)
			}
			// Nothing written, and no command run: "ran" would be in the tree.
			assertUnchanged(t, root, before)

			code, out, errOut, _, _ = run(append([]string{allowMistypedFlag}, c.args...))
			if code != c.allowed {
				t.Errorf("under %s: exit %d, want %d: %s%s", allowMistypedFlag, code, c.allowed, out, errOut)
			}
			if !strings.Contains(errOut, "warning: patch line 6 is \"HUHUNK\"") {
				t.Errorf("under %s, no warning: %q", allowMistypedFlag, errOut)
			}
		})
	}
}

// A patch with two is refused once, naming both, in patch order, so that it
// does not cost two rounds; the second is #92's worst shape, the rest of the
// command in the payload.
const twoStrays = "@@ file notes.txt\n@@ old\none\n@@ new\nONE\nHUHUNK\n@@ old\ntwo\n@@ new\nTWO\nHUNKK\nmake test\n"

func TestARefusalNamesEveryMistypedDelimiter(t *testing.T) {
	root := cliTree(t, map[string]string{"notes.txt": "one\ntwo\n"})
	before := snapshot(t, root)
	code, out, errOut := runCLI(t, root, nil, twoStrays)
	if code != exitUsage || out != "" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	lines := strings.Split(strings.TrimSuffix(errOut, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2: %q", len(lines), errOut)
	}
	for i, want := range []string{
		`hunk: patch line 6 is "HUHUNK", which looks like a mistyped heredoc delimiter; it would be payload for notes.txt`,
		`hunk: patch line 11 is "HUNKK", which looks like a mistyped heredoc delimiter; it and the 1 line after it would be payload for notes.txt, ` +
			`so nothing was written. If it was meant to end the heredoc, take them out of the patch and run any command they held, ` +
			`which the shell never did; if they are text, pass --allow-mistyped-delimiter`,
	} {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("line %d\n got %q\nwant it to start %q", i+1, lines[i], want)
		}
	}
	assertUnchanged(t, root, before)
	golden(t, "cli-mistyped-delimiter-refused-every-line", errOut)
}

// --json: not ok, exit 1, the error every line, and warnings carrying each as
// the warning did, with the refusal for its message, so a program reads the
// lines without parsing a sentence.
func TestAMistypedDelimiterRefusedInJSON(t *testing.T) {
	notes := map[string]string{"notes.txt": "one\ntwo\n"}
	for _, args := range [][]string{{"--json"}, {"--json", "--dry-run"}, {"--json", "--quiet"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, errOut := runCLI(t, cliTree(t, notes), args, twoStrays)
			if code != exitUsage || errOut != "" {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			var r struct {
				OK       bool          `json:"ok"`
				Exit     int           `json:"exit"`
				Error    string        `json:"error"`
				Files    []any         `json:"files"`
				Warnings []jsonWarning `json:"warnings"`
			}
			must(t, json.Unmarshal([]byte(out), &r))
			if r.OK || r.Exit != exitUsage || r.Files != nil {
				t.Errorf("ok %v, exit %d, files %v", r.OK, r.Exit, r.Files)
			}
			if len(r.Warnings) != 2 {
				t.Fatalf("%d warnings: %s", len(r.Warnings), out)
			}
			var msgs []string
			for i, want := range []struct{ line, after int }{{6, 0}, {11, 1}} {
				w := r.Warnings[i]
				if w.Kind != "mistyped_delimiter" || w.PatchLine != want.line || w.LinesAfter == nil || *w.LinesAfter != want.after || w.Path != "notes.txt" || w.Patch != "" {
					t.Errorf("warning %d = %+v", i+1, w)
				}
				if !strings.Contains(w.Message, "so nothing was written") || !strings.HasSuffix(w.Message, allowMistypedFlag) {
					t.Errorf("warning %d's message is not the refusal: %q", i+1, w.Message)
				}
				msgs = append(msgs, w.Message)
			}
			if r.Error != strings.Join(msgs, "\n") {
				t.Errorf("error %q, want every refusal", r.Error)
			}
		})
	}
	t.Run("golden", func(t *testing.T) {
		_, out, _ := runCLI(t, cliTree(t, notes), []string{"--json"}, strayPatch)
		golden(t, "cli-mistyped-delimiter-refused-json", out)
	})
	t.Run("text golden", func(t *testing.T) {
		_, _, errOut := runCLI(t, cliTree(t, notes), nil, strayPatch)
		golden(t, "cli-mistyped-delimiter-refused", errOut)
	})
}

// --each refuses the run with the patches that do not parse, nothing tried,
// each stray under its patch's name; under the flag the run goes as it did.
func TestEachRefusesAMistypedDelimiter(t *testing.T) {
	setup := func(t *testing.T, second string) (root, p1, p2, mark string) {
		root = cliTree(t, map[string]string{"notes.txt": "one\ntwo\n"})
		dir := t.TempDir()
		p1 = writePatch(t, dir, "p1.hunk", strayPatch)
		p2 = writePatch(t, dir, "p2.hunk", second)
		return root, p1, p2, pidFile(t, "ran")
	}
	clean := "@@ file notes.txt\n@@ old\none\n@@ new\nONE\n"

	t.Run("one patch with one", func(t *testing.T) {
		root, p1, p2, mark := setup(t, clean)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", fmt.Sprintf("echo ran >> '%s'", mark), "--each", p1, p2}, "")
		if code != exitUsage || out != "" {
			t.Fatalf("exit %d, stdout %q", code, out)
		}
		want := "hunk: 1 of 2 patches could not be read; nothing was tried\n\n" +
			p1 + ": " + strings.TrimPrefix(strayRefused, "hunk: ")
		if errOut != want {
			t.Errorf("stderr\n got %q\nwant %q", errOut, want)
		}
		if n := markerLines(t, mark); n != 0 {
			t.Errorf("the command ran %d times", n)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("both, every line under its patch", func(t *testing.T) {
		root, p1, p2, mark := setup(t, twoStrays)
		code, _, errOut := runCLI(t, root, []string{"--try", fmt.Sprintf("echo ran >> '%s'", mark), "--each", p1, p2}, "")
		if code != exitUsage || !strings.HasPrefix(errOut, "hunk: 2 patches could not be read; nothing was tried\n") {
			t.Fatalf("exit %d: %q", code, errOut)
		}
		for _, w := range []string{p1 + ": patch line 6 is \"HUHUNK\"", p2 + ": patch line 6 is \"HUHUNK\"", "\npatch line 11 is \"HUNKK\""} {
			if !strings.Contains(errOut, w) {
				t.Errorf("stderr does not say %q: %q", w, errOut)
			}
		}
		if n := markerLines(t, mark); n != 0 {
			t.Errorf("the command ran %d times", n)
		}
	})

	t.Run("--json", func(t *testing.T) {
		root, p1, p2, mark := setup(t, clean)
		code, out, _ := runCLI(t, root, []string{"--json", "--try", fmt.Sprintf("echo ran >> '%s'", mark), "--each", p1, p2}, "")
		var r struct {
			Exit int `json:"exit"`
			Each struct {
				Patches []jsonEachPatch `json:"patches"`
			} `json:"each"`
			Warnings []jsonWarning `json:"warnings"`
		}
		must(t, json.Unmarshal([]byte(out), &r))
		if code != exitUsage || r.Exit != exitUsage || len(r.Each.Patches) != 2 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if got := r.Each.Patches[0].Error; got != strings.TrimSuffix(strings.TrimPrefix(strayRefused, "hunk: "), "\n") {
			t.Errorf("p1's error %q", got)
		}
		if len(r.Warnings) != 1 || r.Warnings[0].Patch != p1 || r.Warnings[0].PatchLine != 6 ||
			!strings.HasPrefix(r.Warnings[0].Message, p1+" line 6 is") || !strings.HasSuffix(r.Warnings[0].Message, allowMistypedFlag) {
			t.Errorf("warnings %+v", r.Warnings)
		}
	})

	t.Run("under the flag, tried as before", func(t *testing.T) {
		root, p1, p2, mark := setup(t, clean)
		// Every command exits 0, so no patch is caught, and that is exit 3:
		// the run is the one v0.3.16 made, both patches tried and warned of.
		code, out, errOut := runCLI(t, root, []string{allowMistypedFlag, "--try", fmt.Sprintf("echo ran >> '%s'", mark), "--each", p1, p2}, "")
		if code != exitVerifyFailed || markerLines(t, mark) != 3 || !strings.Contains(errOut, "warning: "+p1+" line 6 is \"HUHUNK\"") {
			t.Errorf("exit %d, ran %d times: %s%s", code, markerLines(t, mark), out, errOut)
		}
	})

	t.Run("golden", func(t *testing.T) {
		root, p1, p2, _ := setup(t, clean)
		_, _, errOut := runCLI(t, root, []string{"--try", "true", "--each", p1, p2}, "")
		golden(t, "cli-each-mistyped-delimiter-refused", strings.ReplaceAll(errOut, filepath.Dir(p1)+string(filepath.Separator), ""))
	})
}

// The sentence, in each of its shapes.
func TestStrayRefusal(t *testing.T) {
	for _, c := range []struct {
		name, patch string
		s           Stray
		want        string
	}{
		{
			"alone", "",
			Stray{Line: 6, Path: "notes.txt", Text: []byte("HUHUNK")},
			`patch line 6 is "HUHUNK", which looks like a mistyped heredoc delimiter; it would be payload for notes.txt, so nothing was written. ` +
				`If it was meant to end the heredoc, take it out of the patch; if it is text, pass --allow-mistyped-delimiter`,
		},
		{
			"two lines after it", "",
			Stray{Line: 3, Path: "a.md", Text: []byte("HUNKK"), After: 2},
			`patch line 3 is "HUNKK", which looks like a mistyped heredoc delimiter; it and the 2 lines after it would be payload for a.md, so nothing was written. ` +
				`If it was meant to end the heredoc, take them out of the patch and run any command they held, which the shell never did; if they are text, pass --allow-mistyped-delimiter`,
		},
		{
			"under --each, named by its patch", "p1.hunk",
			Stray{Line: 6, Path: "notes.txt", Text: []byte("  HUNK")},
			`p1.hunk line 6 is "  HUNK", which looks like a mistyped heredoc delimiter; it would be payload for notes.txt`,
		},
		{
			// A CR left in the text is a byte of the line, and shows.
			"a control in the path is named, a CR in the line shown", "",
			Stray{Line: 2, Path: "a\x1b.md", Text: []byte("HUNK\r")},
			`patch line 2 is "HUNK\r", which looks like a mistyped heredoc delimiter; it would be payload for a<U+001B>.md`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := strayRefusal(c.patch, c.s)
			if !strings.HasPrefix(got, c.want) {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
			if name, ok := firstControl(got); ok {
				t.Errorf("prints %s raw: %q", name, got)
			}
		})
	}
	// Error is one refusal a line, in patch order.
	e := &MistypedDelimiters{Strays: []Stray{{Line: 2, Path: "a", Text: []byte("HUHUNK")}, {Line: 5, Path: "b", Text: []byte("HUNKK")}}}
	if lines := strings.Split(e.Error(), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[0], "patch line 2") || !strings.HasPrefix(lines[1], "patch line 5") {
		t.Errorf("Error() = %q", e.Error())
	}
	if ExitCode(fmt.Errorf("wrapped: %w", e)) != exitUsage {
		t.Errorf("ExitCode = %d, want %d", ExitCode(e), exitUsage)
	}
}

// A path and a line come from the patch, so the sentence is fuzzed: it never
// prints a control character, quotes the line as shownLine would, and ends in
// the flag.
func FuzzStrayRefusal(f *testing.F) {
	f.Add("", 6, "notes.txt", "HUHUNK", 0)
	f.Add("p1.hunk", 3, "a\x1b[2J.md", "HUNK\r", 2)
	f.Add("p\x9b.hunk", 1, "\u0085", "\x00HUNK", -1)
	f.Fuzz(func(t *testing.T, patch string, line int, path, text string, after int) {
		got := strayRefusal(patch, Stray{Line: line, Path: path, Text: []byte(text), After: after})
		if name, ok := firstControl(got); ok {
			t.Fatalf("prints %s raw: %q", name, got)
		}
		if !strings.Contains(got, quoteCut(text)) || !strings.HasSuffix(got, allowMistypedFlag) {
			t.Errorf("%q does not quote %q, or does not end in the flag", got, text)
		}
	})
}

// Every document says such a line is refused and names the flag, beside the
// mistyped closer it is about.
func TestTheDocsSayAMistypedDelimiterIsRefused(t *testing.T) {
	const reach = 900
	for name, doc := range formatDocuments(t) {
		t.Run(name, func(t *testing.T) {
			i := strings.Index(doc, allowMistypedFlag)
			if i < 0 {
				t.Fatalf("never names %s", allowMistypedFlag)
			}
			near := doc[max(0, i-reach):min(len(doc), i+reach)]
			for _, w := range []string{"HUHUNK", "refuse", "exit 1"} {
				if !strings.Contains(near, w) {
					t.Errorf("%q is not within %d bytes of %s", w, reach, allowMistypedFlag)
				}
			}
		})
	}
}
