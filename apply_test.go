package main

import (
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fixture builds a tree from a name->contents map and returns it with a Txn.
func fixture(t *testing.T, files map[string]string) (*Tree, string) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	for name, body := range files {
		full := filepath.Join(root, name)
		must(t, os.MkdirAll(filepath.Dir(full), 0o755))
		must(t, os.WriteFile(full, []byte(body), 0o644))
	}
	tree, err := OpenTree(root, false)
	must(t, err)
	t.Cleanup(func() { tree.Close() })
	return tree, root
}

// snapshot records every regular file's contents and mode, so "nothing was
// written" can be asserted rather than assumed. §4.1 attaches a tree state to
// four exit codes and this is how those are checked.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = fi.Mode().String() + "\x00" + string(b)
		return nil
	})
	must(t, err)
	return out
}

func assertUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := snapshot(t, root)
	if len(before) != len(after) {
		t.Fatalf("file count changed: %d -> %d", len(before), len(after))
	}
	var names []string
	for n := range before {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if before[n] != after[n] {
			t.Errorf("%s changed; the tree should be byte-identical", n)
		}
	}
}

func run(t *testing.T, tree *Tree, patch string, opt Options) (*Result, error) {
	t.Helper()
	p, err := Parse([]byte(patch), DefaultMarker)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return NewTxn(tree, opt).Run(p)
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	must(t, err)
	return string(b)
}

func TestApply(t *testing.T) {
	t.Run("one hunk", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "package a\n\nfunc f() {}\n"})
		r, err := run(t, tree, "@@ file a.go\n@@ old\nfunc f() {}\n@@ new\nfunc f(x int) {}\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "package a\n\nfunc f(x int) {}\n" {
			t.Errorf("got %q", got)
		}
		if r.Hunks != 1 || len(r.Files) != 1 || r.Files[0].Added != 1 || r.Files[0].Removed != 1 {
			t.Errorf("result = %+v", r)
		}
	})

	t.Run("many hunks across many files, one write each", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{
			"a.go": "one\ntwo\nthree\n",
			"b.go": "alpha\nbeta\n",
		})
		r, err := run(t, tree, ""+
			"@@ file a.go\n@@ old\none\n@@ new\nONE\n"+
			"@@ old\nthree\n@@ new\nTHREE\n"+
			"@@ file b.go\n@@ old\nbeta\n@@ new\nBETA\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "ONE\ntwo\nTHREE\n" {
			t.Errorf("a.go = %q", got)
		}
		if got := readFile(t, root, "b.go"); got != "alpha\nBETA\n" {
			t.Errorf("b.go = %q", got)
		}
		if r.Hunks != 3 || len(r.Files) != 2 {
			t.Errorf("result = %+v", r)
		}
	})

	// §3.5: hunks apply against the file as previous hunks have left it, so a
	// second hunk may legally match text a first one created.
	t.Run("a hunk matches text an earlier hunk created", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "start\n"})
		_, err := run(t, tree, ""+
			"@@ file a.go\n@@ old\nstart\n@@ new\nmiddle\n"+
			"@@ old\nmiddle\n@@ new\nend\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "end\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("x2 replaces both", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "P\nq\nP\n"})
		_, err := run(t, tree, "@@ file a.go\n@@ old x2\nP\n@@ new\nZ\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "Z\nq\nZ\n" {
			t.Errorf("got %q", got)
		}
	})

	// One file named several ways is one file, read once and written once
	// (§3.5). The resolved name is the key, never the string the patch wrote.
	t.Run("spellings of one path are one file", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"sub/a.go": "one\ntwo\n"})
		must(t, os.Symlink("sub/a.go", filepath.Join(root, "alias.go")))
		_, err := run(t, tree, ""+
			"@@ file sub/a.go\n@@ old\none\n@@ new\nONE\n"+
			"@@ file ./sub/../sub/a.go\n@@ old\ntwo\n@@ new\nTWO\n"+
			"@@ file alias.go\n@@ old\nONE\n@@ new\nUNO\n", Options{})
		must(t, err)
		if got := readFile(t, root, "sub/a.go"); got != "UNO\nTWO\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("the mode survives", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"s.sh": "#!/bin/sh\nfalse\n"})
		must(t, os.Chmod(filepath.Join(root, "s.sh"), 0o755))
		_, err := run(t, tree, "@@ file s.sh\n@@ old\nfalse\n@@ new\ntrue\n", Options{})
		must(t, err)
		fi, err := os.Stat(filepath.Join(root, "s.sh"))
		must(t, err)
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("mode %v, want 0755", fi.Mode().Perm())
		}
	})

	t.Run("a symlinked target is written through", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"sub/real.go": "old\n"})
		must(t, os.Symlink("sub/real.go", filepath.Join(root, "link.go")))
		_, err := run(t, tree, "@@ file link.go\n@@ old\nold\n@@ new\nnew\n", Options{})
		must(t, err)
		fi, err := os.Lstat(filepath.Join(root, "link.go"))
		must(t, err)
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Error("the symlink was replaced")
		}
		if got := readFile(t, root, "sub/real.go"); got != "new\n" {
			t.Errorf("target = %q", got)
		}
	})
}

