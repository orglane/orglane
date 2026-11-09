//go:build integration

package apps_test

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"

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

// mapExistingGitHubTeam maps an Orglane team to an existing GitHub team.
func (h *linkHarness) mapExistingGitHubTeam(team store.Team, providerTeam githubfake.Team) {
	h.t.Helper()
	if _, err := h.svc.SetTeamMapping(h.ctx, audit.System, h.inst.ID, team.ID, apps.TeamMappingInput{
		Mode: provider.MappingExisting, ExternalRef: strconv.FormatInt(providerTeam.ID, 10),
	}); err != nil {
		h.t.Fatal(err)
	}
}

// A queued removal from a mapped team is canceled when that team's mapping is
// deleted before the guard runs: the team is no longer Orglane's, so its
// members stay. The guard must not apply the stale intent.
func TestGitHubPendingTeamRemovalCanceledByUnmapping(t *testing.T) {
	h := newGitHubHarness(t)
	alice := h.person("alice")
	platform := h.team("platform", alice)
	release := h.team("release")
	crew := h.gh.AddTeam("Release Crew", "made on GitHub")
	h.mapTeam(h.inst, platform)
	h.mapExistingGitHubTeam(release, crew)
	h.linkAs(alice, 1001, "alice-gh")
	h.drain()
	h.gh.Accept(1001)
	h.gh.SetTeamMember(crew.ID, 1001, githubfake.RoleMember)
	// A drift plan sees the manual membership in the mapped "release" team
	// and queues its removal.
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: h.inst.ID, PersonID: alice.ID, Drift: true}, 1, 8); err != nil {
		t.Fatal(err)
	}
	key := "remove_from_team:" + strconv.FormatInt(crew.ID, 10)
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE instance_id = $1 AND person_id = $2 AND action_key = $3 AND state = 'pending'`, h.inst.ID, alice.ID, key); n != 1 {
		t.Fatalf("%d pending removals, want 1", n)
	}
	if err := h.svc.RemoveTeamMapping(h.ctx, audit.System, h.inst.ID, release.ID); err != nil {
		t.Fatal(err)
	}
	// The guard runs before the mapping-triggered replans, the order that
	// used to apply the stale removal.
	if err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if m := h.gh.TeamMembers(crew.ID); m[1001] != "member/active" {
		t.Fatalf("a stale removal deleted a membership in an unmapped team: %v", m)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE instance_id = $1 AND person_id = $2 AND action_key = $3 AND state = 'dropped'`, h.inst.ID, alice.ID, key); n != 1 {
		t.Fatalf("%d dropped removals, want 1", n)
	}
}

// heldTeamRemovals is a held batch of four removals from one mapped existing
// GitHub team.
type heldTeamRemovals struct {
	h       *linkHarness
	people  []store.Person
	ids     []int64
	crew    githubfake.Team
	release store.Team
	held    *apps.HeldBatch
}

// newHeldTeamRemovals sets up four people with access through one mapped
// team and a manual membership in another mapped "existing" team, and holds
// their removal batch.
func newHeldTeamRemovals(t *testing.T) *heldTeamRemovals {
	t.Helper()
	h := newGitHubHarness(t)
	f := &heldTeamRemovals{h: h, ids: []int64{1001, 1002, 1003, 1004}}
	for _, name := range []string{"p0", "p1", "p2", "p3"} {
		f.people = append(f.people, h.person(name))
	}
	platform := h.team("platform", f.people...)
	f.release = h.team("release")
	f.crew = h.gh.AddTeam("Release Crew", "made on GitHub")
	h.mapTeam(h.inst, platform)
	h.mapExistingGitHubTeam(f.release, f.crew)
	for i, p := range f.people {
		h.linkAs(p, f.ids[i], p.Username+"-gh")
	}
	h.drain()
	for _, id := range f.ids {
		h.gh.Accept(id)
		h.gh.SetTeamMember(f.crew.ID, id, githubfake.RoleMember)
	}
	for _, p := range f.people {
		if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: h.inst.ID, PersonID: p.ID, Drift: true}, 1, 8); err != nil {
			t.Fatal(err)
		}
	}
	h.drain()
	held, err := h.svc.HeldRemovals(h.ctx, h.inst.ID)
	if err != nil || held == nil || int(held.Batch.ItemCount) != len(f.people) || len(held.Items) != len(f.people) {
		t.Fatalf("held = %+v, %v", held, err)
	}
	f.held = held
	return f
}

