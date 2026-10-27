package main

// Text and JSON output (§5).
//
// These messages are the product. §8.1: "The exact bytes of every failure
// message, because those messages are the product. A reworded diagnostic is a
// contract change." So every shape here is goldened, and both renderers take
// one Report and nothing else, which is what makes §5.2's promise structural:
// "--json carries everything the text does, so a harness never parses the
// text."
//
// The JSON failure shape has a second consumer that does not exist yet. §10:
// the MCP server "will return" it, "which is why that shape is specified now".
// So §5.2's object is followed exactly rather than tidied.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// A Verify is the outcome of --verify (§5.1's JSON, §5.3's text).
//
// verify-and-rollback populates it and does the restoring; this file renders
// it. The seam is here because rendering §5.3 in two places is the only way to
// end up with two different §5.3s.
type Verify struct {
	Ran     bool
	OK      bool
	Command string
	Seconds float64

	// Tail is the last --verify-lines lines of combined output, and TotalLines
	// how many there were, so the report can say what it elided.
	Tail       []string
	TotalLines int

	// Applied, RolledBack and NotRestored describe the failure (§5.3, §6.3).
	Applied     int
	RolledBack  int
	NotRestored []NotRestored

	// Gone are files hunk created that something else removed before rollback
	// could. Absence is what rolling a create back leaves, so they are counted
	// in RolledBack and are not NotRestored, but something other than hunk
	// touched the tree during the verify, and the report says so (§6.3).
	Gone []string

	// Left are directories commit made that rollback left where they are,
	// because they are not empty or are no longer the directories hunk made.
	// The batch's own changes are undone, so the exit is still 3; the report
	// names them, since something else made them so.
	Left []LeftDir

	// Kept means --keep-on-fail left the changes in place. The exit is still 3
	// (§4): the code reports what the verify said, not what was done about it.
	Kept bool

	// Try means this is --try's command rather than a verify: the batch is put
	// back whatever it does, and the exit is its Status (§4.1).
	Try    bool
	Status int

	// TimedOut is the --timeout that ended the command, and Signal the signal
	// to hunk that ended it, when either did. A signal can also arrive while the
	// batch is being written, and then the command never runs: Ran is false and
	// Signal is set. Cut means sh had exited and something it started still held
	// its output open, so hunk stopped reading verifyWaitDelay later.
	TimedOut time.Duration
	Signal   os.Signal
	Cut      bool
}

// A NotRestored is a file rollback left alone because something rewrote it
// after hunk did (§6.3). Naming it is the whole of exit 4's usefulness.
type NotRestored struct {
	Path     string
	Resolved string // where Path led, when that is another name (§6.5)
	Reason   string
}

// withResolved is a path as the patch wrote it, followed by where it led when
// that is another name. The name comes from the tree, a link's destination
// among it, so a control character in it is shown by name, as a refusal
// shows one.
func withResolved(path, resolved string) string {
	if resolved == "" {
		return path
	}
	return path + " " + resolvedClause(showControls(resolved))
}

// A Report is everything one invocation has to say.
type Report struct {
	Exit     int
	Result   *Result
	Failures []Failure
	Verify   *Verify
	Err      error
	DryRun   bool
	// Hunks is the size of the whole patch, which §5.2's header needs ("2 of 5
	// hunks did not match") and which Result cannot supply because there is no
	// Result on a failure.
	Hunks int
	// Strays are the patch's lines that look like a mistyped heredoc
	// delimiter (#92), warned of after the report whatever the outcome.
	Strays []Stray
}

// count is the summary form: "1 file", "3 files". Distinct from plural, which
// reads "a tab" and is right in a detail sentence and wrong in a count.
func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// ExitCode maps an error to §4.1's closed set. It lives here rather than in
// main.go because §5.2's JSON carries "exit", and two mappings for one table is
// how they diverge.
func ExitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var pe *ParseError
	if errors.As(err, &pe) {
		return exitUsage
	}
	var md *MistypedDelimiters
	if errors.As(err, &md) {
		return exitUsage
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return exitNoMatch
	}
	// A path refusal is a validation failure: §6.1's line is whether editing
	// the patch would fix it, and here it would.
	var pr *PathRefusal
	if errors.As(err, &pr) {
		return exitNoMatch
	}
	var ce *ChangedError
	if errors.As(err, &ce) {
		return exitChanged
	}
	return exitIO
}

