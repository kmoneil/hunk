# hunk

<p align="center">
  <a href="https://github.com/kmoneil/hunk/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/kmoneil/hunk/actions/workflows/ci.yml/badge.svg"></a>
  <a href="LICENSE"><img alt="Licence" src="https://img.shields.io/badge/licence-Apache--2.0-blue"></a>
  <a href="go.mod"><img alt="Go" src="https://img.shields.io/badge/go-1.26%2B-00ADD8"></a>
  <a href="go.mod"><img alt="Dependencies" src="https://img.shields.io/badge/dependencies-0-brightgreen"></a>
</p>

A transactional multi-file text editor, built for coding agents rather than for
people.

One invocation carries many literal-text edits across many files, plus a command
that says whether they were right. Nothing is written until every edit is known
to match exactly as many times as it claimed to. If the command then fails,
every file goes back to the bytes and mode it had.

```sh
hunk --verify 'go build ./... && go test ./...' <<'HUNK'
@@ file internal/cli/root.go
@@ old
	bind.mustHaveBoundEveryGlobal()
@@ new
	bind.mustHaveBoundEveryGlobal()
	bind.mustHaveBoundEveryScope()
@@ file internal/cli/scope.go
@@ old x2
	GlobalProject
@@ new
	GlobalProject, GlobalScope
HUNK
```

```
M internal/cli/root.go  +2 -1
M internal/cli/scope.go +2 -2
2 files, 2 hunks, +4 -3, verify ok (1.9s)
```

## Why this exists

Agents edit files by writing a shell heredoc that reads a file, replaces some
text, and writes it back. Across 30 of my own project histories I counted
**5,834 such edits**, making 11,964 replacements. Of those calls:

| | |
| --- | --- |
| used `.replace(old, new, 1)` | **41%** |
| had no uniqueness check at all | **43%** |
| bundled a build or test command after the edit | **58%** |

The first row is the expensive one. `replace(old, new, 1)` does not mean "there
is one match". It means "change whichever match comes first", and when the text
occurs twice it silently edits the wrong one and reports success. Two in five
measured edits were exposed to that.

The last row says the intent was already there: most edits were followed by a
check. What was missing was a way to make the check decide the outcome, so
agents hand-rolled backups and restore lines, and 5% of the time they wrote a
`.bak` file and a `cp` to undo it.

The missing primitive is not string replacement. It is the transaction.

## Refusing is the feature

Most editors try to be helpful when the text does not match. This one stops.

An edit whose target does not occur exactly the expected number of times fails
the **whole batch**, before anything is written. There is no "one or more" and
no "closest match". If you do not know how many times the text occurs, that is
precisely the case this tool exists to refuse.

A refusal costs nothing only if the report is good enough to fix the patch
without opening the file again, so the report prints the bytes that are actually
there:

```
hunk: 1 hunk did not match; nothing was written

hunk 1  server.go  (patch line 2)
  expected 1 occurrence, found 0

  the text is there with different leading whitespace, at line 42:
     42 | →if err != nil {
     43 | →→return fmt.Errorf("listen: %w", err)
     44 | →}
  your old indents with spaces and the file with tabs, 4 spaces to a tab
```

The `→` are real tabs, made visible because the difference *is* whitespace and
two identical-looking lines would help nobody. The fix is a paste, not a
re-read.

It names the cause where it can: a tab against spaces, a trailing space, CRLF
against LF, or a curly quote and a no-break space copied out of rendered
Markdown. Where it cannot name a cause it still prints the closest span in the
file and the first line that differs, with the column. When the text occurs too
many times it prints every line number and suggests the count to write.

## The format

Not a diff. There are no line numbers, no context lines, and no `-`/`+`
markers. You paste the exact text you want replaced.

```
@@ file path/to/file.go
@@ old
the exact text to find
@@ new
what to replace it with
```

Every byte between two directive lines is payload. **There is no escaping
anywhere**: no JSON strings, no backslash-n, no shell quoting of the text. That
is the property that makes a heredoc work, and it is why an agent can emit a
patch without a serialisation step that can itself be wrong.

