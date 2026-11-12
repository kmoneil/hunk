package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// A Slip is a replacement whose two ends disagree about a newline (#111),
// which in the field was a blank line left between hunks to keep a generated
// patch readable. Rule 1 (§3.3) made each of those a newline in the new text
// that the old text did not have, and every one-line replacement gained an
// empty line after it: 53 of them, at exit 0, which only git diff showed. The
// batch applies as written, since the rule is working; the report says what
// the ends did, after everything else, as it does for a stray (#92).
//
// The rule is the narrow one. An old with a newline and a new with two
// (a\n to A\n\n) is how an empty line is added on purpose, so it is not
// warned of, and is the way to mean the empty line. A new ending in a newline
// at the end of a file that had no final newline gives it one, which is
// benign. The same disagreement at the payloads' starts has not been seen.
//
// A replacement that changed nothing is one too (#118): in the field, a hunk
// in a batch of twelve whose new text had been left as its old, which the
// report counted among the hunks and hid in the file's row of the others.
// Rereading the patch found it. The no-op is decided (the file is not
// rewritten); the warning says it happened.
//
// An empty new is how lines are deleted, when its old ends in a newline. When
// it does not, and the old text is whole lines but for the last one's break,
// the text goes and the break stays, an empty line where it was (#150): in the
// field, a row taken out of a markdown table, whose empty line ended the
// table there. The same empty line #111 leaves, reached from the other side.
//
// And a deletion whose ends agree can still leave an empty line too many
// (#169): text that stood between two empty lines, taken out, lets them touch.
// In the field it was a function between two others, whose formatter check
// then refused the file three rounds running with no word of which hunk did
// it; a blank line under @@ new, the obvious fix, made it three.
type Slip struct {
	Kind SlipKind
	Hunk int    // 1-based, in the patch
	Line int    // the hunk's patch line
	Path string // as the patch wrote it
	// Count of the hunk's Of replacements did it; under @@ old xN only some
	// may, by what follows each.
	Count, Of int
	// Text is the line the first one leaves, without its ending: the one
	// before the empty line, the joined one, or the file's last. For an
	// emptied line it is the line as it was, before its text went.
	Text []byte
	// For joined empty lines, of the first: how many empty lines in a row it
	// leaves, the most either side had, and how many of the ones below old it
	// would have to take as well to leave that many; whether its old stopped
	// before its line break, which goes too; and whether its new was line
	// breaks rather than empty.
	Run, Kept, Take int
	Emptied, Breaks bool
}

// SlipKind says which way the ends disagreed and what that left.
type SlipKind int

const (
	// The new text ends in a newline and the old does not, and a newline
	// follows the match: an empty line after the replacement.
	SlipEmptyLine SlipKind = iota + 1
	// The old text ends in a newline and the new, which is not empty, does
	// not: the line after the match is joined onto the replacement.
	SlipJoinedLine
	// The same, at the end of the file: the file no longer ends in a newline.
	SlipNoFinalNewline
	// The new text is the old text, so the hunk matched and changed nothing.
	SlipNoChange
	// The new text differs from the old only in line endings, which --eol
	// auto writes as the file's, so the hunk changed nothing either. The fix
	// is --eol strict rather than the patch, so it is said apart.
	SlipLineEndings
	// The new text is empty and the old text, which does not end in a
	// newline, started a line and stopped before its break: the break stays,
	// an empty line where the text was.
	SlipEmptiedLine
	// The new text is empty or only line breaks, the old text has a line of
	// text and stood between empty lines, and with it gone they touch: one
	// run of empty lines where there were two. An emptied line between two
	// empty lines is this, not SlipEmptiedLine, so that one remedy covers
	// both.
	SlipJoinedEmptyLines
)

// jsonKind is the kind as --json's warnings carry it. Both ways of changing
// nothing are one kind, which says what the hunk did; the message says why.
// So are both ways of leaving an empty line.
func (k SlipKind) jsonKind() string {
	switch k {
	case SlipEmptyLine, SlipEmptiedLine, SlipJoinedEmptyLines:
		return "empty_line"
	case SlipJoinedLine:
		return "joined_line"
	case SlipNoChange, SlipLineEndings:
		return "no_change"
	default:
		return "no_final_newline"
	}
}

