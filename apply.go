package main

// The transaction (§6.1, §6.2, §6.4).
//
// §1 names the missing primitive and it is this file: "The missing primitive is
// not string replacement. It is the transaction."
//
// The phase order is the design. Nothing is written until every hunk is known
// to match and every target is known to be unchanged since it was read:
//
//	1. Parse    syntax error, exit 1, nothing read       (patch.go)
//	2. Load     read each file once; bytes, mode, SHA-256
//	3. Validate one walk, in order, in memory; a mismatch marks the file
//	            failed and skips its later hunks (§3.5)
//	4. Check    re-hash every target; any difference, exit 6, nothing written
//	5. Commit   temp, fsync, chmod, rename, per file          (paths.go)
//	6. Verify   and 7. Roll back are verify-and-rollback's card
//
// The batch is not atomic and the tool says so in --help rather than
// pretending otherwise (§6.2). Per-file replacement is atomic via rename(2); a
// crash between file three and file four leaves three files written. Neither
// gap has been observed, and both are repaired by the version control the tree
// is already under.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"time"
)

// EOL is the --eol mode (§6.4).
type EOL int

const (
	// EOLAuto converts a payload to the file's dominant line ending before
	// matching, and converts new the same way on write. Translation, not fuzzy
	// matching: a heredoc from an agent's shell is always LF, so without it a
	// CRLF file could not be edited at all.
	EOLAuto EOL = iota
	// EOLStrict matches bytes as given.
	EOLStrict
)

// Options are the knobs the transaction reads. The rest of §4's flags belong to
// later phases or to the CLI.
type Options struct {
	EOL EOL
	// Context caps the lines of file text echoed in a near-miss report (§4's
	// --context, default 20). Zero means the default.
	Context int
}

// DefaultContext is §4's default for --context. §11 keeps it at 20: the span is
// bounded by the length of old, so the cap rarely binds.
const DefaultContext = 20

func (o Options) context() int {
	if o.Context <= 0 {
		return DefaultContext
	}
	return o.Context
}

// diagnoseLimit is §7.2's cap of ten diagnosed hunks per batch.
const diagnoseLimit = 10

// A Failure is one hunk that did not apply, or one skipped because an earlier
// hunk against the same file did not. Both text and JSON reports render from
// this, so §5.2's promise that "--json carries everything the text does" is
// structural rather than maintained by hand.
type Failure struct {
	Hunk      int    // 1-based index in the patch
	Path      string // as the patch wrote it, which is what the agent can act on
	PatchLine int    // §5.2 prints "(patch line 9)"
	Expected  int    // occurrences the hunk claimed
	Found     int    // occurrences actually present

	// SkippedAfter is the hunk that failed first against this file. Non-zero
	// means this hunk was never evaluated, and Expected and Found are unset.
	SkippedAfter int

	// Near explains the miss (§7). Nil for a skipped hunk, and nil past
	// §7.2's cap of ten diagnosed hunks per batch.
	Near *Diagnosis
}

// Skipped reports whether this hunk was never evaluated (§5.2 reports those
// separately, so the agent fixes one real mismatch rather than chasing
// phantoms).
func (f Failure) Skipped() bool { return f.SkippedAfter != 0 }

// A FileResult is one line of §5.1's success output.
type FileResult struct {
	Path    string
	Op      string // "modify"; "create" and "delete" arrive with their own card
	Added   int
	Removed int
}

// A Result is a successful apply.
type Result struct {
	Files []FileResult
	Hunks int
}

// ValidationError is exit 2: one or more hunks did not match, and nothing was
// written. §4.1 calls this "the interesting one", and the exit an agent should
// expect to see routinely and act on without alarm.
type ValidationError struct{ Failures []Failure }

func (e *ValidationError) Error() string {
	bad, skipped := 0, 0
	for _, f := range e.Failures {
		if f.Skipped() {
			skipped++
		} else {
			bad++
		}
	}
	msg := fmt.Sprintf("%d hunks did not match", bad)
	if skipped > 0 {
		msg += fmt.Sprintf(", %d skipped", skipped)
	}
	return msg + "; nothing was written"
}

// ChangedError is exit 6: a file changed on disk between load and commit.
// Something else is writing the tree; re-read and retry.
type ChangedError struct{ Path string }

func (e *ChangedError) Error() string {
	return e.Path + " changed on disk between being read and being written; nothing was written"
}

// UnsupportedOpError is temporary. The parser emits five ops and this file
// applies one, and a hunk it cannot perform has to say so rather than silently
// not doing it. Silently not doing an edit the patch asked for is the failure
// class the whole tool is built against, and "not implemented yet" is not an
// excuse the caller can see.
type UnsupportedOpError struct {
	Op   Op
	Path string
	Line int
}

