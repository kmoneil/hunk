package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"strings"
)

// invert.go is "hunk invert": a patch printed as the patch that undoes it
// (issue #119). A patch applied on top of other uncommitted work in the same
// files had to be taken back out, to commit the two apart, and git add -p is
// interactive. The inverse came from a 30-line script that swapped each old
// and new, which is the read-transform-write shape hunk exists to replace,
// and rule 1's trailing newline is easy to get wrong in it.
//
// Like quote, it writes a patch and applies nothing. It reads no tree: what
// it prints goes through the parser and the transaction like any other
// patch, and the apply's guards are what check it. A replacement's inverse
// asks for its new text exactly as many times as it was written, with no copy
// overlapping, so a count that is right means the copies it wrote, and any
// other copy of that text makes the count wrong and is refused. So a
// replacement's inverse applies exactly or not at all. A create's is weaker,
// and --help says how.
//
// The hunks come out last first. Hunks apply in order against the file as
// the earlier ones left it (§3.5), so a to b then b to c undoes as c to b
// then b to a; in patch order the first finds no b. What has no inverse the
// patch can say is refused by name, every such hunk, rather than guessed.

// invertUsage is what "hunk invert --help" prints.
const invertUsage = `hunk invert prints the patch that undoes a patch, so that it can be checked
and applied like any other.

  hunk invert [flags] [-f PATCH]

The patch comes from -f or from stdin, and its inverse goes to stdout. Each
replacement has its old and new swapped and keeps its count, and the hunks come
last first, since each undoes the file as the hunks before it left it. A create
becomes a replacement that takes its text out of the file, then a delete, so a
file whose created text has changed since is refused rather than deleted.

  hunk invert -f p.hunk | hunk --dry-run
  hunk invert -f p.hunk | hunk --verify 'go build ./...'

Nothing is read from the tree. The apply checks the inverse as it checks any
patch: each new text has to be in its file exactly as many times as the hunk
wrote it, so an inverse whose text the file no longer holds, or holds somewhere
else too, is refused and nothing is written. Apply it with the --eol and
--marker the patch was applied with.

WHAT IT CANNOT CHECK

That a created file holds nothing but what was created. No hunk can say "only
if the file is exactly this", so text added to a created file since, around the
text it was created with, is deleted with it. Read the --dry-run first. And a
directory a create made is left in place, empty: a delete removes the file.

Under --eol auto, the inverse's text is written in the line ending its file has
most of when the inverse is applied, as any patch's is. A patch that changed
which ending that is, as joining the only two lines of a CRLF file does, gets
its text back in the other one. Where that text would begin or end inside a
CRLF, or put a carriage return just before a newline, the inverse is refused, as
any replacement that would split a CRLF or join one is.

WHAT HAS NO INVERSE

A patch with any of these is refused, every one named:

  @@ delete       the patch does not hold the bytes to put back
  @@ append       it may also have ended the file's last line, and the line
                  ending it added is the file's; the patch holds neither
  @@ prepend      the line ending it may have added is the file's, under the
                  --eol it was applied with; the patch holds neither
  an empty new    the text it took out has nothing left to find its place by
  an @@word old   the inverse would write back an old line such as
                  "@@include x", which a patch refuses where it is written,
                  unless the new holds it too; under another --marker it is text

FLAGS

  -f FILE                     Read the patch from FILE rather than stdin.
  --marker STR                Directive prefix, of the patch and of its
                              inverse. (@@)
  --allow-mistyped-delimiter  Invert a patch with a line that looks like a
                              mistyped heredoc delimiter, as hunk applies one.

REFUSALS

A refusal is written twice: on stderr, and on stdout as an "@@ end" line that
carries the reason, which no patch may hold. So the hunk it is piped into stops
at exit 1 instead of applying part of an inverse, or none at all at exit 0.

  1  a flag, a patch that does not parse or names a mistyped heredoc
     delimiter, or a hunk with no inverse
  5  I/O error writing the inverse
`

// invertFlags is the subcommand's flag set, made in one place so a test can
// hold --help's FLAGS section to it.
type invertFlags struct {
	fs          *flag.FlagSet
	file        *string
	marker      *string
	allowStrays *bool
	help, h     *bool
}

