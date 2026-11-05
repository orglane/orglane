//go:build integration

package apps_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
)

// Regression tests for changes committed while both a job and its
// follow-up twin are running: River shows a job running until it persists
// the completion, which it does after the work returned (batched, after the
// framework's locks are released), so both twins may have read the state
// already. The change must still get a job that reads it.

// runAsRunning works a job by hand while River shows it running, as a
// worker that returned before River recorded its completion.
func (h *harness) runAsRunning(kind string, work func() error) {
	h.t.Helper()
	h.exec(`UPDATE river_job SET state = 'running', attempted_at = now() WHERE kind = $1 AND state <> 'running'`, kind)
	if err := work(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) running(kind string) int {
	return h.count(`SELECT count(*) FROM river_job WHERE kind = $1 AND state = 'running'`, kind)
}

func (h *harness) continuations(kind string) int {
	return h.count(`SELECT count(*) FROM river_job WHERE kind = $1 AND state IN ('available', 'scheduled', 'retryable') AND args ? 'continuation'`, kind)
}

// TestMappingWhileTwinsRun: provision_instance (enqueueOnce).
func TestMappingWhileTwinsRun(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.drain()
	kind := apps.ProvisionInstanceArgs{}.Kind()
	first := h.team("first")
	h.mapTeam(inst, first)
	h.runAsRunning(kind, func() error {
		return h.svc.RunProvisionInstance(h.ctx, apps.ProvisionInstanceArgs{InstanceID: inst.ID, Reason: apps.ReasonMapping})
	})
	h.mapTeam(inst, first) // queues the follow-up twin
	h.runAsRunning(kind, func() error {
		return h.svc.RunProvisionInstance(h.ctx, apps.ProvisionInstanceArgs{InstanceID: inst.ID, Reason: apps.ReasonMapping, FollowUp: true})
	})
	if n := h.running(kind); n != 2 {
		t.Fatalf("%d running provision_instance jobs, want both twins", n)
	}
	second := h.team("second")
	h.mapTeam(inst, second)
	if n, c := h.jobs(kind), h.continuations(kind); n != 1 || c != 1 {
		t.Fatalf("%d runnable provision_instance jobs (%d continuations), want one continuation", n, c)
	}
	// Bounded: a further change while the continuation waits joins it.
	h.mapTeam(inst, h.team("third"))
	if n := h.jobs(kind); n != 1 {
		t.Fatalf("%d runnable provision_instance jobs after a second change, want 1", n)
	}
	h.exec(`DELETE FROM river_job WHERE state = 'running'`) // River records the twins' completion
	h.drain()
	teams := h.tp.Teams(inst.ID)
	for _, ref := range []string{"team-second", "team-third"} {
		if _, ok := teams[ref]; !ok {
			t.Fatalf("provider teams %v lack %s: the mapping was lost", teams, ref)
		}
	}
}

// TestConfirmationWhileTwinsRun: apply_removals (enqueueOnce with a
// schedule) and the removal confirmation.
func TestConfirmationWhileTwinsRun(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	people := []store.Person{h.person("alice"), h.person("bob"), h.person("carol"), h.person("dave")}
	h.drain()
	h.planRemovals(inst, people...)
	kind := apps.ApplyRemovalsArgs{}.Kind()
	replan := func() {
		t.Helper()
		if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: people[0].ID}, 1, 8); err != nil {
			t.Fatal(err)
		}
	}
	replan() // queues the guard
	h.runAsRunning(kind, func() error {
		return h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8) // holds the four removals
	})
	replan() // queues the follow-up twin
	h.runAsRunning(kind, func() error {
		return h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID, FollowUp: true}, 1, 8)
	})
	if n := h.running(kind); n != 2 {
		t.Fatalf("%d running apply_removals jobs, want both twins", n)
	}
	b, err := h.svc.HeldRemovals(h.ctx, inst.ID)
	if err != nil || b == nil {
		t.Fatalf("held batch = %v, %v", b, err)
	}
	if err := h.svc.ConfirmRemovals(h.ctx, audit.System, inst.ID, b.Batch.ID, b.Batch.ItemsHash); err != nil {
		t.Fatal(err)
	}
	if n := h.continuations(kind); n != 1 {
		t.Fatalf("%d apply_removals continuations after the confirmation, want 1", n)
	}
	h.exec(`DELETE FROM river_job WHERE state = 'running'`)
	h.drain()
	if held := h.removalPeople(inst, "held"); len(held) != 0 {
		t.Fatalf("confirmed removals of %v still held", held)
	}
	for _, p := range people {
		if acct, _ := h.tp.Account(inst.ID, p.Username); acct.Member {
			t.Fatalf("%s kept access after the confirmation", p.Username)
		}
	}
}

