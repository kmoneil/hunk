package main

// The break set's tests (§3.2, §4.3, issue #100): one file, several patches,
// a "@@ patch NAME" heading each, read only under --try --each. The unit rows
// drive splitPatchSet alone, on bytes; the live rows drive the real CLI
// against a real tree and real files, with a marker file proving "nothing was
// tried", as --each's own tests do.

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestABreakSetSplitsAtItsHeadings is the splitter's happy paths: where the
// sections begin and end, byte for byte, and what each is called. The bytes
// matter more than the names: a section is a slice of the file, so a CRLF
// ending, a missing final newline and a payload's blank line all reach the
// parser exactly as the file holds them.
func TestABreakSetSplitsAtItsHeadings(t *testing.T) {
	type sec struct {
		name string
		line int
		src  string
	}
	for _, c := range []struct {
		name, marker, src string
		want              []sec
	}{
		{
			"no heading: the file is one unnamed patch", "@@",
			"@@ file a.txt\n@@ old\nx\n@@ new\ny\n",
			[]sec{{"", 0, "@@ file a.txt\n@@ old\nx\n@@ new\ny\n"}},
		},
		{
			"one heading at line 1", "@@",
			"@@ patch m01\n@@ delete b.txt\n",
			[]sec{{"m01", 1, "@@ delete b.txt\n"}},
		},
		{
			"two headings, each patch its own lines", "@@",
			"@@ patch m01\n@@ file a.txt\n@@ old\nx\n@@ new\ny\n@@ patch m02\n@@ delete b.txt\n",
			[]sec{
				{"m01", 1, "@@ file a.txt\n@@ old\nx\n@@ new\ny\n"},
				{"m02", 7, "@@ delete b.txt\n"},
			},
		},
		{
			// §3.3: a payload's blank line is its trailing newline. The
			// heading must not eat it, or m01's new text loses the newline
			// the author left for it.
			"a heading ends the payload before it, blank line kept", "@@",
			"@@ patch m01\n@@ file a.txt\n@@ old\nx\n@@ new\ny\n\n@@ patch m02\n@@ delete b.txt\n",
			[]sec{
				{"m01", 1, "@@ file a.txt\n@@ old\nx\n@@ new\ny\n\n"},
				{"m02", 8, "@@ delete b.txt\n"},
			},
		},
		{
			"a name may hold spaces, as a path may", "@@",
			"@@ patch no strip\n@@ delete b.txt\n",
			[]sec{{"no strip", 1, "@@ delete b.txt\n"}},
		},
		{
			"a tab separates, as in any directive", "@@",
			"@@\tpatch\tm01\n@@ delete b.txt\n",
			[]sec{{"m01", 1, "@@ delete b.txt\n"}},
		},
		{
			"a CRLF file's sections keep their bytes", "@@",
			"@@ patch m01\r\n@@ delete b.txt\r\n",
			[]sec{{"m01", 1, "@@ delete b.txt\r\n"}},
		},
		{
			"no final newline stays missing", "@@",
			"@@ patch m01\n@@ delete b.txt",
			[]sec{{"m01", 1, "@@ delete b.txt"}},
		},
		{
			// Rule 3 under --marker: the respelled heading splits, and the
			// default one is payload, exactly as "@@ old" would be.
			"another marker splits, and @@ patch is payload under it", "%%",
			"%% patch m01\n%% create a.txt\n@@ patch not a heading\n",
			[]sec{{"m01", 1, "%% create a.txt\n@@ patch not a heading\n"}},
		},
		{
			// #96 decided a capital stays payload; with no real heading
			// anywhere, the file is one patch and the parser owns the line.
			"a capital is payload, as rule 3 has it", "@@",
			"@@ Patch m01\n@@ delete b.txt\n",
			[]sec{{"", 0, "@@ Patch m01\n@@ delete b.txt\n"}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			secs, err := splitPatchSet([]byte(c.src), c.marker)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(secs) != len(c.want) {
				t.Fatalf("%d sections, want %d: %+v", len(secs), len(c.want), secs)
			}
			for i, w := range c.want {
				g := secs[i]
				if g.name != w.name || g.line != w.line || string(g.src) != w.src {
					t.Errorf("section %d:\n got {%q %d %q}\nwant {%q %d %q}",
						i+1, g.name, g.line, g.src, w.name, w.line, w.src)
				}
			}
		})
	}
}

