# providers

Provider adapters (Go, compiled into the core binary, part of the root Go module) and managed service packages. See [docs/architecture.md](../docs/architecture.md) and [ADR 0002](../docs/adr/0002-capability-provider-instance.md).

Contracts: [docs/specs/providers.md](../docs/specs/providers.md) (adapters, registry, provisioning, webhooks, events) and [docs/specs/agent.md](../docs/specs/agent.md) (managed packages). No adapter is implemented yet; `agenttest/` is a test-only managed package for the agent's tests (compiled into the agent only with the build tag `agenttest`).
