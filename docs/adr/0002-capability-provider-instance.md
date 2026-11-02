# 0002. Capability → Provider → Instance, managed and external providers

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Companies choose different tools, and many already use SaaS (GitHub, Telegram, Google). Orglane must integrate deeply without locking the architecture to any single vendor, and it must support moving from SaaS to self-hosted services over time.

## Decision

- **Capability:** an abstract business function (`code`, `files`, `calendar`, `contacts`, `tasks`, `chat`, `mail`, `ingress.tunnel`, ...).
- **Provider:** an implementation of one or more capabilities (`forgejo`, `github`, `telegram`, ...).
- **Instance:** a concrete installed or connected service in the company, with `management: managed | connected`.
- **Binding:** maps a capability to an instance, for example `code: git-main`.
- Providers are either **managed** (Orglane deploys and operates them) or **external** (SaaS with stable public APIs). Connecting arbitrary existing self-hosted installs comes later.
- Adapters are small composable contracts: `UserProvisioner`, `EventSource`, `DeepLinker`, `CapabilityManifest`, `Lifecycle` (managed only), plus capability-specific readers (for example "pull requests awaiting my review").
- A **capability matrix** honestly shows what each provider supports, for example "provisioning: invite only; deprovisioning: org removal".
- **Cross-service links** (a task key in a PR title, for example) are made by Orglane, not by each pair of services: N+M integrations instead of N×M.
- Adapters are statically compiled Go code in the monorepo until the provider SDK is published (v0.2).
- Providers can declare **employee steps** (for example "Link your GitHub"), which form the onboarding checklist.

## Consequences

- "Start where you are, move home when ready": switching a binding from `github` to `forgejo` plus data import is a first-class flow.
- Interchangeable APIs don't mean automatic data migration. Migrations are explicit tools.
- External providers can have weaker guarantees, and the UI must show that.

## Alternatives considered

- **Hard-wiring one vendor per function:** simpler, but against the product's core promise.
- **A huge universal plugin interface:** brittle and hard to implement partially.
