---
name: hunk
description: >-
  Edit files with `hunk`, a transactional multi-file editor whose refusals
  are the point. Use it instead of writing a Python or shell heredoc that
  reads a file, replaces text and writes it back - that shape, `python3 -
  <<'PY'` with a read/write pair, is what this tool exists to replace. Use
  it whenever an edit spans more than one replacement or more than one file,
  whenever an edit needs a build or test run after it that must undo the
  edit if it fails, and whenever the same text might occur more than once
  and hitting the wrong one would be silent. Read this before the first
  `hunk` invocation: exit 2 is the tool working, and treating it as an
  obstacle to route around is how you produce a broken tree.
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
| `expected 1 occurrence, found 0` with a span printed | Re-read the file | The report printed the file's lines. Paste that span into `old` and resend. Where it marks whitespace, a `→` is a tab and a `·` at a line's end a space: put them back as you paste. A character it cannot print is shown as `<U+001B>`. `span` in `--json` is the bytes as they are |
| `... 3 more lines not shown (--context 3)` under the span | Paste what is shown | The span is cut. Rerun with the `--context` it names, then paste the whole span; a cut one replaces only part of the block |
| `also nearly there at line 6` | Paste and hope it is the copy you meant | The span shown is one of several. Pasted, it edits that copy; if you meant another, or it says some have the same bytes, add a line of context that only your copy has |
| `expected 1 occurrence, found 5` with line numbers | Pick one and hope | Add surrounding context to `old` until it is unique, or say `@@ old x5` if you mean all five |
| `found 2 that overlap`, or `found 2 across both line endings` | Say `@@ old x2` | Add a line of context until `old` fits one place. Overlapping copies share lines, and a copy in a file's other line ending is one this hunk cannot edit, so no count applies |
| `the text is there with different leading whitespace` | Retype it by hand | The gutter shows the real line with each tab as `→`. Copy it with every `→` a tab again, or take `span` from `--json` |
| `no anchor found` | Try a shorter `old` | Every line of your `old` was compared with every line of the file, whitespace ignored, and not one of them is there. Check the path, or that an earlier hunk did not already change it |
| `would fall above the top of the file` | Re-read the file | Your `old` overhangs the top. The line it did find is printed with its number; drop the lines above it from your `old` |
| `skipped: same file as hunk N` | Fix this hunk | It was never evaluated. Fix hunk N; this one may then be fine |
| exit 1, `--verify cannot see the command piped into tail fail` | Wrap the pipe in a subshell to get it past | Drop the pipe. The verify's output is printed only when it fails, and then only its last 40 lines |
| exit 3, `verify failed, rolled back` | Re-apply and hope | Every file is back to the same bytes and mode. The verify output is below the line; fix the patch |
| exit 4, `was not restored` | Ignore it | The tree is inconsistent. Read which file and why, then repair it before doing anything else |
| exit 5 | Assume the edit landed | Read the message. Unless it says otherwise, nothing was written, or it was put back. The cause is outside the patch: a missing shell, a name the filesystem refused, a full disk |
| exit 6, `changed on disk` | Retry the same patch | Something else wrote the tree. Re-read the file and rebuild the patch against what is there now |
| exit 1, `patch line 6 is "HUHUNK", which looks like a mistyped heredoc delimiter` | Pass `--allow-mistyped-delimiter` to get it past | Take the line out of the patch and send it again: nothing was written. If it counts lines after it, they were the rest of your command: take them out too and run them yourself. The flag is for a line you mean as text, as in a document about `hunk`; under it the patch applies and a `warning:` names the line |

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

Two kinds of line are the exception, and both are in text you can see before
you send it:

- **A payload line that begins with `@@`** is read as a directive. Inside an
  `@@ old` that fails the patch. Inside an `@@ new` or an `@@ create` it can
  parse, and then a different patch applies from the one you meant. That
  happens when you edit a file which quotes a hunk patch: a test fixture, a
  doc, this file. `--marker '%%'` changes the directive prefix for the whole
  patch and the `@@` lines become ordinary text.
