# hunk. Standard library only, no network, no code generation.

GO  ?= go
BIN := hunk

.PHONY: all build fmt fmt-check vet test gates deps-gate check corpus corpus-test hooks clean

all: check

build:
	$(GO) build -o $(BIN) .

fmt:
	$(GO) fmt ./...

fmt-check:
	@out=$$(gofmt -l .); \
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

# Once per clone. The commit-msg gate refuses tool attribution.
hooks:
	git config core.hooksPath .githooks
	@echo "commit-msg gate enabled from .githooks"

clean:
	rm -f $(BIN)
