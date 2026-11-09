//go:build !unix

package main

import (
	"io/fs"
	"os"
)

// recreatedMode is the mode a file @@ create makes reads back as on Windows,
// where of a mode only the owner's write bit is kept, as the read-only
// attribute's absence: what a file --inverse-to puts back after a delete
// comes back as (#155).
const recreatedMode fs.FileMode = 0o666

// keepOwner keeps nothing where a file has no POSIX owner and group, which here
// means Windows: access there is the file's ACL, and a rewrite takes the one its
// directory passes down, as an editor's save does. See owner_unix.go.
func keepOwner(_ *os.File, _ fs.FileInfo, mode fs.FileMode) fs.FileMode { return mode }
