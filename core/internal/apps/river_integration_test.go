//go:build integration

package apps_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/jobs"
)

// TestJobsOnRiver runs the framework's jobs on a real, started River
// client with the apps queue: a team membership change made through
// identity reaches the provider without anyone working jobs by hand.
func TestJobsOnRiver(t *testing.T) {
	h := newHarness(t)
	cfg := config.Default().Jobs
	cfg.FetchPollInterval = 100 * time.Millisecond
	client, err := jobs.NewClient(h.pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), jobs.Options{Apps: h.svc})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
	alice := h.person("alice")
	inst := h.instance(nil)
	team := h.team("platform")
	h.mapTeam(inst, team)
	if _, err := h.ids.SetTeamMember(h.ctx, audit.System, team.ID, alice.ID, identity.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if acct, ok := h.tp.Account(inst.ID, "alice"); ok && acct.Teams["team-platform"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("alice was not provisioned; calls %v", h.tp.Calls())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := h.count(`SELECT count(*) FROM river_job WHERE kind LIKE 'orglane.apps.%' AND queue <> $1`, apps.QueueName); n != 0 {
		t.Fatalf("%d apps jobs ran outside the apps queue", n)
	}
}
