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
	"github.com/orglane/orglane/core/internal/onboarding"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
)

// TestProvisionAllPeople: activating an all_people instance provisions
// every active person, creates verified "provisioned" links, audits each
// change, and a rerun changes nothing.
func TestProvisionAllPeople(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.person("alice"), h.person("bob")
	invited, err := h.ids.CreatePerson(h.ctx, audit.System, identity.NewPerson{Username: "ivy", WorkEmail: "ivy@acme.test", DisplayName: "Ivy"})
	if err != nil {
		t.Fatal(err)
	}
	inst := h.instance(nil)
	h.drain()

	for _, p := range []store.Person{alice, bob} {
		r := h.result(inst, p)
		if r.Status != apps.StatusDone || !r.Access || !r.GrantedByOrglane || r.AppliedHash == nil {
			t.Fatalf("%s: result %+v", p.Username, r)
		}
		acct, ok := h.tp.Account(inst.ID, p.Username)
		if !ok || !acct.Member {
			t.Fatalf("%s has no account", p.Username)
		}
		link, err := store.New(h.pool).GetVerifiedAccountLink(h.ctx, store.GetVerifiedAccountLinkParams{PersonID: p.ID, Provider: "testprovider", Realm: inst.ID.String()})
		if err != nil || link.Method != provider.LinkProvisioned || link.ExternalID != acct.ID || link.InstanceID == nil || *link.InstanceID != inst.ID {
			t.Fatalf("%s link = %+v, %v", p.Username, link, err)
		}
	}
	if h.count(`SELECT count(*) FROM provisioning_results WHERE person_id = $1`, invited.ID) != 0 {
		t.Fatal("an invited person without access got a result row")
	}
	if _, ok := h.tp.Account(inst.ID, "ivy"); ok {
		t.Fatal("an invited person got an account")
	}
	applied, links := h.auditCount(audit.ProvisioningActionApplied), h.auditCount(audit.AccountLinkVerified)
	if applied != 2 || links != 2 {
		t.Fatalf("audit: %d actions applied, %d links verified, want 2 and 2", applied, links)
	}

	// Idempotent rerun: a drift check plans again, nothing changes.
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if h.auditCount(audit.ProvisioningActionApplied) != applied || h.auditCount(audit.AccountLinkVerified) != links {
		t.Fatal("a rerun changed provider state")
	}
	r := h.result(inst, alice)
	if r.Status != apps.StatusDone || r.ErrorCode != nil {
		t.Fatalf("after rerun: %+v", r)
	}
	// Inviting someone who then accepts gives them access (observer path).
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, invited.ID, identity.StatusActive); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if _, ok := h.tp.Account(inst.ID, "ivy"); !ok {
		t.Fatal("an activated person got no account")
	}
}

