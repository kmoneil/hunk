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
	"strings"
)

// DiagKind is which of §7's three shapes a diagnosis is.
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
	// DiagTooMany: old occurs, just not the claimed number of times.
	DiagTooMany
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

	// DiffLine, Yours, Theirs and Column describe the first line that differs
	// (DiagClosest only). DiffLine is 1-based within old.
	DiffLine int
	Yours    []byte
	Theirs   []byte
	Column   int

	// Lines are where each match starts (DiagTooMany only), capped.
	Lines []int
	// MoreLines is how many matches were not listed.
	MoreLines int
	// Found is the total number of matches (DiagTooMany only), which the
	// suggested "@@ old x N" needs.
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

	anchorAt, starts := anchors(oldLines, fileLines)
	if len(starts) == 0 {
		return &Diagnosis{Kind: DiagNoAnchor}
	}

	// §7.1 step 2, corrected: the span starts where old would start, which is
	// anchorAt lines above the matched line. Identical when the anchor is old's
	// first line, and wrong otherwise, which is the case step 1 exists for.
	for _, at := range starts {
		start := at - anchorAt
		if start < 0 {
			continue
		}
		span := spanAt(fileLines, start, len(oldLines))
		if d := explain(oldLines, span, file); d != nil {
			d.Line = start + 1
			d.Span = capSpan(span, maxContext)
			return d
		}
	}

	// §7.1 step 4, the floor. Fewest differing lines, earliest on a tie so a
	// golden has one answer.
	best, bestStart, bestDiff := [][]byte(nil), 0, -1
	for _, at := range starts {
		start := at - anchorAt
		if start < 0 {
			continue
		}
		span := spanAt(fileLines, start, len(oldLines))
		if n := differingLines(oldLines, span); bestDiff < 0 || n < bestDiff {
			best, bestStart, bestDiff = span, start, n
		}
	}
	if best == nil {
		return &Diagnosis{Kind: DiagNoAnchor}
	}
	d := &Diagnosis{Kind: DiagClosest, Line: bestStart + 1, Span: capSpan(best, maxContext)}
	d.DiffLine, d.Yours, d.Theirs, d.Column = firstDifference(oldLines, best)
	return d
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

