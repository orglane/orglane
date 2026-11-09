package agent

import (
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// purgedKey leaves the agent with a completed purge of "git" at generation 2
// and returns its purge spec and operation.
func purgedKey(t *testing.T, cfg purgeEnv) (*Agent, *agentapi.Spec, agentapi.Operation) {
	t.Helper()
	a, purge, op := purgeAgent(t, cfg, nil)
	a.reconcile(t.Context(), a.instance("git"))
	assertPurged(t, a, op.ID, purge.InstanceID)
	return a, purge, op
}

// failParentSync makes the instances/ directory sync fail, as a writeback
// failure between accepting a new binding and publishing its operation does.
func failParentSync(a *Agent) {
	a.opts.syncDir = func(path string) error {
		if path == a.instancesDir {
			return errors.New("injected parent sync failure")
		}
		return syncDir(path)
	}
}

// TestKeyReuseSyncFailureCompletesAcceptance: a new instance B reuses a key
// after A's completed purge, at the same generation. The first PUT persists
// B's binding and desired spec but fails to sync the directory entry before
// the operation; it answers 500. The retry (with and without a restart) must
// complete B's own operation and never return A's retained succeeded purge.
// A's operation stays A's terminal result.
func TestKeyReuseSyncFailureCompletesAcceptance(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "continue"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			cfg := newPurgeEnv(t)
			a, purge, prior := purgedKey(t, cfg)
			reused := testSpec(purge.Generation)
			reused.InstanceID = uuid.Must(uuid.NewV7())

			failParentSync(a)
			for i := range 2 {
				rec := put(t, a, cfg.now, "git", mustJSON(t, reused))
				if rec.Code != http.StatusInternalServerError {
					t.Fatalf("acceptance %d with a failed parent sync: %d %s", i, rec.Code, rec.Body)
				}
				if op, ok := a.ops.forGeneration("git", reused.Generation, reused.InstanceID); ok {
					t.Fatalf("failed acceptance published operation %+v", op)
				}
			}
			a.opts.syncDir = nil

			if restart {
				a = cfg.agent(t, nil)
				st, err := loadState(a.instanceDir("git"))
				if err != nil || st.id != reused.InstanceID || st.desired == nil ||
					st.desired.Generation != reused.Generation || st.current != nil {
					t.Fatalf("binding after the restart: %+v, %v", st, err)
				}
				status, found, err := a.status(t.Context(), "git")
				if err != nil || !found {
					t.Fatalf("status before the retry: %t, %v", found, err)
				}
				if status.Operation != nil {
					t.Fatalf("status before the retry reports operation %+v", status.Operation)
				}
			}

			rec := put(t, a, cfg.now, "git", mustJSON(t, reused))
			if rec.Code != http.StatusOK {
				t.Fatalf("retry after the failed sync: %d %s", rec.Code, rec.Body)
			}
			got := decodeOp(t, rec)
			if got.ID == prior.ID || got.Kind != agentapi.KindApply || got.State != agentapi.OpQueued ||
				got.Generation != reused.Generation {
				t.Fatalf("retry returned %+v, want the new instance's queued apply", got)
			}
			if owner := a.ops.ownerOf(got.ID); owner != reused.InstanceID {
				t.Fatalf("operation %s is owned by %s, want %s", got.ID, owner, reused.InstanceID)
			}
			if saved, ok := a.ops.get(prior.ID); !ok || saved.State != agentapi.OpSucceeded ||
				a.ops.ownerOf(prior.ID) != purge.InstanceID {
				t.Fatalf("previous instance's purge operation changed: %+v (%t)", saved, ok)
			}

			// An identical retry keeps the same operation.
			again := decodeOp(t, put(t, a, cfg.now, "git", mustJSON(t, reused)))
			if again.ID != got.ID || again.State != got.State {
				t.Fatalf("repeated retry returned %+v, want %s %s", again, got.ID, got.State)
			}
		})
	}
}