Six directives:

| directive | does |
| --- | --- |
| `@@ file PATH` | sets the target for the hunks that follow |
| `@@ old` / `@@ new` | replace text. `@@ old x3` means exactly three occurrences, and replaces all three |
| `@@ create PATH` | create a file, with the payload as its contents, at mode 0644 less the umask |
| `@@ delete PATH` | delete a file |
| `@@ append PATH` | append the payload |
| `@@ prepend PATH` | prepend the payload |

`@@ delete` removes the whole file. To remove a block of lines, replace it with
an empty `@@ new`, which with rule 1 below takes the block's last newline with
it.

Every other directive writes through a symlink to the file it leads to, and
leaves the link in place. `@@ delete` refuses a path that is itself a symlink,
because deleting through it would remove a file the patch never named and leave
the link behind; the refusal names the file it leads to. Name that file to
delete it, or remove the link with `rm`. A symlink in a directory above the
file is fine: `@@ delete` removes the file the path names.

The report names each path as you wrote it and, when it led to a file under
another name, that name as well:

```
M link.txt +1 -1  (it resolves to real.txt)
```

That is the name version control knows, and the link's is not: after an edit
through `link.txt`, `git diff link.txt` shows nothing and `git checkout
link.txt` restores nothing. A spelling the directory stores another way is
named the same way, such as `sub/README.MD` for `Sub/README.md` on a filesystem
that folds case. A file rollback could not put back carries the same clause,
since the report sends you to version control for it, and `--json` gives the
name as `"resolved"`. Under `--allow-outside-root` a file outside the root is
named by its absolute path, so a write that left the root says where it went.

Every path is resolved against `--root`, the current directory unless one is
given. An absolute path is under the root when it names the root's directory by
any spelling, so on macOS `/tmp/x/f.txt` and `/private/tmp/x/f.txt` are the same
file. A symlink that stays inside the root is followed wherever it stands on
the path, whether it holds an absolute path or a relative one. A path that
leads outside, through `..` or a symlink, is refused with exit 2, and the
refusal names the symlink when there is one, and the root as you gave it.
A `..` written after a symlink in a patch's path is refused too, when
following the link and cancelling it as text name two different files: the
refusal prints both readings, so name the file itself.
`--allow-outside-root` lifts that for every path in the batch, not only the one
that needed it, and those paths are then opened by name rather than through the
handle `hunk` otherwise holds on the root: a symlink on any of them is followed
wherever it leads at the moment it is used. Give a batch that has to reach
outside the flag on its own, apart from the rest of the edit.

hunk never edits `.git`, which is git's own and not part of the working tree.
A path that is `.git` or inside it is refused with exit 2, whether `.git` is a
directory or the `gitdir:` file of a worktree or submodule, whichever symlink
leads there, and in any spelling git itself treats as `.git` on some
filesystem: another case, trailing dots or spaces, the Windows short name
`GIT~1`. With `--root` inside a `.git`, every path is refused. `.gitignore`,
`.github/` and the rest of the tree are edited as usual. The rule is about the
name, so a repository kept elsewhere, a bare `repo.git` or one named by
`GIT_DIR`, is not recognised.

hunk edits files. A path that names a directory, a named pipe, a socket or a
device is refused with exit 2, for every directive, before anything is read
from it: a named pipe is never waited on, never read, and never replaced by a
regular file.

An `@@ old` whose count cannot see every copy of its text is refused with exit
2, never applied to the copies it did see. Copies that overlap share lines, as
two copies of a two-line `old` do in a run of three identical lines, so no edit
can take one without the other. And under `--eol auto`, which translates the
payload to the file's dominant line ending before counting, an `old` that spans
a line break is also looked for in a file's other ending. Either way the report
lists every copy's line, for `@@ old x2` as for a bare `@@ old`, and the fix is a
line of context that makes `old` fit one place.