// noted is a slip and the file it is in, which result reads to tell whether
// the batch's end still bears it out.
type noted struct {
	Slip
	f *file
}

// stands reports whether the batch's end still bears the slip out, as far
// as the end can say: not in a file the batch deletes, and a final newline
// taken away that no later hunk gave back and that the file's row does not
// already say, as a create's does (§5.1). An empty or a joined line that a
// later hunk mended is not seen, and the warning names the hunk that left
// it. A hunk that changed nothing stands whatever came after it, the file's
// delete included: what it was written to do was not done.
func (s noted) stands() bool {
	switch op := s.f.finalOp(); {
	case s.Kind == SlipNoChange || s.Kind == SlipLineEndings:
		return true
	case op == "" || op == "delete":
		return false
	case s.Kind == SlipNoFinalNewline:
		return !s.f.created && !bytes.HasSuffix(s.f.cur, lf)
	}
	return true
}

// noteSlips records hunk n's replacements whose ends disagree about a
// newline, f.cur being the file as the hunks before it left it, next as this
// one leaves it, and old and repl the payloads in the file's line ending.
// Each occurrence is found as bytes.Replace finds it, left to right and not
// overlapping, which Validate has already shown are exactly h.Count of them,
// and judged by what follows it in next: under @@ old xN the newline after
// one match can be the start of the next, which the fuzzer found, and then
// no empty line is left.
func (x *Txn) noteSlips(n int, h Hunk, f *file, old, repl, next []byte) {
	changes := make([]change, 0, h.Count)
	for at := 0; len(changes) < h.Count; {
		p := at + bytes.Index(f.cur[at:], old)
		at = p + len(old)
		changes = append(changes, change{at: p, end: at, repl: repl})
	}
	x.noteChangeSlips(n, h, f, changes, next)
}

// noteChangeSlips is noteSlips for any changes, each its own old and its own
// replacement, which an @@ re hunk's matches are (#161). Each change is
// judged on its own ends. A replacement with a newline at its end where its
// old had none leaves an empty line when a line break follows it; one with
// none where its old had one joins the next line on, or leaves the file with
// no final newline. An empty replacement of an old that does not end in a
// newline leaves an empty line where the old started a line and stopped
// before its break (#150), counted once for replacements side by side on one
// line, and quoting the line whose break stays, as it was before its text
// went; one with no text, a CR alone before its LF that --eol strict took,
// was empty already. An empty one of an old that ends in a newline is how
// lines are deleted (§3.3), and ends that agree are what was meant.
//
// Before those, a run of touching changes whose new text is empty or only
// line breaks, with an empty line just above it and one just below, is judged
// whole (#169): with its text gone the two empty lines touch, and the warning
// says how many are in a row now. Its changes are then not judged again, so
// an emptied line between two empty lines has the one warning whose remedy
// covers both.
//
// The k-th change starts in next where it started in f.cur, moved by the
// difference in length of every change before it.
func (x *Txn) noteChangeSlips(n int, h Hunk, f *file, changes []change, next []byte) {
	cur := f.cur
	first := len(x.slips)
	note := func(kind SlipKind, text []byte) *Slip {
		s := x.slipOf(first, kind)
		if s == nil {
			x.slips = append(x.slips, noted{Slip{
				Kind: kind, Hunk: n, Line: h.Line, Path: h.Path, Of: h.Count, Text: text,
			}, f})
			s = &x.slips[len(x.slips)-1].Slip
		}
		s.Count++
		return s
	}
	joined := make([]bool, len(changes))
	for i, d := 0, 0; i < len(changes); {
		j, e := i, changes[i].at+d
		var repl []byte
		for ; ; j++ {
			c := changes[j]
			repl = append(repl, c.repl...)
			d += len(c.repl) - (c.end - c.at)
			if j+1 == len(changes) || changes[j+1].at != c.end {
				break
			}
		}
		if s, ok := joinsEmptyLines(cur, next, changes[i].at, changes[j].end, e, repl); ok {
			if slip := note(SlipJoinedEmptyLines, s.Text); slip.Count == 1 {
				slip.Run, slip.Kept, slip.Take, slip.Emptied, slip.Breaks = s.Run, s.Kept, s.Take, s.Emptied, s.Breaks
			}
			for k := i; k <= j; k++ {
				joined[k] = true
			}
		}
		i = j + 1
	}
	d, last := 0, -1
	for i, c := range changes {
		old, repl := cur[c.at:c.end], c.repl
		e := c.at + d
		end := e + len(repl)
		d += len(repl) - len(old)
		if joined[i] {
			continue
		}
		oldNL, newNL := bytes.HasSuffix(old, lf), bytes.HasSuffix(repl, lf)
		if len(repl) == 0 && !oldNL {
			if len(old) == 0 || e == last || (e > 0 && next[e-1] != '\n') ||
				!(bytes.HasPrefix(next[e:], lf) || bytes.HasPrefix(next[e:], crlf)) {
				continue
			}
			last = e
			if line := lineAt(cur, c.end-1); len(line) > 0 {
				note(SlipEmptiedLine, line)
			}
			continue
		}
		if oldNL == newNL || len(repl) == 0 {
			continue
		}
		var kind SlipKind
		switch {
		case newNL && (bytes.HasPrefix(next[end:], lf) || bytes.HasPrefix(next[end:], crlf)):
			kind = SlipEmptyLine
		case newNL:
			// Inside a line, the new text breaks it, which reads as meant; at
			// the end of a file, it gives the file a final newline.
			continue
		case end < len(next):
			kind = SlipJoinedLine
		default:
			kind = SlipNoFinalNewline
		}
		note(kind, lineAt(next, end-1))
	}
}

