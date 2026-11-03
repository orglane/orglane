//go:build integration

package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/tests/auditfault"
	"github.com/orglane/orglane/tests/webauthntest"
)

// The mutations of package identity are covered in its own
// TestAuditFailureInDatabaseRollsBackMutation. These tests cover those that
// need the full stack: registering a passkey (a WebAuthn ceremony) and the
// OIDC provider's audited writes (package oidcop: grant revocation and
// signing key rotation).

func TestAuditFailureRollsBackPasskeyRegistration(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.setupOwner(e.browser()).Person.ID
	actor := audit.Actor{Kind: audit.ActorPerson, ID: owner.String()}

	ceremony, options, err := e.stack.Identity.BeginPasskeyRegistration(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	var creation struct {
		PublicKey map[string]any `json:"publicKey"`
	}
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &creation); err != nil {
		t.Fatal(err)
	}
	credential, err := webauthntest.New(origin).Create(creation.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}

	// Taking the ceremony and storing the passkey roll back: the same
	// ceremony then completes without the fault.
	auditfault.AssertRollsBack(t, e.pool, auditfault.Mutation{
		Reject: "NEW.action = '" + string(audit.PasskeyAdded) + "'",
		Tables: 3, // webauthn_ceremonies, passkeys, audit_entries
		Run: func(faulty bool) error {
			_, err := e.stack.Identity.FinishPasskeyRegistration(ctx, actor, owner, ceremony, "Laptop", response)
			return auditfault.Expect(faulty, err)
		},
	})
}

// TestAuditFailureRollsBackGrantRevocation covers each way an OIDC grant is
// revoked: reusing a refresh token, reusing a code, and revoking a refresh
// token at the revocation endpoint. The revoked grant and tokens roll back
// with the failed entry. The token endpoint rejects the reused token or code
// either way (zitadel answers invalid_grant for any storage error); the
// revocation endpoint answers with a server error instead of reporting the
// token revoked.
func TestAuditFailureRollsBackGrantRevocation(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	e.setupOwner(b)
	e.registerClient(identity.NewOIDCClient{ID: "mobile", Public: true, Native: true, RefreshTokens: true})
	p := newPKCE()

	// code returns a fresh authorization code; b's session answers at once.
	code := func() string {
		t.Helper()
		return clientRedirect(t, b.authorize(authParams("mobile", p))).Get("code")
	}
	exchangeForm := func(code string) url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "client_id": {"mobile"}, "code_verifier": {p.verifier}}
	}
	refreshForm := func(token string) url.Values {
		return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}, "client_id": {"mobile"}}
	}
	// post sends a token or revocation request and returns the status and
	// the OAuth error code.
	post := func(path string, form url.Values) (int, string) {
		t.Helper()
		r := b.req("POST", path, form, "Origin", "")
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(r.body, &body)
		return r.StatusCode, body.Error
	}
	tokens := func() tokenResponse {
		t.Helper()
		status, tr := b.token(exchangeForm(code()))
		if status != http.StatusOK || tr.RefreshToken == "" {
			t.Fatalf("token exchange = %d %+v", status, tr)
		}
		return tr
	}
	type outcome struct {
		status int
		error  string
	}
	// expect checks the outcome of a request that revokes a grant, with the
	// audit entry failing (faulty) and without.
	expect := func(path string, form url.Values, faulty, ok outcome) func(bool) error {
		return func(isFaulty bool) error {
			want := ok
			if isFaulty {
				want = faulty
			}
			if status, oauthErr := post(path, form); status != want.status || oauthErr != want.error {
				return fmt.Errorf("%s = %d %q, want %d %q", path, status, oauthErr, want.status, want.error)
			}
			return nil
		}
	}
	invalidGrant := outcome{http.StatusBadRequest, "invalid_grant"}
	revoked := "NEW.action = '" + string(audit.OIDCGrantRevoked) + "'"
	const grantTables = 4 // oidc_grants, oidc_refresh_tokens, oidc_access_tokens, audit_entries

	t.Run("refresh token reused", func(t *testing.T) {
		used := tokens().RefreshToken
		if status, tr := b.token(refreshForm(used)); status != http.StatusOK {
			t.Fatalf("refresh = %d %+v", status, tr)
		}
		auditfault.AssertRollsBack(t, e.pool, auditfault.Mutation{
			Reject: revoked, Tables: grantTables,
			Run: expect("/oidc/token", refreshForm(used), invalidGrant, invalidGrant),
		})
	})

	t.Run("code reused", func(t *testing.T) {
		c := code()
		if status, tr := b.token(exchangeForm(c)); status != http.StatusOK {
			t.Fatalf("token exchange = %d %+v", status, tr)
		}
		auditfault.AssertRollsBack(t, e.pool, auditfault.Mutation{
			Reject: revoked, Tables: grantTables,
			Run: expect("/oidc/token", exchangeForm(c), invalidGrant, invalidGrant),
		})
	})

	t.Run("refresh token revoked", func(t *testing.T) {
		form := url.Values{"token": {tokens().RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {"mobile"}}
		auditfault.AssertRollsBack(t, e.pool, auditfault.Mutation{
			Reject: revoked, Tables: grantTables,
			Run: expect("/oidc/revoke", form, outcome{http.StatusInternalServerError, "server_error"}, outcome{http.StatusOK, ""}),
		})
	})
}

// TestAuditFailureRollsBackKeyRotation fails the new key's entry, after the
// active key was retired and audited: the retirement rolls back and the
// active key keeps signing.
func TestAuditFailureRollsBackKeyRotation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	auditfault.AssertRollsBack(t, e.pool, auditfault.Mutation{
		Reject: "NEW.action = '" + string(audit.SigningKeyCreated) + "'",
		Run: func(faulty bool) error {
			return auditfault.Expect(faulty, e.stack.Keys.Rotate(ctx))
		},
	})
	if e.auditCount(audit.SigningKeyRetired) != 1 {
		t.Error("the rotation without the fault did not retire the key exactly once")
	}
}
