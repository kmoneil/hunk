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
| 3 | verify failed, rolled back | the same bytes and mode |
| 4 | verify failed and rollback was incomplete | **inconsistent** |
| 5 | I/O error | untouched, unless the message says otherwise |
| 6 | a file changed on disk between read and write | untouched |

**Under `--try CMD` the exit is `CMD`'s own status** once every file is back,
and 4 if one could not be put back. The report starts `tried` when `CMD` ran;
without that word, the exit is `hunk`'s own, and `CMD` never ran. 124 means
`--timeout` ended `CMD`, as `timeout(1)` exits; under `--verify` a timeout is 3,
rolled back.

**What rollback leaves.** A symbolic link standing where `hunk` expected its own
file is never removed or overwritten: the file is named and the exit is 4. A
directory `hunk` made that holds something, or is no longer the one it made, is
left and named in one line, and the exit stays 3: the batch's own changes are
undone.

**A signal to `hunk` while the command runs** (SIGINT, SIGTERM, SIGHUP) ends the
command and everything it started, puts the batch back, and then ends `hunk` by
the same signal: a shell sees 130, 143 or 129. 4 means a file could not be put
back. SIGKILL cannot be caught, and leaves the batch applied.

`--json` gives the same information as one object, including the near-miss span,
so a program never has to parse the text.

## What it does not promise

Replacing one file is atomic. **The batch is not**: a crash part-way through
leaves the files before it written. A write that fails part-way, such as a full
disk or a name the filesystem refuses, is put back instead, at exit 5, and the
message names the file that failed. **Nor is there a lock.** A writer that
finished between `hunk`'s read and its write is caught, at exit 6, but one
writing at the same time is not: two `hunk` runs on the same files at once
usually both succeed, and can leave some files as one wrote them and some as
the other did. Both are repaired by the version control the tree is already
under. Do not build anything on the batch being atomic across a power failure,
or on two agents editing the same files at once.

A rewritten file, including one a rollback puts back, keeps its bytes and its
permission bits, and its group where the user is in that group; where not, the
group gets only what everyone else had. It keeps the owner only under root, and
keeps no ACL or extended attribute.
