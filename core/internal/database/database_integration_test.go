//go:build integration

package database_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/tests/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var logger = slog.New(slog.NewTextHandler(io.Discard, nil))

func connect(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := database.Connect(context.Background(), config.Database{URL: url, MaxConns: 5, ConnectTimeout: 10 * time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newMigrator(t *testing.T, pool *pgxpool.Pool) *database.Migrator {
	t.Helper()
	m, err := database.NewMigrator(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestMigrations(t *testing.T) {
	ctx := context.Background()
	pool := connect(t, pgtest.NewDatabase(t))
	m := newMigrator(t, pool)

	if err := m.Check(ctx); err == nil {
		t.Fatal("Check() on an empty database succeeded, want pending migrations")
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	if err := m.Check(ctx); err != nil {
		t.Fatalf("Check() after Up() error = %v", err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second Up() error = %v", err)
	}

	for _, table := range append([]string{"goose_db_version", "river_job", "river_migration"}, coreTables...) {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
			t.Errorf("table %s exists = %v, %v", table, exists, err)
		}
	}

	if _, err := pool.Exec(ctx, `INSERT INTO instance_settings (key, value) VALUES ('example', '{"a": 1}')`); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO instance_settings (key, value) VALUES ('', '1')`); err == nil {
		t.Fatal("empty key accepted, want check violation")
	}
}

// TestMigrationsWithSingleConnectionPool is a regression test: the
// migration lock must not take the only pooled connection away from goose and
// River. The context bounds the test so a deadlock fails instead of hanging.
func TestMigrationsWithSingleConnectionPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := database.Connect(ctx, config.Database{URL: pgtest.NewDatabase(t), MaxConns: 1, ConnectTimeout: 10 * time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	m := newMigrator(t, pool)
	for i := range 2 { // fresh database, then already migrated
		if err := m.Up(ctx); err != nil {
			t.Fatalf("Up() run %d with max_conns=1: %v", i+1, err)
		}
	}
	if err := m.Check(ctx); err != nil {
		t.Fatalf("Check() with max_conns=1: %v", err)
	}
}

func TestConcurrentMigrations(t *testing.T) {
	url := pgtest.NewDatabase(t)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		m := newMigrator(t, connect(t, url))
		wg.Go(func() { errs[i] = m.Up(context.Background()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("migrator %d: %v", i, err)
		}
	}
	if err := newMigrator(t, connect(t, url)).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConnectTimeout(t *testing.T) {
	start := time.Now()
	_, err := database.Connect(context.Background(), config.Database{
		URL: "postgres://orglane@127.0.0.1:1/orglane?connect_timeout=1", MaxConns: 1, ConnectTimeout: 2 * time.Second,
	}, logger)
	if err == nil {
		t.Fatal("Connect() to a closed port succeeded")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("Connect() took %s, want about the connect timeout", time.Since(start))
	}
}
