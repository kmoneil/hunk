package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Makefile is the developer's half of the gate, and these are the claims it
// makes that nothing else would notice it stop making. They run make itself on
// a real filesystem rather than reading the recipe, with one exception that
// says why.

// childEnv is this process's environment for a make or git the suite starts,
// without what an outer git or an outer make put there, and with extra on top.
//
// The pre-commit hook runs this suite, and git runs a hook with GIT_DIR and
// GIT_INDEX_FILE naming the repository being committed to. A git started here
// with those inherited does not act on the temporary repository in its working
// directory: it acts on that one. On 2026-10-06 the first run of these tests
// inside the hook re-initialized the real repository as bare, unset its
// core.hooksPath and staged a temporary tree over a worktree's index. So every
// GIT_ variable goes, not a list of the known ones. MAKEFLAGS and MAKELEVEL go
// because `make check` runs this suite, and an outer make's jobserver and
// flags are not this test's.
func childEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name := strings.SplitN(kv, "=", 2)[0]
		if strings.HasPrefix(name, "GIT_") || name == "MAKEFLAGS" || name == "MAKELEVEL" || name == "MFLAGS" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func TestChildEnvDropsWhatAnOuterGitSet(t *testing.T) {
	t.Setenv("GIT_DIR", "/outer/.git")
	t.Setenv("GIT_INDEX_FILE", "/outer/.git/index")
	t.Setenv("MAKEFLAGS", "-j8")
	env := childEnv("GIT_CONFIG_NOSYSTEM=1")
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_INDEX_FILE=") || strings.HasPrefix(kv, "MAKEFLAGS=") {
			t.Errorf("passed %q through", kv)
		}
	}
	if env[len(env)-1] != "GIT_CONFIG_NOSYSTEM=1" {
		t.Errorf("dropped the extra the caller asked for: %v", env[len(env)-1])
	}
}

// runMake runs a target of this repository's Makefile with extra variables and
// environment.
func runMake(t *testing.T, env []string, args ...string) (code int, out string) {
	t.Helper()
	needsMake(t)
	cmd := exec.Command("make", append([]string{"--no-print-directory"}, args...)...)
	cmd.Env = childEnv(env...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, b.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), b.String()
	default:
		t.Fatalf("make %v: %v", args, err)
		return 0, ""
	}
}

// fakeGo writes a stand-in for the go command that `make size` can be pointed
// at with GO=. It handles `build -o PATH .`, appends PATH to a log, and then
// does what mode says: writes the bytes it is given, makes a directory where
// the binary should be, or fails.
func fakeGo(t *testing.T) (prog, log string) {
	t.Helper()
	dir := t.TempDir()
	prog, log = filepath.Join(dir, "go"), filepath.Join(dir, "log")
	must(t, os.WriteFile(prog, []byte(`#!/bin/sh
echo "$3" >> "$FAKEGO_LOG"
case "$FAKEGO_MODE" in
fail) exit 1 ;;
dir) mkdir -p "$3" ;;
*) printf '%s' "$FAKEGO_BYTES" > "$3" ;;
esac
`), 0o755))
	return prog, log
}

