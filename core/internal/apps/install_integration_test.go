//go:build integration

package apps_test

import (
	"errors"
	"net/url"
	"testing"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// TestManagedRemoval drives remove, purge and reinstall through their
// operations without an agent (the operations are finished as agent_apply
// does): states, data retention, bindings, credentials, audit entries,
// the key's reservation, and generation fencing (a stale result of an
// older generation never changes the instance).
func TestManagedRemoval(t *testing.T) {
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *apps.Config) { c.Packages = registry })
	ctx := h.ctx
	inst, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	id := inst.ID
	finish := func(gen int64, state, code string) {
		t.Helper()
		if err := h.svc.FinishForTest(ctx, id, gen, state, code); err != nil {
			t.Fatalf("finish generation %d: %v", gen, err)
		}
	}
	expect := func(state string, retained bool, desired int64) apps.Instance {
		t.Helper()
		got := h.reload(id)
		if got.State != state || got.DataRetained != retained || got.DesiredGeneration != desired {
			t.Fatalf("instance: state %s retained %v desired %d, want %s %v %d (code %s)", got.State, got.DataRetained, got.DesiredGeneration, state, retained, desired, derefS(got.HealthCode))
		}
		return got
	}
	finish(1, agentapi.OpSucceeded, "")
	if _, err := h.svc.Transition(ctx, audit.System, id, apps.StateActive, ""); err != nil {
		t.Fatal(err)
	}
	if b, _ := h.svc.InstanceBindings(ctx, id); len(b) != 1 {
		t.Fatalf("bindings of the active instance = %+v", b)
	}

	// Only managed instances in a removable state; purge needs the key.
	if _, err := h.svc.Remove(ctx, audit.System, id, true, "wrong"); !errors.Is(err, apps.ErrInvalid) {
		t.Fatalf("purge without the key = %v", err)
	}
	conn := h.instance(nil)
	if _, err := h.svc.Remove(ctx, audit.System, conn.ID, false, ""); !errors.Is(err, apps.ErrNotSupported) {
		t.Fatalf("removing a connected instance = %v", err)
	}
	if _, err := h.svc.Reinstall(ctx, audit.System, id); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("reinstalling an active instance = %v", err)
	}

	// Remove: removing, bindings gone, generation 2 is absent.
	if _, err := h.svc.Remove(ctx, audit.System, id, false, ""); err != nil {
		t.Fatal(err)
	}
	expect(apps.StateRemoving, false, 2)
	if b, _ := h.svc.InstanceBindings(ctx, id); len(b) != 0 {
		t.Fatalf("bindings of a removing instance = %+v", b)
	}
	if op := h.operation(id, 2); op.Kind != agentapi.KindRemove {
		t.Fatalf("removal operation %+v", op)
	}
	if _, err := h.svc.Remove(ctx, audit.System, id, false, ""); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("removing twice = %v", err)
	}
	// A newer absent generation (a resync after a generation conflict)
	// makes generation 2's result stale: it changes nothing.
	gen3, err := h.svc.ResyncForTest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	finish(2, agentapi.OpSucceeded, "")
	expect(apps.StateRemoving, false, gen3)
	// The current generation fails: failed with remove_failed.
	finish(gen3, agentapi.OpFailed, agentapi.CodeComposeFailed)
	if got := expect(apps.StateFailed, false, gen3); derefS(got.HealthCode) != apps.CodeRemoveFailed {
		t.Fatalf("failed removal code %s", derefS(got.HealthCode))
	}
	// Removed again from failed: removed, data retained, key reserved.
	if _, err := h.svc.Remove(ctx, audit.System, id, false, ""); err != nil {
		t.Fatal(err)
	}
	finish(gen3+1, agentapi.OpSucceeded, "")
	if got := expect(apps.StateRemoved, true, gen3+1); got.RemovedAt == nil {
		t.Fatal("removed_at is not set")
	}
	if _, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest", BaseURL: "https://other.work.acme.test"}); !errors.Is(err, apps.ErrKeyReserved) {
		t.Fatalf("installing the retained key = %v", err)
	}
	if _, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest", Key: "other"}); apps.Code(err) != apps.CodeHostTaken {
		t.Fatalf("installing the retained host = %v", err)
	}

	// Purge of the retained instance fails; removed again it is retained
	// (the purge did not finish); a reinstall brings it back.
	gen := gen3 + 2
	if _, err := h.svc.Remove(ctx, audit.System, id, true, inst.Key); err != nil {
		t.Fatal(err)
	}
	expect(apps.StateRemoving, false, gen)
	finish(gen, agentapi.OpFailed, agentapi.CodeDatabaseFailed)
	expect(apps.StateFailed, false, gen)
	if _, err := h.svc.Remove(ctx, audit.System, id, false, ""); err != nil {
		t.Fatal(err)
	}
	finish(gen+1, agentapi.OpSucceeded, "")
	expect(apps.StateRemoved, true, gen+1)
	if _, err := h.svc.Reinstall(ctx, audit.System, id); err != nil {
		t.Fatal(err)
	}
	got := expect(apps.StateInstalling, false, gen+2)
	if got.RemovedAt != nil {
		t.Fatal("removed_at is still set")
	}
	if op := h.operation(id, gen+2); op.Kind != agentapi.KindApply || *op.ToVersion != agenttest.Version {
		t.Fatalf("reinstall operation %+v", op)
	}
	h.exec(`DELETE FROM river_job WHERE kind = $1`, apps.SetupArgs{}.Kind())
	finish(gen+2, agentapi.OpSucceeded, "")
	if h.jobs(apps.SetupArgs{}.Kind()) != 1 {
		t.Fatal("the reinstalled instance runs no Setup")
	}
	if _, err := h.svc.Transition(ctx, audit.System, id, apps.StateActive, ""); err != nil {
		t.Fatal(err)
	}

	// Purge: removed, nothing retained, credentials deleted, key and host
	// free again.
	if _, err := h.svc.Remove(ctx, audit.System, id, true, inst.Key); err != nil {
		t.Fatal(err)
	}
	finish(gen+3, agentapi.OpSucceeded, "")
	expect(apps.StateRemoved, false, gen+3)
	if n := h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1`, id); n != 0 {
		t.Fatalf("%d credentials after the purge", n)
	}
	again, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatalf("installing the purged key again: %v", err)
	}
	if again.ID == id || again.Key != inst.Key {
		t.Fatalf("new instance %+v", again.AppInstance)
	}
	for action, want := range map[audit.Action]int{audit.AppInstanceRemoved: 5, audit.AppInstanceReinstalled: 1, audit.AppInstancePurged: 1} {
		if n := h.auditCount(action); n != want {
			t.Errorf("%s: %d entries, want %d", action, n, want)
		}
	}
}

// TestInstallChecks: the production checks of an install (CheckInstall)
// and the host rules every install applies.
func TestInstallChecks(t *testing.T) {
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := url.Parse("http://orglane.localhost:8080")
	h := newHarness(t, func(c *apps.Config) {
		c.Packages, c.WorkspaceURL, c.DomainBase = registry, local, "orglane.localhost"
		c.ServiceHost = func(label string) string { return label + ".orglane.localhost" }
	})
	ctx := h.ctx
	pkg, _ := registry.Get(agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version})
	plan := apps.InstallPlan{Provider: "agenttest", Key: "x", Host: "agenttest.orglane.localhost", Package: pkg}
	// Local mode: service hosts under .localhost, a workspace under it.
	if err := h.svc.CheckInstall(ctx, apps.InstallPlan{Host: "git.example.com", Package: pkg}); apps.Code(err) != apps.CodeHostNotLocal {
		t.Fatalf("a non-local host = %v", err)
	}
	// Without an agent: refused.
	if err := h.svc.CheckInstall(ctx, plan); apps.Code(err) != apps.CodeAgentUnavailable {
		t.Fatalf("without an agent = %v", err)
	}
	bare, _ := url.Parse("http://localhost:8080")
	other := h.restartCore(func(c *apps.Config) { c.WorkspaceURL = bare })
	if err := other.CheckInstall(ctx, plan); apps.Code(err) != apps.CodeLocalhostWorkspace {
		t.Fatalf("a bare localhost workspace = %v", err)
	}

	// Every install: the default host and URL, a valid host other than
	// the workspace's.
	inst, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	if *inst.BaseUrl != "http://agenttest.orglane.localhost:8080" {
		t.Fatalf("base URL %s", *inst.BaseUrl)
	}
	for host, want := range map[string]error{
		"http://orglane.localhost:8080":           apps.ErrInvalid,
		"http://bad_host.localhost":               apps.ErrInvalid,
		"http://agenttest.orglane.localhost:9999": nil, // taken, whatever the port
	} {
		_, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest", Key: "second", BaseURL: host})
		if want != nil && !errors.Is(err, want) {
			t.Errorf("host %s = %v, want %v", host, err, want)
		}
		if want == nil && apps.Code(err) != apps.CodeHostTaken {
			t.Errorf("host %s = %v, want host_taken", host, err)
		}
	}
	if n := h.count(`SELECT count(*) FROM app_instances WHERE key = 'second'`); n != 0 {
		t.Fatalf("refused installs left %d instances", n)
	}
	// The agent's status without an agent.
	st, err := h.svc.AgentStatus(ctx)
	if err != nil || st.Enabled || st.Connected || st.Code != apps.CodeAgentUnavailable {
		t.Fatalf("agent status %+v, %v", st, err)
	}
}
