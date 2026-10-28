package main

// --isolated, from issue #130. --try writes its batch into the tree and puts
// it back after, so anything else that reads the tree meanwhile reads the patch
// as if it were real: a build in another terminal, an editor's language
// server, a sync to another machine. The tree lock (#128) keeps another hunk
// out and nothing else. The issue was a mutation run beside a long build over
// the same tree: the build compiled against one of the mutants and failed, and
// a build that had shipped would have shipped it.
//
// So --isolated copies the root once, after the preflight has matched the
// patches against the tree itself, and does everything after that in the
// copy: the batch or each patch, the build, the command, the putting back. The
// tree is read once and never written, and its lock is let go once the copy
// is made, so it can be edited while the run goes on. The copy is removed at
// the end, a caught signal included; SIGKILL leaves it in the temporary
// directory.
//
// It copies all of the root, .git and build caches included, so that the
// command sees the tree it would have seen. Modes and modification times come
// with the bytes, since a build that compares times would otherwise take an
// object copied after its source for an up-to-date one. A link whose
// destination is an absolute name under the root is pointed at the same name
// in the copy; any other is copied as written, so one that leads out of the
// root still does. Named pipes, sockets and devices are left out: there is
// nothing in one to copy. So is anything that goes between being listed and
// being read: the issue's own case is a build running in the tree while it is
// copied, and a build's cache comes and goes.
//
// The copy is a clone where the filesystem has one (#132). On APFS each file
// is cloned with fclonefileat, so a tree with a 60 GB build cache is copied in
// seconds into next to no space, and on Linux io.Copy's copy_file_range does
// the same on btrfs and XFS. Where neither can, every byte is copied. Either
// way the run says, once the copy is made, how much it was, how much of it
// was cloned, and how long it took.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// copyMade is what making the copy came to, for the line that says so (#132):
// where it is, how many regular files and bytes, how many of those files were
// cloned rather than copied, and how long it took. Clones says this system
// clones at all, without which Cloned is no news.
type copyMade struct {
	Dir     string
	Files   int
	Bytes   int64
	Cloned  int
	Clones  bool
	Seconds float64
}

// copyError is a copy that could not be made: exit 5, with nothing run.
type copyError struct{ err error }

func (e *copyError) Error() string {
	return fmt.Sprintf("could not copy the tree for --isolated: %v; nothing was run, and the tree is untouched", e.err)
}

func (e *copyError) Unwrap() error { return e.err }

// copyInterrupted is a signal that came while the copy was being made. The
// copy is removed, nothing runs, and hunk dies of the signal.
type copyInterrupted struct{ sig os.Signal }

func (e *copyInterrupted) Error() string {
	return "interrupted by " + signalName(e.sig) + " while copying the tree for --isolated; nothing was run, and the tree is untouched"
}

// isolate copies t's root into a new directory under the temporary directory
// and opens the copy as a Tree that answers for t: a refusal names t's root,
// and an absolute path under t's root is the same name under the copy's.
// stop is asked before each entry, and a signal it returns ends the copy with
// a *copyInterrupted. made is what the copy came to, and remove takes it
// away. On an error there is no copy left to remove.
func isolate(t *Tree, stop func() os.Signal) (c *Tree, made copyMade, remove func() error, err error) {
	start := time.Now()
	dir, err := os.MkdirTemp("", "hunk-isolated-")
	if err != nil {
		return nil, made, nil, &copyError{err}
	}
	// Resolved, so that the links pointed into it and the tree opened on it
	// spell it one way: on macOS the temporary directory is under /var, which
	// is /private/var.
	if resolved, linkErr := filepath.EvalSymlinks(dir); linkErr == nil {
		dir = resolved
	}
	remove = func() error { return removeCopy(dir) }
	made = copyMade{Dir: dir, Clones: clones}
	if err = copyRoot(t, dir, &made, stop); err == nil {
		c, err = OpenTree(dir, false)
	}
	if err != nil {
		_ = remove()
		var ci *copyInterrupted
		if !errors.As(err, &ci) {
			err = &copyError{err}
		}
		return nil, copyMade{}, nil, err
	}
	made.Seconds = time.Since(start).Seconds()
	c.copyOf = t
	return c, made, remove, nil
}