- **A payload line that is exactly `HUNK`** ends the heredoc. That is the
  shell's rule, not `hunk`'s: `hunk` gets the lines above it, which may still
  be a patch that applies, and the shell runs every line below it as a
  command. Before you send, check that no payload line is the delimiter, and if
  one is, pick another word for both ends (`<<'HUNK2'` and `HUNK2`). This file
  has such lines, and so does any document about `hunk`.

The opposite slip is one you cannot see before you send: a closer the shell
does not take for `HUNK`. `HUHUNK` above the real `HUNK` is a payload line.
With no real closer, as with `HUNKK`, an indented `  HUNK` or `HUNK` and a
trailing space, the heredoc runs to the end of your command, and the rest of
the command is payload too and never runs. `hunk` refuses such a patch, exit
1, with nothing written, and names every such line. It is the commonest slip
there is, about one heredoc in sixty: resend the patch without the line.
`--allow-mistyped-delimiter` applies a patch that means one, as a document
about `hunk` may, and warns of it instead; `--json` carries each line in
`warnings` either way.

When a payload is text you have not read line by line, write the patch to a
file with your file-writing tool, outside the tree, and run `hunk -f FILE`. A
file does not pass through the shell, so no line in it can end anything.

A patch may use CRLF line endings, as a file written on Windows or text piped
from PowerShell does. A carriage return at the end of a directive line is its
ending; under the default `--eol auto`, a patch whose every line ends in CRLF
reads its payloads the same way, and `--eol strict` keeps every payload byte. A
control character anywhere else in a directive line is refused and named: in a
path with exit 2, stuck to a directive word with exit 1. So is a directive
missing its space, `@@file b.go`, or with a no-break space for one, wherever
the line is, with exit 1: as payload it would have sent every hunk after it to
the file before it. `--marker` writes such a line as text.

## The five directives

`@@ old` / `@@ new` is the one you want most of the time. The other four exist,
and an agent that has not seen this list reaches for `Write` or a shell instead:

| Directive | What it does |
| --- | --- |
| `@@ file PATH` | Sets the file the `@@ old` hunks under it apply to |
| `@@ old` / `@@ new` | Replaces literal text, exactly as many times as claimed |
| `@@ create PATH` | Makes a file, payload is its whole content. Refuses if it exists |
| `@@ delete PATH` | **Removes the whole file.** It is not a way to delete lines. Refuses a path that is a symlink; name the file it leads to |
| `@@ append PATH` | Adds the payload at the end of the file |
| `@@ prepend PATH` | Adds it at the start |

**To remove a block of lines, replace it with nothing:**

```
@@ file runtime/oneshot.zig
@@ old
    std.debug.print("probe A\n", .{});
    std.debug.print("probe B\n", .{});

@@ new
```

The blank line before `@@ new` is rule 1 below, and it is what carries the
block's last newline out with it. `old` is still the exact bytes: `hunk` will
not remove text it has not been shown, so if you no longer have what you
inserted, read it back before you write the patch.

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

The same blank line is what gives a file you create its final newline. The
payload is the whole file, so if its last line runs straight into `HUNK`, the
file has no final newline, the report says `(no final newline)`, and a
formatter run as a check (`zig fmt --check`) refuses it. The `@@ create` in the
example at the top leaves that blank line on purpose. `@@ append` and
`@@ prepend` need no blank line: they add whole lines.

**2. `@@ old` means exactly one occurrence.** `@@ old x3` means exactly three,
and replaces all three. There is no "one or more". If you do not know the count,
that is the thing `hunk` is here to refuse: add context until it is unique.
Every copy counts, including ones a left-to-right count would miss: copies that
overlap, which a run of identical lines makes, and, in a file that mixes CRLF
and LF, copies of a multi-line `old` in its other line ending. Those are refused
rather than edited, whatever the count says.

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

## Retrying a large batch: parts

A refusal writes nothing, so it costs the whole batch again. A batch is a
concatenation, though, so a large one can be kept as parts, one per target
file, and piped in together:

