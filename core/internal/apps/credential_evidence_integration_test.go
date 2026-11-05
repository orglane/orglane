//go:build integration

package apps_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
)

// Regression tests for provider call executions, evidence, the admin
// override and drift detection (docs/specs/providers.md "Credentials",
// concurrency model). They use the in-repo test provider; no provider API
// is replaced.

// deadlinesOf are the leases of the executions writing row's value, oldest
// execution first.
func deadlinesOf(t *testing.T, h *harness, row uuid.UUID) []time.Time {
	t.Helper()
	rows, err := h.pool.Query(h.ctx, `SELECT deadline FROM app_credential_calls WHERE credential_id = $1 ORDER BY started_at, deadline`, row)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// assertLeasesKept fails unless every lease in before still exists unchanged.
func assertLeasesKept(t *testing.T, h *harness, row uuid.UUID, before []time.Time) {
	t.Helper()
	after := deadlinesOf(t, h, row)
	for _, d := range before {
		found := false
		for _, a := range after {
			found = found || a.Equal(d)
		}
		if !found {
			t.Fatalf("an older execution's lease %v was ended or shortened: now %v", d, after)
		}
	}
}

// assertNoNewValue fails unless a new rotation and a direct replacement are
// both refused.
func assertNoNewValue(t *testing.T, h *harness, inst apps.Instance, why string) {
	t.Helper()
	if _, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret"); !errors.Is(err, apps.ErrRotationInProgress) {
		t.Fatalf("new rotation %s = %v, want rotation_in_progress", why, err)
	}
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", []byte("test-only-direct")); !errors.Is(err, apps.ErrRotationInProgress) {
		t.Fatalf("direct replacement %s = %v, want rotation_in_progress", why, err)
	}
}

// TestSameRowRetryWaitsForOlderExecution: an execution loses its backend
// inside its call. While its lease holds, the job's retry of the same row is
// deferred without a provider call (at most one execution per credential),
// and no new value is admitted; the older execution's lease is untouched.
func TestSameRowRetryWaitsForOlderExecution(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	f := startInFlight(t, h, h.svc, inst)
	before := deadlinesOf(t, h, f.id)
	f.terminate(t, h)
	calls := len(h.tp.Calls())
	err := h.svc.RunRotateCredential(bounded(t), apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: f.version, CredentialID: f.id})
	if err == nil {
		t.Fatal("a retry overlapped an execution that may still act")
	}
	if len(h.tp.Calls()) != calls {
		t.Fatal("the deferred retry contacted the provider")
	}
	assertLeasesKept(t, h, f.id, before)
	assertNoNewValue(t, h, inst, "while an execution may still act")
	if err = f.finish(t); err == nil {
		t.Fatal("a worker without its connection reported success")
	}
	if err = h.retry(inst, f.version); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.secret(inst, "webhook_secret"), h.tp.Secret(inst.ID, "webhook_secret")) {
		t.Fatal("the active version and the provider's value differ")
	}
}

// TestSameRowAbandonWaitsForOlderExecution: the same for an abandon re-apply.
func TestSameRowAbandonWaitsForOlderExecution(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	var id, activeID uuid.UUID
	if err = h.pool.QueryRow(h.ctx, `SELECT id FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND version = $2`, inst.ID, version).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err = h.pool.QueryRow(h.ctx, `SELECT id FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND state = 'active'`, inst.ID).Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	args := apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, CredentialID: id, Abandon: true}
	reached, release := h.tp.Block("rotate:webhook_secret")
	f := &inFlight{version: version, id: id, release: release, done: make(chan error, 1)}
	t.Cleanup(f.let)
	go func() { f.done <- h.svc.RunRotateCredential(context.Background(), args) }()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-apply did not reach the provider")
	}
	f.pid = workerPID(t, h, inst)
	before := deadlinesOf(t, h, activeID)
	f.terminate(t, h)
	calls := len(h.tp.Calls())
	if err = h.svc.RunRotateCredential(bounded(t), args); err == nil {
		t.Fatal("a re-apply overlapped an execution that may still act")
	}
	if len(h.tp.Calls()) != calls || h.credState(inst, version) != "abandoning" {
		t.Fatal("the deferred re-apply contacted the provider or changed the candidate")
	}
	assertLeasesKept(t, h, activeID, before)
	assertNoNewValue(t, h, inst, "while a re-apply may still act")
	if err = f.finish(t); err == nil {
		t.Fatal("a worker without its connection reported success")
	}
	expireLeases(h, inst)
	if err = h.svc.RunRotateCredential(h.ctx, args); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, version); s != "discarded" || !bytes.Equal(h.tp.Secret(inst.ID, "webhook_secret"), old) {
		t.Fatalf("after abandon: candidate %s, provider on the active value: %v", s, bytes.Equal(h.tp.Secret(inst.ID, "webhook_secret"), old))
	}
}

