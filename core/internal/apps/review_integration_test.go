//go:build integration

package apps_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/providers/provider"
)

// Regression tests for the review of PR #74 (each fails without its fix).

// TestRotationAbandonRace: a forward attempt paused in the provider call
// while the admin abandons must not leave the provider on the candidate.
// The abandon intent is persisted and both jobs are serialized per
// credential, so the abandon job re-applies the active value afterwards.
func TestRotationAbandonRace(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	old := h.secret(inst, "webhook_secret")
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	reached, release := h.tp.Block("rotate:webhook_secret")
	forward := make(chan error, 1)
	go func() {
		forward <- h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version})
	}()
	<-reached
	if err := h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "webhook_secret"); err != nil {
		t.Fatal(err)
	}
	abandon := make(chan error, 1)
	go func() {
		abandon <- h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: version, Abandon: true})
	}()
	time.Sleep(500 * time.Millisecond) // an unserialized abandon finishes here
	close(release)
	if err := <-forward; err != nil {
		t.Fatal(err)
	}
	if err := <-abandon; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.tp.Secret(inst.ID, "webhook_secret"), old) {
		t.Fatal("the provider uses the abandoned candidate")
	}
	if !bytes.Equal(h.secret(inst, "webhook_secret"), old) {
		t.Fatal("the active credential changed")
	}
	if s := h.credState(inst, version); s != "discarded" {
		t.Fatalf("candidate = %s, want discarded", s)
	}
}

// TestStaleRemovalNotApplied: a removal whose person regained access while
// the guard applied an earlier item is rechecked under the person's lock
// and not applied.
func TestStaleRemovalNotApplied(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice, bob := h.person("alice"), h.person("bob")
	h.drain()
	for _, p := range []string{"alice", "bob"} {
		if acct, ok := h.tp.Account(inst.ID, p); !ok || !acct.Member {
			t.Fatalf("%s not provisioned", p)
		}
	}
	for _, id := range [][16]byte{alice.ID, bob.ID} {
		if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, id, identity.StatusSuspended); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: id}, 1, 8); err != nil {
			t.Fatal(err)
		}
	}
	h.exec(`DELETE FROM river_job`)
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE state = 'pending'`); n != 2 {
		t.Fatalf("%d pending removals, want 2", n)
	}
	reached, release := h.tp.Block("apply:remove_from_org") // alice's comes first
	done := make(chan error, 1)
	go func() { done <- h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8) }()
	<-reached
	// Bob comes back while alice's removal is in flight; his own job drops
	// his pending removal.
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, bob.ID, identity.StatusActive); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: bob.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if acct, _ := h.tp.Account(inst.ID, "alice"); acct.Member {
		t.Fatal("alice's removal did not apply")
	}
	if acct, _ := h.tp.Account(inst.ID, "bob"); !acct.Member {
		t.Fatal("a stale removal took bob's access")
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, bob.ID); n != 0 {
		t.Fatal("bob's dropped removal is marked applied")
	}
}

// TestNoRemovalsAfterFailedAction: moving a person to another team, the
// failed addition stops the plan; the removal of the old team is neither
// queued nor applied.
func TestNoRemovalsAfterFailedAction(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	policy := provider.MappedTeams
	if _, err := h.svc.UpdateInstance(h.ctx, audit.System, inst.ID, apps.InstanceUpdate{AccessPolicy: &policy}); err != nil {
		t.Fatal(err)
	}
	pat := h.person("pat")
	old, next := h.team("old", pat), h.team("next")
	h.mapTeam(inst, old)
	h.mapTeam(inst, next)
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "pat"); !acct.Teams["team-old"] {
		t.Fatalf("pat = %+v", acct)
	}
	if err := h.ids.RemoveTeamMember(h.ctx, audit.System, old.ID, pat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ids.SetTeamMember(h.ctx, audit.System, next.ID, pat.ID, identity.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	h.exec(`DELETE FROM river_job`)
	h.tp.Fail("apply:add_to_team:team-next", retryable("timeout"))
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: pat.ID}, 1, 8); err == nil {
		t.Fatal("expected a retryable failure")
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1`, pat.ID); n != 0 {
		t.Fatalf("%d removals queued after a failed action", n)
	}
	acts := h.actions(h.result(inst, pat))
	if len(acts) != 2 || acts[0].Outcome != apps.OutcomeFailed || acts[1].Key != "remove_from_team:team-old" || acts[1].Outcome != apps.OutcomeNotAttempted {
		t.Fatalf("actions = %+v", acts)
	}
	// The retry moves pat: first the addition, then the removal.
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: pat.ID}, 2, 8); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "pat"); !acct.Teams["team-next"] || acct.Teams["team-old"] {
		t.Fatalf("pat after retry = %+v", acct)
	}
}

