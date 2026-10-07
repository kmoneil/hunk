package main

// Near-miss diagnosis (§7).
//
// This is the feature that makes the tool faster rather than merely safer.
// Mandatory uniqueness turns a silent wrong-occurrence edit into a refusal, and
// a refusal only pays for itself if the report carries the exact bytes to paste
// back. §7.1's floor is the product: every anchored near miss prints the file's
// actual span, whether or not the cause can be named. Naming it is a bonus.
//
// Nothing here reads a file or knows what a patch is.

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// DiagKind is which of §7's shapes a diagnosis is.
type DiagKind int

const (
	// DiagNamed: a normalization explained the miss (§7.1 step 3).
	DiagNamed DiagKind = iota
	// DiagClosest: nothing explained it, so the closest span and the first
	// differing line are reported (§7.1 step 4). This is the floor.
	DiagClosest
	// DiagNoAnchor: not one line of old appears in the file, even ignoring
	// whitespace. The text is not there and no span would help.
	DiagNoAnchor
	// DiagNoRoom: a line of old is in the file, but every place it sits is so
	// near the top that old's earlier lines would begin above line one. §7.1
	// step 2 skips such a span rather than clamping it, so there is nothing to
	// compare; saying "the text is not there" about it would be false, which is
	// what this kind exists to stop.
	DiagNoRoom
	// DiagTooMany: old occurs, just not the claimed number of times.
	DiagTooMany
	// DiagOverlap: old occurs more often than the count can see, because
	// copies share bytes. Counting and replacing both go left to right and skip
	// what they have passed, so of two overlapping copies only the leftmost is
	// seen by either, and editing it is a guess.
	DiagOverlap
	// DiagMixedEndings: under --eol auto, old occurs in the file's other line
	// ending too. The payload is translated to the dominant ending before it is
	// counted, so the count cannot see those copies, and editing the rest is a
	// guess.
	DiagMixedEndings
)

// A Diagnosis explains why a hunk did not apply. It renders to both the text
// and the JSON report, so §5.2's promise that "--json carries everything the
// text does" cannot drift.
type Diagnosis struct {
	Kind DiagKind

	// Cause is the short machine-readable name, §5.2's "cause": "indentation",
	// "trailing whitespace", "line endings", "unicode lookalikes",
	// "whitespace". Empty unless Kind is DiagNamed.
	Cause string
	// Headline is the human clause: "the text is there with different leading
	// whitespace".
	Headline string
	// Detail is the sentence after the span: "your old used a tab; the file
	// uses 4 spaces". May be empty.
	Detail string

	// Line is the 1-based line where the reported span starts.
	Line int
	// Span is the file's actual lines for that span. These are the bytes the
	// agent pastes.
	Span [][]byte
	// SpanLines is the span's whole length when --context cut it short, and
	// zero when it did not (§7.2: "beyond that it prints a count"). Until
	// 2026-10-07 a cut span said nothing, the skill said to paste the span, and
	// pasted it replaced the first lines of the block and left the rest, at
	// exit 0.
	SpanLines int
	// Also is where every other span as near as this one starts, capped, and
	// AlsoMore counts those not listed: every other span a normalization
	// explains (DiagNamed), or every other with as few differing lines
	// (DiagClosest). Until 2026-10-07 the report showed the first and said
	// nothing of the rest, and pasting it edited that copy, at exit 0, whichever
	// one the agent meant.
	Also     []int
	AlsoMore int
	// AlsoSame is how many of those spans hold the same bytes as this one
	// (DiagNamed). Pasted, the span then matches more than once and is refused
	// as too many, so the report may not say it edits one copy.
	AlsoSame int

	// DiffLine, Yours, Theirs and Column describe the first line that differs
	// (DiagClosest only). DiffLine is 1-based within old, and DiagNoRoom
	// borrows it for the line of old that matched: same meaning, a line number
	// into old, and it is what says how far old overhangs the top of the file.
	DiffLine int
	Yours    []byte
	Theirs   []byte
	Column   int

	// Lines are where each match starts (DiagTooMany, DiagOverlap and
	// DiagMixedEndings), capped.
	Lines []int
	// Endings is each listed match's line endings, "CRLF", "LF" or "mixed",
	// parallel to Lines (DiagMixedEndings only).
	Endings []string
	// MoreLines is how many matches were not listed.
	MoreLines int
	// Found is the total number of matches: what the suggested "@@ old x N"
	// needs (DiagTooMany), and the true count where the count missed some
	// (DiagOverlap, DiagMixedEndings).
	Found int

	// Shifted is the number of hunks that changed this file earlier in the same
	// batch. Non-zero means Line counts against the in-memory file rather than
	// the one on disk, and the report has to say so: nothing was written, so an
	// agent that opens the file sees different numbers.
	Shifted int
}

// tooManyLimit is §7.1's cap on listed match positions.
const tooManyLimit = 10

