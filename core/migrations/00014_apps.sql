-- The provider framework (docs/specs/providers.md "Data model"): app
-- instances of compiled provider adapters, their desired specs and agent
-- operations (managed only), capability bindings, sealed credentials,
-- verified account links, single-use OAuth state, team mappings,
-- provisioning results and the removal guard, webhook deliveries and
-- normalized events.
--
-- Every mutation goes through core/internal/apps (or identity for account
-- links) and writes an audit entry in the same transaction.

-- +goose Up
CREATE TABLE app_instances (
    id                       uuid PRIMARY KEY DEFAULT uuidv7(),
    -- Short, stable, human-readable ID: agent directory, Compose project and
    -- svc_<key> database. Immutable.
    key                      text NOT NULL CHECK (key ~ '^[a-z][a-z0-9-]{0,22}[a-z0-9]$'),
    provider                 text NOT NULL CHECK (provider ~ '^[a-z][a-z0-9-]{1,30}$'),
    management               text NOT NULL CHECK (management IN ('managed', 'connected')),
    display_name             text NOT NULL CHECK (btrim(display_name) <> '' AND length(display_name) <= 100),
    state                    text NOT NULL CHECK (state IN ('connecting', 'installing', 'active', 'degraded',
                                 'upgrading', 'failed', 'disconnected', 'removing', 'removed')),
    base_url                 text,
    -- Non-secret, validated against the adapter's Manifest.Settings.
    settings                 jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(settings) = 'object'),
    access_policy            text NOT NULL CHECK (access_policy IN ('all_people', 'mapped_teams')),
    manage_existing_members  boolean NOT NULL DEFAULT false,
    -- managed only
    package_id               text,
    package_version          text,
    desired_generation       bigint NOT NULL DEFAULT 0,
    observed_generation      bigint NOT NULL DEFAULT 0,
    health                   text NOT NULL DEFAULT 'unknown' CHECK (health IN ('unknown', 'healthy', 'unhealthy')),
    -- Stable code (also the reason of failed and degraded states, e.g.
    -- setup_failed, webhook_deleted); never free text from providers.
    health_code              text CHECK (health_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    health_checked_at        timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    removed_at               timestamptz,
    -- Managed, removed without purge: directory, snapshots and svc_<key>
    -- database still exist.
    data_retained            boolean NOT NULL DEFAULT false,
    CHECK ((management = 'managed') = (package_id IS NOT NULL)),
    CHECK (NOT data_retained OR state = 'removed')
);
-- A key stays reserved while an instance uses it or its data is retained.
CREATE UNIQUE INDEX app_instances_key_key ON app_instances (key) WHERE state <> 'removed' OR data_retained;

-- The desired state the core pushes to the agent (docs/specs/agent.md).
-- Append-only history; the row with the highest generation is current.
CREATE TABLE app_desired_specs (
    instance_id         uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    generation          bigint NOT NULL CHECK (generation > 0),
    package_version     text NOT NULL,
    run_state           text NOT NULL CHECK (run_state IN ('running', 'stopped', 'absent')),
    purge               boolean NOT NULL DEFAULT false,
    settings            jsonb NOT NULL CHECK (jsonb_typeof(settings) = 'object'),
    -- {"db_password": 1, ...}: which credential versions to send.
    credential_versions jsonb NOT NULL CHECK (jsonb_typeof(credential_versions) = 'object'),
    reason              text NOT NULL CHECK (reason IN ('install', 'configure', 'upgrade', 'retry', 'revert', 'remove', 'reinstall', 'resync')),
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, generation)
);

-- Core-side record of agent operations.
CREATE TABLE app_operations (
    id                 uuid PRIMARY KEY DEFAULT uuidv7(),
    instance_id        uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    generation         bigint,
    kind               text NOT NULL CHECK (kind IN ('apply', 'upgrade', 'snapshot', 'restore', 'remove')),
    state              text NOT NULL CHECK (state IN ('queued', 'running', 'awaiting_confirmation',
                           'succeeded', 'failed', 'rolled_back')),
    phase              text,
    agent_operation_id uuid,
    error_code         text,
    from_version       text,
    to_version         text,
    started_by         uuid REFERENCES people (id), -- NULL for system
    created_at         timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz
);
CREATE INDEX app_operations_instance_idx ON app_operations (instance_id, created_at DESC);

-- Capability -> app instance, with at most one default per capability.
CREATE TABLE app_bindings (
    capability  text NOT NULL CHECK (capability ~ '^[a-z][a-z0-9_.]{0,40}$'),
    instance_id uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    is_default  boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (capability, instance_id)
);
CREATE UNIQUE INDEX app_bindings_default_key ON app_bindings (capability) WHERE is_default;

