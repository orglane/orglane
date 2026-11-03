//go:build integration

package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/tests/webauthntest"
)

func TestSetupTokenProtection(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.browser()

	var status struct{ Required bool }
	r := b.req("GET", "/api/v1/auth/setup", nil)
	expectStatus(t, r, http.StatusOK)
	if r.json(t, &status); !status.Required {
		t.Fatal("a fresh instance must require setup")
	}

	// The token is created once; restarts do not print it again.
	token, err := e.stack.Identity.EnsureSetupToken(ctx)
	if err != nil || !strings.HasPrefix(token, identity.SetupTokenPrefix) {
		t.Fatalf("EnsureSetupToken() = %q, %v", token, err)
	}
	if again, err := e.stack.Identity.EnsureSetupToken(ctx); again != "" || err != nil {
		t.Fatalf("second EnsureSetupToken() = %q, %v; want no new token", again, err)
	}
	var stored string
	if err := e.pool.QueryRow(ctx, "SELECT value::text FROM instance_settings WHERE key = 'setup_token'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, token) || strings.Contains(stored, strings.TrimPrefix(token, identity.SetupTokenPrefix)) {
		t.Fatal("the setup token is stored in the clear")
	}

	owner := map[string]string{"workEmail": ownerEmail, "username": "owner", "displayName": "Olivia Owner", "password": ownerPassword}
	with := func(token, password string) map[string]string {
		m := map[string]string{"setupToken": token, "password": password}
		for k, v := range owner {
			if k != "password" {
				m[k] = v
			}
		}
		return m
	}

	// Anyone on the network without the token cannot claim the instance.
	expectStatus(t, b.req("POST", "/api/v1/auth/setup", with("", ownerPassword)), http.StatusForbidden)
	expectStatus(t, b.req("POST", "/api/v1/auth/setup", with(identity.SetupTokenPrefix+"guess", ownerPassword)), http.StatusForbidden)
	expectStatus(t, b.req("POST", "/api/v1/auth/setup", with(token, "short")), http.StatusBadRequest)
	if _, err := e.stack.Identity.GetPersonByWorkEmail(ctx, ownerEmail); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("a rejected setup created the owner: %v", err)
	}

	r = b.req("POST", "/api/v1/auth/setup", with(token, ownerPassword))
	expectStatus(t, r, http.StatusCreated)
	var res signInResult
	r.json(t, &res)
	if res.Person.WorkEmail != ownerEmail || res.CsrfToken == "" || !res.PasskeyOffer {
		t.Fatalf("setup result = %+v", res)
	}
	c := cookieNamed(r.Response, web.SecureSessionCookie)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.Path != "/" ||
		!strings.HasPrefix(c.Value, identity.SessionTokenPrefix) {
		t.Fatalf("session cookie = %+v, want a host-only __Host- cookie (Secure, HttpOnly, SameSite=Lax)", c)
	}

	// The token is single use and setup closes for good.
	expectStatus(t, e.browser().req("POST", "/api/v1/auth/setup", with(token, ownerPassword)), http.StatusConflict)
	if _, err := e.stack.Identity.ResetSetupToken(ctx, audit.System); !errors.Is(err, identity.ErrSetupDone) {
		t.Fatalf("ResetSetupToken() after setup = %v, want ErrSetupDone", err)
	}
	r = b.req("GET", "/api/v1/auth/setup", nil)
	if r.json(t, &status); status.Required {
		t.Fatal("setup still required after it completed")
	}

	// The owner holds every permission.
	expectStatus(t, b.req("GET", "/api/v1/instance", nil), http.StatusOK)
	for _, action := range []audit.Action{audit.SetupTokenCreated, audit.InstanceSetupCompleted, audit.PersonPasswordChanged, audit.SignInSucceeded, audit.SessionCreated} {
		if e.auditCount(action) == 0 {
			t.Errorf("no %s audit entry", action)
		}
	}
	var hash string
	if err := e.pool.QueryRow(ctx, "SELECT password_hash FROM person_credentials").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") || strings.Contains(hash, ownerPassword) {
		t.Fatalf("stored password = %q, want an argon2id hash", hash)
	}
}