// `make size` builds into a directory of its own, removes it however the recipe
// ends, and fails when any step does. Until 2026-10-06 it built to
// /tmp/hunk-size, a fixed name in a directory every user can write, and a size
// it could not read was printed as empty and passed: the "dir" row exited 0.
func TestMakeSize(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		bytes  string
		budget string
		code   bool // non-zero exit wanted
		want   string
		absent string
	}{
		{name: "under budget", bytes: "12345", budget: "5", want: "5 bytes, budget 5"},
		{name: "over budget", bytes: "123456", budget: "5", code: true, want: "binary is 6 bytes, over the 5 budget"},
		{name: "build fails", mode: "fail", budget: "5", code: true, absent: "bytes"},
		{name: "build leaves a directory", mode: "dir", budget: "5", code: true, absent: "budget 5"},
	}
	prog, log := fakeGo(t)
	var seen []string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			must(t, os.WriteFile(log, nil, 0o644))
			code, out := runMake(t,
				[]string{"TMPDIR=" + tmp, "FAKEGO_LOG=" + log, "FAKEGO_MODE=" + tt.mode, "FAKEGO_BYTES=" + tt.bytes},
				"size", "GO="+prog, "SIZE_BUDGET_BYTES="+tt.budget)
			flat := strings.Join(strings.Fields(out), " ")
			if (code != 0) != tt.code {
				t.Errorf("exit %d, want non-zero %v; output:\n%s", code, tt.code, out)
			}
			if tt.want != "" && !strings.Contains(flat, tt.want) {
				t.Errorf("output %q does not say %q", flat, tt.want)
			}
			if tt.absent != "" && strings.Contains(flat, tt.absent) {
				t.Errorf("output %q says %q, which a failed step must not reach", flat, tt.absent)
			}

			b, err := os.ReadFile(log)
			must(t, err)
			built := strings.TrimSpace(string(b))
			// A fresh directory under TMPDIR, never TMPDIR itself and never a
			// fixed name somebody else could have put something at first.
			if filepath.Dir(filepath.Dir(built)) != tmp {
				t.Errorf("built to %q, want a fresh directory under %q", built, tmp)
			}
			seen = append(seen, built)
			left, err := os.ReadDir(tmp)
			must(t, err)
			if len(left) != 0 {
				t.Errorf("left %d entries in TMPDIR, the first %q", len(left), left[0].Name())
			}
		})
	}
	// Every row got its own TMPDIR, so this compares the directory under it.
	names := map[string]bool{}
	for _, s := range seen {
		names[filepath.Base(filepath.Dir(s))] = true
	}
	if len(names) < 2 && len(seen) > 1 {
		t.Errorf("every run built into a directory named %v; want a fresh name each time", names)
	}
}

// No recipe names a fixed path under /tmp. TestMakeSize proves the one target
// that had one; this is the rule for the next target, and it reads the text
// because the claim is about every recipe, including ones no test runs.
func TestMakefileNamesNoFixedTempPath(t *testing.T) {
	b, err := os.ReadFile("Makefile")
	must(t, err)
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "\t") && strings.Contains(line, "/tmp/") {
			t.Errorf("Makefile:%d: a recipe names a fixed path under /tmp; use mktemp -d: %s", i+1, strings.TrimSpace(line))
		}
	}
}

// gitEnv isolates a git command from the machine's own configuration, so that
// a global core.hooksPath or a template directory cannot decide the result, and
// stops it looking for a repository above home, so that a git that finds none
// where a test expected one fails rather than finding somebody else's.
func gitEnv(home string) []string {
	return []string{
		"HOME=" + home,
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(home),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	}
}

func git(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, childEnv(env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// markerHook is a hook that records that it ran and does nothing else.
func markerHook(marks, name string) string {
	return "#!/bin/sh\ntouch '" + filepath.Join(marks, name) + "'\n"
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o755))
	}
}

