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
                └─edge──► managed services (Compose project <project>-<key>) ─services─► postgres
                         └──────────────────────────────────────────────────────────────────┘
```

### Compose bundle changes (#59)

```yaml
services:
  agent:
    # Pinned by version and digest (AGENTS.md). The bundle names the release
    # tag; the installer resolves it to the registry digest and writes
    # ghcr.io/orglane/orglane-agent:<tag>@sha256:<digest> into .env.
    image: ${ORGLANE_AGENT_IMAGE:-ghcr.io/orglane/orglane-agent:<release tag>}
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
      # The bundle's project name: finds PostgreSQL and the networks, and
      # prefixes instance projects, so two installs on one host stay apart.
      ORGLANE_AGENT_COMPOSE_PROJECT: ${COMPOSE_PROJECT_NAME:-orglane}
      # Where published instance ports (Forgejo SSH) bind: 127.0.0.1 in local
      # mode, all interfaces in domain mode. Written by the installer.
      ORGLANE_AGENT_PUBLISH_ADDRESS: ${ORGLANE_AGENT_PUBLISH_ADDRESS:-127.0.0.1}
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

  traefik:
    networks:
      edge:
        ipv4_address: ${ORGLANE_EDGE_NETWORK_PREFIX:-10.231.0}.2
        # Containers on the edge network (managed services) resolve the
        # workspace host to Traefik, so they reach Orglane by its public name.
        aliases:
          - ${ORGLANE_DOMAIN_WORKSPACE_HOST:-${ORGLANE_DOMAIN_BASE:-localhost}}

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

