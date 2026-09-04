package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// render drives the real pipeline: a patch against a real tree, then the
// report. The goldens then cover what an agent actually sees.
func render(t *testing.T, files map[string]string, patch string, opt Options) (text, jsonOut string, rep *Report) {
	t.Helper()
	tree, _ := fixture(t, files)
	p, err := Parse([]byte(patch), DefaultMarker)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res, runErr := NewTxn(tree, opt).Run(p)
	rep = NewReport(res, runErr, nil, false, len(p.Hunks))
	var out, errOut, js bytes.Buffer
	rep.Text(&out, &errOut, false)
	if err := rep.JSON(&js); err != nil {
		t.Fatalf("json: %v", err)
	}
	return out.String() + errOut.String(), js.String(), rep
}

func TestReportGoldens(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		text, js, rep := render(t, map[string]string{
			"internal/cli/root.go":         "one\ntwo\n",
			"internal/registry/globals.go": "alpha\nbeta\n",
		}, ""+
			"@@ file internal/cli/root.go\n@@ old\none\n@@ new\nONE\nEXTRA\n"+
			"@@ file internal/registry/globals.go\n@@ old\nbeta\n@@ new\nBETA\nMORE\n", Options{})
		if rep.Exit != exitOK {
			t.Fatalf("exit %d: %s", rep.Exit, text)
		}
		golden(t, "report-success", text)
		golden(t, "report-success-json", js)
	})

	// §5.2's own example shape: a failing hunk with a named cause, a skipped
	// hunk in the same file, and a too-many hunk in another.
	t.Run("validation failure", func(t *testing.T) {
		text, js, rep := render(t, map[string]string{
			"internal/cli/root.go":         "\"registry\"\nkeep\n",
			"internal/registry/globals.go": "G\nx\nG\ny\nG\n",
		}, ""+
			"@@ file internal/cli/root.go\n@@ old\n\t\"registry\"\n@@ new\nX\n"+
			"@@ old\nkeep\n@@ new\nKEEP\n"+
			"@@ file internal/registry/globals.go\n@@ old x2\nG\n@@ new\nZ\n", Options{})
		if rep.Exit != exitNoMatch {
			t.Fatalf("exit %d", rep.Exit)
		}
		golden(t, "report-validation", text)
		golden(t, "report-validation-json", js)
	})

	t.Run("no anchor", func(t *testing.T) {
		text, js, _ := render(t, map[string]string{"a.go": "alpha\n"},
			"@@ file a.go\n@@ old\nnothing like it\n@@ new\nX\n", Options{})
		golden(t, "report-no-anchor", text)
		golden(t, "report-no-anchor-json", js)
	})

	t.Run("a path refusal", func(t *testing.T) {
		text, js, rep := render(t, map[string]string{"a.go": "x\n"},
			"@@ file ../out.go\n@@ old\nx\n@@ new\ny\n", Options{})
		if rep.Exit != exitNoMatch {
			t.Errorf("exit %d, want 2", rep.Exit)
		}
		if !strings.Contains(text, "climbs out") {
			t.Errorf("text = %q", text)
		}
		if !strings.Contains(js, `"error"`) {
			t.Errorf("json has no error field: %s", js)
		}
	})

	t.Run("verify failure", func(t *testing.T) {
		rep := NewReport(&Result{Hunks: 4}, nil, &Verify{
			Ran: true, OK: false, Command: "go build ./... && go test ./internal/cli/",
			Applied: 4, RolledBack: 3, TotalLines: 2,
			Tail: []string{
				"--- internal/cli/root.go:41:2: undefined: scope",
				"--- FAIL github.com/kmoneil/jr/internal/cli [build failed]",
			},
		}, false, 4)
		if rep.Exit != exitVerifyFailed {
			t.Fatalf("exit %d, want 3", rep.Exit)
		}
		var out, errOut, js bytes.Buffer
		rep.Text(&out, &errOut, false)
		must(t, rep.JSON(&js))
		golden(t, "report-verify-failed", errOut.String())
		golden(t, "report-verify-failed-json", js.String())
	})

	// Exit 4 is the worst moment in the tool's life: the tree is inconsistent
	// and the agent has only this message to repair it from (§6.3).
	t.Run("rollback incomplete", func(t *testing.T) {
		rep := NewReport(&Result{Hunks: 2}, nil, &Verify{
			Ran: true, OK: false, Command: "make fmt && go test ./...",
			Applied: 2, RolledBack: 1, TotalLines: 1,
			Tail: []string{"FAIL github.com/kmoneil/jr [build failed]"},
			NotRestored: []NotRestored{{
				Path:   "internal/cli/root.go",
				Reason: "it changed after hunk wrote it, so restoring would discard somebody else's work; --verify-may-format says the verify command is expected to rewrite files",
			}},
		}, false, 2)
		if rep.Exit != exitRollbackFailed {
			t.Fatalf("exit %d, want 4", rep.Exit)
		}
		var out, errOut, js bytes.Buffer
		rep.Text(&out, &errOut, false)
		must(t, rep.JSON(&js))
		golden(t, "report-rollback-incomplete", errOut.String())
		golden(t, "report-rollback-incomplete-json", js.String())
	})

	t.Run("dry run", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"a.go": "one\n"})
		p, err := Parse([]byte("@@ file a.go\n@@ old\none\n@@ new\nONE\nTWO\n"), DefaultMarker)
		must(t, err)
		res, runErr := NewTxn(tree, Options{}).Run(p)
		must(t, runErr)
		rep := NewReport(res, nil, nil, true, len(p.Hunks))
		var out, errOut bytes.Buffer
		rep.Text(&out, &errOut, false)
		golden(t, "report-dry-run", out.String())
	})
}

