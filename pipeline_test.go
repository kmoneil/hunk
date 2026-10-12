package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// maskingRows is every shape the card decided (issue #80), refused and not.
// filter is the one the refusal names, and "" is a command hunk runs as it
// always has. A wrong refusal and a missing one are both defects, so the
// commands that are left alone are named as carefully as the ones that are
// not.
var maskingRows = []struct {
	name, command, filter string
}{
	// The filters, and the shapes the field wrote them in.
	{"the command the issue was filed from", "zig build test -Dmodule=pubsub 2>&1 | tail -25", "tail"},
	{"the issue's repro, whose last list element is the pipe", "echo building; false | tail -1", "tail"},
	{"head", "go build ./internal/wir 2>&1 | head -40", "head"},
	{"tee", "make check 2>&1 | tee /tmp/check.log", "tee"},
	{"cat", "go test ./... | cat", "cat"},
	{"after &&, the field's commonest longer shape", "go vet ./... && go test ./... 2>&1 | tail -5", "tail"},
	{"after ||", "make fast || make slow | tail -3", "tail"},
	{"after a newline", "go vet ./...\ngo test ./... | tail -3", "tail"},
	{"at the end of three stages", "a | grep -v x | tail", "tail"},

	// What may follow the pipeline without ending it.
	{"a trailing semicolon", "a | tail -5;", "tail"},
	{"trailing blank lines", "a | tail -5\n\n", "tail"},
	{"a trailing comment", "a | tail -5 # the last five", "tail"},
	{"a trailing backslash, which is a character", `a | tail -5 \`, "tail"},
	{"its output redirected", "a | tail -5 > /tmp/out", "tail"},
	{"its output redirected with no blank", "a | tail>out", "tail"},

	// Blanks, breaks and continuations.
	{"no blanks at all", "a|tail", "tail"},
	{"tabs for blanks", "a\t|\ttail\t-5", "tail"},
	{"a continued line", "go test ./... \\\n  | tail -5", "tail"},
	{"a line continued after the pipe", "go test ./... 2>&1 | \\\n  tail -5", "tail"},
	{"line breaks after the pipe", "a |\n\n tail", "tail"},
	{"a line break after &&", "a &&\nb | tail", "tail"},
	{"a comment line first", "# run it\na | tail", "tail"},
	{"a hash inside a word, which is not a comment", "a b#c | tail", "tail"},

	// The filter's own word.
	{"an absolute path", "a | /usr/bin/tail -5", "tail"},
	{"behind an assignment", "a | LC_ALL=C tail -5", "tail"},
	{"behind two assignments", "a | X=1 _y2=3 cat", "cat"},
	{"behind a redirection", "a | 2>/dev/null tail", "tail"},
	{"through |&", "a |& tail -5", "tail"},

	// A | that is not a pipe, in front of one that is.
	{"a | in single quotes earlier", "go test -run 'TestA|TestB' ./... | tail -3", "tail"},
	{"a | in double quotes earlier", `go test -run "A|B" . 2>&1 | tail -3`, "tail"},
	{"an escaped | earlier", `grep a\|b f | tail`, "tail"},
	{"an escaped quote inside double quotes", `echo "a \" | b" | tail`, "tail"},
	{"a dollar that ends a quoted pattern", `grep "ok$" log | cat`, "cat"},
	{"a variable, which is only a word", "$GO test ./... | tail", "tail"},

	// Redirections that hold an operator's character.
	{"2>&1 against the pipe", "a 2>&1|tail", "tail"},
	{">|, which is not a pipe", "a >| log | tail", "tail"},
	{"&>, which is not a background job", "a &> log | tail", "tail"},
	{"&>>", "a &>> log | tail", "tail"},
	{">>", "a >> log | tail", "tail"},
	{"<>", "a <> f | tail", "tail"},
	{"<&", "a <&3 | tail", "tail"},
	{"a first stage that is only a redirection", "< log | tail", "tail"},
	{"digits that are arguments, not a descriptor", "seq 1 5 | tail -2", "tail"},

	// A compound command piped as a whole is still piped.
	{"a brace group", "{ a; b; } | tail -5", "tail"},
	{"a loop", "for f in a b; do echo $f; done | head", "head"},

	// No pipeline, or one whose last stage decides.
	{"no pipe", "go test ./...", ""},
	{"a filter that nothing is piped into", "cat a.txt", ""},
	{"one alone after a semicolon", "make check > log 2>&1; cat log", ""},
	{"one alone after &&", "make check > log 2>&1 && tail -1 log", ""},
	{"one alone after ||", "make fast || cat log", ""},
	{"one alone on the last line", "a | b\ntail -1 log", ""},
	{"no command", "", ""},
	{"only blanks and a comment", "  # nothing\n", ""},
	{"grep decides", "go vet ./... 2>&1 | grep -q 'no issues'", ""},
	{"a filter that is not last", `uv run ty check src 2>&1 | tail -1 | grep -q "All checks passed"`, ""},

	// The pipe is not in the last list element, so something else decides.
	{"a verify made unable to fail on purpose", "go test ./... 2>&1 | tail -20; true", ""},
	{"a filter before &&", "a | tail && b", ""},
	{"a filter before ||", "a | tail || b", ""},
	{"a filter on the line before", "a | tail\nb", ""},
	{"the field's gofmt guard", "gofmt -l . | grep . && exit 1; go vet ./...", ""},
	{"a loop that pipes inside its body", "for f in a b; do go test ./$f | tail -3; done", ""},

	// A | that is not a pipe.
	{"in single quotes", "echo 'a | tail'", ""},
	{"in double quotes", `echo "a | tail"`, ""},
	{"escaped", `echo a \| tail`, ""},
	{"in a comment", "a # | tail", ""},

	// A last stage that is not one of the four, as written.
	{"a longer name", "a | tailor", ""},
	{"a double-quoted filter", `a | "tail"`, ""},
	{"a single-quoted filter", "a | 'tail'", ""},
	{"an escaped filter", `a | \tail`, ""},
	{"behind command", "a | command tail", ""},
	{"a variable", "a | $TAIL", ""},
	{"a relative path, which is somebody's own program", "a | ./tail", ""},
	{"a relative path with a directory", "a | bin/tail", ""},
	{"an absolute path to something else", "a | /usr/bin/tailor", ""},
	{"a stage that is only an assignment", "a | X=1", ""},
	{"a stage that is only a redirection", "a | > f", ""},
	{"a word with = that is not an assignment: a digit first", "a | 1X=2 tail", ""},
	{"a word with = that is not an assignment: nothing first", "a | =x tail", ""},
	{"a word with = that is not an assignment: a hyphen", "a | a-b=c tail", ""},
	{"an assignment after the command word is an argument", "a | env X=1 tail", ""},

	// Outside what the scanner models: run as before.
	{"a command substitution", "cd $(git rev-parse --show-toplevel) && go test ./... | tail", ""},
	{"backquotes", "cd `pwd` && a | tail", ""},
	{"a braced variable", "${GO:-go} test ./... | tail", ""},
	{"a substitution in double quotes", `echo "$(date)"; a | tail`, ""},
	{"backquotes in double quotes", "echo \"`date`\"; a | tail", ""},
	{"a braced variable in double quotes", `echo "${HOME}"; a | tail`, ""},
	{"an ANSI-C quote", `printf $'a\n'; a | tail`, ""},
	{"a subshell", "(a | tail)", ""},
	{"a parenthesised filter", "a | (tail)", ""},
	{"a function", "f() { a; }; f | tail", ""},
	{"a closing parenthesis alone", "a ) | tail", ""},
	{"a here-document", "cat <<EOF | tail\nx\nEOF", ""},
	// Its text is not shell, and reading it as shell finds pipes in prose and
	// quotes that open on an apostrophe.
	{"a here-document that runs to the end of the command", "cat <<EOF\nresults | tail", ""},
	{"a here-document whose text holds a quote", "cat <<EOF\nit's here\nEOF\necho 'a | tail' \\'", ""},
	{"a here-string", "a <<< x | tail", ""},
	{"a background job", "a | tail &", ""},
	{"a background job earlier", "server & a | tail", ""},
	{"a case terminator", "a ;; b | tail", ""},
	{"a leading semicolon", "; a | tail", ""},
	{"a single quote that does not close", "a 'b | tail", ""},
	{"a double quote that does not close", `a "b | tail`, ""},
	{"a single quote that does not close, after the filter", "a | tail 'x", ""},
	{"a double quote that ends on a backslash", `a | tail "x\`, ""},

	// What sh would refuse.
	{"an empty first stage", "| tail", ""},
	{"an empty middle stage", "a | | tail", ""},
	{"an empty stage before &&", "a | && b | tail", ""},
	{"an empty stage before ||", "a | || b | tail", ""},
	{"a command that ends on a pipe", "a | tail |", ""},
	{"a command that ends on a pipe after a filter earlier", "a | tail; b |", ""},
	{"a command that ends on &&", "a | tail &&", ""},
	{"a command that ends on ||", "a | tail ||", ""},
	{"a command that ends on a line break after an operator", "a | tail &&\n", ""},
	{"a redirection with no file before a pipe", "a > | tail", ""},
	{"a redirection with no file at the end", "a | tail >", ""},
	{"a redirection with no file before a newline", "a | tail >\n", ""},
	{"two redirections running", "a > > f | tail", ""},
	{"a redirection and then &>", "a > &> f | tail", ""},

	// A status that is not the filter's, or a | that is not a pipe.
	{"negated, so it always fails", "! a | tail", ""},
	{"negated after &&", "b && ! a | tail", ""},
	{"a [[ test", "[[ $x =~ a|tail ]]", ""},
	{"a [[ test in a later stage", "a | [[ $x =~ b|tail ]]", ""},

	// The command has said how it wants its pipe read.
	{"set -o pipefail", "set -o pipefail; a | tail", ""},
	{"zsh's spelling of it", "setopt PIPE_FAIL; a | tail", ""},
	{"PIPESTATUS, wherever it is mentioned", "a | tail # PIPESTATUS is not read here", ""},
	{"pipestatus in lower case", "a | tail # see $pipestatus", ""},
}

func TestMaskingFilter(t *testing.T) {
	for _, c := range maskingRows {
		t.Run(c.name, func(t *testing.T) {
			got, ok := maskingFilter(c.command)
			if got != c.filter || ok != (c.filter != "") {
				t.Errorf("maskingFilter(%q) = %q, %v; want %q", c.command, got, ok, c.filter)
			}
			// What comes before a semicolon is another list, and cannot change
			// which pipeline is last.
			if again, _ := maskingFilter("true; " + c.command); again != c.filter {
				t.Errorf("after \"true; \" the answer is %q, not %q", again, c.filter)
			}
		})
	}
}

// lastPipeline is what maskingFilter stands on, and the rows above cannot tell
// a stage read wrongly from one that merely was not a filter.
func TestLastPipeline(t *testing.T) {
	for _, c := range []struct {
		name, command string
		want          []string
	}{
		{"one command", "go test ./...", []string{"go"}},
		{"three stages", "a x | b y | c z", []string{"a", "b", "c"}},
		{"only the last list", "a | b; c | d && e | f", []string{"e", "f"}},
		{"the command word is taken as written", `a | "tail" -5 | /bin/cat`, []string{"a", `"tail"`, "/bin/cat"}},
		{"assignments and redirections are not it", "X=1 a 2>&1 | >f Y=2 b", []string{"a", "b"}},
		{"a stage with no command word", "< f | X=1 | c", []string{"", "", "c"}},
		{"nothing at all", " \n", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := lastPipeline(c.command)
			if !ok || !slices.Equal(got, c.want) {
				t.Errorf("lastPipeline(%q) = %q, %v; want %q", c.command, got, ok, c.want)
			}
		})
	}
}

// listOf writes the filters the way a sentence does: "tail, head, tee or cat",
// each inside quote.
func listOf(words []string, quote string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = quote + w + quote
	}
	last := len(quoted) - 1
	return strings.Join(quoted[:last], ", ") + " or " + quoted[last]
}

func TestAVerifyPipedIntoAFilterIsRefused(t *testing.T) {
	// The command leaves a file behind if it runs at all, so an unchanged tree
	// says both that nothing was written and that nothing ran.
	const piped = "touch ran.txt; false | tail -1"

	t.Run("exit 1, nothing written and nothing run", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--verify", piped}, vPatch)
		if code != exitUsage || out != "" {
			t.Errorf("exit %d, stdout %q; want 1 and the refusal on stderr", code, out)
		}
		golden(t, "cli-verify-piped-into-a-filter", errOut)
		assertUnchanged(t, root, before)
	})

	t.Run("--json carries the whole of it", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--json", "--verify", piped}, vPatch)
		if code != exitUsage || errOut != "" {
			t.Errorf("exit %d, stderr %q; want 1 and nothing on stderr", code, errOut)
		}
		golden(t, "cli-verify-piped-into-a-filter-json", out)
		var v struct {
			OK    bool
			Exit  int
			Error string
		}
		must(t, json.Unmarshal([]byte(out), &v))
		// The fix is in the error, because the error is all --json has.
		if v.OK || v.Exit != exitUsage || !strings.Contains(v.Error, "drop the pipe") {
			t.Errorf("%s", out)
		}
		assertUnchanged(t, root, before)
	})

	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		// A dry run that passed and a real run that was then refused would be
		// a round trip spent learning nothing.
		{"under --dry-run too", []string{"--dry-run", "--verify", piped}, "piped into tail"},
		{"under --keep-on-fail too", []string{"--keep-on-fail", "--verify", piped}, "piped into tail"},
		{"under --quiet, which is for successes", []string{"--quiet", "--verify", piped}, "piped into tail"},
		{"quoting the --verify-lines in force", []string{"--verify-lines", "7", "--verify", piped}, "--verify-lines (7)"},
		{"and the default when none is", []string{"--verify", piped}, "--verify-lines (40)"},
		// Before the patch is read: the answer is on the command line.
		{"before a -f that does not exist", []string{"-f", "/nonexistent/p.txt", "--verify", piped}, "piped into tail"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, c.args, vPatch)
			if code != exitUsage || out != "" || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d, stdout %q, stderr %q; want 1 and %q", code, out, errOut, c.want)
			}
			assertUnchanged(t, root, before)
		})
	}

	t.Run("before a patch that does not parse", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--verify", piped}, "@@ bogus\n")
		if code != exitUsage || !strings.Contains(errOut, "piped into tail") || strings.Contains(errOut, "patch line") {
			t.Errorf("exit %d, stderr %q", code, errOut)
		}
	})

	for _, f := range statusFilters {
		t.Run("naming "+f, func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, []string{"--verify", "false | " + f}, vPatch)
			if code != exitUsage ||
				!strings.Contains(errOut, "piped into "+f+" fail") || !strings.Contains(errOut, "and that is "+f+";") {
				t.Errorf("exit %d, stderr %q", code, errOut)
			}
			assertUnchanged(t, root, before)
		})
	}

	t.Run("after the refusals that were there first", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, _, errOut := runCLI(t, root, []string{"--try", "true", "--verify", piped}, vPatch)
		if code != exitUsage || !strings.Contains(errOut, "cannot both be set") || strings.Contains(errOut, "piped into") {
			t.Errorf("exit %d, stderr %q", code, errOut)
		}
	})
}

// The refusal is for --verify and for what hunk can read. Everything else runs
// as it did, and these are the rows that would notice it stop.
func TestWhatTheFilterRefusalLeavesAlone(t *testing.T) {
	t.Run("--try is not read: a filter on its output is how it is used", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", "printf 'first\\nsecond\\n' | tail -1"}, vPatch)
		if code != exitOK || !strings.HasPrefix(out, "tried 1 hunk") || !strings.Contains(out, "second") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("a pipeline whose last stage decides passes on it", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, out, errOut := runCLI(t, root, []string{"--verify", "cat a.txt | tail -1 | grep -q ONE"}, vPatch)
		if code != exitOK || !strings.Contains(out, "verify ok") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})

	t.Run("and fails on it", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, []string{"--verify", "cat a.txt | tail -1 | grep -q NOPE"}, vPatch)
		if code != exitVerifyFailed {
			t.Errorf("exit %d, stderr %q; want 3", code, errOut)
		}
		assertUnchanged(t, root, before)
	})

	// The refusal's own advice, followed. Where sh has pipefail the pipeline
	// fails with its first stage; where it does not, set refuses the option
	// and the command fails there. Either way the verify says no.
	t.Run("a command under set -o pipefail keeps its pipe and can fail", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, []string{"--verify", "set -o pipefail; false | tail -1"}, vPatch)
		if code != exitVerifyFailed {
			t.Errorf("exit %d, stderr %q; want 3", code, errOut)
		}
		assertUnchanged(t, root, before)
	})

	// Not a behaviour to be proud of, and pinned for that reason: hunk does
	// not read a subshell, so this verify still cannot fail. When hunk is not
	// sure, it runs the command, and a row that turned this into a refusal
	// would have to say what else the scanner now claims to read.
	t.Run("a command outside what is modelled runs as before", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, out, errOut := runCLI(t, root, []string{"--verify", "(false | tail -1)"}, vPatch)
		if code != exitOK || !strings.Contains(out, "verify ok") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
}

// The filters are a list in code and a sentence in three documents, and a
// filter added to one is a refusal the others do not warn of. --help and
// README.md are what a reader of the binary and of the repository has; the
// skill is what an agent has read before it writes the command.
func TestEveryDocumentNamesTheFiltersAVerifyIsRefusedFor(t *testing.T) {
	var help, errOut bytes.Buffer
	if code := cli([]string{"--help"}, strings.NewReader(""), &help, &errOut); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if want := "ends in " + listOf(statusFilters, ""); !strings.Contains(flat(help.String()), want) {
		t.Errorf("--help does not say %q", want)
	}
	// The reflex is to shorten the output, so each document has to say what
	// already does. The skill says it without the flag: skillOmits leaves
	// --verify-lines out, and what an agent needs is that there is nothing to
	// shorten.
	for p, already := range map[string]string{
		"README.md": "`--verify-lines N`",
		filepath.Join(".agents", "skills", "hunk", "SKILL.md"): "only its last 40 lines",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		doc := flat(string(b))
		if want := listOf(statusFilters, "`"); !strings.Contains(doc, want) {
			t.Errorf("%s does not name the filters as %q", p, want)
		}
		if !strings.Contains(doc, already) {
			t.Errorf("%s does not say %q, which is what makes the pipe unnecessary", p, already)
		}
	}
}