// joinsEmptyLines reports whether replacing cur[at:end] with repl, which is
// next[e:] onwards, lets two runs of empty lines touch, and if so the slip's
// text and counts. It does when repl is empty or only line breaks, the old
// text starts a line and has a line with text, empty lines stand both above
// and below that text, and the run left where it was is longer than either.
// Above counts old's own empty lines before its first line of text; below
// counts those after its last, and the ones after its line break, or after
// the one that follows it when it stops before its own, which stays. An
// empty line is one with nothing but its ending. How many are in a row is
// counted in next, where an earlier change may have touched them too.
func joinsEmptyLines(cur, next []byte, at, end, e int, repl []byte) (Slip, bool) {
	old := cur[at:end]
	off := firstTextLine(old)
	if off < 0 || len(bytes.ReplaceAll(bytes.ReplaceAll(repl, crlf, nil), lf, nil)) > 0 ||
		(at > 0 && cur[at-1] != '\n') {
		return Slip{}, false
	}
	below, trail := end, 0
	emptied := !bytes.HasSuffix(old, lf)
	if !emptied {
		trail = emptyAbove(old, len(old))
	} else {
		switch {
		case bytes.HasPrefix(cur[end:], lf):
			below++
		case bytes.HasPrefix(cur[end:], crlf):
			below += 2
		default:
			return Slip{}, false
		}
	}
	lead := emptyBelow(old, 0)
	a, b := emptyAbove(cur, at)+lead, trail+emptyBelow(cur, below)
	run := emptyAbove(next, e) + emptyBelow(next, e)
	if a == 0 || b == 0 || run <= max(a, b) {
		return Slip{}, false
	}
	// The line quoted is the file's whole, as an emptied line's is: old may
	// stop before the end of its last.
	return Slip{
		Text: lineAt(cur, at+off), Run: run, Kept: max(a, b), Take: max(0, min(a, b)-lead-trail),
		Emptied: emptied, Breaks: len(repl) > 0,
	}, true
}

// firstTextLine is where the first line of b with anything in it but its
// ending starts, or -1 when there is none.
func firstTextLine(b []byte) int {
	off := 0
	for l := range bytes.SplitAfterSeq(b, lf) {
		if len(bytes.TrimSuffix(bytes.TrimSuffix(l, lf), cr)) > 0 {
			return off
		}
		off += len(l)
	}
	return -1
}

