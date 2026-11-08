//go:build integration

package apps_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/github/githubfake"
	"github.com/orglane/orglane/providers/provider"
)

// GitHub provisioning end to end in the core: real PostgreSQL, the real
// jobs, removal guard and adapter, against the fake GitHub that validates
// every request and response against GitHub's OpenAPI description.

// newGitHubHarness is the link harness with GitHub's default policy
// (mapped teams: seats cost money).
func newGitHubHarness(t *testing.T) *linkHarness {
	t.Helper()
	h := newLinkHarness(t)
	policy := provider.MappedTeams
	inst, err := h.svc.UpdateInstance(h.ctx, audit.System, h.inst.ID, apps.InstanceUpdate{AccessPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	h.inst = inst
	h.drain()
	return h
}

// linkAs links the person's GitHub account (a new GitHub user).
func (h *linkHarness) linkAs(p store.Person, id int64, login string) store.AccountLink {
	h.t.Helper()
	h.gh.AddUser(githubfake.User{ID: id, Login: login})
	h.gh.AuthorizeAs(id)
	link, err := h.link(p)
	if err != nil {
		h.t.Fatal(err)
	}
	return link
}

func (h *linkHarness) teamRef(team store.Team) int64 {
	h.t.Helper()
	m, err := store.New(h.pool).GetAppTeamMapping(h.ctx, store.GetAppTeamMappingParams{InstanceID: h.inst.ID, TeamID: team.ID})
	if err != nil || m.ExternalRef == nil {
		h.t.Fatalf("mapping of %s = %+v, %v", team.Slug, m, err)
	}
	n, err := strconv.ParseInt(*m.ExternalRef, 10, 64)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *linkHarness) leave(team store.Team, p store.Person) {
	h.t.Helper()
	if err := h.ids.RemoveTeamMember(h.ctx, audit.System, team.ID, p.ID); err != nil {
		h.t.Fatal(err)
	}
}

// A person of a mapped team waits for the employee step, is invited by
// user ID once linked (Orglane's grant, proven by GitHub's answer), joins
// the GitHub team, and loses organization membership through the removal
// guard when they leave the team.
func TestGitHubProvisioning(t *testing.T) {
	h := newGitHubHarness(t)
	alice := h.person("alice")
	platform := h.team("platform", alice)
	h.mapTeam(h.inst, platform)
	h.drain()
	ghTeam := h.teamRef(platform)
	if ts := h.gh.Teams(); len(ts) != 1 || ts[0].Slug != "platform" {
		t.Fatalf("GitHub teams = %+v", ts)
	}
	r := h.result(h.inst, alice)
	if r.Status != apps.StatusPendingEmployeeAction || r.EmployeeStep == nil || *r.EmployeeStep != githubprovider.StepLinkAccount || len(h.gh.Invitations()) != 0 {
		t.Fatalf("before linking: %+v", r)
	}

	h.linkAs(alice, 1001, "alice-gh")
	h.drain()
	r = h.result(h.inst, alice)
	if r.Status != apps.StatusDone || !r.GrantedByOrglane || r.ErrorCode != nil {
		t.Fatalf("after linking: %+v", r)
	}
	inv := h.gh.Invitations()
	if len(inv) != 1 || inv[0].Invitee != 1001 || inv[0].Inviter != h.gh.BotLogin() || h.gh.TeamMembers(ghTeam)[1001] != "member/pending" {
		t.Fatalf("invitations %+v, team %v", inv, h.gh.TeamMembers(ghTeam))
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'kind' = 'invite' AND metadata->>'proof' = 'response'`, string(audit.ProvisioningActionApplied)); n != 1 {
		t.Fatalf("%d audited invitations", n)
	}
	// No provisioned link: the person's own (oauth) link stays the only one.
	if n := h.count(`SELECT count(*) FROM account_links WHERE person_id = $1`, alice.ID); n != 1 {
		t.Fatalf("%d links", n)
	}

	// Accepting changes nothing to do; a drift check plans again.
	h.gh.Accept(1001)
	applied := h.auditCount(audit.ProvisioningActionApplied)
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if h.auditCount(audit.ProvisioningActionApplied) != applied || h.gh.TeamMembers(ghTeam)[1001] != "member/active" {
		t.Fatal("a drift check changed GitHub")
	}

	// Leaving the only mapped team takes the organization membership away,
	// through the guard (a small removal applies without confirmation).
	h.leave(platform, alice)
	h.drain()
	if _, ok := h.gh.Members()[1001]; ok {
		t.Fatal("alice is still a member")
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, alice.ID); n != 1 {
		t.Fatalf("%d applied removals", n)
	}
	if r := h.result(h.inst, alice); r.Status != apps.StatusDone || r.Access {
		t.Fatalf("after leaving: %+v", r)
	}
}

// Leaving one of several mapped teams removes only that GitHub team through
// the removal guard; organization membership stays.
func TestGitHubTeamRemovalThroughTheGuard(t *testing.T) {
	h := newGitHubHarness(t)
	alice := h.person("alice")
	platform, backend := h.team("platform", alice), h.team("backend", alice)
	h.mapTeam(h.inst, platform)
	h.mapTeam(h.inst, backend)
	h.linkAs(alice, 1001, "alice-gh")
	h.drain()
	h.gh.Accept(1001)
	platformRef, backendRef := h.teamRef(platform), h.teamRef(backend)
	h.leave(platform, alice)
	h.drain()
	if m := h.gh.TeamMembers(platformRef); m[1001] != "" {
		t.Fatalf("left the unmapped team: %v", m)
	}
	if m := h.gh.TeamMembers(backendRef); m[1001] != "member/active" {
		t.Fatalf("lost the remaining team: %v", m)
	}
	if _, ok := h.gh.Members()[1001]; !ok {
		t.Fatal("organization membership was removed")
	}
	if r := h.result(h.inst, alice); r.Status != apps.StatusDone || !r.Access || !r.GrantedByOrglane {
		t.Fatalf("after leaving one team: %+v", r)
	}
}

// A pending invitation is canceled when access goes away before it is
// accepted; owners are never removed.
func TestGitHubInvitationCancelAndOwnerProtection(t *testing.T) {
	h := newGitHubHarness(t)
	alice, olga := h.person("alice"), h.person("olga")
	platform := h.team("platform", alice, olga)
	h.mapTeam(h.inst, platform)
	h.linkAs(alice, 1001, "alice-gh")
	h.linkAs(olga, 1002, "olga-gh")
	h.drain()
	h.gh.Accept(1002)
	h.gh.SetRole(1002, githubfake.RoleOwner) // promoted on GitHub
	h.gh.AddUser(githubfake.User{ID: 1, Login: "founder"})
	h.gh.AddMember(1, githubfake.RoleOwner)

	h.leave(platform, alice)
	h.leave(platform, olga)
	h.drain()
	if inv := h.gh.Invitations(); len(inv) != 0 {
		t.Fatalf("invitations = %+v", inv)
	}
	if h.gh.Members()[1002] != githubfake.RoleOwner {
		t.Fatal("an owner was removed")
	}
	r := h.result(h.inst, olga)
	if r.Status != apps.StatusFailed || r.ErrorCode == nil || *r.ErrorCode != githubprovider.CodeOwnerProtected {
		t.Fatalf("owner result = %+v", r)
	}
}

// People who were in the organization before Orglane are not Orglane's:
// they join their teams but are never removed, and are reported.
func TestGitHubExistingMembersAreNotRemoved(t *testing.T) {
	h := newGitHubHarness(t)
	bob := h.person("bob")
	h.gh.AddUser(githubfake.User{ID: 2002, Login: "bob-gh"})
	h.gh.AddMember(2002, githubfake.RoleMember)
	platform := h.team("platform", bob)
	h.mapTeam(h.inst, platform)
	h.linkAs(bob, 2002, "bob-gh")
	h.drain()
	ghTeam := h.teamRef(platform)
	r := h.result(h.inst, bob)
	if r.Status != apps.StatusDone || r.GrantedByOrglane || h.gh.TeamMembers(ghTeam)[2002] != "member/active" || len(h.gh.Invitations()) != 0 {
		t.Fatalf("existing member: %+v, team %v", r, h.gh.TeamMembers(ghTeam))
	}
	h.leave(platform, bob)
	h.drain()
	if _, ok := h.gh.Members()[2002]; !ok {
		t.Fatal("an existing member was removed")
	}
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	unmanaged, err := h.svc.UnmanagedAccounts(h.ctx, h.inst.ID)
	if err != nil || !slices.ContainsFunc(unmanaged, func(u store.AppUnmanagedAccount) bool { return u.ExternalID == "2002" }) {
		t.Fatalf("unmanaged = %+v, %v", unmanaged, err)
	}
}

// Ownership belongs to the account Orglane invited: when the person links
// another account, the invited one becomes unmanaged and the new one is
// not removable just because the person's earlier account was Orglane's.
func TestGitHubOwnershipFollowsTheLinkedAccount(t *testing.T) {
	h := newGitHubHarness(t)
	alice := h.person("alice")
	platform := h.team("platform", alice)
	h.mapTeam(h.inst, platform)
	h.linkAs(alice, 1001, "alice-gh")
	h.drain()
	h.gh.Accept(1001)
	if !h.result(h.inst, alice).GrantedByOrglane {
		t.Fatal("the invitation was not Orglane's")
	}
	// Her second account was in the organization already.
	h.gh.AddUser(githubfake.User{ID: 1009, Login: "alice-work"})
	h.gh.AddMember(1009, githubfake.RoleMember)
	h.linkAs(alice, 1009, "alice-work")
	h.drain()
	if r := h.result(h.inst, alice); r.GrantedByOrglane {
		t.Fatalf("ownership moved to the new account: %+v", r)
	}
	h.leave(platform, alice)
	h.drain()
	members := h.gh.Members()
	if _, ok := members[1009]; !ok {
		t.Fatal("the pre-existing account was removed")
	}
	if _, ok := members[1001]; !ok {
		t.Fatal("the replaced account was removed (it is reported, not removed)")
	}
}

// "existing" mappings use the team the admin picked; the provider team
// list marks the teams Orglane created.
func TestGitHubExistingTeamMapping(t *testing.T) {
	h := newGitHubHarness(t)
	carol := h.person("carol")
	crew := h.gh.AddTeam("Release Crew", "made on GitHub")
	release := h.team("release", carol)
	if _, err := h.svc.SetTeamMapping(h.ctx, audit.System, h.inst.ID, release.ID, apps.TeamMappingInput{Mode: provider.MappingExisting, ExternalRef: "release-crew"}); err == nil {
		t.Fatal("a mapping by slug was accepted")
	}
	if _, err := h.svc.SetTeamMapping(h.ctx, audit.System, h.inst.ID, release.ID, apps.TeamMappingInput{Mode: provider.MappingExisting, ExternalRef: strconv.FormatInt(crew.ID, 10)}); err != nil {
		t.Fatal(err)
	}
	h.mapTeam(h.inst, h.team("platform"))
	h.linkAs(carol, 3003, "carol-gh")
	h.drain()
	if h.gh.TeamMembers(crew.ID)[3003] != "member/pending" {
		t.Fatalf("crew = %v", h.gh.TeamMembers(crew.ID))
	}
	teams, err := h.svc.ProviderTeams(h.ctx, h.inst.ID)
	if err != nil || len(teams) != 2 {
		t.Fatalf("provider teams = %+v, %v", teams, err)
	}
	for _, pt := range teams {
		if pt.Managed != (pt.Slug == "platform") {
			t.Fatalf("provider team %+v", pt)
		}
	}
}

// A rate limit snoozes the person's job until GitHub's reset instead of
// failing it.
func TestGitHubProvisioningRateLimit(t *testing.T) {
	h := newGitHubHarness(t)
	dave := h.person("dave")
	platform := h.team("platform", dave)
	h.mapTeam(h.inst, platform)
	h.drain()
	h.gh.Fail("POST", "/orgs/"+h.gh.Org().Login+"/invitations", 429, map[string][]string{"Retry-After": {"30"}})
	h.linkAs(dave, 4004, "dave-gh")
	h.drain()
	if r := h.result(h.inst, dave); r.Status != apps.StatusDone || len(h.gh.Invitations()) != 1 {
		t.Fatalf("after a rate limit: %+v", r)
	}
}

// Revoking the verified link ends Orglane's ownership of the invited account:
// it is reported, never removed, and a later account link starts unmanaged.
func TestGitHubRevokedLinkEndsOwnership(t *testing.T) {
	h := newGitHubHarness(t)
	alice := h.person("alice")
	platform := h.team("platform", alice)
	h.mapTeam(h.inst, platform)
	link := h.linkAs(alice, 1001, "alice-gh")
	h.drain()
	h.gh.Accept(1001)
	if !h.result(h.inst, alice).GrantedByOrglane {
		t.Fatal("the invitation was not Orglane's")
	}
	if err := h.svc.RevokeAccountLink(h.ctx, personActor(alice.ID), alice.ID, link.ID, apps.RevokedBySelf); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if r := h.result(h.inst, alice); r.GrantedByOrglane {
		t.Fatalf("ownership survived the revoked link: %+v", r)
	}
	h.leave(platform, alice)
	h.drain()
	if _, ok := h.gh.Members()[1001]; !ok {
		t.Fatal("an account whose link was revoked was removed")
	}
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	unmanaged, err := h.svc.UnmanagedAccounts(h.ctx, h.inst.ID)
	if err != nil || !slices.ContainsFunc(unmanaged, func(u store.AppUnmanagedAccount) bool { return u.ExternalID == "1001" }) {
		t.Fatalf("unmanaged = %+v, %v", unmanaged, err)
	}
}

// A GitHub rename is observed by the next plan and refreshes the verified
// link's informational login; access is unchanged.
func TestGitHubRenameRefreshesTheVerifiedLink(t *testing.T) {
	h := newGitHubHarness(t)
	alice := h.person("alice")
	h.mapTeam(h.inst, h.team("platform", alice))
	h.linkAs(alice, 1001, "alice-gh")
	h.drain()
	h.gh.RenameUser(1001, "alice-renamed")
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	links, err := h.svc.MyAccountLinks(h.ctx, alice.ID)
	if err != nil || len(links.Verified) != 1 || links.Verified[0].Login != "alice-renamed" {
		t.Fatalf("links = %+v, %v", links, err)
	}
	if r := h.result(h.inst, alice); r.Status != apps.StatusDone || !r.GrantedByOrglane {
		t.Fatalf("result after rename = %+v", r)
	}
}

// Suspending a person takes their GitHub access away like leaving teams.
func TestGitHubSuspendedPersonIsRemoved(t *testing.T) {
	h := newGitHubHarness(t)
	erin := h.person("erin")
	h.mapTeam(h.inst, h.team("platform", erin))
	h.linkAs(erin, 5005, "erin-gh")
	h.drain()
	h.gh.Accept(5005)
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, erin.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if _, ok := h.gh.Members()[5005]; ok {
		t.Fatal("a suspended person kept organization membership")
	}
}
