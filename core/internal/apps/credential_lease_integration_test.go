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

// Provider calls in flight are durable leases (docs/specs/providers.md
// "Credentials", concurrency model): these tests terminate a worker's
// database backend during its provider call, which releases its session
// lock while the call keeps running.

// inFlight is a forward rotation worker paused inside its provider call.
type inFlight struct {
	version int32
	id      uuid.UUID
	release chan<- struct{}
	once    sync.Once
	done    chan error
	pid     uint32
}

func (f *inFlight) let() { f.once.Do(func() { close(f.release) }) }

// startInFlight makes webhook_secret's candidate uncertain (its first
// answer lost, the provider installed it) and pauses its retry inside the
// provider call.
func startInFlight(t *testing.T, h *harness, svc *apps.Service, inst apps.Instance) *inFlight {
	t.Helper()
	h.tp.LoseResponses("webhook_secret", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.rotate(inst, version); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	expireLeases(h, inst) // the retry runs after the lost execution's lease
	f := &inFlight{version: version, done: make(chan error, 1)}
	if err = h.pool.QueryRow(h.ctx, `SELECT id FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND version = $2`, inst.ID, version).Scan(&f.id); err != nil {
		t.Fatal(err)
	}
	reached, release := h.tp.Block("rotate:webhook_secret")
	f.release = release
	t.Cleanup(f.let)
	go func() {
		f.done <- svc.RunRotateCredential(context.Background(), apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, CredentialID: f.id})
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not reach the provider")
	}
	f.pid = workerPID(t, h, inst)
	return f
}

// workerPID is the database backend of the worker holding webhook_secret's
// credential lock.
func workerPID(t *testing.T, h *harness, inst apps.Instance) uint32 {
	t.Helper()
	var pid uint32
	key := apps.CredentialLockIDForTest(inst.ID, "webhook_secret")
	if err := h.pool.QueryRow(h.ctx, `SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND classid::bigint = (($1::bigint >> 32) & 4294967295) AND objid::bigint = ($1::bigint & 4294967295) AND objsubid = 1`, key).Scan(&pid); err != nil {
		t.Fatalf("find the worker's backend: %v", err)
	}
	return pid
}

// finish lets the paused call complete and returns the worker's result.
func (f *inFlight) finish(t *testing.T) error {
	t.Helper()
	f.let()
	select {
	case err := <-f.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not return")
		return nil
	}
}

// terminate ends the worker's database backend and waits until it is gone.
func (f *inFlight) terminate(t *testing.T, h *harness) {
	t.Helper()
	var ok bool
	if err := h.pool.QueryRow(h.ctx, `SELECT pg_terminate_backend($1)`, f.pid).Scan(&ok); err != nil || !ok {
		t.Fatalf("terminate the worker's backend: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for h.count(`SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, f.pid) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker's backend did not exit")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// expireLeases lets every execution's lease of the instance end, as time
// passing would.
func expireLeases(h *harness, inst apps.Instance) {
	h.exec(`UPDATE app_credential_calls SET deadline = now() - interval '1 second'
		WHERE credential_id IN (SELECT id FROM app_credentials WHERE instance_id = $1)`, inst.ID)
}

// leaseOf is the latest lease of the executions writing row's value.
func leaseOf(t *testing.T, h *harness, row uuid.UUID) time.Time {
	t.Helper()
	var d time.Time
	if err := h.pool.QueryRow(h.ctx, `SELECT max(deadline) FROM app_credential_calls WHERE credential_id = $1`, row).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestRotationLeaseSurvivesBackendLoss: the worker's backend dies during a
// retry whose candidate proof by use then activates. Its session lock is
// gone, but the call's lease still refuses a direct replacement and a new
// rotation until it expired, so the late call can never overwrite them.
func TestRotationLeaseSurvivesBackendLoss(t *testing.T) {
	for _, next := range []string{"direct", "rotation"} {
		t.Run(next, func(t *testing.T) {
			h := newHarness(t)
			inst := h.instance(nil)
			f := startInFlight(t, h, h.svc, inst)
			installed := h.tp.Secret(inst.ID, "webhook_secret")
			body := prBody("acme", 848)
			if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(installed, body, "lease-proof", "pull_request"), body); err != nil {
				t.Fatal(err)
			}
			if s := h.credState(inst, f.version); s != "active" {
				t.Fatalf("candidate = %s, want active (proof by use)", s)
			}
			f.terminate(t, h)
			direct := []byte("test-only-direct-replacement")
			attempt := func() error {
				if next == "direct" {
					return h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", direct)
				}
				_, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
				return err
			}
			if err := attempt(); !errors.Is(err, apps.ErrRotationInProgress) {
				t.Fatalf("%s after the worker's backend died = %v, want rotation_in_progress", next, err)
			}
			// The call completes at the provider; its worker cannot record it.
			if err := f.finish(t); err == nil {
				t.Fatal("a worker without its connection reported success")
			}
			if err := attempt(); !errors.Is(err, apps.ErrRotationInProgress) {
				t.Fatalf("%s before the lease expired = %v, want rotation_in_progress", next, err)
			}
			expireLeases(h, inst)
			if err := attempt(); err != nil {
				t.Fatalf("%s after the lease expired: %v", next, err)
			}
			if next == "rotation" {
				h.drain()
				if !bytes.Equal(h.secret(inst, "webhook_secret"), h.tp.Secret(inst.ID, "webhook_secret")) {
					t.Fatal("the provider's value and the active version differ")
				}
			}
		})
	}
}

// TestRotationStaleResultFenced: an execution whose lease ended while its
// call ran records nothing, even a definite answer, and its job retries
// instead of completing as if the candidate were resolved; the retry then
// records normally. The same holds for an abandon re-apply.
func TestRotationStaleResultFenced(t *testing.T) {
	t.Run("forward", func(t *testing.T) {
		h := newHarness(t)
		inst := h.instance(nil)
		f := startInFlight(t, h, h.svc, inst)
		expireLeases(h, inst) // the call outlived its lease (a stalled worker)
		if err := f.finish(t); err == nil {
			t.Fatal("an execution that lost its lease completed its job")
		}
		if s := h.credState(inst, f.version); s != "uncertain" {
			t.Fatalf("a stale result was recorded: candidate = %s", s)
		}
		if n := h.auditCount(audit.AppCredentialRotated); n != 0 {
			t.Fatalf("a stale result recorded %d activations", n)
		}
		if err := h.rotate(inst, f.version); err != nil {
			t.Fatal(err)
		}
		if s := h.credState(inst, f.version); s != "active" {
			t.Fatalf("candidate after the retry = %s, want active", s)
		}
	})
	t.Run("abandon", func(t *testing.T) {
		h := newHarness(t)
		inst := h.instance(nil)
		version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
		if err != nil {
			t.Fatal(err)
		}
		if err = h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
			t.Fatal(err)
		}
		args := apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: true}
		reached, release := h.tp.Block("rotate:webhook_secret")
		done := make(chan error, 1)
		go func() { done <- h.svc.RunRotateCredential(context.Background(), args) }()
		<-reached
		expireLeases(h, inst)
		close(release)
		if err = <-done; err == nil {
			t.Fatal("a re-apply that lost its lease completed its job")
		}
		if s := h.credState(inst, version); s != "abandoning" {
			t.Fatalf("a stale re-apply discarded the candidate: %s", s)
		}
		if err = h.svc.RunRotateCredential(h.ctx, args); err != nil {
			t.Fatal(err)
		}
		if s := h.credState(inst, version); s != "discarded" {
			t.Fatalf("candidate after the retry = %s, want discarded", s)
		}
	})
}

