# tests

Integration and end-to-end tests. Integration tests run against real services (PostgreSQL, Forgejo, ...) in containers, never against mocks of provider APIs.

Unit tests live next to the code they test (`*_test.go`, `*.test.ts`).

| Path | Contents |
|---|---|
| `pgtest/` | Starts PostgreSQL with testcontainers-go for Go integration tests |
| `integration/` | Runs the `orglane` binary against PostgreSQL: start-up, migrations, health checks, internal listener, graceful shutdown, idle memory, first-run setup, demo mode and the CLI |
| `demotest/` | Signs in to a server in demo mode (`orglane server --demo`) as a person of the "Acme Robotics" dataset (`demo/`) for end-to-end tests |
| `webauthntest/` | A software WebAuthn authenticator (ES256, "none" attestation) for passkey tests |
| `conformance/` | `make conformance`: the OpenID Conformance Suite against a local server, all in Docker |
| `compose/` | Builds the core image and starts the `deploy/compose` bundle: routing through Traefik in local mode, certificate issuance against Pebble in domain mode (`make test-compose`) |

Go integration tests carry the `integration` build tag. `make test` runs them when Docker is available; `make test-integration` always does.
