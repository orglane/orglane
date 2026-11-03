-- Sign-in and the embedded OIDC provider (ADR 0003, #6): password and
-- passkey credentials, sign-in throttling, OIDC clients, authorization
-- requests, tokens and signing keys.
--
-- Secrets are never stored in the clear: passwords as argon2id hashes,
-- random tokens (codes, refresh tokens, client secrets) as SHA-256 hashes,
-- signing keys encrypted with a key derived from the instance master secret.
-- Rows are changed only through package identity (and its oidcop
-- subpackage), which writes audit entries in the same transaction.

-- +goose Up

-- How a session was authenticated: OIDC reports it as amr and checks
-- max_age against created_at (the authentication time).
ALTER TABLE sessions
    ADD COLUMN auth_methods text[] NOT NULL DEFAULT '{}'
        CHECK (auth_methods <@ ARRAY['password', 'passkey']::text[]);

-- Sign-in credentials and preferences of a person. A person without a row
-- has no password and has never dismissed the passkey offer.
CREATE TABLE person_credentials (
    person_id                  uuid PRIMARY KEY REFERENCES people (id) ON DELETE CASCADE,
    -- argon2id PHC string, parameters included, so they can be raised later.
    password_hash              text CHECK (password_hash LIKE '$argon2id$%' AND length(password_hash) <= 512),
    password_updated_at        timestamptz,
    passkey_offer_dismissed_at timestamptz,
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    CHECK ((password_hash IS NULL) = (password_updated_at IS NULL))
);

-- WebAuthn credentials. The WebAuthn user handle is the person ID.
CREATE TABLE passkeys (
    id            uuid PRIMARY KEY DEFAULT uuidv7(),
    person_id     uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    credential_id bytea NOT NULL CHECK (octet_length(credential_id) BETWEEN 1 AND 1023),
    name          text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
    -- The verified credential record (public key, sign counter, flags,
    -- transports, AAGUID) as serialized by go-webauthn.
    credential    jsonb NOT NULL CHECK (jsonb_typeof(credential) = 'object'),
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz
);

CREATE UNIQUE INDEX passkeys_credential_id_key ON passkeys (credential_id);
CREATE INDEX passkeys_person_id_idx ON passkeys (person_id);

-- WebAuthn ceremonies in flight: the challenge state between "options" and
-- "finish". Single use and short-lived; the ID is random (v4), not
-- time-ordered.
CREATE TABLE webauthn_ceremonies (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL CHECK (kind IN ('registration', 'login')),
    -- The person registering a passkey; NULL for a sign-in.
    person_id    uuid REFERENCES people (id) ON DELETE CASCADE,
    session_data jsonb NOT NULL CHECK (jsonb_typeof(session_data) = 'object'),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    CHECK ((kind = 'registration') = (person_id IS NOT NULL))
);

CREATE INDEX webauthn_ceremonies_expires_at_idx ON webauthn_ceremonies (expires_at);

-- Failed password sign-ins per account (the lower-cased work email as typed,
-- whether or not such a person exists, so lockout does not reveal which
-- accounts exist).
CREATE TABLE sign_in_throttles (
    account         text PRIMARY KEY CHECK (account <> '' AND length(account) <= 254),
    failures        integer NOT NULL CHECK (failures >= 0),
    last_failure_at timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz
);

-- OIDC clients (relying parties). Registered by operators; there is no
-- dynamic client registration endpoint.
CREATE TABLE oidc_clients (
    id                        text PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9._~-]{1,100}$'),
    name                      text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
    -- SHA-256 of the random client secret; NULL for public clients.
    secret_hash               bytea CHECK (octet_length(secret_hash) = 32),
    application_type          text NOT NULL CHECK (application_type IN ('web', 'native', 'user_agent')),
    auth_method               text NOT NULL CHECK (auth_method IN ('client_secret_basic', 'client_secret_post', 'none')),
    redirect_uris             text[] NOT NULL CHECK (cardinality(redirect_uris) > 0),
    post_logout_redirect_uris text[] NOT NULL DEFAULT '{}',
    grant_types               text[] NOT NULL CHECK (grant_types <@ ARRAY['authorization_code', 'refresh_token']::text[] AND cardinality(grant_types) > 0),
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CHECK ((auth_method = 'none') = (secret_hash IS NULL))
);