- **Opt-in, off by default (ADR 0006, maintainer decision):** the `agent` service only starts with `COMPOSE_PROFILES=managed`. Without it the core runs unchanged and `GET /api/v1/apps/agent` reports `not_connected`. The core cannot start the agent itself, so Settings → Apps shows the exact one-line command to run on the host, `curl -fsSL https://orglane.app/install.sh | sh -s -- enable-managed` (or `sh install.sh enable-managed` with a local copy), and refreshes when the agent connects.
- **Installer (#59):** always generates `ORGLANE_AGENT_SECRET` (64 hex characters) into `.env`, so enabling the agent later needs no core restart. The interactive installer does not ask and does not enable managed services; the option `--managed` and the subcommand `enable-managed` write `COMPOSE_PROFILES=managed`, `ORGLANE_AGENT_IMAGE` (see "Image"), `ORGLANE_AGENT_PUBLISH_ADDRESS` and, in local mode, the `*.localhost` base (see "Local mode"), then run `docker compose up -d`. It creates `instances/` (mode 700). `uninstall` first runs `docker compose down` for every project under `instances/*/compose.yaml`, then the bundle. `status` shows the agent.
- **Image (#59, maintainer decision):** a separate image `ghcr.io/orglane/orglane-agent`, a second target of the root `Dockerfile`: distroless static base, the `orglane` binary, the static Docker CLI and the Compose plugin, both pinned by version and SHA-256. The core image stays distroless without Docker tooling. Released together with the core image by `images.yml`, same tag. **Pinned by version and digest like every bundle image (AGENTS.md, no exception):** the bundle references the agent image by release tag, and the installer resolves that published tag to its registry digest (the multi-arch index digest, for example by `docker pull` of the tag and reading `RepoDigests`) and writes `ORGLANE_AGENT_IMAGE=ghcr.io/orglane/orglane-agent:<tag>@sha256:<digest>` into `.env`. If no digest can be resolved, the installer stops with an error instead of writing a bare tag. The core's version check against the agent (see "Versions") stays as an extra guard against skew; it does not replace the digest. The core image (`ORGLANE_IMAGE`) still uses a bare release tag today; giving it the same treatment is a follow-up outside M2.
- **Hardening of the core's internal listener (#59):** since managed services join the edge network, the internal listener (`/traefik/config`) answers only requests from Traefik's fixed address.

### Configuration

Core (`core/internal/config`, section `agent`):

| Key | Env | Default | Meaning |
|---|---|---|---|
| `agent.socket` | `ORGLANE_AGENT_SOCKET` | empty | Agent socket path. Empty, or an empty secret, disables managed services |
| `agent.secret` | `ORGLANE_AGENT_SECRET` | empty | Shared secret, at least 32 characters |
| `agent.sync_interval` | `ORGLANE_AGENT_SYNC_INTERVAL` | `5m` | Status sync and re-push of lagging desired state |

Agent (`orglane agent`; it does not require `database.url` or `domain.base`):

| Key | Env | Default |
|---|---|---|
| `agent.listen` | `ORGLANE_AGENT_LISTEN` | `/run/orglane-agent/agent.sock` |
| `agent.secret` | `ORGLANE_AGENT_SECRET` | required |
| `agent.data_dir` | `ORGLANE_AGENT_DATA_DIR` | `/var/lib/orglane` (instances under `instances/`, agent state under `instances/.agent/`) |
| `agent.peer_uid` | `ORGLANE_AGENT_PEER_UID` | `65532`; `-1` disables the check (tests, development) |
| `agent.compose_project` | `ORGLANE_AGENT_COMPOSE_PROJECT` | `orglane`; the bundle passes `COMPOSE_PROJECT_NAME`. Finds PostgreSQL and the networks; instance projects are `<project>-<key>` |
| `agent.postgres_service`, `agent.postgres_user` | | `postgres`, `orglane` |
| `agent.edge_network`, `agent.services_network` | | `<project>_edge`, `<project>_services` |
| `agent.publish_address` | `ORGLANE_AGENT_PUBLISH_ADDRESS` | `127.0.0.1`; host address for published instance ports |
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
- **Response signature** (agent → core): `X-Orglane-Signature: base64url(HMAC-SHA256(K, "orglane-agent-response/v1\n" + base64url(request mac) + "\n" + status + "\n" + hex(SHA-256(body))))`. The core discards responses without a valid signature (`agent_untrusted`), so a process that took over the socket path cannot feed it status. Because a request body is sent before its response can be checked, the client sends a signed `GET /v1/health` first on every new connection and sends requests that carry secrets (`PUT /v1/instances/{key}`) only on a connection whose peer has proven the key.
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
| The key's directory belongs to another `instance_id` | `409 instance_mismatch` (a removed instance's data is still retained under that key) |
| `generation` < accepted generation | `409 stale_generation` (body: accepted generation) |
| `generation` = accepted, same canonical hash | `200` with the operation that applied it (idempotent retry) |
| `generation` = accepted, different hash | `409 generation_conflict` |
| Package version not embedded | `422 unknown_package_version` |
| Settings or secrets fail the manifest's schema | `422 invalid_input` (field names only, never values) |
| Version lower than `current.json`'s, or a change from `current.json`'s version not allowed by `upgrade.min_from` | `422 upgrade_path_blocked` (body: the required stop version). Checks compare with `current.json` (what runs), not with the last accepted spec, so a revert spec after a rollback (the version that runs) is a plain apply |
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
    Rule       string    `json:"rule,omitempty"` // the policy rule, with policy_violation
    SnapshotID string    `json:"snapshot_id,omitempty"`
    StartedAt  time.Time `json:"started_at"`
    FinishedAt *time.Time `json:"finished_at,omitempty"`
}
```

Operations are persisted under `instances/.agent/operations/<id>.json` and kept 7 days, so the core can poll them across agent restarts. Error codes: `unauthenticated`, `agent_busy`, `instance_mismatch`, `stale_generation`, `generation_conflict`, `unknown_package_version`, `invalid_input`, `policy_violation`, `upgrade_path_blocked`, `insufficient_disk`, `pull_failed`, `compose_failed`, `database_failed`, `hook_failed`, `health_timeout`, `confirm_timeout`, `aborted`, `snapshot_failed`, `rollback_failed`, `interrupted`, plus `not_found` (unknown instance, operation or endpoint), `superseded` (a queued operation whose spec was replaced by a newer generation before it ran) and `internal`.

## Desired state and reconciliation

**Ownership:** the core database is the source of truth (`app_desired_specs`, [providers.md](providers.md#app_desired_specs-managed-only)). The agent stores two files per instance: `desired.json` (last accepted spec) and `current.json` (the spec that is rendered and running). Both are mode `0600` and contain secrets.

**Push:** the core's `orglane.apps.agent_apply` job pushes a spec and polls its operation (snoozing between polls). An unreachable agent is a retryable error (`agent_unavailable`); the job keeps retrying with backoff and the UI shows "waiting for the agent".

**Sync:** `orglane.apps.agent_sync` runs every `agent.sync_interval`, at core start, and when `GET /v1/health` shows a new boot ID. It reads all instance statuses, updates `app_instances` (phase, health, observed generation, running version), calls `Lifecycle.CheckHealth` for active instances, and re-pushes the current spec of every instance whose accepted generation is behind the database (or unknown to the agent, e.g. after its data was lost).

**Reconcile loop (agent):**

- Triggers: an accepted spec (immediately); agent start (every instance, after upgrade journal recovery); Docker `die`/`destroy`/`health_status` events of containers labeled `com.docker.compose.project=<project>-<key>` (debounced 5 s, at most once a minute per instance); every `agent.reconcile_interval`.
- Per instance, serialized. Reconciliation always targets `current.json`; only an operation moves `current` toward `desired`:

```
if upgrade.json exists: recover (see "Upgrade") and stop

