package main

// --each: one --try command over several patches, one at a time (§4.3).
//
// A deliberate-break run applies each of a set of small patches alone against
// the test suite, to see that the suite notices each one. Before this mode it
// was a hand-rolled shell loop reading --try's exit, which conflates on
// purpose: a patch that does not parse exits 1 as the ordinary catch does, a
// command that exits 2 reads as "did not match", 124 as a timeout, and an
// exit-4 report's first line says "the command exited 0", which the most
// careful loop in the field scored as a missed mutant before carrying on
// against a broken tree.
//
// So the contract here is the loop done right, once:
//
//   - every patch is read, parsed and validated before the first try, and one
//     that fails refuses the lot, so no patch is ever scored by an exit that
//     was really hunk's;
//   - each patch is applied alone, the command run, and every file put back,
//     exactly as one --try does, with --timeout per command;
//   - one line per patch goes to stderr as it finishes, so a run the harness
//     kills still shows every patch that completed;
//   - the run stops at once where carrying on would lie: a file not put back,
//     a patch refused after the preflight passed, a command that cannot
//     start, a signal;
//   - the exit is the run's, never a command's: 0 when every patch was tried
//     and put back and every command exited nonzero, 3 when a command exited
//     0 or timed out (as a verify whose verdict went against the caller), and
//     the single-run codes where the run stopped.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"
	"unicode/utf8"
)

// eachConfig is everything invoke decided before handing over: the flags, the
// opened tree, and where the report goes.
type eachConfig struct {
	names        []string
	command      string
	marker       string
	opt          Options
	timeout      time.Duration
	verifyLines  int
	verifyFormat bool
	dryRun       bool
	asJSON       bool
	quiet        bool
}

// eachPatch is one patch of the run: the file as the caller spelled it, its
// parsed form, and what happened to it.
type eachPatch struct {
	name string
	p    *Patch
	// loadErr is a read or parse failure, found before anything is validated.
	loadErr error
	// failures is the preflight's refusal, or a mid-run one, either of which
	// stops everything.
	failures []Failure
	// res is the preflight's diffstat, which is the whole result of --dry-run.
	res *Result
	// v is the try's outcome, nil until this patch's turn came.
	v *Verify
}

// runEach is the mode: preflight, then the loop, then one summary. It returns
// the exit code and, through interrupted, the signal main should die of.
func runEach(cfg eachConfig, tree *Tree, stdout, stderr io.Writer, interrupted *os.Signal) int {
	patches := make([]*eachPatch, len(cfg.names))
	for i, name := range cfg.names {
		patches[i] = &eachPatch{name: name}
		src, err := os.ReadFile(name)
		if err != nil {
			patches[i].loadErr = err
			continue
		}
		p, err := Parse(src, cfg.marker)
		if err != nil {
			patches[i].loadErr = err
			continue
		}
		patches[i].p = p
	}
	if bad := countLoadErrs(patches); bad > 0 {
		return eachRefused(cfg, patches, "could not be read", exitUsage, stdout, stderr)
	}

	// The preflight: every patch validated against the tree before the first
	// try, each on a fresh transaction, as its own run would be. A failure
	// here refuses the lot: the typo in patch 19 is found before the first
	// command runs, and a patch that failed the preflight can never be scored
	// by a command's exit.
	code := exitNoMatch
	bad := 0
	for _, ep := range patches {
		res, err := NewTxn(tree, cfg.opt).Preview(ep.p)
		if err == nil {
			ep.res = res
			continue
		}
		bad++
		var ve *ValidationError
		if errors.As(err, &ve) {
			ep.failures = ve.Failures
		} else {
			ep.loadErr = err
			// A path refusal is a validation failure (§6.1); anything else
			// here is I/O, and 5 outranks 2 when both are in the list.
			if ExitCode(err) == exitIO {
				code = exitIO
			}
		}
	}
	if bad > 0 {
		// The wording tracks the exit one to one, which is what lets a reader,
		// scripts/corpus among them, read the code off the text: "could not
		// be read" is 1, "would not apply" is 2, "could not be checked" is 5.
		what := "would not apply"
		if code == exitIO {
			what = "could not be checked"
		}
		return eachRefused(cfg, patches, what, code, stdout, stderr)
	}

	if cfg.dryRun {
		return eachDryRun(cfg, patches, stdout)
	}
	return eachRun(cfg, patches, tree, stdout, stderr, interrupted)
}

