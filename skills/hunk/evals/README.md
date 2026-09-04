# Evals for the hunk skill

`evals.json` holds the cases. `grade.py` scores the runs, checking every
assertion against the tree a run left behind or the commands it ran, never
against the agent's own account of itself.

## Running them

The runs themselves are agent invocations, not something a script can launch.
Scaffold one directory per case per arm per run, give each agent the task and
its working directory, then `make skill-grade`.

Each `eval-N/` needs an `eval_metadata.json` naming the case, and the grader
refuses to score a directory whose name disagrees with `evals.json` or that has
no metadata at all. Workspace ids are the scaffold's numbering and are not
promised to match the case list forever: iteration-3's `eval-9` is
`four-small-edits`, a case that was run, reported on, and never landed, and
`evals.json` case 9 is now something else entirely.

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

### Two kinds of assertion, and only one of them has a baseline

Every assertion carries a kind and the summary reports them apart.

- **outcome**: did the job get done. Both arms can pass these, so the delta
  between the arms means something.
- **tool choice**: which tool or flag was reached for. An arm without the tool
  cannot pass "used hunk" and cannot fail "did not overtrigger". **The delta on
  this half is not a result**, it is arithmetic, and counting it inflated a
  published figure 2.4x before anybody noticed.

The comparison that measures a tool-choice change is the **skill's previous
revision against its current one**, both arms holding the binary. The no-skill
arm still earns its place on such a case, for the outcome half.

Case 9 is the only case in the suite whose point is tool choice: cases 0, 6 and
7 all ask for the verification behaviour in the prompt, so they measure whether
an instruction was followed. Case 9's prompt mentions no build, no test and no
rollback, and the assertion is whether the run gated the batch anyway.

## The grader is tested now

`grade_test.py` builds synthetic runs of every shape case 9 can produce and
asserts what the grader says about each: a run that verified, one that did not,
one whose `--verify` command cannot fail, one that did the job with `Edit` and
no `hunk` at all. `make skill-grade` runs it first.

It also carries the two tables the grader's docstrings describe: which
invocations count as using `hunk`, and which commands count as a verify. Four
rows in the first are the four times this grader was wrong.

**It found a defect in a case before that case had ever been run.** Case 9's
first draft added a second *return* value to `Greet` and claimed a half-done
batch would leave `main.go` assigning two values to one. Go has a special case
for exactly that shape: `fmt.Println(greeter.Greet("world"))` compiles whatever
`Greet` returns, so the broken tree built and the case had no teeth. A missing
*argument* has no such special case, which is why the case asks for a parameter.

## The grader is not exempt

It was wrong four times, and each fix moved the published delta: it matched
`\bhunk\b`, which the workspace path satisfies; it counted `sed` on the agent's
own transcript as a fallback; it counted `hunk --version` probes as usage, which
hid a fix working; and it missed `hunk < patch` stdin redirects. It now carries
a table of invocations it must and must not recognise. Check the instrument
before believing the measurement.
