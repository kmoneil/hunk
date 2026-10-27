package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lockAt opens dir as a confined tree and takes its lock, writing or not. What
// it took is let go when the test ends, if the test has not let it go first.
func lockAt(t *testing.T, dir string, writes bool) (release func(), err error) {
	t.Helper()
	tree, err := OpenTree(dir, false)
	must(t, err)
	t.Cleanup(func() { _ = tree.Close() })
	return lockTreeFor(t, tree, writes)
}

func lockTreeFor(t *testing.T, tree *Tree, writes bool) (func(), error) {
	t.Helper()
	release, err := lockTree(tree, writes)
	if err == nil {
		t.Cleanup(release)
	}
	return release, err
}

// lockBase is a directory with a tree in it, r, and directories in and beside
// it, every name resolved so that a refusal's Dir compares as text.
func lockBase(t *testing.T) string {
	t.Helper()
	base := cliTree(t, nil)
	for _, d := range []string{"r/sub/deep", "r/a", "r/b"} {
		must(t, os.MkdirAll(filepath.Join(base, d), 0o755))
	}
	return base
}

// assertBusy fails unless err is the lock's refusal naming dir, as the root
// or as a directory above it.
func assertBusy(t *testing.T, err error, dir string, above bool) {
	t.Helper()
	var tb *TreeBusyError
	if !errors.As(err, &tb) {
		t.Fatalf("got %v, want a refusal naming %s", err, dir)
	}
	if tb.Dir != dir || tb.Above != above {
		t.Errorf("refused at %s (above: %v), want %s (above: %v)", tb.Dir, tb.Above, dir, above)
	}
	if ExitCode(err) != exitChanged {
		t.Errorf("exit %d, want %d", ExitCode(err), exitChanged)
	}
}

// Who refuses whom, decided 2026-10-27 for #128: a run that writes takes its
// root exclusively and a dry run shares it, and each takes every directory
// above its root shared. So two runs refuse each other when one's tree holds
// the other's and either writes, and never otherwise, with one exception
// stated in README: a dry run rooted above a writer shares the directory they
// meet at and is let through.
func TestTheTreeLock(t *testing.T) {
	needsTreeLock(t)
	for _, c := range []struct {
		name          string
		first, second string // roots under the base
		firstWrites   bool
		secondWrites  bool
		refusedAt     string // under the base; "" when the second is let through
		above         bool
	}{
		{"two runs that write, one tree", "r", "r", true, true, "r", false},
		{"a dry run while a run writes", "r", "r", true, false, "r", false},
		{"a run that writes while a dry run reads", "r", "r", false, true, "r", false},
		{"two dry runs", "r", "r", false, false, "", false},
		{"a run below one that writes", "r", "r/sub", true, true, "r", true},
		{"a dry run below one that writes", "r", "r/sub", true, false, "r", true},
		{"two directories below one that writes", "r", "r/sub/deep", true, true, "r", true},
		{"a run that writes above one that writes", "r/sub", "r", true, true, "r", false},
		{"a run that writes above a dry run", "r/sub", "r", false, true, "r", false},
		{"a dry run above one that writes, the exception", "r/sub", "r", true, false, "", false},
		{"two trees side by side", "r/a", "r/b", true, true, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := lockBase(t)
			if _, err := lockAt(t, filepath.Join(base, c.first), c.firstWrites); err != nil {
				t.Fatalf("the first run was refused: %v", err)
			}
			_, err := lockAt(t, filepath.Join(base, c.second), c.secondWrites)
			if c.refusedAt == "" {
				if err != nil {
					t.Fatalf("the second run was refused: %v", err)
				}
				return
			}
			assertBusy(t, err, filepath.Join(base, c.refusedAt), c.above)
		})
	}
}

// A run that ends lets its tree go, and so does one that was refused, which
// must not keep what it took on the way to the refusal.
func TestTheTreeLockIsLetGo(t *testing.T) {
	needsTreeLock(t)
	t.Run("by the run that held it", func(t *testing.T) {
		base := lockBase(t)
		release, err := lockAt(t, filepath.Join(base, "r"), true)
		must(t, err)
		release()
		if _, err := lockAt(t, filepath.Join(base, "r"), true); err != nil {
			t.Fatalf("a run after the first ended was refused: %v", err)
		}
	})
	t.Run("by the run that was refused", func(t *testing.T) {
		// The refused run took r/sub/deep, and r/sub above it, before it met
		// r. Kept, they would refuse the next run at r/sub/deep once r is free.
		base := lockBase(t)
		release, err := lockAt(t, filepath.Join(base, "r"), true)
		must(t, err)
		_, err = lockAt(t, filepath.Join(base, "r/sub/deep"), true)
		assertBusy(t, err, filepath.Join(base, "r"), true)
		release()
		if _, err := lockAt(t, filepath.Join(base, "r/sub/deep"), true); err != nil {
			t.Fatalf("the refused run kept what it took: %v", err)
		}
	})
}

