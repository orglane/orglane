//go:build integration

package apps_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/providers/provider"
)

// Regression tests for the credential concurrency model
// (docs/specs/providers.md "Credentials"). They use the in-repo test
// provider; no provider API is replaced.

// sizedPool is a pool of at most size connections on the harness database.
func sizedPool(t *testing.T, h *harness, size int32) *pgxpool.Pool {
	t.Helper()
	cfg := h.pool.Config()
	cfg.MaxConns = size
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// serviceOn is another service (a restarted or second worker process) on
// pool with the harness's providers, a queue and an optional clock.
func serviceOn(t *testing.T, h *harness, pool *pgxpool.Pool, now func() time.Time) *apps.Service {
	t.Helper()
	reg, err := apps.NewRegistry(h.tp, h.managed)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	svc := apps.NewService(pool, reg, ring, apps.Config{Now: now})
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetQueue(rc)
	return svc
}

// bounded is a context for one step that must not wait on itself.
func bounded(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// activationGate pauses every uncertain → active change of a credential row
// inside its transaction (after the activating transaction took its locks)
// until release is called. It is a trigger in the test database.
func activationGate(t *testing.T, h *harness, lockKey int64) (gatePID uint32, release func()) {
	t.Helper()
	fn := fmt.Sprintf("hold_activation_%d", lockKey)
	h.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state = 'uncertain' AND NEW.state = 'active' THEN
        PERFORM pg_advisory_xact_lock(%d);
    END IF;
    RETURN NEW;
END $$`, fn, lockKey))
	h.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON app_credentials FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn))
	gate, err := h.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = gate.Exec(t.Context(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := gate.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { release(); gate.Release() })
	return gate.Conn().PgConn().PID(), release
}

// blockedBy waits until a backend waits on a lock held by pid and returns
// that backend's pid.
func blockedBy(t *testing.T, h *harness, pid uint32) uint32 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked uint32
		err := h.pool.QueryRow(t.Context(), `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, pid).Scan(&blocked)
		if err == nil {
			return blocked
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no backend waits on a lock of %d", pid)
	return 0
}

// TestRotationRejectedAfterUnknown: a rejected retry proves only that the
// retry wrote nothing. The earlier attempt with a lost answer installed the
// candidate, so it stays uncertain across retries and a worker restart, and
// deliveries the provider signs with it still verify and prove it.
func TestRotationRejectedAfterUnknown(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	h.tp.LoseResponses("webhook_secret", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, version); err == nil {
		t.Fatal("a lost answer must leave the outcome unknown")
	}
	installed := h.tp.Secret(inst.ID, "webhook_secret")
	if bytes.Equal(installed, old) {
		t.Fatal("the provider did not commit the candidate")
	}
	h.tp.RotationOutcomes("webhook_secret", provider.RotationRejected, provider.RotationRejected)
	if err = h.rotate(inst, version); err == nil {
		t.Fatal("a rejected retry after an unknown outcome must stay retryable")
	}
	restarted := serviceOn(t, h, h.pool, nil)
	if err = restarted.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version}); err == nil {
		t.Fatal("a restarted worker's rejected retry must stay retryable")
	}
	if s := h.credState(inst, version); s != "uncertain" {
		t.Fatalf("candidate = %s, want uncertain", s)
	}
	if n := h.auditCount(audit.AppCredentialRotationFailed); n != 0 {
		t.Fatalf("rotation_failed recorded %d times for a candidate that may be in use", n)
	}
	if !bytes.Equal(h.secret(inst, "webhook_secret"), old) {
		t.Fatal("the active version changed")
	}
	body := prBody("acme", 481)
	if _, err = h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(installed, body, "unknown-then-rejected", "pull_request"), body); err != nil {
		t.Fatalf("a delivery signed with the installed candidate must verify: %v", err)
	}
	if s := h.credState(inst, version); s != "active" {
		t.Fatalf("candidate = %s, want active (proof by use)", s)
	}
}