// TestAHeadingFaultRefusesTheSet is the splitter's refusals, one by one: each
// is a *ParseError, exit 1, named by the combined file's line, and the set is
// refused whole rather than split around the fault.
func TestAHeadingFaultRefusesTheSet(t *testing.T) {
	for _, c := range []struct {
		name, src string
		line      int
		says      string
	}{
		{
			"a heading with no name",
			"@@ patch\n@@ delete b.txt\n",
			1, `"@@ patch" needs a name; every patch of the set is tried and reported by it`,
		},
		{
			// directive trims the argument, so spaces are no name either.
			"a heading with only spaces for a name",
			"@@ patch   \n@@ delete b.txt\n",
			1, `"@@ patch" needs a name`,
		},
		{
			"a control character in a name",
			"@@ patch m\x1b01\n@@ delete b.txt\n",
			1, `the name after "@@ patch" contains U+001B, a control character, which every report of the patch would print; remove it`,
		},
		{
			"two patches with one name, both lines named",
			"@@ patch m01\n@@ delete a.txt\n@@ patch m01\n@@ delete b.txt\n",
			3, `"@@ patch m01" gives a second patch the name of the one at line 1; every patch of the set needs a name of its own`,
		},
		{
			"content above the first heading",
			"@@ delete a.txt\n@@ patch m02\n@@ delete b.txt\n",
			2, `"@@ patch m02" follows content that belongs to no patch; a break set begins with its first heading, so put one above that content or take the headings out`,
		},
		{
			"a blank line above the first heading is content too",
			"\n@@ patch m01\n@@ delete b.txt\n",
			2, "follows content that belongs to no patch",
		},
		{
			"a heading straight after a heading",
			"@@ patch m01\n@@ patch m02\n@@ delete b.txt\n",
			1, `"@@ patch m01" names a patch with nothing in it; every patch of the set needs at least one hunk`,
		},
		{
			"a heading as the last line",
			"@@ patch m01\n@@ delete a.txt\n@@ patch m02",
			3, `"@@ patch m02" names a patch with nothing in it`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			secs, err := splitPatchSet([]byte(c.src), DefaultMarker)
			if secs != nil || err == nil {
				t.Fatalf("not refused: %+v, %v", secs, err)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("not a ParseError: %#v", err)
			}
			if pe.Line != c.line {
				t.Errorf("line %d, want %d: %v", pe.Line, c.line, err)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("message does not say %q: %v", c.says, err)
			}
			if code := ExitCode(err); code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
		})
	}
}

// renumber is what keeps "patch line 9" meaning line 9 of the file the caller
// can open, whatever section the line is in.
func TestRenumberNamesTheCombinedFilesLines(t *testing.T) {
	t.Run("hunks and strays move by the section's offset", func(t *testing.T) {
		sec := patchSection{name: "m02", line: 6}
		p := &Patch{Hunks: []Hunk{{Line: 2}}, Strays: []Stray{{Line: 3}}}
		if err := sec.renumber(p, nil); err != nil {
			t.Fatal(err)
		}
		if p.Hunks[0].Line != 8 || p.Strays[0].Line != 9 {
			t.Errorf("hunk line %d and stray line %d, want 8 and 9", p.Hunks[0].Line, p.Strays[0].Line)
		}
	})
	t.Run("a ParseError moves, through its wrapper", func(t *testing.T) {
		pe := &ParseError{Line: 3, Msg: "x"}
		err := patchSection{line: 6}.renumber(nil, notADirective{pe})
		if err == nil || pe.Line != 9 {
			t.Errorf("line %d, want 9 (%v)", pe.Line, err)
		}
	})
	t.Run("line 0 is about the patch as a whole and stays 0", func(t *testing.T) {
		pe := &ParseError{Line: 0, Msg: "x"}
		_ = patchSection{line: 6}.renumber(nil, pe)
		if pe.Line != 0 {
			t.Errorf("line %d, want 0", pe.Line)
		}
	})
	t.Run("an error that is no ParseError passes through", func(t *testing.T) {
		other := errors.New("io")
		if err := (patchSection{line: 6}).renumber(nil, other); !errors.Is(err, other) {
			t.Errorf("got %v", err)
		}
	})
	t.Run("a file with no heading is not renumbered", func(t *testing.T) {
		p := &Patch{Hunks: []Hunk{{Line: 2}}}
		pe := &ParseError{Line: 3, Msg: "x"}
		_ = patchSection{}.renumber(p, pe)
		if p.Hunks[0].Line != 2 || pe.Line != 3 {
			t.Errorf("moved: hunk %d, error %d", p.Hunks[0].Line, pe.Line)
		}
	})
}

