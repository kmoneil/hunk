package main

import (
	"slices"
	"strings"
)

// statusFilters are the commands a --verify command's last pipeline may not
// end in (§4). Each exits 0 whatever it was sent, so the pipeline's status is
// the filter's and the command in front of it fails unseen. grep is left out
// on purpose: its status depends on what it read, and of the six verifies in
// the field that ended in one, five were a test somebody wrote. The sixth was
// a grep -v, which invertedGrep reads (#107).
var statusFilters = []string{"tail", "head", "tee", "cat"}

// greps are the names an inverted grep is read under: egrep and fgrep are
// grep -E and grep -F.
var greps = []string{"grep", "egrep", "fgrep"}

// keepsItsPipe is what a command says to be run as written. Under pipefail a
// failing stage fails its pipeline, and a command that reads PIPESTATUS has
// taken the status into its own hands. Matched in any case; pipe_fail is how
// zsh spells the option.
var keepsItsPipe = []string{"pipefail", "pipe_fail", "pipestatus"}

// maskingFilter reports the filter that the last pipeline of a --verify
// command ends in, which makes that pipeline's exit status the filter's.
// `go test ./... 2>&1 | tail -5` exits 0 with every test failing, and until
// 2026-10-12 hunk kept such a batch and said "verify ok" (issue #80).
//
// It answers only when it is sure. A refusal that is wrong takes away a
// command that worked, and one that is missing costs what it cost before, so
// whatever lastPipeline does not model is run as it always was. So is a
// pipeline that starts with !, whose status is the filter's negated and which
// therefore always fails, and one that holds a [[, inside which | is not a
// pipe.
//
// The filter is its stage's command word, spelled plainly or as an absolute
// path. A quoted one, one behind `command`, and a relative path, which is
// somebody's own program, are not read as the filter. A grep is one when its
// words put -v in effect, and is named "grep -v" however they spell it.
func maskingFilter(command string) (string, bool) {
	lower := strings.ToLower(command)
	for _, w := range keepsItsPipe {
		if strings.Contains(lower, w) {
			return "", false
		}
	}
	stages, ok := lastPipeline(command)
	if !ok || len(stages) < 2 || stages[0].cmd == "!" ||
		slices.ContainsFunc(stages, func(s stage) bool { return s.cmd == "[[" }) {
		return "", false
	}
	last := stages[len(stages)-1]
	name := last.cmd
	if strings.HasPrefix(name, "/") {
		name = name[strings.LastIndexByte(name, '/')+1:]
	}
	switch {
	case slices.Contains(statusFilters, name):
		return name, true
	case slices.Contains(greps, name) && invertedGrep(last.args):
		return name + " -v", true
	}
	return "", false
}

// filterStatus is what a refusal says the pipeline's status is, after "and
// that is". A grep's status depends on what it read, so the refusal says why
// an inverted one's is no test.
func filterStatus(filter string) string {
	if strings.HasSuffix(filter, " -v") {
		return filter + ", which exits 0 whenever one line it reads does not match"
	}
	return filter
}

// Short options grep takes, GNU's and the BSD one macOS ships. One that takes
// an argument takes the rest of its word, or the next word: -ev is the pattern
// "v", and -e -v the pattern "-v". A digit is GNU's -NUM, a context length.
const (
	grepShortArg  = "ABCDdefm"
	grepShortFlag = "abcEFGHhIiJLlMnOoPpqRrSsTUuVvwXxyZz0123456789"
)

// grepLongArg are grep's long options that take the next word as their
// argument unless they carry one after =. grepLongFlag are the rest it has,
// none of which takes the next word: --color and --colour take one only
// after =. grepStatusOnly are the ones that make it print no lines.
var (
	grepLongArg = []string{
		"regexp", "file", "max-count", "after-context", "before-context", "context",
		"label", "group-separator", "binary-files", "devices", "directories",
		"exclude", "exclude-from", "exclude-dir", "include", "include-dir",
	}
	grepLongFlag = []string{
		"extended-regexp", "fixed-strings", "basic-regexp", "perl-regexp",
		"ignore-case", "no-ignore-case", "word-regexp", "line-regexp", "null-data",
		"no-messages", "only-matching", "byte-offset", "with-filename", "no-filename",
		"line-number", "initial-tab", "unix-byte-offsets", "null", "no-group-separator",
		"text", "recursive", "dereference-recursive", "line-buffered", "binary",
		"bz2decompress", "decompress", "lzma", "xz", "help", "version",
	}
	grepStatusOnly = []string{"quiet", "silent", "count", "files-with-matches", "files-without-match"}
)

