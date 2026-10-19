// Command hunk applies a batch of literal-text edits across several files as a
// single transaction, optionally gated on a verification command. Nothing is
// written until every edit is known to match and every target is known to be
// unchanged since it was read; if the verification fails, the tree goes back.
//
// The specification is SPEC.md, which this repository does not ship. Section
// references in these files (§3, §6.1) point at it.
//
// This file owns flags, wiring, and exit codes (§4).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// version is the release this binary was built from, set by the release
// workflow with -ldflags. Empty in every other build, where the toolchain's own
// record is better evidence than a constant somebody has to remember to bump.
var version string

// versionString is what --version prints, given the build information the
// toolchain recorded, or nil when there is none. Taking it as an argument
// rather than reading it is what makes the cases below testable; the reading is
// one line at the call site.
//
// The three ways this binary reaches somebody, each measured rather than
// assumed:
//
//   - go install ...@v0.2.0, which the README documents and the field runs.
//     The module version is exact: "v0.2.0".
//   - a local build from a checkout, which reports a pseudo-version built from
//     the last tag, the commit time, the revision and a dirty marker:
//     "v0.1.1-0.20260907131653-3e525bcc7aaa+dirty". Precise, and not the
//     release being cut, which is why an artifact has to be stamped.
//   - go build -buildvcs=false, or a build from a copy that is not under
//     version control, which reports "(devel)" and records no revision either.
//
// An earlier draft dug the revision out of bi.Settings for a "(devel, abc123)"
// form. Running it found that case does not arise on a toolchain this module
// can be built by: where a revision exists the pseudo-version already carries
// it, and where it does not, there is nothing to dig for.
func versionString(bi *debug.BuildInfo) string {
	if version != "" {
		return version
	}
	if bi == nil {
		return "(unknown)"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return "(devel)"
}

// Exit codes are a closed set (§4.1). Every exit path in the program maps to
// exactly one of them, which is why there is no log.Fatal anywhere below. The
// tree state each code implies is part of the contract and is asserted by the
// exit-code walk test, not merely documented:
//
//	code  meaning                                     tree
//	0     applied, and verify passed if given         changed
//	1     usage or parse error                        untouched
//	2     validation failed: a hunk did not match     untouched
//	3     verify failed, rolled back                  untouched
//	4     verify failed and rollback was incomplete   inconsistent (§6.3)
//	5     I/O error                                   see the message
//	6     a file changed on disk between load and commit  untouched
//
// Code 2 is the one an agent should expect to see routinely and act on without
// alarm: fix the patch and resend.
const (
	exitOK             = 0
	exitUsage          = 1
	exitNoMatch        = 2
	exitVerifyFailed   = 3
	exitRollbackFailed = 4
	exitIO             = 5
	exitChanged        = 6
)

// usage is what --help prints. §9 requires it to be "complete enough to use the
// tool from cold", and three of its sentences are things the spec explicitly
// commits it to, each with a test:
//
//   - §6.2's batch-atomicity limit, "because a tool that overstates its
//     guarantees is worse than one that has none".
//   - §6.3's --verify-may-format entry, which states the flag and the reason
//     together rather than only the behaviour.
//   - §11's figure for why --verify is optional.
const usage = `hunk applies literal text edits, in batches, as one transaction.

  hunk [flags]                 patch on stdin
  hunk [flags] -f patch.txt    patch from a file
  hunk --try CMD --each a.hunk b.hunk ...
                               try each patch alone, one at a time
  hunk format                  print the grammar and one worked example
  hunk quote FILE START END    print lines of FILE as a hunk, to replace a
                               block without pasting it; see its --help

Nothing is written until every hunk in the batch is known to match exactly as
many times as it claimed, and until every target is known to be unchanged since
it was read. A hunk whose text is not there fails the whole batch and the tree
is untouched.

FLAGS

  --verify CMD          Shell command run after a successful apply. Non-zero
                        rolls everything back. Optional: 58% of the measured
                        calls this tool was designed from bundled one, and
                        requiring it would be obnoxious for documentation edits.
                        Refused, exit 1, if its last pipeline ends in tail,
                        head, tee or cat: the exit status would be the
                        filter's, and the verify could not fail. The output is
                        printed only on failure, already cut by --verify-lines.
  --verify-may-format   On verify failure, or after --try, restore original
                        bytes even for files the command rewrote. Set this when
                        the command formats. Without it, a file that changed
                        after hunk wrote it is left alone and the exit is 4,
                        because silently reverting another writer's work is the
                        one behaviour that would make this tool dangerous. With
                        it, you are asserting nothing else writes the tree
                        during the verify.
  --verify-lines N      Tail of the --verify or --try output to print. (40)
                        Under --each, given, it prints under every patch's
                        line, a caught one's too, to say what caught it; not
                        given, only under a command that exited 0 or timed
                        out.
  --keep-on-fail        Leave the applied changes in place when verify fails,
                        for inspection. Still exits 3.
  --try CMD             Apply, run CMD as --verify would, then put every file
                        back whatever it says, and exit with its status. For a
                        print statement or a measurement you do not mean to
                        keep. 4 if a file could not be put back.
  --each PATCH...       With --try: run its command over several patches, one
                        at a time, the patches as arguments after the flags.
                        Every patch is parsed and validated before the first
                        try, so none is ever scored by an exit that was really
                        hunk's, and the command runs once with no patch
                        applied, so none is scored by a command that fails
                        anyway: one that fails there refuses the run, exit 1.
                        A command whose last pipeline ends in tail, head, tee
                        or cat is refused too, as under --verify, since the
                        exit it scores would be the filter's.
                        A file may hold several patches, one "@@ patch NAME"
                        heading each; every patch is tried alone, its line
                        named FILE:NAME. Only --each reads the heading: a
                        single run given one is refused, exit 1.
                        A line per patch goes to stderr as it finishes; the
                        run stops at once if a file is not put back. The exit
                        is the run's, never a command's: 0 when every command
                        exited nonzero, 3 when one exited 0 or timed out,
                        which in a deliberate-break run means the suite missed
                        a break or hung on it, or a patch did not build under
                        --build. --timeout applies to each command separately.
  --build CMD           With --try --each: run CMD once on the tree as it is,
                        then after each patch is applied and before the --try
                        command. A break that does not compile exits nonzero
                        like a caught one; a patch whose build exits nonzero
                        or times out is reported as "did not build", its
                        output shown and its --try command not run, and the
                        exit is 3. A build that fails with no patch applied
                        refuses the run, exit 1, and so does one whose last
                        pipeline ends in tail, head, tee or cat: a patch that
                        does not build would read as caught.
  --timeout DURATION    End the --verify or --try command after DURATION, such
                        as 90s or 5m, and everything it started with it: exit
                        3 under --verify, rolled back, and 124 under --try, put
                        back. No default. Set it below any limit your harness
                        puts on this call.
  --dry-run             Validate and print the diffstat. Write nothing.
  --root DIR            Resolve relative paths and run --verify or --try here.
                        (cwd)
  --marker STR          Directive prefix. (@@)
  -f FILE               Read the patch from a file instead of stdin.
  --json                Emit the result as one JSON object on stdout.
  --quiet               Print nothing on success. Failures are still reported.
  --context N           Max lines of file text echoed in a near-miss report.
                        (20)
  --eol auto|strict     auto converts payload line endings to the file's
                        dominant ending before matching and on write. (auto)
  --allow-outside-root  Permit paths that resolve outside --root. It applies
                        to every path in the batch, and opens each by name
                        rather than through the root, so a symlink on any of
                        them is followed wherever it leads.
  --allow-mistyped-delimiter
                        Apply a patch holding a line that looks like a
                        mistyped heredoc delimiter, such as HUHUNK above the
                        real HUNK, and warn of each after the report. Without
                        it such a patch is refused, exit 1, nothing written,
                        every such line named: in the field the line was a
                        typo every time. For a document about hunk, which may
                        really hold one.
  --version             Print the version and exit.

EXIT CODES

  0  applied, and verify passed if given            tree changed
  1  usage or parse error                           tree untouched
  2  a hunk did not match                           tree untouched
  3  verify failed, rolled back                     tree untouched
  4  verify failed and rollback was incomplete      tree inconsistent
  5  I/O error                                      tree untouched, unless
                                                    the message says otherwise
  6  a file changed on disk between load and commit tree untouched

Exit 2 is the one to expect routinely and act on without alarm: fix the patch
and resend. The report prints the bytes that are actually in the file, so the
fix is usually a paste rather than a re-read.

Under --try the exit is the command's own status, 124 for a --timeout. Under
--try --each it is the run's, and a command's status never passes through: 0
when every patch was tried and put back and every command exited nonzero, 3
when a command exited 0 or timed out or a patch did not build, and the table's
own codes where the run refused or stopped.

SIGINT, SIGTERM or SIGHUP while the --verify or --try command runs ends the
command and everything it started, puts the batch back, and then ends hunk by
the same signal, which a shell reports as 130, 143 or 129; 4 if a file could not
be put back. SIGKILL cannot be caught and leaves the batch applied.

WHAT THIS DOES NOT PROMISE

Replacing one file is atomic, via rename(2). The batch is not. A write that
fails part-way through, such as a full disk or a name the filesystem refuses,
puts the files before it back and exits 5, but a crash part-way through leaves
them written. Nor is there a lock. Every file is re-read just before writing,
which catches a writer that finished in between, not one writing at the same
time: two runs on the same files at once usually both succeed, and can leave
some files as one wrote them and some as the other did. Both gaps are repaired
by the version control the tree is already under.

Run "hunk format" for the patch grammar.
`

func main() {
	var interrupted os.Signal
	code := invoke(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, &interrupted)
	// A signal that ended the --verify or --try command is re-raised once the
	// batch is back and the report is out, so that a parent sees what it
	// would have seen before hunk caught it: a death by that signal, which a
	// shell reports as 130 or 143. Where it cannot be, on Windows, the exit
	// is that number.
	if interrupted != nil {
		raise(interrupted)
	}
	os.Exit(code)
}

// invoke is main with its edges injected, so the exit-code walk can drive it
// in-process and assert the tree state each code implies. It never re-raises a
// signal, which would end a test that called it: it returns the status a shell
// would have seen, and sets *interrupted, where interrupted is not nil, to the
// signal main should die of once it returns.
func invoke(args []string, stdin io.Reader, stdout, stderr io.Writer, interrupted *os.Signal) int {
	if len(args) > 0 && args[0] == "format" {
		if len(args) > 1 {
			fmt.Fprintf(stderr, "hunk: format takes no arguments\n")
			return exitUsage
		}
		fmt.Fprint(stdout, formatDoc)
		return exitOK
	}
	// quote reads the tree and writes a patch; it has its own flags and reads
	// no patch, so it is told apart before anything here is parsed.
	if len(args) > 0 && args[0] == "quote" {
		return runQuote(args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("hunk", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // the errors are written below, once, to stderr
	fs.Usage = func() {}

	var (
		verify       = fs.String("verify", "", "")
		try          = fs.String("try", "", "")
		each         = fs.Bool("each", false, "")
		build        = fs.String("build", "", "")
		verifyFormat = fs.Bool("verify-may-format", false, "")
		verifyLines  = fs.Int("verify-lines", 40, "")
		keepOnFail   = fs.Bool("keep-on-fail", false, "")
		dryRun       = fs.Bool("dry-run", false, "")
		root         = fs.String("root", "", "")
		marker       = fs.String("marker", DefaultMarker, "")
		patchFile    onceFlag
		asJSON       = fs.Bool("json", false, "")
		quiet        = fs.Bool("quiet", false, "")
		context      = fs.Int("context", DefaultContext, "")
		eol          = fs.String("eol", "auto", "")
		allowOutside = fs.Bool("allow-outside-root", false, "")
		allowStrays  = fs.Bool("allow-mistyped-delimiter", false, "") // allowMistypedFlag
		timeoutFlag  = fs.String("timeout", "", "")
		help         = fs.Bool("help", false, "")
		h            = fs.Bool("h", false, "")
		showVersion  = fs.Bool("version", false, "")
	)
	fs.Var(&patchFile, "f", "")

	// fail reports an error found before the transaction runs. §4: a caller
	// that passed --json "asked for it on every path", so it gets the object
	// on stdout rather than a line on stderr. Until 2026-09-22 none of these
	// paths printed one. The text is what they printed before, byte for byte.
	wantsJSON := jsonRequested(args)
	fail := func(code int, err error, hint string) int {
		// A refusal of mistyped delimiters (#97) carries its lines into
		// --json's warnings, and is a hunk: line each in the text.
		var md *MistypedDelimiters
		refused := errors.As(err, &md)
		if wantsJSON {
			rep := &Report{Exit: code, Err: err}
			if refused {
				rep.Strays = md.Strays
			}
			if werr := rep.JSON(stdout); werr != nil {
				fmt.Fprintf(stderr, "hunk: %v\nhunk: the --json report could not be written: %v\n", err, werr)
			}
			return code
		}
		if refused {
			for _, line := range strings.Split(md.Error(), "\n") {
				fmt.Fprintf(stderr, "hunk: %s\n", line)
			}
			return code
		}
		fmt.Fprintf(stderr, "hunk: %v\n", err)
		if hint != "" {
			fmt.Fprintln(stderr, hint)
		}
		return code
	}

	if err := fs.Parse(args); err != nil {
		return fail(exitUsage, err, `Run "hunk --help" for the flags.`)
	}
	wantsJSON = *asJSON
	if *help || *h {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	// Before anything reads stdin, like --help: a caller asking which binary
	// this is has not given it a patch.
	if *showVersion {
		bi, _ := debug.ReadBuildInfo()
		fmt.Fprintf(stdout, "hunk %s\n", versionString(bi))
		return exitOK
	}
	// A command flag given an empty command is refused, not read as unset.
	// Until 2026-10-19 it was read that way, each at exit 0: --try '' applied
	// the batch and kept it, where --try promises to put it back; --build ''
	// tried every patch unbuilt; --verify '' kept the batch unchecked. "$CMD"
	// with CMD unset is how one gets written.
	if name := emptyCommand(fs); name != "" {
		why := commandFlags[name]
		if name == "try" && *each {
			why = "there is no command to try each patch with"
		}
		return fail(exitUsage, fmt.Errorf("--%s was given an empty command, so %s; "+
			"if it came from a variable, that variable is empty", name, why), "")
	}
	switch {
	case *each:
		// --each's patches are the positional arguments, so its refusals
		// stand where "unexpected argument" otherwise would (§4.3).
		switch {
		case *try == "" && *verify != "":
			return fail(exitUsage, errors.New("--each needs --try, not --verify: a verify keeps the batch that passes, and --each puts every patch back"), "")
		case *try == "":
			return fail(exitUsage, errors.New("--each tries each patch alone and does nothing without --try CMD"), "")
		case patchFile.path != "":
			return fail(exitUsage, errors.New("--each takes its patches as arguments, not from -f"), "")
		case fs.NArg() == 0:
			return fail(exitUsage, errors.New("--each needs at least one patch file"), "")
		}
		seen := make(map[string]bool, fs.NArg())
		for _, name := range fs.Args() {
			// The flag package stops reading flags at the first file, so a
			// flag after one would be read as a patch named like one, and a
			// run of forty would be one patch short with nothing said.
			if strings.HasPrefix(name, "-") {
				return fail(exitUsage, fmt.Errorf("argument %q begins with \"-\": flags come before --each's patch files", name), "")
			}
			if seen[name] {
				return fail(exitUsage, fmt.Errorf("%s is listed twice", name), "")
			}
			seen[name] = true
		}
	case fs.NArg() > 0:
		// "format" is a subcommand and has to come first, which is what §4's
		// synopsis says. Saying that beats "unexpected argument".
		if fs.Arg(0) == "format" {
			return fail(exitUsage, errors.New(`format is a subcommand and takes no flags; run "hunk format"`), "")
		}
		if fs.Arg(0) == "quote" {
			return fail(exitUsage, errors.New(`quote is a subcommand and comes first, its flags after it; run "hunk quote --help"`), "")
		}
		return fail(exitUsage, fmt.Errorf("unexpected argument %q; the patch comes from stdin or -f", fs.Arg(0)), "")
	}
	// --build runs between a patch and its command, a step only --each has:
	// a single --try or --verify builds first in its own command.
	if *build != "" && !*each {
		return fail(exitUsage, errors.New("--build runs before each patch's command under --try --each, and does nothing without --each; "+
			`one command can build first itself, as in "BUILD && TEST"`), "")
	}

	// --verify-may-format only ever acts during a rollback, and --keep-on-fail
	// means no rollback happens. Together it is not contradictory, it is inert,
	// and a flag that silently does nothing is what this tool refuses
	// everywhere else.
	if *keepOnFail && *verifyFormat {
		return fail(exitUsage, errors.New("--keep-on-fail and --verify-may-format cannot both be set; "+
			"--verify-may-format only acts while rolling back, and --keep-on-fail means not rolling back"), "")
	}
	if *verifyLines < 1 {
		return fail(exitUsage, fmt.Errorf("--verify-lines must be at least 1, not %d", *verifyLines), "")
	}
	// --try always puts the tree back, and --verify keeps it when the check
	// passes, so the pair asks for two outcomes of one run (§4.1).
	if *try != "" && *verify != "" {
		return fail(exitUsage, errors.New("--try and --verify cannot both be set; "+
			"--try always puts the tree back, so there is nothing for --verify to keep"), "")
	}
	if *keepOnFail && *verify == "" {
		return fail(exitUsage, errors.New("--keep-on-fail does nothing without --verify"), "")
	}
	// --verify-may-format governs putting files back, which --try does too.
	if *verifyFormat && *verify == "" && *try == "" {
		return fail(exitUsage, errors.New("--verify-may-format does nothing without --verify or --try"), "")
	}
	// A --verify whose last pipeline ends in tail exits 0 whatever the command
	// in front of it did, so it keeps a failing batch and says "verify ok":
	// issue #80, and §4's flag that would do nothing in its most convincing
	// form. It is refused here, before the patch is read, and under --dry-run
	// too, so that a dry run that passes is not followed by a refusal. The fix
	// is in the error rather than in a hint, because --json carries only the
	// error. A single --try is not read: its tree goes back whatever the
	// command says, and a filter on its output is how it is used.
	if filter, ok := maskingFilter(*verify); ok {
		return fail(exitUsage, fmt.Errorf("--verify cannot see the command piped into %[1]s fail: "+
			"a pipeline's exit status is its last command's, and that is %[1]s; "+
			"drop the pipe, since hunk prints the output only when the verify fails, "+
			"cut to its last lines by --verify-lines (%[2]d), or set -o pipefail in the command", filter, *verifyLines), "")
	}
	// Under --each the run is scored by exits, the command's and the build's,
	// so the reason a single --try is not read does not hold (#101). A command
	// piped into a filter scores every patch as missed; a build piped into one
	// passes a patch that does not build on to the command, which fails on it,
	// and the patch is scored as caught.
	if *each {
		if filter, ok := maskingFilter(*try); ok {
			return fail(exitUsage, fmt.Errorf("--try cannot see the command piped into %[1]s fail under --each: "+
				"a pipeline's exit status is its last command's, and that is %[1]s, so every patch would read as missed; "+
				"drop the pipe, since --verify-lines N prints the last N lines of each patch's output under its line, "+
				"or set -o pipefail in the command", filter), "")
		}
		if filter, ok := maskingFilter(*build); ok {
			return fail(exitUsage, fmt.Errorf("--build cannot see the build piped into %[1]s fail: "+
				"a pipeline's exit status is its last command's, and that is %[1]s, so a patch that does not build would be tried and read as caught; "+
				"drop the pipe, since hunk prints a failing build's last lines, --verify-lines (%[2]d) of them, "+
				"or set -o pipefail in the command", filter, *verifyLines), "")
		}
	}
	timeout, err := parseTimeout(*timeoutFlag, *verify != "" || *try != "")
	if err != nil {
		return fail(exitUsage, err, "")
	}

	opt := Options{Context: *context}
	switch *eol {
	case "auto":
		opt.EOL = EOLAuto
	case "strict":
		opt.EOL = EOLStrict
	default:
		return fail(exitUsage, fmt.Errorf("--eol must be auto or strict, not %q", *eol), "")
	}
	if *marker == "" {
		return fail(exitUsage, errors.New("--marker must not be empty"), "")
	}
	if *context < 1 {
		return fail(exitUsage, fmt.Errorf("--context must be at least 1, not %d", *context), "")
	}

	// --each reads its patches itself, one file each, once the tree is open.
	var p *Patch
	if !*each {
		src, readErr := readPatch(patchFile.path, stdin)
		if readErr != nil {
			return fail(exitUsage, readErr, "")
		}
		var parseErr error
		p, parseErr = Parse(src, *marker)
		if parseErr != nil {
			return fail(exitUsage, parseErr, heredocHint(parseErr, patchFile.path == ""))
		}
		// A line that looks like a mistyped heredoc delimiter is refused
		// before anything is read (#97), every one named, unless the caller
		// says the patch means it. The warning #92 gave came after the write.
		if len(p.Strays) > 0 && !*allowStrays {
			return fail(exitUsage, &MistypedDelimiters{Strays: p.Strays}, "")
		}
	}

	dir := *root
	if dir == "" {
		dir = "."
	}
	tree, err := OpenTree(dir, *allowOutside)
	if err != nil {
		return fail(exitIO, err, "")
	}
	// The root descriptor goes away with the process either way, and a close
	// error here has nothing left to report it to.
	defer func() { _ = tree.Close() }()

	// A --verify or --try command runs through sh. With no sh to find, the
	// command cannot start, and the batch would be written only to be rolled
	// back, rewriting every file for nothing. So sh is looked for first, and a
	// missing one is refused before anything is written. The rollback in
	// runVerify is for whatever this cannot see.
	if name, cmd := commandFlag(*verify, *try); cmd != "" && !*dryRun {
		if _, lookErr := exec.LookPath("sh"); lookErr != nil {
			return fail(exitIO, fmt.Errorf("%s runs its command with sh, which was not found (%v); nothing was written", name, lookErr), "")
		}
	}

	if *each {
		// --verify-lines given, rather than defaulted, asks --each for every
		// command's tail, the caught ones' too (#89).
		linesGiven := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "verify-lines" {
				linesGiven = true
			}
		})
		return runEach(eachConfig{
			names: fs.Args(), command: *try, build: *build, marker: *marker, opt: opt, allowStrays: *allowStrays,
			timeout: timeout, verifyLines: *verifyLines, everyTail: linesGiven, verifyFormat: *verifyFormat,
			dryRun: *dryRun, asJSON: *asJSON, quiet: *quiet,
		}, tree, stdout, stderr, interrupted)
	}

	txn := NewTxn(tree, opt)
	// A signal while the batch is on disk ends the command and puts the batch
	// back, rather than killing hunk where it stands with the batch applied,
	// which is what it did until 2026-10-06, --try's included. The handler goes
	// in once nothing can stop the commit but a signal, and comes out when the
	// batch is kept or back: before then, a signal kills a load that writes
	// nothing, as it always has, and after, it kills a hunk that has finished.
	var sigs chan os.Signal
	if _, cmd := commandFlag(*verify, *try); cmd != "" && !*dryRun {
		sigs = make(chan os.Signal, 1)
		txn.beforeCommit = func() { notifyInterrupts(sigs) }
		defer signal.Stop(sigs)
	}
	stop := Stop{Timeout: timeout, Signals: sigs}
	var res *Result
	if *dryRun {
		res, err = txn.Preview(p)
	} else {
		res, err = txn.Run(p)
	}

	var v *Verify
	switch {
	case err == nil && *try != "" && *dryRun:
		v = &Verify{Command: *try, Try: true}
	case err == nil && *verify != "" && *dryRun:
		// §4: --dry-run writes nothing and runs no verify. The report says so
		// rather than leaving the caller to assume it passed.
		v = &Verify{Command: *verify}
	case err == nil && pending(sigs) != nil:
		// The signal came while the batch was being written. Commit finished,
		// and the command never starts.
		v, err = interruptedBeforeTheCommand(txn, *verify, *try, pending(sigs), *verifyFormat)
	case err == nil && *try != "":
		v, err = runTry(txn, tree, *try, *verifyLines, *verifyFormat, stop)
	case err == nil && *verify != "":
		v, err = runVerify(txn, tree, *verify, *verifyLines, *keepOnFail, *verifyFormat, stop)
	}
	if sigs != nil {
		signal.Stop(sigs)
	}
	if v != nil && v.Signal != nil && interrupted != nil && len(v.NotRestored) == 0 {
		*interrupted = v.Signal
	}

	rep := NewReport(res, err, v, *dryRun, len(p.Hunks))
	rep.Strays = p.Strays
	if *asJSON {
		// --json wins over --quiet: a caller that asked for machine output
		// asked for it on every path. It is alone on stdout.
		if err := rep.JSON(stdout); err != nil {
			// Exit 5 leaves the tree as it was unless the message says
			// otherwise, and here the batch may be on disk.
			if rep.changedTheTree() {
				fmt.Fprintf(stderr, "hunk: the batch is applied, but the --json report could not be written: %v\n", err)
			} else {
				fmt.Fprintf(stderr, "hunk: the --json report could not be written: %v\n", err)
			}
			return exitIO
		}
		return rep.Exit
	}
	rep.Text(stdout, stderr, *quiet)
	return rep.Exit
}

// runVerify is phases 6 and 7: run the command, and put the tree back if it
// failed (§6.1, §6.3).
func runVerify(txn *Txn, tree *Tree, command string, lines int, keep, mayFormat bool, stop Stop) (*Verify, error) {
	v, err := RunVerify(command, tree.Root(), lines, stop)
	if err != nil {
		// Nothing was verified, and --verify keeps a batch only when its
		// check passes, so the batch goes, decided 2026-09-22. Until then it
		// stayed, unverified, at exit 5. --keep-on-fail keeps it, as it keeps
		// a batch whose check failed.
		v = &Verify{Command: command, Applied: txn.Applied()}
		if keep {
			v.Kept = true
			return v, fmt.Errorf("could not run the verify command: %w; the changes are left in place (--keep-on-fail)", err)
		}
		return v, cannotStart("the verify command", "rolled back", err, v, txn, mayFormat)
	}
	if v.OK {
		return v, nil
	}
	v.Applied = txn.Applied()
	if keep {
		// §4: --keep-on-fail leaves the changes and still exits 3. The code
		// reports what the verify said, not what was done about it.
		v.Kept = true
		return v, nil
	}
	v.RolledBack, v.NotRestored, v.Gone, v.Left = txn.Rollback(mayFormat)
	return v, nil
}

// runTry is --try (§4.1): run the command, then put the batch back whatever it
// said. A command that cannot start still gets the batch put back, and is exit
// 5 through the error, reported like any other.
func runTry(txn *Txn, tree *Tree, command string, lines int, mayFormat bool, stop Stop) (*Verify, error) {
	v, err := RunVerify(command, tree.Root(), lines, stop)
	if err != nil {
		v = &Verify{Command: command, Try: true, Applied: txn.Applied()}
		return v, cannotStart("the --try command", "put back", err, v, txn, mayFormat)
	}
	v.Try = true
	if v.TimedOut > 0 {
		v.Status = exitTimedOut
	}
	v.Applied = txn.Applied()
	v.RolledBack, v.NotRestored, v.Gone, v.Left = txn.Rollback(mayFormat)
	return v, nil
}

// cannotStart puts the batch back after a command that could not start, and
// says whether it managed to: it names how many files it could not put back
// rather than claiming it did, and the report lists them.
func cannotStart(what, done string, err error, v *Verify, txn *Txn, mayFormat bool) error {
	return fmt.Errorf("could not run %s: %w; %s", what, err, putBack(txn, v, mayFormat, done))
}

// putBack rolls the batch back after a command that never ran to an end of its
// own, and says how it went, for the message: "the batch was" done, or how
// many files could not be put back, rather than claiming it was.
func putBack(txn *Txn, v *Verify, mayFormat bool, done string) string {
	v.RolledBack, v.NotRestored, v.Gone, v.Left = txn.Rollback(mayFormat)
	if len(v.NotRestored) > 0 {
		return fmt.Sprintf("%s could not be put back", count(len(v.NotRestored), "file"))
	}
	return "the batch was " + done
}

// interruptedBeforeTheCommand puts the batch back after a signal that arrived
// while it was being written, before its command could start.
func interruptedBeforeTheCommand(txn *Txn, verify, try string, sig os.Signal, mayFormat bool) (*Verify, error) {
	name, command := commandFlag(verify, try)
	v := &Verify{Command: command, Try: try != "", Signal: sig, Applied: txn.Applied()}
	return v, fmt.Errorf("interrupted by %s before the %s command ran; %s",
		signalName(sig), name, putBack(txn, v, mayFormat, "put back"))
}

// pending is the signal waiting on sigs, if one is. It is put back, so asking
// twice gives the same answer.
func pending(sigs chan os.Signal) os.Signal {
	select {
	case sig := <-sigs:
		sigs <- sig
		return sig
	default:
		return nil
	}
}

// notifyInterrupts is signal.Notify for the interrupts, as a variable so a
// test can deliver one at the moment the handler goes in, which is the only
// way to land a signal between the handler and the first write every time.
var notifyInterrupts = func(c chan<- os.Signal) { signal.Notify(c, interrupts...) }

// exitTimedOut is --try's exit when --timeout ended its command: 124, as
// timeout(1) exits, so a caller can tell it from anything the command said.
const exitTimedOut = 124

// parseTimeout reads --timeout. Empty means none, which is the default:
// hunk cannot know the limit its caller works under, and a default that
// failed a slow, honest verify would be worse than none. hasCommand is whether
// --verify or --try was given, without which --timeout would do nothing.
func parseTimeout(s string, hasCommand bool) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("--timeout must be a duration with a unit, such as 90s or 5m, not %q", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--timeout must be more than zero, not %q", s)
	}
	if !hasCommand {
		return 0, errors.New("--timeout does nothing without --verify or --try")
	}
	return d, nil
}

// commandFlags is every flag that takes a shell command, each with what it
// would do given an empty one. TestEveryCommandFlagHasEveryGuard reads the
// flags that --help spells with CMD and requires each to be here, and to have
// a row in its table for every guard on a command, refused or let through
// with a reason: a guard written at one flag is how #101 came about, and a
// flag added after it had never been asked.
var commandFlags = map[string]string{
	"verify": "the batch would be kept with nothing run to check it",
	"try":    "the batch would be applied and kept, where --try puts every file back",
	"build":  "every patch would be tried unbuilt",
}

// emptyCommand names the command flag that was given, but given an empty or
// blank command, if one was. A flag left out is not given.
func emptyCommand(fs *flag.FlagSet) (name string) {
	fs.Visit(func(f *flag.Flag) {
		if _, ok := commandFlags[f.Name]; ok && name == "" && strings.TrimSpace(f.Value.String()) == "" {
			name = f.Name
		}
	})
	return name
}

// commandFlag names the flag whose command the batch will run, if either.
func commandFlag(verify, try string) (name, cmd string) {
	if try != "" {
		return "--try", try
	}
	return "--verify", verify
}

// jsonRequested reports whether --json is among the arguments, read without
// the flag package: an unknown flag stops the flag package before it reaches
// a --json that comes after it, and that caller asked for JSON too.
func jsonRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "json" {
			continue
		}
		if !hasVal {
			return true
		}
		b, err := strconv.ParseBool(val)
		return err == nil && b
	}
	return false
}

