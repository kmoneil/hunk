package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The classifier is the artifact of this measurement, not the numbers: every
// boundary here moves a published figure, so each one is a case.
func TestIsHeredocEdit(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  string
		want bool
	}{
		{"§1's verbatim example", "python3 - <<'PY'\nimport pathlib\np = pathlib.Path(\"docs/x.md\")\np.write_text(t)\nPY", true},
		{"no dash", "python3 <<'PY'\nopen('a','w').write(s)\nPY", true},
		{"python2 spelling", "python <<EOF\nopen('a','w').write(s)\nEOF", true},
		{"unquoted delimiter", "python3 - <<PY\nopen('a','w').write(s)\nPY", true},
		{"a heredoc that only reads", "python3 - <<'PY'\nprint(open('a').read())\nPY", false},
		{"not python", "cat <<'EOF' > a\nx\nEOF", false},
		{"a shell heredoc mentioning python", "cat <<'EOF'\npython3 write_text\nEOF", false},
		{"an inline python -c", "python3 -c \"open('a','w').write(s)\"", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := IsHeredocEdit(c.cmd); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestHasUniquenessGuard(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  string
		want bool
	}{
		{"assert old in t", "assert old in t", true},
		{"a bare assert", "assert s != s2", true},
		{"if not in, raising", "if old not in t: raise SystemExit(1)", true},
		{"if in, positive", "if old in t:\n    t = t.replace(old, new)", true},
		{"a count check", "assert t.count(old) == 1", true},
		{"a raise", "raise ValueError('missing')", true},
		{"nothing at all", "t = t.replace(old, new)", false},
		{"a count limit is not a guard", "t = t.replace(old, new, 1)", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := HasUniquenessGuard(c.cmd); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A count limit takes the first match without checking there is only one,
// which is the silent wrong-occurrence edit §1.2 calls the most expensive
// failure in the corpus. It is counted, and counted separately.
func TestLimitsToFirstMatch(t *testing.T) {
	for cmd, want := range map[string]bool{
		"t.replace(old, new, 1)":   true,
		"t.replace(old, new,1)":    true,
		"re.sub(p, r, s, count=1)": true,
		"t.replace(old, new)":      false,
		"t.replace(old, new, 3)":   false,
		"assert old in t":          false,
	} {
		if got := LimitsToFirstMatch(cmd); got != want {
			t.Errorf("LimitsToFirstMatch(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// §1: 66% bundled a verify command, which is the figure --help quotes for why
// --verify exists and is optional. Only what runs after the heredoc counts.
func TestBundlesVerify(t *testing.T) {
	edit := "python3 - <<'PY'\nopen('a','w').write(s)\nPY\n"
	for _, c := range []struct {
		name string
		cmd  string
		want bool
	}{
		{"§1's verbatim example", edit + "make fmt && go test ./internal/...", true},
		{"a plain go test", edit + "go test ./...", true},
		{"pytest", edit + "pytest -q", true},
		{"npm test", edit + "npm test", true},
		{"nothing after", edit, false},
		{"a mention inside the payload only", "python3 - <<'PY'\ns='go test'\nopen('a','w').write(s)\nPY", false},
		{"an unrelated command after", edit + "ls -la", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := BundlesVerify(c.cmd); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestHandRollsBackup(t *testing.T) {
	for cmd, want := range map[string]bool{
		"cp $S/invariants.md.bak docs/invariants.md": true,
		"cp a.go /tmp/a.go":                          true,
		"cp a.go a.go.orig":                          true,
		"cp a.go b.go":                               false,
		"rm -f a.go":                                 false,
	} {
		if got := HandRollsBackup(cmd); got != want {
			t.Errorf("HandRollsBackup(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestCountReplaces(t *testing.T) {
	for cmd, want := range map[string]int{
		"":                               0,
		"t.replace(a,b)":                 1,
		"t.replace(a,b)\nt.replace(c,d)": 2,
		"re.sub(a,b,c)":                  1,
		"t.replace (a,b)":                1,
	} {
		if got := CountReplaces(cmd); got != want {
			t.Errorf("CountReplaces(%q) = %d, want %d", cmd, got, want)
		}
	}
}

// The dominant shape in the corpus assigns the path to a variable first. The
// first draft of Paths only saw the literal-in-a-call form and missed 69% of
// the calls, which put file touches at a third of §1's figure.
func TestPaths(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  string
		want []string
	}{
		{"a literal in Path()", `p = pathlib.Path("docs/invariants.md")`, []string{"docs/invariants.md"}},
		{"a literal in open()", `open('internal/a.go').read()`, []string{"internal/a.go"}},
		{"the variable form", "p='internal/lexer/lexer_test.go'\ns=open(p).read()", []string{"internal/lexer/lexer_test.go"}},
		{"both, deduplicated", "p='a.go'\nopen('a.go','w').write(s)", []string{"a.go"}},
		{"two files", "p='a.go'\nq='b/c.go'", []string{"a.go", "b/c.go"}},
		{"a non-path assignment is not a path", "s='hello world'\nmode='w'", nil},
		{"a bare word is not a path", "x='PY'", nil},
		{"a directory is a path", "d='internal/cli/'", []string{"internal/cli/"}},
		{"order is first appearance", "p='b.go'\nq='a.go'", []string{"b.go", "a.go"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Paths(c.cmd); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A failed attempt is read off the tool result, not guessed at. It is what
// §12's calls-per-successful-edit counts.
func TestFailed(t *testing.T) {
	for out, want := range map[string]bool{
		"Traceback (most recent call last):\n  ...\nAssertionError": true,
		"AssertionError":               true,
		"FileNotFoundError: [Errno 2]": true,
		"ok  github.com/x  0.1s":       false,
		"":                             false,
	} {
		if got := Failed(out); got != want {
			t.Errorf("Failed(%q) = %v, want %v", out, got, want)
		}
	}
}

// A synthetic corpus, so the walker's arithmetic is checked against numbers
// that can be counted by hand.
func fixtureCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "-workspace-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(blocks ...map[string]any) string {
		b, err := json.Marshal(map[string]any{"message": map[string]any{"content": blocks}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	use := func(id, name, cmd string) map[string]any {
		return map[string]any{
			"type": "tool_use", "id": id, "name": name,
			"input": map[string]any{"command": cmd},
		}
	}
	res := func(id, out string) map[string]any {
		return map[string]any{"type": "tool_result", "tool_use_id": id, "content": out}
	}

	edit := func(path, body string) string {
		return "python3 - <<'PY'\np='" + path + "'\ns=open(p).read()\n" + body + "\nopen(p,'w').write(s)\nPY"
	}
	var sb strings.Builder
	// 1: a guarded edit that succeeds, bundling a verify.
	sb.WriteString(line(use("t1", "Bash", edit("a.go", "assert 'x' in s\ns=s.replace('x','y')")+"\ngo test ./...")))
	sb.WriteString(line(res("t1", "ok")))
	// 2: an unguarded edit that fails, then a read, then a success. Three calls
	// for one edit, which is what §12 counts.
	sb.WriteString(line(use("t2", "Bash", edit("b.go", "s=s.replace('p','q')"))))
	sb.WriteString(line(res("t2", "Traceback (most recent call last):\nAssertionError")))
	sb.WriteString(line(use("t3", "Read", "")))
	sb.WriteString(line(res("t3", "contents")))
	sb.WriteString(line(use("t4", "Bash", edit("b.go", "assert 'p' in s\ns=s.replace('p','q')"))))
	sb.WriteString(line(res("t4", "ok")))
	// 3: a count limit and a hand-rolled backup, two files in one call.
	sb.WriteString(line(use("t5", "Bash",
		"cp c.go c.go.bak\npython3 - <<'PY'\np='c.go'\nq='d.go'\ns=open(p).read()\ns=s.replace('m','n',1)\nopen(p,'w').write(s)\nopen(q,'w').write(s)\nPY")))
	sb.WriteString(line(res("t5", "ok")))
	// 4: not an edit at all.
	sb.WriteString(line(use("t6", "Bash", "ls -la")))
	sb.WriteString(line(res("t6", "a.go")))
	sb.WriteString(line(use("t7", "Edit", "")))
	sb.WriteString(line(res("t7", "ok")))

	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// The rule that decides the denominator of every rate in §12. The mention
// cases are not hypothetical: each one is a shape that has already been
// miscounted, here or in the eval grader.
func TestIsHunkCall(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  string
		want bool
	}{
		{"a bare invocation", "hunk -f p.txt", true},
		{"with flags", "hunk --verify 'make test' -f p.txt", true},
		{"a heredoc patch", "hunk <<'HUNK'\n@@ file a.go\nHUNK", true},
		{"after a separator", "cd /tmp && hunk -f p.txt", true},
		{"piped into", "cat p.txt | hunk", true},
		{"on its own", "hunk", true},
		{"after a newline", "set -e\nhunk -f p.txt", true},
		{"the workspace path, which the eval grader once counted", "cd /workspace/hunk && ls", false},
		{"a make target", "make hunk", false},
		{"merely spoken of", "echo hunk is a tool", false},
		// The probe written to inspect these very transcripts contained this
		// literal as part of its own matching rule, and counted itself.
		{"the string inside a python payload", "python3 - <<'PY'\nif \"| hunk \" in cmd:\n    pass\nPY", false},
		{"a grep pattern", "grep -rn '| hunk ' .", false},
		{"a quoted mention in an echo", "echo '; hunk -f p.txt'", false},
		{"an unterminated heredoc swallows the rest", "cat <<'EOF'\n; hunk -f p.txt", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := IsHunkCall(c.cmd); got != c.want {
				t.Errorf("got %v, want %v\nskeleton: %q", got, c.want, commandSkeleton(c.cmd))
			}
		})
	}
}

func TestCommandSkeleton(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  string
		want string
	}{
		{"nothing to strip", "hunk -f p.txt", "hunk -f p.txt"},
		// The delimiter goes with the quotes, which is why these read oddly.
		// It is the command words that have to survive, and they do.
		{"the line introducing a heredoc survives", "hunk <<'HUNK'\npayload\nHUNK", "hunk <<"},
		{"an unquoted delimiter is a command word and stays", "a <<A\nx\nA", "a <<A"},
		{"a quoted verify keeps its invocation", "hunk --verify 'make test' -f p", "hunk --verify  -f p"},
		{"two heredocs", "a <<'A'\nx\nA\nb <<'B'\ny\nB", "a <<\nb <<"},
		{"an unterminated heredoc takes the rest", "a <<'A'\nx\ny", "a <<"},
		{"an unterminated quote takes the rest", "echo 'x; hunk -f p", "echo "},
		{"a command after a terminated heredoc survives", "a <<'A'\nx\nA\nhunk -f p", "a <<\nhunk -f p"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := commandSkeleton(c.cmd); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// §12 wants an exit-2 rate, an exit-4 rate and a recovery fraction, and the
// transcript records no exit code, so each one is read from hunk's output.
func TestHunkExit(t *testing.T) {
	for _, c := range []struct {
		name   string
		cmd    string
		result string
		want   int
	}{
		{"applied", "hunk -f p", "M a.go +2 -1\n1 file, 1 hunk, +2 -1\n", 0},
		{"applied with a verify", "hunk -f p", "M a.go +2 -1\n1 file, 1 hunk, +2 -1, verify ok (0.7s)\n", 0},
		{"a dry run is still exit 0", "hunk --dry-run -f p", "M a.go +2 -1\n1 file, 1 hunk, +2 -1 (dry run: nothing written)\n", 0},
		{"a parse error", "hunk -f p", "hunk: patch line 17: create a.go is not implemented yet\n", 1},
		{"a usage error", "hunk -f p", "hunk: --eol must be auto or strict, not \"lf\"\n", 1},
		{"an unexpected argument", "hunk x", "hunk: unexpected argument \"x\"; the patch comes from stdin or -f\n", 1},
		{"a hunk did not match", "hunk -f p", "hunk: 1 hunk did not match; nothing was written\n", 2},
		{"several did not match", "hunk -f p", "hunk: 2 of 3 hunks did not match, 1 skipped; nothing was written\n", 2},
		{"verify failed and rolled back", "hunk -f p", "hunk: applied 4 hunks, verify failed, rolled back 3 files\n", 3},
		{"rollback incomplete", "hunk -f p", "hunk: applied 2 hunks, verify failed, rolled back 1 file, 1 file left alone\n", 4},
		{"changed on disk", "hunk -f p", "hunk: a.go changed on disk between being read and being written; nothing was written\n", 6},
		{"--json says so outright", "hunk --json -f p", "{\n  \"ok\": false,\n  \"exit\": 6,\n}\n", 6},
		{"a probe is not an application", "hunk --help", "hunk applies literal text edits\n", ExitProbe},
		{"so is the grammar", "hunk format", "@@ file PATH\n", ExitProbe},
		{"and --version", "hunk --version", "hunk 0.1.0\n", ExitProbe},
		{"an I/O error is not guessed at", "hunk -f p", "hunk: open a.go: permission denied\n", ExitUnclassified},
		{"nor is silence, which --quiet produces", "hunk --quiet -f p", "", ExitUnclassified},
		{"nor is an exit code too large to be one", "hunk --json -f p", "{\n  \"exit\": 99999999999999999999,\n}\n", ExitUnclassified},
		// is_error would have said "success" for this one, which is why it is
		// not consulted: the shell's status is the echo's, not hunk's.
		{
			"the exit is read from the text, not from the shell's status",
			"hunk -f p; echo \"exit=$?\"", "hunk: 1 hunk did not match; nothing was written\nexit=2\n", 2,
		},
		// The verify's own output is agent-written text in the same result.
		{
			"a verify that prints the word rolled back does not become exit 3",
			"hunk -f p", "M a.go +1 -1\n1 file, 1 hunk, +1 -1, verify ok\n  note: applied 4 hunks, verify failed\n", 0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := HunkExit(c.cmd, c.result); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// reportGoldens maps every whole-report golden in testdata/ to the exit it
// belongs to. cli-help is the one that is a probe rather than a report.
var reportGoldens = map[string]int{
	"report-success":                  0,
	"report-success-json":             0,
	"report-success-spec-example":     0,
	"report-dry-run":                  0,
	"cli-all-three-ops":               0,
	"cli-seam-added":                  0,
	"cli-trivial-note":                0,
	"cli-spec-example":                0,
	"cli-spec-example-full":           0,
	"report-validation":               2,
	"report-validation-json":          2,
	"report-no-anchor":                2,
	"report-no-anchor-json":           2,
	"report-no-room":                  2,
	"report-no-room-json":             2,
	"report-verify-failed":            3,
	"report-verify-failed-json":       3,
	"cli-verify-failed":               3,
	"report-rollback-incomplete":      4,
	"report-rollback-incomplete-json": 4,
	"cli-rollback-incomplete":         4,
	"cli-help":                        ExitProbe,
}

// The classifier reads hunk's diagnostics, and SPEC §8.1 says "a reworded
// diagnostic is a contract change". Asserting against the golden files rather
// than against strings copied out of them means a rewording fails this
// measurement in the same run instead of quietly moving a §12 figure.
func TestHunkExitAgainstTheGoldens(t *testing.T) {
	for name, want := range reportGoldens {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name+".txt"))
			if err != nil {
				t.Fatalf("%v: the golden this rule is written against is gone", err)
			}
			cmd := "hunk -f p.txt"
			if want == ExitProbe {
				cmd = "hunk --help"
			}
			if got := HunkExit(cmd, string(b)); got != want {
				t.Errorf("got %d, want %d\n--- golden ---\n%s", got, want, b)
			}
		})
	}
}

// A new report golden is a new shape this classifier has to know about, and
// nothing else would say so. The compiler cannot check it, so a test does.
func TestEveryReportGoldenIsClassified(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("..", "..", "testdata", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range names {
		name := strings.TrimSuffix(filepath.Base(path), ".txt")
		if !strings.HasPrefix(name, "report-") && !strings.HasPrefix(name, "cli-") {
			continue // a near-miss fragment, not a whole report
		}
		if _, ok := reportGoldens[name]; !ok {
			t.Errorf("testdata/%s.txt is a report golden that HunkExit has no expectation for. "+
				"Add it to reportGoldens, or §12 will count it as unclassified.", name)
		}
	}
}

// Agent-shaped input, so the property is totality: every pair of strings
// produces a category and never a panic.
func FuzzHunkExit(f *testing.F) {
	f.Add("hunk -f p.txt", "1 file, 1 hunk, +1 -1\n")
	f.Add("hunk --help", "")
	f.Add("", "")
	f.Add("hunk <<'H'\n@@ old\nH", "hunk: 1 hunk did not match; nothing was written")
	f.Add("hunk --json -f p", "{\"exit\": 3}")
	f.Fuzz(func(t *testing.T, cmd, result string) {
		code := HunkExit(cmd, result)
		if code < ExitUnclassified {
			t.Fatalf("HunkExit returned %d, which is not a category", code)
		}
		// IsHunkCall and HunkExit read the same skeleton; neither may panic on
		// input the other accepts.
		_ = IsHunkCall(cmd)
	})
}

func TestWalk(t *testing.T) {
	s, err := Walk(fixtureCorpus(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"sessions", s.Sessions, 1},
		{"tool calls", s.ToolCalls, 7},
		{"bash calls", s.BashCalls, 5},
		{"edit tool calls", s.EditToolCalls, 1},
		{"heredoc edits", s.HeredocEdits, 4},
		{"replaces", s.Replaces, 4},
		{"without a guard", s.NoGuard, 2},
		{"limiting to the first match", s.FirstOnly, 1},
		{"with neither", s.NoGuardNorLimit, 1},
		{"bundling a verify", s.BundledVerify, 1},
		{"with a backup", s.HandRolledBackup, 1},
		{"file touches", s.FileTouches, 5},
		{"max files in one call", s.MaxFiles, 2},
		{"failed attempts", s.FailedAttempts, 1},
		// a.go, b.go, c.go and d.go each reach the tree once.
		{"edit episodes", s.Episodes, 4},
		// b.go took three calls: the failure, the read, and the success.
		{"calls in episodes", s.CallsInEpisodes, 1 + 3 + 1 + 1},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if got := s.CallsPerSuccessfulEdit(); got != 1.5 {
		t.Errorf("calls per successful edit = %v, want 1.5", got)
	}
	if s.Projects["-workspace-demo"] != 4 {
		t.Errorf("by project = %v", s.Projects)
	}
}

// A tool_result's content is a string in most records and a list of blocks in
// some, and reading it raw is what made every anchored rule match nothing.
func TestResultText(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  string
		want string
	}{
		{
			"a plain string, with its escapes undone", `"hunk: 1 hunk did not match\nnothing was written"`,
			"hunk: 1 hunk did not match\nnothing was written",
		},
		{"a list of blocks, joined", `[{"type":"text","text":"a\n"},{"type":"text","text":"b"}]`, "a\nb"},
		{"an empty list", `[]`, ""},
		{"null", `null`, ""},
		{"a shape this tool does not know is returned raw", `42`, "42"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := resultText(json.RawMessage(c.raw)); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// The §12 numbers that had no instrument until 2026-09-05: the exit table, the
// probe split, and the recovery fraction.
func TestWalkCountsHunkExits(t *testing.T) {
	s, err := Walk(fixtureHunkCorpus(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  int
		want int
	}{
		{"invocations, probes included", s.HunkCalls, 8},
		{"applications", s.HunkApplications, 7},
		{"probes", s.HunkProbes, 1},
		{"unclassified", s.HunkUnclassified, 1},
		{"refusals", s.HunkExits[2], 4},
		{"successes", s.HunkExits[0], 1},
		{"incomplete rollbacks", s.HunkExits[4], 1},
		// Four refusals, and only two of them can be scored: one followed by a
		// success, one followed by an exit 4, one followed by a call this
		// classifier could not read, and one with nothing after it at all. The
		// last two are in neither the numerator nor the denominator, because
		// calling either a failure to recover would score §7 against a gap in
		// this file.
		{"refusals with a classified next call", s.Exit2Followed, 2},
		{"refusals recovered", s.Exit2Recovered, 1},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if got := s.Exit2RecoveryRate(); got != 0.5 {
		t.Errorf("recovery rate = %v, want 0.5", got)
	}
	if got := (Stats{}).Exit2RecoveryRate(); got != 0 {
		t.Errorf("recovery rate with nothing to divide = %v, want 0", got)
	}
	// The block is printed only when there were calls, and the unclassified
	// row is printed even at zero, because a silent zero is what hid an
	// unwritten map behind an omitempty.
	rep := s.Report()
	for _, want := range []string{"applications", "probes", "exit 2", "unclassified", "exit-2 recovery"} {
		if !strings.Contains(rep, want) {
			t.Errorf("the report does not mention %q:\n%s", want, rep)
		}
	}
}

// A session with hunk in it, kept apart from fixtureCorpus so that §1's counts
// and §12's do not have to be read together.
func fixtureHunkCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "-workspace-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(blocks ...map[string]any) string {
		b, err := json.Marshal(map[string]any{"message": map[string]any{"content": blocks}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	use := func(id, cmd string) map[string]any {
		return map[string]any{
			"type": "tool_use", "id": id, "name": "Bash",
			"input": map[string]any{"command": cmd},
		}
	}
	res := func(id, out string) map[string]any {
		return map[string]any{"type": "tool_result", "tool_use_id": id, "content": out}
	}
	var sb strings.Builder
	add := func(id, cmd, out string) {
		sb.WriteString(line(use(id, cmd)))
		sb.WriteString(line(res(id, out)))
	}
	add("h0", "hunk format", "@@ file PATH\n")
	add("h1", "hunk -f p.txt", "hunk: 1 hunk did not match; nothing was written\n")
	add("h2", "hunk -f p.txt", "M a.go +1 -1\n1 file, 1 hunk, +1 -1\n")
	add("h3", "hunk -f p.txt", "hunk: 1 hunk did not match; nothing was written\n")
	add("h4", "hunk --verify 'make fmt' -f p.txt",
		"hunk: applied 2 hunks, verify failed, rolled back 1 file, 1 file left alone\n")
	add("h5", "hunk -f p.txt", "hunk: 1 hunk did not match; nothing was written\n")
	// An application whose output this classifier does not recognise. The
	// refusal before it cannot be scored either way.
	add("h6", "hunk -f p.txt", "hunk: open a.go: permission denied\n")
	// A refusal with nothing after it: the session ended, which is not a
	// failure to recover.
	add("h7", "hunk -f p.txt", "hunk: 1 hunk did not match; nothing was written\n")
	// Mentions rather than invocations. Neither is a call.
	add("m1", "grep -rn '| hunk ' .", "classify.go:89\n")
	add("m2", "cd /workspace/hunk && ls", "main.go\n")

	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// §12 compares two runs. A measurement that does not repeat cannot be compared
// with itself, let alone with a later one.
func TestWalkIsReproducible(t *testing.T) {
	root := fixtureCorpus(t)
	a, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("two runs disagree:\n%s\n%s", ja, jb)
	}
}

func TestReportIsStable(t *testing.T) {
	s, err := Walk(fixtureCorpus(t))
	if err != nil {
		t.Fatal(err)
	}
	s.Root = "<root>" // the temp dir is not stable across runs
	got := s.Report()
	want, err := os.ReadFile(filepath.Join("testdata", "fixture-report.txt"))
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "fixture-report.txt"), []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil {
		t.Fatalf("%v (UPDATE_GOLDEN=1 go test ./... to create it, then read it)", err)
	}
	if got != string(want) {
		t.Errorf("the report changed. That is a contract change: §12 compares two of these.\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// A record shape this tool does not know is not an error: transcripts carry
// many, and a measurement that dies on one is a measurement nobody can run.
func TestUnknownRecordsAreSkipped(t *testing.T) {
	root := t.TempDir()
	body := "not json at all\n" +
		`{"type":"summary"}` + "\n" +
		`{"message":{"content":"a string, not a list"}}` + "\n" +
		`{"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "s.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Walk(root)
	if err != nil {
		t.Fatalf("a malformed line stopped the walk: %v", err)
	}
	if s.BashCalls != 1 {
		t.Errorf("bash calls = %d, want 1", s.BashCalls)
	}
}
