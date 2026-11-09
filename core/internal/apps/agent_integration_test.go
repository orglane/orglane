//go:build integration

package apps_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/agent"
	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/jobs"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

const wiringAgentSecret = "core-agent-wiring-test-secret-0123456789abcdef" //nolint:gosec // a test secret

// Images of the bundle stand-in, as in core/internal/agent's tests.
const (
	bundlePostgresImage = "postgres:18.6-alpine3.24@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5"
	bundleBusyboxImage  = "busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"
)

// agentHost is a real agent process (in-process, real Docker) on a minimal
// stand-in of the Compose bundle: PostgreSQL on the services network and a
// container on the edge network, labeled like the bundle's.
type agentHost struct {
	t        *testing.T
	project  string
	socket   string
	dataDir  string
	registry *packages.Registry
	stop     func()
}

func newAgentHost(t *testing.T) *agentHost {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available")
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	project := "core" + hex.EncodeToString(b)
	file := filepath.Join(t.TempDir(), "compose.yaml")
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
    init: true
    command: ["sleep", "2147483647"]
    networks: [edge]
networks:
  edge: {}
  database: {internal: true}
  services: {internal: true}
`, project, bundlePostgresImage, bundleBusyboxImage)
	if err := os.WriteFile(file, []byte(bundle), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		// Instance projects first: they use the bundle's networks.
		out, _ := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label=com.docker.compose.project="+packages.InstanceProject(project, "managed-test")).Output() //nolint:gosec // test commands
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_ = exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, ids...)...).Run() //nolint:gosec // test commands
		}
		_ = exec.CommandContext(ctx, "docker", "network", "rm", packages.InstanceProject(project, "managed-test")+"_default").Run()                                     //nolint:gosec // test commands
		if out, err := exec.CommandContext(ctx, "docker", "compose", "-p", project, "-f", file, "down", "--volumes", "--remove-orphans").CombinedOutput(); err != nil { //nolint:gosec // test commands
			t.Logf("bundle down: %v\n%s", err, out)
		}
	})
	if out, err := exec.CommandContext(t.Context(), "docker", "compose", "-p", project, "-f", file, "up", "--detach", "--wait").CombinedOutput(); err != nil { //nolint:gosec // test commands
		t.Fatalf("bundle up: %v\n%s", err, out)
	}
	sockDir, err := os.MkdirTemp("", "core-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	return &agentHost{t: t, project: project, socket: filepath.Join(sockDir, "agent.sock"), dataDir: t.TempDir(), registry: registry}
}

// newestAgenttest is the agenttest version a new install gets: the newest
// one embedded (agenttest also has the versions of the upgrade tests).
func newestAgenttest(t *testing.T, registry *packages.Registry) string {
	t.Helper()
	pkg, ok := registry.Latest("agenttest")
	if !ok {
		t.Fatal("agenttest is not embedded")
	}
	return pkg.Manifest.Version
}

// start runs the agent on the host's data directory until stop.
func (a *agentHost) start() {
	a.t.Helper()
	cfg := config.Default().Agent
	cfg.Secret = wiringAgentSecret
	cfg.Listen = a.socket
	cfg.DataDir = a.dataDir
	cfg.PeerUID = -1
	cfg.ComposeProject = a.project
	cfg.EdgeNetwork, cfg.ServicesNetwork = a.project+"_edge", a.project+"_services"
	cfg.ReconcileInterval = time.Hour
	ag, err := agent.New(agent.Options{
		Config: cfg, Version: "dev", Registry: a.registry, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		EventDebounce: time.Second, EventMinInterval: 2 * time.Second,
	})
	if err != nil {
		a.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ag.Run(ctx) }()
	stopped := false
	a.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			a.t.Errorf("agent: %v", err)
		}
	}
	a.t.Cleanup(a.stop)
	for range 200 {
		if c, err := (&net.Dialer{}).DialContext(a.t.Context(), "unix", a.socket); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.t.Fatal("the agent did not listen")
}

func (a *agentHost) client() *agentclient.Client {
	a.t.Helper()
	c, err := agentclient.New(a.socket, wiringAgentSecret, "dev")
	if err != nil {
		a.t.Fatal(err)
	}
	a.t.Cleanup(c.Close)
	return c
}

// agentConfig points a service at the agent host with fast test pacing.
func (a *agentHost) agentConfig(cfg *apps.Config) {
	cfg.Agent = a.client()
	cfg.Packages = a.registry
	cfg.AgentPollInterval = 200 * time.Millisecond
	cfg.AgentRetryMax = time.Second
	cfg.AgentWatchInterval = 100 * time.Millisecond
	cfg.SyncInterval = time.Minute
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

// applyUntilDone works an agent_apply job by hand until it finishes,
// sleeping through snoozes and retries. It returns how often it ran.
func applyUntilDone(t *testing.T, svc *apps.Service, args apps.AgentApplyArgs) int {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	for runs := 1; ; runs++ {
		err := svc.RunAgentApply(t.Context(), args)
		var snooze *river.JobSnoozeError
		switch {
		case err == nil:
			return runs
		case errors.As(err, &snooze):
			time.Sleep(min(snooze.Duration, 200*time.Millisecond))
		default:
			t.Logf("apply generation %d: %v", args.Generation, err)
			time.Sleep(500 * time.Millisecond)
		}
		if time.Now().After(deadline) {
			t.Fatalf("generation %d did not finish", args.Generation)
		}
	}
}

// applySucceeded is applyUntilDone for a generation that must apply: a
// failed operation fails the test here, with its error code, not later as a
// missing file or an unexpected state.
func (h *harness) applySucceeded(args apps.AgentApplyArgs) {
	h.t.Helper()
	applyUntilDone(h.t, h.svc, args)
	if op := h.operation(args.InstanceID, args.Generation); op.State != agentapi.OpSucceeded {
		h.t.Fatalf("generation %d: operation %s, error code %q", args.Generation, op.State, derefS(op.ErrorCode))
	}
}

func (h *harness) operation(id uuid.UUID, gen int64) store.AppOperation {
	h.t.Helper()
	op, err := store.New(h.pool).GetAppPushOperation(h.ctx, store.GetAppPushOperationParams{InstanceID: id, Generation: &gen})
	if err != nil {
		h.t.Fatalf("operation of generation %d: %v", gen, err)
	}
	return op
}

func (h *harness) reload(id uuid.UUID) apps.Instance {
	h.t.Helper()
	inst, err := h.svc.Instance(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return inst
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not in time", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func httpGet(port int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// TestAgentWiring drives a managed instance through a real agent with real
// Docker: an install that waits for the agent and then reaches running and
// active on River; a refused spec failing the install; an agent restart
// noticed by its boot ID; a lost agent state recovered by the sync; a core
// restart and a lost write in the middle of an operation; a generation
// conflict; a superseded generation; provider health moving the instance
// between active and degraded; and health becoming unknown while the agent
// is gone.
func TestAgentWiring(t *testing.T) {
	host := newAgentHost(t)
	h := newHarness(t, host.agentConfig)
	ctx := h.ctx
	port := freePort(t)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": port, "message": "from-core"})

	// The agent is not running yet: the install waits for it.
	inst, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != apps.StateInstalling || inst.DesiredGeneration != 1 || *inst.BaseUrl != "https://agenttest.work.acme.test" {
		t.Fatalf("installed %+v", inst.AppInstance)
	}
	gen1 := apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1}
	if err := h.svc.RunAgentApply(ctx, gen1); err == nil {
		t.Fatal("a push without an agent did not ask for a retry")
	}
	if op := h.operation(inst.ID, 1); op.State != agentapi.OpQueued || derefS(op.ErrorCode) != apps.CodeAgentUnavailable || op.Kind != agentapi.KindApply {
		t.Fatalf("waiting operation %+v", op)
	}
	if got := h.reload(inst.ID); got.WaitingForAgent != apps.CodeAgentUnavailable {
		t.Fatalf("waiting for the agent = %q", got.WaitingForAgent)
	}

	// The agent starts: River retries the push, polls the operation and
	// runs Setup, which activates the instance.
	host.start()
	watch, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go h.svc.WatchAgent(watch)
	jcfg := config.Default().Jobs
	jcfg.FetchPollInterval = 100 * time.Millisecond
	client, err := jobs.NewClient(h.pool, jcfg, slog.New(slog.NewTextHandler(io.Discard, nil)), jobs.Options{Apps: h.svc})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "install", 4*time.Minute, func() bool { return h.reload(inst.ID).State == apps.StateActive })
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := client.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	got := h.reload(inst.ID)
	op := h.operation(inst.ID, 1)
	if op.State != agentapi.OpSucceeded || op.AgentOperationID == nil || op.ErrorCode != nil || op.FinishedAt == nil ||
		got.ObservedGeneration != 1 || derefS(got.PackageVersion) != newestAgenttest(t, host.registry) || got.WaitingForAgent != "" || h.agentProv.Setups(inst.ID) != 1 {
		t.Fatalf("after install: %+v, operation %+v, setups %d", got.AppInstance, op, h.agentProv.Setups(inst.ID))
	}
	waitUntil(t, "the service answers", time.Minute, func() bool { return strings.Contains(httpGet(port), "from-core") })

	// A spec the agent refuses (a port below the package's minimum) fails
	// the install; the agent never started anything for it.
	h.agentProv.SetPackageSettings(map[string]any{"http_port": 80})
	bad, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest", Key: "refused", BaseURL: "https://refused.work.acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: bad.ID, Generation: 1})
	if op, got := h.operation(bad.ID, 1), h.reload(bad.ID); op.State != agentapi.OpFailed || derefS(op.ErrorCode) != agentapi.CodeInvalidInput ||
		got.State != apps.StateFailed || derefS(got.HealthCode) != apps.CodeInstallFailed {
		t.Fatalf("refused install: %+v, %+v", op, got.AppInstance)
	}
	h.agentProv.SetPackageSettings(map[string]any{"http_port": port, "message": "from-core"})

	// The agent restarts: the watcher sees the new boot ID and queues a
	// sync, which finds nothing lost.
	syncJobs := func() int { return h.jobs(apps.AgentSyncArgs{}.Kind()) }
	h.exec(`DELETE FROM river_job WHERE kind = $1`, apps.AgentSyncArgs{}.Kind())
	host.stop()
	host.start()
	waitUntil(t, "a sync after the agent restarted", 30*time.Second, func() bool { return syncJobs() == 1 })
	if err := h.svc.RunAgentSync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.DesiredGeneration != 1 || got.ObservedGeneration != 1 || got.State != apps.StateActive || got.Health != apps.HealthHealthy {
		t.Fatalf("after an agent restart: %+v", got.AppInstance)
	}
	stopWatch()

	// The agent loses its state (a new, empty data directory): the sync
	// pushes the desired state again as generation 2.
	host.stop()
	host.dataDir = t.TempDir()
	host.start()
	if err := h.svc.RunAgentSync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.DesiredGeneration != 2 {
		t.Fatalf("lost state was not pushed again: %+v", got.AppInstance)
	}
	gen2 := apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2}
	var snooze *river.JobSnoozeError
	if err := h.svc.RunAgentApply(ctx, gen2); !errors.As(err, &snooze) {
		t.Fatalf("first run of generation 2 = %v", err)
	}
	first := h.operation(inst.ID, 2)
	if first.AgentOperationID == nil || first.Kind != agentapi.KindApply {
		t.Fatalf("pushed operation %+v", first)
	}
	// The record of the agent's operation is lost (as if the core died
	// before it committed), and the core restarts: the repeated push gets
	// the same operation, which is polled to its end.
	h.exec(`UPDATE app_operations SET agent_operation_id = NULL, state = 'queued', phase = NULL, error_code = NULL, finished_at = NULL WHERE id = $1`, first.ID)
	restarted := h.restartCore(host.agentConfig)
	if err := restarted.RunAgentApply(ctx, gen2); err != nil && !errors.As(err, &snooze) {
		t.Fatal(err)
	}
	if again := h.operation(inst.ID, 2); again.AgentOperationID == nil || *again.AgentOperationID != *first.AgentOperationID {
		t.Fatalf("the repeated push got operation %v, want %v", again.AgentOperationID, *first.AgentOperationID)
	}
	applyUntilDone(t, restarted, gen2)
	if runs := applyUntilDone(t, restarted, gen2); runs != 1 {
		t.Fatalf("a finished generation ran %d times", runs)
	}
	if op, got := h.operation(inst.ID, 2), h.reload(inst.ID); op.State != agentapi.OpSucceeded || got.ObservedGeneration != 2 || got.State != apps.StateActive {
		t.Fatalf("after recovery: %+v, %+v", op, got.AppInstance)
	}
	if h.agentProv.Setups(inst.ID) != 1 || h.jobs(apps.SetupArgs{}.Kind()) != 0 {
		t.Fatal("a re-push of an active instance ran Setup again")
	}
	waitUntil(t, "the reinstalled service answers", time.Minute, func() bool { return strings.Contains(httpGet(port), "from-core") })

	// The configuration that builds the spec changes between two pushes
	// of a generation: the agent answers generation_conflict and the same
	// desired state is pushed as the next generation.
	gen3, err := h.svc.ResyncForTest(ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunAgentApply(ctx, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: gen3}); err != nil && !errors.As(err, &snooze) {
		t.Fatalf("push of generation %d = %v", gen3, err)
	}
	h.exec(`UPDATE app_operations SET agent_operation_id = NULL, state = 'queued', phase = NULL, error_code = NULL, finished_at = NULL
		WHERE instance_id = $1 AND generation = $2`, inst.ID, gen3)
	h.exec(`UPDATE app_instances SET base_url = 'https://code.work.acme.test' WHERE id = $1`, inst.ID)
	if err := h.svc.RunAgentApply(ctx, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: gen3}); err != nil {
		t.Fatal(err)
	}
	if op, got := h.operation(inst.ID, gen3), h.reload(inst.ID); op.State != agentapi.OpFailed || derefS(op.ErrorCode) != agentapi.CodeGenerationConflict || got.DesiredGeneration != gen3+1 {
		t.Fatalf("conflict: %+v, %+v", op, got.AppInstance)
	}
	h.applySucceeded(apps.AgentApplyArgs{InstanceID: inst.ID, Generation: gen3 + 1})

	// A generation replaced before it was pushed ends superseded; the
	// newer one is applied.
	older, err := h.svc.ResyncForTest(ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := h.svc.ResyncForTest(ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: older})
	h.applySucceeded(apps.AgentApplyArgs{InstanceID: inst.ID, Generation: newer})
	if op := h.operation(inst.ID, older); op.State != agentapi.OpFailed || derefS(op.ErrorCode) != agentapi.CodeSuperseded || op.AgentOperationID != nil {
		t.Fatalf("superseded operation %+v", op)
	}
	if op, got := h.operation(inst.ID, newer), h.reload(inst.ID); op.State != agentapi.OpSucceeded || got.ObservedGeneration != newer || got.State != apps.StateActive {
		t.Fatalf("newer generation: %+v, %+v", op, got.AppInstance)
	}

	// The provider's health check fails: degraded; it recovers: active.
	h.agentProv.Fail("health", retryable("unreachable"))
	if err := h.svc.RunAgentSync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateDegraded || derefS(got.HealthCode) != apps.CodeHealthCheckFailed {
		t.Fatalf("after a failed health check: %+v", got.AppInstance)
	}
	if err := h.svc.RunAgentSync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive || got.Health != apps.HealthHealthy || got.HealthCode != nil {
		t.Fatalf("after a passing health check: %+v", got.AppInstance)
	}

	// The agent is gone for two sync intervals: health becomes unknown;
	// the agent is back: healthy again.
	host.stop()
	h.exec(`UPDATE app_instances SET health_checked_at = now() - interval '1 hour' WHERE id = $1`, inst.ID)
	if err := h.svc.RunAgentSync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.Health != apps.HealthUnknown || derefS(got.HealthCode) != apps.CodeAgentUnavailable || got.State != apps.StateActive {
		t.Fatalf("with the agent gone: %+v", got.AppInstance)
	}
	host.start()
	if err := h.svc.RunAgentSync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.Health != apps.HealthHealthy || got.HealthCode != nil || got.DesiredGeneration != newer {
		t.Fatalf("with the agent back: %+v", got.AppInstance)
	}
	if n := h.auditCount(audit.AppInstanceStateChanged); n < 4 {
		t.Fatalf("%d state changes audited", n)
	}
}

// TestAgentApplyWithoutAgent: with managed services off, a push waits for
// the agent and the sync keeps a job for it; a generation replaced while
// waiting ends superseded without failing the install.
func TestAgentApplyWithoutAgent(t *testing.T) {
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	errBlocked := errors.New("blocked by the preflight")
	var plans []apps.InstallPlan
	h := newHarness(t, func(c *apps.Config) {
		c.Packages = registry
		c.InstallPreflight = func(_ context.Context, p apps.InstallPlan) (apps.InstallBudget, error) {
			plans = append(plans, p)
			if p.Key == "blocked" {
				return apps.InstallBudget{}, errBlocked
			}
			return apps.InstallBudget{}, nil
		}
	})
	if _, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "testmanaged"}); err == nil {
		t.Fatal("installed a provider whose package is not embedded")
	}
	if _, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest", Key: "blocked"}); !errors.Is(err, errBlocked) {
		t.Fatalf("install refused by the preflight = %v", err)
	}
	if n := h.count(`SELECT count(*) FROM app_instances`); n != 0 {
		t.Fatalf("a refused install left %d instances", n)
	}
	inst, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	if h.jobs(apps.AgentApplyArgs{}.Kind()) != 1 {
		t.Fatal("the install queued no push")
	}
	if len(plans) != 2 || plans[1].Key != "managed-test" || plans[1].BaseURL != "https://agenttest.work.acme.test" || plans[1].Package.Manifest.Version != newestAgenttest(t, registry) {
		t.Fatalf("preflight plans = %+v", plans)
	}
	if up := inst.View().Upstream; up == nil || up.String() != "http://managed-test-web:8080" {
		t.Fatalf("upstream = %v", up)
	}
	if err := h.svc.RunAgentApply(h.ctx, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1}); err == nil {
		t.Fatal("a push without an agent did not ask for a retry")
	}
	if got := h.reload(inst.ID); got.WaitingForAgent != apps.CodeAgentUnavailable || got.State != apps.StateInstalling {
		t.Fatalf("waiting: %+v %q", got.AppInstance, got.WaitingForAgent)
	}
	// The job is discarded (attempts exhausted): the sync queues it again.
	h.exec(`DELETE FROM river_job WHERE kind = $1`, apps.AgentApplyArgs{}.Kind())
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.jobs(apps.AgentApplyArgs{}.Kind()) != 1 {
		t.Fatal("the sync did not queue the waiting push again")
	}
	gen2, err := h.svc.ResyncForTest(h.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunAgentApply(h.ctx, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	op := h.operation(inst.ID, 1)
	if op.State != agentapi.OpFailed || derefS(op.ErrorCode) != agentapi.CodeSuperseded || op.FinishedAt == nil {
		t.Fatalf("replaced operation %+v", op)
	}
	if err := h.svc.RunAgentApply(h.ctx, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: gen2}); err == nil {
		t.Fatal("a push without an agent did not ask for a retry")
	}
	if got := h.reload(inst.ID); got.State != apps.StateInstalling || got.WaitingForAgent != apps.CodeAgentUnavailable {
		t.Fatalf("a superseded generation changed the instance: %+v", got.AppInstance)
	}
	// Observations are fenced by generation: an older one changes nothing.
	q := store.New(h.pool)
	v3, v1 := "3.0.0-1", "1.0.0-1"
	h.exec(`UPDATE app_instances SET observed_generation = 3, package_version = $2 WHERE id = $1`, inst.ID, v3)
	if err := q.SetAppInstanceApplied(h.ctx, store.SetAppInstanceAppliedParams{ID: inst.ID, Generation: 2, PackageVersion: &v1}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.SetAppInstanceObservation(h.ctx, store.SetAppInstanceObservationParams{ID: inst.ID, ObservedGeneration: 1, PackageVersion: &v1}); err != nil || n != 0 {
		t.Fatalf("stale observation: %d rows, %v", n, err)
	}
	if got := h.reload(inst.ID); got.ObservedGeneration != 3 || derefS(got.PackageVersion) != v3 {
		t.Fatalf("an older observation moved the instance back: %+v", got.AppInstance)
	}
	// Health the sync could not check for two intervals becomes unknown,
	// replacing the sync's own code.
	h.exec(`UPDATE app_instances SET state = 'degraded', health = 'unhealthy', health_code = 'agent_unhealthy',
		health_checked_at = now() - interval '1 hour' WHERE id = $1`, inst.ID)
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.Health != apps.HealthUnknown || derefS(got.HealthCode) != apps.CodeAgentUnavailable || got.State != apps.StateDegraded {
		t.Fatalf("stale health: %+v", got.AppInstance)
	}
	ops, err := h.svc.Operations(h.ctx, inst.ID, 10, 0)
	if err != nil || len(ops) != 2 || derefS(ops[0].Reason) != apps.ReasonResync || *ops[0].Generation != gen2 || derefS(ops[1].Reason) != apps.ReasonInstall {
		t.Fatalf("history = %+v, %v", ops, err)
	}
}

func derefS(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
