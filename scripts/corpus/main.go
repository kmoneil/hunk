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
	//
	// HunkCalls keeps its meaning, every invocation including the probes, so
	// the baseline's "hunk calls 1" stays true. HunkApplications is the
	// denominator of the rates: an exit-2 rate over a count that includes
	// --help is not a rate.
	HunkCalls        int         `json:"hunk_calls"`
	HunkApplications int         `json:"hunk_applications"`
	HunkProbes       int         `json:"hunk_probes"`
	HunkUnclassified int         `json:"hunk_unclassified"`
	HunkExits        map[int]int `json:"hunk_exit_codes,omitempty"`

	// §12: "the fraction of exit-2s followed by a success on the next call.
	// That fraction is the near-miss report's score." Exit2Followed is the
	// denominator: an exit 2 whose next application this file could classify.
	// A pair whose second half is unclassified counts in neither, because
	// calling it a failure to recover would score §7 against a gap in this
	// classifier.
	Exit2Followed  int `json:"exit2_with_a_classified_next_call"`
	Exit2Recovered int `json:"exit2_followed_by_exit0"`

	// Adoption, which §12 names as the open risk and had no instrument for
	// until 2026-09-07. Counted per call that used a thing at least once
	// rather than per occurrence: the question is whether an agent reaches
	// for it at all, not how many hunks a patch had.
	HunkTrivialNotes  int            `json:"hunk_trivial_notes"`
	HunkDirectives    map[string]int `json:"hunk_directives,omitempty"`
	HunkFlagUse       map[string]int `json:"hunk_flags,omitempty"`
	HunkPatchFromFile int            `json:"hunk_patch_from_file"`

	// A verify that rewrites files is the only way to reach exit 4, so this is
	// the number §11's second open question has been waiting on since
	// 2026-09-05. Zero exit-4s over a corpus with none of these in it is not
	// evidence that the default is right; it is evidence that the case has not
	// arisen.
	HunkFormattingVerify int `json:"hunk_verify_rewrites_files"`
}

// hunkAdoption records what one hunk call reached for. Counted over every
// invocation including the probes: "hunk format" passing no flags and no
// directives is a fact about adoption too, and excluding it would need a rule
// about which calls are allowed to have none.
func (s *Stats) hunkAdoption(cmd, result string) {
	if PrintsTrivialNote(result) {
		s.HunkTrivialNotes++
	}
	for _, d := range Directives(cmd) {
		s.HunkDirectives[d]++
	}
	for _, f := range HunkFlags(cmd) {
		s.HunkFlagUse[f]++
	}
	if ReadsPatchFromFile(cmd) {
		s.HunkPatchFromFile++
	}
	if VerifyRewritesFiles(cmd) {
		s.HunkFormattingVerify++
	}
}

// Exit2RecoveryRate is §7's score: of the refusals whose next application is
// known, how many were followed by one that applied. §12: "If it is low, §7 is
// not doing its job."
func (s Stats) Exit2RecoveryRate() float64 {
	if s.Exit2Followed == 0 {
		return 0
	}
	return float64(s.Exit2Recovered) / float64(s.Exit2Followed)
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
		Date:           "",
		Root:           root,
		Projects:       map[string]int{},
		HunkExits:      map[int]int{},
		HunkDirectives: map[string]int{},
		HunkFlagUse:    map[string]int{},
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
				results[c.ToolUseID] += resultText(c.Content)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	// hunkExits is every hunk application's exit in call order, for §12's
	// recovery fraction. Probes are not in it: an agent running --help between
	// a refusal and its fix has not failed to recover.
	var hunkExits []int

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
		if IsHunkCall(c.command) {
			s.HunkCalls++
			s.hunkAdoption(c.command, results[c.id])
			switch code := HunkExit(c.command, results[c.id]); code {
			case ExitProbe:
				s.HunkProbes++
			case ExitUnclassified:
				s.HunkApplications++
				s.HunkUnclassified++
				hunkExits = append(hunkExits, ExitUnclassified)
			default:
				s.HunkApplications++
				s.HunkExits[code]++
				hunkExits = append(hunkExits, code)
			}
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

	// §12's recovery fraction, within a session and in call order. A refusal
	// with no application after it is not a failure to recover, it is the end
	// of the session, so it is in neither the numerator nor the denominator.
	for i, code := range hunkExits {
		if code != ExitNoMatch || i+1 >= len(hunkExits) {
			continue
		}
		if hunkExits[i+1] == ExitUnclassified {
			continue
		}
		s.Exit2Followed++
		if hunkExits[i+1] == ExitOK {
			s.Exit2Recovered++
		}
	}
	return nil
}

// resultText is a tool_result's content as the text an agent saw. The field is
// either a JSON string or a list of blocks, so the raw bytes will not do: they
// carry the quotes and leave the newlines escaped, and no rule that anchors to
// the start of a line can read them.
//
// Failed() has read the raw form since this file was written and gets away with
// it because its patterns are substrings with no anchors. HunkExit's are
// anchored, because a verify command's own output shares the same result, and
// on raw bytes every one of them silently matched nothing: five applications
// classified, five unclassified. Found by running it.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			b.WriteString(x.Text)
		}
		return b.String()
	}
	return string(raw) // a shape this tool does not know is better than nothing
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
	if s.HunkCalls > 0 {
		fmt.Fprintf(&b, "    applications          %7d\n", s.HunkApplications)
		fmt.Fprintf(&b, "    probes                %7d\n", s.HunkProbes)
		var codes []int
		for c := range s.HunkExits {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		for _, c := range codes {
			fmt.Fprintf(&b, "      exit %d              %7d\n", c, s.HunkExits[c])
		}
		// Printed even at zero. A silent zero is what hid an unwritten map
		// behind an omitempty for a day.
		fmt.Fprintf(&b, "      unclassified        %7d\n", s.HunkUnclassified)
		fmt.Fprintf(&b, "  exit-2 recovery            %6.2f   %d of %d\n",
			s.Exit2RecoveryRate(), s.Exit2Recovered, s.Exit2Followed)

		// Adoption. The zeros are the point, so the directives are printed in
		// the tool's own order rather than only the ones somebody used.
		applied := s.HunkExits[ExitOK]
		fmt.Fprintf(&b, "  trivial note printed    %9d   of %d that applied\n",
			s.HunkTrivialNotes, applied)
		fmt.Fprintf(&b, "  directives used          ")
		for _, d := range []string{"file", "old", "new", "create", "delete", "append", "prepend"} {
			fmt.Fprintf(&b, " %s %d", d, s.HunkDirectives[d])
		}
		fmt.Fprintln(&b)
		if s.HunkPatchFromFile > 0 {
			fmt.Fprintf(&b, "    patch from a file (-f)%7d   directives unknown for those\n",
				s.HunkPatchFromFile)
		}
		fmt.Fprintf(&b, "  flags used               ")
		type fu struct {
			flag string
			n    int
		}
		var flags []fu
		for f, n := range s.HunkFlagUse {
			flags = append(flags, fu{f, n})
		}
		sort.Slice(flags, func(i, j int) bool {
			if flags[i].n != flags[j].n {
				return flags[i].n > flags[j].n
			}
			return flags[i].flag < flags[j].flag
		})
		for _, f := range flags {
			fmt.Fprintf(&b, " %s %d", f.flag, f.n)
		}
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "  verify rewrites files   %9d   exit 4 needs one of these\n",
			s.HunkFormattingVerify)
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
