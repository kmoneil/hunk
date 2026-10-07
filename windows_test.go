//go:build windows

package main

// What only Windows has: volumes that may not be there, 8.3 short names, and
// a link destination that names no volume. Each was found on windows-latest
// by a probe on 2026-10-07.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// An unconfined create on a volume that is not there ends, refused, with
// nothing written. Until 2026-10-07 it never ended: MkdirAll's walk up from
// E:\nope had no stop at E:\, and the probe killed it after ten seconds while a
// dry run of the same patch said exit 0. Run as its own process, killed if it
// has not finished, so a hang is a failure and not a stuck job.
func TestACreateOnAVolumeThatIsNotThereEnds(t *testing.T) {
	missing := ""
	for _, l := range "EFGHIJKLMNOPQRSTUVWXYZ" {
		if _, err := os.Stat(string(l) + `:\`); err != nil {
			missing = string(l)
			break
		}
	}
	if missing == "" {
		t.Skip("every drive letter is in use")
	}
	exe, err := os.Executable()
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--allow-outside-root")
	cmd.Env = append(os.Environ(), "HUNK_TEST_AS_HUNK=1")
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader("@@ create " + missing + `:\nope\x.txt` + "\nx\n")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("still running after 20s: a create on %s: never ends", missing)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != exitIO {
		t.Errorf("err %v, want exit %d: %s", err, exitIO, out)
	}
	if !strings.Contains(string(out), missing+`:\nope`) {
		t.Errorf("output does not name the directory it could not make:\n%s", out)
	}
}

// One file edited through its long name and its 8.3 alias is one file: both
// edits land, and the report counts one file. The probe saw "2 files" at exit
// 0 and the first edit gone.
func TestAShortNameIsTheLongName(t *testing.T) {
	root := cliTree(t, map[string]string{"a long directory name/f.txt": "one\ntwo\n"})
	long := filepath.Join(root, "a long directory name")
	short := shortPathName(t, long)
	if filepath.Base(short) == filepath.Base(long) {
		t.Skip("this volume makes no 8.3 names")
	}
	alias := filepath.Base(short)
	patch := "@@ file a long directory name/f.txt\n@@ old\none\n@@ new\nONE\n" +
		"@@ file " + alias + "/f.txt\n@@ old\ntwo\n@@ new\nTWO\n"
	code, out, errOut := runCLI(t, root, nil, patch)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if got := readFile(t, long, "f.txt"); got != "ONE\nTWO\n" {
		t.Errorf("f.txt = %q, want both edits", got)
	}
	if !strings.Contains(out, "1 file, 2 hunks") {
		t.Errorf("report = %q, want one file", out)
	}
}

// Unconfined, a link destination that names no volume is on the link's own
// volume, as Windows reads it, whichever volume the root is on: here the link
// is on the workspace's volume and the root on the temporary directory's.
// Until 2026-10-07 it was spliced as a relative path, naming a file under the
// link's own directory.
func TestARootedDestinationIsOnTheLinksVolume(t *testing.T) {
	ws := os.Getenv("GITHUB_WORKSPACE")
	root := t.TempDir()
	if ws == "" || strings.EqualFold(filepath.VolumeName(ws), filepath.VolumeName(root)) {
		t.Skip("needs a second volume, as the windows-latest runner's workspace is")
	}
	far, err := os.MkdirTemp(ws, "rooted-")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(far) })
	must(t, os.MkdirAll(filepath.Join(far, "target"), 0o755))
	must(t, os.WriteFile(filepath.Join(far, "target", "f.txt"), []byte("far\n"), 0o644))
	link := filepath.Join(far, "rooted")
	must(t, os.Symlink(far[len(filepath.VolumeName(far)):]+`\target`, link))

	tree, err := OpenTree(root, true)
	must(t, err)
	defer tree.Close()
	tg, err := tree.Resolve(filepath.Join(link, "f.txt"))
	must(t, err)
	if want := filepath.Join(far, "target", "f.txt"); !strings.EqualFold(tg.name, want) {
		t.Errorf("name = %q, want %q", tg.name, want)
	}
	kernel, err := os.Stat(filepath.Join(link, "f.txt"))
	must(t, err)
	got, err := os.Stat(tg.name)
	must(t, err)
	if !os.SameFile(kernel, got) {
		t.Errorf("%s is not the file Windows opens through the link", tg.name)
	}
}

func shortPathName(t *testing.T, p string) string {
	t.Helper()
	from, err := syscall.UTF16PtrFromString(p)
	must(t, err)
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(from, &buf[0], uint32(len(buf)))
	must(t, err)
	return syscall.UTF16ToString(buf[:n])
}