func TestASectionIsShownByItsFileAndName(t *testing.T) {
	if got := (patchSection{}).shown("breaks.hunk"); got != "breaks.hunk" {
		t.Errorf("unnamed: %q", got)
	}
	if got := (patchSection{name: "m01"}).shown("breaks.hunk"); got != "breaks.hunk:m01" {
		t.Errorf("named: %q", got)
	}
}

// breakSetFixture is the issue's shape: one tree, one file of two breaks and
// one plain patch file beside it.
func breakSetFixture(t *testing.T) (root, set, plain, mark string) {
	t.Helper()
	root = cliTree(t, map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n", "c.txt": "gamma\n"})
	dir := t.TempDir()
	set = writePatch(t, dir, "set.hunk",
		"@@ patch m01-alpha\n@@ file a.txt\n@@ old\nalpha\n@@ new\nALPHA\n"+
			"@@ patch m02-beta\n@@ file b.txt\n@@ old\nbeta\n@@ new\nBETA\n")
	plain = writePatch(t, dir, "plain.hunk", "@@ file c.txt\n@@ old\ngamma\n@@ new\nGAMMA\n")
	return root, set, plain, pidFile(t, "ran")
}

// TestABreakSetIsTriedPatchByPatch is the feature end to end: each heading
// alone, named FILE:NAME, beside a file with no heading, through text, --json
// and --dry-run.
func TestABreakSetIsTriedPatchByPatch(t *testing.T) {
	t.Run("each patch alone, every file put back", func(t *testing.T) {
		root, set, plain, mark := breakSetFixture(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--try", counted(mark, catchesAll), "--each", set, plain}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		// The baseline run, then one per patch: three patches from two files.
		if n := markerLines(t, mark); n != 4 {
			t.Errorf("the command ran %d times, want 4", n)
		}
		assertUnchanged(t, root, before)
		for _, line := range []string{
			"tried " + set + ":m01-alpha",
			"tried " + set + ":m02-beta",
			"tried " + plain,
		} {
			if !strings.Contains(errOut, line) {
				t.Errorf("stderr has no %q:\n%s", line, errOut)
			}
		}
		if !strings.Contains(out, "tried 3 patches and put back every file; every command exited nonzero") {
			t.Errorf("summary: %q", out)
		}
	})

	t.Run("a missed break is its section's own line and exit 3", func(t *testing.T) {
		root, set, plain, mark := breakSetFixture(t)
		// The suite tests a.txt and c.txt only, so m02-beta's break gets
		// through, and it alone.
		code, _, errOut := runCLI(t, root,
			[]string{"--try", counted(mark, "grep -q alpha a.txt && grep -q gamma c.txt"), "--each", set, plain}, "")
		if code != exitVerifyFailed {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		missed := ""
		for line := range strings.Lines(errOut) {
			if strings.Contains(line, set+":m02-beta") {
				missed = line
			}
		}
		if !strings.Contains(missed, "the command exited 0") {
			t.Errorf("the missed break's line does not name the section:\n%s", errOut)
		}
		if !strings.Contains(errOut, "2 exited nonzero, 1 exited 0") {
			t.Errorf("summary: %s", errOut)
		}
	})

	t.Run("--json names each section", func(t *testing.T) {
		root, set, plain, mark := breakSetFixture(t)
		code, out, _ := runCLI(t, root,
			[]string{"--json", "--try", counted(mark, catchesAll), "--each", set, plain}, "")
		var r struct {
			Exit int `json:"exit"`
			Each struct {
				Patches []jsonEachPatch `json:"patches"`
			} `json:"each"`
		}
		must(t, json.Unmarshal([]byte(out), &r))
		if code != exitOK || r.Exit != exitOK || len(r.Each.Patches) != 3 {
			t.Fatalf("exit %d: %s", code, out)
		}
		for i, want := range []string{set + ":m01-alpha", set + ":m02-beta", plain} {
			if r.Each.Patches[i].Patch != want {
				t.Errorf("patch %d is %q, want %q", i+1, r.Each.Patches[i].Patch, want)
			}
		}
	})

	t.Run("--dry-run is the preflight per section", func(t *testing.T) {
		root, set, plain, mark := breakSetFixture(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--dry-run", "--try", counted(mark, catchesAll), "--each", set, plain}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		if n := markerLines(t, mark); n != 0 {
			t.Errorf("the command ran %d times under --dry-run", n)
		}
		assertUnchanged(t, root, before)
		for _, want := range []string{set + ":m01-alpha", set + ":m02-beta", "3 patches would apply"} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout has no %q:\n%s", want, out)
			}
		}
	})
}

// TestABreakSetFaultRefusesTheRun is the live side of the splitter's
// refusals: exit 1, the command never ran, the tree untouched, and the
// wording goldened, since the messages are the contract (§8.1).
func TestABreakSetFaultRefusesTheRun(t *testing.T) {
	for _, c := range []struct {
		golden, body string
	}{
		{"cli-each-break-set-no-name", "@@ patch\n@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n"},
		{"cli-each-break-set-one-name-twice", "@@ patch m01\n@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n@@ patch m01\n@@ delete b.txt\n"},
		{"cli-each-break-set-content-above", "@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n@@ patch m02\n@@ delete b.txt\n"},
		{"cli-each-break-set-nothing-in-it", "@@ patch m01\n@@ patch m02\n@@ delete b.txt\n"},
	} {
		t.Run(c.golden, func(t *testing.T) {
			root, _, plain, mark := breakSetFixture(t)
			bad := writePatch(t, t.TempDir(), "bad.hunk", c.body)
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root,
				[]string{"--try", counted(mark, catchesAll), "--each", bad, plain}, "")
			if code != exitUsage || out != "" {
				t.Fatalf("exit %d, stdout %q: %s", code, out, errOut)
			}
			if n := markerLines(t, mark); n != 0 {
				t.Errorf("the command ran %d times", n)
			}
			assertUnchanged(t, root, before)
			if !strings.Contains(errOut, "1 of 2 patches could not be read; nothing was tried") {
				t.Errorf("header: %s", errOut)
			}
			golden(t, c.golden, stripDirs(errOut, bad, plain))
		})
	}

	t.Run("cli-each-break-set-no-name-json", func(t *testing.T) {
		// The command is a constant here, since the golden holds it; the text
		// rows above prove with their marker that it never runs.
		root, _, plain, _ := breakSetFixture(t)
		bad := writePatch(t, t.TempDir(), "bad.hunk", "@@ patch\n@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n")
		code, out, _ := runCLI(t, root,
			[]string{"--json", "--try", catchesAll, "--each", bad, plain}, "")
		var r struct {
			Exit int `json:"exit"`
			Each struct {
				Patches []jsonEachPatch `json:"patches"`
			} `json:"each"`
		}
		must(t, json.Unmarshal([]byte(out), &r))
		if code != exitUsage || r.Exit != exitUsage || len(r.Each.Patches) != 2 {
			t.Fatalf("exit %d: %s", code, out)
		}
		if got := r.Each.Patches[0].Error; !strings.Contains(got, `"@@ patch" needs a name`) {
			t.Errorf("the set's error is %q", got)
		}
		golden(t, "cli-each-break-set-no-name-json", stripDirs(out, bad, plain))
	})
}

