//go:build integration

// Package compose starts the Compose bundle in deploy/compose (Traefik, the
// core image built from this repository, PostgreSQL) and checks it from the
// outside, through Traefik:
//
//   - local mode: plain HTTP routing by Host header to /healthz and /readyz;
//   - domain mode: certificate issuance against Pebble, Let's Encrypt's ACME
//     test server, then HTTPS, HSTS and the HTTP to HTTPS redirect;
//   - managed services (profile "managed"): the agent image built from this
//     repository starts, the core reaches it over the shared socket, the core
//     container has neither the Docker socket nor a Docker CLI, managed
//     services on the edge network reach the workspace by its name but not
//     the core's internal listener.
//
// Needs Docker with Compose v2. Run with `make test-compose` or
// `go test -tags integration ./tests/compose/`.
package compose

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orglane/orglane/tests/demotest"
)

const (
	// image is the core image built from this repository for the tests.
	image = "orglane:compose-test"
	// agentImage is the agent image (target agent) built from it.
	agentImage = "orglane-agent:compose-test"
	// testDomain resolves to Traefik inside the Compose network only (see
	// deploy/compose/pebble/compose.pebble.yaml).
	testDomain = "work.orglane.test"
)

var (
	repoRoot   = filepath.Join("..", "..")
	composeDir = filepath.Join(repoRoot, "deploy", "compose")
)

