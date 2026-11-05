//go:build integration

package apps_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/providers/testprovider"
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

// onOneConnection is the harness's service on a pool of one connection, the
// smallest supported database.max_conns.
func (h *harness) onOneConnection() *apps.Service {
	h.t.Helper()
	cfg := h.pool.Config()
	cfg.MaxConns = 1
	p, err := pgxpool.NewWithConfig(h.ctx, cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(p.Close)
	keyring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		h.t.Fatal(err)
	}
	svc := apps.NewService(p, h.svc.Registry(), keyring, apps.Config{})
	rc, err := river.NewClient(riverpgxv5.New(p), &river.Config{})
	if err != nil {
		h.t.Fatal(err)
	}
	svc.SetQueue(rc)
	return svc
}

// TestWebhooksOnOneConnection: ensuring webhooks, with the credential reads
// of the adapter, needs no second pool connection while it holds its lock
// connection: it completes with database.max_conns=1, from the daily job
// and from Setup.
func TestWebhooksOnOneConnection(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	installing, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "testmanaged"})
	if err != nil {
		t.Fatal(err)
	}
	svc := h.onOneConnection()
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	if err := svc.RunEnsureWebhooks(ctx, apps.EnsureWebhooksArgs{InstanceID: inst.ID}); err != nil {
		t.Fatalf("ensure on one connection: %v", err)
	}
	active, err := h.svc.EnvForTest(inst).Credential(h.ctx, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if h.tp.Hooks(inst.ID) != 1 || !bytes.Equal(h.tp.Secret(inst.ID, "webhook_secret"), active) {
		t.Fatal("the hook does not have the active secret")
	}
	if err := svc.RunSetup(ctx, apps.SetupArgs{InstanceID: installing.ID}, 1, 8); err != nil {
		t.Fatalf("setup on one connection: %v", err)
	}
	if got, _ := h.svc.Instance(h.ctx, installing.ID); got.State != apps.StateActive || h.managed.Hooks(installing.ID) != 1 {
		t.Fatalf("after setup: %s, %d hooks", got.State, h.managed.Hooks(installing.ID))
	}
}

// TestRepairWaitsForRotation: a rotation hands the provider a new secret
// and loses the answer; the webhook repair (daily and from Setup) leaves
// the provider's secret alone while the candidate is unresolved, so a
// delayed delivery signed with the new secret proves it by use, and the
// core's active secret is the one the provider uses.
func TestRepairWaitsForRotation(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.RunEnsureWebhooks(h.ctx, apps.EnsureWebhooksArgs{InstanceID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	old := h.tp.Secret(inst.ID, "webhook_secret")
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	h.tp.LoseResponses("webhook_secret", 1)
	if err := h.rotate(inst, version); err == nil {
		t.Fatal("a lost answer was taken as an outcome")
	}
	candidate := h.tp.Secret(inst.ID, "webhook_secret")
	if bytes.Equal(candidate, old) || h.credState(inst, version) != "uncertain" {
		t.Fatalf("after the lost answer: %s", h.credState(inst, version))
	}
	// The daily repair: deferred, the provider keeps the candidate.
	if err := h.svc.RunEnsureWebhooks(h.ctx, apps.EnsureWebhooksArgs{InstanceID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.tp.Secret(inst.ID, "webhook_secret"), candidate) || h.tp.Hooks(inst.ID) != 1 {
		t.Fatal("the repair put the active secret back over the uncertain rotation")
	}
	// A delivery the provider signed with the candidate arrives: proof by use.
	body, err := json.Marshal(testprovider.Payload{Organization: "acme", Action: "opened", ID: 7, Number: 1, Title: "x", Actor: "bob", ActorID: 2})
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{}
	hdr.Set(testprovider.SignatureHeader, testprovider.Sign(candidate, body))
	hdr.Set(testprovider.DeliveryHeader, "delayed-1")
	hdr.Set("X-Forgejo-Event", "pull_request")
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, hdr, body); err != nil {
		t.Fatal(err)
	}
	if h.credState(inst, version) != "active" {
		t.Fatalf("candidate = %s, want active (proven by use)", h.credState(inst, version))
	}
	active, err := h.svc.EnvForTest(inst).Credential(h.ctx, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(active, h.tp.Secret(inst.ID, "webhook_secret")) {
		t.Fatal("the core's active secret is not the one the provider uses")
	}

	// Setup with a rotation in progress: the instance is set up and
	// activated; the hook is left to the rotation.
	installing, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "testmanaged"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RotateCredential(h.ctx, audit.System, installing.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: installing.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.svc.Instance(h.ctx, installing.ID); got.State != apps.StateActive || h.managed.Hooks(installing.ID) != 0 {
		t.Fatalf("setup during a rotation: %s, %d hooks", got.State, h.managed.Hooks(installing.ID))
	}
}
