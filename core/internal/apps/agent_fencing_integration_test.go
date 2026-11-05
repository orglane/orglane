//go:build integration

package apps_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/packages"
)

// delayedStatuses delays a real agent's Instances answer: after reads it,
// and before the sync gets it, after runs (a newer generation finishes).
// Every status and operation comes from the real agent and Docker.
type delayedStatuses struct {
	apps.AgentClient
	after func()
}

func (c *delayedStatuses) Instances(ctx context.Context) ([]agentapi.InstanceStatus, error) {
	statuses, err := c.AgentClient.Instances(ctx)
	if err == nil && c.after != nil {
		f := c.after
		c.after = nil
		f()
	}
	return statuses, err
}

// TestStaleHealthCannotDegradeNewerGeneration: a status read while
// generation 1's web container is paused (unhealthy) arrives after
// generation 2 was applied; it must not degrade the instance.
func TestStaleHealthCannotDegradeNewerGeneration(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	delayed := &delayedStatuses{AgentClient: host.client()}
	h := newHarness(t, host.agentConfig, func(c *apps.Config) { c.Agent = delayed })
	port := freePort(t)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": port, "message": "first"})
	inst, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1})
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	project := packages.InstanceProject(host.project, inst.Key)
	out, err := exec.CommandContext(h.ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project="+project, "--filter", "label=com.docker.compose.service=web").Output() //nolint:gosec // test commands
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		t.Fatal("no web container")
	}
	if out, err := exec.CommandContext(h.ctx, "docker", "pause", id).CombinedOutput(); err != nil { //nolint:gosec // test commands
		t.Fatalf("pause: %v %s", err, out)
	}
	delayed.after = func() {
		if out, err := exec.CommandContext(h.ctx, "docker", "unpause", id).CombinedOutput(); err != nil { //nolint:gosec // test commands
			t.Fatalf("unpause: %v %s", err, out)
		}
		h.agentProv.SetPackageSettings(map[string]any{"http_port": port, "message": "second"})
		gen, err := h.svc.ResyncForTest(h.ctx, inst.ID)
		if err != nil {
			t.Fatal(err)
		}
		applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: gen})
	}
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	got := h.reload(inst.ID)
	if got.ObservedGeneration != 2 || got.State != apps.StateActive || got.Health != apps.HealthHealthy {
		t.Fatalf("stale generation 1 health overwrote successful generation 2: observed=%d state=%s health=%s code=%s", got.ObservedGeneration, got.State, got.Health, derefS(got.HealthCode))
	}
	// The next sync reads generation 2's status and records it.
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive || got.Health != apps.HealthHealthy || got.HealthCheckedAt == nil {
		t.Fatalf("after a current sync: %+v", got.AppInstance)
	}
}

// TestStaleRecoveryCannotRunSetupForNewerGeneration: the agent's own
// retry got install_failed generation 1 running, but before that status
// reaches the sync a newer generation 2 was recorded and refused; the
// stale status must not move the instance back to installing or run Setup.
func TestStaleRecoveryCannotRunSetupForNewerGeneration(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	delayed := &delayedStatuses{AgentClient: host.client()}
	h := newHarness(t, host.agentConfig, func(c *apps.Config) { c.Agent = delayed })
	port := freePort(t)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": port})
	inst, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1})
	h.exec(`DELETE FROM river_job WHERE kind = $1`, apps.SetupArgs{}.Kind())
	// The core recorded generation 1 as failed; the agent's own retry has
	// brought it up since.
	h.exec(`UPDATE app_instances SET state = 'failed', health = 'unhealthy', health_code = 'install_failed', observed_generation = 0 WHERE id = $1`, inst.ID)
	h.exec(`UPDATE app_operations SET state = 'failed', error_code = 'compose_failed' WHERE instance_id = $1 AND generation = 1`, inst.ID)
	delayed.after = func() {
		h.agentProv.SetPackageSettings(map[string]any{"http_port": 80})
		gen, err := h.svc.ResyncForTest(h.ctx, inst.ID)
		if err != nil {
			t.Fatal(err)
		}
		applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: gen})
		if got := h.reload(inst.ID); got.State != apps.StateFailed {
			t.Fatalf("expected failed generation 2, got %s", got.State)
		}
	}
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateFailed || h.jobs(apps.SetupArgs{}.Kind()) != 0 {
		t.Fatalf("stale generation 1 recovered unapplied generation 2: desired=%d observed=%d state=%s setup_jobs=%d", got.DesiredGeneration, got.ObservedGeneration, got.State, h.jobs(apps.SetupArgs{}.Kind()))
	}

}

// TestInstallFailedRecoversWhenTheAgentRunsIt: without a newer
// generation, an install_failed instance whose generation the agent's own
// retry got running goes back to installing and runs Setup, also while an
// earlier setup job still shows running in River (it may have read the
// state already, so it never takes the new job's place).
func TestInstallFailedRecoversWhenTheAgentRunsIt(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	h := newHarness(t, host.agentConfig)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t)})
	inst, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1})
	h.exec(`UPDATE river_job SET state = 'running', attempted_at = now() WHERE kind = $1`, apps.SetupArgs{}.Kind())
	h.exec(`UPDATE app_instances SET state = 'failed', health = 'unhealthy', health_code = 'install_failed', observed_generation = 0 WHERE id = $1`, inst.ID)
	h.exec(`UPDATE app_operations SET state = 'failed', error_code = 'compose_failed' WHERE instance_id = $1 AND generation = 1`, inst.ID)
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateInstalling || got.ObservedGeneration != 1 || h.jobs(apps.SetupArgs{}.Kind()) != 1 {
		t.Fatalf("not recovered: %+v, setup jobs %d", got.AppInstance, h.jobs(apps.SetupArgs{}.Kind()))
	}
}

