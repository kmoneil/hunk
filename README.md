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
every file goes back to how it was.

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
  your old used 4 spaces; the file uses a tab
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
| `@@ create PATH` | create a file, with the payload as its contents |
| `@@ delete PATH` | delete a file |
| `@@ append PATH` | append the payload |
| `@@ prepend PATH` | prepend the payload |

`@@ delete` removes the whole file. To remove a block of lines, replace it with
an empty `@@ new`, which with rule 1 below takes the block's last newline with
it.

Three rules worth knowing before the first patch:

1. **A payload never gains a trailing newline.** To match text that ends in a
   newline, leave a blank line before the next directive.
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

`hunk format` prints the full grammar and a worked example that applies.

## Verify, and rollback

`--verify CMD` runs after a successful apply, from `--root`, through `sh -c`.
If it exits non-zero, every file goes back and so does the exit code. You do not
need a backup and you do not need a cleanup step.

This is the flag worth reaching for whenever a batch touches more than one file
of a compiled language. Every hunk can match, every file can be plausible on its
own, and the batch can still not build, because what broke is *between* the
files.

One subtlety, because it is the one behaviour that could make the tool
dangerous. If your verify command rewrites files (`gofmt -w`, `prettier
--write`, `make fmt`), pass `--verify-may-format`. Without it, a file that
changed after `hunk` wrote it is left alone rather than reverted, and the exit
is 4. `hunk` cannot tell a formatter from another process writing the same tree,
and silently discarding somebody else's work is worse than stopping.

## Exit codes

| code | means | tree |
| --- | --- | --- |
| 0 | applied, and verify passed if given | changed |
| 1 | usage or parse error | untouched |
| 2 | a hunk did not match | untouched |
| 3 | verify failed, rolled back | untouched |
| 4 | verify failed and rollback was incomplete | **inconsistent** |
| 5 | I/O error | see the message |
| 6 | a file changed on disk between load and commit | untouched |

**Exit 2 is not an error condition to route around.** It is the tool working,
and it is the one to expect routinely: fix the patch and resend. Exit 4 is the
only one that leaves the tree in a state you have to look at.

`--json` emits the same information as one object, near-miss span included, so a
program never has to parse the text.

## What it does not promise

Replacing one file is atomic, via `rename(2)`. **The batch is not.** A crash or
a full disk part-way through leaves the files before it written.

Every target is re-hashed between validation and writing, so another process
writing the tree is caught and the exit is 6 with nothing written. That narrows
the window to microseconds without closing it. There is no lock file.

Both gaps are repaired by the version control your tree is already under, which
is why neither is worth the complexity of closing.

## Install

```sh
go install github.com/kmoneil/hunk@latest
```

Or from a clone, which also installs the agent skill:

```sh
git clone https://github.com/kmoneil/hunk && cd hunk && make install
```

That puts the binary on your `PATH` and the skill in `~/.claude/skills/hunk`,
which teaches an agent when to reach for the tool and, more importantly, what to
do when it refuses.

A static binary. No configuration file, no state directory, no network, and no
dependency outside the Go standard library. The last two are gated rather than
promised: one by a test that walks the transitive import graph, the other by a
check that `go.mod` declares no requirements at all. `make check` fails if
either stops being true.

## Development

```sh
make check    # gofmt, vet, the dependency gate, tests
make build    # ./hunk
make hooks    # enable the commit-msg gate, once per clone
```

The failure messages are the product here, not a byproduct of it, so they are
held byte-for-byte by golden files in `testdata/`. Rewording a diagnostic is a
deliberate change to a contract, not a test that needs regenerating.

## Status

All six directives work, with thirteen flags.

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
