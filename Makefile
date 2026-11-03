# Orglane task runner. Every target works from the repository root and covers
# both the Go module (core/, providers/) and the pnpm workspace (web/, packages/*).
#
# Toolchain pins (single source of truth):
#   Go             go.mod (`go` and `toolchain` lines)
#   Node           .nvmrc and package.json `engines`
#   pnpm           package.json `packageManager` (via corepack)
#   golangci-lint  GOLANGCI_LINT_VERSION below
#   oapi-codegen   OAPI_CODEGEN_VERSION below
#   sqlc           SQLC_VERSION below
#   govulncheck    GOVULNCHECK_VERSION below
#   go-licenses    GO_LICENSES_VERSION below

GOLANGCI_LINT_VERSION := v2.15.0
OAPI_CODEGEN_VERSION  := v2.8.2
SQLC_VERSION          := v1.31.1
GOVULNCHECK_VERSION   := v1.9.0
GO_LICENSES_VERSION   := v2.0.1

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

ROOT          := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
TOOLS_BIN     := $(ROOT)/.bin
GOLANGCI_LINT := $(TOOLS_BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
OAPI_CODEGEN  := $(TOOLS_BIN)/oapi-codegen-$(OAPI_CODEGEN_VERSION)
SQLC          := $(TOOLS_BIN)/sqlc-$(SQLC_VERSION)
GOVULNCHECK   := $(TOOLS_BIN)/govulncheck-$(GOVULNCHECK_VERSION)
GO_LICENSES   := $(TOOLS_BIN)/go-licenses-$(GO_LICENSES_VERSION)
CI_TOOL       := go run ./tools/ci
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
UI_TOKENS       := @orglane/ui-tokens
WEB             := @orglane/web

# sqlc query layer: core/internal/store/queries/*.sql against core/migrations.
SQLC_CFG := core/internal/store/sqlc.yaml

# Local development database (deploy/dev/compose.yaml).
DEV_COMPOSE := docker compose -f deploy/dev/compose.yaml
# Demo mode uses its own database next to the development one.
DEMO_DB     := orglane_demo
DEMO_DB_URL := postgres://orglane:orglane@127.0.0.1:5432/$(DEMO_DB)?sslmode=disable

.PHONY: help bootstrap check-toolchain deps dev dev-down demo demo-reset build build-go build-ts generate check-generate check-generate-go check-generate-ts \
	test test-go test-unit test-integration measure-memory conformance test-ts test-e2e test-e2e-if-available \
	lint lint-go lint-ts fmt fmt-go fmt-ts security vulncheck licenses licenses-go licenses-ts clean

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*## "}; {printf "  \033[36m%-17s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------------------
## Setup

bootstrap: check-toolchain deps $(GOLANGCI_LINT) $(OAPI_CODEGEN) $(SQLC) ## Install all dependencies and pinned tools

deps: ## Download Go modules and install pnpm packages (no tools)
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

# sqlc embeds the PostgreSQL parser (cgo); the first build takes a minute.
$(SQLC):
	@echo "installing sqlc $(SQLC_VERSION) into $(TOOLS_BIN)"
	GOBIN=$(TOOLS_BIN) go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	mv $(TOOLS_BIN)/sqlc $@

$(GOVULNCHECK):
	@echo "installing govulncheck $(GOVULNCHECK_VERSION) into $(TOOLS_BIN)"
	GOBIN=$(TOOLS_BIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	mv $(TOOLS_BIN)/govulncheck $@

$(GO_LICENSES):
	@echo "installing go-licenses $(GO_LICENSES_VERSION) into $(TOOLS_BIN)"
	GOBIN=$(TOOLS_BIN) go install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)
	mv $(TOOLS_BIN)/go-licenses $@

## ---------------------------------------------------------------------------
## Development

dev: ## Start a local Postgres and run the server on http://localhost:8080
	$(DEV_COMPOSE) up -d --wait
	go run ./core/cmd/orglane server -config deploy/dev/orglane.yaml

dev-down: ## Stop the local Postgres (its data stays in a Docker volume)
	$(DEV_COMPOSE) down

demo: ## Run the server in demo mode with the Acme dataset on http://localhost:8080 (own database)
	$(DEV_COMPOSE) up -d --wait
	@$(DEV_COMPOSE) exec -T postgres psql -U orglane -d orglane -tAc "SELECT 1 FROM pg_database WHERE datname = '$(DEMO_DB)'" | grep -q 1 || \
		$(DEV_COMPOSE) exec -T postgres createdb -U orglane $(DEMO_DB)
	ORGLANE_DATABASE_URL='$(DEMO_DB_URL)' go run ./core/cmd/orglane server -config deploy/dev/orglane.yaml --demo

demo-reset: ## Drop the demo database, so the next `make demo` loads the dataset afresh
	$(DEV_COMPOSE) up -d --wait
	$(DEV_COMPOSE) exec -T postgres dropdb -U orglane --if-exists --force $(DEMO_DB)

generate: $(OAPI_CODEGEN) $(SQLC) ## Regenerate code from the OpenAPI contract (Go server, TypeScript client) and the SQL queries (sqlc)
	$(OAPI_CODEGEN) -config $(API_CODEGEN_CFG) $(API_SPEC) > $(API_GENERATED)
	$(SQLC) generate -f $(SQLC_CFG)
	$(PNPM) --silent --filter $(API_CLIENT) run generate
	$(PNPM) --silent --filter $(UI_TOKENS) run generate
	$(PNPM) --silent --filter $(WEB) run i18n:extract

# Fails when generated code no longer matches the contract or the SQL queries
# (part of `make lint`).
check-generate: check-generate-go check-generate-ts

check-generate-go: $(OAPI_CODEGEN) $(SQLC)
	@$(OAPI_CODEGEN) -config $(API_CODEGEN_CFG) $(API_SPEC) | diff -u $(API_GENERATED) - >/dev/null || \
		{ echo "error: $(API_GENERATED) is stale, run 'make generate'"; exit 1; }
	@$(SQLC) diff -f $(SQLC_CFG) || \
		{ echo "error: the sqlc query layer in core/internal/store is stale, run 'make generate'"; exit 1; }

check-generate-ts:
	@$(PNPM) --silent --filter $(API_CLIENT) run check-generate
	@$(PNPM) --silent --filter $(UI_TOKENS) run check-generate
	@$(PNPM) --silent --filter $(WEB) run i18n:check

# The web UI (web/dist) is embedded into the binary, so it is built first.
build: ## Build everything (the binary embeds the web UI)
	$(MAKE) --no-print-directory build-ts
	$(MAKE) --no-print-directory build-go

build-go:
	go build -trimpath -ldflags '$(GO_LDFLAGS)' -o bin/orglane ./core/cmd/orglane

build-ts:
	$(PNPM) run build

test: test-go test-ts test-e2e-if-available ## Run all tests (integration and e2e when Docker and Chromium are available)

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

conformance: ## Run the OpenID Conformance Suite against a local server (needs Docker)
	tests/conformance/run.sh

test-ts:
	$(PNPM) run test

# Playwright end-to-end tests of the web UI against a real server and
# PostgreSQL (web/e2e). Needs Docker and Playwright's Chromium
# (`pnpm --filter @orglane/web exec playwright install chromium`).
test-e2e: ## Run the web end-to-end tests (needs Docker and Playwright's Chromium)
	$(PNPM) --silent --filter $(WEB) run build
	$(PNPM) --silent --filter $(WEB) run test:e2e

test-e2e-if-available:
	@if ! docker info >/dev/null 2>&1; then \
		echo "warning: Docker is not available, skipping the web end-to-end tests"; \
	elif ! $(PNPM) --silent --filter $(WEB) exec node e2e/chromium-installed.mjs; then \
		echo "warning: Playwright's Chromium is not installed, skipping the web end-to-end tests (pnpm --filter $(WEB) exec playwright install chromium)"; \
	else \
		$(MAKE) --no-print-directory test-e2e; \
	fi

lint: lint-go lint-ts ## Run all linters and type checks

lint-go: $(GOLANGCI_LINT) check-generate-go
	go mod tidy -diff
	$(GOLANGCI_LINT) run --build-tags integration ./...
	$(GOLANGCI_LINT) fmt --diff ./...

lint-ts: check-generate-ts
	$(PNPM) run lint

security: vulncheck licenses ## Scan for known vulnerabilities and check dependency licenses

vulncheck: $(GOVULNCHECK)
	$(GOVULNCHECK) -tags integration ./...

# Dependency licenses must be compatible with ADR 0016: the "server" policy for
# everything (AGPL-3.0-or-later server, web UI and tooling) and the stricter
# "apache" policy for production dependencies of the Apache-2.0 packages.
# Policies live in tools/ci/internal/licenses.
licenses: licenses-go licenses-ts

# GOROOT is passed explicitly: go-licenses recognizes the standard library by
# its GOROOT, which must match the toolchain selected by go.mod.
licenses-go: $(GO_LICENSES)
	GOROOT="$$(go env GOROOT)" GOFLAGS=-tags=integration $(GO_LICENSES) report ./... --ignore github.com/orglane/orglane | $(CI_TOOL) licenses -policy server -format go-licenses

licenses-ts:
	$(PNPM) -r licenses list --json | $(CI_TOOL) licenses -policy server -format pnpm
	$(PNPM) --filter './packages/*' licenses list --prod --json | $(CI_TOOL) licenses -policy apache -format pnpm

fmt: fmt-go fmt-ts ## Format all code

fmt-go: $(GOLANGCI_LINT)
	go mod tidy
	$(GOLANGCI_LINT) fmt ./...

fmt-ts:
	$(PNPM) run fmt

clean: ## Remove build output and installed tools
	rm -rf bin .bin
	find packages -name dist -type d -prune -exec rm -rf {} + 2>/dev/null || true
	find web/dist -mindepth 1 ! -name .gitkeep -exec rm -rf {} + 2>/dev/null || true
	rm -rf web/test-results web/playwright-report
