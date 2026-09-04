package main

// The classifier. This file is the artifact of the measurement, not the
// numbers: §1's table and §12's comparison are only meaningful if the rule that
// produced them is written down and can be argued with.
//
// Every boundary case here changes a published figure, so each one is a named
// function with a table test rather than a regexp buried in a walker.

import (
	"regexp"
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
)

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
