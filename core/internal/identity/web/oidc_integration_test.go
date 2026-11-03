//go:build integration

package web_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/web"
)

const redirectURI = "https://app.acme.test/callback"

func mustKeyring(t *testing.T) *crypto.Keyring {
	t.Helper()
	k, err := crypto.NewKeyring("integration-test-master-secret-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (e *env) registerClient(in identity.NewOIDCClient) string {
	e.t.Helper()
	if in.Name == "" {
		in.Name = in.ID
	}
	if in.RedirectURIs == nil {
		in.RedirectURIs = []string{redirectURI}
	}
	_, secret, err := e.stack.Identity.CreateOIDCClient(context.Background(), audit.System, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return secret
}

type pkce struct{ verifier, challenge string }

func newPKCE() pkce {
	v := crypto.RandomToken("")
	sum := sha256.Sum256([]byte(v))
	return pkce{verifier: v, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}
}

// authorize starts an authorization request and follows the provider's
// redirects until it leaves the workspace or shows the login form. It
// returns the last response.
func (b *browser) authorize(params url.Values) response {
	b.e.t.Helper()
	r := b.req("GET", "/oidc/authorize?"+params.Encode(), nil)
	return b.follow(r)
}

func (b *browser) follow(r response) response {
	b.e.t.Helper()
	for range 10 {
		loc := r.Header.Get("Location")
		if r.StatusCode < 300 || r.StatusCode >= 400 || loc == "" {
			return r
		}
		u, err := r.Request.URL.Parse(loc)
		if err != nil {
			b.e.t.Fatal(err)
		}
		if u.Host != workspaceHost {
			return r // back at the client
		}
		if u.Path == web.SignInPath {
			return r // the web UI's sign-in screen
		}
		r = b.req("GET", u.String(), nil)
	}
	b.e.t.Fatal("too many redirects")
	return r
}

func authParams(clientID string, p pkce, extra ...string) url.Values {
	v := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {"openid profile email offline_access"},
		"state":         {"state-123"},
		"nonce":         {"nonce-456"},
	}
	if p.challenge != "" {
		v.Set("code_challenge", p.challenge)
		v.Set("code_challenge_method", "S256")
	}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v
}

// clientRedirect returns the query of a redirect back to the client.
func clientRedirect(t *testing.T, r response) url.Values {
	t.Helper()
	loc := r.Header.Get("Location")
	if !strings.HasPrefix(loc, redirectURI) {
		t.Fatalf("expected a redirect to the client, got %d %q: %s", r.StatusCode, loc, r.body)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// signInURL returns the auth request of a redirect to the web UI's
// sign-in screen, or "" for any other response.
func signInRedirect(r response) (authRequest string, query url.Values) {
	loc, err := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != http.StatusSeeOther || err != nil || loc.Path != web.SignInPath {
		return "", nil
	}
	return loc.Query().Get("authRequest"), loc.Query()
}

// signInScreen does what the web UI's sign-in screen does for an auth
// request: sign in as the owner through the API, then return to /oidc/login, which
// completes the request. It returns the failed sign-in response, or where
// the flow ends.
func (b *browser) signInScreen(page response, password string) response {
	b.e.t.Helper()
	authRequest, _ := signInRedirect(page)
	if authRequest == "" {
		b.e.t.Fatalf("expected a redirect to the sign-in screen, got %d %q: %s", page.StatusCode, page.Header.Get("Location"), page.body)
	}
	r := b.req("POST", "/api/v1/auth/sign-in", map[string]string{"email": ownerEmail, "password": password})
	if r.StatusCode != http.StatusOK {
		return r
	}
	var res signInResult
	r.json(b.e.t, &res)
	b.csrf = res.CsrfToken
	return b.follow(b.req("GET", "/oidc/login?authRequest="+url.QueryEscape(authRequest), nil))
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

func (b *browser) token(form url.Values, basic ...string) (int, tokenResponse) {
	b.e.t.Helper()
	headers := []string{"Origin", ""}
	if len(basic) == 2 {
		headers = append(headers, "Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(basic[0]+":"+basic[1])))
	}
	r := b.req("POST", "/oidc/token", form, headers...)
	if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("Pragma") != "no-cache" {
		b.e.t.Errorf("token response cache headers = %q, %q; want no-store", r.Header.Get("Cache-Control"), r.Header.Get("Pragma"))
	}
	var tr tokenResponse
	r.json(b.e.t, &tr)
	return r.StatusCode, tr
}

// verifyIDToken checks the signature against the JWKS and returns the
// claims.
func (b *browser) verifyIDToken(raw string) map[string]any {
	b.e.t.Helper()
	sig, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		b.e.t.Fatal(err)
	}
	r := b.req("GET", "/oidc/keys", nil)
	expectStatus(b.e.t, r, http.StatusOK)
	var jwks jose.JSONWebKeySet
	r.json(b.e.t, &jwks)
	keys := jwks.Key(sig.Signatures[0].Header.KeyID)
	if len(keys) != 1 {
		b.e.t.Fatalf("kid %q not in the JWKS", sig.Signatures[0].Header.KeyID)
	}
	payload, err := sig.Verify(keys[0])
	if err != nil {
		b.e.t.Fatalf("ID token signature: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		b.e.t.Fatal(err)
	}
	return claims
}

func bearer(token string) []string { return []string{"Authorization", "Bearer " + token} }

func TestOIDCCodeFlowWithPKCE(t *testing.T) {
	e := newEnv(t)
	owner := e.setupOwner(e.browser())
	e.registerClient(identity.NewOIDCClient{ID: "mobile", Public: true, Native: true, RefreshTokens: true})

	b := e.browser()
	p := newPKCE()
	page := b.authorize(authParams("mobile", p))
	r := b.signInScreen(page, "wrong password!")
	expectStatus(t, r, http.StatusUnauthorized)
	r = b.signInScreen(page, ownerPassword)
	q := clientRedirect(t, r)
	if q.Get("state") != "state-123" || q.Get("code") == "" {
		t.Fatalf("callback = %v", q)
	}
	code := q.Get("code")

	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "client_id": {"mobile"}}
	// The code verifier is required and checked.
	bad := url.Values{}
	for k, v := range exchange {
		bad[k] = v
	}
	bad.Set("code_verifier", newPKCE().verifier)
	if status, tr := b.token(bad); status != http.StatusBadRequest || tr.Error != "invalid_grant" {
		t.Fatalf("wrong code_verifier = %d %+v", status, tr)
	}
	// A failed exchange consumed the code: the flow starts over.
	page = b.authorize(authParams("mobile", p))
	q = clientRedirect(t, page) // the browser session answers at once
	exchange.Set("code", q.Get("code"))
	exchange.Set("code_verifier", p.verifier)
	status, tr := b.token(exchange)
	if status != http.StatusOK || tr.AccessToken == "" || tr.IDToken == "" || tr.RefreshToken == "" || tr.TokenType != "Bearer" {
		t.Fatalf("token exchange = %d %+v", status, tr)
	}

	claims := b.verifyIDToken(tr.IDToken)
	if claims["iss"] != origin || claims["sub"] != owner.Person.ID.String() || claims["nonce"] != "nonce-456" ||
		!slices.Contains(toStrings(claims["aud"]), "mobile") {
		t.Fatalf("ID token claims = %v", claims)
	}
	if amr := toStrings(claims["amr"]); !slices.Contains(amr, "pwd") {
		t.Errorf("amr = %v, want pwd", amr)
	}
	if _, ok := claims["auth_time"]; !ok {
		t.Error("ID token without auth_time")
	}

	// userinfo and the product API accept the access token.
	var info map[string]any
	r = b.req("GET", "/oidc/userinfo", nil, bearer(tr.AccessToken)...)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &info)
	if info["sub"] != owner.Person.ID.String() || info["email"] != ownerEmail || info["email_verified"] != true || info["preferred_username"] != "owner" {
		t.Fatalf("userinfo = %v", info)
	}
	api := e.browser()
	expectStatus(t, api.req("GET", "/api/v1/instance", nil, bearer(tr.AccessToken)...), http.StatusOK)
	expectStatus(t, api.req("GET", "/api/v1/me", nil, bearer(tr.AccessToken)...), http.StatusOK)
	expectStatus(t, api.req("GET", "/api/v1/instance", nil, bearer("garbage")...), http.StatusUnauthorized)
	// Bearer requests need no CSRF token.
	expectStatus(t, api.req("POST", "/api/v1/me/passkey-offer/dismiss", nil, append(bearer(tr.AccessToken), "Origin", "")...), http.StatusNoContent)

	// Refresh tokens rotate; a used one revokes the whole grant.
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}, "client_id": {"mobile"}}
	status, rotated := b.token(refresh)
	if status != http.StatusOK || rotated.RefreshToken == "" || rotated.RefreshToken == tr.RefreshToken || rotated.AccessToken == "" {
		t.Fatalf("refresh = %d %+v", status, rotated)
	}
	b.verifyIDToken(rotated.IDToken)
	if status, again := b.token(refresh); status != http.StatusBadRequest || again.Error != "invalid_grant" {
		t.Fatalf("reused refresh token = %d %+v", status, again)
	}
	if status, _ := b.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rotated.RefreshToken}, "client_id": {"mobile"}}); status != http.StatusBadRequest {
		t.Fatalf("the rotated token survived reuse detection: %d", status)
	}
	expectStatus(t, api.req("GET", "/api/v1/me", nil, bearer(rotated.AccessToken)...), http.StatusUnauthorized)
	if e.auditCount(audit.OIDCGrantRevoked) == 0 {
		t.Error("revoking the grant was not audited")
	}

	// A code can be exchanged once; reusing it revokes what it issued.
	q = clientRedirect(t, b.authorize(authParams("mobile", p)))
	exchange.Set("code", q.Get("code"))
	status, first := b.token(exchange)
	if status != http.StatusOK {
		t.Fatalf("exchange = %d %+v", status, first)
	}
	if status, again := b.token(exchange); status != http.StatusBadRequest || again.Error != "invalid_grant" {
		t.Fatalf("reused code = %d %+v", status, again)
	}
	expectStatus(t, api.req("GET", "/api/v1/me", nil, bearer(first.AccessToken)...), http.StatusUnauthorized)

	// Suspending the person invalidates their tokens at once.
	q = clientRedirect(t, b.authorize(authParams("mobile", p)))
	exchange.Set("code", q.Get("code"))
	_, live := b.token(exchange)
	expectStatus(t, api.req("GET", "/api/v1/me", nil, bearer(live.AccessToken)...), http.StatusOK)
	e.exec("UPDATE people SET status = 'suspended'")
	expectStatus(t, api.req("GET", "/api/v1/me", nil, bearer(live.AccessToken)...), http.StatusUnauthorized)
	// zitadel answers userinfo failures with 403.
	if r := b.req("GET", "/oidc/userinfo", nil, bearer(live.AccessToken)...); r.StatusCode != http.StatusUnauthorized && r.StatusCode != http.StatusForbidden {
		t.Fatalf("userinfo for a suspended person = %d %s", r.StatusCode, r.body)
	}
}

