# 0004. Permission-based authorization, roles as bundles

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Companies need different duties separated (for example, HR shouldn't manage infrastructure and admins needn't see personal data), but small teams need simplicity.

## Decision

- Code checks **granular permissions** such as `people.manage`, `people.sensitive.read`, `apps.manage`, `security.manage` and `billing.manage`, never role names.
- **Roles are bundles of permissions.** v0.1 ships **Owner / Admin / Member**.
- New presets (People manager / HR) and manager-scoped rights (managers seeing their reports) are added later as configuration, without code changes.
- Module and agent scopes (ADR 0013) use the same permission vocabulary.

## Consequences

- Simple for small companies, extensible for larger ones.
- Every endpoint must declare the permission it requires, and tests must cover denial.
