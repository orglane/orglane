# Provider framework (M2 contracts, #57)

This spec defines how the core integrates providers: adapter interfaces, the registry, the capability matrix, the data model, account links, provisioning, webhooks, normalized events, deep links, the API surface, permissions and the test strategy. The managed-service side (agent, packages, upgrades) is in [agent.md](agent.md); the decisions behind it are in [ADR 0019](../adr/0019-agent-transport-and-packages.md).

It builds on [ADR 0002](../adr/0002-capability-provider-instance.md) (capability → provider → instance → binding), [ADR 0004](../adr/0004-permissions-and-roles.md) (permissions), [ADR 0006](../adr/0006-agent-and-desired-state.md), [ADR 0007](../adr/0007-distribution-upgrades-rollback.md), [ADR 0008](../adr/0008-ingress-domains-tunnels.md), [ADR 0010](../adr/0010-events-and-notifications.md) and [ADR 0018](../adr/0018-domain-model.md). Nothing here changes an accepted ADR.

Each later M2 issue cites the sections it implements; see [Issue map](#issue-map).

## Terms

| Term | Meaning in code and docs |
|---|---|
| Capability | `code` in M2. Later `files`, `calendar`, `tasks`, `chat`, ... |
| Provider | An adapter for one external or managed product: `forgejo`, `github` |
| App instance | One concrete service of a provider in the company: a managed Forgejo, or a connected GitHub organization. Called **app instance** in code (`app_instances`, `/api/v1/apps/instances`) because "instance" already means the Orglane installation (`instance_settings`, `instance.read`) |
| Instance key | Short, stable, human-readable ID of an app instance (`git`, `github-acme`). Used in directory names, Compose project names and database names. Immutable |
| Management | `managed` (Orglane runs it through the agent) or `connected` (an external service Orglane talks to) |
| Binding | Capability → app instance, with at most one default per capability |
| Account link | A person ↔ an account at a provider, with proof of ownership |
| Desired access | What a person should have in an app instance, computed from people, teams and mappings |

## Code layout and registry

```
providers/
  provider/            # package provider: contracts only (interfaces, types). No imports from core/internal.
  forgejo/             # Forgejo adapter (#61, #62, #63)
    managed/           # embedded managed package versions (agent.md "Package format")
  github/              # GitHub adapter (#64, #65, #66)
  testprovider/        # in-repo fake provider for framework unit tests only (#58). Not a mock of a real API.
core/internal/apps/    # framework: registry wiring, store access, provisioning and webhook jobs, ingress routes
core/internal/agentapi # wire types shared by the agent server and the core client (agent.md)
```

- Adapters live in the root module under `providers/` (repository layout in AGENTS.md). They cannot import `core/internal/...` (Go's `internal` rule), so the contracts are in `providers/provider` and the core passes services in through the `Env` interface. `providers/provider` is **not** a public SDK: it may change in any release until the provider SDK (v0.2).
- The registry is static: `core/internal/apps/registry.go` lists the compiled adapters in a slice. There is no dynamic loading. `testprovider` is registered only in tests.
- Namespacing: provider IDs match `^[a-z][a-z0-9-]{1,30}$` and are unique. Everything a provider contributes is prefixed by its ID: employee step IDs (`github.link-account`), credential names are scoped by app instance, event types are prefixed by capability (`code.`), audit metadata carries `provider`.

## Adapter contracts

Package `providers/provider`. An adapter implements `CapabilityManifest` and any subset of the optional interfaces; the core discovers them with type assertions. All methods must be safe for concurrent use. Context cancellation must be honored.

```go
package provider

type ID string         // "forgejo", "github"
type Capability string // "code"
type Management string // "managed" | "connected"

// CapabilityManifest is the only required interface.
type CapabilityManifest interface {
    Manifest() Manifest
}

type Manifest struct {
    ID            ID
    Name          string         // English display name; the UI translates by "providers.<id>.name"
    Capabilities  []Capability
    Management    Management     // a provider is either managed or connected in M2
    Package       string         // managed package ID (agent.md); "" for connected providers
    Features      []FeatureSupport
    Credentials   []CredentialSpec
    EmployeeSteps []EmployeeStepSpec
    Settings      []SettingSpec  // non-secret, provider-specific instance settings, validated by the core
    DefaultKey    string         // default instance key, e.g. "git"
    DefaultPolicy AccessPolicy   // all_people | mapped_teams
}

// Instance is the read-only view of an app instance passed to adapters.
type Instance struct {
    ID         uuid.UUID
    Key        string
    Provider   ID
    Management Management
    BaseURL    *url.URL           // public base URL: https://git.acme.com, https://github.com/acme
    Upstream   *url.URL           // managed only: direct URL on the edge network, e.g. http://git-forgejo:3000
    Settings   map[string]any     // validated against Manifest.Settings
}

// Env gives adapters the core services they may use. Nothing else is reachable.
type Env interface {
    // Credential returns the current plaintext of a credential of this instance.
    // Plaintext must not be logged, stored, returned in errors or put in results.
    Credential(ctx context.Context, name string) ([]byte, error)
    // Credentials returns current and not yet retired previous versions, newest
    // first (webhook secret rotation).
    Credentials(ctx context.Context, name string) ([][]byte, error)
    // HTTPClient has timeouts, a response size limit, no cookies and refuses
    // redirects to another host.
    HTTPClient() *http.Client
    Logger() *slog.Logger
    Now() time.Time
    // URLs built from the core configuration (ADR 0018); adapters never build hosts.
    WorkspaceURL() *url.URL
    IssuerURL() *url.URL
    WebhookURL(instanceID uuid.UUID) *url.URL
}
```

### UserProvisioner

```go
type UserProvisioner interface {
    // Plan reads the provider's current state for one person and returns the
    // actions that make it match desired. Plan must not change provider state.
    Plan(ctx context.Context, env Env, inst Instance, d DesiredAccess) (Plan, error)
    // Apply performs one action. It must be idempotent ("ensure" semantics):
    // applying an already satisfied action succeeds without changes.
    Apply(ctx context.Context, env Env, inst Instance, a Action) (ActionOutcome, error)
    // SyncTeams makes the provider-side teams for the instance's team mappings
    // exist and returns their external references. Membership is per person.
    SyncTeams(ctx context.Context, env Env, inst Instance, teams []TeamMapping) ([]TeamRef, error)
    // Members lists the provider-side accounts of the instance (for the
    // "unmanaged accounts" report and drift checks). Paged by the adapter.
    Members(ctx context.Context, env Env, inst Instance) ([]ExternalAccount, error)
}

type DesiredAccess struct {
    PersonID    uuid.UUID
    Username    string        // people.username
    DisplayName string
    WorkEmail   string
    Access      bool          // false: the person must not have access
    Teams       []TeamRef     // provider-side teams the person must be in (exactly these, among mapped teams)
    Link        *AccountLink  // verified link in this provider realm, if any
    Managed     bool          // removal allowed: Orglane granted the access or the instance manages existing members
}

type Plan struct {
    Actions []Action
    // EmployeeStep is set when nothing can be done until the person acts
    // (GitHub: no verified link). Status becomes pending_employee_action.
    EmployeeStep string // e.g. "github.link-account"
}

type ActionKind string // see "Action kinds"

type Action struct {
    Kind   ActionKind
    Key    string            // stable idempotency key, e.g. "add_to_team:<team-id>"
    Params map[string]string // non-secret
    Removal bool             // true for actions that take access away (removal guard)
}

type ActionOutcome struct {
    Changed bool           // false: already satisfied
    Link    *ExternalAccount // set when the action created an account (Forgejo) -> verified link
}
```

Action kinds in M2: `create_account`, `update_account`, `invite`, `cancel_invitation`, `add_to_team`, `remove_from_team`, `remove_from_org`. Offboarding kinds (`suspend_account`, `revoke_tokens`, `remove_ssh_keys`) are reserved for M5.

### Account verification

```go
// AccountVerifier proves that a person owns an account at the provider.
// Providers whose accounts Orglane creates (Forgejo) do not implement it:
// creation is the proof.
type AccountVerifier interface {
    // BeginVerify returns the provider URL the person's browser is sent to.
    // state and the PKCE verifier are generated and stored by the core.
    BeginVerify(ctx context.Context, env Env, inst Instance, state, codeChallenge string) (*url.URL, error)
    // CompleteVerify exchanges the callback parameters for the account that
    // authorized. Any token obtained is used once and revoked; it is never stored.
    CompleteVerify(ctx context.Context, env Env, inst Instance, code, codeVerifier string) (ExternalAccount, error)
}

type ExternalAccount struct {
    Realm      string // "github.com"; for Forgejo the instance ID
    ExternalID string // provider's immutable numeric/opaque user ID, as text
    Login      string // current login, informational (logins change)
}
```

### EventSource

```go
type EventSource interface {
    // DeliveryID returns the provider's unique delivery ID from the headers.
    DeliveryID(h http.Header) (string, error)
    // Verify checks the signature over the raw body with each candidate secret
    // (Env.Credentials("webhook_secret")). Constant-time comparison.
    Verify(ctx context.Context, env Env, inst Instance, h http.Header, body []byte) error // ErrBadSignature
    // Normalize turns a verified delivery into events. It must be a pure
    // function of its input: no provider API calls, no clock except d.ReceivedAt.
    Normalize(inst Instance, d Delivery) ([]Event, error)
    // EnsureWebhooks creates or updates provider-side webhooks pointing at
    // env.WebhookURL(inst.ID). GitHub returns nil (the App carries its webhook).
    EnsureWebhooks(ctx context.Context, env Env, inst Instance) error
}

type Delivery struct {
    ID         string
    EventName  string      // X-GitHub-Event / X-Forgejo-Event
    Headers    http.Header // allowlisted headers only
    Body       []byte
    ReceivedAt time.Time
}
```

### DeepLinker

```go
type DeepLinker interface {
    // OpenURL returns the URL that opens target in the provider, signed in
    // through Orglane when the provider supports it. target is a validated
    // relative path (see "Deep links"). The result's scheme and host must equal
    // inst.BaseURL's; the core checks it again.
    OpenURL(inst Instance, target string) (*url.URL, error)
}
```

### Lifecycle (managed providers)

```go
type Lifecycle interface {
    // Inputs returns the provider-specific template settings and the names of the
    // credentials to pass as secrets to the agent (agent.md "Desired spec").
    Inputs(ctx context.Context, env Env, inst Instance) (Inputs, error)
    // Setup runs once after the first successful install (create the admin
    // API token, the organization, the webhooks). Idempotent; retried on failure.
    Setup(ctx context.Context, env Env, inst Instance) error
    // Verify is the provider-level health check through inst.Upstream. The
    // core calls it to confirm upgrades and on every health sync.
    Verify(ctx context.Context, env Env, inst Instance) error
}

// Connector connects an external provider (GitHub App). Steps are provider specific;
// the core stores the state token and the instance row between them.
type Connector interface {
    BeginConnect(ctx context.Context, env Env, inst Instance, state string) (ConnectStep, error)
    CompleteConnect(ctx context.Context, env Env, inst Instance, step string, params url.Values) (ConnectResult, error)
}

type ConnectResult struct {
    Done        bool
    Next        *ConnectStep        // e.g. "install the App on the organization"
    Settings    map[string]any      // e.g. app_id, installation_id, organization id and login
    Credentials map[string][]byte   // stored encrypted by the core, then zeroed
    BaseURL     *url.URL
}
```

### Capability readers

Readers serve native views and MCP (M3, M6). M2 defines the shape only; no reader is implemented or called in M2.

```go
type CodeReader interface {
    PullRequestsAwaitingReview(ctx context.Context, env Env, inst Instance, acct ExternalAccount) ([]PullRequest, error)
}
```

### Errors

```go
var (
    ErrBadSignature = errors.New("provider: bad webhook signature")
    ErrNotFound     = errors.New("provider: not found")
)

// Error classifies provider failures for retries and results.
type Error struct {
    Code       string        // stable, lower_snake_case: "rate_limited", "seat_limit", "owner_protected", ...
    Retryable  bool
    RetryAfter time.Duration // from Retry-After / rate limit reset, if known
    Err        error         // wrapped cause; must not contain secrets or response bodies
}
```

Adapters return `*Error` for anything the user or admin should see; any other error is treated as retryable with code `provider_error`. Provider response bodies are never copied into errors (they can echo tokens or personal data).

## Capability matrix

Built from `Manifest.Features`; served by the API and shown in Settings → Apps. It is honest: what is not implemented is `unsupported` or `planned`, never omitted.

```go
type Feature string
type Support string // "supported" | "partial" | "planned" | "unsupported"

type FeatureSupport struct {
    Feature   Feature
    Support   Support
    Note      string // i18n key, e.g. "providers.github.features.accounts_invite_only"
    Milestone string // for "planned": "M5"
}
```

Feature keys: `sso`, `deep_links`, `accounts.create`, `accounts.invite`, `accounts.link`, `teams.sync`, `deprovision.team_removal`, `deprovision.org_removal`, `deprovision.suspend`, `deprovision.revoke_tokens`, `events.webhooks`, `lifecycle.install`, `lifecycle.upgrade`, `lifecycle.snapshot`, `lifecycle.backup`.

M2 matrix:

| Feature | Forgejo (managed) | GitHub (connected) |
|---|---|---|
| `sso` | supported (OIDC from Orglane) | unsupported (personal accounts) |
| `deep_links` | supported, silent SSO | partial: direct link, no SSO |
| `accounts.create` | supported | unsupported |
| `accounts.invite` | unsupported (not needed) | supported, needs a linked account |
| `accounts.link` | supported (created by Orglane) | supported (employee step, OAuth) |
| `teams.sync` | supported | supported |
| `deprovision.team_removal` | supported | supported |
| `deprovision.org_removal` | unsupported (single org, account stays) | supported |
| `deprovision.suspend` | planned, M5 | unsupported |
| `deprovision.revoke_tokens` | planned, M5 | unsupported |
| `events.webhooks` | supported | supported |
| `lifecycle.install` / `upgrade` / `snapshot` | supported | unsupported |
| `lifecycle.backup` | planned, M7 | unsupported |

## Data model

Migrations are added by #58 (all tables below) in one migration, with a working `Down`. Conventions as in `core/migrations/00002_identity.sql`: UUIDv7 keys, `timestamptz`, text enums with `CHECK`. Every mutation goes through `core/internal/apps` (or `identity` for account links) and writes an audit entry in the same transaction ([Audit](#audit)).

### app_instances

```sql
CREATE TABLE app_instances (
    id                       uuid PRIMARY KEY DEFAULT uuidv7(),
    key                      text NOT NULL CHECK (key ~ '^[a-z][a-z0-9-]{0,22}[a-z0-9]$'),
    provider                 text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9-]{1,30}$'),
    management               text NOT NULL CHECK (management IN ('managed', 'connected')),
    display_name             text NOT NULL CHECK (btrim(display_name) <> '' AND length(display_name) <= 100),
    state                    text NOT NULL CHECK (state IN ('connecting', 'installing', 'active', 'degraded',
                                 'upgrading', 'failed', 'disconnected', 'removing', 'removed')),
    base_url                 text,                -- set once known
    settings                 jsonb NOT NULL DEFAULT '{}', -- non-secret, validated against Manifest.Settings
    access_policy            text NOT NULL CHECK (access_policy IN ('all_people', 'mapped_teams')),
    manage_existing_members  boolean NOT NULL DEFAULT false,
    -- managed only
    package_id               text,
    package_version          text,                -- running version (observed)
    desired_generation       bigint NOT NULL DEFAULT 0,
    observed_generation      bigint NOT NULL DEFAULT 0,
    health                   text NOT NULL DEFAULT 'unknown' CHECK (health IN ('unknown', 'healthy', 'unhealthy')),
    health_code              text,                -- stable code, no free text from providers
    health_checked_at        timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    removed_at               timestamptz,
    CHECK ((management = 'managed') = (package_id IS NOT NULL))
);
CREATE UNIQUE INDEX app_instances_key_key ON app_instances (key) WHERE state <> 'removed';
```

State machines:

```
managed:   installing ──► active ◄──► degraded
               │            │  ▲
               ▼            ▼  │
             failed       upgrading ──► failed (rollback failed; admin action)
                            │
           active/degraded/failed ──► removing ──► removed

connected: connecting ──► active ◄──► degraded (token or API failures)
               │            │
               ▼            ▼
            removed     disconnected (installation removed at the provider) ──► removed
```

Provisioning and webhook processing run only in `active`, `degraded` and (webhooks only) `upgrading`.

### app_desired_specs (managed only)

The desired state the core pushes to the agent (agent.md). Append-only history; the row with the highest generation is current.

```sql
CREATE TABLE app_desired_specs (
    instance_id      uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    generation       bigint NOT NULL CHECK (generation > 0),
    package_version  text NOT NULL,
    run_state        text NOT NULL CHECK (run_state IN ('running', 'stopped', 'absent')),
    purge            boolean NOT NULL DEFAULT false,
    settings         jsonb NOT NULL,          -- non-secret template settings
    credential_versions jsonb NOT NULL,       -- {"db_password": 1, ...}: which versions to send
    reason           text NOT NULL CHECK (reason IN ('install', 'configure', 'upgrade', 'revert', 'remove', 'resync')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, generation)
);
```

### app_operations (managed only)

Core-side record of agent operations; the source for "upgrade status" in the API and Settings → Apps (#60).

```sql
CREATE TABLE app_operations (
    id                 uuid PRIMARY KEY DEFAULT uuidv7(),
    instance_id        uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    generation         bigint,
    kind               text NOT NULL CHECK (kind IN ('apply', 'upgrade', 'snapshot', 'restore', 'remove')),
    state              text NOT NULL CHECK (state IN ('queued', 'running', 'awaiting_confirmation',
                           'succeeded', 'failed', 'rolled_back')),
    phase              text,          -- agent phase, e.g. "snapshotting", "verifying"
    agent_operation_id uuid,
    error_code         text,
    from_version       text,
    to_version         text,
    started_by         uuid REFERENCES people (id),   -- NULL for system
    created_at         timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz
);
CREATE INDEX app_operations_instance_idx ON app_operations (instance_id, created_at DESC);
```

### app_bindings

```sql
CREATE TABLE app_bindings (
    capability  text NOT NULL CHECK (capability ~ '^[a-z][a-z0-9_.]{0,40}$'),
    instance_id uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    is_default  boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (capability, instance_id)
);
CREATE UNIQUE INDEX app_bindings_default_key ON app_bindings (capability) WHERE is_default;
```

An instance gets a binding for each capability of its provider when it becomes `active`; the first one per capability is the default. M2 has both Forgejo and GitHub bound to `code` (the golden scenario uses both).

### app_credentials

```sql
CREATE TABLE app_credentials (
    instance_id uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    name        text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_]{0,40}$'),
    version     integer NOT NULL CHECK (version > 0),
    ciphertext  bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    retire_at   timestamptz,          -- previous versions stay valid until then
    PRIMARY KEY (instance_id, name, version)
);
```

See [Credentials](#credentials).

### account_links

Only **verified** links (and their revoked history). Unverified evidence from the M1 GitHub import stays in `pending_account_links` (migration 00012), which this spec does not change; see [Account links](#account-links).

```sql
CREATE TABLE account_links (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    person_id    uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    provider     text NOT NULL,
    realm        text NOT NULL,   -- "github.com"; Forgejo: the app instance ID
    external_id  text NOT NULL CHECK (length(external_id) BETWEEN 1 AND 100),
    login        text NOT NULL CHECK (length(login) BETWEEN 1 AND 100),
    method       text NOT NULL CHECK (method IN ('oauth', 'provisioned')),
    instance_id  uuid REFERENCES app_instances (id) ON DELETE SET NULL, -- where it was established
    state        text NOT NULL DEFAULT 'verified' CHECK (state IN ('verified', 'revoked')),
    evidence     text CHECK (evidence IN ('matched', 'mismatched', 'none')), -- vs pending_account_links
    verified_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);
CREATE UNIQUE INDEX account_links_account_key ON account_links (provider, realm, external_id) WHERE state = 'verified';
CREATE UNIQUE INDEX account_links_person_key  ON account_links (person_id, provider, realm) WHERE state = 'verified';
```

### app_team_mappings

```sql
CREATE TABLE app_team_mappings (
    instance_id   uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    team_id       uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    external_ref  text,           -- provider team ID once created or chosen
    external_name text,           -- provider team name/slug, informational
    mode          text NOT NULL CHECK (mode IN ('create', 'existing')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, team_id)
);
```

`create`: Orglane creates a provider team named after the Orglane team slug. `existing`: the admin picks an existing provider team (GitHub). Archived Orglane teams keep their mapping but grant nothing.

### provisioning_results

One row per person × app instance that has, or had, desired access or a link.

```sql
CREATE TABLE provisioning_results (
    instance_id      uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    person_id        uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    status           text NOT NULL CHECK (status IN ('queued', 'done', 'pending_employee_action', 'failed')),
    access           boolean NOT NULL,          -- desired access at the last plan
    employee_step    text,                      -- set when pending_employee_action
    granted_by_orglane boolean NOT NULL DEFAULT false,
    desired_hash     bytea,                     -- SHA-256 of the canonical DesiredAccess
    applied_hash     bytea,                     -- desired_hash when the last apply succeeded
    actions          jsonb NOT NULL DEFAULT '[]', -- last plan: [{kind, key, outcome}], no secrets
    error_code       text,
    attempts         integer NOT NULL DEFAULT 0,
    last_attempt_at  timestamptz,
    next_attempt_at  timestamptz,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, person_id)
);
CREATE INDEX provisioning_results_person_idx ON provisioning_results (person_id);
```

### webhook_deliveries

```sql
CREATE TABLE webhook_deliveries (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    instance_id  uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    delivery_id  text NOT NULL CHECK (length(delivery_id) BETWEEN 1 AND 100),
    event_name   text NOT NULL CHECK (length(event_name) <= 100),
    received_at  timestamptz NOT NULL DEFAULT now(),
    body         bytea,              -- sealed; NULL after retention
    headers      jsonb NOT NULL,     -- allowlisted headers only, never Authorization/Cookie
    body_sha256  bytea NOT NULL,
    status       text NOT NULL DEFAULT 'received' CHECK (status IN ('received', 'processed', 'ignored', 'failed')),
    error_code   text,
    event_count  integer NOT NULL DEFAULT 0,
    processed_at timestamptz
);
CREATE UNIQUE INDEX webhook_deliveries_dedupe_key ON webhook_deliveries (instance_id, delivery_id);
```

### events

Append-only, like `audit_entries`: a trigger rejects `UPDATE`, `DELETE` and `TRUNCATE`, except deletes by the retention job when M3 defines retention (M2 has no retention for events).

```sql
CREATE TABLE events (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    type           text NOT NULL CHECK (type ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){2}$'),
    schema_version integer NOT NULL CHECK (schema_version > 0),
    instance_id    uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    provider       text NOT NULL,
    delivery_id    uuid REFERENCES webhook_deliveries (id) ON DELETE SET NULL,
    dedupe_key     text NOT NULL,
    occurred_at    timestamptz NOT NULL,
    received_at    timestamptz NOT NULL,
    actor          jsonb NOT NULL,   -- ExternalRef
    subject        jsonb NOT NULL,   -- Subject
    targets        jsonb NOT NULL DEFAULT '[]', -- []Target
    data           jsonb NOT NULL DEFAULT '{}'
);
CREATE UNIQUE INDEX events_dedupe_key ON events (instance_id, dedupe_key);
CREATE INDEX events_instance_time_idx ON events (instance_id, occurred_at DESC);
```

Events reference **external accounts**, not people. Recipients are resolved through account links at delivery time in M3 (ADR 0010).

## Credentials

- Stored only in `app_credentials`, sealed with `crypto.Sealer` under a new purpose `crypto.PurposeAppCredentials = "orglane/v1/app-credentials"`. Additional data binds the ciphertext to its row: `"app-credential\x00" + instance_id + "\x00" + name + "\x00" + version`. Webhook bodies use a separate purpose `PurposeWebhookDeliveries = "orglane/v1/webhook-deliveries"` with AD `instance_id + "\x00" + delivery row id`.
- Plaintext lives only in memory for the duration of a call. It never appears in API responses, logs, audit metadata, job arguments, errors or `provisioning_results`. Job arguments carry IDs only.
- Credential names per provider (declared in `Manifest.Credentials`):

  | Provider | Name | Source | Rotation in M2 |
  |---|---|---|---|
  | forgejo | `db_password` | generated by the core | no (reinstall) |
  | forgejo | `admin_password` | generated by the core; used once by `Setup` to mint the token | no |
  | forgejo | `admin_token` | minted by `Setup` through the Forgejo API | yes: mint new, retire old |
  | forgejo | `oidc_client_secret` | identity OIDC client of this instance | no |
  | forgejo | `webhook_secret` | generated by the core | yes, 24 h overlap |
  | github | `app_private_key` | manifest conversion; replaced by an admin upload | yes: admin uploads a new key |
  | github | `app_client_secret` | manifest conversion | no (regenerate in GitHub, upload: later) |
  | github | `webhook_secret` | manifest conversion; replaced by the core | yes, 24 h overlap |

- **Rotation:** a new version is inserted; the previous one gets `retire_at` (now + overlap, default 24 h for webhook secrets, now for others). The adapter then pushes the new value to the provider (Forgejo: update the org hook; GitHub: `PATCH /app/hook/config` with the App JWT), in a retried job. Readers use the highest version; `Env.Credentials` also returns versions whose `retire_at` is in the future, so webhook verification accepts both during the overlap. A daily cleanup job deletes retired versions. Each rotation writes `app_credential.rotated` with name and version only.
- **Master secret:** credentials depend on `identity.master_secret` like every other sealed column. It must never change (existing rule); re-encryption under a new master secret is out of scope.
- Generated secrets are 32 random bytes in unpadded base64url (`crypto.RandomToken("")`), which also satisfies the agent's secret input format (agent.md).

## Account links

**What proves ownership:**

| Provider | Proof | Method |
|---|---|---|
| GitHub | The person completes GitHub's OAuth authorization for the instance's GitHub App, from their own Orglane session, with `state` bound to that session and person, and PKCE (S256). The core reads `GET /user` with the user token, then revokes the token. | `oauth` |
| Forgejo (managed) | Orglane created the account through the admin API with the Orglane OIDC source and the person's OIDC subject as login name. | `provisioned` |

Nothing else creates a verified link: not an email match, not the M1 import, not an admin assertion. Admins with `people.manage` can **revoke** a link (wrong account), never create one.

**Resolving M1 `pending` links (#65):**

1. `pending_account_links` rows (provider `github`, `organization_id`, `external_id`, `person_id`, `login`) are import evidence. They never grant access, never trigger invitations and never authorize removals.
2. When a GitHub instance is active for organization ID `X`, the API and the onboarding step show the person's pending evidence for `X` ("we expect @login") alongside the link state. The union is exposed as `state: pending | verified`.
3. When the person verifies: if the authorized GitHub user ID equals a pending `external_id` of that person for `X`, the link is stored with `evidence = matched`. If it differs, the link is still stored (proof wins) with `evidence = mismatched`, and the admin view shows both. If the authorized ID is pending evidence of **another** person, the link is still stored for the authenticated person; the other person's evidence is shown as conflicting.
4. Pending rows are not deleted by M2; the import lifecycle owns them.

**Uniqueness:** one verified link per provider account, one per person per realm. Verifying an account already linked to another person fails with `account_already_linked`; an admin must revoke the other link first. Verifying a different account for the same person revokes the old link in the same transaction. The old account then becomes unmanaged: it is reported, not removed.

**Login changes:** links are keyed by the immutable external ID. Adapters refresh `login` when they observe a change (API reads, webhook payloads) and resolve the current login from the ID before calls that need a login.

## Desired access and team mappings

Computed by `core/internal/apps` from the directory, never stored as truth (only its hash in `provisioning_results`):

```
for each app instance in (active, degraded) whose adapter implements UserProvisioner:
  for each person:
    eligible := person.status == 'active'
    teams    := mapped provider teams of the person's direct, non-archived team memberships
    access   := eligible && (instance.access_policy == 'all_people' || len(teams) > 0)
    managed  := result.granted_by_orglane || instance.manage_existing_members || instance.management == 'managed'
```

- `invited` people get access when they accept the invitation and become `active`. `suspended` and `offboarded` people have `access = false`; in M2 that removes team memberships (both providers) and the GitHub organization membership. Suspension, token and SSH key revocation are M5.
- Team hierarchy: only direct memberships count in M2.
- No role elevation: Orglane never makes anyone a provider admin or org owner in M2.
- Defaults: Forgejo `all_people`; GitHub `mapped_teams` (seats cost money).

## Provisioning

### Flow

```
directory change (person status, team membership, mapping, link verified, instance activated)
  → in the same transaction: enqueue orglane.apps.provision_person {instance_id, person_id}
    for every affected pair (River InsertTx; the transactional outbox)
  → job: compute DesiredAccess → hash; if hash == applied_hash and status == done and no drift check due: stop
  → adapter.Plan → actions (or EmployeeStep)
  → for each action in order: removal guard → adapter.Apply → audit entry if Changed
  → write provisioning_results (status, actions, error_code, applied_hash)
```

- `orglane.apps.provision_instance {instance_id, reason}`: calls `SyncTeams`, then enqueues `provision_person` for every person with access, a result row or a link. Runs when an instance becomes active, when a mapping changes, and daily (drift check). It also refreshes the "unmanaged accounts" report from `Members`.
- Jobs run on a dedicated River queue `apps` (2 workers by default) so provider slowness never blocks email or identity jobs.
- At most one queued and one running job per (instance, person): uniqueness by args over the states available/scheduled/retryable, plus a transaction advisory lock on (instance, person) while applying. A change during a run enqueues one follow-up.

### Results

| Status | Meaning |
|---|---|
| `queued` | Change detected, job not finished yet |
| `done` | Provider state matches desired access |
| `pending_employee_action` | Nothing can be done until the person acts (`employee_step` names the step) |
| `failed` | An action failed permanently or retries are exhausted (`error_code`) |

### Idempotency, retries, partial failure

- Every action is an "ensure" and must succeed without changes when already satisfied (`ActionOutcome.Changed = false`). Rerunning a job after a crash at any point converges.
- Retryable errors: River retries with backoff, at most 8 attempts. `RetryAfter` snoozes the job until the provider's reset (GitHub rate limits) without spending an attempt. Non-retryable errors (`seat_limit`, `owner_protected`, `invalid_login`, ...) mark the result `failed` immediately; it is retried when desired access changes or an admin presses Retry.
- Partial failure: people are independent. Within one person, actions run in plan order and stop at the first failure; applied actions are not undone (they are idempotent), and the next run continues from a fresh plan. The result lists each action's outcome (`changed`, `unchanged`, `failed`, `not_attempted`, `held`).

### Removal guard

Removing access by mistake is worse than keeping it a little longer:

- Removal actions are applied only when `DesiredAccess.Managed` is true: Orglane granted the access (`granted_by_orglane`), or the admin enabled `manage_existing_members` for the instance after a dry-run preview. Pre-existing members that Orglane does not manage are reported as unmanaged, never removed.
- Bulk guard: if one `provision_instance` sweep would remove access from more than max(3, 10 %) of the instance's managed accounts, removal actions are `held` and the instance shows a confirmation for `apps.manage`. Confirmation releases exactly the held set.
- GitHub organization owners are never removed (`owner_protected`).

### Employee steps

- Declared in `Manifest.EmployeeSteps` with a namespaced ID (`github.link-account`) and an action path (`/me/accounts/github`).
- The core registers one `onboarding.Step` per declared step. `Check` returns nil when the person has no desired access to any instance of the provider, `Complete: true` when a verified link exists in the realm, and otherwise an incomplete step with the action URL (a local path, as `onboarding.Registry` requires).
- Provider steps are **not required** for finishing welcome in M2 (the person may need to create a GitHub account first); see Open questions.
- Completing the step (link verified) enqueues `provision_person` for the person in every instance of that realm.

## Webhooks

### Endpoint

`POST /api/v1/webhooks/{instanceId}` on the workspace host, `x-orglane-permission: public`. The provider's signature is the authentication. The handler needs the exact raw body for verification and must not decode JSON before it. The contract declares the body as an opaque binary string; if the generated strict handler cannot hand over raw bytes for `application/json`, #58 registers the route as a plain handler and keeps it in the contract with its permission, so the permission check at startup still covers it.

| Step | Rule |
|---|---|
| Instance | Unknown, `removed` or `connecting` → `404`. The response does not say which |
| Size | Body over 5 MiB → `413`, nothing stored |
| Headers | Only an allowlist is kept (`X-GitHub-Event`, `X-GitHub-Delivery`, `X-GitHub-Hook-Installation-Target-ID`, `X-Forgejo-Event`, `X-Forgejo-Delivery`, `Content-Type`, `User-Agent`) |
| Signature | `EventSource.Verify` over the raw body, constant time, each candidate secret. Failure → `401`, nothing stored, a rate-limited warning log without body or headers |
| GitHub only | The installation target ID must equal the instance's installation; otherwise `401` |
| Dedupe | `INSERT ... ON CONFLICT (instance_id, delivery_id) DO NOTHING`. New → enqueue `orglane.apps.process_delivery {delivery_row_id}` in the same transaction, answer `202`. Duplicate → `200`, no job |

The handler does no provider API calls and no normalization, so it answers within milliseconds (GitHub expects an answer within 10 seconds).

Signatures:

| Provider | Header | Algorithm | Delivery ID |
|---|---|---|---|
| GitHub | `X-Hub-Signature-256: sha256=<hex>` | HMAC-SHA256 of the raw body with the App's webhook secret | `X-GitHub-Delivery` (GUID) |
| Forgejo | `X-Forgejo-Signature: <hex>` | HMAC-SHA256 of the raw body with the hook secret | `X-Forgejo-Delivery` (UUID) |

### Processing

`process_delivery` opens the sealed body, calls `Normalize`, inserts events (`ON CONFLICT (instance_id, dedupe_key) DO NOTHING`) and sets the delivery `processed` (or `ignored` for event names the adapter does not normalize) in one transaction. Normalization errors are permanent: `failed` with an error code, no retry. Database errors retry.

### Untrusted input rules

- Payloads are untrusted even after verification (a provider user controls titles, bodies and logins).
- JSON is decoded with size limits into typed structs; unknown fields are ignored.
- Strings are validated or truncated: titles 256 runes, excerpts 280 runes, logins against the provider's login pattern, repository names against `owner/name`.
- URLs from payloads are kept only if their scheme and host equal the instance's base URL host (GitHub: `github.com`), else dropped. They are never fetched by the core.
- No HTML or Markdown is rendered or stored as markup; excerpts are plain text.
- Bodies and headers are never logged.

### Replay and retention

- Replays of a captured delivery are rejected by the delivery ID dedupe for as long as the delivery row exists (30 days). Neither provider signs a timestamp, so older replays pass verification but produce no duplicate events thanks to the event dedupe key; they are visible as new deliveries.
- Retention (daily job `orglane.apps.webhook_cleanup`): `body` is set to NULL 7 days after processing (30 days for `failed`); delivery rows are deleted after 30 days.

## Normalized events

Stored now; delivery (Inbox, push) is M3. Types in M2:

| Type | Source | Subject | Targets |
|---|---|---|---|
| `code.pull_request.opened` | Forgejo, GitHub | pull request | requested reviewers (`review_requested`), assignees (`assigned`) |
| `code.review.requested` | Forgejo, GitHub | pull request | the requested reviewer (`review_requested`) |
| `code.mention.created` | Forgejo, GitHub (PR/issue bodies and comments) | pull request or issue | mentioned logins (`mentioned`), at most 50 |
| `code.branch.pushed` | Forgejo | repository branch | none |
| `code.membership.changed` | GitHub (`member`, `membership`, `organization`) | organization or team | the affected account (`member_added`, `member_removed`, `member_invited`) |

```go
type Event struct {
    Type          string
    SchemaVersion int        // 1
    DedupeKey     string     // default: "<delivery_id>/<index>"; poll sources define their own
    OccurredAt    time.Time
    Actor         ExternalRef
    Subject       Subject
    Targets       []Target
    Data          map[string]any // type-specific, documented per type, small
}

type ExternalRef struct {
    ExternalID string `json:"external_id,omitempty"` // omitted when only a login is known (mentions)
    Login      string `json:"login"`
}

type Subject struct {
    Kind       string `json:"kind"`        // "pull_request" | "issue" | "repository" | "branch" | "organization" | "team"
    ExternalID string `json:"external_id"` // provider's stable ID
    Repository string `json:"repository,omitempty"` // "owner/name": the visibility reference for M3 checks
    Number     int    `json:"number,omitempty"`
    Title      string `json:"title,omitempty"`
    URL        string `json:"url,omitempty"` // validated, see untrusted input rules
}

type Target struct {
    ExternalRef
    Reason string `json:"reason"` // "review_requested" | "mentioned" | "assigned" | "member_added" | ...
}
```

Mention extraction: `@login` tokens outside fenced and inline code, login pattern of the provider, deduplicated, at most 50 per event. Self-mentions are dropped.

## Deep links and silent SSO

### Contract

`GET /api/v1/apps/instances/{instanceId}/open?to=<path>` → `{"url": "..."}`, permission `authenticated`. The caller must be an `active` person and the instance `active` or `degraded`. Clients open the returned URL in a new tab (web) or the external browser (mobile handoff in M4 wraps it).

Validation of `to` (shared helper in `core/internal/apps`, unit tested with the same corpus as `onboarding.Registry` action URLs):

- 1–2048 bytes; starts with `/`; does not start with `//`; contains no `\`, no ASCII control characters and no spaces; parses as a URL with empty scheme, user info and host. Query and fragment are kept.
- The adapter's result is re-parsed; its scheme and host must equal the instance base URL's, else `500` (adapter bug) and a log entry. The core never accepts an absolute URL from a client.
- GitHub additionally requires the path to be under `/<org-login>/`.

### Forgejo: sign in and go

`OpenURL` returns Forgejo's OAuth start URL for the Orglane source with the target as `redirect_to`, for example `https://git.acme.com/user/oauth2/orglane?redirect_to=%2Facme%2Frobot%2Fpulls%2F3`. Forgejo redirects to Orglane's `/oidc/authorize`; Orglane sees the existing session and, because first-party clients get no consent screen, redirects straight back (`core/internal/identity/web/login.go`). Forgejo signs the person in and redirects to the relative `redirect_to`. Without an Orglane session the person signs in once and continues. #62 confirms the exact start URL against the pinned Forgejo version (fallback: `/user/login?redirect_to=` with the Orglane source as the only sign-in method).

Open-redirect protection is layered: Orglane validates `to`; the OIDC client has exact `redirect_uris`; Forgejo only follows relative `redirect_to` values.

### Forgejo OIDC configured automatically (#62)

1. On install, the core creates OIDC client `app-<key>` with `identity.CreateOIDCClient` (confidential, `client_secret_basic`, redirect URI `https://<forgejo host>/user/oauth2/orglane/callback`, audited) and stores the secret as credential `oidc_client_secret`.
2. The secret and the issuer URL go to the agent as inputs. The package's `configure` hook runs `forgejo admin auth add-oauth` (or `update-oauth` when the source exists) with `--provider openidConnect` and `--auto-discover-url <issuer>/.well-known/openid-configuration`, reading the secret from the container environment (agent.md "Hooks").
3. Forgejo settings in the template: registration only through the external source, no local sign-in form for people (the service admin keeps API access), username from `preferred_username`.
4. Accounts are pre-created by provisioning with `source_id` of that source and `login_name` = the person's OIDC subject, so the first SSO sign-in matches the existing account.
5. The Forgejo container reaches the issuer through Traefik under its public name (agent.md "Networking").

### GitHub

GitHub accounts are personal; there is no SSO. `OpenURL` returns `https://github.com` + `to`. The matrix says so.

## Managed instance lifecycle in the core

The core side of [agent.md](agent.md):

1. `POST /api/v1/apps/instances` (managed provider) validates the key, the host (`config.ServiceHost(<route host label>)`, overridable per instance; it must be a valid hostname, distinct from the workspace host and from other instances' hosts) and, in domain mode, that the host resolves to the same addresses as the workspace host (as the installer does). It checks agent health and the package's resource hints against the host's reported memory and disk.
2. It creates the instance (`installing`), the credentials, the OIDC client (Forgejo) and desired spec generation 1, and enqueues `orglane.apps.agent_apply {instance_id, generation}`.
3. `agent_apply` pushes the spec and polls the operation (snoozing between polls) until it is terminal, mirroring it in `app_operations`.
4. On success the instance becomes `active`, `Lifecycle.Setup` runs (job, retried), the ingress gets the instance's routes, bindings are created and `provision_instance` is enqueued.

**Ingress routes (#61):** `core/internal/ingress` builds routes from the active managed instances in addition to the workspace router: one router per manifest route, named `orglane-app-<key>-<route>`, host from the instance, upstream `http://<key>-<service>:<port>` (the service's alias on the edge network), the same security headers, TLS and HTTP→HTTPS redirect as the workspace in domain mode. The dynamic configuration is therefore no longer static: the internal handler rebuilds it from the database on each poll (cheap, Traefik polls every 10 s). While an instance is `upgrading`, its routers point to the core, which answers `503` with `Retry-After` (agent.md "Upgrade").

## API

All under `/api/v1`, contract-first in `core/api/openapi.yaml`. Errors are RFC 9457 problem details with a stable `code`. Every operation has allowed and denied tests.

| Method | Path | Permission | Purpose | Issue |
|---|---|---|---|---|
| GET | `/apps/providers` | `apps.manage` | Compiled providers with capability matrix and settings schema | #58 |
| GET | `/apps/instances` | `apps.manage` | List app instances with state, health, version | #58 |
| POST | `/apps/instances` | `apps.manage` | Install a managed provider (202 + operation) | #61 |
| GET | `/apps/instances/{instanceId}` | `apps.manage` | Detail: state, settings, bindings, last operations, unmanaged account count | #58 |
| PATCH | `/apps/instances/{instanceId}` | `apps.manage` | Display name, access policy, `manage_existing_members`, provider settings | #58 |
| DELETE | `/apps/instances/{instanceId}` | `apps.manage` | Remove; `?purge=true` also deletes data (requires `confirm` = instance key) | #59/#61 |
| GET | `/apps/instances/{instanceId}/operations` | `apps.manage` | Operation history (install, upgrades, rollbacks) | #60 |
| POST | `/apps/instances/{instanceId}/upgrade` | `apps.manage` | Retry a failed or start a pending upgrade | #60 |
| GET | `/apps/instances/{instanceId}/logs` | `apps.manage` | Bounded service logs, redacted; audited | #59 |
| POST | `/apps/instances/{instanceId}/credentials/{name}/rotate` | `apps.manage` | Rotate a rotatable credential | #58 |
| PUT | `/apps/instances/{instanceId}/credentials/{name}` | `apps.manage` | Upload a replacement (GitHub private key) | #64 |
| GET | `/apps/bindings` | `apps.manage` | Capability bindings | #58 |
| PUT | `/apps/bindings/{capability}` | `apps.manage` | Set the default instance of a capability | #58 |
| GET | `/apps/instances/{instanceId}/team-mappings` | `apps.manage` | Mappings with provider team names | #58 |
| PUT | `/apps/instances/{instanceId}/team-mappings/{teamId}` | `apps.manage` | Create or change a mapping | #58 |
| DELETE | `/apps/instances/{instanceId}/team-mappings/{teamId}` | `apps.manage` | Remove a mapping | #58 |
| GET | `/apps/instances/{instanceId}/provisioning` | `apps.manage` | Results for all people (paged), unmanaged accounts, held removals | #58 |
| POST | `/apps/instances/{instanceId}/provisioning/plan` | `apps.manage` | Dry run: planned actions per person, no changes | #58 |
| POST | `/apps/instances/{instanceId}/provisioning/retry` | `apps.manage` | Retry failed results (all or `personId`) | #58 |
| POST | `/apps/instances/{instanceId}/provisioning/confirm-removals` | `apps.manage` | Release held removals | #58 |
| GET | `/apps/agent` | `apps.manage` | Agent connected, version, host resources | #59 |
| POST | `/apps/github/connect` | `apps.manage` | Start the GitHub App manifest flow; returns the form to post to GitHub | #64 |
| POST | `/apps/github/connect/complete` | `apps.manage` | Manifest code or installation callback (`state`, `code` / `installation_id`) | #64 |
| GET | `/people/{personId}/apps` | `people.manage` | Provisioning results and links (pending and verified) of one person | #58 |
| DELETE | `/people/{personId}/account-links/{linkId}` | `people.manage` | Revoke a wrong link | #65 |
| GET | `/me/apps` | `authenticated` | Instances the caller has access to, with home URLs | #58 |
| GET | `/me/account-links` | `authenticated` | Caller's links (pending evidence and verified) | #65 |
| POST | `/me/account-links/{provider}/start` | `authenticated` | Begin verification; returns the provider URL | #65 |
| POST | `/me/account-links/{provider}/complete` | `authenticated` | Finish verification with `state` and `code` (called by the SPA callback route) | #65 |
| DELETE | `/me/account-links/{linkId}` | `authenticated` | Unlink own account | #65 |
| GET | `/apps/instances/{instanceId}/open` | `authenticated` | Deep link (`?to=`) | #62 |
| POST | `/webhooks/{instanceId}` | `public` | Provider webhooks | #58 |

Browser callbacks from providers land on SPA routes (`/settings/apps/github/callback`, `/me/accounts/github/callback`), which call the `complete` operations. No product endpoint outside `/api/v1` is added.

## Permissions

No new permission is needed in M2. ADR 0004 already names `apps.manage`, and the registry has it.

| Permission | Covers |
|---|---|
| `apps.manage` | Everything under `/apps/...`: install, connect, configure, upgrade, remove, mappings, provisioning views and actions, logs, credentials |
| `people.manage` | A person's app access and links on the person page; revoking a wrong link |
| `security.manage` | Reading the audit log, which includes all app entries |
| `authenticated` | Own links, own apps, deep links |
| `public` | Webhooks (authenticated by provider signature) |

Owner and Admin hold `apps.manage` already; Member holds none of these.

## Audit

New actions (`core/internal/audit`), all in the transaction of the change; metadata never contains secrets, tokens or payloads:

`app_instance.created`, `app_instance.updated`, `app_instance.removed`, `app_instance.upgrade_started`, `app_instance.upgraded`, `app_instance.rolled_back`, `app_instance.logs_read`, `app_credential.created`, `app_credential.rotated`, `app_binding.set`, `app_team_mapping.set`, `app_team_mapping.removed`, `account_link.verified`, `account_link.revoked`, `provisioning.action_applied` (target: the person; metadata: instance, action kind, key; only when `Changed`), `provisioning.removals_confirmed`.

New target kinds: `app_instance`, `account_link`. System jobs use `audit.System`.

## Provider specifics

### Forgejo (#61, #62, #63)

- Package and install: [agent.md "Package format"](agent.md#package-format). Instance key default `git`, host label `git`.
- `Setup`: create the service admin (done by the package's `configure` hook with `admin_password`), mint `admin_token` through the API with basic auth, create the organization (setting `organization`, default derived from the workspace name, editable before the first sync), create the org webhook (`EnsureWebhooks`).
- Accounts: `POST /api/v1/admin/users` with `source_id`, `login_name` = OIDC subject, `username` = `people.username`, `email` = work email, `must_change_password = false`. The response's user ID becomes a `provisioned` link (realm = instance ID).
- Teams: `create` mappings only; `SyncTeams` creates org teams named after Orglane team slugs.
- Webhooks: one org webhook, content type JSON, events: pull request, pull request review request, issue comment, push. Forgejo blocks webhooks to private addresses by default, so the template allows the workspace host (`[webhook] ALLOWED_HOST_LIST`).

### GitHub (#64, #65, #66)

- Connect: the GitHub App **manifest flow** for the organization. The core creates the instance (`connecting`) first, so the manifest can carry `https://<workspace>/api/v1/webhooks/<instanceId>` as the webhook URL, the SPA callback routes as redirect, setup and callback URLs, and the minimal permissions. The manifest code (valid one hour) is exchanged for app ID, slug, client ID and secret, webhook secret and private key, which are stored as credentials. Then the admin installs the App on the organization; the core verifies the installation with `GET /app/installations/{id}` (App JWT) and that its account is the expected organization, and stores organization ID, login and installation ID in `settings`. Manual setup (admin creates the App and pastes the values) is a documented fallback.
- Minimal permissions (candidate set; #64 verifies each against GitHub's documentation and documents the result in `docs/github.md`): organization **Members: read and write** (invitations, memberships, teams); repository **Metadata: read** (mandatory), **Pull requests: read**, **Issues: read** (comments for mentions). Webhook events: `pull_request`, `pull_request_review`, `issue_comment`, `pull_request_review_comment`, `member`, `membership`, `organization`.
- Tokens: App JWT (RS256, `iat` 60 s in the past, `exp` ≤ 10 min) signed with go-jose (already a dependency); installation tokens minted on demand, cached in memory until 5 minutes before expiry, never persisted.
- Invitations by user ID (`POST /orgs/{org}/invitations` with `invitee_id`), so a login change cannot redirect an invitation. Removal with `DELETE /orgs/{org}/memberships/{username}` after resolving the current login from the ID (also cancels pending invitations). Team membership with the team membership endpoints. Organization owners are never removed.
- Users without a verified link: `pending_employee_action` with step `github.link-account`.
- Webhook deliveries whose installation target ID differs from the instance's are refused.

## Testing strategy

**Rule (AGENTS.md):** integration tests use real services; no mocks of provider APIs. The in-repo `testprovider` exercises the framework (jobs, results, retries, guard) without pretending to be a real provider.

| Area | Tests | Where |
|---|---|---|
| Framework | Unit tests with `testprovider`: plan/apply ordering, idempotent rerun, partial failure, retry classification, snooze on `RetryAfter`, removal guard, desired access computation, `to` validation corpus, webhook verification and dedupe | #58 |
| Store | Integration tests (`pgtest`) for every table: constraints, unique links, append-only events, credential sealing and AD binding, migration up/down | #58 |
| API | Allowed and denied tests for every operation; audit entries per mutation | #58 onward |
| Forgejo | testcontainers with the **same image reference** as the package manifest (read from the embedded manifest in the test): accounts, teams, webhooks with real signatures, deep link through a real OIDC round trip; agent install path in `tests/integration` | #61–#63 |
| GitHub | Live tests against a real test organization and test GitHub App (below) | #64–#66 |
| Web | Playwright: Settings → Apps, install flow, connect up to the GitHub redirect, the link step up to the GitHub redirect | #58, #61, #64, #65 |

### GitHub live tests

Build tag `integration`, package `providers/github`, plus a scenario in `tests/integration` (#67). They need these secrets, provided by the maintainer as a GitHub Actions **environment** `github-live` restricted to `main` and manual dispatch (never exposed to pull requests from forks, ADR 0017):

| Secret | Content |
|---|---|
| `ORGLANE_TEST_GITHUB_ORG` | Login of a dedicated test organization (no real code or people) |
| `ORGLANE_TEST_GITHUB_APP_ID` | App ID of a dedicated test GitHub App installed on that organization |
| `ORGLANE_TEST_GITHUB_APP_CLIENT_ID` | Its client ID |
| `ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY` | Its private key (PEM) |
| `ORGLANE_TEST_GITHUB_INSTALLATION_ID` | The installation on the test organization |
| `ORGLANE_TEST_GITHUB_WEBHOOK_SECRET` | The App's webhook secret |
| `ORGLANE_TEST_GITHUB_USER_LOGIN`, `ORGLANE_TEST_GITHUB_USER_ID` | A dedicated test user account (not an org owner) that tests invite, add to teams and remove |
| `ORGLANE_TEST_GITHUB_USER_TOKEN` | A fine-grained token of that test user, only to accept invitations and to open PRs/comments that trigger webhooks |

Behavior:

- Locally and on pull requests, missing secrets skip the tests with `t.Skip("GitHub live tests need ORGLANE_TEST_GITHUB_* (see docs/specs/providers.md#github-live-tests)")`.
- A workflow `github-live.yml` runs them on pushes to `main`, nightly and by `gh workflow run github-live.yml --ref <branch>` (how a maintainer checks a GitHub PR before merging, like `conformance.yml`). It sets `ORGLANE_REQUIRE_GITHUB_LIVE=1`, which turns a missing secret into a failure.
- Runs are serialized (`concurrency: github-live`). Every test names its teams and repositories `orglane-test-<run id>-...` and deletes them; a janitor step removes leftovers older than a day.
- Webhooks cannot reach CI runners. Live webhook tests trigger a real event in the test organization, read the App's delivery log (`GET /app/hook/deliveries`, App JWT) until the delivery appears, and post the recorded headers and body to the local endpoint: real GitHub signatures, no mock. The test App's webhook URL points to an unroutable host.
- Fixture tests (#66) commit real deliveries recorded from the test organization, with their original signature headers. That requires the test App's webhook secret to be committed as a test constant, which is acceptable only because the App's webhook URL is unroutable and the organization holds no real data (see Open questions).
- The OAuth browser leg of "Link your GitHub" cannot be automated without signing in to GitHub as the test user in a browser. Tests cover the start URL (PKCE, state), the callback handling with a real user token from `ORGLANE_TEST_GITHUB_USER_TOKEN` against `GET /user`, and everything after verification. The full browser round trip is a documented manual check in #65 (see Open questions).

## Non-goals (M2)

- Kubernetes operator (cloud; implements the agent contract later).
- Public provider SDK, conformance suite and generators (v0.2). `providers/provider` is internal-quality API.
- Offboarding: account suspension, token and SSH key revocation, the per-provider report (M5). M2 only removes team memberships and GitHub organization membership.
- Inbox, notifications, recipient resolution, push (M3). M2 stores events only.
- Backups UI, schedules, S3 targets, test restores (M7). M2 has upgrade snapshots only.
- Connecting existing self-hosted installs (an existing Forgejo as `connected`).
- Migrating data between providers (GitHub → Forgejo).
- Capability readers in the UI or MCP (M3, M6).
- GitHub Enterprise Server and GitHub Enterprise Managed Users.

## Issue map

| Issue | Sections |
|---|---|
| #58 Provider framework | Code layout and registry, Adapter contracts, Capability matrix, Data model, Credentials, Account links (table only), Desired access, Provisioning, Webhooks (endpoint, processing, retention), Normalized events (storage), API rows marked #58, Permissions, Audit, Testing (framework, store, API) |
| #59 Agent | [agent.md](agent.md): Process model, Transport, Agent API, Desired state and reconciliation, Instance directory, Package format (rendering, policy), Networking, Per-service database, Status and logs; here: Managed instance lifecycle (steps 2–3) |
| #60 Upgrades | [agent.md](agent.md): Snapshots, Upgrade; here: `app_operations`, ingress maintenance routing |
| #61 Forgejo package | [agent.md](agent.md) Package format; here: Managed instance lifecycle, Ingress routes, Provider specifics → Forgejo (install, setup) |
| #62 Forgejo SSO and provisioning | Deep links and silent SSO, Forgejo OIDC configured automatically, Provisioning, Provider specifics → Forgejo |
| #63 Forgejo webhooks | Webhooks, Normalized events, Provider specifics → Forgejo (webhooks) |
| #64 GitHub App | Adapter contracts (Connector), Credentials, Provider specifics → GitHub (connect, permissions, tokens), GitHub live tests |
| #65 GitHub linking and access | Account links, Desired access, Provisioning (removal guard, employee steps), Provider specifics → GitHub |
| #66 GitHub webhooks | Webhooks, Normalized events, GitHub live tests (delivery log replay, fixtures) |
| #67 Acceptance | All; agent.md Upgrade (rollback demonstration) |

## Open questions for the maintainer

1. **GitHub OAuth browser leg in CI.** Full automation needs a browser sign-in to GitHub as the test user (password and TOTP stored as CI secrets). Accept that, or keep the browser leg as a manual check per release? This spec assumes manual.
2. **Committed fixture secret.** #66 wants real GitHub-signed fixtures. That needs the test App's webhook secret in the repository. Accept (unroutable webhook URL, empty test org), or re-sign fixtures with a fixture secret (then they are no longer GitHub-signed)?
3. **Who provides the GitHub test resources:** a dedicated test organization, test App and test user account owned by the project. Their names are needed before #64 can run live tests.
4. **Link step and welcome.** Should `github.link-account` block finishing welcome (required) or stay optional in the checklist? This spec makes it optional.
5. **Existing GitHub members.** Default for `manage_existing_members` is off (pre-existing members are reported, not removed). Confirm, or prefer on-after-preview during connect?
6. **Forgejo organization name** default (derived from the workspace or company name). Is there a company slug to use, or should the admin always type it at install?
7. **Event retention.** Events are append-only and kept forever in M2. Acceptable until M3 defines retention?