// TestMintedRetryAfterUnknownIsNotMade: a value the provider mints differs
// per attempt, so after an unknown outcome no further attempt is made for an
// adapter without read-back (another mint could invalidate the published
// one): the candidate stays uncertain until abandon or an admin resolution.
func TestMintedRetryAfterUnknownIsNotMade(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "api_token", []byte("first")); err != nil {
		t.Fatal(err)
	}
	h.tp.LoseResponses("api_token", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "api_token")
	if err != nil {
		t.Fatal(err)
	}
	run := func() error {
		return h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "api_token", Version: version})
	}
	if err = run(); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	expireLeases(h, inst)
	calls := len(h.tp.Calls())
	if err = run(); err != nil {
		t.Fatalf("an unsafe retry should end the job without a call: %v", err)
	}
	if len(h.tp.Calls()) != calls {
		t.Fatal("a provider-minted credential was minted again after an unknown outcome")
	}
	var state string
	if err = h.pool.QueryRow(h.ctx, `SELECT state FROM app_credentials WHERE instance_id = $1 AND name = 'api_token' AND version = $2`, inst.ID, version).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("candidate = %s, %v; want uncertain", state, err)
	}
	if got := h.secret(inst, "api_token"); string(got) != "first" {
		t.Fatalf("the active token changed: %q", got)
	}
}

// lateRemote separates a call's remote effect from its answer: the adapter
// answers unknown at once, while the provider applies the value only when
// the test releases it (a request processed after the client gave up).
type lateRemote struct {
	*testprovider.Provider
	mu      sync.Mutex
	pending chan struct{} // closed by the test to let the remote effect land
	landed  chan error
}

func (p *lateRemote) hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending, p.landed = make(chan struct{}), make(chan error, 1)
}

func (p *lateRemote) ApplyCredential(ctx context.Context, env provider.Env, inst provider.Instance, name string, version int, value []byte) (provider.Rotation, error) {
	p.mu.Lock()
	pending, landed := p.pending, p.landed
	p.mu.Unlock()
	if pending == nil {
		return p.Provider.ApplyCredential(ctx, env, inst, name, version, value)
	}
	current, err := env.Credential(ctx, name) // authenticated before answering
	clear(current)
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return provider.Rotation{Outcome: provider.RotationUnknown}, err
	}
	copied := bytes.Clone(value)
	go func() {
		<-pending
		_, err := p.Provider.ApplyCredential(context.WithoutCancel(ctx), fixedEnv{}, inst, name, version, copied)
		landed <- err
	}()
	return provider.Rotation{Outcome: provider.RotationUnknown}, nil
}

// fixedEnv is the Env of a request already in flight at the provider.
type fixedEnv struct{ provider.Env }

func (fixedEnv) Credential(context.Context, string) ([]byte, error) { return nil, provider.ErrNotFound }

// land lets the held remote effect apply and waits for it.
func (p *lateRemote) land(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	pending, landed := p.pending, p.landed
	p.pending = nil
	p.mu.Unlock()
	close(pending)
	select {
	case err := <-landed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the remote effect did not land")
	}
}

func lateRemoteService(t *testing.T, h *harness) (*apps.Service, *lateRemote) {
	t.Helper()
	p := &lateRemote{Provider: h.tp}
	reg, err := apps.NewRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	// No queue: it only works rotation attempts.
	return apps.NewService(h.pool, reg, ring, apps.Config{}), p
}

func driftAudits(h *harness) int {
	return h.auditCount(audit.AppCredentialDrift)
}

// pastDriftGrace ages the end of every older version beyond the grace for
// deliveries signed just before a change.
func pastDriftGrace(h *harness, inst apps.Instance) {
	h.exec(`UPDATE app_credentials SET ended_at = ended_at - interval '6 minutes' WHERE instance_id = $1 AND ended_at IS NOT NULL`, inst.ID)
}

