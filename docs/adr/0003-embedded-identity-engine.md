# 0003. Embedded OIDC identity engine and own login UI

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Identity is the heart of the product: SSO into every service, provisioning, offboarding, device-bound mobile sessions and "Sign in with Orglane" for modules. We want our own login design, a rich people directory, a light footprint for home servers and a single source of truth.

## Decision

**Own the experience and the data. Don't own the cryptography.**

- **Directory and profiles** (fields, visibility, org structure) live in the Orglane core and are fully ours.
- The **OIDC provider is embedded in the Go core** and built on `zitadel/oidc` (an OpenID-certified library), with `go-webauthn` for passkeys.
- **Own login UI**, in Orglane's design system.
- The **OpenID Conformance Suite runs in CI**.
- An **external security audit** is required before 1.0.
- LDAP facade (for LDAP-only apps), SAML and SCIM come later.

## Consequences

- One binary, one data model, no user synchronization between two systems.
- We own session security, MFA and account recovery, so this area is high-risk in the review process (ADR 0017).
- Services speak standard OIDC to us, so replacing the engine later wouldn't affect integrations.

## Alternatives considered

- **Keycloak:** most mature, but heavy (JVM, about 1 GB of RAM) and themed rather than designed.
- **Zitadel as a separate service:** good fit technically, but means a second source of truth plus sync and vendor direction risk.
- **Authentik:** not truly multi-organization, limited UI control.
- **Ory Kratos + Hydra:** headless, but several services and a duplicate identity store.
