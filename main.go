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
	"runtime/debug"
	"strconv"
	"strings"
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
  hunk format                  print the grammar and one worked example

Nothing is written until every hunk in the batch is known to match exactly as
many times as it claimed, and until every target is known to be unchanged since
it was read. A hunk whose text is not there fails the whole batch and the tree
is untouched.

FLAGS

  --verify CMD          Shell command run after a successful apply. Non-zero
                        rolls everything back. Optional: 58% of the measured
                        calls this tool was designed from bundled one, and
                        requiring it would be obnoxious for documentation edits.
  --verify-may-format   On verify failure, restore original bytes even for files
                        the verify command rewrote. Set this when --verify
                        formats. Without it, a file that changed after hunk
                        wrote it is left alone and the exit is 4, because
                        silently reverting another writer's work is the one
                        behaviour that would make this tool dangerous. With it,
                        you are asserting nothing else writes the tree during
                        the verify.
  --verify-lines N      Tail of the verify output printed on failure. (40)
  --keep-on-fail        Leave the applied changes in place when verify fails,
                        for inspection. Still exits 3.
  --try CMD             Apply, run CMD as --verify would, then put every file
                        back whatever it says, and exit with its status. For a
                        print statement or a measurement you do not mean to
                        keep. 4 if a file could not be put back.
  --dry-run             Validate and print the diffstat. Write nothing.
  --root DIR            Resolve relative paths and run --verify here. (cwd)
  --marker STR          Directive prefix. (@@)
  -f FILE               Read the patch from a file instead of stdin.
  --json                Emit the result as one JSON object on stdout.
  --quiet               Print nothing on success. Failures are still reported.
  --context N           Max lines of file text echoed in a near-miss report. (20)
  --eol auto|strict     auto converts payload line endings to the file's
                        dominant ending before matching and on write. (auto)
  --allow-outside-root  Permit paths that resolve outside --root.
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
	os.Exit(cli(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// cli is main with its edges injected, so the exit-code walk can drive it
// in-process and assert the tree state each code implies.
func cli(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "format" {
		if len(args) > 1 {
			fmt.Fprintf(stderr, "hunk: format takes no arguments\n")
			return exitUsage
		}
		fmt.Fprint(stdout, formatDoc)
		return exitOK
	}

	fs := flag.NewFlagSet("hunk", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // the errors are written below, once, to stderr
	fs.Usage = func() {}

	var (
		verify       = fs.String("verify", "", "")
		try          = fs.String("try", "", "")
		verifyFormat = fs.Bool("verify-may-format", false, "")
		verifyLines  = fs.Int("verify-lines", 40, "")
		keepOnFail   = fs.Bool("keep-on-fail", false, "")
		dryRun       = fs.Bool("dry-run", false, "")
		root         = fs.String("root", "", "")
		marker       = fs.String("marker", DefaultMarker, "")
		patchFile    = fs.String("f", "", "")
		asJSON       = fs.Bool("json", false, "")
		quiet        = fs.Bool("quiet", false, "")
		context      = fs.Int("context", DefaultContext, "")
		eol          = fs.String("eol", "auto", "")
		allowOutside = fs.Bool("allow-outside-root", false, "")
		help         = fs.Bool("help", false, "")
		h            = fs.Bool("h", false, "")
		showVersion  = fs.Bool("version", false, "")
	)

	// fail reports an error found before the transaction runs. §4: a caller
	// that passed --json "asked for it on every path", so it gets the object
	// on stdout rather than a line on stderr. Until 2026-09-22 none of these
	// paths printed one. The text is what they printed before, byte for byte.
	wantsJSON := jsonRequested(args)
	fail := func(code int, err error, hint string) int {
		if wantsJSON {
			if werr := (&Report{Exit: code, Err: err}).JSON(stdout); werr != nil {
				fmt.Fprintf(stderr, "hunk: %v\nhunk: the --json report could not be written: %v\n", err, werr)
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
	if fs.NArg() > 0 {
		// "format" is a subcommand and has to come first, which is what §4's
		// synopsis says. Saying that beats "unexpected argument".
		if fs.Arg(0) == "format" {
			return fail(exitUsage, errors.New(`format is a subcommand and takes no flags; run "hunk format"`), "")
		}
		return fail(exitUsage, fmt.Errorf("unexpected argument %q; the patch comes from stdin or -f", fs.Arg(0)), "")
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

	src, err := readPatch(*patchFile, stdin)
	if err != nil {
		return fail(exitUsage, err, "")
	}

	p, err := Parse(src, *marker)
	if err != nil {
		return fail(exitUsage, err, "")
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

	txn := NewTxn(tree, opt)
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
	case err == nil && *try != "":
		v, err = runTry(txn, tree, *try, *verifyLines, *verifyFormat)
	case err == nil && *verify != "":
		if *dryRun {
			// §4: --dry-run writes nothing and runs no verify. The report says
			// so rather than leaving the caller to assume it passed.
			v = &Verify{Command: *verify}
		} else {
			v, err = runVerify(txn, tree, *verify, *verifyLines, *keepOnFail, *verifyFormat)
		}
	}

	rep := NewReport(res, err, v, *dryRun, len(p.Hunks))
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
func runVerify(txn *Txn, tree *Tree, command string, lines int, keep, mayFormat bool) (*Verify, error) {
	v, err := RunVerify(command, tree.Root(), lines)
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
	v.RolledBack, v.NotRestored, v.Gone = txn.Rollback(mayFormat)
	return v, nil
}

// runTry is --try (§4.1): run the command, then put the batch back whatever it
// said. A command that cannot start still gets the batch put back, and is exit
// 5 through the error, reported like any other.
func runTry(txn *Txn, tree *Tree, command string, lines int, mayFormat bool) (*Verify, error) {
	v, err := RunVerify(command, tree.Root(), lines)
	if err != nil {
		v = &Verify{Command: command, Try: true, Applied: txn.Applied()}
		return v, cannotStart("the --try command", "put back", err, v, txn, mayFormat)
	}
	v.Try = true
	v.Applied = txn.Applied()
	v.RolledBack, v.NotRestored, v.Gone = txn.Rollback(mayFormat)
	return v, nil
}

// cannotStart puts the batch back after a command that could not start, and
// says whether it managed to: it names how many files it could not put back
// rather than claiming it did, and the report lists them.
func cannotStart(what, done string, err error, v *Verify, txn *Txn, mayFormat bool) error {
	v.RolledBack, v.NotRestored, v.Gone = txn.Rollback(mayFormat)
	back := "the batch was " + done
	if len(v.NotRestored) > 0 {
		back = fmt.Sprintf("%s could not be put back", count(len(v.NotRestored), "file"))
	}
	return fmt.Errorf("could not run %s: %w; %s", what, err, back)
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
