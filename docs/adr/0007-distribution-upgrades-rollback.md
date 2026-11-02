# 0007. Orglane as a distribution: tested version sets, upgrades with rollback

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Upgrades are the main reason people abandon self-hosting. Upgrades fail, some products need many intermediate upgrade steps, and nobody tests their backups.

## Decision

- An Orglane release is a **tested set of provider versions**, like a Linux distribution. Users don't pick service versions.
- Each managed provider ships a **package**: manifest, deployment template, health checks, backup hooks and **required upgrade stops**.
- **Upgrade flow:** snapshot → new image → migrations → health check → **automatic rollback** on failure.
- **Backups:** off by default during onboarding (Orglane works without them), but the health panel shows their status persistently. Targets are local or S3-compatible storage. A **test restore** runs regularly and its result is visible.

## Consequences

- Fewer combinations to support and a much safer upgrade path.
- Provider updates reach users through Orglane releases, which requires a timely release cadence for security fixes.
