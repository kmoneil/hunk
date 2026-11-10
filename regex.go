package main

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// regex.go is @@ re (issue #161): a replacement whose old is a regular
// expression, held to the same count as @@ old. When one change has to land
// on texts that differ, forty call sites each with its own argument, an agent
// wrote an @@ old for each or fell back to sed -i, which exits 0 whether it
// matched nothing or twice the places meant; 1,613 of 3,498 sed -i edits in
// 402 sessions used regex features.
//
//	@@ re x3
//	\boldName\((\w+)\)
//	@@ new
//	newName($1, ctx)
//
// The pattern is Go's regexp, RE2, so it runs in time linear in the file and
// no pattern can hang. Its matches are FindAll's, leftmost first and never
// overlapping, and their number has to be exactly the count or the batch is
// refused. A match that a pattern could also have started inside of is not
// refused, as copies of @@ old that overlap are: decided on the issue, since
// that would refuse nearly every greedy pattern (foo.* matches again inside
// its own match), and leftmost-first is the only match a replacement takes.
// Each match is replaced by @@ new expanded as Regexp.Expand expands it: $1
// and ${1} a group by number, $name and ${name} one by name, $$ a $.
//
// The payloads follow every rule @@ old's do. The pattern never gains a
// trailing newline, so a blank line before @@ new puts a literal line break
// at its end. Under --eol auto a literal line break in the pattern, and in
// @@ new, stands for the file's line ending, as in a payload; \n written as
// an escape is the regexp's and means a newline alone.

// parseRegex reads the @@ re hunk whose directive line, ln, is the line before
// lines[i], and returns it and the index of the line after it.
func (p *Patch) parseRegex(lines [][]byte, i, ln int, cur, arg, marker string) (Hunk, int, error) {
	dir := marker + " re"
	if cur == "" {
		return Hunk{}, 0, &ParseError{ln, fmt.Sprintf(
			"no file is in effect; put %q before this", marker+" file <path>")}
	}
	claimed, err := parseCount(arg, ln, marker, "re")
	if err != nil {
		return Hunk{}, 0, err
	}
	patStart := i
	pattern, next := collectPayload(lines, i, marker)
	patEnd := next
	i = next
	if i >= len(lines) {
		return Hunk{}, 0, &ParseError{ln, fmt.Sprintf(
			"%q has no %q; every pattern needs the text to put in place of each match, even if that text is empty. If the line is payload text, %s",
			dir, marker+" new", markerWayOut)}
	}
	w2, a2, _ := directive(lines[i], marker)
	// An empty pattern matches at every position, as an empty old would.
	if len(pattern) == 0 || (p.CRLF && len(stripCR(pattern)) == 0) {
		if w2 != "new" {
			stop := lines[i]
			return Hunk{}, 0, namingLine(ln, i+1, func(at int) string {
				return fmt.Sprintf(
					"%q has an empty pattern, ended by %s at line %d, a directive; if that line is payload text, %s",
					dir, shownLine(stop), at, markerWayOut)
			})
		}
		return Hunk{}, 0, &ParseError{ln, fmt.Sprintf(
			"%q has an empty pattern, which would match at every position in the file rather than at one", dir)}
	}
	if w2 != "new" {
		return Hunk{}, 0, namingLine(i+1, ln, func(at int) string {
			return fmt.Sprintf(
				"expected %q to close the %q at line %d, found %q; if the line is payload text, %s",
				marker+" new", dir, at, marker+" "+w2, markerWayOut)
		})
	}
	if a2 != "" {
		return Hunk{}, 0, &ParseError{i + 1, fmt.Sprintf(
			"%q takes no argument; the occurrence count goes on %q", marker+" new", dir)}
	}
	re, err := regexp.Compile(string(pattern))
	if err != nil {
		return Hunk{}, 0, &ParseError{ln, fmt.Sprintf(
			"the pattern of this %q is not one Go's regexp reads: %v", dir, err)}
	}
	newLine := i + 1
	i++
	tmpl, next := collectPayload(lines, i, marker)
	if err := p.refuseUnknownWords(lines, i, next, cur, lines[patStart:patEnd], marker); err != nil {
		return Hunk{}, 0, err
	}
	p.noteStrays(lines, i, next, cur, lines[patStart:patEnd])
	if ref, at, ok := missingGroup(re, tmpl); ok {
		return Hunk{}, 0, &ParseError{newLine + 1 + bytes.Count(tmpl[:at], lf), fmt.Sprintf(
			"%s in the %q of the %q at line %d names no group of its pattern, which has %s, so each match would put nothing there; write $$ for a $ that is text, and ${1}x for group 1 followed by x",
			ref, marker+" new", dir, ln, count(re.NumSubexp(), "group"))}
	}
	return Hunk{Op: OpRegex, Path: cur, Line: ln, Count: claimed, Old: pattern, New: tmpl, Re: re}, next, nil
}

