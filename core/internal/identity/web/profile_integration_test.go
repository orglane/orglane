//go:build integration

package web_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
)

type apiField struct {
	ID         string `json:"id"`
	Key        string `json:"key"`
	Library    bool   `json:"library"`
	Visibility string `json:"visibility"`
	Archived   bool   `json:"archived"`
	Options    []struct {
		Key   string `json:"key"`
		Label string `json:"label"`
	} `json:"options"`
}

type apiProfile struct {
	Fields []struct {
		Field      apiField `json:"field"`
		Value      *string  `json:"value"`
		YearHidden bool     `json:"yearHidden"`
		Editable   bool     `json:"editable"`
	} `json:"fields"`
}

func (p apiProfile) values() map[string]string {
	out := map[string]string{}
	for _, f := range p.Fields {
		if f.Value != nil {
			out[f.Field.Key] = *f.Value
		}
	}
	return out
}

// TestProfileFieldsAPI covers the profile field operations end to end:
// managing definitions needs people.manage, and profiles follow the self
// and admin rules of package profile.
func TestProfileFieldsAPI(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	ownerID := e.setupOwner(owner).Person.ID
	adminPerson := e.createMember("ada")
	if err := e.stack.Identity.AssignRole(context.Background(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.signIn(adminPerson.WorkEmail)
	maxPerson := e.createMember("max")
	member := e.browser()
	member.signIn(maxPerson.WorkEmail)
	other := e.createMember("mia")
	anonymous := e.browser()

	// Definitions: people.manage only.
	for _, op := range []struct{ method, path string }{
		{"GET", "/api/v1/profile-fields"},
		{"POST", "/api/v1/profile-fields"},
		{"PATCH", "/api/v1/profile-fields/0190f3c4-0000-7000-8000-000000000001"},
		{"GET", "/api/v1/profile-field-library"},
		{"POST", "/api/v1/profile-field-library/telegram"},
	} {
		body := any(nil)
		if op.method != "GET" {
			body = map[string]any{"label": "X", "type": "text"}
		}
		expectStatus(t, member.req(op.method, op.path, body), http.StatusForbidden)
		expectStatus(t, anonymous.req(op.method, op.path, body), http.StatusUnauthorized)
	}

	// The admin turns on library fields and creates a custom select field.
	expectStatus(t, admin.req("POST", "/api/v1/profile-field-library/telegram", nil), http.StatusCreated)
	expectStatus(t, admin.req("POST", "/api/v1/profile-field-library/telegram", nil), http.StatusOK)
	expectStatus(t, admin.req("POST", "/api/v1/profile-field-library/birthday", nil), http.StatusCreated)
	expectStatus(t, admin.req("POST", "/api/v1/profile-field-library/emergency_contact", nil), http.StatusCreated)
	expectStatus(t, admin.req("POST", "/api/v1/profile-field-library/nope", nil), http.StatusNotFound)
	r := admin.req("POST", "/api/v1/profile-fields", map[string]any{
		"label": "Office", "type": "select", "visibility": "admins",
		"options": []map[string]string{{"label": "Berlin"}, {"label": "Lisbon"}},
	})
	expectStatus(t, r, http.StatusCreated)
	var office apiField
	r.json(t, &office)
	if office.Key != "office" || len(office.Options) != 2 || office.Options[1].Key != "lisbon" {
		t.Fatalf("created field: %+v", office)
	}
	expectStatus(t, admin.req("POST", "/api/v1/profile-fields", map[string]any{"key": "office", "label": "Office 2", "type": "text"}), http.StatusConflict)
	expectStatus(t, admin.req("POST", "/api/v1/profile-fields", map[string]any{"label": "Bad", "type": "number"}), http.StatusBadRequest)
	expectStatus(t, admin.req("PATCH", "/api/v1/profile-fields/"+office.ID, map[string]any{"label": "Office location"}), http.StatusOK)
	expectStatus(t, admin.req("PATCH", "/api/v1/profile-fields/0190f3c4-0000-7000-8000-000000000001", map[string]any{"label": "X"}), http.StatusNotFound)

	var lib struct {
		Items []struct {
			Key   string    `json:"key"`
			Field *apiField `json:"field"`
		} `json:"items"`
	}
	r = admin.req("GET", "/api/v1/profile-field-library", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &lib)
	enabled := map[string]bool{}
	for _, it := range lib.Items {
		enabled[it.Key] = it.Field != nil && !it.Field.Archived
	}
	if !enabled["telegram"] || !enabled["birthday"] || enabled["phone"] {
		t.Errorf("library state: %v", enabled)
	}

	// Max fills in their own profile; editing someone else's is forbidden.
	maxProfile := "/api/v1/people/" + maxPerson.ID.String() + "/profile"
	r = member.req("PATCH", maxProfile, map[string]any{"values": []map[string]any{
		{"key": "telegram", "value": "@max_tg"}, {"key": "birthday", "value": "1991-07-04"},
		{"key": "office", "value": "lisbon"},
	}})
	expectStatus(t, r, http.StatusOK)
	var p apiProfile
	r.json(t, &p)
	if got := p.values(); got["telegram"] != "max_tg" || got["birthday"] != "1991-07-04" || got["office"] != "lisbon" || got["emergency_contact"] != "" {
		t.Errorf("own profile after the change: %v", got)
	}
	expectStatus(t, member.req("PATCH", maxProfile, map[string]any{"values": []map[string]any{{"key": "birthday", "value": "July 4th"}}}), http.StatusBadRequest)
	expectStatus(t, member.req("PATCH", "/api/v1/people/"+other.ID.String()+"/profile",
		map[string]any{"values": []map[string]any{{"key": "telegram", "value": "mia_tg"}}}), http.StatusForbidden)
	expectStatus(t, anonymous.req("GET", maxProfile, nil), http.StatusUnauthorized)
	expectStatus(t, member.req("GET", "/api/v1/people/0190f3c4-0000-7000-8000-000000000001/profile", nil), http.StatusNotFound)

	// Sensitive fields require the scope even for the person themselves.
	expectStatus(t, member.req("PATCH", maxProfile, map[string]any{"values": []map[string]any{{"key": "emergency_contact", "value": "Mom"}}}), http.StatusBadRequest)
	expectStatus(t, owner.req("PATCH", maxProfile, map[string]any{"values": []map[string]any{{"key": "emergency_contact", "value": "Mom"}}}), http.StatusOK)

	// An admin cannot declassify or edit a sensitive value without its scope.
	var sensitiveID string
	for _, item := range lib.Items {
		if item.Key == "emergency_contact" {
			sensitiveID = item.Field.ID
		}
	}
	if sensitiveID == "" {
		t.Fatal("missing sensitive field")
	}
	for _, body := range []map[string]any{{"sensitive": false}, {"sensitive": false, "visibility": "everyone"}} {
		expectStatus(t, admin.req("PATCH", "/api/v1/profile-fields/"+sensitiveID, body), http.StatusForbidden)
	}
	// Type is immutable, and visibility changes cannot bypass sensitivity.
	r = admin.req("PATCH", "/api/v1/profile-fields/"+sensitiveID, map[string]any{"visibility": "everyone", "type": "text"})
	expectStatus(t, r, http.StatusOK)
	var definition struct {
		Type      string `json:"type"`
		Sensitive bool   `json:"sensitive"`
	}
	r.json(t, &definition)
	if definition.Type != "long_text" || !definition.Sensitive {
		t.Errorf("sensitive definition changed: %+v", definition)
	}
	expectStatus(t, admin.req("PATCH", maxProfile, map[string]any{"values": []map[string]any{{"key": "emergency_contact", "value": "Probe"}}}), http.StatusBadRequest)
	for _, b := range []*browser{admin, member} {
		r := b.req("PATCH", maxProfile, map[string]any{"values": []map[string]any{}})
		expectStatus(t, r, http.StatusOK)
		var response apiProfile
		r.json(t, &response)
		for _, entry := range response.Fields {
			if entry.Field.Key == "emergency_contact" {
				t.Error("update response leaked sensitive field")
			}
		}
	}

	// What each caller sees of Max's profile.
	for name, tc := range map[string]struct {
		b    *browser
		want map[string]string
	}{
		"another member": {e.signedIn(other.WorkEmail), map[string]string{"telegram": "max_tg", "birthday": "--07-04"}},
		"admin":          {admin, map[string]string{"telegram": "max_tg", "birthday": "--07-04", "office": "lisbon"}},
		"owner":          {owner, map[string]string{"telegram": "max_tg", "birthday": "1991-07-04", "office": "lisbon", "emergency_contact": "Mom"}},
	} {
		r := tc.b.req("GET", maxProfile, nil)
		expectStatus(t, r, http.StatusOK)
		var p apiProfile
		r.json(t, &p)
		got := p.values()
		if len(got) != len(tc.want) {
			t.Errorf("%s sees %v, want %v", name, got, tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s sees %s = %q, want %q", name, k, got[k], v)
			}
		}
	}

	// The admin edits Max's profile; the owner's sensitive read is audited.
	expectStatus(t, admin.req("PATCH", maxProfile, map[string]any{"values": []map[string]any{{"key": "office", "value": "berlin"}}}), http.StatusOK)
	before := e.auditCount(audit.PersonSensitiveFieldsRead)
	expectStatus(t, owner.req("GET", maxProfile, nil), http.StatusOK)
	if n := e.auditCount(audit.PersonSensitiveFieldsRead); n != before+1 {
		t.Errorf("the owner's sensitive read was audited %d times", n-before)
	}
	expectStatus(t, owner.req("GET", "/api/v1/people/"+ownerID.String()+"/profile", nil), http.StatusOK)

	// Archiving a field hides it from profiles.
	expectStatus(t, admin.req("PATCH", "/api/v1/profile-fields/"+office.ID, map[string]any{"archived": true}), http.StatusOK)
	r = admin.req("GET", maxProfile, nil)
	r.json(t, &p)
	if _, ok := p.values()["office"]; ok {
		t.Error("an archived field is still in the profile")
	}
	var list struct {
		Items []apiField `json:"items"`
	}
	r = admin.req("GET", "/api/v1/profile-fields?includeArchived=true", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &list)
	if len(list.Items) != 4 {
		t.Errorf("%d fields with archived ones, want 4", len(list.Items))
	}
	r = admin.req("GET", "/api/v1/profile-fields", nil)
	r.json(t, &list)
	if len(list.Items) != 3 {
		t.Errorf("%d active fields, want 3", len(list.Items))
	}
}

// signedIn returns a browser signed in as the person with the given email.
func (e *env) signedIn(email string) *browser {
	e.t.Helper()
	b := e.browser()
	b.signIn(email)
	return b
}

// This exercises production NewStack wiring, not a directly injected resolver.
func TestProfileTeamVisibilityUsesProductionResolver(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	subject := e.createMember("subject")
	teammate := e.createMember("teammate")
	outsider := e.createMember("outsider")
	team, err := e.stack.Identity.CreateTeam(t.Context(), audit.System, identity.NewTeam{Slug: "platform", Name: "Platform"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{subject.ID, teammate.ID} {
		if _, err := e.stack.Identity.SetTeamMember(t.Context(), audit.System, team.ID, id, identity.TeamRoleMember); err != nil {
			t.Fatal(err)
		}
	}
	expectStatus(t, owner.req("POST", "/api/v1/profile-fields", map[string]any{"label": "Team note", "type": "text", "visibility": "team"}), http.StatusCreated)
	path := "/api/v1/people/" + subject.ID.String() + "/profile"
	expectStatus(t, owner.req("PATCH", path, map[string]any{"values": []map[string]any{{"key": "team_note", "value": "Shared team value"}}}), http.StatusOK)
	reader := e.signedIn(teammate.WorkEmail)
	var visible apiProfile
	response := reader.req("GET", path, nil)
	expectStatus(t, response, http.StatusOK)
	response.json(t, &visible)
	if visible.values()["team_note"] != "Shared team value" {
		t.Error("production stack did not expose team value to an actual teammate")
	}
	response = e.signedIn(outsider.WorkEmail).req("GET", path, nil)
	expectStatus(t, response, http.StatusOK)
	response.json(t, &visible)
	if _, ok := visible.values()["team_note"]; ok {
		t.Error("production stack exposed team field to outsider")
	}
	expectStatus(t, owner.req("POST", "/api/v1/teams/"+team.ID.String()+"/archive", nil), http.StatusOK)
	response = reader.req("GET", path, nil)
	expectStatus(t, response, http.StatusOK)
	response.json(t, &visible)
	if _, ok := visible.values()["team_note"]; ok {
		t.Error("production stack grants team field visibility through archived team")
	}
}