// assertDriftShown fails unless the instance detail reports drift of exactly
// version of webhook_secret.
func assertDriftShown(t *testing.T, h *harness, inst apps.Instance, version int32) {
	t.Helper()
	d, err := h.svc.Detail(h.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.CredentialDrift) != 1 || d.CredentialDrift[0].Name != "webhook_secret" || d.CredentialDrift[0].Version != version {
		t.Fatalf("credential drift = %+v, want webhook_secret v%d", d.CredentialDrift, version)
	}
}

// assertNoRepair fails if anything was queued for the credential after the
// rotations: drift is reported, never repaired automatically.
func assertNoRepair(t *testing.T, h *harness, queued int) {
	t.Helper()
	if n := h.jobs(apps.RotateCredentialArgs{}.Kind()); n != queued {
		t.Fatalf("rotate_credential jobs = %d, want %d (drift queues nothing)", n, queued)
	}
}

// TestLateRemoteEffectIsReportedAsDrift: a call's answer is lost, proof by
// use activates its value, and a later rotation completes. The old request
// is only then applied by the provider: the next delivery, signed with the
// retiring version, is accepted and reported as drift once (replays of it
// add nothing), visible on the instance, and nothing is queued.
func TestLateRemoteEffectIsReportedAsDrift(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	svc, late := lateRemoteService(t, h)
	h.tp.LoseResponses("webhook_secret", 1)
	v2, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, v2); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	secret2 := h.tp.Secret(inst.ID, "webhook_secret")
	expireLeases(h, inst)
	late.hold()
	if err = svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: v2}); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	body := prBody("acme", 851)
	if _, err = h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(secret2, body, "late-proof", "pull_request"), body); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, v2); s != "active" {
		t.Fatalf("v2 = %s, want active (proof by use)", s)
	}
	assertNoNewValue(t, h, inst, "while an execution may still act")
	expireLeases(h, inst)
	v3, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, v3); err != nil {
		t.Fatal(err)
	}
	queued := h.jobs(apps.RotateCredentialArgs{}.Kind())
	late.land(t) // the provider applies the old v2 request now
	pastDriftGrace(h, inst)
	for i := range 3 { // one delivery and two replays of it
		body = prBody("acme", 852)
		if _, err = h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(secret2, body, "late-drift", "pull_request"), body); err != nil {
			t.Fatalf("delivery %d signed with the retiring version within its overlap: %v", i, err)
		}
	}
	if n := driftAudits(h); n != 1 {
		t.Fatalf("drift reported %d times, want once", n)
	}
	assertDriftShown(t, h, inst, v2)
	assertNoRepair(t, h, queued)
	// Rotating again is the remedy; the report then no longer shows.
	v4, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, v4); err != nil {
		t.Fatal(err)
	}
	if d, err := h.svc.Detail(h.ctx, inst.ID); err != nil || len(d.CredentialDrift) != 0 {
		t.Fatalf("drift after rotating again = %+v, %v", d.CredentialDrift, err)
	}
}

// TestDriftGracePeriod: a delivery signed with the previous version right
// after a rotation (sent before the change) is not drift.
func TestDriftGracePeriod(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, version); err != nil {
		t.Fatal(err)
	}
	body := prBody("acme", 860)
	if _, err = h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(old, body, "in-flight", "pull_request"), body); err != nil {
		t.Fatal(err)
	}
	if n := driftAudits(h); n != 0 {
		t.Fatalf("a delivery sent before the change was reported as drift (%d)", n)
	}
}

