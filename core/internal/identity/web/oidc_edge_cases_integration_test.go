//go:build integration

package web_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/oidcop"
	"github.com/orglane/orglane/tests/auditfault"
)

// Edge cases of the revocation endpoint (RFC 7009), the logout
// confirmation and signing key rotation that the main flow tests in
// oidc_integration_test.go do not reach. Already covered there: revoking an
// access token with its own hint (TestOIDCAuthorizationRules), revoking a
// refresh token with or without the refresh_token hint and by another
// client (TestRevokeRefreshToken), and the logout confirmation with a
// session (TestOIDCEndSession).

// revocationClient is a confidential client with refresh tokens: it can
// revoke and introspect its tokens.
type revocationClient struct {
	t      *testing.T
	b      *browser // signed in as the owner, answers authorization requests
	api    *browser // no session: only the bearer token authenticates
	secret string
}

func newRevocationClient(e *env) *revocationClient {
	b := e.browser()
	e.setupOwner(b)
	secret := e.registerClient(identity.NewOIDCClient{ID: "forgejo", RefreshTokens: true})
	return &revocationClient{t: e.t, b: b, api: e.browser(), secret: secret}
}

func (c *revocationClient) basic() []string {
	return []string{"Origin", "", "Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte("forgejo:"+c.secret))}
}

func (c *revocationClient) issue() tokenResponse {
	c.t.Helper()
	q := clientRedirect(c.t, c.b.authorize(authParams("forgejo", pkce{})))
	status, tr := c.b.token(url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {redirectURI}}, "forgejo", c.secret)
	if status != http.StatusOK || tr.AccessToken == "" || tr.RefreshToken == "" {
		c.t.Fatalf("token exchange = %d %+v", status, tr)
	}
	return tr
}

func (c *revocationClient) refresh(token string) (int, tokenResponse) {
	c.t.Helper()
	return c.b.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}}, "forgejo", c.secret)
}

// revoke revokes a token; RFC 7009 answers 200 for any token, valid or not.
func (c *revocationClient) revoke(token, hint string) {
	c.t.Helper()
	form := url.Values{"token": {token}}
	if hint != "" {
		form.Set("token_type_hint", hint)
	}
	r := c.b.req("POST", "/oidc/revoke", form, c.basic()...)
	if r.StatusCode != http.StatusOK {
		c.t.Fatalf("revoke (hint %q) = %d %s, want 200", hint, r.StatusCode, r.body)
	}
}

// accepted reports whether an access token is accepted. The product API,
// userinfo and introspection must agree.
func (c *revocationClient) accepted(token string) bool {
	c.t.Helper()
	api := c.api.req("GET", "/api/v1/me", nil, bearer(token)...).StatusCode == http.StatusOK
	userinfo := c.api.req("GET", "/oidc/userinfo", nil, bearer(token)...).StatusCode == http.StatusOK
	r := c.b.req("POST", "/oidc/introspect", url.Values{"token": {token}}, c.basic()...)
	expectStatus(c.t, r, http.StatusOK)
	var introspection struct {
		Active bool `json:"active"`
	}
	r.json(c.t, &introspection)
	if api != userinfo || api != introspection.Active {
		c.t.Fatalf("access token accepted by the API: %v, userinfo: %v, introspection: %v", api, userinfo, introspection.Active)
	}
	return api
}

