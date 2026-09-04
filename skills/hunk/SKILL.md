---
name: hunk
description: Edit files with `hunk`, a transactional multi-file editor whose refusals are the point. Use it instead of writing a Python or shell heredoc that reads a file, replaces text and writes it back - that shape, `python3 - <<'PY'` with a read/write pair, is what this tool exists to replace. Use it whenever an edit spans more than one replacement or more than one file, whenever an edit needs a build or test run after it that must undo the edit if it fails, and whenever the same text might occur more than once and hitting the wrong one would be silent. Read this before the first `hunk` invocation: exit 2 is the tool working, and treating it as an obstacle to route around is how you produce a broken tree.
---

# hunk

`hunk` applies literal text edits across several files as one transaction. Its
central promise is inverted from most editors:

**When the text is not there, or is there a different number of times than you
said, `hunk` refuses the whole batch and writes nothing. It never guesses which
occurrence you meant.**

Everything below follows from that. A refusal is the tool doing its job, and the
report it prints is usually enough to fix the patch without re-reading anything.

## Exit 2 is not a failure to route around

This is the one rule that matters most, because the reflex it overrides is the
strongest one you have. When `hunk` exits 2, **do not fall back to a Python
heredoc.** That fallback is exactly the thing this tool replaced, and it has
none of the guards: it will happily edit the wrong occurrence and report
success.

| You see | Do not | Do |
| --- | --- | --- |
| `expected 1 occurrence, found 0` with a span printed | Re-read the file | The report printed the file's actual bytes. Paste that span into `old` and resend |
| `expected 1 occurrence, found 5` with line numbers | Pick one and hope | Add surrounding context to `old` until it is unique, or say `@@ old x5` if you mean all five |
| `the text is there with different leading whitespace` | Retype it by hand | The gutter shows the real line. Copy it, tabs and all |
| `no anchor found` | Try a shorter `old` | Every line of your `old` was compared with every line of the file, whitespace ignored, and not one of them is there. Check the path, or that an earlier hunk did not already change it |
| `would fall above the top of the file` | Re-read the file | Your `old` overhangs the top. The line it did find is printed with its number; drop the lines above it from your `old` |
| `skipped: same file as hunk N` | Fix this hunk | It was never evaluated. Fix hunk N; this one may then be fine |
| exit 3, `verify failed, rolled back` | Re-apply and hope | The tree is back to how it was. The verify output is below the line; fix the patch |
| exit 4, `was not restored` | Ignore it | The tree is inconsistent. Read which file and why, then repair it before doing anything else |
| exit 6, `changed on disk` | Retry the same patch | Something else wrote the tree. Re-read the file and rebuild the patch against what is there now |

The near-miss report exists so that a failed match costs zero extra round trips.
Using it is the whole point.

## The format, in one call

`hunk format` prints the grammar and a worked example that applies. Run it once
if you have not seen the format; it costs about a kilobyte.

The shape:

```
hunk --verify 'go test ./...' <<'HUNK'
@@ file internal/cli/root.go
@@ old
	bind.mustHaveBoundEveryGlobal()
@@ new
	bind.mustHaveBoundEveryGlobal()
	bind.mustHaveBoundEveryScope()
@@ old x2
	GlobalProject
@@ new
	GlobalProject, GlobalScope
@@ create internal/cli/scope.go
package cli

HUNK
```

Every byte between two `@@` lines is payload. **No escaping, ever**: no JSON
strings, no shell quoting of the text, no backslash-n. Paste the source as it
is.

## The four rules that catch people

**1. A payload never gains a trailing newline.** To match text that ends in a
newline, leave a blank line before the next directive. This is the one subtle
thing in the format:

```
@@ old
func foo() {

@@ new
func foo(ctx context.Context) {

```

**2. `@@ old` means exactly one occurrence.** `@@ old x3` means exactly three,
and replaces all three. There is no "one or more". If you do not know the count,
that is the thing `hunk` is here to refuse: add context until it is unique.

**3. Hunks apply in order, against the file as earlier hunks left it.** So a
later hunk may match text an earlier one created. A hunk that fails stops the
later hunks *against that file* and they are reported skipped, not failed.

**4. `@@ file` stays in effect** until the next path-bearing directive, so
several `@@ old`/`@@ new` pairs can follow one `@@ file`.

## Composing a large batch: `--dry-run`

`--dry-run` matches every hunk and writes nothing:

```
$ hunk --dry-run -f patch.txt
M internal/cli/root.go +3 -1
1 file, 2 hunks, +3 -1 (dry run: nothing written)
```

It runs the whole validation phase, so a near miss is reported in full and the
exit is still 2 when a hunk does not match. A twelve-hunk batch can be made
right before a byte is written. It does **not** run `--verify`, because there is
nothing applied to verify.

## When to reach for it, and when not

`hunk` earns its keep when an edit has something to go wrong: several changes
that must land together, a target that might occur more than once, or a build
that has to pass or the whole thing goes back. Those are the cases where the
alternative is a heredoc plus a guard plus a backup plus a cleanup call.

**It is not free.** Writing the patch, running it, and reading the result costs
a few more calls than `Edit` does. For a single unambiguous string in one file
with nothing to verify, that is a few calls spent buying a guard you did not
need, and `Edit` is the better tool. Reaching for `hunk` reflexively is the
mirror of the habit this replaces, and it costs the same thing: round trips.

| Situation | Use |
| --- | --- |
| You are about to write `python3 - <<'PY'` to edit a file | `hunk`, always |
| Several replacements, or several files, that must land together | `hunk` |
| An edit that must be undone if the tests fail | `hunk --verify '...'` |
| More than one file of a compiled language in one batch | `hunk --verify 'go build ./...'` |
| A batch big enough that you want to see it match first | `hunk --dry-run`, then the same patch without it |
| The text might occur more than once | `hunk`, which refuses rather than guessing |
| One unambiguous string, one file, nothing to verify | `Edit`. Not this tool |
| Reading, searching, renaming, moving | Not this tool |

## Exit codes and limits

`0` applied, `1` bad usage or patch, `2` a hunk did not match, `3` verify failed
and the tree went back, `6` the file changed underneath you. Codes `4` and `5`,
and what the tool does **not** guarantee, are in `references/exit-codes.md`;
read it if you meet one.

`--json` gives the same information as one object, including the near-miss span,
so a program never has to parse the text.

## `--verify` owns the outcome

`--verify CMD` runs after a successful apply, in `--root`, via `sh -c`. If it
exits non-zero, **every file goes back** and the exit is 3. You do not need a
backup and you do not need a cleanup call.

**Reach for it without being asked when a batch touches more than one file of a
compiled language.** `--verify 'go build ./...'` is the case in point: every
hunk can match, every file can be plausible on its own, and the batch still not
build, because the thing that broke is between the files. A helper renamed in
one file and called in another is the shape, and it is exactly what a
multi-file edit is for. The flag costs nothing when it passes and saves a
broken tree when it does not.

**Prefer the repository's own gate** (`make check`, `npm test`) over a command
you compose. `--verify` decides on the exit code, and some tools report a
problem and exit 0 anyway: `gofmt -l .` prints every unformatted file and
succeeds. A verify that cannot fail is worse than none, because it says the
tree is good.

One flag to know: if your verify command **rewrites files** (`gofmt -w`,
`prettier --write`, `make fmt`), pass `--verify-may-format`. Without it, a file
the formatter touched is left alone rather than reverted, and the exit is 4,
because `hunk` cannot tell a formatter from another agent writing the same tree
and will not silently discard somebody's work.

