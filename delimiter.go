package main

// A payload line that looks like a mistyped heredoc delimiter (#92).
//
// Every example hunk ships wraps the patch in <<'HUNK' ... HUNK. A closer the
// shell does not take for the delimiter, "HUHUNK", "HUNKK", "  HUNK", is
// payload to hunk. With the real closer after it, the patch is well formed and
// the line goes into the file at exit 0. With none, the heredoc runs to the
// end of the command, and the commands after it go into the file too, never
// having run. No shell says a word either way under sh -c, bash, zsh or dash.
//
// So the parser notes such a line, the patch applies as written, since a
// document about hunk may really hold one, and the report says where it went.
// The rule is the line, once the spaces, tabs and carriage returns around it
// are gone, being one word of ASCII letters and digits that holds HUNK or is
// one edit from it, and not being the line HUNK itself. The issue's "ends in
// HUNK" missed HUNKK, and caught MAX_CHUNK ending a Go line; on 116,879 text
// files this rule fired only on an indented HUNK in markdown about hunk.
//
// The line HUNK itself is left alone: it cannot come through <<'HUNK', so it
// came through -f or another delimiter, on purpose. So is a line of an "@@ old"
// payload, which has to match the file, and a line of "@@ new" that its old
// already holds, which the hunk carries over rather than writes.

import "bytes"

// delimiterWord is the delimiter every example uses, and so the one a typo is
// made of.
const delimiterWord = "HUNK"

var delimiterBytes = []byte(delimiterWord)

// A Stray is a payload line that looks like a mistyped heredoc delimiter.
type Stray struct {
	Line int    // the 1-based patch line
	Path string // the file its payload is for, as the patch names it
	Text []byte // the line, without the CR that ends it in a CRLF patch
	// After is how many payload lines follow it in the same payload, the
	// blank one that gives the payload its final newline not counted. With
	// no real closer they are the rest of the command, which never ran.
	After int
}

// looksLikeDelimiter is the rule. crlf says the patch is uniformly CRLF, where
// one CR ending a line is its line ending (§3.2, §6.4), so "HUNK" and a CR is
// the line HUNK.
func looksLikeDelimiter(line []byte, crlf bool) bool {
	exact := line
	if crlf {
		exact = bytes.TrimSuffix(line, cr)
	}
	if string(exact) == delimiterWord {
		return false
	}
	// On the bytes, since every line of every payload that writes comes
	// through here: a string of each was an allocation a line.
	w := bytes.Trim(line, " \t\r")
	if len(w) == 0 {
		return false
	}
	for _, c := range w {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return bytes.Contains(w, delimiterBytes) || oneEditFrom(string(w), delimiterWord)
}

// oneEditFrom reports whether a is b with one byte inserted, deleted or
// replaced, or two neighbours swapped. The caller has made both ASCII.
func oneEditFrom(a, b string) bool {
	switch len(a) - len(b) {
	case 0:
		i := 0
		for i < len(a) && a[i] == b[i] {
			i++
		}
		if i == len(a) {
			return false
		}
		if a[i+1:] == b[i+1:] {
			return true
		}
		return i+1 < len(a) && a[i] == b[i+1] && a[i+1] == b[i] && a[i+2:] == b[i+2:]
	case 1:
		return dropsOne(a, b)
	case -1:
		return dropsOne(b, a)
	}
	return false
}

// dropsOne reports whether short is long with one byte taken out.
func dropsOne(long, short string) bool {
	i := 0
	for i < len(short) && long[i] == short[i] {
		i++
	}
	return long[i+1:] == short[i:]
}

// noteStrays records the lines of a payload that writes, lines[start:end],
// that look like a mistyped delimiter. old is the hunk's old payload as lines,
// nil for create, append and prepend: a line old holds is carried over.
func (p *Patch) noteStrays(lines [][]byte, start, end int, path string, old [][]byte) {
	last := end
	if last > start && len(p.lineText(lines[last-1])) == 0 {
		last--
	}
	var held map[string]bool
	for i := start; i < end; i++ {
		if !looksLikeDelimiter(lines[i], p.CRLF) {
			continue
		}
		// Compared without the CR that ends a CRLF patch's line, which the
		// patch's last line may not have: fuzzing found "  HUNK" carried over
		// and noted all the same, its old line ending and its new one not.
		if held == nil {
			held = make(map[string]bool, len(old))
			for _, l := range old {
				held[string(p.lineText(l))] = true
			}
		}
		if held[string(p.lineText(lines[i]))] {
			continue
		}
		// A blank line is never one, so a stray stands before last.
		p.Strays = append(p.Strays, Stray{Line: i + 1, Path: path, Text: p.lineText(lines[i]), After: last - i - 1})
	}
}

// lineText is a patch line without its ending: the CR that ends it in a CRLF
// patch, and nothing otherwise.
func (p *Patch) lineText(line []byte) []byte {
	if p.CRLF {
		return bytes.TrimSuffix(line, cr)
	}
	return line
}
