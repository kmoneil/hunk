package main

// Text and JSON output (§5).
//
// These messages are the product. §8.1: "The exact bytes of every failure
// message, because those messages are the product. A reworded diagnostic is a
// contract change." So every shape here is goldened, and both renderers take
// one Report and nothing else, which is what makes §5.2's promise structural:
// "--json carries everything the text does, so a harness never parses the
// text."
//
// The JSON failure shape has a second consumer that does not exist yet. §10:
// the MCP server "will return" it, "which is why that shape is specified now".
// So §5.2's object is followed exactly rather than tidied.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// A Verify is the outcome of --verify (§5.1's JSON, §5.3's text).
//
// verify-and-rollback populates it and does the restoring; this file renders
// it. The seam is here because rendering §5.3 in two places is the only way to
// end up with two different §5.3s.
type Verify struct {
	Ran     bool
	OK      bool
	Command string
	Seconds float64

	// Tail is the last --verify-lines lines of combined output, and TotalLines
	// how many there were, so the report can say what it elided.
	Tail       []string
	TotalLines int

	// Applied, RolledBack and NotRestored describe the failure (§5.3, §6.3).
	Applied     int
	RolledBack  int
	NotRestored []NotRestored

	// Kept means --keep-on-fail left the changes in place. The exit is still 3
	// (§4): the code reports what the verify said, not what was done about it.
	Kept bool
}

// A NotRestored is a file rollback left alone because something rewrote it
// after hunk did (§6.3). Naming it is the whole of exit 4's usefulness.
type NotRestored struct {
	Path   string
	Reason string
}

// A Report is everything one invocation has to say.
type Report struct {
	Exit     int
	Result   *Result
	Failures []Failure
	Verify   *Verify
	Err      error
	DryRun   bool
	// Hunks is the size of the whole patch, which §5.2's header needs ("2 of 5
	// hunks did not match") and which Result cannot supply because there is no
	// Result on a failure.
	Hunks int
}

// count is the summary form: "1 file", "3 files". Distinct from plural, which
// reads "a tab" and is right in a detail sentence and wrong in a count.
func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// ExitCode maps an error to §4.1's closed set. It lives here rather than in
// main.go because §5.2's JSON carries "exit", and two mappings for one table is
// how they diverge.
func ExitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var pe *ParseError
	if errors.As(err, &pe) {
		return exitUsage
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return exitNoMatch
	}
	// A path refusal is a validation failure: §6.1's line is whether editing
	// the patch would fix it, and here it would.
	var pr *PathRefusal
	if errors.As(err, &pr) {
		return exitNoMatch
	}
	var ce *ChangedError
	if errors.As(err, &ce) {
		return exitChanged
	}
	return exitIO
}

// NewReport assembles what the renderers take.
func NewReport(res *Result, err error, v *Verify, dryRun bool, hunks int) *Report {
	r := &Report{Result: res, Verify: v, Err: err, DryRun: dryRun, Hunks: hunks,
		Exit: ExitCode(err)}
	var ve *ValidationError
	if errors.As(err, &ve) {
		r.Failures = ve.Failures
	}
	if v != nil && v.Ran && !v.OK {
		r.Exit = exitVerifyFailed
		if len(v.NotRestored) > 0 {
			r.Exit = exitRollbackFailed
		}
	}
	return r
}

// Text writes §5's human output. Success goes to out; anything an agent has to
// act on goes to errOut, so a caller that redirects stdout still sees why.
//
// quiet suppresses the success report only (§4). A failure is never silent:
// --quiet says "print nothing on success", and a caller who wanted no output at
// all would not have run the tool.
func (r *Report) Text(out, errOut io.Writer, quiet bool) {
	if r.Exit == exitOK {
		if quiet {
			return
		}
		r.writeSuccess(out)
		return
	}
	switch {
	case len(r.Failures) > 0:
		r.writeValidationFailure(errOut)
	case r.Verify != nil && r.Verify.Ran && !r.Verify.OK:
		r.writeVerifyFailure(errOut)
	default:
		fmt.Fprintf(errOut, "hunk: %v\n", r.Err)
	}
}

