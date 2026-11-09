package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// inverse.go is --inverse-to (issue #155): the apply writes the patch that
// undoes its batch, before the commit, from what it knows once the batch has
// validated: every file's bytes before and after, and where each hunk
// landed.
//
// hunk invert (#119) prints an inverse from the patch alone, and refuses what
// the patch cannot say how to undo: a deletion, whose text leaves nothing to
// find its place by; a new text the file holds more often than the hunk wrote
// it, which the inverse's count then refuses; a delete, an append, a prepend.
// In the field both of the first two came at undo time, after the edit had
// landed and other runs had gone on top of it, and the second went unnoticed
// and left a planted bug in the tree.
//
// Here the bytes are known, so nothing is refused for want of them. A
// replacement whose inverse would apply as hunk invert writes it is written
// that way. Any other is undone one copy at a time, last first, each copy's
// old text widened by whole lines until the file holds it once. A deletion is
// undone by the lines around it, an append by the text at the file's end, a
// delete by a create of the bytes it took.
//
// Nothing is trusted to that reasoning. The inverse is parsed back and
// validated against the batch's final bytes in memory, by the same Validate
// any patch goes through, and every file has to come out as it was before
// the batch: its bytes, and whether it is there at all. It is first replayed
// under --eol auto, which is how it will usually be applied, and then under
// strict for a file whose mixed endings auto cannot match; the report gives
// the command that applies it, with whichever flags it needed.

// An edit is one hunk as Validate applied it, recorded under --inverse-to.
type edit struct {
	n int // the hunk's number in the batch
	h Hunk
	f *file
	// A replacement's old and new text as written to the file, converted for
	// --eol, and where each copy of old was in the file before it.
	old, repl []byte
	at        []int
	// before is the file as the hunks before this one left it: the bytes a
	// delete took, and for an append or a prepend, its length.
	before []byte
}

// copiesAt is where each of the n copies of old is in b, left to right and
// not overlapping, as bytes.Replace finds them. Validate has shown there are
// exactly n.
func copiesAt(b, old []byte, n int) []int {
	at := make([]int, 0, n)
	for off := 0; len(at) < n; {
		i := off + bytes.Index(b[off:], old)
		at = append(at, i)
		off = i + len(old)
	}
	return at
}

// Inverse is the patch --inverse-to wrote: where, and the arguments that
// apply it, flags first and -f last. Extra says it needed flags the batch's
// own --root and --allow-outside-root do not account for: another --marker,
// --eol strict, --allow-mistyped-delimiter. Those are said under --quiet too,
// since without them the inverse is refused.
//
// Nothing says the batch changed no file, so there was nothing to undo and no
// file was written: a patch with no hunks is refused, so there is none that
// would apply as a no-op.
type Inverse struct {
	Path    string
	Args    []string
	Extra   bool
	Nothing bool
	// written says the file is there; it goes when the batch does not stay.
	written bool
}

