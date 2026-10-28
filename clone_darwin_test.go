//go:build darwin

package main

// fclonefile against the real filesystem, and the number it calls against the
// SDK's header (#132). The fallback to a copy, and the check on what a clone
// made, are isolate_test.go's, where they hold on every system.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A clone has the source's bytes, is a file of its own, and is refused, with
// the kernel's or the runtime's error, where there is already something, where
// the name cannot be a C string, and for a file that is not open.
func TestFclonefile(t *testing.T) {
	dir := cliTree(t, map[string]string{"a.txt": "abc\n"})
	in, err := os.Open(filepath.Join(dir, "a.txt"))
	must(t, err)
	b := filepath.Join(dir, "b.txt")
	must(t, fclonefile(in, b))
	must(t, os.WriteFile(b, []byte("changed\n"), 0o644))
	if got, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(got) != "abc\n" {
		t.Errorf("a write to the clone reached the source: %q, %v", got, err)
	}
	for _, c := range []struct {
		name string
		in   *os.File
		to   string
		want error
	}{
		{"something is there", in, b, syscall.EEXIST},
		{"no such directory", in, filepath.Join(dir, "none", "c.txt"), syscall.ENOENT},
		{"a NUL in the name", in, filepath.Join(dir, "c\x00.txt"), syscall.EINVAL},
		{"no file", nil, filepath.Join(dir, "c.txt"), os.ErrInvalid},
	} {
		if err := fclonefile(c.in, c.to); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// The runtime's own error, internal/poll's, which is not os.ErrClosed.
	must(t, in.Close())
	if err := fclonefile(in, filepath.Join(dir, "c.txt")); err == nil || err.Error() != "use of closed file" {
		t.Errorf("a closed file: %v, want use of closed file", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "c.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused clone left c.txt: %v", err)
	}
}

// The number fclonefile traps with is fclonefileat's in the SDK this machine
// builds against. The compiler cannot see it, and a wrong number is another
// syscall run with these arguments.
func TestTheCloneNumberIsTheSDKs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sdk, err := exec.CommandContext(ctx, "xcrun", "--show-sdk-path").Output()
	if err != nil {
		t.Skipf("no SDK to read: %v", err)
	}
	header := filepath.Join(strings.TrimSpace(string(sdk)), "usr", "include", "sys", "syscall.h")
	b, err := os.ReadFile(header)
	if err != nil {
		t.Skipf("no header to read: %v", err)
	}
	m := regexp.MustCompile(`(?m)^#define\s+SYS_fclonefileat\s+(\d+)\s*$`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s defines no SYS_fclonefileat", header)
	}
	if n, _ := strconv.Atoi(string(m[1])); n != sysFclonefileat {
		t.Errorf("%s says fclonefileat is %d; hunk calls %d", header, n, sysFclonefileat)
	}
}
