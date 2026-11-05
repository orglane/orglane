//go:build integration

package apps_test

import (
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
)

// holdWebhookLock takes the lock ensureWebhooks waits for, as a rotation
// attempt of webhook_secret holds it, and returns a function that waits
// until someone else waits for it.
func (h *harness) holdWebhookLock(inst apps.Instance) (waiting func(), release func()) {
	h.t.Helper()
	conn, err := h.pool.Acquire(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	id := apps.CredentialLockIDForTest(inst.ID, "webhook_secret")
	if _, err := conn.Exec(h.ctx, "SELECT pg_advisory_lock($1)", id); err != nil {
		h.t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_, _ = conn.Exec(h.ctx, "SELECT pg_advisory_unlock($1)", id)
			conn.Release()
		}
	}
	h.t.Cleanup(release)
	waiting = func() {
		h.t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for h.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`) == 0 {
			if time.Now().After(deadline) {
				h.t.Fatal("nobody waits for the webhook lock")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return waiting, release
}

// TestSetupWebhooksAfterStateChange: an instance that leaves installing
// while setup waits for the webhook lock gets no webhooks, and setup
// neither activates it nor overwrites its failure with setup_failed.
func TestSetupWebhooksAfterStateChange(t *testing.T) {
	h := newHarness(t)
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "testmanaged"})
	if err != nil {
		t.Fatal(err)
	}
	waiting, release := h.holdWebhookLock(inst)
	done := make(chan error, 1)
	go func() { done <- h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8) }()
	waiting()
	if _, err := h.svc.Transition(h.ctx, audit.System, inst.ID, apps.StateFailed, apps.CodeInstallFailed); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("setup: %v", err)
	}
	got, _ := h.svc.Instance(h.ctx, inst.ID)
	if got.State != apps.StateFailed || got.HealthCode == nil || *got.HealthCode != apps.CodeInstallFailed {
		t.Fatalf("after setup: %s %v, want failed with install_failed", got.State, got.HealthCode)
	}
	if n := h.managed.Hooks(inst.ID); n != 0 {
		t.Fatalf("webhooks ensured %d times for a failed instance", n)
	}

	// The daily repair likewise skips an instance that left active.
	active := h.instance(nil)
	waiting, release = h.holdWebhookLock(active)
	go func() { done <- h.svc.RunEnsureWebhooks(h.ctx, apps.EnsureWebhooksArgs{InstanceID: active.ID}) }()
	waiting()
	if _, err := h.svc.Transition(h.ctx, audit.System, active.ID, apps.StateUpgrading, ""); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if n := h.tp.Hooks(active.ID); n != 0 {
		t.Fatalf("webhooks ensured %d times for an upgrading instance", n)
	}
}

// TestWebhookSecretBackfilled: an instance without any webhook_secret (it
// was created before its adapter declared one) gets an active one from the
// webhook repair, once; an instance whose secret is being rotated in keeps
// its candidate.
func TestWebhookSecretBackfilled(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	versions := func() int {
		return h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret'`, inst.ID)
	}
	h.exec(`DELETE FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret'`, inst.ID)
	created := h.auditCount(audit.AppCredentialCreated)
	for range 2 {
		if err := h.svc.RunEnsureWebhooks(h.ctx, apps.EnsureWebhooksArgs{InstanceID: inst.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND state = 'active'`, inst.ID); n != 1 || versions() != 1 {
		t.Fatalf("%d active of %d versions, want one", n, versions())
	}
	if n := h.auditCount(audit.AppCredentialCreated) - created; n != 1 {
		t.Fatalf("%d credential audit entries, want 1", n)
	}
	if _, err := h.svc.EnvForTest(inst).Credential(h.ctx, "webhook_secret"); err != nil {
		t.Fatalf("the backfilled secret: %v", err)
	}
	if n := h.tp.Hooks(inst.ID); n != 2 {
		t.Fatalf("webhooks ensured %d times, want 2", n)
	}

	// Only a candidate (the first value is being rotated in): no backfill.
	h.exec(`DELETE FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret'`, inst.ID)
	if _, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunEnsureWebhooks(h.ctx, apps.EnsureWebhooksArgs{InstanceID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND state = 'active'`, inst.ID); n != 0 || versions() != 1 {
		t.Fatalf("backfilled next to a candidate: %d active of %d", n, versions())
	}
}
