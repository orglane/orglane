package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
)

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
// status, and no operation of the key other than want.
func assertPurged(t *testing.T, a *Agent, want uuid.UUID) {
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
	if op, ok := a.ops.forGeneration("git", 2); !ok || op.ID != want || op.State != agentapi.OpSucceeded {
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
	assertPurged(t, a, op.ID)

	// The lost answer is retried after the purge, also after a restart.
	retryPurge(t, a, cfg.now, purge, op.ID)
	b := cfg.agent(t, nil)
	retryPurge(t, b, cfg.now, purge, op.ID)
	assertPurged(t, b, op.ID)

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
			assertPurged(t, b, op.ID)
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
	} {
		if got, _ := b.ops.get(c.op.ID); got.State != c.state || got.ErrorCode != c.code || got.Phase != "" {
			t.Errorf("%s: operation after the restart = %+v, want %s %q", c.name, got, c.state, c.code)
		}
	}
}
