//go:build integration

package apps_test

import (
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
)

// TestForgejoPurgeReleasesOIDCClient: a Forgejo instance's OIDC client
// app-<key> belongs to the key, so a purge (which releases the key) must
// delete it with its grants and tokens; a retained removal and a reinstall
// keep it. Without the deletion a fresh install under the same key fails to
// register its client ("a client with this ID exists").
func TestForgejoPurgeReleasesOIDCClient(t *testing.T) {
	registry, err := packages.NewRegistry(forgejopkg.FS)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, apps.Config{Packages: registry}, forgejo.New())
	ctx := h.ctx
	settings := map[string]any{forgejo.SettingOrganization: "acme"}

	install := func() apps.Instance {
		t.Helper()
		inst, err := h.svc.Install(ctx, audit.System, apps.NewInstance{
			Provider: forgejo.ID, Key: "git", BaseURL: "https://git.acme.test", Settings: settings,
		})
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		return inst
	}
	// finish ends an operation as the agent's answer does, then activation
	// marks the instance active (Setup is not needed for removal states).
	finish := func(inst apps.Instance, gen int64) {
		t.Helper()
		if err := h.svc.FinishForTest(ctx, inst.ID, gen, agentapi.OpSucceeded, ""); err != nil {
			t.Fatalf("finish generation %d: %v", gen, err)
		}
	}
	activate := func(inst apps.Instance, gen int64) {
		t.Helper()
		finish(inst, gen)
		if _, err := h.svc.Transition(ctx, audit.System, inst.ID, apps.StateActive, ""); err != nil {
			t.Fatalf("activate: %v", err)
		}
	}
	const providerClientID = "app-git"
	client := func() {
		t.Helper()
		if _, err := h.ids.OIDCClient(ctx, providerClientID); err != nil {
			t.Fatalf("OIDC client %s: %v", providerClientID, err)
		}
	}

	inst := install()
	activate(inst, 1)
	client()

	// OIDC state issued through the client: a purge must remove it too.
	alice := h.person("alice")
	grant := uuid.Must(uuid.NewV7())
	if _, err := h.pool.Exec(ctx, `INSERT INTO oidc_grants (id, client_id, person_id) VALUES ($1, $2, $3)`,
		grant, providerClientID, alice.ID); err != nil {
		t.Fatal(err)
	}
	refresh := sha256.Sum256([]byte("refresh-token"))
	if _, err := h.pool.Exec(ctx, `INSERT INTO oidc_refresh_tokens
			(token_hash, grant_id, client_id, person_id, scopes, audience, auth_time, expires_at)
		VALUES ($1, $2, $3, $4, '{openid}', '{orglane}', now(), now() + interval '1 hour')`,
		refresh[:], grant, providerClientID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO oidc_access_tokens
			(client_id, person_id, grant_id, scopes, audience, expires_at)
		VALUES ($1, $2, $3, '{openid}', '{orglane}', now() + interval '1 hour')`,
		providerClientID, alice.ID, grant); err != nil {
		t.Fatal(err)
	}

	// A retained removal keeps the client: the reinstall uses it again.
	if _, err := h.svc.Remove(ctx, audit.System, inst.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	finish(inst, 2)
	if got := h.reload(inst.ID); got.State != apps.StateRemoved || !got.DataRetained {
		t.Fatalf("retained removal: %+v", got.AppInstance)
	}
	client()
	if _, err := h.svc.Reinstall(ctx, audit.System, inst.ID); err != nil {
		t.Fatal(err)
	}
	activate(inst, 3)
	client()

	// A successful purge deletes the client and its OIDC state in the
	// purge's transaction, so the released key can be installed again:
	// before the deletion this install failed with invalid_input
	// oidc_client ("a client with this ID exists").
	if _, err := h.svc.Remove(ctx, audit.System, inst.ID, true, inst.Key); err != nil {
		t.Fatal(err)
	}
	finish(inst, 4)
	if got := h.reload(inst.ID); got.State != apps.StateRemoved || got.DataRetained {
		t.Fatalf("purge: %+v", got.AppInstance)
	}
	again := install()
	if again.ID == inst.ID || again.Key != inst.Key {
		t.Fatalf("fresh install under the purged key: %+v", again.AppInstance)
	}
	client()
	// The old state went with the client; the new client has none of it.
	for table := range map[string]struct{}{
		"oidc_grants": {}, "oidc_refresh_tokens": {}, "oidc_access_tokens": {},
	} {
		var n int
		if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE client_id = $1", providerClientID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s rows after the purge = %d", table, n)
		}
	}
	var deleted, created int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action = 'oidc_client.deleted'),
			count(*) FILTER (WHERE action = 'oidc_client.created')
		FROM audit_entries WHERE target_id = $1`, providerClientID).Scan(&deleted, &created); err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || created != 2 {
		t.Fatalf("oidc_client audit entries: %d deleted, %d created", deleted, created)
	}
}
