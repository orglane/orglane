//go:build integration

package apps_test

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
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
	return holdTeamRemovals(t, newGitHubHarness(t))
}

// holdTeamRemovals is newHeldTeamRemovals on a given GitHub harness.
func holdTeamRemovals(t *testing.T, h *linkHarness) *heldTeamRemovals {
	t.Helper()
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
	deletes        int
}

func (t *failAfterDelete) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if r.Method == http.MethodDelete && !strings.HasPrefix(r.URL.Path, "/applications/") {
		t.deletes++ // provisioning removals, not the link's OAuth grant cleanup
	}
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

// deleteCount is how many provisioning DELETEs reached GitHub.
func (t *failAfterDelete) deleteCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.deletes
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
// read fails: the acceptance is durable across the plan's cleanup and a
// restart, a transient failure on the job's last attempt keeps it pending, the
// DELETE is never issued again, and the removal ends in exactly one audit
// entry. With restored, the desired access changes and grant puts the target
// back on GitHub first: the read-only confirmation cannot tell it from the old
// access GitHub has not removed yet, so the removal stays accepted (never
// superseded, never removed again) until GitHub stops serving the target. In
// both variants remove completes the removal on GitHub and the confirmation
// applies and audits it. The removal must already be planned.
func exerciseGitHubAcceptedRemoval(t *testing.T, h *linkHarness, fault *failAfterDelete, p store.Person, externalID, deletePath, confirmPath string, restored bool, grant func(), granted func() bool, remove func()) {
	t.Helper()
	var key string
	if err := h.pool.QueryRow(h.ctx, `SELECT action_key FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, p.ID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	audits := func(action audit.Action) int {
		return h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'key' = $2`, string(action), key)
	}
	var snooze *river.JobSnoozeError
	deletes := fault.deleteCount()

	// The DELETE is accepted, then the confirmation read fails.
	fault.afterDelete(deletePath, confirmPath)
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 1, 8)
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("accepted removal with a failed confirmation = %v, want a 5s snooze", err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending' AND error_code = $2`, p.ID, provider.CodeRemovalPending); n != 1 {
		t.Fatalf("removals with the durable acceptance = %d, want 1", n)
	}
	if n := audits(audit.ProvisioningActionApplied); n != 0 {
		t.Fatalf("audit entries before the confirmation = %d, want 0", n)
	}

	// The plan's cleanup keeps the accepted row.
	if _, err := store.New(h.pool).DropPendingRemovals(h.ctx, store.DropPendingRemovalsParams{InstanceID: h.inst.ID, PersonID: p.ID, KeepKeys: []string{}, AcceptedCode: provider.CodeRemovalPending}); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending' AND error_code = $2`, p.ID, provider.CodeRemovalPending); n != 1 {
		t.Fatalf("accepted removals after the plan cleanup = %d, want 1", n)
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
	if n := audits(audit.ProvisioningActionApplied); n != 0 {
		t.Fatalf("audit entries before the confirmation = %d, want 0", n)
	}
	fault.clear()

	if restored {
		// The target is there again while the accepted removal is
		// unconfirmed: the read-only confirmation keeps watching instead of
		// removing it again or declaring the removal over.
		h.exec(`UPDATE provisioning_removals SET desired_hash = decode(repeat('00', 32), 'hex') WHERE person_id = $1 AND state = 'pending'`, p.ID)
		grant()
		err := svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 2, 8)
		if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
			t.Fatalf("read-only confirmation with the target back = %v, want a 5s snooze", err)
		}
		if !granted() {
			t.Fatal("the confirmation removed the target")
		}
		if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending' AND error_code = $2`, p.ID, provider.CodeRemovalPending); n != 1 {
			t.Fatalf("accepted removals while the target is back = %d, want 1", n)
		}
	}
	remove()
	if err := svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 3, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, p.ID); n != 1 {
		t.Fatalf("applied removals = %d, want 1", n)
	}
	if n := audits(audit.ProvisioningActionApplied); n != 1 {
		t.Fatalf("audit entries after the confirmation = %d, want 1", n)
	}
	if n := audits(audit.ProvisioningRemovalSuperseded); n != 0 {
		t.Fatalf("superseded audit entries = %d, want 0", n)
	}
	if n := fault.deleteCount() - deletes; n != 1 {
		t.Fatalf("DELETEs = %d, want exactly 1", n)
	}
}

