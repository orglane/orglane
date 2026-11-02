# 0010. Event pipeline, Inbox as source of truth, priorities

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Unified notifications are a core promise, and also the fastest way to annoy people if done badly.

## Decision

- **Pipeline:** ingest (verify signatures) → normalize into Orglane events → resolve recipients via account links → **check permissions at delivery time** → group and deduplicate → store → deliver.
- **The Inbox is the source of truth.** Every notification lands there first; push, Web Push and Telegram are delivery channels. Read in one place means read everywhere, and delivered pushes are cleared on other devices.
- **Grouping is mandatory** ("3 new comments on PR #142").
- **Channels in v0.1:** Inbox, mobile push, Web Push (no relay needed), Telegram DM. Email digests come later.
- **Priorities:** `normal` (respects Do Not Disturb), `time_sensitive` (iOS Time Sensitive: breaks through Focus if the user allows), `critical` (later, for on-call; requires Apple approval).
- **No custom quiet hours.** Users rely on OS Do Not Disturb, and urgency is expressed through priority.
- Preferences are a simple category × channel matrix with sensible defaults.
- On-call scheduling is a future capability (`oncall`).
- Events are stored append-only through a Postgres outbox. No message broker.

## Consequences

- One mental model for every channel.
- Delivery-time permission checks keep notifications from leaking access that has since been revoked.
