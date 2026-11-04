# providers

Provider adapters (Go, compiled into the core binary, part of the root Go module) and managed service packages. See [docs/architecture.md](../docs/architecture.md) and [ADR 0002](../docs/adr/0002-capability-provider-instance.md).

Contracts: [docs/specs/providers.md](../docs/specs/providers.md) (adapters, registry, provisioning, webhooks, events) and [docs/specs/agent.md](../docs/specs/agent.md) (managed packages).

| Package | What |
|---|---|
| `provider/` | The contracts: manifest, capability matrix, optional interfaces (`UserProvisioner`, `EventSource`, `Lifecycle`, `CredentialRotator`, ...) and shared types. Not a public SDK yet |
| `testprovider/` | An in-memory fake for the framework's own tests and the web acceptance build (`appstest`). Not a mock of any real provider API; never compiled into production binaries |
| `agenttest/` | A test-only managed package for the agent's tests (compiled into the agent only with the build tag `agenttest`) |

No real adapter is implemented yet; the core lists compiled adapters in `core/internal/apps/registry.go`.