// TestAFaultInsideASectionNamesTheCombinedFilesLine: the parse and
// validation refusals a section earns are reported under FILE:NAME, by the
// line of the file the caller wrote, not the section's own counting.
func TestAFaultInsideASectionNamesTheCombinedFilesLine(t *testing.T) {
	write := func(t *testing.T, body string) (root, set, mark string) {
		t.Helper()
		root = cliTree(t, map[string]string{"a.txt": "alpha\n", "b.txt": "beta\n"})
		set = writePatch(t, t.TempDir(), "set.hunk", body)
		return root, set, pidFile(t, "ran")
	}

	t.Run("a parse error in the second patch", func(t *testing.T) {
		// "@@ new" with no "@@ old" at the combined file's line 9.
		root, set, mark := write(t,
			"@@ patch m01\n@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n"+
				"@@ patch m02\n@@ file b.txt\n@@ new\nY\n")
		code, _, errOut := runCLI(t, root, []string{"--try", counted(mark, "true"), "--each", set}, "")
		if code != exitUsage || markerLines(t, mark) != 0 {
			t.Fatalf("exit %d, ran %d times: %s", code, markerLines(t, mark), errOut)
		}
		if want := set + `:m02: patch line 9: "@@ new" with no "@@ old" before it`; !strings.Contains(errOut, want) {
			t.Errorf("stderr does not say %q:\n%s", want, errOut)
		}
	})

	t.Run("a validation failure in the second patch", func(t *testing.T) {
		root, set, mark := write(t,
			"@@ patch m01\n@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n"+
				"@@ patch m02\n@@ file b.txt\n@@ old\nNOT THERE\n@@ new\nY\n")
		code, _, errOut := runCLI(t, root, []string{"--try", counted(mark, "true"), "--each", set}, "")
		if code != exitNoMatch || markerLines(t, mark) != 0 {
			t.Fatalf("exit %d, ran %d times: %s", code, markerLines(t, mark), errOut)
		}
		for _, want := range []string{set + ":m02:", "(patch line 9)"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr does not say %q:\n%s", want, errOut)
			}
		}
	})

	t.Run("a mistyped delimiter in the second patch", func(t *testing.T) {
		// strayPatch's HUHUNK sits at its own line 6; as the second patch of
		// a set it is the combined file's line 7.
		root, set, mark := write(t, "@@ patch m01\n@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n@@ patch m02\n"+
			strings.ReplaceAll(strayPatch, "notes.txt", "b.txt"))
		code, _, errOut := runCLI(t, root, []string{"--try", counted(mark, "true"), "--each", set}, "")
		if code != exitUsage || markerLines(t, mark) != 0 {
			t.Fatalf("exit %d, ran %d times: %s", code, markerLines(t, mark), errOut)
		}
		if want := set + `:m02: patch line 13 is "HUHUNK"`; !strings.Contains(errOut, want) {
			t.Errorf("stderr does not say %q:\n%s", want, errOut)
		}
	})

	t.Run("a glued heading is refused, not a boundary and not payload", func(t *testing.T) {
		root, set, mark := write(t,
			"@@ patch m01\n@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n"+
				"@@patch m02\n@@ file b.txt\n@@ old\nbeta\n@@ new\nY\n")
		code, _, errOut := runCLI(t, root, []string{"--try", counted(mark, "true"), "--each", set}, "")
		if code != exitUsage || markerLines(t, mark) != 0 {
			t.Fatalf("exit %d, ran %d times: %s", code, markerLines(t, mark), errOut)
		}
		// The faulty line stays in m01's section, so the parser's #96
		// refusal names it, at the combined file's line 7.
		if want := `:m01: patch line 7: the line would be the directive "@@ patch" but for the space missing after "@@"`; !strings.Contains(errOut, want) {
			t.Errorf("stderr does not say %q:\n%s", want, errOut)
		}
	})
}

