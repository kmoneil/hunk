package main

import (
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Every concession this suite makes to the platform it runs on, in one file, so
// that a reader can find all of them by opening one file rather than by
// grepping for runtime.GOOS.
//
// The suite tests real filesystem behaviour against a real filesystem on
// purpose: symlinks, modes, renames and permission failures are the subject,
// and a mock of os.Rename proves nothing about os.Rename. That decision is what
// makes running on three operating systems worth the minutes, and it is also
// what makes two of them disagree.

// native renders a slash-separated expectation for the running platform.
//
// Tree.toName returns OS-native separators because that is what it hands to
// os.Root, so on Windows an internal name is "sub\\in.txt". A patch always
// contains slashes, so the expectations are written with slashes and converted
// here rather than spelled twice.
//
// This form never reaches anybody: a report prints the path as the patch wrote
// it, which is Target.Orig and is compared without conversion in the same
// tests. If that ever stops being true, these conversions are the wrong fix and
// the separator is leaking into the product.
func native(p string) string { return filepath.FromSlash(p) }

// needsPOSIXPerms skips a test whose subject Windows does not have.
//
// Two kinds of test need it, and both are about permission bits rather than
// about hunk:
//
//   - a test that asserts a file mode survives a write. Windows has no POSIX
//     permission bits for it to survive, and every file reads back 0666.
//   - a test that forces a failure by removing permission from a directory.
//     On Windows chmod does not make a directory unreadable or unwritable, the
//     operation succeeds, and the assertion fires as though the guard under
//     test were missing when what is missing is the way of provoking it.
//
// Skipping is the honest option and it is narrower than it looks: what goes
// untested on Windows is the mode plumbing and the negative paths, not the
// transaction, the matching, the diagnosis or the rollback bookkeeping. The
// alternative was excluding Windows from CI, which would have made the same
// concession without recording it. See issue #1.
func needsPOSIXPerms(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX permission bits, and chmod does not restrict a directory: issue #1")
	}
}

// assertMode checks a permission bit set on the platforms that have one.
//
// Preferred over needsPOSIXPerms wherever the mode is one assertion inside a
// test that checks other things too: skipping the whole test to skip one line
// would take the write cycle and the read-back with it, and those work
// everywhere.
func assertMode(t *testing.T, got fs.FileMode, want fs.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // no POSIX permission bits to assert; see needsPOSIXPerms
	}
	if got.Perm() != want {
		t.Errorf("mode %v, want %v", got.Perm(), want)
	}
}

// notExistPhrase is what the operating system says when a file is not there.
//
// Asserting the exact prose of an OS error is asserting the OS, but dropping
// the assertion would stop checking that the reason reaches the user at all,
// which is the part that is hunk's. So it is selected per platform rather than
// removed.
func notExistPhrase() string {
	if runtime.GOOS == "windows" {
		return "cannot find the path"
	}
	return "no such file"
}

// slashPaths folds path separators in a golden that would otherwise differ by
// platform.
//
// Unlike native() above, this one is about a string that does reach the reader.
// PathRefusal.Resolved is filepath.Clean's output, so where a path genuinely
// resolves somewhere else, the clause naming it renders natively: on Windows
// "sub/../../outside.txt" resolves to "..\outside.txt" and the message says so.
// That is where the path led rather than what the patch wrote, so a native
// separator is defensible there and this stays a test fix. What it costs is
// exactly this: one golden that cannot be compared across platforms unless the
// separators are folded first.
//
// The neighbouring case was not defensible and is not handled here. A path that
// resolved to itself with the slashes turned round printed a clause that said
// nothing, because the check for a redundant clause was a string comparison
// between a native path and a written one. Detail() folds separators before
// deciding now. The Windows job found it against testdata/cli-path-refused.txt
// on this card's first run, which is the argument for these goldens existing.
//
// Applied on every platform rather than only on Windows, so all three compare
// the same bytes. Nothing else in these reports contains a backslash.
func slashPaths(s string) string { return strings.ReplaceAll(s, `\`, "/") }
