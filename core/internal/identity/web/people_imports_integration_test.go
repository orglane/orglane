//go:build integration

package web_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/peopleimport"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/tests/mailtest"
)

func importMapping(targets ...string) peopleimport.Mapping {
	out := peopleimport.Mapping{Columns: []peopleimport.Column{}, SkipRows: []int{}}
	for i, target := range targets {
		out.Columns = append(out.Columns, peopleimport.Column{Column: i, Target: target})
	}
	return out
}
func stageImport(t *testing.T, b *browser, text string) peopleimport.Run {
	t.Helper()
	r := b.req(http.MethodPost, "/api/v1/people-imports", strings.NewReader(text))
	expectStatus(t, r, http.StatusCreated)
	var run peopleimport.Run
	r.json(t, &run)
	return run
}
func previewImport(t *testing.T, b *browser, run peopleimport.Run, mapping peopleimport.Mapping) peopleimport.Run {
	t.Helper()
	r := b.req(http.MethodPost, "/api/v1/people-imports/"+run.ID.String()+"/preview", mapping)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &run)
	return run
}
func applyImport(t *testing.T, b *browser, run peopleimport.Run) peopleimport.Run {
	t.Helper()
	r := b.req(http.MethodPost, "/api/v1/people-imports/"+run.ID.String()+"/apply", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &run)
	return run
}
func TestPeopleImportPublicAPIAndPermissions(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	memberPerson := e.createMember("importmember")
	member := e.browser()
	member.signIn(memberPerson.WorkEmail)
	run := stageImport(t, owner, "username,work_email,display_name\nnewperson,newperson@acme.test,New Person\n")
	paths := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v1/people-imports/template", nil},
		{http.MethodPost, "/api/v1/people-imports", strings.NewReader("a\nb\n")},
		{http.MethodGet, "/api/v1/people-imports/" + run.ID.String(), nil},
		{http.MethodPost, "/api/v1/people-imports/" + run.ID.String() + "/preview", importMapping("username", "work_email", "display_name")},
		{http.MethodPost, "/api/v1/people-imports/" + run.ID.String() + "/apply", nil},
		{http.MethodGet, "/api/v1/people-imports/" + run.ID.String() + "/errors", nil},
	}
	for _, p := range paths {
		expectStatus(t, member.req(p.method, p.path, p.body), http.StatusForbidden)
	}
	expectStatus(t, owner.req(http.MethodGet, "/api/v1/people-imports/"+uuid.NewString(), nil), http.StatusNotFound)
	expectStatus(t, owner.req(http.MethodPost, "/api/v1/people-imports/"+run.ID.String()+"/apply", nil), http.StatusConflict)
	run = previewImport(t, owner, run, importMapping("username", "work_email", "display_name"))
	if run.Rows[0].Decision != "create" {
		t.Fatalf("preview=%+v", run.Rows)
	}
	run = applyImport(t, owner, run)
	if run.Rows[0].Result != "created" {
		t.Fatalf("result=%+v", run.Rows)
	}
	template := owner.req(http.MethodGet, "/api/v1/people-imports/template", nil)
	expectStatus(t, template, http.StatusOK)
	if !strings.HasPrefix(template.Header.Get("Content-Type"), "text/csv") || !bytes.Contains(template.body, []byte("work_email")) {
		t.Fatal("template content/type invalid")
	}
}

