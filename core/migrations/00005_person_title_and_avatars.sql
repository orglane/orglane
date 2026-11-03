-- Person title and avatar, and the "demo" authentication method (#12).
--
-- The title is a base field of a person (docs/architecture.md "Core domain
-- model"). Avatars are small images stored in the database so a backup of
-- the core database is complete; the content type is an allow-list and SVG
-- content is validated by package identity before it is written (no
-- scripts, no external references). Rows are changed only through package
-- identity, which writes an audit entry in the same transaction.

-- +goose Up
ALTER TABLE people
    ADD COLUMN title text CHECK (title IS NULL OR (btrim(title) <> '' AND length(title) <= 200));

CREATE TABLE person_avatars (
    person_id    uuid PRIMARY KEY REFERENCES people (id) ON DELETE CASCADE,
    content_type text NOT NULL CHECK (content_type IN ('image/svg+xml', 'image/png', 'image/webp')),
    data         bytea NOT NULL CHECK (octet_length(data) BETWEEN 1 AND 262144),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- One-click sign-in in demo mode authenticates a session with "demo".
ALTER TABLE sessions DROP CONSTRAINT sessions_auth_methods_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_auth_methods_check
    CHECK (auth_methods <@ ARRAY['password', 'passkey', 'demo']::text[]);

-- +goose Down
-- Demo sessions cannot be represented without the "demo" method: they end.
UPDATE sessions
SET auth_methods = array_remove(auth_methods, 'demo'), revoked_at = coalesce(revoked_at, now())
WHERE 'demo' = ANY (auth_methods);
ALTER TABLE sessions DROP CONSTRAINT sessions_auth_methods_check;
ALTER TABLE sessions ADD CONSTRAINT sessions_auth_methods_check
    CHECK (auth_methods <@ ARRAY['password', 'passkey']::text[]);

DROP TABLE person_avatars;
ALTER TABLE people DROP COLUMN title;