// Confirming a held batch after its team's mapping was deleted must drop its
// stale items: the team is no longer Orglane's, so nobody is removed.
func TestGitHubHeldTeamRemovalsCanceledByUnmapping(t *testing.T) {
	f := newHeldTeamRemovals(t)
	if err := f.h.svc.RemoveTeamMapping(f.h.ctx, audit.System, f.h.inst.ID, f.release.ID); err != nil {
		t.Fatal(err)
	}
	f.h.drain() // finish the mapping-triggered replans
	if err := f.h.svc.ConfirmRemovals(f.h.ctx, audit.System, f.h.inst.ID, f.held.Batch.ID, f.held.Batch.ItemsHash); err != nil {
		t.Fatal(err)
	}
	f.h.drain()
	for _, id := range f.ids {
		if m := f.h.gh.TeamMembers(f.crew.ID); m[id] != "member/active" {
			t.Fatalf("a confirmed stale removal deleted a membership in an unmapped team: %v", m)
		}
	}
	if n := f.h.count(`SELECT count(*) FROM provisioning_removals WHERE batch_id = $1 AND state = 'held'`, f.held.Batch.ID); n != 0 {
		t.Fatalf("%d held items after confirmation", n)
	}
	var state string
	if err := f.h.pool.QueryRow(f.h.ctx, `SELECT state FROM provisioning_removal_batches WHERE id = $1`, f.held.Batch.ID).Scan(&state); err != nil || state != "applied" {
		t.Fatalf("batch = %q, %v", state, err)
	}
}

// An unrelated mapping change must not cancel a held removal: Mapped is not
// part of the person's desired hash, and the removal's target is still
// mapped, so confirming applies it.
func TestGitHubHeldTeamRemovalsSurviveUnrelatedMappingChange(t *testing.T) {
	f := newHeldTeamRemovals(t)
	f.h.mapTeam(f.h.inst, f.h.team("other"))
	f.h.drain()
	held, err := f.h.svc.HeldRemovals(f.h.ctx, f.h.inst.ID)
	if err != nil || held == nil || held.Batch.ID != f.held.Batch.ID || len(held.Items) != len(f.people) {
		t.Fatalf("held after an unrelated mapping change = %+v, %v", held, err)
	}
	if err := f.h.svc.ConfirmRemovals(f.h.ctx, audit.System, f.h.inst.ID, f.held.Batch.ID, f.held.Batch.ItemsHash); err != nil {
		t.Fatal(err)
	}
	f.h.drain()
	for _, id := range f.ids {
		if m := f.h.gh.TeamMembers(f.crew.ID); m[id] != "" {
			t.Fatalf("an unrelated mapping change dropped the held removal: %v", m)
		}
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

// failAfterDelete fails the next GET to getPath once a DELETE to deletePath
// has been seen (or the next GET outright, with failTheNextRead). It models a
// transient confirmation-read failure after GitHub accepted a removal without
// changing the fake's state.
type failAfterDelete struct {
	base http.RoundTripper

	mu             sync.Mutex
	deletePath     string
	getPath        string
	armAfterDelete bool
	failNext       bool
}

func (t *failAfterDelete) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if t.armAfterDelete && r.Method == http.MethodDelete && r.URL.Path == t.deletePath {
		t.failNext = true
		t.armAfterDelete = false
	}
	fail := t.failNext && r.Method == http.MethodGet && r.URL.Path == t.getPath
	if fail {
		t.failNext = false
	}
	t.mu.Unlock()
	if fail {
		return &http.Response{
			Status: "502 Bad Gateway", StatusCode: http.StatusBadGateway,
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: make(http.Header), Body: http.NoBody, Request: r,
		}, nil
	}
	return t.base.RoundTrip(r)
}

func (t *failAfterDelete) afterDelete(deletePath, getPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deletePath, t.getPath, t.armAfterDelete, t.failNext = deletePath, getPath, true, false
}

func (t *failAfterDelete) failTheNextRead() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failNext = true
}

func (t *failAfterDelete) readPath(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.getPath = path
}

