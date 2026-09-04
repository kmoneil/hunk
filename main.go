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
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

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

EXIT CODES

  0  applied, and verify passed if given            tree changed
  1  usage or parse error                           tree untouched
  2  a hunk did not match                           tree untouched
  3  verify failed, rolled back                     tree untouched
  4  verify failed and rollback was incomplete      tree inconsistent
  5  I/O error                                      see the message
  6  a file changed on disk between load and commit tree untouched

Exit 2 is the one to expect routinely and act on without alarm: fix the patch
and resend. The report prints the bytes that are actually in the file, so the
fix is usually a paste rather than a re-read.

WHAT THIS DOES NOT PROMISE

Replacing one file is atomic, via rename(2). The batch is not. A crash or a
full disk part-way through leaves the files before it written, and the check
that guards against a second writer narrows that window to microseconds without
closing it. Both are repaired by the version control the tree is already under.

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
	)

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stderr, "hunk: %v\nRun \"hunk --help\" for the flags.\n", err)
		return exitUsage
	}
	if *help || *h {
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	if fs.NArg() > 0 {
		// "format" is a subcommand and has to come first, which is what §4's
		// synopsis says. Saying that beats "unexpected argument".
		if fs.Arg(0) == "format" {
			fmt.Fprintln(stderr, "hunk: format is a subcommand and takes no flags; run \"hunk format\"")
		} else {
			fmt.Fprintf(stderr, "hunk: unexpected argument %q; the patch comes from stdin or -f\n", fs.Arg(0))
		}
		return exitUsage
	}

	// --verify-may-format only ever acts during a rollback, and --keep-on-fail
	// means no rollback happens. Together it is not contradictory, it is inert,
	// and a flag that silently does nothing is what this tool refuses
	// everywhere else.
	if *keepOnFail && *verifyFormat {
		fmt.Fprintln(stderr, "hunk: --keep-on-fail and --verify-may-format cannot both be set; "+
			"--verify-may-format only acts while rolling back, and --keep-on-fail means not rolling back")
		return exitUsage
	}
	if *verifyLines < 1 {
		fmt.Fprintf(stderr, "hunk: --verify-lines must be at least 1, not %d\n", *verifyLines)
		return exitUsage
	}
	for name, set := range map[string]bool{
		"--verify-may-format": *verifyFormat,
		"--keep-on-fail":      *keepOnFail,
	} {
		if set && *verify == "" {
			fmt.Fprintf(stderr, "hunk: %s does nothing without --verify\n", name)
			return exitUsage
		}
	}

	opt := Options{Context: *context}
	switch *eol {
	case "auto":
		opt.EOL = EOLAuto
	case "strict":
		opt.EOL = EOLStrict
	default:
		fmt.Fprintf(stderr, "hunk: --eol must be auto or strict, not %q\n", *eol)
		return exitUsage
	}
	if *marker == "" {
		fmt.Fprintln(stderr, "hunk: --marker must not be empty")
		return exitUsage
	}
	if *context < 1 {
		fmt.Fprintf(stderr, "hunk: --context must be at least 1, not %d\n", *context)
		return exitUsage
	}

	src, err := readPatch(*patchFile, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "hunk: %v\n", err)
		return exitUsage
	}

	p, err := Parse(src, *marker)
	if err != nil {
		fmt.Fprintf(stderr, "hunk: %v\n", err)
		return exitUsage
	}

	dir := *root
	if dir == "" {
		dir = "."
	}
	tree, err := OpenTree(dir, *allowOutside)
	if err != nil {
		fmt.Fprintf(stderr, "hunk: %v\n", err)
		return exitIO
	}
	defer tree.Close()

	txn := NewTxn(tree, opt)
	var res *Result
	if *dryRun {
		res, err = txn.Preview(p)
	} else {
		res, err = txn.Run(p)
	}

	var v *Verify
	if err == nil && *verify != "" {
		if *dryRun {
			// §4: --dry-run writes nothing and runs no verify. The report says
			// so rather than leaving the caller to assume it passed.
			v = &Verify{Command: *verify}
		} else {
			v, err = runVerify(txn, tree, *verify, *verifyLines, *keepOnFail, *verifyFormat)
			if err != nil {
				fmt.Fprintf(stderr, "hunk: %v\n", err)
				return exitIO
			}
		}
	}

	rep := NewReport(res, err, v, *dryRun, len(p.Hunks))
	if *asJSON {
		// --json wins over --quiet: a caller that asked for machine output
		// asked for it on every path. It is alone on stdout.
		if err := rep.JSON(stdout); err != nil {
			fmt.Fprintf(stderr, "hunk: %v\n", err)
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
		return nil, err
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
	v.RolledBack, v.NotRestored = txn.Rollback(mayFormat)
	return v, nil
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
