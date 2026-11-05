//go:build integration

package apps_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/server"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/tests/demotest"
	"github.com/orglane/orglane/tests/pgtest"
)

// browser is an HTTP client with cookies that reaches every host through
// Traefik, as a browser that resolves *.localhost to loopback.
type browser struct {
	*http.Client
}

func newBrowser(t *testing.T, b *managedtest.Bundle) browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := b.Client()
	c.Jar = jar
	c.CheckRedirect = nil // follow redirects, at most 10
	return browser{c}
}

// page is what a request ended with, after redirects.
type page struct {
	StatusCode int
	URL        *url.URL
}

func (br browser) do(t *testing.T, method, rawURL string, body any) (page, []byte) {
	t.Helper()
	var in io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		in = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, in)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := br.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return page{StatusCode: resp.StatusCode, URL: resp.Request.URL}, out
}

// startCore runs `orglane server` in-process on the database in local mode
// (http://orglane.localhost, demo mode for one-click sign-in), behind the
// bundle's Traefik, which polls the core's routes. It returns the config.
func startCore(t *testing.T, dbURL string, public, internal int) *config.Config {
	t.Helper()
	host := managedtest.CoreListenHost()
	vars := map[string]string{
		"ORGLANE_DATABASE_URL":           dbURL,
		"ORGLANE_DOMAIN_BASE":            "orglane.localhost",
		"ORGLANE_DOMAIN_WORKSPACE_HOST":  managedtest.WorkspaceHost,
		"ORGLANE_DOMAIN_SCHEME":          "http",
		"ORGLANE_ENVIRONMENT":            "development",
		"ORGLANE_INGRESS_MODE":           "local",
		"ORGLANE_SERVER_LISTEN":          net.JoinHostPort(host, strconv.Itoa(public)),
		"ORGLANE_SERVER_INTERNAL_LISTEN": net.JoinHostPort(host, strconv.Itoa(internal)),
		"ORGLANE_INGRESS_UPSTREAM_URL":   managedtest.CoreURL(public),
		"ORGLANE_IDENTITY_MASTER_SECRET": masterSecret,
		"ORGLANE_DEMO_ENABLED":           "true",
	}
	cfg, err := config.Load("", func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, cfg, "dev", slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server: %v", err)
		}
	})
	return cfg
}

