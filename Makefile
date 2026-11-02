# Orglane task runner. Every target works from the repository root and covers
# both the Go module (core/, providers/) and the pnpm workspace (web/, packages/*).
#
# Toolchain pins (single source of truth):
#   Go             go.mod (`go` and `toolchain` lines)
#   Node           .nvmrc and package.json `engines`
#   pnpm           package.json `packageManager` (via corepack)
#   golangci-lint  GOLANGCI_LINT_VERSION below

GOLANGCI_LINT_VERSION := v2.15.0

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

ROOT          := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
TOOLS_BIN     := $(ROOT)/.bin
GOLANGCI_LINT := $(TOOLS_BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
PNPM          ?= pnpm

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help bootstrap check-toolchain dev build build-go build-ts test test-go test-ts \
	lint lint-go lint-ts fmt fmt-go fmt-ts clean

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------------------
## Setup

bootstrap: check-toolchain $(GOLANGCI_LINT) ## Install all dependencies and pinned tools
	go mod download
	$(PNPM) install --frozen-lockfile

check-toolchain:
	@command -v go >/dev/null || { echo "error: Go is not installed (see go.mod for the version)"; exit 1; }
	@command -v node >/dev/null || { echo "error: Node.js is not installed (see .nvmrc for the version)"; exit 1; }
	@want="$$(cat .nvmrc)"; have="$$(node --version | sed 's/^v//')"; \
		if [ "$${have%%.*}" != "$${want%%.*}" ]; then \
			echo "error: Node.js $$want is required (found $$have, see .nvmrc)"; exit 1; \
		elif [ "$$have" != "$$want" ]; then \
			echo "warning: Node.js $$want is pinned in .nvmrc (found $$have)"; \
		fi
	@if ! command -v $(PNPM) >/dev/null; then \
		if command -v corepack >/dev/null; then \
			echo "pnpm not found, enabling it with corepack"; corepack enable pnpm; \
		else \
			echo "error: pnpm is not installed. Install corepack ('npm install -g corepack') and run 'corepack enable pnpm'"; exit 1; \
		fi; \
	fi

# The binary name carries the version, so bumping GOLANGCI_LINT_VERSION
# installs the new release on the next run.
$(GOLANGCI_LINT):
	@echo "installing golangci-lint $(GOLANGCI_LINT_VERSION) into $(TOOLS_BIN)"
	GOBIN=$(TOOLS_BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(TOOLS_BIN)/golangci-lint $@

## ---------------------------------------------------------------------------
## Development

dev: ## Run the development setup (placeholder until the core and web skeletons land)
	go run ./core/cmd/orglane
	$(PNPM) run dev

build: build-go build-ts ## Build everything

build-go:
	go build -trimpath -ldflags '$(GO_LDFLAGS)' -o bin/orglane ./core/cmd/orglane

build-ts:
	$(PNPM) run build

test: test-go test-ts ## Run all tests

test-go:
	go test -race -count=1 ./...

test-ts:
	$(PNPM) run test

lint: lint-go lint-ts ## Run all linters and type checks

lint-go: $(GOLANGCI_LINT)
	go mod tidy -diff
	$(GOLANGCI_LINT) run ./...
	$(GOLANGCI_LINT) fmt --diff ./...

lint-ts:
	$(PNPM) run lint

fmt: fmt-go fmt-ts ## Format all code

fmt-go: $(GOLANGCI_LINT)
	go mod tidy
	$(GOLANGCI_LINT) fmt ./...

fmt-ts:
	$(PNPM) run fmt

clean: ## Remove build output and installed tools
	rm -rf bin .bin
	find packages web -name dist -type d -prune -exec rm -rf {} + 2>/dev/null || true
