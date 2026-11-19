package main

// A taken line ending (#182).
//
// A replacement whose old and new both end on the carriage return of a CRLF
// the file has keeps that line ending. DiagnoseSplitCRLF lets it through,
// since nothing is parted, and a patch of CRLF lines written in an LF heredoc
// ends every payload that way. What it leaves its inverse is a carriage
// return to find that is half of a line ending, and under --eol auto a line
// ending is written in whichever ending the file has most of when the inverse
// is applied.
//
// So when a later hunk of the batch takes that line ending in, carriage
// return and newline both, and the batch leaves the file LF where it was
// CRLF, the inverse writes the line break back as a newline alone. The
// carriage return the first hunk's inverse looks for is then gone, and its old
// can match another at the right count. Until 2026-11-19 that applied at exit
// 0: the nightly fuzz found "x\r" -> "\r" and then "\n" -> "0" in "x\r\n\r",
// whose inverse left "\nx\r". Such a hunk is refused now, and the hunks that
// kept its line endings are named.
//
// Only a file whose line ending is CRLF can have one. In an LF file, a copy
// that takes a CRLF in has a CRLF's bytes in its old, which is a misread and
// refused before anything is kept or taken (DiagnoseMisreadCRLF, #170).
//
// The refusal stands whether or not the rest of the patch has an inverse. A
// patch is a concatenation of parts, and the part that holds the two hunks
// can be inverted on its own.

import (
	"fmt"
	"strings"
)

// keptCR is a carriage return a replacement kept, at its offset in the file
// as the batch has left it, and the hunk that kept it.
type keptCR struct{ at, by int }

// takenCRLF is a hunk that took in line endings earlier hunks kept: the line
// of each, as the file stood before the hunk, capped, how many there were,
// the hunks that kept them, and how many hunks had changed the file by then.
type takenCRLF struct {
	hunk      int
	by        []int
	lines     []int
	found     int
	moreLines int
	shifted   int
}

// trackKept follows f.kept through hunk n's replacement of count copies of
// old with repl in f.cur, which has not happened yet. A kept carriage return
// that a copy holds is rewritten, and one whose newline the copy holds too is
// taken in, which is recorded in f.taken; the rest shift by what the copies
// before them added or took out. Then the carriage returns this replacement
// keeps are added. old and repl are the payloads as --eol auto converted
// them.
//
// Nothing is looked for unless there is something kept or to keep, so a
// batch that never ends old and new on a carriage return pays nothing.
func (f *file) trackKept(n int, old, repl []byte, count int) {
	keeps := len(old) > 0 && old[len(old)-1] == '\r' && len(repl) > 0 && repl[len(repl)-1] == '\r'
	if len(f.kept) == 0 && !keeps {
		return
	}
	at := copiesAt(f.cur, old, count)
	grow := len(repl) - len(old)
	var kept []keptCR
	var offsets, by []int
	taken := 0
	k := 0 // the copies wholly before the carriage return
	for _, c := range f.kept {
		for k < len(at) && at[k]+len(old) <= c.at {
			k++
		}
		if k < len(at) && at[k] <= c.at {
			if c.at+1 < at[k]+len(old) {
				taken++
				if len(offsets) < tooManyLimit {
					offsets = append(offsets, c.at)
				}
				by = insertHunk(by, c.by)
			}
			continue
		}
		kept = append(kept, keptCR{at: c.at + k*grow, by: c.by})
	}
	if taken > 0 {
		f.taken = append(f.taken, takenCRLF{
			hunk: n, by: by, lines: lineNumbers(f.cur, offsets),
			found: taken, moreLines: taken - len(offsets), shifted: f.applied,
		})
	}
	var fresh []keptCR
	if keeps {
		for i, s := range at {
			if e := s + len(old); e < len(f.cur) && f.cur[e] == '\n' {
				fresh = append(fresh, keptCR{at: s + i*grow + len(repl) - 1, by: n})
			}
		}
	}
	f.kept = mergeKept(kept, fresh)
}

// mergeKept is a and b, each in file order, as one list in file order. A
// merge and not a sort: the generic sort's code alone was most of what the
// binary's size budget had left.
func mergeKept(a, b []keptCR) []keptCR {
	if len(b) == 0 {
		return a
	}
	out := make([]keptCR, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		if a[0].at < b[0].at {
			out, a = append(out, a[0]), a[1:]
		} else {
			out, b = append(out, b[0]), b[1:]
		}
	}
	return append(append(out, a...), b...)
}

// insertHunk is ns, in order and each once, with n in it.
func insertHunk(ns []int, n int) []int {
	i := 0
	for i < len(ns) && ns[i] < n {
		i++
	}
	if i < len(ns) && ns[i] == n {
		return ns
	}
	ns = append(ns, 0)
	copy(ns[i+1:], ns[i:])
	ns[i] = n
	return ns
}

// takenCRLFs is a failure for each hunk that took in a line ending an earlier
// hunk kept, in a file the batch leaves with LF line endings where it had
// CRLF. Only the final state says which files those are, so it is asked
// after the walk, as pathConflicts is, and a file a hunk failed on has no
// final state to ask.
func (x *Txn) takenCRLFs(p *Patch) []Failure {
	var failures []Failure
	for _, f := range x.files {
		if f.failedAt != 0 || dominantEOL(f.cur) == f.eol {
			continue
		}
		for _, t := range f.taken {
			h := p.Hunks[t.hunk-1]
			failures = append(failures, Failure{
				Hunk: t.hunk, Path: h.Path, PatchLine: h.Line,
				Expected: h.Count, Found: h.Count, Near: t.diagnosis(),
			})
		}
	}
	return failures
}

// diagnosis is the refusal's near-miss block: where each line ending was, and
// what the hunks that kept them would lose.
func (t takenCRLF) diagnosis() *Diagnosis {
	who := hunkList(t.by)
	const leaves = ", and the batch leaves the file's line ending LF where it was CRLF, so the batch's inverse would write the "
	var detail string
	switch {
	case t.found == 1:
		detail = who + "'s old and new end on that line ending's carriage return" + leaves +
			"line break back as a newline alone, and the carriage return " + who + "'s inverse looks for would be gone"
	case len(t.by) == 1:
		detail = who + "'s old and new end on those line endings' carriage returns" + leaves +
			"line breaks back as newlines alone, and the carriage returns " + who + "'s inverse looks for would be gone"
	default:
		detail = "the old and new of " + who + " end on those line endings' carriage returns" + leaves +
			"line breaks back as newlines alone, and the carriage returns their inverses look for would be gone"
	}
	return &Diagnosis{
		Kind: DiagTakenCRLF, Cause: "taken line ending",
		Detail: detail + "; write this hunk and " + who + " as one, or pass --eol strict to edit the bytes as they are",
		Found:  t.found, Lines: t.lines, MoreLines: t.moreLines, Shifted: t.shifted,
	}
}

// hunkList names hunks in prose: "hunk 1", "hunks 1 and 3", "hunks 1, 2 and 3".
func hunkList(ns []int) string {
	if len(ns) == 1 {
		return fmt.Sprintf("hunk %d", ns[0])
	}
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = fmt.Sprint(n)
	}
	return "hunks " + strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}
