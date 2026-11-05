package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -f - is stdin, as no -f is (#142). Until 2026-11-05 "-" was a file name, so
// "hunk -f - <<'HUNK'" was refused as "open -: no such file or directory".

func TestPatchFromStdin(t *testing.T) {
	for _, c := range []struct {
		file string
		want bool
	}{
		{"", true},
		{"-", true},
		{"./-", false},
		{"--", false},
		{" -", false},
		{"- ", false},
		{"p.hunk", false},
		{"/dev/stdin", false}, // a path, read as one
	} {
		if got := patchFromStdin(c.file); got != c.want {
			t.Errorf("patchFromStdin(%q) = %v, want %v", c.file, got, c.want)
		}
	}
}

// Every stdin behaviour, run twice: with no -f and with -f -. The two runs
// have to agree on the exit, on both streams and on the tree, and each case
// names what that agreement is, so that two runs which agree on the wrong
// thing still fail.
func TestDashFReadsStdinAsNoFDoes(t *testing.T) {
	const applies = "@@ file f.txt\n@@ old\na\n@@ new\nb\n"
	for _, c := range []struct {
		name, patch string
		args        []string
		code        int
		says        string // in stdout or stderr
		after       string // f.txt afterwards
	}{
		{"a patch that applies", applies, nil, exitOK, "M f.txt +1 -1", "b\n"},
		{"under --dry-run", applies, []string{"--dry-run"}, exitOK, "dry run: nothing written", "a\n"},
		{"under --json", applies, []string{"--json"}, exitOK, `"exit": 0`, "b\n"},
		{"a near miss", "@@ file f.txt\n@@ old\nz\n@@ new\nb\n", nil, exitNoMatch, "expected 1 occurrence, found 0", "a\n"},
		{"an empty patch", "   \n", nil, exitUsage, "hunk: the patch is empty", "a\n"},
		{"a first line that is not a directive, with the heredoc hint", "import json\n", nil, exitUsage, heredocHintText, "a\n"},
		{"a mistyped delimiter", "@@ file f.txt\n@@ old\na\n@@ new\nb\nHUHUNK\n", nil, exitUsage, "looks like a mistyped heredoc delimiter", "a\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			type run struct {
				code     int
				out, err string
				after    string
			}
			do := func(args []string) run {
				root := cliTree(t, map[string]string{"f.txt": "a\n"})
				code, out, errOut := runCLI(t, root, append(c.args, args...), c.patch)
				b, err := os.ReadFile(filepath.Join(root, "f.txt"))
				must(t, err)
				return run{code, strings.ReplaceAll(out, root, "ROOT"), strings.ReplaceAll(errOut, root, "ROOT"), string(b)}
			}
			noF, dashF := do(nil), do([]string{"-f", "-"})
			if dashF != noF {
				t.Errorf("-f - and no -f differ\n-f -:  %+v\nno -f: %+v", dashF, noF)
			}
			if dashF.code != c.code {
				t.Errorf("-f -: exit %d, want %d; stderr %q", dashF.code, c.code, dashF.err)
			}
			if !strings.Contains(dashF.out+dashF.err, c.says) {
				t.Errorf("-f -: output does not say %q\nstdout %q\nstderr %q", c.says, dashF.out, dashF.err)
			}
			if dashF.after != c.after {
				t.Errorf("-f -: f.txt = %q, want %q", dashF.after, c.after)
			}
		})
	}
}

// A file named "-" in the working directory, which -f is relative to, is not
// what -f - reads, even when stdin holds nothing; -f ./- reads it.
func TestDashFIsNotAFileNamedDash(t *testing.T) {
	cwd := t.TempDir()
	must(t, os.WriteFile(filepath.Join(cwd, "-"), []byte("@@ file f.txt\n@@ old\na\n@@ new\nfrom the file\n"), 0o644))
	t.Chdir(cwd)
	const fromStdin = "@@ file f.txt\n@@ old\na\n@@ new\nfrom stdin\n"
	for _, c := range []struct {
		name  string
		f     string
		stdin string
		code  int
		says  string
		after string
	}{
		{"-f - reads stdin", "-", fromStdin, exitOK, "M f.txt +1 -1", "from stdin\n"},
		{"-f - with nothing on stdin is an empty patch, not the file", "-", "", exitUsage, "hunk: the patch is empty", "a\n"},
		{"-f ./- reads the file", "./-", fromStdin, exitOK, "M f.txt +1 -1", "from the file\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "a\n"})
			code, out, errOut := runCLI(t, root, []string{"-f", c.f}, c.stdin)
			if code != c.code {
				t.Errorf("exit %d, want %d; stderr %q", code, c.code, errOut)
			}
			if !strings.Contains(out+errOut, c.says) {
				t.Errorf("output does not say %q\nstdout %q\nstderr %q", c.says, out, errOut)
			}
			b, err := os.ReadFile(filepath.Join(root, "f.txt"))
			must(t, err)
			if string(b) != c.after {
				t.Errorf("f.txt = %q, want %q", b, c.after)
			}
		})
	}
}

