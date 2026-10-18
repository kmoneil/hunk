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
//
// --build, from issue #86, is the same false catch one level down: a break
// that does not compile makes the test command exit nonzero, and Zig and Go
// exit 1 for a compile error as for a failing test. So a build command runs
// between each patch and its command, and a patch whose build fails is its
// own line and count, its command not run. It runs once on the tree as it is
// first, so that "did not build" means the patch broke the build.
//
// The command runs once on the tree as it is too, from issue #90, and for the
// same reason: a command that fails with no patch applied fails under every
// patch, and every line would read as a catch at exit 0, the strongest result
// this mode has, from a run that tested nothing. A test filter that matches no
// test, an exit taken from the wrong end of a pipe, a cd from the wrong
// directory: each is a careful caller's mistake, and each was in the field.
//
// A caught patch's output prints only when --verify-lines is given (#89): a
// passing suite's forty lines under every catch would bury the misses, and
// the caller who wants to know what caught each asks for the lines that say.

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
	build        string // --build, empty for none
	marker       string
	opt          Options
	timeout      time.Duration
	verifyLines  int
	everyTail    bool // --verify-lines given, not defaulted: every command's tail prints (#89)
	verifyFormat bool
	dryRun       bool
	asJSON       bool
	quiet        bool
	allowStrays  bool // --allow-mistyped-delimiter: warn of strays (#92) rather than refuse (#97)
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
	// v is the try's outcome, nil until this patch's turn came, and nil when
	// its build failed, since then the command never ran.
	v *Verify
	// build is the --build command's run for this patch, nil without --build
	// or before the patch's turn. When it failed, its rollback is the one that
	// put the files back.
	build *Verify
}

// outcome is the run that put this patch's files back: the command's, or the
// build's when the command never ran. Nil for a patch the run did not reach.
func (ep *eachPatch) outcome() *Verify {
	if ep.v != nil {
		return ep.v
	}
	return ep.build
}

// runEach is the mode: preflight, then the loop, then one summary. It returns
// the exit code and, through interrupted, the signal main should die of.
//
// A patch with lines that look like a mistyped delimiter is refused with the
// patches that do not parse (#97), so nothing is tried. Under
// --allow-mistyped-delimiter they are warned of (#92) after all of it,
// whatever the run's end, each under its patch's name, and always as lines to
// come out of the patch, since --each puts every file back.
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
		if len(p.Strays) > 0 && !cfg.allowStrays {
			patches[i].loadErr = &MistypedDelimiters{Strays: p.Strays}
			continue
		}
		patches[i].p = p
	}
	o, e := &tally{w: stdout}, &tally{w: stderr}
	code := eachLoaded(cfg, patches, tree, o, e, interrupted)
	if !cfg.asJSON {
		gap := o.n+e.n > 0
		for _, ep := range patches {
			if ep.p != nil && len(ep.p.Strays) > 0 {
				writeStrays(stderr, ep.name, ep.p.Strays, false, gap)
				gap = false
			}
		}
	}
	return code
}

