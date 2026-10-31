package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// quote.go is "hunk quote": a line range of a file printed as a hunk, so that
// replacing a whole block does not start with pasting it (issue #82).
//
// It is a second way to write a patch and no part of applying one. The text it
// prints goes through the parser and the transaction like any other, and still
// has to match exactly once, so the grammar and every promise the apply makes
// are what they were. What it adds is on the way in, and three sessions had
// built it by hand with sed before it was written here. Each of those scripts
// could weld a directive onto a payload, obey a directive that was in the
// replacement text, or leave a CRLF file with both endings, all at exit 0.
//
// One thing it cannot check, and says so in its help and on stderr: the
// numbers. Text read off the file a moment ago always matches, so a line
// number gone stale since the caller read the file quotes another block and
// the apply cannot tell. The report of each range's first and last line is
// there for that.

// quoteUsage is what "hunk quote --help" prints.
const quoteUsage = `hunk quote prints a line range of a file as a hunk, so that replacing a whole
block does not start with pasting it.

  hunk quote [flags] FILE START END

On stdout: "@@ file FILE", "@@ old", and lines START through END of FILE,
counted from 1. It is a patch like any other. Follow it with "@@ new" and the
replacement, or pass --new, then concatenate it with other hunks and pipe the
lot into hunk:

  { hunk quote --new new/parse.go src/parse.go 40 95
    cat rest.hunk
  } | hunk --dry-run

Nothing is written to the tree. The apply still has to find the quoted text
exactly once, so a range too short to be unique is refused there, and so is a
file that changed in between.

WHAT IT CANNOT CHECK

The numbers. The text quoted is whatever is at those lines now, so it always
matches, and a line number that has gone stale since you read the file quotes
another block. Each range's first and last line are printed on stderr for that
reason: run the batch with --dry-run first and read them.

FLAGS

  --new FILE            Add "@@ new" and FILE's content, so the range is
                        replaced by it. An empty FILE deletes the lines. FILE
                        is relative to the working directory, as -f is.
  --marker STR          Directive prefix. (@@) Give the same one to the hunk
                        that applies the patch. For when a line of the range,
                        or of --new's file, would read as a directive.
  --root DIR            Resolve FILE here, as the apply will. (cwd)
  --allow-outside-root  Permit a FILE that resolves outside --root.

REFUSALS

A refusal is written twice: on stderr, and on stdout as an "@@ end" line that
carries the reason, which no patch may hold. So the batch the quote was meant
for stops at exit 1 instead of applying without it.

  1  a flag, a number, START after END, a FILE no directive line can name,
     or a --new file that cannot be read
  2  FILE refused as the apply would refuse it, a line past the end of FILE,
     a line of the range or of --new's file that is a directive under the
     marker, or a range that is one empty line
  5  I/O error
`

// A quoteRefusal is hunk quote declining, with the exit it means (§4.1's set:
// 1 for a call that is wrong whatever the tree holds, 2 for what the tree
// decides).
type quoteRefusal struct {
	code int
	msg  string
}

// quoteFlags is the subcommand's flag set, made in one place so a test can
// hold --help's FLAGS section to it.
type quoteFlags struct {
	fs           *flag.FlagSet
	newFile      *string
	marker       *string
	root         *string
	allowOutside *bool
	help, h      *bool
}

