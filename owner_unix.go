//go:build unix

package main

import (
	"io/fs"
	"os"
	"syscall"
)

// keepOwner gives the file being written, f, the owner and group of the file it
// replaces, as far as the invoker may, and returns the mode to set on it after.
// like is that file as load read it, from the descriptor its bytes came through,
// and nil for a file that was not there, which keeps whatever a new file gets.
//
// Every write is a new file renamed over the old one, so until 2026-10-06 the
// file came out the invoker's, in the directory's group or the invoker's, with
// the old mode applied to whichever group that was: a 0640 file in a small
// group became 0640 in a large one, after an edit and after a rollback that
// reported the tree put back. Decided that day, in the order GNU sed tries and
// with vim's last resort:
//
//   - the owner and the group, which for another user's file only root can do;
//   - the group alone, which needs the invoker to be in it;
//   - else the group gets the bits "other" has, so that nobody in the group the
//     file now has can do more than anybody could. Never a refusal: sed -i
//     makes the same edit without complaint.
//
// A filesystem with no owners refuses both chowns, and there the group is
// usually already the right one, so the last resort is only for a group that
// actually differs. The owner of another user's file is not kept without
// privilege, and ACLs and extended attributes are not kept at all: the standard
// library has no way to copy them on macOS. README says so.
func keepOwner(f *os.File, like fs.FileInfo, mode fs.FileMode) fs.FileMode {
	if like == nil {
		return mode
	}
	want := like.Sys().(*syscall.Stat_t)
	if fchown(f, int(want.Uid), int(want.Gid)) == nil || fchown(f, -1, int(want.Gid)) == nil {
		return mode
	}
	if fi, err := f.Stat(); err == nil && fi.Sys().(*syscall.Stat_t).Gid == want.Gid {
		return mode
	}
	return mode&^0o070 | (mode&0o007)<<3
}