// TestGitHubRemovalConfirmationFailureKeepsAcceptance: when the confirmation
// read fails after GitHub accepted the removal, the accepted state is durable
// in PostgreSQL. A restart keeps it, a transient failure on the job's last
// attempt keeps the removal pending instead of failing it, and the eventual
// confirmation writes exactly one audit entry; with restored access the
// removal is settled as superseded and the restored grant stays. Each removal
// kind goes through the same core guard, with exactly one DELETE.
func TestGitHubRemovalConfirmationFailureKeepsAcceptance(t *testing.T) {
	for _, restored := range []bool{false, true} {
		prefix := "confirmed/"
		if restored {
			prefix = "restored/"
		}
		t.Run(prefix+"organization membership", func(t *testing.T) {
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
			exerciseGitHubAcceptedRemoval(t, h, fault, alice, "1001", path, path, restored, grant, granted, remove)
		})
		t.Run(prefix+"team membership", func(t *testing.T) {
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
			exerciseGitHubAcceptedRemoval(t, h, fault, alice, "1001", path, path, restored, grant, granted, remove)
		})
		t.Run(prefix+"invitation", func(t *testing.T) {
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
			exerciseGitHubAcceptedRemoval(t, h, fault, alice, "1001", deletePath, confirmPath, restored, grant, granted, remove)
		})
	}
}

// acceptedOrgRemoval is a GitHub organization removal that GitHub accepted
// but keeps serving (the "removal state machine" of the PR, state accepted):
// alice left her only mapped team, the guard sent one DELETE, and the row is
// pending with removal_pending.
type acceptedOrgRemoval struct {
	h        *linkHarness
	fault    *failAfterDelete
	alice    store.Person
	platform store.Team
	key      string
}

func newAcceptedOrgRemoval(t *testing.T, readbacks int) *acceptedOrgRemoval {
	t.Helper()
	h, fault := newFaultyGitHubHarness(t)
	alice := h.person("alice")
	platform := h.team("platform", alice)
	h.mapTeam(h.inst, platform)
	h.linkAs(alice, 1001, "alice-gh")
	h.drain()
	h.gh.Accept(1001)
	h.gh.QueueOrgRemovals(readbacks)
	h.leave(platform, alice)
	planGitHubRemoval(t, h, alice)
	a := &acceptedOrgRemoval{h: h, fault: fault, alice: alice, platform: platform}
	if err := h.pool.QueryRow(h.ctx, `SELECT action_key FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, alice.ID).Scan(&a.key); err != nil {
		t.Fatal(err)
	}
	// pending --DELETE accepted, still served--> accepted
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 1, 8)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("accepted removal = %v, want a 5s snooze", err)
	}
	if state, code := a.row(t); state != "pending" || code != provider.CodeRemovalPending {
		t.Fatalf("accepted removal row = %s/%s, want pending/%s", state, code, provider.CodeRemovalPending)
	}
	if n := fault.deleteCount(); n != 1 {
		t.Fatalf("DELETEs = %d, want 1", n)
	}
	if n := a.audits(audit.ProvisioningActionApplied); n != 0 {
		t.Fatalf("audit entries before the confirmation = %d, want 0", n)
	}
	a.wantResult(t, apps.StatusQueued, provider.CodeRemovalPending)
	return a
}

// wantResult checks alice's provisioning result: its status and the code
// the admin sees.
func (a *acceptedOrgRemoval) wantResult(t *testing.T, status, code string) {
	t.Helper()
	var gotStatus, gotCode string
	if err := a.h.pool.QueryRow(a.h.ctx, `SELECT status, coalesce(error_code, '') FROM provisioning_results WHERE instance_id = $1 AND person_id = $2`, a.h.inst.ID, a.alice.ID).Scan(&gotStatus, &gotCode); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || gotCode != code {
		t.Fatalf("result = %s/%q, want %s/%q", gotStatus, gotCode, status, code)
	}
}

// row is the state and error code of alice's latest removal row.
func (a *acceptedOrgRemoval) row(t *testing.T) (state, code string) {
	t.Helper()
	if err := a.h.pool.QueryRow(a.h.ctx, `SELECT state, coalesce(error_code, '') FROM provisioning_removals WHERE person_id = $1 AND action_key = $2 ORDER BY created_at DESC, id DESC LIMIT 1`, a.alice.ID, a.key).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	return state, code
}

func (a *acceptedOrgRemoval) audits(action audit.Action) int {
	return a.h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'key' = $2`, string(action), a.key)
}

