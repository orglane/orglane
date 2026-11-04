-- +goose Up
-- Source evidence only. No authentication or ownership proof is established.
CREATE TABLE pending_account_links (
    provider text NOT NULL CHECK (provider = 'github'),
    organization_id bigint NOT NULL CHECK (organization_id > 0),
    external_id bigint NOT NULL CHECK (external_id > 0),
    person_id uuid NOT NULL REFERENCES people(id),
    login text NOT NULL CHECK (length(login) BETWEEN 1 AND 39),
    avatar_url text NOT NULL CHECK (length(avatar_url) <= 4096),
    state text NOT NULL DEFAULT 'pending' CHECK (state = 'pending'),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, organization_id, external_id)
);
-- +goose Down
DROP TABLE pending_account_links;