func newInvertFlags() *invertFlags {
	fs := flag.NewFlagSet("hunk invert", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // the errors are written by runInvert, once
	fs.Usage = func() {}
	fl := &invertFlags{
		fs:          fs,
		file:        new(string),
		marker:      fs.String("marker", DefaultMarker, ""),
		allowStrays: fs.Bool(allowMistypedFlag[2:], false, ""),
		help:        fs.Bool("help", false, ""),
		h:           fs.Bool("h", false, ""),
	}
	// One -f is one patch, as for the apply.
	fs.Func("f", "", func(v string) error {
		if *fl.file != "" {
			return fmt.Errorf("already reading %s; hunk invert takes one patch", *fl.file)
		}
		*fl.file = v
		return nil
	})
	return fl
}

// A noInverse is a hunk the patch cannot say how to undo, and why.
type noInverse struct {
	hunk int
	h    Hunk
	why  string
}

// runInvert is the subcommand.
func runInvert(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// A refusal goes to stderr, and its first line into the patch as well, as
	// "@@ end" with an argument, the line the parser refuses wherever it sits:
	// an inverse piped into hunk that printed nothing would apply nothing at
	// exit 0, and the caller would take the patch as undone. The marker is
	// found by hand first, as quote finds it, so a call whose flags do not
	// parse still stops the apply it is piped into.
	marker := markerArg(args)
	refuse := func(code int, msg string) int {
		lines := strings.Split(msg, "\n")
		for i, l := range lines {
			lines[i] = showControls(l)
		}
		fmt.Fprintf(stderr, "hunk invert: %s\n", strings.Join(lines, "\n"))
		fmt.Fprintf(stdout, "%s end refused by hunk invert: %s\n", marker, lines[0])
		return code
	}

	fl := newInvertFlags()
	if err := fl.fs.Parse(args); err != nil {
		return refuse(exitUsage, fmt.Sprintf(`%v; run "hunk invert --help"`, err))
	}
	if *fl.help || *fl.h {
		fmt.Fprint(stdout, invertUsage)
		return exitOK
	}
	if *fl.marker == "" {
		return refuse(exitUsage, "--marker must not be empty")
	}
	if name, ok := firstControl(*fl.marker); ok {
		return refuse(exitUsage, fmt.Sprintf(
			"--marker contains %s, a control character, and a directive line could not start with it", name))
	}
	marker = *fl.marker
	if fl.fs.NArg() > 0 {
		return refuse(exitUsage, fmt.Sprintf(
			"unexpected argument %q; the patch comes from stdin or -f, as for hunk", fl.fs.Arg(0)))
	}

	src, err := readPatch(*fl.file, stdin)
	if err != nil {
		return refuse(exitUsage, err.Error())
	}
	p, err := Parse(src, marker)
	if err != nil {
		return refuse(exitUsage, err.Error())
	}
	// The same refusal the apply gives (#97), for the same reason: the line
	// was probably meant to end a heredoc, and the patch is not the one
	// meant. An inverse of it would carry the slip back out.
	if len(p.Strays) > 0 && !*fl.allowStrays {
		return refuse(exitUsage, (&MistypedDelimiters{Strays: p.Strays}).Error())
	}

	inv, faults := invertPatch(p, marker)
	if len(faults) > 0 {
		has := " have"
		if len(faults) == 1 {
			has = " has"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s%s no inverse, so the patch was not inverted\n", count(len(faults), "hunk"), has)
		for _, f := range faults {
			fmt.Fprintf(&b, "\nhunk %d  %s  (patch line %d)\n  %s", f.hunk, showControls(f.h.Path), f.h.Line, f.why)
		}
		return refuse(exitUsage, b.String())
	}
	if _, err := stdout.Write(printPatch(inv, p.CRLF, marker)); err != nil {
		fmt.Fprintf(stderr, "hunk invert: the inverse could not be written: %v\n", err)
		return exitIO
	}
	return exitOK
}

// invertPatch is the hunks that undo p, last first, or every hunk of p that
// has no inverse, in patch order.
func invertPatch(p *Patch, marker string) ([]Hunk, []noInverse) {
	var faults []noInverse
	for i, h := range p.Hunks {
		var why string
		// The apply refuses such a path (exit 2), so the patch never
		// applied. Printed back, a CR at its end would be read as the line's
		// ending, and the inverse would name another file: FuzzInvert found
		// "00\r".
		if name, ok := firstControl(h.Path); ok {
			faults = append(faults, noInverse{i + 1, h, fmt.Sprintf(
				"its path holds %s, a control character, which hunk refuses in a path, so the patch never applied", name)})
			continue
		}
		switch h.Op {
		case OpDelete:
			why = "a delete is not inverted: the patch does not hold the bytes to put back"
		case OpAppend:
			why = "an append is not inverted: it may also have ended the file's last line, and the line ending it added is the file's; the patch holds neither"
		case OpPrepend:
			why = "a prepend is not inverted: the line ending it may have added is the file's, under the --eol it was applied with; the patch holds neither"
		case OpReplace:
			// What an empty old would be, which the parser refuses: in a CRLF
			// patch a blank line is a CR, which --eol auto reads as its
			// ending.
			if emptyPayload(h.New, p.CRLF) {
				why = "its new text is empty, so it took text out, and nothing is left in the file to find the place to put it back by"
				break
			}
			// The inverse writes the old back, and the parser refuses "@@"
			// glued to a word that is no directive where a payload writes it
			// (#138), unless the inverse's old, this hunk's new, holds it.
			// Printed, the inverse was refused when applied, and --marker
			// could not help, since the inverse is written under the patch's.
			olds := bytes.Split(h.Old, lf)
			if n, _, ok := p.unknownWordIn(olds, bytes.Split(h.New, lf), marker); ok {
				why = fmt.Sprintf("its inverse would write %s back, which starts like a directive and is none, and a patch refuses such a line where it is written; to invert it, write the patch under another --marker",
					shownLine(olds[n]))
			}
		}
		if why != "" {
			faults = append(faults, noInverse{i + 1, h, why})
		}
	}
	if len(faults) > 0 {
		return nil, faults
	}

	var inv []Hunk
	for i := len(p.Hunks) - 1; i >= 0; i-- {
		h := p.Hunks[i]
		switch h.Op {
		case OpReplace:
			inv = append(inv, Hunk{Op: OpReplace, Path: h.Path, Count: h.Count, Old: h.New, New: h.Old})
		case OpCreate:
			// The text is taken out first, so the delete runs only on a
			// file that still holds it. A bare delete would remove whatever
			// the file holds by then. It does not show that the file holds
			// nothing else, which no hunk can say, and --help says so. An
			// empty create has no text to look for, and is a delete.
			if !emptyPayload(h.Body, p.CRLF) {
				inv = append(inv, Hunk{Op: OpReplace, Path: h.Path, Count: 1, Old: h.Body, New: []byte{}})
			}
			inv = append(inv, Hunk{Op: OpDelete, Path: h.Path})
		}
	}
	return inv, nil
}

// emptyPayload reports whether a payload is empty as an old: no bytes, or in
// a CRLF patch nothing but the CR its blank line ends in. It is the parser's
// test for an empty old.
func emptyPayload(b []byte, crlf bool) bool {
	return len(b) == 0 || (crlf && len(stripCR(b)) == 0)
}

// printPatch writes hunks as a patch under marker, which the parser reads
// back as the same hunks: a payload is its bytes and one \n, which the
// parser takes off again (§3.3), and an empty one is nothing at all. In a
// CRLF patch every payload line already ends in its CR, so with the
// directive lines ended in CRLF the inverse is CRLF throughout, as the patch
// was, and --eol auto reads its payloads the same way. "@@ file" is written
// before a replacement whenever the file in effect is another.
//
// All but one: a CRLF patch with no final newline ends in a payload whose
// last line has no CR. Printed with a bare \n, that one line made the
// inverse not CRLF, and --eol auto then read every other payload's CRs as
// text, which FuzzInvert found. It is given the CR, which under --eol auto
// is the same text; under --eol strict it is a byte more, which can only
// make the inverse fail to match.
func printPatch(hunks []Hunk, crlf bool, marker string) []byte {
	eol := "\n"
	if crlf {
		eol = "\r\n"
	}
	var b bytes.Buffer
	payload := func(p []byte) {
		if len(p) > 0 {
			b.Write(p)
			if crlf && !bytes.HasSuffix(p, cr) {
				b.WriteByte('\r')
			}
			b.WriteByte('\n')
		}
	}
	cur := ""
	for _, h := range hunks {
		switch h.Op {
		case OpReplace:
			if h.Path != cur {
				fmt.Fprintf(&b, "%s file %s%s", marker, h.Path, eol)
				cur = h.Path
			}
			b.WriteString(marker + " old")
			if h.Count > 1 {
				fmt.Fprintf(&b, " x%d", h.Count)
			}
			b.WriteString(eol)
			payload(h.Old)
			b.WriteString(marker + " new" + eol)
			payload(h.New)
		case OpDelete:
			fmt.Fprintf(&b, "%s delete %s%s", marker, h.Path, eol)
			cur = h.Path
		}
	}
	return b.Bytes()
}
