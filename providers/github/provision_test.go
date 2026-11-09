package github

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/github/githubfake"
	"github.com/orglane/orglane/providers/provider"
)

// Provisioning with the real adapter against the fake GitHub, whose every
// request and response is validated against GitHub's OpenAPI description.

func desired(id int64, login string, access, managed bool, teams ...provider.TeamRef) provider.DesiredAccess {
	return provider.DesiredAccess{
		PersonID: uuid.New(), Username: "alice", DisplayName: "Alice", WorkEmail: "alice@acme.test",
		Access: access, Managed: managed, Teams: teams, Mapped: teams,
		Link: &provider.AccountLink{ID: uuid.New(), Method: provider.LinkOAuth, Account: provider.ExternalAccount{Realm: Realm, ExternalID: strconv.FormatInt(id, 10), Login: login}},
	}
}

func kinds(p provider.Plan) []string {
	out := []string{}
	for _, a := range p.Actions {
		k := a.Key
		if a.Removal {
			k += "!"
		}
		out = append(out, k)
	}
	return out
}

func applyAll(t *testing.T, a *Adapter, env provider.Env, inst provider.Instance, p provider.Plan) []provider.ActionOutcome {
	t.Helper()
	var outs []provider.ActionOutcome
	for _, act := range p.Actions {
		out, err := a.Apply(t.Context(), env, inst, act)
		if err != nil {
			t.Fatalf("apply %s: %v", act.Key, err)
		}
		outs = append(outs, out)
	}
	return outs
}

func code(err error) string {
	c, _, _ := provider.Classify(err)
	return c
}

func syncTeam(t *testing.T, a *Adapter, env provider.Env, inst provider.Instance, slug string) provider.TeamRef {
	t.Helper()
	refs, err := a.SyncTeams(t.Context(), env, inst, []provider.TeamMapping{{TeamID: uuid.New(), TeamSlug: slug, TeamName: "Team " + slug, Mode: provider.MappingCreate}})
	if err != nil || len(refs) != 1 {
		t.Fatalf("sync %s = %v, %v", slug, refs, err)
	}
	return refs[0]
}

// A linked person is invited by user ID and added to their teams; the
// invitation is Orglane's by GitHub's answer, and by its inviter (the App's
// bot) when the ensure runs again. Accepting changes nothing to plan.
func TestProvisionInvitesAndAddsToTeams(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	f.AddUser(githubfake.User{ID: 1001, Login: "alice-gh"})
	team := syncTeam(t, a, env, inst, "platform")
	if ts := f.Teams(); len(ts) != 1 || ts[0].Slug != "platform" || ts[0].Description[:len(teamMark(inst))] != teamMark(inst) {
		t.Fatalf("teams = %+v", ts)
	}
	d := desired(1001, "alice-gh", true, false, team)
	plan, err := a.Plan(t.Context(), env, inst, d)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(plan); !slices.Equal(got, []string{"invite", "add_to_team:" + team.ExternalRef}) {
		t.Fatalf("plan = %v", got)
	}
	outs := applyAll(t, a, env, inst, plan)
	inv := f.Invitations()
	if len(inv) != 1 || inv[0].Invitee != 1001 || inv[0].Inviter != f.BotLogin() {
		t.Fatalf("invitations = %+v", inv)
	}
	if p := outs[0].Proof; !outs[0].Changed || p == nil || p.Kind != provider.ProofResponse || p.Evidence != "invitation:"+strconv.FormatInt(inv[0].ID, 10) || outs[0].Link.ExternalID != "1001" {
		t.Fatalf("invite outcome = %+v", outs[0])
	}
	teamID, _ := strconv.ParseInt(team.ExternalRef, 10, 64)
	if m := f.TeamMembers(teamID); m[1001] != "member/pending" {
		t.Fatalf("team members = %v", m)
	}
	// Ensured again: unchanged, proven by the bot's invitation.
	out, err := a.Apply(t.Context(), env, inst, plan.Actions[0])
	if err != nil || out.Changed || out.Proof == nil || out.Proof.Kind != provider.ProofMarker {
		t.Fatalf("invite ensure = %+v, %v", out, err)
	}
	if plan, err = a.Plan(t.Context(), env, inst, d); err != nil || len(plan.Actions) != 0 {
		t.Fatalf("plan after apply = %v, %v", kinds(plan), err)
	}
	f.Accept(1001)
	if f.Members()[1001] != githubfake.RoleMember || f.TeamMembers(teamID)[1001] != "member/active" {
		t.Fatal("accepting did not make a member")
	}
	if plan, err = a.Plan(t.Context(), env, inst, d); err != nil || len(plan.Actions) != 0 {
		t.Fatalf("plan after accepting = %v, %v", kinds(plan), err)
	}
	// An active membership proves nothing.
	out, err = a.Apply(t.Context(), env, inst, plan2(provider.ActionInvite, "1001"))
	if err != nil || out.Changed || out.Proof != nil {
		t.Fatalf("invite of a member = %+v, %v", out, err)
	}
}

func plan2(kind provider.ActionKind, id string) provider.Action {
	return accountAction(kind, id, kind == provider.ActionCancelInvitation || kind == provider.ActionRemoveFromOrg)
}

// Without a verified link nothing can be done but wait for the person.
func TestProvisionWaitsForTheLink(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	d := desired(1, "x", true, false)
	d.Link = nil
	plan, err := a.Plan(t.Context(), env, inst, d)
	if err != nil || plan.EmployeeStep != StepLinkAccount || len(plan.Actions) != 0 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	d.Access = false
	if plan, err = a.Plan(t.Context(), env, inst, d); err != nil || plan.EmployeeStep != "" || len(plan.Actions) != 0 {
		t.Fatalf("plan without access = %+v, %v", plan, err)
	}
	if len(f.Calls()) != 0 {
		t.Fatalf("GitHub was called: %v", f.Calls())
	}
}