Three rules worth knowing before the first patch:

1. **A payload never gains a trailing newline.** To match text that ends in a
   newline, leave a blank line before the next directive. `@@ append` and
   `@@ prepend` add whole lines, so they need no blank line: a payload with no
   trailing newline gets one when it is written, and `@@ append` first ends the
   file's last line if it has none. `@@ create` writes its payload exactly, so
   leave the blank line if the new file should end in a newline. The report
   says `(no final newline)` when it does not.
2. **Hunks apply in order, against the file as earlier hunks left it.** So a
   later hunk can match text an earlier one created. A hunk that fails stops the
   later hunks against *that file*, which are reported as skipped rather than
   failed, so you fix one real mismatch instead of chasing its echoes.
3. **A line is a directive only if** it starts with `@@` at column 0, the next
   character is a space or tab, and the first word is one of the directive
   names. Everything else is payload, so a Markdown file full of `@@` passes
   through untouched, and so does a unified diff pasted into a payload. If your
   payload really does contain a line like `@@ old`, `--marker` picks a
   different prefix for the whole patch.

A patch may end its lines with CRLF, as PowerShell does when it pipes text: one
carriage return at the end of a directive line is the line's ending, not part
of a path or a directive. Under the default `--eol auto`, a patch whose every
line ends in CRLF has its payload read the same way, so it matches an LF file
and creates a CRLF one; `--eol strict` keeps every payload byte as written. A
control character anywhere else in a directive line is refused and named
rather than printed: in a path, such as an escape sequence or a carriage return
inside the name, with exit 2, and stuck to a directive word, such as `@@ end`
followed by a form feed, as a syntax error with exit 1.

The heredoc around a patch has one rule of its own, and it is the shell's, not
`hunk`'s: **a payload line that is exactly the delimiter ends the heredoc.**
With `<<'HUNK'`, a line reading `HUNK` in the text you paste stops the patch
there. `hunk` gets the lines above it, which may still be a patch that applies,
and the shell runs every line below it as a command. Check the payload for the
delimiter before sending, and if it is there, choose another word for both
ends. When the payload is text you have not read line by line, write the patch
to a file and run `hunk -f FILE` instead: a file does not go through the shell,
so no line in it can end anything.

`hunk format` prints the full grammar and a worked example that applies.

## Replacing a whole block: `hunk quote`

`@@ old` wants the text it replaces, and for a block of a hundred lines that
means pasting a hundred lines, which is slow and is where mistakes get in. When
you know the block by its line numbers, `hunk quote FILE START END` prints it
as a hunk instead: `@@ file FILE`, `@@ old`, and lines START through END,
counted from 1.

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

`--new FILE` adds `@@ new` and FILE's content, so the range is replaced by it.
Without it the hunk stops after the old text, and you write `@@ new` and the
replacement yourself, as the second one above does. Either way what comes out
is a patch like any other: concatenate it with other hunks, pipe it in, and it
is one batch and one transaction.

Nothing about applying changes, and `hunk quote` writes nothing to the tree. The
quoted text still has to be in the file exactly once when the batch is applied,
so a range too short to be unique is refused there, and so is a file that
changed in between.

**What it cannot check is the numbers.** The text it quotes is whatever is at
those lines now, so it always matches. A line number that has gone stale since
you read the file, or is off by one, quotes a different block, and that block
is then replaced at exit 0. So each range's first and last line are printed on
stderr, the `quoted` lines above, and the way to use them is to run the batch
with `--dry-run` first, read them, and run it again without. Every range is
read before anything is applied, so all the numbers are the file's as it is
now, not as an earlier hunk in the batch will leave it.

What it decides for you, so that the result is whole lines whichever file ends
in a newline:

- **A range is whole lines and so is the replacement.** `old` is the lines
  without the last one's line ending and `new` is the `--new` file without its
  last one, so a file or a replacement with no final newline comes out the same
  as one with it, and a CRLF file stays CRLF under the default `--eol auto`.
