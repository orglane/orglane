// Package jobs runs background jobs on a Postgres-backed queue inside the
// core process (River, ADR 0009). No broker or extra service is involved.
//
// Background work must be justified: periodic jobs are off unless configured,
// and new jobs are picked up through LISTEN/NOTIFY with a slow fallback poll.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/mail"
)

// NoopArgs are the arguments of the example no-op job.
type NoopArgs struct{}

// Kind implements river.JobArgs.
func (NoopArgs) Kind() string { return "orglane.noop" }

// NoopWorker works NoopArgs jobs by doing nothing. It shows how jobs are
// registered and lets operators check the queue end to end
// (jobs.noop.enabled).
type NoopWorker struct {
	river.WorkerDefaults[NoopArgs]
	Logger *slog.Logger
}

// Work implements river.Worker.
func (w *NoopWorker) Work(ctx context.Context, job *river.Job[NoopArgs]) error {
	w.Logger.DebugContext(ctx, "noop job ran", "job_id", job.ID)
	return nil
}

// IdentityMaintenanceArgs are the arguments of the hourly identity
// maintenance job: signing key rotation and cleanup of expired sign-in and
// OIDC state.
type IdentityMaintenanceArgs struct{}

// Kind implements river.JobArgs.
func (IdentityMaintenanceArgs) Kind() string { return "orglane.identity_maintenance" }

// IdentityMaintenanceInterval is how often identity maintenance runs. Key
// rotation is also checked whenever a token is signed, so this only bounds
// how late a rotation can be when no tokens are issued.
const IdentityMaintenanceInterval = time.Hour

// IdentityMaintenanceWorker runs Maintain.
type IdentityMaintenanceWorker struct {
	river.WorkerDefaults[IdentityMaintenanceArgs]
	Maintain func(ctx context.Context) error
}

// Work implements river.Worker.
func (w *IdentityMaintenanceWorker) Work(ctx context.Context, _ *river.Job[IdentityMaintenanceArgs]) error {
	return w.Maintain(ctx)
}

// ImportCleanupArgs are the arguments of the hourly people import cleanup
// job: it deletes expired import runs (docs/specs/people-import.md
// "Lifecycle").
type ImportCleanupArgs struct{}

// Kind implements river.JobArgs.
func (ImportCleanupArgs) Kind() string { return "orglane.people_import_cleanup" }

// ImportCleanupInterval is how often the import cleanup runs. Expired runs
// are already unreadable before it deletes them, so this only bounds how
// long their rows are kept.
const ImportCleanupInterval = time.Hour

// ImportCleanupWorker runs Cleanup.
type ImportCleanupWorker struct {
	river.WorkerDefaults[ImportCleanupArgs]
	Cleanup func(ctx context.Context) error
}

// Work implements river.Worker.
func (w *ImportCleanupWorker) Work(ctx context.Context, _ *river.Job[ImportCleanupArgs]) error {
	return w.Cleanup(ctx)
}

// Client is the job queue client.
type Client = river.Client[pgx.Tx]

// Options add optional jobs to the client.
type Options struct {
	// IdentityMaintenance, when set, runs hourly and on start.
	IdentityMaintenance func(ctx context.Context) error
	// ImportCleanup, when set, runs hourly and on start.
	ImportCleanup func(ctx context.Context) error
	// Mail, when set, delivers queued system email (mail.SendArgs). The
	// client is attached to it as its queue.
	Mail *mail.Service
	// Apps, when set, runs the provider framework's jobs on their own
	// queue (apps.QueueName) with the daily cleanup and drift check and the
	// agent sync. The client is attached to it as its queue.
	Apps *apps.Service
}

// NewClient builds the job queue client. Start it with Start and stop it with
// Stop (or StopAndCancel when the shutdown deadline is near).
func NewClient(pool *pgxpool.Pool, cfg config.Jobs, logger *slog.Logger, opts Options) (*Client, error) {
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, &NoopWorker{Logger: logger}); err != nil {
		return nil, fmt.Errorf("register noop worker: %w", err)
	}

	if opts.Mail != nil {
		if err := river.AddWorkerSafely(workers, &mail.Worker{Service: opts.Mail}); err != nil {
			return nil, fmt.Errorf("register email worker: %w", err)
		}
	}

	var periodic []*river.PeriodicJob
	queues := map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.MaxWorkers}}
	if opts.Apps != nil {
		if err := apps.Register(workers, opts.Apps); err != nil {
			return nil, err
		}
		periodic = append(periodic, opts.Apps.PeriodicJobs()...)
		n := cfg.AppsMaxWorkers
		if n < 1 {
			n = apps.DefaultMaxWorkers
		}
		queues[apps.QueueName] = river.QueueConfig{MaxWorkers: n}
	}
	if opts.IdentityMaintenance != nil {
		if err := river.AddWorkerSafely(workers, &IdentityMaintenanceWorker{Maintain: opts.IdentityMaintenance}); err != nil {
			return nil, fmt.Errorf("register identity maintenance worker: %w", err)
		}
		periodic = append(periodic, river.NewPeriodicJob(
			river.PeriodicInterval(IdentityMaintenanceInterval),
			func() (river.JobArgs, *river.InsertOpts) { return IdentityMaintenanceArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}
	if opts.ImportCleanup != nil {
		if err := river.AddWorkerSafely(workers, &ImportCleanupWorker{Cleanup: opts.ImportCleanup}); err != nil {
			return nil, fmt.Errorf("register import cleanup worker: %w", err)
		}
		periodic = append(periodic, river.NewPeriodicJob(
			river.PeriodicInterval(ImportCleanupInterval),
			func() (river.JobArgs, *river.InsertOpts) { return ImportCleanupArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}
	if cfg.Noop.Enabled {
		periodic = append(periodic, river.NewPeriodicJob(
			river.PeriodicInterval(cfg.Noop.Interval),
			func() (river.JobArgs, *river.InsertOpts) { return NoopArgs{}, nil },
			nil,
		))
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            queues,
		Workers:           workers,
		PeriodicJobs:      periodic,
		FetchPollInterval: cfg.FetchPollInterval,
		Logger:            logger.With("component", "jobs"),
	})
	if err != nil {
		return nil, fmt.Errorf("create job queue client: %w", err)
	}
	if opts.Mail != nil {
		opts.Mail.SetQueue(client)
	}
	if opts.Apps != nil {
		opts.Apps.SetQueue(client)
	}
	return client, nil
}