// patchCount is count for a word whose plural is not an s: "1 patch",
// "2 patches".
func patchCount(n int) string {
	if n == 1 {
		return "1 patch"
	}
	return fmt.Sprintf("%d patches", n)
}

func countLoadErrs(patches []*eachPatch) int {
	n := 0
	for _, ep := range patches {
		if ep.loadErr != nil {
			n++
		}
	}
	return n
}

// eachRefused reports a preflight that failed: every failing patch in full,
// under its name, and nothing tried.
func eachRefused(cfg eachConfig, patches []*eachPatch, what string, code int, stdout, stderr io.Writer) int {
	if cfg.asJSON {
		return eachJSON(cfg, patches, code, "", stdout, stderr)
	}
	bad := 0
	for _, ep := range patches {
		if ep.loadErr != nil || len(ep.failures) > 0 {
			bad++
		}
	}
	if len(patches) > bad {
		fmt.Fprintf(stderr, "hunk: %d of %s %s; nothing was tried\n", bad, patchCount(len(patches)), what)
	} else {
		fmt.Fprintf(stderr, "hunk: %s %s; nothing was tried\n", patchCount(bad), what)
	}
	for _, ep := range patches {
		switch {
		case ep.loadErr != nil:
			fmt.Fprintf(stderr, "\n%s: %v\n", showControls(ep.name), ep.loadErr)
		case len(ep.failures) > 0:
			fmt.Fprintf(stderr, "\n%s:", showControls(ep.name))
			writeFailureBlocks(stderr, ep.failures)
		}
	}
	return code
}

// eachDryRun is the preflight alone: one line per patch with its diffstat, as
// --dry-run prints nothing but what would happen.
func eachDryRun(cfg eachConfig, patches []*eachPatch, stdout io.Writer) int {
	if cfg.asJSON {
		return eachJSON(cfg, patches, exitOK, "", stdout, io.Discard)
	}
	if cfg.quiet {
		return exitOK
	}
	width := eachNameWidth(patches)
	hunks, added, removed := 0, 0, 0
	for _, ep := range patches {
		fmt.Fprintf(stdout, "%-*s  %s, +%d -%d\n",
			width, showControls(ep.name), count(ep.res.Hunks, "hunk"), ep.res.Added(), ep.res.Removed())
		hunks += ep.res.Hunks
		added += ep.res.Added()
		removed += ep.res.Removed()
	}
	fmt.Fprintf(stdout, "%s would apply, %s, +%d -%d (dry run: nothing written, command not run)\n",
		patchCount(len(patches)), count(hunks, "hunk"), added, removed)
	return exitOK
}