if desired.generation > current.generation and desired.generation is not blocked:
    if desired.package.version != current.package.version:
        run the upgrade flow            # moves current only on commit
        stop
    target := desired                   # apply: configure, stop, remove
else:
    target := current                   # includes: desired.generation blocked after a failed or rolled-back upgrade

converge(target):
  running: render → policy check → write files atomically (temp + rename) → ensure data dirs
           → ensure database role and database → docker compose up -d --remove-orphans --wait (start timeout)
           → if rendered output changed or first install: run configure hooks
  stopped: docker compose stop
  absent:  docker compose down --remove-orphans; with purge: drop database and role, delete instances/<key>
  on success: current := target
```

- **Blocked generations:** a generation whose upgrade failed or rolled back is recorded in `instances/<key>/blocked.json` and is never tried again by the agent, whatever triggers the reconcile (Docker events, the periodic run, an agent restart, the core being offline). The instance keeps running `current`. The block ends with the next accepted generation: the core's **revert** spec (same version as `current`, the acknowledgment of the rollback) or an admin's retry (a new generation with the target version). A failed non-upgrade apply leaves `current` running and is retried by the periodic reconcile at most 3 times, then blocked the same way.
- Every Compose call is `docker compose --project-name <project>-<key> --project-directory <dir> -f <dir>/compose.yaml --env-file <dir>/.env ...`. The explicit `-f` keeps a stray `compose.override.yaml` from being loaded. `--remove-orphans` on every `up` removes containers of services a package version dropped or renamed, so they cannot keep ports or data access.
- `up -d` is idempotent: it recreates missing or changed containers and leaves the rest. Manual edits of generated files are overwritten on the next reconcile; this is documented in deploy/README.md.
- A container that crash-loops makes the instance `degraded`; the agent does not restart it beyond Docker's restart policy.
- After a rollback the core records a `revert` spec (the running version) as soon as it sees the operation, so desired and actual agree; an admin retries the upgrade explicitly (providers.md "Managed instance lifecycle").

**When something is down:**

| Down | Effect |
|---|---|
| Core | Services keep running. The agent keeps reconciling `current.json` and applies an already accepted newer generation. An upgrade waiting for confirmation rolls back after `confirm_timeout`; the blocked generation is not retried |
| Agent | Services keep running (Docker restart policies). Pushes queue in the core. Health in the core becomes `unknown` after two missed syncs |
| Both, host reboot | Docker restarts the bundle and the services; the agent recovers journals, then reconciles; the core re-syncs |

**Instance phases** (reported by the agent): `pending`, `installing`, `running`, `degraded`, `stopped`, `upgrading`, `rolling_back`, `failed`, `removing`, `absent`.

## Instance directory

```
${ORGLANE_DATA_DIR}/instances/
  .agent/                    agent state: boot ID, operations/ (0700)
  <key>/                     0700, root
    instance.json            0600  {"instance_id": ...}: binds the directory to one app instance
    desired.json             0600  last accepted spec (secrets)
    current.json             0600  spec that is running (secrets)
    blocked.json             0600  generations that failed or rolled back (see "Reconcile loop")
    compose.yaml             0644  rendered project, readable by the admin
    .env                     0600  ORGLANE_SECRET_<NAME>='<value>' for Compose interpolation
    files/                   rendered package files (config, hook scripts), mounted read-only
    data/<name>/             bind-mounted service data, owner and mode from the manifest
    snapshots/<id>/          see "Snapshots"
    upgrade.json             upgrade journal, only during an upgrade
