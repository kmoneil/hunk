package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
)

// runCLI drives the real entry point in process, with a real tree.
// TestMain makes the test binary hunk itself when HUNK_TEST_AS_HUNK is set, so
// a test can run it as a separate process. §8.1's concurrency test is about two
// hunk processes, and two goroutines in one would share a scheduler and a
// process that two runs of the tool do not.
func TestMain(m *testing.M) {
	if os.Getenv("HUNK_TEST_AS_HUNK") == "1" {
		main()
	}
	os.Exit(m.Run())
}

func runCLI(t *testing.T, root string, args []string, stdin string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	args = append([]string{"--root", root}, args...)
	code = cli(args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func cliTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	for name, body := range files {
		full := filepath.Join(root, name)
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(t, os.WriteFile(full, []byte(body), 0o644))
	}
	return root
}

// §4.1 is a closed set, and four of its seven rows promise the tree is
// untouched. Those are assertions, not documentation: the whole tree is hashed
// before and after.
func TestExitCodeWalk(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		args      []string
		stdin     string
		want      int
		untouched bool
	}{
		{
			name:  "0, applied",
			files: map[string]string{"a.go": "one\n"},
			stdin: "@@ file a.go\n@@ old\none\n@@ new\nONE\n",
			want:  exitOK,
		},
		{
			name:      "1, a parse error",
			files:     map[string]string{"a.go": "one\n"},
			stdin:     "@@ file a.go\n@@ old\none\n",
			want:      exitUsage,
			untouched: true,
		},
		{
			name:      "1, an unknown flag",
			files:     map[string]string{"a.go": "one\n"},
			args:      []string{"--nope"},
			stdin:     "@@ file a.go\n@@ old\none\n@@ new\nONE\n",
			want:      exitUsage,
			untouched: true,
		},
		{
			// A valid patch on stdin that must not be applied: --version stops
			// before the tree is opened, in the way --help does.
			name:      "0, --version stops before anything is read",
			files:     map[string]string{"a.go": "one\n"},
			args:      []string{"--version"},
			stdin:     "@@ file a.go\n@@ old\none\n@@ new\nONE\n",
			want:      exitOK,
			untouched: true,
		},
		{
			name:      "2, a hunk did not match",
			files:     map[string]string{"a.go": "one\n"},
			stdin:     "@@ file a.go\n@@ old\nabsent\n@@ new\nX\n",
			want:      exitNoMatch,
			untouched: true,
		},
		{
			name:      "2, a path that escapes the root",
			files:     map[string]string{"a.go": "one\n"},
			stdin:     "@@ file ../out.go\n@@ old\none\n@@ new\nONE\n",
			want:      exitNoMatch,
			untouched: true,
		},
		{
			name:      "2, a missing file",
			files:     map[string]string{"a.go": "one\n"},
			stdin:     "@@ file gone.go\n@@ old\none\n@@ new\nONE\n",
			want:      exitNoMatch,
			untouched: true,
		},
		{
			name:      "2, a create over a file that exists",
			files:     map[string]string{"a.go": "one\n"},
			stdin:     "@@ create a.go\nnew\n",
			want:      exitNoMatch,
			untouched: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, c.files)
			before := snapshot(t, root)
			code, _, _ := runCLI(t, root, c.args, c.stdin)
			if code != c.want {
				t.Errorf("exit %d, want %d", code, c.want)
			}
			if c.untouched {
				assertUnchanged(t, root, before)
			}
		})
	}

	// Exit 6 needs a writer between load and commit, which the CLI closes
	// itself; it is asserted at the phase level in apply_test.go. The mapping
	// from a ChangedError to 6 is asserted in report_test.go. This records why
	// the walk above has six rows and not seven.
	if ExitCode(&ChangedError{Path: "x"}) != exitChanged {
		t.Error("the exit-6 mapping moved")
	}
}

// The project's acceptance test, minus the part that needs an op this binary
// does not have. §3.6's example ends in @@ create, which belongs to
// create-delete-append-prepend; the whole example is that card's done-when.
//
// The prefix is taken from the string `hunk format` prints, not retyped, so
// this cannot drift from what the tool teaches.
func TestSpecWorkedExampleApplies(t *testing.T) {
	const createDirective = "@@ create internal/cli/scope.go"
	i := strings.Index(formatExamplePatch, createDirective)
	if i < 0 {
		t.Fatalf("§3.6's example no longer creates a file; take the whole example now")
	}
	prefix := formatExamplePatch[:i]

	root := cliTree(t, map[string]string{
		"internal/cli/root.go":         "import (\n\t\"github.com/kmoneil/jr/internal/registry\"\n)\n\nfunc main() {\n\tbind.mustHaveBoundEveryGlobal()\n}\n",
		"internal/registry/globals.go": "const (\n\tGlobalProject\n\tGlobalOther\n\tGlobalProject\n)\n",
	})
	code, out, errOut := runCLI(t, root, nil, prefix)
	if code != exitOK {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	golden(t, "cli-spec-example", out)

	got := readFile(t, root, "internal/cli/root.go")
	for _, want := range []string{
		"bind.mustHaveBoundEveryGlobal()\n\tbind.mustHaveBoundEveryScope()",
		"\"github.com/kmoneil/jr/internal/registry\"\n\t\"github.com/kmoneil/jr/internal/scope\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("root.go is missing %q:\n%s", want, got)
		}
	}
	if g := readFile(t, root, "internal/registry/globals.go"); strings.Count(g, "GlobalProject, GlobalScope") != 2 {
		t.Errorf("the x2 hunk did not replace both:\n%s", g)
	}
}