// missingGroup is the first reference in tmpl that names no group of re, as
// written, and where it starts. Regexp.Expand replaces such a reference with
// nothing, so "$HOME" in a shell script, or "$1x" meant as group 1 and an x,
// which Expand reads as a group named 1x, would vanish from every match.
//
// References are read as Expand reads them: $$ is a $; $ then a name of
// letters, digits and _, or the same in braces, is a reference, a name all
// digits without a leading zero being a group's number; a $ that starts
// neither is text.
func missingGroup(re *regexp.Regexp, tmpl []byte) (string, int, bool) {
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '$' {
			continue
		}
		if i+1 < len(tmpl) && tmpl[i+1] == '$' {
			i++
			continue
		}
		name, n := refName(tmpl[i+1:])
		if n == 0 {
			continue
		}
		if !hasGroup(re, name) {
			return string(tmpl[i : i+1+n]), i, true
		}
		i += n
	}
	return "", 0, false
}

// refName reads the name after a $ as Regexp.Expand does, and how many bytes
// it took, 0 for none.
func refName(b []byte) (string, int) {
	brace := len(b) > 0 && b[0] == '{'
	s := b
	if brace {
		s = b[1:]
	}
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRune(s[i:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			break
		}
		i += size
	}
	if i == 0 {
		return "", 0
	}
	if !brace {
		return string(s[:i]), i
	}
	if i >= len(s) || s[i] != '}' {
		return "", 0
	}
	return string(s[:i]), i + 2
}

// hasGroup reports whether name is one of re's groups.
func hasGroup(re *regexp.Regexp, name string) bool { return groupIndex(re, name) >= 0 }

// groupIndex is the group name refers to, as Regexp.Expand reads it: a
// number from 0 to re's count of groups, without a leading zero, or the
// first group of that name; -1 for none.
func groupIndex(re *regexp.Regexp, name string) int {
	num := 0
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' || num > re.NumSubexp() {
			num = -1
			break
		}
		num = num*10 + int(name[i]-'0')
	}
	if num >= 0 && !(name[0] == '0' && len(name) > 1) {
		if num <= re.NumSubexp() {
			return num
		}
		return -1
	}
	return re.SubexpIndex(name)
}

// A segment is a piece of one match's replacement: text of the template, or
// a group's text, copied from from in the file. A join of a carriage return
// to a newline is a boundary between two pieces whose bytes did not stand
// together in the file, so a replacement has to say where each of its
// pieces came from.
type segment struct {
	b    []byte
	from int // the group's offset in the file, -1 for the template's text
}

