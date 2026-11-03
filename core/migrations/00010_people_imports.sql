-- +goose Up
-- Staging is encrypted in its entirety: mapping may classify any column as sensitive.
CREATE TABLE people_import_runs (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    created_by uuid NOT NULL REFERENCES people(id),
    source text NOT NULL CHECK (source <> '' AND length(source) <= 50),
    encoding text NOT NULL,
    delimiter text NOT NULL,
    headers_sealed bytea NOT NULL,
    mapping jsonb NOT NULL DEFAULT '{"columns":[],"createTeams":false,"sendInvitations":false,"skipRows":[]}',
    state text NOT NULL DEFAULT 'staged' CHECK (state IN ('staged','previewed','applied')),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX people_import_runs_creator ON people_import_runs(created_by, created_at);
CREATE TABLE people_import_rows (
    run_id uuid NOT NULL REFERENCES people_import_runs(id) ON DELETE CASCADE,
    row_number integer NOT NULL CHECK (row_number BETWEEN 2 AND 1048576),
    payload_sealed bytea NOT NULL,
    decision text NOT NULL DEFAULT 'skip' CHECK (decision IN ('create','update','skip','conflict')),
    error_code text NOT NULL DEFAULT '',
    preview_sealed bytea,
    result text NOT NULL DEFAULT 'pending' CHECK (result IN ('pending','created','updated','skipped','failed')),
    PRIMARY KEY (run_id, row_number)
);
-- External identifiers are source-scoped hashes; imports never store their plaintext.
CREATE TABLE people_import_identities (
    source text NOT NULL,
    external_hash bytea NOT NULL CHECK (octet_length(external_hash) = 32),
    person_id uuid NOT NULL REFERENCES people(id),
    PRIMARY KEY (source, external_hash)
);

-- +goose Down
DROP TABLE people_import_identities;
DROP TABLE people_import_rows;
DROP TABLE people_import_runs;
