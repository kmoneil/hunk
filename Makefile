# hunk. Standard library only, no network, no code generation.

GO  ?= go
BIN := hunk

# Every developer tool is pinned here and nowhere else, and CI reads these
# values with `make -s print-GOFUMPT_VERSION` rather than repeating them. A
# floating formatter or linter fails a pull request that changed nothing, and
# fails a release, because the release gate re-runs on a tag that cannot move.
#
# None of these is a dependency. They are installed as binaries and never enter
# go.mod, which deps-gate below is what actually proves.
GOFUMPT_VERSION        := v0.11.0
GOLANGCI_LINT_VERSION  := v2.12.2
GOVULNCHECK_VERSION    := v1.6.0

# A stdlib-only static binary that was 3,802,193 bytes when this was written.
# The budget is not a target, it is a tripwire: an embed, a dependency or a
# large table would move it and nothing else in the gate would notice.
SIZE_BUDGET_BYTES := 4718592

FUZZTIME ?= 20s

.PHONY: all build fmt fmt-check vet lint test gates deps-gate check corpus corpus-test skill-grade skill-grade-test install hooks tools vuln size fuzz fuzz-targets clean

all: check

build:
	$(GO) build -o $(BIN) .

fmt:
	gofumpt -w $$(git ls-files '*.go' | grep -v '^skills/')
	cd scripts/corpus && gofumpt -w .

# The eval fixtures under skills/*-workspace/ include deliberately unformatted
# Go, because one eval case is about a verify command that reformats. They are
# not this module's source and gofmt has no business walking them.
fmt-check:
	@out=$$(gofumpt -l . 2>/dev/null | grep -v '^skills/[^/]*-workspace/' || true); \
	if [ -n "$$out" ]; then \
		echo "not gofumpt'd:"; echo "$$out"; echo "run: make fmt"; exit 1; \
	fi

vet:
	$(GO) vet ./...

# `config verify` first, and not as a formality. Under `version: "2"` an
# unknown top-level key is ignored rather than rejected, so a v1 key name
# silently disables whatever it configures and the run still says ok.
lint:
	golangci-lint config verify
	golangci-lint run ./...

# There are no dependencies, so everything this can find is in the standard
# library the module builds against. That is the reason to run it rather than a
# reason to skip it: GO-2026-4970 was a root escape via symlink in os.Root,
# which is the exact mechanism paths.go relies on for confinement, and it was
# live in the toolchain this module asked for until the go directive was moved
# to 1.26.8.
vuln:
	govulncheck ./...

# The tripwire, not a target. See SIZE_BUDGET_BYTES.
size:
	@$(GO) build -o /tmp/hunk-size .
	@n=$$(wc -c < /tmp/hunk-size); rm -f /tmp/hunk-size; \
	if [ "$$n" -gt "$(SIZE_BUDGET_BYTES)" ]; then \
		echo "binary is $$n bytes, over the $(SIZE_BUDGET_BYTES) budget"; exit 1; \
	fi; \
	echo "binary $$n bytes, budget $(SIZE_BUDGET_BYTES)"

# Discovered rather than listed, so the nightly cannot silently sweep a subset.
# A hand-written list of targets goes stale the moment somebody adds one, and
# the five it skipped would look exactly like five that passed.
fuzz-targets:
	@$(GO) test -list='^Fuzz' . | grep '^Fuzz'

# Every target in the tree, each for FUZZTIME, or one with FUZZTARGET. `go
# test` already replays every seed and every committed crasher on every run,
# which is the regression half and the half worth blocking a merge on. This is
# the discovery half.
fuzz:
	@for t in $${FUZZTARGET:-$$($(GO) test -list='^Fuzz' . | grep '^Fuzz')}; do \
		echo "fuzz: $$t"; \
		$(GO) test -run='^$$' -fuzz="^$$t$$" -fuzztime=$(FUZZTIME) . || exit 1; \
	done

test:
	$(GO) test ./...

# The claims the spec makes that the compiler does not enforce.
gates: deps-gate

# §8: "no dependencies outside the standard library". That is a one-line
# promise with nothing else keeping it, so it gets a gate rather than an
# intention. go.sum is checked too: a require line can be removed while the sum
# file still records that one was there.
deps-gate:
	@if grep -qE '^[[:space:]]*require' go.mod; then \
		echo "go.mod has a require directive. hunk is standard library only (SPEC §8)."; \
		exit 1; \
	fi
	@if [ -s go.sum ]; then \
		echo "go.sum is non-empty. hunk is standard library only (SPEC §8)."; \
		exit 1; \
	fi

check: fmt-check vet lint gates test corpus-test

# Grade the skill eval runs in skills/hunk-workspace/. Does not launch the runs
# (those are agent invocations); it scores the trees they left behind.
#
#   make skill-grade WORKSPACE=skills/hunk-workspace/iteration-4
#
# Defaults to the iteration in grade.py. The grader's own tests run first: it
# has been wrong four times and each fix moved a published number, so "check
# the instrument before believing the measurement" gets a mechanism rather than
# a sentence in a README.
skill-grade: skill-grade-test
	python3 skills/hunk/evals/grade.py $(WORKSPACE)

skill-grade-test:
	python3 skills/hunk/evals/grade_test.py

# scripts/corpus is its own module: it is not part of the binary and must not
# appear in hunk's coverage, its dependency gate, or its import graph. Its tests
# still run under `make check`, because the classifier it holds is what §12's
# comparison rests on.
corpus-test:
	cd scripts/corpus && $(GO) vet ./... && $(GO) test ./...

# Re-derive §1's table and §12's figures. Writes a dated report to _reports/,
# which is gitignored: the method is tracked, the measurement is not.
corpus:
	@mkdir -p _reports
	cd scripts/corpus && $(GO) run . > ../../_reports/corpus-$$(date +%Y-%m-%d).txt
	cd scripts/corpus && $(GO) run . --json > ../../_reports/corpus-$$(date +%Y-%m-%d).json
	@echo "wrote _reports/corpus-$$(date +%Y-%m-%d).{txt,json}"

# Install the binary and the agent skill for personal use.
#
# The skill ships SKILL.md and references/ only. evals/ is development
# material: a grader and its cases, of no use to a session that is trying to
# edit a file.
SKILLDIR ?= $(HOME)/.claude/skills/hunk
install:
	$(GO) install .
	@mkdir -p $(SKILLDIR)/references
	@cp skills/hunk/SKILL.md $(SKILLDIR)/
	@cp skills/hunk/references/*.md $(SKILLDIR)/references/
	@echo "binary:  $$($(GO) env GOPATH)/bin/hunk"
	@echo "skill:   $(SKILLDIR)/SKILL.md"

# Once per clone. Enables both hooks: commit-msg refuses tool attribution, and
# pre-commit runs the gate cheapest-first.
hooks:
	git config core.hooksPath .githooks
	@echo "hooks enabled from .githooks: commit-msg, pre-commit"

# The pinned developer tools. Not dependencies: `go install` puts binaries in
# GOPATH/bin and never touches go.mod, which `make gates` re-proves afterwards.
tools:
	$(GO) install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	@$(MAKE) --no-print-directory gates

# So CI reads a version from one place. `make -s print-GOFUMPT_VERSION`.
print-%:
	@echo "$($*)"

clean:
	rm -f $(BIN)