// eachRun is the loop: each patch applied alone, the command run, every file
// put back, a line per patch to stderr as it finishes.
func eachRun(cfg eachConfig, patches []*eachPatch, tree *Tree, stdout, stderr io.Writer, interrupted *os.Signal) int {
	// One channel for the whole run: the handler goes in at the first patch's
	// commit and stays, so a signal between patches is seen too, rather than
	// killing hunk between two consistent states and reading as a crash.
	sigs := make(chan os.Signal, 1)
	defer signal.Stop(sigs)
	stop := Stop{Timeout: cfg.timeout, Signals: sigs}
	width := eachNameWidth(patches)
	start := time.Now()

	tried, zeros, timeouts := 0, 0, 0
	stopAt := -1      // index of the patch the run stopped at, -1 for none
	var stopErr error // a mid-run refusal or I/O failure, reported before the summary
	var sig os.Signal // the signal that ended the run, re-raised by main

	for i, ep := range patches {
		if s := pending(sigs); s != nil {
			// Between patches: the tree is whole and nothing is running.
			sig = s
			stopAt = i
			break
		}
		txn := NewTxn(tree, cfg.opt)
		txn.beforeCommit = func() { notifyInterrupts(sigs) }
		if _, err := txn.Run(ep.p); err != nil {
			// The preflight matched every patch, so a refusal here means the
			// tree changed under the run: by the command, or by another
			// writer. Carrying on would score every later patch against a
			// tree nobody validated.
			stopAt, stopErr = i, err
			var ve *ValidationError
			if errors.As(err, &ve) {
				ep.failures = ve.Failures
			}
			break
		}
		var v *Verify
		if s := pending(sigs); s != nil {
			// The signal came while the batch was being written: commit
			// finished, and the command never starts, as one --try has it.
			// The sentence is the per-patch line's; the signal is the exit.
			v, _ = interruptedBeforeTheCommand(txn, "", cfg.command, s, cfg.verifyFormat)
			ep.v = v
		} else {
			var tryErr error
			v, tryErr = runTry(txn, tree, cfg.command, cfg.verifyLines, cfg.verifyFormat, stop)
			ep.v = v
			if tryErr != nil {
				// The command could not start. The batch is back (or the
				// line says what is not), and a shell that lost sh mid-run
				// will not find it for the next patch either.
				stopAt, stopErr = i, tryErr
				writeEachLine(cfg, stderr, width, ep)
				break
			}
		}
		writeEachLine(cfg, stderr, width, ep)
		if v.Signal != nil {
			sig = v.Signal
			stopAt = i
			break
		}
		if len(v.NotRestored) > 0 {
			stopAt = i
			break
		}
		tried++
		switch {
		case v.TimedOut > 0:
			timeouts++
		case v.Status == 0:
			zeros++
		}
	}
	o := eachOutcome{
		tried: tried, zeros: zeros, timeouts: timeouts,
		stopAt: stopAt, stopErr: stopErr, sig: sig,
		seconds: time.Since(start).Seconds(),
	}
	code := eachExit(patches, o)
	if sig != nil && code != exitRollbackFailed && interrupted != nil {
		*interrupted = sig
	}
	if cfg.asJSON {
		return eachJSON(cfg, patches, code, eachStopReason(patches, stopAt, sig, stopErr), stdout, stderr)
	}
	writeEachEnd(cfg, patches, o, code, stdout, stderr)
	return code
}

// eachOutcome is how the loop ended, for the exit and the summary.
type eachOutcome struct {
	tried, zeros, timeouts int
	stopAt                 int   // index of the patch the run stopped at, -1 for none
	stopErr                error // a mid-run refusal or I/O failure
	sig                    os.Signal
	seconds                float64
}

// eachExit is §4.3's exit: the run's own, never a command's.
func eachExit(patches []*eachPatch, o eachOutcome) int {
	switch {
	case o.stopAt >= 0 && patches[o.stopAt].v != nil && len(patches[o.stopAt].v.NotRestored) > 0:
		return exitRollbackFailed
	case o.sig != nil:
		return signalExit(o.sig)
	case o.stopErr != nil:
		return ExitCode(o.stopErr)
	case o.zeros > 0 || o.timeouts > 0:
		// As a verify that went against the caller exits: the tree is back in
		// place, and the verdict is the news (§4.3).
		return exitVerifyFailed
	}
	return exitOK
}

