#!/bin/sh
# Configure hook: writes the "message" setting into the instance database.
set -eu
psql "$PGURL" -X -q -v ON_ERROR_STOP=1 -v msg="$MESSAGE" <<'SQL'
CREATE TABLE IF NOT EXISTS agenttest_config (
    id         int PRIMARY KEY,
    message    text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO agenttest_config (id, message) VALUES (1, :'msg')
ON CONFLICT (id) DO UPDATE SET message = EXCLUDED.message, updated_at = now();
SQL