// TestAHeadingOutsideEachIsRefused: a break set applied as one batch is never
// what was meant, so a single run refuses the heading wherever it sits,
// payload included, and --marker is the way to write the line as text.
func TestAHeadingOutsideEachIsRefused(t *testing.T) {
	set := "@@ patch m01\n@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n"

	t.Run("at line 1, from stdin", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, nil, set)
		if code != exitUsage {
			t.Fatalf("exit %d: %s%s", code, out, errOut)
		}
		assertUnchanged(t, root, before)
		golden(t, "cli-parse-patch-heading", out+errOut)
	})

	t.Run("--json", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
		code, out, _ := runCLI(t, root, []string{"--json"}, set)
		if code != exitUsage {
			t.Fatalf("exit %d: %s", code, out)
		}
		golden(t, "cli-parse-patch-heading-json", out)
	})

	t.Run("inside a payload, nothing written", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, nil,
			"@@ file a.txt\n@@ old\nalpha\n@@ new\nX\n@@ patch m02\nY\n")
		if code != exitUsage {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		assertUnchanged(t, root, before)
		if !strings.Contains(errOut, `patch line 6: "@@ patch" heads one patch of a break set`) {
			t.Errorf("stderr: %s", errOut)
		}
	})

	t.Run("a --verify never runs", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
		mark := pidFile(t, "ran")
		code, _, _ := runCLI(t, root, []string{"--verify", fmt.Sprintf("echo ran >> '%s'", mark)}, set)
		if code != exitUsage || markerLines(t, mark) != 0 {
			t.Fatalf("exit %d, the command ran %d times", code, markerLines(t, mark))
		}
	})

	t.Run("--marker writes the line as text", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "alpha\n"})
		code, _, errOut := runCLI(t, root, []string{"--marker", "%%"},
			"%% file a.txt\n%% old\nalpha\n%% new\n@@ patch m01\n")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if got := readFile(t, root, "a.txt"); got != "@@ patch m01\n" {
			t.Errorf("a.txt is %q", got)
		}
	})
}

