# 0006. Privileged agent and desired-state reconciliation

- **Status:** Accepted
- **Date:** 2026-11-03

## Context

Orglane installs and operates services. Giving the public-facing server access to the Docker socket is a serious security risk. We also want the same model for Compose (self-hosted) and Kubernetes (cloud) to avoid a rewrite later.

## Decision

- A separate **agent** (`orglane agent`, the same binary in another mode) is the only component with Docker access. It listens on a local socket and accepts **declarative operations** (install, configure, upgrade, back up, restore), never arbitrary commands.
- The core stores the **desired state**; the agent **reconciles** toward it. It reacts to changes and runs an infrequent periodic check.
- The agent generates **plain Compose files on disk** under `/var/lib/orglane/instances/<id>` and drives `docker compose`.
- In the cloud, a Kubernetes operator implements the same interface.
- The agent is only installed when managed services are used.

## Consequences

- A smaller attack surface for the web-facing core.
- Homelab users can read exactly what runs, with no hidden magic.
- The Compose and Kubernetes backends share contracts and test suites.
