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
// the root, so links are resolved here: every link on a path, which is also what
// gives one file one name.

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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

func (e *PathRefusal) Error() string { return e.Path + ": " + e.Detail() }

// Detail is the message without the leading path, for a report that has already
// printed the path on a line of its own (§5.2).
//
// It carries the root, because the commonest way to reach a path refusal is a
// process that is not in the directory it thinks it is in, and no reason on its
// own can show that. Error is defined in terms of it so the two renderings
// cannot drift: reading Reason directly is how the missing-file refusal came to
// be the only one that would not say where the tool had looked.
func (e *PathRefusal) Detail() string {
	var b strings.Builder
	b.WriteString(e.Reason)
	// Compared with the separators folded, because the clause exists to say
	// where a path led when that is somewhere else. filepath.Clean returns a
	// native path, so on Windows "../outside.txt" resolves to "..\outside.txt"
	// and a plain string comparison printed "(it resolves to ..\outside.txt)"
	// about the path the caller had just written. ToSlash is identity on
	// POSIX, where a backslash is an ordinary character in a filename and must
	// keep telling two paths apart.
	if e.Resolved != "" && filepath.ToSlash(e.Resolved) != filepath.ToSlash(e.Path) {
		fmt.Fprintf(&b, " (it resolves to %s)", e.Resolved)
	}
	fmt.Fprintf(&b, "; the root is %s", e.Root)
	return b.String()
}

// A Target is a location the Tree has already checked. Only Tree.Resolve makes
// one, and Spellings.Respell only respells one it was given, so no Tree method
// can be handed a path that was never checked. That is the guarantee, and it is
// structural rather than a rule somebody follows.
type Target struct {
	name string // relative to the tree root when confined, absolute when not
	orig string // as written in the patch, for messages
	link bool   // a symlink on the path the patch wrote led here
}

// Orig is the path as the patch wrote it. Reports say that, not the resolved
// name, because that is the string the agent can act on.
func (t Target) Orig() string { return t.orig }

// ViaSymlink reports whether a symlink anywhere on the path the patch wrote led
// to this target. §6.5 writes through the link rather than replacing it.
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
	// No file name can hold a NUL byte, since a path reaches the operating
	// system as a string that ends at one. Left to the system call, it was
	// refused only where load happened to look: under a directory the batch
	// creates, load's stat stops at the missing directory, and the name was
	// first refused at the rename, after commit had written the files before
	// it. Fuzzing found that on 2026-09-17.
	if strings.Contains(p, "\x00") {
		return Target{}, &PathRefusal{
			Path: p, Root: t.root,
			Reason: "the path contains a NUL byte, which no filesystem accepts",
		}
	}
	name, err := t.toName(p)
	if err != nil {
		return Target{}, err
	}
	final, viaLink, err := t.resolveLinks(p, name)
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

