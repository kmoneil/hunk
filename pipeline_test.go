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

	// An inverted grep exits 0 whenever one line does not match (#107).
	{"#107's command", `zig build check 2>&1 | grep -v "^pass "`, "grep -v"},
	{"#107's repro", `sh -c 'echo "error: the build broke"; exit 1' 2>&1 | grep -v '^pass '`, "grep -v"},
	{"--invert-match", "a | grep --invert-match x", "grep -v"},
	{"-v bundled first", "a | grep -vE 'x|y'", "grep -v"},
	{"-v bundled last", "a | grep -Ev 'x|y'", "grep -v"},
	{"-v after the pattern, which grep permutes", "a | grep x -v", "grep -v"},
	{"-v with -e taking each pattern", "a | grep -v -e x -e y", "grep -v"},
	{"-ve, whose e takes the next word", "a | grep -ve x", "grep -v"},
	{"after an option with its value attached", "a | grep -m5 -v x", "grep -v"},
	{"after a long option whose value is the next word", "a | grep --max-count 5 -v x", "grep -v"},
	{"after a long option whose value follows =", "a | grep --label=out -v x", "grep -v"},
	{"after --color, which takes nothing from the next word", "a | grep --color -v x", "grep -v"},
	{"after --color=never", "a | grep --color=never -v x", "grep -v"},
	{"after GNU's -NUM", "a | grep -2 -v x", "grep -v"},
	{"with -s, which quiets only errors", "a | grep -sv x", "grep -v"},
	{"with -o, which still exits on what it selected", "a | grep -vo x", "grep -v"},
	{"reading a file, whose status is still the pipeline's", "a | grep -v x log", "grep -v"},
	{"behind a redirection", "a | grep -v x 2>/dev/null", "grep -v"},
	{"a quoted -v", "a | grep '-v' x", "grep -v"},
	{"an escaped -v", `a | grep \-v x`, "grep -v"},
	{"a dollar in single quotes is a character", "a | grep -v '^$'", "grep -v"},
	{"an escaped dollar in double quotes is a character", `a | grep -v "^\$"`, "grep -v"},
	{"a quoted glob is a character", "a | grep -v 'x*'", "grep -v"},
	{"egrep", "a | egrep -v 'x|y'", "egrep -v"},
	{"fgrep", "a | fgrep -v x", "fgrep -v"},
	{"an absolute path to grep", "a | /usr/bin/grep -v x", "grep -v"},
	{"two in a row", "a | grep -v x | grep -v y", "grep -v"},
	{"after --, -q is the pattern", "a | grep -v -- -q", "grep -v"},

	// A grep that is a test, is not inverted, or is not read for sure.
	{"grep with no -v", "a | grep x", ""},
	{"-vq", "a | grep -vq x", ""},
	{"-v and --quiet", "a | grep -v --quiet x", ""},
	{"-v and --silent", "a | grep -v --silent x", ""},
	{"-vc", "a | grep -vc x", ""},
	{"--count", "a | grep --count -v x", ""},
	{"-vl", "a | grep -vl x", ""},
	{"-v and -L", "a | grep -v -L x", ""},
	{"--files-with-matches", "a | grep --files-with-matches -v x", ""},
	{"--files-without-match", "a | grep --files-without-match -v x", ""},
	{"a quoted -q", "a | grep -v '-q' x", ""},
	{"-ev, whose e takes the v", "a | grep -ev x", ""},
	{"-mv, whose m takes the v", "a | grep -mv x", ""},
	{"-e -v, whose pattern is -v", "a | grep -e -v", ""},
	{"--regexp -v, whose pattern is -v", "a | grep --regexp -v", ""},
	{"after --, -v is the pattern", "a | grep -- -v", ""},
	{"a quoted -e taking the -v", `a | grep "-e" -v`, ""},
	{"an option neither grep has", "a | grep -K -v x", ""},
	{"a long option neither grep has", "a | grep --frobnicate -v x", ""},
	{"an abbreviated long option, which GNU takes", "a | grep --inv x", ""},
	{"a flag given a value", "a | grep --invert-match=yes x", ""},
	{"an option missing its argument", "a | grep -v -e", ""},
	{"a long option missing its argument", "a | grep -v --regexp", ""},
	{"a variable, which could hold -q", `a | grep -v "$PAT"`, ""},
	{"an unquoted variable", "a | grep -v $PAT", ""},
	{"a glob, which could name a file -q", "a | grep -v x*", ""},
	{"a bracket", "a | grep -v [0-9]", ""},
	{"a brace, which bash expands", "a | grep -{v,q} x", ""},
	{"rg, whose options are not grep's", "a | rg -v x", ""},
	{"grep behind command", "a | command grep -v x", ""},
	{"a relative path to a grep", "a | ./grep -v x", ""},
	{"a quoted grep", "a | 'grep' -v x", ""},
	{"an inverted grep that is not last", "a | grep -v x | grep -q y", ""},
	{"an inverted grep before &&", "a | grep -v x && b", ""},
	{"an inverted grep under pipefail", "set -o pipefail; a | grep -v x", ""},
	{"an inverted grep negated", "! a | grep -v x", ""},
	{"an inverted grep with nothing piped in", "grep -v x log", ""},
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
			stages, ok := lastPipeline(c.command)
			var got []string
			for _, s := range stages {
				got = append(got, s.cmd)
			}
			if !ok || !slices.Equal(got, c.want) {
				t.Errorf("lastPipeline(%q) = %q, %v; want %q", c.command, got, ok, c.want)
			}
		})
	}
}

