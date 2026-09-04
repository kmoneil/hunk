// Command corpus measures the transcript history that §1 of hunk's spec is
// derived from, so §12 can be answered twice.
//
// §12: "The measurement in §1 is the test. Re-run it a few weeks after the
// one-line note lands in each project's agent instructions." A number produced
// once by hand is not a number you can re-run, and two of the four figures §12
// names have never been measured at all.
//
// Its own module, deliberately. It is not part of the binary and must not
// appear in hunk's coverage, its dependency gate, or its import graph.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Stats is everything one run measures. §1's table is the top half; §12's
// comparison is the bottom.
type Stats struct {
	Date     string         `json:"date"`
	Root     string         `json:"root"`
	Sessions int            `json:"sessions"`
	Projects map[string]int `json:"heredoc_edits_by_project"`

	// §1's table.
	BashCalls        int `json:"bash_calls"`
	ToolCalls        int `json:"tool_calls"`
	EditToolCalls    int `json:"edit_tool_calls"`
	HeredocEdits     int `json:"heredoc_edits"`
	Replaces         int `json:"replaces"`
	NoGuard          int `json:"heredoc_edits_without_guard"`
	FirstOnly        int `json:"heredoc_edits_limiting_to_first_match"`
	NoGuardNorLimit  int `json:"heredoc_edits_with_neither"`
	BundledVerify    int `json:"heredoc_edits_bundling_verify"`
	HandRolledBackup int `json:"heredoc_edits_with_backup"`
	FileTouches      int `json:"file_touches"`
	MaxReplaces      int `json:"max_replaces_in_one_call"`
	MaxFiles         int `json:"max_files_in_one_call"`
	CommandBytes     int `json:"command_bytes"`

	// §12's numbers, which §1 does not have.
	Episodes        int `json:"edit_episodes"`
	CallsInEpisodes int `json:"calls_in_edit_episodes"`
	FailedAttempts  int `json:"failed_attempts"`

	// For the second run, once hunk is in use.
	HunkCalls int         `json:"hunk_calls"`
	HunkExits map[int]int `json:"hunk_exit_codes,omitempty"`
}

// CallsPerSuccessfulEdit is §12's headline comparison, and the number the whole
// design is measured by (§1.2: "the tool is measured by calls per successful
// edit, not by bytes per call").
//
// An episode runs from the first call that attempts a given file change to the
// first one that leaves the text on disk, counting every call in between
// including re-reads. That is the card's definition, chosen before any code so
// it could not be chosen to flatter the result.
func (s Stats) CallsPerSuccessfulEdit() float64 {
	if s.Episodes == 0 {
		return 0
	}
	return float64(s.CallsInEpisodes) / float64(s.Episodes)
}

func main() {
	root := flag.String("root", defaultRoot(), "transcript root")
	asJSON := flag.Bool("json", false, "emit JSON")
	flag.Parse()

	s, err := Walk(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus:", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(s); err != nil {
			fmt.Fprintln(os.Stderr, "corpus:", err)
			os.Exit(1)
		}
		return
	}
	fmt.Print(s.Report())
}

func defaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// a tool call, flattened out of the transcript
type call struct {
	id      string
	name    string
	command string
}

// Walk measures every transcript under root.
func Walk(root string) (*Stats, error) {
	s := &Stats{
		Date:      "",
		Root:      root,
		Projects:  map[string]int{},
		HunkExits: map[int]int{},
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		project := projectOf(root, path)
		if err := s.session(path, project); err != nil {
			return err
		}
		s.Sessions++
		return nil
	})
	return s, err
}

func projectOf(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "?"
	}
	if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
		return rel[:i]
	}
	return "?"
}

