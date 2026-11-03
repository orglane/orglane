//go:build integration

package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/jobs"
	"github.com/orglane/orglane/core/internal/mail"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/tests/mailtest"
)

// Attach the real River inserter without starting consumers, so a test can
// change grants/settings before making the queued delivery attempt.
func attachMailQueue(t *testing.T, e *env) {
	t.Helper()
	if _, err := jobs.NewClient(e.pool, e.cfg.Jobs, slog.New(slog.NewTextHandler(io.Discard, nil)), jobs.Options{Mail: e.stack.Mail}); err != nil {
		t.Fatal(err)
	}
}

func queuedCapability(t *testing.T, e *env, id uuid.UUID) (store.Invitation, string) {
	t.Helper()
	q := store.New(e.pool)
	inv, err := q.GetInvitation(t.Context(), id)
	if err != nil || inv.EmailID == nil {
		t.Fatal("missing queued invitation")
	}
	row, err := q.GetOutboxEmail(t.Context(), *inv.EmailID)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.NewKeyring(e.cfg.Identity.MasterSecret)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := keys.Sealer(crypto.PurposeEmailOutbox).Open(row.SealedMessage, row.ID[:])
	if err != nil {
		t.Fatal(err)
	}
	var message mail.Message
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	return inv, tokenOf(t, mailtest.Link(message.Text, origin+identity.InvitationPath+"#"))
}

