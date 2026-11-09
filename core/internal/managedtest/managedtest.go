//go:build integration

// Package managedtest runs managed services for integration tests with real
// Docker: a minimal stand-in for the Compose bundle (PostgreSQL on the
// services network and the real Traefik on the edge network, labeled like
// the bundle), Traefik routes from the core's ingress configuration, an
// HTTP client that reaches services through Traefik, and a way to run a
// test as root, as the agent runs in the bundle.
//
// It does not import the agent, so the agent's own tests can use it.
package managedtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/ingress"
)

// Images of the bundle (deploy/compose), pinned like there.
const (
	PostgresImage = "postgres:18.6-alpine3.24@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5"
	TraefikImage  = "traefik:v3.7.17@sha256:30ff62491aacdb862ceb8c9edcf2627da638d7deafdd4b6dab32fe34f8aa3e90"
)

// rootEnv marks a test binary that RunAsRoot started.
const rootEnv = "ORGLANE_TEST_AS_ROOT"

// RunAsRoot makes a test run as root on Linux, as the agent does in the
// bundle: managed services run as their own users (Forgejo: uid 1000), and
// only a root agent can give them their data directories, or delete what
// they wrote there on purge. On Linux without root it runs the test again
// in a child process of the same test binary through passwordless sudo
// (CI runners have it), streams its output and returns true: the caller
// returns at once, the child did the work. Without sudo the test is
// skipped. Elsewhere (macOS: Docker Desktop maps file ownership) and as
// root it returns false and the test runs here.
func RunAsRoot(t *testing.T) bool {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		return false
	}
	if os.Getenv(rootEnv) != "" {
		t.Fatal("managedtest: the test was started as root but does not run as root")
	}
	if err := exec.CommandContext(t.Context(), "sudo", "-n", "true").Run(); err != nil {
		t.Skip("needs root (the agent sets the owners of data directories only as root) or passwordless sudo to run as root")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	timeout := 20 * time.Minute
	if d, ok := t.Deadline(); ok {
		timeout = time.Until(d) - 30*time.Second
	}
	args := []string{"-n", "--preserve-env", "--", "env", rootEnv + "=1", exe,
		"-test.run", "^" + regexp.QuoteMeta(t.Name()) + "$", "-test.v", "-test.count=1", "-test.timeout", timeout.String()}
	cmd := exec.CommandContext(t.Context(), "sudo", args...) //nolint:gosec // the test binary itself
	cmd.Stdout, cmd.Stderr = t.Output(), t.Output()
	if err := cmd.Run(); err != nil {
		t.Fatalf("the test as root failed: %v", err)
	}
	return true
}

// Docker runs a Docker command and returns its trimmed output.
func Docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput() //nolint:gosec // test commands
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Bundle is a running bundle stand-in.
type Bundle struct {
	// Project is the bundle's Compose project; its networks are
	// <Project>_edge and <Project>_services.
	Project string
	// TraefikPort is Traefik's HTTP entry point on 127.0.0.1.
	TraefikPort int
	dynamicDir  string
	// withCore: Traefik also polls a core, which serves the workspace.
	withCore bool
}

// BusyboxImage serves the stand-in workspace (StartBundle without a core).
const BusyboxImage = "busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// WorkspaceHost is the workspace host of the bundle in local mode.
const WorkspaceHost = "orglane.localhost"

// StartBundle starts PostgreSQL and the real Traefik. Traefik reads its
// dynamic configuration from a file (in the bundle it polls the core):
// see Routes. There is no core: a stand-in serves a static OpenID
// discovery document as the workspace, so a package that configures
// sign-in with Orglane at install (Forgejo) can be installed. Tests that
// sign in use StartBundleWithCore.
func StartBundle(t *testing.T) *Bundle {
	t.Helper()
	return startBundle(t, "")
}

// StartBundleWithCore starts the bundle for a core that runs in the test
// process and listens on the host: Traefik polls the core's internal
// listener (internalPort) for its configuration, exactly as in the bundle,
// and carries the workspace host as its alias on the edge network, so
// managed services reach Orglane by its public name. The core's
// ingress.upstream_url must be CoreURL(publicPort).
func StartBundleWithCore(t *testing.T, internalPort int) *Bundle {
	t.Helper()
	return startBundle(t, "http://host.docker.internal:"+strconv.Itoa(internalPort)+"/traefik/config")
}

// CoreURL is the URL of a core listening on the host, as containers reach
// it.
func CoreURL(port int) string { return "http://host.docker.internal:" + strconv.Itoa(port) }