// §5.1's example, reproduced. Two of its three rows put the stat at column 31
// and the third at 32; padding to the longest path gives 31 for all, so this
// pins the rule and keeps the spec's own rows.
func TestSuccessLinesUpOnTheLongestPath(t *testing.T) {
	rep := &Report{Result: &Result{Hunks: 4, Files: []FileResult{
		{Path: "internal/cli/root.go", Op: "modify", Added: 2, Removed: 1},
		{Path: "internal/registry/globals.go", Op: "modify", Added: 2, Removed: 2},
		{Path: "internal/cli/scope.go", Op: "create", Added: 4, Removed: 0},
	}}}
	var out, errOut bytes.Buffer
	rep.Text(&out, &errOut, false)
	golden(t, "report-success-spec-example", out.String())

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	col := -1
	for _, l := range lines[:3] {
		at := strings.Index(l, "+")
		if col == -1 {
			col = at
		}
		if at != col {
			t.Errorf("stat column %d, want %d: %q", at, col, l)
		}
	}
	if got := lines[3]; got != "3 files, 4 hunks, +8 -3" {
		t.Errorf("total = %q", got)
	}
}

// §4.1 is a closed set, and §5.2's JSON carries the number, so one mapping
// serves both. Two mappings for one table is how they diverge.
func TestExitCode(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, exitOK},
		{"a parse error", &ParseError{Line: 1, Msg: "x"}, exitUsage},
		{"a hunk that did not match", &ValidationError{}, exitNoMatch},
		{"a refused path", &PathRefusal{Path: "x"}, exitNoMatch},
		{"a file changed underneath", &ChangedError{Path: "x"}, exitChanged},
		{"an I/O error", fs.ErrPermission, exitIO},
		{"a wrapped validation error", fmt.Errorf("wrapped: %w", &ValidationError{}), exitNoMatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ExitCode(c.err); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}

	t.Run("a failed verify is exit 3, and exit 4 when rollback was incomplete", func(t *testing.T) {
		if got := NewReport(nil, nil, &Verify{Ran: true, OK: false}, false, 0).Exit; got != exitVerifyFailed {
			t.Errorf("got %d, want 3", got)
		}
		v := &Verify{Ran: true, OK: false, NotRestored: []NotRestored{{Path: "a"}}}
		if got := NewReport(nil, nil, v, false, 0).Exit; got != exitRollbackFailed {
			t.Errorf("got %d, want 4", got)
		}
	})
}