// stripDirs cuts each path's directory out of a report, leaving the base
// names the goldens can hold. A --json report escapes the path, so on
// Windows the directory appears as C:\\Users\\... and the raw spelling never
// matches: both are stripped.
func stripDirs(out string, paths ...string) string {
	for _, p := range paths {
		if i := strings.LastIndexAny(p, `/\`); i >= 0 {
			dir := p[:i+1]
			out = strings.ReplaceAll(out, dir, "")
			out = strings.ReplaceAll(out, strings.ReplaceAll(dir, `\`, `\\`), "")
		}
	}
	return out
}

// The splitter reads bytes it was handed and nothing else, like the parser it
// feeds (§6.1), and the import list is the assertion, in the habit of
// TestParserHasNoFileAccess.
func TestSplitterHasNoFileAccess(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "patchset.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse patchset.go: %v", err)
	}
	allowed := map[string]bool{"errors": true, "fmt": true}
	for _, im := range f.Imports {
		path, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", im.Path.Value, err)
		}
		if !allowed[path] {
			t.Errorf("patchset.go imports %q; the splitter reads no files and needs no I/O", path)
		}
	}
}

// The heading is only worth having if an agent can find it: the three format
// documents and the help all carry it. SKILL.md's mention is
// TestTheSkillDocumentsEveryDirective's, since "patch" is a directive word.
func TestTheDocsCarryTheBreakSetHeading(t *testing.T) {
	for name, text := range map[string]string{"hunk format": formatDoc, "--help": usage} {
		if !strings.Contains(text, `"@@ patch NAME"`) {
			t.Errorf("%s never says %q", name, `"@@ patch NAME"`)
		}
		if !strings.Contains(text, "FILE:NAME") {
			t.Errorf("%s never says how a patch of a set is named", name)
		}
	}
	for name, text := range formatDocuments(t) {
		if !strings.Contains(text, "@@ patch") {
			t.Errorf("%s never says %q", name, "@@ patch")
		}
	}
}

// Agent-shaped input, so the property is totality, plus the three the split
// promises: a file with no heading is one unnamed section holding the file;
// every section is a contiguous slice of the input; and no section holds a
// heading, so the parser behind the splitter can never meet one.
func FuzzSplitPatchSet(f *testing.F) {
	f.Add("@@ patch m01\n@@ delete b.txt\n", "@@")
	f.Add("@@ patch m01\n@@ file a\n@@ old\nx\n@@ new\ny\n@@ patch m02\n@@ delete b\n", "@@")
	f.Add("@@ file a\n@@ old\nx\n@@ new\ny\n", "@@")
	f.Add("@@ patch\n", "@@")
	f.Add("@@ patch a\n@@ patch a\n", "@@")
	f.Add("%% patch m01\r\n%% delete b\r\n", "%%")
	f.Add("x\n@@ patch m01\n@@ delete b\n", "@@")
	f.Fuzz(func(t *testing.T, src, marker string) {
		if marker == "" {
			marker = DefaultMarker
		}
		secs, err := splitPatchSet([]byte(src), marker)
		if err != nil {
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("refusal is not a ParseError: %#v", err)
			}
			if secs != nil {
				t.Fatal("sections beside an error")
			}
			return
		}
		if len(secs) == 0 {
			t.Fatal("no sections and no error")
		}
		if secs[0].line == 0 {
			if len(secs) != 1 || string(secs[0].src) != src || secs[0].name != "" {
				t.Fatalf("a file with no heading is not the one unnamed section: %+v", secs)
			}
			return
		}
		for i, sec := range secs {
			if sec.name == "" || sec.line < 1 {
				t.Errorf("section %d is malformed: %+v", i+1, sec)
			}
			if !strings.Contains(src, string(sec.src)) {
				t.Errorf("section %d is not a slice of the input", i+1)
			}
			for j, line := range splitLines(sec.src) {
				if word, _, ok := directive(line, marker); ok && word == "patch" {
					t.Errorf("section %d still holds a heading at its line %d", i+1, j+1)
				}
			}
		}
	})
}