// TestSignUpSkipPasskeySignOutSignIn is the acceptance scenario: sign up
// with a password, skip the passkey offer, sign out, sign in again.
func TestSignUpSkipPasskeySignOutSignIn(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	res := e.setupOwner(b)
	if !res.PasskeyOffer {
		t.Fatal("a new account must be offered a passkey")
	}

	expectStatus(t, b.req("POST", "/api/v1/me/passkey-offer/dismiss", nil), http.StatusNoContent)
	var me struct {
		PasskeyOffer bool     `json:"passkeyOffer"`
		Permissions  []string `json:"permissions"`
		CsrfToken    string   `json:"csrfToken"`
		Session      struct {
			ID          uuid.UUID `json:"id"`
			AuthMethods []string  `json:"authMethods"`
			Current     bool      `json:"current"`
		} `json:"session"`
	}
	r := b.req("GET", "/api/v1/me", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &me)
	if me.PasskeyOffer || !me.Session.Current || me.CsrfToken != b.csrf || len(me.Permissions) == 0 {
		t.Fatalf("me after skipping the passkey = %+v", me)
	}

	expectStatus(t, b.req("POST", "/api/v1/auth/sign-out", nil), http.StatusNoContent)
	expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusUnauthorized)

	again := b.signIn(ownerEmail)
	if again.PasskeyOffer {
		t.Fatal("the skipped passkey offer came back at the next sign-in")
	}
	r = b.req("GET", "/api/v1/me", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &me)
	if me.Session.AuthMethods[0] != "password" {
		t.Errorf("auth methods = %v", me.Session.AuthMethods)
	}

	// The browser kept its device: one device, two sessions.
	var devices, sessions int
	if err := e.pool.QueryRow(context.Background(), "SELECT (SELECT count(*) FROM devices), (SELECT count(*) FROM sessions)").Scan(&devices, &sessions); err != nil {
		t.Fatal(err)
	}
	if devices != 1 || sessions != 2 {
		t.Errorf("devices = %d, sessions = %d; want the web device reused", devices, sessions)
	}
	if e.auditCount(audit.PasskeyOfferDismissed) != 1 {
		t.Error("dismissing the passkey offer was not audited")
	}

	// Wrong passwords are rejected without saying why.
	r = b.req("POST", "/api/v1/auth/sign-in", map[string]string{"email": ownerEmail, "password": "wrong password!"})
	expectStatus(t, r, http.StatusUnauthorized)
	r2 := b.req("POST", "/api/v1/auth/sign-in", map[string]string{"email": "nobody@acme.test", "password": "wrong password!"})
	expectStatus(t, r2, http.StatusUnauthorized)
	if strings.ReplaceAll(string(r.body), "/api/v1/auth/sign-in", "") != strings.ReplaceAll(string(r2.body), "/api/v1/auth/sign-in", "") {
		t.Errorf("unknown and known accounts are told apart:\n%s\n%s", r.body, r2.body)
	}
}