// Diagnose explains why old is absent from file. maxContext caps the printed
// span (§7.2): a diagnostic that blows the context window is a worse failure
// than no diagnostic.
func Diagnose(old, file []byte, maxContext int) *Diagnosis {
	oldLines := splitLines(old)
	fileLines := splitLines(file)
	if len(oldLines) == 0 {
		return &Diagnosis{Kind: DiagNoAnchor}
	}

	starts, high := candidateSpans(oldLines, fileLines)
	if len(starts) == 0 {
		if high.file < 0 {
			return &Diagnosis{Kind: DiagNoAnchor}
		}
		// The one line that did match is the whole report: it is where the
		// agent has to look, and its number is what says how far old overhangs
		// the top of the file.
		d := &Diagnosis{Kind: DiagNoRoom, Line: high.file + 1, DiffLine: high.old + 1}
		d.setSpan(spanAt(fileLines, high.file, 1), maxContext)
		return d
	}

	// §7.1 step 3: the first normalization that makes a candidate span equal to
	// old wins, and the candidates are ordered by anchor, most distinctive
	// first. Every candidate is asked, so the report can say where else the
	// text is as near.
	form := newOldForm(oldLines)
	var named *Diagnosis
	var also []int
	var shown []byte
	for _, start := range starts {
		span := spanAt(fileLines, start, len(oldLines))
		switch d := form.explain(span, file); {
		case d == nil:
		case named == nil:
			d.Line = start + 1
			d.setSpan(span, maxContext)
			named, shown = d, bytes.Join(span, lf)
		default:
			also = append(also, start+1)
			if bytes.Equal(bytes.Join(span, lf), shown) {
				named.AlsoSame++
			}
		}
	}
	if named != nil {
		named.setAlso(also)
		return named
	}

	// §7.1 step 4, the floor. Fewest differing lines, earliest on a tie so a
	// golden has one answer. The tie breaks on position in the file rather than
	// on which line of old anchored the span, so the report does not depend on
	// the search order. The rest of the tie is listed.
	diffs := make([]int, len(starts))
	best := 0
	for i, start := range starts {
		diffs[i] = differingLines(oldLines, spanAt(fileLines, start, len(oldLines)))
		if diffs[i] < diffs[best] || (diffs[i] == diffs[best] && start < starts[best]) {
			best = i
		}
	}
	span := spanAt(fileLines, starts[best], len(oldLines))
	d := &Diagnosis{Kind: DiagClosest, Line: starts[best] + 1}
	d.setSpan(span, maxContext)
	d.DiffLine, d.Yours, d.Theirs, d.Column = firstDifference(oldLines, span)
	for i, start := range starts {
		if i != best && diffs[i] == diffs[best] {
			also = append(also, start+1)
		}
	}
	d.setAlso(also)
	return d
}

// setSpan keeps span as the one to show, cut to maxContext lines (§7.2), and
// when it was cut, how long it was.
func (d *Diagnosis) setSpan(span [][]byte, maxContext int) {
	d.Span = span
	if maxContext > 0 && len(span) > maxContext {
		d.Span, d.SpanLines = span[:maxContext], len(span)
	}
}

// setAlso keeps where the other spans as near start, in file order, listing
// the first tooManyLimit and counting the rest, as too-many does.
func (d *Diagnosis) setAlso(lines []int) {
	sort.Ints(lines)
	if len(lines) > tooManyLimit {
		d.AlsoMore = len(lines) - tooManyLimit
		lines = lines[:tooManyLimit]
	}
	d.Also = lines
}

// DiagnoseTooMany reports where each occurrence starts (§7.1, last paragraph).
func DiagnoseTooMany(old, file []byte) *Diagnosis {
	d := &Diagnosis{Kind: DiagTooMany}
	// An empty needle makes bytes.Index return 0 forever and the scan never
	// advances. The parser refuses an empty old, so this is unreachable through
	// the tool, but a hang is the worst failure mode there is and this costs a
	// line. Found by a fuzz seed, which hung the whole suite.
	if len(old) == 0 {
		return d
	}
	for off := 0; ; {
		i := bytes.Index(file[off:], old)
		if i < 0 {
			break
		}
		at := off + i
		d.Found++
		if len(d.Lines) < tooManyLimit {
			d.Lines = append(d.Lines, bytes.Count(file[:at], lf)+1)
		} else {
			d.MoreLines++
		}
		off = at + len(old)
	}
	return d
}

// DiagnoseOverlap reports every copy of old in file, the overlapping ones
// included, for a count that saw fewer of them.
func DiagnoseOverlap(old, file []byte) *Diagnosis {
	n, starts := matchStarts(file, old, tooManyLimit)
	return &Diagnosis{
		Kind: DiagOverlap, Cause: "overlap",
		Detail: "overlapping copies share bytes, so one cannot be replaced without the other; " +
			"add a line of context so old fits one place",
		Found: n, Lines: lineNumbers(file, starts), MoreLines: n - len(starts),
	}
}

// DiagnoseMixedEndings reports every copy of old in file whatever its line
// endings, each labelled with them, for a count under --eol auto that saw only
// the copies in the file's dominant ending. old is the payload as the patch
// wrote it, and both are compared with CRLF folded to LF. Folding keeps every
// line break, so a line number in the folded file is the same line in the
// file.
func DiagnoseMixedEndings(old, file []byte) *Diagnosis {
	fold, folded := toEOL(old, "\n"), toEOL(file, "\n")
	n, starts := matchStarts(folded, fold, tooManyLimit)
	d := &Diagnosis{
		Kind: DiagMixedEndings, Cause: "mixed line endings",
		Detail: "the file has mixed line endings, and --eol auto counted only the copies in its " +
			"dominant one; add a line of context so old fits one place",
		Found: n, Lines: lineNumbers(folded, starts), MoreLines: n - len(starts),
	}
	// i walks file and j walks folded in step, so each copy's own bytes can be
	// read for what its line breaks are. A "\r\n" in file is one "\n" in
	// folded; every other byte is itself.
	i, j := 0, 0
	for _, p := range starts {
		for ; j < p; j++ {
			if file[i] == '\r' && i+1 < len(file) && file[i+1] == '\n' {
				i++
			}
			i++
		}
		sawCRLF, sawLF := false, false
		for k, at := j, i; k < p+len(fold); k++ {
			if folded[k] == '\n' {
				if file[at] == '\r' {
					sawCRLF = true
					at++
				} else {
					sawLF = true
				}
			}
			at++
		}
		switch {
		case sawCRLF && sawLF:
			d.Endings = append(d.Endings, "mixed")
		case sawCRLF:
			d.Endings = append(d.Endings, "CRLF")
		default:
			d.Endings = append(d.Endings, "LF")
		}
	}
	return d
}

