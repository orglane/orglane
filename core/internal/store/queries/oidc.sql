-- name: CreateOIDCClient :one
INSERT INTO oidc_clients (id, name, secret_hash, application_type, auth_method, redirect_uris, post_logout_redirect_uris, grant_types)
VALUES (@id, @name, @secret_hash, @application_type, @auth_method, @redirect_uris::text[], @post_logout_redirect_uris::text[], @grant_types::text[])
RETURNING *;

-- name: GetOIDCClient :one
SELECT * FROM oidc_clients WHERE id = @id;

-- name: ListOIDCClients :many
SELECT * FROM oidc_clients ORDER BY id;

-- name: CreateOIDCAuthRequest :one
INSERT INTO oidc_auth_requests (
    client_id, redirect_uri, scopes, state, nonce, response_type, response_mode,
    code_challenge, code_challenge_method, prompt, max_age_seconds, login_hint, expires_at
) VALUES (
    @client_id, @redirect_uri, @scopes::text[], @state, @nonce, @response_type, @response_mode,
    @code_challenge, @code_challenge_method, @prompt::text[], @max_age_seconds, @login_hint,
    now() + make_interval(secs => @ttl_seconds::float8)
)
RETURNING *;

-- name: GetOIDCAuthRequest :one
SELECT * FROM oidc_auth_requests WHERE id = @id AND expires_at > now();

-- name: CompleteOIDCAuthRequest :one
-- Records who signed in for a pending request. A request completes once.
UPDATE oidc_auth_requests
SET person_id = @person_id, session_id = @session_id, auth_time = @auth_time, auth_methods = @auth_methods::text[]
WHERE id = @id AND person_id IS NULL AND expires_at > now()
RETURNING *;

-- name: SetOIDCAuthCode :execrows
UPDATE oidc_auth_requests SET code_hash = @code_hash
WHERE id = @id AND person_id IS NOT NULL AND code_hash IS NULL AND expires_at > now();

-- name: ClaimOIDCAuthCode :one
-- Marks a code as used and returns its request; a code can be claimed once.
UPDATE oidc_auth_requests SET code_used_at = now()
WHERE code_hash = @code_hash AND code_used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: GetOIDCAuthRequestByCodeHash :one
SELECT * FROM oidc_auth_requests WHERE code_hash = @code_hash;

-- name: DeletePendingOIDCAuthRequest :exec
-- Deletes a request whose code was never exchanged. Exchanged requests stay
-- until they expire, for code reuse detection.
DELETE FROM oidc_auth_requests WHERE id = @id AND code_used_at IS NULL;

-- name: DeleteExpiredOIDCAuthRequests :execrows
DELETE FROM oidc_auth_requests WHERE expires_at <= now();

-- name: CreateOIDCAccessToken :one
INSERT INTO oidc_access_tokens (client_id, person_id, grant_id, scopes, audience, expires_at)
VALUES (@client_id, @person_id, @grant_id, @scopes::text[], @audience::text[], now() + make_interval(secs => @ttl_seconds::float8))
RETURNING *;

-- name: GetValidOIDCAccessToken :one
-- A valid access token is not revoked, not expired, of a grant that is not
-- revoked and belongs to an active person.
SELECT t.*
FROM oidc_access_tokens t
JOIN oidc_grants g ON g.id = t.grant_id
JOIN people p ON p.id = t.person_id
WHERE t.id = @id AND t.revoked_at IS NULL AND t.expires_at > now()
  AND g.revoked_at IS NULL AND p.status = 'active';

-- name: GetOIDCAccessToken :one
SELECT * FROM oidc_access_tokens WHERE id = @id;

-- name: RevokeOIDCAccessToken :execrows
UPDATE oidc_access_tokens SET revoked_at = now() WHERE id = @id AND revoked_at IS NULL;

-- name: CreateOIDCRefreshToken :one
INSERT INTO oidc_refresh_tokens (token_hash, grant_id, client_id, person_id, scopes, audience, auth_time, auth_methods, expires_at)
VALUES (@token_hash, @grant_id, @client_id, @person_id, @scopes::text[], @audience::text[], @auth_time, @auth_methods::text[], @expires_at)
RETURNING *;

-- name: GetOIDCRefreshTokenByHash :one
SELECT * FROM oidc_refresh_tokens WHERE token_hash = @token_hash;

-- name: UseOIDCRefreshToken :one
-- Marks a refresh token as used (rotated). Only one exchange can win.
UPDATE oidc_refresh_tokens SET used_at = now()
WHERE id = @id AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
RETURNING *;

-- name: CreateOIDCGrant :exec
INSERT INTO oidc_grants (id, client_id, person_id) VALUES (@id, @client_id, @person_id)
ON CONFLICT (id) DO NOTHING;

-- name: GetOIDCGrant :one
SELECT * FROM oidc_grants WHERE id = @id;

-- name: LockOIDCGrant :one
-- Serializes token issuance and revocation of a grant.
SELECT * FROM oidc_grants WHERE id = @id FOR UPDATE;

-- name: RevokeOIDCGrant :execrows
UPDATE oidc_grants SET revoked_at = now() WHERE id = @id AND revoked_at IS NULL;

-- name: DeleteExpiredOIDCGrants :execrows
-- Grants without tokens are kept for a day, beyond the lifetime of their
-- code, so code reuse is still detected.
DELETE FROM oidc_grants g
WHERE g.created_at < now() - interval '1 day'
  AND NOT EXISTS (SELECT 1 FROM oidc_refresh_tokens r WHERE r.grant_id = g.id)
  AND NOT EXISTS (SELECT 1 FROM oidc_access_tokens a WHERE a.grant_id = g.id);

-- name: RevokeOIDCGrantRefreshTokens :execrows
UPDATE oidc_refresh_tokens SET revoked_at = now() WHERE grant_id = @grant_id AND revoked_at IS NULL;

-- name: RevokeOIDCGrantAccessTokens :execrows
UPDATE oidc_access_tokens SET revoked_at = now() WHERE grant_id = @grant_id AND revoked_at IS NULL;

-- name: RevokePersonClientOIDCGrants :execrows
-- Revokes every refresh token of a person for a client (RP-initiated logout).
UPDATE oidc_refresh_tokens SET revoked_at = now()
WHERE person_id = @person_id AND client_id = @client_id AND revoked_at IS NULL;

-- name: DeleteExpiredOIDCAccessTokens :execrows
DELETE FROM oidc_access_tokens WHERE expires_at < now() - interval '1 day';

-- name: DeleteExpiredOIDCRefreshTokens :execrows
DELETE FROM oidc_refresh_tokens WHERE expires_at < now() - interval '1 day';

-- name: CreateSigningKey :one
INSERT INTO signing_keys (id, algorithm, public_key, private_key, created_at)
VALUES (@id, @algorithm, @public_key, @private_key, @created_at)
RETURNING *;

-- name: GetActiveSigningKey :one
SELECT * FROM signing_keys WHERE retired_at IS NULL;

-- name: ListPublishedSigningKeys :many
-- The active key and retired keys still within their grace period, newest
-- first.
SELECT * FROM signing_keys
WHERE retired_at IS NULL OR expires_at > @now::timestamptz
ORDER BY created_at DESC, id DESC;

-- name: RetireSigningKey :one
UPDATE signing_keys SET retired_at = @retired_at::timestamptz, expires_at = @expires_at::timestamptz
WHERE id = @id AND retired_at IS NULL
RETURNING *;

-- name: DeleteExpiredSigningKeys :execrows
DELETE FROM signing_keys WHERE expires_at <= @now::timestamptz;