// eachLoaded is the run once every patch has been read and parsed.
func eachLoaded(cfg eachConfig, patches []*eachPatch, tree *Tree, stdout, stderr io.Writer, interrupted *os.Signal) int {
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
		return eachJSON(cfg, patches, eachBaselines{}, code, "", stdout, stderr)
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
		return eachJSON(cfg, patches, eachBaselines{}, exitOK, "", stdout, io.Discard)
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
	ran := "command"
	if cfg.build != "" {
		ran = "build and command"
	}
	fmt.Fprintf(stdout, "%s would apply, %s, +%d -%d (dry run: nothing written, %s not run)\n",
		patchCount(len(patches)), count(hunks, "hunk"), added, removed, ran)
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

	tried, zeros, timeouts, notBuilt := 0, 0, 0, 0
	stopAt := -1      // index of the patch the run stopped at, -1 for none
	var stopErr error // a mid-run refusal or I/O failure, reported before the summary
	var sig os.Signal // the signal that ended the run, re-raised by main
	todo := patches

	// The handler goes in before anything runs: every command runs in its
	// own process group, and would run on after a Ctrl-C that killed hunk.
	notifyInterrupts(sigs)
	var bases eachBaselines
	if cfg.build != "" {
		// The build on the tree as it is, before any patch: one that fails
		// here fails under every patch, and "did not build" would be true of
		// each line and false about each patch.
		b, err := runOnTheTree(cfg.build, cfg, tree, stop)
		if err != nil {
			return eachBaselineRefused(cfg, patches, bases, nil,
				fmt.Sprintf("could not run the --build command: %v; nothing was tried", err), exitIO, stdout, stderr)
		}
		bases.build = b
		switch {
		case b.Signal != nil:
			sig, stopAt, todo = b.Signal, 0, nil
		case !b.OK:
			return eachBaselineRefused(cfg, patches, bases, b, baselineFailure("the build", b, "not built"), exitUsage, stdout, stderr)
		}
	}
	if sig == nil {
		// The command on the tree as it is, after the build's, which is the
		// more precise refusal when the tree does not build: one that fails
		// here, every patch would read as caught.
		c, err := runOnTheTree(cfg.command, cfg, tree, stop)
		if err != nil {
			return eachBaselineRefused(cfg, patches, bases, nil,
				fmt.Sprintf("could not run the --try command: %v; nothing was tried", err), exitIO, stdout, stderr)
		}
		bases.command = c
		switch {
		case c.Signal != nil:
			sig, stopAt, todo = c.Signal, 0, nil
		case c.TimedOut > 0:
			return eachBaselineRefused(cfg, patches, bases, c, baselineFailure("the command", c, "timed out"), exitUsage, stdout, stderr)
		case !c.OK:
			return eachBaselineRefused(cfg, patches, bases, c, baselineFailure("the command", c, "caught"), exitUsage, stdout, stderr)
		}
	}

	for i, ep := range todo {
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
		tryErr := tryOne(cfg, ep, txn, tree, sigs, stop)
		writeEachLine(cfg, stderr, width, ep)
		if tryErr != nil {
			// The command or the build could not start. The batch is back
			// (or the line says what is not), and a shell that lost sh
			// mid-run will not find it for the next patch either.
			stopAt, stopErr = i, tryErr
			break
		}
		v := ep.outcome()
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
		case ep.v == nil:
			notBuilt++
		case v.TimedOut > 0:
			timeouts++
		case v.Status == 0:
			zeros++
		}
	}
	o := eachOutcome{
		tried: tried, zeros: zeros, timeouts: timeouts, notBuilt: notBuilt,
		stopAt: stopAt, stopErr: stopErr, sig: sig,
		seconds: time.Since(start).Seconds(),
	}
	code := eachExit(patches, o)
	if sig != nil && code != exitRollbackFailed && interrupted != nil {
		*interrupted = sig
	}
	if cfg.asJSON {
		return eachJSON(cfg, patches, bases, code, eachStopReason(patches, stopAt, sig, stopErr), stdout, stderr)
	}
	writeEachEnd(cfg, patches, o, code, stdout, stderr)
	return code
}

// tryOne is one patch's turn once its batch is on disk: the build where there
// is one, then the command, unless the build failed or a signal came first.
// Whatever ran last put the files back, and ep.outcome() is that run. The
// error is a build or a command that could not start.
func tryOne(cfg eachConfig, ep *eachPatch, txn *Txn, tree *Tree, sigs chan os.Signal, stop Stop) error {
	if s := pending(sigs); s != nil {
		// The signal came while the batch was being written: commit
		// finished, and nothing runs, as one --try has it. The sentence is
		// the per-patch line's; the signal is the exit.
		ep.v, _ = interruptedBeforeTheCommand(txn, "", cfg.command, s, cfg.verifyFormat)
		return nil
	}
	if cfg.build != "" {
		b, err := runBuild(cfg, txn, tree, stop)
		ep.build = b
		if err != nil || !b.OK {
			return err
		}
		if s := pending(sigs); s != nil {
			// The build had exited 0 when the signal came: the command never
			// starts.
			ep.v, _ = interruptedBeforeTheCommand(txn, "", cfg.command, s, cfg.verifyFormat)
			return nil
		}
	}
	v, err := runTry(txn, tree, cfg.command, cfg.verifyLines, cfg.verifyFormat, stop)
	ep.v = v
	return err
}

// runOnTheTree runs command once, from the root, under the run's timeout and
// signals: --build, or either baseline. A timeout is 124 in Status, as
// --try's is.
func runOnTheTree(command string, cfg eachConfig, tree *Tree, stop Stop) (*Verify, error) {
	b, err := RunVerify(command, tree.Root(), cfg.verifyLines, stop)
	if err != nil {
		return nil, err
	}
	if b.TimedOut > 0 {
		b.Status = exitTimedOut
	}
	return b, nil
}

// runBuild is --build with one patch on disk. A build that exits 0 leaves the
// batch for the command; any other end puts it back, since the command will
// not run. One that cannot start puts it back too, and is exit 5 through the
// error, as a command that cannot start is.
func runBuild(cfg eachConfig, txn *Txn, tree *Tree, stop Stop) (*Verify, error) {
	b, err := runOnTheTree(cfg.build, cfg, tree, stop)
	if err != nil {
		b = &Verify{Command: cfg.build, Applied: txn.Applied()}
		return b, cannotStart("the --build command", "put back", err, b, txn, cfg.verifyFormat)
	}
	b.Applied = txn.Applied()
	if !b.OK {
		b.RolledBack, b.NotRestored, b.Gone, b.Left = txn.Rollback(cfg.verifyFormat)
	}
	return b, nil
}