// TestRevocationTokenTypeHints revokes each kind of token with a wrong and
// with an unknown token_type_hint: the hint only speeds up the lookup (RFC
// 7009, section 2.1), so the token is revoked either way. A refresh token
// with the access_token hint has its own regression test,
// TestRevokeRefreshTokenWithAccessTokenHint. The access token
// is checked right after the revocation, before anything else uses the
// grant, so a refresh attempt (which revokes a grant on reuse) cannot be
// what rejected it.
func TestRevocationTokenTypeHints(t *testing.T) {
	e := newEnv(t)
	c := newRevocationClient(e)

	for _, tc := range []struct {
		name, hint string
		refresh    bool // revoke the refresh token, else the access token
	}{
		{"access token, refresh_token hint", "refresh_token", false},
		{"access token, unknown hint", "id_token", false},
		{"refresh token, unknown hint", "no_such_type", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := c.issue()
			if !c.accepted(tr.AccessToken) {
				t.Fatal("a fresh access token is rejected")
			}
			before := e.auditCount(audit.OIDCGrantRevoked)
			token := tr.AccessToken
			if tc.refresh {
				token = tr.RefreshToken
			}
			c.revoke(token, tc.hint)
			if c.accepted(tr.AccessToken) {
				t.Fatal("the access token is accepted after the revocation")
			}
			if !tc.refresh {
				// Revoking an access token does not revoke its grant.
				if n := e.auditCount(audit.OIDCGrantRevoked) - before; n != 0 {
					t.Fatalf("%d oidc_grant.revoked entries for revoking an access token, want 0", n)
				}
				return
			}
			if n := e.auditCount(audit.OIDCGrantRevoked) - before; n != 1 {
				t.Fatalf("%d oidc_grant.revoked entries, want 1", n)
			}
			if status, again := c.refresh(tr.RefreshToken); status != http.StatusBadRequest || again.Error != "invalid_grant" {
				t.Fatalf("refresh with the revoked token = %d %+v", status, again)
			}
		})
	}
}

// TestRevocationOfUnknownTokens: tokens this provider did not issue (or not
// in this form) are answered with 200 and change nothing (RFC 7009, section
// 2.2), whatever the hint.
func TestRevocationOfUnknownTokens(t *testing.T) {
	e := newEnv(t)
	c := newRevocationClient(e)
	live := c.issue()
	before := e.auditCount(audit.OIDCGrantRevoked)

	for _, token := range []string{
		"garbage",
		crypto.RandomToken(""),
		crypto.RandomToken(oidcop.RefreshTokenPrefix), // the refresh token format, never issued
		live.RefreshToken + "x",
		live.AccessToken[:len(live.AccessToken)-4],
	} {
		for _, hint := range []string{"", "access_token", "refresh_token", "no_such_type"} {
			c.revoke(token, hint)
		}
	}
	if !c.accepted(live.AccessToken) {
		t.Fatal("revoking unknown tokens rejected a live access token")
	}
	if n := e.auditCount(audit.OIDCGrantRevoked) - before; n != 0 {
		t.Fatalf("%d oidc_grant.revoked entries for unknown tokens, want 0", n)
	}
	if status, tr := c.refresh(live.RefreshToken); status != http.StatusOK {
		t.Fatalf("refresh after revoking unknown tokens = %d %+v", status, tr)
	}
}

// TestRepeatedRevocation: revoking a token that is already revoked answers
// 200 every time (RFC 7009) and audits the grant revocation once.
func TestRepeatedRevocation(t *testing.T) {
	e := newEnv(t)
	c := newRevocationClient(e)

	t.Run("access token", func(t *testing.T) {
		tr := c.issue()
		for range 3 {
			c.revoke(tr.AccessToken, "access_token")
			if c.accepted(tr.AccessToken) {
				t.Fatal("the revoked access token is accepted")
			}
		}
	})

	t.Run("refresh token", func(t *testing.T) {
		tr := c.issue()
		before := e.auditCount(audit.OIDCGrantRevoked)
		for _, hint := range []string{"refresh_token", "refresh_token", "", "access_token"} {
			c.revoke(tr.RefreshToken, hint)
			if c.accepted(tr.AccessToken) {
				t.Fatal("an access token of the revoked grant is accepted")
			}
		}
		if n := e.auditCount(audit.OIDCGrantRevoked) - before; n != 1 {
			t.Fatalf("%d oidc_grant.revoked entries after repeated revocation, want 1", n)
		}
		if status, again := c.refresh(tr.RefreshToken); status != http.StatusBadRequest || again.Error != "invalid_grant" {
			t.Fatalf("refresh with the revoked token = %d %+v", status, again)
		}
	})
}

