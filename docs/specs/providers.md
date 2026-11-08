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
                                  // (the fenced alias while an upgrade candidate runs, agent.md "Upgrade")
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

### Shared types

Every type the interfaces use is defined here; #58 implements them in `providers/provider` as written.

```go
type AccessPolicy string // "all_people" | "mapped_teams"

// CredentialSpec declares a credential of an instance (see "Credentials").
type CredentialSpec struct {
    Name      string        // ^[a-z][a-z0-9_]{0,40}$, unique per provider
    Source    string        // "generated" (core makes 32 random bytes) | "provider" (from Connect/Setup) | "upload" (admin)
    GenerateRotation bool // core-generated replacements of provider-supplied initial secrets
    ReadBack  bool          // provider-minted values: ApplyCredential reconciles an earlier attempt of the same version (see "Provider call executions")
    Rotatable bool
    Overlap   time.Duration // how long the previous version stays valid after the new one is acknowledged
    Agent     bool          // passed to the agent as a secret input (managed packages)
}

// EmployeeStepSpec declares an onboarding step the provider needs from employees.
type EmployeeStepSpec struct {
    ID          string // namespaced: "<provider>.<step>", e.g. "github.link-account"
    Title       string // English fallback; the UI translates by "onboarding.steps.<id>.title"
    Description string // English fallback; "onboarding.steps.<id>.description"
    ActionPath  string // local path, e.g. "/me/accounts/github" (onboarding.Registry rules)
    Required    bool   // M2: false for every provider step (maintainer decision)
}

// SettingSpec declares a non-secret instance setting. Values are JSON scalars.
type SettingSpec struct {
    Name     string   // ^[a-z][a-z0-9_]{0,40}$
    Type     string   // "string" | "int" | "bool"
    Pattern  string   // strings: RE2, anchored
    Enum     []string
    Min, Max int64    // ints
    Default  any
    Mutable  bool     // false: fixed after the instance is created (e.g. organization)
}

// TeamMapping is one row of app_team_mappings as adapters see it.
type TeamMapping struct {
    TeamID      uuid.UUID
    TeamSlug    string    // Orglane team slug; name for teams the adapter creates
    TeamName    string
    Mode        string    // "create" | "existing"
    ExternalRef string    // provider team ID; empty until created (mode "create")
}

// TeamRef identifies a provider-side team.
type TeamRef struct {
    TeamID      uuid.UUID // the Orglane team it is mapped from
    ExternalRef string    // provider team ID (GitHub: numeric team ID; Forgejo: team ID)
    Name        string    // provider slug or name, informational
}

// AccountLink is a verified link as adapters see it.
type AccountLink struct {
    ID         uuid.UUID
    Account    ExternalAccount
    Method     string // "oauth" | "provisioned"
    VerifiedAt time.Time
}

// Inputs are what Lifecycle.Inputs adds to the desired spec (agent.md "Desired spec").
type Inputs struct {
    Settings map[string]any // package settings (validated by the agent against the package manifest)
    Secrets  []string       // credential names sent as secrets; must be CredentialSpec entries with Agent = true
}

// ConnectStep tells the UI what to do next during Connector flows.
type ConnectStep struct {
    Name   string            // provider-defined step name, e.g. "create_app", "install_app"
    Method string            // "GET" (redirect) | "POST" (auto-submitted form; GitHub manifest flow)
    URL    *url.URL          // provider URL (host checked against the provider's allowlist)
    Form   map[string]string // form fields for POST (e.g. "manifest")
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
    // Every Event.DedupeKey is derived from the signed body only (see
    // "Normalized events"). ErrForeignDelivery marks an authentic delivery that
    // is not for this instance (GitHub: another installation or organization).
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

// Optional (#66): deliveries that change the connection itself or ask
// provisioning to look again. Pure like Normalize, signed body fields only,
// ErrForeignDelivery for another instance. The core calls it before
// Normalize, in the transaction that stores the delivery's events.
type DeliveryEffects interface {
    Effects(inst Instance, d Delivery) (Effect, error)
}

type Effect struct {
    State  string // "" | "degraded" | "disconnected" | "restored" (degraded -> active, only if the health code is Code)
    Code   string // the health code State sets, or for "restored" the one it clears
    Resync bool   // queue a drift run of the instance's provisioning
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
    // CheckHealth is the provider-level health check through inst.Upstream.
    // The core calls it to confirm upgrades and on every health sync. (Not
    // named Verify: EventSource.Verify has another signature, and a managed
    // provider such as Forgejo implements both.)
    CheckHealth(ctx context.Context, env Env, inst Instance) error
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

Readers serve native views and MCP. They are **deferred to M3/M6**, which define their interfaces and result types (for example a `CodeReader` with "pull requests awaiting my review"). M2 defines no reader type, so nothing in M2 depends on one.

### Errors

```go
var (
    ErrBadSignature = errors.New("provider: bad webhook signature")
    ErrNotFound     = errors.New("provider: not found")
    // ErrForeignDelivery: authentic, but for another installation or organization.
    ErrForeignDelivery = errors.New("provider: delivery is not for this instance")
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
| `deprovision.revoke_tokens` | planned, M5 (access tokens and SSH keys; users manage their own SSH keys in Forgejo meanwhile) | unsupported |
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
    -- managed, removed without purge: directory, snapshots and svc_<key> database still exist
    data_retained            boolean NOT NULL DEFAULT false,
    CHECK ((management = 'managed') = (package_id IS NOT NULL)),
    CHECK (NOT data_retained OR state = 'removed')
);
-- A key stays reserved while an instance uses it or its data is retained.
CREATE UNIQUE INDEX app_instances_key_key ON app_instances (key) WHERE state <> 'removed' OR data_retained;
```

**Keys and removal.** Instance rows are never deleted (audit, events and links reference them). A key identifies the agent directory, the Compose project and the `svc_<key>` database, so it is reserved until all of them are gone:

- `DELETE /apps/instances/{id}` without purge stops and removes the containers and sets `removed` with `data_retained = true`. The key stays reserved; the agent directory stays bound to this instance ID (agent.md "Instance directory").
- `POST /apps/instances/{id}/reinstall` brings a retained instance back: same ID, key, credentials and data, a new desired generation with `run_state = running`; the instance goes through `installing` again.
- `DELETE /apps/instances/{id}?purge=true` (also allowed on a retained instance) deletes the directory, the snapshots, the database and the role, the OIDC client and the credentials, then sets `data_retained = false`. Only then can a new instance use the key.
- Tests (#59, #61): creating an instance with a retained key fails with `key_reserved`; reinstall keeps data; after purge the key is free and a new instance with it starts empty.

State machines:

```
managed:   installing ──► active ◄──► degraded            (active only after Lifecycle.Setup succeeded)
               │            │  ▲
               ▼            ▼  │
             failed       upgrading ──► failed (rollback failed; admin action)
       (agent apply or setup retries exhausted; admin retry → installing)
           active/degraded/failed ──► removing ──► removed (data_retained, or purged) ──► installing (reinstall)

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
    reason           text NOT NULL CHECK (reason IN ('install', 'configure', 'upgrade', 'retry', 'revert', 'remove', 'reinstall', 'resync')),
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
    state       text NOT NULL CHECK (state IN ('pending', 'uncertain', 'abandoning', 'active', 'retiring', 'discarded')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,         -- when the provider acknowledged it
    retire_at   timestamptz,          -- retiring versions stay valid until then
    id          uuid NOT NULL DEFAULT uuidv7(), -- durable row identity (versions are reused after cleanup)
    stuck_reported_at timestamptz,    -- the one rotation_stuck report of this row
    ended_at    timestamptz,          -- when it stopped being active or a candidate (retired, discarded)
    drift_detected_at timestamptz,    -- the one drift report of this row
    PRIMARY KEY (instance_id, name, version)
);
CREATE UNIQUE INDEX app_credentials_active_key ON app_credentials (instance_id, name) WHERE state = 'active';
CREATE UNIQUE INDEX app_credentials_pending_key ON app_credentials (instance_id, name) WHERE state IN ('pending', 'uncertain', 'abandoning');
CREATE UNIQUE INDEX app_credentials_id_key ON app_credentials (id);