// eachBaselines is the runs on the tree as it is, before the first patch:
// --build's where there is one, then the command's. Either is nil where it
// did not run.
type eachBaselines struct {
	build, command *Verify
}

// baselineFailure is why a run on the tree as it is refused the run: who ran,
// how it ended, and what every patch would have read as.
func baselineFailure(who string, b *Verify, reads string) string {
	what := fmt.Sprintf("exited %d", b.Status)
	if b.TimedOut > 0 {
		what = "timed out after " + shortDuration(b.TimedOut)
	}
	return who + " " + what + " with no patch applied, so every patch would read as " + reads + "; nothing was tried"
}

// eachBaselineRefused reports a run refused before its first patch by a run
// on the tree as it is: the reason, and the failed run's output where it ran,
// which is what the caller fixes the command or the tree from.
func eachBaselineRefused(cfg eachConfig, patches []*eachPatch, bases eachBaselines, failed *Verify, why string, code int, stdout, stderr io.Writer) int {
	if cfg.asJSON {
		return eachJSON(cfg, patches, bases, code, why, stdout, stderr)
	}
	fmt.Fprintf(stderr, "hunk: %s\n", why)
	if failed != nil {
		writeCommandOutput(stderr, failed)
	}
	return code
}

// eachOutcome is how the loop ended, for the exit and the summary.
type eachOutcome struct {
	tried, zeros, timeouts int
	notBuilt               int   // patches whose --build failed, their command not run
	stopAt                 int   // index of the patch the run stopped at, -1 for none
	stopErr                error // a mid-run refusal or I/O failure
	sig                    os.Signal
	seconds                float64
}

// eachExit is §4.3's exit: the run's own, never a command's.
func eachExit(patches []*eachPatch, o eachOutcome) int {
	switch {
	case o.stopAt >= 0 && patches[o.stopAt].outcome() != nil && len(patches[o.stopAt].outcome().NotRestored) > 0:
		return exitRollbackFailed
	case o.sig != nil:
		return signalExit(o.sig)
	case o.stopErr != nil:
		return ExitCode(o.stopErr)
	case o.zeros > 0 || o.timeouts > 0 || o.notBuilt > 0:
		// As a verify that went against the caller exits: the tree is back in
		// place, and the verdict is the news (§4.3). A patch that did not
		// build was shown nothing, so it is not a catch either.
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
			patchCount(o.tried), eachVerdict(o.tried, o.zeros, o.timeouts, o.notBuilt), o.seconds)
	}
}

