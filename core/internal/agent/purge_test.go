package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// pruneOperation marks an operation as finished long ago and prunes it.
func pruneOperation(t *testing.T, a *Agent, id uuid.UUID, now time.Time) {
	t.Helper()
	old := now.Add(-8 * 24 * time.Hour)
	if _, err := a.ops.update(id, func(o *agentapi.Operation) { o.FinishedAt = &old }); err != nil {
		t.Fatal(err)
	}
	if err := a.ops.prune(); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.ops.get(id); ok {
		t.Fatal("operation was not pruned")
	}
}

// purgeAgent returns an agent whose Docker calls all succeed (the Postgres
// lookup finds a container), with an installed instance "git" at
// generation 1 and an accepted purge at generation 2.
func purgeAgent(t *testing.T, cfg purgeEnv, hook func(string)) (*Agent, *agentapi.Spec, agentapi.Operation) {
	t.Helper()
	a := cfg.agent(t, hook)
	if rec := put(t, a, cfg.now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatalf("install: %d %s", rec.Code, rec.Body)
	}
	// Pretend generation 1 runs.
	if err := writeSpec(filepath.Join(a.instanceDir("git"), currentFile), testSpec(1)); err != nil {
		t.Fatal(err)
	}
	purge := testSpec(2)
	purge.State, purge.Purge = agentapi.StateAbsent, true
	rec := put(t, a, cfg.now, "git", mustJSON(t, purge))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("purge: %d %s", rec.Code, rec.Body)
	}
	var op agentapi.Operation
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	return a, purge, op
}

type purgeEnv struct {
	dir string
	now time.Time
}

func newPurgeEnv(t *testing.T) purgeEnv { return purgeEnv{dir: t.TempDir(), now: time.Now()} }

func (c purgeEnv) agent(t *testing.T, hook func(string)) *Agent {
	t.Helper()
	cfg := testConfig(t)
	cfg.DataDir = c.dir
	a, err := New(Options{Config: cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return c.now },
		runner: fixedRunner("postgres-container"), hook: hook})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// signedPut is put without test assertions, for other goroutines.
func signedPut(t *testing.T, a *Agent, now time.Time, key string, body []byte) (*httptest.ResponseRecorder, []byte) {
	req, mac := signed(t, testKey(t), http.MethodPut, "/v1/instances/"+key, body, now)
	return serve(a.Handler(), req), mac
}

func decodeOp(t *testing.T, rec *httptest.ResponseRecorder) agentapi.Operation {
	t.Helper()
	var op agentapi.Operation
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		t.Fatalf("not an operation: %d %s", rec.Code, rec.Body)
	}
	return op
}

// assertPurged checks that the key is free: no directory, no trash, no
// status, and no operation of the key other than want, owned by owner.
func assertPurged(t *testing.T, a *Agent, want, owner uuid.UUID) {
	t.Helper()
	if _, err := os.Stat(a.instanceDir("git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory after the purge: %v", err)
	}
	if entries, err := os.ReadDir(a.trashDir()); err != nil || len(entries) != 0 {
		t.Fatalf("trash after the purge: %v, %v", entries, err)
	}
	if _, found, err := a.status(t.Context(), "git"); found || err != nil {
		t.Fatalf("status after the purge: found %v, %v", found, err)
	}
	if got := a.ops.ownerOf(want); got != owner {
		t.Fatalf("purge operation %s is owned by %s, want %s", want, got, owner)
	}
	if op, ok := a.ops.forGeneration("git", 2, owner); !ok || op.ID != want || op.State != agentapi.OpSucceeded {
		t.Fatalf("purge operation = %+v, want %s succeeded", op, want)
	}
	for _, op := range a.ops.unfinished() {
		t.Fatalf("unfinished operation after the purge: %+v", op)
	}
}

// retryPurge sends the purge spec again, as the core does when the answer
// to the first push was lost, and expects the purge's own operation.
func retryPurge(t *testing.T, a *Agent, now time.Time, purge *agentapi.Spec, want uuid.UUID) {
	t.Helper()
	rec := put(t, a, now, "git", mustJSON(t, purge))
	if op := decodeOp(t, rec); rec.Code != http.StatusOK || op.ID != want || op.State != agentapi.OpSucceeded {
		t.Fatalf("retried purge = %d %+v, want 200 with %s succeeded", rec.Code, op, want)
	}
}