// TestRemovalPendingWaitsForTheProvider: a provider that accepted a removal
// but has not finished it (GitHub's queued removal) keeps the guard's removal
// pending, its audit entry unwritten and the person queued; the retry applies
// and audits it once the provider confirms.
func TestRemovalPendingWaitsForTheProvider(t *testing.T) {
	h := newHarness(t)
	alice := h.person("alice")
	inst := h.instance(nil)
	h.drain()
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	h.tp.Fail("apply:remove_from_org", &provider.Error{Code: provider.CodeRemovalPending, Retryable: true, RetryAfter: 5 * time.Second})
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("pending removal = %v, want a 5s snooze", err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, alice.ID); n != 1 {
		t.Fatalf("pending removals = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending' AND error_code = $2`, alice.ID, provider.CodeRemovalPending); n != 1 {
		t.Fatalf("removals marked accepted = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'key' = 'remove_from_org'`, string(audit.ProvisioningActionApplied)); n != 0 {
		t.Fatalf("audit entries for the unconfirmed removal = %d, want 0", n)
	}
	if r := h.result(inst, alice); r.Status != apps.StatusQueued {
		t.Fatalf("result while the removal is pending = %+v, want queued", r)
	}

	// GitHub finishes the removal between attempts: the confirming run
	// itself changes nothing (the provider reports no change), but the
	// earlier accepted removal is audited now.
	h.tp.RemoveMember(inst.ID, "alice")
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 2, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, alice.ID); n != 1 {
		t.Fatalf("applied removals = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'key' = 'remove_from_org'`, string(audit.ProvisioningActionApplied)); n != 1 {
		t.Fatalf("audit entries for the confirmed removal = %d, want 1", n)
	}
}

// TestRemovalPendingDoesNotBlockOtherPeople: one person's accepted-but-
// unconfirmed removal snoozes the job for its next check without stopping
// another person's removal in the same run.
func TestRemovalPendingDoesNotBlockOtherPeople(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.person("alice"), h.person("bob")
	inst := h.instance(nil)
	h.drain()
	for _, p := range []store.Person{alice, bob} {
		if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, p.ID, identity.StatusSuspended); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: p.ID}, 1, 8); err != nil {
			t.Fatal(err)
		}
	}
	h.tp.Fail("apply:remove_from_org", &provider.Error{Code: provider.CodeRemovalPending, Retryable: true, RetryAfter: 5 * time.Second})
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("pending removal = %v, want a 5s snooze", err)
	}
	// The faulted removal is pending; the other person's removal still ran.
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE state = 'pending'`); n != 1 {
		t.Fatalf("pending removals = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE state = 'applied'`); n != 1 {
		t.Fatalf("applied removals = %d, want 1", n)
	}
	var faulted uuid.UUID
	if err := h.pool.QueryRow(h.ctx, `SELECT person_id FROM provisioning_removals WHERE state = 'pending'`).Scan(&faulted); err != nil {
		t.Fatal(err)
	}
	login := alice.Username
	if faulted == bob.ID {
		login = bob.Username
	}
	// GitHub finishes the accepted removal; the next run confirms both.
	h.tp.RemoveMember(inst.ID, login)
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 2, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE state = 'applied'`); n != 2 {
		t.Fatalf("applied removals = %d, want 2", n)
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'key' = 'remove_from_org'`, string(audit.ProvisioningActionApplied)); n != 2 {
		t.Fatalf("audit entries = %d, want 2", n)
	}
}