func newQuoteFlags() *quoteFlags {
	fs := flag.NewFlagSet("hunk quote", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // the errors are written by runQuote, once
	fs.Usage = func() {}
	return &quoteFlags{
		fs:           fs,
		newFile:      fs.String("new", "", ""),
		marker:       fs.String("marker", DefaultMarker, ""),
		root:         fs.String("root", "", ""),
		allowOutside: fs.Bool("allow-outside-root", false, ""),
		help:         fs.Bool("help", false, ""),
		h:            fs.Bool("h", false, ""),
	}
}

// A Quote is one range of one file as the text of a hunk.
type Quote struct {
	Patch []byte
	// First and Last are the range's first and last lines without their
	// endings, for the report.
	First, Last []byte
}

// runQuote is the subcommand. It reads FILE through the tree, as the apply
// will, and writes nothing to it.
func runQuote(args []string, stdout, stderr io.Writer) int {
	// A refusal goes to stderr, and into the patch as well. The patch is
	// usually one part of a brace group piped into the apply, which cannot see
	// this process fail: a quote that printed nothing left the rest of the
	// batch to apply without it, at exit 0 (issue #80's shape, from the other
	// end). "@@ end" with an argument is a line the parser refuses wherever it
	// sits, so the batch stops at exit 1 and says why.
	//
	// The line is only a directive under the marker the apply reads by, so the
	// marker is found by hand before the flag package runs: a call whose flags
	// do not parse has still been told it, unless --marker is what was
	// misspelled, and then this falls back to the default.
	marker := markerArg(args)
	refuse := func(code int, msg string) int {
		msg = showControls(msg)
		fmt.Fprintf(stderr, "hunk quote: %s\n", msg)
		fmt.Fprintf(stdout, "%s end refused by hunk quote: %s\n", marker, msg)
		return code
	}

	fl := newQuoteFlags()
	fs := fl.fs
	// The flag package stops at the first argument that is not a flag, and the
	// natural spelling puts --new after the range. So it is run again on what
	// follows each positional argument.
	var pos []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return refuse(exitUsage, fmt.Sprintf(`%v; run "hunk quote --help"`, err))
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if *fl.help || *fl.h {
		fmt.Fprint(stdout, quoteUsage)
		return exitOK
	}
	// A marker that cannot head a line is refused under the default, which is
	// what markerArg made of it.
	if *fl.marker == "" {
		return refuse(exitUsage, "--marker must not be empty")
	}
	if name, ok := firstControl(*fl.marker); ok {
		return refuse(exitUsage, fmt.Sprintf(
			"--marker contains %s, a control character, and a directive line could not start with it", name))
	}
	// The marker as the flag package read it, which is what markerArg found
	// except where an argument that looks like the flag was another flag's
	// value.
	marker = *fl.marker

	if len(pos) != 3 {
		return refuse(exitUsage, fmt.Sprintf(
			`quote takes FILE START END, and got %s; run "hunk quote --help"`, count(len(pos), "argument")))
	}
	path := pos[0]
	start, r := lineNumber(pos[1])
	if r != nil {
		return refuse(r.code, r.msg)
	}
	end, r := lineNumber(pos[2])
	if r != nil {
		return refuse(r.code, r.msg)
	}
	if start > end {
		return refuse(exitUsage, fmt.Sprintf("START is %d and END is %d, so the range runs backwards", start, end))
	}
	// The parser trims a path (§3.2), so one that begins or ends with a space
	// would name another file in the patch than it does here, and an empty
	// one would leave "@@ file" with no path at all.
	if path == "" {
		return refuse(exitUsage, "FILE is empty")
	}
	if strings.Trim(path, " \t") != path {
		return refuse(exitUsage, fmt.Sprintf(
			"%q begins or ends with a space or a tab, which a directive line drops, so no patch can name it", path))
	}

	// --new's file is the caller's, like -f: relative to the working
	// directory, not to --root, and a failure to read it is the call's.
	var repl []byte
	hasNew := false
	fs.Visit(func(f *flag.Flag) { hasNew = hasNew || f.Name == "new" })
	if hasNew {
		b, err := os.ReadFile(*fl.newFile)
		if err != nil {
			return refuse(exitUsage, fmt.Sprintf("--new: %v", err))
		}
		repl = b
	}

	dir := *fl.root
	if dir == "" {
		dir = "."
	}
	tree, err := OpenTree(dir, *fl.allowOutside)
	if err != nil {
		return refuse(exitIO, err.Error())
	}
	defer func() { _ = tree.Close() }()

	// FILE is loaded as an "@@ old" on it would be, by the transaction's own
	// load, so every refusal here is the one the apply would give for the same
	// path, in the same words: outside the root, inside .git, a directory, a
	// named pipe, a file in the way, absent.
	hunk := Hunk{Op: OpReplace, Path: path}
	f, err := NewTxn(tree, Options{}).load(hunk)
	if err != nil {
		return refuse(ExitCode(err), err.Error())
	}
	if r := requireState(f, hunk, tree.Shown(), false); r != nil {
		return refuse(exitNoMatch, r.Error())
	}

	q, r := quoteLines(path, f.orig, start, end, repl, hasNew, *fl.newFile, marker)
	if r != nil {
		return refuse(r.code, r.msg)
	}
	if _, err := stdout.Write(q.Patch); err != nil {
		fmt.Fprintf(stderr, "hunk quote: the patch could not be written: %v\n", err)
		return exitIO
	}
	// The report, on stderr so it is never part of the patch. Not behind
	// "hunk: ", which is how an error starts (#73).
	lines := strconv.Itoa(start)
	ends := shownLine(q.First)
	if end > start {
		lines += "-" + strconv.Itoa(end)
		ends += " to " + shownLine(q.Last)
	}
	fmt.Fprintf(stderr, "quoted %s %s, %s: %s\n", path, lines, count(end-start+1, "line"), ends)
	return exitOK
}

// markerArg is --marker's value read off the arguments without the flag
// package, which stops at the first flag it does not know: the last one given,
// as the flag package would have it, or the default.
func markerArg(args []string) string {
	marker := DefaultMarker
	for i, a := range args {
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "marker" {
			continue
		}
		switch {
		case hasVal:
			marker = val
		case i+1 < len(args):
			marker = args[i+1]
		}
	}
	// One that could not head a line is no use to a refusal.
	if _, bad := firstControl(marker); bad || marker == "" {
		return DefaultMarker
	}
	return marker
}

// lineNumber reads START or END: digits, and at least 1. Atoi alone takes a
// sign, and "-3" is nobody's line.
func lineNumber(s string) (int, *quoteRefusal) {
	bad := &quoteRefusal{exitUsage, fmt.Sprintf("START and END are line numbers counted from 1, and %q is not one", s)}
	if s == "" {
		return 0, bad
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, bad
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, bad
	}
	return n, nil
}

// quoteLines is lines start through end of file as a hunk on path: "@@ file",
// "@@ old" and the lines, and with hasNew an "@@ new" and repl. newName is how
// a message names the file repl came from.
//
// Whole lines out, whole lines in. old is the lines without the last one's
// line ending, and new is repl without its last one, so the two come out even
// whether or not either file ends in a newline, and a CRLF file keeps the CRLF
// after the range (under --eol auto the apply converts the rest, §6.4). That
// is also the cut a hand-written "@@ new" needs after a quote with no --new:
// typed the ordinary way, its payload has no final newline either (§3.3).
//
// An empty repl deletes the lines, and then old takes its last line ending
// too: a new of nothing after an old cut short would leave a blank line where
// the block was. At an unterminated last line there is no ending to take, and
// the line before keeps its own.
//
// Each payload is written as its bytes and one newline, which is how a patch
// says exactly those bytes: the lines up to the next directive joined by
// newlines, with none added (§3.3).
func quoteLines(path string, file []byte, start, end int, repl []byte, hasNew bool, newName, marker string) (*Quote, *quoteRefusal) {
	total := lineCount(file)
	if end > total {
		// The first line asked for that is not there.
		missing := end
		if start > total {
			missing = start
		}
		return nil, &quoteRefusal{exitNoMatch, fmt.Sprintf(
			"%s has %s, so there is no line %d", path, count(total, "line"), missing)}
	}

	// from is where line start begins, to where line end's text stops, and
	// next where the line after it begins, which is past its ending.
	var from, to, next int
	q := &Quote{}
	for n, off := 1, 0; n <= end; n++ {
		textEnd, after := len(file), len(file)
		if i := bytes.IndexByte(file[off:], '\n'); i >= 0 {
			textEnd, after = off+i, off+i+1
			// A CR before the LF is the line's ending, not its text.
			if textEnd > off && file[textEnd-1] == '\r' {
				textEnd--
			}
		}
		if n == start {
			from, q.First = off, file[off:textEnd]
		}
		if n == end {
			to, next, q.Last = textEnd, after, file[off:textEnd]
		}
		off = after
	}

	old := file[from:to]
	deletes := hasNew && len(repl) == 0
	if deletes {
		old = file[from:next]
	}
	// One empty line. An empty old matches at every position rather than at
	// one, and the parser refuses it (§3.3) with a sentence about payloads
	// that says nothing of a range.
	if len(old) == 0 {
		return nil, &quoteRefusal{exitNoMatch, fmt.Sprintf(
			"%s line %d is empty, and an old of nothing matches at every position in the file; "+
				"quote it together with a line beside it", path, start)}
	}
	if bytes.HasSuffix(repl, crlf) {
		repl = repl[:len(repl)-2]
	} else {
		repl = bytes.TrimSuffix(repl, lf)
	}

	// A line that is a directive under the marker would be read as one: in the
	// range it cuts old short, which the parser refuses, and in the replacement
	// it is obeyed, so a new text that quotes "@@ delete b.txt" deleted b.txt
	// at exit 0 when the quoting was done with sed. Refused here for both, and
	// so is a line the parser refuses wherever it is, a directive with a
	// control character stuck to it.
	if r := refuseDirectives(old, path, start, marker); r != nil {
		return nil, r
	}
	if hasNew {
		if r := refuseDirectives(repl, newName, 1, marker); r != nil {
			return nil, r
		}
		if r := refuseUnknownWords(repl, old, newName, marker); r != nil {
			return nil, r
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "%s file %s\n%s old\n", marker, path, marker)
	b.Write(old)
	b.WriteByte('\n')
	if hasNew {
		fmt.Fprintf(&b, "%s new\n", marker)
		b.Write(repl)
		b.WriteByte('\n')
	}
	q.Patch = b.Bytes()
	return q, nil
}

// refuseUnknownWords refuses a line of the replacement that the parser refuses
// in a payload that writes (#138): "@@" glued to a word that is no directive,
// "@@include x", which unknownWord takes, unless the range holds it, since
// the hunk then carries it over. Lines compared whole, as the parser compares
// them in the patch quote prints, which no CRLF reading applies to: its
// directive lines end in LF alone. Line numbers are the replacement file's.
func refuseUnknownWords(repl, old []byte, name, marker string) *quoteRefusal {
	var held map[string]bool
	for i, line := range bytes.Split(repl, lf) {
		word, ok := unknownWord(line, marker)
		if !ok {
			continue
		}
		if held == nil {
			held = make(map[string]bool)
			for _, l := range bytes.Split(old, lf) {
				held[string(l)] = true
			}
		}
		if held[string(line)] {
			continue
		}
		return &quoteRefusal{exitNoMatch, fmt.Sprintf(
			"%s line %d is %s, which starts like a directive, but %s is not one, and a patch refuses such a line where it would be written; "+
				"pass --marker with a prefix that no line quoted starts with, here and to the hunk that applies the patch",
			name, 1+i, shownLine(line), quoteCut(word))}
	}
	return nil
}

// refuseDirectives refuses a payload with a line the parser would not read as
// text, naming the file it came from and the line there. first is the number
// of the payload's first line in that file.
func refuseDirectives(payload []byte, name string, first int, marker string) *quoteRefusal {
	for i, line := range bytes.Split(payload, lf) {
		what := ""
		if _, _, ok := directive(line, marker); ok {
			what = "which a patch reads as a directive and not as text"
		} else if _, ok := nearDirective(bytes.TrimSuffix(line, cr), marker); ok {
			what = "which a patch refuses as a directive with a control character in it"
		} else if _, ok := separatorFault(line, marker); ok {
			what = "which a patch refuses as a directive missing its space, or with one that is not ASCII"
		}
		if what == "" {
			continue
		}
		return &quoteRefusal{exitNoMatch, fmt.Sprintf(
			"%s line %d is %s, %s; pass --marker with a prefix that no line quoted starts with, "+
				"here and to the hunk that applies the patch", name, first+i, shownLine(line), what)}
	}
	return nil
}
