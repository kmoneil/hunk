package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The skill is installed two ways that meet at the same two folders: make
// install copies it there, and the README's Homebrew steps link Homebrew's
// copy there. Each met what the other left and did the wrong thing without a
// word (#179). ln -s onto a folder put the link inside it and exited 0, so the
// agent went on reading the old copy; cp onto a link wrote through it, so
// make install overwrote Homebrew's copy in the Cellar. Both run here for
// real, on a temporary home, from each state the other leaves.

// brewSkill makes a stand-in Homebrew prefix holding the skill where the
// formula puts it, and a brew on a directory of its own that answers
// --prefix with it. It returns the skill's directory and that bin directory.
func brewSkill(t *testing.T) (skill, bin string) {
	t.Helper()
	prefix, bin := t.TempDir(), t.TempDir()
	skill = filepath.Join(prefix, "opt", "hunk", "share", "hunk", "skill")
	must(t, os.MkdirAll(filepath.Join(skill, "references"), 0o755))
	must(t, os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("Homebrew's skill\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(skill, "references", "exit-codes.md"), []byte("Homebrew's reference\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(bin, "brew"),
		[]byte("#!/bin/sh\n[ \"$1\" = --prefix ] && echo '"+prefix+"'\n"), 0o755))
	return skill, bin
}

// installState is a state of the two skill folders under home that the other
// way of installing leaves, or none.
type installState struct {
	name string
	make func(t *testing.T, home, skill string)
}

func installStates() []installState {
	dirs := func(home string) []string {
		return []string{filepath.Join(home, ".claude", "skills", "hunk"), filepath.Join(home, ".agents", "skills", "hunk")}
	}
	return []installState{
		{"nothing there", func(*testing.T, string, string) {}},
		{"a folder from make install", func(t *testing.T, home, _ string) {
			for _, d := range dirs(home) {
				must(t, os.MkdirAll(filepath.Join(d, "references"), 0o755))
				must(t, os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("an old copy\n"), 0o644))
			}
		}},
		{"a folder holding the link the old README nested in it", func(t *testing.T, home, skill string) {
			for _, d := range dirs(home) {
				must(t, os.MkdirAll(d, 0o755))
				must(t, os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("an old copy\n"), 0o644))
				must(t, os.Symlink(skill, filepath.Join(d, "skill")))
			}
		}},
		{"Homebrew's links", func(t *testing.T, home, skill string) {
			for _, d := range dirs(home) {
				must(t, os.MkdirAll(filepath.Dir(d), 0o755))
				must(t, os.Symlink(skill, d))
			}
		}},
	}
}

// homebrewBlock is the README's sh block under the Homebrew heading that links
// the skill. Exactly one: a second would be steps this does not run.
func homebrewBlock(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("README.md")
	must(t, err)
	section := regexp.MustCompile("(?ms)^### macOS and Linux, with Homebrew\n(.*?)^#{2,3} ").FindSubmatch(b)
	if section == nil {
		t.Fatal("README.md has no \"### macOS and Linux, with Homebrew\" section")
	}
	var blocks []string
	for _, m := range regexp.MustCompile("(?s)```sh\n(.*?)```").FindAllSubmatch(section[1], -1) {
		if strings.Contains(string(m[1]), "ln -s") {
			blocks = append(blocks, string(m[1]))
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("the Homebrew section has %d sh blocks that link the skill, and this runs exactly one", len(blocks))
	}
	return blocks[0]
}

// The README's Homebrew steps, as written, in each shell a reader is likely
// to paste them into, from each state an earlier install leaves: both
// folders end as links to Homebrew's skill, its files untouched, and the last
// line printed is where the link leads.
func TestTheREADMEsHomebrewStepsLinkTheSkill(t *testing.T) {
	needsShellSymlinks(t)
	block := homebrewBlock(t)
	for _, shell := range []string{"sh", "bash", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			t.Logf("no %s on PATH", shell)
			continue
		}
		for _, st := range installStates() {
			t.Run(shell+"/"+st.name, func(t *testing.T) {
				skill, bin := brewSkill(t)
				home := t.TempDir()
				st.make(t, home, skill)
				cmd := exec.Command(path, "-c", block)
				cmd.Dir = home
				cmd.Env = childEnv("HOME="+home, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", shell, err, out)
				}
				lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
				if last := lines[len(lines)-1]; last != skill {
					t.Errorf("the last line is %q, want the skill's path %q\n%s", last, skill, out)
				}
				for _, d := range []string{".claude/skills/hunk", ".agents/skills/hunk"} {
					to, err := os.Readlink(filepath.Join(home, filepath.FromSlash(d)))
					if err != nil || to != skill {
						t.Errorf("~/%s is not a link to the skill: %q, %v", d, to, err)
					}
				}
				// Run again over its own links, ln -s without the rm follows
				// each into Homebrew's folder and leaves a link there.
				if got := readFile(t, skill, "SKILL.md"); got != "Homebrew's skill\n" {
					t.Errorf("Homebrew's SKILL.md is now %q", got)
				}
				if names := entries(t, skill); names != "SKILL.md references" {
					t.Errorf("Homebrew's skill folder holds %s", names)
				}
			})
		}
	}
}