// matchStarts counts every occurrence of pat in text, overlapping ones
// included, and returns the offsets of the first keep. It is Knuth-Morris-Pratt
// and linear in both: restarting bytes.Index one byte past each match is
// quadratic on a run of one repeated byte, which is the overlapping case
// exactly, and this runs on the shapes that are.
func matchStarts(text, pat []byte, keep int) (int, []int) {
	if len(pat) == 0 {
		return 0, nil
	}
	// fail[i] is the length of the longest proper prefix of pat[:i+1] that is
	// also its suffix: where to resume after a mismatch at i+1.
	fail := make([]int, len(pat))
	for i, k := 1, 0; i < len(pat); i++ {
		for k > 0 && pat[i] != pat[k] {
			k = fail[k-1]
		}
		if pat[i] == pat[k] {
			k++
		}
		fail[i] = k
	}
	n := 0
	var starts []int
	for i, k := 0, 0; i < len(text); i++ {
		for k > 0 && text[i] != pat[k] {
			k = fail[k-1]
		}
		if text[i] == pat[k] {
			k++
		}
		if k == len(pat) {
			n++
			if len(starts) < keep {
				starts = append(starts, i-len(pat)+1)
			}
			k = fail[k-1]
		}
	}
	return n, starts
}

// lineNumbers is the 1-based line each offset in b starts on. The offsets are
// capped at tooManyLimit by every caller, so counting from the top for each is
// linear in b.
func lineNumbers(b []byte, offsets []int) []int {
	lines := make([]int, len(offsets))
	for i, at := range offsets {
		lines[i] = bytes.Count(b[:at], lf) + 1
	}
	return lines
}

// candidateSpans implements §7.1 steps 1 and 2. It returns the file line
// indices where old would begin, in the order they are worth trying.
//
// §7.1 step 1 offered two lines of old as anchors, the first non-blank one and
// the longest, and then step 5 printed "not one line of your old appears in the
// file, even ignoring whitespace". Those two do not agree, and the field found
// the gap between them: an old of two lines, the first of which was the longest
// and appeared in the file only mid-line, the second of which was in the file
// verbatim. The first line matched nothing, the longest line was the first line
// again, and the second line was never offered to the search at all. The report
// said the text was not there about a file that contained half of it.
//
// Every line of old is an anchor candidate now, so the sentence is something
// the tool has checked rather than something it assumes. §7.1's two lines are
// still tried first and in its order, so an old that anchors today anchors on
// the same line afterwards; only "no anchor found" can move.
// It also returns the first match it had to reject for want of room above it,
// so that a search which finds nothing usable can still say what it saw. Both
// fields are -1 when every anchor matched nothing at all.
func candidateSpans(oldLines, fileLines [][]byte) ([]int, anchorMatch) {
	index := anchorIndex(fileLines)
	var out []int
	high := anchorMatch{file: -1, old: -1}
	seen := make(map[int]bool)
	for _, i := range anchorOrder(oldLines) {
		for _, at := range index[anchorKey(oldLines[i])] {
			// §7.1 step 2: the span starts where old would start, which is i
			// lines above the matched line. Identical when the anchor is old's
			// first line and wrong otherwise, which is the case the ordering
			// exists for. A span that would begin above line one is skipped
			// rather than clamped, because a clamped span compares the wrong
			// text.
			start := at - i
			if start < 0 {
				if high.file < 0 {
					high = anchorMatch{file: at, old: i}
				}
				continue
			}
			// Two anchors pointing at one span are one candidate: explain is a
			// pure function of the span, so asking it twice cannot say anything
			// new. This is also what bounds the search by the length of the
			// file rather than by that times the length of old (§7.2).
			if seen[start] {
				continue
			}
			seen[start] = true
			out = append(out, start)
		}
	}
	return out, high
}

// An anchorMatch is a line of old found at a line of the file, both 0-based.
type anchorMatch struct{ file, old int }

// anchorOrder is which lines of old to offer as anchors, best first: §7.1's
// first non-blank line, then §7.1's longest line, then the rest by decreasing
// length, earliest on a tie so the order is total.
//
// Length is the proxy for distinctiveness, because a long line is less likely
// to match a line it does not belong to. A blank line would match every blank
// line in the file, so blank lines are not anchors at all, which is also what
// keeps anchorKey's key non-empty.
func anchorOrder(oldLines [][]byte) []int {
	var order []int
	for i, l := range oldLines {
		if len(bytes.TrimSpace(l)) > 0 {
			order = append(order, i)
		}
	}
	if len(order) == 0 {
		return nil // old is entirely blank: no anchor and no fallback
	}
	rest := order[1:]
	sort.SliceStable(rest, func(a, b int) bool {
		return len(bytes.TrimSpace(oldLines[rest[a]])) >
			len(bytes.TrimSpace(oldLines[rest[b]]))
	})
	return order
}

// anchorIndex groups the file's lines by anchorKey, so that an anchor attempt
// is a lookup rather than a pass over the file. Two attempts could afford a
// pass each; one per line of old could not, and building this costs one pass,
// which is fewer than the two it replaces.
func anchorIndex(fileLines [][]byte) map[string][]int {
	index := make(map[string][]int, len(fileLines))
	for i, l := range fileLines {
		k := anchorKey(l)
		index[k] = append(index[k], i)
	}
	return index
}

