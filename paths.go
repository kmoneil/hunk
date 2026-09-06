package main

// Path safety (§6.5).
//
// Every path in a patch comes from a payload the tool did not write, and this
// file is the only thing between that and a write outside the tree.
//
// The confinement is os.Root, not a lexical check. A check-then-use design
// validates a path and then operates on it by name, and a symlink swapped in
// between defeats it; os.Root resolves each component against a held directory
// descriptor and has no such gap. What os.Root does not do is §6.5's
// write-through, and it refuses an absolute symlink even when it points inside
// the root, so the final component is resolved here.

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxLinkHops bounds symlink chain resolution. The kernel uses 40; this is a
// source tree, and a chain that long is a loop or an attack.
const maxLinkHops = 32

// A PathRefusal is a path the tree would not touch. It is a validation failure
// (exit 2), not an I/O error, because the fix is in the patch: §6.1 draws that
// line at "can the agent fix it by editing the patch", and this it can.
//
// The message names what the path resolved to as well as what was written.
// An agent that sees only "refused: vendor/x.go" cannot tell why; the whole
// point is that some link in the chain pointed somewhere else.
type PathRefusal struct {
	Path     string // as written in the patch
	Root     string
	Resolved string // where it led, when that is known and different
	Reason   string
}

func (e *PathRefusal) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Path, e.Reason)
	if e.Resolved != "" && e.Resolved != e.Path {
		fmt.Fprintf(&b, " (it resolves to %s)", e.Resolved)
	}
	fmt.Fprintf(&b, "; the root is %s", e.Root)
	return b.String()
}

// A Target is a location the Tree has already checked. Only Tree.Resolve makes
// one, so no Tree method can be handed a path that was never checked. That is
// the guarantee, and it is structural rather than a rule somebody follows.
type Target struct {
	name string // relative to the tree root when confined, absolute when not
	orig string // as written in the patch, for messages
	link bool   // the patch named a symlink and this is what it led to
}

// Orig is the path as the patch wrote it. Reports say that, not the resolved
// name, because that is the string the agent can act on.
func (t Target) Orig() string { return t.orig }

// ViaSymlink reports whether the patch named a symlink that this target is the
// destination of. §6.5 writes through the link rather than replacing it.
func (t Target) ViaSymlink() bool { return t.link }

// A Tree is the file tree a patch applies to, confined to the root unless
// --allow-outside-root was given.
type Tree struct {
	root string   // absolute, with symlinks in the root path itself resolved
	r    *os.Root // nil when unconfined
}

// OpenTree opens root for confined access. The root is resolved once, because
// a symlinked root would otherwise make every path under it look like an
// escape.
func OpenTree(root string, allowOutside bool) (*Tree, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Deliberately named apart from the err above: a failure to resolve is not
	// an error here, it means the root is not a symlink and abs already stands.
	// Shadowing err said the same thing less clearly and govet flagged it.
	if resolved, linkErr := filepath.EvalSymlinks(abs); linkErr == nil {
		abs = resolved
	}
	t := &Tree{root: abs}
	if allowOutside {
		return t, nil
	}
	t.r, err = os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tree) Close() error {
	if t.r != nil {
		return t.r.Close()
	}
	return nil
}

// Root is the resolved root directory.
func (t *Tree) Root() string { return t.root }

// Resolve maps a path as written in a patch to the target to operate on.
//
// When the path names a symlink, the target is what the link leads to, so a
// write goes through the link instead of replacing it (§6.5). os.Rename and
// os.Root.Rename both replace a link, so this resolution is what makes the
// difference, not a flag on the write.
func (t *Tree) Resolve(p string) (Target, error) {
	if p == "" {
		return Target{}, &PathRefusal{Path: p, Root: t.root, Reason: "the path is empty"}
	}
	name, err := t.toName(p)
	if err != nil {
		return Target{}, err
	}
	final, viaLink, err := t.followFinalLink(p, name)
	if err != nil {
		return Target{}, err
	}
	return Target{name: final, orig: p, link: viaLink}, nil
}

