# 0005. Single-company core, one instance per company in the cloud

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Self-hosted installs serve one company. The cloud must serve many companies cheaply and safely, and ideally run exactly the same software.

## Decision

- The **core serves one company per instance**.
- **Orglane Cloud runs one Orglane instance per company** (the same image as self-hosted) on regional k3s clusters. A regional Postgres cluster holds one database per company.
- Shared cloud services are limited to billing, the push relay, discovery and Link.
- The mobile app's data model supports multiple companies per device (the switching UI comes later).

## Consequences

- One code path for self-hosted and cloud; no "cloud-only" branches in the core.
- Isolation comes from deployment, not from `WHERE tenant_id = ?`, so a query bug can't leak another company's data.
- Per-company backup and restore are trivial, and moving between cloud and self-hosted is a supported export/import.
- Requires fleet rollout tooling in the cloud control plane, which is needed anyway for per-company managed services.
- Under research: shared pools for heavy stateless services (document editors, CI runners).

## Alternatives considered

- **One multi-tenant core serving all companies:** more efficient per company, but adds leak risk to every query, introduces a separate cloud code path and makes per-tenant restore harder.