- **An empty `--new` file deletes the lines**, endings included, leaving no
  blank line behind. `--new /dev/null` is one.
- **A line that would read as a directive is refused**, in the range or in the
  `--new` file, naming the file and the line. A replacement that quotes
  `@@ delete b.txt` must not delete `b.txt`. Give `--marker` another prefix, to
  every `hunk quote` and to the `hunk` that applies the batch, and write the
  batch's other hunks under it too.
- **`FILE` is found as the apply will find it**: relative to `--root`, and
  refused for the same reasons in the same words.

**A refusal is written into the patch as well as to stderr.** The `hunk` at the
end of a brace group cannot see a command inside it fail, so a quote that
refused and printed nothing would leave the rest of the batch to apply without
it. Instead it prints an `@@ end` line that carries the reason. `@@ end` takes
no argument, so the batch stops at exit 1 with nothing written, and says why:

```
hunk: patch line 66: "@@ end" takes no argument; found: refused by hunk quote: src/parse.go has 180 lines, so there is no line 210
```

`hunk quote` exits 1 for a call that is wrong whatever the tree holds (a flag,
a number, START after END, a `--new` file that cannot be read), 2 for what the
tree decides (a FILE the apply would refuse, a line past the end, a directive
line, a range that is one empty line), and 5 for an I/O error. `hunk quote
--help` has the whole of it.

## Verify, and rollback

`--verify CMD` runs after a successful apply, from `--root`, through `sh -c`.
If it exits non-zero, every file goes back and so does the exit code. You do not
need a backup and you do not need a cleanup step. To keep the changes instead,
and read what broke before fixing forward, pass `--keep-on-fail`; the exit is
still 3. If the command cannot start at all, with no `sh` on the `PATH` for
instance, nothing is written and the exit is 5: an edit that was never checked
is not kept.

`CMD` is any shell command, run with your permissions, and so is `--try`'s:
that is what the two flags are for. So a harness rule that allows `hunk` allows
every command those flags carry. An agent that may run `hunk` without asking may
run anything through `--verify`, and a rule should be written knowing that.

This is the flag worth reaching for whenever a batch touches more than one file
of a compiled language. Every hunk can match, every file can be plausible on its
own, and the batch can still not build, because what broke is *between* the
files.

`--verify` decides on the command's exit status, and a shell pipeline's status
is its last command's. So `go test ./... 2>&1 | tail -5` exits 0 with every test
failing, and a verify written that way cannot fail. `hunk` refuses one at exit
1, before it reads the patch: a `--verify` command whose last pipeline ends in
`tail`, `head`, `tee` or `cat`. Drop the pipe. The command's output is printed
only when the verify fails, and then only its last 40 lines, which
`--verify-lines N` changes. A pipeline that ends in `grep` is left alone,
because `grep`'s status depends on what it read, and so is `--try`, whose tree
goes back whatever its command says. The check reads plain pipelines and nothing
cleverer: a command with a subshell, a command substitution or a here-document
runs as written, and so does one that says `set -o pipefail`, which is how to
keep a pipe and still have the verify fail.

One subtlety, because it is the one behaviour that could make the tool
dangerous. If your verify command rewrites files (`gofmt -w`, `prettier
--write`, `make fmt`), pass `--verify-may-format`. Without it, a file that
changed after `hunk` wrote it is left alone rather than reverted, and the exit
is 4. `hunk` cannot tell a formatter from another process writing the same tree,
and silently discarding somebody else's work is worse than stopping. A file
`hunk` created that is already gone is the exception: gone is what rolling a
create back leaves, so it counts as rolled back and the exit stays 3, with one
line naming the file as removed by something else.