func TestApplyRefuses(t *testing.T) {
	t.Run("no match, and nothing is written", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\ntwo\n"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ file a.go\n@@ old\nabsent\n@@ new\nx\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want *ValidationError, got %T: %v", err, err)
		}
		if len(ve.Failures) != 1 || ve.Failures[0].Expected != 1 || ve.Failures[0].Found != 0 {
			t.Errorf("failures = %+v", ve.Failures)
		}
		if ve.Failures[0].PatchLine != 2 {
			t.Errorf("patch line = %d, want 2", ve.Failures[0].PatchLine)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("too many matches", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "P\nP\nP\n"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ file a.go\n@@ old\nP\n@@ new\nZ\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("got %T: %v", err, err)
		}
		if ve.Failures[0].Found != 3 {
			t.Errorf("found = %d, want 3", ve.Failures[0].Found)
		}
		assertUnchanged(t, root, before)
	})

	// §6.4: overlapping matches are counted left to right, non-overlapping, the
	// way strings.Count does. "aa" in "aaa" is 1, and the mismatch is reported
	// rather than guessed at.
	t.Run("overlapping matches are counted the way strings.Count does", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "aaa"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ file a.txt\n@@ old x2\naa\n@@ new\nb\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("got %T: %v", err, err)
		}
		if ve.Failures[0].Expected != 2 || ve.Failures[0].Found != 1 {
			t.Errorf("expected/found = %d/%d, want 2/1", ve.Failures[0].Expected, ve.Failures[0].Found)
		}
		assertUnchanged(t, root, before)
	})

	// §8.1's cascade test, all three assertions in one case: the failing hunk
	// is reported failed, the later hunk against the same file is reported
	// skipped rather than failed, and a hunk against another file is still
	// evaluated.
	t.Run("a failure cascades within its file and not beyond it", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{
			"a.go": "one\ntwo\n",
			"b.go": "beta\n",
		})
		before := snapshot(t, root)
		_, err := run(t, tree, ""+
			"@@ file a.go\n@@ old\none\n@@ new\nONE\n"+ // 1, would apply
			"@@ old\nabsent\n@@ new\nx\n"+ // 2, fails
			"@@ old\nONE\n@@ new\nUNO\n"+ // 3, skipped: depends on 1, same file as 2
			"@@ file b.go\n@@ old\nnope\n@@ new\ny\n", // 4, other file, still evaluated
			Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("got %T: %v", err, err)
		}
		if len(ve.Failures) != 3 {
			t.Fatalf("got %d failures, want 3: %+v", len(ve.Failures), ve.Failures)
		}
		f2, f3, f4 := ve.Failures[0], ve.Failures[1], ve.Failures[2]
		if f2.Hunk != 2 || f2.Skipped() {
			t.Errorf("hunk 2 should be a failure: %+v", f2)
		}
		if f3.Hunk != 3 || f3.SkippedAfter != 2 {
			t.Errorf("hunk 3 should be skipped after 2: %+v", f3)
		}
		if f4.Hunk != 4 || f4.Skipped() || f4.Found != 0 {
			t.Errorf("hunk 4 should be evaluated and failed: %+v", f4)
		}
		if msg := ve.Error(); !strings.Contains(msg, "2 hunks did not match") ||
			!strings.Contains(msg, "1 skipped") || !strings.Contains(msg, "nothing was written") {
			t.Errorf("summary = %q", msg)
		}
		assertUnchanged(t, root, before)
	})

	// A missing file is a per-hunk refusal, not an error that aborts the load,
	// so a patch with several of them reports all of them in one round trip.
	t.Run("a missing file is a reported refusal, not an aborted load", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"a.go": "x\n"})
		_, err := run(t, tree, ""+
			"@@ file gone.go\n@@ old\nx\n@@ new\ny\n"+
			"@@ file also-gone.go\n@@ old\nx\n@@ new\ny\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want *ValidationError, got %T: %v", err, err)
		}
		if len(ve.Failures) != 2 {
			t.Fatalf("got %d failures, want both reported: %+v", len(ve.Failures), ve.Failures)
		}
		for _, f := range ve.Failures {
			if !strings.Contains(f.Refusal, "no such file") {
				t.Errorf("hunk %d refusal = %q", f.Hunk, f.Refusal)
			}
		}
	})

	// ReadFile on a directory is not fs.ErrNotExist, so without this it would
	// fall through to an I/O error. §6.1's rule is whether editing the patch
	// would fix it, and here it would.
	t.Run("a directory is exit 2 and says so", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"sub/a.go": "x\n"})
		_, err := run(t, tree, "@@ file sub\n@@ old\nx\n@@ new\ny\n", Options{})
		var pr *PathRefusal
		if !errors.As(err, &pr) {
			t.Fatalf("want *PathRefusal, got %T: %v", err, err)
		}
		if !strings.Contains(pr.Reason, "is a directory") {
			t.Errorf("reason = %q", pr.Reason)
		}
	})

	// A replace after a delete in the same batch fails against the file the
	// batch removed, and says so rather than "no such file", which would send
	// the agent looking at a disk that still has it.
	t.Run("a hunk after a delete says an earlier hunk deleted it", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "x\n"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ delete a.go\n@@ old\nx\n@@ new\ny\n", Options{})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want *ValidationError, got %T: %v", err, err)
		}
		if !strings.Contains(ve.Failures[0].Refusal, "earlier hunk in this batch deleted it") {
			t.Errorf("refusal = %q", ve.Failures[0].Refusal)
		}
		assertUnchanged(t, root, before)
	})
}