// /dev/null is a character device, which reaches the branch a terminal does:
// -f - from a prompt is the same wait as no -f, and refused the same way.
func TestDashFOnACharacterDeviceIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"hunk", []string{"--root", t.TempDir(), "-f", "-"}, "hunk: no patch: give one on stdin or with -f. Run \"hunk format\" for the grammar\n"},
		{"hunk invert", []string{"invert", "-f", "-"}, "hunk invert: no patch: give one on stdin or with -f. Run \"hunk format\" for the grammar\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			devnull, err := os.Open(os.DevNull)
			if err != nil {
				t.Skip(err)
			}
			defer devnull.Close()
			var out, errOut bytes.Buffer
			if code := cli(c.args, devnull, &out, &errOut); code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if errOut.String() != c.want {
				t.Errorf("stderr = %q, want %q", errOut.String(), c.want)
			}
		})
	}
}

// hunk invert reads its patch through the same path, so -f - is stdin there
// too, and a file named "-" is not read.
func TestInvertDashFReadsStdin(t *testing.T) {
	cwd := t.TempDir()
	must(t, os.WriteFile(filepath.Join(cwd, "-"), []byte("@@ file g.txt\n@@ old\nx\n@@ new\ny\n"), 0o644))
	t.Chdir(cwd)
	const patch = "@@ file f.txt\n@@ old\na\n@@ new\nb\n"
	const inverse = "@@ file f.txt\n@@ old\nb\n@@ new\na\n"
	for _, c := range []struct {
		name, stdin string
		code        int
		out, err    string
	}{
		{"a patch", patch, exitOK, inverse, ""},
		{
			"nothing", "", exitUsage,
			"@@ end refused by hunk invert: the patch is empty. Run \"hunk format\" for the grammar\n",
			"hunk invert: the patch is empty. Run \"hunk format\" for the grammar\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := invertCLI(t, []string{"-f", "-"}, c.stdin)
			noFCode, noFOut, noFErr := invertCLI(t, nil, c.stdin)
			if code != noFCode || out != noFOut || errOut != noFErr {
				t.Errorf("-f - and no -f differ\n-f -:  %d %q %q\nno -f: %d %q %q", code, out, errOut, noFCode, noFOut, noFErr)
			}
			if code != c.code || out != c.out || errOut != c.err {
				t.Errorf("-f -: exit %d, stdout %q, stderr %q\nwant      %d, stdout %q, stderr %q", code, out, errOut, c.code, c.out, c.err)
			}
		})
	}
}

// The refusals that look at -f still stand for -f -: it is one patch, and
// --each takes none from it.
func TestDashFRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"-f - twice", []string{"-f", "-", "-f", "-"}, "hunk: invalid value \"-\" for flag -f: already reading -; one -f is one patch"},
		{"-f - and a file", []string{"-f", "-", "-f", "p.hunk"}, "hunk: invalid value \"p.hunk\" for flag -f: already reading -; one -f is one patch"},
		{"-f - under --each", []string{"--try", "true", "--each", "-f", "-", "p.hunk"}, "hunk: --each takes its patches as arguments, not from -f\n"},
		{"hunk invert, -f - twice", []string{"invert", "-f", "-", "-f", "-"}, "hunk invert: invalid value \"-\" for flag -f: already reading -; hunk invert takes one patch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := cliTree(t, map[string]string{"f.txt": "a\n"})
			before := snapshot(t, root)
			t.Chdir(root)
			var out, errOut bytes.Buffer
			code := cli(c.args, strings.NewReader("@@ file f.txt\n@@ old\na\n@@ new\nb\n"), &out, &errOut)
			if code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if !strings.HasPrefix(errOut.String(), c.want) {
				t.Errorf("stderr = %q, want it to start %q", errOut.String(), c.want)
			}
			assertUnchanged(t, root, before)
		})
	}
}
