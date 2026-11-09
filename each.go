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
// Both run once more after the last patch, from issue #110: something besides
// the patches can change during a run, a linter upgraded or a service gone,
// and then every later patch is caught by that rather than by its test. A
// run that fails there leaves every catch in doubt, and the exit is 3, as for
// a break that passed.
//
// A caught patch's output prints only when --verify-lines is given (#89): a
// passing suite's forty lines under every catch would bury the misses, and
// the caller who wants to know what caught each asks for the lines that say.
//
// --measure, from issue #154, is the same loop with the scoring taken out,
// for comparing patches by what the command prints, a benchmark's numbers:
// every run's status and output is a result and none is a catch. The tree as
// it is is an entry of the run, base, rather than a gate before it, and runs
// first in each round; --rounds N repeats base and the patches N times,
// interleaved, which is what evens out a machine's drift between them. Each
// run's command and build get HUNK_PATCH and HUNK_ROUND in their environment,
// so one command can name a file for each run's output. The exit is 0 when
// every run ran to its end, and 3 when one timed out or did not build, since
// a measurement is missing.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
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
	isolated     bool // --isolated: every patch is tried in a copy of the tree (#130)
	// links is what --isolated-link names, checked, to link beside the copy (#140).
	links []besideLink
	// letGo lets the tree's lock go, once an --isolated run has its copy.
	letGo func()
	// inCopy is set once the copy is made: the run from there is in it.
	inCopy bool
	// copied is that copy (#132), for --json; the text said so as it was made.
	copied *copyMade
	// measure is --measure (#154): every run is a result and none a catch,
	// and the tree as it is is an entry of each round rather than a gate.
	measure bool
	rounds  int // --rounds, 1 unless given
}

// measureBase is the name of a --measure run with no patch applied, on its
// line, in --json and in HUNK_PATCH. A patch file of that name is refused
// under --measure, so the name is never two entries'.
const measureBase = "base"

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
	// Under --measure each entry is one run (#154): base says it has no
	// patch, round is its round from 1, and rounds how many the run has. All
	// three are zero outside --measure.
	base          bool
	round, rounds int
}

// env is what a --measure run's build and command get in their environment
// (#154): the entry's name and its round, so one command can name a file for
// each run's output. Nil outside --measure.
func (ep *eachPatch) env() []string {
	if ep.round == 0 {
		return nil
	}
	return []string{"HUNK_PATCH=" + ep.name, "HUNK_ROUND=" + strconv.Itoa(ep.round)}
}

// inRound is name with the entry's round after it, under --measure with more
// than one round, for the sentences that name an entry: "a.hunk in round 2".
func (ep *eachPatch) inRound(name string) string {
	if ep.rounds > 1 {
		return fmt.Sprintf("%s in round %d", name, ep.round)
	}
	return name
}

// shown is the entry's name as the text prints it, with its round where it
// has one that says something.
func (ep *eachPatch) shown() string {
	return ep.inRound(showControls(ep.name))
}