// TestRevokeRotatedRefreshToken revokes one refresh token of a family that
// was rotated twice: an already used one, or the current one. Either
// revokes the grant: every access token of the family is rejected at once,
// before any refresh attempt, and no refresh token of the family works
// afterwards.
func TestRevokeRotatedRefreshToken(t *testing.T) {
	e := newEnv(t)
	c := newRevocationClient(e)

	for _, revoked := range []int{0, 2} {
		name := map[int]string{0: "used token", 2: "current token"}[revoked]
		t.Run(name, func(t *testing.T) {
			family := []tokenResponse{c.issue()}
			for range 2 {
				status, next := c.refresh(family[len(family)-1].RefreshToken)
				if status != http.StatusOK || next.RefreshToken == "" {
					t.Fatalf("refresh = %d %+v", status, next)
				}
				family = append(family, next)
			}
			for i, tr := range family {
				if !c.accepted(tr.AccessToken) {
					t.Fatalf("access token %d of the family is rejected before the revocation", i)
				}
			}
			before := e.auditCount(audit.OIDCGrantRevoked)

			c.revoke(family[revoked].RefreshToken, "refresh_token")
			for i, tr := range family {
				if c.accepted(tr.AccessToken) {
					t.Fatalf("access token %d of the family is accepted after the revocation", i)
				}
			}
			if n := e.auditCount(audit.OIDCGrantRevoked) - before; n != 1 {
				t.Fatalf("%d oidc_grant.revoked entries, want 1", n)
			}
			for i, tr := range family {
				if status, again := c.refresh(tr.RefreshToken); status != http.StatusBadRequest || again.Error != "invalid_grant" {
					t.Fatalf("refresh with token %d of the revoked family = %d %+v", i, status, again)
				}
			}
			// The refresh attempts found a revoked grant: nothing more to audit.
			if n := e.auditCount(audit.OIDCGrantRevoked) - before; n != 1 {
				t.Fatalf("%d oidc_grant.revoked entries after refresh attempts, want 1", n)
			}
		})
	}
}

