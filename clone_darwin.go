//go:build darwin

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// clones says that this system clones a file into the copy rather than
// copying its bytes, so the line --isolated prints counts the clones (#132).
const clones = true

// sysFclonefileat is fclonefileat(2)'s number in the SDK's <sys/syscall.h>.
// The standard library has no wrapper for it, and syscall.Syscall6 on darwin
// traps straight into the kernel, so it is called by its number. Apple does
// not promise the numbers, which is why copyFile takes a clone only once it
// has looked at what the clone made.
const sysFclonefileat = 517

// cloneFile makes to a clone of in, which on APFS shares in's blocks until
// either is written: a 60 GB build cache is copied in seconds and next to no
// space, and the tree's file is safe, since a write to the copy's goes to
// blocks of its own. A clone of a file on another volume, or on one that
// cannot clone (HFS+, exFAT, a network share), is an error, and the file is
// copied. A variable, so that a test can stand in a clone that fails or
// makes the wrong file.
var cloneFile = fclonefile

func fclonefile(in *os.File, to string) error {
	p, err := syscall.BytePtrFromString(to)
	if err != nil {
		return err
	}
	rc, err := in.SyscallConn()
	if err != nil {
		return err
	}
	// AT_FDCWD, which is -2, and as a constant will not convert to a
	// uintptr. to is absolute, so the directory it names does not matter.
	atFdcwd := -2
	var errno syscall.Errno
	err = rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall6(sysFclonefileat, fd, uintptr(atFdcwd), uintptr(unsafe.Pointer(p)), 0, 0, 0)
	})
	if err == nil && errno != 0 {
		err = errno
	}
	return err
}