func TestPasskeyRegistrationAndSignIn(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	owner := e.setupOwner(b)
	authn := webauthntest.New(origin)

	var ceremony struct {
		CeremonyID uuid.UUID      `json:"ceremonyId"`
		PublicKey  map[string]any `json:"publicKey"`
	}
	r := b.req("POST", "/api/v1/me/passkeys/options", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &ceremony)
	if rp, _ := ceremony.PublicKey["rp"].(map[string]any); rp["id"] != workspaceHost {
		t.Fatalf("relying party = %v, want the workspace host %s (ADR 0018)", rp, workspaceHost)
	}
	credential, err := authn.Create(ceremony.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	r = b.req("POST", "/api/v1/me/passkeys", map[string]any{"ceremonyId": ceremony.CeremonyID, "name": "Laptop", "credential": credential})
	expectStatus(t, r, http.StatusCreated)
	var pk struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	}
	r.json(t, &pk)
	if pk.Name != "Laptop" {
		t.Fatalf("passkey = %+v", pk)
	}
	// A ceremony is single use.
	expectStatus(t, b.req("POST", "/api/v1/me/passkeys", map[string]any{"ceremonyId": ceremony.CeremonyID, "credential": credential}), http.StatusBadRequest)

	var list struct{ Items []struct{ ID uuid.UUID } }
	r = b.req("GET", "/api/v1/me/passkeys", nil)
	expectStatus(t, r, http.StatusOK)
	if r.json(t, &list); len(list.Items) != 1 || list.Items[0].ID != pk.ID {
		t.Fatalf("passkeys = %+v", list)
	}

	// Sign in with the passkey in a new browser, without a username.
	signInWithPasskey := func(c *browser) response {
		r := c.req("POST", "/api/v1/auth/passkey/options", nil)
		expectStatus(t, r, http.StatusOK)
		r.json(t, &ceremony)
		assertion, err := authn.Get(ceremony.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		return c.req("POST", "/api/v1/auth/passkey", map[string]any{"ceremonyId": ceremony.CeremonyID, "credential": assertion})
	}
	other := e.browser()
	r = signInWithPasskey(other)
	expectStatus(t, r, http.StatusOK)
	var res signInResult
	r.json(t, &res)
	if res.Person.ID != owner.Person.ID || res.PasskeyOffer {
		t.Fatalf("passkey sign-in = %+v", res)
	}
	other.csrf = res.CsrfToken
	var me struct {
		Session struct{ AuthMethods []string } `json:"session"`
	}
	r = other.req("GET", "/api/v1/me", nil)
	if r.json(t, &me); len(me.Session.AuthMethods) != 1 || me.Session.AuthMethods[0] != "passkey" {
		t.Fatalf("session auth methods = %v", me.Session.AuthMethods)
	}

	// A replayed assertion (same ceremony) fails.
	assertion, _ := authn.Get(ceremony.PublicKey)
	expectStatus(t, e.browser().req("POST", "/api/v1/auth/passkey", map[string]any{"ceremonyId": ceremony.CeremonyID, "credential": assertion}), http.StatusUnauthorized)

	if e.auditCount(audit.PasskeyAdded) != 1 {
		t.Error("adding the passkey was not audited")
	}

	// Removed passkeys no longer sign in.
	expectStatus(t, b.req("DELETE", "/api/v1/me/passkeys/"+pk.ID.String(), nil), http.StatusNoContent)
	expectStatus(t, b.req("DELETE", "/api/v1/me/passkeys/"+pk.ID.String(), nil), http.StatusNotFound)
	expectStatus(t, signInWithPasskey(e.browser()), http.StatusUnauthorized)
	if e.auditCount(audit.PasskeyRemoved) != 1 || e.auditCount(audit.SignInFailed) < 2 {
		t.Error("passkey removal or failed passkey sign-ins were not audited")
	}
}

func TestSessionRevocationIsImmediate(t *testing.T) {
	e := newEnv(t)
	a := e.browser()
	e.setupOwner(a)
	b := e.browser()
	b.signIn(ownerEmail)

	var list struct {
		Items []struct {
			ID      uuid.UUID `json:"id"`
			Current bool      `json:"current"`
			Device  struct{ Name string }
		}
	}
	r := a.req("GET", "/api/v1/me/sessions", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &list)
	if len(list.Items) != 2 {
		t.Fatalf("sessions = %+v", list)
	}
	var otherID uuid.UUID
	for _, s := range list.Items {
		if !s.Current {
			otherID = s.ID
		}
	}

	expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusOK)
	expectStatus(t, a.req("DELETE", "/api/v1/me/sessions/"+otherID.String(), nil), http.StatusNoContent)
	expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusUnauthorized)
	expectStatus(t, a.req("GET", "/api/v1/me", nil), http.StatusOK)

	// Sessions of other people are not found.
	member := e.createMember("mia")
	m := e.browser()
	m.signIn(member.WorkEmail)
	r = m.req("GET", "/api/v1/me/sessions", nil)
	r.json(t, &list)
	expectStatus(t, a.req("DELETE", "/api/v1/me/sessions/"+list.Items[0].ID.String(), nil), http.StatusNotFound)
	expectStatus(t, m.req("GET", "/api/v1/me", nil), http.StatusOK)

	// Suspending a person ends their sessions at once.
	if _, err := e.stack.Identity.SetPersonStatus(context.Background(), audit.System, member.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, m.req("GET", "/api/v1/me", nil), http.StatusUnauthorized)

	// Idle sessions expire.
	e.exec("UPDATE sessions SET last_seen_at = now() - $1::interval", strconv.Itoa(int(e.cfg.Identity.SessionIdleTimeout.Seconds())+60)+" seconds")
	expectStatus(t, a.req("GET", "/api/v1/me", nil), http.StatusUnauthorized)
}

