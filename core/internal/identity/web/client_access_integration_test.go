//go:build integration

package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/oidcop"
	"github.com/orglane/orglane/core/internal/store"
)

// The client access check runs where every authorization request completes:
// a refused person goes back to the client with access_denied and no code,
// whether they sign in for the request or already have a session; others
// and other clients are unaffected.
func TestClientAccessCheck(t *testing.T) {
	e := newEnv(t)
	owner := e.setupOwner(e.browser())
	e.registerClient(identity.NewOIDCClient{ID: "app-git", AppInstance: true, Public: true, Native: true})
	e.registerClient(identity.NewOIDCClient{ID: "other", Public: true, Native: true})
	var asked []string
	e.stack.Auth.SetClientAccess(func(_ context.Context, clientID string, personID uuid.UUID) (bool, error) {
		asked = append(asked, clientID)
		return clientID != "app-git" || personID != owner.Person.ID, nil
	})

	b := e.browser()
	p := newPKCE()
	// Signing in for the request: refused.
	r := b.signInScreen(b.authorize(authParams("app-git", p)), ownerPassword)
	if q := clientRedirect(t, r); q.Get("error") != "access_denied" || q.Get("code") != "" || q.Get("state") != "state-123" {
		t.Fatalf("refused sign-in = %v", q)
	}
	// With the session from that sign-in: refused at once too.
	if q := clientRedirect(t, b.authorize(authParams("app-git", newPKCE()))); q.Get("error") != "access_denied" || q.Get("code") != "" {
		t.Fatalf("refused with a session = %v", q)
	}
	// Another client: the session signs in as usual.
	if q := clientRedirect(t, b.authorize(authParams("other", newPKCE()))); q.Get("code") == "" {
		t.Fatalf("other client = %v", q)
	}
	if len(asked) != 3 {
		t.Fatalf("checks = %v, want one per completed request", asked)
	}

	// A failing check fails closed.
	e.stack.Auth.SetClientAccess(func(context.Context, string, uuid.UUID) (bool, error) { return false, context.DeadlineExceeded })
	r = b.authorize(authParams("other", newPKCE()))
	if r.StatusCode != http.StatusInternalServerError || r.Header.Get("Location") != "" {
		t.Fatalf("failing check = %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}

// The decision is enforced again where codes and tokens are issued:
// access withdrawn after the sign-in completed (the browser holds the
// callback, or the client holds the code or a refresh token) yields no code
// and no tokens.
func TestClientAccessAtIssuance(t *testing.T) {
	e := newEnv(t)
	owner := e.setupOwner(e.browser())
	e.registerClient(identity.NewOIDCClient{ID: "app-git", AppInstance: true, Public: true, Native: true, RefreshTokens: true})
	var allowed atomic.Bool
	allowed.Store(true)
	e.stack.Auth.SetClientAccess(func(context.Context, string, uuid.UUID) (bool, error) { return true, nil })
	e.stack.Provider.SetClientAccess(func(_ context.Context, q *store.Queries, clientID string, personID uuid.UUID) (bool, error) {
		if q == nil {
			t.Error("the issuance check got no transaction")
		}
		return clientID != "app-git" || personID != owner.Person.ID || allowed.Load(), nil
	})
	b := e.browser()
	b.signInScreen(b.authorize(authParams("app-git", newPKCE())), ownerPassword)

	// Withdrawn while the browser holds the callback redirect: no code.
	p := newPKCE()
	r := b.req("GET", "/oidc/authorize?"+authParams("app-git", p).Encode(), nil)
	for range 10 {
		loc, err := r.Request.URL.Parse(r.Header.Get("Location"))
		if err != nil || r.StatusCode < 300 || r.StatusCode >= 400 {
			t.Fatalf("no callback redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
		}
		if loc.Path == oidcop.PathCallback {
			allowed.Store(false)
			r = b.req("GET", loc.String(), nil)
			break
		}
		r = b.req("GET", loc.String(), nil)
	}
	if q := clientRedirect(t, r); q.Get("error") != "access_denied" || q.Get("code") != "" {
		t.Fatalf("callback after withdrawal = %v", q)
	}

	// Withdrawn after the code was issued: the exchange fails and spends
	// the code.
	allowed.Store(true)
	p = newPKCE()
	q := clientRedirect(t, b.authorize(authParams("app-git", p)))
	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {q.Get("code")}, "redirect_uri": {redirectURI}, "client_id": {"app-git"}, "code_verifier": {p.verifier}}
	allowed.Store(false)
	if status, tr := b.token(exchange); status != http.StatusBadRequest || tr.Error != "invalid_grant" || tr.AccessToken != "" {
		t.Fatalf("exchange after withdrawal = %d %+v", status, tr)
	}
	allowed.Store(true)
	if status, tr := b.token(exchange); status != http.StatusBadRequest || tr.AccessToken != "" {
		t.Fatalf("the spent code was exchanged = %d %+v", status, tr)
	}

	// Withdrawn while the client holds a refresh token: no new tokens.
	p = newPKCE()
	q = clientRedirect(t, b.authorize(authParams("app-git", p)))
	exchange.Set("code", q.Get("code"))
	exchange.Set("code_verifier", p.verifier)
	status, tr := b.token(exchange)
	if status != http.StatusOK || tr.RefreshToken == "" {
		t.Fatalf("exchange = %d %+v", status, tr)
	}
	allowed.Store(false)
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}, "client_id": {"app-git"}}
	if status, tr := b.token(refresh); status != http.StatusBadRequest || tr.Error != "invalid_grant" || tr.AccessToken != "" {
		t.Fatalf("refresh after withdrawal = %d %+v", status, tr)
	}

	// A failing check denies too.
	allowed.Store(true)
	e.stack.Provider.SetClientAccess(func(context.Context, *store.Queries, string, uuid.UUID) (bool, error) {
		return false, context.DeadlineExceeded
	})
	p = newPKCE()
	r = b.authorize(authParams("app-git", p))
	if loc := r.Header.Get("Location"); strings.Contains(loc, "code=") {
		t.Fatalf("a failing check issued a code: %s", loc)
	}
}

// Client IDs with the app prefix are reserved for app instances.
func TestAppClientPrefixReserved(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"app-git", "APP-git", "App-x"} {
		if _, _, err := e.stack.Identity.CreateOIDCClient(context.Background(), audit.System, identity.NewOIDCClient{ID: id, Name: id, Public: true, Native: true, RedirectURIs: []string{redirectURI}}); err == nil {
			t.Errorf("an operator client %q took the app prefix", id)
		}
	}
	if _, _, err := e.stack.Identity.CreateOIDCClient(context.Background(), audit.System, identity.NewOIDCClient{ID: "mobile2", AppInstance: true, Name: "x", Public: true, Native: true, RedirectURIs: []string{redirectURI}}); err == nil {
		t.Error("an app client without the prefix")
	}
}
