//go:build integration

package apps_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

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
	if _, err := h.svc.CheckInstall(ctx, apps.InstallPlan{Host: "git.example.com", Package: pkg}); apps.Code(err) != apps.CodeHostNotLocal {
		t.Fatalf("a non-local host = %v", err)
	}
	// Without an agent: refused.
	if _, err := h.svc.CheckInstall(ctx, plan); apps.Code(err) != apps.CodeAgentUnavailable {
		t.Fatalf("without an agent = %v", err)
	}
	bare, _ := url.Parse("http://localhost:8080")
	other := h.restartCore(func(c *apps.Config) { c.WorkspaceURL = bare })
	if _, err := other.CheckInstall(ctx, plan); apps.Code(err) != apps.CodeLocalhostWorkspace {
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
		"http://orglane.localhost:8080":             apps.ErrInvalid,
		"http://bad_host.localhost":                 apps.ErrInvalid,
		"http://agenttest.orglane.localhost:9999":   apps.ErrInvalid, // not the workspace's port
		"http://agenttest.orglane.localhost":        apps.ErrInvalid, // nor without it
		"https://agenttest.orglane.localhost:8080":  apps.ErrInvalid, // nor its scheme
		"http://u:p@second.orglane.localhost:8080":  apps.ErrInvalid,
		"http://second.orglane.localhost:8080/path": apps.ErrInvalid,
		"http://second.orglane.localhost:8080/?q=1": apps.ErrInvalid,
		"http://agenttest.orglane.localhost:8080":   nil, // taken: checked below
	} {
		_, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest", Key: "second", BaseURL: host})
		if want != nil && !errors.Is(err, want) {
			t.Errorf("host %s = %v, want %v", host, err, want)
		}
		if want == nil && apps.Code(err) != apps.CodeHostTaken {
			t.Errorf("host %s = %v, want host_taken", host, err)
		}
	}
	// An invalid host is invalid input before any preflight (DNS, agent).
	refusing := h.restartCore(func(c *apps.Config) {
		c.InstallPreflight = func(context.Context, apps.InstallPlan) (apps.InstallBudget, error) {
			return apps.InstallBudget{}, errors.New("the preflight ran")
		}
	})
	for _, host := range []string{"http://bad_host.example", "http://orglane.localhost:8080", "http://third.orglane.localhost:65536"} {
		if _, err := refusing.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest", Key: "third", BaseURL: host}); !errors.Is(err, apps.ErrInvalid) {
			t.Errorf("host %s with a preflight = %v, want invalid input", host, err)
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

// budgetAgent reports a fixed host: total memory, plenty of disk.
type budgetAgent struct{ total uint64 }

func (a budgetAgent) Health(context.Context) (*agentapi.Health, error) {
	return &agentapi.Health{Version: "dev", MemTotal: a.total, DiskFree: 1 << 40}, nil
}
func (budgetAgent) Instances(context.Context) ([]agentapi.InstanceStatus, error) { return nil, nil }
func (budgetAgent) Apply(context.Context, string, *agentapi.Spec) (*agentapi.Operation, bool, error) {
	return nil, false, errors.New("not used")
}
func (budgetAgent) Operation(context.Context, uuid.UUID) (*agentapi.Operation, error) {
	return nil, errors.New("not used")
}

// TestConcurrentInstallsShareTheMemoryBudget: two installs whose
// preflights both passed against the same remaining memory (enough for
// one) are serialized by the install lock, which counts the first: only one
// is recorded, the other is refused with insufficient_memory. (The agent's
// health is the only fake here: the host's memory cannot be set otherwise.)
func TestConcurrentInstallsShareTheMemoryBudget(t *testing.T) {
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	pkg, _ := registry.Get(agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version})
	total := uint64(apps.BundleMemory) + uint64(pkg.Manifest.Resources.Memory.Min) //nolint:gosec // a small constant
	var svc *apps.Service
	var ready sync.WaitGroup
	ready.Add(2)
	release := make(chan struct{})
	local, _ := url.Parse("http://orglane.localhost")
	h := newHarness(t, func(c *apps.Config) {
		c.WorkspaceURL, c.Packages, c.Agent, c.CoreVersion = local, registry, budgetAgent{total}, "dev"
		c.InstallPreflight = func(ctx context.Context, p apps.InstallPlan) (apps.InstallBudget, error) {
			b, err := svc.CheckInstall(ctx, p)
			ready.Done()
			<-release // both preflights passed before either install records anything
			return b, err
		}
	})
	svc = h.svc
	results := make(chan error, 2)
	for _, key := range []string{"first", "second"} {
		go func() {
			_, err := svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest", Key: key, BaseURL: "http://" + key + ".orglane.localhost"})
			results <- err
		}()
	}
	ready.Wait()
	close(release)
	var accepted, refusedMemory int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			accepted++
		case apps.Code(err) == apps.CodeInsufficientMemory:
			refusedMemory++
		default:
			t.Errorf("install: %v", err)
		}
	}
	st, err := svc.AgentStatus(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || refusedMemory != 1 || st.MemoryReserved > total {
		t.Fatalf("accepted %d, refused for memory %d, reserved %d of %d", accepted, refusedMemory, st.MemoryReserved, total)
	}
	if n := h.count(`SELECT count(*) FROM app_instances`); n != 1 {
		t.Fatalf("%d instances recorded", n)
	}
}

