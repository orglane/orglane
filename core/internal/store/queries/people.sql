-- name: CreatePerson :one
INSERT INTO people (username, work_email, display_name, title, contact_email, status)
VALUES (@username, @work_email, @display_name, sqlc.narg(title), sqlc.narg(contact_email), @status)
RETURNING *;

-- name: GetPerson :one
SELECT * FROM people WHERE id = @id;

-- name: GetPersonForUpdate :one
SELECT * FROM people WHERE id = @id FOR UPDATE;

-- name: GetPersonByWorkEmail :one
SELECT * FROM people WHERE lower(work_email) = lower(@work_email);

-- name: UpdatePerson :one
UPDATE people
SET username = @username, work_email = @work_email, display_name = @display_name, title = sqlc.narg(title), manager_id = sqlc.narg(manager_id), updated_at = clock_timestamp()
WHERE id = @id
RETURNING *;

-- name: SetPersonStatus :one
UPDATE people SET status = @status, updated_at = clock_timestamp()
WHERE id = @id
RETURNING *;

-- name: GetPersonByUsername :one
SELECT * FROM people WHERE username = @username;

-- name: CountPeople :one
SELECT count(*) FROM people;

-- name: ListDirectory :many
-- The people directory: everyone except offboarded people, by display name,
-- with the keys of their roles and whether they have an avatar.
SELECT
    p.*,
    coalesce(
        (SELECT array_agg(r.key ORDER BY r.id)::text[]
         FROM person_roles pr JOIN roles r ON r.id = pr.role_id
         WHERE pr.person_id = p.id),
        '{}'::text[]
    )::text[] AS role_keys,
    EXISTS (SELECT 1 FROM person_avatars a WHERE a.person_id = p.id) AS has_avatar
FROM people p
WHERE p.status <> 'offboarded'
ORDER BY lower(p.display_name), p.id;

-- name: ListDirectoryPage :many
SELECT
    sqlc.embed(p),
    coalesce((SELECT array_agg(r.key ORDER BY r.key)::text[]
              FROM person_roles pr JOIN roles r ON r.id = pr.role_id
              WHERE pr.person_id = p.id), '{}'::text[])::text[] AS role_keys,
    EXISTS (SELECT 1 FROM person_avatars a WHERE a.person_id = p.id) AS has_avatar,
    coalesce((SELECT jsonb_agg(jsonb_build_object('id', t.id, 'name', t.name, 'slug', t.slug) ORDER BY lower(t.name), t.id)
              FROM team_memberships m JOIN teams t ON t.id = m.team_id
              WHERE m.person_id = p.id AND t.archived_at IS NULL), '[]'::jsonb)::jsonb AS teams
FROM people p
WHERE (p.status <> 'offboarded' OR sqlc.narg(updated_since)::timestamptz IS NOT NULL)
  AND (sqlc.narg(updated_since)::timestamptz IS NULL OR p.directory_updated_at >= sqlc.narg(updated_since)::timestamptz)
  AND (sqlc.narg(cursor)::uuid IS NULL OR p.id > sqlc.narg(cursor)::uuid)
  AND (@search::text = '' OR lower(p.display_name || ' ' || p.username || ' ' || p.work_email || ' ' || coalesce(p.title, '')) LIKE lower(@search::text) ESCAPE E'\\')
  AND (sqlc.narg(team_id)::uuid IS NULL OR EXISTS (
      SELECT 1 FROM team_memberships m JOIN teams t ON t.id = m.team_id
      WHERE m.person_id = p.id AND m.team_id = sqlc.narg(team_id)::uuid AND t.archived_at IS NULL))
  AND (@role_key::text = '' OR EXISTS (
      SELECT 1 FROM person_roles pr JOIN roles r ON r.id = pr.role_id
      WHERE pr.person_id = p.id AND r.key = @role_key::text))
ORDER BY p.id
LIMIT @page_size;

-- name: GetDirectoryPerson :one
SELECT
    sqlc.embed(p),
    coalesce((SELECT array_agg(r.key ORDER BY r.key)::text[]
              FROM person_roles pr JOIN roles r ON r.id = pr.role_id
              WHERE pr.person_id = p.id), '{}'::text[])::text[] AS role_keys,
    EXISTS (SELECT 1 FROM person_avatars a WHERE a.person_id = p.id) AS has_avatar,
    coalesce((SELECT jsonb_agg(jsonb_build_object('id', t.id, 'name', t.name, 'slug', t.slug) ORDER BY lower(t.name), t.id)
              FROM team_memberships m JOIN teams t ON t.id = m.team_id
              WHERE m.person_id = p.id AND t.archived_at IS NULL), '[]'::jsonb)::jsonb AS teams
FROM people p
WHERE p.id = @id AND p.status <> 'offboarded';

-- name: ListManagerAncestors :many
WITH RECURSIVE chain (id, manager_id) AS (
    SELECT p.id, p.manager_id FROM people p WHERE p.id = @id
    UNION
    SELECT p.id, p.manager_id FROM people p JOIN chain c ON p.id = c.manager_id
)
SELECT chain.id FROM chain;
