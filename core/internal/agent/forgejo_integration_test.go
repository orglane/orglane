//go:build integration

package agent

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
)

// TestForgejoPackage installs the Forgejo package with the real agent on
// PostgreSQL and reaches it through the real Traefik with the routes the
// core builds. Then: the adapter's Lifecycle against it (health, setup,
// organization names), the admin token with every ambiguous answer the
// adapter must not mistake for proof, Git over SSH on the published port, a
// fenced start followed by the unfenced render (the fence must not outlive
// it), changed settings and secrets, remove, reinstall with the data, and
// purge so the key starts empty (docs/specs/agent.md "Testing",
// providers.md "Keys and removal").
func TestForgejoPackage(t *testing.T) {
	if managedtest.RunAsRoot(t) {
		return
	}
	b := managedtest.StartBundle(t)
	registry, err := packages.NewRegistry(forgejopkg.FS)
	if err != nil {
		t.Fatal(err)
	}
	e := startAgent(t, b.Project, registry)
	const key = "git"
	dir := e.agent.instanceDir(key)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", packages.InstanceProject(b.Project, key), "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
	client := b.Client()
	adapter := forgejo.New()
	id := uuid.Must(uuid.NewV7())
	secrets := managedtest.ForgejoSecrets()
	sshPort := managedtest.FreePort(t)
	env := &managedtest.Env{Client: client, Creds: map[string][]byte{forgejo.CredentialAdminPassword: []byte(secrets[forgejo.CredentialAdminPassword])}}
	upstream := &url.URL{Scheme: "http", Host: packages.ServiceAlias(key, "forgejo", false) + ":3000"}
	inst := provider.Instance{
		ID: id, Key: key, Provider: forgejo.ID, Management: provider.Managed,
		BaseURL: &url.URL{Scheme: "http", Host: managedtest.ForgejoHost}, Upstream: upstream,
		Settings: map[string]any{forgejo.SettingOrganization: "orglane", forgejo.SettingSSHPort: int64(sshPort)},
	}
	admin := managedtest.Auth{User: forgejo.AdminUsername, Password: secrets[forgejo.CredentialAdminPassword]}
	get := func(path string, auth managedtest.Auth) int {
		code, _ := managedtest.Request(t, client, http.MethodGet, managedtest.ForgejoHost, path, auth, nil)
		return code
	}
	healthy := func() bool { return get("/api/healthz", managedtest.Auth{}) == http.StatusOK }
	tokens := e.psqlCount("svc_git", "access_token")

	// The routes go in before the install, as for a reinstall: Traefik
	// answers 502 until the service is up.
	b.Routes(t, managedtest.ForgejoRoutes(key)...)

	start := time.Now()
	gen := int64(1)
	e.mustApplyKey(key, managedtest.ForgejoSpec(id, gen, sshPort, secrets, agentapi.StateRunning))
	t.Logf("installed Forgejo %s in %s", forgejopkg.Current, time.Since(start).Round(time.Second))
	managedtest.Eventually(t, time.Minute, "Forgejo answers through Traefik", healthy)
	if err := adapter.CheckHealth(t.Context(), env, inst); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	// The configure hook saved the Orglane sign-in source (from the
	// workspace's discovery document) and published its ID.
	if code, body := managedtest.Request(t, client, http.MethodGet, managedtest.ForgejoHost, "/assets/orglane/auth-source.json", managedtest.Auth{}, nil); code != http.StatusOK ||
		!strings.Contains(string(body), `"name":"orglane"`) {
		t.Fatalf("published sign-in source: %d %s", code, body)
	}
	// Nobody browses anonymously.
	if code := get("/explore/repos", managedtest.Auth{}); code != http.StatusSeeOther {
		t.Fatalf("anonymous explore = %d, want a redirect to sign in", code)
	}

	// Setup: the configure hook made the service admin; Setup creates the
	// organization, and running it again changes nothing.
	for range 2 {
		if err := adapter.Setup(t.Context(), env, inst); err != nil {
			t.Fatalf("Setup: %v", err)
		}
	}
	if code := get("/api/v1/orgs/orglane", admin); code != http.StatusOK {
		t.Fatalf("organization = %d", code)
	}
	if got := e.psql("svc_git", `select visibility from "user" where lower_name = 'orglane'`); got != "1" { // 1: limited
		t.Fatalf("organization visibility = %q, want limited", got)
	}

	// Organization names: what the setting accepts, Forgejo accepts; what
	// Forgejo refuses never reaches it.
	edge := inst
	edge.Settings = map[string]any{forgejo.SettingOrganization: "Acme.Robo_t-1"}
	if err := adapter.Setup(t.Context(), env, edge); err != nil {
		t.Fatalf("Setup with an edge-case name: %v", err)
	}
	for _, bad := range []string{"acme-", "acme..org", "explore", "a.keys"} {
		edge.Settings = map[string]any{forgejo.SettingOrganization: bad}
		if err := adapter.Setup(t.Context(), env, edge); err == nil {
			t.Errorf("Setup accepted %q", bad)
		} else if c, _, _ := provider.Classify(err); c != forgejo.CodeInvalidSettings {
			t.Errorf("Setup %q: %v, want %s before Forgejo", bad, err, forgejo.CodeInvalidSettings)
		}
	}

	testAdminToken(t, e, adapter, env, inst, client, tokens)

	// Git over SSH on the published port, bound to loopback in local mode.
	managedtest.Eventually(t, 30*time.Second, "the SSH port answers", func() bool {
		b, err := managedtest.SSHBanner(sshPort)
		return err == nil && strings.HasPrefix(b, "SSH-2.0-")
	})

	gen = testFence(t, e, b, key, id, gen, sshPort, secrets, admin)

	// A new SSH port and a new admin password: re-rendered, the configure
	// hook resets the password, the old one stops working.
	secrets2 := managedtest.ForgejoSecrets()
	secrets2[forgejo.CredentialDBPassword] = secrets[forgejo.CredentialDBPassword]
	secrets2[forgejo.CredentialSecretKey] = secrets[forgejo.CredentialSecretKey]
	sshPort2 := managedtest.FreePort(t)
	gen++
	e.mustApplyKey(key, managedtest.ForgejoSpec(id, gen, sshPort2, secrets2, agentapi.StateRunning))
	managedtest.Eventually(t, time.Minute, "Forgejo answers after the change", healthy)
	admin2 := managedtest.Auth{User: forgejo.AdminUsername, Password: secrets2[forgejo.CredentialAdminPassword]}
	if code := get("/api/v1/user", admin2); code != http.StatusOK {
		t.Fatalf("the new admin password does not work: %d", code)
	}
	if code := get("/api/v1/user", admin); code != http.StatusUnauthorized {
		t.Fatalf("the old admin password still works: %d", code)
	}
	managedtest.Eventually(t, 30*time.Second, "SSH on the new port", func() bool {
		b, err := managedtest.SSHBanner(sshPort2)
		return err == nil && strings.HasPrefix(b, "SSH-2.0-")
	})
	if _, err := managedtest.SSHBanner(sshPort); err == nil {
		t.Fatal("the old SSH port is still published")
	}
	env.Creds[forgejo.CredentialAdminPassword] = []byte(secrets2[forgejo.CredentialAdminPassword])
	token := e.mintToken(t, adapter, env, inst, 10)

	logIdleMemory(t, packages.InstanceProject(b.Project, key))

	// Remove: containers go, data and database stay.
	gen++
	e.mustApplyKey(key, managedtest.ForgejoSpec(id, gen, sshPort2, secrets2, agentapi.StateAbsent))
	if c := e.containers(key); len(c) != 0 {
		t.Fatalf("containers after remove: %v", c)
	}
	if code := get("/api/healthz", managedtest.Auth{}); code != http.StatusBadGateway {
		t.Fatalf("a removed instance answers %d through its route, want 502", code)
	}

	// Reinstall: same instance, same secrets, same data.
	gen++
	e.mustApplyKey(key, managedtest.ForgejoSpec(id, gen, sshPort2, secrets2, agentapi.StateRunning))
	managedtest.Eventually(t, time.Minute, "Forgejo answers after the reinstall", healthy)
	if code := get("/api/v1/orgs/orglane", managedtest.Auth{Token: token}); code != http.StatusOK {
		t.Fatalf("organization after the reinstall = %d", code)
	}
	if err := adapter.Setup(t.Context(), env, inst); err != nil {
		t.Fatalf("Setup after the reinstall: %v", err)
	}

	// Purge: the directory, the database and the role go.
	purge := managedtest.ForgejoSpec(id, gen+1, sshPort2, secrets2, agentapi.StateAbsent)
	purge.Purge = true
	e.mustApplyKey(key, purge)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory after purge: %v", err)
	}
	if got := e.psql("orglane", "select count(*) from pg_database where datname = 'svc_git'"); got != "0" {
		t.Fatalf("database after purge: %s", got)
	}

	// The key starts empty for a new instance: no organization, no tokens.
	id2 := uuid.Must(uuid.NewV7())
	secrets3 := managedtest.ForgejoSecrets()
	e.mustApplyKey(key, managedtest.ForgejoSpec(id2, 1, sshPort2, secrets3, agentapi.StateRunning))
	managedtest.Eventually(t, time.Minute, "the new instance answers", healthy)
	if code := get("/api/v1/orgs/orglane", managedtest.Auth{User: forgejo.AdminUsername, Password: secrets3[forgejo.CredentialAdminPassword]}); code != http.StatusNotFound {
		t.Fatalf("organization of the new instance = %d, want 404", code)
	}
	if code := get("/api/v1/user", managedtest.Auth{Token: token}); code != http.StatusUnauthorized {
		t.Fatalf("an old token works on the new instance: %d", code)
	}
	purge = managedtest.ForgejoSpec(id2, 2, sshPort2, secrets3, agentapi.StateAbsent)
	purge.Purge = true
	e.mustApplyKey(key, purge)
}

