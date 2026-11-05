//go:build integration

package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/ingress"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
)

// traefikImage is the bundle's Traefik image (deploy/compose).
const traefikImage = "traefik:v3.7.17@sha256:30ff62491aacdb862ceb8c9edcf2627da638d7deafdd4b6dab32fe34f8aa3e90"

// forgejoHost is the instance host in local mode (agent.md "Local mode").
const forgejoHost = "git.orglane.localhost"

// startTraefikBundle starts a minimal stand-in for the Compose bundle with
// the real Traefik: PostgreSQL on the services network and Traefik on the
// edge network, publishing its HTTP entry point on a loopback port and
// reading its dynamic configuration from a file the test writes (in the
// bundle it polls the core instead). It returns the project, the directory
// of the dynamic configuration and Traefik's host port.
func startTraefikBundle(t *testing.T) (project, dynamicDir string, port int) {
	t.Helper()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	project = "fgj" + hex.EncodeToString(b)
	dir := t.TempDir()
	dynamicDir = filepath.Join(dir, "dynamic")
	if err := os.Mkdir(dynamicDir, 0o755); err != nil { //nolint:gosec // read by Traefik
		t.Fatal(err)
	}
	file := filepath.Join(dir, "compose.yaml")
	bundle := fmt.Sprintf(`name: %s
services:
  postgres:
    image: %s
    environment: {POSTGRES_USER: orglane, POSTGRES_DB: orglane, POSTGRES_PASSWORD: bundle-test}
    networks: [database, services]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U orglane -d orglane"]
      interval: 1s
      retries: 60
  traefik:
    image: %s
    command:
      - --entrypoints.web.address=:80
      - --providers.file.directory=/etc/traefik/dynamic
      - --providers.file.watch=true
      - --ping=true
      - --log.level=ERROR
    volumes:
      - %s:/etc/traefik/dynamic:ro
    ports: ["127.0.0.1::80"]
    networks: [edge]
    healthcheck:
      test: ["CMD", "traefik", "healthcheck", "--ping"]
      interval: 1s
      retries: 60
networks:
  edge: {}
  database: {internal: true}
  services: {internal: true}
`, project, postgresImage, traefikImage, dynamicDir)
	if err := os.WriteFile(file, []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", project, "-f", file, "down", "--volumes", "--remove-orphans").CombinedOutput() //nolint:gosec // test commands
		if err != nil {
			t.Logf("bundle down: %v\n%s", err, out)
		}
	})
	docker(t, "compose", "-p", project, "-f", file, "up", "--detach", "--wait")
	addr := docker(t, "compose", "-p", project, "-f", file, "port", "traefik", "80")
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("traefik port %q: %v", addr, err)
	}
	port, _ = strconv.Atoi(p)
	return project, dynamicDir, port
}

// writeRoutes writes the core's Traefik configuration (ingress.Build) for
// the routes into Traefik's file provider. JSON is valid YAML.
func writeRoutes(t *testing.T, dir string, routes []ingress.AppRoute) {
	t.Helper()
	cfg, err := config.Load("", func(k string) (string, bool) {
		v, ok := map[string]string{
			"ORGLANE_DATABASE_URL":           "postgres://orglane@postgres:5432/orglane",
			"ORGLANE_DOMAIN_BASE":            "orglane.localhost",
			"ORGLANE_DOMAIN_SCHEME":          "http",
			"ORGLANE_ENVIRONMENT":            "development",
			"ORGLANE_INGRESS_MODE":           "local",
			"ORGLANE_SERVER_INTERNAL_LISTEN": ":9090",
			"ORGLANE_INGRESS_UPSTREAM_URL":   "http://orglane:8080",
		}[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ingress.Build(cfg, routes))
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".orglane.yaml.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil { //nolint:gosec // read by Traefik
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "orglane.yaml")); err != nil {
		t.Fatal(err)
	}
}

// viaTraefik is an HTTP client that sends every request to Traefik's
// published port, whatever the URL's host: the host decides the route, as
// for a browser that resolves *.localhost to loopback.
func viaTraefik(port int) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, "127.0.0.1:"+strconv.Itoa(port))
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// forgejoEnv is the core's provider.Env for the adapter: fixed credentials
// and the client through Traefik.
type forgejoEnv struct {
	creds  map[string][]byte
	client *http.Client
}

func (e *forgejoEnv) Credential(_ context.Context, name string) ([]byte, error) {
	if v, ok := e.creds[name]; ok {
		return v, nil
	}
	return nil, provider.ErrNotFound
}

