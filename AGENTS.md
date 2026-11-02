# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository.

## Project status

Pre-alpha. The repository currently contains **documentation only**. Build, test and run commands will be added here in milestone M0 (see [ROADMAP.md](ROADMAP.md)).

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

## Repository layout (planned)

See the "Repository layout" section in [docs/architecture.md](docs/architecture.md).