func (e *UnsupportedOpError) Error() string {
	return fmt.Sprintf("patch line %d: %s %s is not implemented yet", e.Line, e.Op, e.Path)
}

// a loaded file, and the hunks' running effect on it
type file struct {
	target  Target
	orig    []byte
	cur     []byte // as the hunks so far have left it
	mode    fs.FileMode
	sum     [sha256.Size]byte
	eol     string // the dominant line ending (§6.4)
	changed bool

	failedAt int // the first hunk against this file that did not match
	applied  int // hunks that changed this file before the current one
	added    int
	removed  int

	// wrote is the hash of what commit put on disk. Rollback compares against
	// it to tell a formatter from a second writer (§6.3), which is
	// verify-and-rollback's card and the reason this is recorded here.
	wrote [sha256.Size]byte
}

// A Txn is one invocation's transaction over a Tree.
type Txn struct {
	tree  *Tree
	opt   Options
	files []*file          // first-appearance order, which is §5.1's output order
	index map[string]*file // by resolved name, so one file is one entry
	// per hunk, filled by Load. Validate indexes rather than resolving again:
	// resolving twice is both wasted work and a second error path that cannot
	// happen, since Load would already have refused it.
	hunkFile []*file
}

func NewTxn(tree *Tree, opt Options) *Txn {
	return &Txn{tree: tree, opt: opt, index: map[string]*file{}}
}

// Run performs phases 2 through 5. On any error nothing has been written,
// except that a commit failure part-way through leaves the files before it
// written, which is §6.2's stated limit.
func (x *Txn) Run(p *Patch) (*Result, error) {
	if err := x.Load(p); err != nil {
		return nil, err
	}
	if failures := x.Validate(p); len(failures) > 0 {
		return nil, &ValidationError{Failures: failures}
	}
	if err := x.Check(); err != nil {
		return nil, err
	}
	if err := x.Commit(); err != nil {
		return nil, err
	}
	return x.result(len(p.Hunks)), nil
}

// Preview is --dry-run: load and validate, and stop (§4). Nothing is written,
// so there is no check phase either; the check exists to protect a commit from
// a second writer and there is no commit to protect.
func (x *Txn) Preview(p *Patch) (*Result, error) {
	if err := x.Load(p); err != nil {
		return nil, err
	}
	if failures := x.Validate(p); len(failures) > 0 {
		return nil, &ValidationError{Failures: failures}
	}
	return x.result(len(p.Hunks)), nil
}

// Load reads every referenced file once (§6.1 step 2), recording bytes, mode
// and a SHA-256 of the original.
//
// A missing file is exit 2 rather than exit 5, because the fix is in the patch.
// §6.1 draws that line explicitly and the rule generalizes: if editing the
// patch would fix it, it is a validation failure.
func (x *Txn) Load(p *Patch) error {
	x.hunkFile = make([]*file, len(p.Hunks))
	for i, h := range p.Hunks {
		if h.Op != OpReplace {
			return &UnsupportedOpError{Op: h.Op, Path: h.Path, Line: h.Line}
		}
		f, err := x.load(h)
		if err != nil {
			return err
		}
		x.hunkFile[i] = f
	}
	return nil
}

func (x *Txn) load(h Hunk) (*file, error) {
	tg, err := x.tree.Resolve(h.Path)
	if err != nil {
		return nil, err
	}
	// Keyed by resolved name, so "a.go", "./a.go" and a symlink to it are one
	// file, read once and written once (§3.5).
	if f, ok := x.index[tg.name]; ok {
		return f, nil
	}
	fi, err := x.tree.Stat(tg)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &PathRefusal{Path: h.Path, Root: x.tree.Root(),
				Reason: "no such file; a hunk that edits a file needs the file to exist"}
		}
		return nil, err
	}
	if fi.IsDir() {
		return nil, &PathRefusal{Path: h.Path, Root: x.tree.Root(),
			Reason: "it is a directory, not a file"}
	}
	b, err := x.tree.ReadFile(tg)
	if err != nil {
		return nil, err
	}
	f := &file{
		target: tg,
		orig:   b,
		cur:    b,
		mode:   fi.Mode().Perm(),
		sum:    sha256.Sum256(b),
		eol:    dominantEOL(b),
	}
	x.index[tg.name] = f
	x.files = append(x.files, f)
	return f, nil
}