// anchorKey is how two lines are compared when looking for candidates.
//
// §7.1 says "strip it", which reads as trimming whitespace, and that is wrong:
// a byte-exact comparison after trimming rejects exactly the lines rows 4 and 5
// of the table exist to explain. A curly quote or a no-break space makes the
// stripped forms unequal, no candidate is found, and the report says "no anchor
// found" about text that is plainly there.
//
// So the anchor is compared under the loosest normalization in the table:
// lookalikes folded and every whitespace run collapsed. Any pair equal under a
// stricter row is equal under this one, so nothing is lost and every row below
// becomes reachable. Found by reading the goldens, which said "the text is not
// there" about a file containing it.
func anchorKey(line []byte) string {
	return string(collapseSpace(foldLookalikes(line)))
}

func spanAt(fileLines [][]byte, start, n int) [][]byte {
	if start+n > len(fileLines) {
		n = len(fileLines) - start
	}
	return fileLines[start : start+n]
}

// a normalization from §7.1 step 3, in the order the table gives
type normalization struct {
	cause    string
	headline string
	apply    func([]byte) []byte
	detail   func(oldLines, span [][]byte, file []byte) string
}

func normalizations() []normalization {
	return []normalization{
		{
			cause:    "trailing whitespace",
			headline: "the text is there with different trailing whitespace",
			apply:    stripTrailing,
			detail:   detailTrailing,
		},
		{
			cause:    "indentation",
			headline: "the text is there with different leading whitespace",
			apply:    flattenIndent,
			detail:   detailIndent,
		},
		{
			cause:    "line endings",
			headline: "the text is there with different line endings",
			apply:    stripCR,
			detail:   detailEOL,
		},
		{
			cause:    "unicode lookalikes",
			headline: "the text is there with characters that look the same",
			apply:    foldLookalikes,
			detail:   detailUnicode,
		},
		{
			cause:    "whitespace",
			headline: "the text is there with different whitespace",
			apply:    collapseSpace,
			detail:   func(_, _ [][]byte, _ []byte) string { return "" },
		},
	}
}

// An oldForm is old prepared for comparison: its lines, their join, and that
// join under each normalization.
//
// Diagnose compares one span per candidate and there can be one candidate per
// line of the file, so anything computed from old inside that loop is paid for
// once per line of the file. An earlier explain rebuilt the join, allocated a
// fresh normalizations() slice, and applied all five normalizations to old on
// every span, which was half the work of a search whose cost is already the
// file times the length of old (§7.2). Hoisting it here is enforced by the
// shape rather than by care: there is no longer any code that can normalize
// old inside the span loop.
//
// joined and normed are shared across every comparison, so no normalization may
// write through its argument. None does, and TestNormalizationsDoNotMutateTheirInput
// is the assertion rather than this sentence.
type oldForm struct {
	lines  [][]byte
	joined []byte
	norms  []normalization
	normed [][]byte
	// key is anchorKey of the whole of old, for the prefilter in explain.
	key string
}

func newOldForm(oldLines [][]byte) *oldForm {
	o := &oldForm{lines: oldLines, joined: bytes.Join(oldLines, lf), norms: normalizations()}
	o.normed = make([][]byte, len(o.norms))
	for i, n := range o.norms {
		o.normed[i] = n.apply(o.joined)
	}
	o.key = anchorKey(o.joined)
	return o
}

// explain runs §7.1 step 3, stopping at the first normalization that makes the
// span equal to old.
func (o *oldForm) explain(span [][]byte, file []byte) *Diagnosis {
	if len(o.lines) != len(span) {
		return nil
	}
	s := bytes.Join(span, lf)
	if bytes.Equal(o.joined, s) {
		return nil // it matches here; the miss is elsewhere
	}
	// A sound prefilter (§7.2). anchorKey is looser than every row below, so
	// two texts any row makes equal have equal keys, and a span whose key
	// differs from old's cannot be named by any of them. It is the whole
	// span's key and not its lines': collapsing whitespace runs newlines into
	// the rest, so "a b\nc" and "a\nb c" are equal whole and unequal line by
	// line, and row 5 names that pair. It changes no output, only the cost of
	// a span nothing explains, and it is still linear in the span: a constant
	// factor, decided 2026-09-21 without a cap.
	if anchorKey(s) != o.key {
		return nil
	}
	for i, n := range o.norms {
		if bytes.Equal(o.normed[i], n.apply(s)) {
			return &Diagnosis{
				Kind:     DiagNamed,
				Cause:    n.cause,
				Headline: n.headline,
				Detail:   n.detail(o.lines, span, file),
			}
		}
	}
	return nil
}

// stripTrailing removes trailing spaces and tabs, and deliberately not \r.
// A carriage return at the end of a line in a CRLF file is a line ending, not
// trailing whitespace, and trimming it here would let row 1 swallow every CRLF
// mismatch and report it as "trailing whitespace differs". Row 3 exists to say
// something far more useful about those.
func stripTrailing(b []byte) []byte {
	lines := bytes.Split(b, lf)
	for i := range lines {
		lines[i] = bytes.TrimRight(lines[i], " \t")
	}
	return bytes.Join(lines, lf)
}

// flattenIndent removes each line's leading whitespace, leaving the rest of the
// line alone (§7.1: "leading only").
//
// §7.1 says "tabs and space runs both to one space", which cannot equate a line
// that is indented with one that is not: a run becomes one space and an absent
// run stays absent. That drops the most common shape of all, an old with a tab
// where the file has none, through to row 5, which reports the vaguer
// "whitespace differs" instead of naming the indentation. Removing the run
// instead is strictly more inclusive, and the rest of the line must still match
// exactly, so the message stays accurate. Found by reading the report goldens.
func flattenIndent(b []byte) []byte {
	lines := bytes.Split(b, lf)
	out := make([][]byte, len(lines))
	for i, l := range lines {
		out[i] = bytes.TrimLeft(l, " \t")
	}
	return bytes.Join(out, lf)
}