// TestAcceptanceCrashBoundariesKeepOwnership stops the process after each
// durable step of accepting a spec, for a fresh key and for a key reused
// after another instance's purge at the same generation, then retries after
// a restart. Every successful retry returns an operation published for the
// current instance; the previous instance's operation is never an answer.
func TestAcceptanceCrashBoundariesKeepOwnership(t *testing.T) {
	for _, reused := range []bool{false, true} {
		for _, point := range []string{"accepted-binding", "accepted-desired", "accepted-operation"} {
			t.Run(fmt.Sprintf("reused=%t/%s", reused, point), func(t *testing.T) {
				cfg := newPurgeEnv(t)
				a := cfg.agent(t, nil)
				var prior agentapi.Operation
				if reused {
					var purge *agentapi.Spec
					a, purge, prior = purgedKey(t, cfg)
					_ = purge
				}
				spec := testSpec(2)
				spec.InstanceID = uuid.Must(uuid.NewV7())

				var published agentapi.Operation
				a.opts.hook = func(p string) {
					if p != point {
						return
					}
					if point == "accepted-operation" {
						if op, ok := a.ops.forGeneration("git", spec.Generation, spec.InstanceID); ok {
							published = op
						}
					}
					runtime.Goexit()
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					signedPut(t, a, cfg.now, "git", mustJSON(t, spec))
				}()
				<-done
				switch point {
				case "accepted-operation":
					if published.ID == uuid.Nil {
						t.Fatal("the operation was not published before the crash point")
					}
				default:
					if published.ID != uuid.Nil {
						t.Fatalf("an operation was published before the crash point: %+v", published)
					}
				}

				b := cfg.agent(t, nil)
				st, err := loadState(b.instanceDir("git"))
				if err != nil || st.id != spec.InstanceID || st.current != nil {
					t.Fatalf("binding after the crash at %s: %+v, %v", point, st, err)
				}
				if point == "accepted-binding" && st.desired != nil {
					t.Fatalf("desired spec before the crash point: %+v", st.desired)
				}
				if point != "accepted-binding" && (st.desired == nil || st.desired.Generation != spec.Generation) {
					t.Fatalf("desired spec after the crash at %s: %+v", point, st.desired)
				}

				rec := put(t, b, cfg.now, "git", mustJSON(t, spec))
				want := http.StatusOK
				if point == "accepted-binding" {
					// The retry accepts the spec again.
					want = http.StatusAccepted
				}
				if rec.Code != want {
					t.Fatalf("retry after the crash at %s: %d %s, want %d", point, rec.Code, rec.Body, want)
				}
				got := decodeOp(t, rec)
				if prior.ID != uuid.Nil && got.ID == prior.ID {
					t.Fatalf("retry returned the previous instance's operation %s", got.ID)
				}
				if got.Kind != agentapi.KindApply || got.Generation != spec.Generation || got.State != agentapi.OpQueued {
					t.Fatalf("retry returned %+v, want the new instance's queued apply", got)
				}
				if owner := b.ops.ownerOf(got.ID); owner != spec.InstanceID {
					t.Fatalf("operation %s is owned by %s, want %s", got.ID, owner, spec.InstanceID)
				}
				if point == "accepted-operation" && got.ID != published.ID {
					t.Fatalf("retry after the publish returned %s, want %s", got.ID, published.ID)
				}
				if n := len(b.ops.unfinishedOwned("git", spec.InstanceID)); n != 1 {
					t.Fatalf("the new instance has %d unfinished operations, want 1", n)
				}
				if prior.ID != uuid.Nil {
					if saved, ok := b.ops.get(prior.ID); !ok || saved.State != agentapi.OpSucceeded {
						t.Fatalf("previous instance's operation after the retry: %+v (%t)", saved, ok)
					}
				}
			})
		}
	}
}

