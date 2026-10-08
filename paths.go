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
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	Root     string // as a message names it, which is Tree.Shown
	Resolved string // where it led, when that is known and different
	Reason   string
}

// Error names the path with any control character in it shown by name, since
// a terminal takes ESC printed raw as an instruction and a CR as a carriage
// return. Path itself stays as written, which is what JSON escapes and a caller
// compares.
func (e *PathRefusal) Error() string { return showControls(e.Path) + ": " + e.Detail() }

// Detail is the message without the leading path, for a report that has already
// printed the path on a line of its own (§5.2).
//
// It carries the root, because the commonest way to reach a path refusal is a
// process that is not in the directory it thinks it is in, and no reason on its
// own can show that. Error is defined in terms of it so the two renderings
// cannot drift: reading Reason directly is how the missing-file refusal came to
// be the only one that would not say where the tool had looked.
//
// A control character anywhere in it is shown by name, as one in Path is: the
// reason can quote a link's destination, which is the tree's bytes, and until
// 2026-10-07 one holding an escape coloured the terminal reading the refusal.
func (e *PathRefusal) Detail() string {
	return showControls(e.detail())
}

func (e *PathRefusal) detail() string {
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
		b.WriteString(" " + resolvedClause(e.Resolved))
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
	// written is the name orig gives before any link is followed or any
	// component respelled: cleaned, and relative to the root when confined.
	// ResolvesTo compares it with name to say where a path led.
	written string
	link    bool // a symlink on the path the patch wrote led here
	// named means the entry the patch named is itself a symlink, as against
	// one in a directory above it. A write goes through either (§6.5), and a
	// delete can go through only the second: through the first it would
	// remove the file the link leads to and leave the link.
	named bool
}

// Orig is the path as the patch wrote it. Reports say that, because it is the
// string the agent wrote and can find in its patch, and where it led beside it
// when that is another name (Tree.ResolvesTo).
func (t Target) Orig() string { return t.orig }

// ViaSymlink reports whether a symlink anywhere on the path the patch wrote led
// to this target. §6.5 writes through the link rather than replacing it.
func (t Target) ViaSymlink() bool { return t.link }

// ResolvesTo is where tg led, as a report shows it, when that is not the name
// the patch wrote: through a symlink, or in a spelling its directory stores
// another way (§6.5, §3.5). It is "" when the path names its file by the
// file's own name, as "./a.txt" and an absolute path under the root do.
//
// Until 2026-10-08 every report named the path as written and nothing else,
// and that is a name git cannot act on: with in.txt -> sub/real.txt,
// "git diff in.txt" is empty and "git checkout in.txt" restores nothing, while
// exit 4 sends the reader to version control for the file it names.
func (t *Tree) ResolvesTo(tg Target) string {
	if got := t.shownName(tg.name); got != t.shownName(tg.written) {
		return got
	}
	return ""
}

// shownName is a name as a report shows it: with slashes, and relative to the
// root wherever it is under it, as a patch would write it. Confined, a name
// already is. Unconfined, it is absolute: under the root by any spelling it is
// shown relative, and outside it stays absolute, since where it went is the
// thing worth seeing. ResolvesTo compares these forms, so a path that reaches
// the root through a link in the root's own path, /tmp on macOS, is not said
// to lead anywhere.
func (t *Tree) shownName(name string) string {
	if t.r == nil {
		if rest, ok := t.underRoot(name); ok {
			name = filepath.Join(rest...)
		}
	}
	return filepath.ToSlash(name)
}

// underRoot is an unconfined name's components under the root. A name is under
// it when a prefix of it is the root's directory, by identity whatever spelling
// reaches it, which is §6.5's rule for a confined absolute path; the two
// spellings the tree already holds are tried as text first, since identity
// stats each prefix. Without identity, a path through another link to the
// root, or a name Respell spelled as its directories store it under a --root
// given in another case, was said to lead to an absolute name.
func (t *Tree) underRoot(name string) ([]string, bool) {
	for _, root := range []string{t.root, t.given} {
		if rest, ok := below(root, name); ok && len(rest) > 0 {
			return rest, true
		}
	}
	if rest, ok := t.within(name); ok && len(rest) > 0 {
		return rest, true
	}
	return nil, false
}

// resolvedClause is the clause a report adds after a path that led somewhere
// else: a refusal, a success row, a file rollback could not put back. One
// wording, so that it means one thing wherever it is read.
func resolvedClause(name string) string { return "(it resolves to " + name + ")" }

