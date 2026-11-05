//go:build integration

package apps_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/providers/provider"
)

func assertRotationOutcome(t *testing.T, h *harness, instanceID uuid.UUID, version int32, state string) {
	t.Helper()
	detail, err := h.svc.Detail(h.ctx, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.CredentialRotations) != 1 || detail.CredentialRotations[0].Name != "webhook_secret" || detail.CredentialRotations[0].Version != version || detail.CredentialRotations[0].State != state {
		t.Fatalf("safe rotation outcome = %+v, want v%d %s", detail.CredentialRotations, version, state)
	}
}

// Framework lifecycle checks use the in-repo provider, not a substituted GitHub API.
func TestRotationSafeOutcomes(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.tp.LoseResponses("webhook_secret", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	assertRotationOutcome(t, h, inst.ID, version, "accepted")
	if err = h.rotate(inst, version); err == nil {
		t.Fatal("lost response must remain uncertain")
	}
	assertRotationOutcome(t, h, inst.ID, version, "verifying")
	if err = h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	expireLeases(h, inst)                                       // the lost forward call can no longer land
	assertRotationOutcome(t, h, inst.ID, version, "cancelling") //nolint:misspell // Existing public API wire state.
	if err = h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: true}); err != nil {
		t.Fatal(err)
	}
	assertRotationOutcome(t, h, inst.ID, version, "cancelled") //nolint:misspell // Existing public API wire state.
	h.exec("DELETE FROM app_credentials WHERE instance_id=$1 AND state='discarded'", inst.ID)
	assertRotationOutcome(t, h, inst.ID, version, "cancelled") //nolint:misspell // Existing public API wire state.
	reused, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if reused != version {
		t.Fatalf("regression must exercise version reuse: got %d, want %d", reused, version)
	}
	assertRotationOutcome(t, h, inst.ID, reused, "accepted")
	if err = h.rotate(inst, reused); err != nil {
		t.Fatal(err)
	}
	assertRotationOutcome(t, h, inst.ID, reused, "verified")
	h.tp.RotationOutcomes("webhook_secret", provider.RotationRejected)
	version, err = h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	assertRotationOutcome(t, h, inst.ID, version, "accepted")
	if err = h.rotate(inst, version); err != nil {
		t.Fatal(err)
	}
	assertRotationOutcome(t, h, inst.ID, version, "failed")
	h.exec("DELETE FROM app_credentials WHERE instance_id=$1 AND state='discarded'", inst.ID)
	assertRotationOutcome(t, h, inst.ID, version, "failed")
}

// pauseQuery is a pgx tracer that holds the first query containing match
// until release is closed: a transaction that began earlier then reaches
// the database later.
type pauseQuery struct {
	match   string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *pauseQuery) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, p.match) {
		p.once.Do(func() {
			close(p.reached)
			<-p.release
		})
	}
	return ctx
}

func (p *pauseQuery) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// An acceptance whose transaction began before the previous rotation's
// completion, but asked for the instance lock after the completion took
// it, waits behind it and commits later with an earlier occurred_at.
func TestRotationOutcomeAfterWaitingAcceptance(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	previous, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	pause := &pauseQuery{match: "FROM app_instances WHERE id = $1 FOR NO KEY UPDATE", reached: make(chan struct{}), release: make(chan struct{})}
	cfg := h.pool.Config()
	cfg.ConnConfig.Tracer = pause
	paused, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer paused.Close()
	accepting := serviceOn(t, h, paused, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type result struct {
		version int32
		err     error
	}
	done := make(chan result, 1)
	go func() {
		version, err := accepting.RotateCredential(ctx, audit.System, inst.ID, "webhook_secret")
		done <- result{version, err}
	}()
	select {
	case <-pause.reached: // the acceptance's transaction has begun
	case <-ctx.Done():
		t.Fatal("the acceptance did not reach its instance lock")
	}
	if err = h.rotate(inst, previous); err != nil {
		t.Fatal(err)
	}
	close(pause.release)
	next := <-done
	if next.err != nil {
		t.Fatal(next.err)
	}
	if got := h.credState(inst, next.version); got != "pending" {
		t.Fatalf("new candidate = %s, want pending", got)
	}
	if got := h.credState(inst, previous); got != "active" {
		t.Fatalf("previous key = %s, want active until replacement verification", got)
	}
	var timestampsInverted, insertedLater bool
	if err = h.pool.QueryRow(t.Context(), `SELECT started.occurred_at < completed.occurred_at, started.id > completed.id
FROM audit_entries started, audit_entries completed
WHERE started.target_id=$1 AND completed.target_id=$1
AND started.action='app_credential.rotation_started' AND (started.metadata->>'version')::int=$2
AND completed.action='app_credential.rotated' AND (completed.metadata->>'version')::int=$3`, inst.ID.String(), next.version, previous).Scan(&timestampsInverted, &insertedLater); err != nil {
		t.Fatal(err)
	}
	if !timestampsInverted || !insertedLater {
		t.Fatalf("race must invert transaction timestamps while retaining insertion-time ID order: timestamps=%v IDs=%v", timestampsInverted, insertedLater)
	}
	assertRotationOutcome(t, h, inst.ID, next.version, "accepted")
}
