package main

import (
	"slices"
	"strings"
)

// statusFilters are the commands a --verify command's last pipeline may not
// end in (§4). Each exits 0 whatever it was sent, so the pipeline's status is
// the filter's and the command in front of it fails unseen. grep is left out
// on purpose: its status depends on what it read, and of the six verifies in
// the field that ended in one, five were a test somebody wrote.
var statusFilters = []string{"tail", "head", "tee", "cat"}

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
// somebody's own program, are not read as the filter.
func maskingFilter(command string) (string, bool) {
	lower := strings.ToLower(command)
	for _, w := range keepsItsPipe {
		if strings.Contains(lower, w) {
			return "", false
		}
	}
	stages, ok := lastPipeline(command)
	if !ok || len(stages) < 2 || stages[0] == "!" || slices.Contains(stages, "[[") {
		return "", false
	}
	name := stages[len(stages)-1]
	if strings.HasPrefix(name, "/") {
		name = name[strings.LastIndexByte(name, '/')+1:]
	}
	if !slices.Contains(statusFilters, name) {
		return "", false
	}
	return name, true
}

// pipelineScan is what lastPipeline knows part-way through a command.
type pipelineScan struct {
	last   []string // the pipeline that ended most recently
	cur    []string // the stages before this one, in the pipeline being read
	cmd    string   // this stage's command word, once it has one
	found  bool     // whether it has
	some   bool     // this stage holds a word or a redirection
	target bool     // a redirection is waiting for its file
	open   bool     // a |, && or || is waiting for its command
}

// word takes one finished word. A redirection's file is not a word of the
// command, and neither are the NAME=value words in front of the command word.
func (p *pipelineScan) word(raw string) {
	p.some, p.open = true, false
	if p.target {
		p.target = false
		return
	}
	if !p.found && !isAssignment(raw) {
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
	p.cur = append(p.cur, p.cmd)
	p.cmd, p.found, p.some = "", false, false
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

// lastPipeline reads the command word of each stage of the last pipeline in a
// shell command: the one after the last ;, &&, || or newline, whose exit
// status is the command's whenever sh gets that far. A stage that is only
// assignments or redirections has the command word "".
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
func lastPipeline(command string) ([]string, bool) {
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