// deadlineProvider records the deadline of each provider call's context.
type deadlineProvider struct {
	*testprovider.Provider
	deadlines chan time.Time
}

func (p deadlineProvider) ApplyCredential(ctx context.Context, env provider.Env, inst provider.Instance, name string, version int, value []byte) (provider.Rotation, error) {
	d, _ := ctx.Deadline() // zero without one
	p.deadlines <- d
	return p.Provider.ApplyCredential(ctx, env, inst, name, version, value)
}

// TestRotationLeaseExpiryLeavesUncertain: a call is bounded by a context
// that ends before its lease; when the lease expires with the worker gone,
// the candidate is still uncertain, and the job's retry resolves it.
func TestRotationLeaseExpiryLeavesUncertain(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	deadlines := make(chan time.Time, 4)
	reg, err := apps.NewRegistry(deadlineProvider{h.tp, deadlines})
	if err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	svc := apps.NewService(h.pool, reg, ring, apps.Config{})
	f := startInFlight(t, h, svc, inst)
	callDeadline := <-deadlines
	lease := leaseOf(t, h, f.id)
	// Same host in tests, so the two clocks may be compared here, with a
	// tolerance well inside the one-minute margin.
	if callDeadline.IsZero() || !callDeadline.Before(lease.Add(-30*time.Second)) {
		t.Fatalf("the call's deadline %v does not end before its lease %v", callDeadline, lease)
	}
	f.terminate(t, h)
	if err = f.finish(t); err == nil {
		t.Fatal("a worker without its connection reported success")
	}
	expireLeases(h, inst)
	if s := h.credState(inst, f.version); s != "uncertain" {
		t.Fatalf("candidate after an expired lease = %s, want uncertain", s)
	}
	if err = h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", []byte("test-only-direct")); !errors.Is(err, apps.ErrRotationInProgress) {
		t.Fatalf("direct replacement of the uncertain candidate = %v", err)
	}
	if err = h.rotate(inst, f.version); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, f.version); s != "active" {
		t.Fatalf("candidate after the retry = %s, want active", s)
	}
}

// TestAbandonWaitsForInFlightCall: an abandon re-apply must not run while
// a forward call may still install the candidate after it: it waits for the
// lease, so the provider ends up with the active value.
func TestAbandonWaitsForInFlightCall(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	f := startInFlight(t, h, h.svc, inst)
	f.terminate(t, h)
	if err := h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	abandon := apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: f.version, CredentialID: f.id, Abandon: true}
	calls := len(h.tp.Calls())
	if err := h.svc.RunRotateCredential(h.ctx, abandon); err == nil {
		t.Fatal("the re-apply ran while the forward call was in flight")
	}
	if len(h.tp.Calls()) != calls || h.credState(inst, f.version) != "abandoning" {
		t.Fatal("the deferred re-apply contacted the provider or changed the candidate")
	}
	// The forward call lands at the provider after all.
	if err := f.finish(t); err == nil {
		t.Fatal("a worker without its connection reported success")
	}
	expireLeases(h, inst)
	if err := h.svc.RunRotateCredential(h.ctx, abandon); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, f.version); s != "discarded" {
		t.Fatalf("candidate = %s, want discarded", s)
	}
	if !bytes.Equal(h.tp.Secret(inst.ID, "webhook_secret"), old) {
		t.Fatal("the provider does not use the active value after abandon")
	}
}