func (e *forgejoEnv) Credentials(ctx context.Context, name string) ([][]byte, error) {
	v, err := e.Credential(ctx, name)
	return [][]byte{v}, err
}

func (e *forgejoEnv) HTTPClient() *http.Client { return e.client }
func (*forgejoEnv) Logger() *slog.Logger       { return slog.New(slog.DiscardHandler) }
func (*forgejoEnv) Now() time.Time             { return time.Now() }
func (*forgejoEnv) WorkspaceURL() *url.URL {
	return &url.URL{Scheme: "http", Host: "orglane.localhost"}
}
func (*forgejoEnv) IssuerURL() *url.URL { return &url.URL{Scheme: "http", Host: "orglane.localhost"} }
func (*forgejoEnv) WebhookURL(uuid.UUID) *url.URL {
	return &url.URL{Scheme: "http", Host: "orglane.localhost"}
}

// dropResponse forwards a request and then loses the answer of the first
// one that matches: Forgejo committed the change, the caller never hears.
type dropResponse struct {
	next   http.RoundTripper
	method string
	path   string
	armed  bool
}

func (d *dropResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.next.RoundTrip(r)
	if err == nil && d.armed && r.Method == d.method && r.URL.Path == d.path {
		d.armed = false
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, errors.New("connection reset (test)")
	}
	return resp, err
}

func forgejoSpec(id uuid.UUID, gen int64, sshPort int, secrets map[string]string, state string) *agentapi.Spec {
	return &agentapi.Spec{
		InstanceID: id,
		Generation: gen,
		Package:    agentapi.PackageRef{ID: "forgejo", Version: forgejopkg.Current},
		State:      state,
		Hosts:      map[string]string{"workspace": "orglane.localhost", "web": forgejoHost},
		URLs: agentapi.URLs{
			Workspace: "http://orglane.localhost", Issuer: "http://orglane.localhost",
			Instance: "http://" + forgejoHost, Webhook: "http://orglane.localhost/api/v1/webhooks/" + id.String(),
		},
		Ingress:  agentapi.IngressLocal,
		Settings: map[string]any{forgejo.SettingSSHPort: sshPort},
		Secrets:  secrets,
	}
}

func forgejoSecrets() map[string]string {
	return map[string]string{
		forgejo.CredentialDBPassword:    crypto.RandomToken(""),
		forgejo.CredentialSecretKey:     crypto.RandomToken(""),
		forgejo.CredentialAdminPassword: crypto.RandomToken(""),
	}
}

// sshBanner reads the SSH identification line of a server.
func sshBanner(port int) (string, error) {
	c, err := (&net.Dialer{Timeout: 3 * time.Second}).Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return bufio.NewReader(c).ReadString('\n')
}