// invertedGrep reports whether a grep's words put -v in effect and none of
// -q, -c, -l and -L (#107). grep -v exits 0 whenever one line it reads does
// not match, which for a build's output is always, so a verify ending in one
// cannot fail: `zig build check 2>&1 | grep -v "^pass "` kept a batch that
// did not parse, at "verify ok". The four do not change that status. They are
// left because each prints no lines, only a status, a count or a name, so
// nobody wrote one to shorten output, and a grep chosen for its status is
// somebody's test.
//
// The words are read as grep reads them, options after the pattern included,
// since GNU's grep and macOS's both take those, up to --. Anything it is not
// sure of is false: an option neither grep has, which might take the next
// word, an option missing its argument, and a word sh would rewrite.
func invertedGrep(args []string) bool {
	inverted, test := false, false
	for i := 0; i < len(args); i++ {
		w, ok := unquoted(args[i])
		if !ok {
			return false
		}
		if w == "--" {
			break
		}
		if len(w) < 2 || w[0] != '-' {
			continue // the pattern, a file, or - for the standard input
		}
		if long, ok := strings.CutPrefix(w, "--"); ok {
			name, _, valued := strings.Cut(long, "=")
			switch {
			case slices.Contains(grepLongArg, name):
				if !valued {
					if i++; i == len(args) {
						return false
					}
				}
			case valued && name != "color" && name != "colour":
				return false
			case name == "invert-match":
				inverted = true
			case slices.Contains(grepStatusOnly, name):
				test = true
			case !slices.Contains(grepLongFlag, name) && name != "color" && name != "colour":
				return false
			}
			continue
		}
		for j := 1; j < len(w); j++ {
			c := w[j]
			switch {
			case strings.IndexByte(grepShortArg, c) >= 0:
				if j == len(w)-1 {
					if i++; i == len(args) {
						return false
					}
				}
				j = len(w)
			case strings.IndexByte(grepShortFlag, c) < 0:
				return false
			case c == 'v':
				inverted = true
			case c == 'q', c == 'c', c == 'l', c == 'L':
				test = true
			}
		}
	}
	return inverted && !test
}