// TestConcurrentInstallsReserveHostAndKey: two concurrent installs of the
// same host (distinct keys), or the same key (distinct hosts): one wins.
func TestConcurrentInstallsReserveHostAndKey(t *testing.T) {
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, sameKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "host", true: "key"}[sameKey], func(t *testing.T) {
			h := newHarness(t, func(c *apps.Config) { c.Packages = registry })
			start := make(chan struct{})
			results := make(chan error, 2)
			for _, name := range []string{"first", "second"} {
				go func() {
					key, host := name, "shared.work.acme.test"
					if sameKey {
						key, host = "shared", name+".work.acme.test"
					}
					<-start
					_, err := h.svc.Install(h.ctx, audit.System, apps.NewInstance{Provider: "agenttest", Key: key, BaseURL: "https://" + host})
					results <- err
				}()
			}
			close(start)
			accepted := 0
			for range 2 {
				if err := <-results; err == nil {
					accepted++
				} else if !errors.Is(err, apps.ErrKeyReserved) && apps.Code(err) != apps.CodeHostTaken {
					t.Errorf("install: %v", err)
				}
			}
			if accepted != 1 {
				t.Fatalf("accepted %d, want 1", accepted)
			}
		})
	}
}

// queuedAgent accepts every spec with an operation that stays queued, as
// the merged agent does for a purge retry that races with the purge's
// completion (#83): the replacement operation is never started.
type queuedAgent struct {
	mu      sync.Mutex
	started time.Time
	ops     map[uuid.UUID]*agentapi.Operation
}

func (a *queuedAgent) Health(context.Context) (*agentapi.Health, error) {
	return &agentapi.Health{Version: "dev", MemTotal: 1 << 40, DiskFree: 1 << 40}, nil
}
func (a *queuedAgent) Instances(context.Context) ([]agentapi.InstanceStatus, error) { return nil, nil }
func (a *queuedAgent) Apply(_ context.Context, key string, spec *agentapi.Spec) (*agentapi.Operation, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	op := &agentapi.Operation{ID: uuid.Must(uuid.NewV7()), Instance: key, Kind: agentapi.KindRemove, Generation: spec.Generation, State: agentapi.OpQueued, StartedAt: a.started}
	a.ops[op.ID] = op
	return op, true, nil
}
func (a *queuedAgent) Operation(_ context.Context, id uuid.UUID) (*agentapi.Operation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	op := *a.ops[id]
	return &op, nil
}

// TestStalledOperationEnds: an operation the agent keeps queued is polled
// until Config.AgentQueueTimeout, then ends failed with agent_stalled; a
// purge stalled like this leaves the instance failed (remove_failed), from
// where it can be removed again, instead of removing forever. (The agent's
// API is the only fake: the merged agent's #83 race cannot be timed from a
// test, and the agent side is fixed in #83.)
func TestStalledOperationEnds(t *testing.T) {
	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	agent := &queuedAgent{started: time.Now(), ops: map[uuid.UUID]*agentapi.Operation{}}
	h := newHarness(t, func(c *apps.Config) {
		c.Packages, c.Agent, c.CoreVersion, c.AgentQueueTimeout = registry, agent, "dev", time.Hour
	})
	ctx := h.ctx
	inst, err := h.svc.Install(ctx, audit.System, apps.NewInstance{Provider: "agenttest"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.FinishForTest(ctx, inst.ID, 1, agentapi.OpSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Transition(ctx, audit.System, inst.ID, apps.StateActive, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Remove(ctx, audit.System, inst.ID, true, inst.Key); err != nil {
		t.Fatal(err)
	}
	purge := apps.AgentApplyArgs{InstanceID: inst.ID, Generation: 2}
	// Queued for a moment: polled (snoozed), nothing changes.
	var snooze *river.JobSnoozeError
	if err := h.svc.RunAgentApply(ctx, purge); !errors.As(err, &snooze) {
		t.Fatalf("a freshly queued operation = %v", err)
	}
	if op := h.operation(inst.ID, 2); op.FinishedAt != nil || op.AgentOperationID == nil {
		t.Fatalf("operation %+v", op)
	}
	// Queued for longer than the timeout: it ends, and so does the removal.
	agent.mu.Lock()
	for _, op := range agent.ops {
		op.StartedAt = time.Now().Add(-2 * time.Hour)
	}
	agent.mu.Unlock()
	if err := h.svc.RunAgentApply(ctx, purge); err != nil {
		t.Fatalf("a stalled operation = %v", err)
	}
	op := h.operation(inst.ID, 2)
	if op.State != agentapi.OpFailed || derefS(op.ErrorCode) != apps.CodeAgentStalled || op.FinishedAt == nil {
		t.Fatalf("stalled operation %+v (code %s)", op, derefS(op.ErrorCode))
	}
	got := h.reload(inst.ID)
	if got.State != apps.StateFailed || derefS(got.HealthCode) != apps.CodeRemoveFailed {
		t.Fatalf("after a stalled purge: %s %s", got.State, derefS(got.HealthCode))
	}
	if h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1`, inst.ID) == 0 {
		t.Fatal("a purge that did not finish deleted the credentials")
	}
	// The purge can be requested again.
	if _, err := h.svc.Remove(ctx, audit.System, inst.ID, true, inst.Key); err != nil {
		t.Fatalf("purging again: %v", err)
	}
}