// TestRotationMinimumPool: with database.max_conns=1 every rotation path
// completes on the worker's lock connection: acknowledgement (with the
// adapter's Env read), a first rejection, an unknown outcome with its stuck
// report, abandon with re-apply, and proof by use.
func TestRotationMinimumPool(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	pool := sizedPool(t, h, 1)
	svc := serviceOn(t, h, pool, nil)
	late := serviceOn(t, h, pool, func() time.Time { return time.Now().Add(25 * time.Hour) })
	run := func(s *apps.Service, version int32, abandon bool) error {
		ctx := bounded(t)
		err := s.RunRotateCredential(ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: abandon})
		if ctx.Err() != nil {
			t.Fatalf("the worker waited on the pool it holds: %v", err)
		}
		return err
	}
	rotate := func() int32 {
		v, err := svc.RotateCredential(bounded(t), audit.System, inst.ID, "webhook_secret")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	acked := rotate()
	if err := run(svc, acked, false); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, acked); s != "active" {
		t.Fatalf("acknowledged candidate = %s", s)
	}
	active := h.secret(inst, "webhook_secret")

	h.tp.RotationOutcomes("webhook_secret", provider.RotationRejected)
	rejected := rotate()
	if err := run(svc, rejected, false); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, rejected); s != "discarded" {
		t.Fatalf("rejected candidate = %s", s)
	}

	h.tp.LoseResponses("webhook_secret", 1)
	unknown := rotate()
	if err := run(late, unknown, false); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	if n := h.auditCount(audit.AppCredentialRotationStuck); n != 1 {
		t.Fatalf("rotation_stuck recorded %d times, want once", n)
	}
	if err := svc.AbandonRotation(bounded(t), audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	expireLeases(h, inst) // the lost forward call can no longer land
	if err := run(svc, unknown, true); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, unknown); s != "discarded" {
		t.Fatalf("abandoned candidate = %s", s)
	}
	if !bytes.Equal(h.tp.Secret(inst.ID, "webhook_secret"), active) {
		t.Fatal("abandon did not re-apply the active value")
	}

	h.tp.LoseResponses("webhook_secret", 1)
	proven := rotate()
	if err := run(svc, proven, false); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	body := prBody("acme", 483)
	if _, err := svc.ReceiveWebhook(bounded(t), inst.ID, delivery(h.tp.Secret(inst.ID, "webhook_secret"), body, "minimum-pool-proof", "pull_request"), body); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, proven); s != "active" {
		t.Fatalf("proven candidate = %s", s)
	}
}

// TestRotationPoolSaturation: two workers hold every connection of the
// pool across their provider calls; neither waits for a second one.
func TestRotationPoolSaturation(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "api_token", []byte("first")); err != nil {
		t.Fatal(err)
	}
	svc := serviceOn(t, h, sizedPool(t, h, 2), nil)
	names := []string{"webhook_secret", "api_token"}
	versions := map[string]int32{}
	for _, name := range names {
		v, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		versions[name] = v
	}
	var releases []chan<- struct{}
	var reached []<-chan struct{}
	for _, name := range names {
		r, rel := h.tp.Block("rotate:" + name)
		reached, releases = append(reached, r), append(releases, rel)
	}
	ctx := bounded(t)
	done := make(chan error, len(names))
	for _, name := range names {
		go func() {
			done <- svc.RunRotateCredential(ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: name, Version: versions[name]})
		}()
	}
	for _, r := range reached {
		select {
		case <-r:
		case <-ctx.Done():
			t.Fatal("a worker did not reach the provider")
		}
	}
	for _, rel := range releases {
		close(rel)
	}
	for range names {
		if err := <-done; err != nil {
			t.Fatalf("a worker of a saturated pool failed: %v", err)
		}
	}
	for _, name := range names {
		var state string
		if err := h.pool.QueryRow(h.ctx, `SELECT state FROM app_credentials WHERE instance_id = $1 AND name = $2 AND version = $3`, inst.ID, name, versions[name]).Scan(&state); err != nil || state != "active" {
			t.Fatalf("%s v%d = %q, %v", name, versions[name], state, err)
		}
	}
}

// TestRotationProofByUseVersusAbandon: proof by use holds the instance and
// candidate locks while it activates; a concurrent cancellation waits on the
// instance lock and then finds no rotation, instead of deadlocking with the
// delivery insert's foreign-key check.
func TestRotationProofByUseVersusAbandon(t *testing.T) {
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
	gatePID, release := activationGate(t, h, 94642062)
	ctx := bounded(t)
	proved := make(chan error, 1)
	go func() {
		body := prBody("acme", 482)
		_, err := h.svc.ReceiveWebhook(ctx, inst.ID, delivery(h.tp.Secret(inst.ID, "webhook_secret"), body, "proof-versus-abandon", "pull_request"), body)
		proved <- err
	}()
	proof := blockedBy(t, h, gatePID)
	abandoned := make(chan error, 1)
	go func() { abandoned <- h.svc.AbandonRotation(ctx, audit.System, inst.ID, "webhook_secret") }()
	blockedBy(t, h, proof)
	release()
	if err := <-proved; err != nil {
		t.Fatalf("proof by use failed: %v", err)
	}
	if err := <-abandoned; !errors.Is(err, apps.ErrNoRotation) {
		t.Fatalf("cancellation after proof = %v, want no_rotation", err)
	}
	if s := h.credState(inst, version); s != "active" {
		t.Fatalf("candidate = %s, want active", s)
	}
	if n := h.auditCount(audit.AppCredentialRotationAbandoned); n != 0 {
		t.Fatalf("a lost cancellation recorded %d intents", n)
	}
	if n := h.count(`SELECT count(*) FROM webhook_deliveries WHERE instance_id = $1`, inst.ID); n != 1 {
		t.Fatalf("deliveries = %d, want the proving one", n)
	}
}