// A Tree is the file tree a patch applies to, confined to the root unless
// --allow-outside-root was given.
type Tree struct {
	root  string      // absolute, with symlinks in the root path itself resolved
	given string      // absolute, as the caller gave it, which is where it thinks it is
	r     *os.Root    // nil when unconfined
	top   fs.FileInfo // the root directory, for telling it by identity
	// dotsAsText is the platform's reading of a ".." in a link's destination:
	// as text on Windows, walked on POSIX (see cleanDots). A field rather than
	// a constant so a test on either can ask for the other's rule.
	dotsAsText bool
}

// OpenTree opens root for confined access. The root is resolved once, because
// a symlinked root would otherwise make every path under it look like an
// escape.
func OpenTree(root string, allowOutside bool) (*Tree, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	t := &Tree{root: abs, given: abs, dotsAsText: runtime.GOOS == "windows"}
	// Deliberately named apart from the err above: a failure to resolve is not
	// an error here, it means the root is not a symlink and abs already stands.
	// Shadowing err said the same thing less clearly and govet flagged it.
	if resolved, linkErr := filepath.EvalSymlinks(abs); linkErr == nil {
		t.root = resolved
	}
	if allowOutside {
		// Unconfined, the identity is only for a report: which names are
		// under the root (underRoot). Nil, if it cannot be described, means
		// under it as text alone.
		t.top, _ = os.Stat(t.root)
		return t, nil
	}
	if t.r, err = os.OpenRoot(t.root); err != nil {
		return nil, err
	}
	// A root that cannot be described has no identity to match, so a path is
	// under it only as text, as every path was until 2026-10-07: os.SameFile
	// is false for a nil FileInfo.
	t.top, _ = t.r.Stat(".")
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

// Shown is the root as a refusal names it. A refusal names the root so a
// caller can see where hunk looked, because the commonest way to one is a
// process not in the directory it thinks it is in; until 2026-10-07 it named
// the resolved root alone, so a caller in /tmp/x read "the root is
// /private/tmp/x", which looks like exactly that mistake. So the root as the
// caller gave it, from --root or its working directory, and the resolved form
// after it when the two differ.
func (t *Tree) Shown() string {
	if t.given == t.root {
		return t.root
	}
	return t.given + ", which is " + t.root
}

// Resolve maps a path as written in a patch to the target to operate on.
//
// When the path names a symlink, the target is what the link leads to, so a
// write goes through the link instead of replacing it (§6.5). os.Rename and
// os.Root.Rename both replace a link, so this resolution is what makes the
// difference, not a flag on the write.
func (t *Tree) Resolve(p string) (Target, error) {
	if p == "" {
		return Target{}, &PathRefusal{Path: p, Root: t.Shown(), Reason: "the path is empty"}
	}
	// No file name can hold a NUL byte, since a path reaches the operating
	// system as a string that ends at one. Left to the system call, it was
	// refused only where load happened to look: under a directory the batch
	// creates, load's stat stops at the missing directory, and the name was
	// first refused at the rename, after commit had written the files before
	// it. Fuzzing found that on 2026-09-17.
	if strings.Contains(p, "\x00") {
		return Target{}, &PathRefusal{
			Path: p, Root: t.Shown(),
			Reason: "the path contains a NUL byte, which no filesystem accepts",
		}
	}
	// Nor does anybody mean a control character in one. Until 2026-10-06
	// "@@ create x" with an escape sequence in the name created that file at
	// exit 0, and the report printed the sequence raw, colouring the
	// terminal; a CR left in a path by a CRLF patch named a file that looked
	// like the one meant and was not. Refused here, beside NUL, so it is one
	// refusal for every op, before anything is read.
	if name, ok := firstControl(p); ok {
		return Target{}, &PathRefusal{
			Path: p, Root: t.Shown(),
			Reason: "the path contains " + name +
				", a control character, which hunk does not allow in a file name; remove it from the patch",
		}
	}
	name, err := t.toName(p)
	if err != nil {
		return Target{}, err
	}
	var named bool
	final, viaLink, err := t.resolveLinks(p, name, &named)
	if err != nil {
		return Target{}, err
	}
	// hunk edits the working tree, and .git is not part of it: its config and
	// hooks are things git later runs. Until 2026-10-06 a patch could append to
	// .git/config or rewrite .git/HEAD, by name, through a link, or with the
	// root inside .git. Checked on the resolved name, so no spelling and no
	// link reaches it, and on the absolute form, so neither does a root.
	full := final
	if t.r != nil {
		full = filepath.Join(t.root, final)
	}
	if insideDotGit(full) {
		return Target{}, &PathRefusal{
			Path: p, Root: t.Shown(), Resolved: final,
			Reason: "it is .git or inside it, which is git's own and not the working tree; hunk never edits there",
		}
	}
	return Target{name: final, orig: p, written: name, link: viaLink, named: named}, nil
}

// insideDotGit reports whether any component of name is .git, or a spelling of
// it that some filesystem takes for it.
func insideDotGit(name string) bool {
	sep := func(r rune) bool { return r < 0x80 && os.IsPathSeparator(byte(r)) }
	return slices.ContainsFunc(strings.FieldsFunc(name, sep), isDotGit)
}

// isDotGit is git's own test for a path component that names .git, applied on
// every platform as git applies it, because a tree is not always on the
// filesystem it was made on. Case folds, which covers macOS and Windows. NTFS
// drops trailing dots and spaces, takes a colon as the start of a stream name,
// and gives .git the short name GIT~1. HFS+ ignores a handful of invisible code
// points altogether, so ".g\u200cit" is .git there. None of these is a name
// anybody means as anything else.
func isDotGit(c string) bool {
	for _, prefix := range []string{".git", "git~1"} {
		if len(c) < len(prefix) || !strings.EqualFold(c[:len(prefix)], prefix) {
			continue
		}
		rest := c[len(prefix):]
		if i := strings.IndexByte(rest, ':'); i >= 0 {
			rest = rest[:i]
		}
		if strings.Trim(rest, ". ") == "" {
			return true
		}
	}
	return strings.EqualFold(strings.Map(dropHFSIgnorable, c), ".git")
}

// dropHFSIgnorable is a strings.Map function that removes the code points HFS+
// leaves out of a name when it compares two, the list git's utf8.c keeps.
func dropHFSIgnorable(r rune) rune {
	switch {
	case r >= 0x200c && r <= 0x200f, r >= 0x202a && r <= 0x202e,
		r >= 0x206a && r <= 0x206f, r == 0xfeff:
		return -1
	}
	return r
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
		// §6.5: absolute paths are allowed only under the root. Under it as
		// text, or under the directory it is by another spelling (see within).
		rel, err := filepath.Rel(t.root, filepath.Clean(p))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel, nil
		}
		if rest, ok := t.within(filepath.Clean(p)); ok {
			return t.joinName("", rest, true), nil
		}
		return "", &PathRefusal{
			Path: p, Root: t.Shown(),
			Reason: "an absolute path is allowed only under the root",
		}
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &PathRefusal{
			Path: p, Root: t.Shown(), Resolved: clean,
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
// Lstat'd as it stands before it is walked, and os.Root has the last word on
// whether it leaves the root, with one exception: it refuses an absolute link
// even when the link stays inside, and the walk translates one, wherever it
// stands. Until 2026-10-07 that was a final link only. One in a parent was
// refused with advice to set --allow-outside-root, which opens every path in
// the batch, for a reason given on 2026-09-04 that went on 2026-09-17: that
// translating it would reopen the window os.Root closes, when os.Root still
// followed every parent link at use. It now gets a name with none in it.
//
// So os.Root's refusal is held while the walk goes on to the link that caused
// it, and only translating an absolute link clears it, by os.Root saying yes
// to the translated path. An absolute link out of the root is refused, and so
// is a walk that climbs above the root with the refusal held, through the link
// followed last. While it is held, a link with a ".." after it on the path is
// not followed: os.Root on Windows cleans a ".." as text before it follows
// anything, so it never follows that link, and following it here would put to
// os.Root a path it was never asked about, whose yes is about a different
// file. Every link followed with the refusal held therefore has no ".." after
// it, and whatever climbs came from its destination, which is why the link
// followed last is the one named.
//
// A path it cannot traverse for any other reason, such as a loop, a file used
// as a directory or a permission, comes back as it stands for load to report,
// unless a ".." is still in it.
//
// A ".." still in it came from a link's destination, and only walking it says
// what it removes. Cleaning it as text instead was this fallback until
// 2026-10-06, and it named a file the kernel does not reach: with
// self.link -> self.link/../c.txt, which the kernel cannot open, a delete of
// self.link deleted c.txt. So it is refused. That costs a few links the kernel
// can follow and os.Root will not, such as a ".." past a chain of nine
// directory links, and buys not having a second walker that can be wrong in a
// different way.
//
// named is set when a link is followed standing last on the path, which is
// the entry the patch named, however many directory links were followed to
// reach it.
func (t *Tree) resolveLinks(orig, name string, named *bool) (string, bool, error) {
	base, parts := t.splitName(name)
	via := false
	links := 0
	// The link followed last and what it held, for a refusal that has to say
	// which link it could not get past, or which one led out.
	var link, dest string
	// A final link whose destination's text climbs out of the root. Until
	// 2026-10-06 that text was enough to refuse it, which refused links that
	// stay inside once walked (with x -> sub/deep, x/../../c.txt is c.txt). The
	// walk decides now, and when os.Root agrees that it leaves, the refusal is
	// worded as the text check worded it.
	climbed := ""
	// os.Root's refusal of the path as it last stood, held while the walk goes
	// on to the absolute link that may account for it; nil when it said yes.
	var held error
	for i, walked := 0, false; ; {
		if !walked {
			walked = true
			current := t.joinName(base, parts, false)
			_, err := t.lstat(current)
			held = nil
			if err != nil && escapes(err) {
				held = err
			}
			if err != nil && held == nil && !errors.Is(err, fs.ErrNotExist) {
				if slices.Contains(parts, "..") {
					return "", false, t.cannotFollow(orig, link, dest, err)
				}
				return filepath.Clean(current), via, nil
			}
		}
		if i == len(parts) {
			return t.stop(orig, t.joinName(base, parts, true), via, held, climbed, link, dest)
		}
		switch parts[i] {
		case ".":
			parts = slices.Delete(parts, i, i+1)
			continue
		case "..":
			if i == 0 {
				// Above the top of the walk. Unconfined, that is the top of an
				// absolute path, which is where it stays. Confined, it is the
				// root, and with os.Root's refusal held the path leaves it.
				if held != nil {
					return t.stop(orig, "", via, held, climbed, link, dest)
				}
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
					Path: orig, Root: t.Shown(), Resolved: t.joinName(base, parts, false),
					Reason: "a symlink on it climbs out of a directory that does not exist",
				}
			}
			return t.stop(orig, t.joinName(base, parts, true), via, held, climbed, link, dest)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			i++
			continue
		}
		if links >= maxLinkHops {
			return "", false, &PathRefusal{
				Path: orig, Root: t.Shown(), Resolved: here,
				Reason: "too many symlinks; the chain loops or is absurd",
			}
		}
		links++
		target, err := t.readlink(here)
		if err != nil {
			return "", false, &PathRefusal{
				Path: orig, Root: t.Shown(), Resolved: here,
				Reason: "the symlink could not be read: " + err.Error(),
			}
		}
		via, link, dest = true, here, target
		if i == len(parts)-1 {
			*named = true
			climbed = t.climbsAsText(here, dest)
		}
		absolute, destBase, destParts, inside := t.linkDestination(base, dest)
		switch {
		case !inside && i == len(parts)-1:
			return "", false, &PathRefusal{
				Path: orig, Root: t.Shown(), Resolved: dest,
				Reason: "it is a symlink out of the root",
			}
		case !inside:
			return "", false, t.leaves(orig, climbed, link, dest)
		case held != nil && slices.Contains(parts[i+1:], "..") && climbed != "":
			return "", false, t.leaves(orig, climbed, link, dest)
		case held != nil && slices.Contains(parts[i+1:], ".."):
			return "", false, t.cannotFollow(orig, link, dest, held)
		}
		if absolute {
			base, parts, i = destBase, slices.Concat(destParts, parts[i+1:]), 0
		} else {
			parts = slices.Concat(parts[:i], destParts, parts[i+1:])
		}
		if t.dotsAsText {
			// Windows reads the path with the destination in it as text, so a
			// ".." cancels the component before it, link or not, and the walk
			// starts again on what is left.
			parts, i = cleanDots(parts), 0
		}
		walked = false
	}
}

// cleanDots cancels each ".." against the component before it, as text, and
// drops each ".", keeping a ".." with nothing before it to cancel. It is how
// Windows reads a path a link's destination has been spliced into, which the
// windows-latest probe showed for every link it made: with x -> sub\deep, a
// link to x\..\c.txt opens c.txt there, where POSIX opens sub\c.txt, and until
// 2026-10-07 hunk walked it as POSIX does on both, so on Windows it edited a
// file other than the one Windows opens.
func cleanDots(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		switch {
		case p == ".":
		case p == ".." && len(out) > 0 && out[len(out)-1] != "..":
			out = out[:len(out)-1]
		default:
			out = append(out, p)
		}
	}
	return out
}

