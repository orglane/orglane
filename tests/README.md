# tests

Integration and end-to-end tests. Integration tests run against real services (PostgreSQL, Forgejo, ...) in containers, never against mocks of provider APIs.

Unit tests live next to the code they test (`*_test.go`, `*.test.ts`).

| Path | Contents |
|---|---|
| `pgtest/` | Starts PostgreSQL with testcontainers-go for Go integration tests |
| `integration/` | Runs the `orglane` binary against PostgreSQL: start-up, migrations, health checks, graceful shutdown, idle memory, first-run setup and the CLI |
| `webauthntest/` | A software WebAuthn authenticator (ES256, "none" attestation) for passkey tests |
| `conformance/` | `make conformance`: the OpenID Conformance Suite against a local server, all in Docker |

Go integration tests carry the `integration` build tag. `make test` runs them when Docker is available; `make test-integration` always does.