// writeEachEnd is the end of the run: the stop's own story, then the summary,
// which is the report.
func writeEachEnd(cfg eachConfig, patches []*eachPatch, o eachOutcome, code int, stdout, stderr io.Writer) {
	// The stop's story first: the refusal in full, or what a failed commit
	// could not put back, each of which the loop could not print.
	if o.stopAt >= 0 {
		ep := patches[o.stopAt]
		switch {
		case len(ep.failures) > 0:
			fmt.Fprintf(stderr, "hunk: %s no longer applies; the preflight matched every patch, so the tree has changed since the run began: by the command, or by another writer\n", showControls(ep.name))
			writeFailureBlocks(stderr, ep.failures)
		case o.stopErr != nil:
			// A per-patch line said what the try did; this is why the run
			// stopped. A failed --try's own not-restored block was the line's.
			fmt.Fprintf(stderr, "hunk: at %s: %v\n", showControls(ep.name), o.stopErr)
			var ce *CommitError
			if errors.As(o.stopErr, &ce) {
				writeNotRestored(stderr, ce.NotRestored)
			}
		}
		if notTried := eachNotTried(patches); len(notTried) > 0 {
			fmt.Fprintf(stderr, "not tried: %s\n", strings.Join(notTried, ", "))
		}
	}

	// The summary: stdout when the run is clean, as any success is, stderr
	// otherwise.
	w := stderr
	if code == exitOK {
		if cfg.quiet {
			return
		}
		w = stdout
	}
	switch {
	case o.stopAt >= 0:
		fmt.Fprintf(w, "tried %d of %s; %s", o.tried, patchCount(len(patches)), eachStopClause(patches, o.stopAt, o.sig, o.stopErr))
		if n := len(eachNotTried(patches)); n > 0 {
			fmt.Fprintf(w, "; %s not tried", patchCount(n))
		}
		fmt.Fprintln(w)
	default:
		fmt.Fprintf(w, "tried %s and put back every file; %s (%.1fs)\n",
			patchCount(o.tried), eachVerdict(o.tried, o.zeros, o.timeouts), o.seconds)
	}
}

// writeEachLine is one patch's line, to stderr the moment it finished, plus
// the command's output where it is the thing to read: a command that exited 0
// or timed out is the one to investigate, and under --quiet those lines are
// all that print.
func writeEachLine(cfg eachConfig, w io.Writer, width int, ep *eachPatch) {
	v := ep.v
	actionable := v.Signal != nil || v.TimedOut > 0 || (v.Ran && v.Status == 0) ||
		len(v.NotRestored) > 0 || len(v.Gone) > 0 || len(v.Left) > 0 || !v.Ran
	if cfg.quiet && !actionable {
		return
	}
	fmt.Fprintf(w, "tried %-*s  ", width, showControls(ep.name))
	switch {
	case v.Signal != nil && !v.Ran:
		fmt.Fprintf(w, "interrupted by %s before the command ran", signalName(v.Signal))
	case v.Signal != nil:
		fmt.Fprintf(w, "interrupted by %s, the command was ended (%.1fs)", signalName(v.Signal), v.Seconds)
	case !v.Ran:
		fmt.Fprint(w, "the command could not start")
	case v.TimedOut > 0:
		fmt.Fprintf(w, "the command timed out after %s", shortDuration(v.TimedOut))
	default:
		fmt.Fprintf(w, "the command exited %d (%.1fs)", v.Status, v.Seconds)
	}
	if len(v.NotRestored) > 0 {
		fmt.Fprintf(w, "; %s left alone", count(len(v.NotRestored), "file"))
	}
	fmt.Fprintln(w)
	if v.Ran && (v.TimedOut > 0 || v.Status == 0) {
		writeCommandOutput(w, v)
	}
	writeAftermath(w, v)
}

// eachStopClause is the summary's reason for stopping, after the full story
// has been told above it.
func eachStopClause(patches []*eachPatch, stopAt int, sig os.Signal, stopErr error) string {
	ep := patches[stopAt]
	name := showControls(ep.name)
	switch {
	case ep.v != nil && len(ep.v.NotRestored) > 0:
		if n := len(ep.v.NotRestored); n > 1 {
			return fmt.Sprintf("stopped at %s: %d files were not restored", name, n)
		}
		return fmt.Sprintf("stopped at %s: 1 file was not restored", name)
	case sig != nil && ep.v == nil:
		return "interrupted by " + signalName(sig)
	case sig != nil:
		return fmt.Sprintf("interrupted by %s during %s", signalName(sig), name)
	default:
		return "stopped at " + name
	}
}

