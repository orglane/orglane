-- name: CreateDevice :one
INSERT INTO devices (person_id, name, platform)
VALUES (@person_id, @name, @platform)
RETURNING *;

-- name: GetDevice :one
SELECT * FROM devices WHERE id = @id;

-- name: GetDeviceForUpdate :one
SELECT * FROM devices WHERE id = @id FOR UPDATE;

-- name: ListPersonDevices :many
SELECT * FROM devices WHERE person_id = @person_id ORDER BY id;

-- name: RevokeDevice :one
UPDATE devices SET revoked_at = now()
WHERE id = @id AND revoked_at IS NULL
RETURNING *;

-- name: TouchDevice :exec
UPDATE devices SET last_seen_at = now()
WHERE id = @id AND last_seen_at < now() - interval '1 minute';
