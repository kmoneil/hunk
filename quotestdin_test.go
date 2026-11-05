package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// "--new -" reads the replacement from stdin, as "-f -" reads a patch (#142).
// Until 2026-11-05 "-" was a file name, refused as "--new: open -: no such
// file or directory", or read when a file named "-" happened to be there.

// quoteWithStdin runs hunk quote in dir, which is the working directory --new is
// relative to, with stdin as given.
func quoteWithStdin(t *testing.T, dir string, stdin io.Reader, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Chdir(dir)
	var out, errOut bytes.Buffer
	code = cli(append([]string{"quote"}, args...), stdin, &out, &errOut)
	return code, out.String(), errOut.String()
}

// Every replacement, from stdin and from a file of the same bytes: the two have
// to agree on the exit and on both streams, and each row names what that is.
func TestQuoteNewDashReadsStdinAsAFileIsRead(t *testing.T) {
	for _, c := range []struct {
		name, repl string
		out        string
	}{
		{"a line", "B\n", "@@ file f.txt\n@@ old\nb\n@@ new\nB\n"},
		{"no final newline", "B", "@@ file f.txt\n@@ old\nb\n@@ new\nB\n"},
		{"several lines", "B\nB2\n", "@@ file f.txt\n@@ old\nb\n@@ new\nB\nB2\n"},
		// Whole lines out: the last line's ending, CRLF or LF, is dropped,
		// and the payload ends in one newline (§4.2).
		{"CRLF", "B\r\n", "@@ file f.txt\n@@ old\nb\n@@ new\nB\n"},
		{"one empty line is a replacement, not nothing", "\n", "@@ file f.txt\n@@ old\nb\n@@ new\n\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := cliTree(t, map[string]string{"f.txt": "a\nb\nc\n", "n.txt": c.repl})
			code, out, errOut := quoteWithStdin(t, dir, strings.NewReader(c.repl), "f.txt", "2", "2", "--new", "-")
			fCode, fOut, fErr := quoteWithStdin(t, dir, strings.NewReader(""), "f.txt", "2", "2", "--new", "n.txt")
			if code != fCode || out != fOut || errOut != fErr {
				t.Errorf("stdin and a file differ\nstdin: %d %q %q\nfile:  %d %q %q", code, out, errOut, fCode, fOut, fErr)
			}
			if code != exitOK || out != c.out || errOut != "quoted f.txt 2, 1 line: \"b\"\n" {
				t.Errorf("exit %d\nstdout %q\nstderr %q\nwant stdout %q", code, out, errOut, c.out)
			}
		})
	}
}

// A file named "-" in the working directory is not what --new - reads; --new
// ./- reads it.
func TestQuoteNewDashIsNotAFileNamedDash(t *testing.T) {
	dir := cliTree(t, map[string]string{"f.txt": "a\nb\nc\n", "-": "FROM THE FILE\n"})
	_, out, _ := quoteWithStdin(t, dir, strings.NewReader("FROM STDIN\n"), "f.txt", "2", "2", "--new", "-")
	if !strings.HasSuffix(out, "@@ new\nFROM STDIN\n") {
		t.Errorf("--new - did not read stdin:\n%s", out)
	}
	_, out, _ = quoteWithStdin(t, dir, strings.NewReader("FROM STDIN\n"), "f.txt", "2", "2", "--new", "./-")
	if !strings.HasSuffix(out, "@@ new\nFROM THE FILE\n") {
		t.Errorf("--new ./- did not read the file:\n%s", out)
	}
}