// TestLegacyOperationWithoutPurgeRecord: an operation without an owner and a
// key without a purge record may predate a reuse by a version that did not
// record purges. It stays unattributed, is not returned as the current
// binding's operation, and is recovered as interrupted; the retry publishes
// the current instance's own operation.
func TestLegacyOperationWithoutPurgeRecord(t *testing.T) {
	cfg := newPurgeEnv(t)
	a := cfg.agent(t, nil)
	rec := put(t, a, cfg.now, "git", mustJSON(t, testSpec(1)))
	legacy := decodeOp(t, rec)

	// An old agent version wrote the operation without instance_id.
	if err := writeJSON(a.ops.path(legacy.ID), legacy); err != nil {
		t.Fatal(err)
	}
	b := cfg.agent(t, nil)
	if owner := b.ops.ownerOf(legacy.ID); owner != uuid.Nil {
		t.Fatalf("legacy operation owner = %s, want none", owner)
	}
	if got, ok := b.ops.get(legacy.ID); !ok || got.State != agentapi.OpFailed || got.ErrorCode != agentapi.CodeInterrupted {
		t.Fatalf("legacy operation after the restart = %+v (%t), want failed interrupted", got, ok)
	}
	if status, found, err := b.status(t.Context(), "git"); err != nil || !found || status.Operation != nil {
		t.Fatalf("status before the retry = %+v, %t, %v", status, found, err)
	}

	again := decodeOp(t, put(t, b, cfg.now, "git", mustJSON(t, testSpec(1))))
	if again.ID == legacy.ID || b.ops.ownerOf(again.ID) != testSpec(1).InstanceID {
		t.Fatalf("retry returned %+v, want the current instance's own operation", again)
	}
}

// TestLegacyOperationOwnership: operations persisted before the owner was
// recorded are assigned to the binding that owns them at start. One that can
// predate the purge that freed the key stays unowned, so the new binding
// neither returns nor recovers it as its own.
func TestLegacyOperationOwnership(t *testing.T) {
	cfg := newPurgeEnv(t)
	a, purge, _ := purgedKey(t, cfg)
	reused := testSpec(purge.Generation)
	reused.InstanceID = uuid.Must(uuid.NewV7())
	rec := put(t, a, cfg.now, "git", mustJSON(t, reused))
	op := decodeOp(t, rec)
	p, err := readPurged(a.purgedPath("git"))
	if err != nil || p == nil {
		t.Fatalf("purge record: %+v, %v", p, err)
	}

	// An old agent version wrote the operation without instance_id. It
	// started after the purge record, so it belongs to B.
	op.StartedAt = p.At.Add(1)
	if err := writeJSON(a.ops.path(op.ID), op); err != nil {
		t.Fatal(err)
	}
	b := cfg.agent(t, nil)
	if owner := b.ops.ownerOf(op.ID); owner != reused.InstanceID {
		t.Fatalf("legacy operation owner = %s, want %s", owner, reused.InstanceID)
	}
	again := decodeOp(t, put(t, b, cfg.now, "git", mustJSON(t, reused)))
	if again.ID != op.ID || again.State != op.State {
		t.Fatalf("retry returned %+v, want the recovered operation %s", again, op.ID)
	}

	// An operation that could predate the purge (started before its record)
	// stays unattributed: recovery fails it as interrupted instead of
	// settling it from the new binding's state.
	old, err := a.ops.create("git", agentapi.KindApply, reused.Generation, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ops.update(old.ID, func(o *agentapi.Operation) {
		o.State, o.StartedAt = agentapi.OpRunning, p.At.Add(-1)
	}); err != nil {
		t.Fatal(err)
	}
	c := cfg.agent(t, nil)
	if owner := c.ops.ownerOf(old.ID); owner != uuid.Nil {
		t.Fatalf("legacy operation before the purge got owner %s", owner)
	}
	if got, ok := c.ops.get(old.ID); !ok || got.State != agentapi.OpFailed || got.ErrorCode != agentapi.CodeInterrupted {
		t.Fatalf("legacy operation after the restart = %+v (%t), want failed interrupted", got, ok)
	}
}