// Removal: a pending invitation is canceled, a membership removed, only
// for managed accounts; owners and billing managers are never removed.
func TestProvisionRemovesManagedAccessOnly(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	for _, u := range []githubfake.User{{ID: 1, Login: "invitee"}, {ID: 2, Login: "member"}, {ID: 3, Login: "owner"}, {ID: 4, Login: "biller"}} {
		f.AddUser(u)
	}
	f.InviteAs(1, f.BotLogin())
	f.AddMember(2, githubfake.RoleMember)
	f.AddMember(3, githubfake.RoleOwner)
	f.AddMember(4, "billing_manager")
	f.AddMember(99, githubfake.RoleOwner) // another owner, so the owner is not the last one
	f.AddUser(githubfake.User{ID: 99, Login: "founder"})

	// Unmanaged: reported, never removed.
	for _, id := range []int64{1, 2, 3} {
		plan, err := a.Plan(t.Context(), env, inst, desired(id, "", false, false))
		if err != nil || len(plan.Actions) != 0 {
			t.Fatalf("unmanaged %d: plan = %v, %v", id, kinds(plan), err)
		}
	}
	plan, err := a.Plan(t.Context(), env, inst, desired(1, "invitee", false, true))
	if err != nil || !slices.Equal(kinds(plan), []string{"cancel_invitation!"}) {
		t.Fatalf("invitee: plan = %v, %v", kinds(plan), err)
	}
	if outs := applyAll(t, a, env, inst, plan); !outs[0].Changed || len(f.Invitations()) != 0 {
		t.Fatalf("cancel = %+v, invitations %v", outs, f.Invitations())
	}
	plan, err = a.Plan(t.Context(), env, inst, desired(2, "member", false, true))
	if err != nil || !slices.Equal(kinds(plan), []string{"remove_from_org!"}) {
		t.Fatalf("member: plan = %v, %v", kinds(plan), err)
	}
	if outs := applyAll(t, a, env, inst, plan); !outs[0].Changed {
		t.Fatalf("remove = %+v", outs)
	}
	if _, ok := f.Members()[2]; ok {
		t.Fatal("the member was not removed")
	}
	// Ensured again: nothing to remove.
	if out, err := a.Apply(t.Context(), env, inst, plan.Actions[0]); err != nil || out.Changed {
		t.Fatalf("remove again = %+v, %v", out, err)
	}
	// Owners are planned (an admin sees why) but never removed.
	plan, err = a.Plan(t.Context(), env, inst, desired(3, "owner", false, true))
	if err != nil || len(plan.Actions) != 1 {
		t.Fatalf("owner: plan = %v, %v", kinds(plan), err)
	}
	if _, err := a.Apply(t.Context(), env, inst, plan.Actions[0]); code(err) != CodeOwnerProtected {
		t.Fatalf("owner removal = %v", err)
	}
	if _, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionRemoveFromOrg, "4")); code(err) != CodeRoleProtected {
		t.Fatalf("billing manager removal = %v", err)
	}
	if m := f.Members(); m[3] != githubfake.RoleOwner || m[4] != "billing_manager" {
		t.Fatalf("members = %v", m)
	}
	// A deleted account has no access left: removals are satisfied.
	f.DeleteUser(2)
	if out, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionRemoveFromOrg, "2")); err != nil || out.Changed {
		t.Fatalf("removal of a deleted account = %+v, %v", out, err)
	}
	if _, err := a.Plan(t.Context(), env, inst, desired(2, "member", true, true)); code(err) != CodeAccountNotFound {
		t.Fatalf("access for a deleted account = %v", err)
	}
}

// GitHub may accept an organization removal before it finishes: the
// membership read still serves it for a moment. The removal is not reported
// as changed until a later read confirms it, so the guard keeps it pending
// and the audit entry waits.
func TestProvisionOrgRemovalWaitsForGitHub(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	f.AddUser(githubfake.User{ID: 70, Login: "dev"})
	f.AddMember(70, githubfake.RoleMember)
	f.QueueOrgRemovals(2)
	act := plan2(provider.ActionRemoveFromOrg, "70")
	out, err := a.Apply(t.Context(), env, inst, act)
	if c, retryable, after := provider.Classify(err); c != CodeRemovalPending || !retryable || after != removalPoll || out.Changed {
		t.Fatalf("first removal = %+v, %v; want retryable %s after %s", out, err, CodeRemovalPending, removalPoll)
	}
	if _, ok := f.Members()[70]; !ok {
		t.Fatal("the membership was reported removed while GitHub still served it")
	}
	// The next read is the last stale one and completes the queued removal;
	// the action then sees the membership gone and changes nothing.
	out, err = a.Apply(t.Context(), env, inst, act)
	if err != nil || out.Changed {
		t.Fatalf("confirmed removal = %+v, %v", out, err)
	}
	if _, ok := f.Members()[70]; ok {
		t.Fatal("the membership was not removed")
	}
}

// A membership that only enterprise teams grant cannot be removed through
// the organization membership API; GitHub keeps it and the action reports a
// permanent error instead of retrying or claiming the removal.
func TestProvisionOrgRemovalOfAnEnterpriseMembership(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	f.AddUser(githubfake.User{ID: 71, Login: "enterprise-member"})
	f.AddIndirectMember(71, githubfake.RoleMember, []string{"ent:platform"})
	_, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionRemoveFromOrg, "71"))
	if c, retryable, _ := provider.Classify(err); c != CodeIndirectMembership || retryable {
		t.Fatalf("enterprise membership removal = %v; want permanent %s", err, CodeIndirectMembership)
	}
	if f.Members()[71] != githubfake.RoleMember {
		t.Fatal("the membership was removed")
	}
}

