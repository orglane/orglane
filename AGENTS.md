# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository.

## Project status

Pre-alpha. The monorepo tooling, the core skeleton (server/agent/cli modes, PostgreSQL, migrations, job queue, contract-first API), the M0 data model (people, roles, devices, sessions, audit log), identity (embedded OpenID Provider, password and passkey sign-in, sessions) and the web skeleton (React SPA embedded into the binary, design tokens, i18n) are in place (see [ROADMAP.md](ROADMAP.md)).

## Toolchain

| Tool | Version | Pinned in |
|---|---|---|
| Go | 1.27.3 | `go.mod` (`go` + `toolchain`; `GOTOOLCHAIN=auto` downloads it) |
| Node.js | 26.13.0 (LTS) | `.nvmrc`, `package.json` `engines` |
| pnpm | 12.16.1 | `package.json` `packageManager` (via corepack) |
| golangci-lint | v2.15.0 | `Makefile` (`GOLANGCI_LINT_VERSION`), installed into `.bin/` |
| oapi-codegen | v2.8.2 | `Makefile` (`OAPI_CODEGEN_VERSION`), installed into `.bin/` |
| sqlc | v1.31.1 | `Makefile` (`SQLC_VERSION`), installed into `.bin/` (needs cgo) |
| govulncheck, go-licenses | see `Makefile` | `Makefile` (`GOVULNCHECK_VERSION`, `GO_LICENSES_VERSION`), installed into `.bin/` |
| PostgreSQL | 18 | `deploy/dev/compose.yaml`, `tests/pgtest` (integration tests) |
| Biome, TypeScript, Vitest | see `package.json` files | `pnpm-lock.yaml` |

Prerequisites: Go, Node.js, corepack (`npm install -g corepack` on Node 25+) and Docker (for integration tests and `make dev`).

## Commands

Run everything from the repository root. Each target covers both Go and TypeScript.

```sh
make bootstrap         # check toolchain, install golangci-lint, oapi-codegen and sqlc, download Go modules, pnpm install
make dev               # start a local Postgres (Docker) and run `orglane server` on http://localhost:8080
make dev-down          # stop the local Postgres
make demo              # demo mode: local Postgres + `orglane server --demo` with the "Acme Robotics" dataset (own database)
make demo-reset        # drop the demo database
make image             # build the core container image (orglane:dev, with the web UI)
make generate          # regenerate the Go server code and the TypeScript client from core/api/openapi.yaml, the sqlc query layer, tokens.css and the English i18n catalog
make build             # build all workspace packages (incl. the web UI), then go build -> bin/orglane with the UI embedded
make test              # Go unit + integration tests (integration only when Docker is available); vitest; web e2e (when Docker and Chromium are available)
make test-unit         # Go unit tests only
make test-integration  # Go unit + integration tests, including the Compose bundle tests (needs Docker)
make test-compose      # only the Compose bundle tests: local mode and domain mode against Pebble (needs Docker)
make test-e2e          # Playwright end-to-end tests of the web UI against a real server (needs Docker and Playwright's Chromium)
make measure-memory    # idle RSS of the server against Postgres (warning only, needs Docker)
make conformance       # OpenID Conformance Suite against a local server (Docker)
make lint              # stale generated code check (Go and TypeScript), go mod tidy check, golangci-lint, Biome, tsc --noEmit
make fmt               # go mod tidy, golangci-lint fmt (gofmt + goimports), biome check --write
make security          # govulncheck; dependency license check (ADR 0016)
make help              # list targets
```

Run `make fmt && make lint && make test` before committing. Language-specific sub-targets exist too (`lint-go`, `test-ts`, ...).

CI (`.github/workflows/ci.yml`) runs the same targets: `make lint`, `make test`, `make build` and `make security`. The trusted Policy workflow (`.github/workflows/policy.yml`, default branch only) then labels the pull request with its `risk/*` tier and sets a `merge-gate` status that says whether it may be merged without a maintainer (ADR 0017). An AI approval must name the head commit SHA (`Codex review: APPROVE for <sha>`). Before merging, verify with `go run ./tools/ci policy -pr <number>` from an up-to-date `main`. See [tools/ci/README.md](tools/ci/README.md). The OpenID Conformance Suite runs in its own workflow (`.github/workflows/conformance.yml`): by hand with `gh workflow run conformance.yml --ref <branch>` and weekly on main (see [tests/conformance/README.md](tests/conformance/README.md)).

Releases are prepared by release-please from the Conventional Commits on `main` (`.github/workflows/release.yml`): it keeps a release pull request with the version bump and the `CHANGELOG.md` entry. Merging it is a maintainer's decision; the tag it creates publishes multi-arch images to `ghcr.io/orglane/orglane` (`.github/workflows/images.yml`). See [docs/releasing.md](docs/releasing.md).

### Integration tests

Integration tests carry the `integration` build tag (`//go:build integration`) and run against a real PostgreSQL started with testcontainers-go (`tests/pgtest`). Use `pgtest.Main` in `TestMain` and `pgtest.NewDatabase(t)` for a fresh database per test. Run one package with `go test -tags integration ./core/internal/database/`. The end-to-end tests of the `orglane` binary live in `tests/integration`. `tests/compose` builds the core image and starts the real Compose bundle (`deploy/compose`): local mode through Traefik, and domain mode with certificates from Pebble, Let's Encrypt's ACME test server.