func TestFlags(t *testing.T) {
	patch := "@@ file a.go\n@@ old\none\n@@ new\nONE\n"

	t.Run("--dry-run writes nothing and says so", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\n"})
		before := snapshot(t, root)
		code, out, _ := runCLI(t, root, []string{"--dry-run"}, patch)
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(out, "dry run: nothing written") {
			t.Errorf("out = %q", out)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("--dry-run still exits 2 on a mismatch", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--dry-run"},
			"@@ file a.go\n@@ old\nabsent\n@@ new\nX\n")
		if code != exitNoMatch {
			t.Errorf("exit %d, want 2", code)
		}
		if !strings.Contains(errOut, "did not match") {
			t.Errorf("err = %q", errOut)
		}
	})

	t.Run("--quiet prints nothing on success and still reports a failure", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\n"})
		_, out, errOut := runCLI(t, root, []string{"--quiet"}, patch)
		if out != "" || errOut != "" {
			t.Errorf("out=%q err=%q", out, errOut)
		}
		_, out, errOut = runCLI(t, root, []string{"--quiet"},
			"@@ file a.go\n@@ old\nabsent\n@@ new\nX\n")
		if errOut == "" {
			t.Error("quiet swallowed a failure")
		}
		if out != "" {
			t.Errorf("stdout = %q", out)
		}
	})

	t.Run("--json is alone on stdout and wins over --quiet", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\n"})
		code, out, errOut := runCLI(t, root, []string{"--json", "--quiet"}, patch)
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		if errOut != "" {
			t.Errorf("stderr = %q", errOut)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
		}
		if v["ok"] != true {
			t.Errorf("ok = %v", v["ok"])
		}
	})

	t.Run("--json on a failure too", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\n"})
		code, out, errOut := runCLI(t, root, []string{"--json"},
			"@@ file a.go\n@@ old\nabsent\n@@ new\nX\n")
		if code != exitNoMatch {
			t.Fatalf("exit %d", code)
		}
		if errOut != "" {
			t.Errorf("stderr = %q", errOut)
		}
		var v map[string]any
		must(t, json.Unmarshal([]byte(out), &v))
		if v["exit"].(float64) != 2 {
			t.Errorf("exit field = %v", v["exit"])
		}
	})

	t.Run("--marker picks another prefix", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "@@ old\n"})
		code, _, errOut := runCLI(t, root, []string{"--marker", "%%"},
			"%% file a.go\n%% old\n@@ old\n%% new\nDONE\n")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if got := readFile(t, root, "a.go"); got != "DONE\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("-f reads the patch from a file, relative to the working directory", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\n"})
		pf := filepath.Join(t.TempDir(), "p.txt")
		must(t, os.WriteFile(pf, []byte(patch), 0o644))
		code, _, errOut := runCLI(t, root, []string{"-f", pf}, "")
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if got := readFile(t, root, "a.go"); got != "ONE\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("--eol strict does not translate", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\r\ntwo\r\n"})
		if code, _, _ := runCLI(t, root, nil, "@@ file a.go\n@@ old\none\n\n@@ new\nONE\n\n"); code != exitOK {
			t.Errorf("auto should have translated: exit %d", code)
		}
		root = cliTree(t, map[string]string{"a.go": "one\r\ntwo\r\n"})
		if code, _, _ := runCLI(t, root, []string{"--eol", "strict"},
			"@@ file a.go\n@@ old\none\n\n@@ new\nONE\n\n"); code != exitNoMatch {
			t.Errorf("strict should have missed: exit %d", code)
		}
	})

	t.Run("--context caps the near-miss span", func(t *testing.T) {
		var file, old strings.Builder
		for i := 0; i < 12; i++ {
			file.WriteString("    x\n")
			old.WriteString("\tx\n")
		}
		root := cliTree(t, map[string]string{"a.go": file.String()})
		_, _, errOut := runCLI(t, root, []string{"--context", "3"},
			"@@ file a.go\n@@ old\n"+old.String()+"@@ new\nY\n")
		if n := strings.Count(errOut, " | "); n != 3 {
			t.Errorf("%d gutter lines, want the --context cap of 3:\n%s", n, errOut)
		}
	})

	t.Run("--allow-outside-root lifts the confinement", func(t *testing.T) {
		outside := t.TempDir()
		if r, err := filepath.EvalSymlinks(outside); err == nil {
			outside = r
		}
		must(t, os.WriteFile(filepath.Join(outside, "o.go"), []byte("one\n"), 0o644))
		root := cliTree(t, map[string]string{"a.go": "x\n"})
		p := "@@ file " + filepath.Join(outside, "o.go") + "\n@@ old\none\n@@ new\nONE\n"
		if code, _, _ := runCLI(t, root, nil, p); code != exitNoMatch {
			t.Errorf("confined run should refuse: exit %d", code)
		}
		code, _, errOut := runCLI(t, root, []string{"--allow-outside-root"}, p)
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		b, err := os.ReadFile(filepath.Join(outside, "o.go"))
		must(t, err)
		if string(b) != "ONE\n" {
			t.Errorf("got %q", b)
		}
	})
}

