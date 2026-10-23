package main

import (
	"bytes"
	"fmt"
	"io"
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
type Slip struct {
	Kind SlipKind
	Hunk int    // 1-based, in the patch
	Line int    // the hunk's patch line
	Path string // as the patch wrote it
	// Count of the hunk's Of replacements did it; under @@ old xN only some
	// may, by what follows each.
	Count, Of int
	// Text is the line the first one leaves, without its ending: the one
	// before the empty line, the joined one, or the file's last.
	Text []byte
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
)

// jsonKind is the kind as --json's warnings carry it.
func (k SlipKind) jsonKind() string {
	switch k {
	case SlipEmptyLine:
		return "empty_line"
	case SlipJoinedLine:
		return "joined_line"
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
// it.
func (s noted) stands() bool {
	switch op := s.f.finalOp(); {
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
	cur := f.cur
	oldNL, newNL := bytes.HasSuffix(old, lf), bytes.HasSuffix(repl, lf)
	// An empty new with an old that ends in a newline is how lines are
	// deleted (§3.3), and ends that agree are what was meant.
	if oldNL == newNL || len(repl) == 0 {
		return
	}
	first := len(x.slips)
	at := 0
	for k := range h.Count {
		p := at + bytes.Index(cur[at:], old)
		at = p + len(old)
		// Where this replacement ends in next: each one before it moved it
		// by the difference in length.
		end := p + k*(len(repl)-len(old)) + len(repl)
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
		s := x.slipOf(first, kind)
		if s == nil {
			x.slips = append(x.slips, noted{Slip{
				Kind: kind, Hunk: n, Line: h.Line, Path: h.Path, Of: h.Count,
				Text: lineAt(next, end-1),
			}, f})
			s = &x.slips[len(x.slips)-1].Slip
		}
		s.Count++
	}
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