func (a *acceptedOrgRemoval) restore(t *testing.T) {
	t.Helper()
	if _, err := a.h.ids.SetTeamMember(a.h.ctx, audit.System, a.platform.ID, a.alice.ID, identity.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
}

// TestGitHubAcceptedRemovalStateMachine walks every transition out of the
// accepted state against PostgreSQL, the real guard and adapter and the fake
// GitHub: a removal GitHub accepted is only ever confirmed read-only (one
// DELETE in total), confirmed absence applies and audits it once, a read
// failure keeps it accepted (also on the last attempt and across a restart),
// and restored access settles it as superseded while the restored grant stays
// and the additive path runs.
func TestGitHubAcceptedRemovalStateMachine(t *testing.T) {
	const pending, applied, dropped, failed = "pending", "applied", "dropped", "failed"
	cases := []struct {
		name string
		// event happens between the acceptance and the checked guard run; it
		// may return a restarted core to run it with.
		event   func(t *testing.T, a *acceptedOrgRemoval) *apps.Service
		attempt int
		// snooze is the guard run's expected snooze; 0 expects no error (a
		// failed removal is recorded on its row, not returned).
		snooze                 time.Duration
		state, code            string
		appliedAudits          int
		supersededAudits       int
		member, invited, drain bool
	}{
		{
			name:   "fresh, still served: stays accepted, no DELETE",
			event:  func(*testing.T, *acceptedOrgRemoval) *apps.Service { return nil },
			snooze: 5 * time.Second, state: pending, code: provider.CodeRemovalPending, member: true,
		},
		{
			name: "fresh, gone: applied and audited once",
			event: func(_ *testing.T, a *acceptedOrgRemoval) *apps.Service {
				a.h.gh.RemoveMember(1001)
				return nil
			},
			state: applied, appliedAudits: 1,
		},
		{
			name: "fresh, read fails on the last attempt: stays accepted",
			event: func(_ *testing.T, a *acceptedOrgRemoval) *apps.Service {
				a.fault.readPath("/user/1001")
				a.fault.failTheNextRead()
				return nil
			},
			attempt: 8, snooze: 5 * time.Second, state: pending, code: provider.CodeRemovalPending, member: true,
		},
		{
			name: "fresh, an enterprise team keeps the membership: failed, no audit",
			event: func(_ *testing.T, a *acceptedOrgRemoval) *apps.Service {
				a.h.gh.AddIndirectMember(1001, githubfake.RoleMember, []string{"ent:platform"})
				return nil
			},
			state: failed, code: "indirect_membership", member: true,
		},
		{
			name: "restart, still served: stays accepted",
			event: func(_ *testing.T, a *acceptedOrgRemoval) *apps.Service {
				return a.h.restartCore()
			},
			snooze: 5 * time.Second, state: pending, code: provider.CodeRemovalPending, member: true,
		},
		{
			name: "replanned while the removal is still wanted: stays accepted",
			event: func(t *testing.T, a *acceptedOrgRemoval) *apps.Service {
				if _, err := a.h.ids.SetPersonStatus(a.h.ctx, audit.System, a.alice.ID, identity.StatusSuspended); err != nil {
					t.Fatal(err)
				}
				if err := a.h.svc.RunProvisionPerson(a.h.ctx, apps.ProvisionPersonArgs{InstanceID: a.h.inst.ID, PersonID: a.alice.ID}, 1, 8); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			snooze: 5 * time.Second, state: pending, code: provider.CodeRemovalPending, member: true,
		},
		{
			name: "restored, still served: keeps watching, membership kept",
			event: func(t *testing.T, a *acceptedOrgRemoval) *apps.Service {
				a.restore(t)
				return nil
			},
			snooze: 5 * time.Second, state: pending, code: provider.CodeRemovalPending, member: true,
		},
		{
			name: "restored after GitHub finished: applied, then invited again",
			event: func(t *testing.T, a *acceptedOrgRemoval) *apps.Service {
				a.h.gh.RemoveMember(1001)
				a.restore(t)
				return nil
			},
			state: applied, appliedAudits: 1, invited: true, drain: true,
		},
		{
			name: "restored, replanned before the confirmation: the grant waits, then runs",
			event: func(t *testing.T, a *acceptedOrgRemoval) *apps.Service {
				a.h.gh.RemoveMember(1001)
				a.restore(t)
				if err := a.h.svc.RunProvisionPerson(a.h.ctx, apps.ProvisionPersonArgs{InstanceID: a.h.inst.ID, PersonID: a.alice.ID}, 1, 8); err != nil {
					t.Fatal(err)
				}
				// The invitation waits for the accepted removal: it could
				// otherwise be mistaken for the old access, or be removed by it.
				if len(a.h.gh.Invitations()) != 0 {
					t.Fatalf("invitations = %+v, want none before the confirmation", a.h.gh.Invitations())
				}
				a.wantResult(t, apps.StatusQueued, provider.CodeRemovalPending)
				return nil
			},
			state: applied, appliedAudits: 1, invited: true, drain: true,
		},
		{
			name: "restored, an enterprise team keeps the membership: superseded",
			event: func(t *testing.T, a *acceptedOrgRemoval) *apps.Service {
				a.h.gh.AddIndirectMember(1001, githubfake.RoleMember, []string{"ent:platform"})
				a.restore(t)
				return nil
			},
			state: dropped, code: "indirect_membership", supersededAudits: 1, member: true,
		},
		{
			name: "restored, the membership read fails: stays accepted",
			event: func(t *testing.T, a *acceptedOrgRemoval) *apps.Service {
				a.restore(t)
				// Not "still served": an unknown answer never supersedes.
				a.fault.readPath("/orgs/" + a.h.gh.Org().Login + "/memberships/alice-gh")
				a.fault.failTheNextRead()
				return nil
			},
			snooze: 5 * time.Second, state: pending, code: provider.CodeRemovalPending, member: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// GitHub keeps serving the removed membership far longer than
			// the test reads it.
			a := newAcceptedOrgRemoval(t, 1000)
			h := a.h
			svc := tc.event(t, a)
			if svc == nil {
				svc = h.svc
			}
			attempt := tc.attempt
			if attempt == 0 {
				attempt = 2
			}
			err := svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, attempt, 8)
			var snooze *river.JobSnoozeError
			switch {
			case tc.snooze > 0:
				if !errors.As(err, &snooze) || snooze.Duration != tc.snooze {
					t.Fatalf("guard run = %v, want a %s snooze", err, tc.snooze)
				}
			case err != nil:
				t.Fatalf("guard run = %v, want settled", err)
			}
			a.fault.clear()
			if state, code := a.row(t); state != tc.state || code != tc.code {
				t.Fatalf("removal row = %s/%q, want %s/%q", state, code, tc.state, tc.code)
			}
			if n := a.audits(audit.ProvisioningActionApplied); n != tc.appliedAudits {
				t.Fatalf("applied audit entries = %d, want %d", n, tc.appliedAudits)
			}
			if n := a.audits(audit.ProvisioningRemovalSuperseded); n != tc.supersededAudits {
				t.Fatalf("superseded audit entries = %d, want %d", n, tc.supersededAudits)
			}
			if tc.drain {
				// The additive path runs for the restored access and leaves
				// nothing open.
				h.drain()
				if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state IN ('pending', 'held')`, a.alice.ID); n != 0 {
					t.Fatalf("open removals after the replan = %d, want 0", n)
				}
			}
			if _, ok := h.gh.Members()[1001]; ok != tc.member {
				t.Fatalf("organization member = %v, want %v", ok, tc.member)
			}
			if got := len(h.gh.Invitations()) > 0; got != tc.invited {
				t.Fatalf("invited = %v, want %v (%+v)", got, tc.invited, h.gh.Invitations())
			}
			if n := a.fault.deleteCount(); n != 1 {
				t.Fatalf("DELETEs = %d, want exactly the accepted one", n)
			}
		})
	}
}

// TestGitHubAcceptedRemovalIsNeverReissued: GitHub serves a removed
// membership for a few reads. Every guard run after the acceptance only
// reads; a repeated DELETE would restart GitHub's processing and never let
// the removal settle. It settles once GitHub stops serving the membership,
// with one DELETE and one audit entry.
func TestGitHubAcceptedRemovalIsNeverReissued(t *testing.T) {
	a := newAcceptedOrgRemoval(t, 3)
	h := a.h
	settled := 0
	for attempt := 2; attempt <= 8; attempt++ {
		err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, attempt, 8)
		if err == nil {
			settled = attempt
			break
		}
		var snooze *river.JobSnoozeError
		if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
			t.Fatalf("confirmation attempt %d = %v, want a 5s snooze", attempt, err)
		}
	}
	if settled == 0 {
		t.Fatal("the accepted removal never settled")
	}
	if state, _ := a.row(t); state != "applied" {
		t.Fatalf("removal = %s, want applied", state)
	}
	if n := a.fault.deleteCount(); n != 1 {
		t.Fatalf("DELETEs = %d, want 1", n)
	}
	if n := a.audits(audit.ProvisioningActionApplied); n != 1 {
		t.Fatalf("audit entries = %d, want 1", n)
	}
	if _, ok := h.gh.Members()[1001]; ok {
		t.Fatal("alice is still a member")
	}
}

// TestGitHubLateRemovalAfterRestorationIsReconciled: access is restored while
// GitHub still serves a membership whose removal it accepted. The removal
// keeps being watched (a still-served target is not proof that the queued
// DELETE stopped); when it lands late, the confirmation audits it and the
// person is planned again, so the restored access comes back without waiting
// for a drift check.
func TestGitHubLateRemovalAfterRestorationIsReconciled(t *testing.T) {
	a := newAcceptedOrgRemoval(t, 1000)
	h := a.h
	a.restore(t)
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: h.inst.ID, PersonID: a.alice.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	a.wantResult(t, apps.StatusQueued, provider.CodeRemovalPending)
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 2, 8)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("guard run while GitHub still serves the membership = %v, want a 5s snooze", err)
	}
	if _, ok := h.gh.Members()[1001]; !ok {
		t.Fatal("alice lost the membership before GitHub's queued removal landed")
	}

	// GitHub's queued DELETE lands now, after the restoration.
	h.gh.RemoveMember(1001)
	h.drain()
	if state, _ := a.row(t); state != "applied" {
		t.Fatalf("removal = %s, want applied", state)
	}
	if n := a.audits(audit.ProvisioningActionApplied); n != 1 {
		t.Fatalf("applied audit entries = %d, want 1", n)
	}
	if n := a.audits(audit.ProvisioningRemovalSuperseded); n != 0 {
		t.Fatalf("superseded audit entries = %d, want 0", n)
	}
	if len(h.gh.Invitations()) != 1 {
		t.Fatalf("invitations = %+v, want alice invited again", h.gh.Invitations())
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state IN ('pending', 'held')`, a.alice.ID); n != 0 {
		t.Fatalf("open removals = %d, want 0", n)
	}
	if n := a.fault.deleteCount(); n != 1 {
		t.Fatalf("DELETEs = %d, want 1", n)
	}
}

// TestGitHubAcceptedRemovalFromAConfirmedBatchHoldsGrants: an accepted
// removal released through a confirmed batch keeps its held row state. It
// must hold the person's grants like an accepted pending removal: a person
// restored before GitHub confirms is not re-added (the late removal could
// land on the new grant, or the grant could be mistaken for the old access)
// and their result says why it waits; the confirmation then re-adds them.
func TestGitHubAcceptedRemovalFromAConfirmedBatchHoldsGrants(t *testing.T) {
	h, fault := newFaultyGitHubHarness(t)
	f := holdTeamRemovals(t, h)
	p0, id0 := f.people[0], f.ids[0]
	path := "/organizations/" + strconv.FormatInt(h.gh.Org().ID, 10) + "/team/" + strconv.FormatInt(f.crew.ID, 10) + "/memberships/" + p0.Username + "-gh"
	fault.afterDelete(path, path)
	if err := h.svc.ConfirmRemovals(h.ctx, audit.System, h.inst.ID, f.held.Batch.ID, f.held.Batch.ItemsHash); err != nil {
		t.Fatal(err)
	}
	err := h.svc.RunApplyRemovals(h.ctx, apps.ApplyRemovalsArgs{InstanceID: h.inst.ID}, 1, 8)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Fatalf("confirmed batch with an unconfirmed removal = %v, want a 5s snooze", err)
	}
	fault.clear()
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'held' AND error_code = $2`, p0.ID, provider.CodeRemovalPending); n != 1 {
		t.Fatalf("accepted held removals = %d, want 1", n)
	}

	// p0 gets the access back before GitHub's removal is confirmed.
	if _, err := h.ids.SetTeamMember(h.ctx, audit.System, f.release.ID, p0.ID, identity.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: h.inst.ID, PersonID: p0.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if m := h.gh.TeamMembers(f.crew.ID)[id0]; m != "" {
		t.Fatalf("p0 was re-added (%s) before the accepted removal was confirmed", m)
	}
	var status, code string
	if err := h.pool.QueryRow(h.ctx, `SELECT status, coalesce(error_code, '') FROM provisioning_results WHERE instance_id = $1 AND person_id = $2`, h.inst.ID, p0.ID).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != apps.StatusQueued || code != provider.CodeRemovalPending {
		t.Fatalf("result while the removal is unconfirmed = %s/%q, want %s/%q", status, code, apps.StatusQueued, provider.CodeRemovalPending)
	}

	// The confirmation sees the membership gone, audits the removal and
	// replans p0, who is added again.
	h.drain()
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied' AND error_code IS NULL`, p0.ID); n != 1 {
		t.Fatalf("applied removals = %d, want 1", n)
	}
	if m := h.gh.TeamMembers(f.crew.ID)[id0]; m != "member/active" {
		t.Fatalf("p0 after the confirmation = %q, want member/active", m)
	}
}