-- One row per provider call execution writing a credential row's value.
CREATE TABLE app_credential_calls (
    id            uuid PRIMARY KEY,   -- the execution's fencing token
    credential_id uuid NOT NULL REFERENCES app_credentials (id) ON DELETE CASCADE,
    started_at    timestamptz NOT NULL DEFAULT now(),
    deadline      timestamptz NOT NULL  -- lease (database time); never shortened
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

### app_oauth_states

Single-use state for browser round trips to providers: account verification (#65) and the GitHub connect flow (#64).

```sql
CREATE TABLE app_oauth_states (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    kind         text NOT NULL CHECK (kind IN ('link', 'connect')),
    state_hash   bytea NOT NULL UNIQUE CHECK (octet_length(state_hash) = 32), -- crypto.HashToken(state)
    instance_id  uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    person_id    uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,  -- who started it
    session_id   uuid NOT NULL REFERENCES sessions (id) ON DELETE CASCADE, -- the browser session it is bound to
    step         text,                -- connect: provider step name
    pkce_verifier bytea,              -- sealed (PurposeAppOAuthState), link only
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,
    CHECK (expires_at > created_at)
);
```

- `state` is `crypto.RandomToken("ols_")`-style random (256 bits); only its hash is stored. The PKCE verifier is sealed with a new purpose `PurposeAppOAuthState = "orglane/v1/app-oauth-state"`, AD = row ID.
- Lifetime: 10 minutes for `link`; 60 minutes for `connect` (the GitHub manifest code is valid for one hour).
- Consumption is atomic and single-use: `UPDATE app_oauth_states SET consumed_at = now() WHERE state_hash = $1 AND kind = $2 AND consumed_at IS NULL AND expires_at > now() AND session_id = $current_session AND person_id = $current_person RETURNING ...`. No row → `invalid_state` (one error for expired, used, foreign and unknown states).
- The daily `orglane.apps.cleanup` job deletes rows 24 hours after expiry.

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

### provisioning_removals and provisioning_removal_batches

Every removal action goes through these tables, whatever planned it (see [Removal guard](#removal-guard)).

```sql
CREATE TABLE provisioning_removal_batches (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    instance_id   uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    state         text NOT NULL CHECK (state IN ('held', 'confirmed', 'applied', 'superseded')),
    item_count    integer NOT NULL,
    items_hash    bytea NOT NULL,      -- SHA-256 over the sorted (removal id, desired_hash) pairs
    created_at    timestamptz NOT NULL DEFAULT now(),
    confirmed_by  uuid REFERENCES people (id),
    confirmed_at  timestamptz
);
CREATE UNIQUE INDEX provisioning_removal_batches_held_key ON provisioning_removal_batches (instance_id) WHERE state = 'held';

CREATE TABLE provisioning_removals (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    instance_id   uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    person_id     uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    action        jsonb NOT NULL,      -- Action (kind, key, params)
    action_key    text NOT NULL,
    desired_hash  bytea NOT NULL,      -- the DesiredAccess hash that planned it
    state         text NOT NULL CHECK (state IN ('pending', 'held', 'applied', 'dropped', 'failed')),
    batch_id      uuid REFERENCES provisioning_removal_batches (id),
    error_code    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX provisioning_removals_open_key ON provisioning_removals (instance_id, person_id, action_key)
    WHERE state IN ('pending', 'held');
CREATE INDEX provisioning_removals_applied_idx ON provisioning_removals (instance_id, updated_at) WHERE state = 'applied';
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
-- Replay key: the authenticated bytes, not the unsigned delivery header (see "Replay and retention").
CREATE UNIQUE INDEX webhook_deliveries_body_key ON webhook_deliveries (instance_id, body_sha256);
```

### events

Append-only, like `audit_entries`: a trigger rejects `UPDATE`, `DELETE` and `TRUNCATE`. **Events are kept until M3 defines their retention** (maintainer decision); M2 deletes no events. Nothing references events with a cascading foreign key, and events reference nothing that M2 deletes: the link to the delivery is a plain column without a foreign key, so deleting old deliveries never updates an event, and `app_instances` rows are never deleted.

```sql
CREATE TABLE events (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    type           text NOT NULL CHECK (type ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){2}$'),
    schema_version integer NOT NULL CHECK (schema_version > 0),
    instance_id    uuid NOT NULL REFERENCES app_instances (id) ON DELETE RESTRICT,
    provider       text NOT NULL,
    delivery_id    uuid,             -- webhook_deliveries.id at ingest; no foreign key (deliveries expire, events stay)
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

- **Rotation is staged**, so the working credential is never retired before its replacement works:
  1. Insert the new version as `pending`. The `active` version stays in use for every outgoing call. At most one pending version per name.
  2. A retried job `orglane.apps.rotate_credential` hands the pending value to its consumer: the provider (Forgejo: replace the org hook by one with the new secret, see "Spec gaps decided in #63"; GitHub: `PATCH /app/hook/config` with the App JWT; Forgejo `admin_token`: mint the new token) or the agent (a new desired generation for `Agent` credentials, acknowledged when the operation succeeds).
  3. Each attempt ends in one of three outcomes, and the adapter must classify it:
     - **Acknowledged** (the provider answered success, or a read-back proves the new value is in use): in one transaction, the candidate → `active` (`activated_at` = now), old active → `retiring` with `retire_at = activated_at + Overlap` (24 h for webhook secrets, 1 h for others).
     - **Rejected** (a definite answer that this attempt did not make the change, such as a validation error before anything was written): if the attempt was the candidate's first handover (it started from `pending`) or the credential is an upload, the candidate → `discarded`; the active version is untouched. Only a proven rejection discards. A rejection of an attempt that started from `uncertain` proves nothing about the earlier attempt and leaves the candidate `uncertain` (see the concurrency model below).
     - **Unknown** (timeout, connection reset, 5xx, the worker dying mid-call: the provider may or may not have committed the change): the candidate → `uncertain`. It is never discarded and never retired while uncertain.
  4. **Resolving `uncertain`**, by the same job, retried with backoff capped at one hour and without an attempt limit:
     - Proof by use: a webhook delivery that verifies only with the uncertain secret proves the provider uses it → acknowledged. A delivery that verifies only with the active secret proves nothing (it may have been sent before the change).
     - Read-back where the provider offers it: Forgejo `admin_token`: tokens are created with a name that contains the version (`orglane-<version>`); listing the service user's token names shows whether the mint committed; a committed token whose value was lost is deleted and the mint repeated. Agent credentials: the operation ID is persisted, and its state is read from the agent. Webhook secrets: neither Forgejo nor GitHub returns the secret, so there is no read-back.
     - Re-apply: setting the same value again is idempotent (hook update, `PATCH /app/hook/config`), so the job re-sends the uncertain value until it gets a definite answer; success → acknowledged.
     - Abandon (admin action only): the job first re-applies the **active** value to the provider until acknowledged, and only then marks the candidate `discarded`.
     - Forced resolution (admin action only, last resort): see *Evidence, override and drift* below.
     After 24 h in `uncertain` the instance shows `rotation_stuck` to admins (audit `app_credential.rotation_stuck`); nothing is retired or discarded automatically.
  5. For incoming verification, `Env.Credentials` returns `pending`, `uncertain`, `active` and `retiring` versions, so deliveries signed with whichever secret the provider actually uses verify during the whole changeover.
  6. The daily cleanup deletes `retiring` versions 7 days after `retire_at` and `discarded` versions 7 days after they were discarded (`ended_at`). They verify nothing meanwhile; they are kept only to detect drift. It never deletes `pending` or `uncertain` versions, and deletes execution records a day after their lease ended.
  7. Tests (#58 with `testprovider`, #63/#66 against the real provider): "provider committed, response lost" by a fault-injecting HTTP transport in front of the real API that forwards the request and then drops the response; across several retries and a worker restart the candidate stays `uncertain`, both secrets verify deliveries, and it becomes `active` after re-apply; a proven rejection discards; abandon re-applies the old value first.
- **Concurrency model.** Binding for every credential path in `core/internal/apps` (store, connect, rotate, upload, abandon, the rotation worker, webhook proof by use, the stuck report, cleanup) and for adapters' `Env` reads.
  - *States.* `pending` (stored, never handed over), `uncertain` (handed over; some attempt's outcome is unknown), `abandoning`, `active`, `retiring`, `discarded`. A *candidate* is a `pending`, `uncertain` or `abandoning` row; there is at most one per name (unique index). Transitions, each a conditional UPDATE whose zero-row path changes nothing and records nothing:

    | From | To | By |
    |---|---|---|
    | (none) | `pending` | rotate / upload (admin), only without a candidate |
    | (none) | `active`, old `active` → `retiring` | direct replacement (connect save, `StoreCredential`), only without a candidate |
    | `pending` | `uncertain` | forward worker, committed with the call's execution record, before the provider call |
    | `pending`/`uncertain` | `active`, old `active` → `retiring` | forward worker on *acknowledged*; proof by use |
    | `uncertain` | `discarded` | forward worker on *rejected*, only when that attempt started from `pending` or the credential is an upload |
    | `pending`/`uncertain` | `abandoning` | admin abandon |
    | `uncertain`/`abandoning` | `active` or `discarded` | admin forced resolution (no lease holds) |
    | `abandoning` | `discarded` | abandon worker, after the provider acknowledged the active value again (or there is none) |
    | `retiring`, `discarded` | deleted | cleanup, 7 days after `retire_at` / the discard (`ended_at`) |
  - *Uncertain, then rejected.* A rejection proves only that the rejecting attempt wrote nothing. An attempt that starts from `uncertain` follows one whose outcome is unknown (lost answer, dying worker), which may have installed the value; its rejection keeps the candidate `uncertain`: it still verifies deliveries, the job retries, the stuck report applies. Only acknowledgement, proof by use, an abandon that re-applies the active value, or an admin's forced resolution ends it. Uploads (`Source` "upload") are the exception: the admin installed the value at the provider and `ApplyCredential` only verifies it, so no attempt can have installed it and every rejection discards.
  - *Direct replacement during a rotation* is refused with `rotation_in_progress`. Superseding the candidate would drop a value the provider may already use, and letting it finish would retire the newer value (#84). So while a candidate exists, the active version is the one the rotation started from, and acknowledgement retires exactly that one.
  - *Provider call executions.* No database lock can fence an external call (a worker's session lock goes away with its backend while its call runs on), and no amount of elapsed time proves that the provider finished processing a request. So each `ApplyCredential` call (forward attempt, abandon re-apply) is an *execution* recorded in `app_credential_calls` on the row whose value it writes, with its own fencing token (`id`) and lease (`deadline = now() + 2 min + 1 min`, database time), in the transaction that rechecks the candidate under the lock order. The call's context ends 2 min after a local instant taken before that transaction, so Orglane itself stops acting on it before its lease ends (local intervals only; database and local clocks are never compared).
    - *At most one execution per credential* (instance and name) holds a lease, whichever row it writes: while one does, any other execution is deferred (retryable error, no provider call), the same row included. There is no overlap, because a value the provider mints differs per attempt, and a later attempt could invalidate the one an earlier attempt published.
    - *Retrying after an unknown outcome.* A retry repeats the same request only when that is safe: the core holds the value (generated or uploaded), or the adapter reads back what the earlier attempt did (`CredentialSpec.ReadBack`; Forgejo's `admin_token` lists tokens named after the version, deletes a stray one and mints again). Otherwise (a provider-minted value without read-back) no further attempt is made: the candidate stays `uncertain` until evidence, an abandon, or an admin's resolution; the stuck report applies.
    - Leases never shrink. Only the execution itself removes its record, after a definite answer (acknowledged, rejected, including a rejection that leaves the candidate in place: a retry after an unknown outcome, an abandon re-apply) and only while its lease holds; then it records the result in the same transaction. If its lease ended first, it records nothing and its job *retries* (it never completes as if the candidate were resolved). An unknown answer leaves the lease to expire.
    - While a lease holds, a new rotation or upload, a direct replacement, connect save and an admin's forced resolution return `rotation_in_progress`. The admin's abandon request only records intent; its re-apply is what waits.
    - Leases pace Orglane's own actions; they are not safety. Safety comes from the candidate rule: while a candidate exists, and it stays `uncertain` until evidence resolves it, no new value is admitted. A remote effect that lands after everything settled is detected as drift (below) and reported, not repaired automatically.
  - *Evidence, override and drift.*
    - Evidence that resolves an `uncertain` candidate: a definite answer to a retry of the same value; proof by use (a delivery that verifies with the candidate and not with the active version); a provider read-back where the adapter supports one. Elapsed time is never evidence.
    - Admin override (`POST …/credentials/{name}/resolve`, `apps.manage`, audited as `app_credential.rotation_resolved` with the resolution): forces an `uncertain` or `abandoning` candidate to `active` (old active retires as usual) or `discarded`, when retries, proof by use and cancelling cannot finish. Refused for a `pending` candidate (cancel it instead), while a lease holds, and for `active` when the candidate has no value (a provider-minted value whose answer was lost: an assertion cannot recover its bytes; `candidate_value_unknown`, only `discarded` remains). The UI asks for a confirmation naming the credential, the version and the consequence. History shows it as `activated_by_admin` or `discarded_by_admin`, never as verified, and the UI never recommends revoking the old value after it. The 24 h stuck report stays.
    - Drift is detected, never repaired automatically (M2). A delivery whose signature verifies only with an older version, one that is neither active nor a candidate (`retiring` within its overlap: accepted; past `retire_at` or `discarded`: still rejected), is drift, unless that version ended (`ended_at`) less than 5 minutes ago (deliveries signed just before a change). It is recorded once per version (`drift_detected_at`, audit `app_credential.drift`): replays and repeated deliveries change nothing, and nothing is queued. The instance detail lists it (`credentialDrift`) while it was detected after the current active version became active, that is until the credential is rotated again; the card tells the admin to rotate it. Older versions are kept 7 days after they stopped being accepted for this, and never accepted. For webhook secrets only, through `EventSource` (GitHub's incoming verification is #66).
  - *Identity.* Versions number the values of one name and are reused once cleanup deleted the newest row. The row's `id` is never reused: rotation jobs carry it (`credential_id`; a job whose version now names another row does nothing), rotation audit entries carry it, and the stuck report is recorded once per row (`stuck_reported_at`).
  - *Lock order.* Locks are taken only in this order; a step may be skipped, never taken after a later one:
    1. One session advisory lock, held across provider calls on a dedicated pool connection: `connect` (per instance) or `credential` (per instance and name, serializing forward and abandon attempts). A holder takes no second session lock. It serializes healthy workers only; provider call executions are fenced by their own records (above).
    2. The `app_instances` row, always `FOR NO KEY UPDATE`. Instance IDs never change, so foreign-key checks of inserts into child tables (credentials, `webhook_deliveries`) take `KEY SHARE`, which never waits on it.
    3. The candidate row (`FOR UPDATE`), then the active row (by its UPDATE).
    4. Leaf writes: audit entries, job inserts, the webhook delivery insert.

    Every transaction that changes a credential row starts with lock 2 (admin paths by loading the instance for update) and rechecks the candidate's `id` and state under lock 3; the only exception is cleanup, which deletes `retiring` and `discarded` rows no transition locks. Proof by use verifies signatures without locks, then takes 2 and 3 and rechecks before activating, in the transaction that stores the delivery.
  - *Connections.* Provider calls run outside transactions, under lock 1 only. Whoever holds lock 1 does all database work on that lock's connection: short transactions, `Env` credential reads by the adapter, execution records and the stuck report. It never acquires a second pool connection, so these paths complete with `database.max_conns=1` and under pool saturation (`heldConn` in `core/internal/apps`). The provisioning jobs follow the same rule with their own session locks (`provision_person`, `provision_instance`, `apply_removals`); `apply_removals` takes each person's provisioning lock on its own connection, after its per-instance lock, instead of on a second one. Code inside a pool transaction reads through that transaction, including the `Env` it hands to adapters. Every `Env` reads through one serialized reader per connection or transaction, so adapters may call it from several goroutines.
- **Rollbacks and snapshots:** a snapshot restore can bring back provider-side state from before a rotation (for example Forgejo's stored hook secret or tokens minted later). Therefore, after every rollback or restore, the core runs `Lifecycle.Setup` again (idempotent) before the instance returns to `active`; it re-asserts the admin token and calls `EnsureWebhooks` with the active secrets. The agent's snapshot keeps its own copy of the secrets it was rendered with, so no core-side version has to be kept for it.
- Each rotation step writes an audit entry (`app_credential.rotated` on activation) with name, version and the row's `credential_id` only. Cancellation intent and its job are recorded only if the pending/uncertain → abandoning transition changes a row. A request that loses to verification, or targets an already-abandoning candidate, returns `no_rotation` (409) without another intent or job; verified history stays verified.
- **Master secret:** credentials depend on `identity.master_secret` like every other sealed column. It must never change (existing rule); re-encryption under a new master secret is out of scope.
- Generated secrets are 32 random bytes in unpadded base64url (`crypto.RandomToken("")`), which also satisfies the agent's secret input format (agent.md).

Credential rotation visibility: the public instance detail includes the latest
safe rotation outcome per credential, including rejection and cancellation after
discarded credential retention cleanup. Upload acceptance, uncertain verification,
verified activation and proven failure are distinct. Values and upstream diagnostics
are never exposed; a failed replacement does not retire the active credential. Current
pending/uncertain/abandoning credential rows are authoritative over retained
history. For names with no candidate, lifecycle history uses the database-generated
UUIDv7 audit IDs (allocated on event insertion after the credential mutation),
not `occurred_at`, whose `now()` default is the transaction start time. This keeps
an acceptance that waited behind a previous completion newer than that completion,
and preserves outcomes after ciphertext cleanup and credential-version reuse.

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

**Link flow (#65, decided):**

- `POST /me/account-links/{provider}/start` (browser session only) picks the oldest active instance of the provider whose adapter verifies accounts (or the given `instanceId`); any of them proves the realm's account, because a GitHub user ID is the same account in every organization (`AccountRealm` = `github.com`). It stores a state (`oll_` + 32 random bytes, only its hash) and a PKCE verifier sealed with `PurposeAppOAuthState` (AD = row ID), bound to the person and the session, expiring after 10 minutes. A new start invalidates the person's older link states for the instance.
- `POST /me/account-links/{provider}/complete` consumes the state first (single-use, only by the same person and session, also when the exchange then fails), then lets the adapter exchange the code. GitHub: `POST https://github.com/login/oauth/access_token` with the App's client ID and secret, `redirect_uri` and `code_verifier`; `GET /user` with the user token; then `DELETE /applications/{client_id}/grant`, which revokes the access token and any refresh token (a failed revocation is logged; the token was never stored). The link stores the numeric user ID; the login is informational.
- Under one transaction lock per realm: the account verified for another person → `account_already_linked` (nothing changes); the same account again → only its login is refreshed; another account of the same person → the old link is revoked (`account_link.revoked`, reason `replaced`) and the new one stored. `account_link.verified` records provider, realm, instance, method, evidence and the external ID (never the login or a token). Every link change queues `provision_person` for the person in every instance of the realm.
- Evidence compares the person's pending rows for the verifying instance's organization: `matched`, `mismatched`, or `none`. Conflicts are computed on read: pending evidence whose account is verified for another person is shown as `conflict`.
- `DELETE /me/account-links/{linkId}` (the person, reason `unlinked`) and `DELETE /people/{personId}/account-links/{linkId}` (`people.manage`, reason `revoked`) revoke a verified link; the link must belong to that person (otherwise 404).

**Login changes:** links are keyed by the immutable external ID. Adapters refresh `login` when they observe a change (API reads, webhook payloads) and resolve the current login from the ID before calls that need a login.

## Desired access and team mappings

Computed by `core/internal/apps` from the directory, never stored as truth (only its hash in `provisioning_results`):

```
for each app instance in (active, degraded) whose adapter implements UserProvisioner:
  for each person:
    eligible := person.status == 'active'
    teams    := mapped provider teams of the person's direct, non-archived team memberships
    access   := eligible && (instance.access_policy == 'all_people' || len(teams) > 0)
    if !access: teams := []          # never plan team additions for someone without access
    managed  := result.granted_by_orglane || instance.manage_existing_members
```

`granted_by_orglane` is set when an action of Orglane created the account, the invitation or the membership (Forgejo accounts are all created by Orglane, so they are managed). `manage_existing_members` **defaults to off** (maintainer decision): pre-existing provider members are reported, never removed, unless an admin turns it on after a dry-run preview.

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
  → for each non-removal action in order: adapter.Apply → audit entry if Changed
  → removal actions are not applied here: they are upserted into provisioning_removals (pending)
    and orglane.apps.apply_removals {instance_id} is enqueued (see "Removal guard")
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
- Retryable errors: River retries with backoff, at most 8 attempts. `RetryAfter` snoozes the job until the provider's reset (GitHub rate limits) without spending an attempt, also on the last attempt; this holds for every job that calls a provider (provisioning, removals, setup, rotation). Non-retryable errors (`seat_limit`, `owner_protected`, `invalid_login`, ...) mark the result `failed` immediately; it is retried when desired access changes or an admin presses Retry.
- Partial failure: people are independent. Within one person, actions run in plan order and stop at the first failure; applied actions are not undone (they are idempotent), and the next run continues from a fresh plan. The result lists each action's outcome (`changed`, `unchanged`, `failed`, `not_attempted`, `held`).

### Removal guard

Removing access by mistake is worse than keeping it a little longer. Every entry point (a person's status change, a team membership change, a mapping deletion, the daily sweep, a link revocation) only *plans* removals; one per-instance job applies them:

- Adapters plan removal actions only when `DesiredAccess.Managed` is true. Pre-existing members that Orglane does not manage are reported as unmanaged, never removed. GitHub organization owners are never removed (`owner_protected`).
- `provision_person` upserts each planned removal into `provisioning_removals` as `pending` with the `desired_hash` that planned it, and enqueues `orglane.apps.apply_removals {instance_id}` scheduled 60 s later (unique per instance, so a burst of directory changes collapses into one run). A pending removal whose person gets access back is set `dropped` by the next `provision_person` run for that person.
- `apply_removals` takes a transaction advisory lock on the instance, then:
  1. **Freshness:** recomputes `DesiredAccess` for each pending removal; if its hash differs from `desired_hash` (the person's desired state changed since planning), the removal is `dropped` and that person is re-planned.
  2. **Threshold:** `n` = fresh pending removals + removals `applied` in the last 24 hours; `limit` = max(3, 10 % of the instance's managed accounts). If `n <= limit`, it applies them in order (`applied` or `failed`, audit per change). Each removal's new state commits in one transaction with its audit entry and its person's next `provision_person` job (an applied or dropped removal re-plans the person), so another person's failure, a crash or a retry never loses it; the job's retry error is returned only after that. A `RetryAfter` ends the run's provider calls and snoozes the job. Counting the last 24 hours means splitting a mass removal into small bursts does not bypass the guard.
  3. **Hold:** otherwise every fresh pending removal becomes `held` in one batch (`provisioning_removal_batches`, `items_hash` over the items). If a held batch already exists, it is `superseded` and its items join the new batch, so there is at most one held batch per instance. Nothing is removed; Settings → Apps shows the batch with each person and action.
- **Confirmation** (`POST .../provisioning/removal-batches/{batchId}/confirm` with the `items_hash` the admin saw): refused with `batch_changed` if the batch is no longer `held` or the hash differs. Otherwise the batch becomes `confirmed` and `apply_removals` applies its items, re-checking freshness per item first: an item whose desired state changed since the preview is `dropped`, never applied. Removals planned after the batch are not part of it and go through the guard again.
- Tests (#58 with `testprovider`, #65 live): deleting a mapping that covers most of the org holds one batch and removes nobody; concurrent person jobs during the hold add to a new batch instead of applying; a person regaining access between preview and confirmation is not removed; a confirmation with a stale `items_hash` is refused; small removals apply without confirmation; 24-hour accumulation triggers the hold.

### Employee steps

- Declared in `Manifest.EmployeeSteps` with a namespaced ID (`github.link-account`) and an action path (`/me/accounts/github`).
- The core registers one `onboarding.Step` per declared step. `Check` returns nil when the person has no desired access to any instance of the provider, `Complete: true` when a verified link exists in the realm, and otherwise an incomplete step with the action URL (a local path, as `onboarding.Registry` requires).
- Provider steps are **not required** for finishing welcome (maintainer decision: `github.link-account` is optional; the person may need to create a GitHub account first). They stay visible in the checklist until done.
- Completing the step (link verified) enqueues `provision_person` for the person in every instance of that realm.

## Webhooks

### Endpoint

`POST /api/v1/webhooks/{instanceId}` on the workspace host, `x-orglane-permission: public`. The provider's signature is the authentication. The handler needs the exact raw body for verification and must not decode JSON before it. The contract declares the body as an opaque binary string; if the generated strict handler cannot hand over raw bytes for `application/json`, #58 registers the route as a plain handler and keeps it in the contract with its permission, so the permission check at startup still covers it.

| Step | Rule |
|---|---|
| Instance | Unknown, `removed` or `connecting` → `404`. The response does not say which |
| Size | Body over 5 MiB → `413`, nothing stored |
| Headers | Only an allowlist is kept (`X-GitHub-Event`, `X-GitHub-Delivery`, `X-Forgejo-Event`, `X-Forgejo-Delivery`, `Content-Type`, `User-Agent`). Headers are not signed: they are used for routing and display only, never for authorization or dedupe |
| Signature | `EventSource.Verify` over the raw body, constant time, each candidate secret. Failure → `401`, nothing stored, a rate-limited warning log without body or headers |
| Dedupe | `INSERT ... ON CONFLICT DO NOTHING` against both unique keys: `(instance_id, body_sha256)` (the signed bytes) and `(instance_id, delivery_id)`. New → enqueue `orglane.apps.process_delivery {delivery_row_id}` in the same transaction, answer `202`. Conflict on either key → `200`, no job |

The handler does no provider API calls and no normalization, so it answers within milliseconds (GitHub expects an answer within 10 seconds).

Signatures:

| Provider | Header | Algorithm | Delivery ID |
|---|---|---|---|
| GitHub | `X-Hub-Signature-256: sha256=<hex>` | HMAC-SHA256 of the raw body with the App's webhook secret | `X-GitHub-Delivery` (GUID) |
| Forgejo | `X-Forgejo-Signature: <hex>` | HMAC-SHA256 of the raw body with the hook secret | `X-Forgejo-Delivery` (UUID) |

### Processing

`process_delivery` opens the sealed body, calls `Normalize`, inserts events (`ON CONFLICT (instance_id, dedupe_key) DO NOTHING`) and sets the delivery `processed` (or `ignored` for event names the adapter does not normalize) in one transaction. Normalization errors are permanent: `failed` with an error code, no retry. Database errors retry.

**Is the delivery for this instance?** Checked after signature verification, from signed body fields only (GitHub's `X-GitHub-Hook-Installation-Target-ID` header identifies where the hook was created, not the installation, and is not signed):

- GitHub: the payload's `installation.id` must equal the instance's installation ID, and `organization.id`, when present, its organization ID. Otherwise `Normalize` returns `ErrForeignDelivery` and the delivery is `ignored` with `foreign_installation`. App-level events without `installation` are handled explicitly: `github_app_authorization` (a user revoked the App) is `ignored` (no user tokens are kept); `meta` with `action: deleted` (the App's hook was deleted) marks the instance `degraded` with `webhook_deleted`; any other event without `installation` is `ignored` with `no_installation`. `installation` events with `action: deleted` or `suspend` mark the instance `disconnected` or `degraded`. These state changes are the adapter's `DeliveryEffects` (see "Spec gaps decided in #66").
- Forgejo: the webhook secret is per instance and the hook is the instance's own org hook; the payload's `repository.owner` (Forgejo 15 payloads carry no `organization`) must be the instance's organization, compared case-insensitively, else `ignored` with `foreign_organization`.

### Untrusted input rules

- Payloads are untrusted even after verification (a provider user controls titles, bodies and logins).
- JSON is decoded with size limits into typed structs; unknown fields are ignored.
- Strings are validated or truncated: titles 256 runes, excerpts 280 runes, logins against the provider's login pattern, repository names against `owner/name`.
- URLs from payloads are kept only if their scheme and host equal the instance's base URL host (GitHub: `github.com`), else dropped. They are never fetched by the core.
- No HTML or Markdown is rendered or stored as markup; excerpts are plain text.
- Bodies and headers are never logged.

### Replay and retention

- Neither provider signs its delivery ID or a timestamp, so a captured request can be replayed with any headers. Replay safety therefore rests on the signed bytes:
  - Delivery level: `(instance_id, body_sha256)` rejects the same signed body under any delivery ID for as long as the delivery row exists (30 days). Legitimate repeats with byte-identical bodies (a provider redelivery) are duplicates by definition; distinct provider events differ in their bodies (IDs, timestamps, SHAs), so no real event is lost.
  - Event level: every event's `dedupe_key` is derived from signed body fields (see [Normalized events](#normalized-events)) and events are kept until M3 defines retention, so a replay of any age never creates a second event. If M3 introduces event retention, it must keep the dedupe keys (or a tombstone) for at least as long as a replay matters.
- Tests (#58, #63, #66): identical signed bytes with a changed delivery header, a changed event header and no delivery header are answered `200` and create no second delivery or event; a delivery older than 30 days (aged in the test) replayed creates a delivery row but no event.
- Retention (daily job `orglane.apps.cleanup`): `body` is set to NULL 7 days after processing (30 days for `failed`); delivery rows are deleted after 30 days. Events are not touched (no foreign key, see `events`). An integration test ages a delivery that produced events, runs the cleanup, and checks the delivery is gone and its events are unchanged.

## Normalized events

Stored now; delivery (Inbox, push) is M3. Types in M2:

| Type | Source | Subject | Targets |
|---|---|---|---|
| `code.pull_request.opened` | Forgejo, GitHub | pull request | requested reviewers (`review_requested`), assignees (`assigned`) |
| `code.review.requested` | Forgejo, GitHub | pull request | the requested reviewer (`review_requested`) |
| `code.mention.created` | Forgejo, GitHub (PR/issue bodies and comments) | pull request or issue | mentioned logins (`mentioned`), at most 50 |
| `code.branch.pushed` | Forgejo | repository branch | none |
| `code.membership.changed` | GitHub (`member`, `membership`, `organization`) | organization, team, or repository (`member`: collaborators) | the affected account (`member_added`, `member_removed`, `member_invited`) |

```go
type Event struct {
    Type          string
    SchemaVersion int        // 1
    DedupeKey     string     // from signed body fields only, per type (table below); never from headers
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

Mention extraction: `@login` tokens in the text of the Markdown (outside code blocks, code spans and raw HTML), login pattern of the provider, deduplicated, at most 50 per event. Self-mentions are dropped.

Dedupe keys and `Data` per type (schema version 1). IDs are the provider's numeric IDs from the payload; `<sha>` is the hex SHA-256 of the raw body, `<n>` the index of the event within the delivery.

| Type | `DedupeKey` | `Data` fields |
|---|---|---|
| `code.pull_request.opened` | `pr:<pull_request.id>:opened` | `draft` (bool), `base_ref`, `head_ref` (strings ≤ 255) |
| `code.review.requested` | `pr:<pull_request.id>:review_requested:<reviewer id>:<pull_request.updated_at>` (team requests: `team:<team id>` instead of the reviewer) | `requested_team` (slug, only for team requests; such events have no targets in M2) |
| `code.mention.created` | `comment:<comment.id>:mentions` for comments; `pr:<id>:body_mentions` / `issue:<id>:body_mentions` for the body on `opened`; Forgejo reviews (their payload has no review ID): `pr:<pull_request.id>:review:<sha>:mentions`; GitHub review comments (their own ID space): `review_comment:<comment.id>:mentions`; GitHub reviews: `review:<review.id>:mentions` | `excerpt` (plain text ≤ 280 runes), `comment_id` (when from a comment), `review_id` (GitHub reviews) |
| `code.branch.pushed` | `push:<repository.id>:<before>:<after>:<sha>` (the transition and the signed body: two pushes may move a ref along the same commits, and only a redelivery of the same bytes is the same push) | `ref`, `before`, `after` (40/64 hex), `commit_count` (int), `forced` (bool, optional: only when the provider reports it; Forgejo does not) |
| `code.membership.changed` | `body:<sha>/<n>` (these payloads carry no stable event ID or timestamp) | `change` (`added` \| `removed` \| `invited`), `team` (slug, optional), `role` (`member` \| `admin`, optional) |

Edited comments and bodies create no new mention events in M2. Unknown fields are never copied into `Data`.

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

1. On install, the core creates OIDC client `app-<key>` with `identity.CreateOIDCClient` (confidential, `client_secret_basic`, audited) and stores the secret as credential `oidc_client_secret`. The redirect URI is built from the instance's configured base URL, including scheme and port: `BaseURL` + `/user/oauth2/orglane/callback`, so `https://git.acme.com/user/oauth2/orglane/callback` in domain mode and `http://git.orglane.localhost/user/oauth2/orglane/callback` in local mode. The same base URL is Forgejo's `ROOT_URL`, and a change of the instance host updates both (the client's redirect URIs and a new desired generation). No code builds the callback from a hard-coded scheme.
2. The secret and the issuer URL go to the agent as inputs. The package's `configure` hook runs `forgejo admin auth add-oauth` (or `update-oauth` when the source exists) with `--provider openidConnect` and `--auto-discover-url <issuer>/.well-known/openid-configuration`, reading the secret from the container environment (agent.md "Hooks").
3. Forgejo settings in the template: registration only through the external source, no local sign-in form for people (the service admin keeps API access), username from `preferred_username`.
4. Accounts are pre-created by provisioning with `source_id` of that source and `login_name` = the person's OIDC subject, so the first SSO sign-in matches the existing account.
5. The Forgejo container reaches the issuer through Traefik under its public name (agent.md "Networking").
6. Tests (#62, required for acceptance): a full OIDC round trip into Forgejo in **both** modes, domain mode (`https`) and local mode (`http://orglane.localhost` issuer, `http://git.orglane.localhost` callback), asserting the registered redirect URI equals the one Forgejo sends.

### GitHub

GitHub accounts are personal; there is no SSO. `OpenURL` returns `https://github.com` + `to`. The matrix says so.

## Managed instance lifecycle in the core

The core side of [agent.md](agent.md):

1. `POST /api/v1/apps/instances` (managed provider) validates the key, the host (`config.ServiceHost(<route host label>)`, overridable per instance; it must be a valid hostname, distinct from the workspace host and from other instances' hosts) and, in domain mode, that the host resolves to the same addresses as the workspace host (as the installer does). It checks agent health and the package's resource hints against the host's reported memory and disk.
2. It creates the instance (`installing`), the credentials, the OIDC client (Forgejo) and desired spec generation 1, and enqueues `orglane.apps.agent_apply {instance_id, generation}`.
3. `agent_apply` pushes the spec and polls the operation (snoozing between polls) until it is terminal, mirroring it in `app_operations`. A failed operation sets the instance `failed` (`install_failed`).
4. On success the ingress gets the instance's routes (needed for `Setup` through the public name, e.g. OIDC) and `orglane.apps.setup {instance_id}` runs `Lifecycle.Setup` (retried with backoff, at most 8 attempts). The instance stays `installing` until `Setup` succeeds, so no provisioning or webhook processing starts before it.
5. When `Setup` succeeds, in one transaction: the instance becomes `active`, bindings are created and `provision_instance` is enqueued. When its retries are exhausted, the instance becomes `failed` with `setup_failed`; `POST /apps/instances/{instanceId}/setup` retries it: in one transaction the instance returns to `installing` and a setup job is queued. Like every setup request, it never relies on a setup job that is still running: the failed attempt's job may still show as running after it committed `setup_failed`. If the only duplicate is running, a continuation job is queued (see the follow-up rule under Provisioning decisions).

After an upgrade rollback the core records a `revert` spec (the running version) as soon as it sees the rolled-back operation, runs `Setup` again (see "Credentials"), and reports the result; the instance returns to `active`. Automatic package upgrades after an Orglane upgrade and the admin's retry are described in agent.md "Upgrade".

**Ingress routes (#61):** `core/internal/ingress` builds routes for managed instances in `installing` (once the agent operation succeeded, so `Setup` can use the public name), `active`, `degraded` and `upgrading`, in addition to the workspace router: one router per manifest route, named `orglane-app-<key>-<route>`, host from the instance, upstream `http://<key>-<service>:<port>` (the service's alias on the edge network), the same security headers, TLS and HTTP→HTTPS redirect as the workspace in domain mode. The dynamic configuration is therefore no longer static: the internal handler rebuilds it from the database on each poll (cheap, Traefik polls every 10 s). While an instance is `upgrading`, its routers point to the core, which answers `503` with `Retry-After` (agent.md "Upgrade").

## API

All under `/api/v1`, contract-first in `core/api/openapi.yaml`. Errors are RFC 9457 problem details with a stable `code`. Every operation has allowed and denied tests.

| Method | Path | Permission | Purpose | Issue |
|---|---|---|---|---|
| GET | `/apps/providers` | `apps.manage` | Compiled providers with capability matrix and settings schema | #58 |
| GET | `/apps/instances` | `apps.manage` | List app instances with state, health, version | #58 |
| POST | `/apps/instances` | `apps.manage` | Install a managed provider (202 + operation) | #61 |
| GET | `/apps/instances/{instanceId}` | `apps.manage` | Detail: state, settings, bindings, last operations, unmanaged account count | #58 |
| PATCH | `/apps/instances/{instanceId}` | `apps.manage` | Display name, access policy, `manage_existing_members`, provider settings | #58 |
| DELETE | `/apps/instances/{instanceId}` | `apps.manage` | Remove; data is retained and the key reserved. `?purge=true` also deletes data (requires `confirm` = instance key) | #59/#61 |
| POST | `/apps/instances/{instanceId}/reinstall` | `apps.manage` | Bring back a removed instance whose data is retained | #59/#61 |
| POST | `/apps/instances/{instanceId}/setup` | `apps.manage` | Retry `Lifecycle.Setup` of a `failed` (`setup_failed`) instance | #58/#61 |
| GET | `/apps/instances/{instanceId}/operations` | `apps.manage` | Operation history (install, upgrades, rollbacks) | #75 |
| POST | `/apps/instances/{instanceId}/upgrade` | `apps.manage` | Retry a rolled-back or failed upgrade (upgrades otherwise start automatically) | #60 |
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
| POST | `/apps/instances/{instanceId}/provisioning/removal-batches/{batchId}/confirm` | `apps.manage` | Release a held removal batch (body: `items_hash`) | #58 |
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

`app_instance.created`, `app_instance.updated`, `app_instance.removed`, `app_instance.reinstalled`, `app_instance.purged`, `app_instance.upgrade_started`, `app_instance.upgraded`, `app_instance.rolled_back`, `app_instance.logs_read`, `app_credential.created`, `app_credential.rotated`, `app_credential.rotation_failed`, `app_credential.rotation_resolved`, `app_credential.drift`, `app_binding.set`, `app_team_mapping.set`, `app_team_mapping.removed`, `account_link.verified`, `account_link.revoked`, `provisioning.action_applied` (target: the person; metadata: instance, action kind, key; only when `Changed`), `provisioning.removals_confirmed`.

New target kinds: `app_instance`, `account_link`. System jobs use `audit.System`.

## Provider specifics

### Forgejo (#61, #62, #63)

- Package and install: [agent.md "Package format"](agent.md#package-format). Instance key default `git`, host label `git`.
- `Setup`: create the service admin (done by the package's `configure` hook with `admin_password`), mint `admin_token` through the API with basic auth, create the organization (setting `organization`, not mutable after creation; default: the company slug, maintainer decision). The core has no company slug setting yet, so the default is the first label of `domain.base` (`acme.com` → `acme`, `orglane.localhost` → `orglane`), shown in the install form and editable there, create the org webhook (`EnsureWebhooks`).
- Accounts: `POST /api/v1/admin/users` with `source_id`, `login_name` = OIDC subject, `username` = `people.username`, `email` = work email, `must_change_password = false`. The response's user ID becomes a `provisioned` link (realm = instance ID).
- Teams: `create` mappings only; `SyncTeams` creates org teams named after Orglane team slugs.
- Webhooks (#63): one org webhook of type `forgejo`, content type JSON, events `push`, `pull_request` (in Forgejo's API it subscribes to every pull request event: opened, review requested, reviews, comments on pull requests) and `issue_comment`; signed with `webhook_secret` (`X-Forgejo-Signature`, HMAC-SHA256 hex). The core creates and repairs it (`EnsureWebhooks`) after every `Setup` and daily; see "Spec gaps decided in #63". Forgejo blocks webhooks to private addresses by default, so the template allows the workspace host (`[webhook] ALLOWED_HOST_LIST`).

### GitHub (#64, #65, #66)

- Connect: the GitHub App **manifest flow** for the organization. The core creates the instance (`connecting`) first, so the manifest can carry `https://<workspace>/api/v1/webhooks/<instanceId>` as the webhook URL, the SPA callback routes as redirect, setup and callback URLs, and the minimal permissions. The manifest code (valid one hour) is exchanged for app ID, slug, client ID and secret, webhook secret and private key, which are stored as credentials. Then the admin installs the App on the organization; the core verifies the installation with `GET /app/installations/{id}` (App JWT) and that its account is the expected organization, and stores organization ID, login and installation ID in `settings`. Manual setup (admin creates the App and pastes the values) is a documented fallback.
- Minimal permissions (candidate set; #64 verifies each against GitHub's documentation and documents the result in `docs/github.md`): organization **Members: read and write** (invitations, memberships, teams); repository **Metadata: read** (mandatory), **Pull requests: read**, **Issues: read** (comments for mentions). Webhook events: `pull_request`, `pull_request_review`, `issue_comment`, `pull_request_review_comment`, `issues` (added in #66 for mentions in issue bodies; Issues read covers it), `member`, `membership`, `organization`.
- Tokens: App JWT (RS256, `iat` 60 s in the past, `exp` ≤ 10 min) signed with go-jose (already a dependency); installation tokens minted on demand, cached in memory until 5 minutes before expiry, never persisted.
- Invitations by user ID (`POST /orgs/{org}/invitations` with `invitee_id`), so a login change cannot redirect an invitation. Removal with `DELETE /orgs/{org}/memberships/{username}` after resolving the current login from the ID (also cancels pending invitations). Team membership with the team membership endpoints. Organization owners are never removed.
- Users without a verified link: `pending_employee_action` with step `github.link-account`.
- Webhook deliveries are matched to the instance by the signed payload's `installation.id` and `organization.id` (see "Webhooks" → "Processing"), never by headers.

## Testing strategy

**Rule (AGENTS.md):** integration tests use real services; no mocks of provider APIs. The in-repo `testprovider` exercises the framework (jobs, results, retries, guard) without pretending to be a real provider. The one exception is GitHub.com, which cannot run in a container: its integration tests use `providers/github/githubfake`, which checks every request and response against GitHub's published OpenAPI description instead of hand-written expectations, and the live tests below exercise the real service.

| Area | Tests | Where |
|---|---|---|
| Framework | Unit tests with `testprovider`: plan/apply ordering, idempotent rerun, partial failure, retry classification, snooze on `RetryAfter`, removal guard, desired access computation, `to` validation corpus, webhook verification and dedupe | #58 |
| Store | Integration tests (`pgtest`) for every table: constraints, unique links, append-only events, credential sealing and AD binding, migration up/down | #58 |
| API | Allowed and denied tests for every operation; audit entries per mutation | #58 onward |
| Forgejo | testcontainers with the **same image reference** as the package manifest (read from the embedded manifest in the test): accounts, teams, webhooks with real signatures, deep link through a real OIDC round trip in domain mode and in local mode (`http://git.orglane.localhost`); agent install path in `tests/integration` | #61–#63 |
| GitHub | Unit and integration tests with the real adapter against `providers/github/githubfake`, a fake GitHub.com that validates every request and response against GitHub's official OpenAPI description (a pinned, vendored subset of github/rest-api-description, MIT) and a hand-written description of the OAuth web flow; live tests against a real test organization and test GitHub App (below) | #64–#66 |
| Web | Playwright: Settings → Apps, install flow, connect up to the GitHub redirect, the link step through the fake GitHub (acceptance build tag `githubtest`) | #58, #61, #64, #65 |

### GitHub live tests

Build tag `integration`, package `providers/github`, plus a scenario in `tests/integration` (#67). The maintainer creates the test resources and provides them through a GitHub Actions **environment** `github-live`, restricted to `main` and manual dispatch (never exposed to pull requests from forks, ADR 0017). Nothing about them is committed; the names below are placeholders.

**Maintainer checklist** (placeholders: organization `orglane-test`, App `Orglane CI`, a dedicated bot user):

| Kind | Name | Content |
|---|---|---|
| variable | `ORGLANE_TEST_GITHUB_ORG` | Login of the test organization (`orglane-test`; no real code or people) |
| variable | `ORGLANE_TEST_GITHUB_APP_ID` | App ID of the test GitHub App (`Orglane CI`), installed on that organization with the permissions from #64's `docs/github.md` |
| variable | `ORGLANE_TEST_GITHUB_APP_CLIENT_ID` | Its client ID |
| variable | `ORGLANE_TEST_GITHUB_INSTALLATION_ID` | Its installation on the test organization |
| variable | `ORGLANE_TEST_GITHUB_USER_LOGIN`, `ORGLANE_TEST_GITHUB_USER_ID` | The bot user (a member who is not an org owner) that tests invite, add to teams and remove |
| secret | `ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY` | The App's private key (PEM) |
| secret | `ORGLANE_TEST_GITHUB_WEBHOOK_SECRET` | The App's webhook secret (only for the real-signature check below; never committed) |
| secret | `ORGLANE_TEST_GITHUB_USER_TOKEN` | A fine-grained token of the bot user to accept invitations and open PRs/comments that trigger webhooks; only when repository cleanup is exercised, conditional test-only Administration: write plus bot admin access on explicitly selected disposable test repositories (see [janitor requirements](../github.md#live-test-resource-ownership-and-janitor)). No product App permission expansion, new secret, classic delete_repo scope or all-organization repository access; #64 connection and an empty janitor run require no repository admin privilege |

The workflow maps all of them to environment variables of the same names; tests read only those.

Behavior:

- Locally and on pull requests, missing values skip the tests with `t.Skip("GitHub live tests need ORGLANE_TEST_GITHUB_* (see docs/specs/providers.md#github-live-tests)")`.
- A workflow `github-live.yml` runs them on pushes to `main`, nightly and by `gh workflow run github-live.yml --ref <branch>` (how a maintainer checks a GitHub PR before merging, like `conformance.yml`). It sets `ORGLANE_REQUIRE_GITHUB_LIVE=1`, which turns a missing value into a failure.
- Runs are serialized (`concurrency: github-live`). Every test names its teams and repositories `orglane-test-<run id>-...` and deletes them; a janitor step removes leftovers older than a day.

**Webhooks in live tests.** Webhooks cannot reach CI runners, and the App's delivery log (`GET /app/hook/deliveries/{id}`) returns the payload as a parsed JSON object, not the raw request bytes, so a re-serialized payload is not guaranteed to match GitHub's signature. Two separate checks therefore cover webhooks:

1. **App payload semantics (always automated):** a test triggers a real event in the test organization, reads the delivery from the App's delivery log, serializes `request.payload`, signs it with the test-only fixture secret, and posts it with the recorded event and delivery headers to the local endpoint of a test instance whose webhook secret is that fixture secret. This covers real App payloads (including `installation`) end to end; it does not cover GitHub's signature.
2. **Real signature over raw bytes:** #66 must establish one byte-exact path before relying on it, in this order: (a) the delivery log, if the test proves for each delivery that HMAC-SHA256 of the serialized payload with `ORGLANE_TEST_GITHUB_WEBHOOK_SECRET` equals the recorded `X-Hub-Signature-256` (the test fails loudly, never falls back silently, if it does not); (b) `gh webhook forward` (GitHub's official CLI extension) for the test organization, which delivers GitHub-signed raw bodies to a local URL. If neither works, the real-signature check moves to the manual pre-release checklist below and #66's PR says so.

**Fixtures (#66, maintainer decision):** real payloads are captured once from the test organization through the delivery log and committed as JSON files (test-organization data only). Tests re-sign them with a test-only fixture secret (a constant in the test, never used by any real App); HMAC is deterministic, so signatures are reproducible. No real secret is ever committed.

**Manual pre-release checks** (maintainer decision: the OAuth browser leg stays manual; everything else runs in CI):

1. "Link your GitHub" end to end in a browser: the real authorization-code exchange with PKCE against GitHub, as the bot user, then the link appears verified and the invitation follows. CI covers the start URL (PKCE challenge, state storage), the callback's state handling, and the identity read (`GET /user` with `ORGLANE_TEST_GITHUB_USER_TOKEN`), but not the code exchange.
2. Only if neither automated path in "Real signature over raw bytes" works: a real App delivery to a reachable Orglane instance verifies.

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
| #58 Provider framework | Spec gaps decided in #58, Code layout and registry, Adapter contracts and Shared types, Capability matrix, Data model (all tables incl. OAuth states and removal batches), Credentials (staged rotation), Account links (table only), Desired access, Provisioning (incl. removal guard), Webhooks (endpoint, processing, replay, retention), Normalized events (storage, dedupe keys), API rows marked #58, Permissions, Audit, Testing (framework, store, API) |
| #59 Agent | [agent.md](agent.md): Process model, Transport, Agent API, Desired state and reconciliation, Instance directory, Package format (rendering, policy), Networking, Per-service database, Status and logs; here: Managed instance lifecycle (steps 2–3) |
| #60 Upgrades | [agent.md](agent.md): Reconcile loop (blocked generations), Snapshots, Upgrade (automatic start, write fence, recovery); here: `app_operations`, Managed instance lifecycle (revert, Setup after rollback) |
| #61 Forgejo package | [agent.md](agent.md) Package format, Local mode, Forgejo SSH; here: Managed instance lifecycle, Keys and removal, Ingress routes, Provider specifics → Forgejo (install, setup) |
| #62 Forgejo SSO and provisioning | Deep links and silent SSO, Forgejo OIDC configured automatically (redirect URI from the base URL; local-mode round-trip test), Provisioning, Provider specifics → Forgejo |
| #63 Forgejo webhooks | Webhooks, Normalized events, Provider specifics → Forgejo (webhooks) |
| #64 GitHub App | Adapter contracts (Connector), Credentials, Provider specifics → GitHub (connect, permissions, tokens), GitHub live tests |
| #65 GitHub linking and access | Account links, Desired access, Provisioning (removal guard, employee steps), Provider specifics → GitHub |
| #66 GitHub webhooks | Webhooks (installation matching, replay), Normalized events (dedupe keys, data), GitHub live tests (payload semantics, raw-signature path, re-signed fixtures) |
| #67 Acceptance | All; agent.md Upgrade (rollback demonstration) |

## Decided questions

The open questions of the first draft were answered by the maintainer (2026-11-04) and are applied above:

| Question | Decision | Section |
|---|---|---|
| GitHub OAuth browser leg in CI | Manual pre-release check; everything else against the real test org in CI | GitHub live tests |
| Webhook fixtures | No real secret committed; captured payloads re-signed with a test-only fixture secret; real signatures covered by the live check | GitHub live tests |
| Test org, App and user | Created by the maintainer; placeholders `orglane-test`, `Orglane CI`, a bot user; referenced only through the `github-live` environment | GitHub live tests (checklist) |
| `github.link-account` and welcome | Optional; does not block finishing welcome | Employee steps |
| `manage_existing_members` | Off by default; existing members are reported, never removed by default | Desired access, Removal guard |
| Forgejo organization name | The company slug (first label of `domain.base` until a company slug setting exists) | Provider specifics → Forgejo |
| Event retention | Kept until M3 defines retention | events, Replay and retention |

## Spec gaps decided in #58

Details the first draft left open, decided while implementing the framework (#58) and agreed with the coordinator. Later issues cite them.

| Gap | Decision | Where |
|---|---|---|
| `Lifecycle.Verify` and `EventSource.Verify` had the same name with different signatures, so a managed provider with webhooks (Forgejo) could not implement both | The lifecycle health check is `Lifecycle.CheckHealth(ctx, env, inst) error`; `EventSource.Verify` keeps its name | `providers/provider` |
| Staged rotation needs a hook that hands a candidate to the provider and classifies the answer | Optional `provider.CredentialRotator`: `ApplyCredential(ctx, env, inst, name, version, value) (Rotation, error)` with outcome `acknowledged`, `rejected` or `unknown` (an error counts as unknown). It must be idempotent. For credentials the provider mints (`Source = provider`, e.g. Forgejo `admin_token`) `value` is nil and the adapter returns the minted value, named after the version so a read-back can tell whether an earlier attempt committed. Agent credentials are not rotated through it (#59/#60: a new desired generation) | `providers/provider`, `core/internal/apps/credentials.go` |
| The candidate's state while it is handed over | A `pending` candidate becomes `uncertain` before the provider call, so a worker dying mid-call never leaves it looking untried | Credentials, step 3 |
| Serializing rotation and abandon | Forward and abandon attempts for one credential hold a session advisory lock on (instance, name) across the provider call and recheck the candidate under it. Abandoning is persisted as candidate state `abandoning` (it still verifies deliveries, is never activated by proof of use and blocks a new rotation) until the abandon job has re-applied the active value and discards it | Credentials, step 4 |
| Per-person order in the removal guard | `apply_removals` stops a person's removals at their first failure in the run; a permanent failure also drops that person's other open removals (they are replanned on retry or a desired-state change). Other people go on | Removal guard |
| Interrupted confirmed batches | A confirmed batch becomes `applied` only when none of its items is still `held`; a run interrupted (for example by the instance leaving a provisionable state) leaves it `confirmed`, and activation queues `apply_removals`, which resumes it | Removal guard |
| Drift checks and queued person jobs | `drift` is part of `provision_person` uniqueness, so a queued ordinary job (which may skip planning when the hashes match) never swallows the daily drift plan | Provisioning |
| Freshness of each removal at apply time | `apply_removals` applies each item under that person's provisioning lock (taken on the connection that holds its instance lock), after re-reading the removal (still in the state it was chosen in), the instance (still provisionable) and the person's desired-access hash; a stale item is dropped and the person replanned | Removal guard |
| Removals after a failed action | Plan order covers removals too: after the first failure nothing runs and no removal is queued (`not_attempted`) | Idempotency, retries, partial failure |
| Keeping grant ownership | **Rule: ownership (`granted_by_orglane`) and a verified `provisioned` link come only from provider-verifiable proof that Orglane's call created the account (`create_account`) or the invitation (`invite`), reported by the adapter as `ActionOutcome.Proof`**. Adding an existing account to a team proves only the team membership and never makes the account managed (which would allow removing it from the organization). Proof is: the confirmed response of Orglane's own call (`response`), or a provider-side marker an ensure checks (`marker`; Forgejo: an account bound to Orglane's auth source with this person's subject; GitHub: an invitation or membership created by Orglane's App installation). The core never infers ownership from access being there now. Intent is separate: before a granting action reaches the provider its intent is committed (`provisioning_results.pending_grant`; a failed write sends nothing). A definite rejection, or a grant answered without proof, clears it without ownership. An uncertain outcome keeps it, and the next run replays the action as an ensure. With proof, ownership, link and audit entries are recorded as on the first successful path. When the access is there without proof (or, for a person who lost access meanwhile, a read-only plan shows it exists), the intent is cleared, the access is neither adopted nor verified nor ever removed automatically, and the result becomes `failed` with `ownership_unproven` for an admin's review; the account is listed in the unmanaged accounts report. Retry Failed accepts it as unmanaged; an explicit Adopt action is a follow-up. Replay failures go through the same retry, snooze, exhaustion and permanent-failure handling as any action and stop the person's run. The flag is never reset and is also recovered from a `provisioned` link established by the instance | Desired access |
| Failed removals and failed results | A permanent removal failure, or a retryable one without `RetryAfter` on the job's last attempt, marks the removal `failed` and the person's result `failed` with the code and the failed action. A failed result is planned again only when the desired-access hash changes or an admin retries (Retry Failed); drift checks and unrelated changes leave it failed | Results, Removal guard |
| Abandoning an uncertain rotation is an admin action without an API row | `POST /apps/instances/{instanceId}/credentials/{name}/abandon` (`apps.manage`): the job re-applies the active value until acknowledged, then discards the candidate | API |
| The account realm of an instance | Optional `provider.AccountRealmer.AccountRealm(inst)`; without it the realm is the instance ID (Forgejo). GitHub returns `github.com` (#65) | `providers/provider` |
| Lifecycle for connected providers | Only managed providers have a lifecycle: the core ignores `Lifecycle` on connected adapters | `core/internal/apps/registry.go` |
| Storage of the unmanaged accounts report | Table `app_unmanaged_accounts (instance_id, external_id, login, seen_at)`, replaced by each `provision_instance` run from `Members`: provider accounts that no person with a managed result (granted by Orglane, or `manage_existing_members`) is linked to. Rows cascade with the instance; no secrets or audit entries (it is a report, nothing is changed) | Data model |
| Reason codes of `failed`, `degraded` and `disconnected` states | Stored in `app_instances.health_code` (`setup_failed`, `webhook_deleted`, ...); state changes are audited as `app_instance.state_changed` | Data model, Audit |
| "At most one queued and one running job per (instance, person)" with River, whose unique states must include `running` | `provision_person` is unique by `(instance_id, person_id, follow_up)` over available, pending, retryable, running and scheduled. An enqueue that finds the job running inserts the one `follow_up` job. The enqueue also stamps `provisioning_results.next_attempt_at`; a run that sees a stamp newer than its start snoozes itself once. The (instance, person) lock is a session advisory lock on a dedicated connection, so actions and their audit entries commit in short transactions. `provision_instance` and `apply_removals` use the same follow-up rule (a change while one runs queues one `follow_up` job) and a session lock per instance. A duplicate that waits is enough: River's unique insert locks it and fetching skips locked rows, so it starts after the change commits. A running one is not, because River records completion only after the work returned, so it may have read the state already. When both the job and its `follow_up` run, the change locks a waiting continuation of an earlier change or inserts one: the follow-up args plus a `continuation` ID that makes it unique on its own (checked insertion). At most one continuation waits | Provisioning |
| Whether a run may stop early | A run stops without planning when the desired hash equals `applied_hash` and the last run ended without an error or employee step, unless it is a drift check | Provisioning |
| Results while removals wait for the guard | `queued` (not `done`) with the removal actions' outcome `held`; `apply_removals` queues the person again after applying | Results |
| Removals planned while a batch is held | They join the held batch (it is superseded by a new one with all items), whatever the threshold says | Removal guard |
| Managed accounts for the removal threshold | Results with a successful apply (`applied_hash` set) that are managed (`granted_by_orglane`, or the instance manages existing members) | Removal guard |
| Webhook deliveries without a delivery ID header | Accepted: the delivery ID becomes `body:` + the first 16 bytes of the body's SHA-256 in hex, so a replay of the same signed bytes is still a duplicate (`200`) | Webhooks |
| Deliveries for instances in other states than `removed` and `connecting` | Stored; processed only in `active`, `degraded` and `upgrading`, otherwise `ignored` with `instance_inactive` | Webhooks |
| Problem details code | `Problem` has an optional `code` (stable, lower_snake_case) | API |
| The apps queue size | `jobs.apps_max_workers` (`ORGLANE_JOBS_APPS_MAX_WORKERS`), default 2 | Provisioning |
| The test provider outside Go tests | Build tag `appstest` registers `providers/testprovider` in the web acceptance binary only, like `githubimporttest`; production binaries never contain it | Code layout and registry |

## Spec gaps decided in #61

| Gap | Decision | Where |
|---|---|---|
| Who mints the Forgejo `admin_token`: `Env` cannot store a credential | `Setup` checks the service admin (basic auth with `admin_password`) and creates the organization; the token is minted by the staged rotation (`CredentialRotator.ApplyCredential`), whose read-back is the admin's token list: tokens are named `orglane-<version>`. Forward (value nil): a token of the version found there was minted by an attempt whose answer was lost, so it is deleted and minted again. Abandon (the core passes the active version and its value): the held token is kept and checked to still authenticate, every newer token (the abandoned candidate) is deleted, nothing is minted. `rejected` only for a definite refusal backed by a read-back that shows no token of the version; failed read-backs and cleanups, and refusals of a name that is taken, are `unknown`. This is correct with or without #80's rule that a rejection after an uncertain attempt keeps the candidate uncertain. Scopes: `write:admin`, `write:organization`, `read:user`. Retired tokens are not yet deleted in Forgejo (follow-up: a retire hook) | `providers/forgejo` |
| Settings a pattern cannot check (reserved names) | Optional `provider.SettingsValidator.ValidateSettings(settings)`, called by the core with the merged, typed settings on create and update; `*provider.SettingError` names the setting. Forgejo checks its naming rules for the immutable `organization` (letters and digits separated by single `-`, `.` or `_`, at most 40 characters, not a name Forgejo reserves or the service admin's) | `providers/provider`, `core/internal/apps` |
| Required settings (an immutable setting the adapter needs) | `SettingSpec.Required`: the core refuses to create an instance without it, omitted or null, unless `Default` fills it, and refuses to remove it later (`settings.<name>`: `is required`); the API shows it as `required`. An immutable setting an instance needs must be required, or an instance created without it could never be repaired. Forgejo's `organization` is required; its default (the first label of `domain.base`) depends on the configuration, so the install API (#61 phase 2) fills it in when the request leaves it out, and the install form shows it | `providers/provider`, `core/internal/apps/settings.go` |
| Setup and a refused organization creation | Read back before the refusal counts: found → done; not found → the refusal (permanent); a failed read-back keeps its own classification (a 503 or 429 stays retryable). Authentication failures are retryable (the configure hook may still run); attempts are bounded | `providers/forgejo` |
| The capability matrix of Forgejo before #62 and #63 | Honest: SSO, deep links, accounts, teams, team removal and webhooks are `planned` for M2 until #62/#63 implement them; `lifecycle.install`, `upgrade` and `snapshot` are supported | `providers/forgejo` |
| When an installing instance gets its routes | When the agent has applied its current generation (`observed_generation >= desired_generation > 0`), so a reinstall gets no route before its containers exist | `core/internal/apps/ingress.go` |
| Hosts of packages with several routes | M2 packages have one route; its host is the instance's base URL host. Further routes are left out until hosts per route exist | `core/internal/apps/ingress.go` |
| How the core knows a request is for an instance under maintenance | Maintenance routers forward to the core with the request header `X-Orglane-Maintenance: 1` (Traefik overwrites any client value); the core answers `503` with `Retry-After: 30` only for that header on a host other than the workspace host | `core/internal/ingress` |
| The configuration when the database is unavailable | `GET /traefik/config` answers `503`, so Traefik keeps its last configuration instead of dropping the app routes | `core/internal/ingress` |
| The DNS check of a service host | Domain mode only: the host must resolve (A and AAAA) to exactly the workspace host's addresses; both names are looked up rooted, so no search domain can stand in for them, with Go's resolver in strict-errors mode (`NewDNSResolver`), so a temporarily failing record type fails the check instead of passing on the other type's addresses; codes `dns_unresolved` (with the workspace's addresses), `dns_mismatch` (with both address sets) and `dns_lookup_failed` (with the lookup error). The install API wires it in (#61 phase 2) | `core/internal/apps/dnscheck.go` |
| Demo mode | Like local mode: the demo dataset has no app instances. With the agent (`install.sh --mode demo --managed`, workspace `http://orglane.localhost`) an admin installs Forgejo from Settings → Apps; without it Settings → Apps shows the `enable-managed` command. `make demo` runs no agent | deploy/README.md |

## Spec gaps decided in #62

Single sign-on and deep links (the first part of #62; provisioning and teams follow).

| Gap | Decision | Where |
|---|---|---|
| How a provider asks for an OIDC client | Optional `provider.OIDCRelyingParty.OIDCClient(inst)` returns the redirect URIs (built from `inst.BaseURL`) and the name of a credential with the new source `identity`. Creating the instance registers the confidential client `app-<key>` (`provider.OIDCClientID`, `client_secret_basic`) with `identity.CreateOIDCClientTx` and stores the secret, in the instance's transaction with the client's audit entry: instance, client and secret exist together or not at all. A refused client (identity's redirect URI rules, or `app-<key>` registered by an operator) is `invalid_input` on `oidc_client`. Without a registrar (`apps.Config.OIDC`) a relying party cannot be installed. The install flow (`Service.Install`, #75) goes through the same instance-creation path (`createInstance`), so this is the only place a client is registered; it runs before the first desired spec, whose credential versions include the secret | `providers/provider`, `core/internal/apps/oidc.go` |
| Rotating `oidc_client_secret` | Not rotatable in M2 (the registry refuses a rotatable `identity` credential); a new secret means a new client. Host changes, which must update the redirect URIs and Forgejo's source together, and deleting the client on purge come with those features | `core/internal/apps/registry.go` |
| Plain-http redirect URIs in local mode | Besides literal loopback hosts, only for a workspace that itself runs on plain http in local ingress mode (`identity.WithLocalServiceDomain(domain.base)`, set by the server only for `ingress.mode: local` with `domain.scheme: http`, and a base of `localhost` or a `*.localhost` name; an https workspace never gets it, also where config validation does not check mode and scheme together, without an internal listener): `http://` with exactly that base or a subdomain of it, written as a lowercase DNS name, no user info, no trailing dot. Browsers resolve every `*.localhost` name to loopback (RFC 6761), so this widens loopback to names, nothing else | `core/internal/identity/oidc_clients.go` |
| Forgejo's sign-in source | Named `orglane`, OpenID Connect with the instance's client, scopes `openid profile email`, local two-factor skipped (Orglane signs people in). The package's configure hook adds it, or updates it, with `forgejo admin auth add-oauth`/`update-oauth`; Forgejo fetches the discovery document then, so Orglane must be reachable (through Traefik's workspace alias), and the hook passes the client secret as a CLI argument (no other way). Forgejo loads the source lazily on the first sign-in after it was added; an update of an existing source (a new issuer or secret) takes effect after Forgejo restarts, which host changes will handle | `providers/forgejo/managed` |
| How the core learns the source's ID (accounts are created with `source_id`) | Forgejo has no API that lists sign-in sources, so the hook publishes `{"name":"orglane","id":N}` as the static asset `/assets/orglane/auth-source.json` (an ID is no secret). `Setup` requires it (retryable `sso_not_configured` until the hook ran) | `providers/forgejo` |
| Who may sign in to an app instance's client | Orglane decides (`apps.Service.OIDCAccess`): client IDs starting with `app-` are reserved for app instances (identity refuses them for any other client), and such a client signs in only a person with a verified `provisioned` link its instance established and access at the last provisioning run, while the instance is `active` or `degraded`. Everything that cannot be decided denies: no instance for the key (purged), a removed instance (data retained or not), an adapter this build does not have, an adapter that is no relying party, and store errors. Clients without the prefix are an operator's and unaffected. The decision runs where the sign-in completes (`identity/web` `ClientAccess`, `access_denied` back to the client) and again, authoritatively, in the transactions that issue the code, exchange it and issue tokens, refreshes included (`oidcop` `ClientAccess`, `access_denied` / `invalid_grant`); there it reads the instance, link and result rows `FOR SHARE`, so a withdrawal (an update of any of them) commits before the issuance and is seen, or after it, never in between. A code refused at exchange is spent. Reason: when Forgejo's callback gets a subject that no account is bound to while a Forgejo session is open, it links the subject to that session's account, whatever `ACCOUNT_LINKING` says (Forgejo 15, `routers/web/auth/oauth.go`), so an unprovisioned person could take over the account of whoever is signed in to Forgejo in that browser. With the check Forgejo only ever gets subjects whose own account Orglane created, and those sign in to that account. Tokens issued before a withdrawal stay valid until they expire (app clients get no refresh tokens) | `core/internal/apps/oidc.go`, `core/internal/identity/web/login.go`, `core/internal/identity/oidcop/storage.go` |
| Forgejo's sign-in settings | Password sign-in form off (`ENABLE_INTERNAL_SIGNIN`), registration off, no automatic accounts on first sign-in (`ENABLE_AUTO_REGISTRATION`), no linking of other accounts (`ACCOUNT_LINKING = disabled`): only accounts Orglane created (bound to the source with the person's subject, the person ID) sign in, and Orglane signs only their people in to the client (see the row above: `ACCOUNT_LINKING` alone does not stop Forgejo from linking a subject to an open session). The service admin keeps API access with basic auth. "Username from `preferred_username`" does not apply: Forgejo creates no accounts itself | `providers/forgejo/managed` |
| Deep links | `GET /api/v1/apps/instances/{id}/open?to=` (`authenticated`): the caller must be an active person (else 403) and the instance `active` or `degraded` (409); providers without deep links are 404. Forgejo's link is `<base>/user/oauth2/orglane?redirect_to=<to>`, confirmed against the pinned Forgejo by the round-trip test | `core/internal/apps/target.go`, `providers/forgejo` |
| The acceptance test | `core/internal/apps` `TestForgejoProvisioningAndSignOn`: the real server (OpenID Provider and job workers) in the test process behind the real Traefik, which polls its routes; real Forgejo installed by the real agent. People of mapped teams get accounts and memberships, an invited person gets theirs after accepting, a membership change propagates, a single removal applies through the guard after its delay, a mass removal is held until confirmed, and a cookie-jar client follows the deep link through Traefik and lands signed in; no Orglane session stops at Orglane's sign-in; a signed-in person without an Orglane-created account does not get in. Local mode only: the https round trip and a real browser (Playwright) run with the compose bundle (#61 phase 2, #67). The server takes the adapters' HTTP client from `server.RunWithStack` there, because the test process is not on the edge network | `core/internal/apps` |

Provisioning and teams (the second part of #62):

| Gap | Decision | Where |
|---|---|---|
| Which Forgejo account is the person's | The one bound to the Orglane source (`source_id`) with the person's OIDC subject (the person ID) as `login_name`, found with the admin API's `source_id` and `login_name` filters. That binding is the marker that proves ownership (`create_account` returns `ProofMarker` "auth_source" for an account that is there, `ProofResponse` for one this call created); a login of the same name that is not bound so is never adopted (`username_taken`, permanent) | `providers/forgejo/provision.go` |
| Plans and their order | A plan lists the account actions (`create_account`, `update_account`), then every addition (the base team first), then the removals, so a removal never runs before the grant that replaces it (the core stops at the first failure and queues no removal after it). A new account gets `create_account` and its additions in one plan. An account that is there without a recorded link gets a `create_account` ensure, which records the link | `providers/forgejo/provision.go` |
| Account updates and links | `update_account` makes the login (rename), display name and email the person's, finding the account by its binding. It reports the account, and the core refreshes the login of the person's verified link of that same account (`RefreshAccountLinkLogin`, informational, never creating or moving a link). A stale link login (a rename whose answer was lost) is planned as an update too | `providers/forgejo/provision.go`, `core/internal/apps/provision.go` |
| Identifiers at apply time | Every action finds the account by its binding (source and subject) when it runs and uses its current login; a login saved in the plan could have been renamed and reused meanwhile. Team actions re-read the team: an addition only to a team that is Orglane's now (else `team_not_managed`), a removal only from one (else it is dropped unchanged) | `providers/forgejo/provision.go` |
| Which teams Orglane manages | Teams it created for `create` mappings, named after the Orglane team slug and marked by their description ("Managed by Orglane ..."), and the base team `orglane-members`. The Owners team is never Orglane's, whatever its description says, so nobody becomes an owner. Mappings of mode `existing` are refused when they are set (`provider.TeamMappingValidator`), and stored references to teams that are not Orglane's are never synced or followed. Removals happen only from mappable teams (marked, not base, not Owners), only for managed accounts. Every team the person is mapped to must exist and be Orglane's before anything is planned: a missing one is the retryable `team_not_synced` (the next sync creates it), one that is not Orglane's the permanent `team_not_managed`; no plan ends without the person's new team. A team with a mapped name that is not marked is never taken over: that mapping stays unsynced and is logged (`team_name_taken`), the others sync | `providers/forgejo/provision.go` |
| Organization membership | In Forgejo 15 removing an account from its last team of an organization also removes it from the organization (`models/org_team.go`, `removeInvalidOrgUser`). Every managed account is therefore in the base team `orglane-members` (organization membership, no repository access), added before any removal and ensured again by each removal, so no removal is from the last team and nobody leaves the organization (`deprovision.org_removal` unsupported); no account is deleted (M5) | `providers/forgejo/provision.go` |
| What a mapped team grants | Write to code, issues, pull requests, releases, wiki and projects, read to packages and actions, on the repositories the organization's owners give the team in Forgejo (`includes_all_repositories` off) | `providers/forgejo/provision.go` |
| How the adapter authenticates | Basic auth as the service admin (`admin_password`), as `Setup` does. Switching provisioning to `admin_token` waits for the install flow to mint its first version | `providers/forgejo` |
| Error codes | Retryable: `forgejo_unavailable` (no answer, 5xx), `rate_limited` (with `Retry-After`), `service_admin_invalid` (401/403), `no_account`, `sso_not_configured`, `team_not_synced`. Permanent: `username_taken`, `invalid_login` (a refused rename to a username no account has), `account_refused`, `team_not_managed`, `forgejo_rejected`. A refused creation is read back first (a lost earlier attempt may have created the account), and the username lookup that tells `username_taken` from `account_refused` keeps its own classification when it fails. All have English texts in the person apps card | `providers/forgejo`, `web/src/lib/apps.ts` |

## Spec gaps decided in #63

| Gap | Decision | Where |
|---|---|---|
| Who calls `EventSource.EnsureWebhooks` | The core: in `RunSetup` after `Lifecycle.Setup` succeeds (a failure counts as a failed Setup attempt, so a Setup after a restore also re-asserts the hook), and daily: the drift check queues `orglane.apps.ensure_webhooks {instance_id}` for every active or degraded instance whose provider has an event source. An ensure installs the active `webhook_secret` at the provider, so it is a provider execution for that credential under the concurrency model above: it holds lock 1 (`credential`, `webhook_secret`) and does all database work on that connection (`heldConn`, so it completes with `database.max_conns=1`); under lock 2 it rechecks the instance (one that left the expected states, installing for Setup, active or degraded for the daily job, gets nothing, and Setup then neither activates it nor fails it with `setup_failed`); **while a candidate of `webhook_secret` exists it installs nothing**, since the provider may already use the candidate and putting the active value back would let a later proof by use activate a value the provider no longer uses (the rotation job sets the hook with the value that wins; Setup still succeeds); and its adapter call is an execution on the active version's row (`app_credential_calls`), deferred while any execution of the credential holds a lease and bounded by its own. It ends its execution after success; after a failure its lease expires, as after an unknown answer | `core/internal/apps/hooks.go` |
| An instance without a `webhook_secret` (created before its adapter declared the generated credential; generated credentials are created with the instance) | Before `EnsureWebhooks`, under the same lock and the instance row lock, the core creates the active version if the instance has no usable version at all (audited as `app_credential.created`). With only a rotation candidate it does nothing: the rotation establishes the active version, and Forgejo's `EnsureWebhooks` answers the retryable `webhook_secret_missing` meanwhile | `core/internal/apps/hooks.go`, `providers/forgejo/hooks.go` |
| Forgejo neither returns a hook's secret nor changes it on an update (`PATCH` ignores `config.secret`, checked against Forgejo 15) | The hook is Orglane's by its URL (the instance's webhook URL); its query `orglane_secret` carries a fingerprint of the hook's secret (the first 8 bytes of a domain-separated SHA-256, in hex; it reveals nothing about a 256-bit random secret, and the core's endpoint ignores the query). Ensure: a hook marked with the active secret is kept and updated (events, content type, `active`), which repairs a changed or deactivated hook; with no such hook (missing, deleted, or another secret after a restore) a new hook with the active secret is created first and the others deleted afterwards, so there is no moment without a hook; extra hooks (a creation whose answer was lost) are deleted. Known limits: a secret changed in Forgejo's settings while the URL keeps its mark is not noticed (rotating the secret repairs it), and deliveries that Forgejo had queued for a deleted hook but not yet sent are lost with it | `providers/forgejo/hooks.go` |
| Rotating the Forgejo `webhook_secret` | `ApplyCredential` replaces the hook as above with the candidate (forward) or the active value (abandon). Acknowledged only on Forgejo's answers; every failure, a refusal included, is `unknown`, because nothing can be read back to prove that an earlier attempt's change did not commit. While both hooks exist, a change is delivered twice with identical bodies, so the second delivery is a duplicate | `providers/forgejo/hooks.go` |
| Forgejo's event names | `X-Forgejo-Event` (Forgejo also sends the same values as `X-GitHub-Event`, `X-Gitea-Event` and `X-Gogs-Event`, and the same delivery UUID as `X-GitHub-Delivery`): `pull_request` for opened and review requests (told apart by `action`), `issue_comment` for comments on issues and pull requests (`is_pull`), `pull_request_comment`, `pull_request_approved` and `pull_request_rejected` for submitted reviews (`action: reviewed`), `push`. Other names are `ignored`; other actions of these names are `processed` with no events. Edited comments create no events | `providers/forgejo/events.go` |
| Forgejo events without an obvious normalization | Team review requests (`requested_reviewer` is null) and `@org/team` mentions have no target in M2 and create no event. Pushes of tags and branch deletions create no `code.branch.pushed`; Forgejo's push payload does not say whether a push was forced, so `Data` has no `forced`; `occurred_at` of a push is the receipt time (commit timestamps are set by authors). Issue openings are not subscribed (the spec's Forgejo events), so issue bodies create no mention events | `providers/forgejo/events.go` |
| Mention tokens | `@login` where `@` follows neither a login character nor `/`, `@` or a backtick (so e-mail addresses are no mentions), and the login (Forgejo's user name pattern, at most 40 characters, trailing `-._` dropped) is not followed by another letter or digit; in text as CommonMark 0.31.2 and its reference implementation commonmark.js parse it. Blocks: lines end at LF, CRLF or CR; block quotes and list items are containers (columns with tabs, lazy continuation lines, nested at most 32 deep); fenced and indented code, the seven kinds of HTML blocks (a kind-1 block ends at the closing tag of any of `script`, `pre`, `style`, `textarea`) and thematic breaks have no text; paragraphs lose their link reference definitions. Inline: code spans (ending with their block), raw HTML (whitespace in tags as JavaScript's `\s`, so form feeds too), autolinks, images and the destinations, titles and labels of links are no text, the text of links is, a reference is a link only to a defined label (labels match after full Unicode case folding, so `ß` matches `ss` and `ﬀ` matches `ff`), and backslash escapes are resolved (`a\@bob` is no mention). Two places follow the spec and cmark instead of commonmark.js: spaces *or tabs* around link destinations and titles (commonmark.js takes spaces only), and no backtick anywhere in a backtick fence's info string (commonmark.js stops looking at U+2028). Checked by a differential fuzz against commonmark.js (with those two rules) of over 500,000 generated documents. A small scanner instead of a Markdown library, because it is linear in the text's length (goldmark takes seconds on a few hundred kilobytes of nested list markers or unclosed links): backtick runs are indexed once, a link destination's end is looked up in an index of its parentheses' nesting, labels are at most 1000 characters, and the searches for the end of titles and raw HTML remember their result, so an adversarial body below the 5 MiB limit cannot occupy a worker | `providers/forgejo/events.go`, `providers/forgejo/markdown.go`, `providers/forgejo/markdown_inline.go` |
| Testing deliveries of a real Forgejo while the core runs in the test process | The test bundle has no route from Forgejo to the test's core, so Forgejo's delivery attempts fail; Forgejo records each attempt with its exact request (headers, `X-Forgejo-Signature`, body). The integration test hands these recorded requests to `ReceiveWebhook` as the endpoint would, so the bytes and signatures are Forgejo's own. Delivery over the network is a follow-up once the bundle can route to a core in the test process | `core/internal/apps/forgejo_webhooks_integration_test.go` |
## Spec gaps decided in #66

| Gap | Decision | Where |
|---|---|---|
| How a delivery changes the instance (installation deleted or suspended, the App's hook deleted), while `Normalize` is pure and returns events only | Optional `provider.DeliveryEffects.Effects(inst, d) (Effect, error)` (see "EventSource"), pure and from signed body fields like `Normalize`. `process_delivery` calls it before `Normalize` in the same transaction and applies it: `degraded` / `disconnected` with a health code (`^[a-z][a-z0-9_]{0,63}$`, else skipped) as an audited system transition; `restored` returns a `degraded` instance to `active` only while its health code is the effect's code, so it clears what an earlier effect set and nothing else; a transition the current state does not allow (an `upgrading` instance) is skipped, not an error. A delivery whose effect changed something but that has no events is `processed` (not `ignored`). Effects errors end the delivery like `Normalize`'s (foreign → `ignored` with the code, others `failed`) | `providers/provider`, `core/internal/apps/webhooks.go` |
| GitHub's effects | `installation` `deleted` → `disconnected` (`installation_deleted`); `suspend` → `degraded` (`installation_suspended`); `unsuspend` → restores `installation_suspended`; `meta` `deleted` (App-level, no installation) → `degraded` (`webhook_deleted`), restored by the next delivery for the installation, which proves the hook works again (installation events do not restore it). The installation check for `installation` events also compares `installation.account.id` with the organization ID. Known limit: deliveries are processed by independent jobs, so a suspension and its unsuspension processed in the wrong order leave the instance `degraded`; a later suspension cycle or reconnecting repairs it | `providers/github/events.go` |
| "Installation and membership events that affect provisioning state" | `organization` `member_added`/`member_removed` and team-scoped `membership` `added`/`removed` set `Effect.Resync`: the core queues `provision_instance` with reason `drift` (forced plans per person, unique, so a burst of deliveries queues at most one run and one follow-up) while the instance is provisionable and its adapter provisions. Membership changes Orglane made itself come back as deliveries too; their drift run finds nothing to do. Removals still go through the removal guard. `member` (repository collaborators) and `member_invited` do not resync | `providers/github/events.go`, `core/internal/apps/webhooks.go` |
| GitHub event names and actions | `pull_request` (`opened`, `review_requested`), `issues` (`opened`, mentions), `issue_comment` and `pull_request_review_comment` (`created`), `pull_request_review` (`submitted`), `organization` (`member_added`, `member_removed`, `member_invited`), `membership` (`added`, `removed`, scope `team`), `member` (`added`, `removed`); `installation` and `meta` only have effects; `github_app_authorization` is `ignored` (`not_normalized`; no user tokens are kept). Other names are `ignored`; other actions of these names are `processed` with no events. Pushes are not normalized for GitHub (`code.branch.pushed` is Forgejo's; it needs the Contents permission M2 does not request) | `providers/github/events.go` |
| Subjects and keys GitHub's payloads leave open | A comment on a pull request's conversation (`issue_comment` with `issue.pull_request`) has subject kind `pull_request` with the issue's ID as `ExternalID` (the payload has no pull request ID); `Repository` and `Number` identify it across events. Team review requests: key `pr:<pull_request.id>:review_requested:team:<team id>:<updated_at>`, `Data.requested_team` (slug), no targets. `member` (collaborators): subject `repository`. `organization` `member_invited`: the target is `user` (an invitation by e-mail address has no account and creates no event); `role` from `invitation.role` (`direct_member` → `member`, `admin`). A deleted team's `membership` payload has no slug: `Data.team` is left out. Membership events occur at the receipt time | `providers/github/events.go` |
| Logins in GitHub payloads | Actors, reviewers and members match GitHub's login pattern or an App bot login (`<name>[bot]`); mentions never match bots | `providers/github/events.go` |
| Mention extraction for GitHub | GitHub's own scanner (no shared Markdown code with Forgejo): `@login` with GitHub's login pattern, not after a letter, digit or one of ``-_./@`\`` (e-mail addresses, escaped `\@`), not followed by a letter, digit or `_`; `@org/team` skipped (no target in M2). Skipped as GitHub renders them: fenced code blocks (``` or ~~~, at most three spaces indented), indented code blocks (after a blank line) and code spans (a backtick run closed by the next run of the same length within the paragraph). Linear in the text: backtick runs are matched through per-length indexes whose cursors only move forward, and at most 256 KiB of a body is read (GitHub limits bodies to 65,536 characters) | `providers/github/mentions.go` |
| Fixtures | Recorded GitHub.com payloads from octokit/webhooks' payload examples (MIT, commit and license in `providers/github/testdata/webhooks`), signed in tests with a test-only fixture secret; examples recorded from repository webhooks get `installation.id` added before signing. Captured test-organization payloads are added when the `github-live` environment exists | `providers/github/testdata/webhooks/README.md` |
| The byte-exact real-signature path | Path (a) is implemented: `TestGitHubLiveWebhookDeliveries` reads recent deliveries from the App's delivery log (read only, no resources created) and fails loudly for any delivery whose logged payload bytes do not verify with `ORGLANE_TEST_GITHUB_WEBHOOK_SECRET` against the logged `X-Hub-Signature-256`; the same payloads, re-signed with the fixture secret, must pass `Verify`, `Effects` and `Normalize` for the test installation (payload semantics). It has not run yet (no `github-live` environment): until it passes on `main`, manual pre-release check 2 stays on the checklist, and if (a) fails there, (b) `gh webhook forward` is next | `providers/github/webhooks_live_integration_test.go` |
| Organization renames | Not handled in M2: the instance keeps the login it connected with (`settings.organization`); deliveries still match by ID. A follow-up refreshes the login from `organization` `renamed` | — |
