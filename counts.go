package main

import "bytes"

// replaceCounts is §5.1's +N -M for a replacement of the n occurrences of old
// in cur by repl, decided 2026-10-22 from issue #111: the file's lines, as git
// diff --numstat counts them, rather than the payloads'. Until then a
// replacement added lineCount(new) and removed lineCount(old) per occurrence,
// so every line of context repeated in both counted as removed and added, and
// a new text gaining a newline its old did not have added no line at all:
// #111's batch took a file from three lines to five and said +2 -2.
//
// Per occurrence, the region is the whole lines its match touches, the line
// it ends on with the newline after it, so that an empty line it leaves is in
// the region, and the next line too when the replacement runs into it, so
// that a line it joins is. Before and after, less the lines the two have in
// common at the start and at the end, are what it removed and added. Occurrences whose
// regions share a line are one region, so x2 on "1 1" is one line changed.
// Two hunks that touch one line each count it, as two commits would: one
// count for the whole file needs a diff of it, which this does not do.
//
// Each occurrence is found as bytes.Replace finds it, left to right and not
// overlapping, which Validate has shown are exactly n of them.
//
// So the bounds of each one's lines are found by cursors that only move
// forward, and cur is read a few times over however many occurrences share a
// line. Until #134 each occurrence looked back from itself to its line's start
// and on to its line's end, which on one line is the whole file each time:
// 207,002 copies on one 220 KB line took 11 seconds on a laptop, and on a CI
// runner more than the 60 the fuzzer allows one input.
func replaceCounts(cur, old, repl []byte, n int) (added, removed int) {
	start, stop, at := 0, 0, 0 // the region open in cur, and where the last match ended
	var after []byte
	flush := func() {
		after = append(after, cur[at:stop]...)
		a, r := regionCounts(cur[start:stop], after)
		added, removed = added+a, removed+r
	}
	// lineAt is where the line holding at starts, and nl the first newline
	// at or past where one was last looked for, or len(cur) if there is none.
	lineAt, nl := 0, -1
	oldNL := bytes.LastIndexByte(old, '\n')
	lineEnd := func(end int) int { // past the newline ending end's line, or len(cur)
		if nl < end {
			nl = len(cur)
			if i := bytes.IndexByte(cur[end:], '\n'); i >= 0 {
				nl = end + i
			}
		}
		return min(nl+1, len(cur))
	}
	for k := range n {
		p := at + bytes.Index(cur[at:], old)
		end := p + len(old)
		ls := lineAt
		if i := bytes.LastIndexByte(cur[at:p], '\n'); i >= 0 {
			ls = at + i + 1
		}
		lineAt = ls
		if oldNL >= 0 {
			lineAt = p + oldNL + 1
		}
		le := end
		if cur[end-1] != '\n' {
			le = lineEnd(end)
		}
		if k > 0 && ls < stop {
			// On a line of the open region: it grows to take this one in.
			after = append(after, cur[at:p]...)
		} else {
			if k > 0 {
				flush()
			}
			start, after = ls, append([]byte(nil), cur[ls:p]...)
		}
		after = append(after, repl...)
		if le == end && end < len(cur) && len(after) > 0 && after[len(after)-1] != '\n' {
			// The match ended its line and the replacement did not: the next
			// line is joined on, and changed.
			le = lineEnd(end)
		}
		at, stop = end, le
	}
	flush()
	return added, removed
}

// regionCounts is the lines after has that before does not, and the other way
// round, as a minimal line diff of the two counts them, which is what git
// does. Each line is compared with its ending, so one that gained or lost its
// newline counts as changed, as it does in git.
//
// The lines the two share at the start and the end are set aside first, and
// the longest common subsequence of what is left is the rest in common: a
// block of ten lines with the third and the seventh rewritten is +2 -2, not
// the +5 -5 of everything between them.
func regionCounts(before, after []byte) (added, removed int) {
	b, a := fileLines(before), fileLines(after)
	for len(b) > 0 && len(a) > 0 && bytes.Equal(b[0], a[0]) {
		b, a = b[1:], a[1:]
	}
	for len(b) > 0 && len(a) > 0 && bytes.Equal(b[len(b)-1], a[len(a)-1]) {
		b, a = b[:len(b)-1], a[:len(a)-1]
	}
	common := commonLines(b, a)
	return len(a) - common, len(b) - common
}

// lcsMost is the most cells commonLines fills, about 4 million, a few
// milliseconds. A replacement whose changed middle is larger, two blocks of
// two thousand lines with nothing in common at either end, counts the whole
// middle as changed: more than git would say, never less, and the file's
// change in lines exact all the same.
const lcsMost = 1 << 22

// commonLines is the length of the longest common subsequence of two runs of
// lines, by the textbook table, a row at a time, or 0 past lcsMost.
func commonLines(b, a [][]byte) int {
	if len(b) == 0 || len(a) == 0 || len(b)*len(a) > lcsMost {
		return 0
	}
	ids := map[string]int{}
	id := func(l []byte) int {
		n, ok := ids[string(l)]
		if !ok {
			n = len(ids)
			ids[string(l)] = n
		}
		return n
	}
	bi, ai := make([]int, len(b)), make([]int, len(a))
	for i, l := range b {
		bi[i] = id(l)
	}
	for j, l := range a {
		ai[j] = id(l)
	}
	prev, row := make([]int, len(a)+1), make([]int, len(a)+1)
	for _, x := range bi {
		for j, y := range ai {
			if x == y {
				row[j+1] = prev[j] + 1
			} else {
				row[j+1] = max(row[j], prev[j+1])
			}
		}
		prev, row = row, prev
	}
	return prev[len(a)]
}

// fileLines is b's lines with their endings, in lineCount's sense: a run
// ending in a newline, and a final run that does not. The parser's
// splitLines drops the endings, which here are half of what a line is.
func fileLines(b []byte) [][]byte {
	var out [][]byte
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n') + 1
		if i == 0 {
			i = len(b)
		}
		out = append(out, b[:i])
		b = b[i:]
	}
	return out
}
