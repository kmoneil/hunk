package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// should-two-runs-at-once-be-guarded decided: no guard, and measure first.
// These are that measurement's tests, written before its first number was read.

func TestOverlappingPairs(t *testing.T) {
	at := func(ms int) time.Time { return time.Unix(0, 0).Add(time.Duration(ms) * time.Millisecond) }
	iv := func(file, cwd string, from, to int) interval {
		return interval{file: file, cwd: cwd, start: at(from), end: at(to)}
	}
	for _, c := range []struct {
		name string
		ivs  []interval
		want int
	}{
		{"two agents, one tree, overlapping", []interval{iv("a", "/t", 0, 10), iv("b", "/t", 5, 15)}, 1},
		{"touching counts: a millisecond cannot tell them apart", []interval{iv("a", "/t", 0, 10), iv("b", "/t", 10, 20)}, 1},
		{"a millisecond apart does not", []interval{iv("a", "/t", 0, 10), iv("b", "/t", 11, 20)}, 0},
		{"one agent cannot collide with itself", []interval{iv("a", "/t", 0, 10), iv("a", "/t", 5, 15)}, 0},
		{"two trees", []interval{iv("a", "/t", 0, 10), iv("b", "/u", 5, 15)}, 0},
		{"three agents at once are three pairs", []interval{iv("a", "/t", 0, 10), iv("b", "/t", 1, 10), iv("c", "/t", 2, 10)}, 3},
		{
			// b and c each overlap a and not each other: the sweep has to drop
			// b before c starts.
			"nested, and disjoint within", []interval{iv("a", "/t", 0, 10), iv("b", "/t", 1, 2), iv("c", "/t", 5, 6)}, 2,
		},
		{"out of order in", []interval{iv("b", "/t", 5, 15), iv("a", "/t", 0, 10)}, 1},
		{"nothing", nil, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := overlappingPairs(c.ivs, nil); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// A corpus with two sessions in one tree, a subagent of one of them, and a
// session in another tree. Each call is a tool_use record and a tool_result
// record, timestamped the way the transcripts are.
func fixtureTwoAgentsCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "-workspace-demo")
	base := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	stamp := func(ms int) string { return base.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano) }
	type step struct {
		id, tool, cmd, out string
		from, to           int
		cwd                string
	}
	write := func(path string, steps ...step) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		var buf []byte
		for _, s := range steps {
			use, _ := json.Marshal(map[string]any{
				"type": "assistant", "timestamp": stamp(s.from), "cwd": s.cwd,
				"message": map[string]any{"content": []map[string]any{{
					"type": "tool_use", "id": s.id, "name": s.tool, "input": map[string]any{"command": s.cmd},
				}}},
			})
			res, _ := json.Marshal(map[string]any{
				"type": "user", "timestamp": stamp(s.to), "cwd": s.cwd,
				"message": map[string]any{"content": []map[string]any{{
					"type": "tool_result", "tool_use_id": s.id, "content": s.out,
				}}},
			})
			buf = append(append(append(append(buf, use...), '\n'), res...), '\n')
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	applied := "M a.go +1 -1\n1 file, 1 hunk, +1 -1\n"
	write(filepath.Join(project, "s1.jsonl"),
		step{"a1", "Bash", "hunk -f p", applied, 1000, 1100, "/t"},
		step{"a2", "Bash", "hunk --help", "hunk applies literal text edits\n", 5000, 5100, "/t"},
		step{"a3", "Edit", "", "ok", 9000, 9100, "/t"},
	)
	write(filepath.Join(project, "s2.jsonl"),
		// Overlaps s1's first hunk run.
		step{"b1", "Bash", "hunk -f q", applied, 1050, 1200, "/t"},
		// A probe beside s1's probe: a probe writes nothing, so it is no run.
		step{"b2", "Bash", "hunk --version", "hunk v0.2.5\n", 5050, 5150, "/t"},
		// Inside s1's Edit: a Bash beside an edit counts.
		step{"b3", "Bash", "go vet ./...", "ok\n", 9020, 9040, "/t"},
		// A millisecond after s1's Edit ends: no overlap.
		step{"b4", "Bash", "go test ./...", "ok\n", 9101, 9500, "/t"},
	)
	// s1's subagent, in the same tree, overlapping s1's Edit.
	write(filepath.Join(project, "s1", "subagents", "agent-x.jsonl"),
		step{"c1", "Write", "", "ok", 9050, 9060, "/t"},
	)
	// Another tree entirely, at the same moment as the first pair.
	write(filepath.Join(root, "-workspace-other", "s3.jsonl"),
		step{"d1", "Bash", "hunk -f r", applied, 1000, 1200, "/u"},
	)
	return root
}

// keep sees each pair once, and a pair it refuses is not counted.
func TestOverlappingPairsKeepsOnlyWhatItIsTold(t *testing.T) {
	at := func(ms int) time.Time { return time.Unix(0, 0).Add(time.Duration(ms) * time.Millisecond) }
	ivs := []interval{
		{file: "a", cwd: "/t", start: at(0), end: at(10)},
		{file: "b", cwd: "/t", start: at(5), end: at(15), edit: true},
		{file: "c", cwd: "/t", start: at(6), end: at(8)},
	}
	edits := func(x, y interval) bool { return x.edit || y.edit }
	if got := overlappingPairs(ivs, edits); got != 2 {
		t.Errorf("pairs with an edit = %d, want 2 (a-b and b-c, not a-c)", got)
	}
	if got := overlappingPairs(ivs, nil); got != 3 {
		t.Errorf("all pairs = %d, want 3", got)
	}
}

func TestWalkCountsAgentsInOneTree(t *testing.T) {
	s, err := Walk(fixtureTwoAgentsCorpus(t))
	if err != nil {
		t.Fatal(err)
	}
	// One pair of hunk runs: a1 and b1. d1 is in another tree, and the probes
	// are not runs.
	if s.HunkRunsOverlapping != 1 {
		t.Errorf("hunk runs overlapping = %d, want 1", s.HunkRunsOverlapping)
	}
	// Edits: s1's Edit with s2's Bash inside it, and with its subagent's
	// Write. a1 with b1 and the probes are Bash on both sides, so they are out,
	// and b4 starts a millisecond after the Edit ends.
	if s.EditsOverlapping != 2 {
		t.Errorf("edits overlapping = %d, want 2", s.EditsOverlapping)
	}
}

// Each arm counts its own: the lab's overlaps are not the field's.
func TestWalkSplitCountsOverlapsPerArm(t *testing.T) {
	s, err := WalkSplit(fixtureTwoAgentsCorpus(t), "-workspace-demo")
	if err != nil {
		t.Fatal(err)
	}
	if s.Here.HunkRunsOverlapping != 1 || s.Field.HunkRunsOverlapping != 0 || s.HunkRunsOverlapping != 1 {
		t.Errorf("here %d, field %d, all %d; want 1, 0, 1",
			s.Here.HunkRunsOverlapping, s.Field.HunkRunsOverlapping, s.HunkRunsOverlapping)
	}
}
