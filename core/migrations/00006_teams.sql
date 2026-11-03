-- Teams and team memberships (#23; docs/architecture.md "Core domain
-- model"). Teams structure the directory and, later, the org chart, and are
-- the unit Orglane maps to access in services (ADR 0002: teams → provider
-- teams and groups).
--
-- Rules:
--   * Slugs are unique across all teams, archived ones included. Renaming
--     a team frees its previous slug for reuse.
--   * Teams are archived, never deleted: an archived team keeps its row,
--     its memberships and its audit history, and is read-only.
--   * parent_id prepares a team hierarchy; package identity rejects cycles.
--   * A membership has a role in the team: member or lead. A team may have
--     any number of leads, including none (co-leadership is allowed).
--
-- Rows are changed only through package identity, which writes an audit
-- entry in the same transaction.

-- +goose Up
CREATE TABLE teams (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    -- Lower case letters, digits and inner hyphens, 1-63 characters.
    slug        text NOT NULL CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    name        text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
    description text CHECK (description IS NULL OR (btrim(description) <> '' AND length(description) <= 1000)),
    parent_id   uuid REFERENCES teams (id) ON DELETE RESTRICT CHECK (parent_id <> id),
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX teams_slug_key ON teams (slug);
CREATE INDEX teams_parent_id_idx ON teams (parent_id);

CREATE TABLE team_memberships (
    team_id    uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    person_id  uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    role       text NOT NULL DEFAULT 'member' CHECK (role IN ('member', 'lead')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, person_id)
);

CREATE INDEX team_memberships_person_id_idx ON team_memberships (person_id);

-- +goose Down
DROP TABLE team_memberships;
DROP TABLE teams;
