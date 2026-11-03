# Orglane task runner. Every target works from the repository root and covers
# both the Go module (core/, providers/) and the pnpm workspace (web/, packages/*).
#
# Toolchain pins (single source of truth):
#   Go             go.mod (`go` and `toolchain` lines)
#   Node           .nvmrc and package.json `engines`
#   pnpm           package.json `packageManager` (via corepack)
#   golangci-lint  GOLANGCI_LINT_VERSION below
#   oapi-codegen   OAPI_CODEGEN_VERSION below

GOLANGCI_LINT_VERSION := v2.15.0
OAPI_CODEGEN_VERSION  := v2.8.2

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

ROOT          := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
TOOLS_BIN     := $(ROOT)/.bin
GOLANGCI_LINT := $(TOOLS_BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
OAPI_CODEGEN  := $(TOOLS_BIN)/oapi-codegen-$(OAPI_CODEGEN_VERSION)
PNPM          ?= pnpm

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO_LDFLAGS := -s -w -X main.version=$(VERSION)

# OpenAPI contract and the code generated from it: the Go server interface
# (oapi-codegen) and the TypeScript client types of packages/api-client
# (openapi-typescript, run by packages/api-client/codegen/generate.mjs).
API_SPEC        := core/api/openapi.yaml
API_CODEGEN_CFG := core/api/oapi-codegen.yaml
API_GENERATED   := core/internal/api/apigen/apigen.gen.go
API_CLIENT      := @orglane/api-client

# Local development database (deploy/dev/compose.yaml).
DEV_COMPOSE := docker compose -f deploy/dev/compose.yaml

.PHONY: help bootstrap check-toolchain dev dev-down build build-go build-ts generate check-generate check-generate-go check-generate-ts \
	test test-go test-unit test-integration measure-memory test-ts \
	lint lint-go lint-ts fmt fmt-go fmt-ts clean

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-17s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------------------
## Setup

bootstrap: check-toolchain $(GOLANGCI_LINT) $(OAPI_CODEGEN) ## Install all dependencies and pinned tools
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

$(OAPI_CODEGEN):
	@echo "installing oapi-codegen $(OAPI_CODEGEN_VERSION) into $(TOOLS_BIN)"
	GOBIN=$(TOOLS_BIN) go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)
	mv $(TOOLS_BIN)/oapi-codegen $@

## ---------------------------------------------------------------------------
## Development

dev: ## Start a local Postgres and run the server on http://localhost:8080
	$(DEV_COMPOSE) up -d --wait
	go run ./core/cmd/orglane server -config deploy/dev/orglane.yaml

dev-down: ## Stop the local Postgres (its data stays in a Docker volume)
	$(DEV_COMPOSE) down

generate: $(OAPI_CODEGEN) ## Regenerate code from the OpenAPI contract (Go server, TypeScript client)
	$(OAPI_CODEGEN) -config $(API_CODEGEN_CFG) $(API_SPEC) > $(API_GENERATED)
	$(PNPM) --silent --filter $(API_CLIENT) run generate

# Fails when generated code no longer matches the contract (part of `make lint`).
check-generate: check-generate-go check-generate-ts

check-generate-go: $(OAPI_CODEGEN)
	@$(OAPI_CODEGEN) -config $(API_CODEGEN_CFG) $(API_SPEC) | diff -u $(API_GENERATED) - >/dev/null || \
		{ echo "error: $(API_GENERATED) is stale, run 'make generate'"; exit 1; }

check-generate-ts:
	@$(PNPM) --silent --filter $(API_CLIENT) run check-generate

build: build-go build-ts ## Build everything

build-go:
	go build -trimpath -ldflags '$(GO_LDFLAGS)' -o bin/orglane ./core/cmd/orglane

build-ts:
	$(PNPM) run build

test: test-go test-ts ## Run all tests

# Integration tests (build tag "integration") need Docker; without it only
# unit tests run.
test-go:
	@if docker info >/dev/null 2>&1; then \
		$(MAKE) --no-print-directory test-integration; \
	else \
		echo "warning: Docker is not available, skipping integration tests"; \
		$(MAKE) --no-print-directory test-unit; \
	fi

test-unit: ## Run Go unit tests only
	go test -race -count=1 ./...

test-integration: ## Run Go unit and integration tests (needs Docker)
	go test -race -count=1 -tags integration ./...

measure-memory: ## Report idle RSS of the server against Postgres (needs Docker; warning only)
	go test -count=1 -tags integration -run '^TestIdleMemory$$' -v ./tests/integration/ | grep -E '^(orglane-memory:|--- FAIL|FAIL)'

test-ts:
	$(PNPM) run test

lint: lint-go lint-ts ## Run all linters and type checks

lint-go: $(GOLANGCI_LINT) check-generate-go
	go mod tidy -diff
	$(GOLANGCI_LINT) run --build-tags integration ./...
	$(GOLANGCI_LINT) fmt --diff ./...

lint-ts: check-generate-ts
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
