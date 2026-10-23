package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// The shapes the counts have a rule for, a file each, and what git says of
// them: one batch applies all of them.
var countShapes = []struct {
	name, file, before, patch string
	added, removed            int
}{
	{"#111: a blank line under each new text", "111.txt", "a\nb\nc\n", "@@ old\na\n@@ new\nA\n\n@@ old\nb\n@@ new\nB\n\n", 4, 2},
	{"context repeated in old and new", "context.txt", "one\ntwo\nthree\n", "@@ old\none\ntwo\n\n@@ new\none\ntwo\ntwo and a half\n\n", 1, 0},
	{"a word inside a line", "word.txt", "x(foo)\n", "@@ old\nfoo\n@@ new\nfoo, bar\n", 1, 1},
	{"x2 on one line", "sameline.txt", "1 1\n", "@@ old x2\n1\n@@ new\n2\n", 1, 1},
	{"x2 on two lines", "twolines.txt", "x = 1\ny = 1\n", "@@ old x2\n1\n@@ new\n2\n", 2, 2},
	{
		"two lines rewritten inside a block", "block.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n",
		"@@ old\n1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n\n@@ new\n1\n2\nthree\n4\n5\n6\nseven\n8\n9\n10\n\n", 2, 2,
	},
	{"the middle of three kept", "middle.txt", "x\ny\nz\n", "@@ old\nx\ny\nz\n\n@@ new\ny\n\n", 0, 2},
	{"a line deleted", "deleted.txt", "a\nb\nc\n", "@@ old\nb\n\n@@ new\n", 0, 1},
	{"a line joined", "joined.txt", "a\nb\nc\n", "@@ old\na\n\n@@ new\nA\n", 1, 2},
	{"a line joined by a deletion inside it", "midjoin.txt", "x = 1\ny\n", "@@ old\n1\n\n@@ new\n", 1, 2},
	{"x3, two joined and one at the end", "x3.txt", "a\nb\na\nc\na\n", "@@ old x3\na\n\n@@ new\nA\n", 3, 5},
	{"the final newline taken away", "final.txt", "a\nb\n", "@@ old\nb\n\n@@ new\nB\n", 1, 1},
	{"a final newline given, the line otherwise the same", "given.txt", "a\nb", "@@ old\nb\n@@ new\nb\n\n", 1, 1},
	{"an empty line added on purpose", "purpose.txt", "a\nb\n", "@@ old\na\n\n@@ new\nA\n\n\n", 2, 1},
	{"x2, the second match taking the newline after the first", "taken.txt", "\na\na", "@@ old x2\n\na\n@@ new\n0\n\n", 2, 3},
	{"a CRLF file", "crlf.txt", "a\r\nb\r\n", "@@ old\na\n@@ new\nA\n", 1, 1},
	{"an append that ends the last line", "seam.txt", "a", "@@ append seam.txt\nb\n", 2, 1},
	{"an empty append that ends the last line", "emptyappend.txt", "x", "@@ append emptyappend.txt\n", 1, 1},
	{"a prepend", "prepend.txt", "b\n", "@@ prepend prepend.txt\na\n", 1, 0},
	{"a file created", "created.txt", "", "@@ create created.txt\nx\ny\n\n", 2, 0},
	{"a file created and then replaced in", "recreated.txt", "", "@@ create recreated.txt\nhi\n\n@@ old\nhi\n\n@@ new\nbye\n", 1, 0},
	{"a file replaced in and then deleted", "gone.txt", "hi\nthere\n", "@@ old\nhi\n@@ new\nbye\n@@ delete gone.txt\n", 0, 2},
}

// countPatch is every shape as one batch.
func countPatch() (files map[string]string, patch string) {
	files = map[string]string{}
	var b strings.Builder
	for _, c := range countShapes {
		if c.before != "" || !strings.HasPrefix(c.patch, "@@ create") {
			files[c.file] = c.before
		}
		if !strings.HasPrefix(c.patch, "@@ append") && !strings.HasPrefix(c.patch, "@@ prepend") && !strings.HasPrefix(c.patch, "@@ create") {
			b.WriteString("@@ file " + c.file + "\n")
		}
		b.WriteString(c.patch)
		if !strings.HasSuffix(c.patch, "\n") {
			b.WriteString("\n")
		}
	}
	return files, b.String()
}

