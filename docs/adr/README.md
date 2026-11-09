# Architecture Decision Records

Each ADR records one significant decision: the context, the decision and its consequences. ADRs are immutable once accepted. To change a decision, add a new ADR that supersedes the old one.

**Statuses:** `Proposed` · `Accepted` · `Superseded by NNNN` · `Deprecated`

| # | Decision | Status |
|---|---|---|
| [0001](0001-technology-stack-and-monorepo.md) | Technology stack and monorepo | Accepted |
| [0002](0002-capability-provider-instance.md) | Capability → Provider → Instance, managed and external providers | Accepted |
| [0003](0003-embedded-identity-engine.md) | Embedded OIDC identity engine and own login UI | Accepted |
| [0004](0004-permissions-and-roles.md) | Permission-based authorization, roles as bundles | Accepted |
| [0005](0005-single-company-core.md) | Single-company core, one instance per company in the cloud | Accepted |
| [0006](0006-agent-and-desired-state.md) | Privileged agent and desired-state reconciliation | Accepted |
| [0007](0007-distribution-upgrades-rollback.md) | Orglane as a distribution: tested version sets, upgrades with rollback | Accepted |
| [0008](0008-ingress-domains-tunnels.md) | Traefik ingress, subdomains, tunnels as a capability | Accepted |
| [0009](0009-resource-philosophy.md) | Resource philosophy: no waste, scales up | Accepted |
| [0010](0010-events-and-notifications.md) | Event pipeline, Inbox as source of truth, priorities | Accepted |
| [0011](0011-encrypted-push-relay.md) | End-to-end encrypted push via an open-source relay | Accepted |
| [0012](0012-mobile-client.md) | Mobile client: discovery, QR sign-in, devices, link handoff | Accepted |
| [0013](0013-extensibility-modules-mcp.md) | Modules = MCP server + manifest, native component kit | Accepted |
| [0014](0014-mail.md) | Mail: address as identity, mailboxes via external providers | Accepted |
| [0015](0015-people-directory-and-import.md) | People directory, profile fields, import pipeline | Accepted |
| [0016](0016-licensing.md) | Licensing: AGPL core, Apache clients and SDKs, no CLA | Accepted |
| [0017](0017-development-process.md) | Agent-driven development process, risk tiers, auto-merge | Accepted; `merge-gate` status superseded by [0020](0020-merge-checklist.md) |
| [0018](0018-domain-model.md) | Domain model: project domain, customer domain, company domains | Accepted |
| [0019](0019-agent-transport-and-packages.md) | Agent placement, core–agent transport and trust, managed package format | Accepted |
| [0020](0020-merge-checklist.md) | Merge checklist instead of a merge-gate commit status | Accepted |

Template: [`0000-template.md`](0000-template.md)
