# hunk. Standard library only, no network, no code generation.

GO  ?= go
BIN := hunk

.PHONY: all build fmt fmt-check vet test gates deps-gate check corpus corpus-test skill-grade install hooks clean

all: check

build:
	$(GO) build -o $(BIN) .

fmt:
	$(GO) fmt ./...

# The eval fixtures under skills/*-workspace/ include deliberately unformatted
# Go, because one eval case is about a verify command that reformats. They are
# not this module's source and gofmt has no business walking them.
fmt-check:
	@out=$$(gofmt -l . | grep -v '^skills/[^/]*-workspace/' || true); \
	if [ -n "$$out" ]; then \
		echo "not gofmt'd:"; echo "$$out"; exit 1; \
	fi

vet:
	$(GO) vet ./...

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

check: fmt-check vet gates test corpus-test

# Grade the skill eval runs in skills/hunk-workspace/. Does not launch the runs
# (those are agent invocations); it scores the trees they left behind.
skill-grade:
	python3 skills/hunk/evals/grade.py

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

# Once per clone. The commit-msg gate refuses tool attribution.
hooks:
	git config core.hooksPath .githooks
	@echo "commit-msg gate enabled from .githooks"

clean:
	rm -f $(BIN)
