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
//
// A command that reads a sibling of the tree by a relative path, ../lib, a
// path dependency in build.zig.zon or a go.mod replace, finds nothing in the
// copy, since its ".." is the temporary directory (#140). --isolated-link
// names each such path, and the copy is made under the names the root's own
// directories have, as deep as the deepest of them climbs, with each linked
// beside it at the place the command climbs to. A link reaches the real path,
// for writing too, which is why nothing is linked that is not named, and
// nothing is that is or holds the tree: the command would test the tree, not
// the patch.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// besideLink is one path --isolated-link names (#140), checked against the
// tree by planLinks.
type besideLink struct {
	given  string   // as given, for a refusal
	climbs int      // how many ".." it starts with
	rest   []string // its components after them
	target string   // the root resolved, joined with it: what the link points at
}

// linkRefusal is a path --isolated-link names that cannot be linked: exit 1,
// before anything is read.
type linkRefusal struct{ msg string }

func (e *linkRefusal) Error() string { return e.msg }

func refuseLink(format string, a ...any) error { return &linkRefusal{fmt.Sprintf(format, a...)} }

// planLinks checks the paths --isolated-link names, in the order given, and is
// what isolate links beside the copy. A path is refused unless it leads out of
// the root by "..", names something that is there, neither is nor holds the
// tree, and is neither named twice nor inside another that is named.
func planLinks(t *Tree, given []string) ([]besideLink, error) {
	var links []besideLink
	for _, g := range given {
		l, err := planLink(t, g)
		if err != nil {
			return nil, err
		}
		for _, o := range links {
			inner, outer := l, o
			if _, ok := below(l.target, o.target); ok {
				inner, outer = o, l
			}
			rest, ok := below(outer.target, inner.target)
			switch {
			case !ok:
			case len(rest) > 0:
				return nil, refuseLink("--isolated-link %s is inside %s, which is linked too; name one of them", inner.given, outer.given)
			case o.given == l.given:
				return nil, refuseLink("--isolated-link %s is listed twice", g)
			default:
				return nil, refuseLink("--isolated-link %s and %s are one path", o.given, g)
			}
		}
		links = append(links, l)
	}
	return links, nil
}

// planLink checks one path --isolated-link names. It is taken as the command
// names it from the root, where the command runs, so it is joined with the
// root resolved: that is the directory whose ".." the kernel walks.
func planLink(t *Tree, g string) (besideLink, error) {
	if g == "" {
		return besideLink{}, refuseLink("--isolated-link was given an empty path; if it came from a variable, that variable is empty")
	}
	if filepath.IsAbs(g) || filepath.VolumeName(g) != "" || os.IsPathSeparator(g[0]) {
		return besideLink{}, refuseLink("--isolated-link %s is absolute, and the command reaches it by that name from the copy too; "+
			"it takes a path out of --root, such as ../lib", g)
	}
	parts := splitPath(filepath.Clean(g))
	l := besideLink{given: g}
	for l.climbs < len(parts) && parts[l.climbs] == ".." {
		l.climbs++
	}
	l.rest = parts[l.climbs:]
	if l.climbs == 0 {
		return besideLink{}, refuseLink("--isolated-link %s is in the tree, and so in the copy already", g)
	}
	if l.climbs > len(splitPath(t.root[len(filepath.VolumeName(t.root)):])) {
		return besideLink{}, refuseLink("--isolated-link %s climbs above the top of the filesystem from %s", g, t.Shown())
	}
	l.target = filepath.Join(t.root, g)
	resolved, err := filepath.EvalSymlinks(l.target)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		// ENOTDIR is a path through a file, ../VERSION/x, which Windows
		// calls not found: there is nothing there either way.
		return besideLink{}, refuseLink("--isolated-link %s is %s, which does not exist", g, l.target)
	case err != nil:
		return besideLink{}, refuseLink("--isolated-link %s is %s, which cannot be reached: %v", g, l.target, err)
	}
	switch t.reaches(resolved) {
	case "is":
		return besideLink{}, refuseLink("--isolated-link %s is the tree itself, whose place the copy takes", g)
	case "in":
		return besideLink{}, refuseLink("--isolated-link %s is in the tree, and so in the copy already", g)
	case "holds":
		return besideLink{}, refuseLink("--isolated-link %s holds the tree, and a link to it would reach the tree itself, not the copy", g)
	}
	return l, nil
}