```

`<key>` is the instance key (providers.md), which is the `<id>` of ADR 0006's path. The directory is bound to the immutable `instance_id` in `instance.json`: a spec for the same key with another instance ID is refused (`instance_mismatch`). A key is reused only after a purge deleted the directory, the database and the role (providers.md "app_instances").

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
| `alias "service"` | `<key>-<service>`, the service's alias on the edge network that the core's routes use; `<key>-<service>-fenced` while the project is rendered fenced (see "Upgrade") |
| `port "name"` | The published host port mapping of a declared port (`<publish address>:<host port>:<container port>`), or nothing while fenced |
| `dir "name"` | Absolute host path of a declared data directory |
| `file "path"` | Absolute host path of a rendered package file |

Template data:

```go
type TemplateData struct {
    Instance struct{ Key, ID, Project string } // Project = "<compose project>-<key>"
    Package  struct{ ID, Version string }
    Hosts    map[string]string // route name -> hostname, plus "workspace"
    URLs     struct{ Workspace, Issuer, Instance, Webhook string }
    Ingress  struct{ Mode string }  // "local" | "domain"
    Fenced   bool               // true while an upgrade candidate runs (see "Upgrade")
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
- Ports: only those declared in `ports`, bound to `agent.publish_address` and the declared setting's value; none at all in a fenced render.
- Aliases on the edge network: only those produced by `alias` (in a fenced render, only `-fenced` aliases).
- Networks: only the project's default network and the external `Networks.Edge` and `Networks.Services`.
- `restart: unless-stopped` and log rotation on every long-running service; a healthcheck on every service in `services`.

## Networking

- **Edge network** (`<project>_edge`, external): routed services join it with the alias `<key>-<service>`; Traefik reaches them at `http://<key>-<service>:<port>` (the upstream in the core's routes, providers.md "Ingress routes"). It also gives services outbound internet access.
- **Services network** (`<project>_services`, internal): services that use the database join it and reach PostgreSQL at `postgres:5432`. The core is not on it.
- **Reaching Orglane by the same name as the browser:** Traefik carries the workspace host as a network alias on the edge network (bundle change above). Docker's embedded DNS answers it for every container on that network, so Forgejo calls OIDC discovery, token and user info, and sends webhooks, to the public workspace URL; the traffic stays on the host, TLS (domain mode) is the real certificate, and no hairpin NAT is needed. Forgejo's webhook host allowlist must include the workspace host. Service-to-service calls by public service host names are not needed in M2 (the core uses the direct upstream); if a later package needs them, Traefik gets those aliases too.
- **Published ports:** only manifest-declared host ports, bound to `agent.publish_address`.

### Local mode (demo, e2e and acceptance tests)

Managed services work in local mode, with `*.localhost` names (maintainer decision): browsers resolve every `*.localhost` name to loopback, where Traefik listens, and containers resolve the workspace host through the Traefik alias.

- **Blocker, and the way around it:** the bare name `localhost` cannot be the workspace host with managed services. Inside a container `localhost` is answered from `/etc/hosts` (the container itself) before Docker's DNS, so no alias can redirect it. Therefore `--managed` / `enable-managed` in local mode set `ORGLANE_DOMAIN_BASE=orglane.localhost` (workspace `http://orglane.localhost`, Forgejo `http://git.orglane.localhost`), and the demo with managed services uses the same. Local mode without managed services keeps `localhost`. Switching an existing local install changes its workspace URL; the installer says so.
- The OIDC issuer is `http://orglane.localhost` for both the browser and Forgejo, so discovery, redirects and tokens agree.
- Chromium and Firefox (and Playwright) resolve `*.localhost` without configuration. Safari on macOS does not resolve subdomains of `localhost`; local mode documents using another browser or adding hosts entries.
- Published ports (Forgejo SSH) bind to `127.0.0.1` in local mode.

### Forgejo SSH (#61, maintainer decision: in M2)

- Git over SSH uses a **direct published port** of the Forgejo container, default host port **2222** (setting `ssh_port`; 22 is normally the host's own SSH daemon). Traefik is not used: SSH has no host name in its handshake (no SNI), so a Traefik TCP entry point could not route by host and would still need one port per instance.
- Forgejo is configured with `SSH_DOMAIN` = its host and `SSH_PORT` = the published port, so clone URLs are `ssh://git@git.acme.com:2222/<org>/<repo>.git`. The domain-mode install shows that the port must be open in the host firewall.
- Users manage their SSH keys in Forgejo. Removing keys on offboarding is M5; until then a suspended person loses SSO and team access but keeps working keys for repositories that remain accessible (the capability matrix says so).
- While an upgrade candidate runs, the port is not published (fence, see "Upgrade").

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

Triggered by a spec whose package version differs from `current.json`.

**When upgrades start (maintainer decision):** automatically. When the core starts with a release that embeds a newer package version than an instance runs, it plans the path (required stops) and records an `upgrade` spec, one instance at a time; each step runs this flow. The result (succeeded, rolled back with the reason, failed) is an `app_operations` row and a report in Settings → Apps, and an audit entry. An admin can retry a rolled-back or failed upgrade (`POST /api/v1/apps/instances/{instanceId}/upgrade`); the agent never retries one by itself.

**Write fence.** A rollback must not discard a write that a client was told succeeded. Therefore the candidate version is never reachable through a client entry point until commit:

- While fenced, the project is rendered with `Fenced = true`: routed services get only `-fenced` aliases (Traefik's upstream `<key>-<service>` does not resolve to them, whatever its route says) and no host port is published (no SSH). The fence is part of the rendered files, so it survives container and host restarts.
- The core verifies the candidate through the fenced alias (`Instance.Upstream` points to it during the upgrade).
- The old version is stopped before the snapshot, so its writes up to the stop are in the snapshot; requests cut off by the stop were not acknowledged.
- For a friendly answer instead of a `502`, the core switches the instance's routes to its own maintenance response (`503`, `Retry-After`) when the operation starts and back when it is terminal. Correctness does not depend on when Traefik picks this up.
- Packages turn off the candidate's outbound side effects where the product allows it while fenced (Forgejo: mailer, cron, webhook delivery). Work the candidate does by itself (migrations, background jobs) is discarded by a rollback and redone by the restored version.

```
preflight ─► pulling ─► stopping ─► snapshotting ─► switching ─► migrating ─► verifying
   │            │                                     │             │            │
   └─ fail: nothing changed, op failed                └─────────────┴────────────┴─ fail ─► rolling_back
                                                                                 │
                                                          awaiting_confirmation ─┤
                                                         confirm ─► committing ─► committed (succeeded)
                                                         abort / confirm_timeout ─► rolling_back
rolling_back (fenced restore) ─► restored (journal fsynced) ─► rolled_back (op rolled_back)
rolling_back ─► rollback_failed (instance failed)
```

| Phase | What happens |
|---|---|
| `preflight` | Version embedded, path allowed, instance not failed, disk space |
| `pulling` | Render the new version into a staging directory, policy check, `docker compose pull`. No downtime yet |
| `stopping` | Stop `backup.stop_services` |
| `snapshotting` | Take the snapshot (above) |
| `switching` | Write the new rendered files, fenced; `up -d --remove-orphans` |
| `migrating` | Run the `migrate` hook if declared |
| `verifying` | Wait for all healthchecks within `health.start_timeout` |
| `awaiting_confirmation` | The core runs `Lifecycle.CheckHealth` through the fenced alias and calls `confirm` or `abort`. No answer within `confirm_timeout` rolls back |
| `committing` | Render unfenced, `up -d --remove-orphans --wait` (containers are recreated with the real aliases and ports); `current.json` := new spec; delete the journal; prune old snapshots beyond retention |

**Journal:** `upgrade.json` holds the operation ID, from/to version and generation, snapshot ID and phase. It is written and fsynced before each phase starts.

**Rollback** has two journaled phases, so data is restored at most once and never after a client could have written to the restored version:

1. `rolling_back` (restore, fenced): `docker compose down` of the candidate; for each data directory, move the current one aside to `data/.<name>.failed-<operation>` (skipped if already moved) and extract the snapshot archive; restore the database (`DROP DATABASE ... WITH (FORCE)`, create, `pg_restore` with objects owned by the service role); write the snapshot's rendered files **fenced**; `up -d --remove-orphans --wait`. Every step is safe to repeat, and nothing in this phase is reachable by a client (the candidate and the restored version are both fenced). When the restored version is healthy, the journal phase becomes `restored` and is fsynced **before** anything is unfenced.
2. `restored` (expose, never restore again): write the snapshot's rendered files unfenced; `up -d --remove-orphans --wait`; record the generation as blocked; `current.json` := the snapshot's spec; mark the operation `rolled_back`; delete the journal. Repeating this phase only re-renders and restarts; it never touches data.

The moved-aside directories are deleted after the next successful upgrade or after 7 days. A manual restore (`POST /v1/instances/{key}/restore`) uses the same two phases.

**Guarantee (narrowed to what the fences enforce):** after `rolled_back`, the instance runs the previous package version with the data of the snapshot plus every write made to the restored version after it was exposed. No write acknowledged to a client through a declared entry point (routes, published ports) is lost: between the snapshot and the commit or the `restored` journal entry no such entry point reaches any version, and after `restored` is durable no data is restored again, whatever crashes or restarts follow. Not covered: work the candidate did on its own (lost or repeated), and calls made to the candidate's fenced alias by the core itself during verification (read-only checks). Every M2 package must route all client writes through declared routes and ports; the policy check enforces that nothing else is published.

**Interrupted upgrades** (agent crash, host reboot): on start the agent reads every journal before anything else:

| Journal phase | Recovery |
|---|---|
| `preflight`, `pulling` | Delete the journal; reconcile the current version (nothing had changed) |
| `stopping`, `snapshotting` | Delete the partial snapshot; start the current version |
| `switching`, `migrating`, `verifying`, `awaiting_confirmation` | Roll back from the snapshot; error code `interrupted` |
| `committing` | Finish the commit: render unfenced and start (confirmation had been received) |
| `rolling_back` | Continue the fenced restore (nothing was reachable yet, so restoring again is safe) |
| `restored` | Only finish exposing: render unfenced and start. Data is **not** restored again: the restored version may already have accepted writes, for example over SSH after Docker restarted it unfenced on a reboot |

Docker may restart containers on a reboot before the agent runs. In every phase before `restored` they come back fenced (the fence is in the rendered files), so no client reaches them; from `restored` on they may come back unfenced, which is why recovery never restores data after that point. The agent never resumes forward. A failed rollback (`rollback_failed`) leaves the snapshot and the moved-aside data in place, marks the instance `failed` and is shown in Settings → Apps with the snapshot ID; `POST /v1/instances/{key}/restore` retries it.

## Status, health and logs

- Instance status: phase, `observed_generation`, `accepted_generation`, running package version, services (`docker compose ps --format json`: state, health), current operation, snapshot count, `data_retained` for removed instances without purge.
- Health has two levels: container healthchecks (agent) and `Lifecycle.CheckHealth` through the provider's API (core). The core shows the worse of the two.
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
| Rollback stays rolled back | After a rollback, with the core stopped: Docker events (container killed), periodic reconciles and an agent restart keep v1 running and never start v2; a revert spec (v1) is accepted as an apply and unblocks; a retry spec (v2, new generation) runs the upgrade again | #60 |
| Write fence | A write acknowledged before the snapshot (HTTP and over the published port) survives the rollback; during `verifying` the route answers 503 and the published port refuses connections; a simulated host restart during `verifying` (stop and start every project container and the agent) leaves the candidate fenced and ends rolled back with the acknowledged write intact | #60 |
| Rollback recovery | Crash (test hook) during `rolling_back` → restart repeats the fenced restore and ends `rolled_back`; crash immediately after the `restored` journal entry and after unfencing, then an acknowledged write over the published SSH port to the restored version before the agent restarts → after recovery the write is still there and data was not restored again | #60 |
| Keys and instance IDs | A spec with the same key and another instance ID is refused while the directory exists; after purge the key can be used again | #59 |
| Bundle | `make test-compose`: the bundle with `COMPOSE_PROFILES=managed` starts, the core reports the agent connected, the core container has no Docker socket and no Docker CLI; #61 adds installing Forgejo and reaching it through Traefik | #59, #61 |
| Installer | `make test-install`: default install has no agent; `--managed`, `enable-managed` (local mode switches to `orglane.localhost`), `uninstall` removes instance projects | #59 |
| Local mode | Forgejo installed in local mode reaches the issuer at `http://orglane.localhost` through the Traefik alias; Playwright signs in to `http://git.orglane.localhost` through Orglane | #61, #62 |
| Forgejo package | Package unit tests; integration install through the agent; idle memory measured and reported | #61 |

## Implementation notes (#59)

Details the sections above leave open, as implemented by the agent process (#59):

- **Routes:** #59 serves `GET /v1/health`, `GET /v1/instances`, `GET`/`PUT /v1/instances/{key}`, `GET /v1/instances/{key}/logs` and `GET /v1/operations/{id}`. Snapshots, restore, confirm and abort come with the upgrade flow (#60), which extends the route table test. Until then a spec whose package version differs from `current.json` is refused with `422 upgrade_path_blocked` (`required_version` = the running version); a removal (`state: absent`) is accepted for any version.
- **Spec decoding:** unknown JSON fields are refused (`400 invalid_input`), so a spec cannot carry an image, a Compose file or a command. Every route name of the manifest plus `workspace` must be in `hosts`, and every declared secret in `secrets`. Settings are `int`, `string` or `bool`; strings never contain control characters (newlines included), whatever the pattern.
- **Authentication:** answers to requests that fail verification (401, and 413 for bodies over 1 MiB) are not signed, so the agent is no signing oracle; the client reports them as `agent_untrusted`. The nonce of a request is recorded only after its MAC verified. Besides `agent.peer_uid`, the agent's own uid may connect (the container healthcheck `orglane agent healthcheck` runs as the agent's user). The socket's group is set only when the agent runs as root.
- **Docker environment:** `docker` and `docker compose` get only `PATH`, `HOME`, `TMPDIR`, `XDG_RUNTIME_DIR` and `DOCKER_*` from the agent's environment, so neither `ORGLANE_AGENT_SECRET` nor `COMPOSE_*` overrides reach Compose interpolation.
- **Templates:** `file "."` is the rendered `files/` directory (mount it rather than single files: files are replaced one by one inside it, so running containers see updates). Files ending in `.sh` are rendered `0755`, others `0644`. `data_dirs[].owner` is optional (the agent's user); it is applied only when the agent runs as root.
- **Manifest:** images need a registry host, a tag and a SHA-256 digest; the bundle's service names (`postgres`, `traefik`, `orglane`, `agent`) cannot be package services.
- **Policy:** service keys are an allowlist (`cap_add`, `cap_drop`, `command`, `depends_on`, `entrypoint`, `environment`, `expose`, `healthcheck`, `image`, `init`, `labels`, `logging`, `networks`, `ports`, `pull_policy`, `read_only`, `restart`, `security_opt`, `shm_size`, `stop_grace_period`, `stop_signal`, `tmpfs`, `user`, `volumes`, `working_dir`); everything else (`build`, `configs`, `volumes_from`, `extra_hosts`, `container_name`, `sysctls`, ...) is refused, and top-level keys other than `name`, `services`, `networks` (and an empty `volumes`) too. `security_opt` may only add `no-new-privileges`. Per-service network settings are limited to `aliases` (no fixed addresses). Bind sources are checked as written and with symlinks resolved; bind propagation must stay private. Labels under `com.docker.` are refused. A `docker compose config` that fails is `compose_failed`.
- **Retries:** an operation ends with its first attempt. The periodic reconcile's retries of a failed apply create a new operation each; after four failed attempts (the first and three retries) the generation is blocked.
- **Hooks:** configure hooks run whenever an instance starts with a render other than the one they last completed for, recorded in `instances/<key>/configured.json` (0600): the first start (also of an instance installed stopped), a start after settings changed (also while stopped), and a reinstall after a removal (which deletes the record). Never for drift repair.
- **Durable results:** an operation reports `succeeded` only after `blocked.json` and then `current.json` are written (in that order, so `current.json` never advances past a failed write); if either write fails it reports `failed` with `internal`, and the generation stays pending for the next reconcile. For `state: absent` with `purge: true`, success means the instance directory was removed and no state file is written; only if a newer spec arrived during the purge does the directory keep `instance.json` and `desired.json`, with the purge recorded as `current.json` so the newer spec applies next.
- **Logs:** `tail` is 1–1000 (default 100); `since` is an RFC 3339 time or a positive Go duration up to 31 days. The agent keeps only a bounded tail of the output while draining it (lines can be long); a cut drops the cut line, so no fragment of a secret escapes redaction, and the answer then starts with `[earlier output truncated]`. Other Docker output is bounded too (16 MiB for standard output, the last 64 KiB of standard error).
- **Socket directory:** the directory of `agent.listen` must hold nothing but the socket (the agent sets its mode and group); a shared directory such as `/tmp` is refused.

## Implementation notes (#70)

Details of the packaging (image, bundle, installer) that the sections above leave open, as implemented by #70:

- **Image:** the `agent` target of the root `Dockerfile` (the core stays the default, last target). Docker CLI 29.9.3 (static release archive) and Compose 5.6.1 (release binary, checked against its published `.sha256`), pinned by SHA-256 per architecture in a download stage on the build platform; the final stage has no `RUN` step. The plugin lives in `/usr/local/libexec/docker/cli-plugins`, a system directory the CLI searches whatever `DOCKER_CONFIG` says. `images.yml` builds both targets as a matrix with per-image cache scopes, verifies the agent image's Docker CLI and Compose plugin on every platform (`verify-image.sh ... agent`), and promotes the aliases of both images in the serialized promote job by one plan, computed from the versions published for both (`ci release-aliases -image <core> -image <agent>`), so core and agent aliases always name the same release.
- **Bundle default:** `ORGLANE_AGENT_IMAGE` defaults to `ghcr.io/orglane/orglane-agent:<release>`, a tag that release-please bumps with the release (`x-release-please-version`, `extra-files` in `release-please-config.json`). The installer always writes the digest-pinned reference.
- **Internal listener:** `server.internal_allowed_peers` (`ORGLANE_SERVER_INTERNAL_ALLOWED_PEERS`), a comma-separated CIDR list; empty or `any` (the default outside the bundle) allows every peer. The bundle sets Traefik's address (`<prefix>.2/32`); other peers get `403`. The check uses the TCP peer address only.
- **Workspace alias:** Docker accepts any string as an alias, but a workspace host with a port (`orglane.localhost:8080`) is no DNS name. The bundle therefore takes the alias from `ORGLANE_EDGE_WORKSPACE_ALIAS` (the installer writes the workspace host without its port), falling back to the expression above. Inside Docker, services reach Traefik on port 80 (443 in domain mode); a local install on another `--http-port` gets a warning, because a service would call the workspace URL with that port.
- **Installer:** `enable-managed` is the install flow on an existing install with managed services on, keeping the installed version unless `--version` is given (the agent must match the core). `ORGLANE_INSTALL_MANAGED=true` in `.env` records the choice, so re-runs and upgrades keep it; there is no `disable-managed` yet. `--agent-image` overrides the release image; the installer always pulls first and pins the digest Docker records for the pulled tag (`RepoDigests`). Only in local and demo mode (development), if the pull fails, the name has no registry host and the image exists on this machine (built here, e.g. `make image-agent`), is it used as it is, with a warning, the same way `--image` works for the core. Domain mode (production) refuses such an image and changes nothing. `RepoDigests` can't tell a local build apart: with the containerd image store a local build has an entry too. A custom build (`--source` without `--version`) needs `--agent-image`. Domain mode publishes service ports on `0.0.0.0`.
- **Uninstall:** stops the agent first (found by its Compose labels, whether the profile is on or not) and removes nothing unless it is confirmed stopped (it would restart the services), then `docker compose down` for each `instances/<key>/compose.yaml` with project `<COMPOSE_PROJECT_NAME or orglane>-<key>` and the project's `.env` when readable, then the bundle. Directories whose name is not a key are skipped.

## Implementation notes (#61)

Details of the Forgejo package (`providers/forgejo/managed/15.0.11-1`) that the sections above leave open:

- **Version:** Forgejo 15.0, Forgejo's long-term support line (supported until mid-2027), from the rootless image (`codeberg.org/forgejo/forgejo:<version>-rootless`, uid 1000, built-in SSH server on 2222), pinned by the digest of its multi-architecture index. A distribution prefers few major upgrades; the next LTS becomes a new package version with `upgrade.min_from` set when it is embedded. `managed.Current` names the version new instances install.
- **Configuration:** everything is set through `FORGEJO__<section>__<KEY>` variables, which the image writes into `app.ini` (in the data directory) on every start, so a changed render applies with the next start and manual edits of `app.ini` do not survive for these keys. The database is the per-service database on the shared PostgreSQL (`MAX_OPEN_CONNS` 10 under the role's limit of 20). `ROOT_URL` is `URLs.Instance`, `SSH_DOMAIN` the instance host, `SSH_PORT` the published port (`ssh_port`), `SSH_LISTEN_PORT` 2222. Registration is off, signing in is required to see anything, repositories are private by default, the mailer (no SMTP for services yet) and Actions (no runners) are off, and webhooks may only go to the workspace host. Fenced renders also turn off cron and block every webhook host (`orglane-fenced.invalid`).
- **Secrets:** `db_password`, `secret_key` (Forgejo's `SECRET_KEY`, generated by the core and never rotated: Forgejo cannot re-encrypt what it protects) and `admin_password`. Forgejo generates its other internal secrets (internal token, JWT secrets) into `app.ini` on the first start; they are part of the data directory and its snapshots.
- **Service admin:** the configure hook (`files/configure.sh`) makes `orglane-admin` an admin with `admin_password`. It first checks the password against the API with `curl -K -` (the password goes to standard input through a shell builtin, so it never is a process argument); only creating the admin or resetting a password that no longer works passes it to the `forgejo admin user` CLI as an argument, which the CLI requires.
- **Hardening:** read-only root file system with a `/tmp` tmpfs, `user: 1000:1000`, all capabilities dropped, `no-new-privileges`, log rotation (3 × 10 MB).
- **Health:** the container healthcheck is `GET /api/healthz` (start period 5 minutes for the first schema creation); `Lifecycle.CheckHealth` calls the same endpoint through the upstream.
- **Resources:** hints 256 MiB minimum, 512 MiB recommended, 2 GiB disk. Measured idle: about 100 MiB for the Forgejo container after install and setup (the integration test logs it).
- **Data directory owner:** `1000:1000` is applied only when the agent runs as root (the bundle). The in-process agent test (`core/internal/agent`, `TestForgejoPackage`) therefore runs on macOS (Docker Desktop maps ownership) and on Linux as root, and skips on Linux otherwise; the bundle test with the agent container covers Linux in CI.

## Non-goals (M2)

- Kubernetes operator (same contract later, ADR 0006).
- Backups: schedules, S3 targets, test restores, the backups UI (M7). M2 snapshots exist for upgrades and tests.
- Online snapshots (`pre_snapshot`/`post_snapshot` hooks are reserved, not run).
- Upgrading Orglane itself (core and agent images); that stays `docker compose pull && up -d` / the installer.
- Third-party or user-supplied packages, remote package downloads.
- Resource limits on services; multi-host.
- Adopting existing Compose projects or self-hosted installs.

## Decided questions

The open questions of the first draft were answered by the maintainer (2026-11-04) and are applied above:

| Question | Decision | Section |
|---|---|---|
| Installer default for managed services | Off (ADR 0006); Settings → Apps shows the one-line `enable-managed` command | Process model |
| Local mode with managed services | Supported with `*.localhost` names and a Traefik network alias; bare `localhost` is a documented blocker, so local mode with managed services uses `orglane.localhost` | Local mode |
| Package upgrades after an Orglane upgrade | Start automatically, with snapshot, health check and automatic rollback; result visible to admins; admins retry failures | Upgrade |
| Forgejo SSH | In M2, direct published port 2222; key revocation stays M5 | Forgejo SSH |
| Separate agent image | Yes, `ghcr.io/orglane/orglane-agent` | Process model |
