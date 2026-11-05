//go:build integration

package apps_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/providers/provider"
)

func (h *harness) secret(inst apps.Instance, name string) []byte {
	h.t.Helper()
	v, err := h.svc.EnvForTest(inst).Credential(h.ctx, name)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func (h *harness) credState(inst apps.Instance, version int32) string {
	h.t.Helper()
	var s string
	if err := h.pool.QueryRow(h.ctx, `SELECT state FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND version = $2`, inst.ID, version).Scan(&s); err != nil {
		h.t.Fatalf("webhook_secret v%d: %v", version, err)
	}
	return s
}

func (h *harness) rotate(inst apps.Instance, version int32) error {
	return h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version})
}

// retry is the job's next attempt as River runs it after an unknown
// outcome: once the earlier execution's lease has ended (at most one
// execution per credential).
func (h *harness) retry(inst apps.Instance, version int32) error {
	expireLeases(h, inst)
	return h.rotate(inst, version)
}

// TestCredentialSealing: credentials are sealed under their own purpose and
// bound to their row; they never appear in plaintext.
func TestCredentialSealing(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "api_token", []byte("token-v2-plaintext")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.secret(inst, "api_token"), []byte("token-v2-plaintext")) {
		t.Fatal("the active credential does not open")
	}
	if h.count(`SELECT count(*) FROM app_credentials WHERE position('token-v2-plaintext'::bytea in ciphertext) > 0`) != 0 {
		t.Fatal("a credential is stored in plaintext")
	}
	// Moving a ciphertext to another row (version) makes it unreadable.
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "api_token", []byte("token-v3")); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE app_credentials SET ciphertext = (SELECT ciphertext FROM app_credentials WHERE instance_id = $1 AND name = 'api_token' AND version = 1)
		WHERE instance_id = $1 AND name = 'api_token' AND version = 2`, inst.ID)
	if _, err := h.svc.EnvForTest(inst).Credential(h.ctx, "api_token"); !errors.Is(err, crypto.ErrDecrypt) {
		t.Fatalf("a swapped ciphertext opened: %v", err)
	}
	// Purpose isolation: with the row's own additional data, the
	// credentials purpose opens the ciphertext and another purpose does not.
	var ct []byte
	var version int32
	if err := h.pool.QueryRow(h.ctx, `SELECT ciphertext, version FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND state = 'active'`, inst.ID).Scan(&ct, &version); err != nil {
		t.Fatal(err)
	}
	k, _ := crypto.NewKeyring(masterSecret)
	ad := apps.CredentialADForTest(inst.ID, "webhook_secret", version)
	if _, err := k.Sealer(crypto.PurposeAppCredentials).Open(ct, ad); err != nil {
		t.Fatalf("the credentials purpose does not open its own row: %v", err)
	}
	if _, err := k.Sealer(crypto.PurposeEmailSettings).Open(ct, ad); err == nil {
		t.Fatal("another purpose opened a credential")
	}
	// Audit metadata names credentials, never values.
	if h.count(`SELECT count(*) FROM audit_entries WHERE metadata::text LIKE '%token-v%'`) != 0 {
		t.Fatal("a credential value reached the audit log")
	}
}

// TestRotationResponseLost: the provider commits the new secret but the
// answer is lost. Across retries and a worker restart the candidate stays
// uncertain, both secrets verify deliveries, and re-applying makes it
// active.
func TestRotationResponseLost(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	h.tp.LoseResponses("webhook_secret", 3)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil || version != 2 {
		t.Fatalf("rotate = %d, %v", version, err)
	}
	if _, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret"); !errors.Is(err, apps.ErrRotationInProgress) {
		t.Fatalf("second rotation = %v", err)
	}
	for range 2 {
		if err := h.retry(inst, version); err == nil {
			t.Fatal("an unknown answer did not ask for a retry")
		}
		if s := h.credState(inst, version); s != "uncertain" {
			t.Fatalf("candidate = %s, want uncertain", s)
		}
		if !bytes.Equal(h.secret(inst, "webhook_secret"), old) {
			t.Fatal("the active secret changed before the provider acknowledged")
		}
	}
	// Both the old and the committed new secret verify deliveries.
	newSecret := h.tp.Secret(inst.ID, "webhook_secret")
	if bytes.Equal(newSecret, old) || len(newSecret) == 0 {
		t.Fatal("the provider did not commit the new secret")
	}
	for i, secret := range [][]byte{old} {
		body := prBody("acme", 100+i)
		if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(secret, body, string(rune('a'+i)), "pull_request"), body); err != nil {
			t.Fatalf("delivery signed with the old secret: %v", err)
		}
	}
	if s := h.credState(inst, version); s != "uncertain" {
		t.Fatal("a delivery signed with the active secret proved the candidate")
	}

	// A worker restart: a fresh service on the same database.
	keyring, _ := crypto.NewKeyring(masterSecret)
	reg, _ := apps.NewRegistry(h.tp)
	ws, _ := url.Parse("https://work.acme.test")
	restarted := apps.NewService(h.pool, reg, keyring, apps.Config{WorkspaceURL: ws, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	expireLeases(h, inst)
	if err := restarted.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version}); err == nil {
		t.Fatal("the third lost answer was taken as success")
	}
	expireLeases(h, inst)
	if err := restarted.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version}); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, version); s != "active" {
		t.Fatalf("candidate = %s, want active", s)
	}
	if s := h.credState(inst, 1); s != "retiring" {
		t.Fatalf("old version = %s, want retiring", s)
	}
	if !bytes.Equal(h.secret(inst, "webhook_secret"), newSecret) {
		t.Fatal("the active secret is not the one the provider uses")
	}
	// The retiring version still verifies during the overlap.
	body := prBody("acme", 200)
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(old, body, "overlap", "pull_request"), body); err != nil {
		t.Fatalf("the retiring secret no longer verifies: %v", err)
	}
	if h.auditCount(audit.AppCredentialRotated) != 1 {
		t.Fatal("activation not audited")
	}
	// Cleanup keeps it until retire_at; then it no longer verifies, but is
	// kept for a week to detect drift, and deleted after.
	if _, err := h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND version = 1`, inst.ID) != 1 {
		t.Fatal("a retiring version was deleted before retire_at")
	}
	h.exec(`UPDATE app_credentials SET retire_at = now() - interval '1 minute' WHERE state = 'retiring'`)
	if _, err := h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND version = 1`, inst.ID) != 1 {
		t.Fatal("a retired version was deleted before drift detection ends")
	}
	body = prBody("acme", 201)
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(old, body, "after-overlap", "pull_request"), body); !errors.Is(err, apps.ErrWebhookSignature) {
		t.Fatalf("a retired secret verified after its overlap: %v", err)
	}
	h.exec(`UPDATE app_credentials SET retire_at = now() - interval '8 days' WHERE state = 'retiring'`)
	if _, err := h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND version = 1`, inst.ID) != 0 {
		t.Fatal("a retired version was kept past drift detection")
	}
}

