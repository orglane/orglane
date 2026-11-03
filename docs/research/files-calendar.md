# Files, calendar and contacts provider research

Research for [#31](https://github.com/orglane/orglane/issues/31), observed on 2026-11-03. Recommendations are provisional; the maintainer chooses providers.

**Draft: incomplete experiments.** Runtime coverage is partial and the issue remains open. Research containers are stopped with volumes preserved while feature verification uses the shared host. Remaining measurements, repository checks and final removal of task-owned containers/volumes are pending.

## Recommendation

Prioritize **Nextcloud** for an integrated files/calendar/contacts proof of concept. Its documented provisioning, sharing and DAV interfaces cover more of the v0.2 employee scenario in one service; this recommendation is not yet supported by a completed local Nextcloud run. Keep **OpenCloud + Radicale** as the modular alternative, conditional on proving account lifecycle, invitation scheduling and notifications end to end. OpenCloud documents a [Radicale integration](https://docs.opencloud.eu/docs/admin/configuration/radicale-integration/) that forwards authenticated DAV requests and offers app tokens for native clients; it is described as best-effort support. Its combined memory footprint and revocation behavior remain unverified.

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
| Nextcloud + PostgreSQL | Unverified | Unverified | Unverified | Pull/extraction interrupted before deployment |
| OpenCloud standalone | Unverified | Unverified | Unverified | Container started; Graph request did not reach readiness before suspension |
| Radicale standalone | 27.9 / 29.4 / 30.8 | Unverified | 1 / 128.1 | Six quiet samples two seconds apart; calendar creation/read metadata passed |
| Baikal image | Unverified | Unverified | Unverified | Pull/extraction interrupted before deployment |
| Seafile CE + MariaDB + Redis | Unverified | Unverified | Unverified | Pull interrupted before deployment |

The shared host became saturated during concurrent work and experiments were suspended by the coordinator. No application steady-state result is inferred from an image download or a started container. **The issue's full local measurement coverage is not yet complete.**

Run from the repository root. Fixtures use unique project names, disposable example credentials, named volumes and loopback ports. They are experiments, not production Orglane packages. Use a fresh project name/port for another run.

```sh
docker compose -p rs-nextcloud -f docs/research/env/nextcloud/compose.yaml up -d
docker compose -p rs-opencloud -f docs/research/env/opencloud/compose.yaml up -d
docker compose -p rs-radicale -f docs/research/env/radicale/compose.yaml up -d
docker compose -p rs-baikal -f docs/research/env/baikal/compose.yaml up -d
docker compose -p rs-seafile -f docs/research/env/seafile/compose.yaml up -d
# Run candidates separately and wait for completed installation/readiness.
sh docs/research/env/measure.sh mem rs-nextcloud 6 2
sh docs/research/env/measure.sh images rs-nextcloud
```

Radicale local protocol evidence: `MKCOL /alice/` returned 405 because the principal already existed, `MKCALENDAR /alice/calendar/` returned **201**, and `PROPFIND` with `Depth: 1` returned **207**. The subsequent combined events/contacts seed stopped at the existing-principal response; it was corrected to permit that response but has not yet been rerun. No successful CardDAV object or loaded run is claimed.

```sh
curl -u alice:research -X MKCALENDAR -H 'Content-Type: application/xml' \
  --data '<C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:set><D:prop><D:displayname>Research</D:displayname></D:prop></D:set></C:mkcalendar>' \
  -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18105/alice/calendar/
curl -u alice:research -X PROPFIND -H 'Depth: 1' \
  -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18105/alice/calendar/
```

The checked-in `dav-smoke.py` is a follow-up reproducibility aid for 20 events and 20 contacts; it is not evidence that those objects were successfully tested in this run. Baikal's mutable `apache-php8.2` tag was resolved to a digest in the fixture; application version must be checked after installation, rather than inferred from its 2026-11-01 image timestamp.

## Upgrade and licensing implications

No upgrade/restore was performed. [Nextcloud upgrades](https://docs.nextcloud.com/server/stable/admin_manual/maintenance/upgrade.html) require the latest patch of the current major, then each successive major; major versions cannot be skipped, background migrations must finish, apps need compatible releases, and downgrade requires restoring a backup. Snapshot the DB, data, configuration and apps together. Nextcloud server 35.0.2, 34.0.5 and 33.0.10 were published October 15: overlapping maintenance lines add package testing work.

[OpenCloud](https://github.com/opencloud-eu/opencloud/releases) released 8.0.1 September 16, 8.1.0 October 5 and 9.0.0 October 27; pin server, web and configuration compatibility, and back up both config and data. [Radicale](https://github.com/Kozea/Radicale/releases) released 3.8.2 October 6, 3.8.3 October 8 and 3.9.0 October 25; retain collections, auth and rights configuration, and test storage-format changes. [Baikal](https://sabre.io/baikal/upgrade/) upgrades replace application files while preserving configuration/data and may upgrade schema. Upstream 0.13.0 was released October 19; the third-party image's publication date is not the application's release date. [Seafile 13→14](https://manual.seafile.com/14.0/upgrade/upgrade_docker_14.0/) changes Compose/environment configuration, so replacing an image tag alone is not a complete upgrade plan. Keep its databases, library data and config in the same recovery point. These observed release dates do not establish future cadence.

Nextcloud server is AGPLv3; OpenCloud server is Apache-2.0; Radicale and Baikal are GPLv3. Seafile has component-specific licensing: server GPLv2, Seahub Apache-2.0, with separate client/proprietary edition terms ([Nextcloud](https://github.com/nextcloud/server/blob/v35.0.2/COPYING), [OpenCloud](https://github.com/opencloud-eu/opencloud/blob/v9.0.0/LICENSE), [Radicale](https://github.com/Kozea/Radicale/blob/v3.9.0/COPYING.md), [Baikal](https://github.com/sabre-io/Baikal/blob/0.13.0/LICENSE), [Seafile](https://github.com/haiwen/seafile/blob/master/LICENSE.txt), [Seahub](https://github.com/haiwen/seahub/blob/master/LICENSE.txt)). API integration with separately deployed services is the proposed boundary; do not copy GPLv2-only Seafile server code into Orglane's AGPLv3 core. Container redistribution, plugins and clients need their own notices/license review under ADR 0016.

## Google Calendar as a connected provider

Use per-person OAuth consent/account linking and `events.list` for upcoming meetings; request the narrowest scopes needed. [Push channels](https://developers.google.com/workspace/calendar/api/guides/push) require a reachable HTTPS receiver, expire and need renewal. Notifications signal resource changes rather than carrying the full event; fetch changes using an incremental [sync token](https://developers.google.com/workspace/calendar/api/guides/sync), and rebuild state on invalidation (`410 Gone`). Keep deleted events and cancellations in the sync model, validate channel/resource identifiers, recheck access at delivery, and show revocation/reauthorization as an employee step. Calendar API access is not Workspace user provisioning; hiring/suspension would require a separately authorized directory integration. No Google account or paid service was accessed for this research.

## Decision gates

Prove Orglane SSO and native-client authentication independently; create users/groups before first login; create a share and a meeting invitation and observe both recipients' events; revoke already-issued sync credentials on offboarding; then execute an upgrade and restore with pinned images/apps. The integrated Nextcloud recommendation favors coverage and operational cohesion; OpenCloud + Radicale remains viable only if its identity and scheduling gaps fit the required employee scenario.

After the last run, use each recorded project's own fixture: `docker compose -p <recorded-project> -f docs/research/env/<candidate>/compose.yaml down -v --remove-orphans`. Verify no containers or volumes remain under those exact Compose project labels. Unrelated Docker resources must remain untouched.