// TestLostAgentOperationReplaysCurrentGeneration: the agent loses its
// state between the push and a poll; the poll's not_found resets the
// operation and the generation is pushed and applied again.
func TestLostAgentOperationReplaysCurrentGeneration(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	h := newHarness(t, host.agentConfig)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t)})
	inst, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	args := apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1}
	var snooze *river.JobSnoozeError
	if err := h.svc.RunAgentApply(h.ctx, args); err != nil && !errors.As(err, &snooze) {
		t.Fatal(err)
	}
	first := h.operation(inst.ID, 1)
	if first.AgentOperationID == nil {
		t.Fatal("no agent operation recorded")
	}
	host.stop()
	host.dataDir = t.TempDir()
	host.start()
	if err := h.svc.RunAgentApply(h.ctx, args); !errors.As(err, &snooze) {
		t.Fatalf("lost operation poll = %v", err)
	}
	if reset := h.operation(inst.ID, 1); reset.AgentOperationID != nil || reset.State != agentapi.OpQueued || reset.FinishedAt != nil {
		t.Fatalf("operation not reset: %+v", reset)
	}
	applyUntilDone(t, h.svc, args)
	if got := h.operation(inst.ID, 1); got.State != agentapi.OpSucceeded || got.AgentOperationID == nil || *got.AgentOperationID == *first.AgentOperationID {
		t.Fatalf("replayed operation = %+v", got)
	}
	if got := h.reload(inst.ID); got.DesiredGeneration != 1 || got.ObservedGeneration != 1 {
		t.Fatalf("replayed generation desired=%d observed=%d", got.DesiredGeneration, got.ObservedGeneration)
	}
}

// resyncedTo applies the generation the sync recorded after the agent lost
// what it had, and checks that the agent and the core agree on it again.
func resyncedTo(t *testing.T, h *harness, host *agentHost, inst apps.Instance, before int64) {
	t.Helper()
	for range 2 { // a second sync must not record yet another generation
		if err := h.svc.RunAgentSync(h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := h.reload(inst.ID)
	if got.DesiredGeneration != before+1 {
		t.Fatalf("the sync did not push the lost state again: desired=%d observed=%d state=%s", got.DesiredGeneration, got.ObservedGeneration, got.State)
	}
	if op := h.operation(inst.ID, got.DesiredGeneration); op.Kind != agentapi.KindApply {
		t.Fatalf("resync operation %+v", op)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: got.DesiredGeneration})
	status, err := host.client().Instance(h.ctx, inst.Key)
	if err != nil {
		t.Fatal(err)
	}
	got = h.reload(inst.ID)
	if status.AcceptedGeneration != got.DesiredGeneration || status.ObservedGeneration != got.DesiredGeneration || got.ObservedGeneration != got.DesiredGeneration {
		t.Fatalf("after the resync: agent accepted=%d observed=%d, core desired=%d observed=%d", status.AcceptedGeneration, status.ObservedGeneration, got.DesiredGeneration, got.ObservedGeneration)
	}
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if again := h.reload(inst.ID); again.DesiredGeneration != got.DesiredGeneration || again.State != apps.StateActive || again.Health != apps.HealthHealthy {
		t.Fatalf("after a sync of the resynced instance: %+v", again.AppInstance)
	}
}

// TestAgentRetainingAnOlderGenerationIsResynced: the agent comes back with
// an older generation's state (its files restored from before generation
// 2). Its status is fresh but lower than what the core recorded, so the
// observation is not written; the lost generation is still pushed again.
func TestAgentRetainingAnOlderGenerationIsResynced(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	h := newHarness(t, host.agentConfig)
	port := freePort(t)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": port, "message": "generation-one"})
	inst, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1})
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(host.dataDir, "instances", inst.Key)
	old := map[string][]byte{}
	for _, name := range []string{"desired.json", "current.json"} {
		if old[name], err = os.ReadFile(filepath.Join(dir, name)); err != nil { //nolint:gosec // the agent's test data directory
			t.Fatal(err)
		}
	}
	h.agentProv.SetPackageSettings(map[string]any{"http_port": port, "message": "generation-two"})
	gen, err := h.svc.ResyncForTest(h.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: gen})
	host.stop()
	for name, data := range old {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host.start()
	if st, err := host.client().Instance(h.ctx, inst.Key); err != nil || st.AcceptedGeneration != 1 || st.ObservedGeneration != 1 {
		t.Fatalf("restored agent: %+v, %v", st, err)
	}
	resyncedTo(t, h, host, inst, 2)
}

// TestAgentWithIdentityButLostSpecsIsResynced: the agent keeps the
// instance's directory and identity but lost its specs, so it reports the
// instance with generation 0; the lost generation is pushed again.
func TestAgentWithIdentityButLostSpecsIsResynced(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	h := newHarness(t, host.agentConfig)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t)})
	inst, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1})
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	host.stop()
	for _, name := range []string{"desired.json", "current.json"} {
		if err := os.Remove(filepath.Join(host.dataDir, "instances", inst.Key, name)); err != nil {
			t.Fatal(err)
		}
	}
	host.start()
	if st, err := host.client().Instance(h.ctx, inst.Key); err != nil || st.InstanceID != inst.ID || st.AcceptedGeneration != 0 || st.ObservedGeneration != 0 {
		t.Fatalf("agent without specs: %+v, %v", st, err)
	}
	resyncedTo(t, h, host, inst, 1)
}
