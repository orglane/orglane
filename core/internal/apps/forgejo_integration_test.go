//go:build integration

package apps_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/agent"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/forgejo"
	"github.com/orglane/orglane/providers/provider"
)

const agentSecret = "integration-agent-secret-0123456789abcdef" //nolint:gosec // a test secret

// startAgent runs the real agent in-process against the bundle.
func startAgent(t *testing.T, b *managedtest.Bundle, registry *packages.Registry) *agentclient.Client {
	t.Helper()
	client, _ := startAgentAt(t, b, registry)
	return client
}

// startAgentAt is startAgent and also returns the socket the core uses.
func startAgentAt(t *testing.T, b *managedtest.Bundle, registry *packages.Registry) (*agentclient.Client, string) {
	t.Helper()
	return startAgentAtLogger(t, b, registry, slog.New(slog.DiscardHandler))
}

// startAgentAtLogger is startAgentAt with a logger, so a scenario can show
// the agent's install errors when it fails.
func startAgentAtLogger(t *testing.T, b *managedtest.Bundle, registry *packages.Registry, logger *slog.Logger) (*agentclient.Client, string) {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "apps-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	cfg := config.Default().Agent
	cfg.Secret = agentSecret
	cfg.Listen = filepath.Join(sockDir, "agent.sock")
	cfg.DataDir = t.TempDir()
	cfg.PeerUID = -1
	cfg.ComposeProject = b.Project
	cfg.EdgeNetwork, cfg.ServicesNetwork = b.Project+"_edge", b.Project+"_services"
	cfg.ReconcileInterval = time.Hour
	a, err := agent.New(agent.Options{Config: cfg, Version: "dev", Registry: registry, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent: %v", err)
		}
	})
	managedtest.Eventually(t, 10*time.Second, "the agent listens", func() bool {
		c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", cfg.Listen)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})
	client, err := agentclient.New(cfg.Listen, agentSecret, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, cfg.Listen
}

// The organization is fixed once the instance exists, so the core refuses
// names Forgejo would refuse before anything is stored (the adapter's
// SettingsValidator).
func TestForgejoOrganizationSetting(t *testing.T) {
	h := newHarnessWith(t, apps.Config{}, forgejo.New())
	for _, bad := range []string{"acme-", "acme..org", "Explore", "orglane-admin", "team.keys"} {
		_, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
			Provider: forgejo.ID, Key: "git", BaseURL: "https://git.acme.test",
			Settings: map[string]any{forgejo.SettingOrganization: bad},
		})
		if apps.Code(err) != "invalid_input" || !strings.Contains(err.Error(), "settings.organization") {
			t.Errorf("%q: %v, want invalid settings.organization", bad, err)
		}
	}
	// Omitted or null: refused too. Stored, the instance could never get
	// its organization (immutable) and Setup would fail on every retry.
	for name, settings := range map[string]map[string]any{"omitted": nil, "null": {forgejo.SettingOrganization: nil}} {
		_, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
			Provider: forgejo.ID, Key: "git", BaseURL: "https://git.acme.test", Settings: settings,
		})
		if apps.Code(err) != "invalid_input" || !strings.Contains(err.Error(), "settings.organization") {
			t.Errorf("organization %s: %v, want invalid settings.organization", name, err)
		}
	}
	var n int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM app_instances`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("instances stored: %d, %v", n, err)
	}
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, Key: "git", BaseURL: "https://git.acme.test",
		Settings: map[string]any{forgejo.SettingOrganization: "Acme.Robotics"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// It can never be removed or changed afterwards.
	for _, v := range []any{nil, "Other"} {
		if _, err := h.svc.UpdateInstance(h.ctx, audit.System, inst.ID, apps.InstanceUpdate{Settings: map[string]any{forgejo.SettingOrganization: v}}); apps.Code(err) != "invalid_input" {
			t.Errorf("update organization to %v: %v", v, err)
		}
	}
	// Mutable settings still validate the whole set on update.
	if _, err := h.svc.UpdateInstance(h.ctx, audit.System, inst.ID, apps.InstanceUpdate{Settings: map[string]any{forgejo.SettingSSHPort: 2422}}); err != nil {
		t.Fatalf("update ssh_port: %v", err)
	}
}

// Creating a relying-party instance registers its OIDC client and stores
// the secret in the same transaction: both exist, or neither.
func TestForgejoOIDCClientRegistration(t *testing.T) {
	h := newHarnessWith(t, apps.Config{}, forgejo.New())
	settings := map[string]any{forgejo.SettingOrganization: "acme"}
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, Key: "git", BaseURL: "https://git.acme.test", Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := h.ids.OIDCClient(h.ctx, "app-git")
	if err != nil {
		t.Fatal(err)
	}
	if client.Name != "Forgejo" || client.AuthMethod != "client_secret_basic" || len(client.RedirectUris) != 1 ||
		client.RedirectUris[0] != "https://git.acme.test/user/oauth2/orglane/callback" {
		t.Fatalf("client = %+v", client)
	}
	secret, err := h.svc.EnvForTest(inst).Credential(h.ctx, forgejo.CredentialOIDCClientSecret)
	if err != nil || !strings.HasPrefix(string(secret), "olcs_") {
		t.Fatalf("the client secret credential: %v", err)
	}
	var audited int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM audit_entries WHERE action = 'oidc_client.created' AND target_id = 'app-git'`).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("audit entries of the client: %d, %v", audited, err)
	}

	// Local mode: http redirect URIs on the local service domain.
	if _, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, Key: "git2", BaseURL: "http://git2.orglane.localhost:8080", Settings: settings,
	}); err != nil {
		t.Fatalf("local mode: %v", err)
	}
	if c, err := h.ids.OIDCClient(h.ctx, "app-git2"); err != nil || c.RedirectUris[0] != "http://git2.orglane.localhost:8080/user/oauth2/orglane/callback" {
		t.Fatalf("local client = %+v, %v", c, err)
	}

	// Refused by identity (plain http elsewhere, or the client exists): no
	// instance, no credentials.
	for key, base := range map[string]string{"git3": "http://git3.acme.test", "git4": "https://git4.acme.test"} {
		if key == "git4" {
			if _, _, err := h.ids.CreateOIDCClient(h.ctx, audit.System, identity.NewOIDCClient{ID: "app-git4", AppInstance: true, Name: "Taken", RedirectURIs: []string{"https://x.acme.test/cb"}}); err != nil {
				t.Fatal(err)
			}
		}
		_, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: forgejo.ID, Key: key, BaseURL: base, Settings: settings})
		if apps.Code(err) != "invalid_input" || !strings.Contains(err.Error(), "oidc_client") {
			t.Errorf("%s: %v, want invalid oidc_client", base, err)
		}
		var n int
		if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM app_instances WHERE key = $1`, key).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d instances stored", key, n)
		}
	}
	var stray int
	if err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM oidc_clients WHERE id = 'app-git3'`).Scan(&stray); err != nil || stray != 0 {
		t.Fatalf("a client without an instance: %d", stray)
	}

	// Without a registrar a relying party cannot be installed.
	reg, err := apps.NewRegistry(forgejo.New())
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	bare := apps.NewService(h.pool, reg, keyring, apps.Config{})
	if _, err := bare.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: forgejo.ID, Key: "git5", BaseURL: "https://git5.acme.test", Settings: settings}); apps.Code(err) != "not_supported" {
		t.Fatalf("without a registrar: %v", err)
	}
}

