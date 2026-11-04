//go:build integration

// Package install runs the installer (deploy/install.sh) end to end, the way
// a user would, but non-interactively and from this checkout (--source) with
// a core image built from it:
//
//   - local mode: install, the web UI and /readyz through Traefik, the env
//     file's permissions, the owner created with the printed setup token, a
//     re-run that keeps the secrets, status, uninstall and purge;
//   - demo mode: install and one-click sign-in as the demo owner;
//   - an edge subnet taken by another Docker network: the failure names
//     ORGLANE_EDGE_NETWORK_PREFIX, and a re-run with a free one installs;
//     malformed or multi-line settings (also a bad prefix kept in .env)
//     are refused and leave .env alone;
//   - domain mode: the DNS check refuses an unresolvable workspace host
//     before anything starts, and an install against Pebble (Let's
//     Encrypt's ACME test server, deploy/compose/pebble) gets a certificate;
//     enable-managed then adds the agent and keeps the certificate;
//   - managed services (docs/specs/agent.md): a default install has no
//     agent; enable-managed on an existing local install refuses an agent
//     image it can't pin by digest and leaves .env alone, pins a registry
//     image by digest, moves the workspace to orglane.localhost and starts
//     the agent, which the core reaches; --managed on a fresh install;
//     uninstall removes the services' Compose projects; the core container
//     never has the Docker socket.
//
// Each test runs its own Compose project on its own edge network and ports,
// so it can run next to tests/compose. The tests run one after another: on
// Docker Desktop (macOS), several PostgreSQL containers initializing
// bind-mounted data directories at the same time sometimes fail with "data
// directory has wrong ownership". The time from start to a ready workspace
// is logged as "install-time:" (images are present, so it excludes
// downloads).
//
// Needs Docker with Compose v2. Run with
// `go test -tags integration -v ./tests/install/`.
package install

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/orglane/orglane/tests/demotest"
)

// image is the core image built from this repository for the tests, and
// agentImage its agent image (Dockerfile target agent).
const (
	image      = "orglane:install-test"
	agentImage = "orglane-agent:install-test"
	// registryImage serves agent images to pin by digest (a registry on the
	// daemon's localhost is trusted over plain HTTP).
	registryImage = "registry:3.0.0@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e"
)

var (
	repoRoot   string
	script     string
	setupToken = regexp.MustCompile(`ost_[A-Za-z0-9_-]{43}`)
)

func TestMain(m *testing.M) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	repoRoot, script = root, filepath.Join(root, "deploy", "install.sh")
	if err := buildImage(); err != nil {
		fmt.Fprintf(os.Stderr, "install tests: build %s (is Docker running?): %v\n", image, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func buildImage() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	for _, args := range [][]string{{"-t", image}, {"--target", "agent", "-t", agentImage}} {
		args = append(append([]string{"build", "--build-arg", "VERSION=install-test"}, args...), repoRoot)
		cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed docker build of this repository
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w\n%s", err, out)
		}
	}
	return nil
}

// install is one installation made by the installer for a test.
type install struct {
	t       *testing.T
	dataDir string
	project string
	// env is added to the installer's environment.
	env []string
}

// newInstall prepares a data directory (not created yet: the installer
// creates it) and a Compose project on the edge network <prefix>.0/24, and
// removes whatever the installer started when the test ends.
func newInstall(t *testing.T, edgePrefix string, env ...string) *install {
	t.Helper()
	parent, err := os.MkdirTemp("", "orglane-install-")
	if err != nil {
		t.Fatal(err)
	}
	// Docker Desktop creates missing bind-mount directories below a symlink
	// (macOS: /var/folders -> /private/var/folders) inside its VM.
	if parent, err = filepath.EvalSymlinks(parent); err != nil {
		t.Fatal(err)
	}
	in := &install{
		t:       t,
		dataDir: filepath.Join(parent, "orglane"),
		project: "orglane-install-test-" + randomHex(t, 4),
	}
	in.env = append([]string{
		"COMPOSE_PROJECT_NAME=" + in.project,
		"ORGLANE_EDGE_NETWORK_PREFIX=" + edgePrefix,
	}, env...)
	t.Cleanup(func() { in.cleanup(parent) })
	return in
}

// setEnv sets an environment variable of the installer for later runs.
func (in *install) setEnv(key, value string) {
	in.unsetEnv(key)
	in.env = append(in.env, key+"="+value)
}

// unsetEnv removes an environment variable of the installer for later runs.
func (in *install) unsetEnv(key string) {
	in.env = slices.DeleteFunc(in.env, func(kv string) bool { return strings.HasPrefix(kv, key+"=") })
}

// run runs the installer and returns its combined output and exit code.
func (in *install) run(args ...string) (string, int) {
	in.t.Helper()
	ctx, cancel := context.WithTimeout(in.t.Context(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", append([]string{script}, args...)...) //nolint:gosec // the installer of this repository
	cmd.Dir = in.t.TempDir()
	cmd.Env = append(cleanEnv(), in.env...)
	// No controlling terminal, as under `curl | sh` in a script: the
	// installer must not try to read answers from /dev/tty.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = -1
		if exit, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exec returns *ExitError itself
			code = exit.ExitCode()
		}
	}
	return string(out), code
}

