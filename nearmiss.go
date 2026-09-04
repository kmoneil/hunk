package main

// Near-miss diagnosis (§7).
//
// This is the feature that makes the tool faster rather than merely safer.
// Mandatory uniqueness turns a silent wrong-occurrence edit into a refusal,
// and a refusal only pays for itself if the report contains the exact bytes to
// paste back. The floor, from §7.1 step 4: every anchored near miss prints the
// file's actual span, whether or not the cause can be named. Naming the cause
// is a bonus.
//
// Nothing here reads a file or knows what a patch is. It is a function of an
// expected span and a file's bytes, which is why it does not wait on apply.go.
