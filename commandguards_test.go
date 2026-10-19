package main

// The guards on a command, and every place hunk takes one (#101). A guard
// had been written at one flag's call site and asked of no other: #80
// refused a filter on --verify and decided a single --try is not read, and
// --each and --build, which score an exit, landed after it with nobody
// asking. So --build piped into tail read a patch that did not build as
// caught, at exit 0, and --try '' kept the batch it promises to put back.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// letThrough is how a cell of commandUses says a guard does not apply there.
// The reason is required, so that leaving a guard off is a decision somebody
// wrote down.
const letThrough = "let through: "

// commandUses is every place hunk takes a shell command, and what each guard
// does there: "refused", or letThrough and the reason. A flag that --help
// spells with CMD fails TestEveryCommandFlagHasEveryGuard until it has a row
// here, and a guard added to commandGuards fails it until every row has said
// what the guard does there.
var commandUses = []struct {
	use, flag string
	args      func(cmd string, patches []string) []string
	// One cell a guard in commandGuards, by its name.
	cells map[string]string
}{
	{
		"--verify", "verify",
		func(cmd string, p []string) []string { return []string{"--verify", cmd, "-f", p[0]} },
		map[string]string{
			"empty":    "refused",
			"filter":   "refused",
			"baseline": letThrough + "a verify runs once, with the batch applied, and a failure there is the rollback it is for",
		},
	},
	{
		"a single --try", "try",
		func(cmd string, p []string) []string { return []string{"--try", cmd, "-f", p[0]} },
		map[string]string{
			"empty":    "refused",
			"filter":   letThrough + "a single --try's output is read and its exit passed back, and 58% of the field's --try calls pipe (#80)",
			"baseline": letThrough + "a single --try's exit is passed back, not scored, so there is nothing for a clean-tree run to protect",
		},
	},
	{
		"--try under --each", "try",
		func(cmd string, p []string) []string { return []string{"--try", cmd, "--each", p[0]} },
		map[string]string{"empty": "refused", "filter": "refused", "baseline": "refused"},
	},
	{
		"--build under --each", "build",
		func(cmd string, p []string) []string {
			return []string{"--try", "true", "--build", cmd, "--each", p[0]}
		},
		map[string]string{"empty": "refused", "filter": "refused", "baseline": "refused"},
	},
}

// commandGuards is every guard on a command: the command it is probed with,
// given a marker file to count its runs in, what its refusal says, and how
// often a refused command still ran. The baseline's refusal is the run on the
// tree as it is, so its command runs once; the others refuse before anything.
type commandGuard struct {
	name    string
	command func(mark string) string
	says    string
	ran     int
}

var commandGuards = []commandGuard{
	{"empty", func(string) string { return "" }, "was given an empty command", 0},
	{"filter", func(mark string) string { return counted(mark, "false | tail -1") }, "cannot see the", 0},
	{"baseline", func(mark string) string { return counted(mark, "exit 7") }, "with no patch applied", 1},
}

var helpCommandFlag = regexp.MustCompile(`(?m)^  --([a-z-]+) CMD\b`)