// linkDestination turns a symlink's destination into components to put where
// the link stood, reports whether they start from the top instead, and reports
// whether that top is inside the tree, which unconfined it always is.
//
// Confined, an absolute destination gets the check os.Root never makes on it,
// since os.Root refuses every one: one inside the root is translated, wherever
// the link stands on the path, and one that leaves the root is reported, for
// the walk to refuse. Windows has two more destinations that start from a top
// and are not absolute, and read as relative either names a file under the
// link's directory that the link does not lead to, as a final link's did until
// 2026-10-07. One that starts at a separator and names no volume is on the
// link's volume, never the working directory's, which the windows-latest probe
// showed from both of the runner's volumes: confined that is the root's, and
// unconfined the volume the walk is on, base's. One that names a volume and does
// not start at its top is relative to a directory on that volume that nothing
// here knows: confined, below does not find it under the root, and unconfined it
// is spliced as it was. On POSIX neither can be written: a name that starts at a
// separator is absolute, and there is no volume to add.
//
// The translation compares the root's components with the destination's as
// written and splices the rest in raw, ".." and all, for the walk to resolve
// physically like any other. Until 2026-10-06 it went through filepath.Rel,
// which cleans as text, so with x -> sub/deep a link to <root>/x/../c.txt was
// c.txt, where the kernel reads sub/c.txt. A rest that climbs back out of the
// root is refused by os.Root in the Lstat that follows.
func (t *Tree) linkDestination(walkBase, dest string) (absolute bool, base string, parts []string, inside bool) {
	top := filepath.IsAbs(dest) || filepath.VolumeName(dest) != "" || dest != "" && os.IsPathSeparator(dest[0])
	if top && filepath.VolumeName(dest) == "" {
		vol := filepath.VolumeName(t.root)
		if t.r == nil {
			vol = filepath.VolumeName(walkBase)
		}
		dest = vol + dest
	}
	if t.r == nil {
		if filepath.IsAbs(dest) {
			vol := filepath.VolumeName(dest)
			return true, vol + string(filepath.Separator), splitPath(dest[len(vol):]), true
		}
		return false, "", splitPath(dest), true
	}
	if top {
		rest, ok := below(t.root, dest)
		if !ok {
			rest, ok = t.within(dest)
		}
		return true, "", rest, ok
	}
	return false, "", splitPath(dest), true
}