// copyRoot copies everything under t's root into dst, which exists and is
// empty, through t's os.Root so that no link in the tree takes the copy
// outside it. Directories get their modes and times last, deepest first, once
// nothing more is written in them. The top one keeps the 0700 MkdirTemp gave
// it rather than the root's: under a shared /tmp the root's 0755 would show
// the tree to every user of the machine, where its own parents may not. Each
// file copied is counted into made.
func copyRoot(t *Tree, dst string, made *copyMade, stop func() os.Signal) error {
	type dirTimes struct {
		path  string
		mode  fs.FileMode
		mtime time.Time
	}
	var dirs []dirTimes
	// The copy itself, when the temporary directory is under the root: walked
	// into, it would be copied into itself until the disk was full.
	self := ""
	if rel, err := filepath.Rel(t.root, dst); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		self = filepath.ToSlash(rel)
	}
	err := fs.WalkDir(t.r.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return skipGone(err)
		}
		if s := stop(); s != nil {
			return &copyInterrupted{s}
		}
		if name == self {
			return fs.SkipDir
		}
		to := filepath.Join(dst, filepath.FromSlash(name))
		switch d.Type() {
		case fs.ModeDir:
			// Asked afresh rather than of d, which may hold what the listing
			// said: a directory gone since is left out, not made empty.
			fi, err := t.r.Lstat(filepath.FromSlash(name))
			if err != nil {
				return skipGone(err)
			}
			if name != "." {
				if err := os.Mkdir(to, 0o700); err != nil {
					return err
				}
				dirs = append(dirs, dirTimes{to, fi.Mode() & keptMode, fi.ModTime()})
			}
			return nil
		case fs.ModeSymlink:
			dest, err := t.r.Readlink(filepath.FromSlash(name))
			if err != nil {
				return skipGone(err)
			}
			return os.Symlink(intoCopy(t, dst, dest), to)
		case 0:
			return copyFile(t, filepath.FromSlash(name), to, made)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0 && err == nil; i-- {
		err = errors.Join(os.Chmod(dirs[i].path, dirs[i].mode), os.Chtimes(dirs[i].path, time.Time{}, dirs[i].mtime))
	}
	return err
}

// skipGone is nil for an entry that went between being listed and being read,
// which is left out, and the error for anything else, which ends the copy.
func skipGone(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// copyFile copies one regular file, its mode and modification time with it,
// as a clone where it can be one, and counts it into made. It is opened as
// load opens a file, without waiting on a named pipe, and one that is no
// longer a regular file by the time it is open is left out as a pipe in the
// listing is.
func copyFile(t *Tree, name, to string, made *copyMade) error {
	in, err := t.openRead(name)
	if err != nil {
		return skipGone(err)
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return err
	}
	size, cloned := fi.Size(), cloneChecked(in, to, fi.Size())
	if !cloned {
		size, err = copyBytes(in, to)
	}
	if err == nil {
		err = os.Chmod(to, fi.Mode()&keptMode)
	}
	if err == nil {
		err = os.Chtimes(to, time.Time{}, fi.ModTime())
	}
	if err == nil {
		made.Files++
		made.Bytes += size
		if cloned {
			made.Cloned++
		}
	}
	return err
}

// cloneChecked is whether to has been made a clone of in, which is size
// bytes. A clone is taken only once it is seen to be a regular file of that
// size: the clone is a syscall called by a number nobody promised, and a
// number that came to mean something else must not leave a wrong file in the
// copy. Anything else it made is taken away, and the file is copied as it is
// on a system that cannot clone. So is one that grew between being opened
// and being cloned, which a build's cache can.
func cloneChecked(in *os.File, to string, size int64) bool {
	if cloneFile(in, to) != nil {
		return false
	}
	if fi, err := os.Lstat(to); err == nil && fi.Mode().IsRegular() && fi.Size() == size {
		return true
	}
	_ = os.Remove(to)
	return false
}

// copyBytes copies in's bytes into a new file at to, and is how many there
// were.
func copyBytes(in *os.File, to string) (int64, error) {
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// intoCopy is a link's destination as the copy holds it. One that names a
// place under t's root by an absolute name, resolved or as the caller gave
// it, names the same place under dst, its components as written so that the
// kernel walks them as it would have; any other is unchanged.
func intoCopy(t *Tree, dst, dest string) string {
	if !filepath.IsAbs(dest) {
		return dest
	}
	for _, root := range []string{t.root, t.given} {
		if rest, ok := below(root, dest); ok {
			return strings.Join(append([]string{dst}, rest...), string(filepath.Separator))
		}
	}
	return dest
}

// removeCopy removes the copy. A directory left without write permission,
// as Go leaves its module cache, stops os.RemoveAll from emptying it, so on a
// failure every directory is made writable and it is tried once more. A
// directory is visited before it is read, so one that could not be read is
// readable by the time the walk reads it.
func removeCopy(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// removeOrWarn removes the copy at the end of a run, and says so when it
// cannot: the tree is not affected, and the exit is the run's, but a copy of
// it is still on disk for somebody to delete.
func removeOrWarn(remove func() error, stderr io.Writer) {
	if err := remove(); err != nil {
		fmt.Fprintf(stderr, "warning: the copy of the tree --isolated made could not all be removed: %v\n", err)
	}
}
