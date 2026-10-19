package main

// A break set: one file, several patches, a "@@ patch NAME" heading each
// (§3.2, §4.3, issue #100).
//
// A deliberate-break run is easiest written and kept as one file, a heading
// per break. Before this, that file had to be split into one file per break
// outside hunk, by an awk one-liner or a shell function, and a slip in the
// splitter was never seen by the preflight --each promises: the field slipped
// once exactly that way. So the split is hunk's, under the same rule as every
// other directive, and every heading fault is refused before the first try.
//
// Only --each reads headings. A single run given one is refused by the parser
// (patch.go), because applying a break set whole is never what was meant.

import (
	"errors"
	"fmt"
)

// A patchSection is one patch of a break-set file: its heading's name and
// line, and the file's bytes from the line after the heading to the next
// heading. A file with no heading is one section, unnamed, line 0. The
// heading's line is also the section's line offset: its patch begins on the
// file's next line, so the patch's line k is the combined file's line+k.
type patchSection struct {
	name string
	line int // 1-based line of the "@@ patch" heading, 0 for none
	src  []byte
}

// splitPatchSet reads one --each file as a break set, splitting it at its
// "@@ patch" headings. Every fault in the headings is a *ParseError, exit 1,
// named by the combined file's line, so a slip in the set refuses the lot
// before anything is tried:
//
//   - a heading with no name, since the name is how the run reports the patch;
//   - a control character in a name, which every such report would print;
//   - two headings with one name, which would make two results
//     indistinguishable;
//   - content above the first heading, which belongs to no named patch;
//   - a heading with nothing under it, which is a patch with nothing to try.
//
// A heading mistyped as "@@patch", or with a control character stuck to it,
// is not a directive and so not a boundary; the parser refuses the section
// holding it (#96), which refuses the run the same way.
func splitPatchSet(src []byte, marker string) ([]patchSection, error) {
	lines := splitLines(src)

	// starts[i] is the byte offset of line i, so a section is a slice of src
	// rather than a reconstruction: every byte, the line endings included,
	// reaches the parser exactly as it is in the file.
	starts := make([]int, len(lines)+1)
	pos := 0
	for i, line := range lines {
		starts[i] = pos
		pos += len(line) + 1
	}
	starts[len(lines)] = len(src)

	type heading struct {
		name string
		idx  int // 0-based line index
	}
	var heads []heading
	seen := make(map[string]int) // name to its heading's 1-based line
	for i, line := range lines {
		word, arg, ok := directive(line, marker)
		if !ok || word != "patch" {
			continue
		}
		ln := i + 1
		if arg == "" {
			return nil, &ParseError{ln, fmt.Sprintf(
				"%q needs a name; every patch of the set is tried and reported by it",
				marker+" patch")}
		}
		if name, found := firstControl(arg); found {
			return nil, &ParseError{ln, fmt.Sprintf(
				"the name after %q contains %s, a control character, which every report of the patch would print; remove it",
				marker+" patch", name)}
		}
		if len(heads) == 0 && i > 0 {
			return nil, &ParseError{ln, fmt.Sprintf(
				"%q follows content that belongs to no patch; a break set begins with its first heading, so put one above that content or take the headings out",
				marker+" patch "+arg)}
		}
		if prev, dup := seen[arg]; dup {
			return nil, &ParseError{ln, fmt.Sprintf(
				"%q gives a second patch the name of the one at line %d; every patch of the set needs a name of its own",
				marker+" patch "+arg, prev)}
		}
		seen[arg] = ln
		heads = append(heads, heading{arg, i})
	}

	if len(heads) == 0 {
		return []patchSection{{src: src}}, nil
	}
	secs := make([]patchSection, 0, len(heads))
	for k, h := range heads {
		from := h.idx + 1
		to := len(lines)
		if k+1 < len(heads) {
			to = heads[k+1].idx
		}
		if from == to {
			return nil, &ParseError{h.idx + 1, fmt.Sprintf(
				"%q names a patch with nothing in it; every patch of the set needs at least one hunk",
				marker+" patch "+h.name)}
		}
		secs = append(secs, patchSection{
			name: h.name, line: h.idx + 1,
			src: src[starts[from]:starts[to]],
		})
	}
	return secs, nil
}

// renumber moves what a section's parse reports onto the combined file's
// lines: the hunks' for the apply's failures, the strays' for the delimiter
// warnings, and a *ParseError's through err. The typo in break 19 is named by
// the line the caller can open, not by one inside a section nobody can see.
func (sec patchSection) renumber(p *Patch, err error) error {
	if sec.line == 0 {
		return err
	}
	if p != nil {
		for i := range p.Hunks {
			p.Hunks[i].Line += sec.line
		}
		for i := range p.Strays {
			p.Strays[i].Line += sec.line
		}
	}
	var pe *ParseError
	if errors.As(err, &pe) && pe.Line > 0 {
		pe.Line += sec.line
	}
	return err
}

// shown is the section's name as the run reports it: the file's spelling for
// a file with no headings, and file:NAME under one.
func (sec patchSection) shown(file string) string {
	if sec.name == "" {
		return file
	}
	return file + ":" + sec.name
}
