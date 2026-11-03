# Architecture Overview

> Status: target architecture for v0.1–v0.2. Details evolve; key decisions are recorded in [`docs/adr/`](adr/).

## Layers

```
┌──────────────────────────────────────────────────────────┐
│ Clients: mobile · web · desktop (later)                  │
├──────────────────────────────────────────────────────────┤
│ Modules & agents (scoped access, consent, audit, MCP)    │
├──────────────────────────────────────────────────────────┤
│ Unified company API: people · code · tasks · calendar ...│
├──────────────────────────────────────────────────────────┤
│ Core: directory, identity, permissions, devices,         │
│       events, notifications, audit                       │
├──────────────────────────────────────────────────────────┤
│ Providers: managed (Forgejo, ...) or external (GitHub,   │
│            Telegram, Google, ...)                        │
├──────────────────────────────────────────────────────────┤
│ Operations: install, upgrades, backups, health           │
└──────────────────────────────────────────────────────────┘
```

Every client, built-in feature, third-party module and AI agent uses **the same unified API**, each with its own permissions. The web UI gets no private shortcuts.

## Runtime components (self-hosted)

| Component | Role |
|---|---|
| `orglane` (Go binary, `server` mode) | API, web UI (embedded static build), identity (OIDC provider), background jobs, event pipeline, MCP server |
| `orglane` (same binary, `agent` mode) | Privileged local agent that manages app instances. Talks to Docker and accepts declarative operations from the core over a local socket. Only needed when managed services are used |
| PostgreSQL | Shared database: one database for the core, plus one per managed service where supported |
| Traefik | Ingress and TLS (Let's Encrypt). Routes come from the core, not from the Docker socket |
| Managed services | For example Forgejo. Generated Compose projects under `/var/lib/orglane/instances/<id>` |

The core has no Redis, message broker, search engine, Node.js server or JVM. Jobs use a Postgres-backed queue inside the process; search uses Postgres full-text search. Configuration adapts to the host's resources.

## Shared services (operated by the project)

| Service | Purpose | Required? |
|---|---|---|
| Push relay (`push.orglane.app`, open source) | Forwards end-to-end encrypted push to APNs/FCM | Only for mobile push; in-app inbox, Web Push and Telegram work without it |
| Discovery | Maps an email domain to a cloud instance (self-hosted instances use `/.well-known/orglane`) | No |
| Link | Subdomain, tunnel, unlimited push, encrypted off-site backups | No (subscription) |
| Cloud control plane (private) | Provisions and upgrades one Orglane instance per company on regional k3s clusters | Cloud only |

## Core domain model

- **Person**: base fields (username, display name, work email, avatar, title, manager, teams, timezone, locale, employment dates, status) plus admin-defined **profile fields**, each with type, section, visibility, edit rights and a sensitivity flag.
- **Team**, **Role** (a bundle of **permissions**), **Membership**.
- **Device** and **Session**: sessions are bound to devices; devices can be revoked and wiped remotely.
- **AccountLink**: a person ↔ external account (a GitHub login, a Telegram user ID, ...).
- **Capability** → **Provider** → **Instance** → **Binding**: for example `code` → `forgejo` → `git-main` (managed) or `code` → `github` → `acme-org` (external). An instance records its URL, credentials, version, health, backup policy and `management: managed | connected`.
- **Event**: a normalized, append-only record (type, actor, subject resource, provider, time).
- **Notification**: event × recipient, with read state, priority (`normal | time_sensitive | critical`) and grouping key.
- **Module**: an installed extension with a manifest, granted **scopes** and extension points.
- **AuditEntry**: who did what, including reads of sensitive fields and access by modules and agents.

## Key flows

### Provisioning

```
Admin adds person / team change
  → desired access computed from teams and roles
  → per provider: create / update / link account / grant team access
  → result per provider recorded (done · pending employee action · failed)
```

External providers can't always create accounts (GitHub accounts are personal, for example). In that case provisioning becomes an **employee step**, "Link your GitHub", shown in the onboarding checklist and in the Inbox.

### Event → notification

```
Provider webhook / poll / internal event
  → verify signature → normalize → resolve recipients via AccountLinks
  → check the recipient can see the resource (at delivery time)
  → group / deduplicate → store in Inbox (source of truth)
  → deliver: mobile push (encrypted) · Web Push · Telegram DM
```

Reading a notification on one device marks it read everywhere and clears delivered pushes on other devices.

### Opening a service without signing in again

- **Web:** a new tab opens the provider's "sign in and go to" URL (for example Forgejo's OAuth start URL with `redirect_to`). The provider redirects to Orglane, Orglane sees the existing session and redirects back silently.
- **Mobile:** the app requests a short-lived, single-use **handoff link** that sets an Orglane session in the browser, then follows the same silent SSO path. Links open in the external browser by default.

### Offboarding

```
Admin blocks person
  → revoke Orglane sessions and devices (remote wipe of app data)
  → per provider: suspend/remove account, revoke tokens/SSH keys where APIs allow,
    remove from orgs/teams/groups (GitHub org, Telegram groups, ...)
  → per-provider report; unresolved items are shown, never hidden
```

## Managed service lifecycle

- **Desired state:** the core stores what should exist; the agent reconciles (install, configure, upgrade, back up, restore). In the cloud, a Kubernetes operator implements the same interface.
- **Packages:** each managed provider ships a versioned package (manifest, Compose template or Helm chart, health checks, backup hooks, required upgrade stops). Packages are part of an Orglane release, which is a tested version set.
- **Upgrades:** snapshot → new image → migrate → health check → automatic rollback on failure.
- **Transparency:** generated Compose files are plain files on disk that anyone can read.

## Deployment models

- **Self-hosted (Community):** Docker Compose, one company per installation. Try it on a laptop with `--demo` (localhost, no domain). Production needs a domain (subdomains per service, e.g. `git.acme.com`) and reachability: a public IP, a Cloudflare Tunnel token, or Orglane Link.
- **Cloud:** one Orglane instance per company (the same image as self-hosted) on regional k3s clusters, with a shared regional Postgres (one database per company). Moving between cloud and self-hosted in either direction works by exporting and importing the instance.

## Extensibility

- **Modules** are MCP servers plus an Orglane manifest (scopes, extension points), with optional UI.
- **UI tiers:** (1) native components from Orglane's kit (List, Detail, Form, Table, Chart, Card, Actions) rendered by every client, which is the default; (2) sandboxed web mini-apps with the Orglane SDK, a cached bundle and a performance budget, for exceptional cases; (3) external services using "Sign in with Orglane" and the API.
- **First extension points:** global search, Home widgets, notification sources.
- **Access:** delegated (acts as the user, sees what the user sees) or module-level (admin-approved scopes). Every access is audited.

## Repository layout (evolutionary)

```
orglane/
├── core/                 # Go: cmd/orglane (server | agent | cli), internal/...
├── providers/            # Provider adapters (Go, compiled into core) + managed packages
├── web/                  # React + Vite (embedded into the core binary)
├── mobile/               # Expo app (Apache-2.0)
├── packages/             # Shared TS: API client, design tokens, component kit, i18n, SDKs (Apache-2.0)
├── services/push-relay/  # Open-source push relay
├── deploy/               # Compose files, install script
├── demo/                 # "Acme" demo dataset (demo mode, screenshots, e2e, module previews)
├── tests/                # Integration and end-to-end tests
├── tools/ci/             # CI tooling: risk labeler, merge gate, license check
└── docs/                 # Brief, architecture, ADRs, specs
```
