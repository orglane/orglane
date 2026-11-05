//go:build integration

package apps_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
)

// Verification takes the instance and candidate locks before it activates
// (docs/specs/providers.md "Credentials", lock order). A cancellation that
// arrives meanwhile waits on the instance lock, then finds the candidate
// gone, and must not invent intent.
func TestRotationVerificationWinsAbandon(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	// A test-database gate pauses the real worker inside its activating
	// transaction, after it took its locks. No provider API is replaced.
	gatePID, release := activationGate(t, h, 94642061)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	forward := make(chan error, 1)
	go func() {
		forward <- h.svc.RunRotateCredential(ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version})
	}()
	worker := blockedBy(t, h, gatePID)
	back := make(chan error, 1)
	go func() { back <- h.svc.AbandonRotation(ctx, audit.System, inst.ID, "webhook_secret") }()
	blockedBy(t, h, worker)
	release()
	if err = <-forward; err != nil {
		t.Fatal(err)
	}
	if err = <-back; !errors.Is(err, apps.ErrNoRotation) {
		t.Fatalf("verification winner must reject late cancellation: got %v, want no_rotation", err)
	}
	if h.auditCount(audit.AppCredentialRotationAbandoned) != 0 {
		t.Fatal("late cancellation emitted a phantom intent")
	}
	if n := h.count(`SELECT count(*) FROM river_job WHERE kind=$1 AND args->>'abandon'='true'`, apps.RotateCredentialArgs{}.Kind()); n != 0 {
		t.Fatalf("late cancellation queued %d phantom jobs", n)
	}
	// Even a stale job must leave the verified terminal outcome intact.
	if err = h.svc.RunRotateCredential(ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: true}); err != nil {
		t.Fatal(err)
	}
	if got := h.credState(inst, version); got != "active" {
		t.Fatalf("verified key = %s, want active", got)
	}
	assertRotationOutcome(t, h, inst.ID, version, "verified")
	assertRotationOutcome(t, h, inst.ID, version, "verified") // fresh detail reload
}

func TestRotationRepeatedAbandonDoesNotRecordIntent(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	if err = h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); !errors.Is(err, apps.ErrNoRotation) {
		t.Fatalf("already-abandoning candidate is not eligible for a new intent: %v", err)
	}
	if n := h.auditCount(audit.AppCredentialRotationAbandoned); n != 1 {
		t.Fatalf("repeated cancellation emitted %d intents, want one", n)
	}
	if n := h.count(`SELECT count(*) FROM river_job WHERE kind=$1 AND args->>'abandon'='true'`, apps.RotateCredentialArgs{}.Kind()); n != 1 {
		t.Fatalf("cancellation jobs = %d, want one", n)
	}
	assertRotationOutcome(t, h, inst.ID, version, "cancelling") //nolint:misspell // existing public API state
	if err = h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: true}); err != nil {
		t.Fatal(err)
	}
	assertRotationOutcome(t, h, inst.ID, version, "cancelled") //nolint:misspell // existing public API state
}
