//go:build integration

package apps_test

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/ingress"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
)

// fixedResolver answers host lookups from a map.
type fixedResolver map[string][]netip.Addr

func (r fixedResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return r[strings.TrimSuffix(host, ".")], nil
}

// TestForgejoInstall installs a real Forgejo through the install flow
// (Service.Install with the production checks, the agent_apply job, the
// core's own ingress routes on a real Traefik, the Setup job), reaches it
// through Traefik, then removes it with its data retained, reinstalls it
// with that data, and purges it (docs/specs/providers.md "Managed instance
// lifecycle in the core", "app_instances").
func TestForgejoInstall(t *testing.T) {
	if managedtest.RunAsRoot(t) {
		return
	}
	b := managedtest.StartBundle(t)
	registry, err := packages.NewRegistry(forgejopkg.FS)
	if err != nil {
		t.Fatal(err)
	}
	agentClient := startAgent(t, b, registry)
	local, _ := url.Parse("http://orglane.localhost")
	var svc *apps.Service
	h := newHarnessWith(t, apps.Config{
		HTTPClient: b.Client(), Packages: registry, Agent: agentClient,
		WorkspaceURL: local, IssuerURL: local, DomainBase: "orglane.localhost", IngressMode: agentapi.IngressLocal,
		ServiceHost: func(label string) string { return label + ".orglane.localhost" },
		InstallPreflight: func(ctx context.Context, p apps.InstallPlan) (apps.InstallBudget, error) {
			return svc.CheckInstall(ctx, p)
		},
		AgentPollInterval: 500 * time.Millisecond,
	}, forgejo.New())
	svc = h.svc
	ctx := h.ctx

	// The agent reports itself and the host's resources.
	st, err := svc.AgentStatus(ctx)
	if err != nil || !st.Connected || st.MemTotal == 0 || st.MemoryReserved != apps.BundleMemory {
		t.Fatalf("agent status %+v, %v", st, err)
	}
	// In domain mode the host must resolve to the workspace's addresses.
	work, _ := url.Parse("https://work.acme.test")
	domain := h.restartCore(func(c *apps.Config) {
		c.IngressMode, c.WorkspaceURL = agentapi.IngressDomain, work
		c.Resolver = fixedResolver{"work.acme.test": {netip.MustParseAddr("192.0.2.1")}, "git.acme.test": {netip.MustParseAddr("192.0.2.2")}}
	})
	pkg, _ := svc.InstallDefaults(mustAdapter(t, svc, forgejo.ID))
	if _, err := domain.CheckInstall(ctx, apps.InstallPlan{Provider: forgejo.ID, Key: "git", Host: "git.acme.test", Package: pkg.Package}); apps.Code(err) != apps.CodeDNSMismatch {
		t.Fatalf("a host resolving elsewhere = %v", err)
	}
	if pkg.Host != managedtest.ForgejoHost || pkg.Key != "git" || pkg.Settings[forgejo.SettingOrganization] != "orglane" {
		t.Fatalf("install defaults %+v", pkg)
	}

	// Install with the defaults (a free SSH port, so tests can run side by side).
	inst, err := svc.Install(ctx, audit.System, apps.NewInstance{Provider: forgejo.ID, Settings: map[string]any{forgejo.SettingSSHPort: managedtest.FreePort(t)}})
	if err != nil {
		t.Fatal(err)
	}
	project := packages.InstanceProject(b.Project, inst.Key)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", project, "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
	if *inst.BaseUrl != "http://"+managedtest.ForgejoHost || inst.Settings[forgejo.SettingOrganization] != "orglane" {
		t.Fatalf("installed %+v", inst.AppInstance)
	}
	// The core's routes, plus one for the upstream alias: Setup reaches
	// Forgejo by its alias, which only containers on the edge network
	// resolve, and this test runs outside Docker.
	routes := func() []ingress.AppRoute {
		r, err := svc.IngressRoutes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return append(r, managedtest.ForgejoRoutes(inst.Key)[1])
	}
	admin := func() managedtest.Auth {
		pw, err := svc.EnvForTest(inst).Credential(ctx, forgejo.CredentialAdminPassword)
		if err != nil {
			t.Fatal(err)
		}
		return managedtest.Auth{User: forgejo.AdminUsername, Password: string(pw)}
	}
	health := func() int {
		code, _ := managedtest.Request(t, b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/healthz", managedtest.Auth{}, nil)
		return code
	}
	activate := func(gen int64) {
		t.Helper()
		applyUntilDone(t, svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: gen})
		if op := h.operation(inst.ID, gen); op.State != agentapi.OpSucceeded {
			t.Fatalf("generation %d: %+v (code %s)", gen, op, derefS(op.ErrorCode))
		}
		b.Routes(t, routes()...)
		// Traefik serves the routes once it has read them.
		managedtest.Eventually(t, time.Minute, "Forgejo through Traefik", func() bool { return health() == http.StatusOK })
		if err := svc.RunSetup(ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
			t.Fatalf("Setup: %v", err)
		}
		if got := h.reload(inst.ID); got.State != apps.StateActive {
			t.Fatalf("after Setup: %s %s", got.State, derefS(got.HealthCode))
		}
	}
	activate(1)
	if out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", //nolint:gosec // test commands
		managedtest.Docker(t, "ps", "-q", "--filter", "label=com.docker.compose.project="+project, "--filter", "label=com.docker.compose.service=forgejo")).Output(); err == nil {
		t.Logf("idle-memory: Forgejo after install and setup: %s", strings.TrimSpace(string(out)))
	}
	if code, body := managedtest.Request(t, b.Client(), http.MethodPost, managedtest.ForgejoHost, "/api/v1/orgs/orglane/repos", admin(), map[string]any{"name": "marker"}); code != http.StatusCreated {
		t.Fatalf("create a repository: %d %s", code, body)
	}

	// Remove: the containers go, the data stays, the route goes.
	if _, err := svc.Remove(ctx, audit.System, inst.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2})
	if got := h.reload(inst.ID); got.State != apps.StateRemoved || !got.DataRetained {
		t.Fatalf("removed: %+v", got.AppInstance)
	}
	if r, _ := svc.IngressRoutes(ctx); len(r) != 0 {
		t.Fatalf("a removed instance has routes %+v", r)
	}
	b.Routes(t, routes()...)
	managedtest.Eventually(t, time.Minute, "the route is gone", func() bool { return health() == http.StatusNotFound })

	// Reinstall: the same data comes back.
	if _, err := svc.Reinstall(ctx, audit.System, inst.ID); err != nil {
		t.Fatal(err)
	}
	activate(3)
	if code, body := managedtest.Request(t, b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/v1/repos/orglane/marker", admin(), nil); code != http.StatusOK {
		t.Fatalf("the repository after the reinstall: %d %s", code, body)
	}

	// Purge: the database and role are dropped, the credentials deleted.
	if _, err := svc.Remove(ctx, audit.System, inst.ID, true, inst.Key); err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 4})
	if got := h.reload(inst.ID); got.State != apps.StateRemoved || got.DataRetained {
		t.Fatalf("purged: %+v", got.AppInstance)
	}
	if n := b.Psql(t, "orglane", "select count(*) from pg_database where datname = 'svc_git'"); n != "0" {
		t.Fatalf("the service database still exists: %s", n)
	}
	// The key is free, its OIDC client included: a fresh install under it
	// registers its own client and is accepted.
	fresh, err := svc.Install(ctx, audit.System, apps.NewInstance{Provider: forgejo.ID, Key: inst.Key})
	if err != nil {
		t.Fatalf("a fresh install after the purge: %v", err)
	}
	if fresh.ID == inst.ID {
		t.Fatalf("a fresh install reused the purged instance: %+v", fresh.AppInstance)
	}
}

func mustAdapter(t *testing.T, svc *apps.Service, id provider.ID) *apps.Adapter {
	t.Helper()
	for _, a := range svc.Registry().Adapters() {
		if a.Manifest.ID == id {
			return a
		}
	}
	t.Fatalf("no adapter %s", id)
	return nil
}