// apiGet calls the Forgejo API through Traefik with basic auth or a token
// and returns the status.
func apiGet(t *testing.T, c *http.Client, path, user, password, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+forgejoHost+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestForgejoPackage installs the Forgejo package with the real agent on
// PostgreSQL and reaches it through Traefik with the routes the core
// builds; then it runs the adapter's Lifecycle against it (health, setup,
// the admin token with a lost answer), checks Git over SSH on the published
// port, changes settings and secrets, removes and reinstalls it with its
// data, and purges it so the key starts empty (docs/specs/agent.md
// "Testing", providers.md "Keys and removal").
func TestForgejoPackage(t *testing.T) {
	if runtime.GOOS == "linux" && os.Geteuid() != 0 {
		// Forgejo runs as uid 1000 and needs its data directory to be its
		// own: only an agent running as root (as in the bundle) sets the
		// owner. make test-compose installs Forgejo with the real agent
		// container in CI.
		t.Skip("needs root on Linux: the agent sets the owner of Forgejo's data directory only as root")
	}
	project, dynamicDir, traefikPort := startTraefikBundle(t)
	registry, err := packages.NewRegistry(forgejopkg.FS)
	if err != nil {
		t.Fatal(err)
	}
	e := startAgent(t, project, registry)
	const key = "git"
	dir := e.agent.instanceDir(key)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", packages.InstanceProject(project, key), "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
	client := viaTraefik(traefikPort)
	adapter := forgejo.New()
	id := uuid.Must(uuid.NewV7())
	secrets := forgejoSecrets()
	sshPort := freePort(t)
	env := &forgejoEnv{client: client, creds: map[string][]byte{forgejo.CredentialAdminPassword: []byte(secrets[forgejo.CredentialAdminPassword])}}
	// The core reaches the upstream on the edge network; the test reaches
	// it through Traefik under the public name.
	inst := provider.Instance{
		ID: id, Key: key, Provider: forgejo.ID, Management: provider.Managed,
		BaseURL: &url.URL{Scheme: "http", Host: forgejoHost}, Upstream: &url.URL{Scheme: "http", Host: forgejoHost},
		Settings: map[string]any{forgejo.SettingOrganization: "orglane", forgejo.SettingSSHPort: int64(sshPort)},
	}

	// The route goes in before the install, as for a reinstall: Traefik
	// answers 502 until the service is up.
	route := ingress.AppRoute{Key: key, Route: "web", Host: forgejoHost, Upstream: "http://" + packages.ServiceAlias(key, "forgejo", false) + ":3000"}
	writeRoutes(t, dynamicDir, []ingress.AppRoute{route})

	start := time.Now()
	e.mustApplyKey(key, forgejoSpec(id, 1, sshPort, secrets, agentapi.StateRunning))
	t.Logf("installed Forgejo %s in %s", forgejopkg.Current, time.Since(start).Round(time.Second))

	// Health through Traefik, then the adapter's checks.
	eventually(t, time.Minute, "Forgejo answers through Traefik", func() bool {
		return apiGet(t, client, "/api/healthz", "", "", "") == http.StatusOK
	})
	if err := adapter.CheckHealth(t.Context(), env, inst); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	// Nobody browses anonymously.
	if code := apiGet(t, client, "/explore/repos", "", "", ""); code != http.StatusSeeOther {
		t.Fatalf("anonymous explore = %d, want a redirect to sign in", code)
	}

	// Setup: the configure hook made the service admin; Setup creates the
	// organization, and running it again changes nothing.
	for range 2 {
		if err := adapter.Setup(t.Context(), env, inst); err != nil {
			t.Fatalf("Setup: %v", err)
		}
	}
	admin, password := forgejo.AdminUsername, secrets[forgejo.CredentialAdminPassword]
	if code := apiGet(t, client, "/api/v1/orgs/orglane", admin, password, ""); code != http.StatusOK {
		t.Fatalf("organization = %d", code)
	}
	if got := e.psql("svc_git", `select visibility from "user" where lower_name = 'orglane'`); got != "1" { // 1: limited
		t.Fatalf("organization visibility = %q, want limited", got)
	}

	// The admin token: minted, used, and minted again after a lost answer.
	r, err := adapter.ApplyCredential(t.Context(), env, inst, forgejo.CredentialAdminToken, 1, nil)
	if err != nil || r.Outcome != provider.RotationAcknowledged {
		t.Fatalf("mint: %+v, %v", r, err)
	}
	token1 := string(r.Value)
	if code := apiGet(t, client, "/api/v1/user", "", "", token1); code != http.StatusOK {
		t.Fatalf("the minted token does not work: %d", code)
	}
	lossy := *client
	lossy.Transport = &dropResponse{next: client.Transport, method: http.MethodPost, path: "/api/v1/users/" + admin + "/tokens", armed: true}
	lossyEnv := &forgejoEnv{client: &lossy, creds: env.creds}
	if r, err := adapter.ApplyCredential(t.Context(), lossyEnv, inst, forgejo.CredentialAdminToken, 2, nil); err == nil || r.Outcome != provider.RotationUnknown {
		t.Fatalf("lost answer: %+v, %v (want unknown)", r, err)
	}
	if got := e.psql("svc_git", `select count(*) from access_token where name = 'orglane-2'`); got != "1" {
		t.Fatalf("the lost mint did not commit (%s tokens): the fault is not realistic", got)
	}
	r, err = adapter.ApplyCredential(t.Context(), lossyEnv, inst, forgejo.CredentialAdminToken, 2, nil)
	if err != nil || r.Outcome != provider.RotationAcknowledged {
		t.Fatalf("retry after the lost answer: %+v, %v", r, err)
	}
	token2 := string(r.Value)
	if got := e.psql("svc_git", `select count(*) from access_token where name = 'orglane-2'`); got != "1" {
		t.Fatalf("tokens of version 2 after the retry: %s, want 1", got)
	}
	for tok, want := range map[string]int{token1: http.StatusOK, token2: http.StatusOK} {
		if code := apiGet(t, client, "/api/v1/user", "", "", tok); code != want {
			t.Fatalf("token: %d, want %d", code, want)
		}
	}

	// Git over SSH on the published port, bound to loopback in local mode.
	eventually(t, 30*time.Second, "the SSH port answers", func() bool {
		b, err := sshBanner(sshPort)
		return err == nil && strings.HasPrefix(b, "SSH-2.0-")
	})

	// A new SSH port and a new admin password: re-rendered, the configure
	// hook resets the password, the old one stops working.
	secrets2 := forgejoSecrets()
	secrets2[forgejo.CredentialDBPassword] = secrets[forgejo.CredentialDBPassword]
	secrets2[forgejo.CredentialSecretKey] = secrets[forgejo.CredentialSecretKey]
	sshPort2 := freePort(t)
	e.mustApplyKey(key, forgejoSpec(id, 2, sshPort2, secrets2, agentapi.StateRunning))
	eventually(t, time.Minute, "Forgejo answers after the change", func() bool {
		return apiGet(t, client, "/api/healthz", "", "", "") == http.StatusOK
	})
	if code := apiGet(t, client, "/api/v1/user", admin, secrets2[forgejo.CredentialAdminPassword], ""); code != http.StatusOK {
		t.Fatalf("the new admin password does not work: %d", code)
	}
	if code := apiGet(t, client, "/api/v1/user", admin, password, ""); code != http.StatusUnauthorized {
		t.Fatalf("the old admin password still works: %d", code)
	}
	eventually(t, 30*time.Second, "SSH on the new port", func() bool {
		b, err := sshBanner(sshPort2)
		return err == nil && strings.HasPrefix(b, "SSH-2.0-")
	})
	if _, err := sshBanner(sshPort); err == nil {
		t.Fatal("the old SSH port is still published")
	}
	env.creds[forgejo.CredentialAdminPassword] = []byte(secrets2[forgejo.CredentialAdminPassword])

	logIdleMemory(t, packages.InstanceProject(project, key))

	// Remove: containers go, data and database stay.
	e.mustApplyKey(key, forgejoSpec(id, 3, sshPort2, secrets2, agentapi.StateAbsent))
	if c := e.containers(key); len(c) != 0 {
		t.Fatalf("containers after remove: %v", c)
	}
	if code := apiGet(t, client, "/api/healthz", "", "", ""); code != http.StatusBadGateway {
		t.Fatalf("a removed instance answers %d through its route, want 502", code)
	}

	// Reinstall: same instance, same secrets, same data.
	e.mustApplyKey(key, forgejoSpec(id, 4, sshPort2, secrets2, agentapi.StateRunning))
	eventually(t, time.Minute, "Forgejo answers after the reinstall", func() bool {
		return apiGet(t, client, "/api/healthz", "", "", "") == http.StatusOK
	})
	if code := apiGet(t, client, "/api/v1/orgs/orglane", "", "", token2); code != http.StatusOK {
		t.Fatalf("organization after the reinstall = %d", code)
	}
	if err := adapter.Setup(t.Context(), env, inst); err != nil {
		t.Fatalf("Setup after the reinstall: %v", err)
	}

	// Purge: the directory, the database and the role go.
	purge := forgejoSpec(id, 5, sshPort2, secrets2, agentapi.StateAbsent)
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
	secrets3 := forgejoSecrets()
	e.mustApplyKey(key, forgejoSpec(id2, 1, sshPort2, secrets3, agentapi.StateRunning))
	eventually(t, time.Minute, "the new instance answers", func() bool {
		return apiGet(t, client, "/api/healthz", "", "", "") == http.StatusOK
	})
	if code := apiGet(t, client, "/api/v1/orgs/orglane", forgejo.AdminUsername, secrets3[forgejo.CredentialAdminPassword], ""); code != http.StatusNotFound {
		t.Fatalf("organization of the new instance = %d, want 404", code)
	}
	if code := apiGet(t, client, "/api/v1/user", "", "", token2); code != http.StatusUnauthorized {
		t.Fatalf("an old token works on the new instance: %d", code)
	}
	e.mustApplyKey(key, func() *agentapi.Spec {
		s := forgejoSpec(id2, 2, sshPort2, secrets3, agentapi.StateAbsent)
		s.Purge = true
		return s
	}())
}

// logIdleMemory logs the memory use of the project's containers after a
// short idle period (reported in the pull request; ADR 0009).
func logIdleMemory(t *testing.T, project string) {
	t.Helper()
	time.Sleep(20 * time.Second)
	ids := strings.Fields(docker(t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+project))
	for _, id := range ids {
		t.Logf("idle memory: %s", docker(t, "stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}", id))
	}
}