// NewReport assembles what the renderers take.
func NewReport(res *Result, err error, v *Verify, dryRun bool, hunks int) *Report {
	r := &Report{
		Result: res, Verify: v, Err: err, DryRun: dryRun, Hunks: hunks,
		Exit: ExitCode(err),
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		r.Failures = ve.Failures
	}
	switch {
	case v != nil && v.Signal != nil:
		// A signal to hunk while the batch was on disk: the command ended, the
		// batch went back, and hunk dies of the same signal once the report is
		// written, which a shell reads as 128 plus it (§4.1). The code says that
		// for a caller of invoke, and for --json. 4 when the tree is not back, since
		// an inconsistent tree is the thing to say.
		r.Exit = signalExit(v.Signal)
		if len(v.NotRestored) > 0 {
			r.Exit = exitRollbackFailed
		}
	case v != nil && v.Try:
		// §4.1: under --try the exit is the command's own status once the tree
		// is back, and 4 when it is not, whatever the command returned. A
		// command --timeout ended has 124 for its status, as timeout(1) exits,
		// so that a caller can tell it from anything the command said.
		if v.Ran {
			r.Exit = v.Status
		}
		if len(v.NotRestored) > 0 {
			r.Exit = exitRollbackFailed
		}
	case v != nil && v.Ran && !v.OK:
		r.Exit = exitVerifyFailed
		if len(v.NotRestored) > 0 {
			r.Exit = exitRollbackFailed
		}
	case v != nil && len(v.NotRestored) > 0:
		// A verify that could not start, and a rollback that could not put
		// everything back: the tree is inconsistent, which is 4 (§4.1).
		r.Exit = exitRollbackFailed
	}
	return r
}

// changedTheTree reports whether the batch is on disk when the report is
// written: applied and kept, rather than refused, rolled back, put back by
// --try, or previewed.
func (r *Report) changedTheTree() bool {
	if r.DryRun || r.Result == nil || len(r.Result.Files) == 0 {
		return false
	}
	v := r.Verify
	return (r.Exit == exitOK && (v == nil || !v.Try)) || (v != nil && v.Kept)
}

// Text writes §5's human output. Success goes to out; anything an agent has to
// act on goes to errOut, so a caller that redirects stdout still sees why.
//
// quiet suppresses the success report only (§4). A failure is never silent:
// --quiet says "print nothing on success", and a caller who wanted no output at
// all would not have run the tool.
//
// A line that looks like a mistyped delimiter is warned of last, on errOut,
// under quiet too: it is a guess about what the caller meant rather than a
// fact about what hunk did, it is something to act on, and a --try report has
// to start "tried" (§4.1).
//
// A replacement whose ends disagree about a newline (#111), or that changed
// nothing (#118), is warned of there too, before the strays, for the same
// reasons.
func (r *Report) Text(out, errOut io.Writer, quiet bool) {
	o, e := &tally{w: out}, &tally{w: errOut}
	r.text(o, e, quiet)
	gap := o.n+e.n > 0
	if r.Result != nil && writeSlips(errOut, "", r.Result.Slips, r.changedTheTree(), gap) {
		gap = false
	}
	writeStrays(errOut, "", r.Strays, r.changedTheTree(), gap)
}

func (r *Report) text(out, errOut io.Writer, quiet bool) {
	// --try's report is its command's transcript, whatever the command said,
	// since that is what the caller ran it for. On exit 0 it is a success
	// report, on stdout and subject to --quiet; otherwise it goes to stderr.
	// Either way it starts `tried`, never the `hunk: ` prefix: §4.4 makes that
	// word how a caller tells the command's own failure from a refused patch.
	if v := r.Verify; v != nil && v.Try && v.Ran {
		if r.Exit == exitOK {
			if !quiet {
				r.writeTry(out)
			}
			return
		}
		r.writeTry(errOut)
		return
	}
	if r.Exit == exitOK {
		if quiet {
			return
		}
		r.writeSuccess(out)
		return
	}
	switch {
	case len(r.Failures) > 0:
		r.writeValidationFailure(errOut)
	case r.Verify != nil && r.Verify.Ran && !r.Verify.OK:
		r.writeVerifyFailure(errOut)
	default:
		fmt.Fprintf(errOut, "hunk: %v\n", r.Err)
		// A commit that failed part-way and could not put everything back
		// names what it left, as exit 4 does (§6.2).
		var ce *CommitError
		if errors.As(r.Err, &ce) {
			writeNotRestored(errOut, ce.NotRestored)
		}
		// A --verify or --try command that could not start, after which the
		// batch could not all be put back.
		if v := r.Verify; v != nil {
			writeNotRestored(errOut, v.NotRestored)
		}
	}
}

