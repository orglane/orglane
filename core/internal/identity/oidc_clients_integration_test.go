//go:build integration

package identity_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/identity"
)

// TestDeleteOIDCClient removes a client with everything issued through it:
// pending authorization requests, grants, access and refresh tokens (all
// reference the client with ON DELETE CASCADE), and audits the deletion.
// That is what a managed app's purge needs to release its app-<key> client
// for a fresh install under the same key.
func TestDeleteOIDCClient(t *testing.T) {
	svc, pool := newService(t)
	ctx := context.Background()
	client, secret, err := svc.CreateOIDCClient(ctx, admin, identity.NewOIDCClient{
		ID: "app-git", AppInstance: true, Name: "Forgejo",
		RedirectURIs: []string{"https://git.acme.test/user/oauth2/orglane/callback"},
	})
	if err != nil || secret == "" {
		t.Fatalf("CreateOIDCClient() = %+v, %q, %v", client, secret, err)
	}
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	grant := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `INSERT INTO oidc_grants (id, client_id, person_id) VALUES ($1, $2, $3)`,
		grant, client.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	refresh := sha256.Sum256([]byte("refresh-token"))
	if _, err := pool.Exec(ctx, `INSERT INTO oidc_refresh_tokens
			(token_hash, grant_id, client_id, person_id, scopes, audience, auth_time, expires_at)
		VALUES ($1, $2, $3, $4, '{openid}', '{orglane}', now(), now() + interval '1 hour')`,
		refresh[:], grant, client.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO oidc_access_tokens
			(client_id, person_id, grant_id, scopes, audience, expires_at)
		VALUES ($1, $2, $3, '{openid}', '{orglane}', now() + interval '1 hour')`,
		client.ID, alice.ID, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO oidc_auth_requests
			(client_id, redirect_uri, scopes, response_type, expires_at)
		VALUES ($1, 'https://git.acme.test/user/oauth2/orglane/callback', '{openid}', 'code', now() + interval '1 hour')`,
		client.ID); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteOIDCClient(ctx, admin, client.ID); err != nil {
		t.Fatalf("DeleteOIDCClient(): %v", err)
	}
	for table, column := range map[string]string{
		"oidc_clients":        "id",
		"oidc_grants":         "client_id",
		"oidc_refresh_tokens": "client_id",
		"oidc_access_tokens":  "client_id",
		"oidc_auth_requests":  "client_id",
	} {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+column+" = $1", client.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s rows after the delete = %d", table, n)
		}
	}
	var actorKind, targetKind, targetID string
	if err := pool.QueryRow(ctx, `SELECT actor_kind, target_kind, target_id FROM audit_entries WHERE action = 'oidc_client.deleted'`).
		Scan(&actorKind, &targetKind, &targetID); err != nil {
		t.Fatalf("oidc_client.deleted audit entry: %v", err)
	}
	if actorKind != string(admin.Kind) || targetKind != "oidc_client" || targetID != client.ID {
		t.Errorf("audit entry = %s %s %s", actorKind, targetKind, targetID)
	}

	// A second delete finds nothing; callers that only need the client gone
	// (an app purge) can treat that as done.
	if err := svc.DeleteOIDCClient(ctx, admin, client.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("deleting a missing client = %v, want ErrNotFound", err)
	}
}
