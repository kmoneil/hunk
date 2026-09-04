package main

import (
	"os/exec"
	"strings"
	"testing"
)

// §9 promises a static binary with "no config file, no state directory, no
// network". Nothing but this asserts the last one.
//
// The check is on the transitive import graph rather than on runtime
// behaviour, because an import that is never called is still a capability the
// binary has and a claim it therefore cannot make. Anything under net/ counts,
// including the parts of it that never open a socket: this tool has no reason
// to reach for any of them, so the strict form costs nothing and the loose form
// would need a judgement call every time it fired.
func TestNoNetworkDependency(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == "net" || strings.HasPrefix(pkg, "net/") {
			t.Errorf("binary depends on %q; SPEC §9 says no network", pkg)
		}
	}
}
