package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	fenced, err := fenceCompose(original)
	if err != nil {
		t.Fatal(err)
	}
	aliases, err := composeAliases(fenced)
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range aliases {
		if alias == "git-fenced" || alias == "git-fenced-fenced" {
			t.Fatalf("original alias survived: %s", alias)
		}
	}
	normalized := []byte(`{"services":{"fenced":{"networks":{"edge":{"aliases":["git-fenced"]}}}}}`)
	if err := checkFenced(normalized, "git-fenced"); err == nil {
		t.Fatal("normalized validation accepts original routed alias")
	}
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
				if _, _, err := a.snapshotRenderedProject("git", id, false); err != nil {
					t.Fatalf("valid project: %v", err)
				}
				if err := os.WriteFile(filepath.Join(a.snapshotPath("git", id), snapshotRendered, name), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for _, fenced := range []bool{false, true} {
				if _, _, err := a.snapshotRenderedProject("git", id, fenced); err == nil || !strings.Contains(err.Error(), "checksum") {
					t.Fatalf("corrupt project accepted (fenced %v): %v", fenced, err)
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