// TestProvisionMappedTeams: mapped_teams instances give access through
// mapped teams only; membership changes queue provisioning in the same
// transaction; a small removal applies without confirmation.
func TestProvisionMappedTeams(t *testing.T) {
	h := newHarness(t)
	alice, bob, carol := h.person("alice"), h.person("bob"), h.person("carol")
	inst := h.instance(nil)
	policy := provider.MappedTeams
	if _, err := h.svc.UpdateInstance(h.ctx, audit.System, inst.ID, apps.InstanceUpdate{AccessPolicy: &policy}); err != nil {
		t.Fatal(err)
	}
	platform := h.team("platform", alice)
	h.mapTeam(inst, platform)
	h.drain()

	acct, ok := h.tp.Account(inst.ID, "alice")
	if !ok || !acct.Teams["team-platform"] {
		t.Fatalf("alice = %+v", acct)
	}
	for _, p := range []string{"bob", "carol"} {
		if _, ok := h.tp.Account(inst.ID, p); ok {
			t.Fatalf("%s got an account without a mapped team", p)
		}
	}
	mappings, err := h.svc.TeamMappings(h.ctx, inst.ID)
	if err != nil || len(mappings) != 1 || mappings[0].ExternalRef == nil || *mappings[0].ExternalRef != "team-platform" {
		t.Fatalf("mappings = %+v, %v", mappings, err)
	}
	// Plan order: create the account, then add it to the team.
	calls := h.tp.Calls()
	ia, it := slices.Index(calls, "apply:create_account"), slices.Index(calls, "apply:add_to_team:team-platform")
	if ia < 0 || it < ia {
		t.Fatalf("calls = %v", calls)
	}

	// Joining the team grants access.
	if _, err := h.ids.SetTeamMember(h.ctx, audit.System, platform.ID, bob.ID, identity.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	if h.jobs(apps.ProvisionPersonArgs{}.Kind()) != 1 {
		t.Fatal("joining a mapped team did not queue provisioning")
	}
	h.drain()
	if acct, ok := h.tp.Account(inst.ID, "bob"); !ok || !acct.Teams["team-platform"] {
		t.Fatalf("bob = %+v", acct)
	}

	// Leaving it takes access away through the guard: two removals, within
	// the limit of 3, apply without confirmation.
	if err := h.ids.RemoveTeamMember(h.ctx, audit.System, platform.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "bob"); acct.Member || len(acct.Teams) != 0 {
		t.Fatalf("bob kept access: %+v", acct)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, bob.ID); n != 2 {
		t.Fatalf("applied removals = %d, want 2", n)
	}
	r := h.result(inst, bob)
	if r.Access || r.Status != apps.StatusDone {
		t.Fatalf("bob result = %+v", r)
	}
	_ = carol
}

// TestProvisionPartialFailureAndRetry: actions run in plan order and stop
// at the first failure; retryable errors retry and converge; permanent
// errors fail at once until an admin retries.
func TestProvisionPartialFailureAndRetry(t *testing.T) {
	h := newHarness(t)
	alice := h.person("alice")
	inst := h.instance(nil)
	team := h.team("platform", alice)
	h.tp.Fail("apply:add_to_team:team-platform", retryable("timeout"))
	h.mapTeam(inst, team)

	// Work the jobs until the failed attempt and look at the result.
	if err := h.svc.RunProvisionInstance(h.ctx, apps.ProvisionInstanceArgs{InstanceID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 1, 8)
	if err == nil {
		t.Fatal("a retryable failure did not ask for a retry")
	}
	r := h.result(inst, alice)
	acts := h.actions(r)
	if r.Status != apps.StatusQueued || r.ErrorCode == nil || *r.ErrorCode != "timeout" || r.Attempts != 1 {
		t.Fatalf("result after failure: %+v", r)
	}
	if len(acts) != 2 || acts[0].Outcome != apps.OutcomeChanged || acts[1].Outcome != apps.OutcomeFailed || acts[1].Error != "timeout" {
		t.Fatalf("actions = %+v", acts)
	}
	// The retry continues from a fresh plan: the account exists, only the
	// team is left.
	h.drain()
	r = h.result(inst, alice)
	if r.Status != apps.StatusDone || r.ErrorCode != nil || r.Attempts != 0 {
		t.Fatalf("result after retry: %+v", r)
	}
	if acts := h.actions(r); len(acts) != 1 || acts[0].Key != "add_to_team:team-platform" || acts[0].Outcome != apps.OutcomeChanged {
		t.Fatalf("retry actions = %+v", acts)
	}

	// Permanent: failed at once, no retry job.
	bob := h.person("bob")
	h.tp.Fail("apply:create_account", permanent("seat_limit"))
	h.drain()
	r = h.result(inst, bob)
	if r.Status != apps.StatusFailed || r.ErrorCode == nil || *r.ErrorCode != "seat_limit" {
		t.Fatalf("bob = %+v", r)
	}
	if h.jobs(apps.ProvisionPersonArgs{}.Kind()) != 0 {
		t.Fatal("a permanent failure was retried")
	}
	n, err := h.svc.RetryFailed(h.ctx, audit.System, inst.ID, nil)
	if err != nil || n != 1 {
		t.Fatalf("retry = %d, %v", n, err)
	}
	h.drain()
	if r := h.result(inst, bob); r.Status != apps.StatusDone {
		t.Fatalf("bob after admin retry = %+v", r)
	}
	if h.auditCount(audit.ProvisioningRetried) != 1 {
		t.Fatal("the admin retry was not audited")
	}

	// Retries are exhausted after the last attempt: failed.
	carol := h.person("carol")
	for range 8 {
		h.tp.Fail("plan", retryable("unavailable"))
	}
	_ = carol
	h.drain()
	if r := h.result(inst, carol); r.Status != apps.StatusFailed || *r.ErrorCode != "unavailable" || r.Attempts != 8 {
		t.Fatalf("carol after exhausted retries = %+v", r)
	}
}

// TestProvisionSnoozesOnRetryAfter: a rate limit snoozes the job until the
// provider's reset without spending an attempt.
func TestProvisionSnoozesOnRetryAfter(t *testing.T) {
	h := newHarness(t)
	alice := h.person("alice")
	inst := h.instance(nil)
	h.tp.Fail("plan", &provider.Error{Code: "rate_limited", Retryable: true, RetryAfter: 90 * time.Second})
	err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 1, 8)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != 90*time.Second {
		t.Fatalf("err = %v, want a 90s snooze", err)
	}
	if r := h.result(inst, alice); r.Status != apps.StatusQueued || *r.ErrorCode != "rate_limited" {
		t.Fatalf("result = %+v", r)
	}
}

// TestProvisionUnmanagedMembers: pre-existing members are reported, never
// removed, unless the instance manages existing members.
func TestProvisionUnmanagedMembers(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	h.tp.AddMember(inst.ID, "outsider")
	h.tp.AddMember(inst.ID, "carol") // matches a person, created outside Orglane
	carol := h.person("carol")
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	r := h.result(inst, carol)
	if r.GrantedByOrglane || r.Status != apps.StatusDone {
		t.Fatalf("carol = %+v", r)
	}
	unmanaged, err := h.svc.UnmanagedAccounts(h.ctx, inst.ID)
	if err != nil || len(unmanaged) != 2 {
		t.Fatalf("unmanaged = %+v, %v", unmanaged, err)
	}
	// Carol leaves: her pre-existing account is not Orglane's to remove.
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, carol.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "carol"); !acct.Member {
		t.Fatal("an unmanaged account was removed")
	}
	if h.count(`SELECT count(*) FROM provisioning_removals`) != 0 {
		t.Fatal("removals were planned for an unmanaged account")
	}
	// The dry run shows what managing existing members would do.
	yes := true
	if _, err := h.svc.UpdateInstance(h.ctx, audit.System, inst.ID, apps.InstanceUpdate{ManageExistingMembers: &yes}); err != nil {
		t.Fatal(err)
	}
	planned, _, err := h.svc.DryRun(h.ctx, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(planned, func(p apps.PlannedPerson) bool { return p.Person.ID == carol.ID })
	if i < 0 || len(planned[i].Actions) == 0 || !planned[i].Actions[len(planned[i].Actions)-1].Removal {
		t.Fatalf("dry run = %+v", planned)
	}
	if h.count(`SELECT count(*) FROM provisioning_removals`) != 0 {
		t.Fatal("the dry run changed something")
	}
}

// TestRemovalGuard covers the hold, supersede, freshness and confirmation
// rules (docs/specs/providers.md "Removal guard").
func TestRemovalGuard(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	policy := provider.MappedTeams
	if _, err := h.svc.UpdateInstance(h.ctx, audit.System, inst.ID, apps.InstanceUpdate{AccessPolicy: &policy}); err != nil {
		t.Fatal(err)
	}
	var people []store.Person
	for _, name := range []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "p9"} {
		people = append(people, h.person(name))
	}
	big := h.team("everyone", people...)
	other := h.team("other", h.person("q0"))
	h.mapTeam(inst, big)
	h.mapTeam(inst, other)
	h.drain()
	for _, p := range people {
		if acct, ok := h.tp.Account(inst.ID, p.Username); !ok || !acct.Member {
			t.Fatalf("%s not provisioned", p.Username)
		}
	}

	// Deleting a mapping that covers most of the org holds one batch and
	// removes nobody.
	if err := h.svc.RemoveTeamMapping(h.ctx, audit.System, inst.ID, big.ID); err != nil {
		t.Fatal(err)
	}
	h.drain()
	held, err := h.svc.HeldRemovals(h.ctx, inst.ID)
	if err != nil || held == nil {
		t.Fatalf("held = %v, %v", held, err)
	}
	if held.Batch.ItemCount != 20 || len(held.Items) != 20 {
		t.Fatalf("held %d items, want 20 (two per person)", held.Batch.ItemCount)
	}
	for _, p := range people {
		if acct, _ := h.tp.Account(inst.ID, p.Username); !acct.Member {
			t.Fatalf("%s was removed while held", p.Username)
		}
	}
	if h.auditCount(audit.ProvisioningRemovalsHeld) != 1 {
		t.Fatal("holding was not audited")
	}

	// A concurrent change during the hold joins a new batch instead of
	// applying: q0 leaves the other team.
	if err := h.ids.RemoveTeamMember(h.ctx, audit.System, other.ID, mustTeamMember(h, other)); err != nil {
		t.Fatal(err)
	}
	h.drain()
	held2, err := h.svc.HeldRemovals(h.ctx, inst.ID)
	if err != nil || held2 == nil || held2.Batch.ID == held.Batch.ID || held2.Batch.ItemCount != 22 {
		t.Fatalf("second batch = %+v, %v", held2, err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removal_batches WHERE instance_id = $1 AND state = 'held'`, inst.ID); n != 1 {
		t.Fatalf("%d held batches, want 1", n)
	}
	if h.count(`SELECT count(*) FROM provisioning_removal_batches WHERE id = $1 AND state = 'superseded'`, held.Batch.ID) != 1 {
		t.Fatal("the first batch was not superseded")
	}
	// Confirming the superseded batch, or with a stale hash, is refused.
	if err := h.svc.ConfirmRemovals(h.ctx, audit.System, inst.ID, held.Batch.ID, held.Batch.ItemsHash); !errors.Is(err, apps.ErrBatchChanged) {
		t.Fatalf("confirm superseded = %v", err)
	}
	if err := h.svc.ConfirmRemovals(h.ctx, audit.System, inst.ID, held2.Batch.ID, held.Batch.ItemsHash); !errors.Is(err, apps.ErrBatchChanged) {
		t.Fatalf("confirm stale hash = %v", err)
	}

	// p0 regains access between preview and confirmation: not removed.
	again := h.team("again", people[0])
	h.mapTeam(inst, again)
	h.drain()
	if err := h.svc.ConfirmRemovals(h.ctx, audit.System, inst.ID, held2.Batch.ID, held2.Batch.ItemsHash); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "p0"); !acct.Member {
		t.Fatal("a person who regained access was removed")
	}
	for _, p := range append(people[1:], store.Person{Username: "q0"}) {
		if acct, _ := h.tp.Account(inst.ID, p.Username); acct.Member {
			t.Fatalf("%s kept access after confirmation", p.Username)
		}
	}
	if h.auditCount(audit.ProvisioningRemovalsConfirmed) != 1 {
		t.Fatal("confirmation was not audited")
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removal_batches WHERE id = $1 AND state = 'applied'`, held2.Batch.ID); n != 1 {
		t.Fatal("the confirmed batch is not applied")
	}
}

func mustTeamMember(h *harness, team store.Team) uuid.UUID {
	h.t.Helper()
	ids, err := store.New(h.pool).ListTeamMemberIDs(h.ctx, team.ID)
	if err != nil || len(ids) != 1 {
		h.t.Fatalf("members of %s: %v, %v", team.Slug, ids, err)
	}
	return ids[0]
}

// TestRemovalGuardCountsLastDay: splitting a mass removal into small bursts
// does not bypass the guard.
func TestRemovalGuardCountsLastDay(t *testing.T) {
	h := newHarness(t)
	a, b, c := h.person("a1"), h.person("b1"), h.person("c1")
	inst := h.instance(nil)
	h.drain()
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, a.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "a1"); acct.Member {
		t.Fatal("a small removal did not apply")
	}
	// One more person: 1 applied in the last day + 1 = 2 <= 3 applies.
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, b.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "b1"); acct.Member {
		t.Fatal("the second small removal did not apply")
	}
	// The third: 2 applied + 1 = 3, still within the limit.
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, c.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "c1"); acct.Member {
		t.Fatal("the third removal did not apply")
	}
	d := h.person("d1")
	h.drain()
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, d.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "d1"); !acct.Member {
		t.Fatal("the fourth removal within a day was not held")
	}
	held, err := h.svc.HeldRemovals(h.ctx, inst.ID)
	if err != nil || held == nil || held.Batch.ItemCount != 1 {
		t.Fatalf("held = %+v, %v", held, err)
	}
	// While that batch is held, a new removal joins it, even though the
	// earlier removals no longer count a day later.
	h.exec(`UPDATE provisioning_removals SET updated_at = now() - interval '25 hours' WHERE state = 'applied'`)
	e := h.person("e1")
	h.drain()
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, e.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if acct, _ := h.tp.Account(inst.ID, "e1"); !acct.Member {
		t.Fatal("a removal applied while a batch was held")
	}
	if held, _ := h.svc.HeldRemovals(h.ctx, inst.ID); held == nil || held.Batch.ItemCount != 2 {
		t.Fatalf("held = %+v", held)
	}
}