// toName brings a patch path into the form this tree's methods take: relative
// to the root when confined, absolute when not.
func (t *Tree) toName(p string) (string, error) {
	if t.r == nil {
		if filepath.IsAbs(p) {
			return filepath.Clean(p), nil
		}
		return filepath.Join(t.root, p), nil
	}
	if filepath.IsAbs(p) {
		// §6.5: absolute paths are allowed only under the root.
		rel, err := filepath.Rel(t.root, filepath.Clean(p))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", &PathRefusal{
				Path: p, Root: t.root,
				Reason: "an absolute path is allowed only under the root",
			}
		}
		return rel, nil
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &PathRefusal{
			Path: p, Root: t.root, Resolved: clean,
			Reason: "the path climbs out of the root",
		}
	}
	return clean, nil
}

// followFinalLink resolves a chain of symlinks at the named location, which is
// §6.5's "a target that is itself a symlink". It exists because os.Root does
// two things this needs undone: it refuses an absolute symlink even when the
// target is inside the root, and its Rename replaces a link rather than
// writing through it.
//
// Symlinks in intermediate components are left to os.Root, which follows the
// relative ones and refuses the rest. Reimplementing its traversal to also
// accept absolute intermediate links would hand back the check-then-use window
// that using os.Root is what closes.
func (t *Tree) followFinalLink(orig, name string) (string, bool, error) {
	via := false
	for hop := 0; ; hop++ {
		if hop >= maxLinkHops {
			return "", false, &PathRefusal{
				Path: orig, Root: t.root, Resolved: name,
				Reason: "too many symlinks; the chain loops or is absurd",
			}
		}
		fi, err := t.lstat(name)
		if err != nil {
			// A path that does not exist yet is not an error here: @@ create
			// makes one, and whether its absence is legal is §6.1's call, not
			// this file's.
			if errors.Is(err, fs.ErrNotExist) {
				return name, via, nil
			}
			if refusal := t.refusalFor(orig, name, err); refusal != nil {
				return "", false, refusal
			}
			return name, via, nil
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return name, via, nil
		}
		dest, err := t.readlink(name)
		if err != nil {
			return "", false, &PathRefusal{
				Path: orig, Root: t.root, Resolved: name,
				Reason: "the symlink could not be read: " + err.Error(),
			}
		}
		via = true
		next, err := t.relinkTarget(orig, name, dest)
		if err != nil {
			return "", false, err
		}
		name = next
	}
}

// relinkTarget turns a symlink's destination into a name in this tree's terms,
// refusing one that leaves the root.
func (t *Tree) relinkTarget(orig, link, dest string) (string, error) {
	if t.r == nil {
		if filepath.IsAbs(dest) {
			return filepath.Clean(dest), nil
		}
		return filepath.Join(filepath.Dir(link), dest), nil
	}
	if filepath.IsAbs(dest) {
		// The case os.Root refuses outright even when it stays inside.
		rel, err := filepath.Rel(t.root, filepath.Clean(dest))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", &PathRefusal{
				Path: orig, Root: t.root, Resolved: dest,
				Reason: "it is a symlink out of the root",
			}
		}
		return rel, nil
	}
	joined := filepath.Join(filepath.Dir(link), dest)
	if joined == ".." || strings.HasPrefix(joined, ".."+string(filepath.Separator)) {
		return "", &PathRefusal{
			Path: orig, Root: t.root,
			Resolved: filepath.Join(t.root, filepath.Dir(link), dest),
			Reason:   "it is a symlink out of the root",
		}
	}
	return joined, nil
}

// refusalFor turns an os.Root escape into a refusal an agent can act on.
// os.Root says "path escapes from parent", which names neither the root nor
// what the path led to.
func (t *Tree) refusalFor(orig, name string, err error) *PathRefusal {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return nil
	}
	if strings.Contains(err.Error(), "escapes from parent") {
		return &PathRefusal{
			Path: orig, Root: t.root, Resolved: name,
			Reason: "it leaves the root, through a symlink or a parent reference; " +
				"an absolute symlink in a parent directory is refused even when its target is inside, " +
				"and --allow-outside-root lifts both",
		}
	}
	return nil
}

func (t *Tree) lstat(name string) (fs.FileInfo, error) {
	if t.r == nil {
		return os.Lstat(name)
	}
	return t.r.Lstat(name)
}

func (t *Tree) readlink(name string) (string, error) {
	if t.r == nil {
		return os.Readlink(name)
	}
	return t.r.Readlink(name)
}

// Stat reports the target's mode. Rollback restores it, so it is read at load
// rather than guessed at commit.
func (t *Tree) Stat(tg Target) (fs.FileInfo, error) {
	if t.r == nil {
		return os.Stat(tg.name)
	}
	return t.r.Stat(tg.name)
}