// A rename (or reuse of the old login) after GitHub accepted a removal must
// not make the confirmation read mistake the old login's 404 for the bound
// account's absence: the confirmation follows the numeric ID's current login,
// so the removal stays pending until the bound account is really gone.
func TestProvisionRemovalConfirmationFollowsTheBoundID(t *testing.T) {
	t.Run("organization membership", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		f.AddUser(githubfake.User{ID: 70, Login: "dev"})
		f.AddMember(70, githubfake.RoleMember)
		f.QueueOrgRemovals(2)
		base, path := env.client.Transport, "/orgs/"+f.Org().Login+"/memberships/dev"
		env.client.Transport = &interleave{base: base, method: http.MethodDelete, path: path, after: true, fn: func() {
			f.RenameUser(70, "dev-renamed")
			f.AddUser(githubfake.User{ID: 71, Login: "dev"}) // the old login, reused without a membership
		}}
		out, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionRemoveFromOrg, "70"))
		if c, retryable, _ := provider.Classify(err); c != CodeRemovalPending || !retryable || out.Changed {
			t.Fatalf("removal after the rename = %+v, %v; want retryable %s", out, err, CodeRemovalPending)
		}
		if f.Members()[70] != githubfake.RoleMember {
			t.Fatal("the bound account was reported removed while GitHub still had it as a member")
		}
		env.client.Transport = base
		if out, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionRemoveFromOrg, "70")); err != nil || out.Changed {
			t.Fatalf("confirmed removal = %+v, %v", out, err)
		}
		if _, ok := f.Members()[70]; ok {
			t.Fatal("the bound account was not removed")
		}
	})
	t.Run("team membership", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		team := syncTeam(t, a, env, inst, "platform")
		teamID := mustInt(team.ExternalRef)
		f.AddUser(githubfake.User{ID: 70, Login: "dev"})
		f.AddMember(70, githubfake.RoleMember)
		f.SetTeamMember(teamID, 70, githubfake.TeamMember)
		base := env.client.Transport
		path := "/organizations/" + strconv.FormatInt(f.Org().ID, 10) + "/team/" + team.ExternalRef + "/memberships/dev"
		env.client.Transport = &interleave{base: base, method: http.MethodDelete, path: path, after: true, fn: func() {
			f.RenameUser(70, "dev-renamed")
			f.AddUser(githubfake.User{ID: 71, Login: "dev"}) // the old login, reused without the team
			// GitHub accepted the removal but has not finished it yet: the
			// bound account still has the team membership under its new login.
			f.SetTeamMember(teamID, 70, githubfake.TeamMember)
		}}
		act := teamAction(provider.ActionRemoveFromTeam, "70", ghTeam{ID: teamID, Slug: team.Name})
		out, err := a.Apply(t.Context(), env, inst, act)
		if c, retryable, _ := provider.Classify(err); c != CodeRemovalPending || !retryable || out.Changed {
			t.Fatalf("team removal after the rename = %+v, %v; want retryable %s", out, err, CodeRemovalPending)
		}
		if f.TeamMembers(teamID)[70] == "" {
			t.Fatal("the bound account's team membership was reported removed")
		}
		env.client.Transport = base
		if out, err := a.Apply(t.Context(), env, inst, act); err != nil || !out.Changed {
			t.Fatalf("confirmed team removal = %+v, %v", out, err)
		}
		if f.TeamMembers(teamID)[70] != "" {
			t.Fatal("the bound account was not removed from the team")
		}
	})
	t.Run("invitation", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		f.AddUser(githubfake.User{ID: 70, Login: "dev"})
		inv := f.InviteAs(70, f.BotLogin())
		base := env.client.Transport
		path := "/orgs/" + f.Org().Login + "/invitations/" + strconv.FormatInt(inv.ID, 10)
		env.client.Transport = &interleave{base: base, method: http.MethodDelete, path: path, after: true, fn: func() {
			f.RenameUser(70, "dev-renamed")
			f.AddUser(githubfake.User{ID: 71, Login: "dev"}) // the old login, reused without an invitation
			// The cancellation has not propagated yet: the bound account still
			// has a pending invitation under its new login.
			f.InviteAs(70, f.BotLogin())
		}}
		act := accountAction(provider.ActionCancelInvitation, "70", true)
		out, err := a.Apply(t.Context(), env, inst, act)
		if c, retryable, _ := provider.Classify(err); c != CodeRemovalPending || !retryable || out.Changed {
			t.Fatalf("cancellation after the rename = %+v, %v; want retryable %s", out, err, CodeRemovalPending)
		}
		if len(f.Invitations()) != 1 {
			t.Fatalf("invitations = %+v, want the bound account's pending invitation", f.Invitations())
		}
		env.client.Transport = base
		if out, err := a.Apply(t.Context(), env, inst, act); err != nil || !out.Changed {
			t.Fatalf("confirmed cancellation = %+v, %v", out, err)
		}
		if len(f.Invitations()) != 0 {
			t.Fatalf("invitations after the confirmed cancellation = %+v", f.Invitations())
		}
	})
}

// A transient failure of the confirmation read after GitHub accepted a removal
// must not lose the acceptance: the action stays pending with the provider's
// retry delay (at least the confirmation poll), and the next call confirms it.
func TestProvisionRemovalConfirmationReadFails(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
		after  time.Duration
	}{
		{name: "server error", status: http.StatusBadGateway, after: removalPoll},
		{name: "rate limit", status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"120"}}, after: 2 * time.Minute},
	}
	assertPending := func(t *testing.T, out provider.ActionOutcome, err error, after time.Duration) {
		t.Helper()
		if c, retryable, got := provider.Classify(err); c != CodeRemovalPending || !retryable || got != after || out.Changed {
			t.Fatalf("removal with a failed confirmation = %+v, %v; want retryable %s after %s", out, err, CodeRemovalPending, after)
		}
	}
	t.Run("organization membership", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f, env, inst := fakeFixture(t)
				a := New()
				f.AddUser(githubfake.User{ID: 70, Login: "dev"})
				f.AddMember(70, githubfake.RoleMember)
				base, path := env.client.Transport, "/orgs/"+f.Org().Login+"/memberships/dev"
				env.client.Transport = &interleave{base: base, method: http.MethodDelete, path: path, after: true, fn: func() {
					f.Fail(http.MethodGet, path, tc.status, tc.header)
				}}
				out, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionRemoveFromOrg, "70"))
				assertPending(t, out, err, tc.after)
				env.client.Transport = base
				if out, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionRemoveFromOrg, "70")); err != nil || out.Changed {
					t.Fatalf("confirmed removal = %+v, %v", out, err)
				}
			})
		}
	})
	t.Run("team membership", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f, env, inst := fakeFixture(t)
				a := New()
				team := syncTeam(t, a, env, inst, "platform")
				teamID := mustInt(team.ExternalRef)
				f.AddUser(githubfake.User{ID: 70, Login: "dev"})
				f.AddMember(70, githubfake.RoleMember)
				f.SetTeamMember(teamID, 70, githubfake.TeamMember)
				base := env.client.Transport
				path := "/organizations/" + strconv.FormatInt(f.Org().ID, 10) + "/team/" + team.ExternalRef + "/memberships/dev"
				env.client.Transport = &interleave{base: base, method: http.MethodDelete, path: path, after: true, fn: func() {
					f.Fail(http.MethodGet, path, tc.status, tc.header)
				}}
				act := teamAction(provider.ActionRemoveFromTeam, "70", ghTeam{ID: teamID, Slug: team.Name})
				out, err := a.Apply(t.Context(), env, inst, act)
				assertPending(t, out, err, tc.after)
				env.client.Transport = base
				if out, err := a.Apply(t.Context(), env, inst, act); err != nil {
					t.Fatalf("confirmed team removal = %+v, %v", out, err)
				}
				if f.TeamMembers(teamID)[70] != "" {
					t.Fatal("the bound account was not removed from the team")
				}
			})
		}
	})
	t.Run("invitation", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f, env, inst := fakeFixture(t)
				a := New()
				f.AddUser(githubfake.User{ID: 70, Login: "dev"})
				inv := f.InviteAs(70, f.BotLogin())
				base := env.client.Transport
				deletePath := "/orgs/" + f.Org().Login + "/invitations/" + strconv.FormatInt(inv.ID, 10)
				readPath := "/orgs/" + f.Org().Login + "/memberships/dev"
				env.client.Transport = &interleave{base: base, method: http.MethodDelete, path: deletePath, after: true, fn: func() {
					f.Fail(http.MethodGet, readPath, tc.status, tc.header)
				}}
				act := accountAction(provider.ActionCancelInvitation, "70", true)
				out, err := a.Apply(t.Context(), env, inst, act)
				assertPending(t, out, err, tc.after)
				env.client.Transport = base
				if out, err := a.Apply(t.Context(), env, inst, act); err != nil {
					t.Fatalf("confirmed cancellation = %+v, %v", out, err)
				}
				if len(f.Invitations()) != 0 {
					t.Fatalf("invitations after the confirmed cancellation = %+v", f.Invitations())
				}
			})
		}
	})
}