// TestEmployeeStep: an instance that needs a linked account leaves the
// person pending_employee_action and shows an optional onboarding step.
func TestEmployeeStep(t *testing.T) {
	h := newHarness(t)
	alice := h.person("alice")
	inst := h.instance(map[string]any{"require_link": true})
	h.drain()
	r := h.result(inst, alice)
	if r.Status != apps.StatusPendingEmployeeAction || r.EmployeeStep == nil || *r.EmployeeStep != "testprovider.link-account" {
		t.Fatalf("result = %+v", r)
	}
	steps, err := h.svc.EmployeeSteps().Progress(h.ctx, onboarding.Viewer{PersonID: alice.ID}, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].ID != "testprovider.link-account" || steps[0].Required || steps[0].Complete || steps[0].ActionURL != "/me/accounts/testprovider" {
		t.Fatalf("steps = %+v", steps)
	}
	// Someone without access to any instance of the provider sees no step;
	// another member sees nobody's steps.
	other := h.person("other")
	if steps, _ := h.svc.EmployeeSteps().Progress(h.ctx, onboarding.Viewer{PersonID: other.ID}, alice.ID); len(steps) != 0 {
		t.Fatalf("another member sees %+v", steps)
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, other.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if steps, _ := h.svc.EmployeeSteps().Progress(h.ctx, onboarding.Viewer{PersonID: other.ID}, other.ID); len(steps) != 0 {
		t.Fatalf("a person without access sees %+v", steps)
	}
	// My apps lists the instance with the step.
	mine, err := h.svc.MyApps(h.ctx, alice.ID)
	if err != nil || len(mine) != 1 || mine[0].Step != "testprovider.link-account" {
		t.Fatalf("my apps = %+v, %v", mine, err)
	}
}

// TestManagedSetup: setup failures retry and finally fail the instance
// with setup_failed; an admin retry activates it, binds it and queues
// provisioning.
func TestManagedSetup(t *testing.T) {
	h := newHarness(t)
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "testmanaged"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != apps.StateInstalling || inst.PackageID == nil {
		t.Fatalf("created %+v", inst.AppInstance)
	}
	h.managed.Fail("setup", retryable("unreachable"))
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err == nil {
		t.Fatal("a failed setup did not ask for a retry")
	}
	h.managed.Fail("setup", retryable("unreachable"))
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 8, 8); err != nil {
		t.Fatal(err)
	}
	got, _ := h.svc.Instance(h.ctx, inst.ID)
	if got.State != apps.StateFailed || got.HealthCode == nil || *got.HealthCode != apps.CodeSetupFailed {
		t.Fatalf("after exhausted setup: %+v", got.AppInstance)
	}
	if _, err := h.svc.RetrySetup(h.ctx, audit.System, inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RetrySetup(h.ctx, audit.System, inst.ID); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("second retry = %v", err)
	}
	h.drain()
	got, _ = h.svc.Instance(h.ctx, inst.ID)
	if got.State != apps.StateActive || h.managed.Setups(inst.ID) != 1 {
		t.Fatalf("after retry: %+v", got.AppInstance)
	}
	b, err := h.svc.InstanceBindings(h.ctx, inst.ID)
	if err != nil || len(b) != 1 || b[0].Capability != "code" || !b[0].IsDefault {
		t.Fatalf("bindings = %+v, %v", b, err)
	}
	if h.auditCount(audit.AppInstanceStateChanged) < 3 {
		t.Fatal("state changes were not audited")
	}
	// A connected instance cannot retry setup.
	conn := h.instance(nil)
	if _, err := h.svc.RetrySetup(h.ctx, audit.System, conn.ID); !errors.Is(err, apps.ErrNotSupported) {
		t.Fatalf("retry setup of a connected instance = %v", err)
	}
}