// OIDCAccess denies every app client ("app-" prefix) it cannot decide for,
// and allows only people the instance provisioned while it is active.
// Operator clients (no prefix) are not its business.
func TestOIDCAccess(t *testing.T) {
	h := newHarnessWith(t, apps.Config{}, forgejo.New())
	settings := map[string]any{forgejo.SettingOrganization: "acme"}
	newInstance := func(key string) apps.Instance {
		t.Helper()
		inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: forgejo.ID, Key: key, BaseURL: "https://" + key + ".acme.test", Settings: settings})
		if err != nil {
			t.Fatal(err)
		}
		return inst
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.pool.Exec(h.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	provision := func(inst apps.Instance, p store.Person, method string, access bool) {
		t.Helper()
		var on any = inst.ID
		if method != provider.LinkProvisioned {
			on = nil
		}
		exec(`INSERT INTO account_links (person_id, provider, realm, external_id, login, method, instance_id) VALUES ($1, 'forgejo', $2, $3, $4, $5, $6)`,
			p.ID, inst.ID.String(), p.Username, p.Username, method, on)
		exec(`INSERT INTO provisioning_results (instance_id, person_id, status, access) VALUES ($1, $2, 'done', $3)`, inst.ID, p.ID, access)
	}
	git := newInstance("git")
	exec(`UPDATE app_instances SET state = 'active' WHERE id = $1`, git.ID)
	maya, chen, aiko, lucas, nora := h.person("maya"), h.person("chen"), h.person("aiko"), h.person("lucas"), h.person("nora")
	provision(git, maya, provider.LinkProvisioned, true)
	provision(git, chen, provider.LinkOAuth, true)
	provision(git, aiko, provider.LinkProvisioned, false)
	exec(`INSERT INTO account_links (person_id, provider, realm, external_id, login, method, instance_id) VALUES ($1, 'forgejo', $2, 'lucas', 'lucas', 'provisioned', $3)`, lucas.ID, git.ID.String(), git.ID)

	// Other instances in the states that cannot be decided.
	installing := newInstance("installing")
	retained, purged := newInstance("retained"), newInstance("purged")
	for _, inst := range []apps.Instance{installing, retained, purged} {
		provision(inst, maya, provider.LinkProvisioned, true)
	}
	exec(`UPDATE app_instances SET state = 'removed', data_retained = true, removed_at = now() WHERE id = $1`, retained.ID)
	exec(`UPDATE app_instances SET state = 'removed', data_retained = false, removed_at = now() WHERE id = $1`, purged.ID)
	// An instance of a provider that is no relying party, under a key whose
	// client ID is reserved.
	other, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "testmanaged", Key: "other", BaseURL: "https://other.acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE app_instances SET state = 'active' WHERE id = $1`, other.ID)

	for _, c := range []struct {
		name   string
		svc    *apps.Service
		client string
		person store.Person
		want   bool
	}{
		{"operator client", h.svc, "mobile", nora, true},
		{"provisioned, active", h.svc, "app-git", maya, true},
		{"no link, no result", h.svc, "app-git", nora, false},
		{"oauth link", h.svc, "app-git", chen, false},
		{"access withdrawn", h.svc, "app-git", aiko, false},
		{"link without a result", h.svc, "app-git", lucas, false},
		{"another instance's link", h.svc, "app-installing", nora, false},
		{"installing", h.svc, "app-installing", maya, false},
		{"removed, data retained", h.svc, "app-retained", maya, false},
		{"removed and purged", h.svc, "app-purged", maya, false},
		{"no instance for the key", h.svc, "app-unknown", maya, false},
		{"no relying party", h.svc, "app-other", maya, false},
		{"adapter not in this build", bareService(t, h), "app-git", maya, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.svc.OIDCAccess(h.ctx, c.client, c.person.ID)
			if err != nil || got != c.want {
				t.Fatalf("OIDCAccess(%s, %s) = %v, %v; want %v", c.client, c.person.Username, got, err, c.want)
			}
		})
	}
	// A store error is no decision: it is returned (the callers deny).
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	if ok, err := h.svc.OIDCAccess(ctx, "app-git", maya.ID); ok || err == nil {
		t.Fatalf("with a failing store: %v, %v", ok, err)
	}
}