// The verify family was refused here until verify-and-rollback landed; that
// test pinned a deliberately temporary contract and went red the moment the
// contract changed, which is what it was for. The flags' behaviour now lives in
// verify_test.go, and what remains here is the interaction that is still the
// CLI's: a flag that would do nothing is refused rather than accepted.
func TestFlagsThatWouldDoNothingAreRefused(t *testing.T) {
	root := cliTree(t, map[string]string{"a.go": "one\n"})
	before := snapshot(t, root)
	code, _, errOut := runCLI(t, root, []string{"--keep-on-fail"},
		"@@ file a.go\n@@ old\none\n@@ new\nONE\n")
	if code != exitUsage {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut, "does nothing without --verify") {
		t.Errorf("err = %q", errOut)
	}
	assertUnchanged(t, root, before)
}

func TestUsageErrors(t *testing.T) {
	for _, c := range []struct {
		name, msg string
		args      []string
		stdin     string
	}{
		{"an unexpected argument", "unexpected argument", []string{"extra"}, "@@ delete a.go\n"},
		{"a bad --eol", "must be auto or strict", []string{"--eol", "sideways"}, ""},
		{"an empty --marker", "must not be empty", []string{"--marker", ""}, ""},
		{"a --context below one", "at least 1", []string{"--context", "0"}, ""},
		{"a -f that does not exist", notExistPhrase(), []string{"-f", "/nonexistent/p.txt"}, ""},
		{"an empty patch", "the patch is empty", nil, "   \n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.go": "one\n"})
			code, out, errOut := runCLI(t, root, c.args, c.stdin)
			if code != exitUsage {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(errOut, c.msg) {
				t.Errorf("err = %q, want it to mention %q", errOut, c.msg)
			}
			if out != "" {
				t.Errorf("stdout = %q; a usage error belongs on stderr", out)
			}
		})
	}
}

