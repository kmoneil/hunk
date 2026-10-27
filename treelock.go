package main

// The tree lock, from issue #128. Two --try runs over one file at once each
// took whatever was on disk at load for the original, so the second took the
// first's patch for it: the first could not put its file back and said so at
// exit 4, and the second put the first's patch back as the file's bytes and
// exited 0, with a break in the tree. Two --try runs on different files of one
// tree were wrong too, silently: each command ran against the other's break,
// and under --each a patch was scored by a break that was not its own.
//
// So a run holds its tree from before it reads until it ends, and a run that
// finds it held reads nothing, writes nothing and exits 6. The lock is
// flock(2) on directories: no file is made for it, which keeps §9's promise of
// no state, and the kernel drops it when the process ends, SIGKILL included,
// so a killed run cannot leave its tree locked. That was why a lock was
// declined in 2026-09, when the field had never had two hunk runs in one tree
// at once; this issue is the first.

// TreeBusyError is exit 6: another hunk run holds the tree, so this one read
// nothing and wrote nothing. Dir is the directory whose lock was refused: the
// root, as the caller gave it, or one above it when Above, which another run
// holds as its own root.
type TreeBusyError struct {
	Dir   string
	Above bool
}

func (e *TreeBusyError) Error() string {
	where := e.Dir + " or a directory below it"
	if e.Above {
		where = e.Dir + ", which this tree is in"
	}
	return "another hunk run holds " + where + "; nothing was read or written. " +
		"A --try, --each or --verify holds its tree until its command ends and the files are back: " +
		"wait for that, or stop it, then run this again"
}

// flock is flock(2), as a variable so a test can make it fail the ways a
// filesystem can. Declared here rather than beside lockTree so that the test
// that sets it compiles on every platform; it is nil where there is no
// flock(2), and lockTree there takes no lock.
var flock = sysFlock