// emptyAbove is how many empty lines end just before b[at], which starts a
// line, counted up to the first line that is not empty.
func emptyAbove(b []byte, at int) int {
	n := 0
	for at > 0 {
		start := bytes.LastIndexByte(b[:at-1], '\n') + 1
		if !bytes.Equal(b[start:at-1], cr) && start != at-1 {
			break
		}
		n++
		at = start
	}
	return n
}

// emptyBelow is how many empty lines start at b[at] onwards, which starts a
// line, counted down to the first line that is not empty. A last line with
// no newline after it is not one.
func emptyBelow(b []byte, at int) int {
	n := 0
	for {
		switch {
		case bytes.HasPrefix(b[at:], lf):
			at++
		case bytes.HasPrefix(b[at:], crlf):
			at += 2
		default:
			return n
		}
		n++
	}
}

// noteNoChange records hunk n, which matched in f and changed nothing: its
// old and new are the same once converted for --eol. When the patch's bytes
// differ, they differ only in line endings, since that is all convert takes
// away. The line quoted is old's first, which is how the hunk is found in the
// patch.
func (x *Txn) noteNoChange(n int, h Hunk, f *file) {
	kind := SlipNoChange
	if !bytes.Equal(h.Old, h.New) {
		kind = SlipLineEndings
	}
	x.slips = append(x.slips, noted{Slip{
		Kind: kind, Hunk: n, Line: h.Line, Path: h.Path, Count: h.Count, Of: h.Count,
		Text: lineAt(h.Old, 0),
	}, f})
}

// slipOf is this hunk's slip of the kind, among those from first on, or nil.
func (x *Txn) slipOf(first int, kind SlipKind) *Slip {
	for i := first; i < len(x.slips); i++ {
		if x.slips[i].Kind == kind {
			return &x.slips[i].Slip
		}
	}
	return nil
}

// lineAt is the line of b that holds b[i], without its ending, or the one
// that b[i] ends when it is a newline: for a slip, the line before the
// empty one, the joined line, or the file's last. It is read off the bytes
// the hunk left, since under @@ old xN a replacement before this one may be
// on the same line.
func lineAt(b []byte, i int) []byte {
	start := bytes.LastIndexByte(b[:i], '\n') + 1
	end := len(b)
	if j := bytes.IndexByte(b[i:], '\n'); j >= 0 {
		end = i + j
	}
	return bytes.TrimSuffix(b[start:end], cr)
}

// writeSlips is the warning for each slip, after a blank line when gap says
// the report printed anything, and reports whether it wrote one. patchName
// is the patch file under --each, and "" otherwise.
func writeSlips(w io.Writer, patchName string, slips []Slip, onDisk, gap bool) bool {
	for i, s := range slips {
		if i == 0 && gap {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "warning: %s\n", slipMessage(patchName, s, onDisk))
	}
	return len(slips) > 0
}

