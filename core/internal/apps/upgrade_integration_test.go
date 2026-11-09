//go:build integration

package apps_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

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

// runningOn installs agenttest 1.0.0-1, applies it and runs Setup, leaving
// an active instance at the start of the upgrade path.
func runningOn(t *testing.T, h *harness) apps.Instance {
	t.Helper()
	inst, err := h.svc.InstallVersionForTest(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest"}, agenttest.Version)
	if err != nil {
		t.Fatal(err)
	}
	h.applySucceeded(apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 1})
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 1}, 1, 8); err != nil {
		t.Fatal(err)
	}
	got := h.reload(inst.ID)
	if got.State != apps.StateActive || derefS(got.PackageVersion) != agenttest.Version {
		t.Fatalf("install %s: %+v", agenttest.Version, got.AppInstance)
	}
	return got
}

// staticAgent is a reachable agent that reports no instances and never
// applies anything. Tests of the planner's database guard do not need a
// running agent.
type staticAgent struct{ boot uuid.UUID }

func (a *staticAgent) Health(context.Context) (*agentapi.Health, error) {
	return &agentapi.Health{Protocol: agentapi.ProtocolVersion, Version: "dev", BootID: a.boot}, nil
}

func (a *staticAgent) Instances(context.Context) ([]agentapi.InstanceStatus, error) {
	return nil, nil
}

func (a *staticAgent) Apply(context.Context, string, *agentapi.Spec) (*agentapi.Operation, bool, error) {
	return nil, false, errors.New("static agent: apply is not used")
}

func (a *staticAgent) Operation(context.Context, uuid.UUID) (*agentapi.Operation, error) {
	return nil, errors.New("static agent: operation is not used")
}

func (a *staticAgent) Confirm(context.Context, uuid.UUID) (*agentapi.Operation, error) {
	return nil, errors.New("static agent: confirm is not used")
}

func (a *staticAgent) Abort(context.Context, uuid.UUID) (*agentapi.Operation, error) {
	return nil, errors.New("static agent: abort is not used")
}

// plannedInstance creates an active managed instance on a chosen key whose
// running generation is already applied, without touching Docker.
func plannedInstance(t *testing.T, h *harness, key string) {
	t.Helper()
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: "agenttest", Key: key, BaseURL: "https://" + key + ".work.acme.test",
		State: apps.StateActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE app_instances
		SET package_version = $2, desired_generation = 1, observed_generation = 1, health = 'healthy'
		WHERE id = $1`, inst.ID, agenttest.Version)
}

// upgradeStatus is the API's upgrade status of an instance.
func upgradeStatus(t *testing.T, h *harness, id uuid.UUID) (apps.UpgradeStatus, bool) {
	t.Helper()
	return upgradeStatusFor(h.ctx, t, h.svc, id)
}

// upgradeStatusFor is upgradeStatus on a service restarted with another
// package registry.
func upgradeStatusFor(ctx context.Context, t *testing.T, svc *apps.Service, id uuid.UUID) (apps.UpgradeStatus, bool) {
	t.Helper()
	inst, err := svc.Instance(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	status, ok, err := svc.UpgradeStatus(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	return status, ok
}

// hasFencedUpstream reports whether a CheckHealth call used a fenced alias.
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
	inst := runningOn(t, h)

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
	// The final Setup still holds the instance (and its maintenance route):
	// the status must not claim up_to_date yet.
	if status, ok := upgradeStatus(t, h, inst.ID); !ok || status.State != apps.UpgradeInProgress ||
		status.CurrentVersion != agenttest.Version3 {
		t.Fatalf("status during the final Setup: %+v, %v", status, ok)
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
	inst := runningOn(t, h)

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

// TestLegacySetupJobCannotActivateAnUpgrade: a setup job queued before
// upgrades carried no generation; if it resumes while the instance is
// upgrading, it must not run Setup or activate it. The same legacy job still
// finishes an install that is the applied generation.
func TestLegacySetupJobCannotActivateAnUpgrade(t *testing.T) {
	h := newHarness(t)
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: "agenttest", BaseURL: "https://agenttest.work.acme.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE app_instances SET state = 'upgrading', package_version = $2, desired_generation = 2, observed_generation = 2 WHERE id = $1`,
		inst.ID, agenttest.Version)
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateUpgrading || h.agentProv.Setups(inst.ID) != 0 {
		t.Fatalf("a legacy setup job affected an upgrade: %+v, setups %d", got.AppInstance, h.agentProv.Setups(inst.ID))
	}

	// A legacy job for an install that is still the applied generation
	// finishes normally.
	h.exec(`UPDATE app_instances SET state = 'installing', desired_generation = 3, observed_generation = 3 WHERE id = $1`, inst.ID)
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive || h.agentProv.Setups(inst.ID) != 1 {
		t.Fatalf("a legacy install setup did not finish: %+v, setups %d", got.AppInstance, h.agentProv.Setups(inst.ID))
	}
}