// eachStopReason is the one-line form of the stop for --json, where the text
// above would have told the story.
func eachStopReason(patches []*eachPatch, stopAt int, sig os.Signal, stopErr error) string {
	if stopAt < 0 {
		return ""
	}
	ep := patches[stopAt]
	switch {
	case len(ep.failures) > 0:
		return fmt.Sprintf("%s no longer applies; the preflight matched every patch, so the tree has changed since the run began", ep.name)
	case stopErr != nil:
		return fmt.Sprintf("at %s: %v", ep.name, stopErr)
	}
	return eachStopClause(patches, stopAt, sig, stopErr)
}

// eachVerdict is the summary's middle: what the commands said.
func eachVerdict(tried, zeros, timeouts int) string {
	if zeros == 0 && timeouts == 0 {
		return "every command exited nonzero"
	}
	parts := make([]string, 0, 3)
	if n := tried - zeros - timeouts; n > 0 {
		parts = append(parts, fmt.Sprintf("%d exited nonzero", n))
	}
	if zeros > 0 {
		parts = append(parts, fmt.Sprintf("%d exited 0", zeros))
	}
	if timeouts > 0 {
		parts = append(parts, fmt.Sprintf("%d timed out", timeouts))
	}
	return strings.Join(parts, ", ")
}

// eachNotTried is every patch the stop left untried, in order, shown as a
// path is.
func eachNotTried(patches []*eachPatch) []string {
	var names []string
	for _, ep := range patches {
		if ep.v == nil && len(ep.failures) == 0 && ep.loadErr == nil {
			names = append(names, showControls(ep.name))
		}
	}
	return names
}

// eachNameWidth pads the per-patch lines to the longest name, in runes, as
// the success report pads its paths (§5.1).
func eachNameWidth(patches []*eachPatch) int {
	width := 0
	for _, ep := range patches {
		width = max(width, utf8.RuneCountInString(showControls(ep.name)))
	}
	return width
}

// eachJSON is the whole run as one object, alone on stdout: the command and
// one entry per patch, with the preflight's failures carrying each patch's
// name. The streamed per-patch lines were stderr's.
func eachJSON(cfg eachConfig, patches []*eachPatch, code int, stopReason string, stdout, stderr io.Writer) int {
	out := jsonReport{OK: code == exitOK, Exit: code, DryRun: cfg.dryRun}
	je := &jsonEach{Command: cfg.command}
	if stopReason != "" {
		out.Error = stopReason
	}
	for _, ep := range patches {
		jp := jsonEachPatch{Patch: ep.name}
		switch {
		case ep.loadErr != nil:
			jp.Error = ep.loadErr.Error()
		case len(ep.failures) > 0:
			for _, f := range ep.failures {
				jf := jsonFailureOf(f)
				jf.Patch = ep.name
				out.Failures = append(out.Failures, jf)
			}
		case cfg.dryRun && ep.res != nil:
			added, removed := ep.res.Added(), ep.res.Removed()
			jp.Hunks, jp.Added, jp.Removed = ep.res.Hunks, &added, &removed
		case ep.v != nil:
			v := ep.v
			status := v.Status
			jp.Ran, jp.Status, jp.Seconds = v.Ran, &status, v.Seconds
			jp.RolledBack, jp.Gone = v.RolledBack, v.Gone
			jp.Left = jsonLefts(v.Left)
			jp.jsonEnded = endedJSON(v)
			if v.Ran && (v.TimedOut > 0 || v.Status == 0) {
				jp.Output = v.Tail
			}
			for _, n := range v.NotRestored {
				jp.NotRestored = append(jp.NotRestored, jsonRestore{n.Path, n.Resolved, n.Reason})
			}
		}
		je.Patches = append(je.Patches, jp)
	}
	out.Each = je
	if err := encodeReport(stdout, out); err != nil {
		if code == exitRollbackFailed {
			fmt.Fprintf(stderr, "hunk: a file was not restored, and the --json report could not be written: %v\n", err)
		} else {
			fmt.Fprintf(stderr, "hunk: the --json report could not be written: %v\n", err)
		}
		return exitIO
	}
	return code
}
