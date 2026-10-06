package main

// The parser (§3).
//
// Turns patch text into a Patch and touches no file while doing it, which is
// what lets §6.1 promise "any syntax error, exit 1, nothing read" structurally
// rather than by discipline. TestParserHasNoFileAccess asserts that on the
// import list rather than trusting it.

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultMarker is the directive prefix (§3.2). --marker replaces it for a
// payload that genuinely contains a line like "@@ old".
const DefaultMarker = "@@"

// Op is what a hunk does to its file.
type Op uint8

const (
	OpReplace Op = iota
	OpCreate
	OpAppend
	OpPrepend
	OpDelete
)

func (o Op) String() string {
	switch o {
	case OpReplace:
		return "replace"
	case OpCreate:
		return "create"
	case OpAppend:
		return "append"
	case OpPrepend:
		return "prepend"
	case OpDelete:
		return "delete"
	}
	return fmt.Sprintf("Op(%d)", uint8(o))
}

// A Hunk is one edit. Path is resolved from the current-file register at the
// point the hunk was written, so nothing downstream has to track it.
//
// Line is the 1-based line in the patch of the directive that opened the hunk.
// It is not decoration: §5.2 prints "(patch line 9)" on every failure, and it
// is painful to reconstruct after parsing.
type Hunk struct {
	Op   Op
	Path string
	Line int

	// OpReplace only.
	Count int // expected occurrences, at least 1 (§3.4)
	Old   []byte
	New   []byte

	// OpCreate, OpAppend, OpPrepend.
	Body []byte
}

// A Patch is a batch of hunks in the order they were written. §3.5: they apply
// in that order, against the file as previous hunks in the same batch have
// left it.
//
// CRLF says every line the patch terminates ends in \r\n, which is what
// PowerShell sends down a pipe and what an editor set to CRLF writes. Under
// --eol auto the CR is then the patch's line terminator rather than payload
// (§6.4). A patch that mixes endings keeps its bytes, so a CR somebody put in a
// payload on purpose survives anywhere a patch is not uniformly CRLF.
type Patch struct {
	Hunks []Hunk
	CRLF  bool
}

// ParseError is every failure this file can produce, and all of them are
// exit 1 with nothing read (§4.1, §6.1). Line is 1-based; 0 means the error is
// about the patch as a whole rather than about a line in it.
type ParseError struct {
	Line int
	Msg  string
}

func (e *ParseError) Error() string {
	if e.Line == 0 {
		return e.Msg
	}
	return fmt.Sprintf("patch line %d: %s", e.Line, e.Msg)
}

// directiveWords is §3.2's rule 3, and it is the rule doing the real work. It
// is why a unified diff pasted into a payload ("@@ -1,3 +1,4 @@", whose first
// word is "-1,3") stays payload, and why a Markdown file full of "@@" does
// not confuse the parser.
var directiveWords = map[string]bool{
	"file": true, "old": true, "new": true, "create": true,
	"delete": true, "append": true, "prepend": true, "end": true,
}

// directive reports whether line is a directive, and if so its keyword and the
// argument text after it. All three of §3.2's conditions have to hold.
//
// One trailing CR is the line's terminator, not part of it. Until 2026-10-06 it
// stayed on whatever line carried it, so a patch sent through PowerShell ended
// in "@@ end\r", which is not "@@ end": it was written into the file as a line
// of text, at exit 0, and "@@ create b.txt\r" created a file whose name ended
// in a carriage return.
func directive(line []byte, marker string) (word, arg string, ok bool) {
	line = bytes.TrimSuffix(line, cr)
	if !bytes.HasPrefix(line, []byte(marker)) {
		return "", "", false
	}
	rest := line[len(marker):]
	if len(rest) == 0 || (rest[0] != ' ' && rest[0] != '\t') {
		return "", "", false
	}
	rest = bytes.TrimLeft(rest, " \t")
	if i := bytes.IndexAny(rest, " \t"); i >= 0 {
		word, arg = string(rest[:i]), string(bytes.Trim(rest[i:], " \t"))
	} else {
		word = string(rest)
	}
	if !directiveWords[word] {
		return "", "", false
	}
	return word, arg, true
}

