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
	"fmt"
	"os"
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

func main() {
	fmt.Fprintln(os.Stderr, "hunk: nothing is implemented yet. See SPEC.md and _plans/progress.md.")
	os.Exit(exitUsage)
}
