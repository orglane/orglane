# Task tracker provider research

Research for [#30](https://github.com/orglane/orglane/issues/30), observed on 2026-11-03. This is a recommendation for the maintainer, not a provider selection or an implemented integration.

**Draft: incomplete experiments.** Runtime coverage is partial and the issue remains open. The candidate projects are stopped with volumes preserved while the shared host is reserved for feature verification; the remaining measurements and final cleanup must follow before this research is complete.

## Recommendation

Prioritize a **Vikunja Community** adapter proof of concept. It has free generic OIDC, an API for assigned tasks, project/team APIs and signed webhooks, with a small deployment. Its account lifecycle needs particular care: first-login OIDC provisioning is not pre-provisioning, and the Community CLI is not a public remote administration API. The agent must expose a fixed, audited lifecycle operation rather than arbitrary container commands (ADR 0006).

Keep **Leantime** as the alternative if its broader project-management workflow is preferred. Plane Community and OpenProject Community are weaker fits for the product's free SSO principle because their documented generic OIDC features require paid editions. Huly requires a larger host and separate lifecycle research. Provider dependencies are isolated managed-service dependencies; none are proposals to add Redis, a broker or a JVM to the Orglane core (ADR 0009).

## Integration comparison

The following is documentation/source evidence unless explicitly called a local test. No candidate completed an OIDC round trip against Orglane in this research; token expiry, logout, account linking, permission mapping and offboarding remain acceptance tests before catalog inclusion.

| Candidate | OIDC and accounts | Tasks, membership and events | Operational fit |
|---|---|---|---|
| Vikunja 2.7.1 | Free [OIDC discovery and claim mapping](https://vikunja.io/docs/openid/); accounts normally created on first login. Pre-created local and OIDC identities need explicit matching policy. Community [CLI](https://vikunja.io/docs/cli/) creates, updates, disables and deletes users; [Pro](https://vikunja.io/docs/pro/) adds instance administration. | Public REST API covers projects, teams and memberships. `GET /api/v1/tasks/all` with an assignee filter supports a Home reader; never treat all visible tasks as "my tasks". [Webhooks](https://vikunja.io/docs/webhooks/) include assignments and comments, HMAC-SHA256 signatures, and no failed-delivery retry. A dedicated mention event was not verified; parse comments only with permission checks. | One app plus PostgreSQL in the fixture; a one-shot volume ownership helper is counted separately. Native API and a straightforward Compose deployment are a good starting point. |
| Plane Community 1.4.3 | [Generic OIDC is marked Pro/Business](https://developers.plane.so/self-hosting/govern/oidc-sso). Community's provider-specific OAuth is not evidence of arbitrary OIDC support. Do not impersonate a supported OAuth vendor. | [Member APIs](https://developers.plane.so/api-reference/members/overview) cover workspace/project membership; membership removal is not instance suspension. [Workspace webhooks](https://developers.plane.so/dev-tools/intro-webhooks) cover work items/comments. Current docs describe v2; do not assume that schema matches 1.4.3 without a delivery test. An assigned-work reader must map user IDs and filter per permitted project. Remote instance user creation/suspension not verified. | Multiple frontends, API, workers, PostgreSQL, Valkey, RabbitMQ and object storage. More moving parts and migrations than Vikunja. |
| Leantime 3.13.2 | Free [OIDC implementation](https://github.com/Leantime/leantime/blob/v3.13.2/app/Domain/Oidc/Services/Oidc.php) supports discovery, JWKS and configurable first-login account creation. Round trip and existing-session revocation untested. | [JSON-RPC API](https://docs.leantime.io/api/usage) exposes annotated service methods, not every PHP method. [User service source](https://github.com/Leantime/leantime/blob/v3.13.2/app/Domain/Users/Services/Users.php) marks `addUser`, `editUser`, `patchUser`, `deleteUser` and `getAll` as API methods with permission checks. Projects/team mapping and an assignee-filtered todo reader need adapter-specific coverage tests. [Personal webhooks](https://github.com/Leantime/leantime/blob/v3.13.2/app/Domain/Notifications/Services/Webhooks.php) are recipient opt-in, queued notifications; not a verified instance-wide assignment/mention feed. Authenticated remote suspension and its session effects remain unverified. | PHP app plus MySQL; cannot share Orglane's PostgreSQL using this deployment. Broader goals, planning and time tracking may suit non-developers. |
| OpenProject 17.9.6 | [OpenID providers are an Enterprise add-on](https://www.openproject.org/docs/system-admin-guide/authentication/openid-providers/). | [API v3](https://www.openproject.org/docs/api/) has users, groups, memberships, projects and filtered work packages. Verify administrator permissions and lock/unlock behavior before promising offboarding. Work-package webhooks are documented under [API and webhooks](https://www.openproject.org/docs/system-admin-guide/api-and-webhooks/); mention notifications need a separate coverage test. | Official all-in-one fixture includes PostgreSQL and background processes inside one container. One image is not one process. |
| Huly | OIDC and administrator lifecycle were not verified locally. | Platform APIs/SDK are a separate adapter investigation; a stable REST "my tasks" contract, suspension and signed mention/assignment delivery were not verified. | [Upstream self-host guide](https://github.com/hcengineering/huly-selfhost) requires at least 8 GB RAM (16 GB recommended). Examined Compose requests 14 distinct images including CockroachDB, Redpanda, Elasticsearch and several platform services. Runtime experiment stopped before deployment because the shared VM had less free RAM than the documented minimum. No memory result is claimed. |

## Measurements and reproducibility

Host: Apple M1 Pro, Linux/arm64 containers in Docker Desktop; Engine 29.8.1, Compose 5.5.1, 10 VM CPUs, 8,214,851,584 bytes VM RAM. The VM was shared with unrelated workloads. These are small synthetic smoke measurements, not capacity guidance or controlled throughput comparisons. Image sizes are sums of Docker `.Size` bytes divided by 1,000,000, not compressed download sizes or unique shared-layer disk usage. Memory is the sum of the project's running containers as reported by `docker stats` (Linux cache adjustment), not host RSS. Stopped migration/ownership jobs are excluded from RAM and included in image counts.

| Candidate | Idle min / median / max MiB | Loaded peak MiB | Image count / MB | Result |
|---|---:|---:|---:|---|
| Vikunja + PostgreSQL | 148.4 / 149.4 / 150.5 | Unverified | 3 / 427.0 | Preserved completed idle run; fresh load seed interrupted before completion |
| Plane CE | Unverified | Unverified | Unverified | Containers started; migrations interrupted at `db.0072` |
| Leantime | Unverified | Unverified | Unverified | Image download/extraction interrupted before deployment |
| OpenProject | Unverified | Unverified | Unverified | Image download interrupted before deployment |
| Huly | Unverified | Unverified | 14 requested / size unverified | Compose configuration examined; download stopped before deployment |

The shared host became saturated during concurrent work. Experiments were suspended by the coordinator; incomplete pulls, initialization and workloads are not successful deployments. **The issue's full local measurement coverage is not yet complete.**

The preserved first run measured Vikunja after a 180-second quiet period: six samples ten seconds apart, **148.4 / 149.4 / 150.5 MiB min/median/max**, last sample app 83.9 MiB + database 66.6 MiB. An earlier run failed when the Docker disk filled and is excluded. A resumed load test with an expired login token produced only 401 responses and is also excluded; successful-request counts are required for the load results below.

Run from the repository root with Docker, curl, jq and ApacheBench installed. Fixtures contain disposable example credentials and loopback ports; they are research examples, not deployable Orglane packages. Use an unused Compose project name for each repeat.

```sh
docker compose -p rs-vikunja -f docs/research/env/vikunja/compose.yaml up -d --wait
# Wait until GET /api/v1/info succeeds, then seed a fresh database.
sh docs/research/env/vikunja/seed.sh > /tmp/vikunja-research.token
sh docs/research/env/measure.sh images rs-vikunja
sh docs/research/env/measure.sh mem rs-vikunja 6 10
sh docs/research/env/measure.sh peak rs-vikunja 35 > /tmp/vikunja-load.txt &
meter=$!
ab -k -c 10 -t 30 -n 1000000 \
  -H "Authorization: Bearer $(cat /tmp/vikunja-research.token)" \
  'http://127.0.0.1:18101/api/v1/tasks/all?page=1&per_page=50'
wait "$meter"
cat /tmp/vikunja-load.txt
```

Plane used the official Compose file at the pinned release, plus a loopback-only proxy override. The initial proxy startup failed because `CERT_ACME_CA` was empty; setting its upstream default fixed HTTP startup. `SITE_ADDRESS=:80` keeps this experiment on HTTP and no certificate issuance was attempted.

```sh
curl -fsSL https://raw.githubusercontent.com/makeplane/plane/v1.4.3/deployments/cli/community/docker-compose.yml \
  -o /tmp/plane-research-compose.yml
docker compose -p rs-plane -f /tmp/plane-research-compose.yml \
  -f docs/research/env/plane/local.yaml \
  --env-file docs/research/env/plane/plane.env up -d
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18102/
# Observed: 200 for the web frontend; not API/migration readiness.
docker logs rs-plane-migrator-1 --tail 5
```

Leantime and OpenProject deployment commands:

```sh
docker compose -p rs-leantime -f docs/research/env/leantime/compose.yaml up -d
docker compose -p rs-openproject -f docs/research/env/openproject/compose.yaml up -d
# Once initialization has completed, measure the appropriate project:
sh docs/research/env/measure.sh mem rs-leantime 6 2
sh docs/research/env/measure.sh images rs-leantime
```

The Huly configuration was fetched from self-host revision `ca3808a57c83518b971343df7d073753f38796ed`. The attempted command used:

```sh
curl -fsSL https://raw.githubusercontent.com/hcengineering/huly-selfhost/ca3808a57c83518b971343df7d073753f38796ed/compose.yml \
  -o /tmp/huly-research-compose.yml
curl -fsSL https://raw.githubusercontent.com/hcengineering/huly-selfhost/ca3808a57c83518b971343df7d073753f38796ed/.huly.nginx \
  -o /tmp/huly-research.nginx
export HULY_NGINX_PATH=/tmp/huly-research.nginx
docker compose --project-directory docs/research/env/huly -p rs-huly \
  -f /tmp/huly-research-compose.yml -f docs/research/env/huly/local.yaml \
  --env-file docs/research/env/huly/local.env up -d
```

This did not reach deployment. The pinned platform release was requested without claiming compatibility with the independently versioned self-host configuration.

## Upgrades, licenses and UI

No in-place upgrade or backup restore was executed. [Vikunja releases](https://github.com/go-vikunja/vikunja/releases) observed: 2.6.0 (August 31), 2.7.0 (October 2), 2.7.1 (October 20); take database/files snapshots before automatic schema migration. Plane 1.4.3 was released October 28; its fresh-install migrations were measured separately from steady state. Its package must pin every companion image and treat DB migrations as potentially irreversible. [Leantime releases](https://github.com/Leantime/leantime/releases) included 3.13.0/1 on October 30 and 3.13.2 on October 31; [OpenProject](https://github.com/opf/openproject/releases) had 17.9.4 on October 26 and 17.9.5/6 on October 30. Those are observed intervals, not promises of future cadence. Huly's platform release was 0.7.426 on July 5, while self-host Compose evolves independently: pin compatible platform/config revisions together.

Vikunja, Plane Community and Leantime core use AGPLv3; OpenProject uses GPLv3; Huly uses EPL-2.0 ([Vikunja](https://github.com/go-vikunja/vikunja/blob/v2.7.1/LICENSE), [Plane](https://github.com/makeplane/plane/blob/v1.4.3/LICENSE.txt), [Leantime](https://github.com/Leantime/leantime/blob/v3.13.2/LICENSE), [OpenProject](https://github.com/opf/openproject/blob/v17.9.6/COPYRIGHT), [Huly](https://github.com/hcengineering/platform/blob/v0.7.426/LICENSE)). This proposal communicates with separate services over APIs; it does not link their code into the AGPL core. Paid plugins, bundled dependencies, redistribution notices and Huly's secondary-license provisions require package-specific review; this is not a blanket license-compatibility determination.

UI quality is **not scored**: a frontend returning HTTP 200 is insufficient evidence of authenticated task workflows. The source/docs show different workflow breadth (Vikunja task views, Plane work items/cycles, Leantime goals/planning, OpenProject work packages, Huly collaboration), but hands-on create/assign/filter/mobile usability reviews remain unverified.

Before selecting a provider, complete Orglane OIDC login and logout, pre-provision a person without duplicate identities, add/remove team/project access, verify existing sessions and API tokens after suspension, capture signed assignment/mention events, and test a snapshot restore across an upgrade. Under ADR 0002, unsupported actions must appear as explicit capability limits. The maintainer chooses the provider after those gaps and workflow preferences are reviewed.

After the final run, remove only these experiment projects with their own fixture files (`docker compose -p <recorded-project> -f <fixture> down -v --remove-orphans`); Plane/Huly need the same upstream files, overrides and environment arguments used at startup. Verify no containers or volumes remain under those exact Compose project labels. No global Docker prune is part of this procedure.