// countsOf is each file's counts from --json.
func countsOf(t *testing.T, out string) map[string][2]int {
	t.Helper()
	var r struct {
		Files []struct {
			Path           string
			Added, Removed int
		}
	}
	must(t, json.Unmarshal([]byte(out), &r))
	got := map[string][2]int{}
	for _, f := range r.Files {
		got[f.Path] = [2]int{f.Added, f.Removed}
	}
	return got
}

// TestTheCountsAreGits (#111): each file's +N -M is what git diff --numstat
// says of the same edit, in a repository of the tree before, for every shape
// the counts have a rule for. Until 2026-10-23 they were the payloads' lines,
// and #111's batch, three lines to five, said +2 -2.
func TestTheCountsAreGits(t *testing.T) {
	needsGit(t)
	files, patch := countPatch()
	root := cliTree(t, files)
	env := gitEnv(t.TempDir())
	git(t, env, root, "init", "-q")
	git(t, env, root, "add", "-A")
	git(t, env, root, "commit", "-q", "-m", "before")

	code, out, errOut := runCLI(t, root, []string{"--json"}, patch)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	got := countsOf(t, out)

	git(t, env, root, "add", "-A")
	gits := map[string][2]int{}
	for line := range strings.SplitSeq(strings.TrimSpace(git(t, env, root, "diff", "--cached", "--numstat")), "\n") {
		f := strings.Split(line, "\t")
		a, errA := strconv.Atoi(f[0])
		r, errR := strconv.Atoi(f[1])
		if len(f) != 3 || errA != nil || errR != nil {
			t.Fatalf("numstat line %q", line)
		}
		gits[f[2]] = [2]int{a, r}
	}
	for _, c := range countShapes {
		want := [2]int{c.added, c.removed}
		if gits[c.file] != want {
			t.Errorf("%s: git says %v, the table %v", c.name, gits[c.file], want)
		}
		if got[c.file] != want {
			t.Errorf("%s: +%d -%d, want +%d -%d as git says", c.name, got[c.file][0], got[c.file][1], c.added, c.removed)
		}
	}
	if len(got) != len(countShapes) || len(gits) != len(countShapes) {
		t.Errorf("%d files reported, %d in git's, %d shapes", len(got), len(gits), len(countShapes))
	}
}

// TestTheCountsRegions: the rule as counts.go has it, case by case, with the
// cases git cannot be asked about: the table's cap, and two hunks on one
// line, which each count, as two commits would.
func TestTheCountsRegions(t *testing.T) {
	for _, c := range []struct {
		name           string
		cur, old, repl string
		n              int
		added, removed int
	}{
		{"a whole line", "a\nb\n", "a\n", "A\n", 1, 1, 1},
		{"a line added after context", "a\nb\n", "a\n", "a\nx\n", 1, 1, 0},
		{"a line added before context", "a\nb\n", "b\n", "x\nb\n", 1, 1, 0},
		{"the empty line after the match is in the region", "a\nb\n", "a", "A\n", 1, 2, 1},
		{"a new text that breaks its line", "a b\n", "a", "A\n", 1, 2, 1},
		{"a match across lines", "a\nb\nc\n", "a\nb", "x", 1, 1, 2},
		{"x2 on one line is one region", "1 1\n", "1", "2", 2, 1, 1},
		{"x3 on one line", "1 1 1\n", "1", "2", 3, 1, 1},
		{"x2 on lines a line apart", "1\nz\n1\n", "1", "2", 2, 2, 2},
		{"x2 whose join reaches the next match's line", "a\na\nb\n", "a\n", "A", 2, 1, 3},
		{"a match at the end of a file with no final newline", "a\nb", "b", "B", 1, 1, 1},
		{"a line whose ending alone changed", "a\nb", "b", "b\n", 1, 1, 1},
		{"a CRLF line made LF", "a\r\nb\r\n", "a\r\n", "a\n", 1, 1, 1},
		{"the whole file", "a\nb\n", "a\nb\n", "c\n", 1, 1, 2},
		{"nothing in common", "a\n", "a\n", "b\nc\n", 1, 2, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, r := replaceCounts([]byte(c.cur), []byte(c.old), []byte(c.repl), c.n)
			if a != c.added || r != c.removed {
				t.Errorf("+%d -%d, want +%d -%d", a, r, c.added, c.removed)
			}
		})
	}

	// Past lcsMost cells the middle counts whole: one line in common among
	// 2049 on each side is not looked for, and among 2048 it is.
	for _, c := range []struct{ lines, added int }{{2048, 2047}, {2049, 2049}} {
		t.Run(fmt.Sprintf("the cap, %d lines a side", c.lines), func(t *testing.T) {
			var before, after bytes.Buffer
			for i := range c.lines {
				if i == c.lines/2 {
					before.WriteString("same\n")
					after.WriteString("same\n")
					continue
				}
				fmt.Fprintf(&before, "b%d\n", i)
				fmt.Fprintf(&after, "a%d\n", i)
			}
			a, r := regionCounts(before.Bytes(), after.Bytes())
			if a != c.added || r != c.added {
				t.Errorf("+%d -%d, want +%d -%d", a, r, c.added, c.added)
			}
		})
	}

	// The lines in common at either end are set aside before the table, so
	// a block past the cap with one line rewritten is still one line: the
	// table is only ever the middle.
	for _, at := range []int{0, 1050, 2099} {
		t.Run(fmt.Sprintf("2100 lines with line %d rewritten, past the cap whole", at), func(t *testing.T) {
			var before, after bytes.Buffer
			for i := range 2100 {
				fmt.Fprintf(&before, "line %d\n", i)
				if i == at {
					fmt.Fprintf(&after, "LINE %d\n", i)
				} else {
					fmt.Fprintf(&after, "line %d\n", i)
				}
			}
			if a, r := regionCounts(before.Bytes(), after.Bytes()); a != 1 || r != 1 {
				t.Errorf("+%d -%d, want +1 -1", a, r)
			}
		})
	}

	t.Run("two hunks on one line each count it", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "x = 1, y = 1\n"})
		_, out, _ := runCLI(t, root, []string{"--json"}, "@@ file a.txt\n@@ old\nx = 1\n@@ new\nx = 2\n@@ old\ny = 1\n@@ new\ny = 2\n")
		if got := countsOf(t, out)["a.txt"]; got != [2]int{2, 2} {
			t.Errorf("%v, want +2 -2: one line, rewritten twice", got)
		}
	})
}