// §6.1 step 4. The guard against a second writer between load and commit, which
// needs the phases driven separately because Run closes the window itself.
func TestCheckCatchesAWriterUnderneath(t *testing.T) {
	for _, c := range []struct {
		name string
		mess func(t *testing.T, root string)
	}{
		{"rewritten", func(t *testing.T, root string) {
			must(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("somebody else\n"), 0o644))
		}},
		{"removed", func(t *testing.T, root string) {
			must(t, os.Remove(filepath.Join(root, "a.go")))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree, root := fixture(t, map[string]string{"a.go": "one\n"})
			p, err := Parse([]byte("@@ file a.go\n@@ old\none\n@@ new\nONE\n"), DefaultMarker)
			must(t, err)
			x := NewTxn(tree, Options{})
			must(t, x.Load(p))
			if f := x.Validate(p); len(f) != 0 {
				t.Fatalf("validate: %+v", f)
			}

			c.mess(t, root)
			before := snapshot(t, root)

			err = x.Check()
			var ce *ChangedError
			if !errors.As(err, &ce) {
				t.Fatalf("want *ChangedError, got %T: %v", err, err)
			}
			if !strings.Contains(ce.Error(), "nothing was written") {
				t.Errorf("message = %q", ce.Error())
			}
			assertUnchanged(t, root, before)
		})
	}
}

