// Command nightly-find takes in what the nightly fuzz sweep found: the run's
// failed fuzz jobs, what each job's log says it was, the inputs its artifact
// holds that the tree has not committed, copied under testdata/fuzz, and the
// body of the card that works it.
//
//	nightly-find [--root DIR] [--repo OWNER/NAME] [--no-copy] [RUN]
//
// RUN is a run's number or its URL, and without one it is the latest failed
// fuzz-nightly run. The body goes to stdout and what was copied to stderr, so
// the body can be piped into karte add.
//
// Done by hand it was the same nine steps each time (t123, t125, t150, t159),
// and the step that decides whether there is anything to fix at all, reading
// the toolchain line for golang/go#75804, was the one the issue's own text had
// to explain.
//
// Its own module, as scripts/corpus is: it is not part of the binary and must
// not appear in hunk's coverage, its dependency gate or its import graph.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A runner runs a command, gh or git, and returns its stdout. Everything the
// tool learns comes through one, so its tests can say what each would print.
type runner func(name string, args ...string) ([]byte, error)

// execRunner is the real one. bin maps a command's name to the program run
// for it, which is the name itself unless a test has said otherwise.
func execRunner(bin map[string]string) runner {
	return func(name string, args ...string) ([]byte, error) {
		prog := name
		if b, ok := bin[name]; ok {
			prog = b
		}
		cmd := exec.Command(prog, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, execRunner(nil)))
}

// run is the command: 0 when it printed a body, 1 when it could not take the
// run in or a file was left for a person to look at, 2 for a bad call.
func run(args []string, stdout, stderr io.Writer, cmd runner) int {
	flags := flag.NewFlagSet("nightly-find", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "the repository whose testdata/fuzz takes the inputs")
	repo := flags.String("repo", "kmoneil/hunk", "the GitHub repository the run is in")
	noCopy := flags.Bool("no-copy", false, "name the new inputs without copying them")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(stderr, "nightly-find: one run at most, as a number or a URL")
		return 2
	}
	id := ""
	if flags.NArg() == 1 {
		var ok bool
		if id, ok = parseRun(flags.Arg(0)); !ok {
			fmt.Fprintf(stderr, "nightly-find: %q is not a run's number or URL\n", flags.Arg(0))
			return 2
		}
	}
	in, err := intake(cmd, *repo, *root, id, !*noCopy)
	if err != nil {
		fmt.Fprintf(stderr, "nightly-find: %v\n", err)
		return 1
	}
	fmt.Fprint(stdout, in.body())
	fmt.Fprint(stderr, in.summary())
	if len(in.problems) > 0 {
		return 1
	}
	return 0
}

// runURL is how a run's URL ends: the number, then nothing or another part.
var runURL = regexp.MustCompile(`/actions/runs/([0-9]+)(?:/|$)`)

// parseRun reads a run's number from the number itself or its URL.
func parseRun(s string) (string, bool) {
	if s != "" && strings.Trim(s, "0123456789") == "" {
		return s, true
	}
	if m := runURL.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	return "", false
}

// runInfo is the part of `gh run view --json` the intake reads.
type runInfo struct {
	ID       int64  `json:"databaseId"`
	HeadSha  string `json:"headSha"`
	URL      string `json:"url"`
	Workflow string `json:"workflowName"`
	Jobs     []struct {
		ID         int64  `json:"databaseId"`
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
	} `json:"jobs"`
}

// issueInfo is one result of `gh issue list --json`.
type issueInfo struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	CreatedAt string `json:"createdAt"`
}

// job is one failed fuzz job: its target, what its log said, and the inputs
// its artifact holds that the tree has not committed.
type job struct {
	target string
	log    jobLog
	inputs []input
}

// input is a file the artifact holds under testdata/fuzz that the tree has
// not committed, and what became of it.
type input struct {
	target, name string
	state        string // "copied", "not copied", "already in the tree"
}

// path is where an input goes in the tree, slash-separated as the log says it.
func (in input) path() string { return "testdata/fuzz/" + in.target + "/" + in.name }

// found is the whole intake of one run.
type found struct {
	run       runInfo
	describe  string // git describe of the run's commit, "" when the clone lacks it
	issue     *issueInfo
	jobs      []job
	committed map[string]bool // what git tracks under testdata/fuzz
	problems  []string        // what a person has to look at: a file not copied, an input missing
}

