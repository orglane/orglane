// Package migrations embeds the core schema migrations.
//
// Migrations are goose SQL files named NNNNN_description.sql and are applied
// in order on server start and by `orglane cli migrate`. Applied migrations
// are never edited; add a new file instead. Changes here are high risk
// (ADR 0017) and need a maintainer review.
package migrations

import "embed"

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS
