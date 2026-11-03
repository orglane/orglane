-- System email and invitations (ADR 0014, #26).
--
-- A person's contact email is where invitations (and, later, recovery
-- messages) go; it is separate from the work email, which is their identity
-- in the workspace. System email is sent through an outbox worked by the job
-- queue: one row per message, with its delivery state. Message content holds
-- invitation links, so it is stored encrypted with a key derived from the
-- instance master secret, and cleared once the message is delivered, given
-- up or canceled. Invitation tokens are stored as SHA-256 hashes only.
--
-- People and invitations are changed only through package identity, email
-- settings and the outbox through package mail; both write audit entries in
-- the same transaction.

-- +goose Up
ALTER TABLE people
    ADD COLUMN contact_email text
        CHECK (contact_email IS NULL OR (length(contact_email) <= 254 AND contact_email ~ '^[^@[:space:]]+@[^@[:space:]]+$'));

CREATE TABLE email_outbox (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    -- What the message is, e.g. "invitation"; for display and metrics.
    kind           text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]{0,62}$'),
    recipient      text NOT NULL CHECK (length(recipient) <= 254 AND recipient ~ '^[^@[:space:]]+@[^@[:space:]]+$'),
    -- queued: waiting for (another) delivery attempt; sent: accepted by the
    -- SMTP server; failed: every attempt failed; canceled: superseded (an
    -- invitation was resent or revoked) before it was sent.
    status         text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sent', 'failed', 'canceled')),
    -- The encrypted message (subject, text and HTML bodies). NULL once the
    -- message left the queue.
    sealed_message bytea,
    attempts       integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts   integer NOT NULL CHECK (max_attempts >= 1),
    -- The error of the last failed attempt, for operators and the UI.
    last_error     text CHECK (length(last_error) <= 1000),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    sent_at        timestamptz,
    CHECK ((status = 'queued') = (sealed_message IS NOT NULL)),
    CHECK ((status = 'sent') = (sent_at IS NOT NULL))
);

CREATE INDEX email_outbox_created_at_idx ON email_outbox (created_at);

-- The invitation of an invited person: one per person, reissued (new token,
-- new expiry) when it is resent. Accepting it activates the person.
CREATE TABLE invitations (
    id                 uuid PRIMARY KEY DEFAULT uuidv7(),
    person_id          uuid NOT NULL UNIQUE REFERENCES people (id) ON DELETE CASCADE,
    -- SHA-256 of the current token; the token itself is only in the link.
    token_hash         bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    -- The language of the invitation email (a BCP 47 tag).
    locale             text NOT NULL CHECK (locale ~ '^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$'),
    invited_by         uuid REFERENCES people (id) ON DELETE SET NULL,
    -- The email carrying the current token; NULL when it was issued as a
    -- link to share (no email configured).
    email_id           uuid REFERENCES email_outbox (id) ON DELETE SET NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    -- When the current token was issued.
    issued_at          timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    accepted_at        timestamptz,
    revoked_at         timestamptz,
    -- Set once the expiry has been audited (by identity maintenance).
    expiry_recorded_at timestamptz,
    CHECK (expires_at > issued_at),
    CHECK (accepted_at IS NULL OR revoked_at IS NULL)
);

CREATE UNIQUE INDEX invitations_token_hash_key ON invitations (token_hash);
CREATE INDEX invitations_email_id_idx ON invitations (email_id);
CREATE INDEX invitations_invited_by_idx ON invitations (invited_by);

-- +goose Down
DROP TABLE invitations;
DROP TABLE email_outbox;
ALTER TABLE people DROP COLUMN contact_email;
