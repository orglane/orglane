# 0009. Resource philosophy: no waste, scales up

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Orglane must run on home servers and small VPSes without becoming slow. A hard memory ceiling would trade performance for a number.

## Decision

- **Light by default, scales up with the host.** The installer detects resources and tunes Postgres and caches accordingly.
- **No unjustified background work.** The agent is event-driven, and periodic tasks are infrequent.
- **The core excludes** Redis, message brokers, search engines, Node.js servers and JVMs. It uses a Postgres-backed job queue inside the process and Postgres full-text search.
- **CI tracks both idle memory and speed** (API latency, screen load) as regression warnings, not hard limits.
- One shared Postgres, with one database per managed service where supported.

## Consequences

- Small installs stay small, and big hosts get fast.
- New core dependencies need a strong justification in an ADR.
