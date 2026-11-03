//go:build integration

// Package compose starts the Compose bundle in deploy/compose (Traefik, the
// core image built from this repository, PostgreSQL) and checks it from the
// outside, through Traefik:
//
//   - local mode: plain HTTP routing by Host header to /healthz and /readyz;
//   - domain mode: certificate issuance against Pebble, Let's Encrypt's ACME
//     test server, then HTTPS, HSTS and the HTTP to HTTPS redirect.
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
	"strings"
	"testing"
	"time"
)

const (
	// image is the core image built from this repository for the tests.
	image = "orglane:compose-test"
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "build", "--build-arg", "VERSION=compose-test", "-t", image, repoRoot) //nolint:gosec // fixed docker build of this repository
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

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
	if out, err := b.compose("up", "--detach", "--wait", "--wait-timeout", "180"); err != nil {
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
	if got := b.psql("SELECT max(version_id) FROM goose_db_version WHERE is_applied"); got != fmt.Sprint(len(migrations)) {
		t.Errorf("applied migration version = %q, want %d", got, len(migrations))
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
