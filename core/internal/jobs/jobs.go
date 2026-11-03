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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/config"
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

// Client is the job queue client.
type Client = river.Client[pgx.Tx]

// NewClient builds the job queue client. Start it with Start and stop it with
// Stop (or StopAndCancel when the shutdown deadline is near).
func NewClient(pool *pgxpool.Pool, cfg config.Jobs, logger *slog.Logger) (*Client, error) {
	workers := river.NewWorkers()
	if err := river.AddWorkerSafely(workers, &NoopWorker{Logger: logger}); err != nil {
		return nil, fmt.Errorf("register noop worker: %w", err)
	}

	var periodic []*river.PeriodicJob
	if cfg.Noop.Enabled {
		periodic = append(periodic, river.NewPeriodicJob(
			river.PeriodicInterval(cfg.Noop.Interval),
			func() (river.JobArgs, *river.InsertOpts) { return NoopArgs{}, nil },
			nil,
		))
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.MaxWorkers}},
		Workers:           workers,
		PeriodicJobs:      periodic,
		FetchPollInterval: cfg.FetchPollInterval,
		Logger:            logger.With("component", "jobs"),
	})
	if err != nil {
		return nil, fmt.Errorf("create job queue client: %w", err)
	}
	return client, nil
}
