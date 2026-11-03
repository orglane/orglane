// Package server runs the core in server mode: database, migrations and
// built-in roles, job queue and the HTTP API, with graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/orglane/orglane/core/internal/api"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/jobs"
)

// Run starts the server and blocks until ctx is canceled (SIGINT/SIGTERM in
// the binary), then shuts down gracefully within cfg.Server.ShutdownTimeout.
func Run(ctx context.Context, cfg *config.Config, version string, logger *slog.Logger) (err error) {
	logger.InfoContext(ctx, "starting orglane server", "version", version, "environment", cfg.Environment,
		"workspace_host", cfg.Domain.WorkspaceHost, "base_domain", cfg.Domain.Base)

	pool, err := database.Connect(ctx, cfg.Database, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	migrator, err := database.NewMigrator(pool, logger)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, migrator.Close()) }()
	if err := migrator.Up(ctx); err != nil {
		return err
	}
	if err := identity.NewService(pool).SeedBuiltinRoles(ctx); err != nil {
		return err
	}

	jobClient, err := jobs.NewClient(pool, cfg.Jobs, logger)
	if err != nil {
		return err
	}
	// The job queue outlives ctx: it is stopped explicitly during shutdown.
	if err := jobClient.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start job queue: %w", err)
	}

	handler, err := api.NewHandler(api.Options{
		Version:       version,
		BaseDomain:    cfg.Domain.Base,
		WorkspaceHost: cfg.Domain.WorkspaceHost,
		ReadinessChecks: []api.ReadinessCheck{
			{Name: "database", Check: pool.Ping},
			{Name: "migrations", Check: migrator.Check},
		},
		Logger: logger,
	})
	if err != nil {
		return errors.Join(err, jobClient.Stop(context.WithoutCancel(ctx)))
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Server.Listen)
	if err != nil {
		return errors.Join(fmt.Errorf("listen on %s: %w", cfg.Server.Listen, err), jobClient.Stop(context.WithoutCancel(ctx)))
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.InfoContext(ctx, "server listening", "addr", ln.Addr().String())

	select {
	case <-ctx.Done():
		logger.Info("shutting down", "timeout", cfg.Server.ShutdownTimeout)
	case err = <-serveErr:
		err = fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.ShutdownTimeout)
	defer cancel()
	return errors.Join(err, shutdown(shutdownCtx, srv, jobClient, logger))
}

func shutdown(ctx context.Context, srv *http.Server, jobClient *jobs.Client, logger *slog.Logger) error {
	var errs []error
	if err := srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("stop http server: %w", err))
	}
	if err := jobClient.Stop(ctx); err != nil {
		logger.Warn("job queue did not stop in time, canceling running jobs", "error", err)
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := jobClient.StopAndCancel(cancelCtx); err != nil {
			errs = append(errs, fmt.Errorf("stop job queue: %w", err))
		}
	}
	if len(errs) == 0 {
		logger.Info("shutdown complete")
	}
	return errors.Join(errs...)
}