// §9: --help "must be complete enough to use the tool from cold", and the spec
// commits it to three specific sentences. A golden catches a reword; these
// catch a deletion, which is the failure that matters.
func TestHelpCarriesWhatTheSpecCommitsItTo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli([]string{"--help"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	golden(t, "cli-help", got)

	// §6.2: the batch-atomicity limit is stated "because a tool that overstates
	// its guarantees is worse than one that has none".
	if !strings.Contains(got, "The batch is not") {
		t.Error("--help does not state the batch-atomicity limit (§6.2)")
	}
	// And the concurrency limit beside it, which until 2026-09-21 it stated as
	// a window "narrowed to microseconds". Two runs started together nearly
	// always both pass the check; TestTwoProcessesOnTheSameFiles counts it.
	// Compared with the page's whitespace collapsed, so where a line wraps is
	// not part of the assertion.
	if flat := strings.Join(strings.Fields(got), " "); !strings.Contains(flat, "not one writing at the same time") {
		t.Error("--help does not state that a concurrent writer is not caught (§6.2)")
	}
	// §6.3: the flag and the reason together, not just the behaviour.
	if !strings.Contains(got, "--verify-may-format") ||
		!strings.Contains(got, "silently reverting another writer's work") {
		t.Error("--help gives --verify-may-format without its reason (§6.3)")
	}
	// §11: --help quotes the figure for why --verify is optional. It was 66%
	// until the baseline measurement found that figure counted a verify word
	// appearing inside the text an edit writes, rather than a command run after
	// it. scripts/corpus is what re-derives it.
	if !strings.Contains(got, "58%") {
		t.Error("--help does not quote §11's figure for why --verify is optional")
	}
	// Every flag in §4's table has an entry of its own. Until 2026-09-21 this
	// was strings.Contains over the whole page, and deleting the -f or the
	// --root entry and regenerating the golden failed nothing: "-f" is also in
	// the synopsis and inside --keep-on-fail, and "--root" is in
	// --allow-outside-root's description.
	listed := helpFlags(got)
	for _, f := range specFlags {
		if !slices.Contains(listed, f) {
			t.Errorf("--help has no entry for %s", f)
		}
	}
	// And every exit code.
	for _, c := range []string{"0", "1", "2", "3", "4", "5", "6"} {
		if !strings.Contains(got, "\n  "+c+"  ") {
			t.Errorf("--help does not list exit code %s", c)
		}
	}
	if errOut.String() != "" {
		t.Errorf("--help wrote to stderr: %q", errOut.String())
	}
}

// specFlags is §4's flag table: every flag the spec commits --help to.
var specFlags = []string{
	"--verify", "--verify-may-format", "--verify-lines", "--keep-on-fail", "--try",
	"--dry-run", "--root", "--marker", "-f", "--json", "--quiet",
	"--context", "--eol", "--allow-outside-root", "--version",
}

// An entry in --help's FLAGS section starts with the flag at column two. The
// lines between entries continue a description and start at column
// twenty-four, so they cannot match.
var reHelpFlag = regexp.MustCompile(`(?m)^  (--?[a-z][a-z-]*)`)

// helpFlags reads the flags --help has entries for, in order. Only the FLAGS
// section counts: the synopsis above it mentions -f too, and a flag mentioned
// is not a flag documented.
func helpFlags(help string) []string {
	_, section, ok := strings.Cut(help, "\nFLAGS\n")
	if !ok {
		return nil
	}
	section, _, _ = strings.Cut(section, "\nEXIT CODES\n")
	var flags []string
	for _, m := range reHelpFlag.FindAllStringSubmatch(section, -1) {
		flags = append(flags, m[1])
	}
	return flags
}

// namesFlag reports whether text names flag as a word of its own. A substring
// is not enough: "--verify-may-format" contains "--verify", and
// "--keep-on-fail" contains "-f".
func namesFlag(text, flag string) bool {
	return regexp.MustCompile(`(^|[^A-Za-z0-9-])` + regexp.QuoteMeta(flag) + `($|[^A-Za-z0-9-])`).
		MatchString(text)
}

func TestHelpFlagsReadsOnlyTheFlagsSection(t *testing.T) {
	const help = "hunk applies edits.\n\n" +
		"  hunk [flags] -f patch.txt    patch from a file\n\n" +
		"FLAGS\n\n" +
		"  --verify CMD          Shell command run after a successful apply. Non-zero\n" +
		"                        --rolls everything back.\n" +
		"  -f FILE               Read the patch from a file.\n" +
		"  --eol auto|strict     auto converts line endings. (auto)\n\n" +
		"EXIT CODES\n\n" +
		"  0  applied\n" +
		"  --not-a-flag\n"
	for _, c := range []struct {
		name, help string
		want       []string
	}{
		{"an entry each, the synopsis, a continuation and exit codes skipped", help, []string{"--verify", "-f", "--eol"}},
		{"no FLAGS section", "hunk applies edits.\n  --verify CMD  a flag outside any section\n", nil},
		{"no EXIT CODES after it, so it runs to the end", "x\nFLAGS\n  --json   JSON.\n", []string{"--json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := helpFlags(c.help); !slices.Equal(got, c.want) {
				t.Errorf("helpFlags = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNamesFlagNeedsAWordOfItsOwn(t *testing.T) {
	for _, c := range []struct {
		text, flag string
		want       bool
	}{
		{"hunk --verify 'go test ./...'", "--verify", true},
		{"runs in `--root`, via sh", "--root", true},
		{"--json at the start", "--json", true},
		{"at the end, --json", "--json", true},
		{"the flag (--json).", "--json", true},
		{"hunk --dry-run -f patch.txt", "-f", true},
		{"pass --verify-may-format", "--verify", false},
		{"pass --keep-on-fail", "-f", false},
		{"--jsonl is another flag", "--json", false},
		{"x--json", "--json", false},
		{"", "--json", false},
	} {
		if got := namesFlag(c.text, c.flag); got != c.want {
			t.Errorf("namesFlag(%q, %q) = %v, want %v", c.text, c.flag, got, c.want)
		}
	}
}

// skillOmits is every flag --help lists that SKILL.md leaves out on purpose,
// each with the reason.
var skillOmits = map[string]string{
	"--verify-lines":       "the 40-line tail is enough to act on, and the whole output is a rerun away",
	"--quiet":              "an agent reads the success report, and printing nothing saves one line and loses the confirmation",
	"--context":            "the near-miss span is bounded by old, so the 20-line cap rarely binds (§11)",
	"--eol":                "auto is the default and translates a CRLF file for an LF heredoc; strict is the byte-exact exception",
	"--allow-outside-root": "it is the way out of §6.5's path safety, and the skill should not be where an agent learns it",
	"--version":            "it describes the binary and edits nothing",
}

// The skill is the one document most agents read before their first call, and
// three things have now shipped in --help and stayed out of it: --dry-run and
// four of the five directives until 2026-09-06, and --keep-on-fail until
// 2026-09-21, when a field report asked for "a keep-changes-and-report mode"
// that had existed since the CLI was wired. TestTheSkillDocumentsEveryDirective
// closed the directive half of that. This is the flag half.
//
// A flag may stay out of the skill, but only by being in skillOmits with its
// reason, so that leaving one out is a decision somebody wrote down. The flags
// come from --help, so a flag the FlagSet defines and --help leaves out is
// invisible here, as it is to TestHelpCarriesWhatTheSpecCommitsItTo.
func TestTheSkillNamesEveryFlagOrSaysWhyNot(t *testing.T) {
	var out bytes.Buffer
	if code := cli([]string{"--help"}, strings.NewReader(""), &out, io.Discard); code != exitOK {
		t.Fatalf("--help: exit %d", code)
	}
	listed := helpFlags(out.String())
	if len(listed) == 0 {
		t.Fatal("read no flags from --help; every check below would pass")
	}
	b, err := os.ReadFile(filepath.Join(".agents", "skills", "hunk", "SKILL.md"))
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	skill := string(b)

	for _, f := range listed {
		t.Run(f, func(t *testing.T) {
			why, omitted := skillOmits[f]
			switch named := namesFlag(skill, f); {
			case !named && !omitted:
				t.Errorf("SKILL.md never names %s; a flag an agent cannot see is one it will not use. "+
					"Name it, or add it to skillOmits with the reason", f)
			case named && omitted:
				t.Errorf("SKILL.md names %s, which skillOmits leaves out because %s; drop the entry", f, why)
			}
		})
	}
	t.Run("every omission is a flag --help lists", func(t *testing.T) {
		for f := range skillOmits {
			if !slices.Contains(listed, f) {
				t.Errorf("skillOmits leaves out %s, which --help has no entry for", f)
			}
		}
	})
}

func TestFormatSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli([]string{"format"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if out.String() != formatDoc {
		t.Error("format printed something other than formatDoc")
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// Somebody typing "hunk" at a prompt would otherwise wait for a program that is
// waiting for them.
func TestATerminalOnStdinIsRefusedRatherThanBlocking(t *testing.T) {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		t.Skip("no controlling terminal")
	}
	defer tty.Close()
	var out, errOut bytes.Buffer
	code := cli(nil, tty, &out, &errOut)
	if code != exitUsage {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "hunk format") {
		t.Errorf("err = %q", errOut.String())
	}
}

// format is a subcommand and comes first, per §4's synopsis. Both ways of
// getting that wrong say which.
func TestFormatSubcommandPlacement(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		msg  string
	}{
		{"an argument after format", []string{"format", "x"}, "takes no arguments"},
		{"a flag before format", []string{"--quiet", "format"}, "subcommand and takes no flags"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := cli(c.args, strings.NewReader(""), &out, &errOut); code != exitUsage {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(errOut.String(), c.msg) {
				t.Errorf("err = %q, want %q", errOut.String(), c.msg)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk gone") }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("pipe gone") }

func TestCLIEdges(t *testing.T) {
	patch := "@@ file a.go\n@@ old\none\n@@ new\nONE\n"

	// Without --root the tree is the working directory (§4).
	t.Run("no --root means the working directory", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\n"})
		t.Chdir(root)
		var out, errOut bytes.Buffer
		if code := cli(nil, strings.NewReader(patch), &out, &errOut); code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut.String())
		}
		if got := readFile(t, root, "a.go"); got != "ONE\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a --root that does not exist is an I/O error", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := cli([]string{"--root", filepath.Join(t.TempDir(), "nope")},
			strings.NewReader(patch), &out, &errOut)
		if code != exitIO {
			t.Errorf("exit %d, want 5", code)
		}
		if errOut.String() == "" {
			t.Error("no message")
		}
	})

	t.Run("a stdout that cannot be written is an I/O error", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.go": "one\n"})
		var errOut bytes.Buffer
		code := cli([]string{"--root", root, "--json"}, strings.NewReader(patch),
			failingWriter{}, &errOut)
		if code != exitIO {
			t.Errorf("exit %d, want 5", code)
		}
	})

	t.Run("a stdin that cannot be read is a usage error", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := cli([]string{"--root", t.TempDir()}, failingReader{}, &out, &errOut)
		if code != exitUsage {
			t.Errorf("exit %d, want 1", code)
		}
		if !strings.Contains(errOut.String(), "pipe gone") {
			t.Errorf("err = %q", errOut.String())
		}
	})

	// A character device on stdin with no -f means nobody piped anything in.
	// The commonest way to hit it is typing "hunk" at a prompt and waiting for
	// a program that is waiting for you; /dev/null reaches the same branch,
	// and the message is true of both: there is no patch.
	t.Run("a character device on stdin is refused rather than read", func(t *testing.T) {
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			t.Skip(err)
		}
		defer devnull.Close()
		var out, errOut bytes.Buffer
		if code := cli([]string{"--root", t.TempDir()}, devnull, &out, &errOut); code != exitUsage {
			t.Errorf("exit %d, want 1", code)
		}
		if !strings.Contains(errOut.String(), "hunk format") {
			t.Errorf("err = %q", errOut.String())
		}
	})
}

// The skill ships a worked example, and an example that does not apply is worse
// than no example: it teaches the shape and then fails on first use. `hunk
// format`'s example is pinned by TestFormatExampleParses; this pins the
// skill's, end to end through the binary rather than only through the parser.
func TestSkillExampleApplies(t *testing.T) {
	skill, err := os.ReadFile(filepath.Join(".agents", "skills", "hunk", "SKILL.md"))
	if err != nil {
		t.Skip("no skill shipped:", err)
	}
	const open, close = "hunk --verify 'go test ./...' <<'HUNK'\n", "\nHUNK\n"
	i := strings.Index(string(skill), open)
	if i < 0 {
		t.Fatal("the skill no longer shows a worked example, or its opening changed")
	}
	rest := string(skill)[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatal("the skill's example is unterminated")
	}
	patch := rest[:j+1]

	root := cliTree(t, map[string]string{
		"internal/cli/root.go": "func main() {\n\tbind.mustHaveBoundEveryGlobal()\n" +
			"\tGlobalProject\n\tGlobalProject\n}\n",
	})
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitOK {
		t.Fatalf("the example in the skill does not apply: exit %d\n%s%s\n--- patch ---\n%s",
			code, out, errOut, patch)
	}
	got := readFile(t, root, "internal/cli/root.go")
	if !strings.Contains(got, "bind.mustHaveBoundEveryScope()") {
		t.Errorf("the replace did not land:\n%s", got)
	}
	if strings.Count(got, "GlobalProject, GlobalScope") != 2 {
		t.Errorf("the x2 hunk did not replace both:\n%s", got)
	}
	if !strings.HasSuffix(readFile(t, root, "internal/cli/scope.go"), "\n") {
		t.Error("the created file has no final newline")
	}
}

// The skill's second example is shell that writes a patch, which is the more
// fragile kind: it has to run, the patch it prints has to apply, and a tree
// where one file is not as expected has to be refused whole. Added 2026-09-21,
// when a field agent did five release bumps with hunk one day and three with a
// Python loop the next, because the skill had filed a repeated literal edit
// under "computed". Its heredocs are run by sh as the skill prints them; only
// the pipe into hunk is replaced by an in-process run.
func TestSkillLoopExampleApplies(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".agents", "skills", "hunk", "SKILL.md"))
	if err != nil {
		t.Fatalf("read the skill: %v", err)
	}
	skill := string(b)
	i := strings.Index(skill, "\n{\n  for f in ")
	if i < 0 {
		t.Fatal("the skill no longer shows a patch written by a loop, or its opening changed")
	}
	rest := skill[i+1:]
	j := strings.Index(rest, "\n} | hunk ")
	if j < 0 {
		t.Fatal("the skill's loop is not piped into hunk")
	}
	script := rest[:j] + "\n}\n"

	generate := func(t *testing.T, root string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = root
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Run(); err != nil {
			t.Fatalf("the skill's loop does not run: %v\n%s\n--- script ---\n%s", err, errOut.String(), script)
		}
		return out.String()
	}
	const before, after = "package main\n\nconst Version = \"1.4.0\"\n", "package main\n\nconst Version = \"1.5.0\"\n"
	changelog := "# Changelog\n\n## 1.5.0 (unreleased)\n\n- one thing\n"

	t.Run("every file has the text once, so all of them change", func(t *testing.T) {
		root := cliTree(t, map[string]string{
			"cmd/a/version.go": before,
			"cmd/b/version.go": before,
			"CHANGELOG.md":     changelog,
		})
		code, out, errOut := runCLI(t, root, nil, generate(t, root))
		if code != exitOK {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		for _, f := range []string{"cmd/a/version.go", "cmd/b/version.go"} {
			if got := readFile(t, root, f); got != after {
				t.Errorf("%s = %q, want %q", f, got, after)
			}
		}
		if got := readFile(t, root, "CHANGELOG.md"); !strings.Contains(got, "## 1.5.0 (2026-09-21)\n") {
			t.Errorf("CHANGELOG.md was not dated:\n%s", got)
		}
		if !strings.Contains(out, "3 files, 3 hunks, +3 -3") {
			t.Errorf("report = %q", out)
		}
	})

	t.Run("one file is not as expected, so nothing is written", func(t *testing.T) {
		files := map[string]string{
			"cmd/a/version.go": before,
			"cmd/b/version.go": "package main\n\nconst Version = \"1.3.9\"\n",
			"CHANGELOG.md":     changelog,
		}
		root := cliTree(t, files)
		code, _, errOut := runCLI(t, root, nil, generate(t, root))
		if code != exitNoMatch {
			t.Fatalf("exit %d, want %d\n%s", code, exitNoMatch, errOut)
		}
		if !strings.Contains(errOut, "cmd/b/version.go") || !strings.Contains(errOut, "nothing was written") {
			t.Errorf("the refusal does not name the file that missed, or does not say nothing was written:\n%s", errOut)
		}
		for name, body := range files {
			if got := readFile(t, root, name); got != body {
				t.Errorf("%s changed under a refusal: %q", name, got)
			}
		}
	})
}

// The skill's parts layout, decided 2026-09-21 as the answer to "one small miss
// throws away the whole batch". It rests on one property, that parts piped
// together are one batch, and on four limits the skill states. The pipe run
// here is the skill's own, taken from its text, so the section cannot change
// its command without this noticing.
func TestSkillPartsLayoutIsOneBatch(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".agents", "skills", "hunk", "SKILL.md"))
	if err != nil {
		t.Skip("no skill shipped:", err)
	}
	const pipe = `cat "$d"/*.hunk | hunk`
	if !strings.Contains(string(b), "$ "+pipe) {
		t.Fatalf("the skill no longer shows %q", pipe)
	}
	// The same shell, the same glob and the same order the agent gets.
	stream := func(t *testing.T, parts map[string]string) string {
		t.Helper()
		d := t.TempDir()
		for name, body := range parts {
			must(t, os.WriteFile(filepath.Join(d, name), []byte(body), 0o644))
		}
		cmd := exec.Command("sh", "-c", strings.TrimSuffix(pipe, " | hunk"))
		cmd.Env = append(os.Environ(), "d="+d)
		out, err := cmd.Output()
		must(t, err)
		return string(out)
	}
	files := map[string]string{"a.go": "const X = 1\n", "b.go": "const Y = 2\n"}
	partA := "@@ file a.go\n@@ old\nconst X = 1\n@@ new\nconst X = 10\n"
	partB := "@@ file b.go\n@@ old\nconst Y = 2\n@@ new\nconst Y = 20\n"

	t.Run("a miss in one part writes nothing in any", func(t *testing.T) {
		root := cliTree(t, files)
		before := snapshot(t, root)
		miss := "@@ file a.go\n@@ old\n    const X = 1\n@@ new\nconst X = 10\n"
		code, _, errOut := runCLI(t, root, nil, stream(t, map[string]string{"1-a.hunk": miss, "2-b.hunk": partB}))
		if code != exitNoMatch {
			t.Fatalf("exit %d, want 2: %s", code, errOut)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("rewriting that part alone and piping again applies both, together", func(t *testing.T) {
		root := cliTree(t, files)
		code, out, errOut := runCLI(t, root, nil, stream(t, map[string]string{"1-a.hunk": partA, "2-b.hunk": partB}))
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if !strings.Contains(out, "2 files, 2 hunks") {
			t.Errorf("not one batch: %q", out)
		}
		if readFile(t, root, "a.go") != "const X = 10\n" || readFile(t, root, "b.go") != "const Y = 20\n" {
			t.Errorf("a.go %q, b.go %q", readFile(t, root, "a.go"), readFile(t, root, "b.go"))
		}
	})

	t.Run("patch lines count through the stream", func(t *testing.T) {
		root := cliTree(t, files)
		missB := "@@ file b.go\n@@ old\nconst Y = 3\n@@ new\nconst Y = 20\n"
		_, _, errOut := runCLI(t, root, nil, stream(t, map[string]string{"1-a.hunk": partA, "2-b.hunk": missB}))
		if !strings.Contains(errOut, "hunk 2  b.go  (patch line 7)") {
			t.Errorf("want the second part's hunk at line 7 of the stream:\n%s", errOut)
		}
	})

	// The first limit, and the reason for it. Without its final newline the
	// first part's last line runs into the second part's "@@ file", which
	// becomes payload, so the second part's hunk is matched against a.go. Here
	// that fails; with text a.go happened to contain, it would edit a.go.
	t.Run("a part with no final newline welds onto the next", func(t *testing.T) {
		root := cliTree(t, files)
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, nil,
			stream(t, map[string]string{"1-a.hunk": strings.TrimSuffix(partA, "\n"), "2-b.hunk": partB}))
		if code != exitNoMatch || !strings.Contains(errOut, "hunk 2  a.go") {
			t.Fatalf("exit %d, want 2 with the second part's hunk against a.go:\n%s", code, errOut)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("an @@ end in a part ends the stream", func(t *testing.T) {
		root := cliTree(t, files)
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, nil,
			stream(t, map[string]string{"1-a.hunk": partA + "@@ end\n", "2-b.hunk": partB}))
		if code != exitUsage || !strings.Contains(errOut, `content after "@@ end"`) {
			t.Fatalf("exit %d, want 1:\n%s", code, errOut)
		}
		assertUnchanged(t, root, before)
	})
}

// The report is a page an agent reads, so the root has to be on the page and
// not only in a struct. The shape is the one the field hit: a shell that had
// moved into a subdirectory, so a path that is right for the repository is
// wrong for the process, and every layer under the report was correct.
func TestARefusalOnThePrintedPageNamesTheRoot(t *testing.T) {
	root := cliTree(t, map[string]string{"sched/sched.zig": "a\nb\nc\n"})
	patch := "@@ file runtime/sched/sched.zig\n@@ old\nb\n@@ new\nB\n"

	code, _, errOut := runCLI(t, root, nil, patch)
	if code != exitNoMatch {
		t.Fatalf("exit %d, want %d: %s", code, exitNoMatch, errOut)
	}
	for _, want := range []string{"no such file", root} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, errOut)
		}
	}

	// §5.2: --json carries everything the text does.
	code, out, _ := runCLI(t, root, []string{"--json"}, patch)
	if code != exitNoMatch {
		t.Fatalf("exit %d with --json", code)
	}
	var v struct {
		Failures []struct{ Refusal string } `json:"failures"`
	}
	must(t, json.Unmarshal([]byte(out), &v))
	if len(v.Failures) != 1 || !strings.Contains(v.Failures[0].Refusal, root) {
		t.Errorf("json refusals = %+v, want the root %q", v.Failures, root)
	}
}

// --version is what an agent runs to find out whether the fix it has just read
// about is in the binary it is holding, so the three ways this binary reaches
// somebody are three cases rather than one.
func TestVersionString(t *testing.T) {
	info := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: v}}
	}
	for _, c := range []struct {
		name    string
		stamped string
		bi      *debug.BuildInfo
		want    string
	}{
		{
			name:    "the release workflow's stamp wins over the build info",
			stamped: "v0.2.0",
			bi:      info("v0.1.1-0.20260907131653-3e525bcc7aaa+dirty"),
			want:    "v0.2.0",
		},
		{
			name: "the module version go install recorded",
			bi:   info("v0.1.0"),
			want: "v0.1.0",
		},
		{
			// What `make build` actually produces, copied from a real run
			// rather than imagined: the toolchain synthesises this from the
			// last tag, the commit time, the revision and the dirty state.
			name: "the pseudo-version a local build reports",
			bi:   info("v0.1.1-0.20260907131653-3e525bcc7aaa+dirty"),
			want: "v0.1.1-0.20260907131653-3e525bcc7aaa+dirty",
		},
		{name: "go build -buildvcs=false, or a copy outside git", bi: info("(devel)"), want: "(devel)"},
		{name: "a build info with no version at all", bi: info(""), want: "(devel)"},
		{name: "no build information at all", bi: nil, want: "(unknown)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			was := version
			version = c.stamped
			t.Cleanup(func() { version = was })
			if got := versionString(c.bi); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// One line on stdout, nothing on stderr, and stdin untouched: failingReader
// errors on any read, so a --version that fell through to the patch would fail
// here rather than block a terminal.
func TestTheVersionFlagPrintsAndStops(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli([]string{"--version"}, failingReader{}, &out, &errOut); code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing", errOut.String())
	}
	got := out.String()
	if !strings.HasPrefix(got, "hunk ") || strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Errorf("stdout = %q, want one line naming the tool and its version", got)
	}
}

// §4: --json "asked for it on every path". Until 2026-09-22 no error found
// before the transaction printed an object: a caller parsing stdout got nothing
// and an exit code. Each of those paths is here, with and without --json, and
// without it the text is what it always was, byte for byte.
func TestJSONOnEveryPath(t *testing.T) {
	root := cliTree(t, map[string]string{"a.txt": "one\n"})
	missing := filepath.Join(root, "nope.txt")
	for _, c := range []struct {
		name  string
		args  []string
		stdin string
		exit  int
		text  string // stderr without --json
	}{
		{
			"an unknown flag",
			[]string{"--bogus"},
			vPatch, exitUsage,
			"hunk: flag provided but not defined: -bogus\nRun \"hunk --help\" for the flags.\n",
		},
		{"a bad flag value", []string{"--eol", "lf"}, vPatch, exitUsage, "hunk: --eol must be auto or strict, not \"lf\"\n"},
		{
			"an unexpected argument",
			[]string{"x"},
			vPatch, exitUsage,
			"hunk: unexpected argument \"x\"; the patch comes from stdin or -f\n",
		},
		{
			"format after flags",
			[]string{"format"},
			vPatch, exitUsage,
			"hunk: format is a subcommand and takes no flags; run \"hunk format\"\n",
		},
		{"inert flags", []string{"--keep-on-fail"}, vPatch, exitUsage, "hunk: --keep-on-fail does nothing without --verify\n"},
		// The OS words a missing file its own way, so only the prefix is ours.
		{"a missing -f", []string{"-f", missing}, "", exitUsage, "hunk: open " + missing + ": "},
		{"a parse error", nil, "@@ bogus\n", exitUsage, "hunk: patch line 1: "},
		{"an unopenable root", []string{"--root", missing}, vPatch, exitIO, "hunk: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			// runCLI puts its own --root first; a later --root wins.
			code, out, errOut := runCLI(t, root, c.args, c.stdin)
			if code != c.exit || out != "" || !strings.HasPrefix(errOut, c.text) {
				t.Errorf("without --json: exit %d, stdout %q, stderr %q; want %d and %q", code, out, errOut, c.exit, c.text)
			}
			if strings.HasSuffix(c.text, "\n") && errOut != c.text {
				t.Errorf("without --json the text changed:\n got %q\nwant %q", errOut, c.text)
			}

			code, js, errOut := runCLI(t, root, append([]string{"--json"}, c.args...), c.stdin)
			var v struct {
				OK    bool
				Exit  int
				Error string
			}
			if err := json.Unmarshal([]byte(js), &v); err != nil {
				t.Fatalf("with --json, no object on stdout (%v): %q, stderr %q", err, js, errOut)
			}
			if code != c.exit || v.Exit != c.exit || v.OK || v.Error == "" || errOut != "" {
				t.Errorf("with --json: exit %d, %s, stderr %q", code, js, errOut)
			}
		})
	}
}