// psqlCount returns a function counting rows of table with the name.
func (e *testEnv) psqlCount(db, table string) func(name string) string {
	return func(name string) string {
		return e.psql(db, "select count(*) from "+table+" where name = '"+name+"'")
	}
}

func (e *testEnv) mintToken(t *testing.T, a *forgejo.Adapter, env provider.Env, inst provider.Instance, version int) string {
	t.Helper()
	r, err := a.ApplyCredential(t.Context(), env, inst, forgejo.CredentialAdminToken, version, nil)
	if err != nil || r.Outcome != provider.RotationAcknowledged {
		t.Fatalf("mint version %d: %+v, %v", version, r, err)
	}
	return string(r.Value)
}

// testAdminToken runs the admin token's rotation outcomes against the real
// API: only proof acknowledges or rejects, everything ambiguous is unknown.
func testAdminToken(t *testing.T, e *testEnv, a *forgejo.Adapter, env *managedtest.Env, inst provider.Instance, client *http.Client, tokens func(string) string) {
	t.Helper()
	works := func(token string) int {
		code, _ := managedtest.Request(t, client, http.MethodGet, managedtest.ForgejoHost, "/api/v1/user", managedtest.Auth{Token: token}, nil)
		return code
	}
	tokensPath := "/api/v1/users/" + forgejo.AdminUsername + "/tokens"
	token1 := e.mintToken(t, a, env, inst, 1)
	if code := works(token1); code != http.StatusOK {
		t.Fatalf("the minted token does not work: %d", code)
	}

	// Committed, answer lost: unknown. A later attempt that cannot
	// authenticate proves nothing either: still unknown, the token exists.
	lossy, faults := managedtest.WithFaults(client)
	lossyEnv := &managedtest.Env{Client: lossy, Creds: env.Creds}
	faults.Drop(http.MethodPost, tokensPath)
	if r, err := a.ApplyCredential(t.Context(), lossyEnv, inst, forgejo.CredentialAdminToken, 2, nil); err == nil || r.Outcome != provider.RotationUnknown {
		t.Fatalf("lost answer: %+v, %v (want unknown)", r, err)
	}
	if got := tokens(forgejo.TokenName(2)); got != "1" {
		t.Fatalf("the lost mint did not commit (%s tokens): the fault is not realistic", got)
	}
	wrong := &managedtest.Env{Client: client, Creds: map[string][]byte{forgejo.CredentialAdminPassword: []byte("wrong")}}
	if r, err := a.ApplyCredential(t.Context(), wrong, inst, forgejo.CredentialAdminToken, 2, nil); err == nil || r.Outcome != provider.RotationUnknown {
		t.Fatalf("lost mint, then no authentication: %+v, %v (want unknown)", r, err)
	}
	// The next attempt deletes the lost token and mints again.
	token2 := e.mintToken(t, a, lossyEnv, inst, 2)
	if got := tokens(forgejo.TokenName(2)); got != "1" {
		t.Fatalf("tokens of version 2 after the retry: %s, want 1", got)
	}

	// The name is taken between the read-back and the mint (an attempt
	// still in flight): Forgejo refuses it, which is no proof. Unknown.
	faults.Before(http.MethodPost, tokensPath, func() {
		code, _ := managedtest.Request(t, client, http.MethodPost, managedtest.ForgejoHost, tokensPath,
			managedtest.Auth{User: forgejo.AdminUsername, Password: string(env.Creds[forgejo.CredentialAdminPassword])},
			map[string]any{"name": forgejo.TokenName(3), "scopes": []string{"read:user"}})
		if code != http.StatusCreated {
			t.Errorf("concurrent mint: %d", code)
		}
	})
	if r, err := a.ApplyCredential(t.Context(), lossyEnv, inst, forgejo.CredentialAdminToken, 3, nil); err == nil || r.Outcome != provider.RotationUnknown {
		t.Fatalf("name taken by a concurrent mint: %+v, %v (want unknown)", r, err)
	}

	// Abandon of version 3: the core re-applies the active version 2 with
	// its value. The held token keeps working, nothing is minted, and the
	// candidate's token is deleted.
	r, err := a.ApplyCredential(t.Context(), env, inst, forgejo.CredentialAdminToken, 2, []byte(token2))
	if err != nil || r.Outcome != provider.RotationAcknowledged || r.Value != nil {
		t.Fatalf("re-apply the active version: %+v, %v", r, err)
	}
	if code := works(token2); code != http.StatusOK {
		t.Fatalf("the active token after re-applying it: %d, want 200", code)
	}
	if got := tokens(forgejo.TokenName(3)); got != "0" {
		t.Fatalf("tokens of the abandoned version 3: %s, want 0", got)
	}
	if got := tokens(forgejo.TokenName(2)) + tokens(forgejo.TokenName(1)); got != "11" {
		t.Fatalf("tokens of versions 2 and 1: %s, want one each", got)
	}
	// A held value Forgejo no longer has is rejected, never re-minted.
	if r, err := a.ApplyCredential(t.Context(), env, inst, forgejo.CredentialAdminToken, 2, []byte("0000000000000000000000000000000000000000")); err != nil || r.Outcome != provider.RotationRejected {
		t.Fatalf("re-apply an unknown value: %+v, %v", r, err)
	}
}

