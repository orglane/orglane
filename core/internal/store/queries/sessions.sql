-- name: CreateSession :one
-- The expiry is computed from the database clock, like every other
-- timestamp, so application clock skew cannot produce an expired session.
INSERT INTO sessions (person_id, device_id, token_hash, expires_at, auth_methods)
VALUES (@person_id, @device_id, @token_hash, now() + make_interval(secs => @ttl_seconds::float8), @auth_methods::text[])
RETURNING *;

-- name: GetSession :one
SELECT * FROM sessions WHERE id = @id;

-- name: GetActiveSessionByTokenHash :one
-- An active session is not revoked, not expired, belongs to an active person
-- and to a device that is not revoked.
SELECT s.*
FROM sessions s
JOIN devices d ON d.id = s.device_id
JOIN people p ON p.id = s.person_id
WHERE s.token_hash = @token_hash
  AND s.revoked_at IS NULL
  AND s.expires_at > now()
  AND d.revoked_at IS NULL
  AND p.status = 'active';

-- name: ListPersonSessions :many
SELECT * FROM sessions WHERE person_id = @person_id ORDER BY id;

-- name: RevokeSession :one
UPDATE sessions SET revoked_at = now()
WHERE id = @id AND revoked_at IS NULL
RETURNING *;

-- name: RevokeDeviceSessions :many
UPDATE sessions SET revoked_at = now()
WHERE device_id = @device_id AND revoked_at IS NULL
RETURNING *;

-- name: RevokePersonSessions :many
UPDATE sessions SET revoked_at = now()
WHERE person_id = @person_id AND revoked_at IS NULL
RETURNING *;

-- name: GetSessionForUpdate :one
SELECT * FROM sessions WHERE id = @id FOR UPDATE;

-- name: GetValidSessionByTokenHash :one
-- Like GetActiveSessionByTokenHash, and the session has been used within
-- the idle timeout.
SELECT s.*
FROM sessions s
JOIN devices d ON d.id = s.device_id
JOIN people p ON p.id = s.person_id
WHERE s.token_hash = @token_hash
  AND s.revoked_at IS NULL
  AND s.expires_at > now()
  AND s.last_seen_at > now() - make_interval(secs => @idle_seconds::float8)
  AND d.revoked_at IS NULL
  AND p.status = 'active';

-- name: TouchSession :exec
-- Records activity at most once a minute, so validating a session does not
-- write on every request.
UPDATE sessions SET last_seen_at = now()
WHERE id = @id AND last_seen_at < now() - interval '1 minute';

-- name: ListValidPersonSessions :many
SELECT s.id, s.device_id, s.created_at, s.last_seen_at, s.expires_at, s.auth_methods,
       d.name AS device_name, d.platform AS device_platform
FROM sessions s
JOIN devices d ON d.id = s.device_id
WHERE s.person_id = @person_id
  AND s.revoked_at IS NULL
  AND s.expires_at > now()
  AND s.last_seen_at > now() - make_interval(secs => @idle_seconds::float8)
  AND d.revoked_at IS NULL
ORDER BY s.id;
