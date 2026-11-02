# 0017. Agent-driven development process, risk tiers, auto-merge

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Most code is written by AI agents working in parallel around the clock, orchestrated outside the repository. Human attention is the scarce resource and must go where risk is highest. External contributors must get fast, predictable feedback.

## Decision

**Work tracking**
- **GitHub Issues and Projects** are the single source of truth; the public roadmap is a Projects board.
- Every feature is an issue with **acceptance criteria**. Larger features get a spec in `docs/specs/`, and architectural decisions get an ADR.
- Labels: `area/*`, `type/*`, `risk/*`, `good first issue`, `help wanted`.

**Writing and review**
- Agents work on branches and open PRs. **Different models write and review** (for example, Claude writes and Codex reviews). CodeRabbit reviews PRs from external contributors.
- **Squash merge only.** Conventional Commits; release notes are automated.
- Contracts first: OpenAPI, adapter interfaces, event schemas.

**Risk tiers (derived from changed paths)**

| Tier | Examples | Merge |
|---|---|---|
| `risk/low` | docs, added tests, translations, patch dependency bumps | auto-merge after green CI |
| `risk/medium` | feature code inside a module | auto-merge after green CI + AI reviewer approval |
| `risk/high` | identity/auth, permissions, cryptography, **DB schema migrations**, the agent, public API contracts, **CI configuration**, licenses, **removing or weakening tests or the golden scenario** | requires a maintainer (CODEOWNERS) |

Auto-merge requires all checks green, AI reviewer approval, no unresolved threads, and a tier other than high. **PRs from external contributors are never auto-merged.** Workflows must never expose secrets to PRs from forks. **Releases (tags) are a human decision.**

**CI gates**
- Lint, types, unit tests.
- Integration tests against **real services** in containers (Postgres, Forgejo, ...).
- End-to-end tests (Playwright for web, Maestro for mobile).
- The **golden scenario**: create company → invite → access granted → event → push → open without re-login → offboard → access revoked with report → restore from backup.
- OpenID Conformance Suite.
- Idle memory and latency tracking (warnings).
- Vulnerability and **dependency license** scanning.

## Consequences

- Humans focus on architecture and high-risk changes; routine work flows automatically.
- Quality is enforced by tests against real systems rather than by review volume.
- Agents can't "fix" CI by weakening it without a human seeing it.