// stripCR removes a trailing carriage return from each line, which is what
// "CRLF to LF" means once the text has been split into lines. A whole-string
// replace of "\r\n" misses the last line's \r, because splitting took its \n
// away, and row 5 then swallowed every CRLF mismatch and called it "different
// whitespace". Found by reading the goldens.
func stripCR(b []byte) []byte {
	lines := bytes.Split(b, lf)
	for i := range lines {
		lines[i] = bytes.TrimSuffix(lines[i], []byte("\r"))
	}
	return bytes.Join(lines, lf)
}

func collapseSpace(b []byte) []byte {
	return []byte(strings.Join(strings.Fields(string(b)), " "))
}

// lookalikes are the characters that survive a copy out of rendered Markdown
// or a word processor and then fail to match source (§7's own list).
var lookalikes = map[rune]rune{
	'\u00a0': ' ', // no-break space
	'\u2007': ' ', // figure space
	'\u202f': ' ', // narrow no-break space
	'\u2018': '\'',
	'\u2019': '\'',
	'\u201c': '"',
	'\u201d': '"',
	'\u2013': '-', // en dash
	'\u2014': '-', // em dash
	'\u2212': '-', // minus sign
}

// Every entry in the table is U+00A0 or above, so a lookalike always encodes
// with its high bit set and no ASCII byte can be one. Source is overwhelmingly
// ASCII, and this runs once per file line in anchorKey and once per candidate
// span in explain, so the scan that proves there is nothing to fold is worth
// more than the decode and rebuild it saves. It was 47% of explain and the
// whole of the map lookup that was 17% of BenchmarkDiagnose.
//
// On that path it returns b itself rather than a copy. Nothing writes through
// the result: anchorKey passes it to collapseSpace, which allocates, and the
// normalizations only compare. An oldForm shares one copy of each normalized
// form across every span comparison, so a caller that did write through it
// would corrupt every later comparison rather than just its own.
func foldLookalikes(b []byte) []byte {
	if isASCII(b) {
		return b
	}
	var out bytes.Buffer
	for _, r := range string(b) {
		if to, ok := lookalikes[r]; ok {
			out.WriteRune(to)
			continue
		}
		out.WriteRune(r)
	}
	return out.Bytes()
}

func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func detailTrailing(oldLines, span [][]byte, _ []byte) string {
	for i := range oldLines {
		if i >= len(span) || bytes.Equal(oldLines[i], span[i]) {
			continue
		}
		return fmt.Sprintf("on line %d your old %s; the file's %s",
			i+1, describeTrailing(oldLines[i]), describeTrailing(span[i]))
	}
	return ""
}

func describeTrailing(line []byte) string {
	tail := line[len(bytes.TrimRight(line, " \t")):]
	tabs := bytes.Count(tail, []byte("\t"))
	spaces := len(tail) - tabs
	switch {
	case len(tail) == 0:
		return "has none"
	case tabs > 0 && spaces > 0:
		return fmt.Sprintf("ends in %s and %s", plural(tabs, "tab"), plural(spaces, "space"))
	case tabs > 0:
		return "ends in " + plural(tabs, "tab")
	default:
		return "ends in " + plural(spaces, "space")
	}
}

// detailIndent says whether the difference is a uniform shift, because the fix
// is different. Row 2 equates any two blocks whose lines differ only in leading
// whitespace, uniform or not. Until 2026-09-21 this sentence described the
// first line that differed as though it described old, so an agent that moved
// its whole old by that amount fixed a uniform shift and met a second refusal
// on anything else.
//
// One non-blank line is its own description. From two up, the sentence either
// says every line differs the same way, or names the line it describes.
func detailIndent(oldLines, span [][]byte, _ []byte) string {
	first := -1
	for i := range oldLines {
		if i < len(span) && !bytes.Equal(oldLines[i], span[i]) {
			first = i
			break
		}
	}
	if first < 0 {
		return ""
	}
	yours, theirs := describeIndent(oldLines[first]), describeIndent(span[first])
	if nonBlank(oldLines) < 2 {
		return fmt.Sprintf("your old used %s; the file uses %s", yours, theirs)
	}
	if a, b, ok := uniformShift(oldLines, span); ok {
		switch {
		case len(b) == 0:
			return fmt.Sprintf("every line of your old is indented %s more than the file", describeIndent(a))
		case len(a) == 0:
			return fmt.Sprintf("every line of your old is indented %s less than the file", describeIndent(b))
		}
		return fmt.Sprintf("every line of your old has %s where the file has %s", describeIndent(a), describeIndent(b))
	}
	if width, oldSpaces, ok := tabConversion(oldLines, span); ok {
		styles := "tabs and the file with spaces"
		if oldSpaces {
			styles = "spaces and the file with tabs"
		}
		return fmt.Sprintf("your old indents with %s, %s to a tab", styles, plural(width, "space"))
	}
	rest := "no other line differs"
	for i := first + 1; i < len(oldLines) && i < len(span); i++ {
		if !bytes.Equal(oldLines[i], span[i]) {
			rest = "the other lines do not all differ the same way"
			break
		}
	}
	return fmt.Sprintf("line %d of your old used %s; the file uses %s, and %s", first+1, yours, theirs, rest)
}