```
$ d=$(mktemp -d)
$ cat > "$d/1-client.hunk" <<'HUNK'
@@ file src/client.go
@@ old
...
HUNK
$ cat > "$d/2-server.hunk" <<'HUNK'
...
HUNK
$ cat "$d"/*.hunk | hunk --verify 'go build ./...'
```

It is still one batch and one transaction: a miss in any part writes nothing in
any. After a refusal, rewrite only the part that missed and run the same pipe
again. Splitting the batch into separate `hunk` calls gives up exactly that,
because the halves can land apart, and files that depend on each other do not
build in between.

Four limits:

- **End every part with a newline.** A heredoc always does. A part whose last
  line has none welds onto the next part's `@@ file` line, and the next part's
  hunks are then matched against the wrong file.
- **No `@@ end` in a part.** It ends the whole stream, and what follows is
  exit 1.
- **"patch line N" counts through the whole stream**, not within a part. The
  report names the file as well, so one part per file is enough to find it.
- **A fresh directory per batch.** The glob takes every part in it, a leftover
  one included.

## The same edit in many files

A version bump, a copyright year, a changed URL: one literal edit, repeated. It
is still a patch. Let a loop write it and pipe it in, and the batch keeps the
count guard and the transaction: if any file does not have the text exactly
once, nothing is written.

List the files first (`ls cmd/*/version.go`, `rg -l`), read the names, and
write them into the loop yourself:

```sh
{
  for f in cmd/api/version.go cmd/worker/version.go; do
    printf '@@ file %s\n' "$f"
    cat <<'HUNK'
@@ old
const Version = "1.4.0"
@@ new
const Version = "1.5.0"
HUNK
  done
  cat <<'HUNK'
@@ file CHANGELOG.md
@@ old
## 1.5.0 (unreleased)
@@ new
## 1.5.0 (2026-09-21)
HUNK
} | hunk --verify 'go build ./...'
```

**The list is yours, not a glob's.** Every path the loop prints becomes a line
of the patch, so a glob would put the repository's own file names into it. A
file name can hold a line break, or a backslash that `echo` turns into one
under `sh`, `zsh` and `dash`, and either way the name arrives as directive
lines you never wrote, which `hunk` then carries out. A name you typed holds
nothing you did not put there, and `printf '%s'` prints it as it is. If a
listing shows a name you would not have typed, leave it out and say so.

Each payload is a quoted heredoc, so nothing in it is escaped. A Python loop
that asserts and writes one file at a time rebuilds the guard and loses the
transaction: an assert that fails on the fourth file leaves the first three
changed.

## Replacing a whole block: `hunk quote`

`@@ old` wants the text it replaces. For a block you know by its line numbers,
because you have just read the file, do not paste a hundred lines and do not
print them into a patch with `sed`. `hunk quote FILE START END` prints lines
START through END as a hunk: `@@ file`, `@@ old`, and the lines.

```sh
{
  hunk quote src/parse.go 40 95 --new new/parse.go
  hunk quote src/parse.go 210 262
  cat <<'HUNK'
@@ new
func walk(t *Tree) error {
	return walkFrom(t.Root)
}
@@ file src/main.go
@@ old
	parse(src)
@@ new
	parse(src, defaults)
HUNK
} | hunk --dry-run
```

```
quoted src/parse.go 40-95, 56 lines: "func parse(src []byte) (*Tree, error) {" to "}"
quoted src/parse.go 210-262, 53 lines: "func walk(t *Tree) error {" to "}"
M src/parse.go +9 -109
M src/main.go  +1 -1
2 files, 3 hunks, +10 -110 (dry run: nothing written)
```

`--new FILE` adds `@@ new` and FILE's content. Without it the hunk stops after
the old text and you write `@@ new` and the replacement yourself, as the second
one does. It is one batch and one transaction with everything else in the pipe,
and the quoted text must still be in the file exactly once when it is applied.