func (t *failAfterDelete) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.armAfterDelete, t.failNext = false, false
}

// newFaultyGitHubHarness is a link harness with the mapped-teams policy whose
// GitHub traffic can be failed by fault.
func newFaultyGitHubHarness(t *testing.T) (*linkHarness, *failAfterDelete) {
	t.Helper()
	fault := &failAfterDelete{}
	h := newLinkHarnessWithTransport(t, func(rt http.RoundTripper) http.RoundTripper {
		fault.base = rt
		return fault
	})
	policy := provider.MappedTeams
	inst, err := h.svc.UpdateInstance(h.ctx, audit.System, h.inst.ID, apps.InstanceUpdate{AccessPolicy: &policy})
	if err != nil {
		t.Fatal(err)
	}
	h.inst = inst
	h.drain()
	return h, fault
}

// planGitHubRemoval runs a person's provisioning so their removal is queued
// through the guard without applying it.
func planGitHubRemoval(t *testing.T, h *linkHarness, p store.Person) {
	t.Helper()
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: h.inst.ID, PersonID: p.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, p.ID); n != 1 {
		t.Fatalf("pending removals = %d, want 1", n)
	}
}

// exerciseGitHubAcceptedRemoval drives an accepted removal whose confirmation
// read fails: the acceptance is durable across a restart, a transient failure
// on the job's last attempt keeps it pending, a desired-state change keeps the
// row and confirms it read-only (a restored grant is not removed again), and
// the eventual confirmation writes exactly one audit entry. The removal must
// already be planned. grant restores the desired access on GitHub, granted
// reports whether it is still there, and remove completes the accepted removal.
func exerciseGitHubAcceptedRemoval(t *testing.T, h *linkHarness, fault *failAfterDelete, p store.Person, externalID, deletePath, confirmPath string, grant func(), granted func() bool, remove func()) {
	t.Helper()
	var key string
	if err := h.pool.QueryRow(h.ctx, `SELECT action_key FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, p.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	audits := func() int {
		return h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'key' = $2`, string(audit.ProvisioningActionApplied), key)
	}
	var snooze *river.JobSnoozeError

	// The DELETE is accepted, then the confirmation read fails.
	fault.afterDelete(deletePath, confirmPath)
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 1, 8)
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("accepted removal with a failed confirmation = %v, want a 5s snooze", err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending' AND error_code = $2`, p.ID, provider.CodeRemovalPending); n != 1 {
		t.Fatalf("removals with the durable acceptance = %d, want 1", n)
	}
	if n := audits(); n != 0 {
		t.Fatalf("audit entries before the confirmation = %d, want 0", n)
	}

	// A desired-state change must not drop the accepted row through either
	// freshness path: the plan's cleanup keeps it, and apply_removals confirms
	// it instead of dropping it on the hash mismatch.
	h.exec(`UPDATE provisioning_removals SET desired_hash = decode(repeat('00', 32), 'hex') WHERE person_id = $1 AND state = 'pending'`, p.ID)
	if _, err := store.New(h.pool).DropPendingRemovals(h.ctx, store.DropPendingRemovalsParams{InstanceID: h.inst.ID, PersonID: p.ID, KeepKeys: []string{}, AcceptedCode: provider.CodeRemovalPending}); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending' AND error_code = $2`, p.ID, provider.CodeRemovalPending); n != 1 {
		t.Fatalf("accepted removals after the plan cleanup = %d, want 1", n)
	}

	// Access is restored while the accepted removal is unconfirmed. The
	// changed desired state keeps the row, but the confirmation must only read:
	// a membership or invitation the new grant created is never removed again.
	grant()
	err = h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 2, 8)
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("read-only confirmation after access returned = %v, want a 5s snooze", err)
	}
	if !granted() {
		t.Fatal("the confirmation removed the restored grant")
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, p.ID); n != 1 {
		t.Fatalf("removals after the read-only confirmation = %d, want 1 pending", n)
	}
	if n := audits(); n != 0 {
		t.Fatalf("audit entries before the confirmation = %d, want 0", n)
	}

	// A restart keeps the acceptance; a transient failure on the retry's first
	// read must not fail the removal on the job's last attempt.
	svc := h.restartCore()
	fault.readPath("/user/" + externalID)
	fault.failTheNextRead()
	err = svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 8, 8)
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("accepted removal on the last attempt = %v, want a 5s snooze", err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, p.ID); n != 1 {
		t.Fatalf("removals after the last-attempt read failure = %d, want 1 pending", n)
	}
	if n := audits(); n != 0 {
		t.Fatalf("audit entries before the confirmation = %d, want 0", n)
	}

	fault.clear()
	remove()
	if err := svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 3, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, p.ID); n != 1 {
		t.Fatalf("applied removals = %d, want 1", n)
	}
	if n := audits(); n != 1 {
		t.Fatalf("audit entries after the confirmation = %d, want 1", n)
	}
}