// TestOIDCLogoutWithoutSession is a regression test for the logout
// confirmation in a browser without a session: no session cookie, or the
// cookie of a session that has ended. GET does not show the confirmation
// page and POST needs no CSRF token, since there is no session to end or
// to authenticate the request; both go on to the signed target, or to the
// workspace when the target is missing or tampered with. A POST must still
// come from the workspace origin, and nothing is revoked or audited.
func TestOIDCLogoutWithoutSession(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	owner := e.setupOwner(b)
	const bye = "https://app.acme.test/bye"
	e.registerClient(identity.NewOIDCClient{ID: "app", Public: true, PostLogoutRedirectURIs: []string{bye}})

	// A signed confirmation link, as end_session hands it out.
	r := b.req("GET", "/oidc/end_session?"+url.Values{"client_id": {"app"}, "post_logout_redirect_uri": {bye}, "state": {"s1"}}.Encode(), nil)
	confirm, err := url.Parse(r.Header.Get("Location"))
	if err != nil || confirm.Path != oidcop.PathLogout {
		t.Fatalf("end_session without a hint = %d %q, want the confirmation page", r.StatusCode, r.Header.Get("Location"))
	}
	signed := url.Values{"next": {confirm.Query().Get("next")}, "sig": {confirm.Query().Get("sig")}}
	if next := signed.Get("next"); !strings.HasPrefix(next, bye) || !strings.Contains(next, "state=s1") {
		t.Fatalf("signed target = %q", next)
	}
	tampered := maps.Clone(signed)
	tampered.Set("next", "https://evil.example/")
	unsigned := url.Values{"next": {signed.Get("next")}}

	// End every session of the owner: b keeps the cookie of an ended one.
	if _, err := e.stack.Identity.RevokePersonSessions(context.Background(), audit.System, owner.Person.ID); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusUnauthorized)
	b.csrf = ""
	revoked := e.auditCount(audit.SessionRevoked)
	var sessions int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM sessions").Scan(&sessions); err != nil {
		t.Fatal(err)
	}

	for name, c := range map[string]*browser{"no session cookie": e.browser(), "an ended session": b} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				form url.Values
				want string
			}{
				{"signed target", signed, signed.Get("next")},
				{"tampered target", tampered, origin + "/"},
				{"unsigned target", unsigned, origin + "/"},
				{"no target", url.Values{}, origin + "/"},
			} {
				r := c.req("GET", oidcop.PathLogout+"?"+tc.form.Encode(), nil)
				if loc := r.Header.Get("Location"); r.StatusCode != http.StatusFound || loc != tc.want {
					t.Errorf("GET with %s = %d %q, want 302 to %q", tc.name, r.StatusCode, loc, tc.want)
				}
				r = c.req("POST", oidcop.PathLogout, tc.form)
				if loc := r.Header.Get("Location"); r.StatusCode != http.StatusSeeOther || loc != tc.want {
					t.Errorf("POST with %s = %d %q, want 303 to %q", tc.name, r.StatusCode, loc, tc.want)
				}
			}
			// Without a session the origin check still applies.
			for _, o := range []string{"https://evil.example", ""} {
				if r := c.req("POST", oidcop.PathLogout, signed, "Origin", o); r.StatusCode != http.StatusForbidden {
					t.Errorf("POST with Origin %q = %d %q, want 403", o, r.StatusCode, r.Header.Get("Location"))
				}
			}
		})
	}

	if n := e.auditCount(audit.SessionRevoked) - revoked; n != 0 {
		t.Errorf("%d session.revoked entries from logouts without a session, want 0", n)
	}
	var after int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM sessions").Scan(&after); err != nil || after != sessions {
		t.Errorf("sessions = %d (%v), want %d: a logout without a session created or deleted one", after, err, sessions)
	}
}

// TestKeyRotationLogsAfterCommit is a regression test: a rotation whose
// transaction fails after creating the new key (here: its audit entry) must
// not log that it created a key or deleted expired ones, since both rolled
// back. A rotation that commits logs both.
func TestKeyRotationLogsAfterCommit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var logs bytes.Buffer
	clk := &clock{t: time.Now()}
	keys := oidcop.NewKeyManager(e.pool, mustKeyring(t), oidcop.KeyManagerOptions{
		Rotation: e.cfg.Identity.SigningKeyRotation, Grace: e.cfg.Identity.SigningKeyGrace,
		Now: clk.Now, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	const created, deleted = "created a new token signing key", "deleted expired signing keys"

	// The key newEnv created is retired; after its grace period it is due
	// for deletion on the next rotation.
	if err := keys.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Advance(e.cfg.Identity.SigningKeyGrace + time.Minute)
	logs.Reset()

	fault := auditfault.Inject(t, e.pool, "NEW.action = '"+string(audit.SigningKeyCreated)+"'", 1)
	if err := keys.Rotate(ctx); !auditfault.Is(err) {
		t.Fatalf("Rotate() with a failing audit entry = %v, want the injected failure", err)
	}
	if fault.Hits() != 1 {
		t.Fatalf("the fault failed %d audit entries, want 1", fault.Hits())
	}
	if s := logs.String(); strings.Contains(s, created) || strings.Contains(s, deleted) {
		t.Fatalf("a rolled back rotation logged:\n%s", s)
	}

	fault.Remove()
	if err := keys.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if s := logs.String(); strings.Count(s, created) != 1 || strings.Count(s, deleted) != 1 {
		t.Fatalf("a committed rotation logged:\n%s", s)
	}
}