// ReadFile reads the target.
func (t *Tree) ReadFile(tg Target) ([]byte, error) {
	if t.r == nil {
		return os.ReadFile(tg.name)
	}
	return t.r.ReadFile(tg.name)
}

// MkdirAll creates the target's parent directories, returning those it made so
// rollback can unwind them (§6.6). Deepest first, which is the order to remove
// them in.
func (t *Tree) MkdirAll(tg Target) ([]string, error) {
	var made []string
	dir := filepath.Dir(tg.name)
	var missing []string
	for d := dir; d != "." && d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := t.lstat(d); err == nil {
			break
		}
		missing = append(missing, d)
	}
	// missing is deepest first; create shallowest first.
	for i := len(missing) - 1; i >= 0; i-- {
		if err := t.mkdir(missing[i]); err != nil {
			return made, err
		}
		made = append(made, missing[i])
	}
	// Report deepest first, which is removal order.
	for i, j := 0, len(made)-1; i < j; i, j = i+1, j-1 {
		made[i], made[j] = made[j], made[i]
	}
	return made, nil
}

func (t *Tree) mkdir(name string) error {
	if t.r == nil {
		return os.Mkdir(name, 0o755)
	}
	return t.r.Mkdir(name, 0o755)
}

// Remove deletes the target. Used by rollback of a create, and by @@ delete.
func (t *Tree) Remove(tg Target) error {
	if t.r == nil {
		return os.Remove(tg.name)
	}
	return t.r.Remove(tg.name)
}

// RemoveDir removes a directory this tool created, by the name MkdirAll
// returned. It is not a Target because no patch named it.
//
// The name is already in this tree's terms, the mirror of mkdir: root-relative
// when confined, absolute when not. Joining the root onto it again doubled the
// path unconfined, which no test executed until one did.
func (t *Tree) RemoveDir(name string) error {
	if t.r == nil {
		return os.Remove(name)
	}
	return t.r.Remove(name)
}

// WriteAtomic replaces the target's contents: temp file in the same directory,
// fsync, chmod, rename (§6.1 step 5).
//
// Because Resolve has already followed the final symlink, the rename lands on
// the link's destination and the link survives. Renaming onto the link itself
// would replace it with a regular file, which §6.5 forbids and which both
// os.Rename and os.Root.Rename do by default.
//
// The directory is not fsynced, so a rename is not durable across power loss.
// That is consistent with §6.2, which designs against "the tests failed" rather
// than "the machine lost power" and says so out loud.
func (t *Tree) WriteAtomic(tg Target, data []byte, mode fs.FileMode) (err error) {
	if tg.name == "" {
		return errors.New("write to an unresolved target; every path goes through Tree.Resolve")
	}
	dir := filepath.Dir(tg.name)
	tmp, f, err := t.createTemp(dir)
	if err != nil {
		return err
	}
	// One cleanup for every way the write can fail, so the temp file cannot
	// outlive a partial write down any path. A second Close after a failed one
	// returns ErrClosed and is harmless.
	defer func() {
		if err != nil {
			_ = f.Close()
			t.removeName(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return t.rename(tmp, tg.name)
}

func (t *Tree) createTemp(dir string) (string, *os.File, error) {
	for i := 0; i < 10000; i++ {
		name := filepath.Join(dir, ".hunk-"+strconv.FormatUint(rand.Uint64(), 36)+".tmp")
		f, err := t.openExcl(name)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("could not create a temp file in %s", dir)
}

func (t *Tree) openExcl(name string) (*os.File, error) {
	const flag = os.O_RDWR | os.O_CREATE | os.O_EXCL
	if t.r == nil {
		return os.OpenFile(name, flag, 0o600)
	}
	return t.r.OpenFile(name, flag, 0o600)
}

func (t *Tree) rename(from, to string) error {
	if t.r == nil {
		return os.Rename(from, to)
	}
	return t.r.Rename(from, to)
}

// removeName deletes the temp file a failed write left behind. Best effort on
// purpose: the write is already returning its own error, and a cleanup failure
// stacked on top of it is not the one worth reporting.
func (t *Tree) removeName(name string) {
	if t.r == nil {
		_ = os.Remove(name)
		return
	}
	_ = t.r.Remove(name)
}
