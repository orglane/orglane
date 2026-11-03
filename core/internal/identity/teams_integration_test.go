//go:build integration

package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
)

func createTeam(t *testing.T, svc *identity.Service, slug string, parent *uuid.UUID) store.Team {
	t.Helper()
	team, err := svc.CreateTeam(context.Background(), admin, identity.NewTeam{Slug: slug, Name: "Team " + slug, ParentID: parent})
	if err != nil {
		t.Fatalf("CreateTeam(%s): %v", slug, err)
	}
	return team
}

func setMember(t *testing.T, svc *identity.Service, team, person uuid.UUID, role identity.TeamRole) {
	t.Helper()
	if _, err := svc.SetTeamMember(context.Background(), admin, team, person, role); err != nil {
		t.Fatalf("SetTeamMember(%s): %v", role, err)
	}
}

func TestSharesTeam(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	bob := createPerson(t, svc, "bob", identity.StatusActive)
	other := createPerson(t, svc, "other", identity.StatusActive)
	team := createTeam(t, svc, "platform", nil)
	setMember(t, svc, team.ID, alice.ID, identity.TeamRoleLead)
	setMember(t, svc, team.ID, bob.ID, identity.TeamRoleMember)
	check := func(a, b uuid.UUID, want bool) {
		t.Helper()
		got, err := svc.SharesTeam(ctx, a, b)
		if err != nil || got != want {
			t.Fatalf("SharesTeam(%s, %s) = %v, %v; want %v", a, b, got, err, want)
		}
	}
	check(alice.ID, bob.ID, true)
	check(bob.ID, alice.ID, true)
	check(alice.ID, other.ID, false)
	check(alice.ID, uuid.New(), false)
	if _, err := svc.ArchiveTeam(ctx, admin, team.ID); err != nil {
		t.Fatal(err)
	}
	check(alice.ID, bob.ID, false)
	active := createTeam(t, svc, "product", nil)
	setMember(t, svc, active.ID, alice.ID, identity.TeamRoleMember)
	setMember(t, svc, active.ID, bob.ID, identity.TeamRoleLead)
	check(alice.ID, bob.ID, true)
	if _, err := svc.SetPersonStatus(ctx, admin, bob.ID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	check(alice.ID, bob.ID, false)
	check(bob.ID, alice.ID, false)
}

// Offboarding queues for the person lock first. Both adding a membership
// and promoting an existing member must then observe the committed status,
// rather than use a stale unlocked read of the active person.
func TestTeamMembershipSerializedWithOffboarding(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "add member"
		if existing {
			name = "promote member"
		}
		t.Run(name, func(t *testing.T) {
			svc, pool := newService(t)
			team := createTeam(t, svc, "platform", nil)
			person := createPerson(t, svc, "alice", identity.StatusActive)
			if existing {
				setMember(t, svc, team.ID, person.ID, identity.TeamRoleMember)
			}
			mark := lastAuditID(t, pool)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			blocker, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }()
			if _, err := blocker.Exec(ctx, "SELECT id FROM people WHERE id = $1 FOR UPDATE", person.ID); err != nil {
				t.Fatal(err)
			}
			offboard := make(chan error, 1)
			go func() {
				_, err := svc.SetPersonStatus(ctx, admin, person.ID, identity.StatusOffboarded)
				offboard <- err
			}()
			waiters := func() int {
				t.Helper()
				var n int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
                    WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			for waiters() < 1 {
				time.Sleep(10 * time.Millisecond)
			}
			membership := make(chan error, 1)
			go func() {
				_, err := svc.SetTeamMember(ctx, admin, team.ID, person.ID, identity.TeamRoleLead)
				membership <- err
			}()
			var membershipErr error
			finished := false
			for !finished && waiters() < 2 {
				select {
				case membershipErr = <-membership:
					finished = true
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-offboard; err != nil {
				t.Fatal(err)
			}
			if !finished {
				membershipErr = <-membership
			}
			if !errors.Is(membershipErr, identity.ErrPersonOffboarded) {
				t.Fatalf("membership after queued offboarding = %v; want ErrPersonOffboarded", membershipErr)
			}
			if existing {
				var role string
				if err := pool.QueryRow(ctx, "SELECT role FROM team_memberships WHERE team_id=$1 AND person_id=$2", team.ID, person.ID).Scan(&role); err != nil || role != "member" {
					t.Fatalf("membership role = %q, %v; want member", role, err)
				}
			} else if n := count(t, pool, "SELECT count(*) FROM team_memberships WHERE team_id=$1 AND person_id=$2", team.ID, person.ID); n != 0 {
				t.Fatalf("offboarded person gained %d memberships", n)
			}
			if got := summarize(auditLog(t, pool, mark)); len(got) != 1 || got[0].Action != "person.status_changed" {
				t.Fatalf("audit = %v; want only the offboarding entry", got)
			}
		})
	}
}

// metadata decodes an audit entry's metadata.
func metadata(t *testing.T, e store.AuditEntry) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Metadata, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTeamCreateAndUpdate(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	mark := lastAuditID(t, pool)

	team, err := svc.CreateTeam(ctx, admin, identity.NewTeam{Slug: " Platform ", Name: "  Platform  ", Description: " Robots and cloud. "})
	if err != nil {
		t.Fatal(err)
	}
	if team.Slug != "platform" || team.Name != "Platform" || team.Description == nil || *team.Description != "Robots and cloud." ||
		team.ParentID != nil || team.ArchivedAt != nil {
		t.Fatalf("created %+v", team)
	}
	entries := auditLog(t, pool, mark)
	want := []entry{{"person", admin.ID, "team.created", "team", team.ID.String()}}
	if got := summarize(entries); !slices.Equal(got, want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
	if m := metadata(t, entries[0]); m["slug"] != "platform" || m["name"] != "Platform" {
		t.Errorf("team.created metadata = %v", m)
	}
	if entries[0].IP == nil || entries[0].IP.String() != "203.0.113.7" || deref(entries[0].UserAgent) != "orglane-test/1.0" {
		t.Errorf("team.created does not record where the change came from: %v %v", entries[0].IP, entries[0].UserAgent)
	}

	// Slugs are unique, case-insensitively by normalization.
	if _, err := svc.CreateTeam(ctx, admin, identity.NewTeam{Slug: "PLATFORM", Name: "Other"}); !errors.Is(err, identity.ErrSlugTaken) {
		t.Fatalf("duplicate slug: %v, want ErrSlugTaken", err)
	}
	for _, in := range []identity.NewTeam{
		{Slug: "", Name: "Empty slug"},
		{Slug: "-platform", Name: "Leading hyphen"},
		{Slug: "platform-", Name: "Trailing hyphen"},
		{Slug: "plat form", Name: "Space"},
		{Slug: "plätform", Name: "Non-ASCII"},
		{Slug: strings.Repeat("x", 64), Name: "Too long"},
		{Slug: "ok", Name: "   "},
		{Slug: "ok", Name: strings.Repeat("é", 101)},
		{Slug: "ok", Name: "Ok", Description: strings.Repeat("d", 1001)},
	} {
		if _, err := svc.CreateTeam(ctx, admin, in); !errors.Is(err, identity.ErrInvalid) {
			t.Errorf("CreateTeam(%q, %q): %v, want ErrInvalid", in.Slug, in.Name, err)
		}
	}
	if got := auditLog(t, pool, mark); len(got) != 1 {
		t.Fatalf("rejected creations wrote audit entries: %v", summarize(got))
	}

	// Updates change the given fields and audit their names.
	mark = lastAuditID(t, pool)
	name, slug, empty := "Platform Engineering", "platform-eng", ""
	updated, err := svc.UpdateTeam(ctx, admin, team.ID, identity.TeamUpdate{Name: &name, Slug: &slug, Description: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != name || updated.Slug != slug || updated.Description != nil || !updated.UpdatedAt.After(team.UpdatedAt) {
		t.Fatalf("updated %+v", updated)
	}
	entries = auditLog(t, pool, mark)
	if got := summarize(entries); len(got) != 1 || got[0].Action != "team.updated" || got[0].TargetID != team.ID.String() {
		t.Fatalf("audit = %v", got)
	}
	if fields, _ := metadata(t, entries[0])["fields"].([]any); !slices.Equal(fields, []any{"slug", "name", "description"}) {
		t.Errorf("team.updated fields = %v", fields)
	}

	// An update that changes nothing writes nothing.
	mark = lastAuditID(t, pool)
	if _, err := svc.UpdateTeam(ctx, admin, team.ID, identity.TeamUpdate{Name: &name, Slug: &slug}); err != nil {
		t.Fatal(err)
	}
	if got := auditLog(t, pool, mark); len(got) != 0 {
		t.Fatalf("a no-op update wrote %v", summarize(got))
	}

	// The old slug is free again; taking a used one is refused.
	other := createTeam(t, svc, "platform", nil)
	if _, err := svc.UpdateTeam(ctx, admin, other.ID, identity.TeamUpdate{Slug: &slug}); !errors.Is(err, identity.ErrSlugTaken) {
		t.Fatalf("update to a taken slug: %v, want ErrSlugTaken", err)
	}
	if _, err := svc.UpdateTeam(ctx, admin, uuid.New(), identity.TeamUpdate{Name: &name}); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("update of an unknown team: %v, want ErrNotFound", err)
	}

	// Reads: list by name, get by ID and slug.
	rows, err := svc.Teams(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	// By name: "Platform Engineering", then "Team platform".
	if len(rows) != 2 || rows[0].Team.ID != team.ID || rows[1].Team.ID != other.ID {
		t.Fatalf("Teams = %+v", rows)
	}
	if got, err := svc.GetTeam(ctx, team.ID); err != nil || got.Team.Slug != slug || got.MemberCount != 0 {
		t.Fatalf("GetTeam = %+v, %v", got, err)
	}
	if got, err := svc.GetTeamBySlug(ctx, "PLATFORM-ENG"); err != nil || got.ID != team.ID {
		t.Fatalf("GetTeamBySlug = %+v, %v", got, err)
	}
	if _, err := svc.GetTeam(ctx, uuid.New()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("GetTeam(unknown) = %v, want ErrNotFound", err)
	}
}

func TestTeamMemberships(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	team := createTeam(t, svc, "platform", nil)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	bob := createPerson(t, svc, "bob", identity.StatusInvited)
	carol := createPerson(t, svc, "carol", identity.StatusActive)
	mark := lastAuditID(t, pool)

	setMember(t, svc, team.ID, alice.ID, identity.TeamRoleMember)
	setMember(t, svc, team.ID, bob.ID, identity.TeamRoleLead)
	// A team may have several leads.
	setMember(t, svc, team.ID, carol.ID, identity.TeamRoleMember)
	setMember(t, svc, team.ID, carol.ID, identity.TeamRoleLead)
	// Setting the role a member has changes nothing.
	setMember(t, svc, team.ID, carol.ID, identity.TeamRoleLead)
	// A lead becomes a member again.
	setMember(t, svc, team.ID, bob.ID, identity.TeamRoleMember)

	members, err := svc.TeamMembers(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	type member struct{ username, role string }
	var got []member
	for _, m := range members {
		got = append(got, member{m.Person.Username, m.Role})
	}
	if want := []member{{"carol", "lead"}, {"alice", "member"}, {"bob", "member"}}; !slices.Equal(got, want) {
		t.Fatalf("members = %v, want %v (leads first, then by name)", got, want)
	}
	if row, err := svc.GetTeam(ctx, team.ID); err != nil || row.MemberCount != 3 {
		t.Fatalf("member count = %d, %v", row.MemberCount, err)
	}

	entries := auditLog(t, pool, mark)
	want := []struct {
		action string
		meta   map[string]any
	}{
		{"team.member_added", map[string]any{"person_id": alice.ID.String(), "role": "member"}},
		{"team.member_added", map[string]any{"person_id": bob.ID.String(), "role": "lead"}},
		{"team.member_added", map[string]any{"person_id": carol.ID.String(), "role": "member"}},
		{"team.member_role_changed", map[string]any{"person_id": carol.ID.String(), "from": "member", "to": "lead"}},
		{"team.member_role_changed", map[string]any{"person_id": bob.ID.String(), "from": "lead", "to": "member"}},
	}
	if len(entries) != len(want) {
		t.Fatalf("audit = %v, want %d entries", summarize(entries), len(want))
	}
	for i, w := range want {
		e := entries[i]
		if e.Action != w.action || deref(e.TargetKind) != "team" || deref(e.TargetID) != team.ID.String() || deref(e.ActorID) != admin.ID {
			t.Errorf("entry %d = %v, want %s on the team", i, summarize(entries[i:i+1]), w.action)
		}
		m := metadata(t, e)
		for k, v := range w.meta {
			if m[k] != v {
				t.Errorf("entry %d (%s): %s = %v, want %v", i, w.action, k, m[k], v)
			}
		}
	}

	// A person's teams.
	other := createTeam(t, svc, "alpha", nil)
	setMember(t, svc, other.ID, carol.ID, identity.TeamRoleMember)
	teams, err := svc.PersonTeams(ctx, carol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 2 || teams[0].Team.Slug != "alpha" || teams[0].Role != "member" || teams[1].Team.Slug != "platform" ||
		teams[1].Role != "lead" || teams[1].MemberCount != 3 {
		t.Fatalf("PersonTeams = %+v", teams)
	}

	// Removing a member, and someone who is not one.
	mark = lastAuditID(t, pool)
	if err := svc.RemoveTeamMember(ctx, admin, team.ID, carol.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveTeamMember(ctx, admin, team.ID, carol.ID); err != nil {
		t.Fatalf("removing a non-member: %v", err)
	}
	entries = auditLog(t, pool, mark)
	if len(entries) != 1 || entries[0].Action != "team.member_removed" {
		t.Fatalf("audit = %v, want one team.member_removed", summarize(entries))
	}
	if m := metadata(t, entries[0]); m["person_id"] != carol.ID.String() || m["role"] != "lead" {
		t.Errorf("team.member_removed metadata = %v", m)
	}

	// Errors.
	mark = lastAuditID(t, pool)
	if _, err := svc.SetTeamMember(ctx, admin, team.ID, uuid.New(), identity.TeamRoleMember); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("unknown person: %v, want ErrNotFound", err)
	}
	if _, err := svc.SetTeamMember(ctx, admin, uuid.New(), alice.ID, identity.TeamRoleMember); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("unknown team: %v, want ErrNotFound", err)
	}
	if err := svc.RemoveTeamMember(ctx, admin, uuid.New(), alice.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("remove from an unknown team: %v, want ErrNotFound", err)
	}
	if _, err := svc.SetTeamMember(ctx, admin, team.ID, alice.ID, "owner"); !errors.Is(err, identity.ErrInvalid) {
		t.Errorf("unknown role: %v, want ErrInvalid", err)
	}
	if _, err := svc.TeamMembers(ctx, uuid.New()); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("members of an unknown team: %v, want ErrNotFound", err)
	}

	// Offboarded people cannot join, and are not listed or counted.
	dave := createPerson(t, svc, "dave", identity.StatusActive)
	if _, err := svc.SetPersonStatus(ctx, admin, dave.ID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetTeamMember(ctx, admin, team.ID, dave.ID, identity.TeamRoleMember); !errors.Is(err, identity.ErrPersonOffboarded) {
		t.Errorf("offboarded person: %v, want ErrPersonOffboarded", err)
	}
	if got := auditLog(t, pool, mark); len(got) != 2 || got[0].Action != "person.created" || got[1].Action != "person.status_changed" {
		t.Fatalf("rejected membership changes wrote %v", summarize(got))
	}
	setMember(t, svc, team.ID, carol.ID, identity.TeamRoleMember)
	if _, err := svc.SetPersonStatus(ctx, admin, carol.ID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetTeamMember(ctx, admin, team.ID, carol.ID, identity.TeamRoleLead); !errors.Is(err, identity.ErrPersonOffboarded) {
		t.Errorf("making an offboarded member lead: %v, want ErrPersonOffboarded", err)
	}
	members, err = svc.TeamMembers(ctx, team.ID)
	if err != nil || len(members) != 2 {
		t.Fatalf("members after offboarding = %d, %v; want 2", len(members), err)
	}
	if row, err := svc.GetTeam(ctx, team.ID); err != nil || row.MemberCount != 2 {
		t.Fatalf("member count after offboarding = %d, %v; want 2", row.MemberCount, err)
	}
	// The membership row stays for the record; it can still be removed.
	if n := count(t, pool, "SELECT count(*) FROM team_memberships WHERE person_id = $1", carol.ID); n != 2 {
		t.Fatalf("%d memberships of the offboarded person, want 2", n)
	}
	if err := svc.RemoveTeamMember(ctx, admin, team.ID, carol.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTeamArchiveKeepsHistory(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	team := createTeam(t, svc, "platform", nil)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	bob := createPerson(t, svc, "bob", identity.StatusActive)
	setMember(t, svc, team.ID, alice.ID, identity.TeamRoleLead)
	setMember(t, svc, team.ID, bob.ID, identity.TeamRoleMember)
	history := len(auditLog(t, pool, nil))
	mark := lastAuditID(t, pool)

	archived, err := svc.ArchiveTeam(ctx, admin, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.ArchivedAt == nil {
		t.Fatal("ArchivedAt is not set")
	}
	entries := auditLog(t, pool, mark)
	if got := summarize(entries); len(got) != 1 || got[0].Action != "team.archived" || got[0].TargetID != team.ID.String() {
		t.Fatalf("audit = %v", got)
	}
	// Archiving again changes nothing.
	mark = lastAuditID(t, pool)
	if again, err := svc.ArchiveTeam(ctx, admin, team.ID); err != nil || !again.ArchivedAt.Equal(*archived.ArchivedAt) {
		t.Fatalf("archive again = %+v, %v", again, err)
	}
	if got := auditLog(t, pool, mark); len(got) != 0 {
		t.Fatalf("archiving again wrote %v", summarize(got))
	}

	// The team, its members and its history stay.
	row, err := svc.GetTeam(ctx, team.ID)
	if err != nil || row.Team.ArchivedAt == nil || row.MemberCount != 2 {
		t.Fatalf("GetTeam(archived) = %+v, %v", row, err)
	}
	members, err := svc.TeamMembers(ctx, team.ID)
	if err != nil || len(members) != 2 || members[0].Person.ID != alice.ID || members[0].Role != "lead" {
		t.Fatalf("members of the archived team = %+v, %v", members, err)
	}
	if n := len(auditLog(t, pool, nil)); n != history+1 {
		t.Fatalf("%d audit entries after archiving, want %d", n, history+1)
	}
	if n := count(t, pool, "SELECT count(*) FROM audit_entries WHERE target_kind = 'team' AND target_id = $1", team.ID.String()); n != 4 {
		t.Fatalf("%d audit entries for the team, want 4 (created, two members added, archived)", n)
	}

	// It is read-only.
	name := "Renamed"
	if _, err := svc.UpdateTeam(ctx, admin, team.ID, identity.TeamUpdate{Name: &name}); !errors.Is(err, identity.ErrTeamArchived) {
		t.Errorf("update: %v, want ErrTeamArchived", err)
	}
	carol := createPerson(t, svc, "carol", identity.StatusActive)
	mark = lastAuditID(t, pool)
	if _, err := svc.SetTeamMember(ctx, admin, team.ID, carol.ID, identity.TeamRoleMember); !errors.Is(err, identity.ErrTeamArchived) {
		t.Errorf("add member: %v, want ErrTeamArchived", err)
	}
	if _, err := svc.SetTeamMember(ctx, admin, team.ID, bob.ID, identity.TeamRoleLead); !errors.Is(err, identity.ErrTeamArchived) {
		t.Errorf("make lead: %v, want ErrTeamArchived", err)
	}
	if err := svc.RemoveTeamMember(ctx, admin, team.ID, bob.ID); !errors.Is(err, identity.ErrTeamArchived) {
		t.Errorf("remove member: %v, want ErrTeamArchived", err)
	}
	if got := auditLog(t, pool, mark); len(got) != 0 {
		t.Fatalf("changes to an archived team wrote %v", summarize(got))
	}

	// It leaves the lists, but keeps its slug.
	if rows, err := svc.Teams(ctx, false); err != nil || len(rows) != 0 {
		t.Fatalf("Teams(active) = %+v, %v", rows, err)
	}
	if rows, err := svc.Teams(ctx, true); err != nil || len(rows) != 1 || rows[0].Team.ID != team.ID {
		t.Fatalf("Teams(archived too) = %+v, %v", rows, err)
	}
	if teams, err := svc.PersonTeams(ctx, alice.ID); err != nil || len(teams) != 0 {
		t.Fatalf("PersonTeams = %+v, %v", teams, err)
	}
	if _, err := svc.CreateTeam(ctx, admin, identity.NewTeam{Slug: "platform", Name: "New platform"}); !errors.Is(err, identity.ErrSlugTaken) {
		t.Fatalf("reusing an archived team's slug: %v, want ErrSlugTaken", err)
	}
	if _, err := svc.ArchiveTeam(ctx, admin, uuid.New()); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("archive an unknown team: %v, want ErrNotFound", err)
	}
}

func TestTeamHierarchy(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	engineering := createTeam(t, svc, "engineering", nil)
	platform := createTeam(t, svc, "platform", &engineering.ID)
	if platform.ParentID == nil || *platform.ParentID != engineering.ID {
		t.Fatalf("parent = %v", platform.ParentID)
	}
	firmware := createTeam(t, svc, "firmware", &platform.ID)

	mark := lastAuditID(t, pool)
	for _, tc := range []struct {
		name         string
		team, parent uuid.UUID
	}{
		{"itself", engineering.ID, engineering.ID},
		{"its child", engineering.ID, platform.ID},
		{"its grandchild", engineering.ID, firmware.ID},
		{"an unknown team", engineering.ID, uuid.New()},
		{"a leaf itself", firmware.ID, firmware.ID},
	} {
		if _, err := svc.UpdateTeam(ctx, admin, tc.team, identity.TeamUpdate{ParentID: &tc.parent}); !errors.Is(err, identity.ErrInvalid) {
			t.Errorf("parent %s: %v, want ErrInvalid", tc.name, err)
		}
	}
	if got := auditLog(t, pool, mark); len(got) != 0 {
		t.Fatalf("rejected parent changes wrote %v", summarize(got))
	}

	// A team with active child teams cannot be archived; archived teams
	// cannot be parents.
	if _, err := svc.ArchiveTeam(ctx, admin, platform.ID); !errors.Is(err, identity.ErrTeamHasChildren) {
		t.Fatalf("archive a parent: %v, want ErrTeamHasChildren", err)
	}
	none := uuid.Nil
	moved, err := svc.UpdateTeam(ctx, admin, firmware.ID, identity.TeamUpdate{ParentID: &none})
	if err != nil || moved.ParentID != nil {
		t.Fatalf("remove parent = %+v, %v", moved, err)
	}
	if _, err := svc.ArchiveTeam(ctx, admin, platform.ID); err != nil {
		t.Fatalf("archive a team without children: %v", err)
	}
	if _, err := svc.UpdateTeam(ctx, admin, firmware.ID, identity.TeamUpdate{ParentID: &platform.ID}); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("archived parent: %v, want ErrInvalid", err)
	}
	if _, err := svc.CreateTeam(ctx, admin, identity.NewTeam{Slug: "drivers", Name: "Drivers", ParentID: &platform.ID}); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("create under an archived parent: %v, want ErrInvalid", err)
	}
	moved, err = svc.UpdateTeam(ctx, admin, firmware.ID, identity.TeamUpdate{ParentID: &engineering.ID})
	if err != nil || moved.ParentID == nil || *moved.ParentID != engineering.ID {
		t.Fatalf("move = %+v, %v", moved, err)
	}
}