// expand is Regexp.Expand(tmpl) for match m of src, as segments. Every
// reference in tmpl names a group of re, which the parser has seen to
// (missingGroup); a group that took part in no match puts nothing.
// FuzzRegexHunk holds its result to regexp.ReplaceAll's.
func expand(re *regexp.Regexp, tmpl, src []byte, m []int) []segment {
	var segs []segment
	text := func(b []byte) {
		if len(b) > 0 {
			segs = append(segs, segment{b, -1})
		}
	}
	for len(tmpl) > 0 {
		i := bytes.IndexByte(tmpl, '$')
		if i < 0 {
			text(tmpl)
			break
		}
		text(tmpl[:i])
		tmpl = tmpl[i+1:]
		if len(tmpl) > 0 && tmpl[0] == '$' {
			text(tmpl[:1])
			tmpl = tmpl[1:]
			continue
		}
		name, n := refName(tmpl)
		if n == 0 {
			text([]byte("$"))
			continue
		}
		tmpl = tmpl[n:]
		// A group that took part in no match is -1 at both ends.
		if g := groupIndex(re, name); g >= 0 && m[2*g+1] > m[2*g] {
			segs = append(segs, segment{src[m[2*g]:m[2*g+1]], m[2*g]})
		}
	}
	return segs
}

// A change is one place a replacement rewrites: cur[at:end] becomes repl,
// made of segs for an @@ re match and nil for a copy of an @@ old.
type change struct {
	at, end int
	repl    []byte
	segs    []segment
}

// applyChanges is cur with every change made, the changes in order and not
// overlapping.
func applyChanges(cur []byte, changes []change) []byte {
	var b bytes.Buffer
	last := 0
	for _, c := range changes {
		b.Write(cur[last:c.at])
		b.Write(c.repl)
		last = c.end
	}
	b.Write(cur[last:])
	return b.Bytes()
}

// validateRegex is Validate's walk for @@ re hunk n against f, the file as
// the hunks before it left it, and the failure to report, or nil once it has
// applied.
func (x *Txn) validateRegex(n int, h Hunk, f *file, diagnosed *int) *Failure {
	fail := &Failure{Hunk: n, Path: h.Path, PatchLine: h.Line, Expected: h.Count, Matches: true}
	re := h.Re
	pat := x.convert(h.Old, f.eol)
	if !bytes.Equal(pat, h.Old) {
		var err error
		if re, err = regexp.Compile(string(pat)); err != nil {
			fail.Refusal = fmt.Sprintf(
				"its pattern, with its line breaks made the file's for --eol auto, is not one Go's regexp reads: %v; write the line breaks as \\r?\\n, or pass --eol strict", err)
			return fail
		}
	}
	locs := re.FindAllSubmatchIndex(f.cur, -1)
	fail.Found = len(locs)
	// Under --eol auto a literal line break in the pattern is the file's
	// dominant ending, so in a file with both it matches only half of them,
	// and a count right about what it saw has still missed some.
	if x.opt.EOL != EOLStrict && bytes.Contains(pat, lf) && mixedEndings(f.cur) {
		other := "\r\n"
		if f.eol == "\r\n" {
			other = "\n"
		}
		if alt, err := regexp.Compile(string(x.convert(h.Old, other))); err == nil {
			if missed := alt.FindAllIndex(f.cur, -1); len(missed) > 0 {
				starts := make([]int, 0, min(len(missed), tooManyLimit))
				for _, m := range missed[:min(len(missed), tooManyLimit)] {
					starts = append(starts, m[0])
				}
				fail.Found = len(locs) + len(missed)
				fail.Near = &Diagnosis{
					Kind: DiagMixedEndings, Cause: "mixed line endings",
					Detail: "the file has mixed line endings, and under --eol auto a line break in the pattern " +
						"matches only its dominant one, so these matches were missed; write the line break as \\r?\\n, which matches either",
					Found: len(missed), Lines: lineNumbers(f.cur, starts), MoreLines: len(missed) - len(starts),
					Shifted: f.applied,
				}
				return fail
			}
		}
	}
	if len(locs) != h.Count {
		if *diagnosed < diagnoseLimit {
			*diagnosed++
			fail.Near = diagnoseMatches(f.cur, locs)
			fail.Near.Shifted = f.applied
		}
		return fail
	}
	tmpl := x.convert(h.New, f.eol)
	changes := make([]change, len(locs))
	for i, m := range locs {
		segs := expand(re, tmpl, f.cur, m)
		var repl []byte
		for _, s := range segs {
			repl = append(repl, s.b...)
		}
		changes[i] = change{at: m[0], end: m[1], repl: repl, segs: segs}
	}
	if x.opt.EOL != EOLStrict {
		if d := diagnoseRegexCRLF(f.cur, changes); d != nil {
			d.Shifted = f.applied
			fail.Near = d
			return fail
		}
	}
	next := applyChanges(f.cur, changes)
	if bytes.Equal(next, f.cur) {
		x.slips = append(x.slips, noted{Slip{
			Kind: SlipNoChange, Hunk: n, Line: h.Line, Path: h.Path, Count: h.Count, Of: h.Count,
			Text: lineAt(f.cur, changes[0].at),
		}, f})
		return nil
	}
	f.changed = true
	f.applied++
	a, r := changeCounts(f.cur, changes)
	f.added += a
	f.removed += r
	x.noteChangeSlips(n, h, f, changes, next)
	if x.inverseTo != "" {
		// Each match is an edit of its own, a literal replacement of one
		// copy, at its place in the file as the matches before it left it,
		// which the inverse undoes last first like any other (inverse.go).
		d := 0
		for _, c := range changes {
			one := Hunk{Op: OpReplace, Path: h.Path, Line: h.Line, Count: 1, Old: f.cur[c.at:c.end], New: c.repl}
			x.edits = append(x.edits, edit{n: n, h: one, f: f, old: one.Old, repl: c.repl, at: []int{c.at + d}})
			d += len(c.repl) - (c.end - c.at)
		}
	}
	f.cur = next
	return nil
}

