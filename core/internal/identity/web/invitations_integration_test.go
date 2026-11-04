//go:build integration

package web_test

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/tests/mailtest"
)

type invitationJSON struct {
	ID           uuid.UUID `json:"id"`
	PersonID     uuid.UUID `json:"personId"`
	DisplayName  string    `json:"displayName"`
	Username     string    `json:"username"`
	WorkEmail    string    `json:"workEmail"`
	ContactEmail string    `json:"contactEmail"`
	Roles        []string  `json:"roles"`
	Status       string    `json:"status"`
	Delivery     string    `json:"delivery"`
	Email        *struct {
		Status      string  `json:"status"`
		Attempts    int     `json:"attempts"`
		MaxAttempts int     `json:"maxAttempts"`
		LastError   *string `json:"lastError"`
	} `json:"email"`
}

type issuedJSON struct {
	Invitation invitationJSON `json:"invitation"`
	Link       *string        `json:"link"`
}

// invite invites a member through the API and returns the response.
func (b *browser) invite(name, contact, username string) issuedJSON {
	b.e.t.Helper()
	r := b.req("POST", "/api/v1/invitations", map[string]string{
		"displayName": name, "contactEmail": contact, "username": username, "role": "member",
	})
	expectStatus(b.e.t, r, http.StatusCreated)
	var out issuedJSON
	r.json(b.e.t, &out)
	return out
}

func (b *browser) invitations() []invitationJSON {
	b.e.t.Helper()
	r := b.req("GET", "/api/v1/invitations", nil)
	expectStatus(b.e.t, r, http.StatusOK)
	var out struct{ Items []invitationJSON }
	r.json(b.e.t, &out)
	return out.Items
}

// tokenOf returns the token in an invitation link.
func tokenOf(t *testing.T, link string) string {
	t.Helper()
	_, token, ok := strings.Cut(link, identity.InvitationPath+"#")
	if !ok || !strings.HasPrefix(token, identity.InvitationTokenPrefix) {
		t.Fatalf("invitation link %q has no token", link)
	}
	return token
}

// useMailpit saves Mailpit as the SMTP server through the API.
func (b *browser) useMailpit(mp *mailtest.Mailpit) {
	b.e.t.Helper()
	r := b.req("PUT", "/api/v1/settings/email", map[string]any{
		"host": mp.SMTPHost, "port": mp.SMTPPort, "tls": "none", "from": "Acme Workspace <no-reply@acme.test>",
	})
	expectStatus(b.e.t, r, http.StatusOK)
}

