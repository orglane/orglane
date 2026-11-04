//go:build integration

package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// Upgrade tests (docs/specs/agent.md "Testing"): agenttest 1.0.0-1 ->
// 2.0.0-1 -> 3.0.0-1 with real Docker, faults, aborts, timeouts, crashes at
// every journal phase and the write fence.

func agenttestRegistry(t *testing.T) *packages.Registry {
	t.Helper()
	r, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// versionSpec is integrationSpec for a package version; versions after
// 1.0.0-1 take the fault setting.
func versionSpec(id uuid.UUID, gen int64, port int, version, fault string) *agentapi.Spec {
	s := integrationSpec(id, gen, port, "upgrade-test", agentapi.StateRunning)
	s.Package.Version = version
	if version != agenttest.Version {
		s.Settings["fault"] = fault
	}
	return s
}

// push sends a spec that must be accepted and returns its operation.
func (e *testEnv) push(key string, spec *agentapi.Spec) *agentapi.Operation {
	e.t.Helper()
	op, accepted, err := e.client.Apply(e.t.Context(), key, spec)
	if err != nil || !accepted {
		e.t.Fatalf("push %s generation %d: %v (accepted %v)", key, spec.Generation, err, accepted)
	}
	return op
}

// waitAwaiting polls an upgrade until it awaits its confirmation.
func (e *testEnv) waitAwaiting(id uuid.UUID) *agentapi.Operation {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		op, err := e.client.Operation(e.t.Context(), id)
		if err != nil {
			e.t.Fatalf("operation %s: %v", id, err)
		}
		if op.State == agentapi.OpAwaitingConfirmation {
			return op
		}
		if op.Terminal() || time.Now().After(deadline) {
			e.t.Fatalf("operation %s: want awaiting confirmation, got %+v", id, op)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (e *testEnv) waitTerminal(id uuid.UUID) *agentapi.Operation {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		op, err := e.client.Operation(e.t.Context(), id)
		if err == nil && op.Terminal() {
			return op
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("operation %s did not finish: %+v, %v", id, op, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// httpPath fetches a path from the published port.
func httpPath(port int, path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+path, http.NoBody)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return string(b), errors.New(resp.Status)
	}
	return string(b), err
}

// clientWrite is a write acknowledged to a client over the published port.
func clientWrite(t *testing.T, port int, text string) {
	t.Helper()
	if got, err := httpPath(port, "/cgi-bin/write?"+text); err != nil || got != "ok\n" {
		t.Fatalf("write %q: %q, %v", text, got, err)
	}
}

func writesLog(t *testing.T, e *testEnv, key string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.agent.instanceDir(key), dataDir, "www", "writes.log")) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// edgeVersion fetches http://<host>:8080/version from the edge network,
// where Traefik would (the bundle's stand-in container).
func (e *testEnv) edgeVersion(host string) (string, error) {
	id := docker(e.t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+e.project, "--filter", "label=com.docker.compose.service=traefik")
	out, err := exec.CommandContext(e.t.Context(), "docker", "exec", id, "wget", "-q", "-T", "3", "-O", "-", "http://"+host+":8080/version").CombinedOutput() //nolint:gosec // test commands
	return strings.TrimSpace(string(out)), err
}

// installV1 installs 1.0.0-1 and records acknowledged writes: one over the
// published port and one row in the database.
func (e *testEnv) installV1(key string, id uuid.UUID, port int) {
	e.t.Helper()
	e.mustApplyKey(key, versionSpec(id, 1, port, agenttest.Version, ""))
	clientWrite(e.t, port, "before-upgrade")
	db := packages.DatabaseName(key)
	e.psql(db, "SET ROLE "+db+"; CREATE TABLE client_writes (v text); INSERT INTO client_writes VALUES ('before-upgrade')")
}

// assertV1Intact checks that 1.0.0-1 serves and the acknowledged writes and
// nothing of the candidate are there.
func (e *testEnv) assertV1Intact(key string, port int, wantWrites ...string) {
	e.t.Helper()
	eventually(e.t, 2*time.Minute, "1.0.0-1 serves on the published port", func() bool {
		v, err := httpPath(port, "/version")
		return err == nil && v == agenttest.Version+"\n"
	})
	if got := writesLog(e.t, e, key); !slices.Equal(got, append([]string{"before-upgrade"}, wantWrites...)) {
		e.t.Fatalf("writes.log = %v", got)
	}
	if _, err := os.Stat(filepath.Join(e.agent.instanceDir(key), dataDir, "www", "written-by")); !errors.Is(err, os.ErrNotExist) {
		e.t.Fatalf("the candidate's own data survived the rollback: %v", err)
	}
	db := packages.DatabaseName(key)
	if got := e.psql(db, "select string_agg(v, ',') from client_writes"); got != "before-upgrade" {
		e.t.Fatalf("client_writes = %q", got)
	}
	if got := e.psql(db, "select count(*) from pg_tables where tablename = 'agenttest_migrations'"); got != "0" {
		e.t.Fatal("the candidate's migration survived the rollback")
	}
	if got := e.psql(db, "select tableowner from pg_tables where tablename = 'client_writes'"); got != db {
		e.t.Fatalf("restored table owner = %q, want %s", got, db)
	}
	st, err := e.client.Instance(e.t.Context(), key)
	if err != nil || st.Package == nil || st.Package.Version != agenttest.Version || st.ObservedGeneration != 1 {
		e.t.Fatalf("status = %+v, %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(e.agent.instanceDir(key), journalFile)); !errors.Is(err, os.ErrNotExist) {
		e.t.Fatalf("journal after the rollback: %v", err)
	}
}

// assertFenced checks the write fence of a running candidate: the published
// port refuses connections and the routes' alias does not resolve on the
// edge network, while the fenced alias serves the candidate.
func (e *testEnv) assertFenced(key string, port int, version string) {
	e.t.Helper()
	if _, err := httpPath(port, "/version"); err == nil {
		e.t.Fatal("the published port answers while the candidate is fenced")
	}
	if out, err := e.edgeVersion(packages.ServiceAlias(key, "web", false)); err == nil {
		e.t.Fatalf("the routed alias answers while fenced: %q", out)
	}
	if out, err := e.edgeVersion(packages.ServiceAlias(key, "web", true)); err != nil || out != version {
		e.t.Fatalf("the fenced alias: %q, %v", out, err)
	}
}

// cleanupProject removes an instance project when the test ends. Register
// it before the agent starts, so it runs after the agent stopped (a running
// agent would repair the removed containers).
func cleanupProject(t *testing.T, project, key string) {
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", packages.InstanceProject(project, key), "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
}

// upgradeSuite runs the upgrade tests as parallel subtests against one
// bundle, so they fit CI's time budget.
type upgradeSuite struct {
	project  string
	registry *packages.Registry
}

// run adds a parallel subtest that holds a Docker slot (dockerSlots).
func (s *upgradeSuite) run(t *testing.T, name string, fn func(t *testing.T)) {
	t.Run(name, func(t *testing.T) {
		t.Parallel()
		holdDockerSlot(t)
		fn(t)
	})
}

// TestUpgrades runs the upgrade tests (docs/specs/agent.md "Testing").
func TestUpgrades(t *testing.T) {
	t.Parallel()
	s := &upgradeSuite{project: startBundle(t), registry: agenttestRegistry(t)}
	s.run(t, "path", s.testPath)
	s.rollback(t)
	s.run(t, "stays-rolled-back", s.testStaysRolledBack)
	s.crashRecovery(t)
	s.run(t, "snapshot-restore", s.testSnapshotAndRestore)
}

// testPath: v1 -> v3 is refused (required stop), v1 -> v2 -> v3 upgrades
// with snapshots, fenced candidates and confirmations; the acknowledged
// writes survive.
func (s *upgradeSuite) testPath(t *testing.T) {
	project := s.project
	const key = "up1"
	cleanupProject(t, project, key) // registered first: runs after the agent stopped
	e := startAgent(t, project, s.registry)
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	e.installV1(key, id, port)

	// The required stop is enforced.
	_, _, err := e.client.Apply(t.Context(), key, versionSpec(id, 2, port, agenttest.Version3, "none"))
	var pe *agentclient.ProblemError
	if !errors.As(err, &pe) || pe.Code != agentapi.CodeUpgradePathBlocked || pe.RequiredVersion != agenttest.Version2 {
		t.Fatalf("1 -> 3: %v", err)
	}

	op := e.push(key, versionSpec(id, 2, port, agenttest.Version2, "none"))
	if op.Kind != agentapi.KindUpgrade {
		t.Fatalf("operation kind = %s", op.Kind)
	}
	op = e.waitAwaiting(op.ID)
	if op.SnapshotID == "" || op.Phase != agentapi.PhaseAwaitingConfirmation {
		t.Fatalf("awaiting operation = %+v", op)
	}
	if st, _ := e.client.Instance(t.Context(), key); st.Phase != agentapi.PhaseUpgrading {
		t.Fatalf("instance phase while awaiting = %s", st.Phase)
	}
	e.assertFenced(key, port, agenttest.Version2)
	// Snapshots and restores wait while an upgrade runs.
	if _, err := e.client.Snapshot(t.Context(), key); !errors.As(err, &pe) || pe.Code != agentapi.CodeAgentBusy {
		t.Fatalf("snapshot during an upgrade: %v", err)
	}
	if done, err := e.client.Confirm(t.Context(), op.ID); err != nil || done.ID != op.ID {
		t.Fatalf("confirm: %+v, %v", done, err)
	}
	op = e.waitTerminal(op.ID)
	if op.State != agentapi.OpSucceeded {
		t.Fatalf("upgrade = %+v", op)
	}
	// A repeated confirmation answers again; an abort is refused.
	if _, err := e.client.Confirm(t.Context(), op.ID); err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	if _, err := e.client.Abort(t.Context(), op.ID); !errors.As(err, &pe) || pe.Code != agentapi.CodeNotAwaitingConfirmation {
		t.Fatalf("abort after commit: %v", err)
	}
	if v, err := httpPath(port, "/version"); err != nil || v != agenttest.Version2+"\n" {
		t.Fatalf("version after the upgrade: %q, %v", v, err)
	}
	if out, err := e.edgeVersion(packages.ServiceAlias(key, "web", false)); err != nil || out != agenttest.Version2 {
		t.Fatalf("routed alias after commit: %q, %v", out, err)
	}
	clientWrite(t, port, "after-upgrade")
	if got := writesLog(t, e, key); !slices.Equal(got, []string{"before-upgrade", "after-upgrade"}) {
		t.Fatalf("writes.log = %v", got)
	}
	db := packages.DatabaseName(key)
	if got := e.psql(db, "select string_agg(version, ',') from agenttest_migrations"); got != agenttest.Version2 {
		t.Fatalf("migrations = %q", got)
	}
	if got := e.psql(db, "select v from client_writes"); got != "before-upgrade" {
		t.Fatalf("client_writes = %q", got)
	}
	st, err := e.client.Instance(t.Context(), key)
	if err != nil || st.Phase != agentapi.PhaseRunning || st.ObservedGeneration != 2 || st.Package.Version != agenttest.Version2 || st.Snapshots != 1 {
		t.Fatalf("status after the upgrade = %+v, %v", st, err)
	}
	snaps, err := e.client.Snapshots(t.Context(), key)
	if err != nil || len(snaps) != 1 || snaps[0].Reason != agentapi.SnapshotReasonUpgrade || snaps[0].Package.Version != agenttest.Version ||
		snaps[0].Database == nil || len(snaps[0].DataDirs) != 1 || snaps[0].ID.String() != op.SnapshotID {
		t.Fatalf("snapshots = %+v, %v", snaps, err)
	}

	// 2.0.0-1 -> 3.0.0-1.
	op = e.push(key, versionSpec(id, 3, port, agenttest.Version3, "none"))
	op = e.waitAwaiting(op.ID)
	e.assertFenced(key, port, agenttest.Version3)
	if _, err := e.client.Confirm(t.Context(), op.ID); err != nil {
		t.Fatal(err)
	}
	if op = e.waitTerminal(op.ID); op.State != agentapi.OpSucceeded {
		t.Fatalf("upgrade to 3 = %+v", op)
	}
	if v, err := httpPath(port, "/version"); err != nil || v != agenttest.Version3+"\n" {
		t.Fatalf("version 3: %q, %v", v, err)
	}
	if got := e.psql(db, "select string_agg(version, ',' order by version) from agenttest_migrations"); got != agenttest.Version2+","+agenttest.Version3 {
		t.Fatalf("migrations = %q", got)
	}
	if got := writesLog(t, e, key); !slices.Equal(got, []string{"before-upgrade", "after-upgrade"}) {
		t.Fatalf("writes.log after 3 = %v", got)
	}

	purge := versionSpec(id, 4, port, agenttest.Version3, "none")
	purge.State, purge.Purge = agentapi.StateAbsent, true
	e.mustApplyKey(key, purge)
	e.checkAudit(map[string]bool{key: true})
}

// rollback: a failing healthcheck, a failing migration, an abort and a
// missing confirmation each roll back with the acknowledged writes intact
// and the generation blocked; a failed rollback waits for a restore.
func (s *upgradeSuite) rollback(t *testing.T) {
	project, registry := s.project, s.registry
	for _, c := range []struct {
		name, fault, code string
		decide            func(e *testEnv, op *agentapi.Operation)
	}{
		{name: "health", fault: "health", code: agentapi.CodeHealthTimeout},
		{name: "migrate", fault: "migrate", code: agentapi.CodeHookFailed},
		{name: "abort", fault: "none", code: agentapi.CodeAborted, decide: func(e *testEnv, op *agentapi.Operation) {
			e.waitAwaiting(op.ID)
			if _, err := e.client.Abort(e.t.Context(), op.ID); err != nil {
				e.t.Fatal(err)
			}
		}},
		{name: "confirm-timeout", fault: "none", code: agentapi.CodeConfirmTimeout, decide: func(e *testEnv, op *agentapi.Operation) {
			e.waitAwaiting(op.ID)
		}},
	} {
		s.run(t, "rollback-"+c.name, func(t *testing.T) {
			key := "rb-" + c.name
			cleanupProject(t, project, key)
			e := startAgent(t, project, registry)
			id := uuid.Must(uuid.NewV7())
			port := freePort(t)
			e.installV1(key, id, port)
			op := e.push(key, versionSpec(id, 2, port, agenttest.Version2, c.fault))
			if c.decide != nil {
				c.decide(e, op)
			}
			op = e.waitTerminal(op.ID)
			if op.State != agentapi.OpRolledBack || op.ErrorCode != c.code || op.SnapshotID == "" {
				t.Fatalf("operation = %+v, want rolled_back %s", op, c.code)
			}
			e.assertV1Intact(key, port)
			st, _ := e.client.Instance(t.Context(), key)
			if !slices.Equal(st.BlockedGenerations, []int64{2}) || st.Phase != agentapi.PhaseRunning {
				t.Fatalf("status after the rollback = %+v", st)
			}
			// A rollback after the confirmation timeout is not undone by a
			// late confirmation.
			if _, err := e.client.Confirm(t.Context(), op.ID); err == nil {
				t.Fatal("a rolled-back upgrade was confirmed")
			}
			e.checkAudit(map[string]bool{key: true})
		})
	}

	s.run(t, "rollback-failed", func(t *testing.T) {
		const key = "rb-failed"
		cleanupProject(t, project, key)
		e := startAgent(t, project, registry)
		id := uuid.Must(uuid.NewV7())
		port := freePort(t)
		e.installV1(key, id, port)
		op := e.push(key, versionSpec(id, 2, port, agenttest.Version2, "none"))
		op = e.waitAwaiting(op.ID)
		// The snapshot's dump goes missing: the rollback cannot restore.
		snapDir := e.agent.snapshotPath(key, uuid.MustParse(op.SnapshotID))
		if err := os.Rename(filepath.Join(snapDir, snapshotDB), filepath.Join(e.dataDir, "db.dump.saved")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.client.Abort(t.Context(), op.ID); err != nil {
			t.Fatal(err)
		}
		if op = e.waitTerminal(op.ID); op.State != agentapi.OpFailed || op.ErrorCode != agentapi.CodeRollbackFailed {
			t.Fatalf("operation = %+v, want rollback_failed", op)
		}
		st, _ := e.client.Instance(t.Context(), key)
		if st.Phase != agentapi.PhaseFailed {
			t.Fatalf("instance after a failed rollback = %+v", st)
		}
		// Nothing retries by itself.
		e.agent.wake(key)
		time.Sleep(2 * time.Second)
		if st, _ := e.client.Instance(t.Context(), key); st.Phase != agentapi.PhaseFailed {
			t.Fatalf("instance after a reconcile = %+v", st)
		}
		// The snapshot is repaired; a restore retries the rollback.
		if err := os.Rename(filepath.Join(e.dataDir, "db.dump.saved"), filepath.Join(snapDir, snapshotDB)); err != nil {
			t.Fatal(err)
		}
		restore, err := e.client.Restore(t.Context(), key, uuid.MustParse(op.SnapshotID), 3)
		if err != nil {
			t.Fatal(err)
		}
		if restore = e.waitTerminal(restore.ID); restore.State != agentapi.OpSucceeded || restore.Kind != agentapi.KindRestore {
			t.Fatalf("restore = %+v", restore)
		}
		eventually(t, time.Minute, "1.0.0-1 serves", func() bool {
			v, err := httpPath(port, "/version")
			return err == nil && v == agenttest.Version+"\n"
		})
		if got := writesLog(t, e, key); !slices.Equal(got, []string{"before-upgrade"}) {
			t.Fatalf("writes.log = %v", got)
		}
		if got := e.psql(packages.DatabaseName(key), "select v from client_writes"); got != "before-upgrade" {
			t.Fatalf("client_writes = %q", got)
		}
		if st, _ := e.client.Instance(t.Context(), key); st.Phase != agentapi.PhaseRunning || st.ObservedGeneration != 3 || st.AcceptedGeneration != 3 {
			t.Fatalf("instance after the restore = %+v", st)
		}
		e.checkAudit(map[string]bool{key: true})
	})
}

// testStaysRolledBack: after a rollback, with no core, Docker
// events, periodic reconciles and an agent restart keep 1.0.0-1 running and
// never start 2.0.0-1; a revert spec is a plain apply, and a retry spec
// with a new generation runs the upgrade again.
func (s *upgradeSuite) testStaysRolledBack(t *testing.T) {
	project, registry := s.project, s.registry
	const key = "stay"
	cleanupProject(t, project, key)
	e := launchAgent(t, project, registry, agentOptions{})
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	e.installV1(key, id, port)
	op := e.push(key, versionSpec(id, 2, port, agenttest.Version2, "health"))
	if op = e.waitTerminal(op.ID); op.State != agentapi.OpRolledBack {
		t.Fatalf("upgrade = %+v", op)
	}
	stillV1 := func(what string) {
		t.Helper()
		eventually(t, 2*time.Minute, what+": 1.0.0-1 serves", func() bool {
			v, err := httpPath(port, "/version")
			return err == nil && v == agenttest.Version+"\n"
		})
		st, err := e.client.Instance(t.Context(), key)
		if err != nil || st.Package.Version != agenttest.Version || st.Operation == nil || st.Operation.ID != op.ID {
			t.Fatalf("%s: status = %+v, %v", what, st, err)
		}
	}

	// A killed container comes back through Docker events, as 1.0.0-1.
	web := docker(t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+packages.InstanceProject(project, key), "--filter", "label=com.docker.compose.service=web")
	docker(t, "rm", "--force", web)
	stillV1("after a removed container")
	// The periodic reconcile.
	e.agent.wake(key)
	time.Sleep(2 * time.Second)
	stillV1("after a reconcile")
	// An agent restart.
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
	e = launchAgent(t, project, registry, agentOptions{dataDir: e.dataDir})
	time.Sleep(3 * time.Second)
	stillV1("after an agent restart")

	// The revert spec (the running version) is a plain apply.
	revert := e.push(key, versionSpec(id, 3, port, agenttest.Version, ""))
	if revert.Kind != agentapi.KindApply {
		t.Fatalf("revert kind = %s", revert.Kind)
	}
	if revert = e.waitTerminal(revert.ID); revert.State != agentapi.OpSucceeded {
		t.Fatalf("revert = %+v", revert)
	}
	// An admin's retry: a new generation with the target version.
	retry := e.push(key, versionSpec(id, 4, port, agenttest.Version2, "none"))
	retry = e.waitAwaiting(retry.ID)
	if _, err := e.client.Confirm(t.Context(), retry.ID); err != nil {
		t.Fatal(err)
	}
	if retry = e.waitTerminal(retry.ID); retry.State != agentapi.OpSucceeded {
		t.Fatalf("retry = %+v", retry)
	}
	if v, err := httpPath(port, "/version"); err != nil || v != agenttest.Version2+"\n" {
		t.Fatalf("after the retry: %q, %v", v, err)
	}
	// The data moved aside by the rollback goes with the successful upgrade.
	entries, err := os.ReadDir(filepath.Join(e.agent.instanceDir(key), dataDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".") {
			t.Fatalf("moved-aside data after a successful upgrade: %s", en.Name())
		}
	}
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
}

// crashRecovery crashes the agent (test hook) at every journal
// phase and checks the recovery of docs/specs/agent.md "Interrupted
// upgrades". During verifying the host restart is simulated: every project
// container and the agent stop and start, and the candidate stays fenced.
func (s *upgradeSuite) crashRecovery(t *testing.T) {
	project, registry := s.project, s.registry
	for i, c := range []struct {
		at, fault, state, code string
		confirm                bool
		hostRestart            bool
		// writeAfterExpose writes to the restored version before the
		// agent restarts.
		writeAfterExpose bool
	}{
		{at: agentapi.PhasePreflight, fault: "none", state: agentapi.OpFailed, code: agentapi.CodeInterrupted},
		{at: agentapi.PhasePulling, fault: "none", state: agentapi.OpFailed, code: agentapi.CodeInterrupted},
		{at: agentapi.PhaseStopping, fault: "none", state: agentapi.OpFailed, code: agentapi.CodeInterrupted},
		{at: agentapi.PhaseSnapshotting, fault: "none", state: agentapi.OpFailed, code: agentapi.CodeInterrupted},
		{at: agentapi.PhaseSwitching, fault: "none", state: agentapi.OpRolledBack, code: agentapi.CodeInterrupted},
		{at: agentapi.PhaseMigrating, fault: "none", state: agentapi.OpRolledBack, code: agentapi.CodeInterrupted},
		{at: agentapi.PhaseVerifying, fault: "none", state: agentapi.OpRolledBack, code: agentapi.CodeInterrupted, hostRestart: true},
		{at: agentapi.PhaseAwaitingConfirmation, fault: "none", state: agentapi.OpRolledBack, code: agentapi.CodeInterrupted},
		{at: agentapi.PhaseCommitting, fault: "none", state: agentapi.OpSucceeded, confirm: true},
		{at: agentapi.PhaseRollingBack, fault: "health", state: agentapi.OpRolledBack, code: agentapi.CodeHealthTimeout},
		{at: agentapi.PhaseRestored, fault: "health", state: agentapi.OpRolledBack, code: agentapi.CodeHealthTimeout},
		{at: "restored:exposed", fault: "health", state: agentapi.OpRolledBack, code: agentapi.CodeHealthTimeout, writeAfterExpose: true},
	} {
		s.run(t, "crash-"+strings.ReplaceAll(c.at, ":", "-"), func(t *testing.T) {
			key := "cr" + strconv.Itoa(i)
			cleanupProject(t, project, key)
			e := launchAgent(t, project, registry, agentOptions{crashAt: c.at})
			id := uuid.Must(uuid.NewV7())
			port := freePort(t)
			e.installV1(key, id, port)
			op := e.push(key, versionSpec(id, 2, port, agenttest.Version2, c.fault))
			if c.confirm {
				op = e.waitAwaiting(op.ID)
				if _, err := e.client.Confirm(t.Context(), op.ID); err != nil {
					t.Fatal(err)
				}
			}
			eventually(t, 4*time.Minute, "the agent crashes at "+c.at, e.agent.crashed.Load)
			if err := e.stop(); !errors.Is(err, errCrashed) {
				t.Fatalf("agent stopped with %v, want the test crash", err)
			}
			if _, err := os.Stat(filepath.Join(e.agent.instanceDir(key), journalFile)); err != nil {
				t.Fatalf("no journal after the crash: %v", err)
			}
			if c.hostRestart {
				e.restartContainers(key)
				// The candidate came back fenced: no client reaches it.
				time.Sleep(2 * time.Second)
				if _, err := httpPath(port, "/version"); err == nil {
					t.Fatal("the candidate is reachable on the published port after a host restart")
				}
				if out, err := e.edgeVersion(packages.ServiceAlias(key, "web", false)); err == nil {
					t.Fatalf("the routed alias answers after a host restart: %q", out)
				}
			}
			var extra []string
			if c.writeAfterExpose {
				// The restored version is exposed: a client writes to it
				// before the agent comes back.
				clientWrite(t, port, "after-restore")
				extra = append(extra, "after-restore")
			}

			e = launchAgent(t, project, registry, agentOptions{dataDir: e.dataDir})
			op = e.waitTerminal(op.ID)
			if op.State != c.state || op.ErrorCode != c.code {
				t.Fatalf("operation after recovery = %+v, want %s %s", op, c.state, c.code)
			}
			if c.state == agentapi.OpSucceeded {
				eventually(t, time.Minute, "2.0.0-1 serves", func() bool {
					v, err := httpPath(port, "/version")
					return err == nil && v == agenttest.Version2+"\n"
				})
				if got := writesLog(t, e, key); !slices.Equal(got, []string{"before-upgrade"}) {
					t.Fatalf("writes.log = %v", got)
				}
			} else {
				e.assertV1Intact(key, port, extra...)
				if st, _ := e.client.Instance(t.Context(), key); !slices.Equal(st.BlockedGenerations, []int64{2}) {
					t.Fatalf("blocked generations = %v", st.BlockedGenerations)
				}
			}
			if entries, err := os.ReadDir(e.agent.snapshotsPath(key)); err == nil {
				for _, en := range entries {
					if _, err := os.Stat(filepath.Join(e.agent.snapshotsPath(key), en.Name(), snapshotComplete)); err != nil {
						t.Fatalf("partial snapshot %s left", en.Name())
					}
				}
			}
			if err := e.stop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// restartContainers stops and starts every container of an instance, as a
// host reboot does (Docker's restart policy brings them back).
func (e *testEnv) restartContainers(key string) {
	e.t.Helper()
	ids := e.containers(key)
	if len(ids) == 0 {
		e.t.Fatal("no containers to restart")
	}
	docker(e.t, append([]string{"stop"}, ids...)...)
	docker(e.t, append([]string{"start"}, ids...)...)
}

// testSnapshotAndRestore takes a manual snapshot and restores it: later
// writes are moved aside, not deleted, and the restore's generation becomes
// the running one.
func (s *upgradeSuite) testSnapshotAndRestore(t *testing.T) {
	project := s.project
	const key = "snap"
	cleanupProject(t, project, key)
	e := startAgent(t, project, s.registry)
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	e.installV1(key, id, port)

	op, err := e.client.Snapshot(t.Context(), key)
	if err != nil || op.Kind != agentapi.KindSnapshot {
		t.Fatalf("snapshot: %+v, %v", op, err)
	}
	if op = e.waitTerminal(op.ID); op.State != agentapi.OpSucceeded || op.SnapshotID == "" {
		t.Fatalf("snapshot operation = %+v", op)
	}
	snaps, err := e.client.Snapshots(t.Context(), key)
	if err != nil || len(snaps) != 1 || snaps[0].Reason != agentapi.SnapshotReasonManual || snaps[0].ID.String() != op.SnapshotID {
		t.Fatalf("snapshots = %+v, %v", snaps, err)
	}
	snapID := snaps[0].ID
	eventually(t, time.Minute, "the instance serves again after the snapshot", func() bool {
		_, err := httpPath(port, "/version")
		return err == nil
	})
	clientWrite(t, port, "after-snapshot")
	db := packages.DatabaseName(key)
	e.psql(db, "INSERT INTO client_writes VALUES ('after-snapshot')")

	var pe *agentclient.ProblemError
	if _, err := e.client.Restore(t.Context(), key, snapID, 1); !errors.As(err, &pe) || pe.Code != agentapi.CodeStaleGeneration {
		t.Fatalf("restore with a stale generation: %v", err)
	}
	if _, err := e.client.Restore(t.Context(), key, uuid.Must(uuid.NewV7()), 2); !errors.As(err, &pe) || pe.Code != agentapi.CodeNotFound {
		t.Fatalf("restore of an unknown snapshot: %v", err)
	}
	restore, err := e.client.Restore(t.Context(), key, snapID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if restore = e.waitTerminal(restore.ID); restore.State != agentapi.OpSucceeded || restore.Generation != 2 {
		t.Fatalf("restore = %+v", restore)
	}
	eventually(t, time.Minute, "the restored version serves", func() bool {
		_, err := httpPath(port, "/version")
		return err == nil
	})
	if got := writesLog(t, e, key); !slices.Equal(got, []string{"before-upgrade"}) {
		t.Fatalf("writes.log after the restore = %v", got)
	}
	if got := e.psql(db, "select string_agg(v, ',') from client_writes"); got != "before-upgrade" {
		t.Fatalf("client_writes after the restore = %q", got)
	}
	aside, err := os.ReadFile(filepath.Join(e.agent.instanceDir(key), dataDir, movedAside("www", restore.ID), "writes.log")) //nolint:gosec // test path
	if err != nil || !strings.Contains(string(aside), "after-snapshot") {
		t.Fatalf("moved-aside data: %q, %v", aside, err)
	}
	st, err := e.client.Instance(t.Context(), key)
	if err != nil || st.ObservedGeneration != 2 || st.AcceptedGeneration != 2 || st.Phase != agentapi.PhaseRunning || st.Snapshots != 1 {
		t.Fatalf("status after the restore = %+v, %v", st, err)
	}
	e.checkAudit(map[string]bool{key: true})
}