// Exit 5 leaves the tree as it was unless the message says otherwise, and one
// exit 5 comes after the batch is written: a --json report stdout will not take.
func TestAJSONReportThatCannotBeWritten(t *testing.T) {
	for _, c := range []struct {
		name, patch, says string
	}{
		{"after the batch was applied", vPatch, "hunk: the batch is applied, but the --json report could not be written: "},
		{"after a refusal", "@@ file a.txt\n@@ old\nabsent\n@@ new\nx\n", "hunk: the --json report could not be written: "},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			var errOut bytes.Buffer
			code := cli([]string{"--root", root, "--json"}, strings.NewReader(c.patch), failingWriter{}, &errOut)
			if code != exitIO || !strings.HasPrefix(errOut.String(), c.says) {
				t.Errorf("exit %d, stderr %q", code, errOut.String())
			}
		})
	}
}

// An error found before the transaction, with --json asked for and a stdout
// that will not take it: the error itself still reaches stderr.
func TestAnEarlyErrorWhoseJSONCannotBeWritten(t *testing.T) {
	var errOut bytes.Buffer
	code := cli([]string{"--json", "--eol", "lf"}, strings.NewReader(""), failingWriter{}, &errOut)
	want := "hunk: --eol must be auto or strict, not \"lf\"\nhunk: the --json report could not be written: disk gone\n"
	if code != exitUsage || errOut.String() != want {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}

func TestJSONRequested(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"--json"}, true},
		{[]string{"-json"}, true},
		{[]string{"--bogus", "--json"}, true},
		{[]string{"--json=true"}, true},
		{[]string{"--json=false"}, false},
		{[]string{"--json=maybe"}, false},
		{[]string{"--jsonx"}, false},
		{[]string{"json"}, false},
		{[]string{"--", "--json"}, false},
		{nil, false},
	} {
		if got := jsonRequested(c.args); got != c.want {
			t.Errorf("jsonRequested(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}