-- Credentials of app instances, sealed (crypto.PurposeAppCredentials, AD
-- bound to instance, name and version). Rotation is staged: pending or
-- uncertain candidates never replace the active version before the provider
-- acknowledged them; an abandoning candidate is discarded once the active
-- value is re-applied.
CREATE TABLE app_credentials (
    instance_id  uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    name         text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_]{0,40}$'),
    version      integer NOT NULL CHECK (version > 0),
    ciphertext   bytea NOT NULL,
    state        text NOT NULL CHECK (state IN ('pending', 'uncertain', 'abandoning', 'active', 'retiring', 'discarded')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    activated_at timestamptz,
    retire_at    timestamptz,
    PRIMARY KEY (instance_id, name, version)
);
CREATE UNIQUE INDEX app_credentials_active_key ON app_credentials (instance_id, name) WHERE state = 'active';
CREATE UNIQUE INDEX app_credentials_pending_key ON app_credentials (instance_id, name) WHERE state IN ('pending', 'uncertain', 'abandoning');

-- Verified links between people and provider accounts (and their revoked
-- history). Unverified import evidence stays in pending_account_links.
CREATE TABLE account_links (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    person_id   uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    provider    text NOT NULL,
    realm       text NOT NULL,
    external_id text NOT NULL CHECK (length(external_id) BETWEEN 1 AND 100),
    login       text NOT NULL CHECK (length(login) BETWEEN 1 AND 100),
    method      text NOT NULL CHECK (method IN ('oauth', 'provisioned')),
    instance_id uuid REFERENCES app_instances (id) ON DELETE SET NULL,
    state       text NOT NULL DEFAULT 'verified' CHECK (state IN ('verified', 'revoked')),
    evidence    text CHECK (evidence IN ('matched', 'mismatched', 'none')),
    verified_at timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK ((state = 'revoked') = (revoked_at IS NOT NULL))
);
CREATE UNIQUE INDEX account_links_account_key ON account_links (provider, realm, external_id) WHERE state = 'verified';
CREATE UNIQUE INDEX account_links_person_key ON account_links (person_id, provider, realm) WHERE state = 'verified';

-- Single-use state for browser round trips to providers (account
-- verification, connect flows). Only the hash of the state is stored.
CREATE TABLE app_oauth_states (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    kind          text NOT NULL CHECK (kind IN ('link', 'connect')),
    state_hash    bytea NOT NULL UNIQUE CHECK (octet_length(state_hash) = 32),
    instance_id   uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    person_id     uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    session_id    uuid NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    step          text,
    pkce_verifier bytea, -- sealed (crypto.PurposeAppOAuthState, AD = row ID), link only
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    consumed_at   timestamptz,
    CHECK (expires_at > created_at)
);

CREATE TABLE app_team_mappings (
    instance_id   uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    team_id       uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    external_ref  text,
    external_name text,
    mode          text NOT NULL CHECK (mode IN ('create', 'existing')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, team_id),
    CHECK (mode = 'create' OR external_ref IS NOT NULL)
);

-- Provider-side accounts no verified, managed link accounts for: the
-- "unmanaged accounts" report, refreshed by provision_instance from
-- UserProvisioner.Members. Reported, never removed.
CREATE TABLE app_unmanaged_accounts (
    instance_id uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    external_id text NOT NULL CHECK (length(external_id) BETWEEN 1 AND 100),
    login       text NOT NULL CHECK (length(login) BETWEEN 1 AND 100),
    seen_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, external_id)
);

-- One row per person x app instance that has, or had, desired access or a
-- link.
CREATE TABLE provisioning_results (
    instance_id        uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    person_id          uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    status             text NOT NULL CHECK (status IN ('queued', 'done', 'pending_employee_action', 'failed')),
    access             boolean NOT NULL,
    employee_step      text,
    granted_by_orglane boolean NOT NULL DEFAULT false,
    desired_hash       bytea CHECK (octet_length(desired_hash) = 32),
    applied_hash       bytea CHECK (octet_length(applied_hash) = 32),
    -- Last plan: [{kind, key, outcome}], no secrets.
    actions            jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(actions) = 'array'),
    error_code         text,
    attempts           integer NOT NULL DEFAULT 0,
    last_attempt_at    timestamptz,
    next_attempt_at    timestamptz,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, person_id),
    CHECK ((status = 'pending_employee_action') = (employee_step IS NOT NULL))
);
CREATE INDEX provisioning_results_person_idx ON provisioning_results (person_id);