// reaches is how a path out of the root, resolved, would reach the tree if it
// were linked beside the copy: "is" when it is the root, "in" when it is
// under it, "holds" when the root is under it, and "" when it does neither.
// By name and by identity, so that a spelling the filesystem folds to the
// root's, or a link to it, is not taken for somewhere else: the command would
// read the tree through it, which holds no patch, and every patch would read
// as missed.
func (t *Tree) reaches(resolved string) string {
	rest, ok := below(t.root, resolved)
	if !ok {
		rest, ok = t.within(resolved)
	}
	switch {
	case ok && len(rest) == 0:
		return "is"
	case ok:
		return "in"
	}
	// What holds the root is one of the directories above it, by identity,
	// which needs nothing of t.top. One that went since it was resolved has
	// no FileInfo, and nil is the same file as nothing.
	fi, _ := os.Stat(resolved)
	for dir := filepath.Dir(t.root); ; dir = filepath.Dir(dir) {
		if above, err := os.Stat(dir); err == nil && os.SameFile(above, fi) {
			return "holds"
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

// makeLink is os.Symlink, a variable so that a test can stand in a link that
// cannot be made, as one cannot be on Windows without the privilege.
var makeLink = os.Symlink

// besideTheCopy makes, under top, the directories the copy is nested in and
// the links beside it, and is the copy's directory: top itself when nothing
// is linked. The copy is as deep under top as the deepest link climbs, under
// the names the root's own directories have, so that a link's ".." from the
// copy are the root's, and the root's own name, climbed to and back, is the
// copy. The links are made before the copy, so that one that cannot be made
// ends the run before a long copy rather than after.
func besideTheCopy(t *Tree, top string, links []besideLink) (string, error) {
	depth := 0
	for _, l := range links {
		depth = max(depth, l.climbs)
	}
	names := splitPath(t.root[len(filepath.VolumeName(t.root)):])
	nest := names[len(names)-depth:]
	dir := filepath.Join(append([]string{top}, nest...)...)
	// Making a directory in the one MkdirTemp just made fails only as the
	// disk does, and is the copy's error then, as a link's is.
	err := os.MkdirAll(dir, 0o700)
	for i := 0; err == nil && i < len(links); i++ {
		l := links[i]
		place := filepath.Join(slices.Concat([]string{top}, nest[:depth-l.climbs], l.rest)...)
		if err = os.MkdirAll(filepath.Dir(place), 0o700); err == nil {
			err = makeLink(l.target, place)
		}
		if err != nil {
			err = fmt.Errorf("--isolated-link %s: %w", l.given, err)
		}
	}
	return dir, err
}

// isolate copies t's root into a new directory under the temporary directory
// and opens the copy as a Tree that answers for t: a refusal names t's root,
// and an absolute path under t's root is the same name under the copy's.
// What links names is linked beside it (#140). stop is asked before each
// entry, and a signal it returns ends the copy with a *copyInterrupted. made
// is what the copy came to, and remove takes it away, links and all. On an
// error there is no copy left to remove.
func isolate(t *Tree, links []besideLink, stop func() os.Signal) (c *Tree, made copyMade, remove func() error, err error) {
	start := time.Now()
	top, err := os.MkdirTemp("", "hunk-isolated-")
	if err != nil {
		return nil, made, nil, &copyError{err}
	}
	// Resolved, so that the links pointed into it and the tree opened on it
	// spell it one way: on macOS the temporary directory is under /var, which
	// is /private/var.
	if resolved, linkErr := filepath.EvalSymlinks(top); linkErr == nil {
		top = resolved
	}
	remove = func() error { return removeCopy(top) }
	dir, err := besideTheCopy(t, top, links)
	made = copyMade{Dir: dir, Clones: clones}
	if err == nil {
		err = copyRoot(t, top, dir, &made, stop)
	}
	if err == nil {
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
// file copied is counted into made. top is the directory MkdirTemp made,
// which is dst or holds it (#140).
func copyRoot(t *Tree, top, dst string, made *copyMade, stop func() os.Signal) error {
	type dirTimes struct {
		path  string
		mode  fs.FileMode
		mtime time.Time
	}
	var dirs []dirTimes
	// The copy itself, when the temporary directory is under the root: walked
	// into, it would be copied into itself until the disk was full.
	self := ""
	if rel, err := filepath.Rel(t.root, top); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
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