// `make hooks` enables exactly the two hooks it names, from the checkout they
// fire in, and nothing a branch carries can add to them. Until 2026-10-06 it set
// core.hooksPath to .githooks, so checking out a branch ran that branch's
// post-checkout, and switching or merging ran its reference-transaction and
// post-merge. The clone here starts on that old setting, as both clones made
// before then do, and the target has to take it away.
//
// The hooks in the tree are markers rather than the real ones: the subject is
// the recipe, and the real gate cannot run in a repository with nothing in it.
func TestMakeHooks(t *testing.T) {
	needsMake(t)
	base := t.TempDir()
	env := gitEnv(base)
	marks := filepath.Join(base, "marks")
	must(t, os.Mkdir(marks, 0o755))
	mk, err := os.ReadFile("Makefile")
	must(t, err)

	up := filepath.Join(base, "upstream")
	writeTree(t, up, map[string]string{
		"Makefile":             string(mk),
		".githooks/pre-commit": markerHook(marks, "pre-commit"),
		".githooks/commit-msg": markerHook(marks, "commit-msg"),
	})
	git(t, env, up, "init", "-q", "-b", "main")
	git(t, env, up, "add", "-A")
	git(t, env, up, "commit", "-q", "-m", "main")
	git(t, env, up, "switch", "-q", "-c", "other")
	unwanted := []string{"post-checkout", "reference-transaction", "post-merge"}
	for _, h := range unwanted {
		writeTree(t, up, map[string]string{".githooks/" + h: markerHook(marks, h)})
	}
	git(t, env, up, "add", "-A")
	git(t, env, up, "commit", "-q", "-m", "other")
	git(t, env, up, "switch", "-q", "main")

	clone := filepath.Join(base, "clone")
	git(t, env, base, "clone", "-q", up, clone)
	git(t, env, clone, "config", "core.hooksPath", ".githooks")

	code, out := runMake(t, env, "-C", clone, "hooks")
	if code != 0 {
		t.Fatalf("make hooks: exit %d\n%s", code, out)
	}
	for _, want := range []string{"hooks: removed core.hooksPath", "hooks: commit-msg pre-commit installed in"} {
		if !strings.Contains(out, want) {
			t.Errorf("make hooks said %q, want it to say %q", out, want)
		}
	}
	cmd := exec.Command("git", "config", "--get", "core.hooksPath")
	cmd.Dir, cmd.Env = clone, childEnv(env...)
	if b, err := cmd.Output(); err == nil {
		t.Errorf("core.hooksPath is still %q", strings.TrimSpace(string(b)))
	}

	git(t, env, clone, "switch", "-q", "other")
	git(t, env, clone, "switch", "-q", "main")
	git(t, env, clone, "merge", "-q", "--no-edit", "other")
	for _, h := range unwanted {
		if _, err := os.Stat(filepath.Join(marks, h)); err == nil {
			t.Errorf("the branch's %s ran on checkout or merge", h)
		}
	}

	git(t, env, clone, "commit", "-q", "--allow-empty", "-m", "probe")
	for _, h := range []string{"pre-commit", "commit-msg"} {
		if _, err := os.Stat(filepath.Join(marks, h)); err != nil {
			t.Errorf("%s did not run at commit: %v", h, err)
		}
	}

	// Again, on a clone that has nothing to migrate: nothing to remove.
	code, out = runMake(t, env, "-C", clone, "hooks")
	if code != 0 || strings.Contains(out, "removed") {
		t.Errorf("make hooks a second time: exit %d\n%s", code, out)
	}
}

// A core.hooksPath that make hooks cannot remove, because it is not this
// clone's, would leave the shims unrun. The target says so and fails.
func TestMakeHooksRefusesAHooksPathElsewhere(t *testing.T) {
	needsMake(t)
	base := t.TempDir()
	env := gitEnv(base)
	must(t, os.WriteFile(filepath.Join(base, ".gitconfig"), []byte("[core]\n\thooksPath = /somewhere\n"), 0o644))
	mk, err := os.ReadFile("Makefile")
	must(t, err)
	repo := filepath.Join(base, "repo")
	writeTree(t, repo, map[string]string{"Makefile": string(mk)})
	git(t, env, repo, "init", "-q", "-b", "main")

	code, out := runMake(t, env, "-C", repo, "hooks")
	if code == 0 || !strings.Contains(out, "core.hooksPath is /somewhere in another scope") {
		t.Errorf("exit %d, want a refusal naming /somewhere:\n%s", code, out)
	}
	if strings.Contains(out, "installed") {
		t.Errorf("installed the shims anyway:\n%s", out)
	}
}

// The pre-commit hook is what a clone still on core.hooksPath actually runs, so
// it is where that clone is told. It refuses before the gate, and without the
// setting it goes on to the gate.
func TestPreCommitRefusesCoreHooksPath(t *testing.T) {
	needsMake(t)
	hook, err := filepath.Abs(filepath.Join(".githooks", "pre-commit"))
	must(t, err)
	base := t.TempDir()
	env := gitEnv(base)
	repo := filepath.Join(base, "repo")
	must(t, os.Mkdir(repo, 0o755))
	git(t, env, repo, "init", "-q", "-b", "main")

	run := func() (int, string) {
		cmd := exec.Command("sh", hook)
		cmd.Dir, cmd.Env = repo, childEnv(env...)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), string(out)
		}
		must(t, err)
		return 0, string(out)
	}

	git(t, env, repo, "config", "core.hooksPath", ".githooks")
	code, out := run()
	if code == 0 || !strings.Contains(out, "core.hooksPath is .githooks") || !strings.Contains(out, "run: make hooks") {
		t.Errorf("with core.hooksPath set: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "pre-commit: gofumpt") {
		t.Errorf("reached the gate before refusing:\n%s", out)
	}

	git(t, env, repo, "config", "--unset", "core.hooksPath")
	if _, out := run(); !strings.Contains(out, "pre-commit: gofumpt") || strings.Contains(out, "run: make hooks") {
		t.Errorf("without core.hooksPath it did not go on to the gate:\n%s", out)
	}
}
