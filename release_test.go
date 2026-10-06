package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// release.yml's manual trigger exists so the file can be exercised without
// spending a version, and until 2026-10-06 it said a dispatch released nothing
// because publish was guarded on the ref being a v* tag. A dispatch can name a
// tag as its ref, though, and then that guard passes: the tag is rebuilt, its
// release's assets are deleted and uploaded again, and the tap and the bucket
// are told to move to it, which after a newer release moves them back.
//
// So every job that can change something outside the run is gated on the
// event, not the ref. "Can change something" is read off the job rather than
// listed here, so a job added later is held to it without anybody remembering
// to add it: a write permission, or a secret.
func TestReleasePublishesOnlyOnATagPush(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	writes := regexp.MustCompile(`(?m)^\s+[a-z-]+: write$|secrets\.`)
	condition := regexp.MustCompile(`(?m)^    if: (.*)$`)
	jobs := workflowJobs(string(b))
	var guarded []string
	for name, body := range jobs {
		if !writes.MatchString(body) {
			continue
		}
		guarded = append(guarded, name)
		cond := condition.FindStringSubmatch(body)
		if cond == nil || !strings.Contains(cond[1], "github.event_name == 'push'") {
			t.Errorf("job %s can write outside the run but is not gated on github.event_name == 'push'; "+
				"a workflow_dispatch on an existing tag would run it", name)
		}
	}
	// The parse is a few regular expressions over YAML, so it is checked
	// against what the file is known to hold: a reader that found no jobs
	// would pass every assertion above.
	slices.Sort(guarded)
	for _, want := range []string{"bump-the-bucket", "bump-the-tap", "publish"} {
		if !slices.Contains(guarded, want) {
			t.Errorf("job %s was not recognised as one that writes outside the run; found %v", want, guarded)
		}
	}
}

// workflowJobs splits a workflow into its jobs' text, by name, with comment
// lines removed so that a job is judged by what it does rather than by what
// its comments mention. It reads the shape this repository's workflows are
// written in, two spaces per level, and nothing more general.
func workflowJobs(src string) map[string]string {
	jobs := map[string]string{}
	header := regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)
	in, name := false, ""
	var body strings.Builder
	flush := func() {
		if name != "" {
			jobs[name] = body.String()
		}
		body.Reset()
	}
	for line := range strings.SplitSeq(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line == "jobs:" {
			in = true
			continue
		}
		if !in {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		if m := header.FindStringSubmatch(line); m != nil {
			flush()
			name = m[1]
			continue
		}
		body.WriteString(line + "\n")
	}
	flush()
	return jobs
}
