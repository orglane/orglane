//go:build integration

package web_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
)

type apiTeam struct {
	ID          uuid.UUID  `json:"id"`
	Slug        string     `json:"slug"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	ParentID    *uuid.UUID `json:"parentId"`
	MemberCount int        `json:"memberCount"`
	ArchivedAt  *string    `json:"archivedAt"`
}

type apiTeamMember struct {
	Person struct {
		ID          uuid.UUID `json:"id"`
		Username    string    `json:"username"`
		DisplayName string    `json:"displayName"`
	} `json:"person"`
	Role string `json:"role"`
}

// TestTeamsAPIPermissions checks who may read and manage teams: every
// signed-in person reads, people.manage (Owner, Admin) manages, and nobody
// without a session does either.
func TestTeamsAPIPermissions(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)

	adminPerson := e.createMember("adele")
	if err := e.stack.Identity.AssignRole(context.Background(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.signIn("adele@acme.test")
	memberPerson := e.createMember("max")
	member := e.browser()
	member.signIn("max@acme.test")
	anonymous := e.browser()

	// An admin creates a team, adds two people and makes one the lead.
	r := admin.req("POST", "/api/v1/teams", map[string]string{"name": "Platform", "slug": "platform", "description": "Robots and cloud."})
	expectStatus(t, r, http.StatusCreated)
	var team apiTeam
	r.json(t, &team)
	if team.Slug != "platform" || team.Name != "Platform" || team.Description == nil || team.MemberCount != 0 || team.ArchivedAt != nil {
		t.Fatalf("created %+v", team)
	}
	teamPath := "/api/v1/teams/" + team.ID.String()
	expectStatus(t, admin.req("PUT", teamPath+"/members/"+memberPerson.ID.String(), map[string]string{"role": "member"}), http.StatusOK)
	r = admin.req("PUT", teamPath+"/members/"+adminPerson.ID.String(), map[string]string{"role": "lead"})
	expectStatus(t, r, http.StatusOK)
	var membership struct {
		TeamID   uuid.UUID `json:"teamId"`
		PersonID uuid.UUID `json:"personId"`
		Role     string    `json:"role"`
	}
	r.json(t, &membership)
	if membership.TeamID != team.ID || membership.PersonID != adminPerson.ID || membership.Role != "lead" {
		t.Fatalf("membership = %+v", membership)
	}

	// The audit entries name the admin and where the request came from.
	var actorID, ip string
	if err := e.pool.QueryRow(context.Background(), `SELECT actor_id, host(ip) FROM audit_entries WHERE action = 'team.created'`).Scan(&actorID, &ip); err != nil {
		t.Fatal(err)
	}
	if actorID != adminPerson.ID.String() || ip == "" {
		t.Fatalf("team.created by %q from %q, want the admin with an address", actorID, ip)
	}

	// Everyone signed in reads teams, members and a person's teams.
	for _, b := range []*browser{owner, admin, member} {
		r = b.req("GET", "/api/v1/teams", nil)
		expectStatus(t, r, http.StatusOK)
		var list struct{ Items []apiTeam }
		r.json(t, &list)
		if len(list.Items) != 1 || list.Items[0].ID != team.ID || list.Items[0].MemberCount != 2 {
			t.Fatalf("teams = %+v", list.Items)
		}
		expectStatus(t, b.req("GET", teamPath, nil), http.StatusOK)
		r = b.req("GET", teamPath+"/members", nil)
		expectStatus(t, r, http.StatusOK)
		var members struct{ Items []apiTeamMember }
		r.json(t, &members)
		if len(members.Items) != 2 || members.Items[0].Person.ID != adminPerson.ID || members.Items[0].Role != "lead" ||
			members.Items[1].Person.Username != "max" || members.Items[1].Role != "member" {
			t.Fatalf("members = %+v", members.Items)
		}
		r = b.req("GET", "/api/v1/people/"+memberPerson.ID.String()+"/teams", nil)
		expectStatus(t, r, http.StatusOK)
		var mine struct {
			Items []struct {
				Team apiTeam `json:"team"`
				Role string  `json:"role"`
			}
		}
		r.json(t, &mine)
		if len(mine.Items) != 1 || mine.Items[0].Team.ID != team.ID || mine.Items[0].Role != "member" {
			t.Fatalf("person teams = %+v", mine.Items)
		}
	}

	// A member can manage nothing; nothing is written.
	before := e.auditCount(audit.TeamMemberAdded) + e.auditCount(audit.TeamMemberRemoved) + e.auditCount(audit.TeamUpdated) +
		e.auditCount(audit.TeamCreated) + e.auditCount(audit.TeamArchived) + e.auditCount(audit.TeamMemberRoleChanged)
	denied := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/teams", map[string]string{"name": "Shadow", "slug": "shadow"}},
		{"PATCH", teamPath, map[string]string{"name": "Mine now"}},
		{"POST", teamPath + "/archive", nil},
		{"PUT", teamPath + "/members/" + memberPerson.ID.String(), map[string]string{"role": "lead"}},
		{"DELETE", teamPath + "/members/" + adminPerson.ID.String(), nil},
	}
	for _, d := range denied {
		expectStatus(t, member.req(d.method, d.path, d.body), http.StatusForbidden)
		expectStatus(t, anonymous.req(d.method, d.path, d.body), http.StatusUnauthorized)
	}
	for _, path := range []string{"/api/v1/teams", teamPath, teamPath + "/members", "/api/v1/people/" + memberPerson.ID.String() + "/teams"} {
		expectStatus(t, anonymous.req("GET", path, nil), http.StatusUnauthorized)
	}
	after := e.auditCount(audit.TeamMemberAdded) + e.auditCount(audit.TeamMemberRemoved) + e.auditCount(audit.TeamUpdated) +
		e.auditCount(audit.TeamCreated) + e.auditCount(audit.TeamArchived) + e.auditCount(audit.TeamMemberRoleChanged)
	if after != before {
		t.Fatalf("denied requests wrote %d team audit entries", after-before)
	}

	// Errors are problem details with the right status.
	expectStatus(t, admin.req("POST", "/api/v1/teams", map[string]string{"name": "Again", "slug": "PLATFORM"}), http.StatusConflict)
	expectStatus(t, admin.req("POST", "/api/v1/teams", map[string]string{"name": "Bad", "slug": "-bad-"}), http.StatusBadRequest)
	expectStatus(t, admin.req("POST", "/api/v1/teams", map[string]string{"slug": "nameless"}), http.StatusBadRequest)
	expectStatus(t, admin.req("PATCH", teamPath, map[string]string{"parentId": "not-a-uuid"}), http.StatusBadRequest)
	expectStatus(t, admin.req("PATCH", teamPath, map[string]string{"parentId": team.ID.String()}), http.StatusBadRequest)
	expectStatus(t, admin.req("PUT", teamPath+"/members/"+memberPerson.ID.String(), map[string]string{"role": "owner"}), http.StatusBadRequest)
	expectStatus(t, admin.req("PUT", teamPath+"/members/"+uuid.NewString(), map[string]string{"role": "member"}), http.StatusNotFound)
	expectStatus(t, admin.req("GET", "/api/v1/teams/"+uuid.NewString(), nil), http.StatusNotFound)
	expectStatus(t, admin.req("GET", "/api/v1/teams/"+uuid.NewString()+"/members", nil), http.StatusNotFound)
	expectStatus(t, admin.req("GET", "/api/v1/people/"+uuid.NewString()+"/teams", nil), http.StatusNotFound)

	// Update, remove a member, archive: the team keeps its members and
	// becomes read-only.
	r = admin.req("PATCH", teamPath, map[string]string{"name": "Platform Engineering", "description": ""})
	expectStatus(t, r, http.StatusOK)
	team = apiTeam{}
	r.json(t, &team)
	if team.Name != "Platform Engineering" || team.Description != nil || team.MemberCount != 2 {
		t.Fatalf("updated %+v", team)
	}
	expectStatus(t, admin.req("DELETE", teamPath+"/members/"+memberPerson.ID.String(), nil), http.StatusNoContent)
	expectStatus(t, admin.req("DELETE", teamPath+"/members/"+memberPerson.ID.String(), nil), http.StatusNoContent)
	r = owner.req("POST", teamPath+"/archive", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &team)
	if team.ArchivedAt == nil || team.MemberCount != 1 {
		t.Fatalf("archived %+v", team)
	}
	expectStatus(t, admin.req("PUT", teamPath+"/members/"+memberPerson.ID.String(), map[string]string{"role": "member"}), http.StatusConflict)
	expectStatus(t, admin.req("PATCH", teamPath, map[string]string{"name": "Zombie"}), http.StatusConflict)
	var list struct{ Items []apiTeam }
	r = member.req("GET", "/api/v1/teams", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &list)
	if len(list.Items) != 0 {
		t.Fatalf("archived team listed: %+v", list.Items)
	}
	r = member.req("GET", "/api/v1/teams?archived=true", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &list)
	if len(list.Items) != 1 || list.Items[0].ArchivedAt == nil {
		t.Fatalf("teams with archived = %+v", list.Items)
	}
	expectStatus(t, member.req("GET", teamPath+"/members", nil), http.StatusOK)
	for action, want := range map[audit.Action]int{
		audit.TeamCreated: 1, audit.TeamMemberAdded: 2, audit.TeamUpdated: 1, audit.TeamMemberRemoved: 1, audit.TeamArchived: 1,
	} {
		if got := e.auditCount(action); got != want {
			t.Errorf("%d %s entries, want %d", got, action, want)
		}
	}
}
