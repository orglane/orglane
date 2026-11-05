//go:build integration

package apps_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	githubprovider "github.com/orglane/orglane/providers/github"
)

// TestGitHubRotationMinimumPool: with database.max_conns=1, the real GitHub
// adapter's rotation paths that end before any network call complete on the
// worker's lock connection: a malformed uploaded key is rejected and
// discarded; a webhook secret rotation reads the (invalid) App key through
// Env, stays uncertain, is reported stuck once, and its abandon keeps
// retrying. No GitHub API is contacted or replaced.
func TestGitHubRotationMinimumPool(t *testing.T) {
	h := newHarness(t)
	pool := sizedPool(t, h, 1)
	reg, err := apps.NewRegistry(githubprovider.New())
	if err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	svc := apps.NewService(pool, reg, ring, apps.Config{Now: func() time.Time { return time.Now().Add(25 * time.Hour) }})
	queue, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetQueue(queue)
	inst, err := svc.CreateInstance(bounded(t), audit.System, apps.NewInstance{Provider: "github", Key: "github-rotation", State: apps.StateActive, Settings: map[string]any{"organization": "orglane-test", "app_id": "456"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.StoreCredential(bounded(t), audit.System, inst.ID, "app_private_key", []byte("invalid-local-fixture")); err != nil {
		t.Fatal(err)
	}
	if err = svc.StoreCredential(bounded(t), audit.System, inst.ID, "webhook_secret", fixtureSecret); err != nil {
		t.Fatal(err)
	}
	state := func(name string, version int32) string {
		t.Helper()
		var s string
		if err := h.pool.QueryRow(h.ctx, `SELECT state FROM app_credentials WHERE instance_id = $1 AND name = $2 AND version = $3`, inst.ID, name, version).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	run := func(name string, version int32, abandon bool) error {
		ctx := bounded(t)
		err := svc.RunRotateCredential(ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: name, Version: version, Abandon: abandon})
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("the worker waited on the pool it holds: %v", err)
		}
		return err
	}

	if err = svc.UploadCredential(bounded(t), audit.System, inst.ID, "app_private_key", []byte("malformed-replacement-fixture")); err != nil {
		t.Fatal(err)
	}
	if err = run("app_private_key", 2, false); err != nil {
		t.Fatalf("a locally rejected upload must complete: %v", err)
	}
	if s := state("app_private_key", 2); s != "discarded" {
		t.Fatalf("rejected upload = %s, want discarded", s)
	}

	version, err := svc.RotateCredential(bounded(t), audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err = run("webhook_secret", version, false); err == nil {
		t.Fatal("an App key that cannot sign must leave the outcome unknown")
	}
	if s := state("webhook_secret", version); s != "uncertain" {
		t.Fatalf("webhook secret candidate = %s, want uncertain", s)
	}
	if n := h.auditCount(audit.AppCredentialRotationStuck); n != 1 {
		t.Fatalf("rotation_stuck recorded %d times, want once", n)
	}
	if err = svc.AbandonRotation(bounded(t), audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	expireLeases(h, inst)
	if err = run("webhook_secret", version, true); err == nil {
		t.Fatal("abandon must keep re-applying until GitHub acknowledges the active secret")
	}
	if s := state("webhook_secret", version); s != "abandoning" {
		t.Fatalf("abandoned candidate = %s, want abandoning until re-applied", s)
	}
	if n := h.auditCount(audit.AppCredentialRotationStuck); n != 1 {
		t.Fatalf("rotation_stuck recorded %d times after abandon, want once per candidate", n)
	}
}