func TestEveryCommandFlagHasEveryGuard(t *testing.T) {
	// The flags come from --help, the same list TestTheSkillNamesEveryFlagOrSaysWhyNot reads.
	var inHelp []string
	for _, m := range helpCommandFlag.FindAllStringSubmatch(usage, -1) {
		inHelp = append(inHelp, m[1])
	}
	var inMap, inRows []string
	for f := range commandFlags {
		inMap = append(inMap, f)
	}
	for _, u := range commandUses {
		if !slices.Contains(inRows, u.flag) {
			inRows = append(inRows, u.flag)
		}
	}
	slices.Sort(inHelp)
	slices.Sort(inMap)
	slices.Sort(inRows)
	if len(inHelp) == 0 {
		t.Fatal("--help spells no flag with CMD; the pattern is out of date, not the help")
	}
	if !slices.Equal(inHelp, inMap) {
		t.Errorf("--help takes a command on %v and commandFlags names %v; a flag in one and not the other skips the empty-command refusal", inHelp, inMap)
	}
	if !slices.Equal(inHelp, inRows) {
		t.Errorf("--help takes a command on %v and commandUses has rows for %v; give each its row, every guard decided", inHelp, inRows)
	}

	for _, u := range commandUses {
		for _, g := range commandGuards {
			cell, ok := u.cells[g.name]
			t.Run(u.use+"/"+g.name, func(t *testing.T) {
				switch {
				case !ok:
					t.Fatalf("%s says nothing about the %s guard: refused, or %q and why", u.use, g.name, letThrough)
				case cell != "refused" && (!strings.HasPrefix(cell, letThrough) || len(cell) == len(letThrough)):
					t.Fatalf("%s's %s cell is %q: refused, or %q and why", u.use, g.name, cell, letThrough)
				}
				root, patches := eachFixture(t)
				mark := pidFile(t, "ran")
				before := snapshot(t, root)
				code, out, errOut := runCLI(t, root, u.args(g.command(mark), patches), "")
				all := out + errOut
				if cell == "refused" {
					if code != exitUsage || !strings.Contains(all, g.says) {
						t.Errorf("exit %d, want 1 and %q: %s", code, g.says, all)
					}
					if n := markerLines(t, mark); n != g.ran {
						t.Errorf("the command ran %d times, want %d", n, g.ran)
					}
				} else {
					if code == exitUsage || strings.Contains(all, g.says) {
						t.Errorf("exit %d, and the guard let through is to stay out of it: %s", code, all)
					}
					if n := markerLines(t, mark); g.name != "empty" && n == 0 {
						t.Errorf("the command never ran: %s", all)
					}
				}
				assertUnchanged(t, root, before)
			})
		}
		for name := range u.cells {
			if !slices.ContainsFunc(commandGuards, func(g commandGuard) bool { return g.name == name }) {
				t.Errorf("%s has a cell for %q, which is no guard in commandGuards", u.use, name)
			}
		}
	}
}

// A blank command is empty too: "$CMD" with CMD set to a newline is no command.
func TestACommandFlagGivenAnEmptyCommandIsRefused(t *testing.T) {
	for _, flag := range []string{"verify", "try", "build"} {
		t.Run("--"+flag+": the golden", func(t *testing.T) {
			root, patches := eachFixture(t)
			before := snapshot(t, root)
			args := []string{"--" + flag, "", "-f", patches[0]}
			if flag == "build" {
				args = []string{"--try", "true", "--build", "", "--each", patches[0]}
			}
			code, out, errOut := runCLI(t, root, args, "")
			if code != exitUsage || out != "" {
				t.Errorf("exit %d, stdout %q; want 1 and the refusal on stderr", code, out)
			}
			golden(t, "cli-empty-"+flag+"-command", errOut)
			assertUnchanged(t, root, before)
		})
	}

	t.Run("--json carries the whole of it", func(t *testing.T) {
		root, patches := eachFixture(t)
		before := snapshot(t, root)
		code, out, errOut := runCLI(t, root, []string{"--json", "--try", "", "-f", patches[0]}, "")
		if code != exitUsage || errOut != "" {
			t.Errorf("exit %d, stderr %q; want 1 and nothing on stderr", code, errOut)
		}
		golden(t, "cli-empty-try-command-json", out)
		var v struct {
			OK    bool
			Exit  int
			Error string
		}
		must(t, json.Unmarshal([]byte(out), &v))
		if v.OK || v.Exit != exitUsage || !strings.Contains(v.Error, "--try was given an empty command") {
			t.Errorf("%s", out)
		}
		assertUnchanged(t, root, before)
	})

	for _, c := range []struct {
		name string
		args func(p []string) []string
		want string
	}{
		{"a blank --verify", func(p []string) []string { return []string{"--verify", "   ", "-f", p[0]} }, "--verify was given an empty command"},
		{"a --try of a tab and a newline", func(p []string) []string { return []string{"--try", "\t\n", "-f", p[0]} }, "--try was given an empty command"},
		{"--try= with nothing after it", func(p []string) []string { return []string{"--try=", "-f", p[0]} }, "--try was given an empty command"},
		{"under --dry-run too", func(p []string) []string { return []string{"--dry-run", "--try", "", "-f", p[0]} }, "--try was given an empty command"},
		{"under --keep-on-fail, whose own refusal would say there is no --verify", func(p []string) []string {
			return []string{"--keep-on-fail", "--verify", "", "-f", p[0]}
		}, "--verify was given an empty command"},
		// --each's own refusal says there is no --try, and there is one.
		{"under --each, ahead of its refusal for a missing --try, and saying what --each lacks", func(p []string) []string {
			return []string{"--try", "", "--each", p[0]}
		}, "--try was given an empty command, so there is no command to try each patch with;"},
		{"before a -f that does not exist", func([]string) []string { return []string{"--try", "", "-f", "/nonexistent/p.hunk"} }, "--try was given an empty command"},
		{"a build blank under --dry-run --each", func(p []string) []string {
			return []string{"--dry-run", "--try", "true", "--build", " ", "--each", p[0]}
		}, "--build was given an empty command"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, patches := eachFixture(t)
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, c.args(patches), "")
			if code != exitUsage || out != "" || !strings.Contains(errOut, c.want) {
				t.Errorf("exit %d, stdout %q, stderr %q; want 1 and %q", code, out, errOut, c.want)
			}
			assertUnchanged(t, root, before)
		})
	}

	// The refusal reads emptiness, not meaning. A command that does nothing
	// runs as it always has, and pinning it says where the line is.
	t.Run("a command that is only a comment is not empty, and runs", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--verify", "# nothing", "-f", patches[0]}, "")
		if code != exitOK || !strings.Contains(out, "verify ok") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
}

