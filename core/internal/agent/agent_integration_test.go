//go:build integration

package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// postgresImage is the bundle's PostgreSQL image (deploy/compose).
const postgresImage = "postgres:18.6-alpine3.24@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5"

// busyboxImage stands in for Traefik on the edge network.
const busyboxImage = "busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// hostile is a setting value that tries to escape its scalar, interpolate
// the agent's secret and run commands. It must arrive in the service as is.
const hostile = `hi $(id) ${ORGLANE_AGENT_SECRET} $$HOME "q" 'single' {{.Secrets}} a: [b] # c`

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput() //nolint:gosec // test commands
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// startBundle starts a minimal stand-in for the Compose bundle: PostgreSQL
// on the services network and a container on the edge network, labeled by
// Compose like the bundle's.
func startBundle(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	project := "agt" + hex.EncodeToString(b)
	dir := t.TempDir()
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
    init: true
    command: ["sleep", "2147483647"]
    networks: [edge]
networks:
  edge: {}
  database: {internal: true}
  services: {internal: true}
`, project, postgresImage, busyboxImage)
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
	return project
}

// auditRunner records every Docker invocation of the agent and runs it.
type auditRunner struct {
	inner runner
	mu    sync.Mutex
	calls [][]string
}

func (r *auditRunner) record(args []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
}

func (r *auditRunner) run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	r.record(args)
	return r.inner.run(ctx, stdin, args...)
}

func (r *auditRunner) tail(ctx context.Context, limit int, args ...string) ([]byte, bool, []byte, error) {
	r.record(args)
	return r.inner.tail(ctx, limit, args...)
}

func (r *auditRunner) pipe(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) ([]byte, error) {
	r.record(args)
	return r.inner.pipe(ctx, stdin, stdout, args...)
}

func (r *auditRunner) stream(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	r.record(args)
	return r.inner.stream(ctx, args...)
}

type testEnv struct {
	t        *testing.T
	project  string
	agent    *Agent
	client   *agentclient.Client
	audit    *auditRunner
	socket   string
	dataDir  string
	registry *packages.Registry
	stop     func() error
}

// agentOptions configure launchAgent.
type agentOptions struct {
	dataDir string // empty: a new temporary directory
	crashAt string // the test crash hook
}

// launchAgent runs an agent in-process with peer_uid -1. stop ends it and
// returns Run's error (errCrashed after the crash hook fired).
func launchAgent(t *testing.T, project string, registry *packages.Registry, o agentOptions) *testEnv {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "agent-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	if o.dataDir == "" {
		o.dataDir = t.TempDir()
	}
	cfg := config.Default().Agent
	cfg.Secret = testSecret
	cfg.Listen = filepath.Join(sockDir, "agent.sock")
	cfg.DataDir = o.dataDir
	cfg.PeerUID = -1
	cfg.ComposeProject = project
	cfg.EdgeNetwork, cfg.ServicesNetwork = project+"_edge", project+"_services"
	cfg.ReconcileInterval = time.Hour
	audit := &auditRunner{inner: newDockerCLI("docker")}
	a, err := New(Options{
		Config: cfg, Version: "dev", Registry: registry, Logger: testLogger(t),
		EventDebounce: time.Second, EventMinInterval: 2 * time.Second, runner: audit, crashAt: o.crashAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	var once sync.Once
	var runErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			runErr = <-done
		})
		return runErr
	}
	t.Cleanup(func() { _ = stop() })
	for range 100 {
		if c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", cfg.Listen); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	client, err := agentclient.New(cfg.Listen, testSecret, "dev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return &testEnv{t: t, project: project, agent: a, client: client, audit: audit, socket: cfg.Listen, dataDir: o.dataDir, registry: registry, stop: stop}
}

func startAgent(t *testing.T, project string, registry *packages.Registry) *testEnv {
	t.Helper()
	e := launchAgent(t, project, registry, agentOptions{})
	t.Cleanup(func() {
		if err := e.stop(); err != nil {
			t.Errorf("agent: %v", err)
		}
	})
	return e
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func integrationSpec(id uuid.UUID, gen int64, port int, message, state string) *agentapi.Spec {
	return &agentapi.Spec{
		InstanceID: id,
		Generation: gen,
		Package:    agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version},
		State:      state,
		Hosts:      map[string]string{"workspace": "orglane.localhost", "web": "agenttest.orglane.localhost"},
		URLs:       agentapi.URLs{Workspace: "http://orglane.localhost", Issuer: "http://orglane.localhost"},
		Ingress:    agentapi.IngressLocal,
		Settings:   map[string]any{"http_port": port, "message": message},
		Secrets:    map[string]string{"db_password": "aW50ZWdyYXRpb24tZGItcGFzc3dvcmQ"}, //nolint:gosec // a test secret
	}
}

// apply pushes a spec and waits for its operation to finish.
func (e *testEnv) apply(key string, spec *agentapi.Spec) *agentapi.Operation {
	e.t.Helper()
	op, accepted, err := e.client.Apply(e.t.Context(), key, spec)
	if err != nil || !accepted {
		e.t.Fatalf("apply %s generation %d: %v (accepted %v)", key, spec.Generation, err, accepted)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for !op.Terminal() {
		if time.Now().After(deadline) {
			e.t.Fatalf("operation %s did not finish: %+v", op.ID, op)
		}
		time.Sleep(250 * time.Millisecond)
		if op, err = e.client.Operation(e.t.Context(), op.ID); err != nil {
			e.t.Fatal(err)
		}
	}
	return op
}

func (e *testEnv) mustApply(spec *agentapi.Spec) { e.t.Helper(); e.mustApplyKey("at1", spec) }

func (e *testEnv) mustApplyKey(key string, spec *agentapi.Spec) {
	e.t.Helper()
	if op := e.apply(key, spec); op.State != agentapi.OpSucceeded {
		e.t.Fatalf("operation for %s generation %d: %+v", key, spec.Generation, op)
	}
}

func (e *testEnv) containers(key string) []string {
	e.t.Helper()
	out := docker(e.t, "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+packages.InstanceProject(e.project, key))
	return strings.Fields(out)
}

func (e *testEnv) psql(db, sql string) string {
	e.t.Helper()
	id := docker(e.t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+e.project, "--filter", "label=com.docker.compose.service=postgres")
	return docker(e.t, "exec", id, "psql", "-U", "orglane", "-d", db, "-Atc", sql)
}

func httpGet(port int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/", http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func eventually(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TestAgentWithDocker installs the agenttest package with real Docker,
// repairs drift, changes settings, stops, removes and purges it, and reuses
// its key (docs/specs/agent.md "Testing").
func TestAgentWithDocker(t *testing.T) {
	project := startBundle(t)
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	e := startAgent(t, project, registry)
	const key = "at1"
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	dir := e.agent.instanceDir(key)
	t.Cleanup(func() {
		// Leave nothing behind if a step failed.
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", packages.InstanceProject(project, key), "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})

	// Install.
	e.mustApply(integrationSpec(id, 1, port, hostile, agentapi.StateRunning))
	if body, err := httpGet(port); err != nil || body != hostile+"\n" {
		t.Fatalf("served %q, %v; want the setting verbatim", body, err)
	}
	if got := e.psql("svc_at1", "select message from agenttest_config where id = 1"); got != hostile {
		t.Fatalf("database row = %q", got)
	}
	st, err := e.client.Instance(t.Context(), key)
	if err != nil || st.Phase != agentapi.PhaseRunning || st.ObservedGeneration != 1 || len(st.Services) != 2 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	for path, want := range map[string]os.FileMode{
		dir: 0o700, filepath.Join(dir, envFile): 0o600, filepath.Join(dir, composeFile): 0o644,
		filepath.Join(dir, desiredFile): 0o600, filepath.Join(dir, currentFile): 0o600, filepath.Join(dir, instanceFile): 0o600,
		filepath.Join(dir, dataDir, "www"): 0o755,
	} {
		if got := fileMode(t, path); got != want {
			t.Errorf("%s: mode %v, want %v", path, got, want)
		}
	}
	compose, err := os.ReadFile(filepath.Join(dir, composeFile)) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compose), "aW50ZWdyYXRpb24tZGItcGFzc3dvcmQ") {
		t.Fatal("the compose file contains a secret")
	}
	logs, err := e.client.Logs(t.Context(), key, agentclient.LogOptions{Service: "worker", Tail: 50})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if strings.Contains(logs, "aW50ZWdyYXRpb24tZGItcGFzc3dvcmQ") {
		t.Fatal("logs contain a secret")
	}

	// Drift: a removed container comes back through Docker events.
	web := docker(t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+packages.InstanceProject(project, key), "--filter", "label=com.docker.compose.service=web")
	docker(t, "rm", "--force", web)
	eventually(t, 2*time.Minute, "the removed web container is recreated", func() bool {
		body, err := httpGet(port)
		return err == nil && body == hostile+"\n"
	})

	// Drift: an edited compose file is rendered again; what it added never
	// runs.
	edited := strings.Replace(string(compose), "services:\n", "services:\n  evil:\n    image: busybox:latest\n    privileged: true\n    command: [\"sleep\", \"600\"]\n", 1)
	if err := os.WriteFile(filepath.Join(dir, composeFile), []byte(edited), 0o600); err != nil { //nolint:gosec // test path
		t.Fatal(err)
	}
	e.agent.wake(key)
	eventually(t, time.Minute, "the compose file is rendered again", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, composeFile)) //nolint:gosec // test path
		return err == nil && bytes.Equal(b, compose)
	})
	time.Sleep(2 * time.Second)
	if out := docker(t, "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.service=evil"); out != "" {
		t.Fatal("a service added to the compose file ran")
	}

	// Configure: a new setting re-renders and reruns the configure hook.
	e.mustApply(integrationSpec(id, 2, port, "second", agentapi.StateRunning))
	eventually(t, time.Minute, "the new message is served", func() bool {
		body, err := httpGet(port)
		return err == nil && body == "second\n"
	})
	if got := e.psql("svc_at1", "select message from agenttest_config where id = 1"); got != "second" {
		t.Fatalf("database row after configure = %q", got)
	}

	// The key is bound to the instance ID.
	_, _, err = e.client.Apply(t.Context(), key, integrationSpec(uuid.Must(uuid.NewV7()), 3, port, "x", agentapi.StateRunning))
	var pe *agentclient.ProblemError
	if !errors.As(err, &pe) || pe.Code != agentapi.CodeInstanceMismatch {
		t.Fatalf("other instance ID: %v", err)
	}

	// Stop and start.
	e.mustApply(integrationSpec(id, 3, port, "second", agentapi.StateStopped))
	if _, err := httpGet(port); err == nil {
		t.Fatal("a stopped instance still serves")
	}
	if st, _ := e.client.Instance(t.Context(), key); st.Phase != agentapi.PhaseStopped {
		t.Fatalf("stopped status = %+v", st)
	}
	e.mustApply(integrationSpec(id, 4, port, "second", agentapi.StateRunning))
	if body, err := httpGet(port); err != nil || body != "second\n" {
		t.Fatalf("after start: %q, %v", body, err)
	}

	// Remove: containers go, data and the database stay, the key stays bound.
	e.mustApply(integrationSpec(id, 5, port, "second", agentapi.StateAbsent))
	if c := e.containers(key); len(c) != 0 {
		t.Fatalf("containers after remove: %v", c)
	}
	if st, _ := e.client.Instance(t.Context(), key); st.Phase != agentapi.PhaseAbsent || !st.DataRetained {
		t.Fatalf("removed status = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, dataDir, "www", "index.html")); err != nil {
		t.Fatalf("data after remove: %v", err)
	}
	if got := e.psql("svc_at1", "select message from agenttest_config where id = 1"); got != "second" {
		t.Fatalf("database after remove = %q", got)
	}

	// Purge: the directory, the database and the role go; the key is free.
	purge := integrationSpec(id, 6, port, "second", agentapi.StateAbsent)
	purge.Purge = true
	e.mustApply(purge)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory after purge: %v", err)
	}
	if got := e.psql("orglane", "select count(*) from pg_database where datname = 'svc_at1'") + e.psql("orglane", "select count(*) from pg_roles where rolname = 'svc_at1'"); got != "00" {
		t.Fatalf("database or role after purge: %s", got)
	}
	if _, err := e.client.Instance(t.Context(), key); !errors.As(err, &pe) || pe.Code != agentapi.CodeNotFound {
		t.Fatalf("status after purge: %v", err)
	}

	// The key can be used again, by another instance, and starts empty.
	id2 := uuid.Must(uuid.NewV7())
	e.mustApply(integrationSpec(id2, 1, port, "fresh", agentapi.StateRunning))
	if body, err := httpGet(port); err != nil || body != "fresh\n" {
		t.Fatalf("reused key: %q, %v", body, err)
	}
	purge2 := integrationSpec(id2, 2, port, "fresh", agentapi.StateAbsent)
	purge2.Purge = true
	e.mustApply(purge2)

	e.checkAudit(map[string]bool{key: true})
}

// checkAudit asserts that every Docker invocation of the agent has one of
// the fixed shapes, for the instance keys of the test, and carries no input
// value: the core cannot make the agent run anything else.
func (e *testEnv) checkAudit(keys map[string]bool) {
	t := e.t
	t.Helper()
	// The hooks of every embedded version (configure and migrate).
	var hooks [][]string
	for _, ref := range e.registry.List() {
		pkg, _ := e.registry.Get(ref)
		for _, h := range pkg.Manifest.Hooks.Configure {
			hooks = append(hooks, append([]string{h.Service}, h.Command...))
		}
		if h := pkg.Manifest.Upgrade.Migrate; h != nil {
			hooks = append(hooks, append([]string{h.Service}, h.Command...))
		}
	}
	psql := []string{"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", "orglane", "-d", "postgres", "-f", "-"}
	psqlQuery := []string{"psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-U", "orglane", "-d", "postgres", "-f", "-"}
	dbKeys := map[string]bool{}
	for k := range keys {
		dbKeys[packages.DatabaseName(k)] = true
	}
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	if len(e.audit.calls) == 0 {
		t.Fatal("no Docker calls recorded")
	}
	for _, call := range e.audit.calls {
		joined := strings.Join(call, " ")
		for _, banned := range []string{hostile, "aW50ZWdyYXRpb24tZGItcGFzc3dvcmQ", testSecret, "second", "fresh"} {
			if strings.Contains(joined, banned) {
				t.Fatalf("a Docker call carries an input value: %v", call)
			}
		}
		switch call[0] {
		case "events":
			continue
		case "ps":
			if !slices.Equal(call[:3], []string{"ps", "--quiet", "--no-trunc"}) {
				t.Fatalf("unexpected call %v", call)
			}
			continue
		case "exec":
			switch {
			case len(call) == 3+len(psql) && call[1] == "-i" && slices.Equal(call[3:], psql):
			case len(call) == 3+len(psqlQuery) && call[1] == "-i" && slices.Equal(call[3:], psqlQuery):
			case len(call) == 8 && slices.Equal(call[2:7], []string{"pg_dump", "-U", "orglane", "-Fc", "-d"}) && dbKeys[call[7]]:
			case len(call) == 12 && call[1] == "-i" && slices.Equal(call[3:7], []string{"pg_restore", "-U", "orglane", "-d"}) && dbKeys[call[7]] &&
				slices.Equal(call[8:], []string{"--no-owner", "--role=" + call[7], "--exit-on-error", "--single-transaction"}):
			default:
				t.Fatalf("unexpected exec %v", call)
			}
			continue
		case "compose":
		default:
			t.Fatalf("unexpected call %v", call)
		}
		if len(call) < 10 || call[1] != "--project-name" || call[3] != "--project-directory" || call[5] != "-f" || call[7] != "--env-file" {
			t.Fatalf("unexpected compose call %v", call)
		}
		key, ok := cutPrefix(call[2], e.project+"-")
		dir := e.agent.instanceDir(key)
		if !ok || !keys[key] || call[4] != dir ||
			(call[6] != filepath.Join(dir, composeFile) && call[6] != filepath.Join(dir, stagingDir, composeFile)) {
			t.Fatalf("compose call for another project or directory: %v", call)
		}
		switch sub := call[9:]; sub[0] {
		case "config", "up", "stop", "down", "ps", "logs", "pull":
		case "exec":
			if len(sub) < 4 || sub[1] != "-T" || !slices.ContainsFunc(hooks, func(h []string) bool { return slices.Equal(sub[2:], h) }) {
				t.Fatalf("compose exec of something that is not a hook: %v", call)
			}
		default:
			t.Fatalf("unexpected compose subcommand %v", call)
		}
	}
}

// TestPolicyRefusesVariants installs every agenttest policy variant through
// the agent with real Docker: each fails with policy_violation and its rule,
// and nothing of it starts.
func TestPolicyRefusesVariants(t *testing.T) {
	project := startBundle(t)
	for _, name := range agenttest.PolicyVariants() {
		t.Run(name, func(t *testing.T) {
			src, rule, err := agenttest.PolicyVariant(name)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := packages.NewRegistry(src)
			if err != nil {
				t.Fatal(err)
			}
			e := startAgent(t, project, registry)
			op := e.apply("pv", integrationSpec(uuid.Must(uuid.NewV7()), 1, freePort(t), "x", agentapi.StateRunning))
			if op.State != agentapi.OpFailed || op.ErrorCode != agentapi.CodePolicyViolation || op.Rule != rule {
				t.Fatalf("operation = %+v, want policy_violation %s", op, rule)
			}
			if c := e.containers("pv"); len(c) != 0 {
				t.Fatalf("containers of a refused project: %v", c)
			}
			if _, err := os.Stat(filepath.Join(e.agent.instanceDir("pv"), composeFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("a refused project was written")
			}
			e.checkAudit(map[string]bool{"pv": true})
		})
	}
}

// TestUnauthenticatedSocketClient talks to the real socket without the key.
func TestUnauthenticatedSocketClient(t *testing.T) {
	project := startBundle(t)
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	e := startAgent(t, project, registry)
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.socket)
	}}}
	body := mustJSON(t, integrationSpec(uuid.Must(uuid.NewV7()), 1, freePort(t), "x", agentapi.StateRunning))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://agent/v1/instances/un", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned PUT: %d", resp.StatusCode)
	}
	if _, err := os.Stat(e.agent.instanceDir("un")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unsigned request created an instance")
	}
}

// TestConfigureHooksAfterStopped: configure hooks run on the first start of
// an instance installed stopped, and on the start after its settings
// changed while it was stopped.
func TestConfigureHooksAfterStopped(t *testing.T) {
	project := startBundle(t)
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	e := startAgent(t, project, registry)
	const key = "at2"
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", packages.InstanceProject(project, key), "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
	row := func() string {
		return e.psql("svc_at2", "select coalesce((select message from agenttest_config where id = 1), 'none')")
	}

	// Installed stopped: nothing runs, no hook yet.
	e.mustApplyKey(key, integrationSpec(id, 1, port, "first", agentapi.StateStopped))
	if _, err := httpGet(port); err == nil {
		t.Fatal("an instance installed stopped serves")
	}
	// The first start runs the hooks for the unchanged render.
	e.mustApplyKey(key, integrationSpec(id, 2, port, "first", agentapi.StateRunning))
	if got := row(); got != "first" {
		t.Fatalf("after the first start the database row is %q", got)
	}
	// Reconfigured while stopped: the next start runs the hooks again.
	e.mustApplyKey(key, integrationSpec(id, 3, port, "first", agentapi.StateStopped))
	e.mustApplyKey(key, integrationSpec(id, 4, port, "changed", agentapi.StateStopped))
	e.mustApplyKey(key, integrationSpec(id, 5, port, "changed", agentapi.StateRunning))
	if got := row(); got != "changed" {
		t.Fatalf("after a start with settings changed while stopped the database row is %q", got)
	}
	if body, err := httpGet(port); err != nil || body != "changed\n" {
		t.Fatalf("served %q, %v", body, err)
	}
	purge := integrationSpec(id, 6, port, "changed", agentapi.StateAbsent)
	purge.Purge = true
	e.mustApplyKey(key, purge)
	e.checkAudit(map[string]bool{key: true})
}