// TestGitHubRemovalConfirmationFailureKeepsAcceptance: when the confirmation
// read fails after GitHub accepted the removal, the accepted state is durable
// in PostgreSQL. A restart keeps it, a transient failure on the job's last
// attempt keeps the removal pending instead of failing it, and the eventual
// confirmation writes exactly one audit entry. Each removal kind goes through
// the same core guard.
func TestGitHubRemovalConfirmationFailureKeepsAcceptance(t *testing.T) {
	t.Run("organization membership", func(t *testing.T) {
		h, fault := newFaultyGitHubHarness(t)
		alice := h.person("alice")
		platform := h.team("platform", alice)
		h.mapTeam(h.inst, platform)
		h.linkAs(alice, 1001, "alice-gh")
		h.drain()
		h.gh.Accept(1001)
		h.leave(platform, alice)
		planGitHubRemoval(t, h, alice)
		path := "/orgs/" + h.gh.Org().Login + "/memberships/alice-gh"
		grant := func() { h.gh.AddMember(1001, githubfake.RoleMember) }
		granted := func() bool { return h.gh.Members()[1001] == githubfake.RoleMember }
		remove := func() { h.gh.RemoveMember(1001) }
		exerciseGitHubAcceptedRemoval(t, h, fault, alice, "1001", path, path, grant, granted, remove)
	})
	t.Run("team membership", func(t *testing.T) {
		h, fault := newFaultyGitHubHarness(t)
		alice := h.person("alice")
		platform, backend := h.team("platform", alice), h.team("backend", alice)
		h.mapTeam(h.inst, platform)
		h.mapTeam(h.inst, backend)
		h.linkAs(alice, 1001, "alice-gh")
		h.drain()
		h.gh.Accept(1001)
		platformRef := h.teamRef(platform)
		h.leave(platform, alice)
		planGitHubRemoval(t, h, alice)
		path := "/organizations/" + strconv.FormatInt(h.gh.Org().ID, 10) + "/team/" + strconv.FormatInt(platformRef, 10) + "/memberships/alice-gh"
		grant := func() { h.gh.SetTeamMember(platformRef, 1001, githubfake.TeamMember) }
		granted := func() bool { return h.gh.TeamMembers(platformRef)[1001] != "" }
		remove := func() { h.gh.RemoveTeamMember(platformRef, 1001) }
		exerciseGitHubAcceptedRemoval(t, h, fault, alice, "1001", path, path, grant, granted, remove)
	})
	t.Run("invitation", func(t *testing.T) {
		h, fault := newFaultyGitHubHarness(t)
		alice := h.person("alice")
		platform := h.team("platform", alice)
		h.mapTeam(h.inst, platform)
		h.linkAs(alice, 1001, "alice-gh")
		h.drain()
		inv := h.gh.Invitations()
		if len(inv) != 1 {
			t.Fatalf("invitations = %+v, want one", inv)
		}
		h.leave(platform, alice)
		planGitHubRemoval(t, h, alice)
		deletePath := "/orgs/" + h.gh.Org().Login + "/invitations/" + strconv.FormatInt(inv[0].ID, 10)
		confirmPath := "/orgs/" + h.gh.Org().Login + "/memberships/alice-gh"
		grant := func() { h.gh.InviteAs(1001, h.gh.BotLogin()) }
		granted := func() bool { return len(h.gh.Invitations()) > 0 }
		remove := func() { h.gh.RemoveMember(1001) }
		exerciseGitHubAcceptedRemoval(t, h, fault, alice, "1001", deletePath, confirmPath, grant, granted, remove)
	})
}