// unquoted is a word as sh hands it to the command: quotes and backslashes
// removed. It is false for a word sh would rewrite, whose result is not known
// here: a $ outside single quotes, and an unquoted glob or brace, which could
// become a file named like an option.
func unquoted(raw string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; c {
		case '\'':
			end := strings.IndexByte(raw[i+1:], '\'')
			if end < 0 {
				return "", false
			}
			b.WriteString(raw[i+1 : i+1+end])
			i += end + 1
		case '"':
			for i++; ; i++ {
				if i >= len(raw) || raw[i] == '$' {
					return "", false
				}
				if raw[i] == '"' {
					break
				}
				if raw[i] == '\\' && i+1 < len(raw) && strings.IndexByte("$`\"\\\n", raw[i+1]) >= 0 {
					i++
					if raw[i] == '\n' {
						continue
					}
				}
				b.WriteByte(raw[i])
			}
		case '\\':
			if i++; i < len(raw) && raw[i] != '\n' {
				b.WriteByte(raw[i])
			}
		case '$', '*', '?', '[', '{':
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// A stage is one command of a pipeline: its command word as written, and the
// words after it, quotes and all. The NAME=value words in front of the command
// word are neither, and nor is a redirection or its file.
type stage struct {
	cmd  string
	args []string
}

// pipelineScan is what lastPipeline knows part-way through a command.
type pipelineScan struct {
	last   []stage  // the pipeline that ended most recently
	cur    []stage  // the stages before this one, in the pipeline being read
	cmd    string   // this stage's command word, once it has one
	args   []string // the words after it
	found  bool     // whether it has
	some   bool     // this stage holds a word or a redirection
	target bool     // a redirection is waiting for its file
	open   bool     // a |, && or || is waiting for its command
}

// word takes one finished word. A redirection's file is not a word of the
// command, and neither are the NAME=value words in front of the command word.
func (p *pipelineScan) word(raw string) {
	p.some, p.open = true, false
	switch {
	case p.target:
		p.target = false
	case p.found:
		p.args = append(p.args, raw)
	case !isAssignment(raw):
		p.cmd, p.found = raw, true
	}
}

// redirect takes a redirection operator, and is false for one that follows
// another with no file between them.
func (p *pipelineScan) redirect() bool {
	if p.target {
		return false
	}
	p.some, p.open, p.target = true, false, true
	return true
}

// endStage closes the stage being read, and is false for a stage sh would not
// take: an empty one, or one whose redirection has no file.
func (p *pipelineScan) endStage() bool {
	if !p.some || p.target {
		return false
	}
	p.cur = append(p.cur, stage{p.cmd, p.args})
	p.cmd, p.args, p.found, p.some = "", nil, false, false
	return true
}

// endList closes the stage and the pipeline it ends.
func (p *pipelineScan) endList() bool {
	if !p.endStage() {
		return false
	}
	p.last, p.cur = p.cur, nil
	return true
}

// lastPipeline reads each stage of the last pipeline in a shell command: the
// one after the last ;, &&, || or newline, whose exit status is the command's
// whenever sh gets that far. A stage that is only assignments or redirections
// has the command word "".
//
// It models words, both kinds of quote, backslash, comments, redirections and
// the operators | |& ; && || and newline. Everything else is false: a
// substitution, a parenthesis, a here-document, a background job, a quote that
// does not close, a stage or a redirection with nothing in it, a command that
// stops at an operator. That is a subset on purpose. Every filtered verify in
// the field was inside it, and a parser for the rest of sh would be a second
// shell to keep right.
//
// One pass: a byte is looked at once, or twice where a quote is being closed.
func lastPipeline(command string) ([]stage, bool) {
	var p pipelineScan
	n := len(command)
	start := -1 // where the word being read began
	endWord := func(end int) {
		if start >= 0 {
			p.word(command[start:end])
			start = -1
		}
	}
	for i := 0; i < n; {
		c := command[i]
		switch c {
		case ' ', '\t':
			endWord(i)
			i++
		case '\n':
			endWord(i)
			i++
			// A newline ends a list. After |, && or || it is a line break
			// inside one, and on a line of its own it is nothing.
			if p.some && !p.endList() {
				return nil, false
			}
		case ';':
			endWord(i)
			i++
			if !p.endList() {
				return nil, false
			}
		case '|':
			endWord(i)
			i++
			if i < n && command[i] == '|' {
				i++
				if !p.endList() {
					return nil, false
				}
			} else {
				if i < n && command[i] == '&' {
					i++ // |& pipes stderr too, where sh has it
				}
				if !p.endStage() {
					return nil, false
				}
			}
			p.open = true
		case '&':
			endWord(i)
			switch {
			case i+1 < n && command[i+1] == '&':
				i += 2
				if !p.endList() {
					return nil, false
				}
				p.open = true
			case i+1 < n && command[i+1] == '>':
				i += 2
				if i < n && command[i] == '>' {
					i++
				}
				if !p.redirect() {
					return nil, false
				}
			default:
				return nil, false // a background job
			}
		case '<', '>':
			// Digits stuck to the front of a redirection are its file
			// descriptor: the 2 of 2>&1 is not a word.
			if start >= 0 && isDigits(command[start:i]) {
				start = -1
			}
			endWord(i)
			i++
			if i < n {
				switch next := command[i]; {
				case c == '<' && next == '<':
					return nil, false // a here-document
				case next == '&', next == '>', c == '>' && next == '|':
					i++ // >& <& >> <> and >|, none of which is an operator
				}
			}
			if !p.redirect() {
				return nil, false
			}
		case '\'':
			end := strings.IndexByte(command[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			if start < 0 {
				start = i
			}
			i += end + 2
		case '"':
			end, ok := closeDouble(command, i+1)
			if !ok {
				return nil, false
			}
			if start < 0 {
				start = i
			}
			i = end
		case '\\':
			if i+1 < n && command[i+1] == '\n' {
				i += 2 // a continued line: neither a character nor a break
				continue
			}
			if start < 0 {
				start = i
			}
			i += 2
		case '#':
			if start >= 0 {
				i++ // inside a word it is a character
				continue
			}
			if nl := strings.IndexByte(command[i:], '\n'); nl >= 0 {
				i += nl
			} else {
				i = n
			}
		case '$':
			// ${ and $' here; $( is refused at its parenthesis, below.
			if i+1 < n && (command[i+1] == '{' || command[i+1] == '\'') {
				return nil, false
			}
			if start < 0 {
				start = i
			}
			i++
		case '`', '(', ')':
			return nil, false
		default:
			if start < 0 {
				start = i
			}
			i++
		}
	}
	endWord(n)
	if p.some {
		if !p.endList() {
			return nil, false
		}
	} else if p.open {
		return nil, false
	}
	return p.last, true
}

// closeDouble returns the index after the double quote that closes the one
// before i. It is false when there is none, and when the quoted text holds a
// substitution, which a double quote does not stop sh from running.
func closeDouble(s string, i int) (int, bool) {
	for i < len(s) {
		switch s[i] {
		case '"':
			return i + 1, true
		case '\\':
			i += 2
		case '`':
			return 0, false
		case '$':
			if i+1 < len(s) && (s[i+1] == '(' || s[i+1] == '{') {
				return 0, false
			}
			i++
		default:
			i++
		}
	}
	return 0, false
}

// isAssignment reports whether a word is NAME=value, which in front of a
// command word sets its environment and is not the command.
func isAssignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	if eq < 1 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := word[i]
		letter := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// isDigits reports whether s, which is not empty, is all decimal digits.
func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
