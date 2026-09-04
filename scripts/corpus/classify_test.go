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
		return map[string]any{"type": "tool_use", "id": id, "name": name,
			"input": map[string]any{"command": cmd}}
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
