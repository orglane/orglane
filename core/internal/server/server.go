// Package server runs the core in server mode: database, migrations and
// built-in roles, job queue, the public HTTP listener (API) and, when
// configured, the internal listener for Traefik (ADR 0008), with graceful
// shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/api"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/demomode"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/core/internal/ingress"
	"github.com/orglane/orglane/core/internal/jobs"
	"github.com/orglane/orglane/core/internal/webui"
	"github.com/orglane/orglane/demo"
	webapp "github.com/orglane/orglane/web"
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
	stack, err := web.NewStack(pool, cfg, logger, web.StackOptions{})
	if err != nil {
		return err
	}
	dataset, err := prepareIdentity(ctx, cfg, pool, stack, logger)
	if err != nil {
		return err
	}

	jobClient, err := jobs.NewClient(pool, cfg.Jobs, logger, jobs.Options{
		IdentityMaintenance: func(ctx context.Context) error { return stack.Maintain(ctx, pool) },
		Mail:                stack.Mail,
	})
	if err != nil {
		return err
	}
	// The job queue outlives ctx: it is stopped explicitly during shutdown.
	if err := jobClient.Start(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("start job queue: %w", err)
	}

	ui, err := webui.New(webapp.Dist())
	if err != nil {
		return errors.Join(err, jobClient.Stop(context.WithoutCancel(ctx)))
	}
	if !ui.Built() {
		logger.WarnContext(ctx, "this binary has no web UI; build it with `make build`")
	}

	handler, err := api.NewHandler(api.Options{
		Version:       version,
		BaseDomain:    cfg.Domain.Base,
		WorkspaceHost: cfg.Domain.WorkspaceHost,
		WorkspaceURL:  cfg.WorkspaceURL(),
		Auth:          stack.Auth,
		Profile:       stack.Profile,
		Imports:       stack.Imports,
		Mail:          stack.Mail,
		WorkEmail:     cfg.WorkEmail,
		Demo:          dataset,
		ReadinessChecks: []api.ReadinessCheck{
			{Name: "database", Check: pool.Ping},
			{Name: "migrations", Check: migrator.Check},
		},
		WebUI:  ui,
		Logger: logger,
	})
	if err != nil {
		return errors.Join(err, jobClient.Stop(context.WithoutCancel(ctx)))
	}

	listeners := []listener{{name: "public", addr: cfg.Server.Listen, handler: handler}}
	if cfg.Server.InternalListen != "" {
		internal, err := ingress.NewHandler(cfg, logger)
		if err != nil {
			return errors.Join(err, jobClient.Stop(context.WithoutCancel(ctx)))
		}
		listeners = append(listeners, listener{name: "internal", addr: cfg.Server.InternalListen, handler: internal})
	}

	servers := make([]*http.Server, 0, len(listeners))
	serveErr := make(chan error, len(listeners))
	for _, l := range listeners {
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", l.addr)
		if err != nil {
			err = fmt.Errorf("listen on %s (%s listener): %w", l.addr, l.name, err)
			return errors.Join(err, shutdownWithin(ctx, cfg.Server.ShutdownTimeout, servers, jobClient, logger))
		}
		srv := newHTTPServer(l.handler, logger)
		servers = append(servers, srv)
		go func() {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("%s http server: %w", l.name, err)
			}
		}()
		logger.InfoContext(ctx, "server listening", "listener", l.name, "addr", ln.Addr().String())
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down", "timeout", cfg.Server.ShutdownTimeout)
	case err = <-serveErr:
	}

	return errors.Join(err, shutdownWithin(ctx, cfg.Server.ShutdownTimeout, servers, jobClient, logger))
}

// shutdownWithin runs shutdown with a fresh timeout, even if ctx is done.
func shutdownWithin(ctx context.Context, timeout time.Duration, servers []*http.Server, jobClient *jobs.Client, logger *slog.Logger) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return shutdown(shutdownCtx, servers, jobClient, logger)
}

// listener is one HTTP listener of the server.
type listener struct {
	name    string
	addr    string
	handler http.Handler
}

func newHTTPServer(handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// prepareIdentity seeds the built-in roles and makes sure a signing key
// exists. On a fresh instance it creates the first-run setup token and logs
// it once; in demo mode it loads the demo dataset instead and returns it.
func prepareIdentity(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, stack *web.Stack, logger *slog.Logger) (*demo.Dataset, error) {
	if cfg.Identity.MasterSecret == config.DevMasterSecret {
		logger.WarnContext(ctx, "using the public development master secret; set identity.master_secret (ORGLANE_IDENTITY_MASTER_SECRET) outside local development")
	}
	if cfg.Domain.Scheme == "http" {
		logger.WarnContext(ctx, "serving the workspace over plain HTTP (domain.scheme: http): local demo mode only")
	}
	if err := stack.Identity.SeedBuiltinRoles(ctx); err != nil {
		return nil, err
	}
	if _, err := stack.Keys.Ensure(ctx); err != nil {
		return nil, fmt.Errorf("signing keys: %w", err)
	}
	if cfg.Demo.Enabled {
		return prepareDemo(ctx, cfg, pool, stack, logger)
	}
	if marker, ok, err := demomode.IsDemo(ctx, pool); err != nil {
		return nil, err
	} else if ok {
		logger.WarnContext(ctx, "this database holds the demo dataset, but demo mode is off: one-click sign-in is disabled. Start with --demo to enable it", "dataset", marker.Dataset)
	}
	token, err := stack.Identity.EnsureSetupToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("setup token: %w", err)
	}
	if token != "" {
		// Printed once: only its hash is stored. `orglane cli setup-token`
		// creates a new one.
		logger.WarnContext(ctx, "first-run setup: no owner exists yet. Open the workspace and create the owner account with this one-time setup token",
			"setup_token", token, "workspace_url", cfg.WorkspaceURL())
	}
	return nil, nil
}

// prepareDemo loads the demo dataset into an empty database (or checks it
// was loaded on an earlier start) and logs the demo banner. First-run setup
// is skipped: the dataset brings its owner.
func prepareDemo(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, stack *web.Stack, logger *slog.Logger) (*demo.Dataset, error) {
	dataset, err := demo.Load(demo.Acme)
	if err != nil {
		return nil, err
	}
	res, err := demomode.Prepare(ctx, pool, stack.Identity, dataset)
	if err != nil {
		return nil, fmt.Errorf("demo mode: %w", err)
	}
	if res.Loaded {
		logger.InfoContext(ctx, "loaded the demo dataset", "dataset", dataset.Name, "version", dataset.Version, "people", res.People, "teams", res.Teams)
	}
	logger.WarnContext(ctx, "DEMO MODE: this instance runs the "+dataset.Company.Name+" demo dataset. Anyone who can reach it can sign in as any demo person, including the owner, with one click. Do not store real data here",
		"dataset", dataset.Name, "workspace_url", cfg.WorkspaceURL(), "sign_in", "POST /api/v1/demo/sign-in")
	return dataset, nil
}

func shutdown(ctx context.Context, servers []*http.Server, jobClient *jobs.Client, logger *slog.Logger) error {
	var errs []error
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop http server: %w", err))
		}
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
