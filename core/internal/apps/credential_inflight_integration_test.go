//go:build integration

package apps_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
)

// TestStoreCredentialDuringInFlightCall: a retry's provider call is still in
// flight when proof by use activates its candidate. A direct replacement
// must be refused until the call returned, or the call would overwrite the
// replacement at the provider after it committed.
func TestStoreCredentialDuringInFlightCall(t *testing.T) {
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
	installed := h.tp.Secret(inst.ID, "webhook_secret")
	expireLeases(h, inst) // the retry runs after the lost execution's lease
	reached, release := h.tp.Block("rotate:webhook_secret")
	ctx := bounded(t)
	retry := make(chan error, 1)
	go func() {
		retry <- h.svc.RunRotateCredential(ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version})
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("the retry did not reach the provider")
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	body := prBody("acme", 845)
	if _, err = h.svc.ReceiveWebhook(ctx, inst.ID, delivery(installed, body, "in-flight-proof", "pull_request"), body); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, version); s != "active" {
		t.Fatalf("candidate = %s, want active (proof by use)", s)
	}
	direct := []byte("test-only-direct-replacement")
	before := h.auditCount(audit.AppCredentialCreated)
	if err = h.svc.StoreCredential(ctx, audit.System, inst.ID, "webhook_secret", direct); !errors.Is(err, apps.ErrRotationInProgress) {
		t.Fatalf("direct replacement during an in-flight provider call = %v, want rotation_in_progress", err)
	}
	if h.auditCount(audit.AppCredentialCreated) != before || !bytes.Equal(h.secret(inst, "webhook_secret"), installed) {
		t.Fatal("the refused replacement changed credentials or the audit log")
	}
	close(release)
	released = true
	if err = <-retry; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.secret(inst, "webhook_secret"), h.tp.Secret(inst.ID, "webhook_secret")) {
		t.Fatal("the active version and the provider's value differ")
	}
	// The retry ended its own execution with the answer (the first one's
	// lease had ended before it began): a direct replacement is allowed.
	if err = h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", direct); err != nil {
		t.Fatal(err)
	}
}

// fanoutProvider is the test provider whose credential calls and
// signature verification read another credential from many goroutines at
// once, as an adapter checking several values in parallel may.
type fanoutProvider struct{ *testprovider.Provider }

func fanoutReads(ctx context.Context, env provider.Env) error {
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			for range 8 {
				v, err := env.Credential(ctx, "api_token")
				clear(v)
				if err != nil {
					errs <- err
					return
				}
				vs, err := env.Credentials(ctx, "api_token")
				for _, v := range vs {
					clear(v)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	return <-errs
}

func (p fanoutProvider) ApplyCredential(ctx context.Context, env provider.Env, inst provider.Instance, name string, version int, value []byte) (provider.Rotation, error) {
	if err := fanoutReads(ctx, env); err != nil {
		return provider.Rotation{Outcome: provider.RotationUnknown}, err
	}
	return p.Provider.ApplyCredential(ctx, env, inst, name, version, value)
}

func (p fanoutProvider) Verify(ctx context.Context, env provider.Env, inst provider.Instance, h http.Header, body []byte) error {
	if err := fanoutReads(ctx, env); err != nil {
		return err
	}
	return p.Provider.Verify(ctx, env, inst, h, body)
}

// TestEnvConcurrentReads: an Env is safe for concurrent use over the pool,
// a held lock connection (rotation worker) and a transaction (proof by use
// while receiving a webhook). Run with -race.
func TestEnvConcurrentReads(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "api_token", []byte("test-only-token")); err != nil {
		t.Fatal(err)
	}
	reg, err := apps.NewRegistry(fanoutProvider{h.tp})
	if err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	pool := sizedPool(t, h, 2)
	svc := apps.NewService(pool, reg, ring, apps.Config{})
	queue, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetQueue(queue)
	h.tp.LoseResponses("webhook_secret", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	// Held connection: the worker's provider call.
	if err = svc.RunRotateCredential(bounded(t), apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version}); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	// Pool, then transaction: receipt verification and proof by use.
	body := prBody("acme", 846)
	if _, err = svc.ReceiveWebhook(bounded(t), inst.ID, delivery(h.tp.Secret(inst.ID, "webhook_secret"), body, "concurrent-env", "pull_request"), body); err != nil {
		t.Fatalf("receipt with concurrent Env reads: %v", err)
	}
	if s := h.credState(inst, version); s != "active" {
		t.Fatalf("candidate = %s, want active (proof by use)", s)
	}
}