// TestInstancesAndBindings: keys, settings validation, bindings and their
// audit entries.
func TestInstancesAndBindings(t *testing.T) {
	h := newHarness(t)
	first := h.instance(nil)
	if first.Key != "test" || first.Settings["organization"] != "acme" || first.Settings["require_link"] != false {
		t.Fatalf("defaults: %+v %v", first.AppInstance, first.Settings)
	}
	if _, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: testprovider.ID}); !errors.Is(err, apps.ErrKeyReserved) {
		t.Fatalf("duplicate key = %v", err)
	}
	for _, key := range []string{"A", "x", "-ab", "ab-", "toolong-toolong-toolong-x"} {
		if _, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, Key: key}); !errors.Is(err, apps.ErrInvalid) {
			t.Errorf("key %q = %v", key, err)
		}
	}
	if _, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "nope"}); !errors.Is(err, apps.ErrProviderUnavailable) {
		t.Fatalf("unknown provider = %v", err)
	}
	if _, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, Key: "bad", Settings: map[string]any{"organization": "Not Valid"}}); !errors.Is(err, apps.ErrInvalid) {
		t.Fatalf("invalid setting = %v", err)
	}
	second, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, Key: "second", State: apps.StateActive})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := h.svc.Bindings(h.ctx)
	if err != nil || len(bindings) != 2 {
		t.Fatalf("bindings = %+v, %v", bindings, err)
	}
	for _, b := range bindings {
		if b.IsDefault != (b.InstanceID == first.ID) {
			t.Fatalf("default binding = %+v", b)
		}
	}
	if err := h.svc.SetDefaultBinding(h.ctx, audit.System, "code", second.ID); err != nil {
		t.Fatal(err)
	}
	bindings, _ = h.svc.Bindings(h.ctx)
	for _, b := range bindings {
		if b.IsDefault != (b.InstanceID == second.ID) {
			t.Fatalf("after switching: %+v", b)
		}
	}
	if err := h.svc.SetDefaultBinding(h.ctx, audit.System, "files", second.ID); !errors.Is(err, apps.ErrNotFound) {
		t.Fatalf("unbound capability = %v", err)
	}
	if h.auditCount(audit.AppBindingSet) != 1 {
		t.Fatal("binding change not audited")
	}

	// Settings: immutable ones stay; mutable ones change and queue work.
	if _, err := h.svc.UpdateInstance(h.ctx, audit.System, first.ID, apps.InstanceUpdate{Settings: map[string]any{"organization": "other"}}); !errors.Is(err, apps.ErrInvalid) {
		t.Fatalf("immutable setting = %v", err)
	}
	if _, err := h.svc.UpdateInstance(h.ctx, audit.System, first.ID, apps.InstanceUpdate{Settings: map[string]any{"unknown": 1}}); !errors.Is(err, apps.ErrInvalid) {
		t.Fatalf("unknown setting = %v", err)
	}
	h.drain()
	name := "Code"
	updated, err := h.svc.UpdateInstance(h.ctx, audit.System, first.ID, apps.InstanceUpdate{DisplayName: &name, Settings: map[string]any{"require_link": true}})
	if err != nil || updated.DisplayName != "Code" || updated.Settings["require_link"] != true {
		t.Fatalf("update = %+v, %v", updated.AppInstance, err)
	}
	if h.jobs(apps.ProvisionInstanceArgs{}.Kind()) != 1 {
		t.Fatal("a settings change did not queue provisioning")
	}
	var fields []string
	if err := h.pool.QueryRow(h.ctx, `SELECT array(SELECT jsonb_array_elements_text(metadata->'fields')) FROM audit_entries WHERE action = 'app_instance.updated'`).Scan(&fields); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fields, []string{"display_name", "settings"}) {
		t.Fatalf("audited fields = %v", fields)
	}
	// A removed instance whose data is retained keeps its key reserved.
	h.exec(`UPDATE app_instances SET state = 'removed', data_retained = true WHERE id = $1`, second.ID)
	if _, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, Key: "second"}); !errors.Is(err, apps.ErrKeyReserved) {
		t.Fatalf("retained key = %v", err)
	}
	h.exec(`UPDATE app_instances SET data_retained = false WHERE id = $1`, second.ID)
	if _, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, Key: "second"}); err != nil {
		t.Fatalf("purged key = %v", err)
	}
}