// §5.2: "--json carries everything the text does, so a harness never parses the
// text." That only stays true if something checks it.
func TestJSONAndTextDoNotDrift(t *testing.T) {
	text, js, rep := render(t, map[string]string{
		"a.go": "\"registry\"\nkeep\n",
		"b.go": "G\nx\nG\n",
	}, ""+
		"@@ file a.go\n@@ old\n\t\"registry\"\n@@ new\nX\n"+
		"@@ old\nkeep\n@@ new\nKEEP\n"+
		"@@ file b.go\n@@ old\nG\n@@ new\nZ\n", Options{})

	var decoded jsonReport
	must(t, json.Unmarshal([]byte(js), &decoded))
	if decoded.Exit != rep.Exit || decoded.OK {
		t.Errorf("envelope: exit=%d ok=%v", decoded.Exit, decoded.OK)
	}
	if len(decoded.Failures) != len(rep.Failures) {
		t.Fatalf("json has %d failures, the report has %d", len(decoded.Failures), len(rep.Failures))
	}
	for _, f := range decoded.Failures {
		if !strings.Contains(text, fmt.Sprintf("hunk %d  %s  (patch line %d)", f.Hunk, f.Path, f.PatchLine)) {
			t.Errorf("json failure %d is not in the text", f.Hunk)
		}
		if f.NearMiss != nil && f.NearMiss.Span != "" {
			for _, l := range strings.Split(f.NearMiss.Span, "\n") {
				if !strings.Contains(text, l) {
					t.Errorf("json span line %q is not in the text", l)
				}
			}
		}
		for _, n := range f.Lines {
			if !strings.Contains(text, fmt.Sprint(n)) {
				t.Errorf("json listed line %d is not in the text", n)
			}
		}
	}
	// §5.2 puts lines beside expected and found, not inside near_miss. §10
	// makes that shape an interface, so it is pinned.
	var raw map[string]any
	must(t, json.Unmarshal([]byte(js), &raw))
	fs := raw["failures"].([]any)
	last := fs[len(fs)-1].(map[string]any)
	if _, ok := last["lines"]; !ok {
		t.Errorf("a too-many failure has no top-level lines: %v", last)
	}
	if nm, ok := last["near_miss"]; ok {
		t.Errorf("a too-many failure should not nest near_miss: %v", nm)
	}
}

func TestQuietAndStreams(t *testing.T) {
	tree, _ := fixture(t, map[string]string{"a.go": "one\n"})
	p, _ := Parse([]byte("@@ file a.go\n@@ old\none\n@@ new\nONE\n"), DefaultMarker)
	res, err := NewTxn(tree, Options{}).Run(p)
	must(t, err)

	t.Run("quiet prints nothing on success", func(t *testing.T) {
		var out, errOut bytes.Buffer
		NewReport(res, nil, nil, false, 1).Text(&out, &errOut, true)
		if out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("out=%q err=%q", out.String(), errOut.String())
		}
	})

	t.Run("success goes to stdout and failures to stderr", func(t *testing.T) {
		var out, errOut bytes.Buffer
		NewReport(res, nil, nil, false, 1).Text(&out, &errOut, false)
		if out.Len() == 0 || errOut.Len() != 0 {
			t.Errorf("success: out=%d err=%d", out.Len(), errOut.Len())
		}

		out.Reset()
		errOut.Reset()
		NewReport(nil, &ValidationError{Failures: []Failure{{Hunk: 1, Path: "a.go", PatchLine: 2}}},
			nil, false, 1).Text(&out, &errOut, false)
		if out.Len() != 0 || errOut.Len() == 0 {
			t.Errorf("failure: out=%d err=%d; a caller redirecting stdout must still see why", out.Len(), errOut.Len())
		}
	})

	// --quiet says "print nothing on success" (§4). A failure is never silent:
	// a caller who wanted no output at all would not have run the tool.
	t.Run("quiet still reports a failure", func(t *testing.T) {
		var out, errOut bytes.Buffer
		NewReport(nil, &ChangedError{Path: "a.go"}, nil, false, 1).Text(&out, &errOut, true)
		if errOut.Len() == 0 {
			t.Error("quiet swallowed a failure")
		}
	})
}

