# Agent and managed packages (M2 contracts, #57)

This spec defines `orglane agent`: where it runs, how the core talks to it, the declarative operations, desired state and reconciliation, the managed package format, networking, per-service databases, snapshots and upgrades with rollback, and how it is tested. Decisions and alternatives are recorded in [ADR 0019](../adr/0019-agent-transport-and-packages.md); the core side of providers (instances, credentials, provisioning) is in [providers.md](providers.md).

It refines [ADR 0006](../adr/0006-agent-and-desired-state.md) and [ADR 0007](../adr/0007-distribution-upgrades-rollback.md) and stays within [ADR 0008](../adr/0008-ingress-domains-tunnels.md), [ADR 0009](../adr/0009-resource-philosophy.md) and [ADR 0018](../adr/0018-domain-model.md).

## Process model

```
                         ┌──────────────── host ────────────────────────────────────────────┐
 internet ─► traefik ─edge─► orglane (server, uid 65532, no Docker socket)                  │
                │                │  unix socket (volume agent-run)                          │
                │                ▼                                                          │
                │           agent (root, network_mode: none) ──► /var/run/docker.sock       │
                │                │ writes ${ORGLANE_DATA_DIR}/instances/<key>/ (same path)   │
                │                ▼                                                          │
                └─edge──► managed services (Compose project orglane-<key>) ─services─► postgres
                         └──────────────────────────────────────────────────────────────────┘
```

### Compose bundle changes (#59)

```yaml
services:
  agent:
    image: ${ORGLANE_AGENT_IMAGE:-ghcr.io/orglane/orglane-agent:latest}
    profiles: [managed]
    restart: unless-stopped
    command: ["agent"]
    user: "0:0"
    network_mode: none
    environment:
      ORGLANE_AGENT_LISTEN: /run/orglane-agent/agent.sock
      # Not ":?": a required variable would break existing .env files even with
      # the profile off. The agent itself refuses to start without a secret.
      ORGLANE_AGENT_SECRET: ${ORGLANE_AGENT_SECRET:-}
      ORGLANE_AGENT_DATA_DIR: ${ORGLANE_DATA_DIR:-/var/lib/orglane}
      ORGLANE_AGENT_PEER_UID: "65532"
      ORGLANE_AGENT_TRAEFIK_ADDRESS: ${ORGLANE_EDGE_NETWORK_PREFIX:-10.231.0}.2
      DOCKER_CONFIG: /tmp/docker
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      # Same path inside and outside: the Docker daemon resolves bind mounts of
      # generated projects on the host.
      - ${ORGLANE_DATA_DIR:-/var/lib/orglane}/instances:${ORGLANE_DATA_DIR:-/var/lib/orglane}/instances
      - agent-run:/run/orglane-agent
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    cap_add: [CHOWN, DAC_OVERRIDE, FOWNER]   # data directories owned by service uids
    security_opt: ["no-new-privileges:true"]
    healthcheck:
      test: ["CMD", "/usr/local/bin/orglane", "agent", "healthcheck"]
    logging: *logging

  orglane:
    environment:
      ORGLANE_AGENT_SOCKET: /run/orglane-agent/agent.sock
      ORGLANE_AGENT_SECRET: ${ORGLANE_AGENT_SECRET:-}   # empty: managed services disabled
    volumes:
      - agent-run:/run/orglane-agent

  postgres:
    networks:
      database: {}
      services: {}

networks:
  # Managed services <-> PostgreSQL. The core is not on it.
  services:
    internal: true

volumes:
  agent-run: {}
```

