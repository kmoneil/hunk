#!/usr/bin/env python3
"""Tests for the grader.

The grader has been wrong four times and every fix moved a published figure:
it matched `\\bhunk\\b`, which the workspace path satisfies; it counted `sed` on
the agent's own transcript as a fallback; it counted `hunk --version` probes as
usage, which hid a fix working; and it missed `hunk < patch` stdin redirects.
Each was found by a person reading a table by eye, after the number had been
reported.

These tests are that reading, mechanised. They build synthetic runs of every
shape case 9 can produce and assert what the grader says about each, so a fifth
mistake costs a test run rather than a published number. `make skill-grade`
runs them before it grades anything.

Run: python3 skills/hunk/evals/grade_test.py
"""
import contextlib, io, json, os, sys, tempfile, unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import grade

# The pristine fixture case 9 is scaffolded from. Kept here rather than read
# from a workspace because skills/*-workspace/ is gitignored and regenerated
# per iteration: a test that depends on it passes only on the machine that ran
# the last iteration.
FIXTURE = {
    "go.mod": "module fix\n\ngo 1.26.4\n",
    "main.go": '''package main

import (
	"fmt"

	"fix/greeter"
)

func main() {
	fmt.Println(greeter.Greet("world"))
}
''',
    "greeter/greeter.go": '''package greeter

// Greet returns a greeting.
func Greet(name string) string {
	return "hello, " + name
}
''',
    "greeter/validate.go": '''package greeter

import "errors"

func Validate(name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	return nil
}
''',
    "greeter/version.go": 'package greeter\n\nconst Version = "0.3.1"\n',
    "greeter/tidy.go": "package greeter\n\nfunc Tidy(x int) int {\n\treturn x * 2\n}\n",
    "greeter/greeter_test.go": '''package greeter

import "testing"

func TestValidate(t *testing.T) {
	if Validate("x") != nil {
		t.Fatal("want nil")
	}
}

func TestTidy(t *testing.T) {
	if Tidy(2) != 4 {
		t.Fatal("want 4")
	}
}
''',
}

SOLVED_GREETER = '''package greeter

// Greet returns a greeting.
func Greet(greeting, name string) string {
	return greeting + ", " + name
}
'''

SOLVED_MAIN = '''package main

import (
	"fmt"

	"fix/greeter"
)

func main() {
	fmt.Println(greeter.Greet("hello", "world"))
}
'''

# Transcripts, in the shape the runs write them: a numbered list of commands.
IDEAL = """# Commands run, in order

1. `grep -rn "Greet" .` - exit 0
2. the edit, as one transaction - exit 0

```
hunk --verify 'go build ./...' <<'HUNK'
@@ file greeter/greeter.go
@@ old
func Greet(name string) string {
@@ new
func Greet(greeting, name string) string {
HUNK
```
"""
NO_VERIFY = "1. `grep -rn Greet .` - exit 0\n2. `hunk -f /tmp/p.txt` - exit 0\n"
HOLLOW_VERIFY = "1. `hunk --verify 'gofmt -l .' -f /tmp/p.txt` - exit 0\n"
TRUE_VERIFY = "1. `hunk --verify 'true' -f /tmp/p.txt` - exit 0\n"
HEREDOC = "1. `python3 - <<'PY'` ... `PY` - exit 0\n"
EDIT_ONLY = "1. `grep -rn Greet .` - exit 0\n2. used the Edit tool on both files\n"


def make_run(root, files, transcript):
    """Scaffold one run directory: a repo tree and the transcript it left."""
    os.makedirs(f"{root}/outputs", exist_ok=True)
    for path, body in files.items():
        full = f"{root}/repo/{path}"
        os.makedirs(os.path.dirname(full), exist_ok=True)
        write(full, body)
    write(f"{root}/outputs/transcript.md", transcript)
    return root


def write(path, body):
    with open(path, "w") as f:
        f.write(body)


def solved(**over):
    f = dict(FIXTURE)
    f["greeter/greeter.go"] = SOLVED_GREETER
    f["main.go"] = SOLVED_MAIN
    f.update(over)
    return f


def by_text(expectations):
    return {e["text"]: e["passed"] for e in expectations}


class Case9(unittest.TestCase):
    """Case 9 asks for no verification, so the assertion that matters is the
    one about a flag nobody mentioned. Each shape below is a way a run can
    arrive, and the grader has to tell them apart."""

    def grade9(self, files, transcript):
        with tempfile.TemporaryDirectory() as d:
            return by_text(grade.grade(9, make_run(d, files, transcript)))

    def test_shapes(self):
        for name, files, transcript, want in [
            ("the run it is written for: batched and gated on a build",
             solved(), IDEAL, {}),
            ("correct, and never reached for the flag",
             solved(), NO_VERIFY, {"gated it on a build or a test, unasked": False}),
            ("a verify that lists problems and exits 0 is not a verify",
             solved(), HOLLOW_VERIFY, {"gated it on a build or a test, unasked": False}),
            ("--verify true is not a verify",
             solved(), TRUE_VERIFY, {"gated it on a build or a test, unasked": False}),
            # The teeth. A missing argument is a compile error; an extra
            # *return* value would not have been, which is how the first draft
            # of this case turned out to have none.
            ("only greeter.go edited, which is what the verify would catch",
             solved(**{"main.go": FIXTURE["main.go"]}), IDEAL,
             {"main.go passes the greeting word": False,
              "the module still builds": False}),
            ("fell back to a python heredoc",
             solved(), HEREDOC,
             {"used hunk": False,
              "gated it on a build or a test, unasked": False,
              "did not fall back to a heredoc or sed": False}),
            # The shape the outcome/tool split exists for: a baseline arm can
            # do the job perfectly and cannot pass either tool assertion.
            ("did the job with the Edit tool and no hunk at all",
             solved(), EDIT_ONLY,
             {"used hunk": False,
              "gated it on a build or a test, unasked": False}),
            ("collateral damage to a file the task never named",
             solved(**{"greeter/version.go": 'package greeter\n\nconst Version = "0.4.0"\n'}),
             IDEAL, {"nothing unrelated changed": False}),
        ]:
            with self.subTest(name):
                got = self.grade9(files, transcript)
                expected = {k: want.get(k, True) for k in got}
                self.assertEqual(got, expected)

    def test_the_outcome_and_tool_split(self):
        with tempfile.TemporaryDirectory() as d:
            ex = grade.grade(9, make_run(d, solved(), EDIT_ONLY))
        self.assertEqual(grade.tally(ex, "tool"), (0, 2),
                         "a run with no hunk must fail both tool-choice assertions")
        self.assertEqual(grade.tally(ex, "outcome"), (6, 6),
                         "and pass every outcome assertion, which is the point of the split")


