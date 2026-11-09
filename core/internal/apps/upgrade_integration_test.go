//go:build integration

package apps_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// registryWithVersions builds the package registry of a simulated Orglane
// release: only the named agenttest versions are embedded.
func registryWithVersions(t *testing.T, versions ...string) *packages.Registry {
	t.Helper()
	src := fstest.MapFS{}
	for _, v := range versions {
		err := fs.WalkDir(agenttest.FS, v, func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := fs.ReadFile(agenttest.FS, name)
			if err != nil {
				return err
			}
			src[name] = &fstest.MapFile{Data: data}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := packages.NewRegistry(src)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// runningOn installs agenttest version, applies it and runs Setup, leaving
// an active instance.
func runningOn(t *testing.T, h *harness, version string) apps.Instance {
	t.Helper()
	inst, err := h.svc.InstallVersionForTest(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"}, version)
	if err != nil {
		t.Fatal(err)
	}
	h.applySucceeded(apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1})
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 1}, 1, 8); err != nil {
		t.Fatal(err)
	}
	got := h.reload(inst.ID)
	if got.State != apps.StateActive || derefS(got.PackageVersion) != version {
		t.Fatalf("install %s: %+v", version, got.AppInstance)
	}
	return got
}

// upgradeStatus is the API's upgrade status of an instance.
func upgradeStatus(t *testing.T, h *harness, id uuid.UUID) (apps.UpgradeStatus, bool) {
	t.Helper()
	inst, err := h.svc.Instance(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	status, ok, err := h.svc.UpgradeStatus(h.ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	return status, ok
}

func hasFencedUpstream(upstreams []string) bool {
	for _, u := range upstreams {
		if strings.HasSuffix(u, "-fenced:8080") {
			return true
		}
	}
	return false
}

// desiredReason is the reason of a desired generation.
func desiredReason(t *testing.T, h *harness, id uuid.UUID, generation int64) string {
	t.Helper()
	ds, err := store.New(h.pool).GetAppDesiredSpec(h.ctx, store.GetAppDesiredSpecParams{InstanceID: id, Generation: generation})
	if err != nil {
		t.Fatal(err)
	}
	return ds.Reason
}

// TestAgentUpgradeConfirmed drives automatic planning across the required
// stop 1.0.0 → 2.0.0 → 3.0.0 through the real agent: each candidate is
// checked through its fenced alias and confirmed, the core chains the next
// step in the operation's transaction, and Setup activates the last one.
func TestAgentUpgradeConfirmed(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	h := newHarness(t, host.agentConfig)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t), "message": "upgrade"})
	inst := runningOn(t, h, agenttest.Version)

	// A new Orglane release would embed 2.0.0-1 and 3.0.0-1; here the core
	// already does. The sync plans the first required stop.
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	got := h.reload(inst.ID)
	if got.State != apps.StateUpgrading || got.DesiredGeneration != 2 {
		t.Fatalf("planner: %+v", got.AppInstance)
	}
	op2 := h.operation(inst.ID, 2)
	if op2.Kind != agentapi.KindUpgrade || derefS(op2.ToVersion) != agenttest.Version2 || derefS(op2.FromVersion) != agenttest.Version {
		t.Fatalf("first step: %+v", op2)
	}
	if status, ok := upgradeStatus(t, h, inst.ID); !ok || status.State != apps.UpgradeInProgress || status.TargetVersion != agenttest.Version2 {
		t.Fatalf("status while upgrading: %+v, %v", status, ok)
	}

	// The agent waits for confirmation: the core checks the fenced alias,
	// confirms, and records the next required stop in the same transaction.
	h.applySucceeded(apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2})
	if op := h.operation(inst.ID, 2); op.State != agentapi.OpSucceeded || derefS(op.Phase) != "" {
		t.Fatalf("confirmed upgrade: %+v", op)
	}
	if !hasFencedUpstream(h.agentProv.HealthUpstreams()) {
		t.Fatalf("the candidate health check did not use the fenced alias: %v", h.agentProv.HealthUpstreams())
	}
	got = h.reload(inst.ID)
	op3 := h.operation(inst.ID, 3)
	if got.State != apps.StateUpgrading || got.DesiredGeneration != 3 || got.ObservedGeneration != 2 ||
		op3.Kind != agentapi.KindUpgrade || derefS(op3.ToVersion) != agenttest.Version3 || derefS(op3.FromVersion) != agenttest.Version2 {
		t.Fatalf("required stop not chained: %+v, next %+v", got.AppInstance, op3)
	}

	// The second step runs and, being the last, queues Setup instead of
	// another upgrade.
	h.applySucceeded(apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 3})
	if got := h.reload(inst.ID); got.State != apps.StateUpgrading || got.ObservedGeneration != 3 || derefS(got.PackageVersion) != agenttest.Version3 {
		t.Fatalf("after the last step: %+v", got.AppInstance)
	}
	if h.jobs(apps.SetupArgs{}.Kind()) == 0 {
		t.Fatal("the last upgrade step queued no setup")
	}
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 3}, 1, 8); err != nil {
		t.Fatal(err)
	}
	got = h.reload(inst.ID)
	if got.State != apps.StateActive || got.Health != apps.HealthHealthy ||
		derefS(got.PackageVersion) != agenttest.Version3 || got.DesiredGeneration != 3 || got.ObservedGeneration != 3 {
		t.Fatalf("after setup: %+v", got.AppInstance)
	}
	if h.agentProv.Setups(inst.ID) != 2 {
		t.Fatalf("setup ran %d times, want install and upgrade", h.agentProv.Setups(inst.ID))
	}
	if status, ok := upgradeStatus(t, h, inst.ID); !ok || status.State != apps.UpgradeUpToDate || status.CurrentVersion != agenttest.Version3 {
		t.Fatalf("final status: %+v, %v", status, ok)
	}
	if n := h.auditCount(audit.AppInstanceUpgradeStarted); n != 2 {
		t.Fatalf("%d upgrade starts audited, want 2", n)
	}
	if n := h.auditCount(audit.AppInstanceUpgraded); n != 2 {
		t.Fatalf("%d upgrades audited, want 2", n)
	}
}