// session measures one transcript.
//
// Two passes, because a tool's result arrives on a later line than the call:
// the first collects results by id, the second walks the calls in order so an
// episode's calls can be counted between a failure and its success.
func (s *Stats) session(path, project string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var calls []call
	results := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), "tool_") {
			continue
		}
		var rec struct {
			Message struct {
				Content []struct {
					Type      string          `json:"type"`
					Name      string          `json:"name"`
					ID        string          `json:"id"`
					ToolUseID string          `json:"tool_use_id"`
					Input     json.RawMessage `json:"input"`
					Content   json.RawMessage `json:"content"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // a record shape this tool does not know is not an error
		}
		for _, c := range rec.Message.Content {
			switch c.Type {
			case "tool_use":
				var in struct {
					Command string `json:"command"`
				}
				_ = json.Unmarshal(c.Input, &in)
				calls = append(calls, call{id: c.ID, name: c.Name, command: in.Command})
			case "tool_result":
				results[c.ToolUseID] += string(c.Content)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	// firstFail is the index of the first failed attempt still open for a path.
	firstFail := map[string]int{}
	for i, c := range calls {
		s.ToolCalls++
		switch c.name {
		case "Edit", "MultiEdit", "Write", "NotebookEdit":
			s.EditToolCalls++
			continue
		case "Bash":
		default:
			continue
		}
		s.BashCalls++
		if strings.HasPrefix(strings.TrimSpace(c.command), "hunk ") ||
			strings.Contains(c.command, "| hunk ") {
			s.HunkCalls++
		}
		if !IsHeredocEdit(c.command) {
			continue
		}

		s.HeredocEdits++
		s.Projects[project]++
		s.CommandBytes += len(c.command)
		n := CountReplaces(c.command)
		s.Replaces += n
		if n > s.MaxReplaces {
			s.MaxReplaces = n
		}
		guarded := HasUniquenessGuard(c.command)
		limited := LimitsToFirstMatch(c.command)
		if !guarded {
			s.NoGuard++
		}
		if limited {
			s.FirstOnly++
		}
		if !guarded && !limited {
			s.NoGuardNorLimit++
		}
		if BundlesVerify(c.command) {
			s.BundledVerify++
		}
		if HandRollsBackup(c.command) {
			s.HandRolledBackup++
		}
		paths := Paths(c.command)
		s.FileTouches += len(paths)
		if len(paths) > s.MaxFiles {
			s.MaxFiles = len(paths)
		}

		failed := Failed(results[c.id])
		for _, p := range paths {
			key := project + "\x00" + p
			if failed {
				s.FailedAttempts++
				if _, open := firstFail[key]; !open {
					firstFail[key] = i
				}
				continue
			}
			start, open := firstFail[key]
			if !open {
				start = i
			}
			s.Episodes++
			s.CallsInEpisodes += i - start + 1
			delete(firstFail, key)
		}
	}
	return nil
}

// Report is §1's table, then the figures §12 needs and §1 does not have.
func (s *Stats) Report() string {
	var b strings.Builder
	pct := func(n int) string {
		if s.HeredocEdits == 0 {
			return ""
		}
		return fmt.Sprintf(" (%.0f%%)", 100*float64(n)/float64(s.HeredocEdits))
	}
	fmt.Fprintf(&b, "%d sessions under %s\n\n", s.Sessions, s.Root)
	fmt.Fprintf(&b, "                            calls    replaces\n")
	fmt.Fprintf(&b, "python-heredoc file edits %7d %11d\n", s.HeredocEdits, s.Replaces)
	fmt.Fprintf(&b, "  no uniqueness guard     %7d%s\n", s.NoGuard, pct(s.NoGuard))
	fmt.Fprintf(&b, "    of which also no      %7d%s   a count limit is not a guard\n",
		s.NoGuardNorLimit, pct(s.NoGuardNorLimit))
	fmt.Fprintf(&b, "    replace(..., 1) limit %7d%s\n", s.FirstOnly, pct(s.FirstOnly))
	fmt.Fprintf(&b, "  bundled a verify command%7d%s\n", s.BundledVerify, pct(s.BundledVerify))
	fmt.Fprintf(&b, "  hand-rolled backup      %7d%s\n", s.HandRolledBackup, pct(s.HandRolledBackup))
	fmt.Fprintf(&b, "bytes of those commands %9d\n", s.CommandBytes)
	if s.HeredocEdits > 0 {
		fmt.Fprintf(&b, "mean replaces per call     %6.1f, max %d\n",
			float64(s.Replaces)/float64(s.HeredocEdits), s.MaxReplaces)
		fmt.Fprintf(&b, "file touches            %9d over %d calls, max %d\n",
			s.FileTouches, s.HeredocEdits, s.MaxFiles)
	}
	fmt.Fprintf(&b, "\ntool mix overall           Bash %d    Edit %d\n\n", s.BashCalls, s.EditToolCalls)

	fmt.Fprintf(&b, "§12, which §1 does not have\n")
	fmt.Fprintf(&b, "  edit episodes           %9d\n", s.Episodes)
	fmt.Fprintf(&b, "  failed attempts         %9d\n", s.FailedAttempts)
	fmt.Fprintf(&b, "  calls per successful edit  %6.2f\n", s.CallsPerSuccessfulEdit())
	fmt.Fprintf(&b, "  hunk calls              %9d\n", s.HunkCalls)
	if len(s.HunkExits) > 0 {
		var codes []int
		for c := range s.HunkExits {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		for _, c := range codes {
			fmt.Fprintf(&b, "    exit %d                %7d\n", c, s.HunkExits[c])
		}
	}

	if len(s.Projects) > 0 {
		fmt.Fprintf(&b, "\nheredoc edits by project (%d projects)\n", len(s.Projects))
		type kv struct {
			k string
			v int
		}
		var rows []kv
		for k, v := range s.Projects {
			rows = append(rows, kv{k, v})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].v != rows[j].v {
				return rows[i].v > rows[j].v
			}
			return rows[i].k < rows[j].k
		})
		for _, r := range rows {
			fmt.Fprintf(&b, "  %-40s %6d\n", r.k, r.v)
		}
	}
	return b.String()
}
