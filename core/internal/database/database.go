// Package database connects to PostgreSQL and applies schema migrations.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/migrations"
)

// Connect opens a connection pool and waits up to cfg.ConnectTimeout for the
// database to answer, so the server can start alongside its database.
func Connect(ctx context.Context, cfg config.Database, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx redacts the password in parse errors.
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = 0 // no idle connections kept open without load (ADR 0009)
	useUTC(poolCfg.ConnConfig)
	poolCfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		scanTimestamptzAsUTC(conn)
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	for attempt := 1; ; attempt++ {
		err = pool.Ping(ctx)
		if err == nil {
			return pool, nil
		}
		if attempt == 1 {
			logger.Info("waiting for the database", "host", poolCfg.ConnConfig.Host, "error", err)
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("database not reachable at %s within %s: %w", poolCfg.ConnConfig.Host, cfg.ConnectTimeout, err)
		case <-time.After(time.Second):
		}
	}
}

// useUTC makes the session time zone UTC, so SQL that formats or truncates
// timestamps (to_char, date_trunc, ::date) is independent of the server's
// configuration.
func useUTC(cfg *pgx.ConnConfig) {
	cfg.RuntimeParams["timezone"] = "UTC"
}

// scanTimestamptzAsUTC makes timestamptz values scan into time.Time in UTC
// instead of the process's local time zone.
func scanTimestamptzAsUTC(conn *pgx.Conn) {
	conn.TypeMap().RegisterType(&pgtype.Type{
		Name:  "timestamptz",
		OID:   pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	})
}

// Migrator applies the core migrations (goose, embedded SQL in
// core/migrations) and the job queue schema (River's own migrations).
type Migrator struct {
	pool   *pgxpool.Pool
	db     *sql.DB
	goose  *goose.Provider
	river  *rivermigrate.Migrator[pgx.Tx]
	logger *slog.Logger
}

// NewMigrator prepares migrations on pool. Close releases its resources but
// not the pool.
func NewMigrator(pool *pgxpool.Pool, logger *slog.Logger) (*Migrator, error) {
	db := stdlib.OpenDBFromPool(pool)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithDisableGlobalRegistry(true),
		goose.WithSlog(logger),
	)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("load migrations: %w", err), db.Close())
	}
	river, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: logger})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("load job queue migrations: %w", err), db.Close())
	}
	return &Migrator{pool: pool, db: db, goose: provider, river: river, logger: logger}, nil
}

// migrationLockID is the Postgres advisory lock serializing migration runs,
// so instances starting at the same time migrate once. River's migrations
// are not safe to run concurrently on their own.
const migrationLockID int64 = 0x6f72676c616e65 // "orglane"

// Up applies all pending migrations.
//
// The advisory lock is held on a dedicated connection outside the pool:
// goose and River draw their connections from the pool, so holding the lock
// on a pooled connection would deadlock a pool of size one.
func (m *Migrator) Up(ctx context.Context) (err error) {
	conn, err := pgx.ConnectConfig(ctx, m.pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("open migration lock connection: %w", err)
	}
	// Closing the session also releases the lock if the unlock below fails.
	defer func() { err = errors.Join(err, conn.Close(context.WithoutCancel(ctx))) }()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		if _, unlockErr := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("release migration lock: %w", unlockErr))
		}
	}()

	results, err := m.goose.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply core migrations: %w", err)
	}
	for _, r := range results {
		m.logger.InfoContext(ctx, "applied migration", "source", r.Source.Path, "duration", r.Duration)
	}
	// River logs each migration it applies.
	if _, err := m.river.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("apply job queue migrations: %w", err)
	}
	return nil
}

// Check returns an error unless every migration has been applied.
func (m *Migrator) Check(ctx context.Context) error {
	pending, err := m.goose.HasPending(ctx)
	if err != nil {
		return fmt.Errorf("check core migrations: %w", err)
	}
	if pending {
		return errors.New("core migrations are pending")
	}
	res, err := m.river.Validate(ctx, nil)
	if err != nil {
		return fmt.Errorf("check job queue migrations: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("job queue migrations are pending: %s", strings.Join(res.Messages, "; "))
	}
	return nil
}

// Close releases the migrator. The pool stays open.
func (m *Migrator) Close() error {
	return m.db.Close()
}