func TestQuoteNewDashRefusals(t *testing.T) {
	devnull := func(t *testing.T) io.Reader {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Skip(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	for _, c := range []struct {
		name  string
		stdin func(t *testing.T) io.Reader
		code  int
		msg   string // the whole refusal, after "hunk quote: "
	}{
		{
			"nothing on stdin", func(*testing.T) io.Reader { return strings.NewReader("") }, exitUsage,
			"--new - read nothing from stdin; to delete the lines, give --new /dev/null",
		},
		{
			"a device on stdin", devnull, exitUsage,
			"--new - reads the replacement from stdin, which is a terminal or a device: pipe the replacement in, or give --new a file",
		},
		{
			"stdin that cannot be read", func(*testing.T) io.Reader { return failingReader{} }, exitUsage,
			"--new -: pipe gone",
		},
		{
			"a line of it that reads as a directive, named as stdin",
			func(*testing.T) io.Reader { return strings.NewReader("B\n@@ end\n") }, exitNoMatch,
			`stdin line 2 is "@@ end", which a patch reads as a directive and not as text; ` +
				"pass --marker with a prefix that no line quoted starts with, here and to the hunk that applies the patch",
		},
		{
			"a line of it glued to a word that is no directive, named as stdin",
			func(*testing.T) io.Reader { return strings.NewReader("B\n@@include x\n") }, exitNoMatch,
			`stdin line 2 is "@@include x", which starts like a directive, but "include" is not one`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := cliTree(t, map[string]string{"f.txt": "a\nb\nc\n"})
			code, out, errOut := quoteWithStdin(t, dir, c.stdin(t), "f.txt", "2", "2", "--new", "-")
			if code != c.code {
				t.Errorf("exit %d, want %d: %s", code, c.code, errOut)
			}
			if !strings.HasPrefix(errOut, "hunk quote: "+c.msg) || strings.Count(errOut, "\n") != 1 {
				t.Errorf("stderr = %q\nwant it to be the line %q", errOut, "hunk quote: "+c.msg+"...")
			}
			// The refusal is printed into the patch as well, so the apply it is
			// piped into stops.
			if !strings.HasPrefix(out, "@@ end refused by hunk quote: "+c.msg) {
				t.Errorf("stdout = %q, want the @@ end line", out)
			}
		})
	}
}

// --new /dev/null still deletes the lines, as an empty file does: it is a
// path, and the device check is stdin's alone.
func TestQuoteNewDevNullStillDeletes(t *testing.T) {
	dir := cliTree(t, map[string]string{"f.txt": "a\nb\nc\n", "empty.txt": ""})
	code, out, errOut := quoteWithStdin(t, dir, strings.NewReader(""), "f.txt", "2", "2", "--new", os.DevNull)
	eCode, eOut, _ := quoteWithStdin(t, dir, strings.NewReader(""), "f.txt", "2", "2", "--new", "empty.txt")
	if code != exitOK || code != eCode || out != eOut {
		t.Errorf("--new %s: exit %d, stdout %q, stderr %q\nan empty file: exit %d, stdout %q", os.DevNull, code, out, errOut, eCode, eOut)
	}
	// The delete's shape: old takes its last line ending, new is empty.
	if !strings.HasPrefix(out, "@@ file f.txt\n@@ old\nb\n\n@@ new\n") {
		t.Errorf("not a delete: %q", out)
	}
}

// End to end: the quote from stdin, piped into the apply, replaces the line.
func TestQuoteNewDashAppliesThroughTheApply(t *testing.T) {
	dir := cliTree(t, map[string]string{"f.txt": "a\nb\nc\n"})
	code, patch, errOut := quoteWithStdin(t, dir, strings.NewReader("B\n"), "f.txt", "2", "2", "--new", "-")
	if code != exitOK {
		t.Fatalf("quote: exit %d: %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, dir, nil, patch); code != exitOK {
		t.Fatalf("apply: exit %d: %s", code, errOut)
	}
	b, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	must(t, err)
	if string(b) != "a\nB\nc\n" {
		t.Errorf("f.txt = %q", b)
	}
}

func TestStdinIsADevice(t *testing.T) {
	regular, err := os.Create(filepath.Join(t.TempDir(), "r"))
	must(t, err)
	defer regular.Close()
	if stdinIsADevice(regular) {
		t.Error("a regular file is a device")
	}
	if stdinIsADevice(strings.NewReader("x")) {
		t.Error("a reader that is no file is a device")
	}
	if f, err := os.Open(os.DevNull); err == nil {
		defer f.Close()
		if !stdinIsADevice(f) {
			t.Errorf("%s is not a device", os.DevNull)
		}
	}
}