// slipMessage is one slip's warning, which --json carries as its message.
// onDisk says the batch is applied, so the remedy is in the file; otherwise
// it is in the patch.
func slipMessage(patchName string, s Slip, onDisk bool) string {
	where := fmt.Sprintf("hunk %d (patch line %d)", s.Hunk, s.Line)
	if patchName != "" {
		where = fmt.Sprintf("%s hunk %d (line %d)", showControls(patchName), s.Hunk, s.Line)
	}
	path, line := showControls(s.Path), shownLine(s.Text)
	// Under @@ old xN, which of its replacements did it; the line quoted is
	// the first's.
	of := ""
	if s.Of > 1 {
		of = fmt.Sprintf("%d of its %d replacements", s.Count, s.Of)
		if s.Count == s.Of {
			of = fmt.Sprintf("each of its %d replacements", s.Of)
		}
	}
	plural := ""
	if s.Count > 1 {
		plural = "s"
	}
	var msg, fix string
	switch s.Kind {
	case SlipNoChange:
		// Nothing was written for it, so the remedy is the patch's
		// whatever became of the batch.
		return fmt.Sprintf("%s changes nothing in %s: its old and new text are the same; if a change was meant, write it under @@ new", where, path)
	case SlipLineEndings:
		return fmt.Sprintf("%s changes nothing in %s: its old and new text differ only in line endings, and --eol auto writes the file's; to change a line's ending, pass --eol strict", where, path)
	case SlipEmptyLine:
		msg = fmt.Sprintf("%s leaves an empty line after %s in %s", where, line, path)
		if of != "" {
			msg = fmt.Sprintf("%s leaves an empty line after %s in %s, the first after %s", where, of, path, line)
		}
		msg += ": its new text ends in a newline and its old text does not"
		fix = "take out the blank line under its new text"
		if onDisk {
			fix = "remove the empty line" + plural + " from " + path
		}
	case SlipEmptiedLine:
		msg = fmt.Sprintf("%s leaves an empty line where %s was in %s", where, line, path)
		if of != "" {
			msg = fmt.Sprintf("%s leaves an empty line where %s was in %s, the first where %s was", where, of, path, line)
		}
		msg += ": its new text is empty and its old text stops before the line break"
		fix = "leave a blank line under its old text, so the line break goes with it"
		if onDisk {
			fix = "remove the empty line" + plural + " from " + path
		}
	case SlipJoinedEmptyLines:
		run := count(s.Run, "empty line")
		msg = fmt.Sprintf("%s leaves %s in a row where %s was in %s", where, run, line, path)
		if of != "" {
			msg = fmt.Sprintf("%s leaves %s in a row where %s was in %s, the first where %s was", where, run, of, path, line)
		}
		what := "is empty"
		if s.Breaks {
			what = "is only line breaks"
		}
		stops := ","
		if s.Emptied {
			stops = ", its old text stops before the line break,"
		}
		msg += ": its new text " + what + stops + " and the empty lines above and below its old text now touch"
		below := "an empty line"
		if s.Take > 1 {
			below = fmt.Sprintf("%d empty lines", s.Take)
		}
		var parts []string
		if s.Breaks {
			parts = append(parts, "take the blank lines out from under its new text")
		}
		switch {
		case s.Emptied && s.Take == 0:
			parts = append(parts, "leave a blank line under its old text, so its line break goes with it")
		case s.Emptied:
			parts = append(parts, fmt.Sprintf("leave %d blank lines under its old text, so its line break and %s below go with it", s.Take+1, below))
		case s.Take == 1:
			parts = append(parts, "leave another blank line under its old text, so an empty line below goes with it")
		case s.Take > 1:
			parts = append(parts, fmt.Sprintf("leave %d more blank lines under its old text, so %s below go with it", s.Take, below))
		}
		fix = strings.Join(parts, ", and ")
		if onDisk {
			fix = "remove the extra empty line from " + path
			if s.Run-s.Kept > 1 || s.Count > 1 {
				fix = "remove the extra empty lines from " + path
			}
		}
	case SlipJoinedLine:
		msg = fmt.Sprintf("%s joins the next line onto its replacement in %s, making %s", where, path, line)
		if of != "" {
			msg = fmt.Sprintf("%s joins the next line onto %s in %s, the first making %s", where, of, path, line)
		}
		msg += ": its old text ends in a newline and its new text does not"
		fix = "leave a blank line under its new text, as under its old text"
		if onDisk {
			fix = "put the line break" + plural + " back in " + path
		}
	default:
		msg = fmt.Sprintf("%s leaves %s with no final newline: its old text ends in a newline at the end of the file and its new text does not", where, path)
		fix = "leave a blank line under its new text, as under its old text"
		if onDisk {
			fix = "end " + path + " in a newline again"
		}
	}
	return msg + "; if that was not meant, " + fix
}

// slipWarnings is slips as --json carries them, among the warnings.
func slipWarnings(patchName string, slips []Slip, onDisk bool) []jsonWarning {
	var out []jsonWarning
	for _, s := range slips {
		out = append(out, jsonWarning{
			Kind: s.Kind.jsonKind(), Patch: patchName, Hunk: s.Hunk, PatchLine: s.Line,
			Path: s.Path, Text: string(s.Text), Message: slipMessage(patchName, s, onDisk),
		})
	}
	return out
}