// fuzzJob is the name the nightly gives a target's job.
var fuzzJob = regexp.MustCompile(`^fuzz \((Fuzz[A-Za-z0-9_]+)\)$`)

// intake reads run id, the latest failed fuzz-nightly run when id is empty,
// and takes in each failed fuzz job.
func intake(cmd runner, repo, root, id string, copyInputs bool) (*found, error) {
	if id == "" {
		out, err := cmd("gh", "run", "list", "-R", repo, "--workflow", "fuzz-nightly.yml", "--status", "failure",
			"--limit", "1", "--json", "databaseId")
		if err != nil {
			return nil, err
		}
		var runs []runInfo
		if err := json.Unmarshal(out, &runs); err != nil {
			return nil, fmt.Errorf("gh run list: %v", err)
		}
		if len(runs) == 0 {
			return nil, errors.New("no failed fuzz-nightly run")
		}
		id = strconv.FormatInt(runs[0].ID, 10)
	}
	out, err := cmd("gh", "run", "view", id, "-R", repo, "--json", "databaseId,headSha,url,workflowName,jobs")
	if err != nil {
		return nil, err
	}
	f := &found{}
	if err := json.Unmarshal(out, &f.run); err != nil {
		return nil, fmt.Errorf("gh run view: %v", err)
	}
	committed, err := committedInputs(cmd, root)
	if err != nil {
		return nil, err
	}
	f.committed = committed
	for _, j := range f.run.Jobs {
		m := fuzzJob.FindStringSubmatch(j.Name)
		if m == nil || j.Conclusion != "failure" {
			continue
		}
		raw, err := cmd("gh", "api", "--allow-escape-sequences", fmt.Sprintf("repos/%s/actions/jobs/%d/logs", repo, j.ID))
		if err != nil {
			return nil, err
		}
		fj := job{target: m[1], log: parseLog(raw)}
		dir, err := os.MkdirTemp("", "nightly-find-")
		if err != nil {
			return nil, err
		}
		// The artifact is the runner's files, so it goes in a directory of its
		// own and is read, never run, and removed whatever happens.
		fj.inputs, err = artifactInputs(cmd, repo, id, fj.target, dir, committed)
		_ = os.RemoveAll(dir)
		if err != nil {
			return nil, err
		}
		f.jobs = append(f.jobs, fj)
	}
	if len(f.jobs) == 0 {
		return nil, fmt.Errorf("run %s has no failed fuzz job", id)
	}
	if out, err := cmd("git", "-C", root, "describe", "--tags", f.run.HeadSha); err == nil {
		f.describe = strings.TrimSpace(string(out))
	}
	if out, err := cmd("gh", "issue", "list", "-R", repo, "--state", "all", "--search", id+" in:body",
		"--json", "number,title,url,createdAt"); err == nil {
		var issues []issueInfo
		if json.Unmarshal(out, &issues) == nil && len(issues) > 0 {
			f.issue = &issues[0]
		}
	}
	for i := range f.jobs {
		f.settle(&f.jobs[i], root, copyInputs)
	}
	return f, nil
}

// committedInputs is every file git tracks under testdata/fuzz, as a set of
// slash-separated paths.
func committedInputs(cmd runner, root string) (map[string]bool, error) {
	out, err := cmd("git", "-C", root, "ls-files", "-z", "--", "testdata/fuzz")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for p := range strings.SplitSeq(string(out), "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return set, nil
}

// corpusName is the name Go gives a fuzz input: sixteen hex digits.
var corpusName = regexp.MustCompile(`^[0-9a-f]{16}$`)

// corpusHeader is how every fuzz corpus file begins.
const corpusHeader = "go test fuzz v1\n"

// artifactInputs downloads target's crasher artifact into dir and returns
// each input in it the tree has not committed. An entry that is not a
// directory of a target holding regular files named as Go names them, each a
// corpus file, is no input of Go's, and is refused rather than passed over:
// it would be the one the run wrote.
func artifactInputs(cmd runner, repo, id, target, dir string, committed map[string]bool) ([]input, error) {
	if _, err := cmd("gh", "run", "download", id, "-R", repo, "-n", "crasher-"+target, "-D", dir); err != nil {
		return nil, err
	}
	var inputs []input
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		switch {
		case rel == ".":
			return nil
		case len(parts) == 1 && d.IsDir():
			return nil
		case len(parts) != 2 || !d.Type().IsRegular():
			return fmt.Errorf("the artifact holds %s, which is not a fuzz input", filepath.ToSlash(rel))
		case !corpusName.MatchString(parts[1]):
			return fmt.Errorf("the artifact holds %s, whose name is not one Go gives an input", filepath.ToSlash(rel))
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(b, []byte(corpusHeader)) {
			return fmt.Errorf("the artifact holds %s, which is not a fuzz corpus file", filepath.ToSlash(rel))
		}
		in := input{target: parts[0], name: parts[1]}
		if !committed[in.path()] {
			in.state = string(b) // held until settle, which replaces it
			inputs = append(inputs, in)
		}
		return nil
	})
	return inputs, err
}

