//go:build integration

package apps_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/testprovider"
)

// TestStoreConstraints checks the constraints of the provider framework's
// tables that the service relies on.
func TestStoreConstraints(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	alice, bob := h.person("alice"), h.person("bob")
	q := store.New(h.pool)
	link := func(person, externalID string) error {
		p := alice
		if person == "bob" {
			p = bob
		}
		_, err := q.InsertAccountLink(h.ctx, store.InsertAccountLinkParams{PersonID: p.ID, Provider: "github", Realm: "github.com", ExternalID: externalID, Login: "login-" + externalID, Method: "oauth"})
		return err
	}
	// One verified link per account and per person and realm.
	if err := link("alice", "1"); err != nil {
		t.Fatal(err)
	}
	if err := link("bob", "1"); err == nil {
		t.Fatal("two people verified the same account")
	}
	if err := link("alice", "2"); err == nil {
		t.Fatal("a person has two verified accounts in one realm")
	}
	l, err := q.GetVerifiedAccountLink(h.ctx, store.GetVerifiedAccountLinkParams{PersonID: alice.ID, Provider: "github", Realm: "github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.RevokeAccountLink(h.ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if err := link("bob", "1"); err != nil {
		t.Fatalf("a revoked link still blocks the account: %v", err)
	}
	if _, err := h.pool.Exec(h.ctx, `UPDATE account_links SET state = 'revoked' WHERE id = $1 AND revoked_at IS NULL`, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO account_links (person_id, provider, realm, external_id, login, method, state) VALUES ($1, 'github', 'x', '9', 'x', 'oauth', 'revoked')`, alice.ID); err == nil {
		t.Fatal("a revoked link without revoked_at was accepted")
	}

	// Single-use OAuth state, bound to the session and the person.
	sess := h.session(alice)
	state := crypto.RandomToken("ols_")
	if _, err := q.InsertAppOAuthState(h.ctx, store.InsertAppOAuthStateParams{ID: newID(), Kind: "link", StateHash: crypto.HashToken(state), InstanceID: inst.ID, PersonID: alice.ID, SessionID: sess, ExpiresAt: time.Now().Add(10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	consume := func(person, session [16]byte) error {
		_, err := q.ConsumeAppOAuthState(h.ctx, store.ConsumeAppOAuthStateParams{StateHash: crypto.HashToken(state), Kind: "link", SessionID: session, PersonID: person})
		return err
	}
	if err := consume(bob.ID, sess); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another person consumed the state: %v", err)
	}
	if err := consume(alice.ID, h.session(alice)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another session consumed the state: %v", err)
	}
	if err := consume(alice.ID, sess); err != nil {
		t.Fatal(err)
	}
	if err := consume(alice.ID, sess); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the state was used twice: %v", err)
	}
	h.exec(`UPDATE app_oauth_states SET created_at = now() - interval '2 days', expires_at = now() - interval '25 hours'`)
	if r, err := h.svc.Cleanup(h.ctx); err != nil || r.OAuthStates != 1 {
		t.Fatalf("cleanup = %+v, %v", r, err)
	}

	// At most one candidate per credential; one default binding per
	// capability; events reference instances without cascading.
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO app_credentials (instance_id, name, version, ciphertext, state) VALUES ($1, 'x', 1, '\x00', 'pending'), ($1, 'x', 2, '\x00', 'uncertain')`, inst.ID); err == nil {
		t.Fatal("two candidates of one credential")
	}
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO app_bindings (capability, instance_id, is_default) VALUES ('code', $1, true)
		ON CONFLICT (capability, instance_id) DO UPDATE SET is_default = true`, inst.ID); err != nil {
		t.Fatal(err)
	}
	second := h.svcInstance("second")
	if _, err := h.pool.Exec(h.ctx, `UPDATE app_bindings SET is_default = true WHERE instance_id = $1`, second); err == nil {
		t.Fatal("two default bindings for one capability")
	}
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO events (type, schema_version, instance_id, provider, dedupe_key, occurred_at, received_at, actor, subject)
		VALUES ('code.pull_request.opened', 1, $1, 'testprovider', 'k', now(), now(), '{}', '{}')`, inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO events (type, schema_version, instance_id, provider, dedupe_key, occurred_at, received_at, actor, subject)
		VALUES ('code.pull_request.opened', 1, $1, 'testprovider', 'k', now(), now(), '{}', '{}')`, inst.ID); err == nil {
		t.Fatal("two events with one dedupe key")
	}
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO events (type, schema_version, instance_id, provider, dedupe_key, occurred_at, received_at, actor, subject)
		VALUES ('pull_request', 1, $1, 'testprovider', 'k2', now(), now(), '{}', '{}')`, inst.ID); err == nil {
		t.Fatal("an event type without capability prefix was accepted")
	}
	if _, err := h.pool.Exec(h.ctx, `DELETE FROM app_instances WHERE id = $1`, inst.ID); err == nil {
		t.Fatal("an instance with events was deleted")
	}
	if _, err := h.pool.Exec(h.ctx, `TRUNCATE events`); err == nil {
		t.Fatal("events can be truncated")
	}
	// Results need a step exactly when pending an employee action.
	if _, err := h.pool.Exec(h.ctx, `INSERT INTO provisioning_results (instance_id, person_id, status, access) VALUES ($1, $2, 'pending_employee_action', true)`, inst.ID, alice.ID); err == nil {
		t.Fatal("pending_employee_action without a step")
	}
}

// session creates a browser session for a person.
func (h *harness) session(p store.Person) [16]byte {
	h.t.Helper()
	var id [16]byte
	if err := h.pool.QueryRow(h.ctx, `WITH d AS (INSERT INTO devices (person_id, name, platform) VALUES ($1, 'Browser', 'web') RETURNING id, person_id)
		INSERT INTO sessions (person_id, device_id, token_hash, expires_at) SELECT person_id, id, $2, now() + interval '1 day' FROM d RETURNING id`,
		p.ID, crypto.HashToken(crypto.RandomToken("ses_"))).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) svcInstance(key string) [16]byte {
	h.t.Helper()
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, Key: key, State: apps.StateActive})
	if err != nil {
		h.t.Fatal(err)
	}
	return inst.ID
}