// TestRotationRejected: only a proven rejection discards the candidate.
func TestRotationRejected(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	h.tp.RotationOutcomes("webhook_secret", provider.RotationRejected)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	h.drain()
	if s := h.credState(inst, version); s != "discarded" {
		t.Fatalf("candidate = %s", s)
	}
	if !bytes.Equal(h.secret(inst, "webhook_secret"), old) || h.auditCount(audit.AppCredentialRotationFailed) != 1 {
		t.Fatal("a rejection touched the active version or was not audited")
	}
	// A rotation can start again.
	if _, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
}

// TestRotationAbandon: abandoning re-applies the active value until the
// provider acknowledges it, and only then discards the candidate.
func TestRotationAbandon(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	h.tp.LoseResponses("webhook_secret", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.rotate(inst, version); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	h.exec(`DELETE FROM river_job WHERE kind = $1`, apps.RotateCredentialArgs{}.Kind()) // stop the normal retries
	if err := h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	abandon := apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: true}
	// The lost forward call may still land at the provider: the re-apply
	// waits for its lease.
	calls := len(h.tp.Calls())
	if err := h.svc.RunRotateCredential(h.ctx, abandon); err == nil || len(h.tp.Calls()) != calls {
		t.Fatalf("the re-apply did not wait for the forward call's lease: %v", err)
	}
	expireLeases(h, inst)
	h.tp.LoseResponses("webhook_secret", 1) // the first re-apply's answer is lost too
	if err := h.svc.RunRotateCredential(h.ctx, abandon); err == nil {
		t.Fatal("abandon discarded before the active value was acknowledged")
	}
	if s := h.credState(inst, version); s != "abandoning" {
		t.Fatalf("candidate = %s, want abandoning (still verifying, not discarded)", s)
	}
	expireLeases(h, inst) // the re-apply's answer was lost; its retry follows the lease
	if err := h.svc.RunRotateCredential(h.ctx, abandon); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, version); s != "discarded" {
		t.Fatalf("candidate = %s, want discarded", s)
	}
	if !bytes.Equal(h.tp.Secret(inst.ID, "webhook_secret"), old) {
		t.Fatal("the provider does not use the active secret after abandoning")
	}
	if err := h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); !errors.Is(err, apps.ErrNoRotation) {
		t.Fatalf("abandon without rotation = %v", err)
	}
}