// FuzzTheCountsAreTheFilesLines: whatever the file and the payloads, a
// replacement's counts are a value, never a panic; added less removed is the
// file's change in lines, exactly; neither is more than the file has; and
// neither is less than a minimal line diff of the file before and after says,
// which is what git counts.
func FuzzTheCountsAreTheFilesLines(f *testing.F) {
	for _, c := range countShapes {
		if strings.HasPrefix(c.patch, "@@ old") {
			p, err := Parse([]byte("@@ file f\n"+c.patch), DefaultMarker)
			if err == nil {
				f.Add([]byte(c.before), p.Hunks[0].Old, p.Hunks[0].New)
			}
		}
	}
	f.Fuzz(func(t *testing.T, start, old, repl []byte) {
		n := bytes.Count(start, old)
		if len(old) == 0 || n == 0 {
			return
		}
		tree, _ := fixture(t, map[string]string{"a.txt": string(start)})
		p := &Patch{Hunks: []Hunk{{Op: OpReplace, Path: "a.txt", Line: 1, Count: n, Old: old, New: repl}}}
		r, err := NewTxn(tree, Options{EOL: EOLStrict}).Preview(p)
		if err != nil {
			return // copies that overlap, which are refused (§3.4)
		}
		got := bytes.Replace(start, old, repl, n)
		if bytes.Equal(got, start) {
			if len(r.Files) != 0 {
				t.Fatalf("an edit that changed nothing is listed: %+v", r.Files)
			}
			return
		}
		a, rm := r.Files[0].Added, r.Files[0].Removed
		was, is := lineCount(start), lineCount(got)
		if a-rm != is-was || a > is || rm > was || a < 0 || rm < 0 {
			t.Fatalf("+%d -%d for %d lines to %d\nstart %q old %q new %q", a, rm, was, is, start, old, repl)
		}
		if was*is <= 1<<16 {
			common := lcsOf(linesOf(start), linesOf(got))
			if a < is-common || rm < was-common {
				t.Errorf("+%d -%d, less than a minimal diff's +%d -%d\nstart %q old %q new %q", a, rm, is-common, was-common, start, old, repl)
			}
		}
	})
}

// linesOf and lcsOf are the fuzz target's oracle, written apart from
// counts.go so that a fault in fileLines or commonLines cannot be in both. It
// shared them until a mutant comparing lines without their endings survived
// the target and every table.
func linesOf(b []byte) []string {
	out := strings.SplitAfter(string(b), "\n")
	if out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func lcsOf(x, y []string) int {
	t := make([][]int, len(x)+1)
	for i := range t {
		t[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	return t[0][0]
}
