package main

// A patch that quotes a patch (#148): an edit to a break set, whose old and
// new are lines like "@@ patch m01" and "@@ old". Under the default marker
// those lines are directives, and every refusal they cause names --marker,
// the way to write them as text, as the refusals of a line that is nearly a
// directive already did. Until 2026-11-06 the commonest of them, "@@ old"
// cut short by a quoted "@@ patch", was refused for its empty payload with a
// hint about inserting text.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The refusals a directive line standing in a payload can cause, each by the
// line it names and its whole message, and the one shape left as it was.
func TestAQuotedDirectiveNamesMarker(t *testing.T) {
	const way = "choose another directive prefix with --marker"
	const noNew = `"@@ old" has no "@@ new"; every old needs the text to put in its place, even if that text is empty. If the line is payload text, ` + way
	const insertHint = "@@ old has an empty payload, which would match at every position in the file rather than at one. A single blank line joins to nothing, so leave two to match an empty line. To insert text, put a surrounding line in old and repeat it in new, or use @@ append or @@ prepend at a file boundary"
	const afterEnd = `content after "@@ end", which terminates the patch; remove it or remove the terminator, or if the terminator is payload text, ` + way

	endedBy := func(line string, at string) string {
		return `"@@ old" has an empty payload, ended by ` + line + ` at line ` + at + `, a directive; if that line is payload text, ` + way
	}
	found := func(word string) string {
		return `expected "@@ new" to close the "@@ old" at line 2, found "@@ ` + word + `"; if the line is payload text, ` + way
	}

	tests := []struct {
		name, marker, patch string
		line                int
		msg                 string
	}{
		// An "@@ old" a directive ends before any text: the issue's shape.
		{"the issue's repro", "", "@@ file r.txt\n@@ old\n@@ patch a\n@@ new\ny\n", 2, endedBy(`"@@ patch a"`, "3")},
		{"ended by @@ file", "", "@@ file a\n@@ old\n@@ file b\n@@ new\ny\n", 2, endedBy(`"@@ file b"`, "3")},
		{"ended by @@ old, its count shown", "", "@@ file a\n@@ old\n@@ old x2\nx\n@@ new\ny\n", 2, endedBy(`"@@ old x2"`, "3")},
		{"ended by @@ create", "", "@@ file a\n@@ old\n@@ create b\n", 2, endedBy(`"@@ create b"`, "3")},
		{"ended by @@ delete", "", "@@ file a\n@@ old\n@@ delete b\n", 2, endedBy(`"@@ delete b"`, "3")},
		{"ended by @@ append", "", "@@ file a\n@@ old\n@@ append b\n", 2, endedBy(`"@@ append b"`, "3")},
		{"ended by @@ prepend", "", "@@ file a\n@@ old\n@@ prepend b\n", 2, endedBy(`"@@ prepend b"`, "3")},
		{"ended by @@ end", "", "@@ file a\n@@ old\n@@ end\n", 2, endedBy(`"@@ end"`, "3")},
		{"after a blank line, which joins to nothing", "", "@@ file a\n@@ old\n\n@@ file b\n", 2, endedBy(`"@@ file b"`, "4")},
		{"in a CRLF patch, the line shown without its CR", "", "@@ file a\r\n@@ old\r\n\r\n@@ patch p\r\n", 2, endedBy(`"@@ patch p"`, "4")},
		{"a long line is cut", "", "@@ file a\n@@ old\n@@ file " + strings.Repeat("x", 70) + "\n", 2, endedBy(`"@@ file `+strings.Repeat("x", 52)+`"...`, "3")},
		{"a control character in it is named, not printed", "", "@@ file a\n@@ old\n@@ file b\x1bc\n", 2, endedBy(`"@@ file b\x1bc"`, "3")},
		{
			"under another marker, that marker's", "%%", "%% file a\n%% old\n%% patch p\n", 2,
			`"%% old" has an empty payload, ended by "%% patch p" at line 3, a directive; if that line is payload text, ` + way,
		},

		// An "@@ old" the patch ends inside has no "@@ new", empty or not.
		{"@@ old the last line", "", "@@ file a\n@@ old\n", 2, noNew},
		{"then a blank line, no longer called empty", "", "@@ file a\n@@ old\n\n", 2, noNew},
		{"and in a CRLF patch", "", "@@ file a\r\n@@ old\r\n\r\n", 2, noNew},
		{"with text", "", "@@ file a\n@@ old\nx\n", 2, noNew},
		{"a quoted @@ old ending a new payload", "", "@@ file a\n@@ old\nx\n@@ new\ny\n@@ old x9\nz\n", 6, noNew},
		{
			"under another marker", "%%", "%% file a\n%% old\nx\n", 2,
			`"%% old" has no "%% new"; every old needs the text to put in its place, even if that text is empty. If the line is payload text, ` + way,
		},

		// An old with text, ended by a directive that is not "@@ new".
		{"found @@ file", "", "@@ file a\n@@ old\nx\n@@ file b\n@@ new\ny\n", 4, found("file")},
		{"found @@ old", "", "@@ file a\n@@ old\nx\n@@ old\nq\n@@ new\ny\n", 4, found("old")},
		{"found @@ create", "", "@@ file a\n@@ old\nx\n@@ create b\n", 4, found("create")},
		{"found @@ delete", "", "@@ file a\n@@ old\nx\n@@ delete b\n", 4, found("delete")},
		{"found @@ append", "", "@@ file a\n@@ old\nx\n@@ append b\n", 4, found("append")},
		{"found @@ prepend", "", "@@ file a\n@@ old\nx\n@@ prepend b\n", 4, found("prepend")},
		{"found @@ end", "", "@@ file a\n@@ old\nx\n@@ end\n", 4, found("end")},
		// Inside an old, a quoted heading never reaches the heading's own
		// refusal, so this one has to name --marker for it.
		{"found @@ patch", "", "@@ file a\n@@ old\nx\n@@ patch m01\n@@ new\ny\n", 4, found("patch")},
		{
			"the @@ old named is the one it closes", "", "@@ file a\n@@ old\np\n@@ new\nq\n@@ old\nx\ny\n@@ file b\n", 9,
			`expected "@@ new" to close the "@@ old" at line 6, found "@@ file"; if the line is payload text, ` + way,
		},
		{
			"found, under another marker", "%%", "%% file a\n%% old\nx\n%% file b\n", 4,
			`expected "%% new" to close the "%% old" at line 2, found "%% file"; if the line is payload text, ` + way,
		},

		// "@@ new" with nothing for it to close.
		{"@@ new first", "", "@@ file a\n@@ new\ny\n", 2, `"@@ new" with no "@@ old" before it; if the line is payload text, ` + way},
		{"@@ new inside a new payload", "", "@@ file a\n@@ old\nx\n@@ new\ny\n@@ new\nz\n", 6, `"@@ new" with no "@@ old" before it; if the line is payload text, ` + way},
		{"@@ new, under another marker", "%%", "%% file a\n%% new\n", 2, `"%% new" with no "%% old" before it; if the line is payload text, ` + way},

		// Text after "@@ end", which is the line named: the terminator is the
		// one that may be quoted.
		{"content after @@ end", "", "@@ delete a\n@@ end\njunk\n", 3, afterEnd},
		{"@@ end inside a create payload", "", "@@ create a\nx\n@@ end\ny\n", 4, afterEnd},
		{
			"content after, under another marker", "%%", "%% delete a\n%% end\n@@ end\n", 3,
			`content after "%% end", which terminates the patch; remove it or remove the terminator, or if the terminator is payload text, ` + way,
		},

		// Left as it was: "@@ old" closed at once by "@@ new" is an insert
		// attempted far more often than a quoted line, and its hint is for
		// that. The fuzz property below names this as its one exception.
		{"@@ old closed at once by @@ new", "", "@@ file a\n@@ old\n@@ new\nx\n", 2, insertHint},
		{"through a blank line", "", "@@ file a\n@@ old\n\n@@ new\nx\n", 2, insertHint},
		{"through a CRLF patch's blank line", "", "@@ file a\r\n@@ old\r\n\r\n@@ new\r\nx\r\n", 2, insertHint},
		{
			"closed by @@ new, under another marker", "%%", "%% file a\n%% old\n%% new\nx\n", 2,
			"%% old has an empty payload, which would match at every position in the file rather than at one. A single blank line joins to nothing, so leave two to match an empty line. To insert text, put a surrounding line in old and repeat it in new, or use %% append or %% prepend at a file boundary",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker := tt.marker
			if marker == "" {
				marker = DefaultMarker
			}
			p, err := Parse([]byte(tt.patch), marker)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("got %+v, %v; want a ParseError", p, err)
			}
			if pe.Line != tt.line || pe.Msg != tt.msg {
				t.Errorf("got line %d:\n  %s\nwant line %d:\n  %s", pe.Line, pe.Msg, tt.line, tt.msg)
			}
		})
	}
}

