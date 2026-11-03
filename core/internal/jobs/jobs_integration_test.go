//go:build integration

package jobs_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/jobs"
	"github.com/orglane/orglane/tests/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func startClient(t *testing.T, cfg config.Jobs) *jobs.Client {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := database.Connect(ctx, config.Database{URL: pgtest.NewDatabase(t), MaxConns: 5, ConnectTimeout: 10 * time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	m, err := database.NewMigrator(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	client, err := jobs.NewClient(pool, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	return client
}

func waitCompleted(t *testing.T, client *jobs.Client, timeout time.Duration) *river.Event {
	t.Helper()
	events, cancel := client.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	t.Cleanup(cancel)
	select {
	case ev := <-events:
		return ev
	case <-time.After(timeout):
		t.Fatalf("no job finished within %s", timeout)
		return nil
	}
}

func TestNoopJobRuns(t *testing.T) {
	cfg := config.Default().Jobs
	client := startClient(t, cfg)
	if _, err := client.Insert(context.Background(), jobs.NoopArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	if ev := waitCompleted(t, client, 15*time.Second); ev.Kind != river.EventKindJobCompleted || ev.Job.Kind != "orglane.noop" {
		t.Fatalf("event = %s %s, want completed orglane.noop", ev.Kind, ev.Job.Kind)
	}
}

func TestPeriodicNoopJob(t *testing.T) {
	cfg := config.Default().Jobs
	cfg.Noop = config.PeriodicJob{Enabled: true, Interval: time.Second}
	client := startClient(t, cfg)
	// The periodic enqueuer runs on the elected leader; election takes a few
	// seconds on a fresh client.
	if ev := waitCompleted(t, client, 30*time.Second); ev.Kind != river.EventKindJobCompleted || ev.Job.Kind != "orglane.noop" {
		t.Fatalf("event = %s %s, want completed orglane.noop", ev.Kind, ev.Job.Kind)
	}
}
