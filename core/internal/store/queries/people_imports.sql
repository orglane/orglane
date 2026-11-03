-- name: CreatePeopleImportRun :one
INSERT INTO people_import_runs (id, created_by, source, encoding, delimiter, headers_sealed)
VALUES (@id, @created_by, @source, @encoding, @delimiter, @headers_sealed) RETURNING *;

-- name: GetPeopleImportRun :one
SELECT * FROM people_import_runs WHERE id = @id AND created_by = @created_by;

-- name: LockPeopleImportRun :one
SELECT * FROM people_import_runs WHERE id = @id AND created_by = @created_by FOR UPDATE;

-- name: SetPeopleImportMapping :exec
UPDATE people_import_runs SET mapping = @mapping, state = 'previewed' WHERE id = @id;

-- name: FinishPeopleImport :exec
UPDATE people_import_runs SET state = 'applied' WHERE id = @id;

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
    'external', (SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY i.source,i.external_hash), '[]') FROM people_import_identities i WHERE i.person_id = @person_id),
    'fields', (SELECT coalesce(jsonb_agg(to_jsonb(f) ORDER BY f.id), '[]') FROM profile_fields f),
    'values', (SELECT coalesce(jsonb_agg(to_jsonb(v) ORDER BY v.field_id), '[]') FROM profile_field_values v WHERE v.person_id = @person_id),
    'memberships', (SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.team_id), '[]') FROM team_memberships m WHERE m.person_id = @person_id)
)::jsonb AS snapshot;