func TestMain(m *testing.M) {
	if err := buildImage(); err != nil {
		fmt.Fprintf(os.Stderr, "compose tests: build %s (is Docker running?): %v\n", image, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func buildImage() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	for _, args := range [][]string{{"-t", image}, {"--target", "agent", "-t", agentImage}} {
		args = append(append([]string{"build", "--build-arg", "VERSION=compose-test"}, args...), repoRoot)
		cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed docker build of this repository
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w\n%s", err, out)
		}
	}
	return nil
}

// lifecycleMu lets one bundle start or stop at a time. On Docker Desktop
// (macOS), a PostgreSQL container initializing its bind-mounted data
// directory while another bundle starts, or deletes its data directory,
// sometimes fails with "data directory has wrong ownership", the more so
// under the load of the whole integration suite (make test). The tests still
// run in parallel while their bundles are up.
var lifecycleMu sync.Mutex

// bundle is one Compose project started for a test.
type bundle struct {
	t       *testing.T
	project string
	files   []string
	env     []string
	dataDir string
}

// startBundle runs `docker compose up --wait` for the given files (relative
// to deploy/compose) with env on top of the test defaults, and tears the
// project down when the test ends.
func startBundle(t *testing.T, files []string, env ...string) *bundle {
	t.Helper()
	dataDir, err := os.MkdirTemp("", "orglane-compose-")
	if err != nil {
		t.Fatal(err)
	}
	// Docker Desktop creates missing bind-mount directories below a symlink
	// (macOS: /var/folders -> /private/var/folders) inside its VM instead of
	// on the host.
	if dataDir, err = filepath.EvalSymlinks(dataDir); err != nil {
		t.Fatal(err)
	}
	// An empty env file keeps a developer's deploy/compose/.env out of the
	// test.
	// t.TempDir, not the data directory: cleanup may only partly delete the
	// data directory first, and later compose commands still need the file.
	envFile := filepath.Join(t.TempDir(), "empty.env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := &bundle{
		t:       t,
		project: "orglane-test-" + randomHex(t, 4),
		files:   files,
		dataDir: dataDir,
		env: append([]string{
			"--env-file=" + envFile,
			"ORGLANE_IMAGE=" + image,
			"ORGLANE_DATA_DIR=" + dataDir,
			"ORGLANE_POSTGRES_PASSWORD=" + randomHex(t, 16),
			"ORGLANE_IDENTITY_MASTER_SECRET=" + randomHex(t, 32),
			// Random localhost ports: tests run in parallel and must not
			// need 80 and 443.
			"ORGLANE_HTTP_PUBLISH=127.0.0.1:",
			"ORGLANE_HTTPS_PUBLISH=127.0.0.1:",
		}, env...),
	}
	t.Cleanup(b.down)
	lifecycleMu.Lock()
	out, err := b.compose("up", "--detach", "--wait", "--wait-timeout", "180")
	lifecycleMu.Unlock()
	if err != nil {
		t.Fatalf("compose up: %v\n%s", err, out)
	}
	return b
}

// compose runs docker compose for the bundle and returns its combined output.
func (b *bundle) compose(args ...string) (string, error) {
	full := []string{"compose", "--project-name", b.project}
	var vars []string
	for _, e := range b.env {
		if f, ok := strings.CutPrefix(e, "--env-file="); ok {
			full = append(full, "--env-file", f)
			continue
		}
		vars = append(vars, e)
	}
	for _, f := range b.files {
		full = append(full, "--file", f)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append(full, args...)...) //nolint:gosec // fixed docker compose invocation
	cmd.Dir = composeDir
	cmd.Env = append(os.Environ(), vars...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (b *bundle) down() {
	t := b.t
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if t.Failed() {
		logs, _ := b.compose("logs", "--no-color", "--timestamps")
		t.Logf("compose logs:\n%s", logs)
	}
	if out, err := b.compose("down", "--volumes", "--remove-orphans", "--timeout", "20"); err != nil {
		t.Errorf("compose down: %v\n%s", err, out)
	}
	if err := os.RemoveAll(b.dataDir); err != nil {
		// On Linux the containers leave files owned by their own users.
		b.removeAsRoot()
	}
}

// removeAsRoot deletes the data directory from a container, using the
// bundle's PostgreSQL image, which is already pulled.
func (b *bundle) removeAsRoot() {
	t := b.t
	images, err := b.compose("config", "--images")
	if err != nil {
		t.Errorf("compose config --images: %v\n%s", err, images)
		return
	}
	for img := range strings.FieldsSeq(images) {
		if !strings.HasPrefix(img, "postgres:") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--volume", b.dataDir+":/data", //nolint:gosec // fixed cleanup command
			"--entrypoint", "sh", img, "-c", "rm -rf /data/* /data/.[!.]*").CombinedOutput()
		if err != nil {
			t.Errorf("clean %s: %v\n%s", b.dataDir, err, out)
		}
		if err := os.RemoveAll(b.dataDir); err != nil {
			t.Errorf("remove %s: %v", b.dataDir, err)
		}
		return
	}
	t.Errorf("no postgres image in the bundle to clean %s with", b.dataDir)
}

// port returns the host address published for a container port.
func (b *bundle) port(service string, containerPort int) string {
	b.t.Helper()
	out, err := b.compose("port", service, fmt.Sprint(containerPort))
	if err != nil {
		b.t.Fatalf("compose port %s %d: %v\n%s", service, containerPort, err, out)
	}
	return strings.TrimSpace(out)
}

// inspect runs docker inspect with a Go template on a service's container.
func (b *bundle) inspect(service, format string) string {
	b.t.Helper()
	id, err := b.compose("ps", "--quiet", service)
	if err != nil || strings.TrimSpace(id) == "" {
		b.t.Fatalf("compose ps %s: %v\n%s", service, err, id)
	}
	ctx, cancel := context.WithTimeout(b.t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, strings.TrimSpace(id)).CombinedOutput() //nolint:gosec // fixed docker inspect
	if err != nil {
		b.t.Fatalf("docker inspect %s: %v\n%s", service, err, out)
	}
	return strings.TrimSpace(string(out))
}

// containerIP returns a service's address on one of the bundle's networks.
func (b *bundle) containerIP(service, network string) string {
	b.t.Helper()
	return b.inspect(service, fmt.Sprintf(`{{(index .NetworkSettings.Networks %q).IPAddress}}`, b.project+"_"+network))
}

// containerEnv returns an environment variable of a service's container.
func (b *bundle) containerEnv(service, name string) string {
	b.t.Helper()
	var env []string
	if err := json.Unmarshal([]byte(b.inspect(service, "{{json .Config.Env}}")), &env); err != nil {
		b.t.Fatalf("environment of %s: %v", service, err)
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// psql runs a query in the bundle's database and returns the trimmed output.
func (b *bundle) psql(query string) string {
	b.t.Helper()
	out, err := b.compose("exec", "-T", "postgres", "psql", "-U", "orglane", "-d", "orglane", "-tAc", query)
	if err != nil {
		b.t.Fatalf("psql %q: %v\n%s", query, err, out)
	}
	return strings.TrimSpace(out)
}

// publishedPorts lists the host ports a service publishes.
func (b *bundle) publishedPorts(service string) []int {
	b.t.Helper()
	out, err := b.compose("ps", "--format", "{{json .Publishers}}", service)
	if err != nil {
		b.t.Fatalf("compose ps %s: %v\n%s", service, err, out)
	}
	var publishers []struct{ PublishedPort int }
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &publishers); err != nil {
		b.t.Fatalf("compose ps %s: %v\n%s", service, err, out)
	}
	var ports []int
	for _, p := range publishers {
		if p.PublishedPort != 0 {
			ports = append(ports, p.PublishedPort)
		}
	}
	return ports
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

// request sends a GET with the given Host header; err is set if the request
// failed.
func request(t *testing.T, client *http.Client, url, host string) (response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return response{code: resp.StatusCode, header: resp.Header, body: body}, err
}

// waitFor retries request until it answers 200 or the timeout expires.
// Traefik picks up routes on its first poll of the core and, in domain mode,
// serves the workspace once the certificate is issued.
func waitFor(t *testing.T, client *http.Client, url, host string, timeout time.Duration) response {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := request(t, client, url, host)
		if err == nil && resp.code == http.StatusOK {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s (Host %s) did not answer 200 within %s: last %d %s, error %v",
				url, host, timeout, resp.code, resp.body, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// checkSecurityHeaders checks the headers middleware generated by the core.
func checkSecurityHeaders(t *testing.T, h http.Header, wantHSTS bool) {
	t.Helper()
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(h.Get("Permissions-Policy"), "camera=()") {
		t.Errorf("Permissions-Policy = %q", h.Get("Permissions-Policy"))
	}
	hsts := h.Get("Strict-Transport-Security")
	switch {
	case wantHSTS && hsts != "max-age=31536000":
		t.Errorf("Strict-Transport-Security = %q, want max-age=31536000", hsts)
	case !wantHSTS && hsts != "":
		t.Errorf("Strict-Transport-Security = %q, want none in local mode", hsts)
	}
}

func checkHealth(t *testing.T, path string, resp response) {
	t.Helper()
	var health struct {
		Status string
		Checks []struct{ Name, Status string }
	}
	if err := json.Unmarshal(resp.body, &health); err != nil || health.Status != "ok" {
		t.Errorf("%s = %d %s, want status ok", path, resp.code, resp.body)
	}
	if path == "/readyz" && len(health.Checks) == 0 {
		t.Errorf("/readyz lists no checks: %s", resp.body)
	}
}

func TestLocalMode(t *testing.T) {
	t.Parallel()
	// Each parallel test needs its own edge subnet.
	b := startBundle(t, []string{"compose.yaml"}, "ORGLANE_INGRESS_MODE=local", "ORGLANE_EDGE_NETWORK_PREFIX=10.231.101")
	web := "http://" + b.port("traefik", 80)
	client := &http.Client{CheckRedirect: noRedirects}

	resp := waitFor(t, client, web+"/healthz", "localhost", time.Minute)
	checkHealth(t, "/healthz", resp)
	checkSecurityHeaders(t, resp.header, false)
	resp = waitFor(t, client, web+"/readyz", "localhost", 30*time.Second)
	checkHealth(t, "/readyz", resp)

	// The image embeds the web UI.
	if resp, err := request(t, client, web+"/", "localhost"); err != nil || resp.code != http.StatusOK ||
		!strings.HasPrefix(resp.header.Get("Content-Type"), "text/html") || !strings.Contains(string(resp.body), `id="root"`) {
		t.Errorf("GET / = %d %q %v, want the web UI", resp.code, resp.header.Get("Content-Type"), err)
	}
	// The workspace host ignores the port, as browsers send it.
	if resp, err := request(t, client, web+"/healthz", "localhost:8000"); err != nil || resp.code != http.StatusOK {
		t.Errorf("Host localhost:8000 = %d %v, want 200", resp.code, err)
	}
	// Routing is by Host: other hosts get Traefik's 404.
	if resp, err := request(t, client, web+"/healthz", "attacker.example"); err != nil || resp.code != http.StatusNotFound {
		t.Errorf("unknown Host = %d %v, want 404", resp.code, err)
	}
	// The internal listener is neither routed nor published.
	// (The public listener answers unknown paths with the web UI.)
	if resp, err := request(t, client, web+"/traefik/config", "localhost"); err != nil ||
		strings.Contains(string(resp.body), "orglane-workspace") || strings.Contains(resp.header.Get("Content-Type"), "json") {
		t.Errorf("/traefik/config through Traefik = %d %s %v, want no route configuration", resp.code, resp.body, err)
	}
	// Only Traefik publishes ports: the core's listeners stay on the Compose
	// networks.
	for _, service := range []string{"orglane", "postgres"} {
		if published := b.publishedPorts(service); len(published) > 0 {
			t.Errorf("%s publishes ports %v, want none", service, published)
		}
	}
	// The core trusts X-Forwarded-For from Traefik's address only.
	if ip, proxies := b.containerIP("traefik", "edge"), b.containerEnv("orglane", "ORGLANE_SERVER_TRUSTED_PROXIES"); ip == "" || proxies != ip+"/32" {
		t.Errorf("trusted proxies = %q, want Traefik's address %q/32", proxies, ip)
	}
	// Local mode runs the core with a plain-HTTP workspace URL.
	if got := b.containerEnv("orglane", "ORGLANE_DOMAIN_SCHEME"); got != "http" {
		t.Errorf("local mode scheme = %q, want http", got)
	}

	// The core migrated the bundle's PostgreSQL 18 and seeded the built-in
	// roles on start, with UUIDv7 keys from uuidv7().
	migrations, err := filepath.Glob(filepath.Join(repoRoot, "core", "migrations", "*.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("list migrations: %v", err)
	}
	// Versions need not be contiguous while independent migration changes
	// are developed. Check the exact applied set, not the file count.
	versions := make([]int, 0, len(migrations))
	for _, file := range migrations {
		prefix, _, _ := strings.Cut(filepath.Base(file), "_")
		version, err := strconv.Atoi(prefix)
		if err != nil {
			t.Fatalf("migration version in %s: %v", file, err)
		}
		versions = append(versions, version)
	}
	slices.Sort(versions)
	wantVersions := make([]string, 0, len(versions))
	for _, version := range versions {
		wantVersions = append(wantVersions, strconv.Itoa(version))
	}
	if got, want := b.psql("SELECT string_agg(version_id::text, ',' ORDER BY version_id) FROM goose_db_version WHERE is_applied AND version_id > 0"), strings.Join(wantVersions, ","); got != want {
		t.Errorf("applied migration versions = %q, want %q", got, want)
	}
	if got := b.psql("SELECT count(*) > 0 AND bool_and(uuid_extract_version(id) = 7) FROM roles WHERE builtin"); got != "t" {
		t.Errorf("built-in roles seeded with UUIDv7 keys = %q, want t", got)
	}

	// Persistent data lives under ORGLANE_DATA_DIR.
	if _, err := os.Stat(filepath.Join(b.dataDir, "postgres", "18")); err != nil {
		t.Errorf("no PostgreSQL data under the data directory: %v", err)
	}
}

func TestDomainModeWithPebble(t *testing.T) {
	t.Parallel()
	b := startBundle(t, []string{"compose.yaml", "pebble/compose.pebble.yaml"},
		"ORGLANE_ACME_EMAIL=admin@orglane.test", "ORGLANE_EDGE_NETWORK_PREFIX=10.231.102")
	websecure := b.port("traefik", 443)
	web := "http://" + b.port("traefik", 80)

	// Certificates are signed by a root Pebble generates at start; fetch it
	// from its management API, which uses Pebble's static test CA.
	roots := x509.NewCertPool()
	roots.AddCert(pebbleRoot(t, b))
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{
		CheckRedirect: noRedirects,
		Transport: &http.Transport{
			// Resolve the test domain to Traefik's published port.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, websecure)
			},
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		},
	}

	url := "https://" + testDomain
	resp := waitFor(t, client, url+"/healthz", testDomain, 2*time.Minute)
	checkHealth(t, "/healthz", resp)
	checkSecurityHeaders(t, resp.header, true)
	resp = waitFor(t, client, url+"/readyz", testDomain, 30*time.Second)
	checkHealth(t, "/readyz", resp)

	// Plain HTTP is redirected to HTTPS.
	resp, err := request(t, &http.Client{CheckRedirect: noRedirects}, web+"/readyz", testDomain)
	if err != nil || resp.code != http.StatusMovedPermanently || resp.header.Get("Location") != url+"/readyz" {
		t.Errorf("HTTP /readyz = %d Location %q %v, want 301 to %s/readyz", resp.code, resp.header.Get("Location"), err, url)
	}

	if got := b.containerEnv("orglane", "ORGLANE_ENVIRONMENT"); got != "production" {
		t.Errorf("domain mode environment = %q, want production", got)
	}

	// The certificate is stored under ORGLANE_DATA_DIR, so it survives
	// restarts.
	if fi, err := os.Stat(filepath.Join(b.dataDir, "traefik", "acme.json")); err != nil || fi.Size() == 0 {
		t.Errorf("no stored certificate in the data directory: %v", err)
	}
}

// pebbleRoot fetches the root that signs Pebble's certificates.
func pebbleRoot(t *testing.T, b *bundle) *x509.Certificate {
	t.Helper()
	minica, err := os.ReadFile(filepath.Join(composeDir, "pebble", "pebble.minica.pem")) //nolint:gosec // a file of this repository
	if err != nil {
		t.Fatal(err)
	}
	apiRoots := x509.NewCertPool()
	if !apiRoots.AppendCertsFromPEM(minica) {
		t.Fatal("pebble.minica.pem holds no certificate")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: apiRoots, MinVersion: tls.VersionTLS12},
	}}
	resp := waitFor(t, client, "https://"+b.port("pebble", 15000)+"/roots/0", "", 30*time.Second)
	cert, err := parsePEMCert(resp.body)
	if err != nil {
		t.Fatalf("Pebble root: %v\n%s", err, resp.body)
	}
	return cert
}

func parsePEMCert(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// TestManagedProfile starts the bundle with managed services enabled, the way
// install.sh enable-managed does in local mode (workspace orglane.localhost).
func TestManagedProfile(t *testing.T) {
	t.Parallel()
	const (
		prefix    = "10.231.103"
		workspace = "orglane.localhost"
	)
	b := startBundle(t, []string{"compose.yaml"},
		"ORGLANE_INGRESS_MODE=local",
		"ORGLANE_EDGE_NETWORK_PREFIX="+prefix,
		"ORGLANE_DOMAIN_BASE="+workspace,
		"COMPOSE_PROFILES=managed",
		"ORGLANE_AGENT_IMAGE="+agentImage,
		"ORGLANE_AGENT_SECRET="+randomHex(t, 32),
		// Demo mode: one-click sign-in as the owner to install Forgejo
		// through the API (install.sh --mode demo --managed).
		"ORGLANE_DEMO_ENABLED=true",
	)
	// compose up --wait returned, so the agent's healthcheck (a signed
	// request over its socket) passed. The workspace answers on its name.
	web := "http://" + b.port("traefik", 80)
	client := &http.Client{CheckRedirect: noRedirects}
	waitFor(t, client, web+"/readyz", workspace, time.Minute)

	// The agent got the bundle's settings.
	for name, want := range map[string]string{
		"ORGLANE_AGENT_COMPOSE_PROJECT": b.project,
		"ORGLANE_AGENT_DATA_DIR":        b.dataDir,
		"ORGLANE_AGENT_PEER_UID":        "65532",
		"ORGLANE_AGENT_PUBLISH_ADDRESS": "127.0.0.1",
	} {
		if got := b.containerEnv("agent", name); got != want {
			t.Errorf("agent %s = %q, want %q", name, got, want)
		}
	}
	if got := b.inspect("agent", "{{.HostConfig.NetworkMode}} {{.Config.User}} {{.HostConfig.ReadonlyRootfs}}"); got != "none 0:0 true" {
		t.Errorf("agent network mode, user, read-only = %q, want none 0:0 true", got)
	}
	// The agent's image carries the Docker CLI and the Compose plugin, and
	// they reach the daemon through the mounted socket.
	if out, err := b.compose("exec", "-T", "agent", "/usr/local/bin/docker", "compose", "version"); err != nil || !strings.Contains(out, "Docker Compose version") {
		t.Errorf("docker compose in the agent: %v\n%s", err, out)
	}
	if out, err := b.compose("exec", "-T", "agent", "/usr/local/bin/docker", "ps", "--quiet",
		"--filter", "label=com.docker.compose.project="+b.project, "--filter", "label=com.docker.compose.service=postgres"); err != nil || strings.TrimSpace(out) == "" {
		t.Errorf("the agent does not see the bundle's PostgreSQL through the Docker socket: %v\n%s", err, out)
	}

	// The core (uid 65532) reaches the agent over the agent-run volume with
	// the shared secret: the signed health request and the signed answer.
	out, err := b.compose("exec", "-T", "-e", "ORGLANE_AGENT_LISTEN=/run/orglane-agent/agent.sock", "orglane",
		"/usr/local/bin/orglane", "agent", "healthcheck")
	if err != nil || !strings.Contains(out, "agent compose-test ready") {
		t.Errorf("agent health from the core container: %v\n%s", err, out)
	}
	if got := b.containerEnv("orglane", "ORGLANE_AGENT_SOCKET"); got != "/run/orglane-agent/agent.sock" {
		t.Errorf("core ORGLANE_AGENT_SOCKET = %q", got)
	}

	// The core container never has the Docker socket, nor a Docker CLI.
	var mounts []struct{ Source, Destination string }
	if err := json.Unmarshal([]byte(b.inspect("orglane", "{{json .Mounts}}")), &mounts); err != nil {
		t.Fatal(err)
	}
	for _, m := range mounts {
		if strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Destination, "docker.sock") {
			t.Errorf("the core container mounts %s at %s", m.Source, m.Destination)
		}
	}
	for _, path := range []string{"/usr/local/bin/docker", "/usr/bin/docker", "/var/run/docker.sock", "/run/docker.sock"} {
		if out, err := b.compose("exec", "-T", "orglane", path, "--version"); err == nil {
			t.Errorf("the core container has %s:\n%s", path, out)
		}
	}

	// PostgreSQL is on the services network, the core is not.
	if b.containerIP("postgres", "services") == "" {
		t.Error("PostgreSQL is not on the services network")
	}
	var networks map[string]any
	if err := json.Unmarshal([]byte(b.inspect("orglane", "{{json .NetworkSettings.Networks}}")), &networks); err != nil {
		t.Fatal(err)
	}
	if _, ok := networks[b.project+"_services"]; ok {
		t.Error("the core is on the services network")
	}

	// A managed service on the edge network reaches the workspace by its
	// public name (Traefik's alias), but not the core's internal listener,
	// which answers Traefik only.
	pg := ""
	images, err := b.compose("config", "--images")
	if err != nil {
		t.Fatalf("compose config --images: %v\n%s", err, images)
	}
	for img := range strings.FieldsSeq(images) {
		if strings.HasPrefix(img, "postgres:") {
			pg = img
		}
	}
	service := func(url string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", b.project+"_edge", //nolint:gosec // fixed docker run
			"--entrypoint", "wget", pg, "-q", "-O", "-", "-T", "10", url).CombinedOutput()
		return string(out), err
	}
	if out, err := service("http://" + workspace + "/healthz"); err != nil || !strings.Contains(out, `"ok"`) {
		t.Errorf("a service on the edge network: GET http://%s/healthz: %v\n%s", workspace, err, out)
	}
	if out, err := service("http://orglane:9090/traefik/config"); err == nil || strings.Contains(out, "orglane-workspace") || !strings.Contains(out, "403") {
		t.Errorf("a service on the edge network got the internal listener (want 403): %v\n%s", err, out)
	}
	if got := b.containerEnv("orglane", "ORGLANE_SERVER_INTERNAL_ALLOWED_PEERS"); got != prefix+".2/32" {
		t.Errorf("internal allowed peers = %q, want Traefik's address %s.2/32", got, prefix)
	}

	installForgejo(t, b, b.port("traefik", 80))
}

// installForgejo installs Forgejo through the API of a managed bundle in
// demo mode (install.sh --mode demo --managed, docs/specs/providers.md
// "Spec gaps decided in #61": Demo mode) and reaches it through the bundle's
// Traefik at git.orglane.localhost, by the route the core gives Traefik.
func installForgejo(t *testing.T, b *bundle, traefik string) {
	t.Helper()
	// Every request goes to Traefik, whatever host it names: the test host
	// need not resolve *.localhost.
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: noRedirects,
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, traefik)
		}},
	}
	owner := demotest.SignInWith(t, client, "http://orglane.localhost", "")
	code, _, body := owner.Get(t, "/api/v1/apps/agent")
	if code != http.StatusOK || !strings.Contains(string(body), `"connected":true`) {
		t.Fatalf("GET /api/v1/apps/agent = %d %s", code, body)
	}
	project := b.project + "-git"
	t.Cleanup(func() {
		// The instance's containers and network, before the bundle goes.
		ctx := context.WithoutCancel(t.Context())
		out, _ := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+project).Output() //nolint:gosec // fixed docker commands
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_ = exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, ids...)...).Run() //nolint:gosec // fixed docker commands
		}
		_ = exec.CommandContext(ctx, "docker", "network", "rm", project+"_default").Run() //nolint:gosec // fixed docker commands
	})
	start := time.Now()
	code, _, body = owner.Do(t, http.MethodPost, "/api/v1/apps/instances", map[string]any{
		"provider": "forgejo",
		"settings": map[string]any{"ssh_port": freeLocalPort(t)},
	})
	if code != http.StatusAccepted {
		t.Fatalf("install Forgejo = %d %s", code, body)
	}
	var inst struct {
		ID, Key, State, BaseURL string
	}
	if err := json.Unmarshal(body, &inst); err != nil || inst.Key != "git" {
		t.Fatalf("installed %s (%v)", body, err)
	}
	// The agent installs it (pulling the image the first time), Setup
	// activates it.
	deadline := time.Now().Add(8 * time.Minute)
	for {
		code, _, body = owner.Get(t, "/api/v1/apps/instances/"+inst.ID)
		var d struct {
			Instance struct{ State, HealthCode string } `json:"instance"`
		}
		_ = json.Unmarshal(body, &d)
		if code == http.StatusOK && d.Instance.State == "active" {
			break
		}
		if d.Instance.State == "failed" || time.Now().After(deadline) {
			_, _, ops := owner.Get(t, "/api/v1/apps/instances/"+inst.ID+"/operations")
			t.Fatalf("Forgejo did not become active: %s\noperations: %s", body, ops)
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("install-time: Forgejo active in %s", time.Since(start).Round(time.Second))
	// Traefik polls the core's routes.
	waitFor(t, client, "http://git.orglane.localhost/api/healthz", "git.orglane.localhost", time.Minute)
	waitFor(t, client, "http://git.orglane.localhost/user/login", "git.orglane.localhost", time.Minute)
	// Idle memory of the Forgejo container after install and setup.
	time.Sleep(15 * time.Second)
	id, err := exec.CommandContext(t.Context(), "docker", "ps", "-q", "--filter", "label=com.docker.compose.project="+project, //nolint:gosec // fixed docker commands
		"--filter", "label=com.docker.compose.service=forgejo").Output()
	if err == nil && strings.TrimSpace(string(id)) != "" {
		if out, err := exec.CommandContext(t.Context(), "docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", strings.TrimSpace(string(id))).Output(); err == nil { //nolint:gosec // fixed docker commands
			t.Logf("idle-memory: Forgejo after install and setup: %s", strings.TrimSpace(string(out)))
		}
	}
}

// freeLocalPort returns a free TCP port on the loopback interface.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}