// The scanner reads a string and answers. It starts no shell and opens no
// file, which is what lets the refusal come before anything else happens, and
// that is asserted on the import list rather than trusted.
func TestThePipelineScannerRunsNothing(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "pipeline.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse pipeline.go: %v", err)
	}
	allowed := map[string]bool{"slices": true, "strings": true}
	for _, im := range f.Imports {
		path, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			t.Fatalf("import path %s: %v", im.Path.Value, err)
		}
		if !allowed[path] {
			t.Errorf("pipeline.go imports %q; it reads a command and runs nothing", path)
		}
	}
}

func FuzzMaskingFilter(f *testing.F) {
	for _, c := range maskingRows {
		f.Add(c.command)
	}
	// Wide ones, so that a scan that went back over what it had read would
	// show on every run of the suite rather than only under the fuzzer.
	f.Add(strings.Repeat("a|", 1<<15) + "tail")
	f.Add(strings.Repeat("'", 1<<16) + " | tail")
	f.Add(strings.Repeat(`"\"" `, 1<<14) + "| tail")
	f.Add(strings.Repeat("a>b ", 1<<14) + "| tail")
	f.Add(strings.Repeat("#\n", 1<<15) + "a | tail")
	f.Add("a | tail " + strings.Repeat("\"", 1<<16))

	f.Fuzz(func(t *testing.T, s string) {
		// Total: every command is a filter or not, never a panic.
		filter, ok := maskingFilter(s)
		if ok != (filter != "") {
			t.Fatalf("maskingFilter(%q) = %q, %v", s, filter, ok)
		}
		if ok {
			// A refusal names one of the four and nothing of the caller's.
			if !slices.Contains(statusFilters, filter) {
				t.Fatalf("%q was refused for %q, which is not a filter", s, filter)
			}
			if !strings.Contains(s, "|") || !strings.Contains(s, filter) {
				t.Fatalf("%q was refused for a pipe into %s that it does not hold", s, filter)
			}
			lower := strings.ToLower(s)
			for _, w := range keepsItsPipe {
				if strings.Contains(lower, w) {
					t.Fatalf("%q says %s and was refused", s, w)
				}
			}
		}
		// What comes before a semicolon is another list.
		if again, _ := maskingFilter("true; " + s); again != filter {
			t.Fatalf("maskingFilter(%q) = %q, and %q after \"true; \"", s, filter, again)
		}
	})
}
