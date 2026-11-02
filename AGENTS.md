# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository.

## Project status

Pre-alpha. The monorepo tooling is in place; the core, web app and API are still skeletons (see [ROADMAP.md](ROADMAP.md)).

## Toolchain

| Tool | Version | Pinned in |
|---|---|---|
| Go | 1.27.3 | `go.mod` (`go` + `toolchain`; `GOTOOLCHAIN=auto` downloads it) |
| Node.js | 26.13.0 (LTS) | `.nvmrc`, `package.json` `engines` |
| pnpm | 12.16.1 | `package.json` `packageManager` (via corepack) |
| golangci-lint | v2.15.0 | `Makefile` (`GOLANGCI_LINT_VERSION`), installed into `.bin/` |
| Biome, TypeScript, Vitest | see `package.json` files | `pnpm-lock.yaml` |

Prerequisites: Go, Node.js, corepack (`npm install -g corepack` on Node 25+) and Docker (for integration tests).

## Commands

Run everything from the repository root. Each target covers both Go and TypeScript.

```sh
make bootstrap   # check toolchain, install golangci-lint, download Go modules, pnpm install
make dev         # development setup (placeholder until the core and web skeletons land)
make build       # go build -> bin/orglane; build all workspace packages
make test        # go test -race ./...; vitest in every workspace package
make lint        # go mod tidy check, golangci-lint (lint + format check), Biome, tsc --noEmit
make fmt         # go mod tidy, golangci-lint fmt (gofmt + goimports), biome check --write
make help        # list targets
```

Run `make fmt && make lint && make test` before committing. Language-specific sub-targets exist too (`lint-go`, `test-ts`, ...).

## Read first

1. [docs/brief.md](docs/brief.md): what Orglane is and its product principles
2. [docs/architecture.md](docs/architecture.md): layers, components, domain model, key flows
3. [docs/adr/](docs/adr/): accepted decisions. **Do not contradict an accepted ADR.** If a change requires it, propose a new ADR instead.
4. [ROADMAP.md](ROADMAP.md): current milestone and scope. Don't implement out-of-scope features.

## Ground rules

- **Language:** all code, comments, docs, commits and PR text are in English.
- **Contracts first:** OpenAPI specs, adapter interfaces and event schemas are defined or updated before the implementation.
- **One API:** the web UI, mobile app, modules and agents use the same public API. No private shortcuts.
- **Permissions:** every endpoint declares the permission it requires. Add tests for both allowed and denied access.
- **No new core infrastructure dependencies** (Redis, brokers, search engines, Node.js servers, JVMs) without an ADR. See [ADR 0009](docs/adr/0009-resource-philosophy.md).
- **Tests use real services** (Postgres, Forgejo, ...) via containers for integration tests. Don't mock provider APIs in integration tests.
- **Never weaken tests to make CI pass.** Removing assertions, skipping tests or changing the golden scenario is a high-risk change that needs a maintainer.
- **i18n:** user-facing strings go through the i18n catalogs, never hard-coded.
- **Security:** never commit secrets. Treat webhook payloads, imported files and module input as untrusted.
- **Commits:** Conventional Commits; one logical change per PR; PRs are squash-merged.
- **Keep it boring:** prefer the standard library and well-known, maintained dependencies. Check that dependency licenses are compatible with AGPL-3.0 (server) or Apache-2.0 (apps, SDKs).

## Risk tiers

High-risk paths (identity, permissions, crypto, DB migrations, the agent, public API contracts, CI config, licenses) always require a maintainer review. See [ADR 0017](docs/adr/0017-development-process.md).

## Repository layout

```
orglane/
├── go.mod                # the single Go module: github.com/orglane/orglane
├── core/                 # Go: cmd/orglane (server | agent | cli), internal/...
├── providers/            # provider adapters (Go, compiled into core) + managed packages
├── package.json          # pnpm workspace root (private, AGPL-3.0-or-later)
├── pnpm-workspace.yaml   # workspace members: web/, packages/*
├── web/                  # React + Vite, embedded into the core binary (AGPL-3.0-or-later)
├── packages/             # shared TypeScript packages (Apache-2.0, see packages/LICENSE)
│   └── ui-tokens/        # design tokens
├── deploy/               # Compose files, install script
├── demo/                 # "Acme" demo dataset
├── tests/                # integration and end-to-end tests
└── docs/                 # brief, architecture, ADRs, specs
```

Conventions:

- Go: one module at the root; unit tests next to the code (`*_test.go`). Lint config in `.golangci.yml`.
- TypeScript: strict mode via `tsconfig.base.json`; every package extends it and defines `build`, `test` and `typecheck` scripts. Lint and format config in `biome.json`. Unit tests next to the code (`*.test.ts`, Vitest).
- New dependencies: pnpm only installs versions published at least a day ago (`minimumReleaseAge` in `pnpm-workspace.yaml`).

The target layout (including `mobile/` and `services/push-relay/`) is described in the "Repository layout" section of [docs/architecture.md](docs/architecture.md).