// Team removals only from the instance's mapped teams, only for managed
// accounts, never of a maintainer; grants come before removals.
func TestProvisionTeamRemovals(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	f.AddUser(githubfake.User{ID: 7, Login: "dev"})
	f.AddMember(7, githubfake.RoleMember)
	backend, frontend := syncTeam(t, a, env, inst, "backend"), syncTeam(t, a, env, inst, "frontend")
	other := f.AddTeam("Secret Ops", "not Orglane's")
	maint := f.AddTeam("Maintained", "picked by an admin")
	maintRef := provider.TeamRef{TeamID: uuid.New(), ExternalRef: strconv.FormatInt(maint.ID, 10)}
	for _, ref := range []provider.TeamRef{backend, maintRef} {
		id, _ := strconv.ParseInt(ref.ExternalRef, 10, 64)
		f.SetTeamMember(id, 7, githubfake.TeamMember)
	}
	f.SetTeamMember(other.ID, 7, githubfake.TeamMember)
	f.SetTeamMember(maint.ID, 7, githubfake.TeamMaintainer)

	d := desired(7, "dev", true, true, frontend)
	d.Mapped = []provider.TeamRef{backend, frontend, maintRef}
	plan, err := a.Plan(t.Context(), env, inst, d)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"add_to_team:" + frontend.ExternalRef, "remove_from_team:" + backend.ExternalRef + "!", "remove_from_team:" + maintRef.ExternalRef + "!"}
	slices.Sort(want[1:])
	got := kinds(plan)
	slices.Sort(got[1:])
	if !slices.Equal(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	for _, act := range plan.Actions {
		_, err := a.Apply(t.Context(), env, inst, act)
		if act.Params[paramTeam] == maintRef.ExternalRef {
			if code(err) != CodeMaintainerProtected {
				t.Fatalf("maintainer removal = %v", err)
			}
		} else if err != nil {
			t.Fatalf("%s: %v", act.Key, err)
		}
	}
	if m := f.TeamMembers(other.ID); m[7] == "" {
		t.Fatal("removed from a team that is not mapped")
	}
	if m := f.TeamMembers(maint.ID); m[7] != "maintainer/active" {
		t.Fatal("a maintainer was removed")
	}
	// Unmanaged: additions only.
	d.Managed = false
	d.Teams, d.Mapped = []provider.TeamRef{backend}, []provider.TeamRef{backend, frontend}
	if plan, err = a.Plan(t.Context(), env, inst, d); err != nil || !slices.Equal(kinds(plan), []string{"add_to_team:" + backend.ExternalRef}) {
		t.Fatalf("unmanaged plan = %v, %v", kinds(plan), err)
	}
	// A mapped team that does not exist (yet) holds the plan back.
	d.Teams = []provider.TeamRef{{TeamID: uuid.New()}}
	if _, err := a.Plan(t.Context(), env, inst, d); code(err) != CodeTeamNotSynced {
		t.Fatalf("unsynced team = %v", err)
	}
}

// A queued team removal stays valid only while its team is still mapped:
// deleting or re-pointing the mapping stops managing that team. Account
// removals do not depend on mappings.
func TestRemovalStillManaged(t *testing.T) {
	a := New()
	team := provider.TeamRef{TeamID: uuid.New(), ExternalRef: "1234"}
	d := provider.DesiredAccess{Teams: []provider.TeamRef{team}, Mapped: []provider.TeamRef{team}}
	removal := teamAction(provider.ActionRemoveFromTeam, "7", ghTeam{ID: 1234})
	if !a.RemovalStillManaged(provider.Instance{}, d, removal) {
		t.Fatal("a removal from a mapped team is stale")
	}
	d.Mapped = nil
	if a.RemovalStillManaged(provider.Instance{}, d, removal) {
		t.Fatal("a removal from an unmapped team is still valid")
	}
	d.Mapped = []provider.TeamRef{{TeamID: team.TeamID, ExternalRef: "9999"}}
	if a.RemovalStillManaged(provider.Instance{}, d, removal) {
		t.Fatal("a removal from a re-pointed mapping is still valid")
	}
	for _, act := range []provider.Action{
		accountAction(provider.ActionRemoveFromOrg, "7", true),
		accountAction(provider.ActionCancelInvitation, "7", true),
	} {
		if !a.RemovalStillManaged(provider.Instance{}, d, act) {
			t.Fatalf("%s is stale without a mapping", act.Kind)
		}
	}
	if a.RemovalStillManaged(provider.Instance{}, d, provider.Action{Kind: provider.ActionRemoveFromTeam, Removal: true}) {
		t.Fatal("a team removal without a target is valid")
	}
}