Rollback acts only on what `hunk` left there, and asks without following
symbolic links. A link standing where `hunk` created, deleted or rewrote a file
is not `hunk`'s, under either flag: rollback leaves it, names the file, and the
exit is 4, since restoring would replace the link with a regular file. A
directory `hunk` made for a new file is removed only while it is still that
directory and empty. One the verify wrote into, or replaced with something
else, is left where it is and named in one line, and the exit stays 3, because
the batch's own changes are undone. And before it touches a file, rollback
checks every directory on the way to it against the one `hunk` wrote through:
if the verify replaced one, with a link or another directory, the file is left
and named, exit 4, rather than restored, created or removed wherever the name
now leads. That holds under `--verify-may-format` and `--try` alike.

`--try CMD` is for an edit you do not mean to keep: a print statement, a
measurement, a deliberate hang to see what the logs say (with `--timeout`). It
applies the batch, runs `CMD` the way `--verify` would, puts every file back
whatever `CMD` says, and exits with `CMD`'s own status. The report starts
`tried` whatever `CMD` said, on stdout when it passed and stderr otherwise, so
a caller can tell `CMD`'s 2 from `hunk`'s own; in `--json`, `try.ran` says it.
If a file could not be put back, because `CMD` rewrote it, the exit is 4, and
`--verify-may-format` puts it back anyway.

### Several patches, one at a time: `--try CMD --each`

