package main

// The classifier. This file is the artifact of the measurement, not the
// numbers: §1's table and §12's comparison are only meaningful if the rule that
// produced them is written down and can be argued with.
//
// Every boundary case here changes a published figure, so each one is a named
// function with a table test rather than a regexp buried in a walker.

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// A python heredoc: "python3 - <<'PY'", "python <<EOF", with or without the
	// dash, and tolerating a redirect before it.
	rePythonHeredoc = regexp.MustCompile(`(?m)\bpython[23]?\b[^\n|;&]*<<-?\s*['"]?\w`)

	// The same, capturing the terminator, so "after the heredoc" means after
	// the payload rather than after its first line.
	reHeredocDelim = regexp.MustCompile(`(?m)\bpython[23]?\b[^\n|;&]*<<-?\s*['"]?(\w+)`)

	// A write, not merely a read. The corpus shape §1 counts is an edit, and a
	// heredoc that only inspects a file is a different thing.
	// The mode has to be open's second argument. Matching a quoted [wa]
	// anywhere inside the parentheses reads open('a') as an append, because the
	// filename starts with an a.
	reWrites = regexp.MustCompile(`write_text\s*\(|\.write\s*\(|open\s*\([^,)]*,\s*['"][wa]`)

	// The guard §1 measures the absence of: a check that the text to be replaced
	// is actually there, before replacing it. Both the positive and negative
	// spellings count, and so does a bare assert: the first draft required
	// "assert ... in" and missed 217 calls that check in another way.
	reGuard = regexp.MustCompile(`\bassert\b|\bif\s+[^\n]*\bin\b|\.count\s*\(|\braise\s+\w*Error`)

	// A count limit is not a guard, and this row exists because §1's figure
	// only reconciles if it was counted as one. ".replace(old, new, 1)" takes
	// the first match without checking there is only one, which is exactly the
	// silent wrong-occurrence edit §1.2 calls the most expensive failure in the
	// corpus. Reported separately so both readings are visible rather than one
	// being chosen quietly.
	reFirstOnly = regexp.MustCompile(`\.replace\s*\([^)]*,\s*1\s*\)|\.sub\([^)]*count\s*=\s*1`)

	// A verification bundled into the same shell call, after the heredoc.
	reVerify = regexp.MustCompile(`\b(make|go\s+(test|build|vet)|pytest|npm\s+(test|run)|cargo\s+(test|build)|gofmt|golangci-lint|ruff|mypy|tsc|jest|ctest|bazel)\b`)

	// A hand-rolled backup: the 4% in §1's table.
	reBackup = regexp.MustCompile(`\.bak\b|\.orig\b|\bcp\s+[^\n]*\s+/tmp/|\bcp\s+[^\n]*\.(backup|save)\b`)

	// Replacements performed, for §1's "replaces" column.
	reReplace = regexp.MustCompile(`\.replace\s*\(|\.sub\s*\(`)

	// The paths a heredoc edit targets. Two forms, because the corpus uses both
	// and only counting the first misses 69% of the calls:
	//
	//	p = pathlib.Path("docs/invariants.md")      a literal in the call
	//	p='internal/lexer/lexer_test.go'            a variable used later
	//
	// The second is the dominant shape and the first draft of this file did not
	// see it, which put file touches at a third of what §1 reports.
	rePathCall = regexp.MustCompile(`(?:Path|open)\s*\(\s*['"]([^'"\n]+)['"]`)
	rePathVar  = regexp.MustCompile(`(?m)^\s*\w+\s*=\s*['"]([^'"\n]+)['"]\s*$`)

	// A captured literal is a path only if it looks like one: no spaces, and
	// either a directory separator or an extension. Otherwise every string
	// assigned on its own line would count.
	rePathish = regexp.MustCompile(`^[^\s]*(?:/|\.[A-Za-z0-9]{1,6})[^\s]*$`)

	// A failed attempt, read off the tool result rather than guessed at.
	reFailed = regexp.MustCompile(`Traceback \(most recent call last\)|AssertionError|FileNotFoundError|SyntaxError|IndentationError`)

	// Any heredoc, capturing its terminator. Used to remove payloads before
	// looking for command words, which is not the same job reHeredocDelim does
	// for BundlesVerify.
	reAnyHeredoc = regexp.MustCompile(`<<-?\s*['"]?(\w+)`)

	// hunk in a command position: the start of the command, or after a
	// separator. **Not "anywhere in the string".**
	//
	// The corpus contains this project's own source, documentation and probe
	// scripts, so every rule that keys off a bare substring eventually matches
	// them. The previous rule was `HasPrefix(cmd, "hunk ") ||
	// Contains(cmd, "| hunk ")`, and the probe written to inspect these very
	// transcripts counted itself, because its python payload contained the
	// literal "| hunk " as part of its own matching rule. Two earlier versions
	// of the same bug: the eval grader matched \bhunk\b against a workspace
	// path, and a tool_result matched §5.2's documentation of a diagnostic.
	reHunkCall = regexp.MustCompile(`(?:^|[;&|(]|\n)\s*hunk(?:\s|$)`)

	// The invocations that ask hunk what it is rather than to edit anything.
	// An agent checking what is available is not an application, and counting
	// it as one puts --help in the denominator of every rate in §12.
	reHunkProbe = regexp.MustCompile(`(?:^|[;&|(]|\n)\s*hunk\s+(?:format|--help|-h|--version)\b`)

	// hunk's own report, exit by exit. Every one of these is goldened in
	// testdata/, and classify_test.go reads the golden files rather than copies
	// of them, so a reworded diagnostic fails this measurement rather than
	// quietly changing it. SPEC §8.1: "A reworded diagnostic is a contract
	// change."
	//
	// The failure lines are anchored to the start of a line because a verify
	// command's own output is in the same tool result, and it is agent-written
	// text this file has no control over.
	// --json says the exit outright, which is better evidence than any rule
	// about wording. Checked first for that reason, and stable because §5.2
	// makes this shape the contract an MCP server will return.
	reExitJSON    = regexp.MustCompile(`(?m)^\s*"exit":\s*(\d+)`)
	reExitApplied = regexp.MustCompile(`(?m)^\d+ files?, \d+ hunks?, [-+]`)
	reExitNoMatch = regexp.MustCompile(`(?m)^hunk: .*did not match.*nothing was written`)
	// Exits 3 and 4 differ by one clause, and that pair is what §11's open
	// question about --verify-may-format turns on, so they are separate rules
	// rather than one rule and a substring test.
	reExitRollbackIncomplete = regexp.MustCompile(`(?m)^hunk: applied .*verify failed.*left alone`)
	reExitVerifyFailed       = regexp.MustCompile(`(?m)^hunk: applied .*verify failed`)
	reExitChanged            = regexp.MustCompile(`changed on disk between being read and being written`)
	// Exit 1 is enumerable: a parse error carries its patch line, and the usage
	// errors are fixed strings in main.go. Exit 5 is not enumerable, because
	// its text is whatever the OS said, so it is never guessed at.
	reExitUsage = regexp.MustCompile(`(?m)^hunk: (?:patch line \d+:|--[a-z-]+ |unexpected argument|format (?:takes no arguments|is a subcommand))`)
)

