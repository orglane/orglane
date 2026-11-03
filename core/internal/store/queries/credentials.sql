-- name: GetPersonCredentials :one
SELECT * FROM person_credentials WHERE person_id = @person_id;

-- name: SetPasswordHash :one
INSERT INTO person_credentials (person_id, password_hash, password_updated_at)
VALUES (@person_id, @password_hash, now())
ON CONFLICT (person_id) DO UPDATE
SET password_hash = EXCLUDED.password_hash, password_updated_at = now(), updated_at = now()
RETURNING *;

-- name: DismissPasskeyOffer :exec
INSERT INTO person_credentials (person_id, passkey_offer_dismissed_at)
VALUES (@person_id, now())
ON CONFLICT (person_id) DO UPDATE
SET passkey_offer_dismissed_at = now(), updated_at = now();

-- name: CreatePasskey :one
INSERT INTO passkeys (person_id, credential_id, name, credential)
VALUES (@person_id, @credential_id, @name, @credential)
RETURNING *;

-- name: GetPasskey :one
SELECT * FROM passkeys WHERE id = @id;

-- name: GetPasskeyByCredentialID :one
SELECT * FROM passkeys WHERE credential_id = @credential_id;

-- name: ListPersonPasskeys :many
SELECT * FROM passkeys WHERE person_id = @person_id ORDER BY id;

-- name: CountPersonPasskeys :one
SELECT count(*) FROM passkeys WHERE person_id = @person_id;

-- name: RecordPasskeyUse :exec
UPDATE passkeys SET credential = @credential, last_used_at = now() WHERE id = @id;

-- name: DeletePasskey :one
DELETE FROM passkeys WHERE id = @id AND person_id = @person_id
RETURNING *;

-- name: CreateWebAuthnCeremony :one
INSERT INTO webauthn_ceremonies (kind, person_id, session_data, expires_at)
VALUES (@kind, @person_id, @session_data, now() + make_interval(secs => @ttl_seconds::float8))
RETURNING *;

-- name: TakeWebAuthnCeremony :one
-- Returns and deletes an unexpired ceremony: each challenge is single use.
DELETE FROM webauthn_ceremonies
WHERE id = @id AND kind = @kind AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredWebAuthnCeremonies :execrows
DELETE FROM webauthn_ceremonies WHERE expires_at <= now();

-- name: GetSignInLock :one
-- Seconds until the account is unlocked; no row when it is not locked.
SELECT ceil(extract(epoch FROM locked_until - now()))::bigint AS retry_after_seconds
FROM sign_in_throttles
WHERE account = @account AND locked_until > now();

-- name: RecordSignInFailure :one
-- Counts consecutive failures; a failure after a quiet window starts over.
INSERT INTO sign_in_throttles (account, failures, last_failure_at)
VALUES (@account, 1, now())
ON CONFLICT (account) DO UPDATE
SET failures = CASE
        WHEN sign_in_throttles.last_failure_at < now() - make_interval(secs => @window_seconds::float8) THEN 1
        ELSE sign_in_throttles.failures + 1
    END,
    last_failure_at = now()
RETURNING *;

-- name: LockSignInAccount :exec
UPDATE sign_in_throttles
SET locked_until = now() + make_interval(secs => @lockout_seconds::float8), failures = 0
WHERE account = @account;

-- name: ClearSignInThrottle :exec
DELETE FROM sign_in_throttles WHERE account = @account;

-- name: DeleteStaleSignInThrottles :execrows
DELETE FROM sign_in_throttles
WHERE last_failure_at < now() - interval '1 day'
  AND (locked_until IS NULL OR locked_until < now());