// matchLine is one match as a refusal shows it: the line it starts on, and
// where on that line it starts and stops, its first line's part of it if it
// runs on.
type matchLine struct {
	Line     int
	Text     []byte
	From, To int
	RunsOn   bool
}

// diagnoseMatches is the refusal of a pattern that matched other than n
// times: each match with its line, so the agent sees what the pattern takes
// before changing it or the count, as the issue asked.
func diagnoseMatches(file []byte, locs [][]int) *Diagnosis {
	d := &Diagnosis{Kind: DiagMatches, Found: len(locs)}
	for _, m := range locs[:min(len(locs), tooManyLimit)] {
		start := bytes.LastIndexByte(file[:m[0]], '\n') + 1
		end := len(file)
		if j := bytes.IndexByte(file[m[0]:], '\n'); j >= 0 {
			end = m[0] + j
		}
		text := bytes.TrimSuffix(file[start:end], cr)
		to := min(m[1], start+len(text)) - start
		d.Matched = append(d.Matched, matchLine{
			Line: bytes.Count(file[:m[0]], lf) + 1, Text: text,
			From: min(m[0]-start, len(text)), To: max(to, min(m[0]-start, len(text))),
			RunsOn: m[1] > end,
		})
		d.Lines = append(d.Lines, d.Matched[len(d.Matched)-1].Line)
	}
	d.MoreLines = len(locs) - len(d.Matched)
	return d
}

// renderMatches writes diagnoseMatches' block: each match's line in the
// gutter with carets under what it matched, one for a match of nothing.
func (d *Diagnosis) renderMatches(b *strings.Builder, indent string) {
	if d.Found == 0 {
		fmt.Fprintf(b, "%sthe pattern matches nowhere in the file\n", indent)
		return
	}
	for _, m := range d.Matched {
		fmt.Fprintf(b, "%s%5d | %s\n", indent, m.Line, shown(m.Text, false))
		lead := []rune(shown(m.Text[:m.From], false))
		for i, r := range lead {
			if r != '\t' {
				lead[i] = ' '
			}
		}
		width := max(1, utf8.RuneCountInString(shown(m.Text[m.From:m.To], false)))
		more := ""
		if m.RunsOn {
			more = " and on"
		}
		fmt.Fprintf(b, "%s      | %s%s%s\n", indent, string(lead), strings.Repeat("^", width), more)
	}
	if d.MoreLines > 0 {
		fmt.Fprintf(b, "%sand %d more\n", indent, d.MoreLines)
	}
	fmt.Fprintf(b, "\n%schange the pattern, or say \"@@ re x%d\"\n", indent, d.Found)
}