// isControl reports whether r is a control character nobody puts in a file name
// or a directive on purpose: C0 but tab, DEL, and C1. Tab is a separator in a
// directive line (§3.2) and was always allowed inside a path. U+009B is a
// terminal's escape introducer as surely as ESC is.
func isControl(r rune) bool {
	return (r < 0x20 && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// controlAt reports whether s[i:] starts with a control character, by
// isControl, the name a message gives it, and how many bytes it takes either
// way. A C1 control is one as the code point and also as a lone byte 0x80 to
// 0x9F that is not part of a UTF-8 sequence: that is how an 8-bit terminal
// reads it, and Linux takes any byte in a name. Inside a valid sequence such a
// byte belongs to another character, as 0xA9 does to the é in "café", and is
// not one. Any other byte that is not UTF-8 is not a control character.
func controlAt(s string, i int) (name string, width int, ok bool) {
	r, w := utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && w == 1 && s[i] >= 0x80 && s[i] <= 0x9f {
		return fmt.Sprintf("0x%02X", s[i]), 1, true
	}
	if isControl(r) {
		return codePoint(r), w, true
	}
	return "", w, false
}

// firstControl names the first control character in s, by controlAt.
func firstControl(s string) (string, bool) {
	for i := 0; i < len(s); {
		name, w, ok := controlAt(s, i)
		if ok {
			return name, true
		}
		i += w
	}
	return "", false
}

// codePoint names a character the way a message can print it, which a control
// character cannot be: ESC printed raw is an instruction to the terminal.
func codePoint(r rune) string { return fmt.Sprintf("U+%04X", r) }

// showControls is s with every control character replaced by its name in angle
// brackets, for a message that has to repeat text it is refusing.
func showControls(s string) string {
	if _, ok := firstControl(s); !ok {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		name, w, ok := controlAt(s, i)
		if ok {
			b.WriteString("<" + name + ">")
		} else {
			b.WriteString(s[i : i+w])
		}
		i += w
	}
	return b.String()
}

// nearDirective reports the directive line would have been but for a control
// character in it: "@@ end" with a form feed after it, "@@ old" with an escape
// inside it, "@@" and "delete" with a vertical tab between them. The character
// is tried both ways, as nothing and as a space, since it can stand in a word or
// in place of the separator.
func nearDirective(line []byte, marker string) (string, bool) {
	s := string(line)
	for _, as := range []string{"", " "} {
		var b strings.Builder
		for i := 0; i < len(s); {
			_, w, ok := controlAt(s, i)
			if ok {
				b.WriteString(as)
			} else {
				b.WriteString(s[i : i+w])
			}
			i += w
		}
		if word, _, ok := directive([]byte(b.String()), marker); ok {
			return word, true
		}
	}
	return "", false
}

// splitLines splits on \n. A final \n terminates the last line rather than
// starting a new one (§3.3), so "a\nb\n" and "a\nb" are both two lines. That
// is what every heredoc and every editor produces, so the agent never has to
// think about it.
func splitLines(src []byte) [][]byte {
	if len(src) == 0 {
		return nil
	}
	if src[len(src)-1] == '\n' {
		src = src[:len(src)-1]
	}
	return bytes.Split(src, []byte{'\n'})
}

// collectPayload gathers lines from i to the next directive or end of input,
// joined by \n with NO trailing newline added (§3.3). It returns the payload
// and the index of the line that stopped it, without consuming that line.
//
// This is the one place the format is subtle, which is why §3.3 states the rule
// three times. A blank line before the next directive leaves an empty final
// element, and joining gives the trailing newline. A payload of a single blank
// line joins to nothing, which is correct and follows from the same rule.
func collectPayload(lines [][]byte, i int, marker string) ([]byte, int) {
	start := i
	for i < len(lines) {
		if _, _, ok := directive(lines[i], marker); ok {
			break
		}
		i++
	}
	if i == start {
		return []byte{}, i
	}
	return bytes.Join(lines[start:i], []byte{'\n'}), i
}

// parseCount reads the occurrence count from an "@@ old" argument (§3.4).
//
// Bare means exactly one, "x3" means exactly three and replaces all three.
// There is no "one or more" and no "zero or more": if the agent does not know
// the count, that is the thing the tool is here to refuse.
func parseCount(arg string, line int, marker string) (int, error) {
	if arg == "" {
		return 1, nil
	}
	bad := &ParseError{line, fmt.Sprintf(
		"%q is not an occurrence count; write %q for exactly one occurrence or %q for exactly three",
		arg, marker+" old", marker+" old x3")}

	digits, isX := strings.CutPrefix(arg, "x")
	if !isX || digits == "" {
		return 0, bad
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, bad
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, bad
	}
	if n == 0 {
		return 0, &ParseError{line, fmt.Sprintf(
			"%q asks for exactly zero occurrences, which asserts that text is absent rather than editing it; hunk has no way to express that",
			marker+" old x0")}
	}
	return n, nil
}

// Parse turns patch text into a Patch. Every failure is a *ParseError and
// means exit 1 with nothing read.
func Parse(src []byte, marker string) (*Patch, error) {
	if marker == "" {
		return nil, &ParseError{0, "marker must not be empty"}
	}
	lines := splitLines(src)
	nl := bytes.Count(src, lf)
	p := &Patch{CRLF: nl > 0 && bytes.Count(src, crlf) == nl}

	// A directive with a control character stuck to it, "@@ end" followed by a
	// form feed or "@@ old" by an escape, is not a directive, so it used to
	// become payload in silence, as "@@ end\r" did. That is never what was
	// meant, and wherever the line is, including inside a payload, it is
	// refused. The way to write such a line as payload is --marker, as for a
	// payload line that is a directive.
	for i, line := range lines {
		if _, _, ok := directive(line, marker); ok {
			continue
		}
		bare := bytes.TrimSuffix(line, cr)
		if word, ok := nearDirective(bare, marker); ok {
			name, _ := firstControl(string(bare))
			return nil, &ParseError{i + 1, fmt.Sprintf(
				"the line would be the directive %q but for %s in it, a control character; remove it, or if the line is payload text, choose another directive prefix with --marker",
				marker+" "+word, name)}
		}
	}

	// The current-file register (§3.5). "@@ file" sets it, and so does every
	// other directive that names a path: a "@@ old" following a "@@ delete"
	// must fail against the file that was just deleted, not silently apply to
	// whichever file happened to be in effect before it. §3.5 says this of
	// "@@ create" only; the generalization is the one sane reading.
	var cur string
	var curLine int

	// requirePath reads a path argument, which is the rest of the directive
	// line. Surrounding whitespace is trimmed: a path with a trailing space is
	// an editor artifact far more often than an intention, and untrimmed it
	// fails later with a message about a file that looks identical.
	requirePath := func(arg string, ln int, word string) (string, error) {
		if arg == "" {
			return "", &ParseError{ln, fmt.Sprintf("%q needs a path", marker+" "+word)}
		}
		return arg, nil
	}

	i := 0
loop:
	for i < len(lines) {
		ln := i + 1
		word, arg, ok := directive(lines[i], marker)
		if !ok {
			return nil, &ParseError{ln, fmt.Sprintf(
				"expected a directive, found payload text; a directive is %q at column 0, then a space or tab, then one of file old new create delete append prepend end",
				marker)}
		}
		i++

		switch word {
		case "end":
			if arg != "" {
				return nil, &ParseError{ln, fmt.Sprintf("%q takes no argument", marker+" end")}
			}
			if i < len(lines) {
				return nil, &ParseError{i + 1, fmt.Sprintf(
					"content after %q, which terminates the patch; remove it or remove the terminator",
					marker+" end")}
			}
			break loop

		case "file":
			path, err := requirePath(arg, ln, "file")
			if err != nil {
				return nil, err
			}
			cur, curLine = path, ln

		case "create", "append", "prepend":
			path, err := requirePath(arg, ln, word)
			if err != nil {
				return nil, err
			}
			cur, curLine = path, ln
			body, next := collectPayload(lines, i, marker)
			i = next
			op := OpCreate
			switch word {
			case "append":
				op = OpAppend
			case "prepend":
				op = OpPrepend
			}
			p.Hunks = append(p.Hunks, Hunk{Op: op, Path: path, Line: ln, Body: body})

		case "delete":
			path, err := requirePath(arg, ln, "delete")
			if err != nil {
				return nil, err
			}
			cur, curLine = path, ln
			p.Hunks = append(p.Hunks, Hunk{Op: OpDelete, Path: path, Line: ln})

		case "old":
			if cur == "" {
				return nil, &ParseError{ln, fmt.Sprintf(
					"no file is in effect; put %q before this",
					marker+" file <path>")}
			}
			count, err := parseCount(arg, ln, marker)
			if err != nil {
				return nil, err
			}
			old, next := collectPayload(lines, i, marker)
			i = next

			// An empty old matches at every position rather than at one:
			// strings.Count(s, "") is len(s)+1, and on an empty file it is
			// exactly 1, so the batch would succeed and insert at offset 0.
			// That is the wrong-position edit §2 exists to refuse, so it is
			// refused here rather than left to produce a count nobody can act
			// on. §3.3's "a payload may be empty" is about new, not old.
			//
			// In a CRLF patch a blank line is "\r", and --eol auto reads the
			// CR as its ending (convert), so an old of one blank line is empty
			// there too. Left to the parser's bytes it was "\r", passed this
			// check, and on an empty file inserted at offset 0: the fuzz
			// property that a CRLF patch is the same patch found it, on the
			// day the CR stripping was written, before it was committed.
			if len(old) == 0 || (p.CRLF && len(stripCR(old)) == 0) {
				return nil, &ParseError{ln, fmt.Sprintf(
					"%s has an empty payload, which would match at every position in the file rather than at one. A single blank line joins to nothing, so leave two to match an empty line. To insert text, put a surrounding line in old and repeat it in new, or use %s or %s at a file boundary",
					marker+" old", marker+" append", marker+" prepend")}
			}

			if i >= len(lines) {
				return nil, &ParseError{ln, fmt.Sprintf(
					"%q has no %q; every old needs the text to put in its place, even if that text is empty",
					marker+" old", marker+" new")}
			}
			w2, a2, _ := directive(lines[i], marker)
			if w2 != "new" {
				return nil, &ParseError{i + 1, fmt.Sprintf(
					"expected %q to close the %q at line %d, found %q",
					marker+" new", marker+" old", ln, marker+" "+w2)}
			}
			if a2 != "" {
				return nil, &ParseError{i + 1, fmt.Sprintf(
					"%q takes no argument; the occurrence count goes on %q",
					marker+" new", marker+" old")}
			}
			i++
			replacement, next := collectPayload(lines, i, marker)
			i = next
			p.Hunks = append(p.Hunks, Hunk{
				Op: OpReplace, Path: cur, Line: ln,
				Count: count, Old: old, New: replacement,
			})

		case "new":
			return nil, &ParseError{ln, fmt.Sprintf(
				"%q with no %q before it",
				marker+" new", marker+" old")}
		}
	}

	if len(p.Hunks) == 0 {
		if curLine > 0 {
			return nil, &ParseError{curLine, "the patch names a file but contains no hunks"}
		}
		return nil, &ParseError{0, "the patch is empty"}
	}
	return p, nil
}

// formatExamplePatch is §3.6's worked example, and it is the example that
// `hunk format` prints. TestFormatExampleParses parses it, so the help text
// cannot drift from the parser: a grammar change that this example does not
// survive fails the build.
//
// It differs from §3.6 as printed in one byte, and SPEC.md now matches: a blank
// line before the terminator, so the created Go file ends in a newline. The
// example as printed created a file without one, which is a bad thing to teach
// by example and which gofmt would immediately undo.
const formatExamplePatch = `@@ file internal/cli/root.go
@@ old
	bind.mustHaveBoundEveryGlobal()
@@ new
	bind.mustHaveBoundEveryGlobal()
	bind.mustHaveBoundEveryScope()
@@ old
	"github.com/kmoneil/jr/internal/registry"
@@ new
	"github.com/kmoneil/jr/internal/registry"
	"github.com/kmoneil/jr/internal/scope"
@@ file internal/registry/globals.go
@@ old x2
	GlobalProject
@@ new
	GlobalProject, GlobalScope
@@ create internal/cli/scope.go
package cli

// Scope binds the project scope flags.
func Scope() {}

`

// formatDoc is what `hunk format` prints (§2 goal 8: "An agent that has never
// seen the tool uses it correctly on its second call").
//
// The first paragraph says what the format is not, before the grammar rather
// than after it. That is the mitigation the name decision accepted a cost for:
// the binary is called hunk and the default marker is @@, and both point at
// unified diffs, which §2 lists as a non-goal. §3.2's rule 3 already keeps a
// pasted diff from confusing the parser; this is what keeps it from being
// pasted.
const formatDoc = `hunk applies literal text edits, in batches, as one transaction.

It is NOT a diff format. It does not read unified diffs. The "@@" below is a
directive marker, not a diff header, and there are no line numbers and no
context markers anywhere in it. You paste the exact text you want replaced.

GRAMMAR

  patch     := directive+ ["@@ end"]
  directive := file | replace | create | append | prepend | delete

  file      := "@@ file " path
  replace   := "@@ old" [" x" N] payload "@@ new" payload
  create    := "@@ create " path payload
  append    := "@@ append " path payload
  prepend   := "@@ prepend " path payload
  delete    := "@@ delete " path

A line is a directive only when all three of these hold: it starts with "@@" at
column 0, the next character is a space or tab, and the first word after that
is one of file old new create delete append prepend end. Every other line is
payload. So a Markdown file full of "@@" passes through untouched, and so does
a unified diff pasted into a payload, whose "@@ -1,3 +1,4 @@" fails the third
test. If a payload really does contain a line like "@@ old", --marker picks
another prefix.

A carriage return at the end of a directive line is its line ending, so a
patch written with CRLF line endings, as PowerShell pipes text, parses the
same. Under --eol auto, a patch whose every line ends in CRLF has its payload
read the same way; --eol strict keeps every payload byte. A control character
anywhere else in a directive line is refused and named, never written: in a
path with exit 2, and stuck to a directive word, as in "@@ end" followed by a
form feed, with exit 1.

The heredoc around a patch has a rule of its own, which is the shell's: a
payload line that is exactly the heredoc's delimiter (HUNK in the example
below) ends the heredoc there. hunk gets the lines above it, which may still be
a patch that applies, and the shell runs the rest as commands. Pick a delimiter
no payload line equals, or write the patch to a file and pass -f FILE, which no
line can end.

PATHS

"@@ file" stays in effect for every following "@@ old" until the next one, and
any directive naming a path sets it the same way. So "@@ old" hunks may follow
a "@@ create" and apply to the content it just created.

COUNTS

"@@ old" means exactly one occurrence. "@@ old x3" means exactly three, and
replaces all three. There is no "one or more" and no "zero or more". If you do
not know the count, that is the thing this tool exists to refuse: add
surrounding context to the old text until it is unique. Copies that overlap,
and under --eol auto copies in a file's other line ending, count as well, and
are refused rather than edited.

PAYLOADS

A payload is every line between its directive and the next one, joined by
newlines, with NO trailing newline added. To match text that ends in a newline,
leave a blank line before the next directive:

  @@ old
  func foo() {

  @@ new
  func foo(ctx context.Context) {

The rule is the same at the end of a patch: a blank line before the terminator
gives the last payload its trailing newline.

append and prepend add whole lines: a payload with no trailing newline gets one,
and append first ends the file's last line if it has none. create writes its
payload exactly, so leave the blank line if the new file should end in a
newline. The report says "(no final newline)" when it does not.

A created file's mode is 0644 less the umask, as for a shell redirect. A file
that existed keeps its mode, including one deleted and created again in the
same batch.

A payload may be empty. "@@ new" followed immediately by another directive
replaces the old text with nothing, which together with the blank-line rule
deletes a line, newline included.

ORDER

Hunks apply in the order written, against the file as earlier hunks in the same
batch have left it. Nothing is written until every hunk in the batch is known
to match. If --verify is given and its command fails, every file goes back.

EXAMPLE

  hunk --verify 'go build ./... && go test ./internal/cli/' <<'HUNK'
` + formatExamplePatch + `HUNK
`
