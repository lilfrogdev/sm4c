# sm4c Makefile.
#
# Design rules:
#   - Every target is deterministic: no `go install` side-effects, no sudo.
#   - Release builds use -trimpath -buildvcs=true so binaries are reproducible.
#   - `make check` is the exact set of gates CI runs locally, and must pass
#     before any PR merges. If CI diverges from `make check`, that is a bug.

SHELL := /usr/bin/env bash

GO      ?= go
BIN_DIR ?= bin
BINARY  ?= sm4c
PKG     := ./...

# Build metadata wired in via -ldflags. COMMIT defaults to the short SHA when
# building from a git checkout; falls back to "unknown" for tarball builds.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/lilfrogdev/sm4c/cmd/sm4c/cli.Version=$(VERSION) \
	-X github.com/lilfrogdev/sm4c/cmd/sm4c/cli.Commit=$(COMMIT) \
	-X github.com/lilfrogdev/sm4c/cmd/sm4c/cli.Date=$(DATE)

BUILD_FLAGS := -trimpath -buildvcs=true -ldflags='$(LDFLAGS)'

.PHONY: all
all: build

.PHONY: build
build: ## Build the sm4c binary into $(BIN_DIR)/$(BINARY)
	@mkdir -p $(BIN_DIR)
	$(GO) build $(BUILD_FLAGS) -o $(BIN_DIR)/$(BINARY) ./cmd/sm4c

.PHONY: install
install: ## go install into $GOBIN (dev convenience; releases do not use this)
	$(GO) install $(BUILD_FLAGS) ./cmd/sm4c

.PHONY: test
test: ## Run unit tests with -race
	$(GO) test -race -count=1 $(PKG)

.PHONY: cover
cover: ## Run unit tests with coverage into coverage.out
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out $(PKG)

.PHONY: vet
vet:
	$(GO) vet $(PKG)

.PHONY: lint
lint: ## Run golangci-lint (installs nothing; must be preinstalled)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found; see https://golangci-lint.run/"; exit 1; }
	golangci-lint run

.PHONY: staticcheck
staticcheck:
	@command -v staticcheck >/dev/null 2>&1 || { echo "staticcheck not found; run: go install honnef.co/go/tools/cmd/staticcheck@latest"; exit 1; }
	staticcheck $(PKG)

.PHONY: vuln
vuln: ## Run govulncheck
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not found; run: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	govulncheck $(PKG)

.PHONY: grep-gates
grep-gates: ## Custom repo grep rules (no network, no hex colors, no shell exec)
	@scripts/check-no-network.sh
	@scripts/check-no-hex-colors.sh
	@scripts/check-no-shell-exec.sh

.PHONY: check
check: vet staticcheck lint vuln grep-gates test ## Run every gate CI runs

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html

.PHONY: help
help: ## Print this help
	@awk 'BEGIN{FS=":.*?## "} /^[a-zA-Z_-]+:.*?## /{printf "  %-15s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
