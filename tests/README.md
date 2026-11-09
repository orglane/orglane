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

## Test tiers

CI splits the tests into tiers so a pull request gets a verdict quickly. The
`changes` job in `.github/workflows/ci.yml` decides, from the changed paths,
which tiers run; keep its path list and this table in sync.

| Tier | Jobs and targets | Runs on pull requests |
|---|---|---|
| Fast | `lint`, `build`, `security`, `make test-unit`, `make test-ts`, `make test-root`, `make test-integration-fast` (PostgreSQL integration with `-race`), `make test-apps` (three shards of `core/internal/apps` PostgreSQL tests, `-race`), `make test-agent` (the agent's non-Docker tests, `-race`), `make test-website` | Always |
| Heavy | `make test-apps-e2e` (three shards: real Forgejo, agent and Traefik, including the M2 acceptance scenario), `make test-agent-e2e` (real Docker projects), `make test-compose`, `make test-install`, `make test-e2e` (web UI) | Only when a path the tier exercises changed, or with the `full-ci` label |

The heavy tier runs without `-race`: the race detector cannot observe
anything inside the containers and multiplies their runtime. Unit tests and
PostgreSQL-only integration tests always run with `-race`. Pushes to `main`
and the nightly schedule run both tiers. The required check named `test` is
an aggregate of every test job: it fails when one failed or was cancelled,
and accepts a job that the tier rules intentionally skipped. The `full-ci`
pull request label forces the heavy tier on the next run; because a `labeled`
event would also fire for every `risk/*` label the Policy workflow adds, the
label is read on push/synchronize runs and the `workflow_dispatch` input
`full-ci` runs everything immediately by hand.

`make test` still runs everything locally, one heavy group after the other
(the Docker Desktop/Colima ownership caveat in the Makefile). To run one
tier:

```sh
make test-integration-fast          # PostgreSQL integration, no managed services
make test-apps SHARD=1 SHARDS=3     # one apps PostgreSQL shard
make test-apps-e2e SHARD=1 SHARDS=3 # one apps Docker e2e shard
make test-agent                     # agent tests that do not start Docker projects
make test-agent-e2e                 # agent Docker e2e
make test-compose test-install      # Compose bundle and installer
make test-e2e                       # web end-to-end tests
```

`tests/shard.sh` lists a package's tests and runs every count-th one, so a
new test joins a shard automatically. `make test-apps` skips the tests named
in `APPS_E2E`; add a new Docker test to that list (and `AGENT_E2E` for the
agent) so it runs in the heavy tier.

The `core/internal/apps`, identity, profile, peopleimport, jobs and demomode
integration tests use `pgtest.MainTemplate`: the migrations run once into a
template database in `TestMain`, and every test still gets its own fresh
database (`CREATE DATABASE ... TEMPLATE`). Packages that test migrations
themselves use `pgtest.Main` without a template.
