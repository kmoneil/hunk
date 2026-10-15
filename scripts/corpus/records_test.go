package main

// Which records the walk reads (§12). A transcript is the conversation, an
// assistant record for each tool_use and a user record for each tool_result,
// and since 2026-10-14 it is other records too: api-request-blob carries the
// request as sent, with copies of calls and of results under the same ids.
// Until 2026-10-15 the walk read them all, and counted each copied call twice.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// at is a timestamp a fixed number of seconds into the fixture's session.
func at(sec int) string {
	return time.Date(2026, 10, 14, 21, 0, sec, 0, time.UTC).Format(time.RFC3339Nano)
}

// typed is one transcript line: a record of type typ, at ts when ts is not
// empty, carrying blocks.
func typed(t *testing.T, typ, ts string, blocks ...map[string]any) string {
	t.Helper()
	rec := map[string]any{"message": map[string]any{"content": blocks}}
	if typ != "" {
		rec["type"] = typ
	}
	if ts != "" {
		rec["timestamp"] = ts
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func bashUse(id, cmd string) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": cmd}}
}

func resultOf(id, out string) map[string]any {
	return map[string]any{"type": "tool_result", "tool_use_id": id, "content": out}
}

// TestReadSessionReadsTheConversationOnly: a call is read from its assistant
// record and its result from its user record, once each, whatever else
// carries a copy.
func TestReadSessionReadsTheConversationOnly(t *testing.T) {
	refused := "hunk: 1 hunk did not match; nothing was written\n"
	applied := "M a.go +1 -1\n1 file, 1 hunk, +1 -1\n"
	body := typed(t, "assistant", at(1), bashUse("t1", "hunk -f p.txt")) +
		// The request as sent: the call again, the command as the harness
		// rewrote it, and no timestamp.
		typed(t, "api-request-blob", "", bashUse("t1", "cd /w && hunk -f p.txt")) +
		typed(t, "user", at(2), resultOf("t1", refused)) +
		typed(t, "api-request-blob", "", resultOf("t1", refused)) +
		typed(t, "assistant", at(3), bashUse("t2", "hunk -f p.txt")) +
		// A record type nobody has seen yet, carrying a third copy of each.
		typed(t, "progress", at(3), bashUse("t2", "hunk -f other.txt"), resultOf("t2", "garbage\n")) +
		// The fixtures' shape until 2026-10-15, which no real transcript has.
		typed(t, "", at(3), bashUse("t9", "rm -rf /"), resultOf("t2", "more garbage\n")) +
		// A line cut short, as a transcript being written can end: it names a
		// tool_use and is not JSON.
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t8"` + "\n" +
		typed(t, "user", at(4), resultOf("t2", applied))

	calls, results, returned, err := readSession(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2: %+v", len(calls), calls)
	}
	for i, want := range []struct{ id, cmd, start string }{
		{"t1", "hunk -f p.txt", at(1)},
		{"t2", "hunk -f p.txt", at(3)},
	} {
		c := calls[i]
		if c.id != want.id || c.command != want.cmd || c.start.Format(time.RFC3339Nano) != want.start {
			t.Errorf("call %d = %s %q at %s, want %s %q at %s",
				i, c.id, c.command, c.start.Format(time.RFC3339Nano), want.id, want.cmd, want.start)
		}
	}
	for id, want := range map[string]string{"t1": refused, "t2": applied} {
		if results[id] != want {
			t.Errorf("result %s = %q, want %q, read once and from its user record", id, results[id], want)
		}
	}
	for id, want := range map[string]string{"t1": at(2), "t2": at(4)} {
		if got := returned[id].Format(time.RFC3339Nano); got != want {
			t.Errorf("returned %s = %s, want %s", id, got, want)
		}
	}
}

// A transcript that cannot be read to its end is an error, not a short session
// counted as if it were whole.
func TestReadSessionSaysWhenItCannotRead(t *testing.T) {
	broken := errors.New("disk gone")
	_, _, _, err := readSession(iotest.ErrReader(broken))
	if !errors.Is(err, broken) {
		t.Errorf("err = %v, want %v", err, broken)
	}
}

// writeSession writes one session of steps to root/-workspace-demo/s.jsonl,
// each step's call and result with the timestamps a real one has, and, with
// copies, an api-request-blob after each record repeating it, as transcripts
// written since 2026-10-14 have.
func writeSession(t *testing.T, root string, copies bool, steps ...[3]string) {
	t.Helper()
	dir := filepath.Join(root, "-workspace-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i, s := range steps {
		id, name, cmd := s[0], "Bash", s[1]
		if cmd == "" {
			name = "Edit"
		}
		use := map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{"command": cmd}}
		sb.WriteString(typed(t, "assistant", at(10*i), use))
		if copies {
			sb.WriteString(typed(t, "api-request-blob", "", use))
		}
		sb.WriteString(typed(t, "user", at(10*i+5), resultOf(id, s[2])))
		if copies {
			sb.WriteString(typed(t, "api-request-blob", "", resultOf(id, s[2])))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWalkCountsACopiedCallOnce: a session with every record copied walks to
// the same figures as the session without the copies, every one of them, and
// the exit-2 recovery reads each refusal once.
func TestWalkCountsACopiedCallOnce(t *testing.T) {
	refused := "hunk: 1 hunk did not match; nothing was written\n"
	applied := "M a.go +1 -1\n1 file, 1 hunk, +1 -1\n"
	steps := [][3]string{
		{"h1", "hunk -f p.txt", refused},
		{"h2", "hunk -f p.txt", applied},
		{"h3", "hunk --help", "hunk applies literal text edits\n"},
		{"h4", "hunk --verify 'go test ./...' -f q.txt", refused},
		{"h5", "hunk --verify 'go test ./...' -f q.txt", applied},
		{"e1", "python3 - <<'PY'\np='b.go'\ns=open(p).read()\ns=s.replace('x','y')\nopen(p,'w').write(s)\nPY", "ok"},
		{"e2", "", "ok"},
	}
	walk := func(copies bool) *Stats {
		root := t.TempDir()
		writeSession(t, root, copies, steps...)
		s, err := Walk(root)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	clean, copied := walk(false), walk(true)

	// The counts the defect doubled, by name, so a failure says which.
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"tool calls", copied.ToolCalls, 7},
		{"bash calls", copied.BashCalls, 6},
		{"edit tool calls", copied.EditToolCalls, 1},
		{"hunk calls", copied.HunkCalls, 5},
		{"hunk applications", copied.HunkApplications, 4},
		{"refusals", copied.HunkExits[ExitNoMatch], 2},
		{"heredoc edits", copied.HeredocEdits, 1},
		// Each refusal is followed by its success: two pairs, both recovered.
		// Read twice, each refusal is followed by its own copy, an exit 2,
		// and the recovery reads 2 of 4.
		{"refusals with a classified next call", copied.Exit2Followed, 2},
		{"refusals recovered", copied.Exit2Recovered, 2},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}

	// And every other figure, the whole object, against the clean walk's.
	encode := func(s *Stats) string {
		s.Root, s.Date = "", ""
		b, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got, want := encode(copied), encode(clean); got != want {
		t.Errorf("the copies changed the figures:\n--- without copies ---\n%s\n--- with copies ---\n%s", want, got)
	}
}