-- The removal guard: every removal is planned into provisioning_removals
-- and applied by one per-instance job; mass removals are held in a batch
-- until an admin confirms them.
CREATE TABLE provisioning_removal_batches (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    instance_id  uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    state        text NOT NULL CHECK (state IN ('held', 'confirmed', 'applied', 'superseded')),
    item_count   integer NOT NULL CHECK (item_count >= 0),
    -- SHA-256 over the sorted (removal id, desired_hash) pairs.
    items_hash   bytea NOT NULL CHECK (octet_length(items_hash) = 32),
    created_at   timestamptz NOT NULL DEFAULT now(),
    confirmed_by uuid REFERENCES people (id),
    confirmed_at timestamptz
);
CREATE UNIQUE INDEX provisioning_removal_batches_held_key ON provisioning_removal_batches (instance_id) WHERE state = 'held';

CREATE TABLE provisioning_removals (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    instance_id  uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    person_id    uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    action       jsonb NOT NULL CHECK (jsonb_typeof(action) = 'object'),
    action_key   text NOT NULL,
    desired_hash bytea NOT NULL CHECK (octet_length(desired_hash) = 32),
    state        text NOT NULL CHECK (state IN ('pending', 'held', 'applied', 'dropped', 'failed')),
    batch_id     uuid REFERENCES provisioning_removal_batches (id),
    error_code   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX provisioning_removals_open_key ON provisioning_removals (instance_id, person_id, action_key)
    WHERE state IN ('pending', 'held');
CREATE INDEX provisioning_removals_applied_idx ON provisioning_removals (instance_id, updated_at) WHERE state = 'applied';
CREATE INDEX provisioning_removals_batch_idx ON provisioning_removals (batch_id) WHERE batch_id IS NOT NULL;

-- Verified webhook deliveries. The body is sealed
-- (crypto.PurposeWebhookDeliveries, AD = instance and row ID) and cleared
-- after retention.
CREATE TABLE webhook_deliveries (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    instance_id  uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    delivery_id  text NOT NULL CHECK (length(delivery_id) BETWEEN 1 AND 100),
    event_name   text NOT NULL CHECK (length(event_name) <= 100),
    received_at  timestamptz NOT NULL DEFAULT now(),
    body         bytea,
    headers      jsonb NOT NULL CHECK (jsonb_typeof(headers) = 'object'),
    body_sha256  bytea NOT NULL CHECK (octet_length(body_sha256) = 32),
    status       text NOT NULL DEFAULT 'received' CHECK (status IN ('received', 'processed', 'ignored', 'failed')),
    error_code   text,
    event_count  integer NOT NULL DEFAULT 0,
    processed_at timestamptz
);
CREATE UNIQUE INDEX webhook_deliveries_dedupe_key ON webhook_deliveries (instance_id, delivery_id);
-- Replay key: the authenticated bytes, not the unsigned delivery header.
CREATE UNIQUE INDEX webhook_deliveries_body_key ON webhook_deliveries (instance_id, body_sha256);
CREATE INDEX webhook_deliveries_received_idx ON webhook_deliveries (received_at);

-- Normalized events. Append-only like audit_entries; kept until M3 defines
-- their retention. delivery_id has no foreign key: deliveries expire,
-- events stay.
CREATE TABLE events (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    type           text NOT NULL CHECK (type ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){2}$'),
    schema_version integer NOT NULL CHECK (schema_version > 0),
    instance_id    uuid NOT NULL REFERENCES app_instances (id) ON DELETE RESTRICT,
    provider       text NOT NULL,
    delivery_id    uuid,
    dedupe_key     text NOT NULL CHECK (length(dedupe_key) BETWEEN 1 AND 500),
    occurred_at    timestamptz NOT NULL,
    received_at    timestamptz NOT NULL,
    actor          jsonb NOT NULL CHECK (jsonb_typeof(actor) = 'object'),
    subject        jsonb NOT NULL CHECK (jsonb_typeof(subject) = 'object'),
    targets        jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(targets) = 'array'),
    data           jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(data) = 'object')
);
CREATE UNIQUE INDEX events_dedupe_key ON events (instance_id, dedupe_key);
CREATE INDEX events_instance_time_idx ON events (instance_id, occurred_at DESC);

-- +goose StatementBegin
CREATE FUNCTION events_reject_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'events is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER events_no_update_delete
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION events_reject_change();

CREATE TRIGGER events_no_truncate
    BEFORE TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION events_reject_change();

-- +goose Down
DROP TABLE events;
DROP FUNCTION events_reject_change();
DROP TABLE webhook_deliveries;
DROP TABLE provisioning_removals;
DROP TABLE provisioning_removal_batches;
DROP TABLE provisioning_results;
DROP TABLE app_unmanaged_accounts;
DROP TABLE app_team_mappings;
DROP TABLE app_oauth_states;
DROP TABLE account_links;
DROP TABLE app_credentials;
DROP TABLE app_bindings;
DROP TABLE app_operations;
DROP TABLE app_desired_specs;
DROP TABLE app_instances;