// heredocHint is the hint for a patch from stdin whose first line is not a
// directive. The usual way to get one is two heredocs on one command line with
// their bodies written in the other order, as in "hunk <<'HUNK' && python3 -
// <<'PY'" with the Python first: the shell feeds each command the body whose
// delimiter comes first, so hunk reads the Python (#63). From -f nothing is
// chained, and a later line is not where a mix-up shows.
func heredocHint(err error, fromStdin bool) string {
	var nd notADirective
	if !fromStdin || !errors.As(err, &nd) || nd.Line != 1 {
		return ""
	}
	return "the patch came from stdin: if two heredocs share its command line, the shell feeds them " +
		"in the order their delimiters appear, so check that the one hunk reads is the patch"
}

// onceFlag is -f: a string flag that refuses to be set twice. As a plain
// flag.String it took the last value silently, so "hunk -f a.hunk -f b.hunk"
// read only b.hunk and exited 0, the silent kind of wrong result this tool
// refuses everywhere else (§4.3).
type onceFlag struct {
	set  bool
	path string
}

func (o *onceFlag) String() string { return o.path }

func (o *onceFlag) Set(v string) error {
	if o.set {
		return fmt.Errorf("already reading %s; one -f is one patch. To try several patches one at a time, use --try CMD --each", o.path)
	}
	o.set, o.path = true, v
	return nil
}

// readPatch reads the patch from -f or from stdin.
//
// -f is relative to the working directory, not to --root: it is the caller's
// file, while --root describes the tree being edited.
func readPatch(file string, stdin io.Reader) ([]byte, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	// A terminal on stdin with no -f means somebody typed "hunk" and is now
	// waiting for a program that is waiting for them. Say so instead.
	if f, ok := stdin.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return nil, fmt.Errorf("no patch: give one on stdin or with -f. " +
				"Run \"hunk format\" for the grammar")
		}
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, fmt.Errorf("the patch is empty. Run \"hunk format\" for the grammar")
	}
	return b, nil
}