func TestCSRFProtection(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	e.setupOwner(b)
	const path = "/api/v1/me/passkey-offer/dismiss"

	expectStatus(t, b.req("POST", path, nil, web.CSRFHeader, ""), http.StatusForbidden)
	expectStatus(t, b.req("POST", path, nil, web.CSRFHeader, "forged"), http.StatusForbidden)
	expectStatus(t, b.req("POST", path, nil, "Origin", "https://evil.example"), http.StatusForbidden)
	expectStatus(t, b.req("POST", path, nil, "Origin", ""), http.StatusForbidden) // no Origin, no Referer
	expectStatus(t, b.req("POST", path, nil, "Origin", "", "Referer", origin+"/settings"), http.StatusNoContent)
	expectStatus(t, b.req("POST", path, nil), http.StatusNoContent)

	// The CSRF token of another session does not work.
	other := e.browser()
	other.signIn(ownerEmail)
	expectStatus(t, b.req("POST", path, nil, web.CSRFHeader, other.csrf), http.StatusForbidden)

	// Login CSRF: sign-in from another origin is refused.
	expectStatus(t, e.browser().req("POST", "/api/v1/auth/sign-in", map[string]string{"email": ownerEmail, "password": ownerPassword},
		"Origin", "https://evil.example"), http.StatusForbidden)
	form := url.Values{"authRequest": {uuid.NewString()}, "email": {ownerEmail}, "password": {ownerPassword}}
	expectStatus(t, e.browser().req("POST", "/oidc/login", form, "Origin", "https://evil.example"), http.StatusForbidden)
}

func TestAccountLockout(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Identity.SignInAccountLimit = 3 })
	e.setupOwner(e.browser())
	b := e.browser()
	wrong := map[string]string{"email": ownerEmail, "password": "not the password"}
	for range 3 {
		expectStatus(t, b.req("POST", "/api/v1/auth/sign-in", wrong), http.StatusUnauthorized)
	}
	// Locked: even the right password is refused for now.
	r := b.req("POST", "/api/v1/auth/sign-in", map[string]string{"email": strings.ToUpper(ownerEmail), "password": ownerPassword})
	expectStatus(t, r, http.StatusTooManyRequests)
	if s, err := strconv.Atoi(r.Header.Get("Retry-After")); err != nil || s < 1 {
		t.Errorf("Retry-After = %q", r.Header.Get("Retry-After"))
	}
	if e.auditCount(audit.SignInLocked) != 1 || e.auditCount(audit.SignInFailed) < 4 {
		t.Errorf("lockout audit: %d locked, %d failed", e.auditCount(audit.SignInLocked), e.auditCount(audit.SignInFailed))
	}

	// Unknown accounts lock the same way, so lockout reveals nothing.
	ghost := map[string]string{"email": "ghost@acme.test", "password": "not the password"}
	for range 3 {
		expectStatus(t, b.req("POST", "/api/v1/auth/sign-in", ghost), http.StatusUnauthorized)
	}
	expectStatus(t, b.req("POST", "/api/v1/auth/sign-in", ghost), http.StatusTooManyRequests)

	// After the lockout the right password works and clears the count.
	e.exec("UPDATE sign_in_throttles SET locked_until = now() - interval '1 second'")
	b.signIn(ownerEmail)
	var rows int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM sign_in_throttles WHERE account = $1", ownerEmail).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("throttle rows after a successful sign-in = %d, %v", rows, err)
	}
}