// within finds the root in an absolute path by identity, where the text does
// not: the first prefix of p, from the top, that is the root directory, and the
// components after it as written. Until 2026-10-07 a path was under the root
// only as text, against the root resolved, so one written through another
// spelling of the same directory was refused as outside it (#62): through /tmp
// on macOS, which is /private/tmp, in any mktemp -d directory there, which is
// under /var and so /private/var, in another case where the filesystem folds
// it, or through the caller's own link to the root.
//
// Only directories above the root are looked up by name, and only to tell
// which of them is the root: nothing is opened there, and what follows it is a
// name under the root like any other, walked by resolveLinks and confined by
// os.Root at every use. A prefix that is not there ends the search, since
// nothing under it can be the root.
func (t *Tree) within(p string) ([]string, bool) {
	vol := filepath.VolumeName(p)
	parts := splitPath(p[len(vol):])
	for i := range parts {
		fi, err := os.Stat(t.joinName(vol+string(filepath.Separator), parts[:i+1], false))
		if err != nil {
			return nil, false
		}
		if os.SameFile(fi, t.top) {
			return parts[i+1:], true
		}
	}
	return nil, false
}

// climbsAsText reports where a confined final link's destination leads when
// its text is cleaned, if that is out of the root, and "" if not. It is only
// ever a wording, the one the refusal had when the text decided: os.Root
// decides whether the link leaves.
func (t *Tree) climbsAsText(link, dest string) string {
	if t.r == nil {
		return ""
	}
	shown, rel := dest, ""
	if filepath.IsAbs(dest) {
		rel, _ = filepath.Rel(t.root, filepath.Clean(dest))
	} else {
		rel = filepath.Join(filepath.Dir(link), dest)
		shown = filepath.Join(t.root, rel)
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return shown
}

// below returns p's components under root when root's components begin it, as
// written: neither path is cleaned, so a ".." stays in the rest to be walked,
// and a "." or ".." among the root's own components is a different path and
// is not below it. Components are compared whole, so <root>2/c.txt is not
// under <root>, which a string prefix would say it is.
func below(root, p string) ([]string, bool) {
	rv, pv := filepath.VolumeName(root), filepath.VolumeName(p)
	rp, pp := splitPath(root[len(rv):]), splitPath(p[len(pv):])
	if !sameComponent(rv, pv) || len(pp) < len(rp) {
		return nil, false
	}
	for i := range rp {
		if !sameComponent(rp[i], pp[i]) {
			return nil, false
		}
	}
	return pp[len(rp):], true
}

// sameComponent compares two path components the way filepath.Rel did when it
// made this comparison: folding case on Windows, and exactly elsewhere.
func sameComponent(a, b string) bool {
	return a == b || (componentsFoldCase && strings.EqualFold(a, b))
}

const componentsFoldCase = runtime.GOOS == "windows"

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

// escapes reports whether err is os.Root's refusal of a path that leaves it,
// which says "path escapes from parent". The error is unexported, so it is
// known by its text, and by the text of the innermost error alone: the whole
// message carries the path. Until 2026-10-07 the whole message was searched,
// so a directory named "escapes from parent" made a loop or a file used as a
// directory read as an escape, and under --allow-outside-root as one that
// flag would lift.
func escapes(err error) bool {
	for inner := errors.Unwrap(err); inner != nil; inner = errors.Unwrap(err) {
		err = inner
	}
	return err != nil && err.Error() == "path escapes from parent"
}

// stop ends the walk at name, unless os.Root's refusal of the path is still
// held, when nothing on the walk accounted for it and it stands.
func (t *Tree) stop(orig, name string, via bool, held error, climbed, link, dest string) (string, bool, error) {
	if held != nil {
		return "", false, t.leaves(orig, climbed, link, dest)
	}
	return name, via, nil
}

// leaves is the refusal of a path that leaves the root, naming the link it
// leaves through when there is one, and offering the flag that lets it with
// what the flag costs: it opens every path in the batch, not this one.
//
// A final link whose destination's text climbs out is worded as it was when
// that text decided, naming where the text leads.
func (t *Tree) leaves(orig, climbed, link, dest string) *PathRefusal {
	if climbed != "" {
		return &PathRefusal{
			Path: orig, Root: t.Shown(), Resolved: climbed,
			Reason: "it is a symlink out of the root",
		}
	}
	reason := "it leaves the root"
	if link != "" {
		reason = fmt.Sprintf("the symlink %s -> %s leads out of the root", link, dest)
	}
	return &PathRefusal{
		Path: orig, Root: t.Shown(),
		Reason: reason + "; --allow-outside-root lifts that for every path in the batch, " +
			"so give a path that has to reach outside a batch of its own",
	}
}

// cannotFollow is the refusal of a ".." after a link the walk cannot follow,
// naming the link and the reason, since a ".." has no answer without it.
func (t *Tree) cannotFollow(orig, link, dest string, err error) *PathRefusal {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err // the path it names is the spliced one, which can run to pages
	}
	return &PathRefusal{
		Path: orig, Root: t.Shown(),
		Reason: fmt.Sprintf("the symlink %s -> %s cannot be followed (%v), "+
			"and a .. after a symlink has no answer without following it; name the file itself",
			link, dest, err),
	}
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

// Stat reports whether something is at the target. The mode load records comes
// from ReadTarget instead, with the bytes it describes.
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
// the walk goes. On Linux and macOS a path Resolve could not traverse comes back
// as its walk had it, which differs from the spelling only by a destination
// spliced in where a link stood, and never by a ".." cleaned away, since one
// is refused there. A final link whose destination itself runs through a file
// is the shape that still leaves the two out of step, and the file named is
// then the wrong one. Stat follows a link where resolution stopped, so a link
// to a file is the file in the way.
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

// ReadTarget reads the target and describes it from the same open file, so the
// mode and the bytes are one file's. Load records both, and commit and rollback
// write those bytes back with that mode. Until 2026-10-06 load took them from
// two lookups by name, Stat and then ReadFile, so a file put at the name between
// the two lent its mode to the bytes of the file put back after it.
//
// A directory comes back with its FileInfo and no bytes, for load to refuse,
// and so do a named pipe, a socket and a device, which are not files anybody
// edits. The open does not block: until 2026-10-06 it did, and a named pipe
// with nobody writing to it held every operation in load for ever, while one
// with a writer was read, edited, and then had a regular file renamed over it.
// O_NONBLOCK lets the open of a pipe return, the fstat on that descriptor says
// what it is, and nothing is read from it. On a regular file the flag changes
// nothing, and Windows ignores it and has no pipes in a tree.
//
// A name that will not open is asked about by name only to say what it is, so
// one the user cannot read is still refused as a directory rather than reported
// as an I/O error, and a socket, which no open takes, as a socket. Nothing read
// is described by that answer.
//
// On an error from the open file the bytes are whatever was read, and the
// caller has the error to say they are not the file.
func (t *Tree) ReadTarget(tg Target) (fs.FileInfo, []byte, error) {
	f, err := t.openRead(tg.name)
	if err != nil {
		if fi, serr := t.stat(tg.name); serr == nil && (fi.IsDir() || notAFile(fi) != "") {
			return fi, nil, nil
		}
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() || notAFile(fi) != "" {
		return fi, nil, err
	}
	// Sized from that fstat, as os.ReadFile sizes from its own, so the file is
	// read without regrowing; the spare MinRead is where the read that returns
	// EOF lands.
	size := 0
	if s := fi.Size(); s > 0 && int64(int(s)) == s {
		size = int(s)
	}
	b := bytes.NewBuffer(make([]byte, 0, size+bytes.MinRead))
	_, err = b.ReadFrom(f)
	return fi, b.Bytes(), err
}

func (t *Tree) open(name string) (*os.File, error) {
	if t.r == nil {
		return os.Open(name)
	}
	return t.r.Open(name)
}

// openRead opens name for ReadTarget, without waiting for a named pipe's writer.
func (t *Tree) openRead(name string) (*os.File, error) {
	const flag = os.O_RDONLY | syscall.O_NONBLOCK
	if t.r == nil {
		return os.OpenFile(name, flag, 0)
	}
	return t.r.OpenFile(name, flag, 0)
}

// notAFile names the kind of thing fi is when it is one hunk will not edit
// besides a directory: a named pipe, a socket or a device. "" for anything
// else, which includes a regular file and Windows' irregular reparse points,
// cloud placeholders among them, that people do edit.
func notAFile(fi fs.FileInfo) string {
	switch m := fi.Mode(); {
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&(fs.ModeDevice|fs.ModeCharDevice) != 0:
		return "a device"
	}
	return ""
}

// A MadeDir is a directory MkdirAll made: its name in this tree's terms, and
// what Lstat said it was right after the mkdir, so rollback can tell that
// directory from something put at its name since (§6.6).
type MadeDir struct {
	Name string
	Info fs.FileInfo
}

// MkdirAll creates the target's parent directories, returning those it made so
// rollback can unwind them (§6.6). Deepest first, which is the order to remove
// them in.
//
// Each is recorded with what Lstat said straight after the mkdir. Until
// 2026-10-07 only the name was kept, and rollback removed whatever stood at it
// by then: the verify's file, or its symlink, in place of hunk's directory.
func (t *Tree) MkdirAll(tg Target) ([]MadeDir, error) {
	var made []MadeDir
	missing := missingAbove(filepath.Dir(tg.name), func(d string) bool {
		_, err := t.lstat(d)
		return err == nil
	})
	// missing is deepest first; create shallowest first.
	for i := len(missing) - 1; i >= 0; i-- {
		err := t.mkdir(missing[i])
		var fi fs.FileInfo
		if err == nil {
			fi, err = t.lstat(missing[i])
		}
		if err != nil {
			// Deepest first on this path too. It returned shallowest first
			// until 2026-09-22, which nothing noticed while nothing unwound a
			// failed MkdirAll: rollback stops at the first directory that will
			// not go, and the shallowest will not while it holds the rest. A
			// directory made and then not described is not reported, since
			// rollback could not tell it from whatever replaced it.
			slices.Reverse(made)
			return made, err
		}
		made = append(made, MadeDir{missing[i], fi})
	}
	// Report deepest first, which is removal order.
	slices.Reverse(made)
	return made, nil
}

// missingAbove lists dir and the directories above it that do not exist,
// deepest first, stopping at the first that does, at the top of the tree, and
// at the top of the volume. The last is the stop that matters on Windows: there
// filepath.Dir("E:\\") is "E:\\" itself, so until 2026-10-07 an unconfined
// create on a volume that is not there never reached a stop, and the list grew
// without end, which the windows-latest probe saw as a hang that a dry run of
// the same patch called exit 0. Confined names end at ".", and POSIX at "/",
// which always exists.
func missingAbove(dir string, exists func(string) bool) []string {
	var missing []string
	for d := dir; d != "." && d != "" && filepath.Dir(d) != d; d = filepath.Dir(d) {
		if exists(d) {
			break
		}
		missing = append(missing, d)
	}
	return missing
}

// A seenDir is a directory on a file's path as commit found it, by name and
// by what Lstat said, so rollback can tell it from whatever stands at that
// name once the verify has run.
type seenDir struct {
	name string
	info fs.FileInfo
}

// dirsOn describes every directory on tg's path, shallowest first, as Lstat
// finds it now: those below the root when confined, and every one from the top
// of the volume when not. It stops at the first it cannot describe, which for a
// path commit has just written is none.
func (t *Tree) dirsOn(tg Target) []seenDir {
	base, parts := t.splitName(filepath.Dir(tg.name))
	var out []seenDir
	for i := range parts {
		if parts[i] == "." {
			continue // a file at the top of the root, which has no directory below it
		}
		name := t.joinName(base, parts[:i+1], true)
		fi, err := t.lstat(name)
		if err != nil {
			break
		}
		out = append(out, seenDir{name, fi})
	}
	return out
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
// fsync, chown, chmod, rename (§6.1 step 5).
//
// like is the file being replaced, as load read it, and nil for a file that was
// not there. The new file takes its owner and group as far as keepOwner can
// give them, and then mode, in that order because a chown can clear bits a
// chmod set.
//
// Because Resolve has already followed the final symlink, the rename lands on
// the link's destination and the link survives. Renaming onto the link itself
// would replace it with a regular file, which §6.5 forbids and which both
// os.Rename and os.Root.Rename do by default.
//
// The directory is not fsynced, so a rename is not durable across power loss.
// That is consistent with §6.2, which designs against "the tests failed" rather
// than "the machine lost power" and says so out loud.
//
// The file ends up at exactly mode, whatever the umask: a modify, an overwrite
// and both rollbacks put back the mode load read, on purpose. A new file goes
// through CreateAtomic instead.
func (t *Tree) WriteAtomic(tg Target, data []byte, mode fs.FileMode, like fs.FileInfo) error {
	return t.writeAtomic(tg, data, 0o600, &mode, like)
}

// CreateAtomic writes a file that was not there, the same way, at perm less the
// umask. The temp file is opened at perm and the kernel applies the umask there,
// as it does for a shell redirect or an editor's new file, so a created file is
// no more readable than the user's other new files. Until 2026-10-06 a create
// went through WriteAtomic, whose fchmod the umask does not filter, and was
// 0644 under every umask.
func (t *Tree) CreateAtomic(tg Target, data []byte, perm fs.FileMode) error {
	return t.writeAtomic(tg, data, perm, nil, nil)
}

// writeAtomic opens the temp file at open, which the umask filters, and then,
// when mode is not nil, gives it like's owner and group as far as keepOwner
// can and sets mode exactly, which the umask does not filter. A create has no
// like and no mode, and keeps what the open gave it.
func (t *Tree) writeAtomic(tg Target, data []byte, open fs.FileMode, mode *fs.FileMode, like fs.FileInfo) (err error) {
	if tg.name == "" {
		return errors.New("write to an unresolved target; every path goes through Tree.Resolve")
	}
	dir := filepath.Dir(tg.name)
	tmp, f, err := t.createTemp(dir, open)
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
	if mode != nil {
		if err = f.Chmod(keepOwner(f, like, *mode)); err != nil {
			return err
		}
	}
	if err = f.Close(); err != nil {
		return err
	}
	return t.rename(tmp, tg.name)
}

// fchown is (*os.File).Chown, as a variable so a test can refuse it. keepOwner's
// last resort runs for real only when a file's group is one the invoker is not
// in, which a test can arrange without privilege where a new file takes its
// directory's group and that group is foreign, as /tmp's is on macOS, and not
// at all on Linux. Declared here rather than beside keepOwner so that the test
// that sets it compiles on every platform.
var fchown = (*os.File).Chown

func (t *Tree) createTemp(dir string, perm fs.FileMode) (string, *os.File, error) {
	for i := 0; i < 10000; i++ {
		name := filepath.Join(dir, ".hunk-"+strconv.FormatUint(rand.Uint64(), 36)+".tmp")
		f, err := t.openExcl(name, perm)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("could not create a temp file in %s", dir)
}

func (t *Tree) openExcl(name string, perm fs.FileMode) (*os.File, error) {
	const flag = os.O_RDWR | os.O_CREATE | os.O_EXCL
	if t.r == nil {
		return os.OpenFile(name, flag, perm)
	}
	return t.r.OpenFile(name, flag, perm)
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
	return Target{
		name: s.tree.joinName(base, spelled, true), orig: tg.orig, written: tg.written,
		link: tg.link, named: tg.named,
	}
}

// stored is the spelling dir keeps for c, which exists there as fi.
//
// A name holding a "~" may be an 8.3 alias, which Windows makes by default on
// its system volume: ALONGD~1 for "a long directory name". The directory
// answers to it and does not list it, so it is matched by identity, after case
// and normalization have had their turn so a hard link's own name still wins.
// Until 2026-10-07 it stayed as written, and the windows-latest probe saw a
// batch edit one file through both names, report two files at exit 0, and keep
// only the second edit.
func (s *Spellings) stored(dir, c string, fi fs.FileInfo) string {
	ascii := isASCII([]byte(c))
	alias := strings.Contains(c, "~")
	if ascii && !alias && (flipCase(c) == c || !s.foldsFor(dir, c, fi)) {
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
	if alias {
		for _, n := range names {
			if s.isFile(filepath.Join(dir, n), fi) {
				return n
			}
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
	f, err := t.open(name)
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