// A team addition never invites: GitHub would invite someone who is not in
// the organization, without the invitation's proof.
func TestProvisionTeamAdditionNeedsInvitation(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	f.AddUser(githubfake.User{ID: 8, Login: "newbie"})
	team := syncTeam(t, a, env, inst, "platform")
	_, err := a.Apply(t.Context(), env, inst, teamAction(provider.ActionAddToTeam, "8", ghTeam{ID: mustInt(team.ExternalRef)}))
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Code != CodeNotInvited || !pe.Retryable || len(f.Invitations()) != 0 {
		t.Fatalf("addition = %v, invitations %v", err, f.Invitations())
	}
}

func mustInt(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		panic(err)
	}
	return n
}

// Actions act on the account's ID: a rename refreshes the link's login,
// and someone else taking the old login is never reached.
func TestProvisionActsOnTheAccountID(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	f.AddUser(githubfake.User{ID: 10, Login: "old-name"})
	f.RenameUser(10, "new-name")
	f.AddUser(githubfake.User{ID: 11, Login: "old-name"}) // the old login, reused
	d := desired(10, "old-name", true, false)
	plan, err := a.Plan(t.Context(), env, inst, d)
	if err != nil || !slices.Equal(kinds(plan), []string{"update_account", "invite"}) {
		t.Fatalf("plan = %v, %v", kinds(plan), err)
	}
	outs := applyAll(t, a, env, inst, plan)
	if l := outs[0].Link; l == nil || l.ExternalID != "10" || l.Login != "new-name" || outs[0].Changed {
		t.Fatalf("update outcome = %+v", outs[0])
	}
	if inv := f.Invitations(); len(inv) != 1 || inv[0].Invitee != 10 {
		t.Fatalf("invitations = %+v", inv)
	}
}

// An invitation someone else made is there, but proves nothing; a refused
// invitation is classified (seats).
func TestProvisionInvitationProof(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	f.AddUser(githubfake.User{ID: 20, Login: "by-hand"})
	f.AddUser(githubfake.User{ID: 99, Login: "admin-person"})
	f.InviteAs(20, "admin-person")
	out, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionInvite, "20"))
	if err != nil || out.Changed || out.Proof != nil {
		t.Fatalf("invite with a human's invitation = %+v, %v", out, err)
	}
	f.AddUser(githubfake.User{ID: 21, Login: "late"})
	f.LimitSeats(1)
	if _, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionInvite, "21")); code(err) != CodeSeatLimit {
		t.Fatalf("invite without seats = %v", err)
	}
}

// Rate limits keep their reset time; missing answers and server errors are
// retryable, refusals are not.
func TestProvisionErrorClassification(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	f.AddUser(githubfake.User{ID: 30, Login: "rl"})
	f.Fail(http.MethodGet, "/user/30", http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}})
	_, err := a.Plan(t.Context(), env, inst, desired(30, "rl", true, false))
	if c, retryable, after := provider.Classify(err); c != CodeRateLimited || !retryable || after != 2*time.Minute {
		t.Fatalf("rate limit = %s %v %s", c, retryable, after)
	}
	f.Fail(http.MethodGet, "/user/30", http.StatusBadGateway, nil)
	if _, retryable, _ := provider.Classify(func() error { _, err := a.Plan(t.Context(), env, inst, desired(30, "rl", true, false)); return err }()); !retryable {
		t.Fatal("a server error is not retryable")
	}
	f.Fail(http.MethodPost, "/orgs/"+f.Org().Login+"/invitations", http.StatusForbidden, nil)
	if _, err := a.Apply(t.Context(), env, inst, plan2(provider.ActionInvite, "30")); code(err) != CodeForbidden {
		t.Fatalf("forbidden = %v", err)
	}
	if c, retryable, _ := provider.Classify(classify(&APIError{Status: 503})); c != CodeUnavailable || !retryable {
		t.Fatalf("a 503 = %s %v", c, retryable)
	}
}

// SyncTeams creates "create" teams once, keeps them by ID, and never takes
// over a team of the same name that lacks this instance's mark; "existing"
// mappings keep the team the admin picked.
func TestSyncTeamsNeverTakesOver(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	taken := f.AddTeam("design", "the designers' own team")
	lost := f.AddTeam("ops", teamMark(inst)+": created by an attempt whose answer was lost")
	picked := f.AddTeam("Release Crew", "")
	f.AddEnterpriseTeam("platform")
	m := func(slug, mode, ref string) provider.TeamMapping {
		return provider.TeamMapping{TeamID: uuid.New(), TeamSlug: slug, TeamName: slug, Mode: mode, ExternalRef: ref}
	}
	mappings := []provider.TeamMapping{
		m("design", provider.MappingCreate, ""), m("ops", provider.MappingCreate, ""), m("platform", provider.MappingCreate, ""),
		m("release", provider.MappingExisting, strconv.FormatInt(picked.ID, 10)), m("gone", provider.MappingExisting, "424242"),
	}
	refs, err := a.SyncTeams(t.Context(), env, inst, mappings)
	if err != nil {
		t.Fatal(err)
	}
	byTeam := map[uuid.UUID]string{}
	for _, r := range refs {
		byTeam[r.TeamID] = r.ExternalRef
	}
	if _, ok := byTeam[mappings[0].TeamID]; ok {
		t.Fatal("a team of the same name that is not Orglane's was taken over")
	}
	if byTeam[mappings[1].TeamID] != strconv.FormatInt(lost.ID, 10) || byTeam[mappings[3].TeamID] != strconv.FormatInt(picked.ID, 10) {
		t.Fatalf("refs = %+v", refs)
	}
	if _, ok := byTeam[mappings[4].TeamID]; ok {
		t.Fatal("a missing existing team was synced")
	}
	platform := byTeam[mappings[2].TeamID]
	if platform == "" {
		t.Fatal("an enterprise team's name blocked the organization team")
	}
	// Synced again with the recorded references: nothing new is created.
	mappings[2].ExternalRef = platform
	n := len(f.Teams())
	if _, err := a.SyncTeams(t.Context(), env, inst, mappings); err != nil || len(f.Teams()) != n {
		t.Fatalf("second sync created teams: %v", err)
	}
	if desc := taken.Description; f.Teams()[0].Description != desc {
		t.Fatal("the other team was changed")
	}

	teams, err := a.ProviderTeams(t.Context(), env, inst)
	if err != nil {
		t.Fatal(err)
	}
	managed := map[string]bool{}
	for _, pt := range teams {
		managed[pt.Slug] = pt.Managed
	}
	if len(teams) != 4 || !managed["ops"] || !managed["platform"] || managed["design"] || managed["release-crew"] {
		t.Fatalf("provider teams = %+v", teams)
	}
	if err := a.ValidateTeamMapping(inst, provider.MappingExisting, "release-crew"); err == nil {
		t.Fatal("an existing mapping by slug was accepted")
	}
	if a.ValidateTeamMapping(inst, provider.MappingExisting, "12") != nil || a.ValidateTeamMapping(inst, provider.MappingCreate, "") != nil {
		t.Fatal("valid mappings refused")
	}
}