// Command is the command line that applies the inverse, its arguments quoted
// for a POSIX shell where they need it.
func (v *Inverse) Command() string {
	parts := []string{"hunk"}
	for _, a := range v.Args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote is s as one shell word: as it is when every byte is one no shell
// reads specially, and otherwise in single quotes, each one inside it closed,
// escaped and reopened.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+=,-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// An InverseError is a batch --inverse-to cannot write an exact inverse for.
// Nothing was written, the inverse included. Exit 1: the batch would apply,
// and it is the flag that cannot be honoured with it.
type InverseError struct {
	Hunk int
	Line int
	Why  string
}

func (e *InverseError) Error() string {
	if e.Hunk == 0 {
		return fmt.Sprintf("--inverse-to cannot write the patch that undoes this batch: %s; nothing was written", e.Why)
	}
	return fmt.Sprintf("--inverse-to cannot undo hunk %d (patch line %d): %s; nothing was written", e.Hunk, e.Line, e.Why)
}

// markerTries is how many markers the inverse tries, the batch's and then
// each with one more @, before it gives up: each is refused only by a file
// holding a line that is a directive under every one before it.
const markerTries = 8

// writeInverse writes the patch that undoes the batch to x.inverseTo, which
// must not exist, and records it in x.inverse for the report. Without
// --inverse-to it does nothing.
func (x *Txn) writeInverse() error {
	if x.inverseTo == "" {
		return nil
	}
	hunks, err := x.inverseHunks()
	if err != nil {
		return err
	}
	if len(hunks) == 0 {
		x.inverse = &Inverse{Path: x.inverseTo, Nothing: true}
		return nil
	}
	if err = x.inverseIsNoTarget(); err != nil {
		return err
	}
	src, args, err := x.inversePatch(hunks)
	if err != nil {
		return err
	}
	out, err := openInverse(x.inverseTo)
	if err != nil {
		return fmt.Errorf("could not write the inverse to %s: %w; nothing was written", x.inverseTo, cause(err))
	}
	_, err = out.Write(src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(x.inverseTo)
		return fmt.Errorf("could not write the inverse to %s: %w; nothing was written", x.inverseTo, cause(err))
	}
	x.inverse = &Inverse{
		Path: x.inverseTo, Args: append(args, "-f", x.inverseTo),
		Extra: len(args) > len(x.inverseArgs), written: true,
	}
	return nil
}

// inverseIsNoTarget refuses an --inverse-to that names a file the batch
// writes. One that exists the CLI has refused already; this is one the batch
// creates, which the inverse written first would stand in the way of, and
// the check would then refuse as a second writer. A name that reaches such a
// file another way, through a link or in another case, still meets that.
func (x *Txn) inverseIsNoTarget() error {
	// A working directory that is gone leaves inv empty, which names no file:
	// the write then says what is wrong.
	inv, _ := filepath.Abs(x.inverseTo)
	// The root is resolved, so the inverse's directory is too: on macOS a
	// temporary directory is /var, which is a link to /private/var.
	if dir, err := filepath.EvalSymlinks(filepath.Dir(inv)); err == nil {
		inv = filepath.Join(dir, filepath.Base(inv))
	}
	for _, f := range x.files {
		name := filepath.FromSlash(f.target.name)
		if !filepath.IsAbs(name) {
			// OpenTree made the root absolute.
			name = filepath.Join(x.tree.Root(), name)
		}
		if f.finalOp() != "" && name == inv {
			return &InverseError{Why: fmt.Sprintf("it names %s, which the batch writes; name a file outside it", showControls(f.target.Orig()))}
		}
	}
	return nil
}

// openInverse makes the inverse's file. O_EXCL: the CLI refused a file that
// was there, and one that has appeared since is somebody's, not this run's to
// overwrite. A variable so that a test can make a write after it fail, which
// a full disk does and nothing a test can arrange on every system does.
var openInverse = func(name string) (io.WriteCloser, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

// kept is the inverse the report gives: one still on disk, or the word that
// none was needed.
func (x *Txn) kept() *Inverse {
	if x.inverse == nil || !x.inverse.written && !x.inverse.Nothing {
		return nil
	}
	return x.inverse
}

// dropInverse takes the inverse back out once the batch is not to stay: an
// inverse of a batch that is not on disk would only be refused, and a file
// left behind would read as an undo still to do. It reports the error of a
// removal that failed, nil if there was nothing to remove.
func (x *Txn) dropInverse() error {
	if x.inverse == nil || !x.inverse.written {
		return nil
	}
	x.inverse.written = false
	if err := os.Remove(x.inverse.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// inverseHunks is every file's edits undone, last first, from the bytes the
// batch leaves: the files in the order the batch first named them.
func (x *Txn) inverseHunks() ([]Hunk, error) {
	var out []Hunk
	for _, f := range x.files {
		if f.finalOp() == "" || (f.finalOp() == "modify" && bytes.Equal(f.cur, f.orig)) {
			// Nothing to undo: untouched, made and removed again, or every
			// change taken back by a later hunk.
			continue
		}
		var mine []edit
		for _, e := range x.edits {
			if e.f == f {
				mine = append(mine, e)
			}
		}
		if f.finalOp() == "delete" && f.mode != recreatedMode {
			last := mine[len(mine)-1]
			return nil, &InverseError{
				Hunk: last.n, Line: last.h.Line,
				Why: fmt.Sprintf("it deletes %s, whose mode is %04o, and the @@ create that would put it back makes a file %04o",
					showControls(f.target.Orig()), f.mode&fs.ModePerm, recreatedMode),
			}
		}
		state := f.cur
		path := f.target.Orig()
		for i := len(mine) - 1; i >= 0; i-- {
			var hs []Hunk
			hs, state = undo(mine[i], path, state)
			out = append(out, hs...)
		}
	}
	return out, nil
}

// undo is the hunks that take state, the file as e left it, back to the file
// as it was before e, and that file.
func undo(e edit, path string, state []byte) ([]Hunk, []byte) {
	switch e.h.Op {
	case OpCreate:
		// The file did not exist before: take the text out, so that a file
		// whose text has changed since is refused rather than deleted, and
		// then delete it.
		var hs []Hunk
		if len(state) > 0 {
			hs = append(hs, Hunk{Op: OpReplace, Path: path, Old: state, New: nil, Count: 1})
		}
		return append(hs, Hunk{Op: OpDelete, Path: path}), nil
	case OpDelete:
		return []Hunk{{Op: OpCreate, Path: path, Body: e.before}}, e.before
	case OpAppend:
		// What append added is everything from the old end on, a line ending
		// it put first included; widen starts the quote at the line that
		// holds the old end, so that line goes whole.
		end := len(e.before)
		a, b := widen(state, end, len(state), true, false)
		return []Hunk{{Op: OpReplace, Path: path, Old: state[a:b], New: state[a:end], Count: 1}}, e.before
	case OpPrepend:
		added := len(state) - len(e.before)
		a, b := widen(state, 0, lineEnd(state, added-1), false, true)
		return []Hunk{{Op: OpReplace, Path: path, Old: state[a:b], New: state[added:b], Count: 1}}, e.before
	}
	return undoReplace(e, path, state)
}

// undoReplace undoes a replacement: as hunk invert writes it where that
// applies, its new text in the file exactly as many times as it wrote it;
// otherwise one copy at a time, last first, so that the copies before it
// stay where they were, each widened until the file holds its old text once.
// A file the replacement emptied has no old text to find: it is deleted and
// made again, which commits as a modify and keeps the file's mode.
func undoReplace(e edit, path string, state []byte) ([]Hunk, []byte) {
	prev := restoreCopies(state, e)
	c := len(e.at)
	switch {
	case len(state) == 0:
		return []Hunk{{Op: OpDelete, Path: path}, {Op: OpCreate, Path: path, Body: prev}}, prev
	case len(e.repl) > 0 && bytes.Count(state, e.repl) == c && !copiesPast(state, e.repl, c):
		return []Hunk{{Op: OpReplace, Path: path, Old: e.repl, New: e.old, Count: c}}, prev
	}
	var hs []Hunk
	cur := state
	d := len(e.repl) - len(e.old)
	for j := c - 1; j >= 0; j-- {
		q := e.at[j] + j*d
		a, b := widen(cur, q, q+len(e.repl), true, true)
		back := slices.Concat(cur[a:q], e.old, cur[q+len(e.repl):b])
		hs = append(hs, Hunk{Op: OpReplace, Path: path, Old: slices.Clone(cur[a:b]), New: back, Count: 1})
		cur = slices.Concat(cur[:a], back, cur[b:])
	}
	return hs, cur
}

// restoreCopies is state with every copy e wrote put back to the text it
// replaced: the file as it was before e.
func restoreCopies(state []byte, e edit) []byte {
	var b bytes.Buffer
	d := len(e.repl) - len(e.old)
	last := 0
	for j, p := range e.at {
		q := p + j*d
		b.Write(state[last:q])
		b.Write(e.old)
		last = q + len(e.repl)
	}
	b.Write(state[last:])
	return b.Bytes()
}

// widen is the region of s to quote for the text at s[lo:hi]: whole lines,
// grown by lines upward where up allows and downward where down does, the
// fewest that leave the region in s once with no copy overlapping. Being in
// s once only gets truer as a region grows, so the fewest is found by
// doubling and then halving, a logarithmic number of scans of s rather than
// one per line, which a file of identical lines would make quadratic. The
// whole of a non-empty s is in s once, so it ends there at the latest.
func widen(s []byte, lo, hi int, up, down bool) (int, int) {
	a := lineStart(s, lo)
	b := len(s)
	switch {
	case hi > lo:
		b = lineEnd(s, hi-1)
	case lo < len(s):
		b = lineEnd(s, lo)
	}
	if a == b && a > 0 {
		// Nothing after the place: the line before it.
		a = lineStart(s, a-1)
	}
	grown := func(k int) (int, int) {
		ga, gb := a, b
		for range k {
			if up && ga > 0 {
				ga = lineStart(s, ga-1)
			}
			if down && gb < len(s) {
				gb = lineEnd(s, gb)
			}
		}
		return ga, gb
	}
	once := func(k int) bool {
		ga, gb := grown(k)
		return !copiesPast(s, s[ga:gb], 1)
	}
	if once(0) {
		return a, b
	}
	hi2 := 1
	for !once(hi2) {
		ga, gb := grown(hi2)
		if (ga == 0 || !up) && (gb == len(s) || !down) {
			// As far as it can go: the whole file, or as much of it as the
			// direction allows, which is then the answer however often it
			// is there. Only the whole file is certain to be there once,
			// and the replay is what refuses the rest.
			return ga, gb
		}
		hi2 *= 2
	}
	lo2 := hi2 / 2
	for lo2+1 < hi2 {
		mid := (lo2 + hi2) / 2
		if once(mid) {
			hi2 = mid
		} else {
			lo2 = mid
		}
	}
	return grown(hi2)
}

// lineStart is where the line that holds s[i] starts; at len(s), the last
// line's start, or len(s) after a final newline.
func lineStart(s []byte, i int) int {
	return bytes.LastIndexByte(s[:i], '\n') + 1
}

// lineEnd is just past the newline that ends the line holding s[i], or len(s)
// for a last line with none, and at len(s).
func lineEnd(s []byte, i int) int {
	if j := bytes.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j + 1
	}
	return len(s)
}

// inversePatch prints hunks as a patch and finds the flags it applies with:
// the batch's marker, or the first after it under which every payload reads
// back as itself; --allow-mistyped-delimiter where a payload holds a line
// the parser takes for a mistyped delimiter; and --eol strict where --eol
// auto does not put every file back as it was.
func (x *Txn) inversePatch(hunks []Hunk) ([]byte, []string, error) {
	marker := x.inverseMarker
	for range markerTries {
		src := printPatch(hunks, false, marker)
		p, err := Parse(src, marker)
		if err == nil && readsBackAs(p.Hunks, hunks) {
			args := slices.Clone(x.inverseArgs)
			if marker != DefaultMarker {
				args = append(args, "--marker", marker)
			}
			if len(p.Strays) > 0 {
				args = append(args, "--allow-mistyped-delimiter")
			}
			switch {
			case x.replays(p, EOLAuto):
			case x.replays(p, EOLStrict):
				args = append(args, "--eol", "strict")
			default:
				return nil, nil, &InverseError{Why: "the patch it wrote does not put every file back as it was, which is a bug in hunk; apply the batch without --inverse-to, and keep a copy"}
			}
			return src, args, nil
		}
		marker += "@"
	}
	return nil, nil, &InverseError{Why: fmt.Sprintf("the files hold lines that read as directives under every marker from %s to %s", x.inverseMarker, marker[:len(marker)-1])}
}

// readsBackAs reports whether the inverse read back is the inverse written.
func readsBackAs(got, want []Hunk) bool {
	return slices.EqualFunc(got, want, func(g, w Hunk) bool {
		return g.Op == w.Op && g.Path == w.Path && g.Count == w.Count &&
			bytes.Equal(g.Old, w.Old) && bytes.Equal(g.New, w.New) && bytes.Equal(g.Body, w.Body)
	})
}

// replays reports whether p, applied under eol to the files as the batch
// leaves them, puts every one back as it was before the batch: its bytes,
// and whether it is there. It reads nothing from disk. The second
// transaction's files are the batch's final bytes, put in its index before
// it loads, so load finds each there by the name it resolves to; a path that
// resolved to any other file would have loaded from disk, and fails the
// replay.
func (x *Txn) replays(p *Patch, eol EOL) bool {
	r := NewTxn(x.tree, Options{Context: x.opt.Context, EOL: eol})
	for _, f := range x.files {
		g := &file{
			target: f.target, resolved: f.resolved, mode: f.mode, eol: "\n",
			notAFile: f.notAFile, inFile: f.inFile, inFileName: f.inFileName,
		}
		if f.present() {
			g.existed = true
			g.orig, g.cur = f.cur, f.cur
			g.eol = dominantEOL(f.cur)
		}
		r.index[f.target.name] = g
		r.files = append(r.files, g)
	}
	if err := r.Load(p); err != nil || len(r.files) != len(x.files) {
		return false
	}
	if len(r.Validate(p)) > 0 {
		return false
	}
	for i, f := range x.files {
		g := r.files[i]
		if g.present() != f.existed || (f.existed && !bytes.Equal(g.cur, f.orig)) {
			return false
		}
	}
	return true
}

// present reports whether the file is there as the hunks so far leave it.
func (f *file) present() bool {
	return (f.existed || f.created) && !f.deleted
}

// line is the report's line for the inverse: the command that applies it, or
// that there was nothing to undo.
func (v *Inverse) line() string {
	if v.Nothing {
		return "nothing to undo: the batch changed nothing, so no inverse was written to " + showControls(v.Path)
	}
	return "to undo: " + v.Command()
}