// A message that names a second line names it as the caller counts: under
// --each, a section's lines are the file's, not the section's, in the words
// as in the "patch line" before them.
func TestTheNamedLineCountsInTheFile(t *testing.T) {
	t.Run("renumber moves it with the error's own", func(t *testing.T) {
		err := namingLine(4, 2, func(at int) string { return "the line at " + strings.Repeat("I", at) })
		if got := (patchSection{line: 6}).renumber(nil, err); got != err {
			t.Fatalf("renumber returned %v, not the error it was given", got)
		}
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Line != 10 || pe.Msg != "the line at IIIIIIII" {
			t.Errorf("got %+v, want line 10 naming line 8", pe)
		}
	})

	t.Run("a file with no heading is not moved", func(t *testing.T) {
		err := namingLine(4, 2, func(at int) string { return strings.Repeat("I", at) })
		_ = patchSection{}.renumber(nil, err)
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Line != 4 || pe.Msg != "II" {
			t.Errorf("got %+v, want line 4 naming line 2", pe)
		}
	})

	t.Run("--each, both messages that name one", func(t *testing.T) {
		root := cliTree(t, map[string]string{"r.txt": "x\n"})
		set := filepath.Join(t.TempDir(), "set.hunk")
		must(t, os.WriteFile(set, []byte(
			"@@ patch a\n@@ file r.txt\n@@ old\nx\n@@ file q\n@@ new\ny\n"+
				"@@ patch b\n@@ file r.txt\n@@ old\n@@ create z\nq\n"), 0o644))
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", "true", "--each", set}, "")
		if code != exitUsage {
			t.Fatalf("exit %d, want %d: %s%s", code, exitUsage, out, errOut)
		}
		for _, want := range []string{
			`set.hunk:a: patch line 5: expected "@@ new" to close the "@@ old" at line 3, found "@@ file"`,
			`set.hunk:b: patch line 10: "@@ old" has an empty payload, ended by "@@ create z" at line 11, a directive`,
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr does not say %q:\n%s", want, errOut)
			}
		}
		assertUnchanged(t, root, before)
	})
}

