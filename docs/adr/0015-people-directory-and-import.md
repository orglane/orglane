# 0015. People directory, profile fields, import pipeline

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

The directory is the backbone of the internal ecosystem. Companies store people data in many places (spreadsheets, Google, Entra ID, GitHub, HR systems) and want rich profiles with privacy controls.

## Decision

**Profile fields**
- **Base fields** are fixed. **Custom fields** are defined by admins.
- A **library of ready-made fields** (Telegram, phone, birthday, T-shirt size, ...) comes with sensible defaults; admins toggle them on.
- Creating a field takes name, type and **visibility** (*Everyone / Team / Admins only*). Advanced settings are collapsed: required at onboarding, who can edit, whether the user can hide it, sensitive.
- Sensitive fields are encrypted, their reads are audited, and they are only reachable through sensitive scopes.
- Small touches are part of the spec: **birthday shows day and month, never the year**; avatar and name sync into managed services; a welcome onboarding flow at first sign-in.
- Privacy: retention policies after offboarding, and data export and deletion on request (GDPR).

**Import pipeline**
```
Source → staging records → matching & preview (create / update / skip / conflict) → apply
```
- **v0.1 sources:** CSV/XLSX (column mapping UI, downloadable template) and GitHub organization members.
- **Later:** Orglane as a **SCIM server** (Okta, Entra ID, JumpCloud, Google push), LDAP/AD, HR systems.
- Re-import is idempotent, matching by email or external ID.
- Under research: the directory source of truth (Orglane master vs sync from an external directory with read-only fields).

## Consequences

- Simple for admins by default, with a complete data model from day one.
- CSV covers every system as a fallback, and SCIM covers most identity providers with one standard.