// TestPurgeRetryDuringCompletion runs the real purge (accept, remove,
// finish) and sends the same purge again while it completes, when the
// directory is being deleted, and after it finished: every retry gets the
// one operation that purged the key, nothing recreates the directory, and
// no second operation is left queued (#83).
func TestPurgeRetryDuringCompletion(t *testing.T) {
	cfg := newPurgeEnv(t)
	type answer struct {
		rec *httptest.ResponseRecorder
		mac []byte
	}
	var a *Agent
	var purge *agentapi.Spec
	answers := make(chan answer, 2)
	hook := func(point string) {
		switch point {
		case "purge-recorded", "purged":
			// The retry arrives between the steps of the completion; it
			// waits for the result, so the hook does not wait for it.
			started := make(chan struct{})
			go func() {
				close(started)
				rec, mac := signedPut(t, a, cfg.now, "git", mustJSON(t, purge))
				answers <- answer{rec, mac}
			}()
			<-started
		}
	}
	a, purge, op := purgeAgent(t, cfg, hook)
	a.reconcile(t.Context(), a.instance("git"))
	for range 2 {
		got := <-answers
		if !agentapi.VerifyResponse(testKey(t), got.mac, got.rec.Code, got.rec.Header(), got.rec.Body.Bytes()) {
			t.Fatal("unsigned answer")
		}
		if o := decodeOp(t, got.rec); got.rec.Code != http.StatusOK || o.ID != op.ID || o.State != agentapi.OpSucceeded {
			t.Fatalf("retry during the purge = %d %+v, want 200 with %s succeeded", got.rec.Code, o, op.ID)
		}
	}
	assertPurged(t, a, op.ID, purge.InstanceID)

	// The lost answer is retried after the purge, also after a restart.
	retryPurge(t, a, cfg.now, purge, op.ID)
	b := cfg.agent(t, nil)
	retryPurge(t, b, cfg.now, purge, op.ID)
	assertPurged(t, b, op.ID, purge.InstanceID)

	// The purge record keeps the generation fence for the purged instance.
	if rec := put(t, b, cfg.now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusConflict || problemCode(t, rec).Code != agentapi.CodeStaleGeneration {
		t.Fatalf("older generation after the purge: %d %s", rec.Code, rec.Body)
	}
	changed := *purge
	changed.Purge = false
	if rec := put(t, b, cfg.now, "git", mustJSON(t, &changed)); rec.Code != http.StatusConflict || problemCode(t, rec).Code != agentapi.CodeGenerationConflict {
		t.Fatalf("purged generation with other content: %d %s", rec.Code, rec.Body)
	}

	// The key is free for another instance.
	other := testSpec(1)
	other.InstanceID = uuid.MustParse("0190f0f0-0000-7000-8000-000000000002")
	rec := put(t, b, cfg.now, "git", mustJSON(t, other))
	if o := decodeOp(t, rec); rec.Code != http.StatusAccepted || o.ID == op.ID || o.State != agentapi.OpQueued {
		t.Fatalf("new instance on the purged key: %d %+v", rec.Code, o)
	}
	// Now the directory decides: the old instance's purge is refused.
	if rec := put(t, b, cfg.now, "git", mustJSON(t, purge)); rec.Code != http.StatusConflict || problemCode(t, rec).Code != agentapi.CodeInstanceMismatch {
		t.Fatalf("old purge on the reused key: %d %s", rec.Code, rec.Body)
	}
}

// TestPurgeCrashRecovery stops the process (a crash) at each step of the
// purge completion and restarts the agent on the same data: the purge ends
// with one durable result, and a retry gets that operation.
func TestPurgeCrashRecovery(t *testing.T) {
	for _, point := range []string{"purge-recorded", "purged", "recorded"} {
		t.Run(point, func(t *testing.T) {
			cfg := newPurgeEnv(t)
			a, purge, op := purgeAgent(t, cfg, func(p string) {
				if p == point {
					runtime.Goexit()
				}
			})
			done := make(chan struct{})
			go func() {
				defer close(done)
				a.reconcile(t.Context(), a.instance("git"))
			}()
			<-done
			if got, _ := a.ops.get(op.ID); got.Terminal() {
				t.Fatalf("operation finished before the crash: %+v", got)
			}

			b := cfg.agent(t, nil)
			got, _ := b.ops.get(op.ID)
			if point == "purge-recorded" {
				// The directory is still there: the purge runs again.
				if got.State != agentapi.OpQueued {
					t.Fatalf("operation after the restart = %+v, want queued", got)
				}
				rec := put(t, b, cfg.now, "git", mustJSON(t, purge))
				if o := decodeOp(t, rec); rec.Code != http.StatusOK || o.ID != op.ID {
					t.Fatalf("retry before the rerun = %d %+v", rec.Code, o)
				}
				b.reconcile(t.Context(), b.instance("git"))
			} else if got.State != agentapi.OpSucceeded {
				t.Fatalf("operation after the restart = %+v, want succeeded", got)
			}
			assertPurged(t, b, op.ID, purge.InstanceID)
			retryPurge(t, b, cfg.now, purge, op.ID)
		})
	}
}

// TestPurgeWithNewerSpec: a spec accepted while the purge runs keeps the
// binding and the spec; the purge is recorded as current and the newer
// spec is pending.
func TestPurgeWithNewerSpec(t *testing.T) {
	cfg := newPurgeEnv(t)
	a, _, op := purgeAgent(t, cfg, nil)
	if _, err := a.ops.update(op.ID, func(o *agentapi.Operation) { o.State = agentapi.OpRunning }); err != nil {
		t.Fatal(err)
	}
	// Accept generation 3 after the database was dropped, before finish.
	st, err := loadState(a.instanceDir("git"))
	if err != nil {
		t.Fatal(err)
	}
	if rec := put(t, a, cfg.now, "git", mustJSON(t, testSpec(3))); rec.Code != http.StatusAccepted {
		t.Fatalf("generation 3: %d %s", rec.Code, rec.Body)
	}
	if code := a.finish(a.instance("git"), st, st.desired, &op, nil); code != "" {
		t.Fatalf("finish = %q", code)
	}
	st, err = loadState(a.instanceDir("git"))
	if err != nil || st.current == nil || st.current.Generation != 2 || !st.pending() || st.desired.Generation != 3 {
		t.Fatalf("state after the purge: %+v, %v", st, err)
	}
	if got, _ := a.ops.get(op.ID); got.State != agentapi.OpSucceeded {
		t.Fatalf("purge operation = %+v", got)
	}
}

// TestFinishNeverWritesAnotherInstance: a result is recorded only into a
// directory still bound to the operation's instance.
func TestFinishNeverWritesAnotherInstance(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	rec := put(t, a, now, "git", mustJSON(t, testSpec(1)))
	op := decodeOp(t, rec)
	dir := a.instanceDir("git")
	st, err := loadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	other := struct {
		InstanceID uuid.UUID `json:"instance_id"`
	}{uuid.MustParse("0190f0f0-0000-7000-8000-000000000002")}
	if err := writeJSON(filepath.Join(dir, instanceFile), other); err != nil {
		t.Fatal(err)
	}
	if code := a.finish(a.instance("git"), st, st.desired, &op, nil); code != agentapi.CodeInternal {
		t.Fatalf("finish into a rebound directory = %q, want internal", code)
	}
	if _, err := os.Stat(filepath.Join(dir, currentFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current.json written into another instance's directory: %v", err)
	}
}

// TestRecoverOutcomes: after a restart an unfinished operation reports what
// is durable on disk (#83).
func TestRecoverOutcomes(t *testing.T) {
	now := time.Now()
	cfg := testConfig(t)
	newAgent := func() *Agent {
		a, err := New(Options{Config: cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := newAgent()
	running := func(key string, gen int64) agentapi.Operation {
		t.Helper()
		rec := put(t, a, now, key, mustJSON(t, testSpec(gen)))
		op := decodeOp(t, rec)
		if _, err := a.ops.update(op.ID, func(o *agentapi.Operation) { o.State, o.Phase = agentapi.OpRunning, agentapi.PhaseStarting }); err != nil {
			t.Fatal(err)
		}
		return op
	}

	// current.json was written, the operation not yet updated.
	applied := running("applied", 1)
	if err := writeSpec(filepath.Join(a.instanceDir("applied"), currentFile), testSpec(1)); err != nil {
		t.Fatal(err)
	}
	// A newer spec was accepted while it ran.
	superseded := running("superseded", 1)
	if err := writeSpec(filepath.Join(a.instanceDir("superseded"), desiredFile), testSpec(2)); err != nil {
		t.Fatal(err)
	}
	// blocked.json blocked the generation, the operation not yet updated.
	blocked := running("blocked", 1)
	if err := writeJSON(filepath.Join(a.instanceDir("blocked"), blockedFile), blockedState{
		Generations: []blockedGeneration{{Generation: 1, ErrorCode: agentapi.CodeHealthTimeout, At: now.UTC()}},
	}); err != nil {
		t.Fatal(err)
	}
	pending := running("pending", 1)
	// The directory is gone without a purge record of this generation.
	gone := running("gone", 1)
	if err := os.RemoveAll(a.instanceDir("gone")); err != nil {
		t.Fatal(err)
	}
	// The directory is gone and a purge record holds the generation, but the
	// operation is an apply: only a remove may recover as succeeded.
	purgedApply := running("purged-apply", 1)
	if err := os.RemoveAll(a.instanceDir("purged-apply")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(a.purgedPath("purged-apply"), purgedRecord{
		Generation: 1, Operation: purgedApply.ID, At: now.UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	b := newAgent()
	for _, c := range []struct {
		name  string
		op    agentapi.Operation
		state string
		code  string
	}{
		{"applied", applied, agentapi.OpSucceeded, ""},
		{"superseded", superseded, agentapi.OpFailed, agentapi.CodeSuperseded},
		{"blocked", blocked, agentapi.OpFailed, agentapi.CodeHealthTimeout},
		{"pending", pending, agentapi.OpQueued, ""},
		{"gone", gone, agentapi.OpFailed, agentapi.CodeInterrupted},
		{"purged-apply", purgedApply, agentapi.OpFailed, agentapi.CodeInterrupted},
	} {
		if got, _ := b.ops.get(c.op.ID); got.State != c.state || got.ErrorCode != c.code || got.Phase != "" {
			t.Errorf("%s: operation after the restart = %+v, want %s %q", c.name, got, c.state, c.code)
		}
	}
}

// TestRecoverPurgeSyncsTheRename: a crash after a purge's rename but before
// its directory sync leaves the operation unfinished and the purge record
// durable. Recovery syncs the rename before it reports success; a sync
// failure is recorded as internal, like one from the purge itself.
func TestRecoverPurgeSyncsTheRename(t *testing.T) {
	failure := errors.New("injected directory sync failure")
	for _, tc := range []struct {
		name  string
		sync  func(string) error
		state string
		code  string
	}{
		{"synced", nil, agentapi.OpSucceeded, ""},
		{"sync failure", func(string) error { return failure }, agentapi.OpFailed, agentapi.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			cfg := testConfig(t)
			cfg.DataDir = t.TempDir()
			opts := Options{Config: cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}}
			a, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			spec := testSpec(2)
			op, err := a.ops.create("git", agentapi.KindRemove, spec.Generation, spec.InstanceID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.ops.update(op.ID, func(o *agentapi.Operation) { o.State = agentapi.OpRunning }); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(a.purgedPath("git"), purgedRecord{
				InstanceID: spec.InstanceID, Generation: spec.Generation, SpecHash: spec.Hash(), Operation: op.ID, At: now.UTC(),
			}); err != nil {
				t.Fatal(err)
			}

			opts.syncDir = tc.sync
			b, err := New(opts)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			got, ok := b.ops.get(op.ID)
			if !ok || got.State != tc.state || got.ErrorCode != tc.code {
				t.Fatalf("operation after recovery = %+v (%t), want %s %q", got, ok, tc.state, tc.code)
			}
		})
	}
}

// TestPurgeRetryAfterPruningSyncsTheRename: the purge record survives
// operation pruning. A retry that has to record the operation again checks
// the rename's directory sync before it reports success, so a failed sync
// stays failed instead of being hidden by a fresh succeeded operation.
func TestPurgeRetryAfterPruningSyncsTheRename(t *testing.T) {
	cfg := newPurgeEnv(t)
	a, purge, op := purgeAgent(t, cfg, nil)
	a.reconcile(t.Context(), a.instance("git"))
	assertPurged(t, a, op.ID, purge.InstanceID)

	pruneOperation(t, a, op.ID, cfg.now)

	// A failed sync on the retry is recorded, not hidden by success.
	a.opts.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	rec := put(t, a, cfg.now, "git", mustJSON(t, purge))
	failed := decodeOp(t, rec)
	if rec.Code != http.StatusOK || failed.State != agentapi.OpFailed || failed.ErrorCode != agentapi.CodeInternal {
		t.Fatalf("retry with a failed sync = %d %+v", rec.Code, failed)
	}

	// Once the rename is synced, the retry records success.
	pruneOperation(t, a, failed.ID, cfg.now)
	a.opts.syncDir = nil
	rec = put(t, a, cfg.now, "git", mustJSON(t, purge))
	if o := decodeOp(t, rec); rec.Code != http.StatusOK || o.State != agentapi.OpSucceeded {
		t.Fatalf("retry after pruning = %d %+v", rec.Code, o)
	}
}

// TestPurgeRetryAfterPruningKeepsIdentity: a pruned purge operation is
// recorded again under the ID the purge record names. Two identical retries
// without pruning between them, and a restart, return that one operation
// instead of accumulating replacements.
func TestPurgeRetryAfterPruningKeepsIdentity(t *testing.T) {
	cfg := newPurgeEnv(t)
	a, purge, op := purgeAgent(t, cfg, nil)
	a.reconcile(t.Context(), a.instance("git"))
	assertPurged(t, a, op.ID, purge.InstanceID)
	pruneOperation(t, a, op.ID, cfg.now)

	rec := put(t, a, cfg.now, "git", mustJSON(t, purge))
	first := decodeOp(t, rec)
	if rec.Code != http.StatusOK || first.ID != op.ID || first.State != agentapi.OpSucceeded {
		t.Fatalf("retry after pruning = %d %+v, want 200 with %s succeeded", rec.Code, first, op.ID)
	}
	rec = put(t, a, cfg.now, "git", mustJSON(t, purge))
	second := decodeOp(t, rec)
	if rec.Code != http.StatusOK || second.ID != first.ID || second.State != agentapi.OpSucceeded {
		t.Fatalf("repeated retry = %d %+v, want 200 with %s succeeded", rec.Code, second, first.ID)
	}

	b := cfg.agent(t, nil)
	rec = put(t, b, cfg.now, "git", mustJSON(t, purge))
	third := decodeOp(t, rec)
	if rec.Code != http.StatusOK || third.ID != op.ID || third.State != agentapi.OpSucceeded {
		t.Fatalf("retry after a restart = %d %+v, want 200 with %s succeeded", rec.Code, third, op.ID)
	}
	assertPurged(t, b, op.ID, purge.InstanceID)
}

// TestPurgeRetryAfterPruningKeepsFailedResult: a sync failure recorded by a
// pruned-operation retry is the terminal result of that operation. Repeated
// retries without pruning it and a restart with a working sync return the
// same failed operation rather than a fresh success.
func TestPurgeRetryAfterPruningKeepsFailedResult(t *testing.T) {
	cfg := newPurgeEnv(t)
	a, purge, op := purgeAgent(t, cfg, nil)
	a.reconcile(t.Context(), a.instance("git"))
	assertPurged(t, a, op.ID, purge.InstanceID)
	pruneOperation(t, a, op.ID, cfg.now)

	a.opts.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	rec := put(t, a, cfg.now, "git", mustJSON(t, purge))
	failed := decodeOp(t, rec)
	if rec.Code != http.StatusOK || failed.ID != op.ID || failed.State != agentapi.OpFailed || failed.ErrorCode != agentapi.CodeInternal {
		t.Fatalf("retry with a failed sync = %d %+v, want 200 with %s failed internal", rec.Code, failed, op.ID)
	}

	a.opts.syncDir = nil
	rec = put(t, a, cfg.now, "git", mustJSON(t, purge))
	if o := decodeOp(t, rec); rec.Code != http.StatusOK || o.ID != failed.ID || o.State != agentapi.OpFailed || o.ErrorCode != agentapi.CodeInternal {
		t.Fatalf("repeated retry = %d %+v, want the retained failure %s", rec.Code, o, failed.ID)
	}
	b := cfg.agent(t, nil)
	rec = put(t, b, cfg.now, "git", mustJSON(t, purge))
	if o := decodeOp(t, rec); rec.Code != http.StatusOK || o.ID != failed.ID || o.State != agentapi.OpFailed || o.ErrorCode != agentapi.CodeInternal {
		t.Fatalf("retry after a restart = %d %+v, want the retained failure %s", rec.Code, o, failed.ID)
	}

	// Pruning the failure lets the next retry check and report again.
	pruneOperation(t, b, failed.ID, cfg.now)
	retryPurge(t, b, cfg.now, purge, op.ID)
}

// TestPurgeRetrySettlesAfterTransientResultFailure: a retry after pruning
// records the purge's operation as running; if writing its terminal result
// then fails, the PUT answers 500 and the operation stays nonterminal. Once
// storage is healthy, a retry settles that recorded replay from the purge
// record (after the same directory sync) instead of returning running
// forever, and a restart keeps the settled result.
func TestPurgeRetrySettlesAfterTransientResultFailure(t *testing.T) {
	cfg := newPurgeEnv(t)
	a, purge, op := purgeAgent(t, cfg, nil)
	a.reconcile(t.Context(), a.instance("git"))
	assertPurged(t, a, op.ID, purge.InstanceID)
	pruneOperation(t, a, op.ID, cfg.now)

	// The running replacement is durable, but replacing its file with the
	// terminal result fails: the operation file is a directory.
	path := a.ops.path(op.ID)
	a.opts.hook = func(point string) {
		if point != "purge-retry-recorded" {
			return
		}
		if err := os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rec := put(t, a, cfg.now, "git", mustJSON(t, purge))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("retry with an unwritable result: %d %s", rec.Code, rec.Body)
	}
	if got, ok := a.ops.get(op.ID); !ok || got.Terminal() || got.State != agentapi.OpRunning {
		t.Fatalf("operation after the failed result write = %+v (%t), want running", got, ok)
	}

	// Storage is healthy again. Reconciliation cannot settle the replay (the
	// directory is gone), so every identical retry must settle it and return
	// the purge's terminal result.
	a.opts.hook = nil
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		a.reconcile(t.Context(), a.instance("git"))
		rec = put(t, a, cfg.now, "git", mustJSON(t, purge))
		got := decodeOp(t, rec)
		if rec.Code != http.StatusOK || got.ID != op.ID || got.State != agentapi.OpSucceeded || got.ErrorCode != "" {
			t.Fatalf("healthy retry %d = %d %+v, want 200 with %s succeeded", i, rec.Code, got, op.ID)
		}
	}

	// The settled result is durable and a later retry keeps it.
	b := cfg.agent(t, nil)
	if got, ok := b.ops.get(op.ID); !ok || got.State != agentapi.OpSucceeded {
		t.Fatalf("operation after the restart = %+v (%t), want succeeded", got, ok)
	}
	assertPurged(t, b, op.ID, purge.InstanceID)
	retryPurge(t, b, cfg.now, purge, op.ID)
}

// TestPurgeRetryAfterPruningCrashRecovery: the operation recorded again for
// a pruned purge is durable under the purge record's ID before its result.
// A crash there recovers that operation, not a new one.
func TestPurgeRetryAfterPruningCrashRecovery(t *testing.T) {
	cfg := newPurgeEnv(t)
	a, purge, op := purgeAgent(t, cfg, func(point string) {
		if point == "purge-retry-recorded" {
			runtime.Goexit()
		}
	})
	a.reconcile(t.Context(), a.instance("git"))
	assertPurged(t, a, op.ID, purge.InstanceID)
	pruneOperation(t, a, op.ID, cfg.now)

	done := make(chan struct{})
	go func() {
		defer close(done)
		signedPut(t, a, cfg.now, "git", mustJSON(t, purge))
	}()
	<-done
	if got, ok := a.ops.get(op.ID); !ok || got.Terminal() {
		t.Fatalf("operation after the crash = %+v (%t), want unfinished", got, ok)
	}

	b := cfg.agent(t, nil)
	got, ok := b.ops.get(op.ID)
	if !ok || got.State != agentapi.OpSucceeded || got.ErrorCode != "" {
		t.Fatalf("operation after the restart = %+v (%t), want succeeded", got, ok)
	}
	retryPurge(t, b, cfg.now, purge, op.ID)
}

// TestPurgeRecordDoesNotSettleAnotherInstance: after a real purge, a new
// instance reuses the key and accepts a remove at the same generation. A
// host crash can lose its directory entry while its operation survives
// (syncing a child does not sync its parent). Recovery must not settle that
// remove from the previous instance's purge record, although key, generation
// and kind match.
func TestPurgeRecordDoesNotSettleAnotherInstance(t *testing.T) {
	cfg := newPurgeEnv(t)
	a, purge, op := purgeAgent(t, cfg, nil)
	a.reconcile(t.Context(), a.instance("git"))
	assertPurged(t, a, op.ID, purge.InstanceID)

	remove := testSpec(purge.Generation)
	remove.InstanceID = uuid.MustParse("0190f0f0-0000-7000-8000-000000000002")
	remove.State, remove.Purge = agentapi.StateAbsent, false
	rec := put(t, a, cfg.now, "git", mustJSON(t, remove))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("remove of the new instance: %d %s", rec.Code, rec.Body)
	}
	reused := decodeOp(t, rec)
	if reused.ID == op.ID || reused.Kind != agentapi.KindRemove || reused.Generation != purge.Generation {
		t.Fatalf("remove of the new instance = %+v", reused)
	}

	// The state the old code permitted: the new binding's directory entry
	// was never durable, its operation was.
	if err := os.RemoveAll(a.instanceDir("git")); err != nil {
		t.Fatal(err)
	}
	p, err := readPurged(a.purgedPath("git"))
	if err != nil || p == nil || p.InstanceID != purge.InstanceID || p.Operation != op.ID {
		t.Fatalf("purge record after the reuse = %+v, %v", p, err)
	}

	b := cfg.agent(t, nil)
	got, ok := b.ops.get(reused.ID)
	if !ok || got.State != agentapi.OpFailed || got.ErrorCode != agentapi.CodeInterrupted {
		t.Fatalf("operation after the restart = %+v (%t), want failed interrupted", got, ok)
	}
}

// TestNewBindingIsDurableBeforeOperation: the directory entry of a new
// binding is synced in instances/ before the operation of its first spec is
// published, so a host crash cannot lose the binding while keeping an
// operation that another instance's purge record could settle. A failed sync
// refuses the acceptance without publishing an operation; the retry syncs
// again.
func TestNewBindingIsDurableBeforeOperation(t *testing.T) {
	cfg := newPurgeEnv(t)
	a := cfg.agent(t, nil)
	var synced []string
	a.opts.syncDir = func(path string) error {
		if path == a.instancesDir {
			if _, ok := a.ops.forGeneration("git", 1, testSpec(1).InstanceID); ok {
				t.Error("the operation was published before the binding was synced")
			}
		}
		synced = append(synced, path)
		return syncDir(path)
	}
	if rec := put(t, a, cfg.now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatalf("install: %d %s", rec.Code, rec.Body)
	}
	if !slices.Contains(synced, a.instancesDir) {
		t.Fatal("the new binding was not synced before the operation")
	}

	failure := errors.New("injected directory sync failure")
	a.opts.syncDir = func(path string) error {
		if path == a.instancesDir {
			return failure
		}
		return syncDir(path)
	}
	wiki := testSpec(1)
	rec := put(t, a, cfg.now, "wiki", mustJSON(t, wiki))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("install with a failed binding sync: %d %s", rec.Code, rec.Body)
	}
	if _, ok := a.ops.forGeneration("wiki", 1, testSpec(1).InstanceID); ok {
		t.Fatal("an operation was published although the binding sync failed")
	}

	a.opts.syncDir = func(path string) error {
		if path == a.instancesDir {
			if _, ok := a.ops.forGeneration("wiki", 1, testSpec(1).InstanceID); ok {
				t.Error("the operation was published before the binding was synced")
			}
		}
		return syncDir(path)
	}
	rec = put(t, a, cfg.now, "wiki", mustJSON(t, wiki))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry after the failed sync: %d %s", rec.Code, rec.Body)
	}
	if op, ok := a.ops.forGeneration("wiki", 1, testSpec(1).InstanceID); !ok || op.State != agentapi.OpQueued {
		t.Fatalf("operation after the retry = %+v (%t), want queued", op, ok)
	}
}