func TestATryOrBuildPipedIntoAFilterIsRefusedUnderEach(t *testing.T) {
	for _, c := range []struct{ name, flag string }{{"--try", "try"}, {"--build", "build"}} {
		args := func(cmd string, p ...string) []string {
			if c.flag == "build" {
				return append([]string{"--try", "true", "--build", cmd, "--each"}, p...)
			}
			return append([]string{"--try", cmd, "--each"}, p...)
		}

		t.Run(c.name+": exit 1, nothing tried, nothing run", func(t *testing.T) {
			root, patches := eachFixture(t)
			mark := pidFile(t, "ran")
			before := snapshot(t, root)
			code, out, errOut := runCLI(t, root, args(counted(mark, "false | tail -1"), patches...), "")
			if code != exitUsage || out != "" {
				t.Errorf("exit %d, stdout %q; want 1 and the refusal on stderr", code, out)
			}
			// The golden's command is the marker's, whose path is a temp dir's.
			if !strings.HasPrefix(errOut, "hunk: --"+c.flag+" cannot see the") {
				t.Errorf("stderr = %q", errOut)
			}
			if n := markerLines(t, mark); n != 0 {
				t.Errorf("the command ran %d times, want none: not even on the tree as it is", n)
			}
			assertUnchanged(t, root, before)
		})

		t.Run(c.name+": the golden", func(t *testing.T) {
			root, patches := eachFixture(t)
			_, _, errOut := runCLI(t, root, args("go test ./... 2>&1 | tail -5", patches...), "")
			golden(t, "cli-each-"+c.flag+"-piped-into-a-filter", errOut)
		})

		t.Run(c.name+": --json carries the whole of it", func(t *testing.T) {
			root, patches := eachFixture(t)
			code, out, errOut := runCLI(t, root, append([]string{"--json"}, args("go test ./... 2>&1 | tail -5", patches...)...), "")
			if code != exitUsage || errOut != "" {
				t.Errorf("exit %d, stderr %q", code, errOut)
			}
			golden(t, "cli-each-"+c.flag+"-piped-into-a-filter-json", out)
			var v struct{ Error string }
			must(t, json.Unmarshal([]byte(out), &v))
			if !strings.Contains(v.Error, "drop the pipe") {
				t.Errorf("%s", out)
			}
		})

		for _, f := range statusFilters {
			t.Run(c.name+": naming "+f, func(t *testing.T) {
				root, patches := eachFixture(t)
				code, _, errOut := runCLI(t, root, args("false | "+f, patches[0]), "")
				if code != exitUsage || !strings.Contains(errOut, "piped into "+f+" fail") || !strings.Contains(errOut, "and that is "+f+", so") {
					t.Errorf("exit %d, stderr %q", code, errOut)
				}
			})
		}

		t.Run(c.name+": under --dry-run too", func(t *testing.T) {
			root, patches := eachFixture(t)
			code, out, errOut := runCLI(t, root, append([]string{"--dry-run"}, args("false | tail -1", patches[0])...), "")
			if code != exitUsage || !strings.Contains(errOut, "cannot see the") {
				t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
		})

		// Before the patches are read: the answer is on the command line.
		t.Run(c.name+": before a patch file that does not exist", func(t *testing.T) {
			root, _ := eachFixture(t)
			code, _, errOut := runCLI(t, root, args("false | tail -1", "/nonexistent/p.hunk"), "")
			if code != exitUsage || !strings.Contains(errOut, "cannot see the") {
				t.Errorf("exit %d, stderr %q", code, errOut)
			}
		})

		t.Run(c.name+": after --each's own refusals", func(t *testing.T) {
			root, _ := eachFixture(t)
			code, _, errOut := runCLI(t, root, args("false | tail -1"), "")
			if code != exitUsage || !strings.Contains(errOut, "--each needs at least one patch file") {
				t.Errorf("exit %d, stderr %q", code, errOut)
			}
		})
	}

	t.Run("the build's refusal quotes the --verify-lines in force", func(t *testing.T) {
		root, patches := eachFixture(t)
		_, _, errOut := runCLI(t, root, []string{"--verify-lines", "7", "--try", "true", "--build", "a | tail", "--each", patches[0]}, "")
		if !strings.Contains(errOut, "--verify-lines (7)") {
			t.Errorf("stderr = %q", errOut)
		}
	})

	t.Run("both piped: the command is named, the one the issue was filed about", func(t *testing.T) {
		root, patches := eachFixture(t)
		_, _, errOut := runCLI(t, root, []string{"--try", "a | tail", "--build", "b | head", "--each", patches[0]}, "")
		if !strings.Contains(errOut, "--try cannot see the command piped into tail") {
			t.Errorf("stderr = %q", errOut)
		}
	})
}

// Where the filter refusal stops is a line two documents draw, and #101 was
// filed against the skill's "anything under --try is left alone", written
// before --each existed. Each now has to say a single --try, and name --build.
func TestTheDocsSayWhereTheFilterRefusalStops(t *testing.T) {
	for _, p := range []string{"README.md", filepath.Join(".agents", "skills", "hunk", "SKILL.md")} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		doc := strings.Join(strings.Fields(string(b)), " ")
		for _, want := range []string{"a single `--try`", "`--build`'s"} {
			if !strings.Contains(doc, want) {
				t.Errorf("%s does not say %q", p, want)
			}
		}
	}
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(strings.Fields(string(b)), " "), "given an empty command") {
		t.Error("README.md does not say a command flag given an empty command is refused")
	}
}

