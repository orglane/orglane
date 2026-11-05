//go:build integration

package web_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/identity"
)

// The client access check runs where every authorization request completes:
// a refused person goes back to the client with access_denied and no code,
// whether they sign in for the request or already have a session; others
// and other clients are unaffected.
func TestClientAccessCheck(t *testing.T) {
	e := newEnv(t)
	owner := e.setupOwner(e.browser())
	e.registerClient(identity.NewOIDCClient{ID: "app-git", Public: true, Native: true})
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
