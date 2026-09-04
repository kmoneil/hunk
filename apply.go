package main

// The transaction (§6.1, §6.2, §6.4).
//
// The phase order is the whole design, and its point is that nothing is
// written until every hunk in the batch is known to match and every target is
// known to be unchanged since it was read:
//
//	1. Parse    syntax error, exit 1, nothing read       (patch.go)
//	2. Load     read each file once; path, bytes, mode, SHA-256
//	3. Validate one walk, in order, in memory; a mismatch marks the file
//	            failed and skips its later hunks (§3.5)
//	4. Check    re-hash every target; any difference, exit 6, nothing written
//	5. Commit   temp file, fsync, chmod, rename, per file
//	6. Verify   sh -c, cwd --root, combined output captured
//	7. Roll back on failure (§6.3)
//
// The batch is not atomic, and the tool says so in --help rather than
// pretending otherwise (§6.2). Per-file replacement is atomic via rename(2);
// a crash between file three and file four leaves three files written.
