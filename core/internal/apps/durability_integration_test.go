//go:build integration

package apps_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// Regression tests for #81: the removal guard, setup and rotation keep
// their progress and follow-up jobs across failures, and honor a
// provider's RetryAfter.

func rateLimited(after time.Duration) error {
	return &provider.Error{Code: "rate_limited", Retryable: true, RetryAfter: after}
}

func wantSnooze(t *testing.T, err error, d time.Duration) {
	t.Helper()
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != d {
		t.Fatalf("err = %v, want a %s snooze", err, d)
	}
}

// planRemovals suspends people and plans their removals without applying
// them: the jobs the changes queue are dropped, so each step runs by hand.
func (h *harness) planRemovals(inst apps.Instance, people ...store.Person) {
	h.t.Helper()
	for _, p := range people {
		if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, p.ID, identity.StatusSuspended); err != nil {
			h.t.Fatal(err)
		}
	}
	h.exec(`DELETE FROM river_job`)
	for _, p := range people {
		if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: p.ID}, 1, 8); err != nil {
			h.t.Fatal(err)
		}
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE instance_id = $1 AND state = 'pending'`, inst.ID); n != len(people) {
		h.t.Fatalf("%d pending removals, want %d", n, len(people))
	}
	h.exec(`DELETE FROM river_job`)
}

// removalPeople returns the people whose removals are in state.
func (h *harness) removalPeople(inst apps.Instance, state string) []uuid.UUID {
	h.t.Helper()
	rows, err := h.pool.Query(h.ctx, `SELECT person_id FROM provisioning_removals WHERE instance_id = $1 AND state = $2 ORDER BY person_id`, inst.ID, state)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) personJobs(personID uuid.UUID) int {
	return h.count(`SELECT count(*) FROM river_job WHERE kind = $1 AND state IN ('available', 'scheduled', 'retryable') AND args->>'person_id' = $2`,
		apps.ProvisionPersonArgs{}.Kind(), personID.String())
}

func (h *harness) removeCalls() int {
	n := 0
	for _, c := range h.tp.Calls() {
		if c == "apply:remove_from_org" {
			n++
		}
	}
	return n
}

// TestRemovalProgressSurvivesAnotherPersonsFailure: when one person's
// removal fails and the job retries, the other person's applied removal
// keeps its follow-up job, so their result does not stay queued forever
// (the retry does not see the applied removal again).
func TestRemovalProgressSurvivesAnotherPersonsFailure(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice, bob := h.person("alice"), h.person("bob")
	h.drain()
	h.planRemovals(inst, alice, bob)

	// The first removal succeeds, the second fails transiently.
	h.tp.Fail("apply:remove_from_org", nil, retryable("unavailable"))
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8)
	var snooze *river.JobSnoozeError
	if err == nil || errors.As(err, &snooze) {
		t.Fatalf("err = %v, want a retry", err)
	}
	applied, pending := h.removalPeople(inst, "applied"), h.removalPeople(inst, "pending")
	if len(applied) != 1 || len(pending) != 1 {
		t.Fatalf("applied %v, pending %v; want one each", applied, pending)
	}
	if n := h.personJobs(applied[0]); n != 1 {
		t.Fatalf("%d provision_person jobs for the person whose removal was applied, want 1", n)
	}
	if n := h.personJobs(pending[0]); n != 0 {
		t.Fatalf("%d provision_person jobs for the person whose removal waits, want 0", n)
	}

	// The follow-up runs through the queue and completes the result.
	h.drain()
	byID := map[uuid.UUID]store.Person{alice.ID: alice, bob.ID: bob}
	if r := h.result(inst, byID[applied[0]]); r.Status != apps.StatusDone {
		t.Fatalf("result of the removed person = %s, want done", r.Status)
	}
	if r := h.result(inst, byID[pending[0]]); r.Status != apps.StatusQueued {
		t.Fatalf("result of the waiting person = %s, want queued", r.Status)
	}

	// The retry applies the rest.
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 2, 8); err != nil {
		t.Fatal(err)
	}
	h.drain()
	for _, p := range []store.Person{alice, bob} {
		if r := h.result(inst, p); r.Status != apps.StatusDone {
			t.Fatalf("%s result = %s, want done", p.Username, r.Status)
		}
		if acct, _ := h.tp.Account(inst.ID, p.Username); acct.Member {
			t.Fatalf("%s kept access", p.Username)
		}
	}
}

// TestStaleRemovalDropKeepsFollowUp: a removal dropped as stale queues its
// person in the same transaction, also when the run then fails.
func TestStaleRemovalDropKeepsFollowUp(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice, bob := h.person("alice"), h.person("bob")
	h.drain()
	h.planRemovals(inst, alice, bob)

	// Alice is reactivated before the guard runs: her removal is stale.
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusActive); err != nil {
		t.Fatal(err)
	}
	h.exec(`DELETE FROM river_job`)
	h.tp.Fail("apply:remove_from_org", retryable("unavailable"))
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8); err == nil {
		t.Fatal("expected a retry")
	}
	if got := h.removalPeople(inst, "dropped"); !slices.Equal(got, []uuid.UUID{alice.ID}) {
		t.Fatalf("dropped removals of %v, want alice", got)
	}
	if n := h.personJobs(alice.ID); n != 1 {
		t.Fatalf("%d provision_person jobs for alice, want 1", n)
	}
	h.drain()
	if r := h.result(inst, alice); r.Status != apps.StatusDone || !r.Access {
		t.Fatalf("alice result = %s (access %v), want done with access", r.Status, r.Access)
	}
	if acct, _ := h.tp.Account(inst.ID, "alice"); !acct.Member {
		t.Fatal("alice lost access")
	}
}

// TestRemovalRateLimitSnoozes: a removal's RetryAfter snoozes the guard
// without spending an attempt, also on the last one: nothing fails, the
// removal stays open, no further provider call is made in that run, and
// what applied before the rate limit keeps its follow-up.
func TestRemovalRateLimitSnoozes(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice, bob := h.person("alice"), h.person("bob")
	h.drain()
	h.planRemovals(inst, alice, bob)

	// Last attempt: the first call is rate limited.
	h.tp.Fail("apply:remove_from_org", rateLimited(time.Hour))
	before := h.removeCalls()
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 8, 8)
	wantSnooze(t, err, time.Hour)
	if n := h.removeCalls() - before; n != 1 {
		t.Fatalf("%d removal calls after a rate limit, want 1", n)
	}
	if got := h.removalPeople(inst, "pending"); len(got) != 2 {
		t.Fatalf("pending removals of %v, want both", got)
	}
	for _, p := range []store.Person{alice, bob} {
		if r := h.result(inst, p); r.Status != apps.StatusQueued {
			t.Fatalf("%s result = %s, want queued", p.Username, r.Status)
		}
		if acct, _ := h.tp.Account(inst.ID, p.Username); !acct.Member {
			t.Fatalf("%s was removed", p.Username)
		}
	}
	// The snoozed job runs again with the same attempt, after the reset.
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 8, 8); err != nil {
		t.Fatal(err)
	}
	h.drain()
	for _, p := range []store.Person{alice, bob} {
		if r := h.result(inst, p); r.Status != apps.StatusDone {
			t.Fatalf("%s result = %s, want done", p.Username, r.Status)
		}
		if acct, _ := h.tp.Account(inst.ID, p.Username); acct.Member {
			t.Fatalf("%s kept access", p.Username)
		}
	}

}

// TestRemovalRateLimitAfterProgress: a rate limit after a successful
// removal snoozes only once that removal and its follow-up are committed.
func TestRemovalRateLimitAfterProgress(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	carol, dave := h.person("carol"), h.person("dave")
	h.drain()
	h.planRemovals(inst, carol, dave)
	h.tp.Fail("apply:remove_from_org", nil, rateLimited(time.Minute))
	wantSnooze(t, h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8), time.Minute)
	applied := h.removalPeople(inst, "applied")
	var removed uuid.UUID
	for _, id := range applied {
		if id == carol.ID || id == dave.ID {
			removed = id
		}
	}
	if removed == uuid.Nil || h.personJobs(removed) != 1 {
		t.Fatalf("applied removals %v; the removed person's follow-up is missing", applied)
	}
}

// TestRetrySetupWhileFailedJobRuns: an admin's retry right after setup
// failed, while River still shows the failed attempt's job running, queues
// a job of its own.
func TestRetrySetupWhileFailedJobRuns(t *testing.T) {
	h := newHarness(t)
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "testmanaged"})
	if err != nil {
		t.Fatal(err)
	}
	h.managed.Fail("setup", permanent("bad_setup"))
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RetrySetup(h.ctx, audit.System, inst.ID); err != nil {
		t.Fatal(err)
	}
	// A worker takes the retry's job, and setup fails again: RunSetup has
	// committed setup_failed, River has not marked the job finished yet.
	h.exec(`UPDATE river_job SET state = 'running', attempted_at = now() WHERE kind = $1`, apps.SetupArgs{}.Kind())
	h.managed.Fail("setup", permanent("bad_setup"))
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	// The admin retries in that window.
	if _, err := h.svc.RetrySetup(h.ctx, audit.System, inst.ID); err != nil {
		t.Fatal(err)
	}
	h.exec(`DELETE FROM river_job WHERE kind = $1 AND state = 'running'`, apps.SetupArgs{}.Kind()) // River finishes the old job
	if n := h.jobs(apps.SetupArgs{}.Kind()); n != 1 {
		t.Fatalf("%d setup jobs after the retry, want 1", n)
	}
	h.drain()
	got, _ := h.svc.Instance(h.ctx, inst.ID)
	if got.State != apps.StateActive || h.managed.Setups(inst.ID) != 1 {
		t.Fatalf("after the retry: %s, %d setups", got.State, h.managed.Setups(inst.ID))
	}
}

// TestSetupRateLimitOnLastAttempt: a RetryAfter on the last attempt
// snoozes setup instead of failing the instance.
func TestSetupRateLimitOnLastAttempt(t *testing.T) {
	h := newHarness(t)
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "testmanaged"})
	if err != nil {
		t.Fatal(err)
	}
	h.managed.Fail("setup", rateLimited(time.Minute))
	wantSnooze(t, h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 8, 8), time.Minute)
	if got, _ := h.svc.Instance(h.ctx, inst.ID); got.State != apps.StateInstalling {
		t.Fatalf("state = %s, want installing", got.State)
	}
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 8, 8); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.svc.Instance(h.ctx, inst.ID); got.State != apps.StateActive {
		t.Fatalf("state = %s, want active", got.State)
	}
}

// TestRotationRateLimitSnoozes: a RetryAfter from the provider snoozes
// both the forward and the abandon attempt of a rotation.
func TestRotationRateLimitSnoozes(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	h.tp.Fail("rotate:webhook_secret", rateLimited(time.Minute))
	wantSnooze(t, h.rotate(inst, version), time.Minute)
	if s := h.credState(inst, version); s != "uncertain" {
		t.Fatalf("candidate = %s, want uncertain", s)
	}
	if err := h.rotate(inst, version); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, version); s != "active" {
		t.Fatalf("candidate = %s, want active", s)
	}

	// Abandoning re-applies the active value; a rate limit snoozes it.
	next, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	abandon := apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: next, Abandon: true}
	h.tp.Fail("rotate:webhook_secret", rateLimited(2*time.Minute))
	wantSnooze(t, h.svc.RunRotateCredential(h.ctx, abandon), 2*time.Minute)
	if s := h.credState(inst, next); s != "abandoning" {
		t.Fatalf("candidate = %s, want abandoning", s)
	}
	if err := h.svc.RunRotateCredential(h.ctx, abandon); err != nil {
		t.Fatal(err)
	}
	if s := h.credState(inst, next); s != "discarded" {
		t.Fatalf("candidate = %s, want discarded", s)
	}
}

// TestBrokenRemovalDoesNotBlockOthers: a removal that fails inside the
// framework (here an undecodable action) stops only its person; the
// people after it are removed and replanned, and the job retries.
func TestBrokenRemovalDoesNotBlockOthers(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice, bob := h.person("alice"), h.person("bob")
	h.drain()
	h.planRemovals(inst, alice, bob)
	// The first removal in apply order is broken.
	var broken uuid.UUID
	if err := h.pool.QueryRow(h.ctx, `UPDATE provisioning_removals SET action = '{"kind": 5}'
		WHERE id = (SELECT id FROM provisioning_removals WHERE instance_id = $1 AND state = 'pending' ORDER BY created_at, id LIMIT 1)
		RETURNING person_id`, inst.ID).Scan(&broken); err != nil {
		t.Fatal(err)
	}
	other := alice
	if broken == alice.ID {
		other = bob
	}
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8); err == nil {
		t.Fatal("expected a retry")
	}
	if got := h.removalPeople(inst, "applied"); !slices.Equal(got, []uuid.UUID{other.ID}) {
		t.Fatalf("applied removals of %v, want %s", got, other.Username)
	}
	if n := h.personJobs(other.ID); n != 1 {
		t.Fatalf("%d provision_person jobs for %s, want 1", n, other.Username)
	}
	if acct, _ := h.tp.Account(inst.ID, other.Username); acct.Member {
		t.Fatalf("%s kept access", other.Username)
	}
}