func TestEOL(t *testing.T) {
	t.Run("an LF payload edits a CRLF file and it stays CRLF", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\r\ntwo\r\nthree\r\n"})
		_, err := run(t, tree, "@@ file a.go\n@@ old\ntwo\n@@ new\nTWO\nEXTRA\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "one\r\nTWO\r\nEXTRA\r\nthree\r\n" {
			t.Errorf("got %q", got)
		}
	})

	// A payload's own line endings are the CRLFs inside it, and a patch is
	// split on \n, so a payload line that ends CRLF is written as a line ending
	// in \r. The blank line before the next directive is what gives the last
	// payload line its ending at all (§3.3). Getting this wrong is how the
	// first draft of these two tests tested nothing.
	crlfPayload := func(lines ...string) string {
		return strings.Join(lines, "\r\n") + "\r\n\n"
	}

	// A heredoc from an agent's shell is always LF, so a CRLF payload means
	// somebody pasted CRLF text. Converting LF to CRLF on it would give
	// "\r\r\n"; probed during recon, and this is what keeps it fixed.
	t.Run("a CRLF payload on a CRLF file gains no extra carriage return", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\r\ntwo\r\n"})
		patch := "@@ file a.go\n@@ old\n" + crlfPayload("two") + "@@ new\n" + crlfPayload("TWO")
		_, err := run(t, tree, patch, Options{})
		must(t, err)
		got := readFile(t, root, "a.go")
		if strings.Contains(got, "\r\r") {
			t.Errorf("doubled carriage return: %q", got)
		}
		if got != "one\r\nTWO\r\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a CRLF payload edits an LF file and is converted down", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\ntwo\n"})
		patch := "@@ file a.go\n@@ old\n" + crlfPayload("two") + "@@ new\n" + crlfPayload("TWO")
		_, err := run(t, tree, patch, Options{})
		must(t, err)
		if got := readFile(t, root, "a.go"); got != "one\nTWO\n" {
			t.Errorf("got %q", got)
		}
	})

	// A bare CR that is not a line ending is content, and byte-exact matching
	// leaves it alone in both directions.
	t.Run("a lone carriage return is content, not a line ending", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.txt": "a\rb\n"})
		_, err := run(t, tree, "@@ file a.txt\n@@ old\na\rb\n@@ new\nc\rd\n", Options{})
		must(t, err)
		if got := readFile(t, root, "a.txt"); got != "c\rd\n" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("strict does not translate, so an LF payload misses a CRLF file", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "one\r\ntwo\r\n"})
		before := snapshot(t, root)
		_, err := run(t, tree, "@@ file a.go\n@@ old\ntwo\n\n@@ new\nTWO\n\n", Options{EOL: EOLStrict})
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("strict should have missed; got %T: %v", err, err)
		}
		assertUnchanged(t, root, before)
	})

	t.Run("dominantEOL", func(t *testing.T) {
		for _, c := range []struct {
			in   string
			want string
		}{
			{"", "\n"},
			{"no newline at all", "\n"},
			{"a\nb\n", "\n"},
			{"a\r\nb\r\n", "\r\n"},
			{"a\r\nb\r\nc\n", "\r\n"},
			{"a\r\nb\nc\n", "\n"},
			{"a\r\nb\n", "\n"}, // a tie goes to LF
		} {
			if got := dominantEOL([]byte(c.in)); got != c.want {
				t.Errorf("dominantEOL(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	})

	t.Run("toEOL is idempotent in both directions", func(t *testing.T) {
		for _, in := range []string{"a\nb", "a\r\nb", "a\r\nb\nc", ""} {
			for _, eol := range []string{"\n", "\r\n"} {
				once := toEOL([]byte(in), eol)
				twice := toEOL(once, eol)
				if string(once) != string(twice) {
					t.Errorf("toEOL(%q, %q) = %q, applying twice = %q", in, eol, once, twice)
				}
			}
		}
	})
}

// §5.1's example is self-consistent under exactly this rule: added is the
// number of lines in new, removed the number in old, summed per file. Its
// "3 files, 4 hunks, +8 -3" is 2+2+4 and 1+2+0.
func TestLineCountAndDiffstat(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"\n", 1},
		{"\n\n", 2},
	} {
		if got := lineCount([]byte(c.in)); got != c.want {
			t.Errorf("lineCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	tree, _ := fixture(t, map[string]string{"a.go": "one\ntwo\nthree\n"})
	r, err := run(t, tree, "@@ file a.go\n@@ old\none\n@@ new\nONE\nEXTRA\n", Options{})
	must(t, err)
	if r.Files[0].Added != 2 || r.Files[0].Removed != 1 {
		t.Errorf("diffstat = +%d -%d, want +2 -1", r.Files[0].Added, r.Files[0].Removed)
	}
}

// §8.1's property test, with the two conditions the recon found it was missing.
// A randomly extracted span is not necessarily unique, so the hunk carries the
// count it actually occurs at; and the replacement is drawn from an alphabet
// disjoint from the file's, so inverting it cannot match more than was written.
func TestApplyThenInvertIsIdentity(t *testing.T) {
	const fileAlpha = "abc \n\t{}"
	const replAlpha = "XYZ"
	rng := rand.New(rand.NewPCG(1, 2))

	pick := func(alpha string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(alpha[rng.IntN(len(alpha))])
		}
		return b.String()
	}

	for i := 0; i < 400; i++ {
		body := pick(fileAlpha, 1+rng.IntN(120))
		if len(body) < 2 {
			continue
		}
		start := rng.IntN(len(body) - 1)
		end := start + 1 + rng.IntN(len(body)-start)
		span := body[start:end]
		repl := pick(replAlpha, 1+rng.IntN(4))
		n := strings.Count(body, span)
		if n == 0 {
			t.Fatalf("extracted span %q does not occur in %q", span, body)
		}

		tree, root := fixture(t, map[string]string{"f.txt": body})
		// A payload is the lines between two directives joined by \n, so
		// emitting the span followed by a newline encodes it exactly.
		fwd := "@@ file f.txt\n@@ old x" + itoa(n) + "\n" + span + "\n@@ new\n" + repl + "\n"
		if _, err := run(t, tree, fwd, Options{}); err != nil {
			t.Fatalf("apply %q -> %q in %q: %v", span, repl, body, err)
		}
		back := "@@ file f.txt\n@@ old x" + itoa(n) + "\n" + repl + "\n@@ new\n" + span + "\n"
		if _, err := run(t, tree, back, Options{}); err != nil {
			t.Fatalf("invert %q -> %q in %q: %v", repl, span, body, err)
		}
		if got := readFile(t, root, "f.txt"); got != body {
			t.Fatalf("round trip changed the file:\n span %q\n repl %q\n want %q\n got  %q",
				span, repl, body, got)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

// A hunk that matched but changed nothing must not rewrite the file: moving an
// mtime invalidates a build cache, and the whole point of the tool is to make
// the next step cheaper.
func TestANoOpHunkDoesNotTouchTheFile(t *testing.T) {
	tree, root := fixture(t, map[string]string{"a.go": "one\ntwo\n"})
	full := filepath.Join(root, "a.go")
	fi0, err := os.Stat(full)
	must(t, err)

	r, err := run(t, tree, "@@ file a.go\n@@ old\none\n@@ new\none\n", Options{})
	must(t, err)
	if len(r.Files) != 0 {
		t.Errorf("a no-op hunk was reported as a change: %+v", r.Files)
	}
	if r.Hunks != 1 {
		t.Errorf("hunks = %d, want 1", r.Hunks)
	}
	fi1, err := os.Stat(full)
	must(t, err)
	if !fi1.ModTime().Equal(fi0.ModTime()) {
		t.Error("the file was rewritten")
	}
	if got := readFile(t, root, "a.go"); got != "one\ntwo\n" {
		t.Errorf("contents changed: %q", got)
	}

	// And a real change beside a no-op still lands, with the no-op absent from
	// the diffstat.
	r, err = run(t, tree, "@@ file a.go\n@@ old\none\n@@ new\none\n@@ old\ntwo\n@@ new\nTWO\n", Options{})
	must(t, err)
	if len(r.Files) != 1 || r.Files[0].Added != 1 || r.Files[0].Removed != 1 {
		t.Errorf("diffstat = %+v, want one file at +1 -1", r.Files)
	}
	if got := readFile(t, root, "a.go"); got != "one\nTWO\n" {
		t.Errorf("got %q", got)
	}
}

func TestLoadErrorPaths(t *testing.T) {
	t.Run("a path that escapes the root is refused before anything is read", func(t *testing.T) {
		tree, _ := fixture(t, map[string]string{"a.go": "x\n"})
		_, err := run(t, tree, "@@ file ../outside.go\n@@ old\nx\n@@ new\ny\n", Options{})
		var pr *PathRefusal
		if !errors.As(err, &pr) {
			t.Fatalf("want *PathRefusal, got %T: %v", err, err)
		}
		if !strings.Contains(pr.Reason, "climbs out") {
			t.Errorf("reason = %q", pr.Reason)
		}
	})

	// Stat succeeds (the directory is traversable) and the read fails. That is
	// an I/O error, exit 5, not a validation failure: editing the patch would
	// not help, a chmod would.
	t.Run("an unreadable file is an I/O error, not a refusal", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"a.go": "x\n"})
		must(t, os.Chmod(filepath.Join(root, "a.go"), 0o000))
		t.Cleanup(func() { os.Chmod(filepath.Join(root, "a.go"), 0o644) })

		_, err := run(t, tree, "@@ file a.go\n@@ old\nx\n@@ new\ny\n", Options{})
		if err == nil {
			t.Fatal("read an unreadable file")
		}
		var pr *PathRefusal
		if errors.As(err, &pr) {
			t.Errorf("a permission problem was reported as a path refusal: %s", pr.Reason)
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("err = %v, want a permission error", err)
		}
	})

	t.Run("an unstattable parent is an I/O error too", func(t *testing.T) {
		tree, root := fixture(t, map[string]string{"locked/a.go": "x\n"})
		must(t, os.Chmod(filepath.Join(root, "locked"), 0o000))
		t.Cleanup(func() { os.Chmod(filepath.Join(root, "locked"), 0o755) })

		_, err := run(t, tree, "@@ file locked/a.go\n@@ old\nx\n@@ new\ny\n", Options{})
		if err == nil {
			t.Fatal("statted through a locked directory")
		}
		var pr *PathRefusal
		if errors.As(err, &pr) {
			t.Errorf("reported as a path refusal: %s", pr.Reason)
		}
	})
}

// Commit is the one phase that can fail after everything has been validated.
// A read-only directory reaches it without any interleaving: load reads,
// validate matches, check re-reads, and the temp file cannot be created.
func TestCommitFailureSurfaces(t *testing.T) {
	tree, root := fixture(t, map[string]string{"sub/a.go": "x\n"})
	must(t, os.Chmod(filepath.Join(root, "sub"), 0o555))
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "sub"), 0o755) })

	_, err := run(t, tree, "@@ file sub/a.go\n@@ old\nx\n@@ new\ny\n", Options{})
	if err == nil {
		t.Fatal("wrote into a read-only directory")
	}
	if got := readFile(t, root, "sub/a.go"); got != "x\n" {
		t.Errorf("contents changed to %q", got)
	}
}