func TestIPRateLimitIgnoresForwardedForByDefault(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Identity.SignInIPLimit = 3 })
	e.setupOwner(e.browser()) // the first attempt from 127.0.0.1
	// A client reaching the core directly cannot escape the limit by
	// inventing a new X-Forwarded-For address per request.
	wrong := map[string]string{"email": "someone@acme.test", "password": "not the password"}
	for i, xff := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		attacker := e.browser()
		attacker.header.Set("X-Forwarded-For", xff)
		want := http.StatusUnauthorized
		if i == 2 {
			want = http.StatusTooManyRequests
		}
		expectStatus(t, attacker.req("POST", "/api/v1/auth/sign-in", wrong), want)
	}
	var ip string
	if err := e.pool.QueryRow(context.Background(), "SELECT host(ip) FROM audit_entries WHERE action = 'sign_in.failed' LIMIT 1").Scan(&ip); err != nil || ip != "127.0.0.1" {
		t.Errorf("audited client IP = %q, %v; want the peer, not X-Forwarded-For", ip, err)
	}
}

func TestIPRateLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.Identity.SignInIPLimit = 3
		c.Server.TrustedProxies = "127.0.0.1/32" // the test client plays the proxy
	})
	e.setupOwner(e.browser())
	attacker := e.browser()
	attacker.header.Set("X-Forwarded-For", "203.0.113.9")
	wrong := map[string]string{"email": "someone@acme.test", "password": "not the password"}
	for range 3 {
		expectStatus(t, attacker.req("POST", "/api/v1/auth/sign-in", wrong), http.StatusUnauthorized)
	}
	r := attacker.req("POST", "/api/v1/auth/sign-in", map[string]string{"email": ownerEmail, "password": ownerPassword})
	expectStatus(t, r, http.StatusTooManyRequests)
	expectStatus(t, attacker.req("POST", "/api/v1/auth/passkey/options", nil), http.StatusTooManyRequests)
	if r.Header.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}

	// Another address is not affected.
	other := e.browser()
	other.header.Set("X-Forwarded-For", "198.51.100.4")
	other.signIn(ownerEmail)

	// Reported once per window, with the client address.
	var n int
	var ip string
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*), max(host(ip)) FROM audit_entries WHERE action = 'sign_in.rate_limited'").Scan(&n, &ip); err != nil {
		t.Fatal(err)
	}
	if n != 1 || ip != "203.0.113.9" {
		t.Errorf("rate-limit audit entries = %d from %s, want 1 from 203.0.113.9", n, ip)
	}
}

func TestPermissionsWithRealSessions(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	member := e.createMember("max")
	m := e.browser()
	m.signIn(member.WorkEmail)

	expectStatus(t, owner.req("GET", "/api/v1/instance", nil), http.StatusOK)
	expectStatus(t, m.req("GET", "/api/v1/instance", nil), http.StatusForbidden)
	expectStatus(t, e.browser().req("GET", "/api/v1/instance", nil), http.StatusUnauthorized)
	// Self-service needs no permission.
	expectStatus(t, m.req("GET", "/api/v1/me/sessions", nil), http.StatusOK)
	expectStatus(t, e.browser().req("GET", "/api/v1/me/sessions", nil), http.StatusUnauthorized)

	// Roles are resolved on every request.
	if err := e.stack.Identity.AssignRole(context.Background(), audit.System, member.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, m.req("GET", "/api/v1/instance", nil), http.StatusOK)
}

func TestHTTPDemoModeCookieNames(t *testing.T) {
	e := newEnv(t)
	plain, err := web.New(web.Config{WorkspaceURL: "http://localhost:8080", IdleTimeout: 1, AbsoluteTimeout: 1, IPLimit: 1, IPWindow: 1},
		e.stack.Identity, nil, mustKeyring(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.SessionCookieName() != web.InsecureSessionCookie || plain.DeviceCookieName() != web.InsecureDeviceCookie {
		t.Errorf("http cookie names = %s, %s", plain.SessionCookieName(), plain.DeviceCookieName())
	}
	if e.stack.Auth.SessionCookieName() != web.SecureSessionCookie {
		t.Errorf("https cookie name = %s", e.stack.Auth.SessionCookieName())
	}
}
