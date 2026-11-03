-- The audit log: who did what, to what, from where (docs/architecture.md
-- "Core domain model"). Entries are written by package audit in the same
-- transaction as the change they describe.
--
-- The table is append-only: triggers reject UPDATE, DELETE and TRUNCATE for
-- every role, including the application's. Actors and targets are recorded
-- by kind and id without foreign keys, so entries outlive what they describe.

-- +goose Up
CREATE TABLE audit_entries (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    -- person: a signed-in person; system: the core itself (startup, jobs);
    -- module, agent: extensions and AI agents acting with granted scopes.
    actor_kind  text NOT NULL CHECK (actor_kind IN ('person', 'system', 'module', 'agent')),
    actor_id    text CHECK (actor_id <> ''),
    -- Dot-separated, e.g. person.created, session.revoked.
    action      text NOT NULL CHECK (action ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
    target_kind text CHECK (target_kind ~ '^[a-z][a-z0-9_]*$'),
    target_id   text CHECK (target_id <> ''),
    metadata    jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(metadata) = 'object'),
    ip          inet,
    user_agent  text CHECK (length(user_agent) <= 1024),
    CHECK (actor_kind = 'system' OR actor_id IS NOT NULL),
    CHECK ((target_kind IS NULL) = (target_id IS NULL))
);

CREATE INDEX audit_entries_occurred_at_idx ON audit_entries (occurred_at);
CREATE INDEX audit_entries_actor_idx ON audit_entries (actor_kind, actor_id, occurred_at);
CREATE INDEX audit_entries_target_idx ON audit_entries (target_kind, target_id, occurred_at);

-- +goose StatementBegin
CREATE FUNCTION audit_entries_reject_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_entries is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON audit_entries
    FOR EACH ROW EXECUTE FUNCTION audit_entries_reject_change();

CREATE TRIGGER audit_entries_no_truncate
    BEFORE TRUNCATE ON audit_entries
    FOR EACH STATEMENT EXECUTE FUNCTION audit_entries_reject_change();

-- +goose Down
DROP TABLE audit_entries;
DROP FUNCTION audit_entries_reject_change();
