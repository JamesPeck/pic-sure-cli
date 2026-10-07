GO      ?= go
BIN     := bin/pic-sure
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build snapshot install-test test fmt-check vet lint print-lint-version check compose-check clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/pic-sure

# A local dry run of the release (.goreleaser.yaml) into dist/: no
# signature, SBOM or upload.
snapshot:
	goreleaser release --snapshot --clean

# install.sh against the snapshot, served like a GitHub release.
install-test: snapshot
	bash smoke/install_test.sh dist

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

# CI's Linux-only compose validation: the render tests, whose compose checks
# run docker compose config --quiet over every golden and fail instead of
# skipping without docker compose.
compose-check:
	PICSURE_REQUIRE_COMPOSE=1 $(GO) test -count=1 ./internal/render

clean:
	rm -rf bin dist