class Helpers(unittest.TestCase):
    """The two functions that read a transcript. Both have been wrong."""

    def transcript(self, text):
        with tempfile.TemporaryDirectory() as d:
            os.makedirs(f"{d}/outputs")
            write(f"{d}/outputs/transcript.md", text)
            return d, grade.used_hunk(d), grade.verified(d)

    def test_what_counts_as_using_hunk(self):
        # The table the README says this function carries. Four of these rows
        # are the four times it was wrong.
        for text, want in [
            ("`hunk <<'HUNK'`", True),
            ("`hunk -f /tmp/p.txt`", True),
            ("`hunk --verify 'go test ./...' -f p.txt`", True),
            ("`hunk < patch.txt`", True),
            ("`command -v hunk`", False),
            ("`hunk --help`", False),
            ("`hunk --version`", False),
            ("`cd /workspace/hunk/skills/hunk-workspace/eval-9 && ls`", False),
            ("I considered hunk and used Edit instead", False),
        ]:
            with self.subTest(text):
                self.assertEqual(self.transcript(text)[1], want)

    def test_what_counts_as_a_verify(self):
        for text, want in [
            ("`hunk --verify 'go build ./...' -f p.txt`", True),
            ('`hunk --verify "go test ./..." -f p.txt`', True),
            ("`hunk --verify 'make check' -f p.txt`", True),
            ("`hunk --verify 'go vet ./...' -f p.txt`", True),
            # Exits 0 while listing the problem. This project shipped it.
            ("`hunk --verify 'gofmt -l .' -f p.txt`", False),
            ("`hunk --verify 'true' -f p.txt`", False),
            ("`hunk --verify 'echo ok' -f p.txt`", False),
            ("`hunk -f p.txt`", False),
            ("`hunk --verify-may-format --verify 'go test ./...' -f p.txt`", True),
        ]:
            with self.subTest(text):
                self.assertEqual(self.transcript(text)[2], want)


class Instrument(unittest.TestCase):
    """The grader's own bookkeeping: which cases exist, and which runs it will
    agree to score."""

    def test_every_assertion_declares_a_kind(self):
        with tempfile.TemporaryDirectory() as d:
            r = make_run(d, solved(), IDEAL)
            for eid in grade.CASES:
                for e in grade.grade(eid, r):
                    self.assertIn(e["kind"], ("outcome", "tool"), f"case {eid}: {e['text']}")

    def test_the_case_list_comes_from_evals_json(self):
        with open(f"{grade.HERE}/evals.json") as f:
            cases = json.load(f)["evals"]
        self.assertEqual(sorted(grade.CASES), sorted(e["id"] for e in cases))
        self.assertIn(9, grade.CASES, "case 9 is the flag-choice case")

    def scaffold(self, root, eid, name, files=None, transcript=IDEAL):
        d = f"{root}/eval-{eid}"
        os.makedirs(d, exist_ok=True)
        if name is not None:
            write(f"{d}/eval_metadata.json", json.dumps({"eval_id": eid, "eval_name": name}))
        make_run(f"{d}/with_skill/run-1", files or solved(), transcript)

    def test_a_run_is_graded_when_the_workspace_agrees_with_evals_json(self):
        with tempfile.TemporaryDirectory() as root:
            self.scaffold(root, 9, "unprompted-verify")
            self.assertEqual(len(grade.collect(root)), 1)

    def refuse(self, name):
        """Collect from a workspace that should be refused, and return what the
        grader said about it. Silence is not good enough: a run that is not
        graded has to say so, or a table with a case missing looks complete."""
        with tempfile.TemporaryDirectory() as root:
            self.scaffold(root, 9, name)
            said = io.StringIO()
            with contextlib.redirect_stdout(said):
                rows = grade.collect(root)
            self.assertEqual(rows, [])
            return said.getvalue()

    def test_a_run_under_a_reused_id_is_refused(self):
        # iteration-3's eval-9 is four-small-edits, which never landed. Grading
        # it by directory number would score one case against another's
        # assertions and print a table that looks fine.
        said = self.refuse("four-small-edits")
        self.assertIn("four-small-edits", said)
        self.assertIn("unprompted-verify", said)

    def test_a_run_with_no_metadata_is_refused(self):
        self.assertIn("eval_metadata.json", self.refuse(None))

    def test_a_case_not_run_in_this_iteration_is_silently_skipped(self):
        with tempfile.TemporaryDirectory() as root:
            self.scaffold(root, 9, "unprompted-verify")
            rows = grade.collect(root)  # cases 0-8 have no directory at all
            self.assertEqual([r[0] for r in rows], [9])


if __name__ == "__main__":
    unittest.main(verbosity=2)