func (r *Report) writeSuccess(w io.Writer) {
	res := r.Result
	if res == nil {
		return
	}
	// Pad to the longest path so the stats line up. §5.1's example puts two of
	// three rows at the column this produces and the third one past it.
	width := 0
	for _, f := range res.Files {
		if len(f.Path) > width {
			width = len(f.Path)
		}
	}
	for _, f := range res.Files {
		fmt.Fprintf(w, "%s %-*s +%d -%d", opLetter(f.Op), width, f.Path, f.Added, f.Removed)
		if f.SeamAdded {
			fmt.Fprint(w, "  (added a final newline)")
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "%s, %s, +%d -%d",
		count(len(res.Files), "file"), count(res.Hunks, "hunk"), res.Added(), res.Removed())
	switch {
	case r.DryRun && r.Verify != nil:
		// §4: --dry-run writes nothing and runs no verify. Both are said,
		// because a caller who passed --verify and sees only "nothing written"
		// has no reason not to assume the verify passed.
		fmt.Fprint(w, " (dry run: nothing written, verify not run)")
	case r.DryRun:
		fmt.Fprint(w, " (dry run: nothing written)")
	case r.Verify != nil && r.Verify.Ran:
		fmt.Fprintf(w, ", verify ok (%.1fs)", r.Verify.Seconds)
	}
	fmt.Fprintln(w)
}

func (r *Report) writeValidationFailure(w io.Writer) {
	bad, skipped := 0, 0
	for _, f := range r.Failures {
		if f.Skipped() {
			skipped++
		} else {
			bad++
		}
	}
	// §5.2's own header: "2 of 5 hunks did not match, 1 skipped". The "of M"
	// half only says something when some hunks did match, so "1 of 1 hunk" is
	// dropped in favour of "1 hunk".
	if r.Hunks > bad {
		fmt.Fprintf(w, "hunk: %d of %s did not match", bad, count(r.Hunks, "hunk"))
	} else {
		fmt.Fprintf(w, "hunk: %s did not match", count(bad, "hunk"))
	}
	if skipped > 0 {
		fmt.Fprintf(w, ", %d skipped", skipped)
	}
	fmt.Fprintln(w, "; nothing was written")

	for _, f := range r.Failures {
		fmt.Fprintf(w, "\nhunk %d  %s  (patch line %d)\n", f.Hunk, f.Path, f.PatchLine)
		if f.Skipped() {
			fmt.Fprintf(w, "  skipped: same file as hunk %d, which did not match\n", f.SkippedAfter)
			continue
		}
		if f.Refusal != "" {
			fmt.Fprintf(w, "  %s\n", f.Refusal)
			continue
		}
		fmt.Fprintf(w, "  expected %s, found %d\n", count(f.Expected, "occurrence"), f.Found)
		if f.Near == nil {
			continue
		}
		if f.Near.Kind != DiagTooMany {
			fmt.Fprintln(w)
		}
		fmt.Fprint(w, f.Near.Render("  "))
	}
}

func (r *Report) writeVerifyFailure(w io.Writer) {
	v := r.Verify
	fmt.Fprintf(w, "hunk: applied %s, verify failed", count(v.Applied, "hunk"))
	switch {
	case v.Kept:
		fmt.Fprint(w, ", changes left in place for inspection (--keep-on-fail)\n")
	case len(v.NotRestored) > 0:
		fmt.Fprintf(w, ", rolled back %s, %s left alone\n",
			count(v.RolledBack, "file"), count(len(v.NotRestored), "file"))
	default:
		fmt.Fprintf(w, ", rolled back %s\n", count(v.RolledBack, "file"))
	}
	fmt.Fprintf(w, "\n$ %s\n", v.Command)
	for _, l := range v.Tail {
		fmt.Fprintln(w, l)
	}
	if v.TotalLines > 0 {
		fmt.Fprintf(w, "(last %d of %d lines)\n", len(v.Tail), v.TotalLines)
	}
	for _, n := range v.NotRestored {
		fmt.Fprintf(w, "\n%s was not restored.\n", n.Path)
		for _, line := range strings.Split(n.Reason, "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
		fmt.Fprintln(w, "  hunk did not write its original bytes back; they are in version control.")
	}
}

// opLetter is §5.1's first column.
func opLetter(op string) string {
	switch op {
	case "create":
		return "A"
	case "delete":
		return "D"
	}
	return "M"
}

// Added and Removed are §5.1's totals.
func (r *Result) Added() int {
	n := 0
	for _, f := range r.Files {
		n += f.Added
	}
	return n
}

func (r *Result) Removed() int {
	n := 0
	for _, f := range r.Files {
		n += f.Removed
	}
	return n
}

// The JSON shapes are §5.1's and §5.2's, field for field. They are a published
// interface (§10), so the encoder follows them rather than tidying them.

type jsonReport struct {
	OK       bool          `json:"ok"`
	Exit     int           `json:"exit"`
	Files    []jsonFile    `json:"files,omitempty"`
	Hunks    int           `json:"hunks,omitempty"`
	Verify   *jsonVerify   `json:"verify,omitempty"`
	Failures []jsonFailure `json:"failures,omitempty"`
	Error    string        `json:"error,omitempty"`
	DryRun   bool          `json:"dry_run,omitempty"`
}

type jsonFile struct {
	Path      string `json:"path"`
	Op        string `json:"op"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	SeamAdded bool   `json:"added_final_newline,omitempty"`
}

type jsonVerify struct {
	Ran         bool          `json:"ran"`
	OK          bool          `json:"ok"`
	Seconds     float64       `json:"seconds"`
	Command     string        `json:"command,omitempty"`
	Output      []string      `json:"output,omitempty"`
	RolledBack  int           `json:"rolled_back,omitempty"`
	Kept        bool          `json:"kept,omitempty"`
	NotRestored []jsonRestore `json:"not_restored,omitempty"`
}

type jsonRestore struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type jsonFailure struct {
	Hunk      int    `json:"hunk"`
	Path      string `json:"path"`
	PatchLine int    `json:"patch_line"`
	Expected  int    `json:"expected,omitempty"`
	// Found is a pointer because zero is a meaningful answer for an evaluated
	// hunk and no answer at all for a skipped one. §5.2's skipped example
	// carries neither expected nor found, and omitempty on an int cannot tell
	// those two zeros apart.
	Found        *int   `json:"found,omitempty"`
	SkippedAfter int    `json:"skipped_after,omitempty"`
	Refusal      string `json:"refusal,omitempty"`
	// Lines sits beside expected and found, not inside near_miss. That is
	// §5.2's shape and §10 makes it an interface.
	Lines    []int     `json:"lines,omitempty"`
	NearMiss *jsonNear `json:"near_miss,omitempty"`
}

type jsonNear struct {
	Cause   string `json:"cause,omitempty"`
	Line    int    `json:"line,omitempty"`
	Span    string `json:"span,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Shifted int    `json:"shifted_by_hunks,omitempty"`
}

// JSON writes one object on stdout (§4). It wins over --quiet: a caller that
// asked for machine output asked for it on every path.
func (r *Report) JSON(w io.Writer) error {
	out := jsonReport{OK: r.Exit == exitOK, Exit: r.Exit, DryRun: r.DryRun}
	if r.Result != nil {
		out.Hunks = r.Result.Hunks
		for _, f := range r.Result.Files {
			out.Files = append(out.Files, jsonFile{f.Path, f.Op, f.Added, f.Removed, f.SeamAdded})
		}
	}
	if v := r.Verify; v != nil {
		jv := &jsonVerify{Ran: v.Ran, OK: v.OK, Seconds: v.Seconds, Command: v.Command,
			Output: v.Tail, RolledBack: v.RolledBack, Kept: v.Kept}
		for _, n := range v.NotRestored {
			jv.NotRestored = append(jv.NotRestored, jsonRestore{n.Path, n.Reason})
		}
		out.Verify = jv
	}
	for _, f := range r.Failures {
		jf := jsonFailure{Hunk: f.Hunk, Path: f.Path, PatchLine: f.PatchLine,
			Expected: f.Expected, SkippedAfter: f.SkippedAfter, Refusal: f.Refusal}
		if !f.Skipped() && f.Refusal == "" {
			found := f.Found
			jf.Found = &found
		}
		if d := f.Near; d != nil {
			if d.Kind == DiagTooMany {
				jf.Lines = d.Lines
			} else if d.Kind != DiagNoAnchor {
				jf.NearMiss = &jsonNear{Cause: d.Cause, Line: d.Line,
					Span: string(joinLines(d.Span)), Detail: d.Detail, Shifted: d.Shifted}
			} else {
				jf.NearMiss = &jsonNear{Cause: "no anchor"}
			}
		}
		out.Failures = append(out.Failures, jf)
	}
	if r.Exit != exitOK && len(r.Failures) == 0 && r.Err != nil {
		out.Error = r.Err.Error()
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Go escapes &, < and > as \\u00xx by default, for embedding in HTML. This
	// output is read by a program, and a --verify command of "go build && go
	// test" coming back as "go build \\u0026\\u0026 go test" is noise an agent
	// has to see through.
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

func joinLines(lines [][]byte) []byte {
	return []byte(strings.Join(byteLinesToStrings(lines), "\n"))
}

func byteLinesToStrings(lines [][]byte) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l)
	}
	return out
}
