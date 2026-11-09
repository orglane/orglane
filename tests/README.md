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
which tiers run; keep its selector in sync with this table. The selector is an
allowlist of paths that may *skip* the heavy tiers: only documentation,
Markdown, the website and license texts skip them; everything else (code,
tests, helpers, migrations, deploy and image inputs, demo data, workspace
configuration) runs them. A new dependency of an e2e test therefore never
silently stops running it; the failure mode is running too much. Rename
detection is off: a move is seen as the deletion of the old path plus the
addition of the new one, so moving `core/**` into `docs/` cannot hide the code
from the selector, and deleted files keep their path. The selector, the shard
selection and the aggregate check have regression tests in `tests/ci`
(`make lint-ci`), including moves into/out of the allowlist and deletions.

| Tier | Jobs and targets | Runs on pull requests |
|---|---|---|
| Fast | `lint`, `build`, `security`, `make test-unit`, `make test-ts`, `make test-root`, `make test-integration-fast` (PostgreSQL integration with `-race`), `make test-apps` (three shards of `core/internal/apps` PostgreSQL tests, `-race`), `make test-agent` (the agent's non-Docker tests, `-race`), `make test-website` | Every pull request with code changes (docs-only PRs keep the existing skip) |
| Heavy | `make test-apps-e2e` (three shards: real Forgejo, agent and Traefik, including the M2 acceptance scenario), `make test-agent-e2e` (real Docker projects), `make test-compose` and `make test-install` (one job), `make test-e2e` (web UI) | Unless every changed path is docs/website/license, or with the `full-ci` label |

The heavy tier runs without `-race`: the race detector cannot observe
anything inside the containers and multiplies their runtime. Unit tests and
PostgreSQL-only integration tests always run with `-race`. Pushes to `main`
and the nightly schedule run both tiers. The required check named `test` is
an aggregate of every test job: it runs with `always()`, so cancelled work
reports a failed verdict instead of disappearing, and it fails on any failed
or cancelled job and on any skip that the changes job did not explicitly
select (the heavy tiers are the only jobs that may be skipped, and only when
the selector left them out). The aggregate's own logic is tested in
`tests/ci`. The `full-ci` pull request label forces every tier immediately,
including on a documentation-only change where it also overrides the docs-only
skip:
`labeled`/`unlabeled` are pull request trigger types, so adding or removing the
label starts a run, and the changes job reads the event's current label
snapshot (removing it returns to the path selection on the next run). Every
label change starts a run, including the `risk/*` label the Policy workflow
adds (one extra run per pull request). The `workflow_dispatch` input
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

`tests/shard.sh` lists a package's tests with the same build tags the run
uses, applies the caller's `-skip` expression to the discovered names, and
runs every count-th one through `tests/ci`; a new test joins a shard
automatically. A shard that would run no test fails, and after a green run
the script checks that at least one `=== RUN` was executed, so a tag, flag or
exclusion mismatch cannot leave a green job with no coverage. `make
test-apps` skips the tests named in `APPS_E2E`; add a new Docker test to that
list (and `AGENT_E2E` for the agent) so it runs in the heavy tier.

The `core/internal/apps`, identity, profile, peopleimport, jobs and demomode
integration tests use `dbtest.Main` (`pgtest.RunWithTemplate`): the migrations
run once into a template database in `TestMain`, and every test still gets its
own fresh database (`CREATE DATABASE ... TEMPLATE`). Packages that test
migrations themselves use `pgtest.Main` without a template.