// The words after a stage's command word are what an inverted grep is read
// from (#107): as written, quotes and all, and without the redirections or
// their files, or the assignments in front of the command word.
func TestLastPipelineKeepsEachStagesWords(t *testing.T) {
	for _, c := range []struct {
		name, command string
		want          [][]string // each stage's words after its command word
	}{
		{"none", "a | b", [][]string{nil, nil}},
		{"as written", `a x | grep -v '^pass ' "y z"`, [][]string{{"x"}, {"-v", "'^pass '", `"y z"`}}},
		{"redirections and their files are not words", "a 2>&1 | grep -v x 2>/dev/null >out y", [][]string{nil, {"-v", "x", "y"}}},
		{"nor are assignments in front", "a | X=1 grep -v x", [][]string{nil, {"-v", "x"}}},
		{"an assignment after the command word is one", "a | env X=1 grep", [][]string{nil, {"X=1", "grep"}}},
		{"only the last list", "a -q | b; c | grep -v x", [][]string{nil, {"-v", "x"}}},
		{"a stage with no command word has none", "< f | grep -v x", [][]string{nil, {"-v", "x"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			stages, ok := lastPipeline(c.command)
			if !ok || len(stages) != len(c.want) {
				t.Fatalf("lastPipeline(%q) = %+v, %v; want %d stages", c.command, stages, ok, len(c.want))
			}
			for i, s := range stages {
				if !slices.Equal(s.args, c.want[i]) {
					t.Errorf("stage %d (%q) has words %q, want %q", i, s.cmd, s.args, c.want[i])
				}
			}
		})
	}
}

// unquoted is a word as sh hands it over, or false where sh would rewrite it.
func TestUnquoted(t *testing.T) {
	for _, c := range []struct {
		raw, want string
		ok        bool
	}{
		{"abc", "abc", true},
		{"", "", true},
		{"'a b'", "a b", true},
		{`"a b"`, "a b", true},
		{`a'b'"c"d`, "abcd", true},
		{`"a\"b"`, `a"b`, true},
		{`"a\b"`, `a\b`, true},
		{`"a\$b"`, "a$b", true},
		{`"a\\b"`, `a\b`, true},
		{"\"a\\\nb\"", "ab", true},
		{`a\ b`, "a b", true},
		{`\-v`, "-v", true},
		{"a\\\nb", "ab", true},
		{`a\`, "a", true},
		{"'$x'", "$x", true},
		{"'*'", "*", true},
		{`"*"`, "*", true},
		{`"$x"`, "", false},
		{"$x", "", false},
		{"x*", "", false},
		{"x?", "", false},
		{"[x]", "", false},
		{"{a,b}", "", false},
		{"'unclosed", "", false},
		{`"unclosed`, "", false},
		{`"ends on a backslash\`, "", false},
	} {
		got, ok := unquoted(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("unquoted(%q) = %q, %v; want %q, %v", c.raw, got, ok, c.want, c.ok)
		}
	}
}

func FuzzUnquoted(f *testing.F) {
	for _, s := range []string{"abc", `'a b'`, `"a\"b"`, `a\ b`, `"$x"`, "x*", `"x\`, "'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, ok := unquoted(raw)
		if !ok && got != "" {
			t.Fatalf("unquoted(%q) = %q, false", raw, got)
		}
		// A word with nothing for sh to remove or rewrite is handed over as
		// it is.
		if !strings.ContainsAny(raw, `'"\$*?[{`) && (!ok || got != raw) {
			t.Fatalf("unquoted(%q) = %q, %v; want it unchanged", raw, got, ok)
		}
		if ok && len(got) > len(raw) {
			t.Fatalf("unquoted(%q) = %q, which is longer", raw, got)
		}
	})
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

	// Named as grep -v however the flag was spelled, and with the reason its
	// status is no test, since a grep's usually is one.
	for _, g := range greps {
		t.Run("naming "+g+" -v", func(t *testing.T) {
			root := cliTree(t, map[string]string{"a.txt": "one\n"})
			before := snapshot(t, root)
			code, _, errOut := runCLI(t, root, []string{"--verify", "false | " + g + " -Ev x"}, vPatch)
			if code != exitUsage || !strings.Contains(errOut, "piped into "+g+" -v fail") ||
				!strings.Contains(errOut, "and that is "+g+" -v, which exits 0 whenever one line it reads does not match;") {
				t.Errorf("exit %d, stderr %q", code, errOut)
			}
			assertUnchanged(t, root, before)
		})
	}

	// #107 as filed. The command leaves a file if it runs at all.
	t.Run("#107's repro: exit 1, nothing written and nothing run", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root,
			[]string{"--verify", `touch ran.txt; sh -c 'echo "error: the build broke"; exit 1' 2>&1 | grep -v '^pass '`}, vPatch)
		if code != exitUsage || out != "" {
			t.Errorf("exit %d, stdout %q; want 1 and the refusal on stderr", code, out)
		}
		golden(t, "cli-verify-piped-into-grep-v", errOut)
		assertUnchanged(t, root, before)
	})

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

	// -q makes an inverted grep a test (#107): it runs, and its status decides
	// both ways, on the grep the platform has.
	t.Run("an inverted grep with -q runs and passes on its status", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, out, errOut := runCLI(t, root, []string{"--verify", "cat a.txt | grep -vq one"}, vPatch)
		if code != exitOK || !strings.Contains(out, "verify ok") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})

	t.Run("and fails on it", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, _, errOut := runCLI(t, root, []string{"--verify", "cat a.txt | grep -vq ONE"}, vPatch)
		if code != exitVerifyFailed {
			t.Errorf("exit %d, stderr %q; want 3", code, errOut)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("a single --try may end in an inverted grep", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--try", "printf 'pass 1\\nerror\\n' | grep -v '^pass '"}, vPatch)
		if code != exitOK || !strings.Contains(out, "\nerror\n") || strings.Contains(out, "\npass 1\n") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
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
	if want := "or in a grep -v without -q, -c, -l or -L"; !strings.Contains(flat(help.String()), want) {
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
		if want := listOf(statusFilters, "`") + ", or in a `grep -v`"; !strings.Contains(doc, want) {
			t.Errorf("%s does not name the inverted grep as %q", p, want)
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
	f.Add("a | grep " + strings.Repeat("-e x ", 1<<13) + "-v")
	f.Add("a | grep " + strings.Repeat(`'x'"y"\z `, 1<<13) + "-v")

	f.Fuzz(func(t *testing.T, s string) {
		// Total: every command is a filter or not, never a panic.
		filter, ok := maskingFilter(s)
		if ok != (filter != "") {
			t.Fatalf("maskingFilter(%q) = %q, %v", s, filter, ok)
		}
		if ok {
			// A refusal names one of the four, or a grep as grep -v, and
			// nothing of the caller's.
			name, grep := strings.CutSuffix(filter, " -v")
			if !slices.Contains(statusFilters, filter) && !(grep && slices.Contains(greps, name)) {
				t.Fatalf("%q was refused for %q, which is not a filter", s, filter)
			}
			if !strings.Contains(s, "|") || !strings.Contains(s, name) {
				t.Fatalf("%q was refused for a pipe into %s that it does not hold", s, filter)
			}
			if grep && !strings.Contains(s, "v") {
				t.Fatalf("%q was refused for an inverted grep with no v in it", s)
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