// TestAgentUpgradeRollbackRevertsAndRetries: a failing candidate check
// aborts the upgrade; the agent rolls back, the core records the revert
// generation and runs Setup after it, then an admin retries the upgrade.
func TestAgentUpgradeRollbackRevertsAndRetries(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	h := newHarness(t, host.agentConfig)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t), "message": "rollback"})
	inst := runningOn(t, h, agenttest.Version)

	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.DesiredGeneration != 2 || got.State != apps.StateUpgrading {
		t.Fatalf("planner: %+v", got.AppInstance)
	}
	// Only the confirmation check fails; the provider itself is healthy.
	h.agentProv.Fail("health", errors.New("candidate is unhealthy"))

	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2})
	op2 := h.operation(inst.ID, 2)
	if op2.State != agentapi.OpRolledBack || derefS(op2.ErrorCode) != agentapi.CodeAborted {
		t.Fatalf("rolled-back operation: %+v", op2)
	}
	got := h.reload(inst.ID)
	revert := h.operation(inst.ID, 3)
	if got.State != apps.StateUpgrading || got.DesiredGeneration != 3 || revert.Kind != agentapi.KindApply ||
		desiredReason(t, h, inst.ID, 3) != apps.ReasonRevert || derefS(revert.ToVersion) != agenttest.Version {
		t.Fatalf("revert not recorded: %+v, %+v", got.AppInstance, revert)
	}
	if status, ok := upgradeStatus(t, h, inst.ID); !ok || status.State != apps.UpgradeInProgress {
		t.Fatalf("status during revert: %+v, %v", status, ok)
	}

	// The revert apply acknowledges the blocked generation and Setup brings
	// the instance back to active on the version that still runs. A core
	// restart between the rollback and this point must find the same state.
	restarted := h.restartCore()
	applyUntilDone(t, restarted, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 3})
	if err := restarted.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 3}, 1, 8); err != nil {
		t.Fatal(err)
	}
	got = h.reload(inst.ID)
	if got.State != apps.StateActive || derefS(got.PackageVersion) != agenttest.Version || got.ObservedGeneration != 3 {
		t.Fatalf("after revert: %+v", got.AppInstance)
	}
	if h.agentProv.Setups(inst.ID) != 2 {
		t.Fatalf("setup ran %d times, want install and rollback", h.agentProv.Setups(inst.ID))
	}
	status, ok := upgradeStatus(t, h, inst.ID)
	if !ok || status.State != apps.UpgradeRolledBack || status.TargetVersion != agenttest.Version2 || status.CurrentVersion != agenttest.Version {
		t.Fatalf("status after rollback: %+v, %v", status, ok)
	}
	if n := h.auditCount(audit.AppInstanceRolledBack); n != 1 {
		t.Fatalf("%d rollbacks audited, want 1", n)
	}

	// The planner never retries it by itself.
	if err := restarted.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.DesiredGeneration != 3 || got.State != apps.StateActive {
		t.Fatalf("the planner retried a rolled-back upgrade: %+v", got.AppInstance)
	}

	// Two admin retries race: the instance row lock lets exactly one record
	// the next step; the other sees the instance upgrading and is refused.
	type result struct {
		inst apps.Instance
		err  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			in, err := restarted.RetryUpgrade(h.ctx, audit.System, inst.ID)
			results <- result{inst: in, err: err}
		}()
	}
	var retried apps.Instance
	succeeded := 0
	for range 2 {
		r := <-results
		switch {
		case r.err == nil:
			succeeded++
			retried = r.inst
		case !errors.Is(r.err, apps.ErrInvalidState):
			t.Fatalf("losing retry: %v", r.err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent retries succeeded, want 1", succeeded)
	}
	if retried.State != apps.StateUpgrading || retried.DesiredGeneration != 4 {
		t.Fatalf("after retry: %+v", retried.AppInstance)
	}
	op4 := h.operation(inst.ID, 4)
	if op4.Kind != agentapi.KindUpgrade || desiredReason(t, h, inst.ID, 4) != apps.ReasonRetry || derefS(op4.ToVersion) != agenttest.Version2 {
		t.Fatalf("retry operation: %+v", op4)
	}
	h.applySucceeded(apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 4})
	if op := h.operation(inst.ID, 5); op.Kind != agentapi.KindUpgrade || derefS(op.ToVersion) != agenttest.Version3 {
		t.Fatalf("the retry did not continue to the required stop: %+v", op)
	}
	h.applySucceeded(apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 5})
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 5}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive || derefS(got.PackageVersion) != agenttest.Version3 {
		t.Fatalf("after the retried upgrade: %+v", got.AppInstance)
	}
}

