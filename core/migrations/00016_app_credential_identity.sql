-- Credential row identity and provider call executions (docs/specs/providers.md
-- "Credentials", concurrency model). Versions number the values of one
-- credential and are reused once retention cleanup has deleted the newest row;
-- id is never reused. Rotation jobs and audit entries name the row by id, so a
-- stale job or report cannot act on a later row with the same version.
-- stuck_reported_at records the one rotation_stuck report of a row.
--
-- app_credential_calls holds one row per provider call execution that writes
-- a credential row's value: its id is the execution's fencing token, deadline
-- its lease in database time. At most one execution per credential holds a
-- lease. A lease never shrinks and is removed only by its own execution after
-- a definite answer, or by cleanup a day after it expired.

--
-- ended_at is when a version stopped being active or a candidate (retired or
-- discarded); retention of older versions and the drift grace period count
-- from it. drift_detected_at records the one drift report of a version: a
-- delivery signed with it after it ended.

-- +goose Up
ALTER TABLE app_credentials
    ADD COLUMN id uuid NOT NULL DEFAULT uuidv7(),
    ADD COLUMN stuck_reported_at timestamptz,
    ADD COLUMN ended_at timestamptz,
    ADD COLUMN drift_detected_at timestamptz;
CREATE UNIQUE INDEX app_credentials_id_key ON app_credentials (id);
-- Versions that ended before this migration: the best known time.
UPDATE app_credentials SET ended_at = created_at WHERE state IN ('retiring', 'discarded');

CREATE TABLE app_credential_calls (
    id            uuid PRIMARY KEY,
    credential_id uuid NOT NULL REFERENCES app_credentials (id) ON DELETE CASCADE,
    started_at    timestamptz NOT NULL DEFAULT now(),
    deadline      timestamptz NOT NULL
);
CREATE INDEX app_credential_calls_credential ON app_credential_calls (credential_id, deadline);

-- +goose Down
DROP TABLE app_credential_calls;
DROP INDEX app_credentials_id_key;
ALTER TABLE app_credentials
    DROP COLUMN drift_detected_at,
    DROP COLUMN ended_at,
    DROP COLUMN stuck_reported_at,
    DROP COLUMN id;