### Running the server locally

`make dev` starts Postgres from `deploy/dev/compose.yaml` and runs `orglane server -config deploy/dev/orglane.yaml`. Other modes: `go run ./core/cmd/orglane cli migrate -config deploy/dev/orglane.yaml`, `... cli version`, `... agent` (stub). `make demo` runs demo mode (`orglane server --demo`): on an empty database it loads the "Acme Robotics" dataset from `demo/` and allows one-click sign-in as any demo person; see [demo/README.md](demo/README.md). Tests and scripts that need realistic data use that dataset (`tests/demotest`).

Configuration is a YAML file (`-config` or `ORGLANE_CONFIG`) plus `ORGLANE_*` environment variables, which override the file. The variable name is the YAML path in upper case joined with `_` (`domain.workspace_host` → `ORGLANE_DOMAIN_WORKSPACE_HOST`). Required: `database.url`, `domain.base` and, in production, `identity.master_secret` (at least 32 characters; never change it on a running instance). On first start the server logs a one-time setup token for creating the owner (`go run ./core/cmd/orglane cli setup-token -config deploy/dev/orglane.yaml` makes a new one). See `core/internal/config/config.go` for every setting and its default. Hosts are never hard-coded outside that package (ADR 0018).

### Compose bundle and Traefik

The production bundle is `deploy/compose/compose.yaml` (Traefik, the core image from the root `Dockerfile`, PostgreSQL); usage is in [deploy/README.md](deploy/README.md). Traefik has no Docker socket: it polls its routes from the core's internal listener (`server.internal_listen`, `GET /traefik/config`), which is never published or routed (ADR 0008). The routes are generated in `core/internal/ingress` from the domain settings and `ingress.mode` (`domain`: HTTPS via Let's Encrypt, HSTS, HTTP→HTTPS redirect, production; `local`: plain HTTP for `--demo`, development). In the bundle, `ORGLANE_INGRESS_MODE` selects `deploy/compose/modes/<mode>.env`. Traefik has a fixed address and is the only entry in `server.trusted_proxies`. Pin every image in the bundle and the `Dockerfile` by version and digest. Changes to routing or the bundle need `make test-compose` to pass.

### API changes

The API is contract-first. Change `core/api/openapi.yaml` first, then run `make generate` (it regenerates the Go server interface and the TypeScript client in `packages/api-client`) and implement the generated `StrictServerInterface` in `core/internal/api`. Commit the regenerated files: `make lint` fails when either is stale. Every operation must declare `x-orglane-permission` (a permission from `core/internal/authz`, or `public`); the server refuses to start and the tests fail otherwise. Add tests for allowed and denied access. Product endpoints live under `/api/v1`; errors are RFC 9457 problem details. Changes to `core/api/**` and `core/migrations/**` are high risk.

Permission names live in one place: the registry in `core/internal/authz` (`authz.go`), together with the built-in Owner / Admin / Member bundles (`roles.go`). The built-in roles are seeded into the database on every start (and by `cli migrate`).

### Database changes

Add a new goose migration in `core/migrations` (never edit an applied one) with a working `-- +goose Down`; `core/internal/database` tests apply and roll back every migration. IDs are UUIDv7 (`DEFAULT uuidv7()`, PostgreSQL 18), timestamps are `timestamptz` and read in UTC. Queries are SQL files in `core/internal/store/queries`; run `make generate` to regenerate the typed sqlc code in `core/internal/store` (`make lint` fails when it is stale). People, roles, devices, sessions and sign-in state are changed only through `core/internal/identity` (OIDC protocol state through `core/internal/identity/oidcop`), which writes an audit entry (`core/internal/audit`) in the same transaction; `audit_entries` is append-only at the database level.

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
- **i18n:** user-facing strings go through the i18n catalogs, never hard-coded (`make lint` checks JSX; see [web/README.md](web/README.md)).
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
│   ├── api-client/       # typed API client and React Query hooks, generated from the OpenAPI contract
│   └── ui-tokens/        # design tokens
├── Dockerfile            # core image: web UI + Go binary on distroless, non-root
├── deploy/               # Compose bundle (compose/), dev database (dev/), install script
├── demo/                 # "Acme" demo dataset
├── tests/                # integration and end-to-end tests
├── tools/ci/             # CI tooling: risk labeler, merge gate, license check (Go)
└── docs/                 # brief, architecture, ADRs, specs
```

Conventions:

- Go: one module at the root; unit tests next to the code (`*_test.go`). Lint config in `.golangci.yml`.
- TypeScript: strict mode via `tsconfig.base.json`; every package extends it and defines `build`, `test` and `typecheck` scripts. Lint and format config in `biome.json`. Unit tests next to the code (`*.test.ts`, Vitest).
- New dependencies: pnpm only installs versions published at least a day ago (`minimumReleaseAge` in `pnpm-workspace.yaml`).

The target layout (including `mobile/` and `services/push-relay/`) is described in the "Repository layout" section of [docs/architecture.md](docs/architecture.md).
