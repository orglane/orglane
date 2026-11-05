//go:build integration

package apps_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agent"
	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
)

const agentSecret = "integration-agent-secret-0123456789abcdef" //nolint:gosec // a test secret

// startAgent runs the real agent in-process against the bundle.
func startAgent(t *testing.T, b *managedtest.Bundle, registry *packages.Registry) *agentclient.Client {
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
	a, err := agent.New(agent.Options{Config: cfg, Version: "dev", Registry: registry, Logger: slog.New(slog.DiscardHandler)})
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
	return client
}

// TestForgejoAdminTokenRotation rotates the admin token of a real Forgejo
// through the framework (RotateCredential, AbandonRotation and the
// rotation job), so the adapter's answers are checked against what the
// core does with them (docs/specs/providers.md "Credentials"): the first
// version is minted; a second one is minted but its answer lost (uncertain);
// abandoning it keeps the active token working, deletes the candidate's
// token in Forgejo and mints nothing.
func TestForgejoAdminTokenRotation(t *testing.T) {
	if managedtest.RunAsRoot(t) {
		return
	}
	b := managedtest.StartBundle(t)
	registry, err := packages.NewRegistry(forgejopkg.FS)
	if err != nil {
		t.Fatal(err)
	}
	agentClient := startAgent(t, b, registry)
	httpClient, faults := managedtest.WithFaults(b.Client())
	h := newHarnessWith(t, apps.Config{HTTPClient: httpClient, Packages: registry}, forgejo.New())

	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, BaseURL: "http://" + managedtest.ForgejoHost,
		Settings: map[string]any{forgejo.SettingOrganization: "orglane"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inst.View().Upstream == nil || inst.View().Upstream.Host != "git-forgejo:3000" {
		t.Fatalf("upstream = %v", inst.View().Upstream)
	}
	// The agent installs it with the credentials the core generated.
	env := h.svc.EnvForTest(inst)
	secrets := map[string]string{}
	lifecycle, _ := inst.Adapter.Lifecycle()
	inputs, err := lifecycle.Inputs(h.ctx, env, inst.View())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range inputs.Secrets {
		v, err := env.Credential(h.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		secrets[name] = string(v)
	}
	project := packages.InstanceProject(b.Project, inst.Key)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", project, "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
	b.Routes(t, managedtest.ForgejoRoutes(inst.Key)...)
	spec := managedtest.ForgejoSpec(inst.ID, 1, managedtest.FreePort(t), secrets, agentapi.StateRunning)
	for k, v := range inputs.Settings {
		spec.Settings[k] = v
	}
	op, _, err := agentClient.Apply(t.Context(), inst.Key, spec)
	if err != nil {
		t.Fatal(err)
	}
	managedtest.Eventually(t, 5*time.Minute, "the install finishes", func() bool {
		op, err = agentClient.Operation(t.Context(), op.ID)
		return err == nil && op.Terminal()
	})
	if op.State != agentapi.OpSucceeded {
		t.Fatalf("install: %+v", op)
	}
	lc, _ := inst.Adapter.Lifecycle()
	if err := lc.Setup(h.ctx, env, inst.View()); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	tokens := func(name string) string {
		return b.Psql(t, "svc_git", "select count(*) from access_token where name = '"+name+"'")
	}
	works := func(token []byte) int {
		code, _ := managedtest.Request(t, b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/v1/user", managedtest.Auth{Token: string(token)}, nil)
		return code
	}
	state := func(version int32) string {
		var st string
		if err := h.pool.QueryRow(h.ctx, `SELECT state FROM app_credentials WHERE instance_id = $1 AND name = $2 AND version = $3`,
			inst.ID, forgejo.CredentialAdminToken, version).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	run := func(version int32, abandon bool) error {
		return h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: forgejo.CredentialAdminToken, Version: version, Abandon: abandon})
	}

	// Version 1: minted and active.
	v1, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, forgejo.CredentialAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(v1, false); err != nil {
		t.Fatalf("rotate to version 1: %v", err)
	}
	active, err := env.Credential(h.ctx, forgejo.CredentialAdminToken)
	if err != nil || state(v1) != "active" {
		t.Fatalf("version 1: %s, %v", state(v1), err)
	}
	if code := works(active); code != http.StatusOK {
		t.Fatalf("the active token: %d", code)
	}

	// Version 2: Forgejo mints it, the answer is lost.
	v2, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, forgejo.CredentialAdminToken)
	if err != nil {
		t.Fatal(err)
	}
	faults.Drop(http.MethodPost, "/api/v1/users/"+forgejo.AdminUsername+"/tokens")
	if err := run(v2, false); err == nil {
		t.Fatal("a lost answer was taken as an outcome")
	}
	if state(v2) != "uncertain" || tokens(forgejo.TokenName(int(v2))) != "1" {
		t.Fatalf("after the lost answer: %s, %s tokens", state(v2), tokens(forgejo.TokenName(int(v2))))
	}

	// Abandon: the job re-applies the active version, then discards.
	if err := h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, forgejo.CredentialAdminToken); err != nil {
		t.Fatal(err)
	}
	expireLeases(h, inst) // the lost mint can no longer be acted on
	if err := run(v2, true); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if state(v2) != "discarded" || state(v1) != "active" {
		t.Fatalf("after abandon: v1 %s, v2 %s", state(v1), state(v2))
	}
	still, err := env.Credential(h.ctx, forgejo.CredentialAdminToken)
	if err != nil || string(still) != string(active) {
		t.Fatalf("the core's active value changed: %v", err)
	}
	if code := works(still); code != http.StatusOK {
		t.Fatalf("the active token after abandon: %d, want 200 (the core holds a token Forgejo deleted)", code)
	}
	if got := tokens(forgejo.TokenName(int(v2))) + tokens(forgejo.TokenName(int(v1))); got != "01" {
		t.Fatalf("tokens of versions 2 and 1 after abandon: %s, want none and one", got)
	}
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
			if _, _, err := h.ids.CreateOIDCClient(h.ctx, audit.System, identity.NewOIDCClient{ID: "app-git4", Name: "Taken", RedirectURIs: []string{"https://x.acme.test/cb"}}); err != nil {
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

// App clients sign in only people the instance provisioned: a verified
// provisioned link this instance established and access at the last run,
// while the instance is active or degraded. Other clients are not app
// clients.
func TestOIDCAccess(t *testing.T) {
	h := newHarnessWith(t, apps.Config{}, forgejo.New())
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, Key: "git", BaseURL: "https://git.acme.test", Settings: map[string]any{forgejo.SettingOrganization: "acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	maya, chen := h.person("maya"), h.person("chen")
	allowed := func(client string, person uuid.UUID) bool {
		t.Helper()
		ok, err := h.svc.OIDCAccess(h.ctx, client, person)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := h.pool.Exec(h.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Not app clients.
	if !allowed("mobile", maya.ID) || !allowed("app-unknown", maya.ID) {
		t.Fatal("a client that is not an app instance's was refused")
	}
	// Installing: nobody.
	exec(`INSERT INTO account_links (person_id, provider, realm, external_id, login, method, instance_id) VALUES ($1, 'forgejo', $2, '7', 'maya', 'provisioned', $3)`, maya.ID, inst.ID.String(), inst.ID)
	exec(`INSERT INTO provisioning_results (instance_id, person_id, status, access) VALUES ($1, $2, 'done', true)`, inst.ID, maya.ID)
	if allowed("app-git", maya.ID) {
		t.Fatal("an installing instance signs people in")
	}
	exec(`UPDATE app_instances SET state = 'active' WHERE id = $1`, inst.ID)
	if !allowed("app-git", maya.ID) {
		t.Fatal("a provisioned person was refused")
	}
	// No link, or a link that is not the instance's provisioned one.
	if allowed("app-git", chen.ID) {
		t.Fatal("a person without an account was allowed")
	}
	exec(`INSERT INTO account_links (person_id, provider, realm, external_id, login, method) VALUES ($1, 'forgejo', $2, '8', 'chen', 'oauth')`, chen.ID, inst.ID.String())
	exec(`INSERT INTO provisioning_results (instance_id, person_id, status, access) VALUES ($1, $2, 'done', true)`, inst.ID, chen.ID)
	if allowed("app-git", chen.ID) {
		t.Fatal("a link Orglane did not provision was accepted")
	}
	// Access gone at the last run.
	exec(`UPDATE provisioning_results SET access = false WHERE person_id = $1`, maya.ID)
	if allowed("app-git", maya.ID) {
		t.Fatal("a person without access was allowed")
	}
	exec(`UPDATE provisioning_results SET access = true WHERE person_id = $1`, maya.ID)
	// Removed: nobody.
	exec(`UPDATE app_instances SET state = 'removed', data_retained = true, removed_at = now() WHERE id = $1`, inst.ID)
	if allowed("app-git", maya.ID) {
		t.Fatal("a removed instance signs people in")
	}
}
