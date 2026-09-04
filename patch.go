package main

// The parser (§3).
//
// Turns patch text into a Patch and touches no file while doing it, which is
// what lets §6.1 promise "any syntax error, exit 1, nothing read" structurally
// rather than by discipline.
//
// Two things to carry in from _plans/ready/the-parser.md before writing code:
//
//   - §3.2's printed grammar cannot parse §3.6's printed example. It makes
//     "@@ file" part of every replace, while §3.5 says a "@@ file" stays in
//     effect until the next one. The real grammar is a flat stream of
//     directives over a current-file register. Fix §3.2 in the spec too, since
//     §3.2 is what `hunk format` hands an agent that has never seen the tool.
//
//   - The payload boundary rule (§3.3) is the highest-risk thing here and the
//     spec states it three times for that reason. No trailing newline is ever
//     added; a blank line before the next directive is how a payload gets one.