A deliberate-break run applies each of a set of small patches alone against
the test suite, to see that the suite notices each one. A shell loop round
`--try` misreads what its exit conflates on purpose: a patch with a typo exits
1 like an ordinary catch, so it scores as "caught" forever; a command that
exits 2 reads as "did not match" and one that exits 124 as a timeout; and when
a file cannot be put back, the report's first line says `the command exited
0`, which a loop reading that line scores as a missed break before carrying on
against a broken tree.

`--each` is that loop done right, once:

```sh
hunk --try 'zig build test' --timeout 170s --each breaks/*.hunk
```

Every patch is read, parsed and validated before the first try, and one that
fails refuses the lot, nothing tried, so the typo in patch 19 is found before
the first command runs. Then each patch alone, in the order given: apply, run
the command, put every file back, exactly as one `--try` does, with
`--timeout` applying to each command separately. One line per patch goes to
stderr as it finishes, so a run your harness kills still shows every patch
that completed:

```
tried breaks/01-shadow-reach.hunk   the command exited 1 (41.2s)
tried breaks/06-texture.hunk        the command exited 0 (38.0s)
tried breaks/19-loose.hunk          the command timed out after 170s
tried 3 patches and put back every file; 1 exited nonzero, 1 exited 0, 1 timed out (249.2s)
```

The command's output is printed only under a patch whose command exited 0 or
timed out, since those are the ones to investigate. The run stops at once
where carrying on would lie: a file not put back (exit 4, the block names
it), a patch refused after the preflight passed (the tree changed under the
run, by the command or by another writer), a command that cannot start, a
signal. Patches the stop left untried are listed as not tried.

The exit is the run's, never a command's: 0 when every patch was tried and
put back and every command exited nonzero; 3, as a verify whose verdict went
against you, when a command exited 0 or timed out; 1, 2, 5 or 6 when the run
refused or stopped; 4 when a file was not put back. In `--json` the run is an
`each` object, one entry per patch, so a script reads `each.patches` rather
than the lines.

**A break that does not compile reads as caught.** Zig and Go exit 1 for a
compile error as for a failing test, and deleting one line easily leaves an
unused parameter or variable behind, which both refuse. So the test command
exits nonzero and the patch counts with the caught ones, though it never
reached a test. `--build CMD` tells the two apart:

```sh
hunk --try 'go test ./...' --build 'go vet ./...' --timeout 170s --each breaks/*.hunk
```

The build runs once on the tree as it is, and a failure there refuses the
run, exit 1, nothing tried: every patch would read as not built, true of each
line and false about each patch. Then it runs after each patch is applied and
before the command. A patch whose build exits nonzero or times out is put
back without its command running, and its line says so, with the build's
output under it, which is the compile error to fix the patch from:

```
tried breaks/01-shadow-reach.hunk  the command exited 1 (41.2s)
tried breaks/04-depth-unused.hunk  did not build: the build exited 1 (1.6s)

$ go vet ./...
# example.com/faults
# [example.com/faults]
vet: ./reach.go:88:2: declared and not used: depth
(last 3 of 3 lines)
tried 2 patches and put back every file; 1 exited nonzero, 1 did not build (44.4s)
```

A patch that did not build is counted apart in the summary and makes the exit
3, since a break that never reached a test was not shown caught. The build
has to compile what the tests compile: `go vet ./...` or `go test -run '^$'
./...`, `zig test --test-no-exec FILE`, `cargo test --no-run`. `go build
./...` and `cargo build` leave the test files out.

`--dry-run --each` is the preflight alone, one diffstat line per patch. And
`-f` given twice is refused rather than read as the last one, which it
silently was before `--each` existed.

### When the command does not end

`--timeout DURATION` (`90s`, `5m`) ends the `--verify` or `--try` command after
that long, and everything it started with it: its whole process group gets
SIGTERM, and SIGKILL five seconds later if anything is left. A verify that
timed out exits 3 and is rolled back; a `--try` that timed out exits 124, as
`timeout(1)` does, and is put back. There is no default, because `hunk` cannot
know the limit its caller works under. **An agent should set one below its own
tool's limit for the call**, and always with `--try` on anything meant to hang.
A harness may not stop a hung verify for you: Claude Code, for one, moves a
command that outlives its timeout into the background rather than killing it,
and `hunk` waits there with the batch applied.

A SIGINT, SIGTERM or SIGHUP to `hunk` while the command runs, a Ctrl-C at a
terminal included, does the same: the command and everything it started are
ended, the batch is put back, the report is printed, and `hunk` then ends by
that same signal, so a shell sees 130, 143 or 129. If a file could not be put
back the exit is 4 instead. SIGKILL cannot be caught, and leaves the batch
applied and the command running.

A command that exits normally but leaves something running that still holds
its output, a server a test suite started for instance, no longer holds
`hunk`: it stops reading five seconds after the command exits, says so in the
report, and leaves that process running, since the command may have meant to
start it.

On Windows there is no process group to end: a timeout or a Ctrl-C ends `sh`,
and the five-second cut on its output covers what `sh` started.

## Exit codes

| code | means | tree |
| --- | --- | --- |
| 0 | applied, and verify passed if given | changed |
| 1 | usage or parse error | untouched |
| 2 | a hunk did not match | untouched |
| 3 | verify failed, rolled back | the same bytes and mode |
| 4 | verify failed and rollback was incomplete | **inconsistent** |
| 5 | I/O error | untouched, unless the message says otherwise |
| 6 | a file changed on disk between load and commit | untouched |

Under `--try` the exit is the command's own status instead, 124 if
`--timeout` ended it, and 4 if a file could not be put back. A `hunk` ended by a
signal during the command dies of that signal once the batch is back (130, 143,
129). Under `--try --each` it is the run's, never a command's: 0 when every
patch was tried and put back and every command exited nonzero, 3 when a
command exited 0 or timed out or, under `--build`, a patch did not build, 4
when a file was not put back, the table's codes when the run refused or
stopped, 1 among them when the build failed before the first patch.

**Exit 2 is not an error condition to route around.** It is the tool working,
and it is the one to expect routinely: fix the patch and resend. Exit 4 is the
only one that leaves the tree in a state you have to look at.

`--json` emits the same information as one object, near-miss span included, so a
program never has to parse the text.

## What it does not promise

Replacing one file is atomic, via `rename(2)`. **The batch is not.** A write
that fails part-way through, such as a full disk or a name the filesystem
refuses, puts back the files written before it and exits 5, naming the file
that failed. A crash part-way through cannot be undone that way, since undoing
needs the process alive, so the files before it stay written.

Every target is re-hashed between validation and writing, so a process that
wrote the tree in between is caught, and the exit is 6 with nothing written.
One writing at the same time is not. There is no lock file, and two `hunk` runs
started together on the same files usually both succeed, which can leave some
files as one wrote them and some as the other did. No file is ever a mix of
the two.

Both gaps are repaired by the version control your tree is already under, which
is why neither is worth the complexity of closing.

Every write is a new file renamed over the old one, so a file `hunk` rewrites,
including one a rollback puts back, keeps its bytes and its mode, setuid,
setgid and sticky included, but is a new file. It keeps the file's group where
you are in that group. Where you are not, the group it ends up in gets only the
permissions everyone else had, so nobody in that group gains anything by the
change, and setgid is dropped, since it would name the wrong group. It keeps
the owner only when run as root: editing a file someone else owns makes it
yours, and drops setuid, which would otherwise run it as you. ACLs and extended
attributes are not kept. A file whose access depends on any of those is better
edited another way.

## Install

Each way below installs the `hunk` binary and its agent skill, which teaches an
agent when to reach for the tool and, more importantly, what to do when it
refuses. The skill is installed twice, one copy per convention:
`~/.agents/skills/hunk` is the unified folder most agents discover skills in
(Codex, Cursor, Copilot, Gemini CLI and more), and `~/.claude/skills/hunk` is
the one folder Claude Code reads: as of 2.1.282 it does not discover skills in
`.agents/`, and its support for the unified folder is an explicit import
command. Identical copies, so every tool finds the same skill.

### macOS and Linux, with Homebrew

```sh
brew install kmoneil/tap/hunk
```

The full name matters: Homebrew's own `hunk` is an unrelated tool, a diff
viewer, and `brew install hunk` installs that one instead.

The formula installs the skill from the same release as the binary. Homebrew
does not write into your home directory, so link the skill in once, one link
per convention, and every `brew upgrade` moves both along with the binary:

```sh
mkdir -p ~/.claude/skills ~/.agents/skills
ln -s "$(brew --prefix)/opt/hunk/share/hunk/skill" ~/.claude/skills/hunk
ln -s "$(brew --prefix)/opt/hunk/share/hunk/skill" ~/.agents/skills/hunk
```

### Windows

#### With Scoop

```powershell
scoop bucket add kmoneil https://github.com/kmoneil/scoop-bucket
scoop install kmoneil/hunk
```

That installs the skill into `~\.claude\skills\hunk` and
`~\.agents\skills\hunk` as well, and `scoop update hunk` keeps all of it at
the same version.

#### Without Scoop

In PowerShell. This downloads the binary for your machine and the skill from
the latest release, checks both against the release's `SHA256SUMS` and installs
nothing unless both match, then puts `hunk.exe` on your `PATH`. It stops at the
first thing that fails, a download included, with nothing installed:

```powershell
& {
    # Stop at the first thing that fails, so a file that did not download
    # installs nothing. Inside & { } so the setting ends with the block.
    $ErrorActionPreference = 'Stop'
    # Windows PowerShell 5.1 does not always offer TLS 1.2, which GitHub requires.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $ProgressPreference = 'SilentlyContinue'
    $arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
    $from = 'https://github.com/kmoneil/hunk/releases/latest/download'
    $get = Join-Path $env:TEMP 'hunk-install'
    New-Item -ItemType Directory -Force -Path $get | Out-Null
    foreach ($file in "hunk-windows-$arch.exe", 'hunk-skill.tar.gz', 'SHA256SUMS') {
        Invoke-WebRequest "$from/$file" -OutFile (Join-Path $get $file) -UseBasicParsing
    }

    $want = @{}
    Get-Content (Join-Path $get 'SHA256SUMS') | ForEach-Object {
        $digest, $name = $_ -split '\s+', 2
        $want[$name] = $digest
    }
    foreach ($file in "hunk-windows-$arch.exe", 'hunk-skill.tar.gz') {
        if ((Get-FileHash (Join-Path $get $file)).Hash -ne $want[$file]) {
            throw "$file does not match SHA256SUMS; nothing was installed"
        }
    }

    $dir = Join-Path $env:LOCALAPPDATA 'Programs\hunk'
    $skills = Join-Path $env:USERPROFILE '.claude\skills'
    $agents = Join-Path $env:USERPROFILE '.agents\skills'
    New-Item -ItemType Directory -Force -Path $dir, $skills, $agents | Out-Null
    Copy-Item (Join-Path $get "hunk-windows-$arch.exe") (Join-Path $dir 'hunk.exe') -Force
    & "$env:SystemRoot\System32\tar.exe" -xzf (Join-Path $get 'hunk-skill.tar.gz') -C $skills
    & "$env:SystemRoot\System32\tar.exe" -xzf (Join-Path $get 'hunk-skill.tar.gz') -C $agents
    $path = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($path -split ';') -notcontains $dir) {
        [Environment]::SetEnvironmentVariable('Path', "$path;$dir", 'User')
    }
}
```

Open a new terminal, and `hunk --version` says which release you have. Running
the same steps again upgrades both.

`--verify` and `--try` run their command with `sh`, which Windows does not
have: without one, those two flags refuse and write nothing, and every other
edit works. `scoop install git` puts one on your `PATH`, and
[Git for Windows](https://git-scm.com/downloads/win) brings one with Git Bash.
Claude Code runs its shell commands in Git Bash when Git for
Windows is installed and in PowerShell when it is not, so install it before
handing hunk to an agent; the skill's examples are written for that shell.

### Anywhere, with Go

```sh
go install github.com/kmoneil/hunk@latest
```

That installs the binary only. The skill is `hunk-skill.tar.gz` in every
[release](https://github.com/kmoneil/hunk/releases/latest), one `hunk/`
directory to extract into `~/.claude/skills`, `~/.agents/skills`, or both.
Or, from a clone on macOS or Linux, `make install` does both:

```sh
git clone https://github.com/kmoneil/hunk && cd hunk && make install
```

A clone carries the skill's source at `.agents/skills/hunk`, the unified
project folder, so an agent that reads it finds the skill in the tree without
any install.

`hunk --version` says which one you have: the release when you installed one,
and a pseudo-version naming the commit when you built from a clone.

A static binary. No configuration file, no state directory, no network, and no
dependency outside the Go standard library. The last two are gated rather than
promised: one by a test that walks the transitive import graph, the other by a
check that `go.mod` declares no requirements at all. `make check` fails if
either stops being true.

From v0.3.8 on, every file in a release also carries build provenance: a
signed statement, held in this repository's attestation store, that it was
built by the release workflow here, from that tag, on a runner GitHub hosts.
The Homebrew tap and the Scoop bucket verify it before they move to a release,
and with [GitHub's CLI](https://cli.github.com/) so can you:

```sh
gh attestation verify hunk-darwin-arm64 --repo kmoneil/hunk
```

## Development

```sh
make tools    # the pinned gofumpt, golangci-lint and govulncheck
make check    # gofumpt, vet, golangci-lint, the dependency and network gates, tests
make build    # bin/hunk
make hooks    # enable the commit-msg and pre-commit hooks, once per clone
```

The failure messages are the product here, not a byproduct of it, so they are
held byte-for-byte by golden files in `testdata/`. Rewording a diagnostic is a
deliberate change to a contract, not a test that needs regenerating.

## Status

All six directives work, with fifteen flags.

Linux, macOS and Windows are tested. CI runs the suite on all three and a merge
is blocked unless it passes on every one, along with the dependency, network,
vulnerability and binary-size gates.

Windows carries one caveat, said out loud because a green check should not
claim more than it checked. The file-mode assertions and the tests that force a
failure by removing permission are skipped there: Windows has no POSIX
permission bits, and `chmod` does not make a directory unreadable, so those
tests would be asserting the platform rather than the tool. Everything else
runs, which is the transaction, the matching, the diagnosis, the near-miss
report and the rollback bookkeeping.

It has one user and a version number that says so.

## License

Apache 2.0. See [LICENSE](LICENSE).