// resolveLinks resolves every symlink on name, in whichever component it is, and
// returns the name with none left in it, so that one file has one name however
// a patch spelled it (§3.5).
//
// §6.5's "a target that is itself a symlink" is the final component, and until
// 2026-09-17 that was the only one resolved here; os.Root followed the rest at
// use. So with d -> sub, sub/b.go and d/b.go were two names for one file: two
// entries in the load index, two writes, and a lost edit reported as applied.
// Everything that reads Target.name needs the one name, which is the load
// index, the conflict check, commit and rollback, so it is made here once.
//
// The walk is os.Root's own, so the two cannot disagree about which file a
// spelling is. A link is replaced by its destination's components where it
// stood, and a ".." removes the component before it, which by then is a
// directory that exists. That is the physical answer, and cleaning the joined
// text is not: with x -> sub/deep, "x/../in.txt" is sub/in.txt, not in.txt.
//
// os.Root still confines every use, and this opens nothing. Each path is
// Lstat'd as it stands before it is walked, so an escape or an absolute link in
// a parent is refused in os.Root's words, exactly as before. A path it cannot
// traverse for any other reason, such as a loop, a file used as a directory or
// a permission, comes back as it stands for load to report.
func (t *Tree) resolveLinks(orig, name string) (string, bool, error) {
	base, parts := t.splitName(name)
	via := false
	links := 0
	for i, walked := 0, false; ; {
		if !walked {
			walked = true
			current := t.joinName(base, parts, false)
			if _, err := t.lstat(current); err != nil && !errors.Is(err, fs.ErrNotExist) {
				if refusal := t.refusalFor(orig, filepath.Clean(current), err); refusal != nil {
					return "", false, refusal
				}
				return filepath.Clean(current), via, nil
			}
		}
		if i == len(parts) {
			return t.joinName(base, parts, true), via, nil
		}
		switch parts[i] {
		case ".":
			parts = slices.Delete(parts, i, i+1)
			continue
		case "..":
			if i == 0 {
				// Above the top of an absolute path, which is where it stays.
				// Confined, os.Root has already refused the path that got here.
				parts = slices.Delete(parts, 0, 1)
				continue
			}
			parts = slices.Delete(parts, i-1, i+1)
			i--
			continue
		}
		here := t.joinName(base, parts[:i+1], true)
		fi, err := t.lstat(here)
		if err != nil {
			// Nothing under a directory that does not exist is a link, so the
			// rest is the name. A ".." in the rest came from a link's
			// destination and climbs out of the missing directory, which has
			// no answer: the kernel stops at the missing directory too.
			if slices.Contains(parts[i+1:], "..") {
				return "", false, &PathRefusal{
					Path: orig, Root: t.root, Resolved: t.joinName(base, parts, false),
					Reason: "a symlink on it climbs out of a directory that does not exist",
				}
			}
			return t.joinName(base, parts, true), via, nil
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			i++
			continue
		}
		if links >= maxLinkHops {
			return "", false, &PathRefusal{
				Path: orig, Root: t.root, Resolved: here,
				Reason: "too many symlinks; the chain loops or is absurd",
			}
		}
		links++
		dest, err := t.readlink(here)
		if err != nil {
			return "", false, &PathRefusal{
				Path: orig, Root: t.root, Resolved: here,
				Reason: "the symlink could not be read: " + err.Error(),
			}
		}
		via = true
		absolute, destBase, destParts, err := t.linkDestination(orig, here, dest, i == len(parts)-1)
		if err != nil {
			return "", false, err
		}
		if absolute {
			base, parts, i = destBase, slices.Concat(destParts, parts[i+1:]), 0
		} else {
			parts = slices.Concat(parts[:i], destParts, parts[i+1:])
		}
		walked = false
	}
}

// linkDestination turns a symlink's destination into components to put where
// the link stood, and reports whether they start from the top instead.
//
// A final link gets the checks os.Root never makes on it, since Lstat does not
// follow the last component: an absolute destination inside the root is
// translated, which os.Root refuses even there, and one that leaves the root is
// refused, as is a relative one whose text climbs out. A link in a parent was
// already followed by os.Root in the Lstat before this walk, which refused it
// if it escaped or was absolute.
func (t *Tree) linkDestination(orig, link, dest string, final bool) (bool, string, []string, error) {
	if t.r == nil {
		if filepath.IsAbs(dest) {
			vol := filepath.VolumeName(dest)
			return true, vol + string(filepath.Separator), splitPath(dest[len(vol):]), nil
		}
		return false, "", splitPath(dest), nil
	}
	if filepath.IsAbs(dest) {
		rel, err := filepath.Rel(t.root, filepath.Clean(dest))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return false, "", nil, &PathRefusal{
				Path: orig, Root: t.root, Resolved: dest,
				Reason: "it is a symlink out of the root",
			}
		}
		return true, "", splitPath(rel), nil
	}
	if joined := filepath.Join(filepath.Dir(link), dest); final &&
		(joined == ".." || strings.HasPrefix(joined, ".."+string(filepath.Separator))) {
		return false, "", nil, &PathRefusal{
			Path: orig, Root: t.root,
			Resolved: filepath.Join(t.root, filepath.Dir(link), dest),
			Reason:   "it is a symlink out of the root",
		}
	}
	return false, "", splitPath(dest), nil
}

// splitName breaks a name in this tree's terms into the part the walk never
// leaves, "" when confined and the volume's top when not, and its components.
func (t *Tree) splitName(name string) (string, []string) {
	if t.r != nil {
		return "", splitPath(name)
	}
	vol := filepath.VolumeName(name)
	return vol + string(filepath.Separator), splitPath(name[len(vol):])
}

// joinName puts a name back together. Cleaned, it is a name the rest of the
// tree takes; raw, it keeps a ".." still to be walked, for os.Root to Lstat.
func (t *Tree) joinName(base string, parts []string, clean bool) string {
	name := strings.Join(parts, string(filepath.Separator))
	if clean {
		name = filepath.Join(parts...)
	}
	if base == "" && name == "" {
		return "."
	}
	return base + name
}