// TestChangeWhileJobRuns: a change committed while provision_instance or
// apply_removals already runs queues one follow-up instead of being
// deduplicated against the running job (and lost).
func TestChangeWhileJobRuns(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	// The activation job is "running" (taken by a worker that may have read
	// the mappings before the change below).
	h.exec(`UPDATE river_job SET state = 'running', attempted_at = now() WHERE kind = $1`, apps.ProvisionInstanceArgs{}.Kind())
	team := h.team("platform", h.person("alice"))
	h.mapTeam(inst, team)
	h.mapTeam(inst, team) // a second change while it runs: still one follow-up
	// One job waits besides the running one (deduplicated on the second
	// change), so the mapping is not lost.
	if n := h.count(`SELECT count(*) FROM river_job WHERE kind = $1 AND state = 'available'`, apps.ProvisionInstanceArgs{}.Kind()); n != 1 {
		t.Fatalf("%d queued provision_instance jobs, want 1", n)
	}
	// The same reason while its job runs gets the one follow-up.
	h.exec(`UPDATE river_job SET state = 'running', attempted_at = now() WHERE kind = $1`, apps.ProvisionInstanceArgs{}.Kind())
	h.mapTeam(inst, team)
	h.mapTeam(inst, team)
	if n := h.count(`SELECT count(*) FROM river_job WHERE kind = $1 AND state = 'available' AND (args->>'follow_up')::bool`, apps.ProvisionInstanceArgs{}.Kind()); n != 1 {
		t.Fatalf("%d follow-up provision_instance jobs, want 1", n)
	}
	h.exec(`DELETE FROM river_job WHERE state = 'running'`)
	h.drain()
	if acct, ok := h.tp.Account(inst.ID, "alice"); !ok || !acct.Teams["team-platform"] {
		t.Fatalf("the change during the run was lost: %+v", acct)
	}

	// Same for apply_removals.
	bob := h.person("bob")
	h.drain()
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, bob.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: bob.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE river_job SET state = 'running', attempted_at = now() WHERE kind = $1`, apps.ApplyRemovalsArgs{}.Kind())
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alicePerson(h).ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alicePerson(h).ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM river_job WHERE kind = $1 AND state <> 'running' AND (args->>'follow_up')::bool`, apps.ApplyRemovalsArgs{}.Kind()); n != 1 {
		t.Fatalf("%d follow-up apply_removals jobs, want 1", n)
	}
}

