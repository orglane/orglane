-- Import retention (docs/specs/people-import.md "Lifecycle"). Apply erases the
-- source material of a run (sealed headers, row payloads and previews) in its
-- own transaction and keeps only value-free captions, decisions and results.
-- Every run carries the time it expires; the import cleanup job deletes runs
-- once it has passed. Unfinished runs expire 24 hours after upload, applied
-- runs 90 days after apply. people_import_identities is not affected.

-- +goose Up
ALTER TABLE people_import_runs
    ALTER COLUMN headers_sealed DROP NOT NULL,
    -- Canonical field names or '' per source column (as the API shows them),
    -- kept once the sealed headers are erased.
    ADD COLUMN captions text[],
    ADD COLUMN applied_at timestamptz,
    ADD COLUMN expires_at timestamptz;
ALTER TABLE people_import_rows ALTER COLUMN payload_sealed DROP NOT NULL;

-- Runs applied before this migration take the time of their apply audit entry.
-- Their source material is erased by the cleanup job, which can decrypt the
-- headers to keep their captions.
UPDATE people_import_runs r SET applied_at = coalesce(
    (SELECT min(a.occurred_at) FROM audit_entries a
     WHERE a.target_kind = 'people_import' AND a.target_id = r.id::text AND a.action = 'import.applied'),
    r.created_at)
WHERE r.state = 'applied';
UPDATE people_import_runs SET expires_at = CASE
    WHEN state = 'applied' THEN applied_at + interval '90 days'
    ELSE created_at + interval '24 hours' END;

ALTER TABLE people_import_runs
    ALTER COLUMN expires_at SET NOT NULL,
    ADD CONSTRAINT people_import_runs_applied_at CHECK ((state = 'applied') = (applied_at IS NOT NULL)),
    ADD CONSTRAINT people_import_runs_headers CHECK (headers_sealed IS NOT NULL OR (state = 'applied' AND captions IS NOT NULL));
CREATE INDEX people_import_runs_expiry ON people_import_runs(expires_at);

-- +goose Down
-- Runs whose source material was erased cannot be represented without it;
-- they are deleted (with their rows) before the columns become required.
DELETE FROM people_import_runs r WHERE r.headers_sealed IS NULL
    OR EXISTS (SELECT 1 FROM people_import_rows w WHERE w.run_id = r.id AND w.payload_sealed IS NULL);
DROP INDEX people_import_runs_expiry;
ALTER TABLE people_import_rows ALTER COLUMN payload_sealed SET NOT NULL;
ALTER TABLE people_import_runs
    DROP CONSTRAINT people_import_runs_headers,
    DROP CONSTRAINT people_import_runs_applied_at,
    DROP COLUMN expires_at,
    DROP COLUMN applied_at,
    DROP COLUMN captions,
    ALTER COLUMN headers_sealed SET NOT NULL;
