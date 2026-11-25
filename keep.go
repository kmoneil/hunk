package main

import (
	"fmt"
	"io"
	"os"
)

// keptFile is the file a patch is kept in: written, closed, and named.
type keptFile interface {
	io.WriteCloser
	Name() string
}

// createKept makes the file keepPatch writes: new, under a name no other run
// has, in the temporary directory, and readable by its owner only, which is
// os.CreateTemp's 0600. A variable so that a test can hand back a file whose
// write fails, which a real file in a directory that took the create does not
// do on demand.
var createKept = func() (keptFile, error) {
	f, err := os.CreateTemp("", "hunk-patch-*.hunk")
	if err != nil {
		return nil, err
	}
	return f, nil
}

// keepPatch writes a patch that came on stdin, its bytes as they were read, to
// a new file in the temporary directory, and returns the file's name (#195).
// A batch --verify rolled back has to be sent again with whatever it missed,
// and a heredoc's patch was only ever in the command that sent it: in the
// field a batch of 12 hunks was typed twice to add a thirteenth. The file is
// left for the system's own clearing of the temporary directory, since the
// run that would use it is a later one.
//
// A file the write failed on is removed, since part of a patch, resent, would
// apply part of a batch.
func keepPatch(src []byte) (string, error) {
	f, err := createKept()
	if err != nil {
		return "", fmt.Errorf("in %s: %w", os.TempDir(), cause(err))
	}
	_, err = f.Write(src)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("in %s: %w", f.Name(), cause(err))
	}
	return f.Name(), nil
}