func (r *Report) writeSuccess(w io.Writer) {
	res := r.Result
	if res == nil {
		return
	}
	// Pad to the longest path so the stats line up. §5.1's example puts two of
	// three rows at the column this produces and the third one past it.
	//
	// Counted in runes, because fmt pads in runes. Until 2026-10-08 it was
	// counted in bytes, so a path with a non-ASCII letter was padded past
	// itself: alone, "M é.txt  +1 -1" in NFD, and beside an ASCII path the
	// two did not line up.
	width := 0
	for _, f := range res.Files {
		width = max(width, utf8.RuneCountInString(f.Path))
	}
	for _, f := range res.Files {
		fmt.Fprintf(w, "%s %-*s +%d -%d", opLetter(f.Op), width, f.Path, f.Added, f.Removed)
		// About the path, so before the notes about the bytes.
		if f.Resolved != "" {
			fmt.Fprint(w, "  "+resolvedClause(showControls(f.Resolved)))
		}
		if f.SeamAdded {
			fmt.Fprint(w, "  (added a final newline)")
		}
		if f.NoFinalNewline {
			fmt.Fprint(w, "  (no final newline)")
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "%s, %s, +%d -%d",
		count(len(res.Files), "file"), count(res.Hunks, "hunk"), res.Added(), res.Removed())
	switch {
	case r.DryRun && r.Verify != nil && r.Verify.Try:
		fmt.Fprint(w, " (dry run: nothing written, command not run)")
	case r.DryRun && r.Verify != nil:
		// §4: --dry-run writes nothing and runs no verify. Both are said,
		// because a caller who passed --verify and sees only "nothing written"
		// has no reason not to assume the verify passed.
		fmt.Fprint(w, " (dry run: nothing written, verify not run)")
	case r.DryRun:
		fmt.Fprint(w, " (dry run: nothing written)")
	case r.Verify != nil && r.Verify.Ran:
		fmt.Fprintf(w, ", verify ok (%.1fs)", r.Verify.Seconds)
	}
	// Nothing follows the total on the same line. A note on a trivial batch
	// used to, and §5.1 says why it went. A verify whose output was cut gets a
	// line of its own, because a process left running is worth a line.
	fmt.Fprintln(w)
	if v := r.Verify; v != nil && v.Ran && v.Cut {
		fmt.Fprintln(w, cutNote())
	}
}

func (r *Report) writeValidationFailure(w io.Writer) {
	bad, skipped := 0, 0
	for _, f := range r.Failures {
		if f.Skipped() {
			skipped++
		} else {
			bad++
		}
	}
	// §5.2's own header: "2 of 5 hunks did not match, 1 skipped". The "of M"
	// half only says something when some hunks did match, so "1 of 1 hunk" is
	// dropped in favour of "1 hunk".
	if r.Hunks > bad {
		fmt.Fprintf(w, "hunk: %d of %s did not match", bad, count(r.Hunks, "hunk"))
	} else {
		fmt.Fprintf(w, "hunk: %s did not match", count(bad, "hunk"))
	}
	if skipped > 0 {
		fmt.Fprintf(w, ", %d skipped", skipped)
	}
	fmt.Fprintln(w, "; nothing was written")
	writeFailureBlocks(w, r.Failures)
}

// writeFailureBlocks is §5.2's body, one block per failure, shared with
// --each's preflight, whose header names the patch rather than counting the
// batch's hunks.
func writeFailureBlocks(w io.Writer, failures []Failure) {
	for _, f := range failures {
		fmt.Fprintf(w, "\nhunk %d  %s  (patch line %d)\n", f.Hunk, f.Path, f.PatchLine)
		if f.Skipped() {
			fmt.Fprintf(w, "  skipped: same file as hunk %d, which did not match\n", f.SkippedAfter)
			continue
		}
		if f.Refusal != "" {
			fmt.Fprintf(w, "  %s\n", f.Refusal)
			continue
		}
		fmt.Fprintf(w, "  expected %s, found %d", count(f.Expected, "occurrence"), f.Found)
		if f.Near == nil {
			fmt.Fprintln(w)
			continue
		}
		// Where the count missed copies, the number found is the true one and
		// says what kind it is, since it is not what a plain count would give.
		switch f.Near.Kind {
		case DiagOverlap:
			fmt.Fprint(w, " that overlap")
		case DiagMixedEndings:
			fmt.Fprint(w, " across both line endings")
		case DiagSplitCRLF:
			// The count was right, so the number found is the claim, and the
			// clause says how many of those copies cannot be edited.
			endings := "CRLF line endings"
			if f.Near.Found == 1 {
				endings = "a CRLF line ending"
			}
			switch {
			case f.Found == 1:
				fmt.Fprint(w, ", and replacing it would split ", endings)
			case f.Near.Found == f.Found:
				fmt.Fprint(w, ", and replacing them would split ", endings)
			default:
				fmt.Fprintf(w, ", and replacing %d of them would split %s", f.Near.Found, endings)
			}
		case DiagJoinedCRLF:
			// As for a split: the count was right, and the clause says how
			// many of the copies cannot be edited.
			pairs := "carriage returns to newlines"
			if f.Near.Found == 1 {
				pairs = "a carriage return to a newline"
			}
			switch {
			case f.Found == 1:
				fmt.Fprint(w, ", and replacing it would join ", pairs)
			case f.Near.Found == f.Found:
				fmt.Fprint(w, ", and replacing them would join ", pairs)
			default:
				fmt.Fprintf(w, ", and replacing %d of them would join %s", f.Near.Found, pairs)
			}
		}
		fmt.Fprintln(w)
		switch f.Near.Kind {
		case DiagTooMany, DiagOverlap, DiagMixedEndings, DiagSplitCRLF, DiagJoinedCRLF:
		default:
			fmt.Fprintln(w)
		}
		fmt.Fprint(w, f.Near.Render("  "))
	}
}

func (r *Report) writeVerifyFailure(w io.Writer) {
	v := r.Verify
	fmt.Fprintf(w, "hunk: applied %s, %s", count(v.Applied, "hunk"), verifyEnded(v))
	switch {
	case v.Kept:
		fmt.Fprint(w, ", changes left in place for inspection (--keep-on-fail)\n")
	case len(v.NotRestored) > 0:
		fmt.Fprintf(w, ", rolled back %s, %s left alone\n",
			count(v.RolledBack, "file"), count(len(v.NotRestored), "file"))
	default:
		fmt.Fprintf(w, ", rolled back %s\n", count(v.RolledBack, "file"))
	}
	writeTranscript(w, v)
}

// writeTry is --try's report: what was tried, what was put back, and what
// the command said, which is the whole reason it ran.
func (r *Report) writeTry(w io.Writer) {
	v := r.Verify
	fmt.Fprintf(w, "tried %s and put back %s", count(v.Applied, "hunk"), count(v.RolledBack, "file"))
	if len(v.NotRestored) > 0 {
		fmt.Fprintf(w, ", %s left alone", count(len(v.NotRestored), "file"))
	}
	switch {
	case v.Signal != nil:
		fmt.Fprintf(w, "; interrupted by %s, the command was ended (%.1fs)\n", signalName(v.Signal), v.Seconds)
	case v.TimedOut > 0:
		fmt.Fprintf(w, "; the command timed out after %s, exit %d\n", shortDuration(v.TimedOut), v.Status)
	default:
		fmt.Fprintf(w, "; the command exited %d (%.1fs)\n", v.Status, v.Seconds)
	}
	writeTranscript(w, v)
}

// writeStrays is the warning for each stray line, after a blank line when the
// report printed anything. patchName is the patch file under --each, where
// several patches share one report, and "" otherwise.
func writeStrays(w io.Writer, patchName string, strays []Stray, onDisk, gap bool) {
	for i, s := range strays {
		if i == 0 && gap {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "warning: %s\n", strayMessage(patchName, s, onDisk))
	}
}

// strayWhere is where a stray line is, as its warning and its refusal name it.
func strayWhere(patchName string, s Stray) string {
	if patchName != "" {
		return fmt.Sprintf("%s line %d", showControls(patchName), s.Line)
	}
	return fmt.Sprintf("patch line %d", s.Line)
}

// strayMessage is one stray line's warning, which --json carries as its
// message. onDisk says the batch is applied, so the line is in the file to be
// taken out of it; otherwise it is to come out of the patch.
func strayMessage(patchName string, s Stray, onDisk bool) string {
	where := strayWhere(patchName, s)
	what, them, are := "it", "it", "is"
	if s.After > 0 {
		what, them, are = "it and the "+count(s.After, "line")+" after it", "them", "are"
	}
	path := showControls(s.Path)
	fix := "take " + them + " out of the patch"
	if onDisk {
		fix = "remove " + them + " from " + path
	}
	// Quoted as it is: a CR left in Text is a byte of the line, in a patch
	// that is not CRLF, and shownLine would take it for the line's ending.
	msg := fmt.Sprintf("%s is %s, which looks like a mistyped heredoc delimiter; %s %s payload for %s, so if it was meant to end the heredoc, %s",
		where, quoteCut(string(s.Text)), what, are, path, fix)
	if s.After > 0 {
		msg += " and run any command they held, which the shell never did"
	}
	return msg
}

// allowMistypedFlag is the flag that applies a patch with stray lines, and
// warns of them, as every patch did in v0.3.15 and v0.3.16.
const allowMistypedFlag = "--allow-mistyped-delimiter"

// strayRefusal is one stray line's refusal (#97): the warning's sentence for a
// batch that was never applied, and the flag that applies it.
func strayRefusal(patchName string, s Stray) string {
	what, them, they := "it", "it", "it is"
	if s.After > 0 {
		what, them, they = "it and the "+count(s.After, "line")+" after it", "them", "they are"
	}
	msg := fmt.Sprintf("%s is %s, which looks like a mistyped heredoc delimiter; %s would be payload for %s, so nothing was written. If it was meant to end the heredoc, take %s out of the patch",
		strayWhere(patchName, s), quoteCut(string(s.Text)), what, showControls(s.Path), them)
	if s.After > 0 {
		msg += " and run any command they held, which the shell never did"
	}
	return msg + "; if " + they + " text, pass " + allowMistypedFlag
}

// MistypedDelimiters refuses a patch with lines that look like a mistyped
// heredoc delimiter (#97), every one of them, before anything is read: exit
// 1, as a patch that does not parse. Until 2026-10-18 the patch applied and a
// warning followed the report, which was after the line was in the file: in
// the field, sixteen times in 935 HUNK heredocs over three days, always the
// closer stuttered on the line above itself, and every one fixed by a second
// command. allowMistypedFlag applies it and warns, as before.
type MistypedDelimiters struct {
	Strays []Stray
}

// Error is one refusal a line, which the text report prefixes each with
// "hunk: " and --each shows under the patch's name.
func (e *MistypedDelimiters) Error() string {
	lines := make([]string, len(e.Strays))
	for i, s := range e.Strays {
		lines[i] = strayRefusal("", s)
	}
	return strings.Join(lines, "\n")
}

// tally is a writer that counts what passes through it, so that a block can
// tell whether anything came before it.
type tally struct {
	w io.Writer
	n int
}

func (t *tally) Write(b []byte) (int, error) {
	n, err := t.w.Write(b)
	t.n += n
	return n, err
}

// verifyEnded is how a failed verify ended, for the report's first line.
func verifyEnded(v *Verify) string {
	switch {
	case v.Signal != nil:
		return "interrupted by " + signalName(v.Signal) + " during the verify"
	case v.TimedOut > 0:
		return "verify timed out after " + shortDuration(v.TimedOut)
	}
	return "verify failed"
}

// cutNote is the line a report adds when hunk stopped reading a command's
// output because something it started still held the pipe after sh exited.
func cutNote() string {
	return "stopped reading " + shortDuration(verifyWaitDelay) +
		" after the command exited: something it started still holds its output open"
}

// signalName is the name a shell user knows a signal by: SIGINT, where Go's
// own String says "interrupt".
func signalName(sig os.Signal) string {
	switch sig {
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return sig.String()
}

// signalExit is 128 plus the signal, the status a shell gives a process the
// signal killed.
func signalExit(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 128 + int(syscall.SIGTERM)
}

// shortDuration prints a duration as somebody would type it: 2m, not 2m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// writeTranscript is the command, its output's tail, and what rolling back
// could not do, shared by a failed verify and --try.
func writeTranscript(w io.Writer, v *Verify) {
	writeCommandOutput(w, v)
	writeAftermath(w, v)
}

// writeCommandOutput is the command and its output's tail. --each prints it
// alone, under a patch whose command exited 0 or timed out, where the
// aftermath lines belong to the patch's own line.
func writeCommandOutput(w io.Writer, v *Verify) {
	fmt.Fprintf(w, "\n$ %s\n", v.Command)
	// The command's output is shown as a span line is, a control character by
	// name: printed raw until 2026-10-07, a build log could clear the screen or
	// set the title of the terminal reading the report.
	for _, l := range v.Tail {
		fmt.Fprintln(w, shown([]byte(l), false))
	}
	if v.TotalLines > 0 {
		fmt.Fprintf(w, "(last %d of %d lines)\n", len(v.Tail), v.TotalLines)
	}
	if v.Cut {
		fmt.Fprintf(w, "(%s)\n", cutNote())
	}
}

// writeAftermath is what the run left that was not the plan: files already
// gone, directories left, files not restored.
func writeAftermath(w io.Writer, v *Verify) {
	for _, p := range v.Gone {
		fmt.Fprintf(w, "\n%s, which hunk created, was already gone: something else removed it.\n", p)
	}
	for _, d := range v.Left {
		// The name is the tree's, through any link, so it is shown as a
		// destination is. Printed raw until 2026-10-08.
		fmt.Fprintf(w, "\n%s, which hunk made as a directory, %s, so hunk left it.\n", showControls(d.Path), d.State)
	}
	writeNotRestored(w, v.NotRestored)
}

func writeNotRestored(w io.Writer, notRestored []NotRestored) {
	for _, n := range notRestored {
		fmt.Fprintf(w, "\n%s was not restored.\n", withResolved(n.Path, n.Resolved))
		// A reason can name a directory on the path, which is the tree's
		// name too. Printed raw until 2026-10-08.
		for _, line := range strings.Split(n.Reason, "\n") {
			fmt.Fprintf(w, "  %s\n", showControls(line))
		}
		// Until 2026-10-20 this said they "are in version control", which no
		// run checks and which is false outside a repository (#106).
		fmt.Fprintln(w, "  hunk did not write its original bytes back, and keeps no copy of them.")
	}
}

// opLetter is §5.1's first column.
func opLetter(op string) string {
	switch op {
	case "create":
		return "A"
	case "delete":
		return "D"
	}
	return "M"
}

// Added and Removed are §5.1's totals.
func (r *Result) Added() int {
	n := 0
	for _, f := range r.Files {
		n += f.Added
	}
	return n
}

func (r *Result) Removed() int {
	n := 0
	for _, f := range r.Files {
		n += f.Removed
	}
	return n
}

// The JSON shapes are §5.1's and §5.2's, field for field. They are a published
// interface (§10), so the encoder follows them rather than tidying them.

type jsonReport struct {
	OK       bool          `json:"ok"`
	Exit     int           `json:"exit"`
	Files    []jsonFile    `json:"files,omitempty"`
	Hunks    int           `json:"hunks,omitempty"`
	Verify   *jsonVerify   `json:"verify,omitempty"`
	Try      *jsonTry      `json:"try,omitempty"`
	Each     *jsonEach     `json:"each,omitempty"`
	Failures []jsonFailure `json:"failures,omitempty"`
	Error    string        `json:"error,omitempty"`
	DryRun   bool          `json:"dry_run,omitempty"`
	// NotRestored is what a commit that failed part-way could not put back.
	// The verify's own list is inside Verify.
	NotRestored []jsonRestore `json:"not_restored,omitempty"`
	// Warnings are the replacements whose ends disagree about a newline
	// (#111) and those that changed nothing (#118), when the batch matched,
	// and the lines that look like a mistyped heredoc delimiter (#92),
	// whatever the outcome.
	Warnings []jsonWarning `json:"warnings,omitempty"`
}

// jsonWarning is one warning, and kind says which. A stray line,
// "mistyped_delimiter": where it is in the patch, the file its payload is
// for, the line, how many payload lines follow it, and the text report's
// sentence. A slip, "empty_line", "joined_line" or "no_final_newline": the
// hunk, its patch line, its file, the line the slip leaves, and the
// sentence. A hunk that changed nothing, "no_change": the same, with its old
// text's first line (#118). Patch names the patch file under --each alone,
// as a failure's does. Kind came in on 2026-10-23 with the slips; a warning
// from before then is a stray.
type jsonWarning struct {
	Kind      string `json:"kind"`
	Patch     string `json:"patch,omitempty"`
	Hunk      int    `json:"hunk,omitempty"`
	PatchLine int    `json:"patch_line"`
	Path      string `json:"path"`
	Text      string `json:"text"`
	// LinesAfter is a stray's alone, and is there when it is 0.
	LinesAfter *int   `json:"lines_after,omitempty"`
	Message    string `json:"message"`
}

// jsonWarnings is strays as --json carries them. refused says the patch was
// refused for them (#97), and each message is then its refusal.
func jsonWarnings(patchName string, strays []Stray, onDisk, refused bool) []jsonWarning {
	var out []jsonWarning
	for _, s := range strays {
		msg := strayMessage(patchName, s, onDisk)
		if refused {
			msg = strayRefusal(patchName, s)
		}
		after := s.After
		out = append(out, jsonWarning{
			Kind: "mistyped_delimiter", Patch: patchName, PatchLine: s.Line, Path: s.Path, Text: string(s.Text),
			LinesAfter: &after, Message: msg,
		})
	}
	return out
}

type jsonFile struct {
	Path           string `json:"path"`
	Resolved       string `json:"resolved,omitempty"`
	Op             string `json:"op"`
	Added          int    `json:"added"`
	Removed        int    `json:"removed"`
	SeamAdded      bool   `json:"added_final_newline,omitempty"`
	NoFinalNewline bool   `json:"no_final_newline,omitempty"`
}

// jsonTry is --try's result. It is there when --try was given, and ran says
// whether the command did; status is its exit, which the top-level exit
// repeats unless the tree could not all be put back (§4.1).
type jsonTry struct {
	Ran         bool          `json:"ran"`
	Status      int           `json:"status"`
	Seconds     float64       `json:"seconds"`
	Command     string        `json:"command"`
	Output      []string      `json:"output,omitempty"`
	RolledBack  int           `json:"rolled_back"`
	NotRestored []jsonRestore `json:"not_restored,omitempty"`
	Gone        []string      `json:"already_gone,omitempty"`
	Left        []jsonLeft    `json:"dirs_left,omitempty"`
	jsonEnded
}

// jsonEnded is how a command was ended early, in either object: the --timeout
// that did it in seconds, the signal to hunk that did it, and whether hunk
// stopped reading output that something the command started still held.
type jsonEnded struct {
	TimedOut    float64 `json:"timed_out_after,omitempty"`
	Interrupted string  `json:"interrupted_by,omitempty"`
	OutputCut   bool    `json:"output_cut,omitempty"`
}

func jsonLefts(left []LeftDir) []jsonLeft {
	var out []jsonLeft
	for _, d := range left {
		out = append(out, jsonLeft{d.Path, d.State})
	}
	return out
}

func endedJSON(v *Verify) jsonEnded {
	e := jsonEnded{TimedOut: v.TimedOut.Seconds(), OutputCut: v.Cut}
	if v.Signal != nil {
		e.Interrupted = signalName(v.Signal)
	}
	return e
}

// jsonEach is --each's run (§4.3): the command and one entry per patch, in
// the order they were given. build is --build's command, and baseline its run
// on the tree as it was before the first patch; try_baseline is the
// command's run there, after the build's (#90). baseline kept its name from
// v0.3.13, where it was the only one. baseline_after and try_baseline_after
// are the same pair once the last patch is back (#110), run only when a patch
// was caught and the run did not stop.
type jsonEach struct {
	Command          string          `json:"command"`
	Build            string          `json:"build,omitempty"`
	Baseline         *jsonEachRun    `json:"baseline,omitempty"`
	TryBaseline      *jsonEachRun    `json:"try_baseline,omitempty"`
	BaselineAfter    *jsonEachRun    `json:"baseline_after,omitempty"`
	TryBaselineAfter *jsonEachRun    `json:"try_baseline_after,omitempty"`
	Patches          []jsonEachPatch `json:"patches"`
}

// jsonEachRun is one --build run, or the command's run on the tree as it is.
// ok is false for a run that exited nonzero, timed out (status 124), was
// interrupted or could not start (ran false). A build's output is there when
// it ended by itself or by the timeout, since that is the compile error the
// patch's author reads; the command's is there always.
type jsonEachRun struct {
	Ran     bool     `json:"ran"`
	OK      bool     `json:"ok"`
	Status  int      `json:"status"`
	Seconds float64  `json:"seconds"`
	Output  []string `json:"output,omitempty"`
	jsonEnded
}

// jsonEachPatch is one patch of an --each run. ran says whether its command
// did; a patch a stop left untried has ran false and nothing else. status is
// a pointer because 0 is the most meaningful answer there is, a break the
// suite let through, and no answer at all for a patch never tried. hunks,
// added and removed are --dry-run's, where no command runs. output is the
// command's tail whenever it ran, a caught patch's included (#89). build is
// the patch's --build run; when its ok is false, ran is false, since the
// command never ran, and rolled_back and the rest are what the build's own
// putting back did.
type jsonEachPatch struct {
	Patch       string        `json:"patch"`
	Ran         bool          `json:"ran"`
	Status      *int          `json:"status,omitempty"`
	Seconds     float64       `json:"seconds,omitempty"`
	Build       *jsonEachRun  `json:"build,omitempty"`
	Hunks       int           `json:"hunks,omitempty"`
	Added       *int          `json:"added,omitempty"`
	Removed     *int          `json:"removed,omitempty"`
	Output      []string      `json:"output,omitempty"`
	RolledBack  int           `json:"rolled_back,omitempty"`
	NotRestored []jsonRestore `json:"not_restored,omitempty"`
	Gone        []string      `json:"already_gone,omitempty"`
	Left        []jsonLeft    `json:"dirs_left,omitempty"`
	Error       string        `json:"error,omitempty"`
	jsonEnded
}

type jsonVerify struct {
	Ran         bool          `json:"ran"`
	OK          bool          `json:"ok"`
	Seconds     float64       `json:"seconds"`
	Command     string        `json:"command,omitempty"`
	Output      []string      `json:"output,omitempty"`
	RolledBack  int           `json:"rolled_back,omitempty"`
	Kept        bool          `json:"kept,omitempty"`
	NotRestored []jsonRestore `json:"not_restored,omitempty"`
	Gone        []string      `json:"already_gone,omitempty"`
	Left        []jsonLeft    `json:"dirs_left,omitempty"`
	jsonEnded
}

// jsonLeft is a directory rollback left: its path, and what it is now, the
// same words the text report uses.
type jsonLeft struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

type jsonRestore struct {
	Path     string `json:"path"`
	Resolved string `json:"resolved,omitempty"`
	Reason   string `json:"reason"`
}

type jsonFailure struct {
	// Patch is the file the failing patch came from, under --each alone,
	// where several patches share one failures array (§4.3).
	Patch     string `json:"patch,omitempty"`
	Hunk      int    `json:"hunk"`
	Path      string `json:"path"`
	PatchLine int    `json:"patch_line"`
	Expected  int    `json:"expected,omitempty"`
	// Found is a pointer because zero is a meaningful answer for an evaluated
	// hunk and no answer at all for a skipped one. §5.2's skipped example
	// carries neither expected nor found, and omitempty on an int cannot tell
	// those two zeros apart.
	Found        *int   `json:"found,omitempty"`
	SkippedAfter int    `json:"skipped_after,omitempty"`
	Refusal      string `json:"refusal,omitempty"`
	// Lines sits beside expected and found, not inside near_miss. That is
	// §5.2's shape and §10 makes it an interface.
	Lines []int `json:"lines,omitempty"`
	// LineEndings is each listed copy's line endings, parallel to Lines, when
	// the count under --eol auto missed copies in the other one.
	LineEndings []string  `json:"line_endings,omitempty"`
	NearMiss    *jsonNear `json:"near_miss,omitempty"`
}

type jsonNear struct {
	Cause string `json:"cause,omitempty"`
	Line  int    `json:"line,omitempty"`
	Span  string `json:"span,omitempty"`
	// SpanLines is the span's whole length when --context cut it, so a
	// program can tell a cut span from the text (§7.2).
	SpanLines int    `json:"span_lines,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// AlsoAt is where every other span as near starts, capped as lines is,
	// and AlsoMore how many more there are.
	AlsoAt   []int `json:"also_at,omitempty"`
	AlsoMore int   `json:"also_more,omitempty"`
	// AlsoSame is how many of them hold the span's own bytes, so that pasted
	// alone it would match more than once.
	AlsoSame int `json:"also_same,omitempty"`
	Shifted  int `json:"shifted_by_hunks,omitempty"`
}

// JSON writes one object on stdout (§4). It wins over --quiet: a caller that
// asked for machine output asked for it on every path.
func (r *Report) JSON(w io.Writer) error {
	out := jsonReport{OK: r.Exit == exitOK, Exit: r.Exit, DryRun: r.DryRun}
	if r.Result != nil {
		out.Hunks = r.Result.Hunks
		for _, f := range r.Result.Files {
			out.Files = append(out.Files, jsonFile{f.Path, f.Resolved, f.Op, f.Added, f.Removed, f.SeamAdded, f.NoFinalNewline})
		}
	}
	if v := r.Verify; v != nil && v.Try {
		jt := &jsonTry{
			Ran: v.Ran, Status: v.Status, Seconds: v.Seconds, Command: v.Command,
			Output: v.Tail, RolledBack: v.RolledBack, Gone: v.Gone, jsonEnded: endedJSON(v),
		}
		for _, n := range v.NotRestored {
			jt.NotRestored = append(jt.NotRestored, jsonRestore{n.Path, n.Resolved, n.Reason})
		}
		jt.Left = jsonLefts(v.Left)
		out.Try = jt
	} else if v != nil {
		jv := &jsonVerify{
			Ran: v.Ran, OK: v.OK, Seconds: v.Seconds, Command: v.Command,
			Output: v.Tail, RolledBack: v.RolledBack, Kept: v.Kept, Gone: v.Gone,
			jsonEnded: endedJSON(v),
		}
		for _, n := range v.NotRestored {
			jv.NotRestored = append(jv.NotRestored, jsonRestore{n.Path, n.Resolved, n.Reason})
		}
		jv.Left = jsonLefts(v.Left)
		out.Verify = jv
	}
	for _, f := range r.Failures {
		out.Failures = append(out.Failures, jsonFailureOf(f))
	}
	if r.Exit != exitOK && len(r.Failures) == 0 && r.Err != nil {
		out.Error = r.Err.Error()
		var ce *CommitError
		if errors.As(r.Err, &ce) {
			for _, n := range ce.NotRestored {
				out.NotRestored = append(out.NotRestored, jsonRestore{n.Path, n.Resolved, n.Reason})
			}
		}
	}
	if r.Result != nil {
		out.Warnings = slipWarnings("", r.Result.Slips, r.changedTheTree())
	}
	var md *MistypedDelimiters
	out.Warnings = append(out.Warnings, jsonWarnings("", r.Strays, r.changedTheTree(), errors.As(r.Err, &md))...)
	return encodeReport(w, out)
}

// jsonFailureOf is one failure's §5.2 object, shared by the batch's report
// and --each's preflight, which adds the patch's name to each.
func jsonFailureOf(f Failure) jsonFailure {
	jf := jsonFailure{
		Hunk: f.Hunk, Path: f.Path, PatchLine: f.PatchLine,
		Expected: f.Expected, SkippedAfter: f.SkippedAfter, Refusal: f.Refusal,
	}
	if !f.Skipped() && f.Refusal == "" {
		found := f.Found
		jf.Found = &found
	}
	if d := f.Near; d != nil {
		switch d.Kind {
		case DiagTooMany:
			jf.Lines = d.Lines
		case DiagOverlap, DiagMixedEndings, DiagSplitCRLF, DiagJoinedCRLF:
			// The lines where copies are, as for too many, and the cause
			// and the remedy, since found alone does not say why a count
			// that equals the claim was refused.
			jf.Lines, jf.LineEndings = d.Lines, d.Endings
			jf.NearMiss = &jsonNear{Cause: d.causeName(), Detail: d.Detail, Shifted: d.Shifted}
		case DiagNoAnchor:
			jf.NearMiss = &jsonNear{Cause: d.causeName()}
		default:
			jf.NearMiss = &jsonNear{
				Cause: d.causeName(), Line: d.Line,
				Span: string(joinLines(d.Span)), SpanLines: d.SpanLines, Detail: d.Detail,
				AlsoAt: d.Also, AlsoMore: d.AlsoMore, AlsoSame: d.AlsoSame, Shifted: d.Shifted,
			}
		}
	}
	return jf
}

// encodeReport writes one report object to w, shared by the batch's report
// and --each's.
func encodeReport(w io.Writer, out jsonReport) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// Go escapes &, < and > as \\u00xx by default, for embedding in HTML. This
	// output is read by a program, and a --verify command of "go build && go
	// test" coming back as "go build \\u0026\\u0026 go test" is noise an agent
	// has to see through.
	enc.SetEscapeHTML(false)
	// Into memory, of strings, numbers, booleans and slices of them, which
	// always encode; the write to w is the one that can fail.
	_ = enc.Encode(out)
	_, err := w.Write(escapeC1(buf.Bytes()))
	return err
}

// escapeC1 escapes the C1 control characters, U+0080 to U+009F, which
// encoding/json leaves raw while it escapes C0. A terminal printing the report
// can read U+009B as the start of a control sequence, and until 2026-10-07 a
// span or a command's output holding one carried it out raw. Everything
// outside a JSON string is ASCII, so each one found is inside a string.
func escapeC1(b []byte) []byte {
	if !bytes.Contains(b, []byte{0xc2}) {
		return b
	}
	var out bytes.Buffer
	for i := 0; i < len(b); i++ {
		if b[i] == 0xc2 && i+1 < len(b) && b[i+1] >= 0x80 && b[i+1] <= 0x9f {
			fmt.Fprintf(&out, `\u%04x`, b[i+1])
			i++
			continue
		}
		out.WriteByte(b[i])
	}
	return out.Bytes()
}

func joinLines(lines [][]byte) []byte {
	return []byte(strings.Join(byteLinesToStrings(lines), "\n"))
}

func byteLinesToStrings(lines [][]byte) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l)
	}
	return out
}