// What the lock skips rather than refuses, each one decided with it: a
// directory above that the invoker does not own or cannot open, a filesystem
// with no flock(2), and a root that is not there. And what it does not skip:
// a flock(2) interrupted by a signal is asked again.
func TestTheTreeLockSkipsWhatItCannotUse(t *testing.T) {
	needsTreeLock(t)
	t.Run("a directory above that another user owns", func(t *testing.T) {
		// / is shared with every user on the machine, so any of them could
		// hold it and refuse every run beneath. Held here as a tree's root,
		// exclusively, which is how such a user would hold it.
		if os.Getuid() == 0 {
			t.Skip("running as root, which owns /")
		}
		if _, err := lockAt(t, "/", true); err != nil {
			t.Fatalf("/ was refused: %v", err)
		}
		if _, err := lockAt(t, filepath.Join(lockBase(t), "r"), true); err != nil {
			t.Fatalf("a run was refused by a directory the invoker does not own: %v", err)
		}
	})
	t.Run("a directory above that cannot be opened", func(t *testing.T) {
		needsPOSIXPerms(t)
		if os.Getuid() == 0 {
			t.Skip("running as root, which opens any directory")
		}
		base := lockBase(t)
		r := filepath.Join(base, "r")
		_, err := lockAt(t, r, true)
		must(t, err)
		must(t, os.Chmod(r, 0o311))
		t.Cleanup(func() { _ = os.Chmod(r, 0o755) })
		if _, err := lockAt(t, filepath.Join(r, "sub"), true); err != nil {
			t.Fatalf("a run was refused by a directory it cannot open: %v", err)
		}
	})
	t.Run("a filesystem with no flock", func(t *testing.T) {
		base := lockBase(t)
		flock = func(int, int) error { return syscall.ENOTSUP }
		t.Cleanup(func() { flock = sysFlock })
		for range 2 {
			if _, err := lockAt(t, filepath.Join(base, "r"), true); err != nil {
				t.Fatalf("refused where the lock cannot be taken: %v", err)
			}
		}
	})
	t.Run("an interrupted flock is asked again", func(t *testing.T) {
		base := lockBase(t)
		_, err := lockAt(t, filepath.Join(base, "r"), true)
		must(t, err)
		calls := 0
		flock = func(fd, how int) error {
			calls++
			if calls == 1 {
				return syscall.EINTR
			}
			return sysFlock(fd, how)
		}
		t.Cleanup(func() { flock = sysFlock })
		_, err = lockAt(t, filepath.Join(base, "r"), true)
		assertBusy(t, err, filepath.Join(base, "r"), false)
		if calls < 2 {
			t.Errorf("flock was called %d times, so the interrupted call was not asked again", calls)
		}
	})
	t.Run("an unconfined root that is not there", func(t *testing.T) {
		// Nothing to lock at the root, and every directory above it is
		// still taken.
		base := lockBase(t)
		_, err := lockAt(t, filepath.Join(base, "r"), true)
		must(t, err)
		tree, err := OpenTree(filepath.Join(base, "r", "nope"), true)
		must(t, err)
		_, err = lockTreeFor(t, tree, true)
		assertBusy(t, err, filepath.Join(base, "r"), true)
	})
}

// A confined tree is locked through the directory it holds open, which every
// read and write goes through, and not through its name, which can come to
// name another directory between the open and the lock.
func TestTheTreeLockIsOnTheDirectoryTheTreeHolds(t *testing.T) {
	needsTreeLock(t)
	base := lockBase(t)
	r, moved := filepath.Join(base, "r"), filepath.Join(base, "moved")
	tree, err := OpenTree(r, false)
	must(t, err)
	t.Cleanup(func() { _ = tree.Close() })
	must(t, os.Rename(r, moved))
	must(t, os.Mkdir(r, 0o755))
	_, err = lockTreeFor(t, tree, true)
	must(t, err)
	_, err = lockAt(t, moved, true)
	assertBusy(t, err, moved, false)
	if _, err := lockAt(t, r, true); err != nil {
		t.Errorf("the directory now at the tree's name was locked: %v", err)
	}
}

