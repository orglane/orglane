//go:build integration

package apps_test

import (
	"bytes"
	"context"
	"fmt"
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

// Round 2 of the review of PR #74.

// TestConfirmedBatchResumesAfterInactive: an instance that stops being
// provisionable while a confirmed batch applies leaves the batch confirmed
// with its remaining items; reactivation resumes them.
func TestConfirmedBatchResumesAfterInactive(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	names := []string{"alice", "bob", "carol", "dave"}
	for _, n := range names {
		h.person(n)
	}
	h.drain()
	for _, n := range names {
		p, _ := h.ids.GetPersonByUsername(h.ctx, n)
		if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, p.ID, identity.StatusSuspended); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: p.ID}, 1, 8); err != nil {
			t.Fatal(err)
		}
	}
	h.exec(`DELETE FROM river_job`)
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	held, err := h.svc.HeldRemovals(h.ctx, inst.ID)
	if err != nil || held == nil || held.Batch.ItemCount != 4 {
		t.Fatalf("held = %+v, %v", held, err)
	}
	if err := h.svc.ConfirmRemovals(h.ctx, audit.System, inst.ID, held.Batch.ID, held.Batch.ItemsHash); err != nil {
		t.Fatal(err)
	}
	h.exec(`DELETE FROM river_job`)
	reached, release := h.tp.Block("apply:remove_from_org")
	done := make(chan error, 1)
	go func() { done <- h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8) }()
	<-reached
	if _, err := h.svc.Transition(h.ctx, audit.System, inst.ID, apps.StateUpgrading, ""); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var state string
	if err := h.pool.QueryRow(h.ctx, `SELECT state FROM provisioning_removal_batches WHERE id = $1`, held.Batch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if left := h.count(`SELECT count(*) FROM provisioning_removals WHERE batch_id = $1 AND state = 'held'`, held.Batch.ID); state != "confirmed" || left != 3 {
		t.Fatalf("interrupted batch = %s with %d held items, want confirmed with 3", state, left)
	}
	if _, err := h.svc.Transition(h.ctx, audit.System, inst.ID, apps.StateActive, ""); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if left := h.count(`SELECT count(*) FROM provisioning_removals WHERE batch_id = $1 AND state = 'held'`, held.Batch.ID); left != 0 {
		t.Fatalf("%d confirmed removals orphaned after reactivation", left)
	}
	for _, n := range names {
		if acct, _ := h.tp.Account(inst.ID, n); acct.Member {
			t.Errorf("suspended %s is still a member", n)
		}
	}
	if err := h.pool.QueryRow(h.ctx, `SELECT state FROM provisioning_removal_batches WHERE id = $1`, held.Batch.ID).Scan(&state); err != nil || state != "applied" {
		t.Fatalf("batch = %s, %v, want applied", state, err)
	}
}

