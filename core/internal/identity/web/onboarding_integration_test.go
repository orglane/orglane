//go:build integration

package web_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/tests/auditfault"
	"github.com/orglane/orglane/tests/webauthntest"
)

func onboarding(t *testing.T, b *browser, path string) apigen.OnboardingProgress {
	t.Helper()
	r := b.req("GET", path, nil)
	expectStatus(t, r, http.StatusOK)
	var p apigen.OnboardingProgress
	r.json(t, &p)
	return p
}
func onboardingStep(t *testing.T, p apigen.OnboardingProgress, id string) apigen.OnboardingStep {
	t.Helper()
	for _, step := range p.Steps {
		if step.Id == id {
			return step
		}
	}
	t.Fatalf("missing step %s in %+v", id, p)
	return apigen.OnboardingStep{}
}

func TestInvitedOnboardingCompletionAndPermissions(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	adminPerson := e.createMember("admin")
	if err := e.stack.Identity.AssignRole(t.Context(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.signedIn(adminPerson.WorkEmail)
	for _, field := range []map[string]any{
		{"key": "office", "label": "Office", "type": "text", "requiredAtOnboarding": true, "userCanHide": true},
		{"key": "badge", "label": "Badge", "type": "text", "requiredAtOnboarding": true, "editableBy": "admins"},
		{"key": "private_note", "label": "Private note", "type": "text", "requiredAtOnboarding": true, "sensitive": true},
	} {
		expectStatus(t, owner.req("POST", "/api/v1/profile-fields", field), http.StatusCreated)
	}
	issued := owner.invite("New Person", "new@example.test", "new")
	member := e.browser()
	token := tokenOf(t, *issued.Link)
	r := member.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": token, "password": ownerPassword})
	expectStatus(t, r, http.StatusOK)
	var signed signInResult
	r.json(t, &signed)
	member.csrf = signed.CsrfToken
	path := "/api/v1/people/" + issued.Invitation.PersonID.String() + "/onboarding"
	profilePath := "/api/v1/people/" + issued.Invitation.PersonID.String() + "/profile"
	expectStatus(t, e.browser().req("GET", "/api/v1/me/onboarding", nil), http.StatusUnauthorized)
	expectStatus(t, e.browser().req("POST", "/api/v1/me/onboarding/finish", nil), http.StatusUnauthorized)
	expectStatus(t, member.req("GET", path, nil), http.StatusForbidden)
	p := onboarding(t, member, "/api/v1/me/onboarding")
	if !p.WelcomePending || onboardingStep(t, p, "profile").Complete {
		t.Fatalf("initial welcome: %+v", p)
	}
	for _, step := range p.Steps {
		if step.Required && step.ActionUrl == nil {
			t.Fatalf("required step has no authorized action: %+v", step)
		}
		if step.Id == "mobile" {
			t.Fatal("mobile must remain hidden until M4")
		}
	}
	expectStatus(t, member.req("POST", "/api/v1/me/onboarding/finish", nil), http.StatusConflict)
	member.signIn(signed.Person.WorkEmail)
	if !onboarding(t, member, "/api/v1/me/onboarding").WelcomePending {
		t.Fatal("interrupted welcome did not survive sign-in")
	}
	expectStatus(t, member.req("POST", "/api/v1/me/passkey-offer/dismiss", nil), http.StatusNoContent)
	if onboardingStep(t, onboarding(t, member, "/api/v1/me/onboarding"), "passkey").Complete {
		t.Fatal("skip falsely completed passkey")
	}
	// Admin-only required fields and fields outside the person's sensitive scope cannot block welcome.
	expectStatus(t, member.req("PATCH", profilePath, map[string]any{"values": []map[string]any{{"key": "office", "value": "Lisbon", "hidden": true}}}), http.StatusOK)
	p = onboarding(t, member, "/api/v1/me/onboarding")
	if !onboardingStep(t, p, "profile").Complete || onboardingStep(t, p, "profile-admin").Required {
		t.Fatalf("unusable required step: %+v", p)
	}
	// Non-sensitive admins must observe no change when an unauthorized sensitive value changes.
	before := onboarding(t, admin, path)
	expectStatus(t, owner.req("PATCH", profilePath, map[string]any{"values": []map[string]any{{"key": "private_note", "value": "never-disclose-this"}}}), http.StatusOK)
	after := onboarding(t, admin, path)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("sensitive presence affected unauthorized progress: before=%+v after=%+v", before, after)
	}
	raw := admin.req("GET", path, nil)
	for _, forbidden := range []string{"private_note", "never-disclose-this", "Lisbon", "office", "badge"} {
		if bytes.Contains(raw.body, []byte(forbidden)) {
			t.Fatalf("progress leaked field metadata/value %q: %s", forbidden, raw.body)
		}
	}
	beforeAudit := e.auditCount(audit.PersonSensitiveFieldsRead)
	onboarding(t, owner, path)
	if e.auditCount(audit.PersonSensitiveFieldsRead) != beforeAudit+1 {
		t.Fatal("authorized sensitive progress read was not audited")
	}
	fault := auditfault.Inject(t, e.pool, "NEW.action = 'person.sensitive_fields_read'", 1)
	expectStatus(t, owner.req("GET", path, nil), http.StatusInternalServerError)
	if fault.Hits() != 1 {
		t.Fatal("sensitive progress did not attempt its required audit")
	}
	fault.Remove()
	fault = auditfault.Inject(t, e.pool, "NEW.action = 'person.onboarding_welcome_finished'", 1)
	expectStatus(t, member.req("POST", "/api/v1/me/onboarding/finish", nil), http.StatusInternalServerError)
	if !onboarding(t, member, "/api/v1/me/onboarding").WelcomePending {
		t.Fatal("failed audit committed welcome finish")
	}
	fault.Remove()
	for range 2 {
		expectStatus(t, member.req("POST", "/api/v1/me/onboarding/finish", nil), http.StatusNoContent)
	}
	if e.auditCount(audit.OnboardingWelcomeFinished) != 1 {
		t.Fatal("welcome finish must audit once")
	}
	p = onboarding(t, member, "/api/v1/me/onboarding")
	if p.WelcomePending || onboardingStep(t, p, "avatar").Complete || onboardingStep(t, p, "passkey").Complete {
		t.Fatalf("finish falsified optional completion: %+v", p)
	}
	expectStatus(t, member.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": token, "password": ownerPassword}), http.StatusConflict)
	member.signIn(signed.Person.WorkEmail)
	if onboarding(t, member, "/api/v1/me/onboarding").WelcomePending {
		t.Fatal("repeated sign-in restarted welcome")
	}
	// Real data changes immediately reopen checklist tasks, without reopening welcome.
	expectStatus(t, member.req("PATCH", profilePath, map[string]any{"values": []map[string]any{{"key": "office", "value": ""}}}), http.StatusOK)
	p = onboarding(t, member, "/api/v1/me/onboarding")
	if onboardingStep(t, p, "profile").Complete || p.WelcomePending {
		t.Fatal("profile clear did not update current checklist")
	}
	if err := e.stack.Identity.SetAvatar(t.Context(), audit.System, issued.Invitation.PersonID, identity.Avatar{ContentType: identity.AvatarSVG, Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><circle r="1"/></svg>`)}); err != nil {
		t.Fatal(err)
	}
	if !onboardingStep(t, onboarding(t, member, "/api/v1/me/onboarding"), "avatar").Complete {
		t.Fatal("avatar presence not reflected")
	}
	if err := e.stack.Identity.RemoveAvatar(t.Context(), audit.System, issued.Invitation.PersonID); err != nil {
		t.Fatal(err)
	}
	if onboardingStep(t, onboarding(t, member, "/api/v1/me/onboarding"), "avatar").Complete {
		t.Fatal("avatar removal not reflected")
	}
	// A real WebAuthn ceremony completes the optional passkey step, and removal reopens it.
	ceremony, options, err := e.stack.Identity.BeginPasskeyRegistration(t.Context(), issued.Invitation.PersonID)
	if err != nil {
		t.Fatal(err)
	}
	rawOptions, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	var creation struct {
		PublicKey map[string]any `json:"publicKey"`
	}
	if err := json.Unmarshal(rawOptions, &creation); err != nil {
		t.Fatal(err)
	}
	credential, err := webauthntest.New(origin).Create(creation.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	rawCredential, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := e.stack.Identity.FinishPasskeyRegistration(t.Context(), audit.System, issued.Invitation.PersonID, ceremony, "Laptop", rawCredential)
	if err != nil {
		t.Fatal(err)
	}
	if !onboardingStep(t, onboarding(t, member, "/api/v1/me/onboarding"), "passkey").Complete {
		t.Fatal("passkey registration not reflected")
	}
	expectStatus(t, member.req("DELETE", "/api/v1/me/passkeys/"+pk.ID.String(), nil), http.StatusNoContent)
	if onboardingStep(t, onboarding(t, member, "/api/v1/me/onboarding"), "passkey").Complete {
		t.Fatal("passkey removal not reflected")
	}
	expectStatus(t, admin.req("GET", "/api/v1/people/0190f3c4-0000-7000-8000-000000000001/onboarding", nil), http.StatusNotFound)
	if onboarding(t, owner, "/api/v1/me/onboarding").WelcomePending {
		t.Fatal("ordinary owner setup should not start invited welcome")
	}
}