// TestSuspensionWhileTwinsRun: provision_person (enqueuePerson), whose
// result marker cannot help once the running twins made their last check.
func TestSuspensionWhileTwinsRun(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice := h.person("alice")
	h.drain()
	kind := apps.ProvisionPersonArgs{}.Kind()
	for i, name := range []string{"Name One", "Name Two"} {
		if _, err := h.ids.UpdatePerson(h.ctx, audit.System, alice.ID, identity.PersonUpdate{DisplayName: &name}); err != nil {
			t.Fatal(err)
		}
		h.runAsRunning(kind, func() error {
			return h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID, FollowUp: i == 1}, 1, 8)
		})
	}
	if n := h.running(kind); n != 2 {
		t.Fatalf("%d running provision_person jobs, want both twins", n)
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if n := h.personJobs(alice.ID); n != 1 {
		t.Fatalf("%d runnable provision_person jobs after the suspension, want 1", n)
	}
	h.exec(`DELETE FROM river_job WHERE state = 'running'`)
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "alice"); acct.Member {
		t.Fatalf("suspended alice kept access; result %s", h.result(inst, alice).Status)
	}
}

// gatedInstanceWorker works provision_instance, then holds the job open
// (River still shows it running) until release is closed.
type gatedInstanceWorker struct {
	river.WorkerDefaults[apps.ProvisionInstanceArgs]
	svc      *apps.Service
	returned chan error
	release  chan struct{}
}

func (w *gatedInstanceWorker) Work(ctx context.Context, j *river.Job[apps.ProvisionInstanceArgs]) error {
	err := w.svc.RunProvisionInstance(ctx, j.Args)
	w.returned <- err
	select {
	case <-w.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return err
}

// TestMappingWhileTwinsRunOnRiver: the same with a started River client
// with two workers: both twins finish their work while River still shows
// them running; a mapping committed then is synced by a continuation that
// River runs.
func TestMappingWhileTwinsRunOnRiver(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.drain()
	workers := river.NewWorkers()
	w := &gatedInstanceWorker{svc: h.svc, returned: make(chan error, 4), release: make(chan struct{})}
	river.AddWorker(workers, w)
	rc, err := river.NewClient(riverpgxv5.New(h.pool), &river.Config{
		Workers:           workers,
		Queues:            map[string]river.QueueConfig{apps.QueueName: {MaxWorkers: 2}},
		FetchCooldown:     time.Millisecond,
		FetchPollInterval: 10 * time.Millisecond,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.svc.SetQueue(rc)
	if err := rc.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			close(w.release)
		}
	}
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rc.StopAndCancel(ctx)
	})
	worked := func() {
		t.Helper()
		select {
		case err := <-w.returned:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("provision_instance did not run")
		}
	}
	kind := apps.ProvisionInstanceArgs{}.Kind()
	first := h.team("first")
	h.mapTeam(inst, first)
	worked()
	h.mapTeam(inst, first) // the follow-up twin
	worked()
	if n := h.running(kind); n != 2 {
		t.Fatalf("%d running provision_instance jobs, want both twins", n)
	}
	h.mapTeam(inst, h.team("second"))
	if n := h.jobs(kind); n != 1 {
		t.Fatalf("%d runnable provision_instance jobs while both twins run, want 1", n)
	}
	release()
	worked() // River decodes and runs the continuation
	if _, ok := h.tp.Teams(inst.ID)["team-second"]; !ok {
		t.Fatalf("provider teams %v lack team-second", h.tp.Teams(inst.ID))
	}
}