// TestGrantSurvivesFailedActionRecord: the grant intent is durable before
// the provider is called, so a failure of the action's bookkeeping (audit
// entry, link) after the provider created the account keeps the account
// Orglane's; and if even the intent cannot be written, nothing is changed
// at the provider and the retry converges.
func TestGrantSurvivesFailedActionRecord(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.drain()
	// The action's bookkeeping fails: storing the provisioned link aborts
	// that transaction after the provider created the account.
	h.exec(`CREATE FUNCTION review_fail_link() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected failure of the action record'; END $$`)
	h.exec(`CREATE TRIGGER review_fail_link BEFORE INSERT ON account_links FOR EACH ROW EXECUTE FUNCTION review_fail_link()`)
	alice := h.person("alice")
	a := apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}
	if err := h.svc.RunProvisionPerson(h.ctx, a, 1, 8); err == nil {
		t.Fatal("the injected failure did not happen")
	}
	h.exec(`DROP TRIGGER review_fail_link ON account_links`)
	if acct, ok := h.tp.Account(inst.ID, "alice"); !ok || !acct.Member {
		t.Fatal("the account was not created")
	}
	if err := h.svc.RunProvisionPerson(h.ctx, a, 2, 8); err != nil {
		t.Fatal(err)
	}
	if r := h.result(inst, alice); !r.GrantedByOrglane || r.Status != apps.StatusDone || r.PendingGrant != nil {
		t.Fatalf("ownership of the created account was lost: %+v", r)
	}
	// The retry restores the bookkeeping of the first successful path: the
	// verified link and its audit entry, and the action's audit entry.
	if n := h.count(`SELECT count(*) FROM account_links WHERE person_id = $1 AND method = 'provisioned' AND state = 'verified'`, alice.ID); n != 1 {
		t.Fatalf("%d verified provisioned links after the retry, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = 'account_link.verified' AND metadata->>'person_id' = $1`, alice.ID.String()); n != 1 {
		t.Fatalf("%d account_link.verified entries, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = 'provisioning.action_applied' AND target_id = $1 AND metadata->>'kind' = 'create_account'`, alice.ID.String()); n != 1 {
		t.Fatalf("%d action entries for the account creation, want 1", n)
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "alice"); acct.Member {
		t.Fatal("an account Orglane created was kept after offboarding")
	}

	// Every result write fails: the intent cannot be recorded, so the
	// provider is not touched; the retry creates and owns the account.
	h.exec(`CREATE FUNCTION review_fail_result() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected failure of result writes'; END $$`)
	h.exec(`CREATE TRIGGER review_fail_result BEFORE INSERT OR UPDATE ON provisioning_results FOR EACH ROW EXECUTE FUNCTION review_fail_result()`)
	bob := h.person("bob")
	b := apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: bob.ID}
	if err := h.svc.RunProvisionPerson(h.ctx, b, 1, 8); err == nil {
		t.Fatal("the injected failure did not happen")
	}
	if _, ok := h.tp.Account(inst.ID, "bob"); ok {
		t.Fatal("the provider changed although the grant intent could not be recorded")
	}
	h.exec(`DROP TRIGGER review_fail_result ON provisioning_results`)
	if err := h.svc.RunProvisionPerson(h.ctx, b, 2, 8); err != nil {
		t.Fatal(err)
	}
	if r := h.result(inst, bob); !r.GrantedByOrglane || r.Status != apps.StatusDone {
		t.Fatalf("bob = %+v", r)
	}
}