// TestRotationStuckReportedPerRow: the stuck report is recorded once per
// candidate row, also when a later candidate reuses the version of a
// cleaned-up one.
func TestRotationStuckReportedPerRow(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	reporting := serviceOn(t, h, h.pool, func() time.Time { return time.Now().Add(25 * time.Hour) })
	for i := 1; i <= 2; i++ {
		h.tp.LoseResponses("webhook_secret", 3) // every forward attempt below
		version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
		if err != nil {
			t.Fatal(err)
		}
		if version != 2 {
			t.Fatalf("lifetime %d: version %d, the test needs version reuse", i, version)
		}
		if err = h.rotate(inst, version); err == nil {
			t.Fatal("expected an unknown outcome")
		}
		for range 2 {
			if _, err = reporting.Cleanup(h.ctx); err != nil {
				t.Fatal(err)
			}
			expireLeases(h, inst)
			if err = reporting.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version}); err == nil {
				t.Fatal("expected an unknown outcome")
			}
		}
		if n := h.auditCount(audit.AppCredentialRotationStuck); n != i {
			t.Fatalf("candidate lifetime %d: rotation_stuck recorded %d times, want %d", i, n, i)
		}
		if i == 1 {
			if err = h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
				t.Fatal(err)
			}
			expireLeases(h, inst) // the lost forward call can no longer land
			if err = h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: true}); err != nil {
				t.Fatal(err)
			}
			h.exec(`UPDATE app_credentials SET ended_at = now() - interval '8 days' WHERE instance_id = $1 AND state = 'discarded'`, inst.ID)
			if _, err = h.svc.Cleanup(h.ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestStoreCredentialDuringRotation: a direct replacement is refused while a
// candidate exists, pending or uncertain, so the rotation can never retire a
// newer directly stored value (#84).
func TestStoreCredentialDuringRotation(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	h.tp.LoseResponses("webhook_secret", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	direct := []byte("test-only-direct-replacement")
	if err = h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", direct); !errors.Is(err, apps.ErrRotationInProgress) {
		t.Fatalf("direct replacement of a pending rotation = %v, want rotation_in_progress", err)
	}
	if err = h.rotate(inst, version); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	if err = h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", direct); !errors.Is(err, apps.ErrRotationInProgress) {
		t.Fatalf("direct replacement of an uncertain rotation = %v, want rotation_in_progress", err)
	}
	if !bytes.Equal(h.secret(inst, "webhook_secret"), old) {
		t.Fatal("the active version changed during the rotation")
	}
	if err = h.retry(inst, version); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, version); s != "active" {
		t.Fatalf("candidate = %s, want active", s)
	}
	if n := h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret'`, inst.ID); n != 2 {
		t.Fatalf("webhook_secret rows = %d, want the original and the rotated one", n)
	}
	// The retry ran after the lost execution's lease and ended its own with
	// the answer: a direct replacement is allowed again.
	if err = h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", direct); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.secret(inst, "webhook_secret"), direct) {
		t.Fatal("the direct replacement is not active")
	}
}

// TestRotationStaleJobAfterVersionReuse: a job names its candidate row by
// its durable ID, so once cleanup deleted the row and a new candidate
// reused the version, the stale job does nothing.
func TestRotationStaleJobAfterVersionReuse(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.tp.RotationOutcomes("webhook_secret", provider.RotationRejected)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	var stale apps.RotateCredentialArgs
	var raw []byte
	if err = h.pool.QueryRow(h.ctx, `SELECT args FROM river_job WHERE kind = $1`, stale.Kind()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	h.decode(raw, &stale)
	if stale.CredentialID == uuid.Nil {
		t.Fatal("the job does not carry the candidate's identity")
	}
	if err = h.rotate(inst, version); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE app_credentials SET ended_at = now() - interval '8 days' WHERE instance_id = $1 AND state = 'discarded'`, inst.ID)
	if _, err = h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	reused, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil || reused != version {
		t.Fatalf("rotate = v%d, %v; the test needs version reuse of v%d", reused, err, version)
	}
	calls := len(h.tp.Calls())
	if err = h.svc.RunRotateCredential(h.ctx, stale); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, reused); s != "pending" || len(h.tp.Calls()) != calls {
		t.Fatalf("the stale job acted on the reused version: %s, %d provider calls", s, len(h.tp.Calls())-calls)
	}
	h.drain()
	if s := h.credState(inst, reused); s != "active" {
		t.Fatalf("the new candidate's own job left it %s", s)
	}
}
