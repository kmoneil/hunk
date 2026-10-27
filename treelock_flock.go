//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var sysFlock = syscall.Flock

// lockTree takes this run's lock on its tree (#128) and returns what lets it
// go, or a *TreeBusyError when another run holds it. A run that writes takes
// the root exclusively and a dry run shares it, so dry runs do not refuse each
// other and nothing reads a tree another run has a batch on. Neither waits: a
// --try holds its tree for as long as its command runs.
//
// Every directory above the root is taken shared, so that a run rooted in a
// directory and one rooted below it refuse each other whichever came first,
// and two runs in sibling directories do not. Only directories the invoker
// owns: the others, / and /tmp among them, are shared with other users, any of
// whom could hold one exclusively and refuse every run beneath it.
//
// A directory that cannot be opened, or a filesystem that has no flock(2), is
// skipped rather than refused. The lock guards hunk against hunk; a tree it
// cannot guard is edited as it was before there was one.
func lockTree(t *Tree, exclusive bool) (release func(), err error) {
	var held []*os.File
	release = func() {
		for _, f := range held {
			_ = f.Close()
		}
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if top, openErr := openTop(t); openErr == nil {
		held = append(held, top)
		if heldByAnother(top, how) {
			release()
			return nil, &TreeBusyError{Dir: t.given}
		}
	}
	uid := os.Getuid()
	for dir := t.root; filepath.Dir(dir) != dir; {
		dir = filepath.Dir(dir)
		f, openErr := os.Open(dir)
		if openErr != nil {
			continue
		}
		if fi, statErr := f.Stat(); statErr != nil || int(fi.Sys().(*syscall.Stat_t).Uid) != uid {
			_ = f.Close()
			continue
		}
		held = append(held, f)
		if heldByAnother(f, syscall.LOCK_SH) {
			release()
			return nil, &TreeBusyError{Dir: dir, Above: true}
		}
	}
	return release, nil
}

// openTop opens the directory the tree works in. Confined, it is opened
// through the root the tree holds, so the lock is on the directory every read
// and write goes through even if its name has since been given to another.
func openTop(t *Tree) (*os.File, error) {
	if t.r != nil {
		return t.r.Open(".")
	}
	return os.Open(t.root)
}

// heldByAnother takes how on f without waiting, and reports whether another
// run holds it. Only EWOULDBLOCK says so: any other failure takes no lock and
// refuses nothing.
func heldByAnother(f *os.File, how int) bool {
	for {
		err := flock(int(f.Fd()), how|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			return errors.Is(err, syscall.EWOULDBLOCK)
		}
	}
}