// anchors implements §7.1 step 1. It returns the index within old of the line
// used as the anchor, and every file line whose stripped form equals it.
func anchors(oldLines, fileLines [][]byte) (int, []int) {
	first := -1
	for i, l := range oldLines {
		if len(bytes.TrimSpace(l)) > 0 {
			first = i
			break
		}
	}
	if first < 0 {
		return 0, nil // old is entirely blank: no anchor and no fallback
	}
	if at := matchingLines(fileLines, oldLines[first]); len(at) > 0 {
		return first, at
	}
	longest := first
	for i := range oldLines {
		if len(bytes.TrimSpace(oldLines[i])) > len(bytes.TrimSpace(oldLines[longest])) {
			longest = i
		}
	}
	if longest == first {
		return first, nil
	}
	return longest, matchingLines(fileLines, oldLines[longest])
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

// matchingLines is only called with a line anchors already found non-blank, so
// the key cannot be empty and there is no guard against matching every blank
// line in the file.
func matchingLines(fileLines [][]byte, want []byte) []int {
	w := anchorKey(want)
	var at []int
	for i, l := range fileLines {
		if anchorKey(l) == w {
			at = append(at, i)
		}
	}
	return at
}

func spanAt(fileLines [][]byte, start, n int) [][]byte {
	if start+n > len(fileLines) {
		n = len(fileLines) - start
	}
	return fileLines[start : start+n]
}

func capSpan(span [][]byte, maxContext int) [][]byte {
	if maxContext > 0 && len(span) > maxContext {
		return span[:maxContext]
	}
	return span
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

// explain runs §7.1 step 3, stopping at the first normalization that makes the
// span equal to old.
func explain(oldLines, span [][]byte, file []byte) *Diagnosis {
	if len(oldLines) != len(span) {
		return nil
	}
	o := bytes.Join(oldLines, lf)
	s := bytes.Join(span, lf)
	if bytes.Equal(o, s) {
		return nil // it matches here; the miss is elsewhere
	}
	for _, n := range normalizations() {
		if bytes.Equal(n.apply(o), n.apply(s)) {
			return &Diagnosis{
				Kind:     DiagNamed,
				Cause:    n.cause,
				Headline: n.headline,
				Detail:   n.detail(oldLines, span, file),
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

// flattenIndent reduces each line's leading whitespace run to one space,
// leaving the rest of the line alone (§7.1: "leading only").
func flattenIndent(b []byte) []byte {
	lines := bytes.Split(b, lf)
	out := make([][]byte, len(lines))
	for i, l := range lines {
		n := len(l) - len(bytes.TrimLeft(l, " \t"))
		if n == 0 {
			out[i] = l
			continue
		}
		out[i] = append([]byte(" "), l[n:]...)
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

func foldLookalikes(b []byte) []byte {
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

func detailIndent(oldLines, span [][]byte, _ []byte) string {
	for i := range oldLines {
		if i >= len(span) || bytes.Equal(oldLines[i], span[i]) {
			continue
		}
		return fmt.Sprintf("your old used %s; the file uses %s",
			describeIndent(oldLines[i]), describeIndent(span[i]))
	}
	return ""
}

func describeIndent(line []byte) string {
	lead := line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
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

// detailEOL distinguishes the two ways this row fires. Under --eol strict it
// means the file is CRLF and the payload is not, and the useful sentence is
// that --eol auto translates that. Under --eol auto the payload has already
// been converted, so reaching here means the file is mixed and this span is the
// minority.
func detailEOL(_, _ [][]byte, file []byte) string {
	nCRLF := bytes.Count(file, crlf)
	nLF := bytes.Count(file, lf) - nCRLF
	if nCRLF > 0 && nLF > 0 {
		return fmt.Sprintf("the file has mixed line endings, %d CRLF and %d LF", nCRLF, nLF)
	}
	if nCRLF > 0 {
		return "the file is CRLF; --eol auto translates that for you"
	}
	return "the file is LF; --eol auto translates that for you"
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

// visible renders a line with whitespace made legible, for the causes where the
// difference is whitespace and the reader would otherwise see two identical
// lines (§7.1 step 5).
func visible(line []byte) string {
	trimmed := bytes.TrimRight(line, " \t")
	tail := line[len(trimmed):]
	var b strings.Builder
	for _, r := range string(trimmed) {
		if r == '\t' {
			b.WriteString("→")
			continue
		}
		b.WriteRune(r)
	}
	for _, r := range string(tail) {
		if r == '\t' {
			b.WriteString("→")
			continue
		}
		b.WriteString("·")
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

// Render writes the near-miss block of §5.2's report, indented under its hunk
// heading. report.go composes the rest around it.
func (d *Diagnosis) Render(indent string) string {
	var b strings.Builder
	// §7.1 step 5 shows tabs "where the difference is whitespace", and §5.2's
	// fallback example prints literal tabs, because there the difference is a
	// typo. Both hold if visibility follows the difference rather than the kind.
	makeVisible := whitespaceCause(d.Cause) ||
		(d.Kind == DiagClosest && differsOnlyInWhitespace(d.Yours, d.Theirs))
	show := func(line []byte) string {
		if makeVisible {
			return visible(line)
		}
		return string(line)
	}
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

	case DiagNamed:
		fmt.Fprintf(&b, "%s%s, at line %d:\n", indent, d.Headline, d.Line)
		for i, l := range d.Span {
			gutter(d.Line+i, l)
		}
		if d.Detail != "" {
			fmt.Fprintf(&b, "%s%s\n", indent, d.Detail)
		}

	case DiagClosest:
		fmt.Fprintf(&b, "%sclosest span starts at line %d; line %d of your old differs at column %d:\n",
			indent, d.Line, d.DiffLine, d.Column)
		fmt.Fprintf(&b, "%s  yours | %s\n", indent, show(d.Yours))
		fmt.Fprintf(&b, "%s  file  | %s\n", indent, show(d.Theirs))
		for i, l := range d.Span {
			gutter(d.Line+i, l)
		}
	}

	if d.Shifted > 0 {
		which := "the hunk before this one"
		if d.Shifted > 1 {
			which = fmt.Sprintf("the %d hunks before this one", d.Shifted)
		}
		fmt.Fprintf(&b, "%sline numbers count against the file as %s left it,\n", indent, which)
		fmt.Fprintf(&b, "%snot as it is on disk: nothing was written.\n", indent)
	}
	return b.String()
}
