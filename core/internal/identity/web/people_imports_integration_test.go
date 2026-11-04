//go:build integration

package web_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
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
		{http.MethodDelete, "/api/v1/people-imports/" + run.ID.String(), nil},
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

func TestImportedInvitationStartsWelcomeWithImportedProfile(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	expectStatus(t, owner.req(http.MethodPost, "/api/v1/profile-fields", map[string]any{
		"key": "office", "label": "Office", "type": "text", "requiredAtOnboarding": true,
	}), http.StatusCreated)
	run := stageImport(t, owner, "username,work_email,display_name,office\njoiner,joiner@acme.test,Imported Joiner,Lisbon\n")
	mapping := importMapping("username", "work_email", "display_name", "profile.office")
	run = applyImport(t, owner, previewImport(t, owner, run, mapping))
	if run.Rows[0].Result != "created" {
		t.Fatalf("apply=%+v", run.Rows)
	}
	person, err := e.stack.Identity.GetPersonByWorkEmail(t.Context(), "joiner@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := e.stack.Identity.WelcomePending(t.Context(), person.ID)
	if err != nil || pending {
		t.Fatalf("unaccepted import welcome pending=%v err=%v", pending, err)
	}
	invitations, err := e.stack.Identity.Invitations(t.Context())
	if err != nil || len(invitations) != 1 {
		t.Fatalf("invitations=%v err=%v", len(invitations), err)
	}
	r := owner.req(http.MethodPost, "/api/v1/invitations/"+invitations[0].ID.String()+"/resend", nil)
	expectStatus(t, r, http.StatusOK)
	var issued issuedJSON
	r.json(t, &issued)
	if issued.Link == nil {
		t.Fatal("manual import invitation has no link")
	}
	member := e.browser()
	r = member.req(http.MethodPost, "/api/v1/auth/invitation/accept", map[string]string{
		"token": tokenOf(t, *issued.Link), "password": ownerPassword,
	})
	expectStatus(t, r, http.StatusOK)
	var signed signInResult
	r.json(t, &signed)
	member.csrf = signed.CsrfToken
	progress := onboarding(t, member, "/api/v1/me/onboarding")
	if !progress.WelcomePending || !onboardingStep(t, progress, "profile").Complete {
		t.Fatalf("imported required profile not reflected in welcome: %+v", progress)
	}
	expectStatus(t, member.req(http.MethodPost, "/api/v1/me/onboarding/finish", nil), http.StatusNoContent)
	// Reimport must preserve both completed welcome and the accepted invitation.
	run = stageImport(t, owner, "username,work_email,display_name,office\njoiner,joiner@acme.test,Imported Joiner,Lisbon\n")
	run = applyImport(t, owner, previewImport(t, owner, run, mapping))
	if run.Rows[0].Result != "skipped" || onboarding(t, member, "/api/v1/me/onboarding").WelcomePending {
		t.Fatalf("reimport restarted completed welcome: %+v", run.Rows)
	}
}

func TestPeopleImportMappingRequestBoundary(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	valid := `{"columns":[{"column":0,"target":"username"},{"column":1,"target":"work_email"},{"column":2,"target":"display_name"}],"createTeams":false,"sendInvitations":false,"skipRows":[]}`
	cases := map[string]string{
		"null":                "null",
		"unknown fields":      strings.TrimSuffix(valid, "}") + `,"roles":["owner"]}`,
		"trailing JSON":       valid + `{}`,
		"oversized":           `{"columns":[{"column":0,"target":"` + strings.Repeat("private-marker", 6000) + `"}]}`,
		"missing item column": strings.Replace(valid, `"column":0,`, "", 1),
		"null item column":    strings.Replace(valid, `"column":0`, `"column":null`, 1),
		"missing item target": strings.Replace(valid, `,"target":"username"`, "", 1),
		"null item target":    strings.Replace(valid, `"target":"username"`, `"target":null`, 1),
		"null column item":    strings.Replace(valid, `{"column":0,"target":"username"}`, "null", 1),
		"null skip row":       strings.Replace(valid, `"skipRows":[]`, `"skipRows":[null]`, 1),
	}
	for _, key := range []string{"columns", "createTeams", "sendInvitations", "skipRows"} {
		for _, missing := range []bool{true, false} {
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(valid), &object); err != nil {
				t.Fatal(err)
			}
			name := "null " + key
			object[key] = json.RawMessage("null")
			if missing {
				name = "missing " + key
				delete(object, key)
			}
			data, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			cases[name] = string(data)
		}
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			run := stageImport(t, owner, "username,work_email,display_name\nnewperson,newperson@acme.test,New Person\n")
			path := "/api/v1/people-imports/" + run.ID.String() + "/preview"
			before := importMutationState(t, e, run.ID)
			response := owner.req(http.MethodPost, path, strings.NewReader(body), "Content-Type", "application/json")
			if response.StatusCode != http.StatusBadRequest {
				t.Errorf("mapping response=%d, want400", response.StatusCode)
			}
			if !bytes.Equal(before, importMutationState(t, e, run.ID)) {
				t.Error("rejected mapping changed preview, people, profile, invitations or audit")
			}
			if bytes.Contains(response.body, []byte("private-marker")) {
				t.Fatal("mapping diagnostic exposed input")
			}
		})
	}
	// The bounded strict decoder must preserve the valid public request.
	run := stageImport(t, owner, "username,work_email,display_name\nnewperson,newperson@acme.test,New Person\n")
	path := "/api/v1/people-imports/" + run.ID.String() + "/preview"
	preview := owner.req(http.MethodPost, path, strings.NewReader(valid), "Content-Type", "application/json")
	expectStatus(t, preview, http.StatusOK)
	preview.json(t, &run)
	if run.State != "previewed" || len(run.Rows) != 1 || run.Rows[0].Decision != "create" || len(run.Mapping.Columns) != 3 || run.Mapping.Columns[0].Column != 0 || run.Mapping.CreateTeams || run.Mapping.SendInvitations || run.Mapping.SkipRows == nil || len(run.Mapping.SkipRows) != 0 {
		t.Fatal("explicit zero, false options and empty skip list were not preserved")
	}
}