// testFence runs an upgrade candidate's fenced render on the instance's
// data and then the unfenced render again, as an upgrade that commits
// (agent.md "Upgrade"). The agent stops the instance (so it does not repair
// the drift meanwhile); the fenced render runs as a project of its own on
// the same data directory and database: no published SSH port, only the
// fenced alias, no mirror updates. Then the agent starts the unfenced
// render: mirror updates are scheduled again, although Forgejo keeps
// app.ini values whose variables disappear. It returns the last generation.
func testFence(t *testing.T, e *testEnv, b *managedtest.Bundle, key string, id uuid.UUID, gen int64, sshPort int, secrets map[string]string, admin managedtest.Auth) int64 {
	t.Helper()
	client := b.Client()
	fencedHost := packages.ServiceAlias(key, "forgejo", true)
	gen++
	e.mustApplyKey(key, managedtest.ForgejoSpec(id, gen, sshPort, secrets, agentapi.StateStopped))

	pkg, _ := e.registry.Get(agentapi.PackageRef{ID: "forgejo", Version: forgejopkg.Current})
	spec := managedtest.ForgejoSpec(id, gen, sshPort, secrets, agentapi.StateRunning)
	in, err := pkg.Inputs(spec)
	if err != nil {
		t.Fatal(err)
	}
	rc := e.agent.renderContext(key)
	rc.Fenced = true
	rc.ComposeProject += "fence" // not an instance project of the agent
	fenced, err := pkg.Render(spec, in, rc)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	compose, envPath := filepath.Join(dir, composeFile), filepath.Join(dir, envFile)
	if err := os.WriteFile(compose, fenced.Compose, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, fenced.Env, 0o600); err != nil {
		t.Fatal(err)
	}
	compose1 := []string{"compose", "--project-name", rc.Project(), "--project-directory", dir, "-f", compose, "--env-file", envPath}
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", append(compose1, "down", "--remove-orphans")...).Run() //nolint:gosec // test commands
	})
	managedtest.Docker(t, append(compose1, "up", "--detach", "--wait")...)
	if code, _ := managedtest.Request(t, client, http.MethodGet, fencedHost, "/api/healthz", managedtest.Auth{}, nil); code != http.StatusOK {
		t.Fatalf("the fenced candidate is not reachable on its alias: %d", code)
	}
	if code, _ := managedtest.Request(t, client, http.MethodGet, managedtest.ForgejoHost, "/api/healthz", managedtest.Auth{}, nil); code != http.StatusBadGateway {
		t.Fatalf("the public route reaches the fenced candidate: %d", code)
	}
	if _, err := managedtest.SSHBanner(sshPort); err == nil {
		t.Fatal("the fenced candidate publishes SSH")
	}
	if s := managedtest.CronSchedule(t, client, fencedHost, admin, "update_mirrors"); s != "-" {
		t.Fatalf("mirror updates while fenced: %q, want off", s)
	}
	managedtest.Docker(t, append(compose1, "down", "--remove-orphans")...)

	gen++
	e.mustApplyKey(key, managedtest.ForgejoSpec(id, gen, sshPort, secrets, agentapi.StateRunning))
	managedtest.Eventually(t, time.Minute, "the unfenced render answers", func() bool {
		code, _ := managedtest.Request(t, client, http.MethodGet, managedtest.ForgejoHost, "/api/healthz", managedtest.Auth{}, nil)
		return code == http.StatusOK
	})
	if s := managedtest.CronSchedule(t, client, managedtest.ForgejoHost, admin, "update_mirrors"); s == "-" {
		t.Fatal("mirror updates stay off after the fence: the fenced render persisted")
	}
	for _, off := range []string{"update_checker", "delete_inactive_accounts"} {
		if s := managedtest.CronSchedule(t, client, managedtest.ForgejoHost, admin, off); s != "-" {
			t.Errorf("cron task %s is scheduled (%q), want off", off, s)
		}
	}
	managedtest.Eventually(t, 30*time.Second, "SSH is published again", func() bool {
		b, err := managedtest.SSHBanner(sshPort)
		return err == nil && strings.HasPrefix(b, "SSH-2.0-")
	})
	return gen
}

// logIdleMemory logs the memory use of the project's containers after a
// short idle period (reported in the pull request; ADR 0009).
func logIdleMemory(t *testing.T, project string) {
	t.Helper()
	time.Sleep(20 * time.Second)
	ids := strings.Fields(managedtest.Docker(t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+project))
	for _, id := range ids {
		t.Logf("idle memory: %s", managedtest.Docker(t, "stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}", id))
	}
}
