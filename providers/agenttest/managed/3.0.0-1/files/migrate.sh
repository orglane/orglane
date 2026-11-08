#!/bin/sh
# Upgrade migration: records the package version in the instance database
# and, with the "fault" setting "migrate", fails after its change, so a
# rollback has to undo a half-applied migration.
set -eu
psql "$PGURL" -X -q -v ON_ERROR_STOP=1 -v version="$PACKAGE_VERSION" <<'SQL'
CREATE TABLE IF NOT EXISTS agenttest_migrations (
    version text PRIMARY KEY,
    at      timestamptz NOT NULL DEFAULT now()
);
INSERT INTO agenttest_migrations (version) VALUES (:'version') ON CONFLICT DO NOTHING;
SQL
if [ "$FAULT" = migrate ]; then
    echo "agenttest: migration fault" >&2
    exit 1
fi