// measureRuns is a --measure run's entries in the order they run (#154): in
// each round the tree as it is, then every patch, so that base and the
// patches are interleaved and drift falls on each alike. A patch's first
// round carries its preflight's result, whose warnings --json gives once.
func measureRuns(patches []*eachPatch, rounds int) []*eachPatch {
	runs := make([]*eachPatch, 0, rounds*(len(patches)+1))
	for r := 1; r <= rounds; r++ {
		runs = append(runs, &eachPatch{name: measureBase, base: true, round: r, rounds: rounds})
		for _, ep := range patches {
			run := &eachPatch{name: ep.name, p: ep.p, round: r, rounds: rounds}
			if r == 1 {
				run.res = ep.res
			}
			runs = append(runs, run)
		}
	}
	return runs
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
// A file may hold several patches, a "@@ patch NAME" heading each (#100).
// Each is its own patch of the run, named file:NAME, and a fault in the
// headings refuses the lot as a patch that does not parse does, named by the
// combined file's own line.
//
// A patch with lines that look like a mistyped delimiter is refused with the
// patches that do not parse (#97), so nothing is tried. Under
// --allow-mistyped-delimiter they are warned of (#92) after all of it,
// whatever the run's end, each under its patch's name, and always as lines to
// come out of the patch, since --each puts every file back.
func runEach(cfg eachConfig, tree *Tree, stdout, stderr io.Writer, interrupted *os.Signal) int {
	var patches []*eachPatch
	for _, name := range cfg.names {
		src, err := os.ReadFile(name)
		if err != nil {
			patches = append(patches, &eachPatch{name: name, loadErr: err})
			continue
		}
		secs, err := splitPatchSet(src, cfg.marker)
		if err != nil {
			patches = append(patches, &eachPatch{name: name, loadErr: err})
			continue
		}
		for _, sec := range secs {
			ep := &eachPatch{name: sec.shown(name)}
			patches = append(patches, ep)
			p, err := Parse(sec.src, cfg.marker)
			if err = sec.renumber(p, err); err != nil {
				ep.loadErr = err
				continue
			}
			if len(p.Strays) > 0 && !cfg.allowStrays {
				ep.loadErr = &MistypedDelimiters{Strays: p.Strays}
				continue
			}
			ep.p = p
		}
	}
	o, e := &tally{w: stdout}, &tally{w: stderr}
	code := eachLoaded(cfg, patches, tree, o, e, interrupted)
	if !cfg.asJSON {
		gap := o.n+e.n > 0
		for _, ep := range patches {
			// A patch's slips (#111), from its preflight, and then its
			// strays. Every patch is put back, so the remedy is the patch's.
			if ep.res != nil && writeSlips(stderr, ep.name, ep.res.Slips, false, gap) {
				gap = false
			}
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
	if cfg.measure {
		// Every run is an entry of its own from here, the report's and
		// --json's included: the tree as it is first in each round (#154).
		patches = measureRuns(patches, cfg.rounds)
	}
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
	if cfg.isolated {
		// Every patch has matched the tree itself, and from here the run is
		// in a copy of it (#130), the runs on the tree as it is included.
		// Nothing but the command writes in the copy, so a file it rewrote
		// is put back regardless, as under --verify-may-format, and the next
		// patch is tried against the copy as it was made.
		copied, made, remove, err := isolate(tree, cfg.links, func() os.Signal { return pending(sigs) })
		var ci *copyInterrupted
		switch {
		case errors.As(err, &ci):
			sig, stopAt, todo = ci.sig, 0, nil
		case err != nil:
			return eachBaselineRefused(cfg, patches, bases, nil, err.Error(), ExitCode(err), stdout, stderr)
		default:
			defer removeOrWarn(remove, stderr)
			defer func() { _ = copied.Close() }()
			cfg.letGo()
			tree = copied
			cfg.verifyFormat, cfg.inCopy, cfg.copied = true, true, &made
			if !cfg.quiet {
				writeCopied(stderr, made)
			}
		}
	}
	// Under --measure neither run below is a gate: the tree as it is is the
	// base entry of each round, built first where there is a build, and how
	// it ends is its line (#154).
	if sig == nil && cfg.build != "" && !cfg.measure {
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
			return eachBaselineRefused(cfg, patches, bases, b, cfg.inACopy(baselineFailure("the build", b, "not built")), exitUsage, stdout, stderr)
		}
	}
	if sig == nil && !cfg.measure {
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
			return eachBaselineRefused(cfg, patches, bases, c, cfg.inACopy(baselineFailure("the command", c, "timed out")), exitUsage, stdout, stderr)
		case !c.OK:
			return eachBaselineRefused(cfg, patches, bases, c, cfg.inACopy(baselineFailure("the command", c, "caught")), exitUsage, stdout, stderr)
		}
	}

	for i, ep := range todo {
		if s := pending(sigs); s != nil {
			// Between patches: the tree is whole and nothing is running.
			sig = s
			stopAt = i
			break
		}
		var tryErr error
		if ep.base {
			tryErr = tryBase(cfg, ep, tree, sigs, stop)
		} else {
			txn := NewTxn(tree, cfg.opt)
			txn.beforeCommit = func() { notifyInterrupts(sigs) }
			if _, err := txn.Run(ep.p); err != nil {
				// The preflight matched every patch, so a refusal here means
				// the tree changed under the run: by the command, or by
				// another writer. Carrying on would score every later patch
				// against a tree nobody validated.
				stopAt, stopErr = i, err
				var ve *ValidationError
				if errors.As(err, &ve) {
					ep.failures = ve.Failures
				}
				break
			}
			tryErr = tryOne(cfg, ep, txn, tree, sigs, stop)
		}
		for _, v := range []*Verify{ep.v, ep.build} {
			if v != nil {
				v.Isolated = cfg.inCopy
			}
		}
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
		stopAt: stopAt, stopErr: stopErr, sig: sig, inCopy: cfg.inCopy, measure: cfg.measure,
	}
	if stopAt < 0 && o.caught() > 0 && !cfg.measure {
		// The tree as it is once more, the last patch back (#110). The runs
		// before the first patch say the command could pass then; these say
		// it still can, so that a catch is the patch's and not that of
		// something that changed meanwhile. Only a catch needs it: a break
		// that passed, hung or did not build is already the news. Under
		// --measure nothing is a catch, and base ran in every round.
		o.sig, o.afterErr = eachAfter(cfg, tree, stop, &bases)
	}
	o.bases = bases
	o.seconds = time.Since(start).Seconds()
	code := eachExit(patches, o)
	if o.sig != nil && code != exitRollbackFailed && interrupted != nil {
		*interrupted = o.sig
	}
	if cfg.asJSON {
		return eachJSON(cfg, patches, bases, code, eachStopReason(patches, o), stdout, stderr)
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
		b, err := runBuild(cfg, txn, tree, stop, ep.env()...)
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
	v, err := runTry(txn, tree, cfg.command, cfg.verifyLines, cfg.verifyFormat, stop, ep.env()...)
	ep.v = v
	return err
}

// tryBase is a --measure run's turn with no patch applied (#154): the build
// where there is one, then the command, on the tree as it is, as tryOne runs
// them on a patch. A build that does not exit 0 leaves the command unrun, and
// a signal between the two leaves it unrun too. Nothing was written, so
// nothing is put back. The error is a build or a command that could not
// start, which stops the run at 5 as it does under a patch.
func tryBase(cfg eachConfig, ep *eachPatch, tree *Tree, sigs chan os.Signal, stop Stop) error {
	if cfg.build != "" {
		b, err := runOnTheTree(cfg.build, cfg, tree, stop, ep.env()...)
		if err != nil {
			ep.build = &Verify{Command: cfg.build}
			return fmt.Errorf("could not run the --build command: %w; no patch was applied", err)
		}
		ep.build = b
		if !b.OK {
			return nil
		}
		if s := pending(sigs); s != nil {
			ep.v = &Verify{Command: cfg.command, Signal: s}
			return nil
		}
	}
	v, err := runOnTheTree(cfg.command, cfg, tree, stop, ep.env()...)
	if err != nil {
		ep.v = &Verify{Command: cfg.command}
		return fmt.Errorf("could not run the --try command: %w; no patch was applied", err)
	}
	ep.v = v
	return nil
}

// runOnTheTree runs command once, from the root, under the run's timeout and
// signals: --build, or either baseline, with env added to the environment. A
// timeout is 124 in Status, as --try's is.
func runOnTheTree(command string, cfg eachConfig, tree *Tree, stop Stop, env ...string) (*Verify, error) {
	b, err := RunVerify(command, tree.Root(), cfg.verifyLines, stop, env...)
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
func runBuild(cfg eachConfig, txn *Txn, tree *Tree, stop Stop, env ...string) (*Verify, error) {
	b, err := runOnTheTree(cfg.build, cfg, tree, stop, env...)
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

// eachAfter runs the build, where there is one, and then the command on the
// tree as it is once the last patch is back (#110), into bases. The build
// goes first for the reason it does before the first patch, and one more: the
// last patch's build may have left what the command runs, and the command
// would then be testing that break. A signal already waiting ends the first
// run as it starts, so there is no separate check for one. It returns the
// signal that ended either run, and the run that could not start.
func eachAfter(cfg eachConfig, tree *Tree, stop Stop, bases *eachBaselines) (os.Signal, *afterCannotStart) {
	if cfg.build != "" {
		b, err := runOnTheTree(cfg.build, cfg, tree, stop)
		if err != nil {
			return nil, &afterCannotStart{who: "the --build command", err: err}
		}
		bases.buildAfter = b
		if !b.OK {
			return b.Signal, nil
		}
	}
	c, err := runOnTheTree(cfg.command, cfg, tree, stop)
	if err != nil {
		return nil, &afterCannotStart{who: "the --try command", err: err}
	}
	bases.commandAfter = c
	return c.Signal, nil
}

// afterCannotStart is a run on the tree as it is after the last patch that
// could not start: exit 5, as the same failure before the first patch is.
type afterCannotStart struct {
	who string // "the --build command" or "the --try command"
	err error
}

func (e *afterCannotStart) Error() string {
	return fmt.Sprintf("could not run %s with no patch applied after the last patch: %v", e.who, e.err)
}

// eachBaselines is the runs on the tree as it is: --build's where there is
// one, then the command's, before the first patch, and the same pair after
// the last (#110). Each is nil where it did not run.
type eachBaselines struct {
	build, command           *Verify
	buildAfter, commandAfter *Verify
}

// failedAfter is the run after the last patch that ended by itself or by the
// timeout without passing, and who ran it: the build's, which goes first, or
// the command's. Nil when both passed or neither ran; one a signal ended is
// the signal's to report.
func (b eachBaselines) failedAfter() (*Verify, string) {
	switch {
	case b.buildAfter != nil && !b.buildAfter.OK && b.buildAfter.Signal == nil:
		return b.buildAfter, "the build"
	case b.commandAfter != nil && !b.commandAfter.OK && b.commandAfter.Signal == nil:
		return b.commandAfter, "the command"
	}
	return nil, ""
}

// endedHow is how a run on the tree as it is ended without passing, in the
// words the sentences about it use: "exited 5", "timed out after 1s".
func endedHow(b *Verify) string {
	if b.TimedOut > 0 {
		return "timed out after " + shortDuration(b.TimedOut)
	}
	return fmt.Sprintf("exited %d", b.Status)
}

// baselineFailure is why a run on the tree as it is refused the run: who ran,
// how it ended, and what every patch would have read as.
func baselineFailure(who string, b *Verify, reads string) string {
	return who + " " + endedHow(b) + " with no patch applied, so every patch would read as " + reads + "; nothing was tried"
}

// inACopy is why, the refusal of a run whose build or command failed on the
// tree as it is, with what an --isolated run adds (#140): it ran in a copy,
// whose ".." is not the tree's, so a command that reads a sibling of the tree
// by a relative path fails there under every patch and with none. That was
// the issue: the copy line named the copy, and nothing said why it mattered.
func (cfg eachConfig) inACopy(why string) string {
	if !cfg.inCopy {
		return why
	}
	return why + "; it ran in a copy of the tree, where a relative path out of --root, such as ../lib, finds nothing unless --isolated-link names it"
}

// afterFailure is why the run on the tree as it is after the last patch leaves
// every catch in doubt: who ran, how it ended, and what that means. It opens
// with "after the last patch" so that no reader, scripts/corpus included,
// takes it for the refusal before the first patch, which is exit 1.
func afterFailure(who string, b *Verify) string {
	s := "after the last patch, " + who + " " + endedHow(b) +
		" with no patch applied, and it passed before the first: something besides the patches makes it fail, "
	if who == "the build" {
		return s + "so the command could not run again to confirm the catches"
	}
	return s + "so any catch may be that"
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
	bases                  eachBaselines     // the runs on the tree as it is, before and after
	afterErr               *afterCannotStart // a run after the last patch that could not start
	inCopy                 bool              // the run was in a copy of the tree, under --isolated (#130)
	measure                bool              // --measure: no run is a catch (#154)
}

// caught is how many patches the command caught: tried, and neither passed,
// timed out nor failed to build. Under --measure, how many runs exited
// nonzero, which is not a catch there.
func (o eachOutcome) caught() int {
	return o.tried - o.zeros - o.timeouts - o.notBuilt
}

// eachExit is §4.3's exit: the run's own, never a command's.
func eachExit(patches []*eachPatch, o eachOutcome) int {
	switch {
	case o.stopAt >= 0 && patches[o.stopAt].outcome() != nil && len(patches[o.stopAt].outcome().NotRestored) > 0:
		if o.inCopy {
			// The copy could not be put back, so no later patch could be
			// tried alone in it; but it is removed, and the tree was never
			// written, which is what 4 would deny.
			return exitIO
		}
		return exitRollbackFailed
	case o.sig != nil:
		return signalExit(o.sig)
	case o.stopErr != nil:
		return ExitCode(o.stopErr)
	case o.afterErr != nil:
		// As the same failure before the first patch is: I/O.
		return exitIO
	case o.timeouts > 0 || o.notBuilt > 0 || (o.zeros > 0 && !o.measure):
		// As a verify that went against the caller exits: the tree is back in
		// place, and the verdict is the news (§4.3). A patch that did not
		// build was shown nothing, so it is not a catch either. Under
		// --measure a command's status is no verdict, but a run that timed
		// out or did not build is a measurement missing (#154).
		return exitVerifyFailed
	}
	if failed, _ := o.bases.failedAfter(); failed != nil {
		// The tree as it is fails after the last patch (#110), so no catch is
		// shown to be the patch's. Not 1, which says nothing was tried, and
		// not 0, which says every catch stands: the lines are the news, as
		// they are at 3.
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
			fmt.Fprintf(stderr, "hunk: %s no longer applies; the preflight matched every patch, so the tree has changed since the run began: by the command, or by another writer\n", ep.shown())
			writeFailureBlocks(stderr, ep.failures)
		case o.stopErr != nil:
			// A per-patch line said what the try did; this is why the run
			// stopped. A failed --try's own not-restored block was the line's.
			fmt.Fprintf(stderr, "hunk: at %s: %v\n", ep.shown(), o.stopErr)
			var ce *CommitError
			if errors.As(o.stopErr, &ce) {
				writeNotRestored(stderr, ce.NotRestored)
			}
		}
		// Under --measure the runs not reached are counted in the summary
		// rather than named here: N rounds of the same names, and --json
		// has each.
		if notTried := eachNotTried(patches); len(notTried) > 0 && !o.measure {
			fmt.Fprintf(stderr, "not tried: %s\n", strings.Join(notTried, ", "))
		}
	}
	// The run on the tree as it is after the last patch, where it did not
	// pass: why, and its output, which is what says what changed (#110).
	if failed, who := o.bases.failedAfter(); failed != nil {
		fmt.Fprintf(stderr, "hunk: %s\n", afterFailure(who, failed))
		writeCommandOutput(stderr, failed)
	}
	if o.afterErr != nil {
		fmt.Fprintf(stderr, "hunk: %v\n", o.afterErr)
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
	where := "and put back every file"
	if o.inCopy {
		where = "in a copy of the tree"
	}
	switch {
	case o.stopAt >= 0 && o.measure:
		fmt.Fprintf(w, "tried %d of %s; %s", o.tried, count(len(patches), "run"), eachStopClause(patches, o))
		if n := len(eachNotTried(patches)); n > 0 {
			fmt.Fprintf(w, "; %s not tried", count(n, "run"))
		}
		fmt.Fprintln(w)
	case o.stopAt >= 0:
		fmt.Fprintf(w, "tried %d of %s; %s", o.tried, patchCount(len(patches)), eachStopClause(patches, o))
		if n := len(eachNotTried(patches)); n > 0 {
			fmt.Fprintf(w, "; %s not tried", patchCount(n))
		}
		fmt.Fprintln(w)
	case o.measure:
		// Every entry ran in every round: the patches are what is left of a
		// round once base is taken out.
		what := "base and " + patchCount(len(patches)/cfg.rounds-1)
		if cfg.rounds > 1 {
			what += fmt.Sprintf(" in %d rounds", cfg.rounds)
		}
		fmt.Fprintf(w, "tried %s %s; %s (%.1fs)\n", what, where, measureVerdict(o), o.seconds)
	default:
		fmt.Fprintf(w, "tried %s %s; %s%s (%.1fs)\n",
			patchCount(o.tried), where, eachVerdict(o.tried, o.zeros, o.timeouts, o.notBuilt), eachAfterClause(o), o.seconds)
	}
}

// measureVerdict is a --measure summary's middle (#154): how the runs ended,
// none of it a catch, with what made the exit 3 named.
func measureVerdict(o eachOutcome) string {
	nonzero := o.caught()
	if nonzero == 0 && o.timeouts == 0 && o.notBuilt == 0 {
		return "every command exited 0"
	}
	parts := make([]string, 0, 4)
	if o.zeros > 0 {
		parts = append(parts, fmt.Sprintf("%d exited 0", o.zeros))
	}
	if nonzero > 0 {
		parts = append(parts, fmt.Sprintf("%d exited nonzero", nonzero))
	}
	if o.timeouts > 0 {
		parts = append(parts, fmt.Sprintf("%d timed out", o.timeouts))
	}
	if o.notBuilt > 0 {
		parts = append(parts, fmt.Sprintf("%d did not build", o.notBuilt))
	}
	return strings.Join(parts, ", ")
}

// writeEachLine is one patch's line, to stderr the moment it finished, plus
// the output where it is the thing to read: a command that exited 0 or timed
// out is the one to investigate, a build that failed is the compile error the
// patch's author fixes, and under --quiet those lines are all that print.
// With --verify-lines given, every command's tail prints, since a caught
// patch's is what says what caught it (#89); --quiet still decides which
// lines print at all. Under --measure every command's tail prints, since the
// output is the measurement, and a command that exited 0 is no news (#154).
func writeEachLine(cfg eachConfig, w io.Writer, width int, ep *eachPatch) {
	v, what := ep.v, "command"
	if v == nil {
		v, what = ep.build, "build"
	}
	// notBuilt is a build that ended by itself or by --timeout, not by a
	// signal and not for want of a shell.
	notBuilt := what == "build" && v.Ran && v.Signal == nil
	actionable := v.Signal != nil || v.TimedOut > 0 || (v.Ran && v.Status == 0 && !cfg.measure) || notBuilt ||
		len(v.NotRestored) > 0 || len(v.Gone) > 0 || len(v.Left) > 0 || !v.Ran
	if cfg.quiet && !actionable {
		return
	}
	fmt.Fprintf(w, "tried %-*s  ", width, showControls(ep.name))
	if ep.rounds > 1 {
		fmt.Fprintf(w, "round %-*d  ", len(strconv.Itoa(ep.rounds)), ep.round)
	}
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
		if v.Isolated {
			fmt.Fprint(w, " in the copy")
		}
	}
	fmt.Fprintln(w)
	if notBuilt || (v.Ran && (v.TimedOut > 0 || v.Status == 0 || ((cfg.everyTail || cfg.measure) && what == "command"))) {
		writeCommandOutput(w, v)
	}
	writeAftermath(w, v)
}

// eachStopClause is the summary's reason for stopping, after the full story
// has been told above it.
func eachStopClause(patches []*eachPatch, o eachOutcome) string {
	ep := patches[o.stopAt]
	name := ep.shown()
	where := ""
	if o.inCopy {
		where = " in the copy"
	}
	switch sig := o.sig; {
	case ep.outcome() != nil && len(ep.outcome().NotRestored) > 0:
		if n := len(ep.outcome().NotRestored); n > 1 {
			return fmt.Sprintf("stopped at %s: %d files were not restored%s", name, n, where)
		}
		return fmt.Sprintf("stopped at %s: 1 file was not restored%s", name, where)
	case sig != nil && ep.outcome() == nil:
		return "interrupted by " + signalName(sig)
	case sig != nil:
		return fmt.Sprintf("interrupted by %s during %s", signalName(sig), name)
	default:
		return "stopped at " + name
	}
}

// eachStopReason is the one-line form of the stop for --json, where the text
// above would have told the story; for a run that did not stop, of the run on
// the tree as it is after the last patch where that did not pass (#110).
func eachStopReason(patches []*eachPatch, o eachOutcome) string {
	if o.stopAt < 0 {
		return eachAfterReason(o)
	}
	ep := patches[o.stopAt]
	switch {
	case len(ep.failures) > 0:
		return fmt.Sprintf("%s no longer applies; the preflight matched every patch, so the tree has changed since the run began", ep.inRound(ep.name))
	case o.stopErr != nil:
		return fmt.Sprintf("at %s: %v", ep.inRound(ep.name), o.stopErr)
	}
	return eachStopClause(patches, o)
}

// eachAfterClause is the summary's word on the run on the tree as it is after
// the last patch, where it did not pass (#110): "" where it passed or did not
// run. Each begins ", but", which is what scripts/corpus reads 3 or 5 off.
func eachAfterClause(o eachOutcome) string {
	failed, who := o.bases.failedAfter()
	switch {
	case o.sig != nil:
		return ", but the check with no patch applied was interrupted by " + signalName(o.sig)
	case o.afterErr != nil:
		return ", but " + o.afterErr.who + " could not start with no patch applied"
	case failed != nil:
		return ", but " + who + " now fails with no patch applied"
	}
	return ""
}

// eachAfterReason is the same for --json's error, as a sentence the text
// prints above the summary.
func eachAfterReason(o eachOutcome) string {
	failed, who := o.bases.failedAfter()
	switch {
	case o.sig != nil:
		return "the check with no patch applied after the last patch was interrupted by " + signalName(o.sig)
	case o.afterErr != nil:
		return o.afterErr.Error()
	case failed != nil:
		return afterFailure(who, failed)
	}
	return ""
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
	out := jsonReport{OK: code == exitOK, Exit: code, DryRun: cfg.dryRun, Isolated: cfg.inCopy, Copy: jsonCopyOf(cfg.copied)}
	je := &jsonEach{Command: cfg.command, Build: cfg.build}
	if cfg.measure {
		je.Measure, je.Rounds = true, cfg.rounds
	}
	if bases.build != nil {
		je.Baseline = eachBuildJSON(bases.build)
	}
	if bases.command != nil {
		je.TryBaseline = eachCommandJSON(bases.command)
	}
	if bases.buildAfter != nil {
		je.BaselineAfter = eachBuildJSON(bases.buildAfter)
	}
	if bases.commandAfter != nil {
		je.TryBaselineAfter = eachCommandJSON(bases.commandAfter)
	}
	if stopReason != "" {
		out.Error = stopReason
	}
	for _, ep := range patches {
		jp := jsonEachPatch{Patch: ep.name, Base: ep.base, Round: ep.round}
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
		if ep.round > 1 {
			// A --measure patch's warnings are its first round's (#154).
			continue
		}
		if ep.res != nil {
			out.Warnings = append(out.Warnings, slipWarnings(ep.name, ep.res.Slips, false)...)
		}
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