// writeEachLine is one patch's line, to stderr the moment it finished, plus
// the output where it is the thing to read: a command that exited 0 or timed
// out is the one to investigate, a build that failed is the compile error the
// patch's author fixes, and under --quiet those lines are all that print.
// With --verify-lines given, every command's tail prints, since a caught
// patch's is what says what caught it (#89); --quiet still decides which
// lines print at all.
func writeEachLine(cfg eachConfig, w io.Writer, width int, ep *eachPatch) {
	v, what := ep.v, "command"
	if v == nil {
		v, what = ep.build, "build"
	}
	// notBuilt is a build that ended by itself or by --timeout, not by a
	// signal and not for want of a shell.
	notBuilt := what == "build" && v.Ran && v.Signal == nil
	actionable := v.Signal != nil || v.TimedOut > 0 || (v.Ran && v.Status == 0) || notBuilt ||
		len(v.NotRestored) > 0 || len(v.Gone) > 0 || len(v.Left) > 0 || !v.Ran
	if cfg.quiet && !actionable {
		return
	}
	fmt.Fprintf(w, "tried %-*s  ", width, showControls(ep.name))
	switch {
	case v.Signal != nil && !v.Ran:
		fmt.Fprintf(w, "interrupted by %s before the command ran", signalName(v.Signal))
	case v.Signal != nil:
		fmt.Fprintf(w, "interrupted by %s, the %s was ended (%.1fs)", signalName(v.Signal), what, v.Seconds)
	case !v.Ran:
		fmt.Fprintf(w, "the %s could not start", what)
	case notBuilt && v.TimedOut > 0:
		fmt.Fprintf(w, "did not build: the build timed out after %s", shortDuration(v.TimedOut))
	case notBuilt:
		fmt.Fprintf(w, "did not build: the build exited %d (%.1fs)", v.Status, v.Seconds)
	case v.TimedOut > 0:
		fmt.Fprintf(w, "the command timed out after %s", shortDuration(v.TimedOut))
	default:
		fmt.Fprintf(w, "the command exited %d (%.1fs)", v.Status, v.Seconds)
	}
	if len(v.NotRestored) > 0 {
		fmt.Fprintf(w, "; %s left alone", count(len(v.NotRestored), "file"))
	}
	fmt.Fprintln(w)
	if notBuilt || (v.Ran && (v.TimedOut > 0 || v.Status == 0 || (cfg.everyTail && what == "command"))) {
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
	case ep.outcome() != nil && len(ep.outcome().NotRestored) > 0:
		if n := len(ep.outcome().NotRestored); n > 1 {
			return fmt.Sprintf("stopped at %s: %d files were not restored", name, n)
		}
		return fmt.Sprintf("stopped at %s: 1 file was not restored", name)
	case sig != nil && ep.outcome() == nil:
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

// eachVerdict is the summary's middle: what the commands said, and how many
// patches never reached theirs because the build failed.
func eachVerdict(tried, zeros, timeouts, notBuilt int) string {
	if zeros == 0 && timeouts == 0 && notBuilt == 0 {
		return "every command exited nonzero"
	}
	parts := make([]string, 0, 4)
	if n := tried - zeros - timeouts - notBuilt; n > 0 {
		parts = append(parts, fmt.Sprintf("%d exited nonzero", n))
	}
	if zeros > 0 {
		parts = append(parts, fmt.Sprintf("%d exited 0", zeros))
	}
	if timeouts > 0 {
		parts = append(parts, fmt.Sprintf("%d timed out", timeouts))
	}
	if notBuilt > 0 {
		parts = append(parts, fmt.Sprintf("%d did not build", notBuilt))
	}
	return strings.Join(parts, ", ")
}

// eachNotTried is every patch the stop left untried, in order, shown as a
// path is.
func eachNotTried(patches []*eachPatch) []string {
	var names []string
	for _, ep := range patches {
		if ep.outcome() == nil && len(ep.failures) == 0 && ep.loadErr == nil {
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
// name, and the runs on the tree as it is where they ran. The streamed
// per-patch lines were stderr's.
func eachJSON(cfg eachConfig, patches []*eachPatch, bases eachBaselines, code int, stopReason string, stdout, stderr io.Writer) int {
	out := jsonReport{OK: code == exitOK, Exit: code, DryRun: cfg.dryRun}
	je := &jsonEach{Command: cfg.command, Build: cfg.build}
	if bases.build != nil {
		je.Baseline = eachBuildJSON(bases.build)
	}
	if bases.command != nil {
		je.TryBaseline = eachCommandJSON(bases.command)
	}
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
		case ep.outcome() != nil:
			if ep.build != nil {
				jp.Build = eachBuildJSON(ep.build)
			}
			// What putting the files back did is the last run's, the
			// build's when the command never ran.
			back := ep.outcome()
			jp.RolledBack, jp.Gone = back.RolledBack, back.Gone
			jp.Left = jsonLefts(back.Left)
			for _, n := range back.NotRestored {
				jp.NotRestored = append(jp.NotRestored, jsonRestore{n.Path, n.Resolved, n.Reason})
			}
			if v := ep.v; v != nil {
				status := v.Status
				jp.Ran, jp.Status, jp.Seconds = v.Ran, &status, v.Seconds
				jp.jsonEnded = endedJSON(v)
				// Every run's output, the caught ones' included: there is
				// nothing to bury in an object, and one --try's carries it
				// (#89).
				if v.Ran {
					jp.Output = v.Tail
				}
			}
		}
		je.Patches = append(je.Patches, jp)
		var md *MistypedDelimiters
		switch {
		case ep.p != nil:
			out.Warnings = append(out.Warnings, jsonWarnings(ep.name, ep.p.Strays, false, false)...)
		case errors.As(ep.loadErr, &md):
			out.Warnings = append(out.Warnings, jsonWarnings(ep.name, md.Strays, false, true)...)
		}
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

// eachBuildJSON is one --build run for --json: its output where it did not
// exit 0 by itself, which is where the text shows it.
func eachBuildJSON(b *Verify) *jsonEachRun {
	jb := &jsonEachRun{Ran: b.Ran, OK: b.OK, Status: b.Status, Seconds: b.Seconds, jsonEnded: endedJSON(b)}
	if b.Ran && !b.OK && b.Signal == nil {
		jb.Output = b.Tail
	}
	return jb
}

// eachCommandJSON is the command's run on the tree as it is for --json: its
// output whatever the end, as every run of the command carries it.
func eachCommandJSON(c *Verify) *jsonEachRun {
	return &jsonEachRun{Ran: c.Ran, OK: c.OK, Status: c.Status, Seconds: c.Seconds, Output: c.Tail, jsonEnded: endedJSON(c)}
}