// diagnoseRegexCRLF is DiagnoseSplitCRLF and DiagnoseJoinedCRLF for an @@ re
// hunk's changes, under --eol auto: the changes that would part a CRLF's two
// bytes, or put a carriage return just before a newline that did not follow
// it, or nil when none would. A pattern meets a CRLF more readily than an old
// does: "." matches a carriage return, so foo=.* takes the \r of a CRLF line,
// and replacing it would leave the line's newline alone.
//
// A split is a match that begins on the newline of a CRLF, or ends on its
// carriage return, whose replacement does not begin or end with that byte. A
// join is read off the result as pieces, the file's kept spans and each
// replacement's segments in turn: where one ends in a carriage return and the
// next that is not empty begins with a newline, the two meet, unless both are
// the file's own and stood together in it. So a group that copies a CRLF the
// file has, twice if the template says so, joins nothing, and a carriage
// return the template writes before a group that starts with a newline does.
// A CRLF the template writes whole is a line ending it typed, which --eol auto
// has made the file's.
func diagnoseRegexCRLF(cur []byte, changes []change) *Diagnosis {
	var splitAt, joinAt []int
	for _, c := range changes {
		if c.end == c.at {
			continue
		}
		r := c.repl
		switch {
		case c.at > 0 && cur[c.at-1] == '\r' && cur[c.at] == '\n' && !bytes.HasPrefix(r, lf):
			splitAt = append(splitAt, c.at-1)
		case cur[c.end-1] == '\r' && c.end < len(cur) && cur[c.end] == '\n' && !bytes.HasSuffix(r, cr):
			splitAt = append(splitAt, c.end-1)
		}
	}
	type piece struct {
		b    []byte
		from int // its offset in cur, or -1 for a replacement
		at   int // where in cur a join at its start is reported
	}
	pieces := make([]piece, 0, 2*len(changes)+1)
	last := 0
	for _, c := range changes {
		pieces = append(pieces, piece{cur[last:c.at], last, last})
		for _, s := range c.segs {
			pieces = append(pieces, piece{s.b, s.from, c.at})
		}
		last = c.end
	}
	pieces = append(pieces, piece{cur[last:], last, last})
	var prev *piece
	for i := range pieces {
		p := &pieces[i]
		if len(p.b) == 0 {
			continue
		}
		if prev != nil && prev.b[len(prev.b)-1] == '\r' && p.b[0] == '\n' &&
			!(prev.from >= 0 && p.from >= 0 && prev.from+len(prev.b) == p.from) {
			joinAt = append(joinAt, p.at)
		}
		prev = p
	}
	const strict = ", or pass --eol strict to edit the bytes as they are"
	switch {
	case len(splitAt) > 0:
		return &Diagnosis{
			Kind: DiagSplitCRLF, Cause: "split line ending",
			Detail: "a match begins on the newline of a CRLF, or ends on its carriage return, and its replacement " +
				"does not, which would part them; \".\" matches a carriage return, so end the pattern before it, " +
				"with [^\\r\\n]* rather than .*" + strict,
			Found: len(splitAt), Lines: lineNumbers(cur, splitAt[:min(len(splitAt), tooManyLimit)]),
			MoreLines: max(0, len(splitAt)-tooManyLimit),
		}
	case len(joinAt) > 0:
		slices.Sort(joinAt)
		joinAt = slices.Compact(joinAt)
		return &Diagnosis{
			Kind: DiagJoinedCRLF, Cause: "joined line ending",
			Detail: "a replacement would put a carriage return just before a newline that did not follow it, " +
				"which reads as a CRLF the patch did not write; take the carriage return out of the replacement" + strict,
			Found: len(joinAt), Lines: lineNumbers(cur, joinAt[:min(len(joinAt), tooManyLimit)]),
			MoreLines: max(0, len(joinAt)-tooManyLimit),
		}
	}
	return nil
}
