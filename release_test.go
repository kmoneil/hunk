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
	for _, want := range []string{"attest", "bump-the-bucket", "bump-the-tap", "publish"} {
		if !slices.Contains(guarded, want) {
			t.Errorf("job %s was not recognised as one that writes outside the run; found %v", want, guarded)
		}
	}
}

// Until every asset is uploaded, the release is a draft, and releases/latest
// does not serve a draft. Before this, the release went public the moment it
// was created, so a reader who hit the window between the create and the last
// upload got a failed install from the README installer, which stops on a
// missing asset (#59). The create step says draft: true, and a later step in
// the same job flips it off once the last file is up.
func TestTheReleaseIsPublishedOnlyWhole(t *testing.T) {
	publish := workflowJobs(readWorkflow(t, "release.yml"))["publish"]
	if publish == "" {
		t.Fatal("release.yml has no publish job")
	}
	create := strings.Index(publish, "uses: softprops/action-gh-release@")
	if create < 0 {
		t.Fatal("publish does not run action-gh-release, so this test read nothing")
	}
	if draft := strings.Index(publish, "draft: true"); draft < create {
		t.Error("the release is not created as a draft, so releases/latest serves it " +
			"while its assets are still uploading")
	}
	uploads := strings.LastIndex(publish, "dist/SHA256SUMS")
	if uploads < 0 {
		t.Fatal("publish does not upload dist/SHA256SUMS, so this test read nothing")
	}
	flip := strings.Index(publish, "-F draft=false")
	if flip < 0 {
		t.Error("nothing flips the draft, so the release would never go public")
	} else if flip < uploads {
		t.Error("the draft is published before the last asset is uploaded")
	}
}

// Nothing is published unattested: an attest job signs build provenance for
// everything in dist, publish waits on it, and what publish uploads comes from
// dist and nowhere else. The guard on attest itself, a job that writes outside
// the run, is TestReleasePublishesOnlyOnATagPush's to hold.
func TestEveryPublishedAssetIsAttestedFirst(t *testing.T) {
	jobs := workflowJobs(readWorkflow(t, "release.yml"))
	attest, publish := jobs["attest"], jobs["publish"]
	if attest == "" || publish == "" {
		t.Fatal("release.yml is missing the attest or the publish job")
	}
	if !strings.Contains(attest, "uses: actions/attest-build-provenance@") {
		t.Error("the attest job does not run attest-build-provenance")
	}
	if !regexp.MustCompile(`(?m)^\s+subject-path: dist/\*$`).MatchString(attest) {
		t.Error("the attestation does not cover dist/*, so a published asset could carry no provenance")
	}
	if !regexp.MustCompile(`(?m)^    needs: \[build, attest\]$`).MatchString(publish) {
		t.Error("publish does not wait on attest, so a release could go out before its provenance exists")
	}
	lines := strings.Split(publish, "\n")
	uploaded := 0
	for i, l := range lines {
		if strings.TrimSpace(l) != "files: |" {
			continue
		}
		key := len(l) - len(strings.TrimLeft(l, " "))
		for _, f := range lines[i+1:] {
			if strings.TrimSpace(f) == "" || len(f)-len(strings.TrimLeft(f, " ")) <= key {
				break
			}
			uploaded++
			if !strings.HasPrefix(strings.TrimSpace(f), "dist/") {
				t.Errorf("publish uploads %s, which is outside dist, the attested set", strings.TrimSpace(f))
			}
		}
	}
	if uploaded < 2 {
		t.Errorf("found %d uploads under files: |, want at least the binaries glob and SHA256SUMS", uploaded)
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
