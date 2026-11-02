# 0016. Licensing: AGPL core, Apache clients and SDKs, no CLA

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Orglane must be real open source (OSI-approved), protected against being resold as a hosted service without giving back, compatible with app stores, and friendly to module developers.

## Decision

| Part | License |
|---|---|
| Server (core, providers, agent) and web UI | **AGPL-3.0-or-later** |
| Mobile and desktop apps | **Apache-2.0** |
| SDKs, module templates, API clients, shared UI packages | **Apache-2.0** |
| Push relay | Open source (AGPL-3.0-or-later) |
| Cloud control plane | Private, separate repository |

- **No CLA bot.** Contributions follow **inbound = outbound**: a contribution is licensed under the license of the part of the repository it targets. `CONTRIBUTING.md` asks contributors to also grant the project maintainers the right to relicense their contribution as part of the project. If external contributions grow significantly, or an enterprise edition is planned, we revisit a CLA.
- **Trademark:** the name and logo aren't covered by the code licenses. See `TRADEMARK.md`; forks are welcome under a different name.
- No source-available licenses (BSL, FSL) for the product.

## Consequences

- Companies can freely run Orglane internally. Hosting providers must share modifications.
- Apps can ship in the App Store and Google Play.
- Module developers, including those building closed-source modules, aren't constrained by the AGPL.
- Without a CLA, the AGPL parts contributed by others can't be relicensed without their consent. This is accepted for now.
- Under research: the copyright status of AI-generated code.