// importMutationState compares private database state without logging its values.
func importMutationState(t *testing.T, e *env, id uuid.UUID) []byte {
	t.Helper()
	var state []byte
	err := e.pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
        'run', (SELECT to_jsonb(r) FROM people_import_runs r WHERE id=$1),
        'rows', (SELECT jsonb_agg(to_jsonb(r) ORDER BY row_number) FROM people_import_rows r WHERE run_id=$1),
        'people', (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM people p),
        'profile_values', (SELECT count(*) FROM profile_field_values),
        'invitations', (SELECT count(*) FROM invitations),
        'audit', (SELECT count(*) FROM audit_entries))`, id).Scan(&state)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPeopleImportApplyArchivedMappedFieldDoesNotMutate(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	existing := e.createMember("fieldimport")
	r := owner.req(http.MethodPost, "/api/v1/profile-fields", map[string]any{"key": "office", "label": "Office", "type": "text"})
	expectStatus(t, r, http.StatusCreated)
	var field apiField
	r.json(t, &field)
	run := stageImport(t, owner, "username,work_email,display_name,office\nfieldimport,"+existing.WorkEmail+",Changed Name,Lisbon\nnewperson,newperson@acme.test,New Person,Paris\n")
	run = previewImport(t, owner, run, importMapping("username", "work_email", "display_name", "profile.office"))
	if run.Rows[0].Decision != "update" || run.Rows[1].Decision != "create" {
		t.Fatalf("preview=%+v", run.Rows)
	}
	expectStatus(t, owner.req(http.MethodPatch, "/api/v1/profile-fields/"+field.ID, map[string]any{"archived": true}), http.StatusOK)
	before := importMutationState(t, e, run.ID)
	r = owner.req(http.MethodPost, "/api/v1/people-imports/"+run.ID.String()+"/apply", nil)
	expectStatus(t, r, http.StatusBadRequest)
	if !bytes.Equal(before, importMutationState(t, e, run.ID)) {
		t.Fatal("invalid apply changed preview, people, profile, invitations or audit")
	}
	for _, value := range []string{"Changed Name", "Lisbon", "Paris", "office"} {
		if bytes.Contains(r.body, []byte(value)) {
			t.Fatal("invalid apply diagnostic exposed mapped data")
		}
	}
}

func TestPeopleImportDeleteAndExpiry(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	memberPerson := e.createMember("importviewer")
	member := e.browser()
	member.signIn(memberPerson.WorkEmail)
	adminPerson := e.createMember("importadmin")
	if err := e.stack.Identity.AssignRole(t.Context(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.signIn(adminPerson.WorkEmail)
	text := "username,work_email,display_name,Private Heading\ndeleted,deleted@acme.test,Deleted Person,private\nbroken,broken@acme.test\n"
	mapping := importMapping("username", "work_email", "display_name")

	staged := stageImport(t, owner, text)
	if !staged.ExpiresAt.Equal(staged.CreatedAt.Add(peopleimport.StagingRetention)) {
		t.Fatalf("staged expiresAt = %v, createdAt = %v", staged.ExpiresAt, staged.CreatedAt)
	}
	applied := applyImport(t, owner, previewImport(t, owner, stageImport(t, owner, text), mapping))
	if !applied.ExpiresAt.After(time.Now().Add(peopleimport.AppliedRetention - time.Hour)) {
		t.Fatalf("applied expiresAt = %v, want 90 days after apply", applied.ExpiresAt)
	}
	// The value-free result and error export outlive the erased source.
	exported := owner.req(http.MethodGet, "/api/v1/people-imports/"+applied.ID.String()+"/errors", nil)
	expectStatus(t, exported, http.StatusOK)
	if string(exported.body) != "row,decision,error,result\n3,conflict,column_count,failed\n" {
		t.Fatalf("error export after apply = %q", exported.body)
	}
	repeated := applyImport(t, owner, applied)
	if repeated.Rows[0].Result != "created" || len(repeated.Headers) != 4 || repeated.Headers[3] != "" {
		t.Fatalf("repeated apply = %+v", repeated)
	}

	// Without people.manage the delete is denied and nothing changes.
	path := "/api/v1/people-imports/" + applied.ID.String()
	expectStatus(t, member.req(http.MethodDelete, path, nil), http.StatusForbidden)
	expectStatus(t, owner.req(http.MethodGet, path, nil), http.StatusOK)

	// Another people.manage holder may delete the owner's run; the uploader
	// may delete their own. Both are audited, without values.
	before := e.auditCount("import.deleted")
	expectStatus(t, admin.req(http.MethodDelete, path, nil), http.StatusNoContent)
	expectStatus(t, owner.req(http.MethodDelete, "/api/v1/people-imports/"+staged.ID.String(), nil), http.StatusNoContent)
	if e.auditCount("import.deleted") != before+2 {
		t.Fatal("deletes not audited")
	}
	var leaked bool
	if err := e.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_entries WHERE action='import.deleted' AND (metadata::text LIKE '%deleted@%' OR metadata::text LIKE '%Private%'))`).Scan(&leaked); err != nil || leaked {
		t.Fatalf("delete audit leaked source values: %v", err)
	}
	for _, id := range []string{applied.ID.String(), staged.ID.String()} {
		expectStatus(t, owner.req(http.MethodGet, "/api/v1/people-imports/"+id, nil), http.StatusNotFound)
		expectStatus(t, owner.req(http.MethodGet, "/api/v1/people-imports/"+id+"/errors", nil), http.StatusNotFound)
		expectStatus(t, owner.req(http.MethodPost, "/api/v1/people-imports/"+id+"/apply", nil), http.StatusNotFound)
		expectStatus(t, owner.req(http.MethodPost, "/api/v1/people-imports/"+id+"/preview", mapping), http.StatusNotFound)
		// Idempotent: deleting again succeeds and records nothing.
		expectStatus(t, owner.req(http.MethodDelete, "/api/v1/people-imports/"+id, nil), http.StatusNoContent)
	}
	expectStatus(t, owner.req(http.MethodDelete, "/api/v1/people-imports/"+uuid.NewString(), nil), http.StatusNoContent)
	if e.auditCount("import.deleted") != before+2 {
		t.Fatal("repeated delete wrote audit entries")
	}
	if _, err := e.stack.Identity.GetPersonByWorkEmail(t.Context(), "deleted@acme.test"); err != nil {
		t.Fatalf("deleting the run removed its imported person: %v", err)
	}

	// An expired run is not found, and the cleanup job removes it.
	expired := stageImport(t, owner, text)
	e.exec("UPDATE people_import_runs SET expires_at = now() - interval '1 second' WHERE id = $1", expired.ID)
	expectStatus(t, owner.req(http.MethodGet, "/api/v1/people-imports/"+expired.ID.String(), nil), http.StatusNotFound)
	if res, err := e.stack.Imports.Cleanup(t.Context()); err != nil || res.Expired != 1 {
		t.Fatalf("cleanup = %+v, %v", res, err)
	}
}
