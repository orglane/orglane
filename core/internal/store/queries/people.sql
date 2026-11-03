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
SET username = @username, work_email = @work_email, display_name = @display_name, title = sqlc.narg(title), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetPersonStatus :one
UPDATE people SET status = @status, updated_at = now()
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