// A delayed removal acts on the account's immutable ID, not the login saved
// in the plan: after a rename and someone else taking the old login, the
// intended account loses access and the other account is untouched.
func TestProvisionDelayedRemovalsUseTheBoundAccount(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	team := syncTeam(t, a, env, inst, "platform")
	teamID := mustInt(team.ExternalRef)
	f.AddUser(githubfake.User{ID: 40, Login: "dev"})
	f.AddMember(40, githubfake.RoleMember)
	f.SetTeamMember(teamID, 40, githubfake.TeamMember)
	f.AddUser(githubfake.User{ID: 41, Login: "someone-else"})
	f.AddMember(41, githubfake.RoleMember)

	// Team removal planned under "dev"; both the account and the old login
	// change before it runs.
	d := desired(40, "dev", true, true)
	d.Teams, d.Mapped = nil, []provider.TeamRef{team}
	plan, err := a.Plan(t.Context(), env, inst, d)
	if err != nil || !slices.Equal(kinds(plan), []string{"remove_from_team:" + team.ExternalRef + "!"}) {
		t.Fatalf("team removal plan = %v, %v", kinds(plan), err)
	}
	f.RenameUser(40, "dev-renamed")
	f.RenameUser(41, "dev") // takes the old login
	f.SetTeamMember(teamID, 41, githubfake.TeamMember)
	if out, err := a.Apply(t.Context(), env, inst, plan.Actions[0]); err != nil || !out.Changed {
		t.Fatalf("team removal = %+v, %v", out, err)
	}
	if m := f.TeamMembers(teamID); m[40] != "" || m[41] != "member/active" {
		t.Fatalf("team members after removal = %v", m)
	}

	// Organization removal planned under the new login; rename again and
	// reuse the login. The bound account is removed, never the new holder.
	plan, err = a.Plan(t.Context(), env, inst, desired(40, "dev-renamed", false, true))
	if err != nil || !slices.Equal(kinds(plan), []string{"remove_from_org!"}) {
		t.Fatalf("org removal plan = %v, %v", kinds(plan), err)
	}
	f.RenameUser(40, "dev-renamed-again")
	f.RenameUser(41, "dev-renamed")
	if out, err := a.Apply(t.Context(), env, inst, plan.Actions[0]); err != nil || !out.Changed {
		t.Fatalf("org removal = %+v, %v", out, err)
	}
	if m := f.Members(); m[40] != "" || m[41] != githubfake.RoleMember {
		t.Fatalf("members after removal = %v", m)
	}
}

// A team's slug is not a stable identifier: renaming the mapped team releases
// it, and an unrelated team can take it while an action runs. Team membership
// reads and changes are addressed by the organization's and the mapped team's
// immutable IDs, so an add or a removal never reaches the team that took the
// slug.
func TestProvisionTeamMembershipsUseTheMappedTeamID(t *testing.T) {
	// reuseSlug renames the mapped team and gives the unrelated team the
	// released slug immediately before the membership read.
	reuseSlug := func(f *githubfake.Fake, mappedID int64) func(*http.Request) bool {
		renamed := false
		return func(r *http.Request) bool {
			if renamed || r.Method != http.MethodGet {
				return false
			}
			if r.URL.Path == "/orgs/"+f.Org().Login+"/teams" || r.URL.Path == "/organizations/"+strconv.FormatInt(f.Org().ID, 10)+"/team/"+strconv.FormatInt(mappedID, 10) {
				renamed = true
				return true
			}
			return false
		}
	}
	rename := func(f *githubfake.Fake, mappedID, otherID int64) func() {
		return func() {
			f.RenameTeam(mappedID, "platform-renamed")
			f.RenameTeam(otherID, "platform")
		}
	}
	t.Run("removal", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		team := syncTeam(t, a, env, inst, "platform")
		teamID := mustInt(team.ExternalRef)
		other := f.AddTeam("unrelated", "not Orglane's")
		f.AddUser(githubfake.User{ID: 60, Login: "dev"})
		f.AddMember(60, githubfake.RoleMember)
		f.SetTeamMember(teamID, 60, githubfake.TeamMember)
		f.SetTeamMember(other.ID, 60, githubfake.TeamMember)
		d := desired(60, "dev", true, true)
		d.Teams, d.Mapped = nil, []provider.TeamRef{team}
		plan, err := a.Plan(t.Context(), env, inst, d)
		if err != nil || !slices.Equal(kinds(plan), []string{"remove_from_team:" + team.ExternalRef + "!"}) {
			t.Fatalf("plan = %v, %v", kinds(plan), err)
		}
		base := env.client.Transport
		match := reuseSlug(f, teamID)
		env.client.Transport = &interleave{base: base, match: match, after: true, fn: rename(f, teamID, other.ID)}
		out, err := a.Apply(t.Context(), env, inst, plan.Actions[0])
		if err != nil || !out.Changed {
			t.Fatalf("removal = %+v, %v", out, err)
		}
		if m := f.TeamMembers(teamID); m[60] != "" {
			t.Fatalf("the mapped team kept the user: %v", m)
		}
		if m := f.TeamMembers(other.ID); m[60] != "member/active" {
			t.Fatalf("the team that took the slug was changed: %v", m)
		}
	})
	t.Run("addition", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		team := syncTeam(t, a, env, inst, "platform")
		teamID := mustInt(team.ExternalRef)
		other := f.AddTeam("unrelated", "not Orglane's")
		f.AddUser(githubfake.User{ID: 61, Login: "newbie"})
		f.AddMember(61, githubfake.RoleMember)
		base := env.client.Transport
		match := reuseSlug(f, teamID)
		env.client.Transport = &interleave{base: base, match: match, after: true, fn: rename(f, teamID, other.ID)}
		out, err := a.Apply(t.Context(), env, inst, teamAction(provider.ActionAddToTeam, "61", ghTeam{ID: teamID, Slug: "platform"}))
		if err != nil || !out.Changed {
			t.Fatalf("addition = %+v, %v", out, err)
		}
		if m := f.TeamMembers(teamID); m[61] != "member/active" {
			t.Fatalf("the mapped team did not get the user: %v", m)
		}
		if m := f.TeamMembers(other.ID); len(m) != 0 {
			t.Fatalf("the team that took the slug was changed: %v", m)
		}
	})
	t.Run("deleted mapped team", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		team := syncTeam(t, a, env, inst, "platform")
		teamID := mustInt(team.ExternalRef)
		f.AddUser(githubfake.User{ID: 62, Login: "dev"})
		f.AddMember(62, githubfake.RoleMember)
		f.SetTeamMember(teamID, 62, githubfake.TeamMember)
		f.DeleteTeam(teamID)
		// Another team takes the deleted team's slug: the mapped ID must not
		// be resolved through it.
		other := f.AddTeam("platform", "not Orglane's")
		for _, act := range []provider.Action{
			teamAction(provider.ActionRemoveFromTeam, "62", ghTeam{ID: teamID, Slug: "platform"}),
			teamAction(provider.ActionAddToTeam, "62", ghTeam{ID: teamID, Slug: "platform"}),
		} {
			_, err := a.Apply(t.Context(), env, inst, act)
			if c, retryable, _ := provider.Classify(err); c != CodeTeamNotSynced || !retryable {
				t.Fatalf("%s = %v, want retryable %s", act.Kind, err, CodeTeamNotSynced)
			}
		}
		if m := f.TeamMembers(other.ID); len(m) != 0 {
			t.Fatalf("the team that took the slug was changed: %v", m)
		}
	})
}

