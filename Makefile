GO      ?= go
BIN     := bin/pic-sure
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build build-release test fmt-check vet lint print-lint-version check clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/pic-sure

# Release build — the single place release ldflags live; local cross-builds
# and the CI release matrix both call this.
# Usage: make build-release GOOS=linux GOARCH=arm64 [OUT=dist/pic-sure]
OUT ?= dist/pic-sure
build-release:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath \
		-ldflags "-s -w $(LDFLAGS)" -o $(OUT) ./cmd/pic-sure

test:
	$(GO) test ./...

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

# Pinned lint version — CI installs exactly this and runs the same target,
# so local and CI lint cannot drift. Never bump it in two places: only here.
GOLANGCI_LINT_VERSION := v2.12.2

lint:
	@golangci-lint version 2>/dev/null | grep -q "$(GOLANGCI_LINT_VERSION:v%=%)" || \
		echo "warning: golangci-lint $(GOLANGCI_LINT_VERSION) expected ($$(golangci-lint version 2>/dev/null || echo 'not installed'))"
	golangci-lint run $$($(GO) list -f '{{.Dir}}' ./...)

print-lint-version:
	@echo $(GOLANGCI_LINT_VERSION)

# What CI runs. The PTY tests in smoke/ run as part of `test`.
check: fmt-check vet lint test

clean:
	rm -rf bin dist