func TestImportedEmailUsesExistingSecureChannelAndCeiling(t *testing.T) {
	mp := mailtest.Start(t)
	e := newEnv(t)
	e.startJobs()
	owner := e.browser()
	e.setupOwner(owner)
	owner.useMailpit(mp)
	mapping := importMapping("username", "work_email", "contact_email", "display_name", "role")
	mapping.SendInvitations = true
	run := stageImport(t, owner, "username,work_email,contact_email,display_name,role\nmailimport,mailimport@acme.test,mailimport@home.test,Mail Import,member\nprivileged,privileged@acme.test,privileged@home.test,Privileged,owner\n")
	run = previewImport(t, owner, run, mapping)
	if run.Rows[0].Decision != "create" || run.Rows[1].Error != "invitation_delivery_unavailable" {
		t.Fatalf("channel preview=%+v", run.Rows)
	}
	run = applyImport(t, owner, run)
	if run.Rows[0].Result != "created" || run.Rows[1].Result != "failed" {
		t.Fatalf("results=%+v", run.Rows)
	}
	messages := mp.Wait(t, "mailimport@home.test", 1, 10*time.Second)
	if len(messages) != 1 || len(mp.Messages(t, "privileged@home.test")) != 0 {
		t.Fatal("unsafe import email delivery")
	}
	var n int
	if err := e.pool.QueryRow(t.Context(), "SELECT count(*) FROM people WHERE username='privileged'").Scan(&n); err != nil || n != 0 {
		t.Fatal("failed elevated email row left person")
	}
	person, err := e.stack.Identity.GetPersonByWorkEmail(t.Context(), "mailimport@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := e.stack.Identity.Invitations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.PersonID == person.ID {
			inv, err := store.New(e.pool).GetInvitation(t.Context(), row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if inv.Channel != "settings_email" || inv.GrantPermissions == nil {
				t.Fatalf("ceiling/channel not captured: %+v", inv)
			}
		}
	}
	exported := owner.req(http.MethodGet, "/api/v1/people-imports/"+run.ID.String()+"/errors", nil)
	expectStatus(t, exported, http.StatusOK)
	for _, forbidden := range []string{"oli_", "home.test", "Privileged", "mailimport@"} {
		if bytes.Contains(exported.body, []byte(forbidden)) {
			t.Fatal("error export leaked source data/capability")
		}
	}
}

func TestImportedPromotionPreservesExistingInvitationCeiling(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	issued := owner.invite("Pending Promotion", "promotion@home.test", "promotion")
	run := stageImport(t, owner, "work_email,role\npromotion@acme.test,owner\n")
	run = previewImport(t, owner, run, importMapping("work_email", "role"))
	if run.Rows[0].Decision != "update" {
		t.Fatalf("preview=%+v", run.Rows)
	}
	run = applyImport(t, owner, run)
	if run.Rows[0].Result != "updated" {
		t.Fatalf("apply=%+v", run.Rows)
	}
	expectStatus(t, e.browser().req(http.MethodPost, "/api/v1/auth/invitation", map[string]string{"token": tokenOf(t, *issued.Link)}), http.StatusNotFound)
	// A fresh owner-authorized invitation is required for the elevated grant.
	fresh := owner.req(http.MethodPost, "/api/v1/invitations/"+issued.Invitation.ID.String()+"/resend", nil)
	expectStatus(t, fresh, http.StatusOK)
	var resent issuedJSON
	fresh.json(t, &resent)
	if resent.Link == nil || *resent.Link == *issued.Link {
		t.Fatal("promotion reused old invitation capability")
	}
}

func TestPeopleImportMappingRequestBoundary(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	run := stageImport(t, owner, "username,work_email,display_name\nnewperson,newperson@acme.test,New Person\n")
	path := "/api/v1/people-imports/" + run.ID.String() + "/preview"
	valid := `{"columns":[{"column":0,"target":"username"},{"column":1,"target":"work_email"},{"column":2,"target":"display_name"}],"createTeams":false,"sendInvitations":false,"skipRows":[]}`
	cases := map[string]string{
		"null":           "null",
		"unknown fields": strings.TrimSuffix(valid, "}") + `,"roles":["owner"]}`,
		"trailing JSON":  valid + `{}`,
		"oversized":      `{"columns":[{"column":0,"target":"` + strings.Repeat("private-marker", 6000) + `"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			response := owner.req(http.MethodPost, path, strings.NewReader(body), "Content-Type", "application/json")
			expectStatus(t, response, http.StatusBadRequest)
			if bytes.Contains(response.body, []byte("private-marker")) {
				t.Fatal("mapping diagnostic exposed input")
			}
		})
	}
	// The bounded strict decoder must preserve the valid public request.
	preview := owner.req(http.MethodPost, path, strings.NewReader(valid), "Content-Type", "application/json")
	expectStatus(t, preview, http.StatusOK)
}