// interleave runs fn once while the adapter is sending the request that
// matches (method and path, or a custom match), before GitHub answers it, or
// just after GitHub answered it. It simulates an external rename racing an
// action.
type interleave struct {
	base   http.RoundTripper
	method string
	path   string
	match  func(*http.Request) bool
	after  bool
	once   sync.Once
	fn     func()
}

func (i *interleave) RoundTrip(r *http.Request) (*http.Response, error) {
	match := i.match != nil && i.match(r) || i.match == nil && r.Method == i.method && r.URL.Path == i.path
	if match && !i.after {
		i.once.Do(i.fn)
	}
	res, err := i.base.RoundTrip(r)
	if match && i.after {
		i.once.Do(i.fn)
	}
	return res, err
}

// A login can be released by a rename and taken by another account while an
// action is running. GitHub's membership mutations are addressed by login,
// so every action re-resolves the bound numeric ID immediately before them:
// a reused login is the retryable login_changed and nothing is removed or
// added. This reproduces the reviewer's during-apply rename.
func TestProvisionRechecksTheBoundAccountBeforeActing(t *testing.T) {
	rename := func(f *githubfake.Fake) func() {
		return func() {
			f.RenameUser(40, "dev-renamed")
			f.RenameUser(41, "dev") // the old login, reused
		}
	}
	t.Run("team removal", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		team := syncTeam(t, a, env, inst, "platform")
		teamID := mustInt(team.ExternalRef)
		for _, u := range []githubfake.User{{ID: 40, Login: "dev"}, {ID: 41, Login: "someone-else"}} {
			f.AddUser(u)
			f.AddMember(u.ID, githubfake.RoleMember)
			f.SetTeamMember(teamID, u.ID, githubfake.TeamMember)
		}
		d := desired(40, "dev", true, true)
		d.Teams, d.Mapped = nil, []provider.TeamRef{team}
		plan, err := a.Plan(t.Context(), env, inst, d)
		if err != nil || !slices.Equal(kinds(plan), []string{"remove_from_team:" + team.ExternalRef + "!"}) {
			t.Fatalf("plan = %v, %v", kinds(plan), err)
		}
		// The rename happens while the team read is in flight.
		base := env.client.Transport
		env.client.Transport = &interleave{base: base, method: http.MethodGet, path: "/organizations/" + strconv.FormatInt(f.Org().ID, 10) + "/team/" + team.ExternalRef, fn: rename(f)}
		_, err = a.Apply(t.Context(), env, inst, plan.Actions[0])
		if c, retryable, _ := provider.Classify(err); err == nil || c != CodeLoginChanged || !retryable {
			t.Fatalf("removal = %v, want retryable %s", err, CodeLoginChanged)
		}
		if m := f.TeamMembers(teamID); m[40] != "member/active" || m[41] != "member/active" {
			t.Fatalf("team members after the race = %v", m)
		}
		// The retry acts on the bound ID (now dev-renamed) and removes 40 only.
		env.client.Transport = base
		out, err := a.Apply(t.Context(), env, inst, plan.Actions[0])
		if err != nil || !out.Changed {
			t.Fatalf("removal retry = %+v, %v", out, err)
		}
		if m := f.TeamMembers(teamID); m[40] != "" || m[41] != "member/active" {
			t.Fatalf("team members after the retry = %v", m)
		}
	})
	t.Run("organization removal", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		f.AddUser(githubfake.User{ID: 40, Login: "dev"})
		f.AddUser(githubfake.User{ID: 41, Login: "someone-else"})
		f.AddMember(40, githubfake.RoleMember)
		f.AddMember(41, githubfake.RoleMember)
		plan, err := a.Plan(t.Context(), env, inst, desired(40, "dev", false, true))
		if err != nil || !slices.Equal(kinds(plan), []string{"remove_from_org!"}) {
			t.Fatalf("plan = %v, %v", kinds(plan), err)
		}
		// The rename happens after the membership read returned account 40.
		base := env.client.Transport
		env.client.Transport = &interleave{base: base, method: http.MethodGet, path: "/orgs/" + f.Org().Login + "/memberships/dev", after: true, fn: rename(f)}
		_, err = a.Apply(t.Context(), env, inst, plan.Actions[0])
		if c, retryable, _ := provider.Classify(err); err == nil || c != CodeLoginChanged || !retryable {
			t.Fatalf("removal = %v, want retryable %s", err, CodeLoginChanged)
		}
		if m := f.Members(); m[40] != githubfake.RoleMember || m[41] != githubfake.RoleMember {
			t.Fatalf("members after the race = %v", m)
		}
		env.client.Transport = base
		out, err := a.Apply(t.Context(), env, inst, plan.Actions[0])
		if err != nil || !out.Changed {
			t.Fatalf("removal retry = %+v, %v", out, err)
		}
		if m := f.Members(); m[40] != "" || m[41] != githubfake.RoleMember {
			t.Fatalf("members after the retry = %v", m)
		}
	})
	t.Run("invitation cancellation", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		f.AddUser(githubfake.User{ID: 40, Login: "dev"})
		f.AddUser(githubfake.User{ID: 41, Login: "someone-else"})
		invitee := f.InviteAs(40, f.BotLogin())
		other := f.InviteAs(41, f.BotLogin())
		plan, err := a.Plan(t.Context(), env, inst, desired(40, "dev", false, true))
		if err != nil || !slices.Equal(kinds(plan), []string{"cancel_invitation!"}) {
			t.Fatalf("plan = %v, %v", kinds(plan), err)
		}
		// The rename happens before the invitation listing: matching by the
		// old login would find account 41's invitation.
		base := env.client.Transport
		env.client.Transport = &interleave{base: base, method: http.MethodGet, path: "/orgs/" + f.Org().Login + "/invitations", fn: rename(f)}
		_, err = a.Apply(t.Context(), env, inst, plan.Actions[0])
		if c, retryable, _ := provider.Classify(err); err == nil || c != CodeLoginChanged || !retryable {
			t.Fatalf("cancellation = %v, want retryable %s", err, CodeLoginChanged)
		}
		if inv := f.Invitations(); len(inv) != 2 || inv[0].ID != invitee.ID || inv[1].ID != other.ID {
			t.Fatalf("invitations after the race = %+v", inv)
		}
		env.client.Transport = base
		out, err := a.Apply(t.Context(), env, inst, plan.Actions[0])
		if err != nil || !out.Changed {
			t.Fatalf("cancellation retry = %+v, %v", out, err)
		}
		if inv := f.Invitations(); len(inv) != 1 || inv[0].ID != other.ID {
			t.Fatalf("invitations after the retry = %+v", inv)
		}
	})
	t.Run("team addition", func(t *testing.T) {
		f, env, inst := fakeFixture(t)
		a := New()
		team := syncTeam(t, a, env, inst, "platform")
		teamID := mustInt(team.ExternalRef)
		for _, u := range []githubfake.User{{ID: 40, Login: "dev"}, {ID: 41, Login: "someone-else"}} {
			f.AddUser(u)
			f.AddMember(u.ID, githubfake.RoleMember)
		}
		// The rename happens after the team membership read, before the PUT.
		base := env.client.Transport
		env.client.Transport = &interleave{base: base, method: http.MethodGet, path: "/organizations/" + strconv.FormatInt(f.Org().ID, 10) + "/team/" + team.ExternalRef + "/memberships/dev", after: true, fn: rename(f)}
		_, err := a.Apply(t.Context(), env, inst, teamAction(provider.ActionAddToTeam, "40", ghTeam{ID: teamID, Slug: "platform"}))
		if c, retryable, _ := provider.Classify(err); err == nil || c != CodeLoginChanged || !retryable {
			t.Fatalf("addition = %v, want retryable %s", err, CodeLoginChanged)
		}
		if m := f.TeamMembers(teamID); len(m) != 0 {
			t.Fatalf("team members after the race = %v", m)
		}
		env.client.Transport = base
		out, err := a.Apply(t.Context(), env, inst, teamAction(provider.ActionAddToTeam, "40", ghTeam{ID: teamID, Slug: "platform"}))
		if err != nil || !out.Changed {
			t.Fatalf("addition retry = %+v, %v", out, err)
		}
		if m := f.TeamMembers(teamID); m[40] != "member/active" || m[41] != "" {
			t.Fatalf("team members after the retry = %v", m)
		}
	})
}

