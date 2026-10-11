package main

import (
	"os"
	"regexp"
	"testing"
)

// The root module's go directive moves for vulnerability fixes (it sits at
// 1.26.8 for GO-2026-4970), and this module's lagged it at 1.26.4 for a month
// with nothing to say so: issue #43's tooling list is where it surfaced. The
// compiler enforces nothing here, so this does.
func TestGoDirectiveMatchesTheRootModule(t *testing.T) {
	directive := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`(?m)^go (\S+)$`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s has no go directive", path)
		}
		return string(m[1])
	}
	if ours, roots := directive("go.mod"), directive("../../go.mod"); ours != roots {
		t.Errorf("scripts/corpus/go.mod says go %s; the root go.mod says go %s", ours, roots)
	}
}
