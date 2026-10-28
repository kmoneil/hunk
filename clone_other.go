//go:build !darwin

package main

import (
	"errors"
	"os"
)

// clones is false where hunk makes no clone of its own (#132). On Linux the
// copy is io.Copy, whose copy_file_range the kernel turns into a clone on
// btrfs and XFS without saying so, which is why the line --isolated prints
// counts no clones here rather than counting none.
const clones = false

// cloneFile is darwin's alone. Here every file is copied.
var cloneFile = func(*os.File, string) error { return errors.ErrUnsupported }