// uniformShift finds the one swap that turns every non-blank line of old into
// the file's line, if there is one: a prefix of old's indentation traded for a
// prefix of the file's. Each line's swap is its two indentations less the
// longest suffix they share, because the swap happens at the start of the line
// and whatever follows it is common to both. Blank lines are excepted, since a
// blank line has no depth. A swap of nothing for nothing is no shift at all.
func uniformShift(oldLines, span [][]byte) (a, b []byte, ok bool) {
	seen := false
	for i, o := range oldLines {
		if i >= len(span) {
			return nil, nil, false
		}
		if isBlank(o) {
			continue
		}
		sa, sb := swapOf(o, span[i])
		if !seen {
			a, b, seen = sa, sb, true
			continue
		}
		if !bytes.Equal(sa, a) || !bytes.Equal(sb, b) {
			return nil, nil, false
		}
	}
	return a, b, seen && (len(a) > 0 || len(b) > 0)
}

// swapOf is what one line of old trades for the file's line at its start: the
// two indentations less the longest suffix they share, since whatever follows
// the swap, alignment included, is common to both.
func swapOf(yours, theirs []byte) (a, b []byte) {
	yours, theirs = indentOf(yours), indentOf(theirs)
	n := 0
	for n < len(yours) && n < len(theirs) && yours[len(yours)-1-n] == theirs[len(theirs)-1-n] {
		n++
	}
	return yours[:len(yours)-n], theirs[:len(theirs)-n]
}

// tabConversion finds the other systematic difference, added 2026-09-22: one
// side indents with spaces and the other with tabs, at the same number of
// spaces to a tab on every line that differs. uniformShift cannot see it,
// because a block two levels deep trades 4 spaces for a tab on one line and 8
// for 2 on the next. Reported as one shift, it read as though the block's
// nesting had changed. Lines whose indentation is the same on both sides are
// consistent with any width. At least two lines must differ, so that a single
// differing line is still named rather than summarized.
func tabConversion(oldLines, span [][]byte) (width int, oldSpaces, ok bool) {
	differing := 0
	for i, o := range oldLines {
		if i >= len(span) {
			return 0, false, false
		}
		if isBlank(o) {
			continue
		}
		a, b := swapOf(o, span[i])
		if len(a) == 0 && len(b) == 0 {
			continue
		}
		spaces, tabs, thisOldSpaces := a, b, true
		if !onlyOf(a, ' ') || !onlyOf(b, '\t') {
			spaces, tabs, thisOldSpaces = b, a, false
			if !onlyOf(a, '\t') || !onlyOf(b, ' ') {
				return 0, false, false
			}
		}
		if len(spaces)%len(tabs) != 0 {
			return 0, false, false
		}
		w := len(spaces) / len(tabs)
		if differing > 0 && (w != width || thisOldSpaces != oldSpaces) {
			return 0, false, false
		}
		width, oldSpaces = w, thisOldSpaces
		differing++
	}
	return width, oldSpaces, differing >= 2
}

// onlyOf reports whether b is non-empty and every byte of it is c.
func onlyOf(b []byte, c byte) bool {
	return len(b) > 0 && len(bytes.Trim(b, string(c))) == 0
}

func indentOf(line []byte) []byte {
	return line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
}

func isBlank(line []byte) bool { return len(bytes.TrimLeft(line, " \t")) == 0 }

func nonBlank(lines [][]byte) int {
	n := 0
	for _, l := range lines {
		if !isBlank(l) {
			n++
		}
	}
	return n
}

func describeIndent(line []byte) string {
	lead := indentOf(line)
	tabs := bytes.Count(lead, []byte("\t"))
	spaces := bytes.Count(lead, []byte(" "))
	switch {
	case tabs > 0 && spaces > 0:
		return fmt.Sprintf("%s and %s", plural(tabs, "tab"), plural(spaces, "space"))
	case tabs > 0:
		return plural(tabs, "tab")
	case spaces > 0:
		return plural(spaces, "space")
	}
	return "no indentation"
}

// detailEOL distinguishes the three ways this row fires, by the file. Mixed: the
// span is in the minority ending. CRLF: only under --eol strict, with a payload
// that is not, and the useful sentence is that --eol auto translates that. LF:
// the payload carries CRs the file does not, which is a CRLF patch, under
// --eol strict or with endings mixed enough that auto keeps its bytes.
//
// That last row said "--eol auto translates that for you" until 2026-10-06,
// under --eol auto, which had not. Writing the patch with LF endings is the
// remedy that is true in both modes.
func detailEOL(_, _ [][]byte, file []byte) string {
	nCRLF := bytes.Count(file, crlf)
	nLF := bytes.Count(file, lf) - nCRLF
	if nCRLF > 0 && nLF > 0 {
		return fmt.Sprintf("the file has mixed line endings, %d CRLF and %d LF", nCRLF, nLF)
	}
	if nCRLF > 0 {
		return "the file is CRLF; --eol auto translates that for you"
	}
	return "the patch's lines end in CRLF and the file's in LF; write the patch with LF line endings"
}

// detailUnicode names the first rune where the two actually diverge, rather
// than the first lookalike anywhere. Which side carries it matters: a curly
// quote in the file came out of the source, and one in the payload came out of
// something that rendered the source.
func detailUnicode(oldLines, span [][]byte, _ []byte) string {
	yours := []rune(string(bytes.Join(oldLines, lf)))
	theirs := []rune(string(bytes.Join(span, lf)))
	for i := 0; i < len(yours) && i < len(theirs); i++ {
		if yours[i] == theirs[i] {
			continue
		}
		if _, ok := lookalikes[theirs[i]]; ok {
			return fmt.Sprintf("the file has %s (U+%04X) at offset %d, where your old has %q",
				runeName(theirs[i]), theirs[i], i, yours[i])
		}
		if _, ok := lookalikes[yours[i]]; ok {
			return fmt.Sprintf("your old has %s (U+%04X) at offset %d, where the file has %q",
				runeName(yours[i]), yours[i], i, theirs[i])
		}
		return ""
	}
	return ""
}