// What the refusal under --each leaves alone, as TestWhatTheFilterRefusalLeavesAlone
// does for --verify: the rows that would notice it reach too far.
func TestWhatTheFilterRefusalUnderEachLeavesAlone(t *testing.T) {
	t.Run("a command whose last stage decides is scored by it", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--try", "cat a.txt | grep -q alpha", "--each", patches[0]}, "")
		if code != exitOK || !strings.Contains(eachLine(errOut, patches[0]), "the command exited 1") {
			t.Errorf("exit %d: %s%s", code, out, errOut)
		}
	})

	t.Run("a build whose last stage decides is scored by it", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--try", catchesAll, "--build", "cat a.txt | grep -q alpha", "--each", patches[0]}, "")
		if code != exitVerifyFailed || !strings.Contains(eachLine(errOut, patches[0]), "did not build") {
			t.Errorf("exit %d: %s%s", code, out, errOut)
		}
	})

	// Where sh has pipefail the pipeline fails with its first stage, and the
	// patch is caught; where it does not, set refuses the option on the tree
	// as it is, and the baseline refuses the run. Either way it was not this
	// refusal, and the command ran.
	t.Run("a command under set -o pipefail keeps its pipe", func(t *testing.T) {
		root, patches := eachFixture(t)
		mark := pidFile(t, "ran")
		_, out, errOut := runCLI(t, root, []string{"--try", counted(mark, "set -o pipefail; grep -q alpha a.txt | tail -1"), "--each", patches[0]}, "")
		if strings.Contains(errOut, "cannot see the") || markerLines(t, mark) == 0 {
			t.Errorf("%s%s", out, errOut)
		}
	})

	t.Run("a filter on the command of a single --try is still read as written", func(t *testing.T) {
		root, patches := eachFixture(t)
		code, out, errOut := runCLI(t, root, []string{"--try", "printf 'first\\nsecond\\n' | tail -1", "-f", patches[0]}, "")
		if code != exitOK || !strings.Contains(out, "second") {
			t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	})
}
