package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestAPaddedNumberOrALastLineQuotesWhatTheNumberDoes (#191): a START or END
// with ASCII whitespace round it, or $ for the last line, is the plain number,
// so the call prints byte for byte what the call with that number prints, on
// both streams and with the same exit, a refusal included. The field's shape
// is the first row: macOS wc -l pads its count.
func TestAPaddedNumberOrALastLineQuotesWhatTheNumberDoes(t *testing.T) {
	root := cliTree(t, map[string]string{
		"f.txt":       tenLines,
		"hq.txt":      "a\nb\nc\n",
		"nofinal.txt": "a\nb\nc",
		"crlf.txt":    "one\r\ntwo\r\nthree\r\n",
		"one.txt":     "x",
	})
	repl := newFile(t, "NINE\nTEN\n")
	for _, c := range []struct {
		name        string
		args, plain []string
	}{
		{"the issue's wc -l count", []string{"hq.txt", "1", "       3"}, []string{"hq.txt", "1", "3"}},
		{"a START padded with a space", []string{"hq.txt", " 3", "3"}, []string{"hq.txt", "3", "3"}},
		{"a tab before and a line break after", []string{"f.txt", "\t2\n", "4"}, []string{"f.txt", "2", "4"}},
		{"every ASCII space round both", []string{"f.txt", " \r\n2\v\f", "\t4 "}, []string{"f.txt", "2", "4"}},
		{"$ for END", []string{"f.txt", "8", "$"}, []string{"f.txt", "8", "10"}},
		{"$ for both", []string{"f.txt", "$", "$"}, []string{"f.txt", "10", "10"}},
		{"$ for START, and END the last line", []string{"f.txt", "$", "10"}, []string{"f.txt", "10", "10"}},
		{"$ padded", []string{"f.txt", " $ ", "\t$\n"}, []string{"f.txt", "10", "10"}},
		{"$ on the first line through the last", []string{"f.txt", "1", "$"}, []string{"f.txt", "1", "10"}},
		// wc -l says 2 here, one short: $ is what reaches the last line.
		{"$ where the last line has no newline", []string{"nofinal.txt", "2", "$"}, []string{"nofinal.txt", "2", "3"}},
		{"$ on a CRLF file", []string{"crlf.txt", "2", "$"}, []string{"crlf.txt", "2", "3"}},
		{"$ on a file of one line with no newline", []string{"one.txt", "1", "$"}, []string{"one.txt", "1", "1"}},
		{"$ with --new", []string{"f.txt", "9", "$", "--new", repl}, []string{"f.txt", "9", "10", "--new", repl}},
		{"$ under --marker", []string{"--marker", "%%", "f.txt", "9", "$"}, []string{"--marker", "%%", "f.txt", "9", "10"}},
		// Refused the same way, since it is the same range.
		{"$ for START, and END past the end", []string{"f.txt", "$", "12"}, []string{"f.txt", "10", "12"}},
		{"a padded END past the end", []string{"hq.txt", "1", "  4"}, []string{"hq.txt", "1", "4"}},
		{"a padded START after END", []string{"f.txt", "6 ", " 3"}, []string{"f.txt", "6", "3"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := quoteCLI(t, root, c.args...)
			wantCode, wantOut, wantErr := quoteCLI(t, root, c.plain...)
			if code != wantCode || out != wantOut || errOut != wantErr {
				t.Errorf("exit %d\n%s%s\nwant, as %q gives, exit %d\n%s%s", code, out, errOut, c.plain, wantCode, wantOut, wantErr)
			}
		})
	}
}

// The issue's use, end to end: the last two lines of a file whose last line
// has no newline, replaced by a quote to $, through the apply. wc -l's count,
// with its padding taken off, would have left the last line where it was.
func TestAQuoteToTheLastLineApplies(t *testing.T) {
	root := cliTree(t, map[string]string{"f.go": "package f\n\nfunc a() {}\nfunc b() {}"})
	_, patch, errOut := quoteCLI(t, root, "f.go", "3", "$", "--new", newFile(t, "func c() {}\n"))
	if want := "quoted f.go 3-4, 2 lines: \"func a() {}\" to \"func b() {}\"\n"; errOut != want {
		t.Errorf("report %q, want %q", errOut, want)
	}
	code, out, applyErr := runCLI(t, root, nil, patch)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, applyErr)
	}
	b, err := os.ReadFile(filepath.Join(root, "f.go"))
	must(t, err)
	if want := "package f\n\nfunc c() {}"; string(b) != want {
		t.Errorf("f.go = %q, want %q", b, want)
	}
}

// hunk quote --help, the README and the skill each say that $ is the last
// line and why a count is not the way to it, after they name hunk quote: the
// field reached for wc -l because nothing said there was another way.
func TestTheDocsSayTheLastLineIsDollar(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		must(t, err)
		return string(b)
	}
	for _, d := range []struct {
		name, doc, dollar string
	}{
		{"hunk quote --help", quoteUsage, "Either may be $, FILE's last line"},
		{"README.md", read("README.md"), "Either may be `$`, the last line"},
		{"SKILL.md", read(filepath.Join(".agents", "skills", "hunk", "SKILL.md")), "END is `$`, the last line"},
	} {
		_, after, _ := strings.Cut(strings.Join(strings.Fields(d.doc), " "), "hunk quote")
		if !strings.Contains(after, d.dollar) {
			t.Errorf("%s says nothing of %q after naming hunk quote", d.name, d.dollar)
		}
		if plain := strings.ReplaceAll(after, "`", ""); !strings.Contains(plain, "wc -l") || !strings.Contains(plain, "one short") {
			t.Errorf("%s does not say wc -l is one short, after naming hunk quote", d.name)
		}
	}
}

// FuzzLineNumber: START and END are typed by an agent, so whatever arrives
// is a number, the last line, or a refusal at exit 1 that quotes it, never a
// panic. The rule is the oracle: with ASCII whitespace taken off, $ is the
// last line, and ASCII digits whose value is at least 1 and fits are that
// value; nothing else is taken.
func FuzzLineNumber(f *testing.F) {
	for _, s := range []string{
		"1", "007", "       3", "\t2\n", "$", " $ ", "", "   ", "0", "00", "-1", "+1",
		"1 2", "$$", "$1", "99999999999999999999", "9223372036854775807",
		"\u00a03", "3\u0085", "\x1b[2J", "\u0661",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, r := lineNumber(s)
		trimmed := strings.Trim(s, " \t\n\v\f\r")
		digits := trimmed != ""
		for _, c := range []byte(trimmed) {
			digits = digits && c >= '0' && c <= '9'
		}
		v, err := strconv.Atoi(trimmed)
		number := digits && err == nil && v >= 1
		if r != nil {
			if n != 0 || r.code != exitUsage || !strings.Contains(r.msg, strconv.Quote(s)+" is not one") {
				t.Fatalf("lineNumber(%q) = %d, refused at %d: %s", s, n, r.code, r.msg)
			}
			if name, ok := firstControl(showControls(r.msg)); ok {
				t.Fatalf("the refusal prints %s raw: %q", name, r.msg)
			}
			if trimmed == "$" || number {
				t.Fatalf("lineNumber(%q) refused what the rule takes: %s", s, r.msg)
			}
			return
		}
		switch {
		case n == lastLine && trimmed != "$":
			t.Fatalf("lineNumber(%q) is the last line", s)
		case n != lastLine && (!number || n != v):
			t.Fatalf("lineNumber(%q) = %d, which the rule does not take", s, n)
		}
	})
}