func TestJSONIsAlwaysOneValidObject(t *testing.T) {
	for _, c := range []struct {
		name string
		rep  *Report
	}{
		{"success", NewReport(&Result{Hunks: 1, Files: []FileResult{{Path: "a", Op: "modify", Added: 1, Removed: 1}}}, nil, nil, false, 1)},
		{"empty", NewReport(nil, nil, nil, false, 0)},
		{"io error", NewReport(nil, fs.ErrPermission, nil, false, 0)},
		{"changed", NewReport(nil, &ChangedError{Path: "a"}, nil, false, 0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			must(t, c.rep.JSON(&b))
			var v map[string]any
			if err := json.Unmarshal(b.Bytes(), &v); err != nil {
				t.Fatalf("not valid JSON: %v\n%s", err, b.String())
			}
			if _, ok := v["ok"]; !ok {
				t.Error("no ok field")
			}
			if _, ok := v["exit"]; !ok {
				t.Error("no exit field")
			}
		})
	}
}

func TestReportEdges(t *testing.T) {
	t.Run("a success report with no result prints nothing", func(t *testing.T) {
		var out, errOut bytes.Buffer
		(&Report{Exit: exitOK}).Text(&out, &errOut, false)
		if out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("out=%q err=%q", out.String(), errOut.String())
		}
	})

	t.Run("verify ok is reported with its duration", func(t *testing.T) {
		rep := NewReport(&Result{Hunks: 1, Files: []FileResult{{Path: "a.go", Op: "modify", Added: 1, Removed: 1}}},
			nil, &Verify{Ran: true, OK: true, Seconds: 11.3}, false, 1)
		var out, errOut bytes.Buffer
		rep.Text(&out, &errOut, false)
		if !strings.Contains(out.String(), "verify ok (11.3s)") {
			t.Errorf("got %q", out.String())
		}
	})

	// §5.1's first column, and the two letters that arrive with
	// create-delete-append-prepend.
	t.Run("the op letter", func(t *testing.T) {
		for op, want := range map[string]string{
			"modify": "M", "create": "A", "delete": "D", "": "M",
		} {
			if got := opLetter(op); got != want {
				t.Errorf("opLetter(%q) = %q, want %q", op, got, want)
			}
		}
	})

	t.Run("a verify failure with no output prints no elision count", func(t *testing.T) {
		rep := NewReport(nil, nil, &Verify{Ran: true, OK: false, Command: "false", Applied: 1}, false, 1)
		var out, errOut bytes.Buffer
		rep.Text(&out, &errOut, false)
		if strings.Contains(errOut.String(), "last 0 of") {
			t.Errorf("got %q", errOut.String())
		}
	})
}

// §5.2's skipped example carries neither expected nor found, and "found": 0
// would say the hunk was evaluated and matched nothing. It was never evaluated.
func TestSkippedHunkJSONOmitsTheCounts(t *testing.T) {
	_, js, _ := render(t, map[string]string{"a.go": "one\ntwo\n"},
		"@@ file a.go\n@@ old\nabsent\n@@ new\nX\n@@ old\none\n@@ new\nONE\n", Options{})
	var raw map[string]any
	must(t, json.Unmarshal([]byte(js), &raw))
	fs := raw["failures"].([]any)
	skipped := fs[1].(map[string]any)
	if _, ok := skipped["skipped_after"]; !ok {
		t.Fatalf("not the skipped failure: %v", skipped)
	}
	for _, f := range []string{"found", "expected", "near_miss"} {
		if v, ok := skipped[f]; ok {
			t.Errorf("skipped hunk carries %q = %v; it was never evaluated", f, v)
		}
	}
	// And an evaluated hunk that found nothing keeps its zero.
	failed := fs[0].(map[string]any)
	if v, ok := failed["found"]; !ok || v.(float64) != 0 {
		t.Errorf("an evaluated hunk lost its found:0, got %v", failed["found"])
	}
}

// A --verify command is shell text, full of & and >. HTML escaping turns it
// into something an agent has to decode by eye.
func TestJSONDoesNotHTMLEscape(t *testing.T) {
	rep := NewReport(nil, nil, &Verify{
		Ran: true, OK: false, Command: "go build ./... && go test 2>&1 | head",
		Applied: 1,
	}, false, 1)
	var b bytes.Buffer
	must(t, rep.JSON(&b))
	for _, esc := range []string{`\u0026`, `\u003e`, `\u003c`} {
		if strings.Contains(b.String(), esc) {
			t.Errorf("still escaping %s: %s", esc, b.String())
		}
	}
	if !strings.Contains(b.String(), "&& go test 2>&1") {
		t.Errorf("command not readable: %s", b.String())
	}
}