// TestBasicSignInForm covers the form for browsers that cannot run the web
// UI (the conformance suite's): it posts to /oidc/login and returns to the
// sign-in screen with an error code, or completes the request.
func TestBasicSignInForm(t *testing.T) {
	e := newEnv(t)
	e.setupOwner(e.browser())
	e.registerClient(identity.NewOIDCClient{ID: "app", Public: true})

	b := e.browser()
	page := b.authorize(authParams("app", newPKCE()))
	authRequest, _ := signInRedirect(page)
	if authRequest == "" {
		t.Fatalf("expected the sign-in screen, got %d", page.StatusCode)
	}
	post := func(password string, headers ...string) response {
		return b.req("POST", "/oidc/login", url.Values{"authRequest": {authRequest}, "email": {ownerEmail}, "password": {password}}, headers...)
	}

	// Login CSRF: posts from another origin are refused.
	expectStatus(t, post(ownerPassword, "Origin", "https://evil.example"), http.StatusForbidden)

	r := post("wrong password!")
	id, q := signInRedirect(r)
	if id != authRequest || q.Get("error") != web.SignInErrorCredentials || q.Get("email") != ownerEmail || len(r.Cookies()) > 0 {
		t.Fatalf("wrong password = %d %q (cookies %v)", r.StatusCode, r.Header.Get("Location"), r.Cookies())
	}

	q = clientRedirect(t, b.follow(post(ownerPassword)))
	if q.Get("code") == "" || q.Get("state") != "state-123" {
		t.Fatalf("callback = %v", q)
	}
	// An unknown or finished request is an error page, not a redirect.
	if r := post(ownerPassword); r.StatusCode != http.StatusBadRequest || !strings.Contains(string(r.body), "expired") {
		t.Fatalf("reused auth request = %d %s", r.StatusCode, r.body)
	}
}