// splitPath breaks p at every separator, dropping empty components.
func splitPath(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator })
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
	return t.stat(tg.name)
}

func (t *Tree) stat(name string) (fs.FileInfo, error) {
	if t.r == nil {
		return os.Stat(name)
	}
	return t.r.Stat(name)
}

// FileAbove reports the file a path runs through, if it runs through one: the
// nearest directory above tg that exists, when that is not a directory. It
// returns the file as the patch spelled it, for a report, and as the tree names
// it, for comparing with other targets.
//
// The directories are asked rather than the error read, because the error
// differs by platform. Linux and macOS say ENOTDIR for a path through a file,
// and Windows says the path does not exist, which reads as a file that is
// merely absent.
//
// It walks tg's name and its spelling in the patch in step. The two can differ
// only where Resolve replaced a link with its destination, and the file in the
// way is always below the link that led to it, so they stay in step as far as
// the walk goes. On Linux and macOS they do not differ at all: Resolve returns
// a path it could not traverse as it stands. Stat follows a link where
// resolution stopped, so a link to a file is the file in the way.
func (t *Tree) FileAbove(tg Target) (shown, name string, ok bool) {
	name, spelled := tg.name, filepath.Clean(filepath.FromSlash(tg.orig))
	for {
		parent := filepath.Dir(name)
		if parent == name || parent == "." {
			return "", "", false
		}
		name, spelled = parent, filepath.Dir(spelled)
		fi, err := t.stat(name)
		if err != nil {
			continue // absent or unreachable: the answer is further up
		}
		if fi.IsDir() {
			return "", "", false
		}
		return filepath.ToSlash(spelled), name, true
	}
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
			// Deepest first on this path too. It returned shallowest first
			// until 2026-09-22, which nothing noticed while nothing unwound a
			// failed MkdirAll: rollback stops at the first directory that will
			// not go, and the shallowest will not while it holds the rest.
			slices.Reverse(made)
			return made, err
		}
		made = append(made, missing[i])
	}
	// Report deepest first, which is removal order.
	slices.Reverse(made)
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

// Spellings gives each file one spelling for the length of a batch, on a
// filesystem that folds case or Unicode normalization.
//
// Resolve gives a file one name through its links, but on such a filesystem
// A.go and a.go are one file under two names, and until 2026-09-17 the load
// index kept them apart: a replace under each lost an edit under exit 0. A
// component that exists is respelled the way its directory stores it. That is
// exact for case and for normalization, it keeps a hard link's two names apart
// because both are stored, and it means a write goes through the stored name.
// A component the batch will create has no stored spelling yet, so it takes the
// batch's first spelling of it wherever its directory folds. Two normalizations
// of such a name cannot be matched without Unicode tables, which go.mod
// excludes, and stay two names: a limit, decided 2026-09-17.
//
// Whether a directory folds is asked of the directory rather than assumed of the
// platform, because Windows and Linux can fold one directory and not the next:
// a name stat'd in the other case is the same file or it is not. A directory is
// read only where that answer is yes, and once per batch.
type Spellings struct {
	tree  *Tree
	folds map[string]bool     // by directory: whether it folds case
	names map[string][]string // by directory: its names as stored
	first map[string]string   // by parent and folded name: the batch's first spelling of a name not there yet
}

// Spellings returns the spellings for one batch.
func (t *Tree) Spellings() *Spellings {
	return &Spellings{tree: t, folds: map[string]bool{}, names: map[string][]string{}, first: map[string]string{}}
}

// Respell returns tg with each component spelled as its directory stores it, or,
// for one that does not exist yet, as this batch first spelled it where its
// directory folds. It names the same file as tg; only the spelling can change.
func (s *Spellings) Respell(tg Target) Target {
	base, parts := s.tree.splitName(tg.name)
	spelled := make([]string, 0, len(parts))
	existing := "" // the deepest directory that exists, which answers for those below it
	exists := true
	for _, c := range parts {
		dir := s.tree.joinName(base, spelled, true)
		if exists {
			if fi, err := s.tree.lstat(filepath.Join(dir, c)); err == nil {
				spelled = append(spelled, s.stored(dir, c, fi))
				continue
			}
			exists, existing = false, dir
		}
		spelled = append(spelled, s.firstSpelling(existing, dir, c))
	}
	return Target{name: s.tree.joinName(base, spelled, true), orig: tg.orig, link: tg.link}
}

