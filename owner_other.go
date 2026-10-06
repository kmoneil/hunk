//go:build !unix

package main

import (
	"io/fs"
	"os"
)

// keepOwner keeps nothing where a file has no POSIX owner and group, which here
// means Windows: access there is the file's ACL, and a rewrite takes the one its
// directory passes down, as an editor's save does. See owner_unix.go.
func keepOwner(_ *os.File, _ fs.FileInfo, mode fs.FileMode) fs.FileMode { return mode }
