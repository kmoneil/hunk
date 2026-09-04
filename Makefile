# hunk. Standard library only, no network, no code generation.

GO  ?= go
BIN := hunk

.PHONY: all build fmt fmt-check vet test gates deps-gate check hooks clean

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

check: fmt-check vet gates test

# Once per clone. The commit-msg gate refuses tool attribution.
hooks:
	git config core.hooksPath .githooks
	@echo "commit-msg gate enabled from .githooks"

clean:
	rm -f $(BIN)