// TestAgentUpgradePlanningAfterOrglaneRelease simulates an Orglane upgrade:
// the instance was installed by a release that only embedded 1.0.0-1, then
// a new core with 2.0.0-1 and 3.0.0-1 starts and plans the required stop
// before the newest version.
func TestAgentUpgradePlanningAfterOrglaneRelease(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	first := registryWithVersions(t, agenttest.Version)
	h := newHarness(t, host.agentConfig, func(c *apps.Config) { c.Packages = first })
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t), "message": "release"})
	inst := runningOn(t, h, agenttest.Version)

	// The same database and agent, a new core release embedding the newer
	// package versions.
	next := h.restartCore(func(c *apps.Config) {
		c.Packages = registryWithVersions(t, agenttest.Version, agenttest.Version2, agenttest.Version3)
	})
	if err := next.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	got := h.reload(inst.ID)
	op2 := h.operation(inst.ID, 2)
	if got.State != apps.StateUpgrading || got.DesiredGeneration != 2 ||
		op2.Kind != agentapi.KindUpgrade || derefS(op2.ToVersion) != agenttest.Version2 {
		t.Fatalf("the new release planned %+v, %+v", got.AppInstance, op2)
	}
	applyUntilDone(t, next, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2})
	if op := h.operation(inst.ID, 2); op.State != agentapi.OpSucceeded {
		t.Fatalf("required stop: %+v", op)
	}
	op3 := h.operation(inst.ID, 3)
	if op3.Kind != agentapi.KindUpgrade || derefS(op3.ToVersion) != agenttest.Version3 {
		t.Fatalf("the newest version was not planned after the stop: %+v", op3)
	}
	applyUntilDone(t, next, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 3})
	if err := next.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 3}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive || derefS(got.PackageVersion) != agenttest.Version3 {
		t.Fatalf("after the simulated release upgrade: %+v", got.AppInstance)
	}
}