// TestGrantSurvivesFailedResultWrite: ownership of an account Orglane
// created is recorded with the action, so a failure of the final result
// write cannot make the account unmanaged (and unremovable).
func TestGrantSurvivesFailedResultWrite(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.drain()
	h.exec(`CREATE SEQUENCE review_fail_once`)
	h.exec(`CREATE FUNCTION review_fail_done() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.status = 'done' THEN
				IF nextval('review_fail_once') = 1 THEN
					RAISE EXCEPTION 'injected failure of the result write';
				END IF;
			END IF;
			RETURN NEW;
		END $$`)
	h.exec(`CREATE TRIGGER review_fail_done BEFORE INSERT OR UPDATE ON provisioning_results FOR EACH ROW EXECUTE FUNCTION review_fail_done()`)
	alice := h.person("alice")
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 1, 8); err == nil {
		t.Fatal("the injected failure did not happen")
	}
	if acct, ok := h.tp.Account(inst.ID, "alice"); !ok || !acct.Member {
		t.Fatal("the account was not created")
	}
	h.drain() // the retry plans nothing: the account exists
	if r := h.result(inst, alice); !r.GrantedByOrglane || r.Status != apps.StatusDone {
		t.Fatalf("result after the retry = %+v", r)
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "alice"); acct.Member {
		t.Fatal("an account Orglane created was kept after offboarding")
	}
}

// TestRemovalFailuresSurface: a permanent removal failure fails the
// person's result with its code and is not retried automatically (not by
// a follow-up, not by the drift check) until an admin retries; exhausted
// retryable failures do the same.
func TestRemovalFailuresSurface(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	carol, dave := h.person("carol"), h.person("dave")
	h.drain()

	h.tp.Fail("apply:remove_from_org", permanent("owner_protected"))
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, carol.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	check := func(p string, code string) {
		t.Helper()
		person, _ := h.ids.GetPersonByUsername(h.ctx, p)
		r := h.result(inst, person)
		if r.Status != apps.StatusFailed || r.ErrorCode == nil || *r.ErrorCode != code {
			t.Fatalf("%s result = %s %v, want failed %s", p, r.Status, r.ErrorCode, code)
		}
		acts := h.actions(r)
		if len(acts) == 0 || acts[len(acts)-1].Key != "remove_from_org" || acts[len(acts)-1].Outcome != apps.OutcomeFailed || acts[len(acts)-1].Error != code {
			t.Fatalf("%s actions = %+v", p, acts)
		}
		if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1`, person.ID); n != 1 {
			t.Fatalf("%s has %d removal rows, want 1 (no automatic retry)", p, n)
		}
		if acct, _ := h.tp.Account(inst.ID, p); !acct.Member {
			t.Fatalf("%s was removed", p)
		}
	}
	check("carol", "owner_protected")
	// The daily drift check does not retry it either.
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	check("carol", "owner_protected")

	// Retryable failures that exhaust the job's attempts surface the same way.
	for range 8 {
		h.tp.Fail("apply:remove_from_org", retryable("unavailable"))
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, dave.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	check("dave", "unavailable")

	// Retry Failed selects both; this time the provider accepts.
	n, err := h.svc.RetryFailed(h.ctx, audit.System, inst.ID, nil)
	if err != nil || n != 2 {
		t.Fatalf("retry = %d, %v", n, err)
	}
	h.drain()
	for _, p := range []string{"carol", "dave"} {
		if acct, _ := h.tp.Account(inst.ID, p); acct.Member {
			t.Fatalf("%s kept access after the retry", p)
		}
	}
	if r := h.result(inst, carol); r.Status != apps.StatusDone {
		t.Fatalf("carol after retry = %+v", r)
	}
}