**Read the `quoted` lines before you apply.** They are each range's first and
last line, and they are the only check on the numbers. The text quoted is
whatever is at those lines now, so it always matches: a number that has gone
stale since you read the file, or is off by one, quotes a different block, and
that block is replaced at exit 0. That is why the example ends in `--dry-run`.
When both ends of every range are the lines you meant, run the same pipe again
without it.

Five things to know:

- **All the numbers are the file's as it is now.** Every quote reads the file
  before anything is applied, so take them from one read and do not adjust a
  later range for what an earlier hunk will add or remove.
- **Whole lines in and out.** The `--new` file's final newline, or the lack of
  one, changes nothing, and neither does the file's.
- **An empty `--new` file deletes the lines** and leaves no blank line:
  `--new /dev/null`.
- **A line that would read as a directive is refused**, in the range or in the
  `--new` file, naming it. Pass `--marker '%%'` to every `hunk quote` and to
  the `hunk` that applies, and write the other hunks under it too.
- **A quote that refuses stops the batch.** It writes its reason into the patch
  as an `@@ end` line, so the `hunk` at the end of the pipe exits 1 with that
  reason and applies nothing. Fix that quote and run the pipe again. Its own
  exits are 1 for a bad call, 2 for a refusal about the file or the range.

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
| You are about to write `python3 - <<'PY'` that reads a file, replaces text and writes it back | `hunk`, always |
| An edit that has to be **computed**: a block built from a list, a file split at an index | Python or the shell. `hunk` takes literal bytes in and literal bytes out, and has no expression language. Compute the patch if it helps, then pipe it in |
| One literal edit repeated across files: a version bump, a year | `hunk`, with a loop that writes the patch (above) |
| Replacing or deleting a whole block you know by its line numbers | `hunk quote`, piped into `hunk --dry-run` first (above) |
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

It never edits `.git`: a path that is `.git` or inside it, in any spelling or
through any symlink, is exit 2. Change git's own files with `git`.

## `--verify` owns the outcome

`--verify CMD` runs after a successful apply, in `--root`, via `sh -c`. If it
exits non-zero, **every file goes back** and the exit is 3. You do not need a
backup and you do not need a cleanup call.

`CMD` is any shell command, run with the user's permissions, and so is
`--try`'s. A permission rule that allows `hunk` therefore allows whatever those
flags carry: do not use them to run a command you would not run directly, and
expect a careful harness to ask about them.

**Reach for it without being asked when a batch touches more than one file of a
compiled language.** `--verify 'go build ./...'` is the case in point: every
hunk can match, every file can be plausible on its own, and the batch still not
build, because the thing that broke is between the files. A helper renamed in
one file and called in another is the shape, and it is exactly what a
multi-file edit is for.