// TestForgejoSingleSignOn signs a person in to a real Forgejo through the
// real Orglane OpenID Provider in local mode (docs/specs/providers.md
// "Forgejo OIDC configured automatically", test 6): creating the instance
// registers its OIDC client with the redirect URI from the base URL
// (http://git.orglane.localhost/user/oauth2/orglane/callback), the agent
// installs Forgejo, whose configure hook saves the sign-in source from
// Orglane's discovery document at http://orglane.localhost (through
// Traefik's alias), and the deep link from the API lands on the target
// signed in, without a second sign-in. Every hop goes through the real
// Traefik with the routes the core serves.
func TestForgejoSingleSignOn(t *testing.T) {
	if managedtest.RunAsRoot(t) {
		return
	}
	public, internal := managedtest.FreePort(t), managedtest.FreePort(t)
	b := managedtest.StartBundleWithCore(t, internal)
	dbURL := pgtest.NewDatabase(t)
	cfg := startCore(t, dbURL, public, internal)
	br := newBrowser(t, b)
	workspace := "http://" + managedtest.WorkspaceHost
	managedtest.Eventually(t, time.Minute, "the core answers through Traefik", func() bool {
		resp, _ := br.do(t, http.MethodGet, workspace+"/healthz", nil)
		return resp.StatusCode == http.StatusOK
	})

	// The instance, as the install flow creates it (#61 phase 2 adds the
	// API): its OIDC client and credentials in one transaction.
	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.Database, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	stack, err := web.NewStack(pool, cfg, slog.New(slog.DiscardHandler), web.StackOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := stack.Apps.CreateInstance(ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, BaseURL: "http://" + managedtest.ForgejoHost,
		Settings: map[string]any{forgejo.SettingOrganization: "acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := stack.Identity.OIDCClient(ctx, provider.OIDCClientID(inst.Key))
	if err != nil {
		t.Fatalf("the OIDC client was not registered: %v", err)
	}
	if want := "http://" + managedtest.ForgejoHost + "/user/oauth2/orglane/callback"; len(client.RedirectUris) != 1 || client.RedirectUris[0] != want {
		t.Fatalf("redirect URIs = %v, want [%s]", client.RedirectUris, want)
	}

	// The agent installs it with the core's inputs and credentials.
	registry, err := packages.NewRegistry(forgejopkg.FS)
	if err != nil {
		t.Fatal(err)
	}
	agentClient := startAgent(t, b, registry)
	env := stack.Apps.EnvForTest(inst)
	lc, _ := inst.Adapter.Lifecycle()
	in, err := lc.Inputs(ctx, env, inst.View())
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{}
	for _, name := range in.Secrets {
		v, err := env.Credential(ctx, name)
		if err != nil {
			t.Fatalf("credential %s: %v", name, err)
		}
		secrets[name] = string(v)
	}
	spec := managedtest.ForgejoSpec(inst.ID, 1, managedtest.FreePort(t), secrets, agentapi.StateRunning)
	for k, v := range in.Settings {
		spec.Settings[k] = v
	}
	project := packages.InstanceProject(b.Project, inst.Key)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", project, "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
	op, _, err := agentClient.Apply(t.Context(), inst.Key, spec)
	if err != nil {
		t.Fatal(err)
	}
	managedtest.Eventually(t, 5*time.Minute, "the install finishes", func() bool {
		op, err = agentClient.Operation(t.Context(), op.ID)
		return err == nil && op.Terminal()
	})
	if op.State != agentapi.OpSucceeded {
		t.Fatalf("install: %+v (the configure hook needs Orglane's discovery document)", op)
	}
	// Running: the core routes it (the install flow sets this).
	if _, err := pool.Exec(ctx, `UPDATE app_instances SET state = 'active', package_version = $2,
		desired_generation = 1, observed_generation = 1 WHERE id = $1`, inst.ID, forgejopkg.Current); err != nil {
		t.Fatal(err)
	}
	forgejoURL := "http://" + managedtest.ForgejoHost
	managedtest.Eventually(t, time.Minute, "Traefik routes Forgejo from the core's configuration", func() bool {
		resp, _ := br.do(t, http.MethodGet, forgejoURL+"/api/healthz", nil)
		return resp.StatusCode == http.StatusOK
	})

	// Setup through the public route (the core uses the upstream on the
	// edge network): service admin, sign-in source, organization.
	view := inst.View()
	view.Upstream = &url.URL{Scheme: "http", Host: managedtest.ForgejoHost}
	adminEnv := &managedtest.Env{Client: b.Client(), Creds: map[string][]byte{forgejo.CredentialAdminPassword: []byte(secrets[forgejo.CredentialAdminPassword])}}
	if err := lc.Setup(ctx, adminEnv, view); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	// People cannot sign in with a password; only through Orglane.
	resp, html := br.do(t, http.MethodGet, forgejoURL+"/user/login", nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(html), `name="password"`) || !strings.Contains(string(html), "/user/oauth2/orglane") {
		t.Fatalf("Forgejo's sign-in page (%d) offers a password form or no Orglane sign-in", resp.StatusCode)
	}

	// A person signs in to Orglane (demo mode: one click).
	resp, body := br.do(t, http.MethodPost, workspace+"/api/v1/demo/sign-in", map[string]string{})
	var signedIn struct {
		Person struct {
			ID, Username, WorkEmail string
		} `json:"person"`
	}
	if err := json.Unmarshal(body, &signedIn); resp.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("sign in to Orglane: %d %s", resp.StatusCode, body)
	}
	person := signedIn.Person

	// Their Forgejo account exists, bound to the Orglane source with their
	// subject (provisioning creates it the same way, #62).
	var source struct{ ID int64 }
	resp, body = br.do(t, http.MethodGet, forgejoURL+"/assets/orglane/auth-source.json", nil)
	if err := json.Unmarshal(body, &source); resp.StatusCode != http.StatusOK || err != nil || source.ID < 1 {
		t.Fatalf("published sign-in source: %d %s", resp.StatusCode, body)
	}
	admin := managedtest.Auth{User: forgejo.AdminUsername, Password: secrets[forgejo.CredentialAdminPassword]}
	code, out := managedtest.Request(t, b.Client(), http.MethodPost, managedtest.ForgejoHost, "/api/v1/admin/users", admin, map[string]any{
		"username": person.Username, "email": person.WorkEmail, "source_id": source.ID, "login_name": person.ID,
		"must_change_password": false, "password": "",
	})
	if code != http.StatusCreated {
		t.Fatalf("create the account: %d %s", code, out)
	}

	// The deep link: Forgejo's sign-in with Orglane, Orglane's existing
	// session, back to Forgejo, signed in, at the target.
	resp, body = br.do(t, http.MethodGet, workspace+"/api/v1/apps/instances/"+inst.ID.String()+"/open?to="+url.QueryEscape("/acme"), nil)
	var link struct{ URL string }
	if err := json.Unmarshal(body, &link); resp.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("deep link: %d %s", resp.StatusCode, body)
	}
	if want := forgejoURL + "/user/oauth2/orglane?redirect_to=%2Facme"; link.URL != want {
		t.Fatalf("deep link = %s, want %s", link.URL, want)
	}
	resp, html = br.do(t, http.MethodGet, link.URL, nil)
	if resp.StatusCode != http.StatusOK || resp.URL.String() != forgejoURL+"/acme" {
		t.Fatalf("the deep link ended at %s (%d), want %s/acme", resp.URL, resp.StatusCode, forgejoURL)
	}
	if !strings.Contains(string(html), person.Username) {
		t.Fatal("the target page does not show the signed-in person")
	}
	if !signedInToForgejo(t, br, forgejoURL) {
		t.Fatal("not signed in to Forgejo after the deep link")
	}

	// Without an Orglane session the deep link stops at Orglane's sign-in.
	anonymous := newBrowser(t, b)
	resp, _ = anonymous.do(t, http.MethodGet, link.URL, nil)
	if resp.URL.Host != managedtest.WorkspaceHost || signedInToForgejo(t, anonymous, forgejoURL) {
		t.Fatalf("without an Orglane session the deep link ended at %s", resp.URL)
	}

	// Signed in to Orglane, but without a Forgejo account Orglane created:
	// Forgejo neither creates one nor links another account to them.
	other := newBrowser(t, b)
	var someone string
	for _, p := range demotest.Dataset(t).People {
		if p.Username != person.Username {
			someone = p.Username
			break
		}
	}
	if resp, body := other.do(t, http.MethodPost, workspace+"/api/v1/demo/sign-in", map[string]string{"username": someone}); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in as %s: %d %s", someone, resp.StatusCode, body)
	}
	other.do(t, http.MethodGet, link.URL, nil)
	if signedInToForgejo(t, other, forgejoURL) {
		t.Fatal("a person without a provisioned account got into Forgejo")
	}
	if code, _ := managedtest.Request(t, b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/v1/users/"+someone, admin, nil); code != http.StatusNotFound {
		t.Fatalf("Forgejo registered %s on its own: %d", someone, code)
	}
}

// signedInToForgejo reports whether the browser has a Forgejo session: the
// settings page answers instead of sending it to sign in.
func signedInToForgejo(t *testing.T, br browser, forgejoURL string) bool {
	t.Helper()
	resp, _ := br.do(t, http.MethodGet, forgejoURL+"/user/settings", nil)
	return resp.StatusCode == http.StatusOK && resp.URL.Path == "/user/settings"
}
