package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hundredLines prints 1 to 100, one a line, in sh (seq is not on every
// runner's).
const hundredLines = "i=1; while [ $i -le 100 ]; do echo $i; i=$((i+1)); done"

// TestJSONCarriesTheTail (#174): what --json carries of a command's output is
// its last --verify-lines lines, 40 by default, for every patch under --each,
// caught or not, for the runs on the tree as it is, and for a single --try;
// never the whole output, which the docs said until 2026-11-12.
func TestJSONCarriesTheTail(t *testing.T) {
	type each struct {
		Each struct {
			Patches []struct {
				Output []string `json:"output"`
			} `json:"patches"`
			TryBaseline *struct {
				Output []string `json:"output"`
			} `json:"try_baseline"`
		} `json:"each"`
	}
	// lines is first..100 as text.
	lines := func(first int) []string {
		var out []string
		for i := first; i <= 100; i++ {
			out = append(out, fmt.Sprint(i))
		}
		return out
	}
	for _, c := range []struct {
		name  string
		flags []string
		want  []string
	}{
		{"--each, --verify-lines not given", nil, lines(61)},
		{"--each, --verify-lines 5", []string{"--verify-lines", "5"}, lines(96)},
		{"--each, --verify-lines past the output", []string{"--verify-lines", "200"}, lines(1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, patches := eachFixture(t)
			args := append(append([]string{"--json"}, c.flags...), "--try", hundredLines+"; "+catchesAll, "--each", patches[0], patches[1])
			code, js, errOut := runCLI(t, root, args, "")
			if code != exitOK {
				t.Fatalf("exit %d: %s%s", code, js, errOut)
			}
			var r each
			must(t, json.Unmarshal([]byte(js), &r))
			if len(r.Each.Patches) != 2 {
				t.Fatalf("patches = %+v", r.Each.Patches)
			}
			for i, p := range r.Each.Patches {
				sameLines(t, fmt.Sprintf("patch %d's output", i), p.Output, c.want)
			}
			if b := r.Each.TryBaseline; b == nil {
				t.Error("no try_baseline")
			} else {
				sameLines(t, "the baseline's output", b.Output, c.want)
			}
		})
	}

	t.Run("a single --try, --verify-lines not given", func(t *testing.T) {
		root := cliTree(t, map[string]string{"a.txt": "one\n"})
		code, js, _ := runCLI(t, root, []string{"--json", "--try", hundredLines}, vPatch)
		var v struct {
			Try struct{ Output []string }
		}
		must(t, json.Unmarshal([]byte(js), &v))
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, js)
		}
		sameLines(t, "the try's output", v.Try.Output, lines(61))
	})
}

// The documents say what --json carries of an output, every one of them, and
// none says it carries the whole of it any more.
func TestTheDocsSayJSONCarriesTheTail(t *testing.T) {
	read := func(path ...string) string {
		b, err := os.ReadFile(filepath.Join(path...))
		must(t, err)
		return strings.Join(strings.Fields(string(b)), " ")
	}
	for _, c := range []struct{ doc, text, says string }{
		{"README.md", read("README.md"), "`--json` carries the same tail for every patch, given or not: its last `--verify-lines` lines, 40 by default, not its whole output."},
		{"SKILL.md", read(".agents", "skills", "hunk", "SKILL.md"), "`--json` carries every patch's last `--verify-lines` lines either way, 40 by default, not its whole output."},
		{"--help", strings.Join(strings.Fields(usage), " "), "--json carries the same tail, for every patch under --each, given or not."},
	} {
		if !strings.Contains(c.text, c.says) {
			t.Errorf("%s does not say %q", c.doc, c.says)
		}
		if strings.Contains(c.text, "carries every patch's output") {
			t.Errorf("%s still says --json carries every patch's output", c.doc)
		}
	}
}