// The capability matrix reflects what this adapter implements.
func TestManifestCapabilities(t *testing.T) {
	support := map[provider.Feature]provider.Support{}
	for _, f := range New().Manifest().Features {
		support[f.Feature] = f.Support
	}
	for _, f := range []provider.Feature{
		provider.FeatureAccountsLink, provider.FeatureAccountsInvite, provider.FeatureTeamsSync,
		provider.FeatureDeprovisionTeamRemoval, provider.FeatureDeprovisionOrgRemoval,
		provider.FeatureEventsWebhooks,
	} {
		if support[f] != provider.Supported {
			t.Errorf("%s = %q, want supported", f, support[f])
		}
	}
	for _, f := range []provider.Feature{
		provider.FeatureSSO, provider.FeatureDeepLinks, provider.FeatureAccountsCreate,
		provider.FeatureDeprovisionSuspend, provider.FeatureDeprovisionRevokeTokens,
		provider.FeatureLifecycleInstall,
		provider.FeatureLifecycleUpgrade, provider.FeatureLifecycleSnapshot, provider.FeatureLifecycleBackup,
	} {
		if support[f] != provider.Unsupported {
			t.Errorf("%s = %q, want unsupported", f, support[f])
		}
	}
}

// Members lists the organization's members, paged.
func TestMembersPaged(t *testing.T) {
	f, env, inst := fakeFixture(t)
	for i := int64(1); i <= 130; i++ {
		f.AddUser(githubfake.User{ID: i, Login: "user-" + strconv.FormatInt(i, 10)})
		f.AddMember(i, githubfake.RoleMember)
	}
	f.AddUser(githubfake.User{ID: 500, Login: "invited"})
	f.InviteAs(500, f.BotLogin())
	members, err := New().Members(t.Context(), env, inst)
	if err != nil || len(members) != 130 || members[129].ExternalID != "130" || members[0].Realm != Realm {
		t.Fatalf("members = %d, %v", len(members), err)
	}
}