// The message is a contract (§8.1), in both renderings: the issue's repro,
// refused at exit 1 with nothing written.
func TestTheQuotedPatchMessageIsGolden(t *testing.T) {
	const patch = "@@ file r.txt\n@@ old\n@@ patch a\n@@ new\ny\n"
	for _, asJSON := range []bool{false, true} {
		name := "cli-parse-quoted-patch"
		var args []string
		if asJSON {
			name += "-json"
			args = []string{"--json"}
		}
		t.Run(name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"r.txt": "x\n"})
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, args, patch)
			if code != exitUsage {
				t.Fatalf("exit %d: %s%s", code, out, errOut)
			}
			assertUnchanged(t, root, before)
			if asJSON {
				golden(t, name, out)
				return
			}
			golden(t, name, errOut)
		})
	}
}

// FuzzAQuotedPatchNamesMarker is the issue's ask as a property. Take a run of
// lines from a patch or break set that reads, and quote it as a hunk's old or
// new. If the quote reads under a marker the lines do not use, then under the
// default one it reads too, or its refusal names --marker: whatever a quoted
// directive breaks, the message offers the way to write it as text. The one
// exception is the empty old closed at once by "@@ new", which keeps its hint
// for inserting text (TestAQuotedDirectiveNamesMarker says why).
func FuzzAQuotedPatchNamesMarker(f *testing.F) {
	f.Add("@@ patch a\n@@ file r.txt\n@@ old\nx\n@@ new\ny\n@@ patch b\n@@ delete q\n", uint16(0), uint16(4), true)
	f.Add("@@ patch a\n@@ file r.txt\n@@ old\nx\n@@ new\ny\n", uint16(2), uint16(5), false)
	f.Add(formatExamplePatch, uint16(3), uint16(9), true)
	f.Add("@@ file a\n@@ old\nx\n@@ new\ny\n@@ end\n", uint16(1), uint16(5), false)
	f.Add("@@ file a\n@@ old\n@@include b\n@@ new\nc\n", uint16(2), uint16(3), false)
	f.Add("@@ file a\n@@ old\nx\n@@ new\ny\n", uint16(3), uint16(5), true)
	// One per refusal, since go test replays the seeds and nothing else: an
	// old the patch ends inside, a quoted "@@ new" closing the outer old, a
	// directive closing an old with text, and text after a quoted "@@ end".
	f.Add("@@ file a\n@@ old\nx\n@@ new\ny\n", uint16(0), uint16(2), false)
	f.Add("@@ file a\n@@ old\nx\n@@ new\ny\n", uint16(2), uint16(4), true)
	f.Add("@@ create a\nx\n@@ delete b\n", uint16(1), uint16(3), true)
	f.Add("@@ file a\n@@ old\nx\n@@ new\ny\n@@ end\n", uint16(2), uint16(6), true)
	f.Fuzz(func(t *testing.T, src string, from, to uint16, asOld bool) {
		secs, err := splitPatchSet([]byte(src), DefaultMarker)
		if err != nil {
			return
		}
		for _, sec := range secs {
			if _, err := Parse(sec.src, DefaultMarker); err != nil {
				return
			}
		}
		lines := splitLines([]byte(src))
		a, b := int(from)%(len(lines)+1), int(to)%(len(lines)+1)
		if a > b {
			a, b = b, a
		}
		if a == b {
			return
		}
		quoted := string(bytes.Join(lines[a:b], []byte{'\n'}))
		wrap := func(m string) []byte {
			if asOld {
				return []byte(m + " file f\n" + m + " old\n" + quoted + "\n" + m + " new\nx\n")
			}
			return []byte(m + " file f\n" + m + " old\nx\n" + m + " new\n" + quoted + "\n")
		}
		if _, err := Parse(wrap("%%"), "%%"); err != nil {
			return // a quote no marker would take as it stands
		}
		patch := wrap(DefaultMarker)
		_, err = Parse(patch, DefaultMarker)
		if err == nil {
			return
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("not a ParseError: %#v", err)
		}
		if strings.Contains(pe.Msg, markerWayOut) || closedAtOnceByNew(patch, pe.Line) {
			return
		}
		t.Errorf("quoting %q as %s: %v, which does not name --marker", quoted, map[bool]string{true: "old", false: "new"}[asOld], pe)
	})
}

// closedAtOnceByNew reports whether line ln of patch is an "@@ old" whose
// payload is empty and closed by "@@ new": the property's one exception, told
// by its shape, so that a quoted line given that shape's words is still seen.
func closedAtOnceByNew(patch []byte, ln int) bool {
	lines := splitLines(patch)
	if ln < 1 || ln > len(lines) {
		return false
	}
	if w, _, _ := directive(lines[ln-1], DefaultMarker); w != "old" {
		return false
	}
	old, next := collectPayload(lines, ln, DefaultMarker)
	if len(old) > 0 || next >= len(lines) {
		return false
	}
	w, _, _ := directive(lines[next], DefaultMarker)
	return w == "new"
}
