-- Instance-wide settings as key/value pairs. Values are JSON so a setting can
-- change shape without a schema migration. The core data model arrives in
-- later migrations (#5).

-- +goose Up
CREATE TABLE instance_settings (
    key        text PRIMARY KEY CHECK (key <> ''),
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE instance_settings;