// TestSetupWaitsForAppliedGeneration: an admin's setup retry while a resync
// is still pending must not activate the instance against the old service;
// the setup queued after the resync applies finishes it.
func TestSetupWaitsForAppliedGeneration(t *testing.T) {
	h := newHarness(t)
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: "agenttest", BaseURL: "https://agenttest.work.acme.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE app_instances SET state = 'failed', health = 'unhealthy', health_code = 'setup_failed',
		package_version = $2, desired_generation = 2, observed_generation = 1 WHERE id = $1`, inst.ID, agenttest.Version)
	retried, err := h.svc.RetrySetup(h.ctx, audit.System, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != apps.StateInstalling || retried.DesiredGeneration != 2 {
		t.Fatalf("after the setup retry: %+v", retried.AppInstance)
	}
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 2}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateInstalling || h.agentProv.Setups(inst.ID) != 0 {
		t.Fatalf("setup ran before the generation was applied: %+v, setups %d", got.AppInstance, h.agentProv.Setups(inst.ID))
	}

	// The resync applied: the setup job that its operation queues finishes.
	h.exec(`UPDATE app_instances SET observed_generation = 2 WHERE id = $1`, inst.ID)
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 2}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive || h.agentProv.Setups(inst.ID) != 1 {
		t.Fatalf("setup after the apply: %+v, setups %d", got.AppInstance, h.agentProv.Setups(inst.ID))
	}
}

// TestAgentUpgradeRollbackWithoutEmbeddedRunningVersion: the normal
// Orglane release layout embeds the new package version and required stops,
// not the version that still runs. A rollback must still record a revert the
// agent can apply, keep the instance routed and let an admin retry.
func TestAgentUpgradeRollbackWithoutEmbeddedRunningVersion(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	first := registryWithVersions(t, agenttest.Version)
	h := newHarness(t, host.agentConfig, func(c *apps.Config) { c.Packages = first })
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t), "message": "rollback without old version"})
	inst := runningOn(t, h)

	// A new Orglane release embeds only 2.0.0-1 and 3.0.0-1; neither the
	// core nor the agent embeds the running 1.0.0-1 any more.
	release := registryWithVersions(t, agenttest.Version2, agenttest.Version3)
	host.restart(release)
	next := h.restartCore(func(c *apps.Config) { c.Packages = release })

	if err := next.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.DesiredGeneration != 2 || got.State != apps.StateUpgrading {
		t.Fatalf("planner: %+v", got.AppInstance)
	}
	h.agentProv.Fail("health", errors.New("candidate is unhealthy"))
	applyUntilDone(t, next, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2})
	if op := h.operation(inst.ID, 2); op.State != agentapi.OpRolledBack {
		t.Fatalf("upgrade: %+v", op)
	}
	if reason := desiredReason(t, h, inst.ID, 3); reason != apps.ReasonRevert {
		t.Fatalf("revert generation reason = %q", reason)
	}

	// The revert is a minimal acknowledgment: the agent resolves the saved
	// project of the version that still runs. It must apply without the
	// package being embedded.
	applyUntilDone(t, next, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 3})
	if op := h.operation(inst.ID, 3); op.State != agentapi.OpSucceeded {
		t.Fatalf("revert: %+v", op)
	}
	if err := next.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 3}, 1, 8); err != nil {
		t.Fatal(err)
	}
	got := h.reload(inst.ID)
	if got.State != apps.StateActive || derefS(got.PackageVersion) != agenttest.Version || got.ObservedGeneration != 3 {
		t.Fatalf("after the revert: %+v", got.AppInstance)
	}
	routes, err := next.IngressRoutes(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var routed bool
	for _, r := range routes {
		if r.Key == inst.Key {
			routed = true
			if r.Maintenance || r.Upstream == "" {
				t.Fatalf("the recovered instance is not routed: %+v", r)
			}
		}
	}
	if !routed {
		t.Fatalf("the recovered instance has no route: %+v", routes)
	}
	if status, ok := upgradeStatusFor(h.ctx, t, next, inst.ID); !ok || status.State != apps.UpgradeRolledBack || status.TargetVersion != agenttest.Version2 {
		t.Fatalf("status after the revert: %+v, %v", status, ok)
	}

	// An admin retry still works from the recovered instance.
	retried, err := next.RetryUpgrade(h.ctx, audit.System, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != apps.StateUpgrading || retried.DesiredGeneration != 4 {
		t.Fatalf("after the retry: %+v", retried.AppInstance)
	}
	applyUntilDone(t, next, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 4})
	if op := h.operation(inst.ID, 4); op.State != agentapi.OpSucceeded {
		t.Fatalf("retried upgrade: %+v", op)
	}
	if op := h.operation(inst.ID, 5); op.Kind != agentapi.KindUpgrade || derefS(op.ToVersion) != agenttest.Version3 {
		t.Fatalf("the retry did not continue to the required stop: %+v", op)
	}
	applyUntilDone(t, next, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 5})
	if err := next.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID, Generation: 5}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive || derefS(got.PackageVersion) != agenttest.Version3 {
		t.Fatalf("after the retried upgrade: %+v", got.AppInstance)
	}
}

// TestAgentUpgradeRetryRecoversFailedRevert: an instance that the old core
// left failed/package_unavailable after a rollback is recoverable through
// the admin retry: the running version is known and the latest upgrade is
// still the rolled-back one.
func TestAgentUpgradeRetryRecoversFailedRevert(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	first := registryWithVersions(t, agenttest.Version)
	h := newHarness(t, host.agentConfig, func(c *apps.Config) { c.Packages = first })
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t), "message": "failed revert retry"})
	inst := runningOn(t, h)

	release := registryWithVersions(t, agenttest.Version2, agenttest.Version3)
	host.restart(release)
	next := h.restartCore(func(c *apps.Config) { c.Packages = release })
	if err := next.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.agentProv.Fail("health", errors.New("candidate is unhealthy"))
	applyUntilDone(t, next, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2})
	if op := h.operation(inst.ID, 2); op.State != agentapi.OpRolledBack {
		t.Fatalf("upgrade: %+v", op)
	}

	// The old behavior: the revert generation could not be assembled and
	// the instance ended failed/package_unavailable with no routes.
	h.exec(`UPDATE app_operations SET state = 'failed', error_code = $2, finished_at = now()
		WHERE instance_id = $1 AND generation = 3`, inst.ID, apps.CodePackageUnavailable)
	h.exec(`UPDATE app_instances SET state = 'failed', health = 'unhealthy', health_code = $2
		WHERE id = $1`, inst.ID, apps.CodePackageUnavailable)
	if got := h.reload(inst.ID); got.State != apps.StateFailed {
		t.Fatalf("simulated failed revert: %+v", got.AppInstance)
	}

	retried, err := next.RetryUpgrade(h.ctx, audit.System, inst.ID)
	if err != nil {
		t.Fatalf("retry a failed revert: %v", err)
	}
	if retried.State != apps.StateUpgrading || retried.DesiredGeneration != 4 ||
		desiredReason(t, h, inst.ID, 4) != apps.ReasonRetry {
		t.Fatalf("after the retry: %+v", retried.AppInstance)
	}
	applyUntilDone(t, next, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 4})
	if op := h.operation(inst.ID, 4); op.State != agentapi.OpSucceeded {
		t.Fatalf("retried upgrade: %+v", op)
	}
	if op := h.operation(inst.ID, 5); op.Kind != agentapi.KindUpgrade || derefS(op.ToVersion) != agenttest.Version3 {
		t.Fatalf("the retry did not continue: %+v", op)
	}
}

// TestAgentUpgradePlannerOneInstanceAtATime: the planner records at most one
// upgrade, even when two syncs race and two instances are eligible.
func TestAgentUpgradePlannerOneInstanceAtATime(t *testing.T) {
	h := newHarness(t, func(c *apps.Config) {
		c.Agent = &staticAgent{boot: uuid.Must(uuid.NewV7())}
		c.Packages = registryWithVersions(t, agenttest.Version, agenttest.Version2, agenttest.Version3)
	})
	plannedInstance(t, h, "one")
	plannedInstance(t, h, "two")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- h.svc.RunAgentSync(h.ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := h.count(`SELECT count(*) FROM app_instances WHERE state = 'upgrading'`); n != 1 {
		t.Fatalf("%d instances upgrading after one sync, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM app_operations WHERE kind = 'upgrade'`); n != 1 {
		t.Fatalf("%d upgrade operations recorded, want 1", n)
	}

	// A later sync does not start the second instance while the first is
	// still upgrading.
	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM app_instances WHERE state = 'upgrading'`); n != 1 {
		t.Fatalf("%d instances upgrading after the next sync, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM app_operations WHERE kind = 'upgrade'`); n != 1 {
		t.Fatalf("%d upgrade operations recorded after the next sync, want 1", n)
	}
}

