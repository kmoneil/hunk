//go:build windows

package main

// A probe, never merged: it asks windows-latest what hunk has only reasoned
// about, logs every answer, and fails on purpose so CI prints the log.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWindowsProbe(t *testing.T) {
	var b strings.Builder
	say := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	exe, err := os.Executable()
	must(t, err)
	wd, _ := os.Getwd()
	say("cwd %s; TMP %s; TempDir %s", wd, os.Getenv("TMP"), os.TempDir())
	if r, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		say("EvalSymlinks(TempDir) = %s", r)
	} else {
		say("EvalSymlinks(TempDir): %v", err)
	}

	say("\n== 1. a create on a volume that is not there, unconfined")
	var missing string
	var present []string
	for _, l := range "DEFGHIJKLMNOPQRSTUVWXYZ" {
		if _, err := os.Stat(string(l) + `:\`); err == nil {
			present = append(present, string(l))
		} else if missing == "" {
			missing = string(l)
		}
	}
	say("volumes present: %v; using %s:", present, missing)
	runHunk := func(args []string, patch string, dir string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Env = append(os.Environ(), "HUNK_TEST_AS_HUNK=1")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(patch)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		took := time.Since(start).Round(10 * time.Millisecond)
		if ctx.Err() != nil {
			return fmt.Sprintf("KILLED after %s (still running): %q", took, out)
		}
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		return fmt.Sprintf("exit %d in %s: %q", code, took, out)
	}
	tmp := t.TempDir()
	create := "@@ create " + missing + `:\nope\x.txt` + "\nx\n"
	say("unconfined create:         %s", runHunk([]string{"--allow-outside-root"}, create, tmp))
	say("unconfined create dry run: %s", runHunk([]string{"--allow-outside-root", "--dry-run"}, create, tmp))
	say("confined create:           %s", runHunk(nil, create, tmp))
	say("unconfined replace:        %s", runHunk([]string{"--allow-outside-root"},
		"@@ file "+missing+`:\nope\x.txt`+"\n@@ old\nx\n@@ new\ny\n", tmp))

	say("\n== 2. a destination with no volume")
	ws := os.Getenv("GITHUB_WORKSPACE")
	say("GITHUB_WORKSPACE %s", ws)
	cTree := t.TempDir()
	if r, err := filepath.EvalSymlinks(cTree); err == nil {
		cTree = r
	}
	must(t, os.MkdirAll(filepath.Join(cTree, "target"), 0o755))
	must(t, os.WriteFile(filepath.Join(cTree, "target", "f.txt"), []byte("on "+filepath.VolumeName(cTree)+"\n"), 0o644))
	rootedDest := cTree[len(filepath.VolumeName(cTree)):] + `\target`
	link := filepath.Join(cTree, "rooted")
	if err := os.Symlink(rootedDest, link); err != nil {
		say("Symlink(%q): %v", rootedDest, err)
	} else {
		rl, rlErr := os.Readlink(link)
		say("link on %s -> %q; Readlink = %q %v", filepath.VolumeName(cTree), rootedDest, rl, rlErr)
		for _, from := range []string{cTree, wd} {
			cmd := exec.Command("cmd", "/c", "type", filepath.Join(link, "f.txt"))
			cmd.Dir = from
			out, err := cmd.CombinedOutput()
			say("  type through it, cwd on %s: %q %v", filepath.VolumeName(from), out, err)
		}
		if b, err := os.ReadFile(filepath.Join(link, "f.txt")); err == nil {
			say("  os.ReadFile through it, cwd on %s: %q", filepath.VolumeName(wd), b)
		} else {
			say("  os.ReadFile through it: %v", err)
		}
		for _, unconfin := range []bool{false, true} {
			tree, err := OpenTree(cTree, unconfin)
			must(t, err)
			tg, err := tree.Resolve(`rooted\f.txt`)
			say("  Resolve unconfined=%v: %q %v", unconfin, tg.name, err)
			tree.Close()
		}
	}
	if ws != "" {
		dTree := filepath.Join(ws, "probe-d")
		must(t, os.MkdirAll(filepath.Join(dTree, "target"), 0o755))
		must(t, os.WriteFile(filepath.Join(dTree, "target", "f.txt"), []byte("on "+filepath.VolumeName(dTree)+"\n"), 0o644))
		// A link on D: whose destination names no volume and exists only on C:.
		dLink := filepath.Join(dTree, "rooted-to-c")
		if err := os.Symlink(rootedDest, dLink); err != nil {
			say("Symlink on D: %v", err)
		} else {
			b, err := os.ReadFile(filepath.Join(dLink, "f.txt"))
			say("link on %s -> %q (which exists only on %s): ReadFile %q %v",
				filepath.VolumeName(dTree), rootedDest, filepath.VolumeName(cTree), b, err)
			cmd := exec.Command("cmd", "/c", "type", filepath.Join(dLink, "f.txt"))
			cmd.Dir = cTree
			out, err := cmd.CombinedOutput()
			say("  type through it with cwd on %s: %q %v", filepath.VolumeName(cTree), out, err)
		}
	}

	say("\n== 3. 8.3 short names")
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	long := filepath.Join(root, "a long directory name")
	must(t, os.MkdirAll(long, 0o755))
	must(t, os.WriteFile(filepath.Join(long, "f.txt"), []byte("one\ntwo\n"), 0o644))
	short := shortPath(long)
	say("long %s; short %s", long, short)
	out, _ := exec.Command("fsutil", "8dot3name", "query", filepath.VolumeName(root)).CombinedOutput()
	say("fsutil 8dot3name query %s: %q", filepath.VolumeName(root), strings.TrimSpace(string(out)))
	if short != "" && short != long {
		sc := filepath.Base(short)
		say("short component %q", sc)
		tree, err := OpenTree(root, false)
		must(t, err)
		sp := tree.Spellings()
		for _, p := range []string{`a long directory name\f.txt`, sc + `\f.txt`, strings.ToLower(sc) + `\f.txt`} {
			tg, err := tree.Resolve(p)
			if err != nil {
				say("  Resolve %q: %v", p, err)
				continue
			}
			say("  Resolve %q = %q; Respell = %q", p, tg.name, sp.Respell(tg).name)
		}
		tree.Close()
		patch := "@@ file a long directory name/f.txt\n@@ old\none\n@@ new\nONE\n" +
			"@@ file " + sc + "/f.txt\n@@ old\ntwo\n@@ new\nTWO\n"
		code, so, se := runCLI(t, root, nil, patch)
		got, _ := os.ReadFile(filepath.Join(long, "f.txt"))
		say("  batch editing one file under both spellings: exit %d %q %q; file now %q", code, so, se, got)
		// The root given by its short name, a path through the long one.
		shortRoot := shortPath(root)
		say("  root short %s", shortRoot)
		if shortRoot != "" {
			tree, err := OpenTree(shortRoot, false)
			if err == nil {
				say("  OpenTree(short).Root() = %s; Shown() = %s", tree.Root(), tree.Shown())
				tg, err := tree.Resolve(filepath.Join(root, "a long directory name", "f.txt"))
				say("  Resolve(long absolute) under the short root: %q %v", tg.name, err)
				tree.Close()
			}
		}
	}

	say("\n== 4. a .. in a link, final and in a parent")
	dd := t.TempDir()
	if r, err := filepath.EvalSymlinks(dd); err == nil {
		dd = r
	}
	dotDotTree(t, dd)
	for name, dest := range dotDotLinks {
		if strings.HasPrefix(dest, "/") {
			dest = dd + dest
		}
		dest = strings.NewReplacer("{root}", dd, "{base}", filepath.Base(dd), "{parent}", filepath.Dir(dd)).Replace(dest)
		if err := os.Symlink(native(dest), filepath.Join(dd, name)); err != nil {
			say("Symlink %s: %v", name, err)
		}
	}
	// Directory links with a .. in them, for the parent position.
	for _, l := range [][2]string{{"pd.link", `x\..`}, {"pu.link", `x\..\..`}, {"pdd.link", `d\..\sub`}} {
		if err := os.Symlink(l[1], filepath.Join(dd, l[0])); err != nil {
			say("Symlink %s: %v", l[0], err)
		}
	}
	content := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return "error: " + err.Error()
		}
		return strings.TrimSpace(string(b))
	}
	resolve := func(unconfin bool, p string) string {
		tree, err := OpenTree(dd, unconfin)
		must(t, err)
		defer tree.Close()
		tg, err := tree.Resolve(p)
		if err != nil {
			var pr *PathRefusal
			if errors.As(err, &pr) {
				return "refused: " + pr.Reason
			}
			return "error: " + err.Error()
		}
		at := tg.name
		if !filepath.IsAbs(at) {
			at = filepath.Join(dd, at)
		}
		return fmt.Sprintf("%s (%s)", tg.name, content(at))
	}
	names := slices.Sorted(maps.Keys(dotDotLinks))
	names = append(names, "pd.link", "pu.link", "pdd.link")
	for _, name := range names {
		rl, _ := os.Readlink(filepath.Join(dd, name))
		paths := []string{name}
		if strings.HasPrefix(name, "p") {
			paths = []string{name + `\c.txt`}
		}
		for _, p := range paths {
			say("%s -> %q\n    kernel: %s\n    confined: %s\n    unconfined: %s",
				p, rl, content(filepath.Join(dd, p)), resolve(false, p), resolve(true, p))
		}
	}

	t.Errorf("PROBE RESULTS (this test fails on purpose so the log prints)\n%s", b.String())
}

func shortPath(p string) string {
	from, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 1024)
	n, err := syscall.GetShortPathName(from, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}