// TestForcedResolutionLateEffectIsReportedAsDrift: elapsed time is no
// evidence, so an uncertain candidate blocks new values after every lease
// ended; an admin forces it discarded, a new rotation completes, and the old
// request lands late. Deliveries signed with the discarded value stay
// rejected and are reported as drift once.
func TestForcedResolutionLateEffectIsReportedAsDrift(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	svc, late := lateRemoteService(t, h)
	v2, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	late.hold()
	if err = svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: v2}); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	assertNoNewValue(t, h, inst, "while v2 is uncertain")
	expireLeases(h, inst)
	assertNoNewValue(t, h, inst, "while v2 is uncertain, after every lease ended")
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveDiscarded); err != nil {
		t.Fatal(err)
	}
	v2Value := candidateValue(t, h, inst, v2)
	v3, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, v3); err != nil {
		t.Fatal(err)
	}
	queued := h.jobs(apps.RotateCredentialArgs{}.Kind())
	late.land(t)
	pastDriftGrace(h, inst)
	for i := range 3 {
		body := prBody("acme", 853+i) // fresh bodies signed with the old value
		if _, err = h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(v2Value, body, "late-discarded", "pull_request"), body); !errors.Is(err, apps.ErrWebhookSignature) {
			t.Fatalf("a delivery signed with a discarded value = %v, want rejected", err)
		}
	}
	if n := driftAudits(h); n != 1 {
		t.Fatalf("drift reported %d times, want once", n)
	}
	assertDriftShown(t, h, inst, v2)
	assertNoRepair(t, h, queued)
	if n := h.count(`SELECT count(*) FROM webhook_deliveries WHERE instance_id = $1`, inst.ID); n != 0 {
		t.Fatalf("a delivery signed with a discarded value was stored (%d)", n)
	}
}

// TestDiscardRetentionCountsFromDiscard: an older version is kept for drift
// detection for 7 days from when it ended, not from when it was created.
func TestDiscardRetentionCountsFromDiscard(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.tp.LoseResponses("webhook_secret", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, version); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	h.exec(`UPDATE app_credentials SET created_at = now() - interval '8 days' WHERE instance_id = $1 AND version = $2`, inst.ID, version)
	expireLeases(h, inst)
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveDiscarded); err != nil {
		t.Fatal(err)
	}
	if _, err = h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND version = $2`, inst.ID, version) != 1 {
		t.Fatal("a version discarded today was deleted (retention counted from its creation)")
	}
	h.exec(`UPDATE app_credentials SET ended_at = now() - interval '8 days' WHERE instance_id = $1 AND version = $2`, inst.ID, version)
	if _, err = h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND version = $2`, inst.ID, version) != 0 {
		t.Fatal("a version discarded 8 days ago was kept")
	}
}

// TestCleanupDeletesExpiredCalls: execution records are deleted a day after
// their lease ended.
func TestCleanupDeletesExpiredCalls(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.tp.LoseResponses("webhook_secret", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, version); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	if _, err = h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM app_credential_calls`) != 1 {
		t.Fatal("a live execution record was deleted")
	}
	h.exec(`UPDATE app_credential_calls SET deadline = now() - interval '2 days'`)
	res, err := h.svc.Cleanup(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Calls != 1 || h.count(`SELECT count(*) FROM app_credential_calls`) != 0 {
		t.Fatalf("expired execution records were kept (deleted %d)", res.Calls)
	}
}

// candidateValue opens a credential version's value the way the core does.
func candidateValue(t *testing.T, h *harness, inst apps.Instance, version int32) []byte {
	t.Helper()
	var ct []byte
	if err := h.pool.QueryRow(h.ctx, `SELECT ciphertext FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND version = $2`, inst.ID, version).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	v, err := ring.Sealer(crypto.PurposeAppCredentials).Open(ct, apps.CredentialADForTest(inst.ID, "webhook_secret", version))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestResolveRotation: the admin override's preconditions, both outcomes,
// audit and history.
func TestResolveRotation(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	if err := h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveActive); !errors.Is(err, apps.ErrNoRotation) {
		t.Fatalf("resolve without a rotation = %v", err)
	}
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", "maybe"); !errors.Is(err, apps.ErrInvalid) {
		t.Fatalf("resolve with an unknown outcome = %v", err)
	}
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveActive); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("resolve a pending candidate = %v, want invalid_state", err)
	}
	h.tp.LoseResponses("webhook_secret", 1)
	if err = h.rotate(inst, version); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveActive); !errors.Is(err, apps.ErrRotationInProgress) {
		t.Fatalf("resolve while a lease holds = %v, want rotation_in_progress", err)
	}
	expireLeases(h, inst)
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveActive); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, version); s != "active" || h.credState(inst, 1) != "retiring" {
		t.Fatalf("after resolve active: v%d %s, v1 %s", version, s, h.credState(inst, 1))
	}
	if bytes.Equal(h.secret(inst, "webhook_secret"), old) {
		t.Fatal("the active value did not change")
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'resolution' = 'active'`, string(audit.AppCredentialRotationResolved)); n != 1 {
		t.Fatalf("resolution audited %d times", n)
	}
	assertRotationOutcome(t, h, inst.ID, version, "activated_by_admin")

	h.tp.LoseResponses("webhook_secret", 1)
	next, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, next); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	if err = h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	expireLeases(h, inst)
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveDiscarded); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, next); s != "discarded" || h.credState(inst, version) != "active" {
		t.Fatalf("after resolve discarded: v%d %s, v%d %s", next, s, version, h.credState(inst, version))
	}
	assertRotationOutcome(t, h, inst.ID, next, "discarded_by_admin")
	// The abandon job queued before finds nothing left to do.
	h.drain()
	if s := h.credState(inst, next); s != "discarded" {
		t.Fatalf("a stale abandon job changed the resolved candidate: %s", s)
	}
}

