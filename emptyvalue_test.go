package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A flag that takes a path or a duration and is given an empty one is refused,
// where until 2026-11-05 it was read as left out, at exit 0: --root "$DIR"
// with DIR unset edited the working directory, -f "$PATCH" read stdin,
// --timeout "$T" set no limit, quote's --root read the working directory, and
// invert's "-f '' -f b.hunk" read b.hunk.

const fromAVariable = "; if it came from a variable, that variable is empty"

// emptyTree is a working directory holding f.txt and a patch that changes it,
// with the same patch on stdin: whichever an empty value fell back to, the
// tree would change, so an unchanged tree is the refusal's proof.
func emptyTree(t *testing.T) (dir string, before map[string]string) {
	t.Helper()
	dir = cliTree(t, map[string]string{"f.txt": "a\n", "n.txt": "N\n"})
	before = snapshot(t, dir)
	t.Chdir(dir)
	return dir, before
}

const emptyStdin = "@@ file f.txt\n@@ old\na\n@@ new\nb\n"

// patchOutside writes the patch outside the tree, so that the snapshot is the
// tree's alone; it is named by its full path.
func patchOutside(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "p.hunk")
	must(t, os.WriteFile(p, []byte(emptyStdin), 0o644))
	return p
}

func TestAnEmptyValueIsRefused(t *testing.T) {
	const p = "P" // stands for a patch file that exists
	for _, c := range []struct {
		name    string
		args    []string
		command string // "hunk", "hunk invert" or "hunk quote", as the message starts
		msg     string // what follows "COMMAND: "
	}{
		{"-f", []string{"-f", ""}, "hunk", "-f was given an empty path"},
		{"-f empty, then a file", []string{"-f", "", "-f", p}, "hunk", "-f was given an empty path"},
		{"a file, then -f empty", []string{"-f", p, "-f", ""}, "hunk", "-f was given an empty path"},
		{"--root", []string{"--root", ""}, "hunk", "--root was given an empty path"},
		{"--root=", []string{"--root="}, "hunk", "--root was given an empty path"},
		{"--timeout under --try", []string{"--timeout", "", "--try", "true"}, "hunk", "--timeout was given an empty duration"},
		{"--timeout under --verify", []string{"--timeout", "", "--verify", "true"}, "hunk", "--timeout was given an empty duration"},
		{"-f under --each, before --each's own refusal", []string{"--try", "true", "--each", "-f", "", p}, "hunk", "-f was given an empty path"},
		{"an --each patch", []string{"--try", "true", "--each", ""}, "hunk", "--each was given an empty path"},
		{"an --each patch after a real one", []string{"--try", "true", "--each", p, ""}, "hunk", "--each was given an empty path"},
		{"two flags, the first in name order named", []string{"--timeout", "", "--root", "", "--try", "true"}, "hunk", "--root was given an empty path"},
		{"invert -f", []string{"invert", "-f", ""}, "hunk invert", "-f was given an empty path"},
		{"invert -f empty, then a file", []string{"invert", "-f", "", "-f", p}, "hunk invert", "-f was given an empty path"},
		{"invert, a file, then -f empty", []string{"invert", "-f", p, "-f", ""}, "hunk invert", "-f was given an empty path"},
		{"quote --root", []string{"quote", "--root", "", "f.txt", "1", "1"}, "hunk quote", "--root was given an empty path"},
		{"quote --new after the range", []string{"quote", "f.txt", "1", "1", "--new", ""}, "hunk quote", "--new was given an empty path"},
		{"quote --new before it", []string{"quote", "--new", "", "f.txt", "1", "1"}, "hunk quote", "--new was given an empty path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, before := emptyTree(t)
			pf := patchOutside(t)
			args := make([]string, len(c.args))
			for i, a := range c.args {
				if a == p {
					a = pf
				}
				args[i] = a
			}
			var out, errOut bytes.Buffer
			code := cli(args, strings.NewReader(emptyStdin), &out, &errOut)
			if code != exitUsage {
				t.Errorf("exit %d, want %d; stderr %q", code, exitUsage, errOut.String())
			}
			want := c.command + ": " + c.msg + fromAVariable + "\n"
			if errOut.String() != want {
				t.Errorf("stderr = %q\nwant     %q", errOut.String(), want)
			}
			// The apply prints nothing on stdout; invert and quote print the
			// refusal as an @@ end line, so the hunk they are piped into stops.
			wantOut := ""
			if c.command != "hunk" {
				wantOut = "@@ end refused by " + c.command + ": " + c.msg + fromAVariable + "\n"
			}
			if out.String() != wantOut {
				t.Errorf("stdout = %q\nwant     %q", out.String(), wantOut)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// --json gets the refusal as the object, on stdout, as every early refusal does.
func TestAnEmptyValueIsRefusedUnderJSON(t *testing.T) {
	dir, before := emptyTree(t)
	var out, errOut bytes.Buffer
	code := cli([]string{"--json", "--root", ""}, strings.NewReader(emptyStdin), &out, &errOut)
	if code != exitUsage {
		t.Errorf("exit %d, want %d", code, exitUsage)
	}
	var rep struct {
		OK    bool   `json:"ok"`
		Exit  int    `json:"exit"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, out.String())
	}
	if rep.OK || rep.Exit != exitUsage || rep.Error != "--root was given an empty path"+fromAVariable {
		t.Errorf("report = %+v", rep)
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want nothing under --json", errOut.String())
	}
	assertUnchanged(t, dir, before)
}

// The controls: each flag given a value goes on as before, and a blank path
// is a name, not empty. Without these, a guard that refused everything would
// pass the test above.
func TestAValueGivenIsNotRefusedAsEmpty(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    func(dir, p string) []string
		code    int
		says    string
		changed bool // f.txt is b afterwards
	}{
		{"-f a file", func(_, p string) []string { return []string{"-f", p} }, exitOK, "M f.txt +1 -1", true},
		{"--root a directory", func(dir, _ string) []string { return []string{"--root", dir} }, exitOK, "M f.txt +1 -1", true},
		{"--timeout a duration", func(_, _ string) []string { return []string{"--timeout", "1m", "--try", "true"} }, exitOK, "tried 1 hunk", false},
		// Passes on the tree as it is, as --each requires, and fails with the patch.
		{"--each a patch", func(_, p string) []string { return []string{"--try", "grep -qx a f.txt", "--each", p} }, exitOK, "every command exited nonzero", false},
		{"invert -f a file", func(_, p string) []string { return []string{"invert", "-f", p} }, exitOK, "@@ old\nb\n@@ new\na\n", false},
		{"quote --root a directory", func(dir, _ string) []string { return []string{"quote", "--root", dir, "f.txt", "1", "1"} }, exitOK, "@@ old\na\n", false},
		{"quote --new a file", func(_, _ string) []string { return []string{"quote", "f.txt", "1", "1", "--new", "n.txt"} }, exitOK, "@@ new\nN\n", false},
		{"-f a blank name, a missing file", func(_, _ string) []string { return []string{"-f", " "} }, exitUsage, notExistPhrase(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, _ := emptyTree(t)
			var out, errOut bytes.Buffer
			code := cli(c.args(dir, patchOutside(t)), strings.NewReader(emptyStdin), &out, &errOut)
			all := out.String() + errOut.String()
			if code != c.code {
				t.Errorf("exit %d, want %d\n%s", code, c.code, all)
			}
			if !strings.Contains(all, c.says) {
				t.Errorf("output does not say %q:\n%s", c.says, all)
			}
			if strings.Contains(all, "was given an empty") {
				t.Errorf("refused as empty:\n%s", all)
			}
			b, err := os.ReadFile(filepath.Join(dir, "f.txt"))
			must(t, err)
			if got := string(b) == "b\n"; got != c.changed {
				t.Errorf("f.txt = %q; changed %v, want %v", b, got, c.changed)
			}
		})
	}
}

func TestEmptyValue(t *testing.T) {
	newFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		var f onceFlag
		fs.Var(&f, "f", "")
		fs.String("root", "", "")
		fs.String("new", "", "")
		fs.String("timeout", "", "")
		fs.String("marker", DefaultMarker, "")
		return fs
	}
	for _, c := range []struct {
		name string
		args []string
		want string // "" for no refusal
	}{
		{"nothing given", nil, ""},
		{"a flag left out is not given, though its default is empty", []string{"-marker", "%%"}, ""},
		{"--root", []string{"-root", ""}, "--root was given an empty path" + fromAVariable},
		{"--new", []string{"-new", ""}, "--new was given an empty path" + fromAVariable},
		{"--timeout", []string{"-timeout", ""}, "--timeout was given an empty duration" + fromAVariable},
		{"-f", []string{"-f", ""}, "-f was given an empty path" + fromAVariable},
		{"-f empty, then a file", []string{"-f", "", "-f", "x"}, "-f was given an empty path" + fromAVariable},
		{"-f a file", []string{"-f", "x"}, ""},
		{"--root blank is a name", []string{"-root", " "}, ""},
		{"a flag not among them, given empty", []string{"-marker", ""}, ""},
		{"two, the first in name order", []string{"-timeout", "", "-new", ""}, "--new was given an empty path" + fromAVariable},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := newFS()
			if err := fs.Parse(c.args); err != nil {
				t.Fatal(err)
			}
			got := ""
			if err := emptyValue(fs); err != nil {
				got = err.Error()
			}
			if got != c.want {
				t.Errorf("emptyValue = %q, want %q", got, c.want)
			}
		})
	}
	if spelled("f") != "-f" || spelled("root") != "--root" {
		t.Errorf("spelled: %q %q", spelled("f"), spelled("root"))
	}
}

func TestOnceFlag(t *testing.T) {
	for _, c := range []struct {
		name  string
		sets  []string
		err   string // of the last Set, "" for none
		path  string
		empty bool
	}{
		{"one", []string{"a"}, "", "a", false},
		{"two", []string{"a", "b"}, "already reading a; AGAIN", "a", false},
		{"empty, then one", []string{"", "a"}, "", "a", true},
		{"one, then empty", []string{"a", ""}, "", "a", true},
		{"empty alone", []string{""}, "", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := onceFlag{again: "AGAIN"}
			var err error
			for _, v := range c.sets {
				err = o.Set(v)
			}
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != c.err || o.path != c.path || o.String() != c.path || o.givenEmpty() != c.empty {
				t.Errorf("err %q path %q empty %v; want %q %q %v", got, o.path, o.givenEmpty(), c.err, c.path, c.empty)
			}
		})
	}
}

// helpValueFlag is a flag in a FLAGS section followed by one space and what it
// takes: "  --root DIR". A flag that takes nothing has two spaces or more.
var helpValueFlag = regexp.MustCompile(`(?m)^  (--?[a-z][a-z-]*) [^ ]`)

func helpValueFlags(help string) []string {
	_, section, ok := strings.Cut(help, "\nFLAGS\n")
	if !ok {
		return nil
	}
	if loc := regexp.MustCompile(`(?m)^[A-Z]`).FindStringIndex(section); loc != nil {
		section = section[:loc[0]]
	}
	var flags []string
	for _, m := range helpValueFlag.FindAllStringSubmatch(section, -1) {
		flags = append(flags, m[1])
	}
	return flags
}

// Every flag the three --help texts spell with a value is refused when given
// an empty one, at exit 1, saying which flag and that the value was empty,
// with nothing written. The list is --help's, so a flag added there is held to
// this without anybody remembering to add a row.
func TestEveryValueFlagRefusesAnEmptyValue(t *testing.T) {
	// What each needs around it to reach its own refusal, rather than a
	// refusal of the call it is in.
	around := map[string][]string{
		"hunk --isolated-link": {"--try", "true", "--isolated"},
		"hunk --each":          {"--try", "true"},
	}
	trailing := map[string][]string{"hunk quote": {"f.txt", "1", "1"}}
	sub := map[string]string{"hunk": usage, "hunk invert": invertUsage, "hunk quote": quoteUsage}
	total := 0
	for command, help := range sub {
		flags := helpValueFlags(help)
		if len(flags) == 0 {
			t.Fatalf("%s --help has no flag with a value; the pattern is out of date, not the help", command)
		}
		total += len(flags)
		for _, fl := range flags {
			t.Run(command+" "+fl, func(t *testing.T) {
				dir, before := emptyTree(t)
				var args []string
				if command != "hunk" {
					args = append(args, strings.TrimPrefix(command, "hunk "))
				}
				args = append(args, around[command+" "+fl]...)
				args = append(args, fl, "")
				args = append(args, trailing[command]...)
				var out, errOut bytes.Buffer
				code := cli(args, strings.NewReader(emptyStdin), &out, &errOut)
				msg := errOut.String()
				if code != exitUsage {
					t.Errorf("%q: exit %d, want %d\n%s", args, code, exitUsage, msg)
				}
				// The flag package spells a flag it refuses with one dash.
				if !namesFlag(msg, fl) && !namesFlag(msg, "-"+strings.TrimLeft(fl, "-")) {
					t.Errorf("%q: the refusal does not name %s: %q", args, fl, msg)
				}
				if !strings.Contains(msg, "empty") && !strings.Contains(msg, `""`) {
					t.Errorf("%q: the refusal does not say the value was empty: %q", args, msg)
				}
				assertUnchanged(t, dir, before)
			})
		}
	}
	// The apply's alone has twelve; fewer means the pattern missed some.
	if total < 15 {
		t.Errorf("found %d flags with a value across the three --help texts; the pattern is missing some", total)
	}
}