- **Opt-in:** the `agent` service only starts with `COMPOSE_PROFILES=managed`. Without it the core runs unchanged and `GET /api/v1/apps/agent` reports `not_connected`; Settings → Apps shows the command that enables managed services.
- **Installer (#59):** always generates `ORGLANE_AGENT_SECRET` (64 hex characters) into `.env`, so enabling the agent later needs no core restart. New option `--managed` (and an interactive question) writes `COMPOSE_PROFILES=managed`; new subcommand `enable-managed` does the same on an existing install. It creates `instances/` (mode 700). `uninstall` first runs `docker compose down` for every project under `instances/*/compose.yaml`, then the bundle. `status` shows the agent.
- **Image (#59):** a second target of the root `Dockerfile`, `ghcr.io/orglane/orglane-agent`: distroless static base, the `orglane` binary, the static Docker CLI and the Compose plugin, both pinned by version and SHA-256. Released together with the core image by `images.yml`, same tag.
- **Hardening of the core's internal listener (#59):** since managed services join the edge network, the internal listener (`/traefik/config`) answers only requests from Traefik's fixed address.

### Configuration

Core (`core/internal/config`, section `agent`):

| Key | Env | Default | Meaning |
|---|---|---|---|
| `agent.socket` | `ORGLANE_AGENT_SOCKET` | empty | Agent socket path. Empty, or an empty secret, disables managed services |
| `agent.secret` | `ORGLANE_AGENT_SECRET` | empty | Shared secret, at least 32 characters |
| `agent.sync_interval` | `ORGLANE_AGENT_SYNC_INTERVAL` | `5m` | Status sync and re-push of lagging desired state |
| `ingress.maintenance_delay` | `ORGLANE_INGRESS_MAINTENANCE_DELAY` | `15s` | How long to wait for Traefik to pick up a maintenance route (poll interval + margin) |

Agent (`orglane agent`; it does not require `database.url` or `domain.base`):

| Key | Env | Default |
|---|---|---|
| `agent.listen` | `ORGLANE_AGENT_LISTEN` | `/run/orglane-agent/agent.sock` |
| `agent.secret` | `ORGLANE_AGENT_SECRET` | required |
| `agent.data_dir` | `ORGLANE_AGENT_DATA_DIR` | `/var/lib/orglane` (instances under `instances/`, agent state under `instances/.agent/`) |
| `agent.peer_uid` | `ORGLANE_AGENT_PEER_UID` | `65532`; `-1` disables the check (tests, development) |
| `agent.compose_project` | `ORGLANE_AGENT_COMPOSE_PROJECT` | `orglane` (the bundle's project: finds PostgreSQL and the networks) |
| `agent.postgres_service`, `agent.postgres_user` | | `postgres`, `orglane` |
| `agent.edge_network`, `agent.services_network` | | `<project>_edge`, `<project>_services` |
| `agent.traefik_address` | `ORGLANE_AGENT_TRAEFIK_ADDRESS` | required when an instance needs it |
| `agent.reconcile_interval` | | `10m` |
| `agent.snapshot_retention` | | `2` upgrade snapshots per instance |

## Transport and authentication

HTTP/1.1 over the Unix socket. JSON bodies (`application/json`), at most 1 MiB per request. Paths are versioned (`/v1/...`).

- **Socket:** the agent creates `/run/orglane-agent/` mode `0750` and the socket mode `0660`, both group `65532`, removing a stale socket on start. The core (uid 65532) can connect; other unprivileged processes cannot.
- **Peer check:** with `agent.peer_uid >= 0` the agent reads `SO_PEERCRED` and closes connections from any other uid before reading the request.
- **Key:** `K = crypto.NewKeyring(secret).Key(crypto.PurposeAgentTransport)` with the new purpose `"orglane/v1/agent-transport"`. The secret is `ORGLANE_AGENT_SECRET`, never the master secret.
- **Request signature** (core → agent):

  ```
  X-Orglane-Timestamp: <unix seconds>
  X-Orglane-Nonce:     <base64url, 16 random bytes>
  Authorization:       Orglane-HMAC <base64url(mac)>

  mac = HMAC-SHA256(K, "orglane-agent-request/v1\n" + METHOD + "\n" + PATH?QUERY + "\n"
                       + timestamp + "\n" + nonce + "\n" + hex(SHA-256(body)))
  ```

  The agent rejects (401, problem code `unauthenticated`) a missing or wrong MAC, a timestamp more than 60 s away from its clock or earlier than its own start, and a nonce seen in the last 120 s. Comparison is constant time.
- **Response signature** (agent → core): `X-Orglane-Signature: base64url(HMAC-SHA256(K, "orglane-agent-response/v1\n" + base64url(request mac) + "\n" + status + "\n" + hex(SHA-256(body))))`. The core discards responses without a valid signature (`agent_untrusted`), so a process that took over the socket path cannot feed it status or receive desired state with secrets.
- **Versions:** `GET /v1/health` returns the protocol (`1`) and the Orglane version. The core sends mutating requests only when the versions are equal (development builds `dev` excepted) and reports `agent_version_mismatch` otherwise. Core and agent images are upgraded together by `docker compose up -d`.

## Agent API

Shared types live in `core/internal/agentapi`; the server in `core/internal/agent`; the core client in `core/internal/agentclient`. Errors are RFC 9457 problem details with a stable `code`.

| Method | Path | Purpose | ADR 0006 operation |
|---|---|---|---|
| GET | `/v1/health` | Version, protocol, boot ID, embedded package versions, host memory and disk | — |
| GET | `/v1/instances` | Status of every known instance | status |
| GET | `/v1/instances/{key}` | Status of one instance | status |
| PUT | `/v1/instances/{key}` | Accept a desired spec (202 + operation, or 200 if already accepted) | install, configure, upgrade, remove |
| POST | `/v1/instances/{key}/snapshots` | Take a snapshot (`{"reason": "manual"}`) | back up |
| GET | `/v1/instances/{key}/snapshots` | List complete snapshots | — |
| POST | `/v1/instances/{key}/restore` | Restore a snapshot (`{"snapshot_id", "generation"}`) | restore |
| GET | `/v1/operations/{id}` | Operation state and phase | — |
| POST | `/v1/operations/{id}/confirm` | Confirm an upgrade in `awaiting_confirmation` | upgrade |
| POST | `/v1/operations/{id}/abort` | Roll back an upgrade in `awaiting_confirmation` | upgrade |
| GET | `/v1/instances/{key}/logs?service=&tail=&since=` | Bounded, redacted service logs (`text/plain`) | — |

There is no endpoint that takes a command, an image, a Compose file or a path. A router test asserts that the route table is exactly this list.

### Desired spec

```go
type Spec struct {
    InstanceID uuid.UUID         `json:"instance_id"`
    Generation int64             `json:"generation"`       // strictly increasing per instance
    Package    PackageRef        `json:"package"`          // {"id": "forgejo", "version": "13.0.2-1"}
    State      string            `json:"state"`            // "running" | "stopped" | "absent"
    Purge      bool              `json:"purge,omitempty"`  // with "absent": delete data and database
    Hosts      map[string]string `json:"hosts"`            // route name -> public hostname; plus "workspace"
    URLs       URLs              `json:"urls"`             // workspace, issuer, instance, webhook
    Ingress    string            `json:"ingress_mode"`     // "local" | "domain"
    Settings   map[string]any    `json:"settings"`         // manifest-declared settings
    Secrets    map[string]string `json:"secrets"`          // manifest-declared secret names -> values
}
```

`PUT /v1/instances/{key}` semantics:

| Case | Answer |
|---|---|
| `generation` < accepted generation | `409 stale_generation` (body: accepted generation) |
| `generation` = accepted, same canonical hash | `200` with the operation that applied it (idempotent retry) |
| `generation` = accepted, different hash | `409 generation_conflict` |
| Package version not embedded | `422 unknown_package_version` |
| Settings or secrets fail the manifest's schema | `422 invalid_input` (field names only, never values) |
| Version change not allowed by `upgrade.min_from`, or a downgrade | `422 upgrade_path_blocked` (body: the required stop version) |
| Otherwise | persisted to `desired.json`, `202` with a new operation |

A spec accepted while another operation runs is applied after it; only the newest accepted spec is applied (intermediate generations are skipped). Snapshot and restore requests during an operation get `409 busy`.

### Operations

```go
type Operation struct {
    ID         uuid.UUID `json:"id"`
    Instance   string    `json:"instance"`
    Kind       string    `json:"kind"`  // "apply" | "upgrade" | "snapshot" | "restore" | "remove"
    Generation int64     `json:"generation,omitempty"`
    State      string    `json:"state"` // "queued" | "running" | "awaiting_confirmation" | "succeeded" | "failed" | "rolled_back"
    Phase      string    `json:"phase,omitempty"`
    ErrorCode  string    `json:"error_code,omitempty"`
    SnapshotID string    `json:"snapshot_id,omitempty"`
    StartedAt  time.Time `json:"started_at"`
    FinishedAt *time.Time `json:"finished_at,omitempty"`
}
```

Operations are persisted under `instances/.agent/operations/<id>.json` and kept 7 days, so the core can poll them across agent restarts. Error codes: `unauthenticated`, `agent_busy`, `stale_generation`, `generation_conflict`, `unknown_package_version`, `invalid_input`, `policy_violation`, `upgrade_path_blocked`, `insufficient_disk`, `pull_failed`, `compose_failed`, `database_failed`, `hook_failed`, `health_timeout`, `confirm_timeout`, `aborted`, `snapshot_failed`, `rollback_failed`, `interrupted`.

## Desired state and reconciliation

**Ownership:** the core database is the source of truth (`app_desired_specs`, [providers.md](providers.md#app_desired_specs-managed-only)). The agent stores two files per instance: `desired.json` (last accepted spec) and `current.json` (the spec that is rendered and running). Both are mode `0600` and contain secrets.

**Push:** the core's `orglane.apps.agent_apply` job pushes a spec and polls its operation (snoozing between polls). An unreachable agent is a retryable error (`agent_unavailable`); the job keeps retrying with backoff and the UI shows "waiting for the agent".

**Sync:** `orglane.apps.agent_sync` runs every `agent.sync_interval`, at core start, and when `GET /v1/health` shows a new boot ID. It reads all instance statuses, updates `app_instances` (phase, health, observed generation, running version), calls `Lifecycle.Verify` for active instances, and re-pushes the current spec of every instance whose accepted generation is behind the database (or unknown to the agent, e.g. after its data was lost).

**Reconcile loop (agent):**

- Triggers: an accepted spec (immediately); agent start (every instance, after upgrade journal recovery); Docker `die`/`destroy`/`health_status` events of containers labeled `com.docker.compose.project=orglane-<key>` (debounced 5 s, at most once a minute per instance); every `agent.reconcile_interval`.
- Per instance, serialized:

```
if upgrade.json exists: recover (see "Upgrade") and stop
if desired.package.version != current.package.version
   and the last operation for desired.generation is not rolled_back: run the upgrade flow
switch desired.state:
  running: render → policy check → write files atomically (temp + rename) → ensure data dirs
           → ensure database role and database → docker compose up -d --wait (start timeout)
           → if rendered output changed or first install: run configure hooks
           → current := desired
  stopped: docker compose stop
  absent:  docker compose down --remove-orphans; with purge: drop database and role, delete instances/<key>
```

- Every Compose call is `docker compose --project-name orglane-<key> --project-directory <dir> -f <dir>/compose.yaml --env-file <dir>/.env ...`. The explicit `-f` keeps a stray `compose.override.yaml` from being loaded.
- `up -d` is idempotent: it recreates missing or changed containers and leaves the rest. Manual edits of generated files are overwritten on the next reconcile; this is documented in deploy/README.md.
- A container that crash-loops makes the instance `degraded`; the agent does not restart it beyond Docker's restart policy.
- An upgrade that rolled back is not retried by the agent: it waits for a new generation. The core then records a `revert` spec (the old version) so desired and actual agree, and an admin retries explicitly.

**When something is down:**

| Down | Effect |
|---|---|
| Core | Services keep running. The agent keeps reconciling from `desired.json`. An upgrade waiting for confirmation rolls back after `confirm_timeout` |
| Agent | Services keep running (Docker restart policies). Pushes queue in the core. Health in the core becomes `unknown` after two missed syncs |
| Both, host reboot | Docker restarts the bundle and the services; the agent recovers journals, then reconciles; the core re-syncs |

**Instance phases** (reported by the agent): `pending`, `installing`, `running`, `degraded`, `stopped`, `upgrading`, `rolling_back`, `failed`, `removing`, `absent`.

## Instance directory

```
${ORGLANE_DATA_DIR}/instances/
  .agent/                    agent state: boot ID, operations/ (0700)
  <key>/                     0700, root
    desired.json             0600  last accepted spec (secrets)
    current.json             0600  spec that is running (secrets)
    compose.yaml             0644  rendered project, readable by the admin
    .env                     0600  ORGLANE_SECRET_<NAME>='<value>' for Compose interpolation
    files/                   rendered package files (config, hook scripts), mounted read-only
    data/<name>/             bind-mounted service data, owner and mode from the manifest
    snapshots/<id>/          see "Snapshots"
    upgrade.json             upgrade journal, only during an upgrade
```

`<key>` is the instance key (providers.md), which is the `<id>` of ADR 0006's path.

## Package format

A managed package version is a directory embedded into the binary:

```
providers/forgejo/managed/
  embed.go                  // package managed; //go:embed all:13.0.2-1 ...; var FS embed.FS
  13.0.2-1/
    manifest.yaml
    compose.yaml.tmpl
    files/                  // optional; *.tmpl files are rendered, others copied
```

`core/internal/packages` is the registry of all embedded package versions; both the core (inputs, routes, resource hints, upgrade paths) and the agent (rendering) use it. Test-only packages (`providers/agenttest/managed`) are compiled in only with the build tag `agenttest`, so a production agent cannot install them.

### Manifest

YAML decoded strictly (unknown fields are errors). Example (illustrative values; #61 sets the real ones):

```yaml
format: 1
id: forgejo
version: 13.0.2-1                # <upstream semver>-<package revision>
provider: forgejo
images:                          # the only images the project may use, by version and digest
  forgejo: codeberg.org/forgejo/forgejo:13.0.2-rootless@sha256:<digest>
services: [forgejo]              # long-running services; each must define a healthcheck
routes:
  - name: web
    service: forgejo
    port: 3000
    host_label: git              # host = config.ServiceHost("git") unless the instance overrides it
ports:                           # host ports the project may publish
  - name: ssh
    service: forgejo
    container: 2222
    setting: ssh_port            # the host port comes from this setting
database:
  enabled: true                  # a database and role on the shared PostgreSQL
data_dirs:
  - name: forgejo
    owner: "1000:1000"
    mode: "0750"
settings:                        # typed, validated inputs from the core
  - {name: ssh_port, type: int, min: 1024, max: 65535, default: 2222}
  - {name: organization, type: string, pattern: '^[A-Za-z0-9][A-Za-z0-9_.-]{0,38}$'}
secrets: [db_password, admin_password, oidc_client_secret, webhook_secret]
capabilities_add: []             # Linux capabilities the policy allows beyond Docker's defaults
resources:                       # hints, not limits (ADR 0009)
  memory: {min: 256Mi, recommended: 512Mi}
  disk: {min: 2Gi}
health:
  start_timeout: 5m
hooks:
  configure:                     # after install and whenever the rendered output changes
    - {service: forgejo, command: ["/orglane/files/configure.sh"], timeout: 2m}
  pre_snapshot: []               # reserved for online backups (M7)
  post_snapshot: []
backup:
  database: true
  data_dirs: [forgejo]
  stop_services: [forgejo]       # stopped while a snapshot is taken
upgrade:
  min_from: 12.0.0-1             # oldest version that may upgrade directly to this one
  migrate:                       # optional; omit when the service migrates on start
    {service: forgejo, command: ["forgejo", "migrate"], timeout: 30m}
  confirm_timeout: 10m
```

- **Versions:** `<upstream>-<revision>`, compared by upstream semantic version, then revision. The revision changes when only the package changes.
- **Required upgrade stops:** a release embeds the current version of each package and every older version that is a required stop on the way from the oldest supported version. The core computes the path from the manifests (each step's `min_from`) and pushes one version at a time; each step is its own snapshot, upgrade and commit. If no path exists, the upgrade is blocked with the name of the Orglane release to install first.
- **Hooks** run with `docker compose exec -T <service> <argv>`. The argv is fixed in the manifest (the package is reviewed code); dynamic values reach hooks only through the service's environment from the rendered project. A non-zero exit or timeout fails the operation with `hook_failed`.
- **Migrations** run after the new version starts (`migrate` hook) or on start; either way the health check covers them.
- **Unit tests for every embedded package (#61):** manifest validates; every image has a digest; the template renders with sample inputs and passes the policy; `min_from` paths are acyclic.

### Template rendering

Go `text/template` with `Option("missingkey=error")` and only these functions:

| Function | Result |
|---|---|
| `quote v` | YAML double-quoted scalar; escapes `\`, `"`, control characters and `$` (as `$$`, so Compose does not interpolate input) |
| `secret "name"` | `"${ORGLANE_SECRET_NAME}"`; the value is written to `.env` (single-quoted). Fails for undeclared names |
| `image "name"` | The pinned reference from the manifest |
| `alias "service"` | `<key>-<service>`, the service's alias on the edge network |
| `dir "name"` | Absolute host path of a declared data directory |
| `file "path"` | Absolute host path of a rendered package file |

Template data:

```go
type TemplateData struct {
    Instance struct{ Key, ID, Project string } // Project = "orglane-<key>"
    Package  struct{ ID, Version string }
    Hosts    map[string]string // route name -> hostname, plus "workspace"
    URLs     struct{ Workspace, Issuer, Instance, Webhook string }
    Ingress  struct{ Mode, TraefikAddress string }
    Networks struct{ Edge, Services string }   // external network names
    Database struct{ Address, Name, User string } // "postgres:5432", "svc_<key>"; password: secret "db_password"
    Settings map[string]any   // validated against the manifest; defaults applied
    Host     struct{ MemoryBytes int64; CPUs int } // for tuning, never for limits
}
```

Inputs are validated before rendering: hostnames against the RFC 1123 pattern, URLs as absolute `http`/`https` without user info, settings by declared type and bounds, secret values against `^[A-Za-z0-9._~+/=-]{1,4096}$` (no quotes, `$` or newlines; generated secrets are base64url). Template authors use `quote` for every input string; the policy check is the real guard.

### Policy check

After rendering, the agent runs `docker compose ... config --format json` and checks the normalized project. Any violation fails the operation with `policy_violation` and the rule name; nothing is started.

- Every image equals an image of the manifest (version and digest).
- Only services listed in `services` (plus one-shot services marked `restart: "no"` without published ports).
- No `privileged`, `pid`/`ipc`/`uts`/`userns_mode` = `host`, no `network_mode` (only `networks`), no `devices`, no `device_cgroup_rules`, no `cgroup_parent`.
- `cap_add` only from `capabilities_add`; no `security_opt` that disables seccomp, AppArmor or `no-new-privileges`.
- Volumes: bind mounts only with sources inside `instances/<key>/data/` or `instances/<key>/files/` (read-only), `tmpfs`; no named volumes; nothing that resolves to the Docker socket.
- Ports: only those declared in `ports`, bound to the declared setting's value.
- Networks: only the project's default network and the external `Networks.Edge` and `Networks.Services`.
- `restart: unless-stopped` and log rotation on every long-running service; a healthcheck on every service in `services`.

## Networking

- **Edge network** (`orglane_edge`, external): routed services join it with the alias `<key>-<service>`; Traefik reaches them at `http://<key>-<service>:<port>` (the upstream in the core's routes, providers.md "Ingress routes"). It also gives services outbound internet access.
- **Services network** (`orglane_services`, internal): services that use the database join it and reach PostgreSQL at `postgres:5432`. The core is not on it.
- **Reaching Orglane:** services that call Orglane (OIDC discovery, token and user info for Forgejo; webhooks to the core) use the public workspace URL. The template maps the workspace host to Traefik's fixed address with `extra_hosts`, so traffic stays on the host, TLS is the real certificate, and no hairpin NAT is needed. Forgejo's webhook host allowlist must include the workspace host.
- **Ports:** only manifest-declared host ports (Forgejo SSH) are published.

## Per-service database

- Name and role: `svc_<key with - replaced by _>` (fits PostgreSQL's 63-byte limit given the key pattern).
- The agent finds the bundle's PostgreSQL container by the labels `com.docker.compose.project=<agent.compose_project>` and `com.docker.compose.service=<agent.postgres_service>`, and runs `psql`, `pg_dump` and `pg_restore` in it with `docker exec` as `agent.postgres_user` over the local socket. It needs no database password and no network.
- Ensure (every apply, idempotent): create the role `LOGIN` if missing and set its password from `db_password`; set `CONNECTION LIMIT 20`; create the database `OWNER` the role, `ENCODING 'UTF8'`, `TEMPLATE template0` if missing; `REVOKE ALL ON DATABASE ... FROM PUBLIC`. SQL goes to `psql` on standard input, never on the command line; identifiers come from the validated key, the password literal from a validated secret.
- Purge: `DROP DATABASE ... WITH (FORCE)`, `DROP ROLE`.
- The installer raises `max_connections` with host memory already; adding a managed service is a reason to raise it (deploy/README.md).

## Snapshots

Taken before every upgrade and on request (`POST /v1/instances/{key}/snapshots`, used by tests in M2 and by backups in M7).

```
instances/<key>/snapshots/<snapshot id, UUIDv7>/     0700
  snapshot.json      format, id, created_at, reason (upgrade | manual), package {id, version},
                     generation, images {name: reference}, database {file, bytes, sha256},
                     data_dirs [{name, file, bytes, sha256}], rendered sha256
  spec.json          0600  the spec at snapshot time (secrets)
  rendered/          compose.yaml, .env, files/
  db.dump            pg_dump -Fc of the service database
  data/<name>.tar.gz each backup.data_dirs entry: tar (owners, modes, symlinks kept), gzip BestSpeed
  COMPLETE           written and fsynced last
```

- Consistency: the services in `backup.stop_services` are stopped for the snapshot; the database is dumped while nothing writes to it.
- A directory without `COMPLETE` is partial and is deleted on agent start.
- Preflight: free space under `instances/` must be at least 1.2 × (database size + data directory sizes) + 1 GiB, else `insufficient_disk` before anything stops.
- Retention: the last `agent.snapshot_retention` upgrade snapshots per instance; manual snapshots stay until deleted (M7 manages them). Images referenced by retained snapshots are not pruned.

## Upgrade

Triggered by a spec whose package version differs from `current.json`. The core first switches the instance's routes to its maintenance response and waits `ingress.maintenance_delay`, then pushes the spec; it restores the routes after the operation is terminal.

```
preflight ─► pulling ─► stopping ─► snapshotting ─► switching ─► migrating ─► verifying
   │            │                                     │             │            │
   └─ fail: nothing changed, op failed                └─────────────┴────────────┴─ fail ─► rolling_back
                                                                                 │
                                                          awaiting_confirmation ─┤
                                                         confirm ─► committing ─► committed (succeeded)
                                                         abort / confirm_timeout ─► rolling_back
rolling_back ─► rolled_back (op rolled_back)   |   rolling_back ─► rollback_failed (instance failed)
```

| Phase | What happens |
|---|---|
| `preflight` | Version embedded, path allowed, instance not failed, disk space |
| `pulling` | Render the new version into a staging directory, policy check, `docker compose pull`. No downtime yet |
| `stopping` | Stop `backup.stop_services` |
| `snapshotting` | Take the snapshot (above) |
| `switching` | Write the new rendered files, `up -d` |
| `migrating` | Run the `migrate` hook if declared |
| `verifying` | Wait for all healthchecks within `health.start_timeout` |
| `awaiting_confirmation` | The core runs `Lifecycle.Verify` through the direct upstream and calls `confirm` or `abort`. No answer within `confirm_timeout` rolls back |
| `committing` | `current.json` := new spec; delete the journal; prune old snapshots beyond retention |

**Journal:** `upgrade.json` holds the operation ID, from/to version and generation, snapshot ID and phase. It is written and fsynced before each phase starts.

**Rollback** (idempotent steps, each safe to repeat): `docker compose down` of the new version; for each data directory, move the current one aside to `data/.<name>.failed-<operation>` and extract the snapshot archive; restore the database (`DROP DATABASE ... WITH (FORCE)`, create, `pg_restore` with objects owned by the service role); write the snapshot's rendered files; `up -d`; wait for health. The moved-aside directories are deleted after the next successful upgrade or after 7 days.

**Guarantee:** after `rolled_back`, the instance runs the previous package version with exactly the data of the snapshot. Because the routes point to maintenance between snapshot and commit, no user change made in that window can be lost (webhooks and API calls from inside the host are the exception; they are retried by their senders).

**Interrupted upgrades** (agent crash, host reboot): on start the agent reads every journal before anything else:

| Journal phase | Recovery |
|---|---|
| `preflight`, `pulling` | Delete the journal; reconcile the current version (nothing had changed) |
| `stopping`, `snapshotting` | Delete the partial snapshot; start the current version |
| `switching`, `migrating`, `verifying`, `awaiting_confirmation` | Roll back from the snapshot; error code `interrupted` |
| `committing` | Finish the commit (confirmation had been received) |
| `rolling_back` | Continue the rollback |

The agent never resumes forward. A failed rollback (`rollback_failed`) leaves the snapshot and the moved-aside data in place, marks the instance `failed` and is shown in Settings → Apps with the snapshot ID; `POST /v1/instances/{key}/restore` retries it.

## Status, health and logs

- Instance status: phase, `observed_generation`, `accepted_generation`, running package version, services (`docker compose ps --format json`: state, health), current operation, snapshot count, `data_retained` for removed instances without purge.
- Health has two levels: container healthchecks (agent) and `Lifecycle.Verify` through the provider's API (core). The core shows the worse of the two.
- `GET /v1/health` includes `mem_total`, `mem_available` and `disk_free` (of `instances/`); the core uses them with the manifests' resource hints: install is refused when total memory is below the sum of `min` of all instances plus 1 GiB for the bundle, or free disk is below `disk.min`; below `recommended` it warns.
- Logs: `docker compose logs --no-color --timestamps --tail <n> <service>`, `tail` at most 1000 lines and the response at most 1 MiB; every secret value of the instance is replaced by `[redacted]`. The core exposes them only to `apps.manage` and audits each read (`app_instance.logs_read`).
- The agent logs JSON to stdout (container logs) and never logs specs, secrets or `.env` contents.

## Testing

| What | How | Issue |
|---|---|---|
| Transport | Unit tests: signatures, clock skew, replay, wrong secret, unsigned or tampered response refused by the client, peer uid check | #59 |
| Route table | Test asserts the exact set of agent routes (no command endpoint) | #59 |
| Rendering and policy | Unit tests per rule with `agenttest` package variants (privileged, host bind mount, Docker socket, unpinned image, undeclared port, host network, extra capability); injection attempts in settings (newline, `$`, quotes, YAML syntax) are rejected or rendered inert | #59 |
| Agent with real Docker | `integration` tag: the test starts a minimal bundle project (PostgreSQL with the two networks, labeled like the bundle) and runs the agent in-process with `peer_uid = -1`; installs the `agenttest` package (a pinned tiny image plus `psql` from the pinned PostgreSQL image for a database write), reconciles drift (container removed, compose file edited), changes settings, stops, removes, purges | #59 |
| Upgrades | Successful upgrade v1 → v2; v2 with a failing healthcheck rolls back with the database row and data file intact; abort and confirm timeout roll back; a crash at each journal phase (test hook `ORGLANE_AGENT_TEST_CRASH_AT=<phase>`, only in `agenttest` builds) recovers as the table says; v1 → v3 with `min_from: v2` is refused, v1 → v2 → v3 works | #60 |
| Bundle | `make test-compose`: the bundle with `COMPOSE_PROFILES=managed` starts, the core reports the agent connected, the core container has no Docker socket and no Docker CLI; #61 adds installing Forgejo and reaching it through Traefik | #59, #61 |
| Installer | `make test-install`: `--managed`, `enable-managed`, `uninstall` removes instance projects | #59 |
| Forgejo package | Package unit tests; integration install through the agent; idle memory measured and reported | #61 |

## Non-goals (M2)

- Kubernetes operator (same contract later, ADR 0006).
- Backups: schedules, S3 targets, test restores, the backups UI (M7). M2 snapshots exist for upgrades and tests.
- Online snapshots (`pre_snapshot`/`post_snapshot` hooks are reserved, not run).
- Upgrading Orglane itself (core and agent images); that stays `docker compose pull && up -d` / the installer.
- Third-party or user-supplied packages, remote package downloads.
- Resource limits on services; multi-host.
- Adopting existing Compose projects or self-hosted installs.

## Open questions for the maintainer

1. **Default for managed services.** This spec keeps the agent off unless `--managed` is chosen (ADR 0006: installed only when used). The interactive installer could default to "yes" in domain mode so "Install Forgejo" works without a second command. Which default?
2. **Local mode and the workspace on bare `localhost`.** Inside a container `localhost` is the container itself, so Forgejo cannot reach Orglane's issuer at `http://localhost`. `extra_hosts` works for any other name (for example `orglane.localhost`, which browsers also resolve to loopback). Options: change the local/demo default host to `orglane.localhost`; or support managed services only in domain mode for M2. #61 has to decide demo behavior; this needs the maintainer's call first.
3. **Automatic package upgrades after an Orglane upgrade.** ADR 0007 says users don't pick versions. This spec has the core start package upgrades automatically when a new Orglane release embeds a newer package version (each with snapshot and rollback). Alternatively the admin presses "Upgrade" with a notice. Which?
4. **Forgejo SSH.** Publishing a host port (default 2222) for Git over SSH is in the example manifest. Keep SSH in M2, or HTTPS only until later?
5. **Agent image name and signing.** `ghcr.io/orglane/orglane-agent` as a second image, built and signed like the core image. Acceptable, or one image with both (Docker CLI in the core image, unused by the server)?
