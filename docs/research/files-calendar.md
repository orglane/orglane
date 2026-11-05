# Files, calendar and contacts provider research

Research for [#31](https://github.com/orglane/orglane/issues/31), observed on 2026-11-03 with bounded Radicale and OpenCloud follow-ups on 2026-11-04. Recommendations are provisional; the maintainer chooses providers.

**Draft: incomplete experiments.** Runtime coverage is partial and the issue remains open. Research is paused at the user's request after the current OpenCloud observation; containers are stopped with volumes preserved. Remaining measurements, repository checks and final removal of task-owned containers/volumes are pending.

## Recommendation

Prioritize **Nextcloud** for an integrated files/calendar/contacts proof of concept. Its documented provisioning, sharing and DAV interfaces cover more of the v0.2 employee scenario in one service. A bounded local run verified provisioning and file/share basics; the calendar/contacts workflow and identity integration remain unverified. Keep **OpenCloud + Radicale** as the modular alternative, conditional on proving account lifecycle, invitation scheduling and notifications end to end. OpenCloud documents a [Radicale integration](https://docs.opencloud.eu/docs/admin/configuration/radicale-integration/) that forwards authenticated DAV requests and offers app tokens for native clients; it is described as best-effort support. Its combined memory footprint and revocation behavior remain unverified.

Seafile is a files-specific alternative, not a calendar or contacts service. Baikal offers a DAV administration UI but needs additional identity/event integration. An external Google Calendar binding is independent of the managed files choice (ADR 0002).

## Capability comparison

Documentation claims below were checked against official sources; local results are identified separately. OIDC round trips against Orglane, real desktop/mobile clients, invitation email delivery, webhook receipt, upgrades and restore were **not** verified in this run.

| Candidate | Identity and lifecycle | Files, events and sync | Calendar/contacts and operations |
|---|---|---|---|
| Nextcloud 35.0.2 | [user_oidc](https://github.com/nextcloud/user_oidc) supplies external OIDC/JIT provisioning and configurable mapping. [OCS provisioning](https://docs.nextcloud.com/server/stable/admin_manual/configuration_user/user_provisioning_api.html) covers users, groups, quotas and enable/disable. Existing app-password/session revocation still needs a test; disabling Orglane login alone is insufficient. | [Share API](https://docs.nextcloud.com/server/stable/developer_manual/client_apis/OCS/ocs-share-api.html), WebDAV, official desktop and mobile sync clients. [Webhook Listeners](https://docs.nextcloud.com/server/stable/admin_manual/webhook_listeners/index.html) can forward internal events via registered listeners; share/calendar event class coverage and background delivery must be tested for pinned apps. | DAV backend is built in; Calendar/Contacts apps add browser interfaces. [Native clients](https://docs.nextcloud.com/server/stable/user_manual/en/groupware/index.html) use CalDAV/CardDAV, commonly with app passwords. PostgreSQL works; production locking/cache and cron configuration are additional package work. |
| OpenCloud 9.0.0 | [External OIDC](https://docs.opencloud.eu/docs/admin/configuration/authentication-and-user-management/external-idp/) requires public clients, authorization code + PKCE and suitable claims. It retains an identity directory; first-login provisioning is configurable. [Graph users](https://docs.opencloud.eu/docs/dev/server/apis/http/graph/users/) and [groups](https://docs.opencloud.eu/docs/dev/server/apis/http/graph/groups/) cover management; suspension and token/session consequences need pinned-version tests. | Graph spaces/permissions, WebDAV and official Web/Desktop/Android/iOS clients. Internal event infrastructure is not evidence of a stable outbound "share created" webhook; no delivered event was verified. | No built-in general calendar/contacts server in the tested standalone fixture. Add a DAV provider; do not double-count its RAM as included here. External-IdP docs served version 7.2 during this check, so compatibility with 9.0.0 requires verification. |
| Radicale 3.9.0 | [Authentication](https://radicale.org/v3.html#auth) supports file/directory/proxy approaches; the fixture disables auth on loopback. Browser OIDC redirects do not authenticate native DAV clients. No Orglane-compatible remote user/group/suspension administration API was verified. | DAV data and access rules, rather than file sync/shares. Change hooks/plugins can be integrated, but no signed share/invite event feed was verified. | CalDAV/CardDAV with filesystem storage; native DAV clients are the UI. Its [protocol limits](https://radicale.org/v3.html#about) matter: do not equate event storage with full meeting scheduling or email invitations. |
| Baikal | Basic/Digest DAV plus a web admin UI; generic OIDC and automated lifecycle APIs not verified. | No general file sync; no verified notification webhook contract. | [Official overview](https://sabre.io/baikal/) documents CalDAV/CardDAV, users/address books/calendars and SQL databases. The [Docker image](https://sabre.io/baikal/docker-install/) is explicitly unofficial. Tested image version and initialized state are recorded below; do not infer current upstream release support from a mutable image tag. |
| Seafile CE 14.0.9 | [Configurable OAuth2](https://manual.seafile.com/14.0/config/oauth/) is documented; this alone does not prove discovery, ID-token validation or Orglane compatibility. [Web API](https://manual.seafile.com/14.0/develop/web_api_v2.1/) covers account/group/library/share operations; exact CE suspension/session semantics unverified. | Library-oriented sync, desktop sync/SeaDrive and mobile clients. [Notification Server](https://manual.seafile.com/14.0/extension/notification-server/) is an optional client-update extension, not a proven Orglane share-event webhook. | No native CalDAV/CardDAV calendar/contacts server. Requires MySQL/MariaDB plus cache. The fixture omits SeaDoc, Caddy, AI and notification extensions, so it is not the default full-stack footprint. |

## Local measurements

Same shared host/method as [tasks](tasks.md#measurements-and-reproducibility): Linux/arm64, Docker Engine 29.8.1, Compose 5.5.1, 10 VM CPUs, 8,214,851,584 bytes VM RAM. RAM is summed `docker stats` cache-adjusted usage in MiB; image size is summed uncompressed `.Size` in decimal MB, counting distinct references, not unique disk usage. Small synthetic workloads do not establish production capacity or comparative performance.

| Candidate | Idle min / median / max MiB | Loaded peak MiB | Images / MB | Local result |
|---|---:|---:|---:|---|
| Nextcloud + PostgreSQL | Unverified | Unverified | 2 / 1796.2 | 35.0.2 readiness, OCS provisioning/membership/sharing and file round trip verified; benchmarks/DAV object tests deferred on host pressure |
| OpenCloud standalone | 401.3 / 401.3 / 401.6 (3 post-readiness samples) | Unverified | 1 / 223.6 | Version 9.0.0 and Basic-auth Graph `/me` 200 verified; pressure abort before provisioning/file/share smoke |
| Radicale standalone | 32.4 / 32.4 / 32.4 (3 pre-batch samples) | Unverified | 1 / 128.1 | 20 events + 20 contacts verified; configured 20-second sampler maximum 37.5 MiB is not an active-batch peak |
| Baikal image | Unverified | Unverified | Unverified | Pull/extraction interrupted before deployment |
| Seafile CE + MariaDB + Redis | Unverified | Unverified | Unverified | Pull interrupted before deployment |

The shared host became saturated during concurrent work and experiments were suspended by the coordinator. Nextcloud, Radicale and OpenCloud were subsequently released separately for bounded follow-ups, described below. No application steady-state result is inferred from an image download, started container or single memory snapshot. **The issue's full local measurement coverage is not yet complete.**

Run from the repository root. Fixtures use unique project names, disposable example credentials, named volumes and loopback ports. They are experiments, not production Orglane packages. Use a fresh project name/port for another run.

```sh
docker compose -p rs-nextcloud -f docs/research/env/nextcloud/compose.yaml \
  -f docs/research/env/nextcloud/bounded.yaml up -d --pull never --no-build
docker compose -p rs-opencloud -f docs/research/env/opencloud/compose.yaml \
  -f docs/research/env/opencloud/bounded.yaml up -d --pull never --no-build
docker compose -p rs-radicale -f docs/research/env/radicale/compose.yaml up -d
docker compose -p rs-baikal -f docs/research/env/baikal/compose.yaml up -d
docker compose -p rs-seafile -f docs/research/env/seafile/compose.yaml up -d
# Run candidates separately and wait for completed installation/readiness.
sh docs/research/env/measure.sh mem rs-nextcloud 6 2
sh docs/research/env/measure.sh images rs-nextcloud
```

### Nextcloud bounded follow-up

Only the recorded `rs-nextcloud` project was resumed; its incomplete app image pull was completed without other candidates/downloads. The arm64 image was `nextcloud:35.0.2-apache`, resolved to `nextcloud@sha256:e0b1a2af52c82c07b7a0e8e59878bd8769e704447734d5d238a836982f51b9c8` (1,498,215,290 uncompressed bytes). With the cached PostgreSQL image, the sum was **2 images / 1796.2 MB**.

The override limited the app to **2 CPUs / 1 GiB**, PostgreSQL to **1 CPU / 512 MiB**. Inspection confirmed both limits, zero restarts and `OOMKilled=false` at the time checked. `/status.php` reported **installed=true, maintenance=false, needsDbUpgrade=false, versionstring=35.0.2, internal version=35.0.2.2**. A readiness poll completed in 32.598 seconds after polling began; that is not installation time from image pull or a cold-start benchmark.

Local observations (disposable users and content):

| Operation | Observed result |
|---|---|
| Create user and group via OCS | HTTP 200 with successful OCS metadata |
| Add membership and read it back | HTTP 200 with successful OCS metadata; expected group present |
| Upload file via WebDAV | HTTP 201 |
| Read the file | HTTP 200; all **1,048,576 bytes** matched the generated payload |
| Create/read a file share to the admin | HTTP 200 with successful OCS metadata; expected recipient verified |
| Disable the user via OCS | HTTP 200 with successful OCS metadata |
| Immediate Basic-auth file GET while disabled | **HTTP 503**, not successful file access; response body/error semantics were not retained |
| Reenable in the script's cleanup block | HTTP 200 with successful OCS metadata; post-enable file read was not reached |

The original research script expected only 401/403 for the disabled-account read and stopped on its assertion. The expected-status assumption was corrected, but the helper has **not been rerun**. Pinned source provides context: [`Session::completeLogin`](https://github.com/nextcloud/server/blob/v35.0.2/lib/private/User/Session.php) rejects disabled users, and the [DAV auth adapter](https://github.com/nextcloud/server/blob/v35.0.2/apps/dav/lib/Connector/Sabre/Auth.php) maps other authentication exceptions to `ServiceUnavailable`. This can explain a 503, but the status alone does not identify the observed exception, demonstrate an access bypass or establish credential/session revocation. App passwords, browser sessions and OIDC remain untested.

The app list showed enabled **dav 2.0.0** and **files_sharing 2.0.0**; **Calendar, Contacts and user_oidc were absent**. No Calendar/Contacts app provisioning, calendar/contact object round trip, scheduling, invitation email, native-client sync or OIDC was verified. Bundled DAV being enabled does not establish a complete calendar/contacts product workflow.

Host one-minute load increased from **5.47** before the resume to **18.57** during the sequential checks and **20.78** by the end. No synthetic load or DAV seed was started, and idle/loaded measurements were deferred. One post-check `docker stats` snapshot showed app **348.3 MiB** and database **44.71 MiB** (about **393.0 MiB** total), with CPU readings 0.01%/0.14%; it is **not an idle series or a load peak**, and the host pressure is not attributed to this candidate. The task-owned stack was stopped and its volumes preserved.

Actual functional-check commands, after readiness:

```sh
curl -fsS http://127.0.0.1:18103/status.php \
  | jq '{installed,maintenance,needsDbUpgrade,versionstring,version}'
# Fresh disposable database only: creates a user/group/file/share.
python3 docs/research/env/nextcloud/basic-smoke.py
# Original run stopped at the disabled-read status assertion; see observations above.
docker exec --user www-data rs-nextcloud-app-1 php occ app:list --output=json \
  | jq '{dav:.enabled.dav,files_sharing:.enabled.files_sharing,calendar:.enabled.calendar,contacts:.enabled.contacts,user_oidc:.enabled.user_oidc}'
sh docs/research/env/measure.sh images rs-nextcloud
docker compose -p rs-nextcloud -f docs/research/env/nextcloud/compose.yaml \
  -f docs/research/env/nextcloud/bounded.yaml stop
```

### OpenCloud standalone follow-up (2026-11-04)

Only the existing `rs-opencloud` project ran, reusing its config/data volumes and cached arm64 image `opencloudeu/opencloud-rolling:9.0.0@sha256:bd57f039d0dd3df510fb25f178beefdf0b1f049097285feead607824460e6506`. No pull, build, reset, forced initialization or authentication reconfiguration was performed. The override set **1 CPU / 768 MiB**; inspection confirmed the limits, **zero restarts** and **OOMKilled=false**. The single image was **223,623,696 uncompressed bytes** (223.6 MB); this footprint is standalone OpenCloud only.

`opencloud version` reported **9.0.0**, edition **rolling**, compiled October 27. With the fixture's existing `PROXY_ENABLE_BASIC_AUTH=true` and disposable admin credentials, **GET `/graph/v1.0/me` returned HTTP 200 with a user ID**. The first readiness probe succeeded in **2.904 seconds after polling began**, with no preceding failed probes; this is not cold-start or installation time. Earlier non-readiness remains an interrupted observation, not an established provider defect.

The readiness watcher allowed at most five minutes, used a three-second HTTP timeout and serial requests, and aborted if one-minute host load exceeded the lower of 10 and its initial value + 3 (**7.95** in this phase), or VM memory PSI `avg10` exceeded 0.1. The planned write smoke had a two-minute limit but was **not started**: after three quiet samples, host load reached **9.68**, above the guard. No user/group/member round trip, file upload, share or synthetic load was attempted. Those steps remain unverified, rather than failed API operations.

The post-readiness samples with two-second sleeps between Docker-stat calls were **401.3 / 401.3 / 401.6 MiB min/median/max**. This brief quiet window is not a long-settled idle baseline, active workload measurement or throughput result. Host one-minute load was **4.51** before startup, **4.95** at readiness and **9.68** after sampling, declining to **8.19** before stopping; no cause is attributed. VM available memory changed from **6,507,472 KiB** after startup to **6,313,052 KiB** at the end, with memory PSI `some/full avg10=0.00` at both checks.

The container was stopped, both `rs-opencloud_config` and `rs-opencloud_data` were preserved, and the runtime slot was returned. No other candidate or combined OpenCloud/Radicale setup was started. The user then requested a pause after recording these results; the research remains incomplete.

Recorded startup/version/sample/stop commands, with an equivalent direct readiness probe:

```sh
docker compose -p rs-opencloud -f docs/research/env/opencloud/compose.yaml \
  -f docs/research/env/opencloud/bounded.yaml up -d --pull never --no-build
docker exec rs-opencloud-app-1 opencloud version
# Actual readiness used a bounded Python watcher; this probes the same endpoint.
# -k applies only to the fixture's local self-signed TLS certificate.
curl -ksS --max-time 3 -u admin:research-pass-1 https://localhost:18104/graph/v1.0/me \
  | jq '{id_present:has("id")}'
sh docs/research/env/measure.sh mem rs-opencloud 3 2
sh docs/research/env/measure.sh images rs-opencloud
docker compose -p rs-opencloud -f docs/research/env/opencloud/compose.yaml \
  -f docs/research/env/opencloud/bounded.yaml stop
```

[Official Docker guidance](https://docs.opencloud.eu/docs/admin/getting-started/container/docker/) supplies standalone setup context. The [v9.0.0 proxy defaults](https://github.com/opencloud-eu/opencloud/blob/v9.0.0/services/proxy/pkg/config/defaults/defaultconfig.go) disable Basic auth by default; this fixture explicitly enables it for testing, consistent with the [developer WebDAV guidance](https://docs.opencloud.eu/docs/dev/server/apis/http/webdav/). Official [Graph users](https://docs.opencloud.eu/docs/dev/server/apis/http/graph/users/) and [groups](https://docs.opencloud.eu/docs/dev/server/apis/http/graph/groups/) docs describe provisioning/membership, but the served docs identify version 7.2 and those operations were not exercised against this 9.0.0 fixture. The authenticated `/me` read establishes no browser SSO, revocation, webhook, upgrade, UI or full file/calendar/contact workflow.

### Radicale bounded follow-up (2026-11-04)

The existing `rs-radicale` project and its data volume were preserved. Only that project was resumed, with the **cached** arm64 image pinned to `tomsquest/docker-radicale:3.9.0.0@sha256:2f215d0e6a78ff68c280975af6ce580b05976f70c62b51640397e3eb5a7aa0e9`, **0.5 CPU / 256 MiB** limits, `--pull never --no-build`, and loopback port 18105. The image's uncompressed size was **128,107,228 bytes** (1 image / 128.1 MB). `/.web/` returned **200** as a liveness probe. The initial `docker exec rs-radicale-app-1 radicale --version` failed because the executable was absent from `PATH`; inspection identified `/venv/bin/radicale`, which returned **3.9.0**.

A separate disposable principal, `research-20261104-b1`, avoided overwriting earlier `alice` data. `PROPFIND` of the principal returned **207**, then `MKCOL` returned **405**. The existing `dav-smoke.py` correction was exercised: it accepted 405 only for an existing principal, then required successful creation of the calendar/address-book collections. The helper exited **0**; an independent loop verified every object UID and collection count.

| Operation | Actual statuses/counts |
|---|---|
| Helper principal `MKCOL` | 1 × 405 (existing principal) |
| Helper `MKCALENDAR` and address-book `MKCOL` | 1 × 201 each |
| Helper object uploads | 40 × PUT 201: 20 events + 20 contacts |
| Helper first-object reads | 2 × GET 200, with event/card markers asserted |
| Independent UID-checked readback | 40 × GET 200: every event and contact |
| Independent collection listings | 2 × PROPFIND 207; exactly 20 objects per collection (21 DAV responses including the collection itself) |

The helper plus independent verification took **0.918 seconds**. Execution was bounded by a **30-second helper deadline**, **45-second overall work deadline**, one request at a time, and host-load abort limit **7.04** (the lower of 10 and initial one-minute load + 3). Host load stayed at **4.04** during the active batch. With `auth=none`, the Basic header selected a test principal; no password authentication, OIDC, revocation, invitation, notification, native-client, upgrade or UI behavior was established.

Three pre-batch `docker stats` samples with two-second sleeps between samples were **32.4 / 32.4 / 32.4 MiB min/median/max**. The earlier six-sample median of 29.4 MiB remains historical evidence from another state. A sampler configured for **20 seconds**, started alongside the batch with two-second sleeps between Docker-stat samples, observed **37.5 MiB maximum**. Because the active batch lasted under a second, the sample timing **cannot establish its active or instantaneous peak**; the window included the quiet tail. Sampler wall-clock duration was not separately instrumented, and no sustained/concurrent load benchmark was run.

Before startup, host one-minute load was **3.52**, with **39% free host memory**. The 10-CPU VM had 8,214,851,584 bytes RAM and reported **6,560,316 KiB available** after startup, with memory PSI `some/full avg10=0.00`. After the sampling tail, host load was **17.11**, VM available memory **6,573,880 KiB**, and memory PSI still zero; the load increase is not attributed to Radicale. No further workload was started. Limits remained intact, with **zero restarts / OOMKilled=false**. The owned container was stopped; volume `rs-radicale_data` was preserved. No other candidate, image pull, build or unrelated resource was touched.

Recorded startup/principal/helper commands (choose a new disposable principal for another run):

```sh
docker compose -p rs-radicale -f docs/research/env/radicale/compose.yaml \
  -f docs/research/env/radicale/bounded.yaml up -d --pull never --no-build
curl -sS --max-time 5 -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18105/.web/
docker exec rs-radicale-app-1 /venv/bin/radicale --version
sh docs/research/env/measure.sh mem rs-radicale 3 2
curl -u research-20261104-b1:research -X PROPFIND -H 'Depth: 0' \
  -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18105/research-20261104-b1/
curl -u research-20261104-b1:research -X MKCOL \
  -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18105/research-20261104-b1/
# Actual helper ran under the deadlines/pressure watcher described above.
sh docs/research/env/measure.sh peak rs-radicale 20 > /tmp/radicale-window.txt &
meter=$!
python3 docs/research/env/dav-smoke.py http://127.0.0.1:18105 \
  --user research-20261104-b1 --password research
```

Independent readback logic used in the same bounded driver:

```sh
python3 - <<'PY'
import base64, collections, urllib.request, xml.etree.ElementTree as ET
principal = 'research-20261104-b1'
base = 'http://127.0.0.1:18105/' + principal
headers = {'Authorization': 'Basic ' + base64.b64encode((principal + ':research').encode()).decode()}
counts = collections.Counter()
for index in range(20):
    for collection, extension, uid in [('calendar', 'ics', f'UID:research-{index}@example.com'),
                                       ('contacts', 'vcf', f'UID:research-{index}')]:
        req = urllib.request.Request(f'{base}/{collection}/{index}.{extension}', headers=headers)
        with urllib.request.urlopen(req, timeout=5) as response:
            assert response.status == 200
            assert uid.encode() in response.read().splitlines()
            counts['GET 200'] += 1
for collection in ['calendar', 'contacts']:
    req = urllib.request.Request(f'{base}/{collection}/', method='PROPFIND',
                                 headers={**headers, 'Depth': '1'})
    with urllib.request.urlopen(req, timeout=5) as response:
        assert response.status == 207
        assert len(ET.fromstring(response.read()).findall('{DAV:}response')) == 21
        counts['PROPFIND 207'] += 1
print(dict(counts))
PY
wait "$meter"
cat /tmp/radicale-window.txt
docker compose -p rs-radicale -f docs/research/env/radicale/compose.yaml \
  -f docs/research/env/radicale/bounded.yaml stop
```

The first Radicale run's principal 405, calendar creation 201 and metadata 207 are retained as earlier observations; its combined-helper failure is now followed by the successful bounded rerun. `dav-smoke.py` has still **not** completed for Nextcloud. Baikal's mutable `apache-php8.2` tag was resolved to a digest in its fixture; application version remains unverified rather than inferred from the 2026-11-01 image timestamp.

## Upgrade and licensing implications

No upgrade/restore was performed. [Nextcloud upgrades](https://docs.nextcloud.com/server/stable/admin_manual/maintenance/upgrade.html) require the latest patch of the current major, then each successive major; major versions cannot be skipped, background migrations must finish, apps need compatible releases, and downgrade requires restoring a backup. Snapshot the DB, data, configuration and apps together. Nextcloud server 35.0.2, 34.0.5 and 33.0.10 were published October 15: overlapping maintenance lines add package testing work.

[OpenCloud](https://github.com/opencloud-eu/opencloud/releases) released 8.0.1 September 16, 8.1.0 October 5 and 9.0.0 October 27; pin server, web and configuration compatibility, and back up both config and data. [Radicale](https://github.com/Kozea/Radicale/releases) released 3.8.2 October 6, 3.8.3 October 8 and 3.9.0 October 25; retain collections, auth and rights configuration, and test storage-format changes. [Baikal](https://sabre.io/baikal/upgrade/) upgrades replace application files while preserving configuration/data and may upgrade schema. Upstream 0.13.0 was released October 19; the third-party image's publication date is not the application's release date. [Seafile 13→14](https://manual.seafile.com/14.0/upgrade/upgrade_docker_14.0/) changes Compose/environment configuration, so replacing an image tag alone is not a complete upgrade plan. Keep its databases, library data and config in the same recovery point. These observed release dates do not establish future cadence.

Nextcloud server is AGPLv3; OpenCloud server is Apache-2.0; Radicale and Baikal are GPLv3. Seafile has component-specific licensing: server GPLv2, Seahub Apache-2.0, with separate client/proprietary edition terms ([Nextcloud](https://github.com/nextcloud/server/blob/v35.0.2/COPYING), [OpenCloud](https://github.com/opencloud-eu/opencloud/blob/v9.0.0/LICENSE), [Radicale](https://github.com/Kozea/Radicale/blob/v3.9.0/COPYING.md), [Baikal](https://github.com/sabre-io/Baikal/blob/0.13.0/LICENSE), [Seafile](https://github.com/haiwen/seafile/blob/master/LICENSE.txt), [Seahub](https://github.com/haiwen/seahub/blob/master/LICENSE.txt)). API integration with separately deployed services is the proposed boundary; do not copy GPLv2-only Seafile server code into Orglane's AGPLv3 core. Container redistribution, plugins and clients need their own notices/license review under ADR 0016.

## Google Calendar as a connected provider

Use per-person OAuth consent/account linking and `events.list` for upcoming meetings; request the narrowest scopes needed. [Push channels](https://developers.google.com/workspace/calendar/api/guides/push) require a reachable HTTPS receiver, expire and need renewal. Notifications signal resource changes rather than carrying the full event; fetch changes using an incremental [sync token](https://developers.google.com/workspace/calendar/api/guides/sync), and rebuild state on invalidation (`410 Gone`). Keep deleted events and cancellations in the sync model, validate channel/resource identifiers, recheck access at delivery, and show revocation/reauthorization as an employee step. Calendar API access is not Workspace user provisioning; hiring/suspension would require a separately authorized directory integration. No Google account or paid service was accessed for this research.

## Decision gates

Prove Orglane SSO and native-client authentication independently; create users/groups before first login; create a share and a meeting invitation and observe both recipients' events; revoke already-issued sync credentials on offboarding; then execute an upgrade and restore with pinned images/apps. The integrated Nextcloud recommendation favors coverage and operational cohesion; OpenCloud + Radicale remains viable only if its identity and scheduling gaps fit the required employee scenario.

After the last run, use each recorded project's own fixture: `docker compose -p <recorded-project> -f docs/research/env/<candidate>/compose.yaml down -v --remove-orphans`. Verify no containers or volumes remain under those exact Compose project labels. Unrelated Docker resources must remain untouched.