func TestOIDCAuthorizationRules(t *testing.T) {
	e := newEnv(t)
	e.setupOwner(e.browser())
	e.registerClient(identity.NewOIDCClient{ID: "spa", Public: true})
	secret := e.registerClient(identity.NewOIDCClient{ID: "forgejo"})
	p := newPKCE()

	// Public clients must use PKCE with S256.
	b := e.browser()
	if q := clientRedirect(t, b.authorize(authParams("spa", pkce{}))); q.Get("error") != "invalid_request" {
		t.Fatalf("public client without PKCE = %v", q)
	}
	plain := authParams("spa", pkce{}, "code_challenge", "abc", "code_challenge_method", "plain")
	if q := clientRedirect(t, b.authorize(plain)); q.Get("error") != "invalid_request" {
		t.Fatalf("plain PKCE = %v", q)
	}
	// Unknown redirect URIs are never redirected to.
	bad := authParams("spa", p, "redirect_uri", "https://evil.example/cb")
	if r := b.authorize(bad); r.StatusCode != http.StatusBadRequest || strings.Contains(r.Header.Get("Location"), "evil") ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(r.body), "invalid sign-in request") {
		t.Fatalf("unregistered redirect URI = %d %q %s", r.StatusCode, r.Header.Get("Location"), r.body)
	}

	// prompt=none without a session: login_required, no form.
	if q := clientRedirect(t, b.authorize(authParams("spa", p, "prompt", "none"))); q.Get("error") != "login_required" || q.Get("state") != "state-123" {
		t.Fatalf("prompt=none = %v", q)
	}

	// A confidential client authenticates with its secret (no PKCE needed).
	page := b.authorize(authParams("forgejo", pkce{}))
	q := clientRedirect(t, b.signInScreen(page, ownerPassword))
	form := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {redirectURI}}
	if status, tr := b.token(form, "forgejo", "wrong-secret"); status != http.StatusUnauthorized && status != http.StatusBadRequest || tr.AccessToken != "" {
		t.Fatalf("wrong client secret = %d %+v", status, tr)
	}
	q = clientRedirect(t, b.authorize(authParams("forgejo", pkce{})))
	form.Set("code", q.Get("code"))
	status, tr := b.token(form, "forgejo", secret)
	if status != http.StatusOK || tr.AccessToken == "" || tr.RefreshToken != "" {
		t.Fatalf("confidential exchange = %d %+v (no refresh grant registered)", status, tr)
	}

	// With a session, prompt=none succeeds and prompt=login asks again.
	if q := clientRedirect(t, b.authorize(authParams("spa", p, "prompt", "none"))); q.Get("code") == "" {
		t.Fatalf("prompt=none with a session = %v", q)
	}
	r := b.authorize(authParams("spa", p, "prompt", "login", "login_hint", ownerEmail))
	authRequest, signInQuery := signInRedirect(r)
	if authRequest == "" || signInQuery.Get("email") != ownerEmail {
		t.Fatalf("prompt=login did not ask to sign in (with the login hint): %d %q %s", r.StatusCode, r.Header.Get("Location"), r.body)
	}
	// The existing session still does not answer it; a new sign-in does.
	if again, _ := signInRedirect(b.follow(b.req("GET", "/oidc/login?authRequest="+url.QueryEscape(authRequest), nil))); again != authRequest {
		t.Fatal("prompt=login was answered by the session that existed before the request")
	}
	if q := clientRedirect(t, b.signInScreen(r, ownerPassword)); q.Get("code") == "" {
		t.Fatalf("prompt=login after signing in again = %v", q)
	}
	time.Sleep(1100 * time.Millisecond)
	if r := b.authorize(authParams("spa", p, "max_age", "1")); func() bool { id, _ := signInRedirect(r); return id == "" }() {
		t.Fatalf("max_age=1 with an older session did not ask to sign in: %d", r.StatusCode)
	}

	// Revoking an access token at the revocation endpoint is immediate.
	api := e.browser()
	expectStatus(t, api.req("GET", "/api/v1/me", nil, bearer(tr.AccessToken)...), http.StatusOK)
	r = b.req("POST", "/oidc/revoke", url.Values{"token": {tr.AccessToken}, "token_type_hint": {"access_token"}},
		"Origin", "", "Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("forgejo:"+secret)))
	expectStatus(t, r, http.StatusOK)
	expectStatus(t, api.req("GET", "/api/v1/me", nil, bearer(tr.AccessToken)...), http.StatusUnauthorized)
}

