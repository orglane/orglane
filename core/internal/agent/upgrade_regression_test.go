package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

func TestFinalizeRetriesOperationPersistence(t *testing.T) {
	for _, phase := range []string{agentapi.PhaseCommitting, agentapi.PhaseRestored} {
		t.Run(phase, func(t *testing.T) {
			now := time.Now()
			a, _ := newTestAgent(t, func() time.Time { return now })
			rec := put(t, a, now, "git", mustJSON(t, testSpec(1)))
			var op agentapi.Operation
			if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
				t.Fatal(err)
			}
			dir := a.instanceDir("git")
			j := &journal{Operation: op.ID, Kind: agentapi.KindUpgrade, To: testSpec(1).Package, ToGeneration: 1, Spec: testSpec(1), Phase: phase}
			if err := writeJournal(dir, j); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(a.ops.path(op.ID)); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(a.ops.path(op.ID), "obstacle"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := a.finalize(a.instance("git"), j, testSpec(1), false, agentapi.OpSucceeded, "", ""); err == nil {
				t.Fatal("finalization ignored operation persistence failure")
			}
			if got, err := readJournal(dir); err != nil || got == nil || got.Phase != phase {
				t.Fatalf("journal lost: %+v %v", got, err)
			}
			if err := os.RemoveAll(a.ops.path(op.ID)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ops.update(op.ID, func(*agentapi.Operation) {}); err != nil {
				t.Fatal(err)
			}
			// Restart: a retained journal prevents an incorrect interrupted result.
			b, err := New(Options{Config: a.cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := b.ops.get(op.ID); got.Terminal() {
				t.Fatalf("restart lost pending finalization: %+v", got)
			}
			if err := b.finalize(b.instance("git"), j, testSpec(1), false, agentapi.OpSucceeded, "", ""); err != nil {
				t.Fatal(err)
			}
			if got, _ := b.ops.get(op.ID); got.State != agentapi.OpSucceeded {
				t.Fatalf("retry result: %+v", got)
			}
			if got, err := readJournal(dir); err != nil || got != nil {
				t.Fatalf("journal after retry: %+v %v", got, err)
			}
		})
	}
}

func TestRestoreCannotReplaceClaimedUpgrade(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	if rec := put(t, a, now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	dir := a.instanceDir("git")
	if err := writeSpec(filepath.Join(dir, currentFile), testSpec(1)); err != nil {
		t.Fatal(err)
	}
	snapID := writeTestSnapshot(t, a, agentapi.SnapshotReasonManual, true)
	next := testSpec(2)
	next.Package.Version = agenttest.Version2
	next.Settings["fault"] = "none"
	if rec := put(t, a, now, "git", mustJSON(t, next)); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	a.crashAt = agentapi.PhasePreflight
	in := a.instance("git")
	a.ops.mu.Lock()
	done := make(chan struct{})
	go func() { defer close(done); a.reconcile(t.Context(), a.instance("git")) }()
	// The worker must claim before it can block on the operation store.
	deadline := time.Now().Add(2 * time.Second)
	claimed := false
	for time.Now().Before(deadline) {
		a.mu.Lock()
		claimed = in.busy
		a.mu.Unlock()
		if claimed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !claimed {
		a.ops.mu.Unlock()
		<-done
		t.Fatal("reconcile did not claim work before operation-store I/O")
	}
	code, p, _ := post(t, a, now, agentapi.InstanceRestorePath("git"), mustJSON(t, agentapi.RestoreRequest{SnapshotID: snapID, Generation: 3}))
	a.ops.mu.Unlock()
	<-done
	if code != http.StatusConflict || p.Code != agentapi.CodeAgentBusy {
		t.Fatalf("restore raced a claimed upgrade: %d %+v", code, p)
	}
	st, err := loadState(dir)
	if err != nil || st.desired.Generation != 2 {
		t.Fatalf("restore changed accepted state: %+v %v", st, err)
	}
	j, err := readJournal(dir)
	if err != nil || j == nil || j.Kind != agentapi.KindUpgrade || j.ToGeneration != 2 {
		t.Fatalf("upgrade journal: %+v %v", j, err)
	}
}

func TestFenceOriginalAliasesEndingInSuffix(t *testing.T) {
	original := []byte("services:\n  fenced:\n    networks:\n      edge:\n        aliases: [git-fenced, git-fenced-fenced]\n")
	routed := []string{"git-fenced", "git-fenced-fenced"}
	fenced, err := fenceCompose(original, routed)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range yamlAliases(t, fenced, "fenced") {
		if slices.Contains(routed, alias) {
			t.Fatalf("original alias survived: %s", alias)
		}
	}
	normalized := []byte(`{"services":{"fenced":{"networks":{"edge":{"aliases":["git-fenced"]}}}}}`)
	if err := checkFenced(normalized, "p-git", routed); err == nil {
		t.Fatal("normalized validation accepts original routed alias")
	}
}

// yamlAliases returns a service's edge aliases from a compose file.
func yamlAliases(t *testing.T, data []byte, service string) []string {
	t.Helper()
	var p struct {
		Services map[string]struct {
			Networks map[string]struct {
				Aliases []string `yaml:"aliases"`
			} `yaml:"networks"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	return p.Services[service].Networks["edge"].Aliases
}

func TestSnapshotRenderedIntegrity(t *testing.T) {
	for _, name := range []string{composeFile, envFile, configuredFile, packages.FilesDir + "/nested/config", "missing-hash"} {
		t.Run(name, func(t *testing.T) {
			a, _ := newTestAgent(t, time.Now)
			dir := a.instanceDir("git")
			if err := os.MkdirAll(filepath.Join(dir, packages.FilesDir, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{composeFile, envFile, configuredFile, packages.FilesDir + "/nested/config"} {
				if err := os.WriteFile(filepath.Join(dir, f), []byte("original"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			id := writeTestSnapshot(t, a, agentapi.SnapshotReasonManual, true)
			hash, err := copyRendered(dir, filepath.Join(a.snapshotPath("git", id), snapshotRendered))
			if err != nil {
				t.Fatal(err)
			}
			snap, _, err := a.loadSnapshot("git", id)
			if err != nil {
				t.Fatal(err)
			}
			snap.RenderedSHA256 = hash
			if name == "missing-hash" {
				snap.RenderedSHA256 = ""
			}
			if err := writeJSON(filepath.Join(a.snapshotPath("git", id), snapshotMeta), snap); err != nil {
				t.Fatal(err)
			}
			if name != "missing-hash" {
				if _, _, err := a.snapshotRenderedProject("git", id); err != nil {
					t.Fatalf("valid project: %v", err)
				}
				if err := os.WriteFile(filepath.Join(a.snapshotPath("git", id), snapshotRendered, name), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := a.snapshotRenderedProject("git", id); err == nil || !strings.Contains(err.Error(), "checksum") {
				t.Fatalf("corrupt project accepted: %v", err)
			}
			// Both phases of a restore check it before anything else.
			for _, fenced := range []bool{false, true} {
				if err := a.writeSnapshotRendered(t.Context(), "git", id, fenced, a.compose("git", nil), nil); err == nil || !strings.Contains(err.Error(), "checksum") {
					t.Fatalf("corrupt project written (fenced %v): %v", fenced, err)
				}
			}
		})
	}
}

func TestRestoredProjectRejectsDifferentInputs(t *testing.T) {
	a, rr := newTestAgent(t, time.Now)
	spec := testSpec(1)
	if err := os.MkdirAll(a.instanceDir("git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(a.instanceDir("git"), restoredProjectFile), restoredProject{Spec: spec, Rendered: &packages.Rendered{}}); err != nil {
		t.Fatal(err)
	}
	changed := testSpec(1)
	changed.Settings["message"] = "new input"
	if err := a.repairRestored(t.Context(), "git", changed); err == nil {
		t.Fatal("saved project accepted changed desired inputs")
	} else if code, _ := errorCode(err); code != agentapi.CodeUnknownPackageVersion {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := rr.count(); n != 0 {
		t.Fatalf("changed inputs invoked Docker %d times", n)
	}
}

// A terminal result can be durable while journal deletion still needs retry.
// Startup and periodic retention must keep exactly that journal's operation.
func TestJournalOperationSurvivesRetention(t *testing.T) {
	for _, phase := range []string{agentapi.PhaseCommitting, agentapi.PhaseRestored} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart=%t", phase, restart), func(t *testing.T) {
				now := time.Now()
				a, _ := newTestAgent(t, func() time.Time { return now })
				r := put(t, a, now, "git", mustJSON(t, testSpec(1)))
				var op agentapi.Operation
				if err := json.Unmarshal(r.Body.Bytes(), &op); err != nil {
					t.Fatal(err)
				}
				dir := a.instanceDir("git")
				j := &journal{Operation: op.ID, Kind: agentapi.KindUpgrade, To: testSpec(1).Package, ToGeneration: 1, Spec: testSpec(1), Phase: phase}
				if err := writeJournal(dir, j); err != nil {
					t.Fatal(err)
				}
				result := agentapi.OpSucceeded
				if phase == agentapi.PhaseRestored {
					result = agentapi.OpRolledBack
				}
				if _, err := a.ops.update(op.ID, func(o *agentapi.Operation) { o.State = result }); err != nil {
					t.Fatal(err)
				}
				unrelated, err := a.ops.create("git", agentapi.KindSnapshot, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := a.ops.update(unrelated.ID, func(o *agentapi.Operation) { o.State = agentapi.OpSucceeded }); err != nil {
					t.Fatal(err)
				}
				now = now.Add(operationRetention + time.Hour)
				if restart {
					a, err = New(Options{Config: a.cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}})
					if err != nil {
						t.Fatal(err)
					}
				} else if err := a.ops.prune(); err != nil {
					t.Fatal(err)
				}
				if _, ok := a.ops.get(op.ID); !ok {
					t.Fatal("retention pruned the operation still referenced by a journal")
				}
				if _, ok := a.ops.get(unrelated.ID); ok {
					t.Fatal("retention kept an unrelated expired operation")
				}
				if err := a.finalize(a.instance("git"), j, testSpec(1), false, result, "", ""); err != nil {
					t.Fatal(err)
				}
				if retained, err := readJournal(dir); err != nil || retained != nil {
					t.Fatalf("journal could not settle: %+v %v", retained, err)
				}
				if err := a.ops.prune(); err != nil {
					t.Fatal(err)
				}
				if _, ok := a.ops.get(op.ID); ok {
					t.Fatal("retention kept the expired operation after journal removal")
				}
				if _, err := os.Stat(a.ops.path(op.ID)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expired operation file: %v", err)
				}
			})
		}
	}
}

func TestRetentionPropagatesJournalReadFailure(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	r := put(t, a, now, "git", mustJSON(t, testSpec(1)))
	var op agentapi.Operation
	if err := json.Unmarshal(r.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ops.update(op.ID, func(o *agentapi.Operation) { o.State = agentapi.OpSucceeded }); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.instanceDir("git"), journalFile), 0700); err != nil {
		t.Fatal(err)
	}
	now = now.Add(operationRetention + time.Hour)
	if err := a.ops.prune(); err == nil {
		t.Fatal("retention ignored an unreadable journal")
	}
	if _, ok := a.ops.get(op.ID); !ok {
		t.Fatal("retention deleted operation with unreadable journal")
	}
	if _, err := New(Options{Config: a.cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}}); err == nil {
		t.Fatal("startup ignored an unreadable journal")
	}
	if _, err := os.Stat(a.ops.path(op.ID)); err != nil {
		t.Fatalf("startup deleted operation despite journal read failure: %v", err)
	}
}

// A journal whose operation file was lost (removed by hand, or by a release
// whose retention ignored journals) must still settle: recovery recreates
// the operation and records the journal's result on it, instead of failing
// every finalize with "operation not found" and keeping the journal.
func TestRecoverJournalRecreatesLostOperation(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	put(t, a, now, "git", mustJSON(t, testSpec(1)))
	dir := a.instanceDir("git")
	lost := uuid.Must(uuid.NewV7())
	from := testSpec(1).Package
	j := &journal{
		Operation: lost, Kind: agentapi.KindUpgrade, From: &from, To: testSpec(2).Package,
		FromGeneration: 1, ToGeneration: 2, Spec: testSpec(2), Phase: agentapi.PhasePreflight,
	}
	if err := writeJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	st, err := loadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if code := a.recoverJournal(t.Context(), a.instance("git"), st, j); code != agentapi.CodeInterrupted {
		t.Fatalf("recovery = %q", code)
	}
	if retained, err := readJournal(dir); err != nil || retained != nil {
		t.Fatalf("journal stayed: %+v %v", retained, err)
	}
	op, ok := a.ops.get(lost)
	if !ok || op.State != agentapi.OpFailed || op.ErrorCode != agentapi.CodeInterrupted ||
		op.Kind != agentapi.KindUpgrade || op.Instance != "git" || op.Generation != 2 || op.FinishedAt == nil {
		t.Fatalf("recreated operation = %+v (%t)", op, ok)
	}
	// Persisted, so the core can poll it after a restart.
	b, err := New(Options{Config: a.cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := b.ops.get(lost); !ok || got.State != agentapi.OpFailed {
		t.Fatalf("after a restart = %+v (%t)", got, ok)
	}
	if st, err := loadState(dir); err != nil || !st.blocked.has(2) {
		t.Fatalf("generation 2 not blocked: %+v %v", st, err)
	}
}

// A durable failed rollback (rollback_failed in the journal) is the
// operation's result even when the process died, or the operation write
// failed, before the operation recorded it: on start and on the next
// reconcile it becomes failed/rollback_failed with the snapshot, never
// failed/interrupted. Upgrades and manual restores share the path.
func TestRecoverDurableRollbackFailure(t *testing.T) {
	for _, kind := range []string{agentapi.KindUpgrade, agentapi.KindRestore} {
		for _, via := range []string{"start", "reconcile"} {
			t.Run(kind+"/"+via, func(t *testing.T) {
				now := time.Now()
				a, _ := newTestAgent(t, func() time.Time { return now })
				if rec := put(t, a, now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
					t.Fatal(rec.Body)
				}
				op, err := a.ops.create("git", kind, 2)
				if err != nil {
					t.Fatal(err)
				}
				a.opPhase(op.ID, agentapi.PhaseRollingBack)
				j := &journal{
					Operation: op.ID, Kind: kind, To: testSpec(2).Package, ToGeneration: 2, Spec: testSpec(2),
					SnapshotID: uuid.Must(uuid.NewV7()), Phase: agentapi.PhaseRollingBack, RollbackFailed: true,
				}
				// The journal is durable, the operation not updated yet.
				if err := writeJournal(a.instanceDir("git"), j); err != nil {
					t.Fatal(err)
				}
				b := a
				if via == "start" {
					if b, err = New(Options{Config: a.cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}}); err != nil {
						t.Fatal(err)
					}
				} else {
					a.reconcile(t.Context(), a.instance("git"))
				}
				got, ok := b.ops.get(op.ID)
				if !ok || got.State != agentapi.OpFailed || got.ErrorCode != agentapi.CodeRollbackFailed ||
					got.Phase != "" || got.SnapshotID != j.SnapshotID.String() {
					t.Fatalf("durable rollback failure recovered as %+v, want failed/rollback_failed with the snapshot", got)
				}
				if kept, err := readJournal(b.instanceDir("git")); err != nil || kept == nil || !kept.RollbackFailed {
					t.Fatalf("failed-rollback journal: %+v %v", kept, err)
				}
			})
		}
	}
}

// A failed rollback blocks snapshot requests: the worker takes none while
// a journal exists, so a queued one would block the recovery restore until
// a restart. The restore that retries the rollback is still accepted.
func TestSnapshotRejectedAfterRollbackFailure(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	if rec := put(t, a, now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	if err := writeSpec(filepath.Join(a.instanceDir("git"), currentFile), testSpec(1)); err != nil {
		t.Fatal(err)
	}
	snapID := writeTestSnapshot(t, a, agentapi.SnapshotReasonUpgrade, true)
	op, err := a.ops.create("git", agentapi.KindUpgrade, 2)
	if err != nil {
		t.Fatal(err)
	}
	j := &journal{
		Operation: op.ID, Kind: agentapi.KindUpgrade, To: testSpec(2).Package, ToGeneration: 2, Spec: testSpec(2),
		SnapshotID: snapID, Phase: agentapi.PhaseRollingBack, RollbackFailed: true,
	}
	if err := writeJournal(a.instanceDir("git"), j); err != nil {
		t.Fatal(err)
	}
	a.reconcile(t.Context(), a.instance("git"))

	manual := mustJSON(t, agentapi.SnapshotRequest{Reason: agentapi.SnapshotReasonManual})
	if code, p, _ := post(t, a, now, agentapi.InstanceSnapshotsPath("git"), manual); code != http.StatusConflict || p.Code != agentapi.CodeAgentBusy {
		t.Fatalf("snapshot after a failed rollback: %d %+v", code, p)
	}
	if in := a.instance("git"); in.snapshotOp != uuid.Nil {
		t.Fatalf("a rejected snapshot was reserved: %s", in.snapshotOp)
	}
	a.reconcile(t.Context(), a.instance("git"))
	code, p, body := post(t, a, now, agentapi.InstanceRestorePath("git"), mustJSON(t, agentapi.RestoreRequest{SnapshotID: snapID, Generation: 3}))
	var restore agentapi.Operation
	if code != http.StatusAccepted || json.Unmarshal(body, &restore) != nil || restore.Kind != agentapi.KindRestore {
		t.Fatalf("recovery restore after a rejected snapshot: %d %+v", code, p)
	}
	if got, err := readJournal(a.instanceDir("git")); err != nil || got == nil || got.Operation != restore.ID || got.RollbackFailed {
		t.Fatalf("journal after the restore request = %+v, %v", got, err)
	}
}

// A failure before the switch blocks the generation with its code before
// the operation records it. A crash in between recovers the same code and
// rule from the journal, not "interrupted".
func TestRecoverDecidedFailureBeforeSwitch(t *testing.T) {
	for _, phase := range []string{agentapi.PhasePreflight, agentapi.PhasePulling, agentapi.PhaseStopping, agentapi.PhaseSnapshotting} {
		t.Run(phase, func(t *testing.T) {
			now := time.Now()
			a, _ := newTestAgent(t, func() time.Time { return now })
			if rec := put(t, a, now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
				t.Fatal(rec.Body)
			}
			op, err := a.ops.create("git", agentapi.KindUpgrade, 2)
			if err != nil {
				t.Fatal(err)
			}
			a.opPhase(op.ID, phase)
			from := testSpec(1).Package
			j := &journal{
				Operation: op.ID, Kind: agentapi.KindUpgrade, From: &from, To: testSpec(2).Package,
				FromGeneration: 1, ToGeneration: 2, Spec: testSpec(2), Phase: phase,
				ErrorCode: agentapi.CodePolicyViolation, Rule: packages.RuleAlias,
			}
			if err := writeJournal(a.instanceDir("git"), j); err != nil {
				t.Fatal(err)
			}
			// What finalize wrote before the crash: the blocked generation.
			blocked := blockedState{Generations: []blockedGeneration{{Generation: 2, ErrorCode: agentapi.CodePolicyViolation, At: now}}}
			if err := writeJSON(filepath.Join(a.instanceDir("git"), blockedFile), blocked); err != nil {
				t.Fatal(err)
			}
			b, err := New(Options{Config: a.cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := b.ops.get(op.ID); got.Terminal() {
				t.Fatalf("start decided the journal's operation: %+v", got)
			}
			st, err := loadState(b.instanceDir("git"))
			if err != nil {
				t.Fatal(err)
			}
			if code := b.recoverJournal(t.Context(), b.instance("git"), st, j); code != agentapi.CodePolicyViolation {
				t.Fatalf("recovery = %q", code)
			}
			got, _ := b.ops.get(op.ID)
			if got.State != agentapi.OpFailed || got.ErrorCode != agentapi.CodePolicyViolation || got.Rule != packages.RuleAlias {
				t.Fatalf("recovered result = %+v, want failed/policy_violation/alias_not_allowed", got)
			}
			if kept, err := readJournal(b.instanceDir("git")); err != nil || kept != nil {
				t.Fatalf("journal stayed: %+v %v", kept, err)
			}
		})
	}
}

// restoreRunner answers every Docker call: `compose config` with the
// normalized projects in order (the last one repeated), everything else
// successfully.
type restoreRunner struct {
	recordingRunner
	configs []string
}

func (r *restoreRunner) run(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	if slices.Contains(args, "config") {
		out := r.configs[0]
		if len(r.configs) > 1 {
			r.configs = r.configs[1:]
		}
		return []byte(out), nil, nil
	}
	return nil, nil, nil
}

// destructive returns the Docker calls other than `compose config`.
func (r *restoreRunner) destructive() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, c := range r.calls {
		if !slices.Contains(c, "config") {
			out = append(out, c)
		}
	}
	return out
}

// Everything a restore uses is checked before its first destructive step:
// a snapshot that fails a check leaves the running deployment and its data
// as they were (no compose down, nothing moved aside, no database step).
func TestRestoreChecksEverythingBeforeChanges(t *testing.T) {
	// `compose config` of the snapshot's project, then of its fenced form.
	original := `{"services":{"web":{"networks":{"edge":{"aliases":["git-web"]}}}}}`
	fenced := `{"services":{"web":{"networks":{"edge":{"aliases":["git-web-fenced"]}}}}}`
	for _, c := range []struct {
		name string
		// config is what `compose config` answers (the fenced project).
		config string
		spoil  func(t *testing.T, a *Agent, id uuid.UUID)
	}{
		{"rendered compose", fenced, corruptSnapshotFile(snapshotRendered + "/" + composeFile)},
		{"rendered env", fenced, corruptSnapshotFile(snapshotRendered + "/" + envFile)},
		{"rendered files", fenced, corruptSnapshotFile(snapshotRendered + "/" + packages.FilesDir + "/web.sh")},
		{"missing rendered digest", fenced, editSnapshot(func(s *agentapi.Snapshot) { s.RenderedSHA256 = "" })},
		{"archive", fenced, corruptSnapshotFile(snapshotData + "/www.tar.gz")},
		{"data directory name", fenced, editSnapshot(func(s *agentapi.Snapshot) { s.DataDirs[0].Name = "../www" })},
		{"database without password", fenced, func(t *testing.T, a *Agent, id uuid.UUID) {
			dump := []byte("dump")
			if err := os.WriteFile(filepath.Join(a.snapshotPath("git", id), snapshotDB), dump, 0o600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(dump)
			editSnapshot(func(s *agentapi.Snapshot) {
				s.Database = &agentapi.SnapshotFile{File: snapshotDB, Bytes: int64(len(dump)), SHA256: hex.EncodeToString(sum[:])}
			})(t, a, id)
			spec := testSpec(1)
			delete(spec.Secrets, packages.SecretDatabasePassword)
			if err := writeSpec(filepath.Join(a.snapshotPath("git", id), snapshotSpec), spec); err != nil {
				t.Fatal(err)
			}
		}},
		// Control: a valid snapshot is restored (the checks above are what
		// stopped those restores).
		{"valid", fenced, nil},
		// The fenced project would publish a port: the fence check fails.
		{"fence check", `{"services":{"web":{"ports":[{"target":8080,"published":"18080"}]}}}`, func(*testing.T, *Agent, uuid.UUID) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _ := newTestAgent(t, time.Now)
			if rec := put(t, a, time.Now(), "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
				t.Fatal(rec.Body)
			}
			dir := a.instanceDir("git")
			if err := os.MkdirAll(filepath.Join(dir, dataDir, "www"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, packages.FilesDir), 0o700); err != nil {
				t.Fatal(err)
			}
			value := filepath.Join(dir, dataDir, "www", "value")
			for path, data := range map[string]string{
				value: "snapshot-data", filepath.Join(dir, composeFile): "services: {web: {}}\n",
				filepath.Join(dir, envFile): "", filepath.Join(dir, packages.FilesDir, "web.sh"): "#!/bin/sh\n",
			} {
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			id := uuid.Must(uuid.NewV7())
			if err := a.takeSnapshot(t.Context(), "git", testSpec(1), backupPlan{dataDirs: []string{"www"}}, agentapi.SnapshotReasonManual, id); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(value, []byte("live-data-after-snapshot"), 0o600); err != nil {
				t.Fatal(err)
			}
			if c.spoil != nil {
				c.spoil(t, a, id)
			}
			deployed, err := os.ReadFile(filepath.Join(dir, composeFile)) //nolint:gosec // test path
			if err != nil {
				t.Fatal(err)
			}
			rr := &restoreRunner{configs: []string{original, c.config}}
			a.docker = rr
			j := &journal{Operation: uuid.Must(uuid.NewV7()), Kind: agentapi.KindRestore, To: testSpec(1).Package, ToGeneration: 2,
				Spec: testSpec(2), SnapshotID: id, Phase: agentapi.PhaseRollingBack, RoutedKnown: true}
			err = a.restoreFenced(t.Context(), a.instance("git"), j)
			if c.spoil == nil {
				data, rerr := os.ReadFile(value) //nolint:gosec // test path
				if err != nil || len(rr.destructive()) == 0 || rerr != nil || string(data) != "snapshot-data" {
					t.Fatalf("valid snapshot: %v, calls %v, data %q", err, rr.destructive(), data)
				}
				return
			}
			if err == nil {
				t.Fatal("the restore accepted a snapshot that fails a check")
			}
			t.Logf("refused: %v", err)
			if calls := rr.destructive(); len(calls) != 0 {
				t.Fatalf("Docker ran before the checks completed: %v", calls)
			}
			if data, err := os.ReadFile(value); err != nil || string(data) != "live-data-after-snapshot" { //nolint:gosec // test path
				t.Fatalf("live data changed: %q %v", data, err)
			}
			entries, err := os.ReadDir(filepath.Join(dir, dataDir))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "www" {
					t.Fatalf("data moved or staged: %s", e.Name())
				}
			}
			if now, err := os.ReadFile(filepath.Join(dir, composeFile)); err != nil || !bytes.Equal(now, deployed) { //nolint:gosec // test path
				t.Fatalf("deployment changed: %v", err)
			}
		})
	}
}

func corruptSnapshotFile(rel string) func(*testing.T, *Agent, uuid.UUID) {
	return func(t *testing.T, a *Agent, id uuid.UUID) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(a.snapshotPath("git", id), filepath.FromSlash(rel)), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func editSnapshot(edit func(*agentapi.Snapshot)) func(*testing.T, *Agent, uuid.UUID) {
	return func(t *testing.T, a *Agent, id uuid.UUID) {
		t.Helper()
		snap, _, err := a.loadSnapshot("git", id)
		if err != nil {
			t.Fatal(err)
		}
		edit(snap)
		if err := writeJSON(filepath.Join(a.snapshotPath("git", id), snapshotMeta), snap); err != nil {
			t.Fatal(err)
		}
	}
}

// A failed extraction leaves no staging directory: the retry of a failed
// rollback is a restore with another operation ID, which would not find
// it. Staging directories left anyway (a crash) are removed by the
// periodic cleanup once no journal exists, never while one does.
func TestRestoreStagingIsRemoved(t *testing.T) {
	a, _ := newTestAgent(t, time.Now)
	if rec := put(t, a, time.Now(), "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	dir := a.instanceDir("git")
	root := filepath.Join(dir, dataDir)
	if err := os.MkdirAll(filepath.Join(root, "www"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{composeFile, envFile} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := uuid.Must(uuid.NewV7())
	if err := a.takeSnapshot(t.Context(), "git", testSpec(1), backupPlan{dataDirs: []string{"www"}}, agentapi.SnapshotReasonManual, id); err != nil {
		t.Fatal(err)
	}
	// An archive that matches its checksum but does not extract.
	garbage := []byte("not a gzip archive")
	if err := os.WriteFile(filepath.Join(a.snapshotPath("git", id), snapshotData, "www.tar.gz"), garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(garbage)
	editSnapshot(func(s *agentapi.Snapshot) {
		s.DataDirs[0].Bytes, s.DataDirs[0].SHA256 = int64(len(garbage)), hex.EncodeToString(sum[:])
	})(t, a, id)
	snap, _, err := a.loadSnapshot("git", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.verifySnapshot("git", snap); err != nil {
		t.Fatal(err)
	}
	if err := a.restoreDataDirs("git", snap, uuid.Must(uuid.NewV7())); err == nil || !strings.Contains(err.Error(), "extract www") {
		t.Fatal("a broken archive was extracted")
	}
	staging := func() []string {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			if strings.Contains(e.Name(), ".restore-") {
				out = append(out, e.Name())
			}
		}
		return out
	}
	if left := staging(); len(left) != 0 {
		t.Fatalf("staging left after a failed extraction: %v", left)
	}

	// Left by a crash: kept while a journal exists, removed once none does.
	left := filepath.Join(root, ".www.restore-"+uuid.Must(uuid.NewV7()).String())
	if err := os.Mkdir(left, 0o700); err != nil {
		t.Fatal(err)
	}
	j := &journal{Operation: uuid.Must(uuid.NewV7()), Kind: agentapi.KindRestore, To: testSpec(1).Package, ToGeneration: 2,
		Spec: testSpec(2), SnapshotID: id, Phase: agentapi.PhaseRollingBack}
	if err := writeJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	a.cleanAllMovedAside()
	if got := staging(); len(got) != 1 {
		t.Fatalf("staging removed while a journal exists: %v", got)
	}
	if err := removeJournal(dir); err != nil {
		t.Fatal(err)
	}
	a.cleanAllMovedAside()
	if got := staging(); len(got) != 0 {
		t.Fatalf("stale staging kept: %v", got)
	}
	// The live data the failed restore moved aside stays (7 days).
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), ".www.failed-") }) {
		t.Fatalf("the moved-aside data was removed: %v", entries)
	}
}