// Validate walks the hunks in order, applying each in memory against the file
// as previous hunks have left it (§6.1 step 3, §3.5). Nothing is written.
//
// A hunk that fails marks its file, and every later hunk against that file is
// reported skipped rather than failed. If hunk 2 was going to create the text
// hunk 3 matches, hunk 3's failure is not information, it is noise, and an
// agent that tries to fix both wastes the round trip this tool exists to save.
// Hunks against other files are still evaluated and still reported.
func (x *Txn) Validate(p *Patch) []Failure {
	var failures []Failure
	diagnosed := 0
	for i, h := range p.Hunks {
		n := i + 1
		f := x.hunkFile[i]
		if f.failedAt != 0 {
			failures = append(failures, Failure{
				Hunk: n, Path: h.Path, PatchLine: h.Line, SkippedAfter: f.failedAt,
			})
			continue
		}
		old := x.convert(h.Old, f.eol)
		found := bytes.Count(f.cur, old)
		if found != h.Count {
			f.failedAt = n
			fail := Failure{
				Hunk: n, Path: h.Path, PatchLine: h.Line,
				Expected: h.Count, Found: found,
			}
			// §7.2 caps diagnosis at ten hunks per batch. Beyond that the
			// failure is still reported; only the near-miss block is dropped,
			// because a diagnostic that blows the context window is a worse
			// failure than no diagnostic.
			if diagnosed < diagnoseLimit {
				diagnosed++
				if found == 0 {
					fail.Near = Diagnose(old, f.cur, x.opt.context())
				} else {
					fail.Near = DiagnoseTooMany(old, f.cur)
				}
				// Validation writes nothing, so the file on disk is still the
				// one loaded. Any hunk that already changed it in memory has
				// shifted the line numbers away from what the agent can see.
				fail.Near.Shifted = f.applied
			}
			failures = append(failures, fail)
			continue
		}
		repl := x.convert(h.New, f.eol)
		next := bytes.Replace(f.cur, old, repl, h.Count)
		// A hunk whose new text equals its old text matched, and changed
		// nothing. Rewriting the file anyway would move its mtime and
		// invalidate a build cache for no reason, so it does not count as a
		// change and the file is not listed in the diffstat.
		if !bytes.Equal(next, f.cur) {
			f.changed = true
			f.applied++
			// Per occurrence, not per hunk: an "@@ old x2" that rewrites two
			// lines changed two lines, and a diffstat saying +1 -1 understates
			// it. §5.1's globals.go row is +2 -2 and §3.6's hunk for that file
			// is exactly an x2 on a one-line old, which is the only row of that
			// example that corresponds to that patch.
			f.added += lineCount(repl) * h.Count
			f.removed += lineCount(old) * h.Count
		}
		f.cur = next
	}
	return failures
}

// Check re-hashes every target and compares to the load-time hash (§6.1 step
// 4). Any difference is exit 6 with nothing written: something else is writing
// the tree. This closes the concurrent-writer window to microseconds; §6.2 says
// plainly that it does not eliminate it, and no lock file is used.
func (x *Txn) Check() error {
	for _, f := range x.files {
		b, err := x.tree.ReadFile(f.target)
		if err != nil {
			return &ChangedError{Path: f.target.Orig()}
		}
		if sha256.Sum256(b) != f.sum {
			return &ChangedError{Path: f.target.Orig()}
		}
	}
	return nil
}

// Commit writes each changed file once (§6.1 step 5), through Tree.WriteAtomic
// so a symlinked target is written through rather than replaced (§6.5).
func (x *Txn) Commit() error {
	for _, f := range x.files {
		if !f.changed {
			continue
		}
		if err := x.tree.WriteAtomic(f.target, f.cur, f.mode); err != nil {
			return err
		}
		f.wrote = sha256.Sum256(f.cur)
	}
	return nil
}

// RunVerify is phase 6 (§6.1 step 6): run cmd via sh -c with cwd root and
// capture combined output.
//
// A command that cannot start is not a failed verify. Exit 3 means the verify
// ran and said no; a missing shell means nothing was verified at all, and
// reporting that as "the tests failed" would be a lie about the tree. That case
// returns an error, which the CLI maps to exit 5.
func RunVerify(command, root string, tailLines int) (*Verify, error) {
	v := &Verify{Ran: true, Command: command}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = root
	start := time.Now()
	out, err := cmd.CombinedOutput()
	v.Seconds = time.Since(start).Seconds()

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		v.OK = true
	case errors.As(err, &exitErr):
		v.OK = false
	default:
		return nil, fmt.Errorf("could not run the verify command: %w", err)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	v.TotalLines = len(lines)
	if tailLines > 0 && len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	v.Tail = lines
	return v, nil
}