// startHolder runs this test binary as hunk on root, with args and the patch on
// stdin, and returns once its command has started, which it says by making
// .started in the tree. The command then waits for .go. So while the test
// runs, the holder is mid-command with whatever it applied on disk, every
// time, rather than probably.
func startHolder(t *testing.T, root, patch string, args ...string) (cmd *exec.Cmd, out *bytes.Buffer) {
	t.Helper()
	exe, err := os.Executable()
	must(t, err)
	cmd = exec.Command(exe, append([]string{"--root", root}, args...)...)
	cmd.Env = childEnv("HUNK_TEST_AS_HUNK=1")
	cmd.Stdin = strings.NewReader(patch)
	out = &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	must(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, ".started")); err == nil {
			return cmd, out
		}
		if time.Now().After(deadline) {
			t.Fatalf("the holder's command never started:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The holder's command: say it started, then wait to be let go.
const holdCommand = `: > .started; while [ ! -e .go ]; do sleep 0.02; done`

// Issue #128, and every run the lock refuses while another holds the tree.
// The holder is a real hunk process, mid-command, with its batch on disk: a
// --try, a --verify, and a --try --each still in its run on the tree as it is.
// Each run against it is refused at exit 6 with nothing read and nothing
// written, a run on another file of the tree included, which is the case that
// went unreported: its command would have run against the holder's break. And
// once the holder ends, the next run goes through.
func TestARunIsRefusedWhileAnotherHoldsTheTree(t *testing.T) {
	needsTreeLock(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh, which --try and --verify run their command with")
	}
	const (
		x = "@@ file f.txt\n@@ old\na\n@@ new\nA\n"
		y = "@@ file f.txt\n@@ old\nb\n@@ new\nB\n"
		g = "@@ file g.txt\n@@ old\ng\n@@ new\nG\n"
	)
	patches := t.TempDir()
	xFile, yFile := filepath.Join(patches, "x.hunk"), filepath.Join(patches, "y.hunk")
	must(t, os.WriteFile(xFile, []byte(x), 0o644))
	must(t, os.WriteFile(yFile, []byte(y), 0o644))

	for _, h := range []struct {
		name   string
		args   []string
		patch  string
		during string // f.txt while the holder's command runs
		after  string // f.txt once it has ended
	}{
		{"a --try", []string{"--try", holdCommand}, x, "A\nb\n", "a\nb\n"},
		{"a --verify", []string{"--verify", holdCommand}, x, "A\nb\n", "A\nb\n"},
		// The command runs on the tree as it is before any patch, so the
		// lock is seen to be held before the first patch is applied; and
		// the patch is caught, because the command fails on it.
		{"a --try --each", []string{"--try", holdCommand + "; ! grep -q A f.txt", "--each", xFile}, "", "a\nb\n", "a\nb\n"},
	} {
		t.Run("while "+h.name+" runs", func(t *testing.T) {
			parent := cliTree(t, nil)
			root := filepath.Join(parent, "tree")
			for name, body := range map[string]string{"f.txt": "a\nb\n", "g.txt": "g\n", "sub/s.txt": "s\n"} {
				must(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755))
				must(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o644))
			}
			holder, out := startHolder(t, root, h.patch, h.args...)

			for _, r := range []struct {
				name  string
				root  string
				args  []string
				patch string
				dir   string // the refusal names it
				above bool
			}{
				{"a --try of the same file", root, []string{"--try", "true"}, y, root, false},
				{"an edit of another file", root, nil, g, root, false},
				{"a --verify", root, []string{"--verify", "true"}, g, root, false},
				{"a dry run", root, []string{"--dry-run"}, y, root, false},
				// 6 and not 2: the lock comes before the tree is read.
				{"a patch that would not match", root, nil, "@@ file f.txt\n@@ old\nnot there\n@@ new\nx\n", root, false},
				{"a --try --each", root, []string{"--try", "false", "--each", yFile}, "", root, false},
				{"a run rooted below it", filepath.Join(root, "sub"), nil, "@@ file s.txt\n@@ old\ns\n@@ new\nS\n", root, true},
				{"a run rooted above it", parent, nil, "@@ file tree/g.txt\n@@ old\ng\n@@ new\nG\n", parent, false},
			} {
				t.Run(r.name, func(t *testing.T) {
					code, stdout, stderr := runCLI(t, r.root, r.args, r.patch)
					if code != exitChanged {
						t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, exitChanged, stdout, stderr)
					}
					want := "hunk: " + (&TreeBusyError{Dir: r.dir, Above: r.above}).Error() + "\n"
					if stderr != want || stdout != "" {
						t.Errorf("stdout %q\nstderr %q\nwant stderr %q", stdout, stderr, want)
					}
				})
			}
			t.Run("--json", func(t *testing.T) {
				code, stdout, stderr := runCLI(t, root, []string{"--json"}, y)
				var got struct {
					OK    bool   `json:"ok"`
					Exit  int    `json:"exit"`
					Error string `json:"error"`
				}
				if err := json.Unmarshal([]byte(stdout), &got); err != nil {
					t.Fatalf("exit %d, stdout not JSON: %v\n%s\nstderr: %s", code, err, stdout, stderr)
				}
				if code != exitChanged || got.OK || got.Exit != exitChanged ||
					got.Error != (&TreeBusyError{Dir: root}).Error() || stderr != "" {
					t.Errorf("exit %d, %+v, stderr %q", code, got, stderr)
				}
			})
			for name, want := range map[string]string{"f.txt": h.during, "g.txt": "g\n", "sub/s.txt": "s\n"} {
				if got := readFile(t, root, name); got != want {
					t.Errorf("while the holder runs, %s = %q, want %q: a refused run wrote", name, got, want)
				}
			}

			must(t, os.WriteFile(filepath.Join(root, ".go"), nil, 0o644))
			if err := holder.Wait(); err != nil {
				t.Fatalf("the holder: %v\n%s", err, out)
			}
			if got := readFile(t, root, "f.txt"); got != h.after {
				t.Errorf("once the holder ended, f.txt = %q, want %q\n%s", got, h.after, out)
			}
			if code, stdout, stderr := runCLI(t, root, nil, g); code != exitOK {
				t.Errorf("a run once the holder ended: exit %d\n%s%s", code, stdout, stderr)
			}
		})
	}
}

