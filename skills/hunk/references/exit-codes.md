# Exit codes and limits

Read this when a `hunk` invocation exits non-zero and the code is not one you
recognise, or when you need to know exactly what the tool does and does not
guarantee.

## Exit codes

| Code | Meaning | Tree |
| --- | --- | --- |
| 0 | applied, verify passed if given | changed |
| 1 | usage or a malformed patch | untouched |
| 2 | a hunk did not match | untouched |
| 3 | verify failed, rolled back | untouched |
| 4 | verify failed and rollback was incomplete | **inconsistent** |
| 5 | I/O error | see the message |
| 6 | a file changed on disk between read and write | untouched |

`--json` gives the same information as one object, including the near-miss span,
so a program never has to parse the text.

## What it does not promise

Replacing one file is atomic. **The batch is not**: a crash part-way through
leaves the files before it written. And the check that guards against a second
writer narrows that window without closing it. Both are repaired by the version
control the tree is already under. Do not build anything on the batch being
atomic across a power failure.
