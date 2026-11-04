# Orglane

**The open-source operating system for your company.**
People, access, tools and notifications in one app. Self-hosted or in the cloud.

> ⚠️ **Pre-alpha.** Orglane is under active early development and is not ready for real company data: expect missing features, breaking changes and no upgrade path between early versions. Sign-in, people, teams, invitations and people import work today; follow the [roadmap](ROADMAP.md), and star the repo to get notified when the first preview ships.

---

## Why

Big companies give their employees one internal app: find a colleague, see every notification, open any internal tool without signing in again, and lose all access in one click when they leave. Small companies get a pile of SaaS accounts or a pile of self-hosted apps that nobody wants to upgrade.

Orglane brings that internal ecosystem to any company, from 3 people to 300, built on proven open-source services.

## What it does (planned)

- **One app** on mobile and web. Sign in with your work email or scan a QR code.
- **One identity.** Hire once → access everywhere. Offboard once → access revoked everywhere, with a per-service report.
- **Real integration, not links.** Users and teams are provisioned into each service, events become one unified Inbox, and every service opens with single sign-on.
- **Start where you are, move home when ready.** Connect GitHub, Telegram and Google Calendar today. Move to managed open-source services like Forgejo later.
- **Self-hosting without the pain.** Tested version sets, upgrades with automatic rollback, backups that are verified by restoring them.
- **Built for extension.** Modules and AI agents use one API with explicit scopes, consent and audit. MCP is supported from day one.
- **Privacy first.** End-to-end encrypted mobile push through an open-source relay.

## How it works

```
 Clients (mobile · web)
        │
 Unified company API  ◄── modules & AI agents (scopes, MCP)
        │
 Core: people · identity (OIDC) · permissions · events · audit
        │
 Providers: managed (Forgejo, ...) or external (GitHub, Telegram, Google, ...)
```

Read more: [Product brief](docs/brief.md) · [Architecture](docs/architecture.md) · [Decisions (ADRs)](docs/adr/)

## Quick start

One command on Linux or macOS with Docker:

```sh
curl -fsSL https://orglane.app/install.sh | sh                        # asks: demo, domain or local
curl -fsSL https://orglane.app/install.sh | sh -s -- --mode demo --yes  # the demo company on a laptop, no questions
```

The demo mode loads the "Acme Robotics" sample company with one-click sign-in as any of its people. Read the script first, or check its SHA-256 on [orglane.app](https://orglane.app). Options, modes and the production setup with your own domain are in [deploy/README.md](deploy/README.md#the-installer).

## Contributing

Ideas, feedback and contributions are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md). If you work with AI coding agents, point them to [AGENTS.md](AGENTS.md).

## License

- Server and web UI: [AGPL-3.0-or-later](LICENSE)
- Mobile and desktop apps, SDKs and shared packages: Apache-2.0 (each with its own `LICENSE` file)

"Orglane" and the Orglane logo are trademarks of the project. See [TRADEMARK.md](TRADEMARK.md).