// TestRotationProofByUse: a delivery that verifies only with the uncertain
// secret proves the provider uses it.
func TestRotationProofByUse(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.tp.LoseResponses("webhook_secret", 5)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.rotate(inst, version); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	body := prBody("acme", 300)
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(h.tp.Secret(inst.ID, "webhook_secret"), body, "proof", "pull_request"), body); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, version); s != "active" {
		t.Fatalf("candidate = %s, want active (proof by use)", s)
	}
	var proof string
	if err := h.pool.QueryRow(h.ctx, `SELECT metadata->>'proof' FROM audit_entries WHERE action = 'app_credential.rotated'`).Scan(&proof); err != nil || proof != "use" {
		t.Fatalf("proof = %q, %v", proof, err)
	}
	// The pending job finds nothing left to do.
	if err := h.rotate(inst, version); err != nil {
		t.Fatal(err)
	}
}

// TestRotationMintedAndStuck: provider-minted credentials store the value
// the provider returns; a candidate uncertain for a day is reported once.
func TestRotationMintedAndStuck(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "api_token", []byte("first")); err != nil {
		t.Fatal(err)
	}
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "api_token")
	if err != nil {
		t.Fatal(err)
	}
	h.drain()
	if got := h.secret(inst, "api_token"); string(got) != "minted-2" || version != 2 {
		t.Fatalf("api_token = %q (v%d)", got, version)
	}

	h.tp.LoseResponses("webhook_secret", 100)
	v, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.rotate(inst, v)
	h.exec(`UPDATE app_credentials SET created_at = now() - interval '25 hours' WHERE state = 'uncertain'`)
	for range 2 {
		if _, err := h.svc.Cleanup(h.ctx); err != nil {
			t.Fatal(err)
		}
		_ = h.rotate(inst, v)
	}
	if h.auditCount(audit.AppCredentialRotationStuck) != 1 {
		t.Fatalf("rotation_stuck recorded %d times, want once", h.auditCount(audit.AppCredentialRotationStuck))
	}
	creds, err := h.svc.Credentials(h.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !apps.RotationStuck(creds, time.Now()) {
		t.Fatal("the instance does not show rotation_stuck")
	}
	if s := h.credState(inst, v); s != "uncertain" {
		t.Fatalf("a stuck candidate was resolved automatically: %s", s)
	}
	// Uploads and unknown credentials are not rotated by Orglane.
	if _, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "nope"); !errors.Is(err, apps.ErrNotFound) {
		t.Fatalf("unknown credential = %v", err)
	}
}