// mustRun runs the installer and fails the test unless it succeeds.
func (in *install) mustRun(args ...string) string {
	in.t.Helper()
	out, code := in.run(args...)
	if code != 0 {
		in.t.Fatalf("install.sh %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

// compose runs docker compose for the installation, as a user would: in the
// data directory, with the settings from its .env file.
func (in *install) compose(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append([]string{"compose"}, args...)...) //nolint:gosec // fixed docker compose invocation
	cmd.Dir = in.dataDir
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// containers lists the containers of the installation's Compose project.
func (in *install) containers() []string {
	in.t.Helper()
	out, err := exec.CommandContext(in.t.Context(), "docker", "ps", "--all", "--quiet", //nolint:gosec // fixed docker ps
		"--filter", "label=com.docker.compose.project="+in.project).CombinedOutput()
	if err != nil {
		in.t.Fatalf("docker ps: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

func (in *install) cleanup(parent string) {
	t := in.t
	if _, err := os.Stat(filepath.Join(in.dataDir, ".env")); err == nil {
		if t.Failed() {
			logs, _ := in.compose("logs", "--no-color", "--timestamps")
			t.Logf("compose logs:\n%s", logs)
		}
		if out, err := in.compose("down", "--volumes", "--remove-orphans", "--timeout", "20"); err != nil {
			t.Errorf("compose down: %v\n%s", err, out)
		}
	}
	if err := os.RemoveAll(parent); err != nil {
		// On Linux the containers leave files owned by their own users.
		out, err := exec.CommandContext(context.Background(), "docker", "run", "--rm", "--volume", parent+":/data", //nolint:gosec // fixed cleanup command
			"--entrypoint", "sh", "alpine:3", "-c", "rm -rf /data/* /data/.[!.]*").CombinedOutput()
		if err != nil {
			t.Errorf("clean %s: %v\n%s", parent, err, out)
		}
		if err := os.RemoveAll(parent); err != nil {
			t.Errorf("remove %s: %v", parent, err)
		}
	}
}

// envFile reads the installation's .env file into a map and checks that
// only its owner can read it.
func (in *install) envFile() map[string]string {
	in.t.Helper()
	path := filepath.Join(in.dataDir, ".env")
	fi, err := os.Stat(path)
	if err != nil {
		in.t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		in.t.Errorf(".env has mode %o, want 600", perm)
	}
	if fi, err := os.Stat(in.dataDir); err != nil || fi.Mode().Perm() != 0o700 {
		in.t.Errorf("data directory mode = %v (%v), want 700", fi.Mode().Perm(), err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own data directory
	if err != nil {
		in.t.Fatal(err)
	}
	vars := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
			vars[k] = v
		}
	}
	return vars
}

// cleanEnv is the test's environment without settings that would leak into
// the installer or Compose from a developer's shell.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ORGLANE_") || strings.HasPrefix(kv, "COMPOSE_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

// freePort returns a localhost TCP port that is free right now.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

func request(t *testing.T, client *http.Client, method, url, host string, body any) response {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{code: resp.StatusCode, header: resp.Header, body: raw}
}

// checkWorkspace checks that the workspace serves the web UI and is ready.
func checkWorkspace(t *testing.T, client *http.Client, base, host string) {
	t.Helper()
	resp := request(t, client, http.MethodGet, base+"/readyz", host, nil)
	var health struct{ Status string }
	if err := json.Unmarshal(resp.body, &health); resp.code != http.StatusOK || err != nil || health.Status != "ok" {
		t.Errorf("GET /readyz = %d %s, want 200 status ok", resp.code, resp.body)
	}
	resp = request(t, client, http.MethodGet, base+"/", host, nil)
	if resp.code != http.StatusOK || !strings.HasPrefix(resp.header.Get("Content-Type"), "text/html") ||
		!strings.Contains(string(resp.body), `id="root"`) {
		t.Errorf("GET / = %d %q, want the web UI", resp.code, resp.header.Get("Content-Type"))
	}
}

// lastLine is the installer's last line of output: the workspace URL.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func TestLocalMode(t *testing.T) {
	in := newInstall(t, "10.231.111")
	port := freePort(t)
	args := []string{"--mode", "local", "--yes", "--source", repoRoot, "--image", image, "--data-dir", in.dataDir, "--http-port", port}

	start := time.Now()
	out := in.mustRun(args...)
	t.Logf("install-time: local mode ready in %s (images present)", time.Since(start).Round(time.Second))

	url, host := "http://localhost:"+port, "localhost:"+port
	if got := lastLine(out); got != url {
		t.Errorf("printed URL = %q, want %q", got, url)
	}
	base := "http://127.0.0.1:" + port
	checkWorkspace(t, http.DefaultClient, base, host)

	env := in.envFile()
	for key, want := range map[string]string{
		"ORGLANE_INSTALL_MODE":          "local",
		"ORGLANE_INGRESS_MODE":          "local",
		"ORGLANE_DEMO_ENABLED":          "false",
		"ORGLANE_DOMAIN_WORKSPACE_HOST": host,
		"ORGLANE_HTTP_PUBLISH":          "127.0.0.1:" + port,
		"ORGLANE_IMAGE":                 image,
		"ORGLANE_DATA_DIR":              in.dataDir,
	} {
		if env[key] != want {
			t.Errorf(".env %s = %q, want %q", key, env[key], want)
		}
	}
	password, secret := env["ORGLANE_POSTGRES_PASSWORD"], env["ORGLANE_IDENTITY_MASTER_SECRET"]
	if !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(password) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(secret) {
		t.Errorf("generated secrets have an unexpected form (lengths %d, %d)", len(password), len(secret))
	}
	if !strings.HasSuffix(env["ORGLANE_POSTGRES_SHARED_BUFFERS"], "MB") {
		t.Errorf("PostgreSQL is not sized: shared_buffers = %q", env["ORGLANE_POSTGRES_SHARED_BUFFERS"])
	}
	if strings.Contains(out, password) || strings.Contains(out, secret) {
		t.Error("the installer printed a secret")
	}
	// Managed services are off by default, but the agent secret is there
	// for enabling them later.
	agentSecret := env["ORGLANE_AGENT_SECRET"]
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(agentSecret) || strings.Contains(out, agentSecret) {
		t.Errorf("agent secret has an unexpected form (length %d) or was printed", len(agentSecret))
	}
	if env["ORGLANE_INSTALL_MANAGED"] != "false" || env["COMPOSE_PROFILES"] != "" || env["ORGLANE_AGENT_IMAGE"] != "" {
		t.Errorf(".env enables managed services by default: %q %q %q", env["ORGLANE_INSTALL_MANAGED"], env["COMPOSE_PROFILES"], env["ORGLANE_AGENT_IMAGE"])
	}
	if c := in.serviceContainers("agent"); len(c) > 0 {
		t.Errorf("a default install runs the agent: %v", c)
	}
	if !strings.Contains(out, "enable-managed") {
		t.Errorf("the output does not say how to enable managed services:\n%s", out)
	}

	// The printed setup token creates the owner: a working sign-in.
	token := setupToken.FindString(out)
	if token == "" {
		t.Fatalf("no setup token in the output:\n%s", out)
	}
	setup := map[string]string{
		"setupToken": token, "workEmail": "owner@acme.test", "username": "owner",
		"displayName": "Owner", "password": "correct horse battery staple",
	}
	if resp := request(t, http.DefaultClient, http.MethodPost, base+"/api/v1/auth/setup", host, setup); resp.code != http.StatusCreated {
		t.Fatalf("setup with the printed token = %d %s", resp.code, resp.body)
	}

	// A re-run repairs the install and keeps the secrets and the data.
	out = in.mustRun(args...)
	if !strings.Contains(out, "already installed") || !strings.Contains(out, "kept the existing secrets") {
		t.Errorf("re-run did not detect the existing install:\n%s", out)
	}
	if setupToken.MatchString(out) || !strings.Contains(out, "setup is complete") {
		t.Errorf("re-run after setup printed a setup token or no sign-in hint:\n%s", out)
	}
	env = in.envFile()
	if env["ORGLANE_POSTGRES_PASSWORD"] != password || env["ORGLANE_IDENTITY_MASTER_SECRET"] != secret || env["ORGLANE_AGENT_SECRET"] != agentSecret {
		t.Error("re-run changed the secrets")
	}
	checkWorkspace(t, http.DefaultClient, base, host)
	resp := request(t, http.DefaultClient, http.MethodGet, base+"/api/v1/auth/setup", host, nil)
	if resp.code != http.StatusOK || !strings.Contains(string(resp.body), `"required":false`) {
		t.Errorf("setup status after re-run = %d %s, want the owner kept", resp.code, resp.body)
	}

	// Changing the mode of an install is refused.
	if out, code := in.run("--mode", "demo", "--yes", "--source", repoRoot, "--image", image, "--data-dir", in.dataDir); code == 0 ||
		!strings.Contains(out, "not supported") {
		t.Errorf("mode change: exit %d\n%s", code, out)
	}

	if out := in.mustRun("status", "--data-dir", in.dataDir); !strings.Contains(out, "the workspace is ready") {
		t.Errorf("status:\n%s", out)
	}

	// Uninstall removes the containers and keeps the data.
	in.mustRun("uninstall", "--data-dir", in.dataDir)
	if c := in.containers(); len(c) > 0 {
		t.Errorf("containers left after uninstall: %v", c)
	}
	if _, err := os.Stat(filepath.Join(in.dataDir, "postgres", "18")); err != nil {
		t.Errorf("uninstall deleted the data: %v", err)
	}
	if _, code := in.run("status", "--data-dir", in.dataDir); code == 0 {
		t.Error("status after uninstall succeeded, want a failure")
	}
	// --purge without confirmation changes nothing.
	if _, code := in.run("uninstall", "--purge", "--data-dir", in.dataDir); code == 0 {
		t.Error("uninstall --purge without --yes succeeded")
	}
	if _, err := os.Stat(in.dataDir); err != nil {
		t.Errorf("unconfirmed purge deleted the data: %v", err)
	}
	in.mustRun("uninstall", "--purge", "--yes", "--data-dir", in.dataDir)
	if _, err := os.Stat(in.dataDir); !os.IsNotExist(err) {
		t.Errorf("data directory after purge: %v, want it deleted", err)
	}
}

func TestDemoMode(t *testing.T) {
	in := newInstall(t, "10.231.112")
	port := freePort(t)
	args := []string{"--mode", "demo", "--yes", "--source", repoRoot, "--image", image, "--data-dir", in.dataDir, "--http-port", port}

	start := time.Now()
	out := in.mustRun(args...)
	t.Logf("install-time: demo mode ready in %s (images present)", time.Since(start).Round(time.Second))

	url := "http://localhost:" + port
	if got := lastLine(out); got != url {
		t.Errorf("printed URL = %q, want %q", got, url)
	}
	if setupToken.MatchString(out) || !strings.Contains(out, "one click") {
		t.Errorf("demo mode output: want the one-click sign-in hint and no setup token:\n%s", out)
	}
	checkWorkspace(t, http.DefaultClient, "http://127.0.0.1:"+port, "localhost:"+port)
	if env := in.envFile(); env["ORGLANE_DEMO_ENABLED"] != "true" || env["ORGLANE_INGRESS_MODE"] != "local" {
		t.Errorf(".env: demo %q, ingress %q", env["ORGLANE_DEMO_ENABLED"], env["ORGLANE_INGRESS_MODE"])
	}

	// One-click sign-in as the demo owner works.
	owner := demotest.SignIn(t, url, "")
	if code, _, body := owner.Get(t, "/api/v1/people"); code != http.StatusOK {
		t.Errorf("GET /api/v1/people as the demo owner = %d %s", code, body)
	}

	// A re-run keeps the demo running with its data.
	secret := in.envFile()["ORGLANE_IDENTITY_MASTER_SECRET"]
	in.mustRun(args...)
	if in.envFile()["ORGLANE_IDENTITY_MASTER_SECRET"] != secret {
		t.Error("re-run changed the master secret")
	}
	demotest.SignIn(t, url, "")

	in.mustRun("uninstall", "--purge", "--yes", "--data-dir", in.dataDir)
	if c := in.containers(); len(c) > 0 {
		t.Errorf("containers left after uninstall: %v", c)
	}
}

// TestEdgeSubnetClash: when another Docker network already uses the edge
// subnet (an older install's leftover, a VPN), the installer names the clash
// and ORGLANE_EDGE_NETWORK_PREFIX instead of a generic failure, and a re-run
// with a free prefix installs. Docker words the clash differently for the
// same subnet and a partial overlap, so both are tried.
func TestEdgeSubnetClash(t *testing.T) {
	const clash, free = "10.231.115", "10.231.116"
	in := newInstall(t, clash)
	port := freePort(t)
	args := []string{"--mode", "local", "--yes", "--source", repoRoot, "--image", image, "--data-dir", in.dataDir, "--http-port", port}

	// A malformed prefix, or one that would add lines to .env, is refused
	// before anything is written.
	for _, prefix := range badPrefixes {
		in.setEnv("ORGLANE_EDGE_NETWORK_PREFIX", prefix)
		if out, code := in.run(args...); code == 0 || !strings.Contains(out, "invalid ORGLANE_EDGE_NETWORK_PREFIX") {
			t.Errorf("prefix %q: exit %d\n%s", prefix, code, out)
		}
		if _, err := os.Stat(in.dataDir); !os.IsNotExist(err) {
			t.Fatalf("prefix %q: the refused install created the data directory: %v", prefix, err)
		}
	}

	in.setEnv("ORGLANE_EDGE_NETWORK_PREFIX", clash)
	for _, other := range [][]string{
		{"--subnet", clash + ".0/24"},
		{"--subnet", clash + ".0/25", "--gateway", clash + ".126"},
	} {
		remove := createNetwork(t, other...)
		out, code := in.run(args...)
		remove()
		if code == 0 {
			t.Fatalf("install with a network %v succeeded:\n%s", other, out)
		}
		for _, want := range []string{clash + ".0/24", "overlaps", "ORGLANE_EDGE_NETWORK_PREFIX="} {
			if !strings.Contains(out, want) {
				t.Errorf("network %v: output lacks %q:\n%s", other, want, out)
			}
		}
		// The compose status is captured: no error about a missing status
		// file, and not the generic failure.
		if strings.Contains(out, "No such file") || strings.Contains(out, "did not start") {
			t.Errorf("network %v: output has a stray or generic error:\n%s", other, out)
		}
	}

	// A re-run with a free prefix installs and keeps it in .env.
	in.setEnv("ORGLANE_EDGE_NETWORK_PREFIX", free)
	out := in.mustRun(args...)
	if got := lastLine(out); got != "http://localhost:"+port {
		t.Errorf("printed URL = %q", got)
	}
	if got := in.envFile()["ORGLANE_EDGE_NETWORK_PREFIX"]; got != free {
		t.Errorf(".env ORGLANE_EDGE_NETWORK_PREFIX = %q, want %q", got, free)
	}
	checkWorkspace(t, http.DefaultClient, "http://127.0.0.1:"+port, "localhost:"+port)

	// On a re-run, a bad prefix, a multi-line setting from the environment
	// or a multi-line option is refused and .env stays as it is.
	envPath := filepath.Join(in.dataDir, ".env")
	before, err := os.ReadFile(envPath) //nolint:gosec // the test's own data directory
	if err != nil {
		t.Fatal(err)
	}
	refused := func(what, want string, extra ...string) {
		t.Helper()
		if out, code := in.run(append(args, extra...)...); code == 0 || !strings.Contains(out, want) {
			t.Errorf("%s: exit %d, want a refusal with %q\n%s", what, code, want, out)
		}
		if after, err := os.ReadFile(envPath); err != nil || !bytes.Equal(after, before) { //nolint:gosec // the test's own data directory
			t.Errorf("%s: .env changed (%v)", what, err)
		}
	}
	for _, prefix := range badPrefixes {
		in.setEnv("ORGLANE_EDGE_NETWORK_PREFIX", prefix)
		refused(fmt.Sprintf("re-run with prefix %q", prefix), "invalid ORGLANE_EDGE_NETWORK_PREFIX")
	}
	in.setEnv("ORGLANE_EDGE_NETWORK_PREFIX", free)
	in.setEnv("COMPOSE_PROJECT_NAME", in.project+"\nORGLANE_DEMO_ENABLED=true")
	refused("multi-line COMPOSE_PROJECT_NAME", "invalid COMPOSE_PROJECT_NAME")
	in.setEnv("COMPOSE_PROJECT_NAME", in.project)
	refused("multi-line --version", "invalid --version", "--version", "0.1.0\nORGLANE_DEMO_ENABLED=true")

	// Without the variable the prefix kept in .env is used: a bad one,
	// edited by hand, is refused too.
	kept := before
	before = bytes.Replace(kept, []byte("ORGLANE_EDGE_NETWORK_PREFIX="+free+"\n"), []byte("ORGLANE_EDGE_NETWORK_PREFIX=10.231.999\n"), 1)
	if bytes.Equal(before, kept) {
		t.Fatalf(".env does not keep ORGLANE_EDGE_NETWORK_PREFIX=%s:\n%s", free, kept)
	}
	if err := os.WriteFile(envPath, before, 0o600); err != nil { //nolint:gosec // the test's own data directory
		t.Fatal(err)
	}
	in.unsetEnv("ORGLANE_EDGE_NETWORK_PREFIX")
	refused("re-run with a bad prefix kept in .env", "invalid ORGLANE_EDGE_NETWORK_PREFIX '10.231.999'")
	if err := os.WriteFile(envPath, kept, 0o600); err != nil { //nolint:gosec // the test's own data directory
		t.Fatal(err)
	}
	// And a good kept one is used as it is.
	in.mustRun(args...)
	if got := in.envFile()["ORGLANE_EDGE_NETWORK_PREFIX"]; got != free {
		t.Errorf("re-run without the variable: .env ORGLANE_EDGE_NETWORK_PREFIX = %q, want %q kept", got, free)
	}
	checkWorkspace(t, http.DefaultClient, "http://127.0.0.1:"+port, "localhost:"+port)

	in.mustRun("uninstall", "--purge", "--yes", "--data-dir", in.dataDir)
	if c := in.containers(); len(c) > 0 {
		t.Errorf("containers left after uninstall: %v", c)
	}
}

// badPrefixes are ORGLANE_EDGE_NETWORK_PREFIX values the installer refuses:
// not three octets, or with extra lines that would end up in .env.
var badPrefixes = []string{
	"10.232.0.0",
	"10.232.256",
	"10.232.0\nORGLANE_DEMO_ENABLED=true",
	"ORGLANE_DEMO_ENABLED=true\n10.232.0",
	"10.232.0\n",
	"10.232.0\r",
}

// createNetwork creates a Docker network with the given options and returns
// a function that removes it; it is removed at the end of the test too.
func createNetwork(t *testing.T, opts ...string) func() {
	t.Helper()
	name := "orglane-install-test-net-" + randomHex(t, 4)
	if out, err := exec.CommandContext(t.Context(), "docker", append(append([]string{"network", "create"}, opts...), name)...).CombinedOutput(); err != nil { //nolint:gosec // fixed docker network create
		t.Fatalf("docker network create %v: %v\n%s", opts, err, out)
	}
	removed := false
	remove := func() {
		if removed {
			return
		}
		removed = true
		if out, err := exec.CommandContext(context.Background(), "docker", "network", "rm", name).CombinedOutput(); err != nil { //nolint:gosec // fixed docker network rm
			t.Errorf("docker network rm %s: %v\n%s", name, err, out)
		}
	}
	t.Cleanup(remove)
	return remove
}

// TestDomainModeRefusesUnreadyDNS: without DNS for the workspace host the
// installer stops before it writes or starts anything, and shows the records
// to create.
func TestDomainModeRefusesUnreadyDNS(t *testing.T) {
	in := newInstall(t, "10.231.113")
	out, code := in.run("--mode", "domain", "--domain", "orglane.test", "--workspace-host", "work.orglane.test",
		"--public-ip", "192.0.2.10", "--yes", "--source", repoRoot, "--image", image, "--data-dir", in.dataDir)
	if code == 0 {
		t.Fatalf("install with unresolvable DNS succeeded:\n%s", out)
	}
	for _, want := range []string{"work.orglane.test", "192.0.2.10", "*.orglane.test", "no certificate was requested"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Without working resolvers (some laptops) the lookup itself fails.
	if !strings.Contains(out, "has no A record") && !strings.Contains(out, "could not look up") {
		t.Errorf("output names no DNS problem:\n%s", out)
	}
	if _, err := os.Stat(in.dataDir); !os.IsNotExist(err) {
		t.Errorf("the refused install created the data directory: %v", err)
	}
	if c := in.containers(); len(c) > 0 {
		t.Errorf("the refused install started containers: %v", c)
	}
}

// TestDomainModeWithPebble installs domain mode with the Pebble overlay
// (through COMPOSE_FILE, which the installer keeps in .env). The DNS and
// reachability checks are skipped: the test domain resolves inside the
// Compose network only. The overlay points Traefik at Pebble whatever
// ORGLANE_ACME_CA_SERVER says, so the test checks the CA the installer keeps
// in .env, and that changing it resets Traefik's ACME account and
// certificates, which Pebble then issues again.
func TestDomainModeWithPebble(t *testing.T) {
	in := newInstall(t, "10.231.114",
		"COMPOSE_FILE=compose.yaml:pebble/compose.pebble.yaml",
		"ORGLANE_HTTP_PUBLISH=127.0.0.1:",
		"ORGLANE_HTTPS_PUBLISH=127.0.0.1:")
	const (
		host       = "work.orglane.test"
		staging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
		production = "https://acme-v02.api.letsencrypt.org/directory"
	)
	args := []string{"--mode", "domain", "--domain", "orglane.test", "--workspace-host", host,
		"--acme-email", "admin@orglane.test", "--skip-checks", "--yes",
		"--source", repoRoot, "--image", image, "--data-dir", in.dataDir}
	start := time.Now()
	out := in.mustRun(append(args, "--acme-ca", "staging")...)
	t.Logf("install-time: domain mode with Pebble ready in %s (images present)", time.Since(start).Round(time.Second))
	if got := lastLine(out); got != "https://"+host {
		t.Errorf("printed URL = %q, want https://%s", got, host)
	}
	if !strings.Contains(out, "certificate issued") || !setupToken.MatchString(out) {
		t.Errorf("output lacks the issued certificate or the setup token:\n%s", out)
	}
	env := in.envFile()
	if env["ORGLANE_INGRESS_MODE"] != "domain" || env["ORGLANE_ACME_EMAIL"] != "admin@orglane.test" {
		t.Errorf(".env: ingress %q, ACME email %q", env["ORGLANE_INGRESS_MODE"], env["ORGLANE_ACME_EMAIL"])
	}
	if got := env["ORGLANE_ACME_CA_SERVER"]; got != staging {
		t.Errorf(".env ORGLANE_ACME_CA_SERVER = %q, want staging %q", got, staging)
	}
	// The workspace is served over HTTPS with a certificate from Pebble.
	checkWorkspace(t, in.pebbleClient(), "https://"+host, host)
	account := in.acmeStorage()

	// A re-run without --acme-ca keeps the CA, the account and the
	// certificate.
	out = in.mustRun(args...)
	if got := in.envFile()["ORGLANE_ACME_CA_SERVER"]; got != staging {
		t.Errorf("re-run without --acme-ca: ORGLANE_ACME_CA_SERVER = %q, want staging kept", got)
	}
	if strings.Contains(out, "Switching the ACME CA") || in.acmeStorage() != account {
		t.Errorf("re-run without --acme-ca reset the ACME storage:\n%s", out)
	}
	checkWorkspace(t, in.pebbleClient(), "https://"+host, host)

	// --acme-ca production switches the CA and resets acme.json, so the
	// certificate is requested again.
	out = in.mustRun(append(args, "--acme-ca", "production")...)
	if got := in.envFile()["ORGLANE_ACME_CA_SERVER"]; got != production {
		t.Errorf("--acme-ca production: ORGLANE_ACME_CA_SERVER = %q, want %q", got, production)
	}
	if !strings.Contains(out, "deleted traefik/acme.json") || !strings.Contains(out, "certificate issued") {
		t.Errorf("CA switch: output lacks the reset or the new certificate:\n%s", out)
	}
	if in.acmeStorage() == account {
		t.Error("CA switch kept the old ACME account")
	}
	checkWorkspace(t, in.pebbleClient(), "https://"+host, host)

	// An invalid CA is refused.
	if out, code := in.run(append(args, "--acme-ca", "ftp://example.test")...); code == 0 || !strings.Contains(out, "invalid --acme-ca") {
		t.Errorf("--acme-ca ftp://example.test: exit %d\n%s", code, out)
	}

	// Domain mode is production: an agent image that can't be pinned by a
	// registry digest is refused, even one that exists on this machine, and
	// .env stays as it is.
	managed := []string{"enable-managed", "--yes", "--source", repoRoot, "--image", image, "--data-dir", in.dataDir}
	envPath := filepath.Join(in.dataDir, ".env")
	before, err := os.ReadFile(envPath) //nolint:gosec // the test's own data directory
	if err != nil {
		t.Fatal(err)
	}
	if out, code := in.run(append(managed, "--agent-image", agentImage)...); code == 0 || !strings.Contains(out, "pinned by a registry digest") {
		t.Errorf("domain mode with a local agent image: exit %d, want a refusal\n%s", code, out)
	}
	if after, err := os.ReadFile(envPath); err != nil || !bytes.Equal(after, before) { //nolint:gosec // the test's own data directory
		t.Errorf("the refused enable-managed changed .env (%v)", err)
	}
	if c := in.serviceContainers("agent"); len(c) > 0 {
		t.Errorf("the refused enable-managed started the agent: %v", c)
	}

	// Enabling managed services on the domain install, with an agent image
	// from a registry, pins it by digest and keeps the address, CA and
	// certificate; service ports are published on every interface.
	reg := startRegistry(t)
	ref := reg.push(agentImage, "orglane-agent", "install-test")
	digest, err := reg.manifestDigest("orglane-agent", "install-test")
	if err != nil {
		t.Fatal(err)
	}
	account = in.acmeStorage()
	out = in.mustRun(append(managed, "--agent-image", ref)...)
	if got := lastLine(out); got != "https://"+host {
		t.Errorf("enable-managed: printed URL = %q, want https://%s", got, host)
	}
	env = in.envFile()
	if env["ORGLANE_DOMAIN_WORKSPACE_HOST"] != host || env["ORGLANE_EDGE_WORKSPACE_ALIAS"] != host || env["ORGLANE_ACME_CA_SERVER"] != production {
		t.Errorf("enable-managed changed the domain settings: host %q, alias %q, CA %q",
			env["ORGLANE_DOMAIN_WORKSPACE_HOST"], env["ORGLANE_EDGE_WORKSPACE_ALIAS"], env["ORGLANE_ACME_CA_SERVER"])
	}
	if in.acmeStorage() != account {
		t.Error("enable-managed reset the ACME storage")
	}
	in.checkManaged(ref+"@"+digest, "0.0.0.0")
	checkWorkspace(t, in.pebbleClient(), "https://"+host, host)

	in.mustRun("uninstall", "--purge", "--yes", "--data-dir", in.dataDir)
	if c := in.containers(); len(c) > 0 {
		t.Errorf("containers left after uninstall: %v", c)
	}
}

// pebbleClient is an HTTPS client that trusts Pebble's certificates and
// connects to Traefik's published port, whatever the host name.
func (in *install) pebbleClient() *http.Client {
	in.t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(in.pebbleRoot())
	// Read on every call: a restarted Traefik gets a new random port.
	websecure := in.port("traefik", 443)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, websecure)
		},
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
}

// acmeStorage returns Traefik's acme.json (ACME account and certificates),
// read inside the container: on Linux the file belongs to root.
func (in *install) acmeStorage() string {
	in.t.Helper()
	out, err := in.compose("exec", "-T", "traefik", "cat", "/data/acme.json")
	if err != nil || !strings.Contains(out, "PrivateKey") {
		in.t.Fatalf("read acme.json: %v\n%s", err, out)
	}
	return out
}

func (in *install) port(service string, containerPort int) string {
	in.t.Helper()
	out, err := in.compose("port", service, fmt.Sprint(containerPort))
	if err != nil {
		in.t.Fatalf("compose port %s %d: %v\n%s", service, containerPort, err, out)
	}
	return strings.TrimSpace(out)
}

// pebbleRoot fetches the root that signs Pebble's certificates from its
// management API (which uses Pebble's static test CA).
func (in *install) pebbleRoot() *x509.Certificate {
	t := in.t
	t.Helper()
	minica, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "compose", "pebble", "pebble.minica.pem")) //nolint:gosec // a file of this repository
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
	resp := request(t, client, http.MethodGet, "https://"+in.port("pebble", 15000)+"/roots/0", "", nil)
	block, _ := pem.Decode(resp.body)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("Pebble root: %d %s", resp.code, resp.body)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// serviceContainers lists the running containers of one service of the
// installation's Compose project.
func (in *install) serviceContainers(service string) []string {
	in.t.Helper()
	return projectContainers(in.t, in.project, "label=com.docker.compose.service="+service, "status=running")
}

// projectContainers lists the containers of a Compose project.
func projectContainers(t *testing.T, project string, filters ...string) []string {
	t.Helper()
	args := []string{"ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project=" + project}
	for _, f := range filters {
		args = append(args, "--filter", f)
	}
	out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput() //nolint:gosec // fixed docker ps
	if err != nil {
		t.Fatalf("docker ps: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

// checkManaged checks a running install with managed services: the agent
// runs from the expected image and answers the core (uid 65532) over the
// shared socket with the shared secret, the core container has no Docker
// socket, and instances/ exists with mode 700.
func (in *install) checkManaged(wantImage, publishAddress string) {
	t := in.t
	t.Helper()
	env := in.envFile()
	for key, want := range map[string]string{
		"ORGLANE_INSTALL_MANAGED":       "true",
		"COMPOSE_PROFILES":              "managed",
		"ORGLANE_AGENT_IMAGE":           wantImage,
		"ORGLANE_AGENT_PUBLISH_ADDRESS": publishAddress,
	} {
		if env[key] != want {
			t.Errorf(".env %s = %q, want %q", key, env[key], want)
		}
	}
	if c := in.serviceContainers("agent"); len(c) != 1 {
		t.Fatalf("agent containers = %v, want one running", c)
	}
	if got := in.inspect("agent", "{{.Config.Image}}"); got != wantImage {
		t.Errorf("the agent runs %q, want %q", got, wantImage)
	}
	out, err := in.compose("exec", "-T", "-e", "ORGLANE_AGENT_LISTEN=/run/orglane-agent/agent.sock", "orglane",
		"/usr/local/bin/orglane", "agent", "healthcheck")
	if err != nil || !strings.Contains(out, "agent install-test ready") {
		t.Errorf("agent health from the core container: %v\n%s", err, out)
	}
	in.checkNoDockerSocket()
	if fi, err := os.Stat(filepath.Join(in.dataDir, "instances")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("instances/ = %v (%v), want a directory with mode 700", fi, err)
	}
	if out := in.mustRun("status", "--data-dir", in.dataDir); !strings.Contains(out, "managed services: agent install-test ready") {
		t.Errorf("status does not show the ready agent:\n%s", out)
	}
}

// checkNoDockerSocket: the core container never mounts the Docker socket.
func (in *install) checkNoDockerSocket() {
	t := in.t
	t.Helper()
	var mounts []struct{ Source, Destination string }
	if err := json.Unmarshal([]byte(in.inspect("orglane", "{{json .Mounts}}")), &mounts); err != nil {
		t.Fatal(err)
	}
	for _, m := range mounts {
		if strings.Contains(m.Source, "docker.sock") || strings.Contains(m.Destination, "docker.sock") {
			t.Errorf("the core container mounts %s at %s", m.Source, m.Destination)
		}
	}
	if out, err := in.compose("exec", "-T", "orglane", "/usr/local/bin/docker", "--version"); err == nil {
		t.Errorf("the core container has a Docker CLI:\n%s", out)
	}
}

// inspect runs docker inspect with a Go template on a service's container.
func (in *install) inspect(service, format string) string {
	in.t.Helper()
	ids := in.serviceContainers(service)
	if len(ids) != 1 {
		in.t.Fatalf("containers of %s: %v, want one", service, ids)
	}
	out, err := exec.CommandContext(in.t.Context(), "docker", "inspect", "--format", format, ids[0]).CombinedOutput() //nolint:gosec // fixed docker inspect
	if err != nil {
		in.t.Fatalf("docker inspect %s: %v\n%s", service, err, out)
	}
	return strings.TrimSpace(string(out))
}

// startInstanceProject starts a stand-in for a managed service's Compose
// project under instances/<key>, attached to the bundle's edge network like
// the agent's projects, and returns its project name.
func (in *install) startInstanceProject(key string) string {
	t := in.t
	t.Helper()
	pg := ""
	raw, err := os.ReadFile(filepath.Join(in.dataDir, "compose.yaml")) //nolint:gosec // the test's own data directory
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?m)^\s*image: (postgres:\S+)`).FindSubmatch(raw); m != nil {
		pg = string(m[1])
	}
	dir := filepath.Join(in.dataDir, "instances", key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	project := in.project + "-" + key
	compose := fmt.Sprintf(`name: %s
services:
  app:
    image: %s
    entrypoint: ["sleep", "infinity"]
    networks: [edge]
networks:
  edge:
    name: %s_edge
    external: true
`, project, pg, in.project)
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o600); err != nil { //nolint:gosec // the test's own data directory
		t.Fatal(err)
	}
	args := []string{"compose", "--project-name", project, "--project-directory", dir, "-f", filepath.Join(dir, "compose.yaml")}
	t.Cleanup(func() {
		_, _ = exec.CommandContext(context.Background(), "docker", append(args, "down", "--timeout", "1")...).CombinedOutput() //nolint:gosec // fixed docker compose
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append(args, "up", "--detach", "--wait")...) //nolint:gosec // fixed docker compose
	cmd.Env = cleanEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start the service project %s: %v\n%s", project, err, out)
	}
	return project
}

// registry is a throwaway image registry on the Docker daemon's loopback
// (host network), which Docker trusts over plain HTTP.
type registry struct {
	t    *testing.T
	id   string
	addr string
}

func startRegistry(t *testing.T) *registry {
	t.Helper()
	// A free port on this machine; with Docker Desktop the host network is
	// the VM's, where a clash is just as unlikely.
	addr := "127.0.0.1:" + freePort(t)
	if out, err := exec.CommandContext(t.Context(), "docker", "pull", "--quiet", registryImage).CombinedOutput(); err != nil { //nolint:gosec // fixed docker pull
		t.Fatalf("pull %s: %v\n%s", registryImage, err, out)
	}
	// Standard output only: the container ID.
	out, err := exec.CommandContext(t.Context(), "docker", "run", "--detach", "--rm", "--network", "host", //nolint:gosec // fixed docker run
		"--env", "REGISTRY_HTTP_ADDR="+addr, registryImage).Output()
	if err != nil {
		t.Fatalf("start a registry: %v\n%s", err, out)
	}
	r := &registry{t: t, id: strings.TrimSpace(string(out)), addr: addr}
	t.Cleanup(func() {
		_, _ = exec.CommandContext(context.Background(), "docker", "rm", "--force", r.id).CombinedOutput() //nolint:gosec // fixed docker rm
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := exec.CommandContext(t.Context(), "docker", "exec", r.id, "wget", "-q", "-O", "/dev/null", "http://"+addr+"/v2/").CombinedOutput() //nolint:gosec // fixed docker exec
		if err == nil {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("the registry did not start: %v\n%s", err, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// push pushes a local image as localhost:<port>/<name>:<tag>, removes the
// local tag (so the installer has to pull it) and returns the reference.
func (r *registry) push(local, name, tag string) string {
	t := r.t
	t.Helper()
	ref := strings.Replace(r.addr, "127.0.0.1", "localhost", 1) + "/" + name + ":" + tag
	for _, args := range [][]string{{"tag", local, ref}, {"push", "--quiet", ref}, {"image", "rm", ref}} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput() //nolint:gosec // fixed docker commands
		cancel()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
	}
	return ref
}

// manifestDigest asks the registry for the digest of a tag.
func (r *registry) manifestDigest(name, tag string) (string, error) {
	out, err := exec.CommandContext(r.t.Context(), "docker", "exec", r.id, "wget", "-q", "-S", "-O", "/dev/null", //nolint:gosec // fixed docker exec
		"--header", "Accept: application/vnd.oci.image.index.v1+json",
		"--header", "Accept: application/vnd.docker.distribution.manifest.list.v2+json",
		"--header", "Accept: application/vnd.oci.image.manifest.v1+json",
		"--header", "Accept: application/vnd.docker.distribution.manifest.v2+json",
		"http://"+r.addr+"/v2/"+name+"/manifests/"+tag).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	m := regexp.MustCompile(`(?i)Docker-Content-Digest:\s*(sha256:[0-9a-f]{64})`).FindSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no digest in %s", out)
	}
	return string(m[1]), nil
}

// TestEnableManagedLocal enables managed services on an existing local
// install: refusals that leave .env alone, an agent image pinned by the
// registry digest of its tag, the move to orglane.localhost, the agent
// reachable from the core, a re-run that keeps managed services, and an
// uninstall that removes the services' projects.
func TestEnableManagedLocal(t *testing.T) {
	in := newInstall(t, "10.231.117")
	port := freePort(t)
	source := []string{"--yes", "--source", repoRoot, "--image", image, "--data-dir", in.dataDir}
	in.mustRun(append([]string{"--mode", "local", "--http-port", port}, source...)...)
	envPath := filepath.Join(in.dataDir, ".env")
	before, err := os.ReadFile(envPath) //nolint:gosec // the test's own data directory
	if err != nil {
		t.Fatal(err)
	}
	agentSecret := in.envFile()["ORGLANE_AGENT_SECRET"]
	refused := func(what, want string, args ...string) {
		t.Helper()
		if out, code := in.run(append([]string{"enable-managed"}, args...)...); code == 0 || !strings.Contains(out, want) {
			t.Errorf("%s: exit %d, want a refusal with %q\n%s", what, code, want, out)
		}
		if after, err := os.ReadFile(envPath); err != nil || !bytes.Equal(after, before) { //nolint:gosec // the test's own data directory
			t.Errorf("%s: .env changed (%v)", what, err)
		}
		if c := in.serviceContainers("agent"); len(c) > 0 {
			t.Errorf("%s: the agent runs: %v", what, c)
		}
	}
	// A custom build has no released agent image.
	refused("custom build without --agent-image", "--agent-image", source...)
	// An agent image that can't be pulled, so no digest: never a bare tag.
	missing := "localhost:" + freePort(t) + "/orglane-agent:install-test"
	refused("unreachable agent image", "nothing was changed", append(source, "--agent-image", missing)...)
	// No install there.
	if out, code := in.run("enable-managed", "--yes", "--data-dir", filepath.Join(t.TempDir(), "none")); code == 0 || !strings.Contains(out, "no Orglane install") {
		t.Errorf("enable-managed without an install: exit %d\n%s", code, out)
	}

	// An agent image from a registry is pinned by the digest of its tag.
	reg := startRegistry(t)
	ref := reg.push(agentImage, "orglane-agent", "install-test")
	digest, err := reg.manifestDigest("orglane-agent", "install-test")
	if err != nil {
		t.Fatal(err)
	}
	out := in.mustRun(append([]string{"enable-managed", "--agent-image", ref}, source...)...)
	workspace := "orglane.localhost:" + port
	if got := lastLine(out); got != "http://"+workspace {
		t.Errorf("printed URL = %q, want http://%s", got, workspace)
	}
	if !strings.Contains(out, "moves from http://localhost:"+port+" to http://"+workspace) {
		t.Errorf("the output does not announce the new workspace address:\n%s", out)
	}
	in.checkManaged(ref+"@"+digest, "127.0.0.1")
	env := in.envFile()
	for key, want := range map[string]string{
		"ORGLANE_DOMAIN_BASE":           "orglane.localhost",
		"ORGLANE_DOMAIN_WORKSPACE_HOST": workspace,
		"ORGLANE_EDGE_WORKSPACE_ALIAS":  "orglane.localhost",
		"ORGLANE_AGENT_SECRET":          agentSecret,
	} {
		if env[key] != want {
			t.Errorf(".env %s = %q, want %q", key, env[key], want)
		}
	}
	if strings.Contains(out, agentSecret) {
		t.Error("the installer printed the agent secret")
	}
	base := "http://127.0.0.1:" + port
	checkWorkspace(t, http.DefaultClient, base, workspace)

	// A re-run keeps managed services; a locally built agent image (no
	// registry digest) is used as it is, like --image.
	out = in.mustRun(append([]string{"--agent-image", agentImage}, source...)...)
	if !strings.Contains(out, "not pinned by a digest") {
		t.Errorf("re-run with a local agent image: no warning about the missing digest:\n%s", out)
	}
	in.checkManaged(agentImage, "127.0.0.1")
	checkWorkspace(t, http.DefaultClient, base, workspace)

	// Uninstall removes the services' projects too, then the bundle.
	project := in.startInstanceProject("svc1")
	out = in.mustRun("uninstall", "--data-dir", in.dataDir)
	if !strings.Contains(out, "stopped the agent") || !strings.Contains(out, "service project svc1") {
		t.Errorf("uninstall output does not name the service project:\n%s", out)
	}
	for _, p := range []string{project, in.project} {
		if c := projectContainers(t, p); len(c) > 0 {
			t.Errorf("containers of %s left after uninstall: %v", p, c)
		}
	}
	in.mustRun("uninstall", "--purge", "--yes", "--data-dir", in.dataDir)
	if _, err := os.Stat(in.dataDir); !os.IsNotExist(err) {
		t.Errorf("data directory after purge: %v, want it deleted", err)
	}
}

// TestManagedFreshInstall: --managed on a new local install starts the agent
// right away, on orglane.localhost.
func TestManagedFreshInstall(t *testing.T) {
	in := newInstall(t, "10.231.118")
	port := freePort(t)
	out := in.mustRun("--mode", "local", "--managed", "--yes", "--source", repoRoot, "--image", image,
		"--agent-image", agentImage, "--data-dir", in.dataDir, "--http-port", port)
	workspace := "orglane.localhost:" + port
	if got := lastLine(out); got != "http://"+workspace {
		t.Errorf("printed URL = %q, want http://%s", got, workspace)
	}
	if !setupToken.MatchString(out) {
		t.Errorf("no setup token in the output:\n%s", out)
	}
	in.checkManaged(agentImage, "127.0.0.1")
	checkWorkspace(t, http.DefaultClient, "http://127.0.0.1:"+port, workspace)
	in.mustRun("uninstall", "--purge", "--yes", "--data-dir", in.dataDir)
	if c := in.containers(); len(c) > 0 {
		t.Errorf("containers left after uninstall: %v", c)
	}
}
