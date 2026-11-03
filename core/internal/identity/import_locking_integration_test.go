//go:build integration

package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
)

// A root team must not appear after an import takes its ordered team snapshot.
// Otherwise a membership operation can lock that new team then wait on an
// earlier imported person while a later import row waits on the team's FK lock.
func TestRootTeamCreationWaitsForImportTeamSnapshot(t *testing.T) {
	svc, pool := newService(t)
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err = identity.LockImportTx(t.Context(), store.New(tx)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		team store.Team
		err  error
	}
	created := make(chan result, 1)
	go func() {
		team, err := svc.CreateTeam(t.Context(), audit.System, identity.NewTeam{Slug: "concurrent-root", Name: "Concurrent Root"})
		created <- result{team: team, err: err}
	}()
	waiting := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE $1::bigint = ANY(pg_blocking_pids(pid)::bigint[]) AND wait_event_type = 'Lock'`, int64(tx.Conn().PgConn().PID())).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			waiting = true
			break
		}
		select {
		case r := <-created:
			t.Fatalf("root team creation escaped import's ordered team snapshot: team=%s,error=%v", r.team.Slug, r.err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		t.Fatal("team creation never waited on the import hierarchy lock")
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-created:
		if r.err != nil || r.team.Slug != "concurrent-root" {
			t.Fatalf("creation after import=%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("root team creation remained blocked after import ended")
	}
}
