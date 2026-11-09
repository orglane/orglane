//go:build integration

// Package dbtest is the database harness of the core integration tests: one
// PostgreSQL container per test binary, with the migrations applied once to a
// template database (see tests/pgtest). Every test still creates its own
// fresh, fully migrated database.
//
// Packages that test the migrations themselves use tests/pgtest.Main instead:
// they need an empty database.
package dbtest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/tests/pgtest"
)

// Main runs the tests with a template database prepared from the migrations.
func Main(m *testing.M) {
	os.Exit(pgtest.RunWithTemplate(m, Migrate))
}

// Migrate applies every migration to databaseURL. It is a pgtest prepare
// function and closes every connection it opens.
func Migrate(ctx context.Context, databaseURL string) error {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := database.Connect(ctx, config.Database{URL: databaseURL, MaxConns: 4, ConnectTimeout: 10 * time.Second}, logger)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()
	m, err := database.NewMigrator(pool, logger)
	if err != nil {
		return fmt.Errorf("migrator: %w", err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