// The acceptance flow with real SMTP: the invite email arrives with a
// working link, the token is single use and accepting signs the person in.
func TestInvitationByEmail(t *testing.T) {
	mp := mailtest.Start(t)
	e := newEnv(t)
	e.startJobs()
	admin := e.browser()
	e.setupOwner(admin)

	// Not configured yet: invitations would be links.
	var settings struct {
		Source      string `json:"source"`
		Host        string `json:"host"`
		PasswordSet bool   `json:"passwordSet"`
	}
	r := admin.req("GET", "/api/v1/settings/email", nil)
	expectStatus(t, r, http.StatusOK)
	if r.json(t, &settings); settings.Source != "none" {
		t.Fatalf("settings source = %q, want none", settings.Source)
	}
	expectStatus(t, admin.req("POST", "/api/v1/settings/email/test", map[string]string{"to": "owner@example.test"}), http.StatusConflict)

	admin.useMailpit(mp)
	r = admin.req("GET", "/api/v1/settings/email", nil)
	if r.json(t, &settings); settings.Source != "settings" || settings.Host != mp.SMTPHost || settings.PasswordSet {
		t.Fatalf("settings = %+v", settings)
	}
	var options struct{ Delivery, WorkEmailPattern string }
	r = admin.req("GET", "/api/v1/invitations/options", nil)
	expectStatus(t, r, http.StatusOK)
	if r.json(t, &options); options.Delivery != "email" || options.WorkEmailPattern != "{username}@acme.test" {
		t.Fatalf("options = %+v", options)
	}

	// A test email goes out right away.
	expectStatus(t, admin.req("POST", "/api/v1/settings/email/test", map[string]string{"to": "owner@example.test"}), http.StatusNoContent)
	if msgs := mp.Wait(t, "owner@example.test", 1, 10*time.Second); !strings.Contains(msgs[0].Subject, "Test email") {
		t.Fatalf("test email subject = %q", msgs[0].Subject)
	}

	issued := admin.invite("Maya Okafor", "maya@example.test", "maya")
	if issued.Link != nil {
		t.Fatal("an emailed invitation must not return the link to the admin")
	}
	inv := issued.Invitation
	if inv.Delivery != "email" || inv.Status != "pending" || inv.WorkEmail != "maya@acme.test" || inv.ContactEmail != "maya@example.test" ||
		len(inv.Roles) != 1 || inv.Roles[0] != "member" {
		t.Fatalf("invitation = %+v", inv)
	}
	person, err := e.stack.Identity.GetPerson(context.Background(), inv.PersonID)
	if err != nil || person.Status != string(identity.StatusInvited) {
		t.Fatalf("invited person = %+v, %v", person, err)
	}

	msg := mp.Wait(t, "maya@example.test", 1, 15*time.Second)[0]
	if msg.Subject != "Olivia Owner invited you to work.acme.test" || msg.From.Address != "no-reply@acme.test" {
		t.Fatalf("invitation email: subject %q from %q", msg.Subject, msg.From.Address)
	}
	link := mailtest.Link(msg.Text, origin+identity.InvitationPath+"#")
	if link == "" || !strings.Contains(msg.HTML, link) || !strings.Contains(msg.HTML, `lang="en"`) {
		t.Fatalf("the email has no invitation link (text %q)", msg.Text)
	}
	for _, forbidden := range []string{"<img", "<script", "url("} {
		if strings.Contains(msg.HTML, forbidden) {
			t.Fatalf("the HTML email contains %q: no remote content or tracking", forbidden)
		}
	}
	token := tokenOf(t, link)

	// Delivered: the queued content is gone and the token was never stored.
	items := admin.invitations()
	if len(items) != 1 || items[0].Email == nil || items[0].Email.Status != "sent" || items[0].Email.Attempts != 1 {
		t.Fatalf("invitations = %+v", items)
	}
	var sealed int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM email_outbox WHERE sealed_message IS NOT NULL").Scan(&sealed); err != nil || sealed != 0 {
		t.Fatalf("queued content left after delivery: %d, %v", sealed, err)
	}
	var leaks int
	if err := e.pool.QueryRow(context.Background(),
		"SELECT (SELECT count(*) FROM invitations WHERE position($1::bytea IN token_hash) > 0) + (SELECT count(*) FROM instance_settings WHERE value::text LIKE '%' || $2 || '%')",
		[]byte(token), token).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("the token is stored in the clear: %d, %v", leaks, err)
	}

	// The invited person cannot sign in before accepting.
	maya := e.browser()
	expectStatus(t, maya.req("POST", "/api/v1/auth/sign-in", map[string]string{"email": "maya@acme.test", "password": ownerPassword}), http.StatusUnauthorized)

	var preview struct{ DisplayName, Username, WorkEmail string }
	r = maya.req("POST", "/api/v1/auth/invitation", map[string]string{"token": token})
	expectStatus(t, r, http.StatusOK)
	if r.json(t, &preview); preview.DisplayName != "Maya Okafor" || preview.WorkEmail != "maya@acme.test" || preview.Username != "maya" {
		t.Fatalf("preview = %+v", preview)
	}
	expectStatus(t, maya.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": token, "password": "short"}), http.StatusBadRequest)

	r = maya.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": token, "password": ownerPassword})
	expectStatus(t, r, http.StatusOK)
	var res signInResult
	r.json(t, &res)
	if res.Person.WorkEmail != "maya@acme.test" || res.CsrfToken == "" || !res.PasskeyOffer {
		t.Fatalf("accept result = %+v", res)
	}
	if c := cookieNamed(r.Response, web.SecureSessionCookie); c == nil || !strings.HasPrefix(c.Value, identity.SessionTokenPrefix) {
		t.Fatalf("no session cookie after accepting: %+v", c)
	}
	maya.csrf = res.CsrfToken
	var me struct {
		Person struct{ WorkEmail string } `json:"person"`
	}
	r = maya.req("GET", "/api/v1/me", nil)
	expectStatus(t, r, http.StatusOK)
	if r.json(t, &me); me.Person.WorkEmail != "maya@acme.test" {
		t.Fatalf("me = %+v", me)
	}
	if person, _ = e.stack.Identity.GetPerson(context.Background(), inv.PersonID); person.Status != string(identity.StatusActive) {
		t.Fatalf("status after accepting = %s, want active", person.Status)
	}

	// Single use.
	other := e.browser()
	expectStatus(t, other.req("POST", "/api/v1/auth/invitation", map[string]string{"token": token}), http.StatusConflict)
	expectStatus(t, other.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": token, "password": ownerPassword}), http.StatusConflict)
	if items := admin.invitations(); len(items) != 0 {
		t.Fatalf("an accepted invitation is still listed: %+v", items)
	}
	// The password chosen on acceptance works.
	other.signIn("maya@acme.test")

	for action, want := range map[audit.Action]int{
		audit.InvitationCreated: 1, audit.InvitationAccepted: 1, audit.EmailSettingsUpdated: 1, audit.EmailSettingsTested: 1,
	} {
		if got := e.auditCount(action); got != want {
			t.Errorf("%s entries = %d, want %d", action, got, want)
		}
	}
}

// Without system email, the admin gets the link to share; everything else
// works the same.
func TestInvitationLinkMode(t *testing.T) {
	e := newEnv(t)
	admin := e.browser()
	e.setupOwner(admin)

	var options struct{ Delivery string }
	admin.req("GET", "/api/v1/invitations/options", nil).json(t, &options)
	if options.Delivery != "link" {
		t.Fatalf("delivery = %q, want link", options.Delivery)
	}
	r := admin.req("POST", "/api/v1/invitations", map[string]string{
		"displayName": "Sam Lee", "contactEmail": "sam@example.test", "username": "Sam.Lee", "workEmail": "samuel@acme.test", "role": "admin",
	})
	expectStatus(t, r, http.StatusCreated)
	var issued issuedJSON
	r.json(t, &issued)
	if issued.Link == nil || issued.Invitation.Delivery != "link" || issued.Invitation.Email != nil ||
		issued.Invitation.Username != "sam.lee" || issued.Invitation.WorkEmail != "samuel@acme.test" {
		t.Fatalf("issued = %+v", issued)
	}
	if !strings.HasPrefix(*issued.Link, origin+"/invite#oli_") {
		t.Fatalf("link = %q", *issued.Link)
	}
	// Taken usernames and work emails conflict.
	expectStatus(t, admin.req("POST", "/api/v1/invitations", map[string]string{
		"displayName": "Sam Again", "contactEmail": "sam2@example.test", "username": "sam.lee", "role": "member",
	}), http.StatusConflict)
	expectStatus(t, admin.req("POST", "/api/v1/invitations", map[string]string{
		"displayName": "No Contact", "contactEmail": "not an address", "username": "nocontact", "role": "member",
	}), http.StatusBadRequest)
	expectStatus(t, admin.req("POST", "/api/v1/invitations", map[string]string{
		"displayName": "No Role", "contactEmail": "x@example.test", "username": "norole", "role": "superuser",
	}), http.StatusBadRequest)

	sam := e.browser()
	r = sam.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": tokenOf(t, *issued.Link), "password": ownerPassword})
	expectStatus(t, r, http.StatusOK)
	var res signInResult
	r.json(t, &res)
	sam.csrf = res.CsrfToken
	// The role came with the invitation.
	expectStatus(t, sam.req("GET", "/api/v1/invitations", nil), http.StatusOK)
}

func TestInvitationConcurrentAccept(t *testing.T) {
	e := newEnv(t)
	admin := e.browser()
	e.setupOwner(admin)
	issued := admin.invite("Maya Okafor", "maya@example.test", "maya")
	token := tokenOf(t, *issued.Link)
	browsers := []*browser{e.browser(), e.browser()}
	start := make(chan struct{})
	statuses := make([]int, len(browsers))
	var wg sync.WaitGroup
	for i, b := range browsers {
		wg.Go(func() {
			<-start
			statuses[i] = b.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": token, "password": ownerPassword}).StatusCode
		})
	}
	close(start)
	wg.Wait()
	winners := 0
	for _, status := range statuses {
		if status == http.StatusOK {
			winners++
		} else if status != http.StatusConflict {
			t.Errorf("accept status = %d, want 200 or 409", status)
		}
	}
	if winners != 1 || e.auditCount(audit.InvitationAccepted) != 1 {
		t.Fatalf("single-use violation: statuses %v", statuses)
	}
}

// An acceptance that commits while a request is between reading the
// invitation and reading the invited person must not make the request answer
// 404. The test holds an exclusive lock on people (blocking reads of it) and
// commits the acceptance once the request waits on that lock, so the
// interleaving that made TestInvitationConcurrentAccept flaky happens every
// time. The losing accept answers 409; a preview may still see the
// invitation as it was before the acceptance (200), but never as invalid.
func TestInvitationAcceptedDuringLookupAnswersConflict(t *testing.T) {
	for _, tc := range []struct {
		path string
		want []int
	}{
		{"/api/v1/auth/invitation", []int{http.StatusOK, http.StatusConflict}},
		{"/api/v1/auth/invitation/accept", []int{http.StatusConflict}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			e := newEnv(t)
			admin := e.browser()
			e.setupOwner(admin)
			issued := admin.invite("Maya Okafor", "maya@example.test", "maya")
			token := tokenOf(t, *issued.Link)
			ctx := t.Context()

			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			// The winner's committed changes, held back behind the lock.
			for _, stmt := range []struct {
				sql string
				arg []any
			}{
				{"LOCK TABLE people IN ACCESS EXCLUSIVE MODE", nil},
				{"UPDATE invitations SET accepted_at = now() WHERE id = $1", []any{issued.Invitation.ID}},
				{"UPDATE people SET status = 'active' WHERE id = $1", []any{issued.Invitation.PersonID}},
			} {
				if _, err := tx.Exec(ctx, stmt.sql, stmt.arg...); err != nil {
					t.Fatal(err)
				}
			}

			status := make(chan int, 1)
			b := e.browser()
			go func() {
				status <- b.req("POST", tc.path, map[string]string{"token": token, "password": ownerPassword}).StatusCode
			}()
			waiting := false
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				var n int
				if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)) AND wait_event_type = 'Lock'`, int(tx.Conn().PgConn().PID())).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n > 0 {
					waiting = true
					break
				}
				select {
				case s := <-status:
					t.Fatalf("lookup finished without reading the invited person: status %d", s)
				default:
				}
			}
			if !waiting {
				t.Fatal("the lookup never waited on the people lock")
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case s := <-status:
				if !slices.Contains(tc.want, s) {
					t.Errorf("status = %d, want one of %v for an invitation accepted during the request", s, tc.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the lookup stayed blocked after the acceptance committed")
			}
			if n := e.auditCount(audit.InvitationAccepted); n != 0 {
				t.Errorf("invitation.accepted entries = %d, want 0 (the request lost)", n)
			}
		})
	}
}

func TestInvitationInitialTeamsAreAtomic(t *testing.T) {
	e := newEnv(t)
	admin := e.browser()
	e.setupOwner(admin)
	ctx := t.Context()
	platform, err := e.stack.Identity.CreateTeam(ctx, audit.System, identity.NewTeam{Slug: "platform", Name: "Platform"})
	if err != nil {
		t.Fatal(err)
	}
	product, err := e.stack.Identity.CreateTeam(ctx, audit.System, identity.NewTeam{Slug: "product", Name: "Product"})
	if err != nil {
		t.Fatal(err)
	}
	invite := func(username string, ids []uuid.UUID) response {
		return admin.req("POST", "/api/v1/invitations", map[string]any{
			"displayName": "Maya Okafor", "contactEmail": "maya@example.test", "username": username, "role": "member", "teamIds": ids,
		})
	}
	// A valid team followed by a missing one cannot leave a partial person,
	// role, membership or invitation behind.
	r := invite("partial", []uuid.UUID{platform.ID, uuid.New()})
	expectStatus(t, r, http.StatusNotFound)
	var count int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM people WHERE username = 'partial'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || e.auditCount(audit.TeamMemberAdded) != 0 || e.auditCount(audit.InvitationCreated) != 0 {
		t.Fatal("invalid team left a partial invitation")
	}
	expectStatus(t, invite("duplicate", []uuid.UUID{platform.ID, platform.ID}), http.StatusBadRequest)
	r = invite("maya", []uuid.UUID{product.ID, platform.ID})
	expectStatus(t, r, http.StatusCreated)
	var issued issuedJSON
	r.json(t, &issued)
	teams, err := e.stack.Identity.PersonTeams(ctx, issued.Invitation.PersonID)
	if err != nil || len(teams) != 2 {
		t.Fatalf("initial teams = %+v, %v", teams, err)
	}
	for _, team := range teams {
		if team.Role != string(identity.TeamRoleMember) {
			t.Fatalf("initial team role = %s", team.Role)
		}
	}
	if e.auditCount(audit.TeamMemberAdded) != 2 {
		t.Fatal("initial memberships were not audited")
	}
	// A stale selection cannot add someone to an archived team.
	if _, err := e.stack.Identity.ArchiveTeam(ctx, audit.System, product.ID); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, invite("archived", []uuid.UUID{product.ID}), http.StatusConflict)
	b := e.browser()
	expectStatus(t, b.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": tokenOf(t, *issued.Link), "password": ownerPassword}), http.StatusOK)
	if members, err := e.stack.Identity.TeamMembers(ctx, platform.ID); err != nil || len(members) != 1 || members[0].Person.ID != issued.Invitation.PersonID {
		t.Fatalf("membership lost on acceptance: %+v, %v", members, err)
	}
}

func TestInvitationQueueFailureRollsBack(t *testing.T) {
	e := newEnv(t) // Deliberately no job client attached.
	admin := e.browser()
	e.setupOwner(admin)
	expectStatus(t, admin.req("PUT", "/api/v1/settings/email", map[string]any{
		"host": "smtp.example.test", "tls": "starttls", "from": "no-reply@acme.test",
	}), http.StatusOK)
	expectStatus(t, admin.req("POST", "/api/v1/invitations", map[string]string{
		"displayName": "Maya Okafor", "contactEmail": "maya@example.test", "username": "maya", "role": "member",
	}), http.StatusInternalServerError)
	var count int
	if err := e.pool.QueryRow(t.Context(), "SELECT count(*) FROM people WHERE username = 'maya'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 || len(admin.invitations()) != 0 || e.auditCount(audit.InvitationCreated) != 0 {
		t.Fatal("failed queue insertion committed part of an invitation")
	}
}

func TestInvitationRevokeResendExpiry(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Identity.InvitationLifetime = 2 * time.Hour })
	ctx := context.Background()
	admin := e.browser()
	e.setupOwner(admin)
	issued := admin.invite("Ravi Patel", "ravi@example.test", "ravi")
	id := issued.Invitation.ID
	first := tokenOf(t, *issued.Link)
	b := e.browser()
	preview := func(token string) int {
		return b.req("POST", "/api/v1/auth/invitation", map[string]string{"token": token}).StatusCode
	}
	if got := preview(first); got != http.StatusOK {
		t.Fatalf("preview = %d", got)
	}

	// Revoke: the link stops working at once; the person stays invited.
	expectStatus(t, admin.req("DELETE", "/api/v1/invitations/"+id.String(), nil), http.StatusNoContent)
	expectStatus(t, admin.req("DELETE", "/api/v1/invitations/"+id.String(), nil), http.StatusNoContent)
	if got := preview(first); got != http.StatusNotFound {
		t.Fatalf("preview of a revoked invitation = %d, want 404", got)
	}
	expectStatus(t, b.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": first, "password": ownerPassword}), http.StatusNotFound)
	if items := admin.invitations(); len(items) != 1 || items[0].Status != "revoked" {
		t.Fatalf("invitations after revoking = %+v", items)
	}

	// Resend: a new link; the old one stays dead.
	r := admin.req("POST", "/api/v1/invitations/"+id.String()+"/resend", nil)
	expectStatus(t, r, http.StatusOK)
	var resent issuedJSON
	r.json(t, &resent)
	second := tokenOf(t, *resent.Link)
	if second == first || resent.Invitation.Status != "pending" || resent.Invitation.ID != id {
		t.Fatalf("resent = %+v", resent)
	}
	if preview(first) != http.StatusNotFound || preview(second) != http.StatusOK {
		t.Fatal("after resending, only the new link may work")
	}

	// Expiry.
	e.exec("UPDATE invitations SET issued_at = now() - interval '3 hours', expires_at = now() - interval '1 hour' WHERE id = $1", id)
	if got := preview(second); got != http.StatusGone {
		t.Fatalf("preview of an expired invitation = %d, want 410", got)
	}
	expectStatus(t, b.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": second, "password": ownerPassword}), http.StatusGone)
	if items := admin.invitations(); len(items) != 1 || items[0].Status != "expired" {
		t.Fatalf("invitations after expiry = %+v", items)
	}
	if err := e.stack.Maintain(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if n, err := e.stack.Identity.RecordExpiredInvitations(ctx); n != 0 || err != nil {
		t.Fatalf("expiry recorded again: %d, %v", n, err)
	}
	// Resending an expired invitation revives it.
	r = admin.req("POST", "/api/v1/invitations/"+id.String()+"/resend", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &resent)
	third := tokenOf(t, *resent.Link)
	expectStatus(t, b.req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": third, "password": ownerPassword}), http.StatusOK)
	// Accepted invitations can be neither resent nor revoked.
	expectStatus(t, admin.req("POST", "/api/v1/invitations/"+id.String()+"/resend", nil), http.StatusConflict)
	expectStatus(t, admin.req("DELETE", "/api/v1/invitations/"+id.String(), nil), http.StatusConflict)
	expectStatus(t, admin.req("DELETE", "/api/v1/invitations/"+uuid.New().String(), nil), http.StatusNotFound)

	for action, want := range map[audit.Action]int{
		audit.InvitationCreated: 1, audit.InvitationRevoked: 1, audit.InvitationResent: 2, audit.InvitationExpired: 1, audit.InvitationAccepted: 1,
	} {
		if got := e.auditCount(action); got != want {
			t.Errorf("%s entries = %d, want %d", action, got, want)
		}
	}
	// Malformed tokens are rejected the same way as unknown ones.
	for _, token := range []string{"", "oli_", "ost_" + strings.TrimPrefix(third, "oli_"), third + "x"} {
		if got := preview(token); got != http.StatusNotFound {
			t.Errorf("preview(%q) = %d, want 404", token, got)
		}
	}
}

// Reissuing an expired token before maintenance must preserve the expiry
// event; revoking it before maintenance must record both lifecycle events.
func TestInvitationMutationRecordsPriorExpiry(t *testing.T) {
	e := newEnv(t)
	admin := e.browser()
	e.setupOwner(admin)
	for i, mutation := range []string{"resend", "revoke"} {
		issued := admin.invite("Expired Invite", mutation+"@example.test", mutation)
		id := issued.Invitation.ID
		e.exec("UPDATE invitations SET issued_at = now() - interval '3 hours', expires_at = now() - interval '1 hour' WHERE id = $1", id)
		path := "/api/v1/invitations/" + id.String()
		if mutation == "resend" {
			expectStatus(t, admin.req("POST", path+"/resend", nil), http.StatusOK)
		} else {
			expectStatus(t, admin.req("DELETE", path, nil), http.StatusNoContent)
		}
		if got := e.auditCount(audit.InvitationExpired); got != i+1 {
			t.Fatalf("%s erased an expiry event: got %d, want %d", mutation, got, i+1)
		}
	}
	if n, err := e.stack.Identity.RecordExpiredInvitations(t.Context()); err != nil || n != 0 || e.auditCount(audit.InvitationExpired) != 2 {
		t.Fatalf("maintenance duplicated prior expiry events: n=%d, err=%v", n, err)
	}
}

// An unreachable SMTP server: the email is retried, every failure shows up
// with its error, and it is given up after the last attempt. Fixing the
// settings and resending delivers it.
func TestInvitationEmailRetries(t *testing.T) {
	mp := mailtest.Start(t)
	e := newEnv(t, func(c *config.Config) {
		c.Email.MaxAttempts = 2
		c.Email.RetryDelay = time.Second
	})
	e.startJobs()
	admin := e.browser()
	e.setupOwner(admin)

	// A port nobody listens on.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	expectStatus(t, admin.req("PUT", "/api/v1/settings/email", map[string]any{
		"host": "127.0.0.1", "port": closed, "tls": "none", "from": "no-reply@acme.test",
	}), http.StatusOK)
	r := admin.req("POST", "/api/v1/settings/email/test", map[string]string{"to": "owner@example.test"})
	expectStatus(t, r, http.StatusBadGateway)
	if !strings.Contains(string(r.body), strconv.Itoa(closed)) {
		t.Fatalf("the test failure does not name the server: %s", r.body)
	}

	id := admin.invite("Lena Fischer", "lena@example.test", "lena").Invitation.ID
	waitEmail := func(want string) invitationJSON {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for {
			items := admin.invitations()
			if len(items) == 1 && items[0].Email != nil && items[0].Email.Status == want {
				return items[0]
			}
			if time.Now().After(deadline) {
				t.Fatalf("email status never became %q: %+v", want, items)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	retrying := waitEmail("retrying")
	if retrying.Email.Attempts != 1 || retrying.Email.LastError == nil || !strings.Contains(*retrying.Email.LastError, "connect") {
		t.Fatalf("retrying = %+v", retrying.Email)
	}
	failed := waitEmail("failed")
	if failed.Email.Attempts != 2 || failed.Email.MaxAttempts != 2 || failed.Email.LastError == nil {
		t.Fatalf("failed = %+v", failed.Email)
	}
	var sealed int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM email_outbox WHERE sealed_message IS NOT NULL").Scan(&sealed); err != nil || sealed != 0 {
		t.Fatalf("a failed email keeps its content: %d, %v", sealed, err)
	}

	admin.useMailpit(mp)
	expectStatus(t, admin.req("POST", "/api/v1/invitations/"+id.String()+"/resend", nil), http.StatusOK)
	mp.Wait(t, "lena@example.test", 1, 15*time.Second)
	if sent := waitEmail("sent"); sent.Email.LastError != nil {
		t.Fatalf("sent = %+v", sent.Email)
	}
}

func TestInvitationPermissions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.browser()
	e.setupOwner(owner)
	id := owner.invite("Pending Person", "pending@example.test", "pending").Invitation.ID

	member := e.createMember("member")
	m := e.browser()
	m.signIn(member.WorkEmail)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/invitations"},
		{"POST", "/api/v1/invitations"},
		{"GET", "/api/v1/invitations/options"},
		{"POST", "/api/v1/invitations/" + id.String() + "/resend"},
		{"DELETE", "/api/v1/invitations/" + id.String()},
		{"GET", "/api/v1/settings/email"},
		{"PUT", "/api/v1/settings/email"},
		{"DELETE", "/api/v1/settings/email"},
		{"POST", "/api/v1/settings/email/test"},
	} {
		expectStatus(t, m.req(c.method, c.path, map[string]string{}), http.StatusForbidden)
		expectStatus(t, e.browser().req(c.method, c.path, map[string]string{}), http.StatusUnauthorized)
	}

	// An admin manages people but cannot hand out the owner role.
	adminPerson := e.createMember("admin")
	if err := e.stack.Identity.AssignRole(ctx, audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	a := e.browser()
	a.signIn(adminPerson.WorkEmail)
	expectStatus(t, a.req("POST", "/api/v1/invitations", map[string]string{
		"displayName": "Would Be Owner", "contactEmail": "owner2@example.test", "username": "owner2", "role": "owner",
	}), http.StatusForbidden)
	a.invite("New Member", "new@example.test", "newmember")
	expectStatus(t, a.req("GET", "/api/v1/settings/email", nil), http.StatusOK)
	// The owner can.
	coowner := owner.req("POST", "/api/v1/invitations", map[string]string{
		"displayName": "Co Owner", "contactEmail": "co@example.test", "username": "coowner", "role": "owner",
	})
	expectStatus(t, coowner, http.StatusCreated)
	var issued issuedJSON
	coowner.json(t, &issued)
	path := "/api/v1/invitations/" + issued.Invitation.ID.String()
	expectStatus(t, a.req("POST", path+"/resend", nil), http.StatusForbidden)
	expectStatus(t, a.req("DELETE", path, nil), http.StatusForbidden)
	expectStatus(t, owner.req("POST", path+"/resend", nil), http.StatusOK)
}

// Email settings from the configuration win and are read-only; saved
// passwords are encrypted at rest and never returned.
func TestEmailSettingsStorage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	admin := e.browser()
	e.setupOwner(admin)

	expectStatus(t, admin.req("PUT", "/api/v1/settings/email", map[string]any{
		"host": "smtp.example.test", "tls": "starttls", "username": "mailer", "from": "no-reply@acme.test",
	}), http.StatusBadRequest) // a username needs a password
	expectStatus(t, admin.req("PUT", "/api/v1/settings/email", map[string]any{
		"host": "smtp.example.test", "tls": "starttls", "from": "Bad\r\nBcc: x@example.test <a@b.test>",
	}), http.StatusBadRequest)
	expectStatus(t, admin.req("PUT", "/api/v1/settings/email", map[string]any{
		"host": "smtp.example.test", "tls": "starttls", "username": "mailer", "password": strings.Repeat("test-password", 2), "from": "no-reply@acme.test",
	}), http.StatusOK)
	var raw string
	if err := e.pool.QueryRow(ctx, "SELECT value::text FROM instance_settings WHERE key = 'email'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "test-password") {
		t.Fatal("the SMTP password is stored in the clear")
	}
	var got struct {
		Port        int    `json:"port"`
		PasswordSet bool   `json:"passwordSet"`
		Password    string `json:"password"`
	}
	r := admin.req("GET", "/api/v1/settings/email", nil)
	if r.json(t, &got); got.Port != 587 || !got.PasswordSet || got.Password != "" || strings.Contains(string(r.body), "test-password") {
		t.Fatalf("settings = %s", r.body)
	}
	// Saving without a password keeps it.
	expectStatus(t, admin.req("PUT", "/api/v1/settings/email", map[string]any{
		"host": "smtp2.example.test", "tls": "tls", "username": "mailer", "from": "no-reply@acme.test",
	}), http.StatusOK)
	settings, err := e.stack.Mail.Settings(ctx)
	if err != nil || settings.SMTP.Password != strings.Repeat("test-password", 2) || settings.SMTP.Port != 465 {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
	expectStatus(t, admin.req("DELETE", "/api/v1/settings/email", nil), http.StatusNoContent)
	if e.auditCount(audit.EmailSettingsUpdated) != 2 || e.auditCount(audit.EmailSettingsRemoved) != 1 {
		t.Fatal("settings changes are not audited")
	}

	cfgEnv := newEnv(t, func(c *config.Config) {
		c.Email.SMTP = config.SMTP{Host: "mail.internal.test", Port: 25, TLS: config.SMTPNoTLS}
		c.Email.From = "no-reply@acme.test"
	})
	b := cfgEnv.browser()
	cfgEnv.setupOwner(b)
	var src struct{ Source string }
	b.req("GET", "/api/v1/settings/email", nil).json(t, &src)
	if src.Source != "config" {
		t.Fatalf("source = %q, want config", src.Source)
	}
	expectStatus(t, b.req("PUT", "/api/v1/settings/email", map[string]any{"host": "x.test", "tls": "none", "from": "a@b.test"}), http.StatusConflict)
	expectStatus(t, b.req("DELETE", "/api/v1/settings/email", nil), http.StatusConflict)
}