// entries is the names in dir, sorted, joined by spaces.
func entries(t *testing.T, dir string) string {
	t.Helper()
	es, err := os.ReadDir(dir)
	must(t, err)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return strings.Join(names, " ")
}

// make install from each state the README's Homebrew steps leave: a folder is
// a copy of its own and is brought up to date; a link is Homebrew's, which
// brew upgrade keeps current, and is refused before anything is installed,
// the binary included, rather than written through. The go here is a stand-in
// that records each call, so no binary is installed anywhere.
func TestMakeInstallRefusesALink(t *testing.T) {
	needsMake(t)
	needsSymlinks(t)
	clone := readFile(t, filepath.Join(".agents", "skills", "hunk"), "SKILL.md")
	for _, st := range installStates() {
		t.Run(st.name, func(t *testing.T) {
			skill, _ := brewSkill(t)
			home, tools := t.TempDir(), t.TempDir()
			st.make(t, home, skill)
			dirs := []string{filepath.Join(home, ".claude", "skills", "hunk"), filepath.Join(home, ".agents", "skills", "hunk")}
			goLog := filepath.Join(tools, "log")
			goProg := filepath.Join(tools, "go")
			must(t, os.WriteFile(goProg, []byte("#!/bin/sh\necho \"$*\" >> '"+goLog+"'\n"), 0o755))
			code, out := runMake(t, nil, "install", "GO="+goProg, "SKILLDIRS="+strings.Join(dirs, " "))
			calls, _ := os.ReadFile(goLog)
			if st.name == "Homebrew's links" {
				want := "make install: " + dirs[0] + " is a link to " + skill + ", and copying the skill there would write through it; nothing was installed."
				if code == 0 || !strings.Contains(out, want) || !strings.Contains(out, "make install SKILLDIRS=") {
					t.Errorf("exit %d, want a refusal saying %q:\n%s", code, want, out)
				}
				if len(calls) > 0 {
					t.Errorf("the refusal installed the binary: go was run with %q", calls)
				}
				for _, d := range dirs {
					if to, err := os.Readlink(d); err != nil || to != skill {
						t.Errorf("%s is no longer the link: %q, %v", d, to, err)
					}
				}
				if got := readFile(t, skill, "SKILL.md"); got != "Homebrew's skill\n" {
					t.Errorf("make install wrote through the link: Homebrew's SKILL.md is %q", got)
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if !strings.HasPrefix(string(calls), "install .\n") {
				t.Errorf("go was run with %q, want install . first", calls)
			}
			for _, d := range dirs {
				if got := readFile(t, d, "SKILL.md"); got != clone {
					t.Errorf("%s/SKILL.md is %.40q, not the clone's", d, got)
				}
				if _, err := os.Stat(filepath.Join(d, "references", "exit-codes.md")); err != nil {
					t.Errorf("no reference copied: %v", err)
				}
			}
		})
	}
	// With no skill folder at all it installs the binary alone, which is
	// what the refusal tells a Homebrew user to run.
	t.Run("SKILLDIRS empty", func(t *testing.T) {
		tools := t.TempDir()
		goLog, goProg := filepath.Join(tools, "log"), filepath.Join(tools, "go")
		must(t, os.WriteFile(goProg, []byte("#!/bin/sh\necho \"$*\" >> '"+goLog+"'\n"), 0o755))
		code, out := runMake(t, nil, "install", "GO="+goProg, "SKILLDIRS=")
		calls, _ := os.ReadFile(goLog)
		if code != 0 || !strings.HasPrefix(string(calls), "install .\n") || strings.Contains(out, "skill:") {
			t.Errorf("exit %d, go run with %q:\n%s", code, calls, out)
		}
	})
}