// stored is the spelling dir keeps for c, which exists there as fi.
func (s *Spellings) stored(dir, c string, fi fs.FileInfo) string {
	ascii := isASCII([]byte(c))
	if ascii && (flipCase(c) == c || !s.foldsFor(dir, c, fi)) {
		// No letter to fold, or a directory that does not fold: the name that
		// was found is the name stored. One that is not ASCII can still be
		// stored in another normalization where case does not fold.
		return c
	}
	names := s.list(dir)
	if slices.Contains(names, c) {
		return c
	}
	for _, n := range names {
		if (strings.EqualFold(n, c) || (!ascii && !isASCII([]byte(n)))) && s.isFile(filepath.Join(dir, n), fi) {
			return n
		}
	}
	return c
}

// foldsFor reports whether dir folds case, asked of c, which exists there as fi
// and has a letter: stat'd in the other case, it is the same file or it is not.
func (s *Spellings) foldsFor(dir, c string, fi fs.FileInfo) bool {
	if v, ok := s.folds[dir]; ok {
		return v
	}
	v := s.isFile(filepath.Join(dir, flipCase(c)), fi)
	s.folds[dir] = v
	return v
}

// firstSpelling is the spelling this batch first used for c, a name not yet in
// dir, where the directory that decides it folds. existing is dir, or the
// deepest directory above it that exists, whose filesystem a new one shares.
func (s *Spellings) firstSpelling(existing, dir, c string) string {
	key := dir + "\x00" + foldKey(c)
	first, ok := s.first[key]
	switch {
	case !ok:
		s.first[key] = c
		return c
	case first == c || !s.foldsDir(existing):
		return c
	}
	return first
}

// foldsDir reports whether dir, which exists, folds case, for a name not in it
// yet. It asks a name in dir that has a letter, and failing that dir's own name
// in its parent. A directory with nothing to ask is assumed to fold (Kevin,
// 2026-09-17): two spellings become one name and the second create is refused,
// which is recoverable, where two names could lose a file.
func (s *Spellings) foldsDir(dir string) bool {
	if v, ok := s.folds[dir]; ok {
		return v
	}
	v := true
	if name, fi, ok := s.askable(dir); ok {
		v = s.isFile(filepath.Join(filepath.Dir(name), flipCase(filepath.Base(name))), fi)
	}
	s.folds[dir] = v
	return v
}

// askable finds a name that can say whether dir folds: one in dir with a
// letter, or dir itself when its own name has one.
func (s *Spellings) askable(dir string) (string, fs.FileInfo, bool) {
	for _, n := range s.list(dir) {
		if flipCase(n) == n {
			continue
		}
		name := filepath.Join(dir, n)
		if fi, err := s.tree.lstat(name); err == nil {
			return name, fi, true
		}
	}
	if base := filepath.Base(dir); flipCase(base) != base {
		if fi, err := s.tree.lstat(dir); err == nil {
			return dir, fi, true
		}
	}
	return "", nil, false
}

// isFile reports whether name exists and is the same file as fi.
func (s *Spellings) isFile(name string, fi fs.FileInfo) bool {
	other, err := s.tree.lstat(name)
	return err == nil && os.SameFile(other, fi)
}

// list is dir's names as the directory stores them, read once a batch and
// sorted, so which listed name is tried first does not depend on the
// filesystem's order. A directory that cannot be read lists nothing, and names
// in it stay as written.
func (s *Spellings) list(dir string) []string {
	if names, ok := s.names[dir]; ok {
		return names
	}
	names, _ := s.tree.readDirNames(dir)
	slices.Sort(names)
	s.names[dir] = names
	return names
}

// readDirNames lists a directory's names as it stores them.
func (t *Tree) readDirNames(name string) ([]string, error) {
	var f *os.File
	var err error
	if t.r == nil {
		f, err = os.Open(name)
	} else {
		f, err = t.r.Open(name)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

// flipCase swaps the case of every letter in s.
func flipCase(s string) string {
	return strings.Map(func(r rune) rune {
		if u := unicode.ToUpper(r); u != r {
			return u
		}
		return unicode.ToLower(r)
	}, s)
}

// foldKey is s with each letter replaced by the least rune it folds with, so
// two names are strings.EqualFold exactly when their keys are equal.
func foldKey(s string) string {
	if !utf8.ValidString(s) {
		return s
	}
	return strings.Map(func(r rune) rune {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		return least
	}, s)
}
