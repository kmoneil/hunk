# hunk

A transactional multi-file edit tool, for agents rather than for people.

One invocation carries many literal-text edits across several files, plus a
command that says whether they were right. Nothing is written until every edit
is known to match exactly as many times as it claimed to. If the verification
command then fails, the tree goes back to how it was.

```
hunk --verify 'go build ./... && go test ./...' <<'HUNK'
@@ file internal/cli/root.go
@@ old
	bind.mustHaveBoundEveryGlobal()
@@ new
	bind.mustHaveBoundEveryGlobal()
	bind.mustHaveBoundEveryScope()
HUNK
```

Three properties, in the order they matter:

- **It refuses ambiguity.** An edit whose target text does not occur exactly the
  expected number of times fails the whole batch before anything is written. A
  silent edit to the wrong occurrence is the most expensive way to be wrong, so
  it is not reachable.
- **It is all or nothing, per invocation.** A failed verification restores every
  file. Per-file replacement is atomic; the batch is not, and the tool says so
  rather than overstating it.
- **It diagnoses the near miss.** When the target text is not found, the report
  prints the bytes that are actually there, and names the cause where it can: a
  tab against four spaces, a trailing space, a curly quote copied out of
  rendered Markdown. The point is that fixing it costs no extra round trip.

The input format takes literal text. There is no JSON escaping and no shell
quoting of the payload, because every byte between two directive lines is
payload, which is the property that makes a heredoc work.

## Status

**Not implemented.** This repository currently holds the module scaffold, the
exit-code contract, and the two gates that keep the claims below honest. The
specification is complete and is not shipped here.

## Install

```
go install github.com/kmoneil/hunk@latest
```

A static binary. No configuration file, no state directory, no network, and no
dependency outside the Go standard library. The last two are asserted by tests
rather than promised: `make check`.

## Build

```
make check    gofmt, vet, the dependency gate, tests
make build    ./hunk
make hooks    enable the commit-msg gate, once per clone
```
