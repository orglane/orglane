-- name: CreatePeopleImportRun :one
INSERT INTO people_import_runs (id, created_by, source, encoding, delimiter, headers_sealed, expires_at)
VALUES (@id, @created_by, @source, @encoding, @delimiter, @headers_sealed,
    now() + sqlc.arg(retention_seconds)::bigint * interval '1 second') RETURNING *;

-- name: GetPeopleImportRun :one
-- An expired run is gone even before the cleanup job deletes it.
SELECT * FROM people_import_runs WHERE id = @id AND created_by = @created_by AND expires_at > now();

-- name: LockPeopleImportRun :one
SELECT * FROM people_import_runs WHERE id = @id AND created_by = @created_by AND expires_at > now() FOR UPDATE;

-- name: SetPeopleImportMapping :exec
UPDATE people_import_runs SET mapping = @mapping, state = 'previewed' WHERE id = @id;

-- name: FinishPeopleImport :one
UPDATE people_import_runs SET state = 'applied', applied_at = now(),
    expires_at = now() + sqlc.arg(retention_seconds)::bigint * interval '1 second'
WHERE id = @id RETURNING *;

-- name: ErasePeopleImportRun :one
-- Keeps value-free captions only; the sealed headers are gone afterwards.
UPDATE people_import_runs SET headers_sealed = NULL, captions = sqlc.arg(captions)::text[]
WHERE id = @id RETURNING *;

-- name: ErasePeopleImportRows :exec
UPDATE people_import_rows SET payload_sealed = NULL, preview_sealed = NULL WHERE run_id = @run_id;

-- name: ListUnerasedAppliedPeopleImportRuns :many
-- Runs applied before apply erased their source material.
SELECT * FROM people_import_runs WHERE state = 'applied' AND headers_sealed IS NOT NULL
ORDER BY id LIMIT sqlc.arg(max_runs)::int FOR UPDATE;

-- name: DeletePeopleImportRun :one
DELETE FROM people_import_runs WHERE id = @id RETURNING id, state;

-- name: DeleteExpiredPeopleImportRuns :many
DELETE FROM people_import_runs WHERE id IN (
    SELECT id FROM people_import_runs WHERE expires_at <= now()
    ORDER BY expires_at LIMIT sqlc.arg(max_runs)::int FOR UPDATE)
RETURNING id, state;

-- name: InsertPeopleImportRow :exec
INSERT INTO people_import_rows (run_id, row_number, payload_sealed, error_code)
VALUES (@run_id, @row_number, @payload_sealed, @error_code);

-- name: ListPeopleImportRows :many
SELECT * FROM people_import_rows WHERE run_id = @run_id ORDER BY row_number;

-- name: PreviewPeopleImportRow :exec
UPDATE people_import_rows SET decision = @decision, error_code = @error_code,
    preview_sealed = @preview_sealed, result = 'pending'
WHERE run_id = @run_id AND row_number = @row_number;

-- name: ResultPeopleImportRow :exec
UPDATE people_import_rows SET result = @result, error_code = @error_code
WHERE run_id = @run_id AND row_number = @row_number;

-- name: FindPeopleImportWorkEmail :many
SELECT * FROM people WHERE lower(work_email) = lower(@email);

-- name: FindPeopleImportContactEmail :many
SELECT * FROM people WHERE lower(contact_email) = lower(@email);

-- name: FindPeopleImportExternalID :many
SELECT p.* FROM people p JOIN people_import_identities i ON i.person_id = p.id
WHERE i.source = @source AND i.external_hash = @external_hash;

-- name: PutPeopleImportExternalID :exec
INSERT INTO people_import_identities (source, external_hash, person_id)
VALUES (@source, @external_hash, @person_id) ON CONFLICT DO NOTHING;

-- name: FindPeopleImportTeams :many
SELECT * FROM teams WHERE lower(name) = lower(@name) ORDER BY id;

-- name: UpdatePeopleImportContact :exec
UPDATE people SET contact_email = sqlc.narg(contact_email), updated_at = now() WHERE id = @id;

-- name: LockPeopleImportTeams :many
-- Imports take every existing team lock before any person lock. This avoids
-- cross-row savepoint lock inversion against team membership mutations.
SELECT * FROM teams ORDER BY id FOR UPDATE;

-- name: PeopleImportSnapshot :one
-- Ciphertext is hashed only in process; no sensitive plaintext is selected.
SELECT jsonb_build_object(
    'person', (SELECT to_jsonb(p) FROM people p WHERE p.id = @person_id),
    'roles', (SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY r.id), '[]') FROM roles r),
    'permissions', (SELECT coalesce(jsonb_agg(to_jsonb(rp) ORDER BY rp.role_id,rp.permission), '[]') FROM role_permissions rp),
    'grants', (SELECT coalesce(jsonb_agg(to_jsonb(pr) ORDER BY pr.role_id), '[]') FROM person_roles pr WHERE pr.person_id = @person_id),
    'pending_accounts', (SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY a.organization_id,a.external_id), '[]') FROM pending_account_links a WHERE a.person_id = @person_id),
    'external', (SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.source,i.external_hash), '[]') FROM people_import_identities i WHERE i.person_id = @person_id),
    'fields', (SELECT coalesce(jsonb_agg(to_jsonb(f) ORDER BY f.id), '[]') FROM profile_fields f),
    'values', (SELECT coalesce(jsonb_agg(to_jsonb(v) ORDER BY v.field_id), '[]') FROM profile_field_values v WHERE v.person_id = @person_id),
    'memberships', (SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.team_id), '[]') FROM team_memberships m WHERE m.person_id = @person_id)
)::jsonb AS snapshot;

-- name: PutPendingGitHubAccount :execrows
INSERT INTO pending_account_links (provider, organization_id, external_id, person_id, login, avatar_url)
VALUES ('github', @organization_id, @external_id, @person_id, @login, @avatar_url)
ON CONFLICT (provider, organization_id, external_id) DO UPDATE
SET login = EXCLUDED.login, avatar_url = EXCLUDED.avatar_url
WHERE pending_account_links.person_id = EXCLUDED.person_id;

-- name: FindPendingGitHubAccount :many
SELECT p.* FROM people p JOIN pending_account_links a ON a.person_id = p.id
WHERE a.provider = 'github' AND a.organization_id = @organization_id AND a.external_id = @external_id;