-- Authorization requests between /authorize and the code exchange. A request
-- is done once a person is set. The code is single use: only its hash is
-- stored, code_used_at marks the exchange, and the row is kept until it
-- expires so a second use of the code can be detected and the tokens issued
-- for it revoked (RFC 6749, section 4.1.2). The request ID is the grant ID of
-- every token issued from it.
CREATE TABLE oidc_auth_requests (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id             text NOT NULL REFERENCES oidc_clients (id) ON DELETE CASCADE,
    redirect_uri          text NOT NULL CHECK (length(redirect_uri) <= 2048),
    scopes                text[] NOT NULL,
    state                 text NOT NULL DEFAULT '' CHECK (length(state) <= 2048),
    nonce                 text NOT NULL DEFAULT '' CHECK (length(nonce) <= 2048),
    response_type         text NOT NULL CHECK (length(response_type) <= 100),
    response_mode         text NOT NULL DEFAULT '' CHECK (length(response_mode) <= 100),
    code_challenge        text NOT NULL DEFAULT '' CHECK (length(code_challenge) <= 256),
    code_challenge_method text NOT NULL DEFAULT '' CHECK (code_challenge_method IN ('', 'S256', 'plain')),
    prompt                text[] NOT NULL DEFAULT '{}',
    max_age_seconds       bigint CHECK (max_age_seconds >= 0),
    login_hint            text NOT NULL DEFAULT '' CHECK (length(login_hint) <= 254),
    person_id             uuid REFERENCES people (id) ON DELETE CASCADE,
    session_id            uuid REFERENCES sessions (id) ON DELETE SET NULL,
    auth_time             timestamptz,
    auth_methods          text[] NOT NULL DEFAULT '{}',
    code_hash             bytea CHECK (octet_length(code_hash) = 32),
    code_used_at          timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    expires_at            timestamptz NOT NULL,
    CHECK ((person_id IS NULL) = (auth_time IS NULL))
);

CREATE UNIQUE INDEX oidc_auth_requests_code_hash_key ON oidc_auth_requests (code_hash);
CREATE INDEX oidc_auth_requests_expires_at_idx ON oidc_auth_requests (expires_at);

-- A grant is everything issued from one authorization (one code); its ID is
-- the authorization request ID. Issuing tokens and revoking the grant both
-- lock the grant row, so a revoked grant can never gain tokens, and tokens of
-- a revoked grant are invalid even if a row was missed.
CREATE TABLE oidc_grants (
    id         uuid PRIMARY KEY,
    client_id  text NOT NULL REFERENCES oidc_clients (id) ON DELETE CASCADE,
    person_id  uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

CREATE INDEX oidc_grants_person_id_idx ON oidc_grants (person_id);

-- Refresh tokens rotate on every use. All tokens issued from one
-- authorization share its grant ID; presenting a refresh token that was
-- already used revokes the whole grant (reuse detection, RFC 9700).
CREATE TABLE oidc_refresh_tokens (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    token_hash   bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    grant_id     uuid NOT NULL REFERENCES oidc_grants (id) ON DELETE CASCADE,
    client_id    text NOT NULL REFERENCES oidc_clients (id) ON DELETE CASCADE,
    person_id    uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    scopes       text[] NOT NULL,
    audience     text[] NOT NULL,
    auth_time    timestamptz NOT NULL,
    auth_methods text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    used_at      timestamptz,
    revoked_at   timestamptz
);

CREATE UNIQUE INDEX oidc_refresh_tokens_token_hash_key ON oidc_refresh_tokens (token_hash);
CREATE INDEX oidc_refresh_tokens_grant_id_idx ON oidc_refresh_tokens (grant_id);
CREATE INDEX oidc_refresh_tokens_person_id_idx ON oidc_refresh_tokens (person_id);
CREATE INDEX oidc_refresh_tokens_expires_at_idx ON oidc_refresh_tokens (expires_at);

-- Access tokens are opaque to clients (encrypted references to this row),
-- so revoking one here takes effect on the next request.
CREATE TABLE oidc_access_tokens (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    client_id  text NOT NULL REFERENCES oidc_clients (id) ON DELETE CASCADE,
    person_id  uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    grant_id   uuid NOT NULL REFERENCES oidc_grants (id) ON DELETE CASCADE,
    scopes     text[] NOT NULL,
    audience   text[] NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX oidc_access_tokens_person_id_idx ON oidc_access_tokens (person_id);
CREATE INDEX oidc_access_tokens_grant_id_idx ON oidc_access_tokens (grant_id);
CREATE INDEX oidc_access_tokens_expires_at_idx ON oidc_access_tokens (expires_at);

-- Token signing keys. The active key (retired_at IS NULL) signs; retired
-- keys stay in the JWKS until expires_at so tokens they signed still verify.
-- The private key is PKCS#8, sealed with AES-256-GCM under a key derived
-- from the instance master secret, with the key ID as associated data.
CREATE TABLE signing_keys (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    algorithm   text NOT NULL CHECK (algorithm IN ('RS256')),
    public_key  bytea NOT NULL,
    private_key bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    retired_at  timestamptz,
    expires_at  timestamptz,
    CHECK ((retired_at IS NULL) = (expires_at IS NULL))
);

CREATE UNIQUE INDEX signing_keys_one_active_key ON signing_keys ((true)) WHERE retired_at IS NULL;

-- +goose Down
DROP TABLE signing_keys;
DROP TABLE oidc_access_tokens;
DROP TABLE oidc_refresh_tokens;
DROP TABLE oidc_grants;
DROP TABLE oidc_auth_requests;
DROP TABLE oidc_clients;
DROP TABLE sign_in_throttles;
DROP TABLE webauthn_ceremonies;
DROP TABLE passkeys;
DROP TABLE person_credentials;
ALTER TABLE sessions DROP COLUMN auth_methods;