// Run's Check branch. Driving the phases separately is the only way to get a
// writer in between, which is the point of the phase being separate.
func TestRunPropagatesACheckFailure(t *testing.T) {
	tree, root := fixture(t, map[string]string{"a.go": "one\n"})
	p, err := Parse([]byte("@@ file a.go\n@@ old\none\n@@ new\nONE\n"), DefaultMarker)
	must(t, err)
	x := NewTxn(tree, Options{})
	must(t, x.Load(p))
	x.Validate(p)
	must(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("else\n"), 0o644))

	if err := x.Check(); err == nil {
		t.Fatal("check passed over a changed file")
	}
	if err := x.Commit(); err != nil {
		t.Fatalf("commit after a failed check: %v", err)
	}
	// Run refuses the same patch on a second Txn, having re-read the file.
	if _, err := run(t, tree, "@@ file a.go\n@@ old\none\n@@ new\nONE\n", Options{}); err == nil {
		t.Error("the stale patch applied")
	}
}

func TestErrorMessages(t *testing.T) {
	ce := &ChangedError{Path: "b.go"}
	for _, want := range []string{"b.go", "between being read and being written", "nothing was written"} {
		if !strings.Contains(ce.Error(), want) {
			t.Errorf("%q missing %q", ce.Error(), want)
		}
	}
	ve := &ValidationError{Failures: []Failure{{Hunk: 1}, {Hunk: 2, SkippedAfter: 1}}}
	if got := ve.Error(); !strings.Contains(got, "1 hunks did not match") || !strings.Contains(got, "1 skipped") {
		t.Errorf("summary = %q", got)
	}
	if got := (&ValidationError{Failures: []Failure{{Hunk: 1}}}).Error(); strings.Contains(got, "skipped") {
		t.Errorf("no skips should mean no skip clause: %q", got)
	}
}