// bareService is the framework on the harness's database without the
// Forgejo adapter (a build that does not have it).
func bareService(t *testing.T, h *harness) *apps.Service {
	t.Helper()
	reg, err := apps.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	return apps.NewService(h.pool, reg, keyring, apps.Config{})
}

// The decision in an issuing transaction holds share locks on the rows it
// read: a withdrawal waits until the issuance commits, so it is ordered
// before or after it, never in between.
func TestOIDCAccessTxSerializesWithdrawal(t *testing.T) {
	h := newHarnessWith(t, apps.Config{}, forgejo.New())
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, Key: "git", BaseURL: "https://git.acme.test", Settings: map[string]any{forgejo.SettingOrganization: "acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	maya := h.person("maya")
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE app_instances SET state = 'active' WHERE id = $1`, []any{inst.ID}},
		{`INSERT INTO account_links (person_id, provider, realm, external_id, login, method, instance_id) VALUES ($1, 'forgejo', $2, '7', 'maya', 'provisioned', $3)`, []any{maya.ID, inst.ID.String(), inst.ID}},
		{`INSERT INTO provisioning_results (instance_id, person_id, status, access) VALUES ($1, $2, 'done', true)`, []any{inst.ID, maya.ID}},
	} {
		if _, err := h.pool.Exec(h.ctx, st.sql, st.args...); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(h.ctx) }()
	if ok, err := h.svc.OIDCAccessTx(h.ctx, store.New(tx), "app-git", maya.ID); !ok || err != nil {
		t.Fatalf("access = %v, %v", ok, err)
	}
	for name, sql := range map[string]string{
		"access withdrawn": `UPDATE provisioning_results SET access = false WHERE instance_id = $1 AND person_id = $2`,
		"link revoked":     `UPDATE account_links SET state = 'revoked', revoked_at = now() WHERE instance_id = $1 AND person_id = $2`,
		"instance changed": `UPDATE app_instances SET state = 'degraded' WHERE id = $1 AND $2::uuid IS NOT NULL`,
	} {
		conn, err := h.pool.Acquire(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = conn.Exec(h.ctx, "SET lock_timeout = '300ms'")
		if err == nil {
			_, err = conn.Exec(h.ctx, sql, inst.ID, maya.ID)
		}
		_, _ = conn.Exec(h.ctx, "RESET lock_timeout")
		conn.Release()
		if err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Fatalf("%s while an issuance decided: %v, want it to wait for the lock", name, err)
		}
	}
	if err := tx.Commit(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `UPDATE provisioning_results SET access = false WHERE instance_id = $1 AND person_id = $2`, inst.ID, maya.ID); err != nil {
		t.Fatalf("withdrawal after the issuance: %v", err)
	}
}
