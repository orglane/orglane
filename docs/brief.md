# Orglane — Product Brief

> Status: living document. Decisions are recorded as ADRs in [`docs/adr/`](adr/); the plan lives in [`ROADMAP.md`](../ROADMAP.md).

## One sentence

Orglane is an open-source operating system for a company: one app where employees find people, tools, notifications and access, built on top of proven open-source services. Run it on your own server or let us run it for you.

## The problem

Big companies have internal ecosystems: one app with an employee directory, profiles, an org chart, notifications that deep-link into every internal tool, corporate VPN and company news. Small and growing companies get none of that. They end up with one of two bad options:

1. **A pile of SaaS tools.** Every tool has its own accounts, its own notifications and its own offboarding checklist. The company's data is spread across vendors.
2. **A pile of self-hosted apps.** You get control over your data, but also an endless ops burden: upgrades that fail, multi-step upgrade paths, no backups, and every app with its own users and logins.

Many people who tried option 2 gave up and moved to option 1, not because self-hosting is wrong but because nobody made it a product.

## The vision

**An internal ecosystem for any company, from 3 people to 300.**

- **One app.** Mobile, web and (later) desktop. Sign in with your work email or scan a QR code, and the whole company is there: people, teams, notifications, services and, later, network access.
- **One identity.** Hire someone once and they get the right access everywhere. Remove them once and access is revoked everywhere, with a report of what was done in each service.
- **Real integration, not a list of links.** Orglane provisions users and teams into each service, receives their events, shows the most common objects (pull requests, tasks, meetings) natively, and opens everything else with single sign-on.
- **Start where you are, move home when ready.** Connect the tools you already use (GitHub, Telegram, Google Calendar) on day one. Later, move each one to a managed open-source service (Forgejo instead of GitHub, for example) with a guided migration.
- **Self-hosting without the pain.** Orglane works like a distribution: tested version sets, upgrades with automatic snapshots and rollback, backups that are actually tested by restoring them.
- **Open to extension.** Modules and AI agents get access to company data through one API with explicit scopes, a consent screen and an audit log. Orglane exposes this API over MCP from the start.

## Who it is for

The product has to be useful to a team of three and grow with the company:

| Company size | What matters most |
|---|---|
| 1–10 people | One-click hosting, painless upgrades, backups, integrated code + tasks + chat, one app |
| 10–50 people | Onboarding and offboarding, roles, notifications, people directory |
| 50+ people | Org chart, company news, VPN, custom modules |

**First audience:** technical founders, small tech teams, agencies and homelab enthusiasts who want their own infrastructure without the ops burden. The product is global and English-first, with localization built into the architecture from day one.

**Later:** companies with data residency requirements, managed service providers (MSPs) running many organizations, and broader SMBs without an IT department.

## Product principles

1. **One ecosystem, not a set of links.** People, teams, SSO, permissions, events and common actions work across every supported service.
2. **Only verified integrations in the official catalog.** An integration is listed only when its adapter, lifecycle and reliability requirements are implemented and tested.
3. **Capabilities, not vendors.** Users choose a capability (code, files, calendar, tasks), then a provider for it. Providers can be managed by Orglane or be external services.
4. **Native where it matters.** The most frequent scenarios (home widgets, inbox, people, the objects you act on daily) are native in Orglane. Deep, specialized work opens the provider's own UI with single sign-on.
5. **The open-source edition is a real product.** Identity, SSO, integrations and notifications are free. No "SSO tax", no artificial limits.
6. **Monetize by removing operational burden.** Paid offerings are convenience and operations (remote access, push without limits, off-site backups, managed hosting), never the core.
7. **Hide infrastructure from regular admins.** No Kubernetes, Docker volumes or database management in the UI.
8. **Security and backups are part of the product.** A backup that has never been restored doesn't count. Access revocation must be verifiable, not declarative.
9. **Excellent UI/UX everywhere.** This includes third-party modules, which render through Orglane's own component kit by default.
10. **No waste.** Light by default, scales up with the host. Nothing runs in the background without a reason.

## Editions and business model

| Edition | What it is | Price |
|---|---|---|
| **Community** | The full self-hosted product, one company per installation | Free, AGPL-3.0 |
| **Orglane Link** | An add-on subscription for self-hosted installs: a subdomain on a dedicated Orglane domain with a tunnel (works behind NAT), push notifications without limits, end-to-end encrypted off-site backups | Subscription |
| **Orglane Cloud** | Fully managed hosting: one isolated Orglane instance per company, in a region of your choice | Subscription |
| **Enterprise** (later) | Support, a fully white-labeled mobile app, dedicated infrastructure | Contract |

**Link comes before Cloud.** It's cheap to operate (a tunnel, a push relay, object storage) and creates recurring revenue without taking on hosting other people's services. Home Assistant's Nabu Casa is a good reference for this model.

Everything works without Orglane's servers. Link and Cloud are conveniences, never requirements. The only exception is mobile push, which technically has to go through the app publisher's relay. That relay is open source, sees only encrypted payloads, and has a generous free tier.

## What Orglane is not

- Not another app installer or a catalog of Compose files.
- Not a Kubernetes platform for end users.
- Not a mail server operator (mailboxes come from external providers).
- Not a rewrite of Forgejo, Nextcloud or any other mature service.

## Open questions

These are tracked in the roadmap as research items:

- **Providers for v0.2:** task tracker; files, calendar and contacts; mail provisioning.
- **Shared pools in the cloud** for heavy stateless services (document editors, CI runners), and the licensing constraints of those services.
- **Copyright status of AI-generated code** in different jurisdictions.
- **Directory source of truth:** Orglane vs an external directory (Google, Entra ID) in sync mode.
- **Pricing** for Link and Cloud, validated against real costs.
