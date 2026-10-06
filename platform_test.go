package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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

// needsSignals skips a test that kills a process with a signal to see how its
// exit is reported. Windows has no signal for "kill -9 $$" to send, and a
// process there always ends with an exit code.
func needsSignals(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows processes end with an exit code, not a signal")
	}
}

// needsMake skips a test that runs this repository's Makefile. Its recipes are
// POSIX shell and the developer workflow they serve runs on macOS and Linux;
// the Windows job tests the binary, and has neither make nor the sh the
// recipes are written for on its PATH by default.
func needsMake(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the Makefile is POSIX shell and is not part of the Windows job")
	}
}

// brokenShell returns a directory to use as PATH in which exec.LookPath finds
// an sh that cannot run: garbage, marked executable where there is such a
// mark. It is how a verify command that is found and still cannot start is
// reached, which is what hunk's rollback is the backstop for when the check
// for sh before writing passes. Windows finds a program by its extension, not
// a mode bit, so there it is sh.exe.
func brokenShell(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "sh"
	if runtime.GOOS == "windows" {
		name = "sh.exe"
	}
	must(t, os.WriteFile(filepath.Join(dir, name), []byte("not a program\n"), 0o755))
	return dir
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

// groupOf is the group a file belongs to. The tests that ask are POSIX ones and
// call needsPOSIXPerms first, since Windows has no file groups. The field is
// read by name, through reflect, because syscall.Stat_t does not exist on
// Windows and naming it would stop this file compiling there.
func groupOf(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Lstat(path)
	must(t, err)
	gid := reflect.ValueOf(fi.Sys()).Elem().FieldByName("Gid")
	if !gid.IsValid() {
		t.Fatalf("%s: %T has no Gid", path, fi.Sys())
	}
	return int(gid.Uint())
}

// newFileGroup is the group a new file in dir gets: the directory's on macOS and
// the BSDs, and the process's on Linux unless the directory is setgid. A file
// hunk rewrites is a new file in its directory, so this is the group it ends up
// in when hunk does nothing about it.
func newFileGroup(t *testing.T, dir string) int {
	t.Helper()
	probe := filepath.Join(dir, ".probe")
	must(t, os.WriteFile(probe, nil, 0o600))
	defer os.Remove(probe)
	return groupOf(t, probe)
}

// otherGroup is a group the invoker is in that is not but, or skips the test.
// A runner whose user is in one group cannot give a file a group that a rewrite
// would lose, and that is a fact about the runner, not about hunk.
func otherGroup(t *testing.T, but int) int {
	t.Helper()
	groups, err := os.Getgroups()
	must(t, err)
	for _, g := range append(groups, os.Getegid()) {
		if g != but {
			return g
		}
	}
	t.Skipf("the invoker is in no group but %d, so no file can have a group a rewrite would lose", but)
	return 0
}

// inGroup reports whether the invoker is in group g, and so may chown a file to it.
func inGroup(t *testing.T, g int) bool {
	t.Helper()
	groups, err := os.Getgroups()
	must(t, err)
	return g == os.Getegid() || slices.Contains(groups, g)
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

// climbingPastTheTopResolves reports whether the platform walks a symlink whose
// relative destination climbs above the top of the filesystem by staying at the
// top, as POSIX does with "/..".
//
// Windows does not walk it. On the windows-latest runner (run 35231243591),
// Resolve returned such a path unresolved, as written and with no error, and the
// only way it does that is the fallback for a path the operating system will not
// traverse: the Lstat made before the walk failed with something other than "does
// not exist", and load is left to report it. Symlinks otherwise work there, the
// directory links in the same test included, so it is this destination and not
// links. The expectation is what the runner showed rather than a skip, so the
// fallback stays tested on the one platform that takes it.
func climbingPastTheTopResolves() bool { return runtime.GOOS != "windows" }

// dotDotIsWalked reports whether the operating system resolves a ".." in a path
// or a link's destination by walking it, as POSIX does, rather than possibly
// cleaning the text before anything walks it, as Windows does.
//
// Most of what resolveLinks answers rests on os.Root's walk and its own, the
// same Go code on all three platforms, and those rows hold everywhere. Three
// things rest on the operating system instead. Unconfined, the Lstat made
// before the walk is its own, and on Windows "nd.link\..\c.txt" is cleaned to
// "c.txt" before the kernel sees it, so a row pinning the fallback for a ".."
// after a file or a loop has no failure to pin. An absolute link's
// destination is stored by CreateSymbolicLinkW, which may clean it the same
// way, and Readlink returns what was stored. And the kernel's own answer, which
// the rows compare with, is the platform's.
//
// That was a prediction when the rows were written. The first windows-latest
// run, on 2026-10-06, showed more: os.Root on Windows is its own code, and it
// walks the confined links the rows pin as refused too. A ".." after a file
// came back as text cleans it, nine directory links were followed, and a link
// to itself was stopped by resolveLinks' own hop limit with its own words. So
// on Windows the fallback those rows exercise is never reached, and nothing
// pins there whether resolveLinks' answer through a ".." in a link is the file
// Windows itself opens. That question is older than these rows.
func dotDotIsWalked() bool { return runtime.GOOS != "windows" }

// unreadableLink makes a symlink at name that Lstat finds and Readlink cannot
// read, where the platform can, and reports whether it did.
//
// macOS enforces a symlink's own mode on readlink, and "chmod -h 000" sets
// it; the standard library has no lchmod there, so the command is run. Linux
// keeps no mode on a link and Windows has no chmod, so both report false and
// the test skips. It is a probe rather than a GOOS check, like foldsCase,
// because what matters is whether the read fails, not which system said so.
func unreadableLink(tb testing.TB, name string) bool {
	tb.Helper()
	if err := os.Symlink("anything", name); err != nil {
		tb.Fatal(err)
	}
	if exec.Command("chmod", "-h", "000", name).Run() != nil {
		return false
	}
	_, err := os.Readlink(name)
	return err != nil
}

// foldsCase reports whether the filesystem the test's temporary directories are
// on folds case, by making a file and looking it up in the other case.
//
// It is not a concession but a fact the assertions need. What a batch does with
// two spellings of one name has two right answers, and which is right depends
// on the filesystem: macOS and Windows fold by default, as does this machine's
// /workspace, which is the Mac's disk; Linux does not. Running the suite with
// TMPDIR on a folding filesystem exercises the other answer.
func foldsCase(tb testing.TB) bool {
	tb.Helper()
	return sameFileUnderTwoNames(tb, "probe.txt", "PROBE.TXT")
}

// foldsNormalization reports the same for the two Unicode normalizations of
// one name. APFS folds them; NTFS and Linux do not.
func foldsNormalization(tb testing.TB) bool {
	tb.Helper()
	return sameFileUnderTwoNames(tb, "\u00e9.txt", "e\u0301.txt")
}

func sameFileUnderTwoNames(tb testing.TB, made, looked string) bool {
	tb.Helper()
	dir := tb.TempDir()
	if err := os.WriteFile(filepath.Join(dir, made), []byte("probe\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	a, err := os.Lstat(filepath.Join(dir, made))
	if err != nil {
		tb.Fatal(err)
	}
	b, err := os.Lstat(filepath.Join(dir, looked))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		tb.Fatal(err)
	}
	return os.SameFile(a, b)
}