func runeName(r rune) string {
	switch r {
	case '\u00a0':
		return "a no-break space"
	case '\u2007':
		return "a figure space"
	case '\u202f':
		return "a narrow no-break space"
	case '\u2018', '\u2019':
		return "a curly quote"
	case '\u201c', '\u201d':
		return "a curly double quote"
	case '\u2013':
		return "an en dash"
	case '\u2014':
		return "an em dash"
	case '\u2212':
		return "a minus sign"
	}
	return "a lookalike character"
}

// differingLines counts lines that differ, plus lines the span is missing
// because it ran off the end of the file. spanAt never returns more lines than
// old has, so the difference is never negative.
func differingLines(oldLines, span [][]byte) int {
	n := len(oldLines) - len(span)
	for i := range oldLines {
		if i >= len(span) || !bytes.Equal(oldLines[i], span[i]) {
			n++
		}
	}
	return n
}

// firstDifference is §7.1 step 4's report: the first line of old that differs,
// both versions, and the column of the first differing byte.
func firstDifference(oldLines, span [][]byte) (line int, yours, theirs []byte, col int) {
	for i := range oldLines {
		var s []byte
		if i < len(span) {
			s = span[i]
		}
		if bytes.Equal(oldLines[i], s) {
			continue
		}
		j := 0
		for j < len(oldLines[i]) && j < len(s) && oldLines[i][j] == s[j] {
			j++
		}
		return i + 1, oldLines[i], s, j + 1
	}
	return 0, nil, nil, 0
}

// plural reads "a tab" rather than "1 tab" for one, which is §5.2's wording:
// "your `old` used a tab; the file uses 4 spaces".
// renderSpan prints the span in the gutter and, when --context cut it, says so
// and what shows the rest: a cut span pasted as old is a different edit.
func (d *Diagnosis) renderSpan(b *strings.Builder, indent string, gutter func(int, []byte)) {
	for i, l := range d.Span {
		gutter(d.Line+i, l)
	}
	if d.SpanLines > 0 {
		hidden := d.SpanLines - len(d.Span)
		s := "s"
		if hidden == 1 {
			s = ""
		}
		fmt.Fprintf(b, "%s    ... %d more line%s not shown (--context %d): old needs all %d, so rerun with --context %d before pasting\n",
			indent, hidden, s, len(d.Span), d.SpanLines, d.SpanLines)
	}
}

// sameBytes says how many of the other spans hold the span's own bytes.
func sameBytes(same, of int) string {
	switch {
	case of == 1:
		return "it has"
	case same == of:
		return "all of them have"
	case same == 1:
		return "one of them has"
	}
	return fmt.Sprintf("%d of them have", same)
}

// alsoAt names where the other spans as near start, as too-many lists its
// matches.
func (d *Diagnosis) alsoAt() string {
	at := make([]string, len(d.Also))
	for i, l := range d.Also {
		at[i] = fmt.Sprint(l)
	}
	where := "line " + at[0]
	if len(at) > 1 || d.AlsoMore > 0 {
		where = "lines " + strings.Join(at, ", ")
	}
	if d.AlsoMore > 0 {
		where += fmt.Sprintf(", and %d more", d.AlsoMore)
	}
	return where
}

func plural(n int, word string) string {
	if n == 1 {
		article := "a "
		if strings.ContainsRune("aeiou", rune(word[0])) {
			article = "an "
		}
		return article + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// shown is a line from the tree as the text report prints it. With mark, which
// is for the causes where the difference is whitespace and the reader would
// otherwise see two identical lines (§7.1 step 5), a tab is shown as → and a
// trailing space as ·.
//
// Either way, a control character is shown by name, <U+001B>, as a path in a
// refusal has been since 2026-10-06: the line came from a file, and printed raw
// an escape clears the reader's screen and a carriage return overwrites what
// came before it, so the report shows a line that is not there. With mark, a
// literal → or · is named the same way, because until 2026-10-07 a line
// holding one beside a tab or a trailing space read "a→b→c··", four characters
// with two meanings each. The bytes themselves are the JSON span's.
func shown(line []byte, mark bool) string {
	// One trailing CR is a CRLF file's line ending, as it is in a patch, and
	// is printed as it always was: it is harmless before the newline, and named
	// it would be on every line of every report about a CRLF file.
	s, ending := strings.CutSuffix(string(line), "\r")
	end := len(s)
	if mark {
		end = len(strings.TrimRight(s, " \t"))
	}
	var b strings.Builder
	for i := 0; i < end; {
		if name, w, ok := controlAt(s, i); ok {
			b.WriteString("<" + name + ">")
			i += w
			continue
		}
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case mark && r == '\t':
			b.WriteString("→")
		case mark && (r == '→' || r == '·'):
			b.WriteString("<" + codePoint(r) + ">")
		default:
			b.WriteString(s[i : i+w])
		}
		i += w
	}
	for _, c := range s[end:] {
		if c == '\t' {
			b.WriteString("→")
		} else {
			b.WriteString("·")
		}
	}
	if ending {
		b.WriteString("\r")
	}
	return b.String()
}

// differsOnlyInWhitespace reports whether two lines are the same once every
// whitespace run is collapsed, which is when rendering whitespace visibly is
// the difference between a useful report and two identical-looking lines.
func differsOnlyInWhitespace(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return false
	}
	return bytes.Equal(collapseSpace(a), collapseSpace(b))
}