// A dry run shares the tree, through the CLI: TestTheTreeLock asks lockTree
// directly, so it cannot see which lock invoke asks for. Another dry run's lock
// is held here as invoke would take it; a dry run goes through, and a --dry-run
// --try with it, which writes nothing either. A run that writes is refused.
// Found by a mutant that had every dry run take the tree exclusively and failed
// nothing.
func TestADryRunSharesTheTree(t *testing.T) {
	needsTreeLock(t)
	root := cliTree(t, map[string]string{"f.txt": "a\n"})
	_, err := lockAt(t, root, false)
	must(t, err)
	const p = "@@ file f.txt\n@@ old\na\n@@ new\nA\n"
	for _, c := range []struct {
		name string
		args []string
		code int
	}{
		{"a dry run", []string{"--dry-run"}, exitOK},
		{"a dry run of a --try", []string{"--dry-run", "--try", "true"}, exitOK},
		{"a run that writes", nil, exitChanged},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code, stdout, stderr := runCLI(t, root, c.args, p); code != c.code {
				t.Errorf("exit %d, want %d\n%s%s", code, c.code, stdout, stderr)
			}
			if got := readFile(t, root, "f.txt"); got != "a\n" {
				t.Errorf("f.txt = %q", got)
			}
		})
	}
}

// The refusal's two wordings, the root's and a directory above it, as an
// agent reads them. §8.1: a reworded diagnostic is a contract change.
func TestTheTreeLockRefusalReads(t *testing.T) {
	golden(t, "tree-busy",
		"hunk: "+(&TreeBusyError{Dir: "/work/repo"}).Error()+"\n"+
			"hunk: "+(&TreeBusyError{Dir: "/work", Above: true}).Error()+"\n")
}

// A hunk killed by SIGKILL holds nothing, even while the command it started
// runs on: the command is in a process group of its own and outlives it, and
// it was not handed the lock, because every file hunk opens is closed on exec.
// So the next run goes through, against the batch the killed run left applied,
// which is §6.2's crash gap and not the lock's.
func TestAKilledRunHoldsNoLock(t *testing.T) {
	needsTreeLock(t)
	needsSignals(t)
	if !pidsFromShAreReal() {
		t.Skip("sh's process IDs name no process here")
	}
	root := cliTree(t, map[string]string{"f.txt": "a\nb\n"})
	holder, _ := startHolder(t, root, "@@ file f.txt\n@@ old\na\n@@ new\nA\n",
		"--try", `echo $$ > .pid; : > .started; exec sleep 60`)
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, root, ".pid")))
	must(t, err)
	command, err := os.FindProcess(pid)
	must(t, err)
	t.Cleanup(func() { _ = command.Kill() })

	must(t, holder.Process.Signal(syscall.SIGKILL))
	_ = holder.Wait()
	if err := command.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the command did not outlive hunk, so this shows nothing: %v", err)
	}
	code, stdout, stderr := runCLI(t, root, nil, "@@ file f.txt\n@@ old\nb\n@@ new\nB\n")
	if code != exitOK {
		t.Fatalf("exit %d with the killed run's command still running\n%s%s", code, stdout, stderr)
	}
	if got := readFile(t, root, "f.txt"); got != "A\nB\n" {
		t.Errorf("f.txt = %q", got)
	}
}
