//go:build integration

package web_test

import (
	"bytes"
	"image"
	"image/png"
	"net/http"
	"testing"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
)

func TestPeopleDirectoryAPIAndBasePermissions(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	subject := e.createMember("annie")
	other := e.createMember("ben")
	member := e.signedIn(subject.WorkEmail)
	path := "/api/v1/people/" + subject.ID.String()
	r := member.req("GET", "/api/v1/people?limit=1", nil)
	expectStatus(t, r, 200)
	etag := r.Header.Get("ETag")
	if etag == "" || r.Header.Get("Cache-Control") != "private, no-cache" {
		t.Fatal("directory lacks private validator")
	}
	expectStatus(t, member.req("GET", "/api/v1/people?limit=1", nil, "If-None-Match", etag), 304)
	expectStatus(t, e.browser().req("GET", "/api/v1/people?limit=1", nil, "If-None-Match", etag), 401)
	for _, query := range []string{"limit=0", "limit=101", "cursor=bad", "updated_since=bad", "updated_since=2020-01-01T00:00:00Z&q=ann"} {
		expectStatus(t, member.req("GET", "/api/v1/people?"+query, nil), 400)
	}
	expectStatus(t, member.req("PATCH", path, map[string]any{"displayName": "Annie Engineer", "title": "Robotics Engineer"}), 200)
	expectStatus(t, member.req("PATCH", path, map[string]any{"username": "imposter"}), 403)
	expectStatus(t, member.req("PATCH", path, map[string]any{"workEmail": "imposter@example.test"}), 403)
	expectStatus(t, member.req("PATCH", path, map[string]any{"managerId": other.ID.String()}), 403)
	expectStatus(t, member.req("PATCH", "/api/v1/people/"+other.ID.String(), map[string]any{"title": "Changed"}), 403)
	expectStatus(t, member.req("PATCH", path, map[string]any{"title": "x", "status": "offboarded"}), 400)
	expectStatus(t, owner.req("PATCH", path, map[string]any{"username": "annie.engineer", "workEmail": "annie.engineer@example.test", "managerId": other.ID.String()}), 200)
	expectStatus(t, owner.req("PATCH", "/api/v1/people/"+other.ID.String(), map[string]any{"managerId": subject.ID.String()}), 400)
	r = member.req("GET", path, nil)
	expectStatus(t, r, 200)
	var person struct {
		DisplayName string
		ManagerID   string
		Title       string
	}
	r.json(t, &person)
	if person.DisplayName != "Annie Engineer" || person.ManagerID != other.ID.String() || person.Title != "Robotics Engineer" {
		t.Fatalf("profile header %+v", person)
	}
	expectStatus(t, member.req("GET", "/api/v1/people?q=annie.engineer%40example.test", nil), 200)
	expectStatus(t, member.req("GET", "/api/v1/people/0190f3c4-0000-7000-8000-000000000001", nil), 404)
	r = member.req("GET", "/api/v1/people?limit=1", nil, "If-None-Match", etag)
	// Any content change on this page invalidates its validator.
	expectStatus(t, r, 304) // The owner's first UUID row is unchanged.

	target := member.req("GET", "/api/v1/people?q=annie.engineer", nil)
	expectStatus(t, target, 200)
	targetETag := target.Header.Get("ETag")
	expectStatus(t, member.req("PATCH", path, map[string]any{"title": "Directory cache revision"}), 200)
	expectStatus(t, member.req("GET", "/api/v1/people?q=annie.engineer", nil, "If-None-Match", targetETag), 200)
	target = member.req("GET", "/api/v1/people?q=annie.engineer", nil)
	targetETag = target.Header.Get("ETag")
	expectStatus(t, owner.req("POST", "/api/v1/profile-field-library/emergency_contact", nil), 201)
	expectStatus(t, owner.req("PATCH", path+"/profile", map[string]any{"values": []map[string]any{{"key": "emergency_contact", "value": "Private secret value"}}}), 200)
	expectStatus(t, member.req("GET", "/api/v1/people?q=annie.engineer", nil, "If-None-Match", targetETag), 304)
	if _, err := e.pool.Exec(t.Context(), `UPDATE people SET contact_email='hidden@personal.test',updated_at=clock_timestamp() WHERE id=$1`, subject.ID); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, member.req("GET", "/api/v1/people?q=annie.engineer", nil, "If-None-Match", targetETag), 304)
	public := member.req("GET", path, nil)
	if bytes.Contains(public.body, []byte("hidden@personal.test")) {
		t.Fatal("private contact leaked into public person")
	}
	hidden := member.req("GET", "/api/v1/people?q=Private", nil)
	expectStatus(t, hidden, 200)
	var noMatches struct{ Items []any }
	hidden.json(t, &noMatches)
	if len(noMatches.Items) != 0 {
		t.Fatal("directory searches leaked a private custom value")
	}
	if _, err := e.stack.Identity.SetPersonStatus(t.Context(), audit.System, other.ID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	// The admin form resubmits the existing manager alongside base edits.
	expectStatus(t, owner.req("PATCH", path, map[string]any{"title": "Still editable", "displayName": "Annie Updated", "managerId": other.ID.String()}), 200)
	delta := member.req("GET", "/api/v1/people?updated_since=2020-01-01T00:00:00Z", nil)
	expectStatus(t, delta, 200)
	var cache struct {
		Items []struct {
			ID          string
			Status      string
			WorkEmail   string
			DisplayName string
			Roles       []string
			Teams       []any
		}
	}
	delta.json(t, &cache)
	found := false
	for _, p := range cache.Items {
		if p.ID == other.ID.String() {
			found = true
			if p.Status != "offboarded" || p.WorkEmail != "" || p.DisplayName != "" || len(p.Roles) != 0 || len(p.Teams) != 0 {
				t.Fatal("tombstone leaked offboarded profile data")
			}
		}
	}
	if !found {
		t.Fatal("incremental response omitted tombstone")
	}
}

