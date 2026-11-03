-- name: CreateInvitation :one
INSERT INTO invitations (person_id, token_hash, locale, invited_by, email_id, expires_at)
VALUES (@person_id, @token_hash, @locale, sqlc.narg(invited_by), sqlc.narg(email_id), now() + make_interval(secs => @ttl_seconds::float8))
RETURNING *;

-- name: GetInvitation :one
SELECT * FROM invitations WHERE id = @id;

-- name: GetInvitationForUpdate :one
SELECT * FROM invitations WHERE id = @id FOR UPDATE;

-- name: GetInvitationByTokenHash :one
SELECT * FROM invitations WHERE token_hash = @token_hash;

-- name: ReissueInvitation :one
-- A new token and expiry, e.g. when the invitation is resent; a revoked
-- invitation becomes valid again.
UPDATE invitations
SET token_hash = @token_hash, locale = @locale, email_id = sqlc.narg(email_id), issued_at = now(),
    expires_at = now() + make_interval(secs => @ttl_seconds::float8),
    revoked_at = NULL, expiry_recorded_at = NULL
WHERE id = @id
RETURNING *;

-- name: RevokeInvitation :one
UPDATE invitations SET revoked_at = now() WHERE id = @id RETURNING *;

-- name: AcceptInvitation :one
UPDATE invitations SET accepted_at = now() WHERE id = @id RETURNING *;

-- name: ListUnrecordedExpiredInvitations :many
-- Expired, still open invitations whose expiry is not audited yet.
SELECT * FROM invitations
WHERE expires_at <= now() AND accepted_at IS NULL AND revoked_at IS NULL AND expiry_recorded_at IS NULL
ORDER BY expires_at
LIMIT 500
FOR UPDATE SKIP LOCKED;

-- name: MarkInvitationExpiryRecorded :exec
UPDATE invitations SET expiry_recorded_at = now() WHERE id = @id;

-- name: ListInvitations :many
-- Open invitations (not accepted) of people who are still invited, newest
-- first, with the person and the delivery state of the current email.
SELECT
    i.id, i.person_id, i.locale, i.invited_by, i.created_at, i.issued_at, i.expires_at, i.revoked_at,
    p.username, p.work_email, p.display_name, p.contact_email,
    coalesce(
        (SELECT array_agg(r.key ORDER BY r.id)::text[]
         FROM person_roles pr JOIN roles r ON r.id = pr.role_id
         WHERE pr.person_id = p.id),
        '{}'::text[]
    )::text[] AS role_keys,
    e.status AS email_status, e.attempts AS email_attempts, e.max_attempts AS email_max_attempts,
    e.last_error AS email_last_error, e.sent_at AS email_sent_at
FROM invitations i
JOIN people p ON p.id = i.person_id
LEFT JOIN email_outbox e ON e.id = i.email_id
WHERE i.accepted_at IS NULL AND p.status = 'invited'
ORDER BY i.issued_at DESC, i.id DESC;

-- name: GetInvitationDetails :one
-- One invitation as ListInvitations returns it.
SELECT
    i.id, i.person_id, i.locale, i.invited_by, i.created_at, i.issued_at, i.expires_at, i.revoked_at,
    p.username, p.work_email, p.display_name, p.contact_email,
    coalesce(
        (SELECT array_agg(r.key ORDER BY r.id)::text[]
         FROM person_roles pr JOIN roles r ON r.id = pr.role_id
         WHERE pr.person_id = p.id),
        '{}'::text[]
    )::text[] AS role_keys,
    e.status AS email_status, e.attempts AS email_attempts, e.max_attempts AS email_max_attempts,
    e.last_error AS email_last_error, e.sent_at AS email_sent_at
FROM invitations i
JOIN people p ON p.id = i.person_id
LEFT JOIN email_outbox e ON e.id = i.email_id
WHERE i.id = @id;