// Exit categories that are not exit codes. Negative, so they cannot collide
// with one.
const (
	// ExitProbe: the invocation asked hunk about itself.
	ExitProbe = -1
	// ExitUnclassified: an application whose output this file does not
	// recognise. Counted and printed rather than folded into exit 5: defaulting
	// the unknown to the least-knowable code would make a reworded message look
	// like a rise in I/O errors.
	ExitUnclassified = -2

	// The two codes the §12 walk needs by name.
	ExitOK      = 0
	ExitNoMatch = 2
)

// commandSkeleton is what is left of a shell command once the text that is
// data rather than command has been removed: heredoc payloads and quoted
// spans. An invocation never lives inside either.
//
// The line introducing a heredoc survives, because that is where the command
// is: `hunk --verify 'make test' <<'HUNK'` keeps its first line and loses the
// patch. Escapes inside double quotes are not handled, which is an accepted
// imprecision: the alternative is a shell parser.
func commandSkeleton(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var kept []string
	for i := 0; i < len(lines); i++ {
		kept = append(kept, lines[i])
		m := reAnyHeredoc.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != m[1] {
			i++
		}
		i++ // and the terminator itself
	}
	return stripQuoted(strings.Join(kept, "\n"))
}

// stripQuoted removes single- and double-quoted spans, keeping everything else
// in place. An unterminated quote takes the rest of the command with it, which
// is what the shell would do.
func stripQuoted(s string) string {
	var b strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsHunkCall reports whether a Bash command runs hunk, as opposed to merely
// mentioning it.
func IsHunkCall(cmd string) bool { return reHunkCall.MatchString(commandSkeleton(cmd)) }

// HunkExit is the exit code a hunk invocation ended with, read from its output
// because the transcript does not record one.
//
// The tool_result carries `content` and `is_error` and no status, and is_error
// is the shell pipeline's verdict rather than hunk's: an agent that writes
// `hunk ...; echo "exit=$?"` to see the code, which this project's own
// transcripts are full of, makes every failure look like a success. So the
// output text is the signal, and the goldens are what make it a reliable one.
//
// Returns ExitProbe for an invocation that asked hunk about itself, and
// ExitUnclassified for output this file does not recognise.
func HunkExit(cmd, result string) int {
	if reHunkProbe.MatchString(commandSkeleton(cmd)) {
		return ExitProbe
	}
	if m := reExitJSON.FindStringSubmatch(result); m != nil {
		// An unparseable number is not an exit code. Falling through to the
		// text rules is right: --json and the human report are not both
		// printed, so this is a malformed result rather than a mixed one.
		if code, err := strconv.Atoi(m[1]); err == nil {
			return code
		}
	}
	switch {
	case reExitChanged.MatchString(result):
		return 6
	case reExitRollbackIncomplete.MatchString(result):
		return 4
	case reExitVerifyFailed.MatchString(result):
		return 3
	case reExitNoMatch.MatchString(result):
		return ExitNoMatch
	case reExitUsage.MatchString(result):
		return 1
	case reExitApplied.MatchString(result):
		return ExitOK
	}
	return ExitUnclassified
}

// IsHeredocEdit reports whether a Bash command is the shape §1 counts: a python
// heredoc that writes a file.
func IsHeredocEdit(cmd string) bool {
	return rePythonHeredoc.MatchString(cmd) && reWrites.MatchString(cmd)
}

// HasUniquenessGuard reports whether the edit checks that what it is replacing
// is there. §1: 37% do not, and "a silent wrong-occurrence edit is the most
// expensive failure in the corpus".
func HasUniquenessGuard(cmd string) bool { return reGuard.MatchString(cmd) }

// BundlesVerify reports whether the same shell call also runs a build or a test
// suite. §1: 66% do, which is the figure --help quotes for why --verify exists
// and is optional.
func BundlesVerify(cmd string) bool {
	// Only what comes after the heredoc's terminator counts. Looking after its
	// opening instead counts a "go test" inside the python payload, which is a
	// string the edit writes and not a verification of it.
	m := reHeredocDelim.FindStringSubmatchIndex(cmd)
	if m == nil {
		return false
	}
	delim := cmd[m[2]:m[3]]
	rest := cmd[m[1]:]
	i := strings.Index(rest, "\n"+delim)
	if i < 0 {
		return false // unterminated: there is no "after"
	}
	return reVerify.MatchString(rest[i+1+len(delim):])
}

// LimitsToFirstMatch reports whether the call caps the replacement at one
// occurrence without checking there is only one. Not a guard: it makes a
// wrong-occurrence edit quiet rather than impossible.
func LimitsToFirstMatch(cmd string) bool { return reFirstOnly.MatchString(cmd) }

// HandRollsBackup reports whether the call saves a copy to restore from. §1: 4%.
func HandRollsBackup(cmd string) bool { return reBackup.MatchString(cmd) }

// CountReplaces is §1's "replaces" column: how many substitutions one call
// performs. §1 reports a mean of 2.0 and a max of 21, which is the measurement
// that says the tool must take many hunks per invocation.
func CountReplaces(cmd string) int { return len(reReplace.FindAllString(cmd, -1)) }

// Paths are the files an edit targets, in order of first appearance and
// deduplicated. §1 counts 6,094 file-touches over 5,767 calls.
func Paths(cmd string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] || !rePathish.MatchString(p) {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, m := range rePathCall.FindAllStringSubmatch(cmd, -1) {
		add(m[1])
	}
	for _, m := range rePathVar.FindAllStringSubmatch(cmd, -1) {
		add(m[1])
	}
	return out
}

// Failed reports whether a tool result shows the edit did not happen. Read off
// the result rather than inferred, because an assertion that fired is the
// signal §12's "calls per successful edit" is counting.
func Failed(result string) bool { return reFailed.MatchString(result) }
