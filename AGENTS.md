# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository.

## Project status

Pre-alpha. The monorepo tooling and the core skeleton (server/agent/cli modes, PostgreSQL, migrations, job queue, contract-first API) are in place; identity, the data model and the web app are next (see [ROADMAP.md](ROADMAP.md)).

## Toolchain

| Tool | Version | Pinned in |
|---|---|---|
| Go | 1.27.3 | `go.mod` (`go` + `toolchain`; `GOTOOLCHAIN=auto` downloads it) |
| Node.js | 26.13.0 (LTS) | `.nvmrc`, `package.json` `engines` |
| pnpm | 12.16.1 | `package.json` `packageManager` (via corepack) |
| golangci-lint | v2.15.0 | `Makefile` (`GOLANGCI_LINT_VERSION`), installed into `.bin/` |
| oapi-codegen | v2.8.2 | `Makefile` (`OAPI_CODEGEN_VERSION`), installed into `.bin/` |
| PostgreSQL | 18 | `deploy/dev/compose.yaml`, `tests/pgtest` (integration tests) |
| Biome, TypeScript, Vitest | see `package.json` files | `pnpm-lock.yaml` |

Prerequisites: Go, Node.js, corepack (`npm install -g corepack` on Node 25+) and Docker (for integration tests and `make dev`).

## Commands

Run everything from the repository root. Each target covers both Go and TypeScript.

```sh
make bootstrap         # check toolchain, install golangci-lint and oapi-codegen, download Go modules, pnpm install
make dev               # start a local Postgres (Docker) and run `orglane server` on http://localhost:8080
make dev-down          # stop the local Postgres
make generate          # regenerate Go server code from core/api/openapi.yaml
make build             # go build -> bin/orglane; build all workspace packages
make test              # Go unit + integration tests (integration only when Docker is available); vitest
make test-unit         # Go unit tests only
make test-integration  # Go unit + integration tests (needs Docker)
make measure-memory    # idle RSS of the server against Postgres (warning only, needs Docker)
make lint              # stale generated code check, go mod tidy check, golangci-lint, Biome, tsc --noEmit
make fmt               # go mod tidy, golangci-lint fmt (gofmt + goimports), biome check --write
make help              # list targets
```

Run `make fmt && make lint && make test` before committing. Language-specific sub-targets exist too (`lint-go`, `test-ts`, ...).

### Integration tests

Integration tests carry the `integration` build tag (`//go:build integration`) and run against a real PostgreSQL started with testcontainers-go (`tests/pgtest`). Use `pgtest.Main` in `TestMain` and `pgtest.NewDatabase(t)` for a fresh database per test. Run one package with `go test -tags integration ./core/internal/database/`. The end-to-end tests of the `orglane` binary live in `tests/integration`.

### Running the server locally

`make dev` starts Postgres from `deploy/dev/compose.yaml` and runs `orglane server -config deploy/dev/orglane.yaml`. Other modes: `go run ./core/cmd/orglane cli migrate -config deploy/dev/orglane.yaml`, `... cli version`, `... agent` (stub).

Configuration is a YAML file (`-config` or `ORGLANE_CONFIG`) plus `ORGLANE_*` environment variables, which override the file. The variable name is the YAML path in upper case joined with `_` (`domain.workspace_host` → `ORGLANE_DOMAIN_WORKSPACE_HOST`). Required: `database.url` and `domain.base`. See `core/internal/config/config.go` for every setting and its default. Hosts are never hard-coded outside that package (ADR 0018).

### API changes

The API is contract-first. Change `core/api/openapi.yaml` first, then run `make generate` and implement the generated `StrictServerInterface` in `core/internal/api`. Every operation must declare `x-orglane-permission` (a permission from `core/internal/authz`, or `public`); the server refuses to start and the tests fail otherwise. Add tests for allowed and denied access. Product endpoints live under `/api/v1`; errors are RFC 9457 problem details. Changes to `core/api/**` and `core/migrations/**` are high risk.

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