// CoreListenHost is the address a core in the test process listens on so
// containers reach it: all interfaces on Linux (the Docker bridge), loopback
// elsewhere (Docker Desktop forwards host.docker.internal to it).
func CoreListenHost() string {
	if runtime.GOOS == "linux" {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

func startBundle(t *testing.T, providerURL string) *Bundle {
	t.Helper()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	project := "mgt" + hex.EncodeToString(b)
	dir := t.TempDir()
	dynamicDir := filepath.Join(dir, "dynamic")
	if err := os.Mkdir(dynamicDir, 0o755); err != nil { //nolint:gosec // read by Traefik
		t.Fatal(err)
	}
	provider := "      - --providers.file.directory=/etc/traefik/dynamic\n      - --providers.file.watch=true\n"
	traefikNet := "    networks: [edge]\n"
	workspace := ""
	if providerURL != "" {
		// The core's routes, plus the file for routes a test adds (the
		// upstream aliases the core reaches on the edge network).
		provider += "      - --providers.http.endpoint=" + providerURL + "\n      - --providers.http.pollInterval=1s\n"
		traefikNet = "    extra_hosts: [\"host.docker.internal:host-gateway\"]\n    networks:\n      edge:\n        aliases: [" + WorkspaceHost + "]\n"
	} else {
		www := filepath.Join(dir, "www")
		if err := os.MkdirAll(filepath.Join(www, ".well-known"), 0o755); err != nil { //nolint:gosec // read by the stand-in
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(www, ".well-known", "openid-configuration"), []byte(stubDiscovery), 0o644); err != nil { //nolint:gosec // public
			t.Fatal(err)
		}
		workspace = fmt.Sprintf(`  workspace:
    image: %s
    command: ["httpd", "-f", "-p", "80", "-h", "/www"]
    volumes: ["%s:/www:ro"]
    networks:
      edge:
        aliases: [%s]
`, BusyboxImage, www, WorkspaceHost)
	}
	file := filepath.Join(dir, "compose.yaml")
	bundle := fmt.Sprintf(`name: %s
services:
  postgres:
    image: %s
    environment: {POSTGRES_USER: orglane, POSTGRES_DB: orglane, POSTGRES_PASSWORD: bundle-test}
    networks: [database, services]
    healthcheck:
      # Over TCP: the image's temporary server during initialization only
      # listens on the socket, and a socket check would pass before the
      # restart that follows it (#78).
      test: ["CMD-SHELL", "pg_isready -h 127.0.0.1 -U orglane -d orglane"]
      interval: 1s
      retries: 60
  traefik:
    image: %s
    command:
      - --entrypoints.web.address=:80
%s      - --ping=true
      - --log.level=ERROR
    volumes:
      - %s:/etc/traefik/dynamic:ro
    ports: ["127.0.0.1::80"]
%s    healthcheck:
      test: ["CMD", "traefik", "healthcheck", "--ping"]
      interval: 1s
      retries: 60
%snetworks:
  edge: {}
  database: {internal: true}
  services: {internal: true}
`, project, PostgresImage, TraefikImage, provider, dynamicDir, traefikNet, workspace)
	if err := os.WriteFile(file, []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", project, "-f", file, "down", "--volumes", "--remove-orphans").CombinedOutput() //nolint:gosec // test commands
		if err != nil {
			t.Logf("bundle down: %v\n%s", err, out)
		}
	})
	Docker(t, "compose", "-p", project, "-f", file, "up", "--detach", "--wait")
	addr := Docker(t, "compose", "-p", project, "-f", file, "port", "traefik", "80")
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("traefik port %q: %v", addr, err)
	}
	port, _ := strconv.Atoi(p)
	return &Bundle{Project: project, TraefikPort: port, dynamicDir: dynamicDir, withCore: providerURL != ""}
}

// stubDiscovery is the stand-in workspace's discovery document: enough for
// a service to save a sign-in source, not to sign anyone in.
const stubDiscovery = `{"issuer":"http://orglane.localhost","authorization_endpoint":"http://orglane.localhost/oidc/authorize",` +
	`"token_endpoint":"http://orglane.localhost/oidc/token","userinfo_endpoint":"http://orglane.localhost/oidc/userinfo",` +
	`"jwks_uri":"http://orglane.localhost/oidc/keys","response_types_supported":["code"],"subject_types_supported":["public"],` +
	`"id_token_signing_alg_values_supported":["RS256"]}`

// LocalConfig is a core configuration in local ingress mode with the base
// domain orglane.localhost, as with managed services in local mode.
func LocalConfig(t *testing.T) *config.Config {
	t.Helper()
	vars := map[string]string{
		"ORGLANE_DATABASE_URL":           "postgres://orglane@postgres:5432/orglane",
		"ORGLANE_DOMAIN_BASE":            "orglane.localhost",
		"ORGLANE_DOMAIN_SCHEME":          "http",
		"ORGLANE_ENVIRONMENT":            "development",
		"ORGLANE_INGRESS_MODE":           "local",
		"ORGLANE_SERVER_INTERNAL_LISTEN": ":9090",
		"ORGLANE_INGRESS_UPSTREAM_URL":   "http://orglane:8080",
	}
	cfg, err := config.Load("", func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Routes writes the core's Traefik configuration (ingress.Build) for the
// routes into Traefik's file provider. JSON is valid YAML. With a core
// (StartBundleWithCore) only the app routes are written: the core serves
// its own.
func (b *Bundle) Routes(t *testing.T, routes ...ingress.AppRoute) {
	t.Helper()
	d := ingress.Build(LocalConfig(t), routes)
	if b.withCore {
		// The core serves the workspace router itself: a second one here
		// would compete with it for the same host.
		delete(d.HTTP.Routers, ingress.RouterWorkspace)
		delete(d.HTTP.Routers, ingress.RouterWorkspaceRedirect)
		delete(d.HTTP.Services, ingress.ServiceCore)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(b.dynamicDir, ".orglane.yaml.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil { //nolint:gosec // read by Traefik
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(b.dynamicDir, "orglane.yaml")); err != nil {
		t.Fatal(err)
	}
}

// Client returns an HTTP client that sends every request to Traefik,
// whatever the URL's host: the host decides the route, as for a browser
// that resolves *.localhost to loopback. It never follows redirects.
func (b *Bundle) Client() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, "127.0.0.1:"+strconv.Itoa(b.TraefikPort))
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Psql runs SQL in a database of the bundle's PostgreSQL and returns the
// unaligned output.
func (b *Bundle) Psql(t *testing.T, db, sql string) string {
	t.Helper()
	id := Docker(t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+b.Project, "--filter", "label=com.docker.compose.service=postgres")
	return Docker(t, "exec", id, "psql", "-U", "orglane", "-d", db, "-Atc", sql)
}

// Eventually polls ok every half second until it holds or timeout passes.
func Eventually(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
