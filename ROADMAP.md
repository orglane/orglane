# Roadmap

> The plan is fixed at the level of milestones and scope headlines. Provider choices (task tracker, files, calendar, mail) are decided by research tracks and added here when made. Small adjustments are expected; **new ideas go to v0.3+ by default**, so launches don't slip.

## Overview

| Milestone | Character | Audience | Status |
|---|---|---|---|
| **M0 — Foundation** | Private. Ends with the repository going public (pre-alpha) | — | Done |
| **v0.1 — Developer Preview** (M1–M7) | Quiet release, devlog only | Maintainers and early adopters | In progress: M1 done, M2 in progress |
| **v0.2 — Public Launch** | Loud: Hacker News, Reddit, Product Hunt | Everyone | Research tracks started |
| **v0.3+** | Growth | — | — |

Until v0.1, pre-alpha releases are numbered 0.0.x (the first is 0.0.1). They are for trying Orglane out, not for real company data.

---

## M0 — Foundation

- Monorepo, CI pipeline, code owners, risk-tier labels, auto-merge rules ([ADR 0017](docs/adr/0017-development-process.md))
- Go core skeleton (`server` / `agent` / `cli` modes), PostgreSQL and migrations
- Embedded OIDC provider with passkeys and own login UI; OpenID Conformance Suite in CI ([ADR 0003](docs/adr/0003-embedded-identity-engine.md))
- Web skeleton (React + Vite, embedded into the binary), design tokens, i18n setup
- Traefik with routes served by the core
- One-command install script; `--demo` skeleton with the "Acme" dataset
- `SECURITY.md`, issue templates, `AGENTS.md`

**Exit:** `curl … | sh` brings up a working sign-in. Docs and skeleton are in place, and **the repository goes public** with a pre-alpha notice.

**Status:** done. Verified on a fresh 2 vCPU / 4 GB VPS with a real domain and a Let's Encrypt certificate; the repository is public.

## Research tracks (in parallel, starting after M0)

Each candidate is deployed and compared on idle and loaded memory, OIDC quality, provisioning/deprovisioning APIs, webhooks/events, design, license and upgrade pain. Output: a short report with a recommendation; the maintainer decides.

- **Tasks** — task tracker provider
- **Files, calendar, contacts** — one service for all three vs lighter separate services
- **Mail** — mailbox provisioning providers

---

## v0.1 — Developer Preview

| | Milestone | Scope |
|---|---|---|
| **M1** ✅ | People | People, teams, Owner/Admin/Member roles, profile field library + custom fields + visibility, CSV/XLSX and GitHub org import, invites, onboarding checklist |
| **M2** | Code | **Forgejo (managed):** agent, package, upgrade with snapshot and rollback, SSO, provisioning, team mapping, webhooks, silent SSO deep links. **GitHub (external):** GitHub App, account linking, invites, removal, webhooks |
| **M3** | Notifications | Event pipeline, Inbox, grouping, priorities, Web Push, Telegram bot (linking, DMs, invite links and removal from groups) |
| **M4** | Mobile app | Expo app: QR and email sign-in, Home, Inbox, People (offline), Apps, devices, end-to-end encrypted push via the open-source relay |
| **M5** | Offboarding & audit | Block → revoke sessions and devices; Forgejo suspension, tokens and SSH keys; GitHub org removal; Telegram removal; per-provider report; audit log |
| **M6** | MCP | Scopes, delegated tokens with consent, read-only MCP server (people, teams, pull requests, notifications) |
| **M7** | Backups | Local and S3 targets, core + Forgejo, test-restore command, health panel |

**M2 status (in progress).** Merged: the provider framework, the Forgejo managed package and lifecycle, install/remove/purge/reinstall through the API and Settings → Apps (#61, #94), SSO/provisioning/team mappings/webhooks/normalized events/deep links, the GitHub App connect/link/provision/removal and webhooks, the core–agent install and apply/sync jobs, and the agent's upgrades with snapshot and rollback. The M2 acceptance scenario (#67) is automated: `core/internal/apps` sets up a company, invites a person, installs Forgejo through the API and the real agent, provisions the person into a mapped team, turns a real Forgejo mention into a normalized event, opens the pull request through the silent-SSO deep link, and demonstrates a Forgejo upgrade rollback; its GitHub part runs with the `github-live` environment and skips explicitly without it. M2 is **not done** yet: core-side upgrade confirmation and planning (#75, PR #104) and the protected `github-live` workflow (#71, PR #79) are still open.

**v0.1 exit criteria**
1. Fresh VPS (2 vCPU / 4 GB) → `curl … | sh` → working with own domain in **≤ 10 minutes**; `--demo` on a laptop in **≤ 2 minutes**.
2. **Golden scenario** automated and green: create company → invite → access in Forgejo and GitHub → mention in a PR → **push on the phone in ≤ 10 s** → open the PR without signing in again → block → access revoked everywhere with a report → restore from backup → everything works.
3. Upgrade 0.1.0 → 0.1.1 succeeds, and a failed upgrade rolls back automatically.
4. The mobile app is publicly installable (TestFlight public link, Google Play or APK).
5. `SECURITY.md`, threat model, OpenID Conformance passed, dependency scans clean.
6. Docs: install, architecture, compatibility matrix, an honest list of limitations.
7. The maintainers run their own work on Orglane for at least two weeks.

---

## v0.2 — Public Launch

Scope (fixed; provider names come from the research tracks):

- **Tasks** — a task tracker provider, with native "my tasks" in Home and the Inbox
- **Files and calendar** — managed providers, plus external **Google Calendar**
- **Public SDKs** — provider SDK and module SDK (global search, Home widgets), component gallery
- **Contributor experience**
  - one-click GitHub Codespaces and a one-command local dev setup with demo data
  - generators: `orglane dev new-provider`, `create-orglane-module`
  - provider conformance test suite
  - `AGENTS.md`, tutorials ("Your first provider in 30 minutes", "Your first widget in 15 minutes")
  - triage agent, issue templates, `good first issue` / `help wanted`
- **Languages:** an initial batch of machine-translated languages (for example German, French, Chinese, Russian), labeled as machine-translated until native speakers review them via Weblate. The i18n infrastructure exists from M0; the UI ships in English only until then.
- **Cloudflare Tunnel** in the setup wizard
- *Optional:* mailbox provisioning via an external mail provider
- **Community:** GitHub Discussions (primary) and Discord

**v0.2 exit criteria** (in addition to all v0.1 criteria)
1. **Non-developer scenario** automated and green: hire a manager → files, calendar and tasks provisioned → task assigned → push → meeting shows on Home → file shared → notification.
2. A new contributor goes from zero to a running dev environment in **≤ 5 minutes** (Codespaces).
3. From zero to a custom provider or widget visible in demo mode in **≤ 30 minutes**, verified with a fresh AI agent and with one human.
4. Versioned public SDKs with documentation and a component gallery.
5. README, 60–90 s demo video recorded from demo data, docs site, screenshots.

---

## v0.3+ (not scheduled)

- **Orglane Link:** subdomain + tunnel, unlimited push, encrypted off-site backups
- Desktop app
- Wiki / knowledge base
- AI: company AI gateway (provider keys and endpoints configured once, chat and personal API keys for every employee), built-in assistant with generative UI using the component kit
- Orglane Cloud beta (one instance per company, regional clusters)
- Corporate VPN
- SCIM server, LDAP facade, SAML
- Native detail screens (PRs, tasks), multi-company switching in the app
- On-call (`oncall` capability), critical alerts
- More providers and migrations ("move home" from SaaS)