func TestAvatarUploadAPIRejectsUnsafeAndUnauthorizedWrites(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	subject := e.createMember("annie")
	other := e.createMember("ben")
	member := e.signedIn(subject.WorkEmail)
	path := "/api/v1/people/" + subject.ID.String() + "/avatar"
	var input bytes.Buffer
	if err := png.Encode(&input, image.NewNRGBA(image.Rect(0, 0, 32, 16))); err != nil {
		t.Fatal(err)
	}
	before := e.auditCount(audit.PersonAvatarSet)
	expectStatus(t, member.req("PUT", path, input.Bytes(), "Content-Type", "image/png"), 204)
	if e.auditCount(audit.PersonAvatarSet) != before+1 {
		t.Fatal("missing upload audit")
	}
	got, err := e.stack.Identity.GetAvatar(t.Context(), subject.ID)
	if err != nil || got.ContentType != identity.AvatarPNG {
		t.Fatalf("persisted upload: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(got.Data)); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, member.req("PUT", "/api/v1/people/"+other.ID.String()+"/avatar", input.Bytes(), "Content-Type", "image/png"), 403)
	expectStatus(t, e.browser().req("PUT", path, input.Bytes(), "Content-Type", "image/png"), 401)
	expectStatus(t, member.req("PUT", path, input.Bytes(), "Content-Type", "image/png", "X-CSRF-Token", ""), 403)
	expectStatus(t, member.req("PUT", path, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "Content-Type", "image/svg+xml"), 415)
	expectStatus(t, member.req("PUT", path, []byte("\x89PNG\r\n\x1a\ninvalid"), "Content-Type", "image/png"), 400)
	expectStatus(t, member.req("PUT", path, input.Bytes(), "Content-Type", "image/jpeg"), 400)
	expectStatus(t, member.req("PUT", path, make([]byte, identity.MaxAvatarUploadBytes+1), "Content-Type", "image/png"), 413)
	if e.auditCount(audit.PersonAvatarSet) != before+1 {
		t.Fatal("rejected upload wrote an audit entry")
	}
	expectStatus(t, owner.req("PUT", path, input.Bytes(), "Content-Type", "image/png"), 204)
	response := member.req("GET", path, nil)
	expectStatus(t, response, http.StatusOK)
	if response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("avatar response is not inert")
	}
}
