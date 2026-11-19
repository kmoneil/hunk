package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The test binary is gh too: with NIGHTLY_FIND_GH set it prints what it was
// asked and exits as told, so execRunner runs a real program.
func TestMain(m *testing.M) {
	if os.Getenv("NIGHTLY_FIND_MAIN") != "" {
		os.Args = append([]string{"nightly-find"}, os.Args[1:]...)
		main()
	}
	if mode := os.Getenv("NIGHTLY_FIND_GH"); mode != "" {
		args := os.Args[1:]
		if mode == "fail" {
			fmt.Fprintln(os.Stderr, "gh: not found: "+strings.Join(args, " "))
			os.Exit(1)
		}
		fmt.Print(strings.Join(args, " "))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestExecRunner(t *testing.T) {
	r := execRunner(map[string]string{"gh": os.Args[0]})
	t.Setenv("NIGHTLY_FIND_GH", "ok")
	out, err := r("gh", "run", "view", "7")
	if err != nil || string(out) != "run view 7" {
		t.Errorf("gh run view 7 = %q, %v; want its arguments back", out, err)
	}
	t.Setenv("NIGHTLY_FIND_GH", "fail")
	_, err = r("gh", "issue", "list")
	if err == nil || !strings.Contains(err.Error(), "gh issue list: exit status 1: gh: not found: issue list") {
		t.Errorf("a failing gh = %v, want its command, status and stderr", err)
	}
	// A name with no entry in the map is run as itself.
	if _, err := execRunner(nil)("nightly-find-no-such-program"); err == nil {
		t.Error("a program that does not exist ran")
	}
}

const (
	repo     = "kmoneil/hunk"
	runID    = "44348995564"
	runURL0  = "https://github.com/kmoneil/hunk/actions/runs/44348995564"
	sha      = "b667022ab3a7f0d72e68b664c8551e02a9e1c5f0"
	target   = "FuzzAnInverseUndoesThePatch"
	newInput = "f1b1d2a859bcf497"
	seed     = "115635d06a5b9867"
	content  = "go test fuzz v1\n[]byte(\"\\xe7\\r\\n\\r\")\n"
)

// fake is gh and git as a run would find them. A key of fail, a command's
// first words, makes that command fail.
type fake struct {
	list      string
	view      string
	logs      map[string]string            // job id -> raw log
	artifacts map[string]map[string]string // target -> path in the artifact -> content
	links     map[string]string            // target -> a path in the artifact to make a symlink
	committed []string
	describe  string
	issues    string
	fail      map[string]bool
}

func (f *fake) run(name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	for prefix := range f.fail {
		if strings.HasPrefix(call, prefix) {
			return nil, errors.New(prefix + ": failed")
		}
	}
	switch {
	case strings.HasPrefix(call, "gh run list "):
		return []byte(f.list), nil
	case strings.HasPrefix(call, "gh run view "):
		return []byte(f.view), nil
	case strings.HasPrefix(call, "gh api "):
		id := regexp.MustCompile(`jobs/([0-9]+)/logs`).FindStringSubmatch(call)[1]
		return []byte(f.logs[id]), nil
	case strings.HasPrefix(call, "gh run download "):
		t := strings.TrimPrefix(args[slices.Index(args, "-n")+1], "crasher-")
		dir := args[slices.Index(args, "-D")+1]
		for p, c := range f.artifacts[t] {
			dst := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(dst, []byte(c), 0o644); err != nil {
				return nil, err
			}
		}
		if p, ok := f.links[t]; ok {
			if err := os.Symlink("elsewhere", filepath.Join(dir, filepath.FromSlash(p))); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case strings.HasPrefix(call, "git ") && slices.Contains(args, "ls-files"):
		return []byte(strings.Join(f.committed, "\x00")), nil
	case strings.HasPrefix(call, "git ") && slices.Contains(args, "describe"):
		if f.describe == "" {
			return nil, errors.New("git describe: unknown revision")
		}
		return []byte(f.describe + "\n"), nil
	case strings.HasPrefix(call, "gh issue list "):
		return []byte(f.issues), nil
	}
	return nil, fmt.Errorf("the fake has no %q", call)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// deadlineLog is golang/go#75804's shape as t117 recorded it from run
// 40092532612, whose logs have expired: "context deadline exceeded" the only
// failure line, no input written, and no toolchain line, which #117 added.
const deadlineLog = "\xef\xbb\xbf2026-10-24T08:00:00.0000000Z Current runner version: '2.329.0'\n" +
	"2026-10-24T08:10:00.0000000Z --- FAIL: FuzzLooksLikeDelimiter (600.07s)\n" +
	"2026-10-24T08:10:00.0000000Z     context deadline exceeded\n" +
	"2026-10-24T08:10:00.0000000Z FAIL\n" +
	"2026-10-24T08:10:00.0000000Z exit status 1\n" +
	"2026-10-24T08:10:00.0000000Z FAIL\tgithub.com/kmoneil/hunk\t600.084s\n" +
	"2026-10-24T08:10:00.0000000Z ##[error]Process completed with exit code 2.\n"

// theFind is run 44348995564 as gh and git answer for it.
func theFind(t *testing.T) *fake {
	return &fake{
		list: `[{"databaseId":44348995564}]`,
		view: `{"databaseId":44348995564,"headSha":"` + sha + `","url":"` + runURL0 + `","workflowName":"fuzz-nightly","jobs":[` +
			`{"databaseId":1,"name":"list the fuzz targets","conclusion":"success"},` +
			`{"databaseId":2,"name":"fuzz (FuzzParse)","conclusion":"success"},` +
			`{"databaseId":134960627380,"name":"fuzz (FuzzAnInverseUndoesThePatch)","conclusion":"failure"},` +
			`{"databaseId":3,"name":"report a find","conclusion":"success"}]}`,
		logs: map[string]string{"134960627380": readFixture(t, "find.log")},
		artifacts: map[string]map[string]string{target: {
			target + "/" + seed:          "go test fuzz v1\n[]byte(\"seed\")\n",
			target + "/" + newInput:      content,
			"FuzzParse/0000000000000001": "go test fuzz v1\n[]byte(\"other\")\n",
		}},
		committed: []string{"testdata/fuzz/" + target + "/" + seed, "testdata/fuzz/FuzzParse/0000000000000001"},
		describe:  "v0.3.49-1-gb667022",
		issues:    `[{"number":182,"title":"fuzz: the nightly sweep found something","url":"https://github.com/kmoneil/hunk/issues/182","createdAt":"2026-11-18T17:10:10Z"}]`,
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name+".txt")
	if *update {
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (go test -update writes it; then read it)", err)
	}
	if got != string(want) {
		t.Errorf("%s changed:\n--- want ---\n%s\n--- got ---\n%s", p, want, got)
	}
}

// The nightly's find of 2026-11-18, taken in: the body a card gets, the input
// copied where the log says it was written, and the next step on stderr.
func TestTheFindIsTakenIn(t *testing.T) {
	root := t.TempDir()
	var out, errOut bytes.Buffer
	if code := run([]string{"--root", root, runURL0 + "/job/134960627380"}, &out, &errOut, theFind(t).run); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	golden(t, "body-find", out.String())
	wantErr := "testdata/fuzz/" + target + "/" + newInput + ": copied\n" +
		"next: karte add \"TITLE\" --type bug -p 2 -m gh=182 -b -   (the body above on stdin)\n"
	if errOut.String() != wantErr {
		t.Errorf("stderr = %q, want %q", errOut.String(), wantErr)
	}
	b, err := os.ReadFile(filepath.Join(root, "testdata", "fuzz", target, newInput))
	if err != nil || string(b) != content {
		t.Errorf("the input in the tree = %q, %v; want the artifact's bytes", b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "testdata", "fuzz", target, seed)); !os.IsNotExist(err) {
		t.Errorf("a committed seed was copied: %v", err)
	}
}

// A hang and a deadline flake: a find of time, and a run with nothing found,
// whose body says to re-run and commit nothing.
func TestOtherShapesAreTakenIn(t *testing.T) {
	f := theFind(t)
	f.view = `{"databaseId":7,"headSha":"abc","url":"u","jobs":[` +
		`{"databaseId":10,"name":"fuzz (FuzzInverseToUndoesTheBatch)","conclusion":"failure"},` +
		`{"databaseId":11,"name":"fuzz (FuzzLooksLikeDelimiter)","conclusion":"failure"}]}`
	f.logs = map[string]string{"10": readFixture(t, "hang.log"), "11": deadlineLog}
	f.artifacts = map[string]map[string]string{
		"FuzzInverseToUndoesTheBatch": {"FuzzInverseToUndoesTheBatch/b63b19337e4dd6b4": content},
		"FuzzLooksLikeDelimiter":      {target + "/" + seed: content},
	}
	f.describe, f.issues = "", "[]"
	root := t.TempDir()
	var out, errOut bytes.Buffer
	if code := run([]string{"--root", root, "7"}, &out, &errOut, f.run); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	golden(t, "body-hang-and-deadline", out.String())
	if strings.Contains(errOut.String(), "next:") {
		t.Errorf("a next step with no issue to name: %s", errOut.String())
	}
}

// A run whose only failure is golang/go#75804's flake has nothing to fix, and
// its body says so.
func TestAFlakeAloneIsTakenIn(t *testing.T) {
	f := theFind(t)
	f.view = `{"databaseId":7,"headSha":"` + sha + `","url":"u","jobs":[{"databaseId":11,"name":"fuzz (FuzzLooksLikeDelimiter)","conclusion":"failure"}]}`
	f.logs = map[string]string{"11": deadlineLog}
	f.artifacts = map[string]map[string]string{"FuzzLooksLikeDelimiter": {target + "/" + seed: content}}
	var out, errOut bytes.Buffer
	if code := run([]string{"--root", t.TempDir(), "7"}, &out, &errOut, f.run); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.HasSuffix(out.String(), "\n\ndone: the failed jobs re-run, or the failure explained; nothing committed\n") {
		t.Errorf("the body does not end on a re-run:\n%s", out.String())
	}
}

// Where the tool's own directory cannot be made, and where the artifact holds
// a directory or a file it cannot read, the run is refused with the reason.
func TestWhatCannotBeReadIsRefused(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	t.Run("no temporary directory", func(t *testing.T) {
		t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "absent"))
		var out, errOut bytes.Buffer
		if code := run([]string{"--root", t.TempDir(), runID}, &out, &errOut, theFind(t).run); code != 1 || !strings.Contains(errOut.String(), "absent: no such file or directory") {
			t.Errorf("exit %d, stderr %q; want 1 and the directory it could not make", code, errOut.String())
		}
	})
	for _, c := range []struct{ name, path, want string }{
		{"a target it cannot read", target, "permission denied"},
		{"an input it cannot read", target + "/" + newInput, "permission denied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			f := theFind(t)
			download := f.run
			var locked string
			f2 := func(name string, args ...string) ([]byte, error) {
				out, err := download(name, args...)
				if len(args) > 1 && args[0] == "run" && args[1] == "download" {
					locked = filepath.Join(args[slices.Index(args, "-D")+1], filepath.FromSlash(c.path))
					if err := os.Chmod(locked, 0); err != nil {
						t.Fatal(err)
					}
				}
				return out, err
			}
			// Put the mode back before the directories are removed.
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			var out, errOut bytes.Buffer
			if code := run([]string{"--root", t.TempDir(), runID}, &out, &errOut, f2); code != 1 || !strings.Contains(errOut.String(), c.want) {
				t.Errorf("exit %d, stderr %q; want 1 and %q", code, errOut.String(), c.want)
			}
		})
	}
}

// main is run itself, as the test binary under NIGHTLY_FIND_MAIN: a bad call
// exits 2 with the reason. go test folds the subprocess's coverage into the
// profile.
func TestMainExits(t *testing.T) {
	cmd := exec.Command(os.Args[0], "--no-such-flag")
	cmd.Env = append(os.Environ(), "NIGHTLY_FIND_MAIN=1")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 || !strings.Contains(string(out), "flag provided but not defined: -no-such-flag") {
		t.Errorf("main = %v, %q; want exit 2 and the flag named", err, out)
	}
}

// Each way an input settles, and the run's exit for it.
func TestAnInputSettles(t *testing.T) {
	dst := func(root string) string { return filepath.Join(root, "testdata", "fuzz", target, newInput) }
	for _, c := range []struct {
		name    string
		args    []string
		prepare func(t *testing.T, root string, f *fake)
		state   string
		code    int
		problem string
		want    string // the file in the tree afterwards; "" for none
	}{
		{name: "copied", state: "copied", want: content},
		{name: "named and not copied under --no-copy", args: []string{"--no-copy"}, state: "not copied"},
		{
			name: "already in the tree, the same bytes, not committed", state: "already in the tree", want: content,
			prepare: func(t *testing.T, root string, _ *fake) { write(t, dst(root), content) },
		},
		{
			name: "in the tree with other bytes", state: "not copied", code: 1, want: "mine",
			problem: "is in the tree with other bytes, and was left as it is",
			prepare: func(t *testing.T, root string, _ *fake) { write(t, dst(root), "mine") },
		},
		{
			name: "testdata/fuzz is a file", state: "not copied", code: 1,
			problem: "could not be copied",
			prepare: func(t *testing.T, root string, _ *fake) { write(t, filepath.Join(root, "testdata", "fuzz"), "x") },
		},
		{
			name: "a directory where the input would go", state: "not copied", code: 1,
			problem: "could not be copied",
			prepare: func(t *testing.T, root string, _ *fake) {
				if err := os.MkdirAll(dst(root), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the log names an input the artifact does not hold", state: "copied", code: 1, want: content,
			problem: "the log names testdata/fuzz/" + target + "/0123456789abcdef, and the artifact holds no new file by that name",
			prepare: func(t *testing.T, _ string, f *fake) {
				f.logs["134960627380"] = strings.ReplaceAll(f.logs["134960627380"], "written to testdata/fuzz/"+target+"/"+newInput,
					"written to testdata/fuzz/"+target+"/0123456789abcdef")
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, f := t.TempDir(), theFind(t)
			if c.prepare != nil {
				c.prepare(t, root, f)
			}
			var out, errOut bytes.Buffer
			code := run(append(append([]string{"--root", root}, c.args...), runID), &out, &errOut, f.run)
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s", code, c.code, errOut.String())
			}
			line := "testdata/fuzz/" + target + "/" + newInput + ": " + c.state + "\n"
			if !strings.Contains(errOut.String(), line) || !strings.Contains(out.String(), "("+c.state+")") {
				t.Errorf("want %q on stderr and in the body:\n%s\n%s", line, errOut.String(), out.String())
			}
			if (c.problem == "") != !strings.Contains(errOut.String(), "look: ") || !strings.Contains(errOut.String(), c.problem) {
				t.Errorf("stderr = %q, want problem %q", errOut.String(), c.problem)
			}
			b, err := os.ReadFile(dst(root))
			if got := string(b); (err == nil) != (c.want != "") || got != c.want {
				t.Errorf("the tree holds %q (%v), want %q", got, err, c.want)
			}
		})
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A run whose find has been worked: the input it wrote is committed, which
// is said, and nothing is left to look at.
func TestAWorkedFindIsTakenIn(t *testing.T) {
	f := theFind(t)
	f.committed = append(f.committed, "testdata/fuzz/"+target+"/"+newInput)
	var out, errOut bytes.Buffer
	if code := run([]string{"--root", t.TempDir(), runID}, &out, &errOut, f.run); code != 0 || strings.Contains(errOut.String(), "look:") {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if want := " The input it wrote, testdata/fuzz/" + target + "/" + newInput + ", is committed already.\n"; !strings.Contains(out.String(), want) {
		t.Errorf("the body does not say %q:\n%s", want, out.String())
	}
}

// Without a run named, the latest failed fuzz-nightly run is taken; several
// new inputs are listed; and a commit the clone lacks is said to be so.
func TestTheLatestRunIsTakenIn(t *testing.T) {
	f := theFind(t)
	f.artifacts[target][target+"/aaaaaaaaaaaaaaaa"] = content
	f.describe = ""
	var out, errOut bytes.Buffer
	if code := run([]string{"--root", t.TempDir()}, &out, &errOut, f.run); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{
		"Nightly run 44348995564 on b667022 (not in this clone: git fetch to describe it): ",
		"The new inputs in the artifact are testdata/fuzz/" + target + "/aaaaaaaaaaaaaaaa (copied), testdata/fuzz/" + target + "/" + newInput + " (copied).",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the body does not say %q:\n%s", want, out.String())
		}
	}
}

// Every refusal, named: the call, what gh and git said, and what the
// artifact holds.
func TestARunIsRefused(t *testing.T) {
	for _, c := range []struct {
		name    string
		args    []string
		prepare func(f *fake)
		code    int
		want    string
	}{
		{name: "two runs", args: []string{"1", "2"}, code: 2, want: "one run at most"},
		{name: "not a run", args: []string{"main"}, code: 2, want: `"main" is not a run's number or URL`},
		{name: "a flag it does not have", args: []string{"--copy"}, code: 2, want: "flag provided but not defined: -copy"},
		{name: "gh run list fails", prepare: func(f *fake) { f.fail = map[string]bool{"gh run list": true} }, code: 1, want: "gh run list: failed"},
		{name: "gh run list is not JSON", prepare: func(f *fake) { f.list = "<html>" }, code: 1, want: "gh run list: invalid character"},
		{name: "no failed run", prepare: func(f *fake) { f.list = "[]" }, code: 1, want: "no failed fuzz-nightly run"},
		{name: "gh run view fails", args: []string{runID}, prepare: func(f *fake) { f.fail = map[string]bool{"gh run view": true} }, code: 1, want: "gh run view: failed"},
		{name: "gh run view is not JSON", args: []string{runID}, prepare: func(f *fake) { f.view = "" }, code: 1, want: "gh run view: unexpected end of JSON input"},
		{name: "git ls-files fails", args: []string{runID}, prepare: func(f *fake) { f.fail = map[string]bool{"git -C": true} }, code: 1, want: "git -C: failed"},
		{name: "the log cannot be read", args: []string{runID}, prepare: func(f *fake) { f.fail = map[string]bool{"gh api": true} }, code: 1, want: "gh api: failed"},
		{name: "the artifact cannot be downloaded", args: []string{runID}, prepare: func(f *fake) { f.fail = map[string]bool{"gh run download": true} }, code: 1, want: "gh run download: failed"},
		{
			name: "no failed fuzz job", args: []string{runID}, code: 1, want: "run 44348995564 has no failed fuzz job",
			prepare: func(f *fake) { f.view = strings.ReplaceAll(f.view, `"failure"`, `"success"`) },
		},
		{
			name: "a file beside the targets", args: []string{runID}, code: 1, want: "the artifact holds README, which is not a fuzz input",
			prepare: func(f *fake) { f.artifacts[target]["README"] = content },
		},
		{
			name: "a directory inside a target", args: []string{runID}, code: 1, want: "the artifact holds " + target + "/x, which is not a fuzz input",
			prepare: func(f *fake) { f.artifacts[target][target+"/x/0000000000000002"] = content },
		},
		{
			name: "a symlink", args: []string{runID}, code: 1, want: "the artifact holds " + target + "/0000000000000003, which is not a fuzz input",
			prepare: func(f *fake) { f.links = map[string]string{target: target + "/0000000000000003"} },
		},
		{
			name: "a name Go does not give", args: []string{runID}, code: 1, want: "the artifact holds " + target + "/ABC, whose name is not one Go gives an input",
			prepare: func(f *fake) { f.artifacts[target][target+"/ABC"] = content },
		},
		{
			name: "a file that is not a corpus file", args: []string{runID}, code: 1, want: "which is not a fuzz corpus file",
			prepare: func(f *fake) { f.artifacts[target][target+"/0000000000000004"] = "#!/bin/sh\n" },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := theFind(t)
			if c.prepare != nil {
				c.prepare(f)
			}
			root := t.TempDir()
			var out, errOut bytes.Buffer
			code := run(append([]string{"--root", root}, c.args...), &out, &errOut, f.run)
			if code != c.code || !strings.Contains(errOut.String(), c.want) || out.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q; want %d and %q", code, out.String(), errOut.String(), c.code, c.want)
			}
			if _, err := os.Stat(filepath.Join(root, "testdata")); !os.IsNotExist(err) {
				t.Errorf("a refused run wrote the tree: %v", err)
			}
		})
	}
}

func TestParseRun(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"44348995564", "44348995564", true},
		{runURL0, "44348995564", true},
		{runURL0 + "/job/134960627380", "44348995564", true},
		{"", "", false},
		{"main", "", false},
		{"12a", "", false},
		{"https://github.com/kmoneil/hunk/actions/runs/", "", false},
		{"https://github.com/kmoneil/hunk/actions/runs/12x", "", false},
	} {
		if got, ok := parseRun(c.in); got != c.want || ok != c.ok {
			t.Errorf("parseRun(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseLog(t *testing.T) {
	find := parseLog([]byte(readFixture(t, "find.log")))
	if find.toolchain != "go1.27.3" || find.input != "testdata/fuzz/"+target+"/"+newInput || find.deadline || find.hung {
		t.Errorf("the find's log = %+v", find)
	}
	if len(find.failure) != 9 || find.failure[0] != "--- FAIL: "+target+" (171.26s)" ||
		find.failure[8] != "    go test -run="+target+"/"+newInput {
		t.Errorf("the find's block = %q", find.failure)
	}
	hang := parseLog([]byte(readFixture(t, "hang.log")))
	if !hang.hung || hang.input == "" || hang.deadline {
		t.Errorf("the hang's log = %+v", hang)
	}
	flake := parseLog([]byte(deadlineLog))
	if !flake.deadline || flake.input != "" || flake.toolchain != "" || len(flake.failure) != 2 {
		t.Errorf("the flake's log = %+v", flake)
	}
	// A second block is not appended to the first; blank lines at the end
	// go; CRLF lines read as LF; a line with no time is read as it is.
	two := parseLog([]byte("--- FAIL: A\r\n    a\r\n\r\nFAIL\r\n--- FAIL: B\nFAIL\n"))
	if !slices.Equal(two.failure, []string{"--- FAIL: A", "    a"}) {
		t.Errorf("two blocks = %q", two.failure)
	}
	if none := parseLog([]byte("fuzz: toolchain go1.27.3\nfuzz: toolchain go1.26.9\nok\n")); none.failure != nil || none.toolchain != "go1.27.3" {
		t.Errorf("no block = %+v, want no failure and the first toolchain line", none)
	}
	// Actions colours the lines a step echoes, and a coloured line is read
	// as its text.
	coloured := parseLog([]byte("2026-11-18T16:48:57.6509434Z \x1b[36;1mfuzz: toolchain go1.27.3\x1b[0m\n\x1b[31m--- FAIL: F (1s)\x1b[0m\nFAIL\n"))
	if coloured.toolchain != "go1.27.3" || !slices.Equal(coloured.failure, []string{"--- FAIL: F (1s)"}) {
		t.Errorf("coloured lines = %+v", coloured)
	}
}

// copyInput writes only where nothing is: settle has looked first, and this
// is what holds if something got there between the look and the write.
func TestCopyInputNeverOverwrites(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "testdata", "fuzz", target, newInput)
	write(t, dst, "mine")
	if err := copyInput(dst, []byte(content)); !errors.Is(err, os.ErrExist) {
		t.Errorf("copyInput over a file = %v, want it to exist already", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "mine" {
		t.Errorf("the file = %q, want it untouched", b)
	}
}

func TestVerdict(t *testing.T) {
	block := []string{"--- FAIL: F (1s)"}
	for _, c := range []struct {
		name string
		l    jobLog
		want string
	}{
		{"no block", jobLog{}, "no --- FAIL line"},
		{"a hang", jobLog{failure: block, input: "x", hung: true}, "a find of time or memory"},
		{"a find", jobLog{failure: block, input: "x"}, "a find"},
		{"no input and no deadline", jobLog{failure: block}, "a failure with no input written"},
		{"a deadline with no toolchain line", jobLog{failure: block, deadline: true}, "golang/go#75804's deadline flake"},
		{"a deadline on go1.26.9", jobLog{failure: block, deadline: true, toolchain: "go1.26.9"}, "golang/go#75804's deadline flake"},
		{"a deadline on go1.27.3", jobLog{failure: block, deadline: true, toolchain: "go1.27.3"}, "a deadline with no input on go1.27.3, which is not golang/go#75804"},
		{"a deadline on a toolchain it cannot read", jobLog{failure: block, deadline: true, toolchain: "devel"}, "a deadline with no input on devel"},
	} {
		if got := c.l.verdict(); !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: verdict %q, want it to open %q", c.name, got, c.want)
		}
	}
}

func TestFixedFlake(t *testing.T) {
	for _, c := range []struct {
		toolchain    string
		fixed, known bool
	}{
		{"go1.26.9", false, true},
		{"go1.27.0", true, true},
		{"go1.27.3", true, true},
		{"go1.100.1", true, true},
		{"go2.0", true, true},
		{"devel", false, false},
	} {
		if fixed, known := fixedFlake(c.toolchain); fixed != c.fixed || known != c.known {
			t.Errorf("fixedFlake(%q) = %v, %v; want %v, %v", c.toolchain, fixed, known, c.fixed, c.known)
		}
	}
}

// The tool writes into the tree in one place, copyInput, and makes and
// removes only its own temporary directory, in intake. Nothing else calls
// a function that writes, so a later change cannot start writing elsewhere
// without this test saying so.
func TestOnlyCopyInputWritesTheTree(t *testing.T) {
	writes := map[string]bool{
		"WriteFile": true, "OpenFile": true, "Create": true, "MkdirAll": true, "Mkdir": true, "Rename": true,
		"Remove": true, "RemoveAll": true, "Symlink": true, "Link": true, "Chmod": true, "MkdirTemp": true, "CreateTemp": true,
	}
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := s.X.(*ast.Ident); ok && x.Name == "os" && writes[s.Sel.Name] {
					got[fn.Name.Name] = append(got[fn.Name.Name], s.Sel.Name)
				}
			}
			return true
		})
	}
	want := map[string][]string{"copyInput": {"MkdirAll", "OpenFile"}, "intake": {"MkdirTemp", "RemoveAll"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("os calls that write, by function = %v, want %v", got, want)
	}
}