func alicePerson(h *harness) store.Person {
	h.t.Helper()
	p, err := h.ids.GetPersonByUsername(h.ctx, "alice")
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// TestAcceptedRemovalIsNotHeldWithNewRemovals: a removal the provider already
// accepted must stay on the confirmation path when other removals cross the
// guard's threshold. It is applied/confirmed, not swept into the held batch
// that an admin would have to release.
func TestAcceptedRemovalIsNotHeldWithNewRemovals(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	people := []store.Person{h.person("alice"), h.person("bob"), h.person("carol"), h.person("dave"), h.person("erin")}
	h.drain()
	alice := people[0]

	// One accepted removal first: the provider answers removal_pending, so the
	// row keeps the durable acceptance marker.
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, alice.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	h.tp.Fail("apply:remove_from_org", &provider.Error{Code: provider.CodeRemovalPending, Retryable: true, RetryAfter: 5 * time.Second})
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8); err == nil {
		t.Fatal("the pending removal did not snooze")
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending' AND error_code = $2`, alice.ID, provider.CodeRemovalPending); n != 1 {
		t.Fatalf("accepted removals = %d, want 1", n)
	}

	// Four more removals cross the threshold of three. The accepted row must
	// still be confirmed; only the four new ones are held.
	for _, p := range people[1:] {
		if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, p.ID, identity.StatusSuspended); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: p.ID}, 1, 8); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 2, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, alice.ID); n != 1 {
		t.Fatalf("accepted removal applied = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'key' = 'remove_from_org'`, string(audit.ProvisioningActionApplied)); n != 1 {
		t.Fatalf("audit entries = %d, want 1 for the accepted removal", n)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'held'`, alice.ID); n != 0 {
		t.Fatalf("accepted removal held = %d, want 0", n)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE instance_id = $1 AND state = 'held'`, inst.ID); n != 4 {
		t.Fatalf("held removals = %d, want 4", n)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removal_batches WHERE instance_id = $1 AND state = 'held'`, inst.ID); n != 1 {
		t.Fatalf("held batches = %d, want 1", n)
	}
}