func whitespaceCause(cause string) bool {
	switch cause {
	case "trailing whitespace", "indentation", "whitespace", "line endings":
		return true
	}
	return false
}

// causeName is the JSON "cause" of §5.2. DiagNamed carries the normalization
// that explained the miss; the two kinds that are their own explanation carry a
// name for the shape instead, because an empty cause tells a reader nothing.
func (d *Diagnosis) causeName() string {
	switch d.Kind {
	case DiagNoAnchor:
		return "no anchor"
	case DiagNoRoom:
		return "no room"
	}
	return d.Cause
}

// Render writes the near-miss block of §5.2's report, indented under its hunk
// heading. report.go composes the rest around it.
func (d *Diagnosis) Render(indent string) string {
	var b strings.Builder
	// §7.1 step 5 shows tabs "where the difference is whitespace", and §5.2's
	// fallback example prints literal tabs, because there the difference is a
	// typo. Both hold if visibility follows the difference rather than the kind.
	makeVisible := whitespaceCause(d.Cause) ||
		(d.Kind == DiagClosest && differsOnlyInWhitespace(d.Yours, d.Theirs))
	show := func(line []byte) string { return shown(line, makeVisible) }
	gutter := func(n int, line []byte) {
		fmt.Fprintf(&b, "%s%5d | %s\n", indent, n, show(line))
	}

	switch d.Kind {
	case DiagNoAnchor:
		fmt.Fprintf(&b, "%sno anchor found: not one line of your old appears in the file,\n", indent)
		fmt.Fprintf(&b, "%seven ignoring whitespace. The text is not there.\n", indent)
		return b.String()

	case DiagTooMany:
		if len(d.Lines) > 0 {
			var at []string
			for _, l := range d.Lines {
				at = append(at, fmt.Sprint(l))
			}
			fmt.Fprintf(&b, "%sat lines %s", indent, strings.Join(at, ", "))
			if d.MoreLines > 0 {
				fmt.Fprintf(&b, ", and %d more", d.MoreLines)
			}
			b.WriteString("\n")
		}
		// §7.1's closing line and §5.2's example both end here, and the
		// suggestion is the actionable half.
		fmt.Fprintf(&b, "\n%sadd surrounding context to old, or say \"@@ old x%d\"\n",
			indent, d.Found)
		return b.String()

	case DiagOverlap, DiagMixedEndings:
		// No "or say @@ old xN" here. Some of the copies are ones this hunk
		// cannot edit at all, so every N either guesses or cannot apply, and
		// until 2026-10-06 the suggestion is what made the guess look
		// sanctioned.
		at := make([]string, len(d.Lines))
		for i, l := range d.Lines {
			at[i] = fmt.Sprint(l)
			if i < len(d.Endings) {
				at[i] += " (" + d.Endings[i] + ")"
			}
		}
		fmt.Fprintf(&b, "%sat lines %s", indent, strings.Join(at, ", "))
		if d.MoreLines > 0 {
			fmt.Fprintf(&b, ", and %d more", d.MoreLines)
		}
		fmt.Fprintf(&b, "\n\n%s%s\n", indent, d.Detail)
		return b.String()

	case DiagNoRoom:
		fmt.Fprintf(&b, "%sline %d of your old is at line %d, and the %s above it\n",
			indent, d.DiffLine, d.Line, plural(d.DiffLine-1, "line"))
		fmt.Fprintf(&b, "%sin your old would fall above the top of the file:\n", indent)
		for i, l := range d.Span {
			gutter(d.Line+i, l)
		}
		fmt.Fprintf(&b, "%sdrop them from your old, or anchor it further down.\n", indent)
		return b.String()

	case DiagNamed:
		fmt.Fprintf(&b, "%s%s, at line %d:\n", indent, d.Headline, d.Line)
		d.renderSpan(&b, indent, gutter)
		if d.Detail != "" {
			fmt.Fprintf(&b, "%s%s\n", indent, d.Detail)
		}
		switch {
		case len(d.Also) > 0 && d.AlsoSame > 0:
			fmt.Fprintf(&b, "%salso nearly there at %s; the span above is line %d's, and %s the same bytes, so pasted alone it matches more than once: add context\n",
				indent, d.alsoAt(), d.Line, sameBytes(d.AlsoSame, len(d.Also)+d.AlsoMore))
		case len(d.Also) > 0:
			fmt.Fprintf(&b, "%salso nearly there at %s; the span above is line %d's, and pasted it edits that copy only\n",
				indent, d.alsoAt(), d.Line)
		}

	case DiagClosest:
		fmt.Fprintf(&b, "%sclosest span starts at line %d; line %d of your old differs at column %d:\n",
			indent, d.Line, d.DiffLine, d.Column)
		fmt.Fprintf(&b, "%s  yours | %s\n", indent, show(d.Yours))
		fmt.Fprintf(&b, "%s  file  | %s\n", indent, show(d.Theirs))
		d.renderSpan(&b, indent, gutter)
		if len(d.Also) > 0 {
			fmt.Fprintf(&b, "%sas close at %s; the span above is line %d's\n", indent, d.alsoAt(), d.Line)
		}
	}

	if d.Shifted > 0 {
		which := "the hunk before this one"
		if d.Shifted > 1 {
			which = fmt.Sprintf("the %d hunks before this one", d.Shifted)
		}
		fmt.Fprintf(&b, "%sline numbers count against the file as %s would have left it,\n", indent, which)
		fmt.Fprintf(&b, "%snot as it is on disk: nothing was written.\n", indent)
	}
	return b.String()
}
