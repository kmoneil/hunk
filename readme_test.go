package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// windowsInstaller is the one powershell block under README's "Without Scoop",
// the block install.yml runs on Windows. Read the same way install.yml reads
// it, so the two cannot be looking at different text.
func windowsInstaller(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("README.md")
	must(t, err)
	readme := string(b)
	const heading = "#### Without Scoop\n"
	i := strings.Index(readme, heading)
	if i < 0 {
		t.Fatal(`README.md has no "Without Scoop" section`)
	}
	section := readme[i+len(heading):]
	if j := regexp.MustCompile(`(?m)^#{2,4} `).FindStringIndex(section); j != nil {
		section = section[:j[0]]
	}
	blocks := regexp.MustCompile("(?s)```powershell\n(.*?)```").FindAllStringSubmatch(section, -1)
	if len(blocks) != 1 {
		t.Fatalf(`"Without Scoop" has %d powershell blocks, and install.yml runs exactly one`, len(blocks))
	}
	return blocks[0][1]
}

// The block stops at its first error. Pasted into a console it runs under
// PowerShell's default, Continue, where a 404 from Invoke-WebRequest is reported
// and the next line runs anyway: until 2026-10-06 an asset the release did not
// have, which SHA256SUMS does not list either, compared $null with $null, passed
// the check, and the block installed whatever had arrived. install.yml never saw
// it, because a GitHub Actions PowerShell step runs under Stop.
//
// So Stop is the block's first statement, and the block is one script block, so
// the preference ends with it rather than staying set in the user's console.
func TestTheWindowsInstallerStopsAtTheFirstError(t *testing.T) {
	var code []string
	for _, line := range strings.Split(windowsInstaller(t), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		code = append(code, line)
	}
	if len(code) < 3 {
		t.Fatalf("the block has %d lines of code", len(code))
	}
	if code[0] != "& {" || code[len(code)-1] != "}" {
		t.Errorf("the block is not one script block, & { ... }: it starts %q and ends %q, so its preferences stay set in the console it was pasted into", code[0], code[len(code)-1])
	}
	stop := "$ErrorActionPreference = 'Stop'"
	at := -1
	for i, line := range code {
		if line == stop {
			at = i
			break
		}
	}
	// At 0 it is first but outside any script block, which the check above
	// has already said.
	switch {
	case at < 0:
		t.Errorf("the block never sets %s, so under the console's Continue a download that fails is reported and the install goes on", stop)
	case at > 1:
		t.Errorf("%s is line %d of the block's code, after %q, which runs before it", stop, at+1, code[at-1])
	}
}

// --verify and --try run any shell command, with the user's permissions, which
// is what they are for. What follows for an agent's harness is the thing worth
// saying: a rule that allows hunk allows every command those flags carry.
func TestTheDocsSayVerifyRunsAnyShellCommand(t *testing.T) {
	for _, p := range []string{"README.md", filepath.Join(".agents", "skills", "hunk", "SKILL.md")} {
		b, err := os.ReadFile(p)
		must(t, err)
		for _, want := range []string{"any shell command", "allows `hunk`"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s does not say %q", p, want)
			}
		}
	}
}

// --allow-outside-root is not one path's exception. It opens the whole batch by
// name, every path, so README and --help both say so. The skill omits the flag
// on purpose (skillOmits), so it is not asked of SKILL.md.
func TestAllowOutsideRootSaysItIsTheWholeBatch(t *testing.T) {
	b, err := os.ReadFile("README.md")
	must(t, err)
	readme := string(b)
	for _, want := range []string{"--allow-outside-root", "every path in the batch", "by name"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md does not say %q", want)
		}
	}

	var out, errOut bytes.Buffer
	if code := cli([]string{"--help"}, strings.NewReader(""), &out, &errOut); code != exitOK {
		t.Fatalf("--help: exit %d", code)
	}
	_, entry, ok := strings.Cut(out.String(), "\n  --allow-outside-root")
	if !ok {
		t.Fatal("--help has no --allow-outside-root entry")
	}
	entry, _, _ = strings.Cut(entry, "\n  --")
	entry = strings.Join(strings.Fields(entry), " ")
	for _, want := range []string{"every path in the batch", "by name"} {
		if !strings.Contains(entry, want) {
			t.Errorf("--help's --allow-outside-root entry %q does not say %q", entry, want)
		}
	}
}