// settle copies each of a job's new inputs into the tree, or says why not,
// and checks the input the log names is among them, or committed already, as
// it is once the find has been worked.
func (f *found) settle(j *job, root string, copyInputs bool) {
	for i := range j.inputs {
		in := &j.inputs[i]
		content := in.state
		dst := filepath.Join(root, filepath.FromSlash(in.path()))
		have, err := os.ReadFile(dst)
		switch {
		case err == nil && string(have) == content:
			in.state = "already in the tree"
		case err == nil:
			in.state = "not copied"
			f.problems = append(f.problems, in.path()+" is in the tree with other bytes, and was left as it is")
		case !copyInputs:
			in.state = "not copied"
		default:
			if err := copyInput(dst, []byte(content)); err != nil {
				in.state = "not copied"
				f.problems = append(f.problems, fmt.Sprintf("%s could not be copied: %v", in.path(), err))
				continue
			}
			in.state = "copied"
		}
	}
	if w := j.log.input; w != "" && !f.committed[w] && !slices.ContainsFunc(j.inputs, func(in input) bool { return in.path() == w }) {
		f.problems = append(f.problems, "the log names "+w+", and the artifact holds no new file by that name")
	}
}

// copyInput writes an input where none is, and only there. It is the one
// place the tool writes into the tree.
func copyInput(dst string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	w, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

// jobLog is what a fuzz job's log says about its failure.
type jobLog struct {
	toolchain string   // the toolchain line's version, "go1.27.3"; "" with no such line
	failure   []string // the --- FAIL block, as Go printed it
	input     string   // the path "Failing input written to" names; "" for none
	deadline  bool     // the block has the line "context deadline exceeded"
	hung      bool     // the block says the worker hung or died
}

var (
	// ansi is a terminal escape: the Run lines of a job log are coloured.
	ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	// stamp is the time Actions puts before every line of a raw job log.
	stamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z `)
)

// parseLog reads a raw job log: a byte order mark, then each line after a
// time, some of them coloured. The failure block runs from the first
// "--- FAIL: " to the line go test ends a failed package with.
func parseLog(raw []byte) jobLog {
	var l jobLog
	in := false
	for line := range strings.SplitSeq(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"), "\n") {
		line = ansi.ReplaceAllString(stamp.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""), "")
		if v, ok := strings.CutPrefix(line, "fuzz: toolchain "); ok && l.toolchain == "" {
			l.toolchain = strings.TrimSpace(v)
			continue
		}
		if !in {
			in = strings.HasPrefix(line, "--- FAIL: ") && l.failure == nil
			if !in {
				continue
			}
		}
		if line == "FAIL" || strings.HasPrefix(line, "FAIL\t") || strings.HasPrefix(line, "exit status ") {
			in = false
			continue
		}
		l.failure = append(l.failure, line)
		t := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(t, "Failing input written to "); ok {
			l.input = v
		}
		l.deadline = l.deadline || t == "context deadline exceeded"
		l.hung = l.hung || strings.HasPrefix(t, "fuzzing process hung or terminated unexpectedly")
	}
	for len(l.failure) > 0 && strings.TrimSpace(l.failure[len(l.failure)-1]) == "" {
		l.failure = l.failure[:len(l.failure)-1]
	}
	return l
}

// goVersion is a toolchain's major and minor version.
var goVersion = regexp.MustCompile(`^go([0-9]+)\.([0-9]+)`)

// fixedFlake reports whether a toolchain has golang/go#75804's fix, which
// go1.27.0 is the first to carry, and whether it could tell.
func fixedFlake(toolchain string) (fixed, known bool) {
	m := goVersion.FindStringSubmatch(toolchain)
	if m == nil {
		return false, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > 1 || major == 1 && minor >= 27, true
}

// verdict is what a job's failure was, in the issue's own terms.
func (l jobLog) verdict() string {
	switch {
	case l.failure == nil:
		return "no --- FAIL line in its log, so it failed some other way: read the log"
	case l.input != "" && l.hung:
		return "a find of time or memory: the fuzzing worker hung or died on the input"
	case l.input != "":
		return "a find"
	case !l.deadline:
		return "a failure with no input written: read its lines"
	}
	fixed, known := fixedFlake(l.toolchain)
	switch {
	case l.toolchain == "" || known && !fixed:
		return "golang/go#75804's deadline flake, as nothing was found on a toolchain without the fix: re-run the job and commit nothing"
	default:
		return "a deadline with no input on " + l.toolchain + ", which is not golang/go#75804: keep the run and look before re-running"
	}
}

// commit is the run's commit as a card names it.
func (f *found) commit() string {
	short := f.run.HeadSha
	if len(short) > 7 {
		short = short[:7]
	}
	if f.describe == "" {
		return short + " (not in this clone: git fetch to describe it)"
	}
	return short + " (" + f.describe + ")"
}

// body is the card's body, in the shape the intake cards have had.
func (f *found) body() string {
	var b strings.Builder
	if f.issue != nil {
		day, _, _ := strings.Cut(f.issue.CreatedAt, "T")
		fmt.Fprintf(&b, "GitHub issue #%d, %s\n%s, opened %s by the nightly fuzz workflow.\n\n",
			f.issue.Number, f.issue.Title, f.issue.URL, day)
	}
	fmt.Fprintf(&b, "Nightly run %d on %s: %s\n", f.run.ID, f.commit(), f.run.URL)
	finds := false
	for _, j := range f.jobs {
		tc := "no toolchain line"
		if j.log.toolchain != "" {
			tc = `toolchain line "fuzz: toolchain ` + j.log.toolchain + `"`
		}
		fmt.Fprintf(&b, "\nJob \"fuzz (%s)\", %s: %s.", j.target, tc, j.log.verdict())
		switch len(j.inputs) {
		case 0:
			if f.committed[j.log.input] {
				fmt.Fprintf(&b, " The input it wrote, %s, is committed already.", j.log.input)
			} else {
				b.WriteString(" The artifact holds no input the tree has not committed.")
			}
		case 1:
			fmt.Fprintf(&b, " The only new input in the artifact is %s (%s).", j.inputs[0].path(), j.inputs[0].state)
		default:
			b.WriteString(" The new inputs in the artifact are")
			for i, in := range j.inputs {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, " %s (%s)", in.path(), in.state)
			}
			b.WriteString(".")
		}
		b.WriteString("\n")
		if len(j.log.failure) > 0 {
			fmt.Fprintf(&b, "\n```\n%s\n```\n", strings.Join(j.log.failure, "\n"))
		}
		finds = finds || j.log.input != ""
	}
	if finds {
		b.WriteString("\ndone: root cause, the fix, and the test that covers it\n")
	} else {
		b.WriteString("\ndone: the failed jobs re-run, or the failure explained; nothing committed\n")
	}
	return b.String()
}

// summary is what the run did to the tree and what is left to a person, for
// stderr.
func (f *found) summary() string {
	var b strings.Builder
	for _, j := range f.jobs {
		for _, in := range j.inputs {
			fmt.Fprintf(&b, "%s: %s\n", in.path(), in.state)
		}
	}
	for _, p := range f.problems {
		fmt.Fprintf(&b, "look: %s\n", p)
	}
	if f.issue != nil {
		fmt.Fprintf(&b, "next: karte add \"TITLE\" --type bug -p 2 -m gh=%d -b -   (the body above on stdin)\n", f.issue.Number)
	}
	return b.String()
}