func TestOIDCEndSession(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	e.setupOwner(b)
	const bye = "https://app.acme.test/bye"
	e.registerClient(identity.NewOIDCClient{ID: "app", Public: true, PostLogoutRedirectURIs: []string{bye}})
	p := newPKCE()
	idToken := func(c *browser) string {
		q := clientRedirect(t, c.authorize(authParams("app", p)))
		_, tr := c.token(url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {redirectURI}, "client_id": {"app"}, "code_verifier": {p.verifier}})
		return tr.IDToken
	}
	endSession := func(c *browser, v url.Values) response {
		return c.req("GET", "/oidc/end_session?"+v.Encode(), nil)
	}
	hint := idToken(b)

	// Invalid requests are rejected before anything happens to the session:
	// a cross-site link cannot sign people out.
	for _, v := range []url.Values{
		{"id_token_hint": {"invalid"}},
		{"id_token_hint": {hint[:len(hint)-4] + "AAAA"}},
		{"client_id": {"app"}, "post_logout_redirect_uri": {"https://evil.example/"}},
		{"client_id": {"nope"}},
	} {
		if r := endSession(b, v); r.StatusCode < 400 {
			t.Errorf("end_session %v = %d %q, want an error", v, r.StatusCode, r.Header.Get("Location"))
		}
		expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusOK)
	}

	// Without an id_token_hint for the signed-in person, the person confirms.
	r := endSession(b, url.Values{"client_id": {"app"}, "post_logout_redirect_uri": {bye}, "state": {"s1"}})
	confirm := r.Header.Get("Location")
	if !strings.HasPrefix(confirm, origin+"/oidc/logout?") {
		t.Fatalf("end_session without a hint = %d %q, want the confirmation page", r.StatusCode, confirm)
	}
	expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusOK)
	u, _ := url.Parse(confirm)
	form := url.Values{"next": {u.Query().Get("next")}, "sig": {u.Query().Get("sig")}}
	token := logoutToken(t, b, confirm)

	// The confirmation must come from the workspace origin and carry the
	// session's CSRF token from the confirmation page.
	other := e.browser()
	other.signIn(ownerEmail) // the same person in another browser
	otherToken := logoutToken(t, other, confirm)
	if otherToken == token {
		t.Fatal("two sessions share a CSRF token")
	}
	for _, c := range []struct {
		name, token string
		headers     []string
	}{
		{"foreign origin", token, []string{"Origin", "https://evil.example"}},
		{"no origin", token, []string{"Origin", ""}},
		{"missing token", "", nil},
		{"wrong token", token[:len(token)-4] + "AAAA", nil},
		{"another session's token", otherToken, nil},
	} {
		f := maps.Clone(form)
		if c.token != "" {
			f.Set("csrf_token", c.token)
		}
		if r := b.req("POST", "/oidc/logout", f, c.headers...); r.StatusCode != http.StatusForbidden {
			t.Errorf("logout confirmation with %s = %d %q, want 403", c.name, r.StatusCode, r.Header.Get("Location"))
		}
		expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusOK)
	}
	expectStatus(t, other.req("GET", "/api/v1/me", nil), http.StatusOK)

	form.Set("csrf_token", token)
	r = b.req("POST", "/oidc/logout", form)
	if loc := r.Header.Get("Location"); r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, bye) || !strings.Contains(loc, "state=s1") {
		t.Fatalf("confirmed logout = %d %q", r.StatusCode, loc)
	}
	expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusUnauthorized)
	// Only this browser's session ended.
	expectStatus(t, other.req("GET", "/api/v1/me", nil), http.StatusOK)

	// The token of the ended session does not confirm for the next one.
	b.signIn(ownerEmail)
	if r := b.req("POST", "/oidc/logout", form); r.StatusCode != http.StatusForbidden {
		t.Fatalf("logout confirmation with the previous session's token = %d %q, want 403", r.StatusCode, r.Header.Get("Location"))
	}
	expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusOK)

	// A tampered target is not followed.
	form.Set("csrf_token", logoutToken(t, b, confirm))
	form.Set("next", "https://evil.example/")
	r = b.req("POST", "/oidc/logout", form)
	if loc := r.Header.Get("Location"); loc != origin+"/" {
		t.Fatalf("logout with a tampered target redirects to %q", loc)
	}

	// An id_token_hint for the signed-in person ends the session at once.
	b.signIn(ownerEmail)
	r = endSession(b, url.Values{"id_token_hint": {idToken(b)}, "post_logout_redirect_uri": {bye}, "state": {"s2"}})
	if loc := r.Header.Get("Location"); !strings.HasPrefix(loc, bye) || !strings.Contains(loc, "state=s2") {
		t.Fatalf("end_session with a hint = %d %q %s", r.StatusCode, loc, r.body)
	}
	expectStatus(t, b.req("GET", "/api/v1/me", nil), http.StatusUnauthorized)

	// A hint for someone else does not end this session without confirmation.
	member := e.createMember("mo")
	m := e.browser()
	m.signIn(member.WorkEmail)
	r = endSession(m, url.Values{"id_token_hint": {hint}, "post_logout_redirect_uri": {bye}})
	if !strings.HasPrefix(r.Header.Get("Location"), origin+"/oidc/logout?") {
		t.Fatalf("end_session with another person's hint = %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	expectStatus(t, m.req("GET", "/api/v1/me", nil), http.StatusOK)
}

func TestDiscoveryDocuments(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	var doc map[string]any
	r := b.req("GET", "/.well-known/orglane", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &doc)
	if doc["workspaceUrl"] != origin || doc["apiBaseUrl"] != origin+"/api/v1" || doc["issuer"] != origin || doc["version"] != "test" || doc["demo"] != false || len(doc) != 5 {
		t.Fatalf("/.well-known/orglane = %v", doc)
	}

	r = b.req("GET", "/.well-known/openid-configuration", nil)
	expectStatus(t, r, http.StatusOK)
	r.json(t, &doc)
	if doc["issuer"] != origin || doc["authorization_endpoint"] != origin+"/oidc/authorize" || doc["jwks_uri"] != origin+"/oidc/keys" ||
		doc["token_endpoint"] != origin+"/oidc/token" || doc["userinfo_endpoint"] != origin+"/oidc/userinfo" {
		t.Fatalf("openid-configuration = %v", doc)
	}
	if m := toStrings(doc["code_challenge_methods_supported"]); !slices.Equal(m, []string{"S256"}) {
		t.Errorf("code_challenge_methods_supported = %v", m)
	}
	if !slices.Contains(toStrings(doc["id_token_signing_alg_values_supported"]), "RS256") {
		t.Errorf("signing algorithms = %v", doc["id_token_signing_alg_values_supported"])
	}
}

func TestSigningKeyRotation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.browser()
	e.setupOwner(b)
	e.registerClient(identity.NewOIDCClient{ID: "app", Public: true})
	p := newPKCE()
	issue := func() string {
		q := clientRedirect(t, b.authorize(authParams("app", p)))
		status, tr := b.token(url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {redirectURI}, "client_id": {"app"}, "code_verifier": {p.verifier}})
		if status != http.StatusOK {
			t.Fatalf("token = %d %+v", status, tr)
		}
		return tr.IDToken
	}
	kid := func(raw string) string {
		sig, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			t.Fatal(err)
		}
		return sig.Signatures[0].Header.KeyID
	}
	jwks := func() []string {
		var set jose.JSONWebKeySet
		b.req("GET", "/oidc/keys", nil).json(t, &set)
		var ids []string
		for _, k := range set.Keys {
			ids = append(ids, k.KeyID)
		}
		return ids
	}

	oldToken := issue()
	oldKid := kid(oldToken)
	if keys := jwks(); !slices.Equal(keys, []string{oldKid}) {
		t.Fatalf("JWKS = %v, want just %s", keys, oldKid)
	}

	// Private keys are encrypted at rest, bound to their row.
	var private, public []byte
	if err := e.pool.QueryRow(ctx, "SELECT private_key, public_key FROM signing_keys").Scan(&private, &public); err != nil {
		t.Fatal(err)
	}
	if _, err := mustKeyring(t).Sealer(crypto.PurposeSigningKeys).Open(private, []byte("other row")); err == nil {
		t.Fatal("the private key opens with the wrong associated data")
	}
	if strings.Contains(string(private), "PRIVATE KEY") || len(private) < 1000 {
		t.Fatalf("the stored private key looks unencrypted (%d bytes)", len(private))
	}

	// Not due yet: nothing changes.
	if rotated, err := e.stack.Keys.Ensure(ctx); err != nil || rotated {
		t.Fatalf("Ensure() before the rotation period = %v, %v", rotated, err)
	}

	// After the rotation period the next token is signed with a new key;
	// the old one stays published for the grace period.
	e.clock.Advance(e.cfg.Identity.SigningKeyRotation + time.Minute)
	newToken := issue()
	newKid := kid(newToken)
	if newKid == oldKid {
		t.Fatal("the signing key did not rotate")
	}
	if keys := jwks(); !slices.Equal(keys, []string{newKid, oldKid}) {
		t.Fatalf("JWKS during the grace period = %v, want [%s %s]", keys, newKid, oldKid)
	}
	b.verifyIDToken(oldToken) // still verifies
	b.verifyIDToken(newToken)

	// After the grace period the old key is gone.
	e.clock.Advance(e.cfg.Identity.SigningKeyGrace + time.Minute)
	if err := e.stack.Maintain(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if keys := jwks(); !slices.Equal(keys, []string{newKid}) {
		t.Fatalf("JWKS after the grace period = %v, want [%s]", keys, newKid)
	}
	if e.auditCount(audit.SigningKeyCreated) != 2 || e.auditCount(audit.SigningKeyRetired) != 1 {
		t.Errorf("key audit entries: %d created, %d retired", e.auditCount(audit.SigningKeyCreated), e.auditCount(audit.SigningKeyRetired))
	}

	// A forced rotation works too.
	if err := e.stack.Keys.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if keys := jwks(); len(keys) != 2 || keys[1] != newKid {
		t.Fatalf("JWKS after a forced rotation = %v", keys)
	}
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, s := range x {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// TestConcurrentCodeReuseRevokesTheWinner is a regression test: when a code
// is replayed while the first exchange is still issuing tokens, the tokens
// of the first exchange must not survive the revocation.
func TestConcurrentCodeReuseRevokesTheWinner(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.browser()
	e.setupOwner(b)
	e.registerClient(identity.NewOIDCClient{ID: "app", Public: true, RefreshTokens: true})
	p := newPKCE()
	q := clientRedirect(t, b.authorize(authParams("app", p)))
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {redirectURI}, "client_id": {"app"}, "code_verifier": {p.verifier}}

	// Hold the first exchange inside its transaction, after it claimed the
	// code and locked its grant, right before it inserts the access token.
	const gate = 727272
	e.exec(`CREATE FUNCTION test_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_xact_lock(` + strconv.Itoa(gate) + `); RETURN NEW; END $$`)
	e.exec("CREATE TRIGGER test_gate BEFORE INSERT ON oidc_access_tokens FOR EACH ROW EXECUTE FUNCTION test_gate()")
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", gate); err != nil {
		t.Fatal(err)
	}
	waiting := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var got int
			if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d lock waiters, want %d", got, n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	type result struct {
		status int
		tr     tokenResponse
	}
	first, replay := make(chan result, 1), make(chan result, 1)
	go func() { s, tr := e.browser().token(exchange); first <- result{s, tr} }()
	waiting(1) // the first exchange waits on the gate, holding the grant lock
	go func() { s, tr := e.browser().token(exchange); replay <- result{s, tr} }()
	waiting(2) // the replay waits for the grant lock
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", gate); err != nil {
		t.Fatal(err)
	}

	won, lost := <-first, <-replay
	if won.status != http.StatusOK || won.tr.AccessToken == "" {
		t.Fatalf("first exchange = %d %+v", won.status, won.tr)
	}
	if lost.status != http.StatusBadRequest || lost.tr.Error != "invalid_grant" {
		t.Fatalf("replayed exchange = %d %+v", lost.status, lost.tr)
	}
	e.exec("DROP TRIGGER test_gate ON oidc_access_tokens")
	api := e.browser()
	expectStatus(t, api.req("GET", "/api/v1/me", nil, bearer(won.tr.AccessToken)...), http.StatusUnauthorized)
	if status, tr := b.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {won.tr.RefreshToken}, "client_id": {"app"}}); status != http.StatusBadRequest {
		t.Fatalf("refresh token of a revoked grant = %d %+v", status, tr)
	}
	var revoked bool
	if err := e.pool.QueryRow(ctx, "SELECT revoked_at IS NOT NULL FROM oidc_grants").Scan(&revoked); err != nil || !revoked {
		t.Fatalf("grant revoked = %v, %v", revoked, err)
	}
}

var logoutTokenField = regexp.MustCompile(`<input type="hidden" name="csrf_token" value="([^"]+)">`)

// logoutToken opens the logout confirmation page in the browser and
// returns the CSRF token of its form.
func logoutToken(t *testing.T, b *browser, confirm string) string {
	t.Helper()
	page := b.req("GET", confirm, nil)
	expectStatus(t, page, http.StatusOK)
	m := logoutTokenField.FindSubmatch(page.body)
	if m == nil {
		t.Fatalf("the logout confirmation page has no CSRF token: %s", page.body)
	}
	return string(m[1])
}
