package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI drives the real entry point in process, with a real tree.
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
			name:      "1, an unimplemented op",
			files:     map[string]string{"a.go": "one\n"},
			stdin:     "@@ delete a.go\n",
			want:      exitUsage,
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

// Accepting a flag and not doing what it says is the failure this tool exists
// to refuse. The verify family belongs to a phase that is not built.
func TestVerifyFamilyIsRefusedRatherThanIgnored(t *testing.T) {
	patch := "@@ file a.go\n@@ old\none\n@@ new\nONE\n"
	for _, args := range [][]string{
		{"--verify", "true"},
		{"--verify-may-format"},
		{"--verify-lines", "10"},
		{"--keep-on-fail"},
	} {
		t.Run(args[0], func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.go": "one\n"})
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, args, patch)
			if code != exitUsage {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.Contains(errOut, "not implemented") || !strings.Contains(errOut, args[0]) {
				t.Errorf("err = %q", errOut)
			}
			assertUnchanged(t, root, before)
		})
	}
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
		{"a -f that does not exist", "no such file", []string{"-f", "/nonexistent/p.txt"}, ""},
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
	// §6.3: the flag and the reason together, not just the behaviour.
	if !strings.Contains(got, "--verify-may-format") ||
		!strings.Contains(got, "silently reverting another writer's work") {
		t.Error("--help gives --verify-may-format without its reason (§6.3)")
	}
	// §11: --help quotes the figure for why --verify is optional.
	if !strings.Contains(got, "66%") {
		t.Error("--help does not quote §11's figure for why --verify is optional")
	}
	// Every flag in §4's table appears.
	for _, f := range []string{
		"--verify", "--verify-may-format", "--verify-lines", "--keep-on-fail",
		"--dry-run", "--root", "--marker", "-f", "--json", "--quiet",
		"--context", "--eol", "--allow-outside-root",
	} {
		if !strings.Contains(got, f) {
			t.Errorf("--help does not mention %s", f)
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
