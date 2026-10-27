//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd)

package main

// sysFlock is nil here: the standard library has no flock(2) on this platform.
var sysFlock func(fd, how int) error

// lockTree takes no lock where there is no flock(2), Windows among them, and
// refuses nothing: two runs at once in one tree are as they were before #128,
// which README says.
func lockTree(*Tree, bool) (func(), error) { return func() {}, nil }