// TestResolveMintedWithoutValue: a provider-minted candidate whose answer
// was lost has no value; it cannot be forced active (an assertion cannot
// recover the provider's bytes), only discarded.
func TestResolveMintedWithoutValue(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "api_token", []byte("first")); err != nil {
		t.Fatal(err)
	}
	h.tp.LoseResponses("api_token", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "api_token")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "api_token", Version: version}); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	expireLeases(h, inst)
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "api_token", apps.ResolveActive); !errors.Is(err, apps.ErrCandidateValueUnknown) {
		t.Fatalf("forcing a value-less candidate active = %v, want candidate_value_unknown", err)
	}
	if got := h.secret(inst, "api_token"); string(got) != "first" {
		t.Fatalf("the active token changed: %q", got)
	}
	if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "api_token", apps.ResolveDiscarded); err != nil {
		t.Fatal(err)
	}
}

// TestDefiniteRejectionEndsOwnExecution: a definite rejection that leaves
// the candidate in place (a retry after an unknown outcome, an abandon
// re-apply) ends its own execution, so the next attempt and an admin's
// resolution are not deferred by a finished call. Unknown outcomes keep
// their lease.
func TestDefiniteRejectionEndsOwnExecution(t *testing.T) {
	live := func(h *harness) int {
		return h.count(`SELECT count(*) FROM app_credential_calls WHERE deadline > now()`)
	}
	t.Run("retry after unknown", func(t *testing.T) {
		h := newHarness(t)
		inst := h.instance(nil)
		h.tp.LoseResponses("webhook_secret", 1)
		version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
		if err != nil {
			t.Fatal(err)
		}
		if err = h.rotate(inst, version); err == nil {
			t.Fatal("expected an unknown outcome")
		}
		if n := live(h); n != 1 {
			t.Fatalf("live executions after an unknown outcome = %d, want 1", n)
		}
		h.tp.RotationOutcomes("webhook_secret", provider.RotationRejected)
		if err = h.retry(inst, version); err == nil {
			t.Fatal("a rejected retry after an unknown outcome must stay retryable")
		}
		if n := live(h); n != 0 {
			t.Fatalf("live executions after a definite rejection = %d, want 0", n)
		}
		if s := h.credState(inst, version); s != "uncertain" {
			t.Fatalf("candidate = %s, want uncertain", s)
		}
		if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveDiscarded); err != nil {
			t.Fatalf("resolution after a finished call: %v", err)
		}
	})
	t.Run("abandon re-apply", func(t *testing.T) {
		h := newHarness(t)
		inst := h.instance(nil)
		version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
		if err != nil {
			t.Fatal(err)
		}
		if err = h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
			t.Fatal(err)
		}
		h.tp.RotationOutcomes("webhook_secret", provider.RotationRejected)
		if err = h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: true}); err == nil {
			t.Fatal("a rejected re-apply must retry")
		}
		if n := live(h); n != 0 {
			t.Fatalf("live executions after a definite rejection = %d, want 0", n)
		}
		if s := h.credState(inst, version); s != "abandoning" {
			t.Fatalf("candidate = %s, want abandoning", s)
		}
		if err = h.svc.ResolveRotation(h.ctx, audit.System, inst.ID, "webhook_secret", apps.ResolveDiscarded); err != nil {
			t.Fatalf("resolution after a finished call: %v", err)
		}
	})
}