func TestEditableSMTPUsesSafeChannelForCustomGrants(t *testing.T) {
	mp := mailtest.Start(t)
	e := newEnv(t)
	attachMailQueue(t, e)
	owner := e.browser()
	e.setupOwner(owner)
	adminPerson := e.createMember("smtpadmin")
	if err := e.stack.Identity.AssignRole(t.Context(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.signIn(adminPerson.WorkEmail)
	admin.useMailpit(mp) // Admin still has the requested SMTP settings access.
	var options struct{ RoleDeliveries map[string]string }
	owner.req("GET", "/api/v1/invitations/options", nil).json(t, &options)
	if options.RoleDeliveries["owner"] != "link" || options.RoleDeliveries["admin"] != "link" || options.RoleDeliveries["member"] != "email" {
		t.Fatal("role delivery choices do not describe the permission-safe channels")
	}
	// Custom bundles prove that no role name or built-in Admin bundle is
	// assumed to be the complete authority of every SMTP configurator.
	e.exec("INSERT INTO roles (key, name) VALUES ('sensitive_custom', 'Sensitive custom')")
	e.exec("INSERT INTO role_permissions (role_id, permission) SELECT id, $1 FROM roles WHERE key = 'sensitive_custom'", string(authz.PeopleSensitiveRead))
	for _, role := range []string{"owner", "admin", "sensitive_custom"} {
		r := owner.req("POST", "/api/v1/invitations", map[string]string{"displayName": "Privileged Invite", "contactEmail": role + "@example.test", "username": role + "_invite", "role": role})
		expectStatus(t, r, http.StatusCreated)
		var issued issuedJSON
		r.json(t, &issued)
		if issued.Link == nil || issued.Invitation.Delivery != "link" || issued.Invitation.Email != nil {
			t.Fatalf("%s grant exposed to editable SMTP", role)
		}
		// Privileged copy-links stay usable with editable SMTP configured.
		expectStatus(t, e.browser().req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": tokenOf(t, *issued.Link), "password": ownerPassword}), http.StatusOK)
	}
	member := owner.invite("Ordinary Member", "ordinary@example.test", "ordinary")
	if member.Link != nil || member.Invitation.Delivery != "email" {
		t.Fatal("ordinary email invitation regressed")
	}
	if messages := mp.Messages(t, "owner@example.test"); len(messages) != 0 {
		t.Fatal("privileged invitation reached editable SMTP")
	}
}

func TestQueuedLegacyAndPromotedCapabilitiesCannotReachSMTP(t *testing.T) {
	trusted, attacker := mailtest.Start(t), mailtest.Start(t)
	e := newEnv(t)
	attachMailQueue(t, e)
	owner := e.browser()
	e.setupOwner(owner)
	owner.useMailpit(trusted)
	adminPerson := e.createMember("smtpadmin")
	if err := e.stack.Identity.AssignRole(t.Context(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.signIn(adminPerson.WorkEmail)
	for _, legacy := range []bool{false, true} {
		username := "promoted"
		if legacy {
			username = "legacy"
		}
		owner.useMailpit(trusted)
		issued := owner.invite("Pending Invite", username+"@example.test", username)
		inv, token := queuedCapability(t, e, issued.Invitation.ID)
		if err := e.stack.Identity.AssignRole(t.Context(), audit.System, inv.PersonID, authz.RoleOwner); err != nil {
			t.Fatal(err)
		}
		if legacy {
			e.exec("UPDATE invitations SET channel = 'legacy', grant_permissions = NULL WHERE id = $1", inv.ID)
		}
		admin.useMailpit(attacker) // An attacker-controlled, reachable real relay.
		if err := e.stack.Mail.Deliver(t.Context(), *inv.EmailID, 1); err == nil {
			t.Fatal("unsafe queued grant was delivered")
		}
		if messages := attacker.Messages(t, username+"@example.test"); len(messages) != 0 {
			t.Fatal("relay received an elevated bearer token")
		}
		row, err := store.New(e.pool).GetOutboxEmail(t.Context(), *inv.EmailID)
		if err != nil || row.Status != "failed" || len(row.SealedMessage) != 0 || row.LastError == nil {
			t.Fatal("unsafe grant was not terminally cleared from outbox")
		}
		if strings.Contains(*row.LastError, token) {
			t.Fatal("capability leaked in failure diagnostic")
		}
		expectStatus(t, e.browser().req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": token, "password": ownerPassword}), http.StatusNotFound)
		r := owner.req("POST", "/api/v1/invitations/"+inv.ID.String()+"/resend", nil)
		expectStatus(t, r, http.StatusOK)
		var resent issuedJSON
		r.json(t, &resent)
		if resent.Link == nil || resent.Invitation.Delivery != "link" {
			t.Fatal("resend did not replace the unsafe email with a safe link")
		}
		expectStatus(t, e.browser().req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": tokenOf(t, *resent.Link), "password": ownerPassword}), http.StatusOK)
	}
}

func TestServerConfiguredSMTPTransitionDoesNotExposeQueuedGrant(t *testing.T) {
	trusted, attacker := mailtest.Start(t), mailtest.Start(t)
	e := newEnv(t, func(c *config.Config) {
		c.Email.SMTP = config.SMTP{Host: trusted.SMTPHost, Port: trusted.SMTPPort, TLS: config.SMTPNoTLS}
		c.Email.From = "no-reply@acme.test"
	})
	attachMailQueue(t, e)
	owner := e.browser()
	e.setupOwner(owner)
	r := owner.req("POST", "/api/v1/invitations", map[string]string{"displayName": "Owner Invite", "contactEmail": "owner-invite@example.test", "username": "owner_invite", "role": "owner"})
	expectStatus(t, r, http.StatusCreated)
	var issued issuedJSON
	r.json(t, &issued)
	if issued.Link != nil || issued.Invitation.Delivery != "email" {
		t.Fatal("immutable configured SMTP cannot deliver trusted grants")
	}
	inv, token := queuedCapability(t, e, issued.Invitation.ID)
	// The immutable channel still delivers an elevated grant normally.
	r = owner.req("POST", "/api/v1/invitations", map[string]string{"displayName": "Trusted Owner", "contactEmail": "trusted@example.test", "username": "trusted_owner", "role": "owner"})
	expectStatus(t, r, http.StatusCreated)
	var trustedIssue issuedJSON
	r.json(t, &trustedIssue)
	trustedInv, _ := queuedCapability(t, e, trustedIssue.Invitation.ID)
	if err := e.stack.Mail.Deliver(t.Context(), *trustedInv.EmailID, 1); err != nil {
		t.Fatal(err)
	}
	message := trusted.Wait(t, "trusted@example.test", 1, 10*time.Second)[0]
	expectStatus(t, e.browser().req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": tokenOf(t, mailtest.Link(message.Text, origin+identity.InvitationPath+"#")), "password": ownerPassword}), http.StatusOK)
	// A later process starts without the immutable SMTP override and resolves
	// saved settings. Delivery must validate the new effective source.
	cfg := e.cfg.Email
	cfg.SMTP = config.SMTP{}
	keys, err := crypto.NewKeyring(e.cfg.Identity.MasterSecret)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	later := mail.NewService(e.pool, cfg, workspaceHost, keys, slog.New(slog.NewTextHandler(&diagnostics, nil)))
	if err := later.PutSettings(t.Context(), audit.System, mail.SettingsInput{Host: attacker.SMTPHost, Port: attacker.SMTPPort, TLS: config.SMTPNoTLS, From: "no-reply@acme.test"}); err != nil {
		t.Fatal(err)
	}
	err = later.Deliver(t.Context(), *inv.EmailID, 1)
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(diagnostics.String(), token) {
		t.Fatal("unsafe grant or diagnostic escaped source-transition check")
	}
	if messages := attacker.Messages(t, "owner-invite@example.test"); len(messages) != 0 {
		t.Fatal("new editable relay received a server-configured elevated capability")
	}
}

func TestSettingsEmailCeilingSurvivesServerOverride(t *testing.T) {
	mp := mailtest.Start(t)
	e := newEnv(t)
	attachMailQueue(t, e)
	owner := e.browser()
	e.setupOwner(owner)
	owner.useMailpit(mp)
	issued := owner.invite("Initially Member", "ceiling@example.test", "ceiling")
	inv, token := queuedCapability(t, e, issued.Invitation.ID)
	if err := e.stack.Identity.AssignRole(t.Context(), audit.System, inv.PersonID, authz.RoleOwner); err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg.Email
	cfg.SMTP = config.SMTP{Host: mp.SMTPHost, Port: mp.SMTPPort, TLS: config.SMTPNoTLS}
	cfg.From = "no-reply@acme.test"
	keys, err := crypto.NewKeyring(e.cfg.Identity.MasterSecret)
	if err != nil {
		t.Fatal(err)
	}
	later := mail.NewService(e.pool, cfg, workspaceHost, keys, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := later.Deliver(t.Context(), *inv.EmailID, 1); err == nil {
		t.Fatal("server override enlarged a settings-email capability's original ceiling")
	}
	if messages := mp.Messages(t, "ceiling@example.test"); len(messages) != 0 {
		t.Fatal("promoted capability reached SMTP after a source change")
	}
	expectStatus(t, e.browser().req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": token, "password": ownerPassword}), http.StatusNotFound)
}

func TestLegacyCopyLinkPromotionRequiresReissue(t *testing.T) {
	mp := mailtest.Start(t)
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	issued := owner.invite("Legacy Copy", "legacy-copy@example.test", "legacy_copy")
	oldToken := tokenOf(t, *issued.Link)
	e.exec("UPDATE invitations SET channel = 'legacy', grant_permissions = NULL WHERE id = $1", issued.Invitation.ID)
	if err := e.stack.Identity.AssignRole(t.Context(), audit.System, issued.Invitation.PersonID, authz.RoleOwner); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, e.browser().req("POST", "/api/v1/auth/invitation", map[string]string{"token": oldToken}), http.StatusNotFound)
	expectStatus(t, e.browser().req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": oldToken, "password": ownerPassword}), http.StatusNotFound)
	owner.useMailpit(mp)
	r := owner.req("POST", "/api/v1/invitations/"+issued.Invitation.ID.String()+"/resend", nil)
	expectStatus(t, r, http.StatusOK)
	var resent issuedJSON
	r.json(t, &resent)
	if resent.Link == nil || resent.Invitation.Email != nil {
		t.Fatal("reissued privileged grant did not use an explicit copy-link")
	}
	expectStatus(t, e.browser().req("POST", "/api/v1/auth/invitation/accept", map[string]string{"token": tokenOf(t, *resent.Link), "password": ownerPassword}), http.StatusOK)
}

type diagnosticLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *diagnosticLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}
func (l *diagnosticLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buffer.String() }

// The hostile DATA echo is exercised at the unit wire boundary. This real
// Postgres/River failure path verifies that the SAME sanitized category,
// with credential-bearing local connection details removed, reaches every
// downstream sink rather than retaining an original error for River.
func TestEmailDiagnosticsAreSafeAtEverySink(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Email.MaxAttempts = 2; c.Email.RetryDelay = time.Second })
	attachMailQueue(t, e)
	owner := e.browser()
	e.setupOwner(owner)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	username, password := "127.0.0.1", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	expectStatus(t, owner.req("PUT", "/api/v1/settings/email", map[string]any{"host": username, "port": port, "tls": "none", "username": username, "password": password, "from": "no-reply@acme.test"}), http.StatusOK)
	issued := owner.invite("Diagnostic Member", "diagnostic@example.test", "diagnostic")
	inv, token := queuedCapability(t, e, issued.Invitation.ID)
	keys, err := crypto.NewKeyring(e.cfg.Identity.MasterSecret)
	if err != nil {
		t.Fatal(err)
	}
	var log diagnosticLog
	logger := slog.New(slog.NewTextHandler(&log, nil))
	service := mail.NewService(e.pool, e.cfg.Email, workspaceHost, keys, logger)
	workerError := service.Deliver(t.Context(), *inv.EmailID, 1)
	if workerError == nil {
		t.Fatal("closed SMTP port did not fail")
	}
	client, err := jobs.NewClient(e.pool, e.cfg.Jobs, logger, jobs.Options{Mail: service})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
	var jobErrors string
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := e.pool.QueryRow(t.Context(), "SELECT coalesce(errors::text, '') FROM river_job WHERE args->>'email_id' = $1", inv.EmailID.String()).Scan(&jobErrors)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(jobErrors, "SMTP connection") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("River did not persist the failed worker result")
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, err := store.New(e.pool).GetOutboxEmail(t.Context(), *inv.EmailID)
	if err != nil || row.LastError == nil {
		t.Fatal("missing persisted delivery diagnostic")
	}
	items := owner.invitations()
	if len(items) != 1 || items[0].Email == nil || items[0].Email.LastError == nil {
		t.Fatal("missing API-visible delivery diagnostic")
	}
	for sink, text := range map[string]string{"returned worker error": workerError.Error(), "outbox": *row.LastError, "API": *items[0].Email.LastError, "logs": log.String(), "River": jobErrors} {
		if !strings.Contains(text, "connection") {
			t.Fatalf("%s has no safe failure category", sink)
		}
		for _, secret := range []string{token, username, password} {
			if strings.Contains(text, secret) {
				t.Fatalf("%s exposed a capability or credential", sink)
			}
		}
	}
}