**It is not free, and this file used to say it was.** What `--verify` costs is a
second build: if compiling is your next call regardless, it compiles twice. What
you buy for that is the rollback, so it earns its cost when being in a broken
tree would be expensive, and earns least when you were about to run the same
command yourself and read the error anyway. A cheap verify (`go vet ./...`, one
package's tests) is the one to reach for reflexively; a five-minute one is a
decision.

**When you expect the verify to fail and want to fix forward**, add
`--keep-on-fail`. The changes stay on disk, the tail of the verify's output is
printed, and the exit is still 3.

**For an edit you mean to throw away**, a print statement to see a value, a
measurement, a deliberate hang (with `--timeout`), use `--try CMD` instead of a
backup and a restore. It applies the batch, runs `CMD`, puts every file back
whatever `CMD` does, prints `CMD`'s output, and exits with `CMD`'s own status,
or 124 if `--timeout` ended it. The report starts `tried` whatever the command
said, on stdout when it passed and stderr otherwise, which is how its exit 2
differs from a refused patch; a script should read `--json`'s `try.ran`.

**For a set of break-patches tried one at a time**, a mutation or
deliberate-break run, use `--try CMD --each a.hunk b.hunk ...` instead of a
shell loop reading `$?`. The loop misreads what `--try`'s exit conflates: a
patch with a typo exits 1 like an ordinary catch, so it scores as "caught"
forever, and a file not put back prints `the command exited 0` on its first
line. `--each` parses and validates every patch before the first try, applies
each alone and puts every file back, prints a line per patch on stderr as it
finishes, and stops at once if a file is not put back, where a loop would carry
on against a broken tree. The exit is the run's, never a command's: 0 means
every command exited nonzero (every break caught), 3 means a command exited 0
or timed out (read the lines), 4 means it stopped with a file not restored.
`--timeout` applies to each command separately. A script should read `--json`'s
`each.patches`.

**The command has to pass with no patch applied.** `--each` runs it once on the
tree as it is first, and one that fails there refuses the run, exit 1, nothing
tried, with its output: every patch would have read as caught. That is what
catches a command whose exit is not the suite's: `exit ${pipestatus[1]}` is
zsh's and the command runs under `sh`, a pipe's status is its last command's,
and a test filter that matches nothing exits 5 under pytest. Read the output:
the command, or the tree, is what to fix.

**To see what caught each break, give `--verify-lines N`.** Without it a caught
patch's output is not printed, only that of one that exited 0 or timed out.
With it, the last N lines of every patch's command print under its line. A
suite that names the failing test far from its end can be made to print it
last, keeping its own status as the exit: `--try 'zig build test > /tmp/out
2>&1; s=$?; grep -m3 "error: .*failed" /tmp/out; exit $s' --verify-lines 3`.
`--json` carries every patch's output either way.

**In a compiled language, add `--build CMD`.** A break that does not compile
makes the test command exit nonzero, so `--each` alone scores it as caught: Zig
and Go exit 1 for a compile error as for a failing test, and deleting one line
easily leaves an unused parameter behind. `--build` runs once on the tree as it
is (a failure there refuses the run, exit 1, nothing tried), then after each
patch and before the command; a patch whose build fails gets its own line,
`did not build`, with the compile error under it to fix the patch from, its
command is not run, and the exit is 3. Use a build that compiles the test files
too: `go vet ./...` or `go test -run '^$' ./...`, `zig test --test-no-exec
FILE`, `cargo test --no-run`; `go build ./...` and `cargo build` leave them
out.

**Pass `--timeout` whenever the command could run long**, below your tool's own
limit for the call: `--timeout 90s` under a two-minute limit. It ends the
command and everything it started, then rolls back (exit 3) or, under `--try`,
puts back (124). Always pass it with `--try` on anything meant to hang. Do not
count on your harness to stop a hung verify: Claude Code moves a command that
outlives its timeout into the background rather than killing it, and `hunk`
waits there with the batch still applied while you carry on. A SIGINT, SIGTERM
or SIGHUP does put the batch back; only SIGKILL cannot.

**Prefer the repository's own gate** (`make check`, `npm test`) over a command
you compose. `--verify` decides on the exit code, and some tools report a
problem and exit 0 anyway: `gofmt -l .` prints every unformatted file and
succeeds. A verify that cannot fail is worse than none, because it says the
tree is good.

**Do not pipe the command into a filter to shorten its output.** A pipeline's
exit status is its last command's, so `go test ./... 2>&1 | tail -5` exits 0
with every test failing. `hunk` refuses a `--verify` whose last pipeline ends
in `tail`, `head`, `tee` or `cat`, at exit 1, before anything is written. There
is nothing to shorten: the command's output is printed only when the verify
fails, and then only its last 40 lines. A pipeline that ends in `grep -q` is a
test and is left alone, and so is anything under `--try`, where a filter on the
output is the usual way to read it. `hunk` reads plain pipelines only, so a
pipe inside a subshell or a `$(...)` is still yours to get right.

One flag to know: if your verify command **rewrites files** (`gofmt -w`,
`prettier --write`, `make fmt`), pass `--verify-may-format`. Without it, a file
the formatter touched is left alone rather than reverted, and the exit is 4,
because `hunk` cannot tell a formatter from another agent writing the same tree
and will not silently discard somebody's work. Nor will it remove or overwrite a
symbolic link it did not make, or a directory it made once that holds anything
or has been replaced, or act on a file whose directory the verify replaced: it
leaves them, and the report names each.

