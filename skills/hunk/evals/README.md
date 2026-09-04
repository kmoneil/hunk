# Evals for the hunk skill

`evals.json` holds the cases. `grade.py` scores the runs, checking every
assertion against the tree a run left behind or the commands it ran, never
against the agent's own account of itself.

## Running them

The runs themselves are agent invocations, not something a script can launch.
Scaffold one directory per case per arm per run, give each agent the task and
its working directory, then `make skill-grade`.

**Scaffold with a shell that word-splits, or do not rely on word splitting.**
The iteration-3 scaffold used `set -- $spec` under zsh, which does not split
unquoted parameter expansions, and every run directory came out with its path
collapsed into spaces. Every agent caught it, but a harness bug like that
normally produces silent garbage rather than a complaint.

## What the numbers mean

The score on its own says nothing. **The delta against a no-skill baseline is
the result**, because a skill that scores well against nothing has not been
shown to do anything. Run both arms, and run each case more than once: the
single most useful finding across three iterations came from a case that passed
in one run and failed in the other.

## The grader is not exempt

It was wrong four times, and each fix moved the published delta: it matched
`\bhunk\b`, which the workspace path satisfies; it counted `sed` on the agent's
own transcript as a fallback; it counted `hunk --version` probes as usage, which
hid a fix working; and it missed `hunk < patch` stdin redirects. It now carries
a table of invocations it must and must not recognise. Check the instrument
before believing the measurement.