// TestAgentUpgradePlanningWaitsForAgent: a core start while the agent is
// down must not put instances into upgrading (their routes would go into
// maintenance although their containers are fine). The next reachable sync
// plans.
func TestAgentUpgradePlanningWaitsForAgent(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	first := registryWithVersions(t, agenttest.Version)
	h := newHarness(t, host.agentConfig, func(c *apps.Config) { c.Packages = first })
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t), "message": "agent down"})
	inst := runningOn(t, h)

	release := registryWithVersions(t, agenttest.Version, agenttest.Version2, agenttest.Version3)
	host.stop()
	next := h.restartCore(func(c *apps.Config) { c.Packages = release })
	if err := next.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive || got.DesiredGeneration != 1 {
		t.Fatalf("planned while the agent was down: %+v", got.AppInstance)
	}
	routes, err := next.IngressRoutes(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Key == inst.Key && r.Maintenance {
			t.Fatalf("the instance's route went into maintenance while the agent was down: %+v", r)
		}
	}

	host.start()
	if err := next.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateUpgrading || got.DesiredGeneration != 2 {
		t.Fatalf("the reachable agent did not trigger the plan: %+v", got.AppInstance)
	}
}

// TestAgentSyncRecoversConfirmationAfterCrash: a core crash between mirroring
// awaiting_confirmation and confirming leaves the River job running (its
// rescue is an hour by default). The next sync must drive the confirmation
// itself, so the agent commits instead of rolling back on confirm_timeout.
func TestAgentSyncRecoversConfirmationAfterCrash(t *testing.T) {
	host := newAgentHost(t)
	host.start()
	h := newHarness(t, host.agentConfig)
	h.agentProv.SetPackageSettings(map[string]any{"http_port": freePort(t), "message": "crash recovery"})
	inst := runningOn(t, h)

	if err := h.svc.RunAgentSync(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.reload(inst.ID); got.State != apps.StateUpgrading || got.DesiredGeneration != 2 {
		t.Fatalf("planner: %+v", got.AppInstance)
	}
	// Push the planned generation directly, as the crashed job did, and
	// wait for the agent's candidate.
	spec, err := h.svc.SpecForTest(h.ctx, inst.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	aop, _, err := host.client().Apply(h.ctx, inst.Key, spec)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the candidate awaits confirmation", 2*time.Minute, func() bool {
		op, err := host.client().Operation(h.ctx, aop.ID)
		return err == nil && op.State == agentapi.OpAwaitingConfirmation
	})

	// The core mirrored the agent operation, then crashed before deciding;
	// River still reports the agent_apply job as running.
	h.exec(`UPDATE app_operations SET agent_operation_id = $2, state = 'awaiting_confirmation',
		phase = 'awaiting_confirmation' WHERE instance_id = $1 AND generation = 2`, inst.ID, aop.ID)
	h.exec(`UPDATE river_job SET state = 'running', attempted_at = now() - interval '10 minutes'
		WHERE kind = 'orglane.apps.agent_apply' AND args->>'instance_id' = $1
		AND (args->>'generation')::bigint = 2`, inst.ID.String())

	// A confirmation that raced the agent's await registration is treated
	// as already decided; the next sync retries it. Without the sync
	// recovery none of these syncs would drive the confirmation at all.
	for range 3 {
		if err := h.svc.RunAgentSync(h.ctx); err != nil {
			t.Fatal(err)
		}
		if op, err := host.client().Operation(h.ctx, aop.ID); err == nil && op.State == agentapi.OpSucceeded {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	// The sync confirmed through the fenced alias; the agent commits on its
	// own from there.
	waitUntil(t, "the agent commits the candidate", 30*time.Second, func() bool {
		op, err := host.client().Operation(h.ctx, aop.ID)
		return err == nil && op.State == agentapi.OpSucceeded
	})
	applyUntilDone(t, h.svc, apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2})
	if op := h.operation(inst.ID, 2); op.State != agentapi.OpSucceeded {
		t.Fatalf("operation after recovery: %+v", op)
	}
	if !hasFencedUpstream(h.agentProv.HealthUpstreams()) {
		t.Fatalf("the recovered confirmation did not use the fenced alias: %v", h.agentProv.HealthUpstreams())
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
	inst := runningOn(t, h)

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