// The root module's go directive moves for vulnerability fixes, and nothing
// else would say this one lagged it, as scripts/corpus's once did.
func TestGoDirectiveMatchesTheRootModule(t *testing.T) {
	directive := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`(?m)^go (\S+)$`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s has no go directive", path)
		}
		return string(m[1])
	}
	if ours, roots := directive("go.mod"), directive("../../go.mod"); ours != roots {
		t.Errorf("scripts/nightly-find/go.mod says go %s; the root go.mod says go %s", ours, roots)
	}
}

// FuzzParseLog: whatever a log holds, reading it ends with a value, and the
// value keeps the shape the body relies on: a block opens on its --- FAIL
// line, an input or a deadline is only ever read from inside one, and a
// verdict is always said.
func FuzzParseLog(f *testing.F) {
	for _, s := range []string{deadlineLog, "--- FAIL: A\n    Failing input written to x\nFAIL\n", "\xef\xbb\xbf", "\x1b[36;1m--- FAIL: \x1b[0m\n", ""} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		l := parseLog(raw)
		if l.failure != nil && !strings.HasPrefix(l.failure[0], "--- FAIL: ") {
			t.Fatalf("a block opens on %q", l.failure[0])
		}
		if (l.input != "" || l.deadline || l.hung) && l.failure == nil {
			t.Fatalf("%+v read from outside a block", l)
		}
		for _, line := range l.failure {
			if strings.ContainsAny(line, "\n") {
				t.Fatalf("a block line holds a newline: %q", line)
			}
		}
		if l.verdict() == "" {
			t.Fatal("no verdict")
		}
	})
}

// FuzzParseRun: a run read from anything is a run's number.
func FuzzParseRun(f *testing.F) {
	for _, s := range []string{runID, runURL0, "", "runs/1/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		id, ok := parseRun(s)
		if ok != (id != "") || strings.Trim(id, "0123456789") != "" {
			t.Fatalf("parseRun(%q) = %q, %v", s, id, ok)
		}
	})
}