// TestRemovalFailureStopsPersonActions: a person's removals stop at their
// first failure (the organization removal after a failed team removal does
// not run), while other people's removals go on.
func TestRemovalFailureStopsPersonActions(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice, bob := h.person("alice"), h.person("bob")
	team := h.team("old", alice) // alice: two removals, bob: one; three fit the limit
	h.mapTeam(inst, team)
	h.drain()
	for _, id := range [][16]byte{alice.ID, bob.ID} {
		if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, id, identity.StatusSuspended); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: id}, 1, 8); err != nil {
			t.Fatal(err)
		}
	}
	h.exec(`DELETE FROM river_job`)
	h.tp.Fail("apply:remove_from_team:team-old", permanent("owner_protected"))
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	first, _ := h.ids.GetPersonByUsername(h.ctx, "alice")
	if acct, _ := h.tp.Account(inst.ID, "alice"); !acct.Member {
		t.Fatal("remove_from_org ran after alice's remove_from_team failed")
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state IN ('pending', 'held')`, first.ID); n != 0 {
		t.Fatalf("alice has %d open removals after her failure", n)
	}
	if r := h.result(inst, alice); r.Status != apps.StatusFailed {
		t.Fatalf("alice = %+v", r)
	}
	if acct, _ := h.tp.Account(inst.ID, "bob"); acct.Member {
		t.Fatal("bob's removal did not go on independently in the same run")
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "alice"); !acct.Member {
		t.Fatal("alice's later removal ran in a later job")
	}
}

// TestPersonDriftNotSwallowed: a drift check for a person whose hashes
// converged plans again even when an ordinary job for them is queued.
func TestPersonDriftNotSwallowed(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice := h.person("alice")
	h.drain()
	// Converge: the second run sees the grant, so applied and desired
	// hashes match and ordinary runs take the shortcut.
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if r := h.result(inst, alice); !bytes.Equal(r.AppliedHash, r.DesiredHash) {
		t.Fatal("the result did not converge")
	}
	acct, _ := h.tp.Account(inst.ID, "alice")
	// Someone removes alice in the provider; then an unrelated edit queues
	// an ordinary job, which takes the applied-hash shortcut.
	if _, err := h.tp.Apply(h.ctx, h.svc.EnvForTest(inst), inst.View(), provider.Action{Kind: provider.ActionRemoveFromOrg, Key: "external", Params: map[string]string{"account": acct.ID}}); err != nil {
		t.Fatal(err)
	}
	title := "New title"
	if _, err := h.ids.UpdatePerson(h.ctx, audit.System, alice.ID, identity.PersonUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionInstance(h.ctx, apps.ProvisionInstanceArgs{InstanceID: inst.ID, Reason: apps.ReasonDrift}); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "alice"); !acct.Member {
		t.Fatal("the drift check was swallowed: alice has no account")
	}
}

// Round 3 of the review of PR #74.

// TestRejectedGrantDoesNotAdoptExistingMember: a team grant the provider
// definitely rejects does not make an existing, unmanaged member Orglane's,
// so suspending them leaves their membership alone.
func TestRejectedGrantDoesNotAdoptExistingMember(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	acct := h.tp.AddMember(inst.ID, "alice")
	alice := h.person("alice")
	h.exec(`INSERT INTO account_links (person_id, provider, realm, external_id, login, method) VALUES ($1, 'testprovider', $2, $3, 'alice', 'oauth')`, alice.ID, inst.ID.String(), acct.ID)
	h.drain()
	if r := h.result(inst, alice); r.GrantedByOrglane {
		t.Fatal("the existing member is managed before any grant")
	}
	team := h.team("platform", alice)
	h.mapTeam(inst, team)
	if err := h.svc.RunProvisionInstance(h.ctx, apps.ProvisionInstanceArgs{InstanceID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	h.exec(`DELETE FROM river_job`)
	h.tp.Fail("apply:add_to_team:team-platform", permanent("seat_limit"))
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if r := h.result(inst, alice); r.GrantedByOrglane || r.PendingGrant != nil {
		t.Fatalf("a rejected grant adopted the member: granted=%v pending=%s", r.GrantedByOrglane, r.PendingGrant)
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "alice"); !acct.Member {
		t.Fatal("an existing member Orglane never granted access was removed")
	}
}

// TestUncertainGrantResolvedByReplay: a grant whose answer was lost keeps
// its intent; the next run replays it as an ensure and records ownership,
// so an account Orglane created this way is removed on offboarding.
func TestUncertainGrantResolvedByReplay(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.drain()
	alice := h.person("alice")
	h.exec(`DELETE FROM river_job`)
	// The provider creates the account; the job dies before recording it
	// (simulated: the answer is an unknown error after the change).
	reached, release := h.tp.Block("apply:create_account")
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(h.ctx)
	go func() {
		done <- h.svc.RunProvisionPerson(ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 1, 8)
	}()
	<-reached
	cancel() // the worker goes away while the provider works
	close(release)
	if err := <-done; err == nil {
		t.Fatal("the interrupted run reported success")
	}
	if acct, ok := h.tp.Account(inst.ID, "alice"); !ok || !acct.Member {
		t.Fatal("the provider did not create the account")
	}
	if r := h.result(inst, alice); r.PendingGrant == nil || r.GrantedByOrglane {
		t.Fatalf("intent = %s, granted = %v; want pending intent, no ownership yet", r.PendingGrant, r.GrantedByOrglane)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 2, 8); err != nil {
		t.Fatal(err)
	}
	r := h.result(inst, alice)
	if !r.GrantedByOrglane || r.PendingGrant != nil || r.Status != apps.StatusDone {
		t.Fatalf("after replay: %+v", r)
	}
	if n := h.count(`SELECT count(*) FROM account_links WHERE person_id = $1 AND method = 'provisioned' AND state = 'verified'`, alice.ID); n != 1 {
		t.Fatalf("%d provisioned links, want 1", n)
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "alice"); acct.Member {
		t.Fatal("an account Orglane created was kept after offboarding")
	}
}

// Round 4 of the review of PR #74.

// TestReplayWithoutProofIsNotAdopted: an interrupted grant whose replay
// finds an account created independently (no provider proof of Orglane's
// call) is neither adopted, verified nor removed; it waits for an admin as
// ownership_unproven and appears in the unmanaged accounts report.
func TestReplayWithoutProofIsNotAdopted(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		name := "active"
		if suspended {
			name = "suspended"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			inst := h.instance(nil)
			h.drain()
			alice := h.person("alice")
			h.exec(`DELETE FROM river_job`)
			a := apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}
			h.tp.Fail("apply:create_account", retryable("connection_refused")) // before any provider change
			if err := h.svc.RunProvisionPerson(h.ctx, a, 1, 8); err == nil {
				t.Fatal("expected the first attempt to fail")
			}
			external := h.tp.AddMember(inst.ID, "alice") // someone else creates the account
			if suspended {
				if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusSuspended); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.svc.RunProvisionPerson(h.ctx, a, 2, 8); err != nil {
				t.Fatal(err)
			}
			r := h.result(inst, alice)
			if r.GrantedByOrglane || r.PendingGrant != nil {
				t.Fatalf("granted=%v pending=%s: an account without proof was adopted or left pending", r.GrantedByOrglane, r.PendingGrant)
			}
			if r.Status != apps.StatusFailed || r.ErrorCode == nil || *r.ErrorCode != apps.CodeOwnershipUnproven {
				t.Fatalf("result = %s %v, want failed %s (admin review)", r.Status, r.ErrorCode, apps.CodeOwnershipUnproven)
			}
			if n := h.count(`SELECT count(*) FROM account_links WHERE person_id = $1 AND method = 'provisioned'`, alice.ID); n != 0 {
				t.Fatal("an independently created account was verified as provisioned")
			}
			if !suspended {
				unmanaged, err := h.svc.UnmanagedAccounts(h.ctx, inst.ID)
				if err != nil || len(unmanaged) != 1 || unmanaged[0].ExternalID != external.ID {
					t.Fatalf("unmanaged = %+v, %v", unmanaged, err)
				}
			}
			// Nothing runs it again on its own, and nothing removes it.
			if err := h.svc.RunDriftCheck(h.ctx); err != nil {
				t.Fatal(err)
			}
			h.drain()
			if acct, _ := h.tp.Account(inst.ID, "alice"); !acct.Member {
				t.Fatal("an account Orglane never proved it created was removed")
			}
			if r := h.result(inst, alice); r.Status != apps.StatusFailed {
				t.Fatalf("the review state was overwritten: %+v", r)
			}
		})
	}
}

// TestReplayFailurePolicy: a failing replay goes through the same failure
// handling as any action: attempts count up to exhaustion (and Retry Failed
// then selects the person), and a permanent rejection fails the person and
// stops the run.
func TestReplayFailurePolicy(t *testing.T) {
	t.Run("exhaustion", func(t *testing.T) {
		h := newHarness(t)
		inst := h.instance(nil)
		h.drain()
		alice := h.person("alice")
		for range 8 {
			h.tp.Fail("apply:create_account", retryable("unavailable"))
		}
		h.drain()
		r := h.result(inst, alice)
		if r.Status != apps.StatusFailed || r.Attempts != 8 {
			t.Fatalf("after 8 failed attempts: status %s, attempts %d", r.Status, r.Attempts)
		}
		if n, err := h.svc.RetryFailed(h.ctx, audit.System, inst.ID, nil); err != nil || n != 1 {
			t.Fatalf("Retry Failed selected %d, %v", n, err)
		}
		h.drain()
		if r := h.result(inst, alice); r.Status != apps.StatusDone || !r.GrantedByOrglane {
			t.Fatalf("after the retry: %+v", r)
		}
	})
	t.Run("permanent", func(t *testing.T) {
		h := newHarness(t)
		inst := h.instance(nil)
		h.drain()
		alice := h.person("alice")
		h.exec(`DELETE FROM river_job`)
		a := apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}
		h.tp.Fail("apply:create_account", retryable("unavailable"))
		if err := h.svc.RunProvisionPerson(h.ctx, a, 1, 8); err == nil {
			t.Fatal("expected the first attempt to fail")
		}
		h.tp.Fail("apply:create_account", permanent("seat_limit"))
		if err := h.svc.RunProvisionPerson(h.ctx, a, 2, 8); err != nil {
			t.Fatal(err)
		}
		r := h.result(inst, alice)
		if r.Status != apps.StatusFailed || r.ErrorCode == nil || *r.ErrorCode != "seat_limit" || r.PendingGrant != nil {
			t.Fatalf("result = %+v", r)
		}
		if _, ok := h.tp.Account(inst.ID, "alice"); ok {
			t.Fatal("the run created the account after the replay was rejected")
		}
	})
}

// Review threads (CodeRabbit) on 8c62436.

// TestTeamGrantDoesNotManageExistingAccount: adding an existing,
// unmanaged account to a mapped team (with proof of the team addition) does
// not make the account Orglane's, so suspending the person never removes
// it from the organization.
func TestTeamGrantDoesNotManageExistingAccount(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	acct := h.tp.AddMember(inst.ID, "alice")
	alice := h.person("alice")
	h.exec(`INSERT INTO account_links (person_id, provider, realm, external_id, login, method) VALUES ($1, 'testprovider', $2, $3, 'alice', 'oauth')`, alice.ID, inst.ID.String(), acct.ID)
	team := h.team("platform", alice)
	h.mapTeam(inst, team)
	h.drain()
	if got, _ := h.tp.Account(inst.ID, "alice"); !got.Teams["team-platform"] {
		t.Fatal("the team addition did not happen")
	}
	if r := h.result(inst, alice); r.GrantedByOrglane {
		t.Fatal("a team addition made an existing account Orglane's")
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if got, _ := h.tp.Account(inst.ID, "alice"); !got.Member {
		t.Fatal("an existing account was removed from the organization")
	}
}

// TestAbandonWithoutActiveVersion: abandoning the first value of a
// credential (no active version to go back to) discards the candidate
// instead of failing forever.
func TestAbandonWithoutActiveVersion(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.tp.LoseResponses("api_token", 1)
	version, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "api_token")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "api_token", Version: version}); err == nil {
		t.Fatal("expected an unknown outcome")
	}
	if err := h.svc.AbandonRotation(h.ctx, audit.System, inst.ID, "api_token"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "api_token", Version: version, Abandon: true}); err != nil {
		t.Fatalf("abandon without an active version = %v", err)
	}
	var state string
	if err := h.pool.QueryRow(h.ctx, `SELECT state FROM app_credentials WHERE instance_id = $1 AND name = 'api_token' AND version = $2`, inst.ID, version).Scan(&state); err != nil || state != "discarded" {
		t.Fatalf("candidate = %s, %v", state, err)
	}
	if _, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "api_token"); err != nil {
		t.Fatalf("a new rotation is blocked: %v", err)
	}
}

// TestStuckReportedOnceUnderConcurrency: concurrent reporters record one
// rotation_stuck entry per candidate.
func TestStuckReportedOnceUnderConcurrency(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.tp.LoseResponses("webhook_secret", 100)
	v, err := h.svc.RotateCredential(h.ctx, audit.System, inst.ID, "webhook_secret")
	if err != nil {
		t.Fatal(err)
	}
	_ = h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "webhook_secret", Version: v})
	h.exec(`UPDATE app_credentials SET created_at = now() - interval '25 hours' WHERE state = 'uncertain'`)
	errs := make(chan error, 8)
	for range 8 {
		go func() { _, err := h.svc.Cleanup(h.ctx); errs <- err }()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if n := h.auditCount(audit.AppCredentialRotationStuck); n != 1 {
		t.Fatalf("rotation_stuck recorded %d times, want 1", n)
	}
}

// TestUndecodableDeliveryFails: a stored delivery whose body or headers
// cannot be decoded ends failed with a code, not stuck as received.
func TestUndecodableDeliveryFails(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", fixtureSecret); err != nil {
		t.Fatal(err)
	}
	h.drain()
	for i, c := range []struct{ damage, code string }{
		{`UPDATE webhook_deliveries SET body = '\x0102' WHERE delivery_id = $1`, "body_unreadable"},
		{`UPDATE webhook_deliveries SET headers = '{"X-Forgejo-Event": 1}' WHERE delivery_id = $1`, "headers_unreadable"},
	} {
		id := fmt.Sprintf("bad-%d", i)
		body := prBody("acme", 700+i)
		if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, body, id, "pull_request"), body); err != nil {
			t.Fatal(err)
		}
		h.exec(c.damage, id)
		h.drain()
		var status, code string
		if err := h.pool.QueryRow(h.ctx, `SELECT status, coalesce(error_code, '') FROM webhook_deliveries WHERE delivery_id = $1`, id).Scan(&status, &code); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || code != c.code {
			t.Errorf("%s: %s/%s, want failed/%s", id, status, code, c.code)
		}
	}
}