// §2 goal 3 is "All or nothing": after a failure the tree is byte-identical to
// before. That is the strongest invariant in the design, the patch is
// agent-written text, and the two together make this the fuzz target worth
// having.
func FuzzApplyIsAllOrNothing(f *testing.F) {
	for _, s := range []string{
		"@@ file a.go\n@@ old\none\n@@ new\nONE\n",
		"@@ file a.go\n@@ old x2\nx\n@@ new\ny\n",
		"@@ file a.go\n@@ old\nabsent\n@@ new\nx\n@@ old\none\n@@ new\nX\n",
		"@@ file sub/b.go\n@@ old\nbeta\n@@ new\n\n",
		"@@ file ../escape\n@@ old\nx\n@@ new\ny\n",
		"@@ delete a.go\n",
		formatExamplePatch,
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, patch string) {
		root := t.TempDir()
		if r, err := filepath.EvalSymlinks(root); err == nil {
			root = r
		}
		if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
			t.Skip()
		}
		os.WriteFile(filepath.Join(root, "a.go"), []byte("one\ntwo\nx\nx\n"), 0o644)
		os.WriteFile(filepath.Join(root, "sub", "b.go"), []byte("alpha\r\nbeta\r\n"), 0o600)

		p, err := Parse([]byte(patch), DefaultMarker)
		if err != nil {
			return
		}
		tree, err := OpenTree(root, false)
		if err != nil {
			t.Fatal(err)
		}
		defer tree.Close()

		before := snapshot(t, root)
		r, err := NewTxn(tree, Options{}).Run(p)
		if err != nil {
			// Every failure path leaves the tree exactly as it was. The one
			// exception §6.2 admits is a commit that fails part-way, which
			// needs a write to fail and cannot happen here.
			assertUnchanged(t, root, before)
			return
		}
		if r == nil {
			t.Fatal("success with no result")
		}
		if r.Hunks != len(p.Hunks) {
			t.Errorf("result reports %d hunks, patch had %d", r.Hunks, len(p.Hunks))
		}
		// A success reports every file it changed and no file it did not.
		after := snapshot(t, root)
		reported := map[string]bool{}
		for _, fr := range r.Files {
			reported[fr.Path] = true
		}
		for name := range before {
			if before[name] != after[name] && len(reported) == 0 {
				t.Errorf("%s changed but the result reported no files", name)
			}
		}
	})
}

// An "@@ old x2" that rewrites two lines changed two lines. Counting per hunk
// would say +1 -1 and understate it; §5.1's globals.go row is +2 -2 for exactly
// this shape.
func TestDiffstatCountsPerOccurrence(t *testing.T) {
	tree, _ := fixture(t, map[string]string{"a.go": "P\nq\nP\nr\nP\n"})
	r, err := run(t, tree, "@@ file a.go\n@@ old x3\nP\n@@ new\nZ\nY\n", Options{})
	must(t, err)
	if r.Files[0].Added != 6 || r.Files[0].Removed != 3 {
		t.Errorf("diffstat = +%d -%d, want +6 -3 (three occurrences, one line each becoming two)",
			r.Files[0].Added, r.Files[0].Removed)
	}
}