// Rollback is phase 7 (§6.3). It restores each file this transaction wrote,
// after checking that the file on disk is still the one it wrote.
//
// The hash comparison is the design. A file that still hashes to what commit
// left is restored. A file that does not was rewritten by something after hunk
// wrote it, and there are two possibilities the tool cannot tell apart: a
// formatter run by --verify, or a second agent working the same tree.
//
// By default such a file is left alone and named, because silently reverting
// another writer's work is the one behaviour that would make this tool
// dangerous. mayFormat is the caller asserting the first case: that the verify
// command is expected to rewrite files and nothing else is writing the tree.
//
// A file the verify deleted counts as differing. Restoring it could not discard
// anybody's work, but the tool cannot tell a formatter's cleanup from a
// deliberate removal, which is the whole reason the default refuses.
func (x *Txn) Rollback(mayFormat bool) (restored int, notRestored []NotRestored) {
	for _, f := range x.files {
		if !f.changed {
			continue
		}
		if !mayFormat {
			now, err := x.tree.ReadFile(f.target)
			if err != nil {
				notRestored = append(notRestored, NotRestored{
					Path: f.target.Orig(),
					Reason: fmt.Sprintf("It is gone from disk, so something removed it after hunk\n"+
						"wrote %s there.\n"+
						"--verify-may-format says the verify command is expected to do that.",
						shortHash(f.wrote)),
				})
				continue
			}
			if sum := sha256.Sum256(now); sum != f.wrote {
				notRestored = append(notRestored, NotRestored{
					Path: f.target.Orig(),
					Reason: fmt.Sprintf("Something rewrote it after hunk did, and restoring could\n"+
						"discard that work. hunk wrote %s; the file is now %s.\n"+
						"--verify-may-format says the verify command is expected to rewrite files.",
						shortHash(f.wrote), shortHash(sum)),
				})
				continue
			}
		}
		if err := x.tree.WriteAtomic(f.target, f.orig, f.mode); err != nil {
			notRestored = append(notRestored, NotRestored{
				Path:   f.target.Orig(),
				Reason: "It could not be written: " + err.Error(),
			})
			continue
		}
		restored++
	}
	return restored, notRestored
}

// Applied is how many hunks actually changed something, for §5.3's first line:
// "applied 4 hunks, verify failed, rolled back 3 files". Hunks, not files, and
// not the size of the patch: a hunk whose new text equalled its old matched and
// changed nothing, and saying it was applied would overstate what the rollback
// has to undo.
func (x *Txn) Applied() int {
	n := 0
	for _, f := range x.files {
		n += f.applied
	}
	return n
}

// shortHash is what a message can carry without becoming unreadable. Enough to
// compare two by eye, which is the only thing it is for.
func shortHash(sum [sha256.Size]byte) string {
	return fmt.Sprintf("%x", sum[:6])
}

func (x *Txn) result(hunks int) *Result {
	r := &Result{Hunks: hunks}
	for _, f := range x.files {
		if !f.changed {
			continue
		}
		r.Files = append(r.Files, FileResult{
			Path: f.target.Orig(), Op: "modify", Added: f.added, Removed: f.removed,
		})
	}
	return r
}

// convert applies --eol (§6.4). Under EOLStrict the payload is matched as
// given.
func (x *Txn) convert(b []byte, eol string) []byte {
	if x.opt.EOL == EOLStrict {
		return b
	}
	return toEOL(b, eol)
}

var (
	lf   = []byte("\n")
	crlf = []byte("\r\n")
)

// dominantEOL is the line ending the majority of the file's terminators use.
//
// A file with no newline at all is LF: there is no evidence either way, and LF
// is what a heredoc produces. A mixed file gets the majority, and the minority
// then stops matching; that is the argument for a "mixed line endings" cause in
// the near-miss report rather than for a different rule here.
func dominantEOL(b []byte) string {
	nCRLF := bytes.Count(b, crlf)
	nLF := bytes.Count(b, lf) - nCRLF
	if nCRLF > nLF {
		return "\r\n"
	}
	return "\n"
}

// toEOL rewrites b to use eol.
//
// It normalizes to LF first in both directions, which is what makes it
// idempotent: converting LF to CRLF on a payload that already uses CRLF would
// otherwise produce "\r\r\n". Probed, not assumed.
func toEOL(b []byte, eol string) []byte {
	norm := bytes.ReplaceAll(b, crlf, lf)
	if eol == "\n" {
		return norm
	}
	return bytes.ReplaceAll(norm, lf, crlf)
}

// lineCount is §5.1's diffstat unit: a run ending in a newline, plus a final
// run that does not. §5.1's own example is self-consistent under exactly this
// rule and under no other, its "+8 -3" being 2+2+4 added and 1+2+0 removed.
func lineCount(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, lf)
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}
