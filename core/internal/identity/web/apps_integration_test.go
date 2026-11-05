//go:build integration

package web_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/providers/testprovider"
)

// TestAppsAPIPermissions checks allowed and denied access for every
// provider framework operation: apps.manage (Owner, Admin) manages apps,
// people.manage reads a person's app access, every signed-in person reads
// their own apps, nobody without a session does any of it, and webhooks
// are authenticated by the provider's signature alone.
func TestAppsAPIPermissions(t *testing.T) {
	e := newEnv(t)
	e.startJobs()
	ctx := context.Background()
	owner := e.browser()
	ownerID := e.setupOwner(owner).Person.ID

	adminPerson := e.createMember("adele")
	if err := e.stack.Identity.AssignRole(ctx, audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.signIn("adele@acme.test")
	memberPerson := e.createMember("max")
	member := e.browser()
	member.signIn("max@acme.test")
	anonymous := e.browser()

	inst, err := e.stack.Apps.CreateInstance(ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, State: apps.StateActive, BaseURL: "https://code.acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("api-test-webhook-secret")
	if err := e.stack.Apps.StoreCredential(ctx, audit.System, inst.ID, "webhook_secret", secret); err != nil {
		t.Fatal(err)
	}
	team, err := e.stack.Identity.CreateTeam(ctx, audit.System, identity.NewTeam{Slug: "platform", Name: "Platform"})
	if err != nil {
		t.Fatal(err)
	}
	// Everyone active gets an account (all_people): wait for the jobs.
	waitFor(t, func() bool { _, ok := e.tp.Account(inst.ID, "max"); return ok })

	// Keep rotations in progress (the provider's answers are lost), so the
	// second one deterministically conflicts.
	e.tp.LoseResponses("webhook_secret", 1_000_000)
	base := "/api/v1/apps/instances/" + inst.ID.String()
	ops := []struct {
		method, path string
		body         any
		perm         authz.Permission
		ok           int // the status an allowed caller gets
	}{
		{"GET", "/api/v1/apps/providers", nil, authz.AppsManage, 200},
		{"GET", "/api/v1/apps/instances", nil, authz.AppsManage, 200},
		{"GET", base, nil, authz.AppsManage, 200},
		{"PATCH", base, map[string]any{"displayName": "Code"}, authz.AppsManage, 200},
		{"POST", base + "/setup", nil, authz.AppsManage, 409}, // connected: not supported
		{"GET", base + "/operations", nil, authz.AppsManage, 200},
		{"GET", "/api/v1/apps/instances/" + uuid.NewString() + "/operations", nil, authz.AppsManage, 404},
		{"POST", base + "/credentials/webhook_secret/rotate", nil, authz.AppsManage, 202},
		{"POST", base + "/credentials/api_token/abandon", nil, authz.AppsManage, 409},                                       // no rotation
		{"POST", base + "/credentials/api_token/resolve", map[string]any{"resolution": "discarded"}, authz.AppsManage, 409}, // no rotation
		{"POST", base + "/credentials/api_token/resolve", map[string]any{"resolution": "maybe"}, authz.AppsManage, 400},
		{"GET", "/api/v1/apps/bindings", nil, authz.AppsManage, 200},
		{"PUT", "/api/v1/apps/bindings/code", map[string]any{"instanceId": inst.ID}, authz.AppsManage, 200},
		{"GET", base + "/team-mappings", nil, authz.AppsManage, 200},
		{"PUT", base + "/team-mappings/" + team.ID.String(), map[string]any{"mode": "create"}, authz.AppsManage, 200},
		{"DELETE", base + "/team-mappings/" + team.ID.String(), nil, authz.AppsManage, 204},
		{"GET", base + "/provisioning", nil, authz.AppsManage, 200},
		{"POST", base + "/provisioning/plan", nil, authz.AppsManage, 200},
		{"POST", base + "/provisioning/retry", map[string]any{}, authz.AppsManage, 202},
		{"POST", base + "/provisioning/removal-batches/" + uuid.NewString() + "/confirm", map[string]any{"itemsHash": strings.Repeat("0", 64)}, authz.AppsManage, 404},
		{"GET", "/api/v1/people/" + memberPerson.ID.String() + "/apps", nil, authz.PeopleManage, 200},
		{"GET", "/api/v1/me/apps", nil, authz.Authenticated, 200},
	}
	for _, op := range ops {
		r := anonymous.req(op.method, op.path, op.body)
		if op.method != "GET" {
			// No session: the CSRF-free request is rejected before anything.
			if r.StatusCode != http.StatusUnauthorized && r.StatusCode != http.StatusForbidden {
				t.Errorf("anonymous %s %s = %d", op.method, op.path, r.StatusCode)
			}
		} else {
			expectStatusT(t, r, http.StatusUnauthorized)
		}
		if op.perm != authz.Authenticated {
			expectStatusT(t, member.req(op.method, op.path, op.body), http.StatusForbidden)
		}
		for _, caller := range []*browser{admin, owner} {
			// Each mutation is idempotent here or answers the same, except
			// the rotation (a second one is in progress) and the mapping
			// removal (gone after the first).
			want := op.ok
			if caller == owner && strings.HasSuffix(op.path, "/rotate") {
				want = http.StatusConflict
			}
			if caller == owner && op.method == "DELETE" {
				want = http.StatusNotFound
			}
			expectStatusT(t, caller.req(op.method, op.path, op.body), want)
		}
	}
	// The member sees their own apps; the owner's own list is theirs.
	var mine struct {
		Items []struct {
			InstanceID uuid.UUID `json:"instanceId"`
			HomeURL    string    `json:"homeUrl"`
			Status     string    `json:"status"`
		} `json:"items"`
	}
	r := member.req("GET", "/api/v1/me/apps", nil)
	r.json(t, &mine)
	if len(mine.Items) != 1 || mine.Items[0].InstanceID != inst.ID || mine.Items[0].HomeURL != "https://code.acme.test" {
		t.Fatalf("member apps = %s", r.body)
	}
	// Paging bounds are checked before narrowing (no wrap to a negative
	// OFFSET and a 500).
	expectStatusT(t, owner.req("GET", base+"/provisioning?offset=3000000000", nil), http.StatusBadRequest)
	expectStatusT(t, owner.req("GET", base+"/operations?limit=101", nil), http.StatusBadRequest)
	expectStatusT(t, owner.req("GET", base+"/operations?offset=3000000000", nil), http.StatusBadRequest)
	// Problem details carry a stable code.
	var problem struct {
		Code string `json:"code"`
	}
	r = owner.req("POST", base+"/credentials/webhook_secret/rotate", nil)
	r.json(t, &problem)
	if r.StatusCode != http.StatusConflict || problem.Code != "rotation_in_progress" {
		t.Fatalf("second rotation = %d %s", r.StatusCode, r.body)
	}
	// Credentials appear by name and version only.
	r = owner.req("GET", base, nil)
	if strings.Contains(string(r.body), string(secret)) || !strings.Contains(string(r.body), `"webhook_secret"`) {
		t.Fatalf("instance detail leaks or lacks credentials: %s", r.body)
	}
	// Mutations are audited with the admin as actor.
	for _, action := range []audit.Action{audit.AppInstanceUpdated, audit.AppBindingSet, audit.AppTeamMappingSet, audit.AppTeamMappingRemoved, audit.AppCredentialRotationStarted, audit.ProvisioningRetried} {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM audit_entries WHERE action = $1 AND actor_id = $2`, string(action), adminPerson.ID.String()).Scan(&n); err != nil || n == 0 {
			t.Errorf("%s by the admin: %d entries, %v", action, n, err)
		}
	}
	_ = ownerID

	// Webhooks: public, authenticated by the signature only.
	body := []byte(`{"organization":"acme","action":"opened","id":5,"number":1,"title":"Hello"}`)
	hook := "/api/v1/webhooks/" + inst.ID.String()
	sig := testprovider.Sign(secret, body)
	expectStatusT(t, anonymous.req("POST", hook, body, testprovider.SignatureHeader, hex.EncodeToString(make([]byte, 32)), testprovider.DeliveryHeader, "w-1", testprovider.EventHeader, "pull_request", "Content-Type", "application/json", "Origin", ""), http.StatusUnauthorized)
	expectStatusT(t, anonymous.req("POST", hook, body, testprovider.SignatureHeader, sig, testprovider.DeliveryHeader, "w-1", testprovider.EventHeader, "pull_request", "Content-Type", "application/json", "Origin", ""), http.StatusAccepted)
	expectStatusT(t, anonymous.req("POST", hook, body, testprovider.SignatureHeader, sig, testprovider.DeliveryHeader, "w-2", testprovider.EventHeader, "pull_request", "Content-Type", "application/json", "Origin", ""), http.StatusOK)
	expectStatusT(t, anonymous.req("POST", "/api/v1/webhooks/"+uuid.NewString(), body, testprovider.SignatureHeader, sig, "Content-Type", "application/json", "Origin", ""), http.StatusNotFound)
	big := make([]byte, apps.MaxWebhookBody+1)
	expectStatusT(t, anonymous.req("POST", hook, big, testprovider.SignatureHeader, testprovider.Sign(secret, big), "Content-Type", "application/json", "Origin", ""), http.StatusRequestEntityTooLarge)
	waitFor(t, func() bool {
		var n int
		_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE instance_id = $1`, inst.ID).Scan(&n)
		return n == 1
	})
}

func expectStatusT(t *testing.T, r response, want int) {
	t.Helper()
	if r.StatusCode != want {
		t.Errorf("%s %s = %d, want %d: %s", r.Request.Method, r.Request.URL.Path, r.StatusCode, want, r.body)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// No GitHub API is mocked here: manifest preparation and invalid callbacks do
// not contact GitHub. The real protocol is exercised in providers/github live tests.
func TestGitHubConnectPermissionsAndState(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	ownerID := e.setupOwner(owner).Person.ID
	adminPerson := e.createMember("github-admin")
	if err := e.stack.Identity.AssignRole(t.Context(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.signIn("github-admin@acme.test")
	e.createMember("github-member")
	member := e.browser()
	member.signIn("github-member@acme.test")
	anonymous := e.browser()
	start := "/api/v1/apps/github/connect"
	complete := start + "/complete"
	for _, op := range []struct {
		method, path string
		body         any
	}{
		{"POST", start, map[string]any{"organization": "orglane-test", "key": "github-denied"}},
		{"POST", complete, map[string]any{"state": "ols_unknown", "code": "code"}},
		{"PUT", "/api/v1/apps/instances/" + uuid.NewString() + "/credentials/app_private_key", map[string]any{"value": "fake"}},
	} {
		expectStatusT(t, member.req(op.method, op.path, op.body), http.StatusForbidden)
		r := anonymous.req(op.method, op.path, op.body)
		if r.StatusCode != 401 && r.StatusCode != 403 {
			t.Fatalf("anonymous status %d", r.StatusCode)
		}
	}
	for i, caller := range []*browser{owner, admin} {
		key := []string{"github-owner", "github-admin"}[i]
		r := caller.req("POST", start, map[string]any{"organization": "orglane-test", "key": key})
		expectStatusT(t, r, 200)
		var reply struct {
			InstanceID uuid.UUID         `json:"instanceId"`
			URL        string            `json:"url"`
			Form       map[string]string `json:"form"`
		}
		r.json(t, &reply)
		u, err := url.Parse(reply.URL)
		if err != nil {
			t.Fatal(err)
		}
		state := u.Query().Get("state")
		if !strings.HasPrefix(state, "ols_") || reply.Form["manifest"] == "" {
			t.Fatal("missing manifest/state")
		}
		var stored []byte
		var person uuid.UUID
		var step string
		if err = e.pool.QueryRow(t.Context(), "SELECT state_hash,person_id,step FROM app_oauth_states WHERE instance_id=$1 AND consumed_at IS NULL", reply.InstanceID).Scan(&stored, &person, &step); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored, crypto.HashToken(state)) || bytes.Equal(stored, []byte(state)) || step != "create_app" || (i == 0 && person != ownerID) {
			t.Fatal("state not hashed and bound to starter")
		}
		callback := map[string]any{"state": state, "installationId": "789"} // wrong step: rejected before any external request
		other := admin
		if i == 1 {
			other = owner
		}
		expectStatusT(t, other.req("POST", complete, callback), 409)
		// Same person, another session must not consume the starter's state.
		another := e.browser()
		if i == 0 {
			another.signIn(ownerEmail)
		} else {
			another.signIn("github-admin@acme.test")
		}
		expectStatusT(t, another.req("POST", complete, callback), 409)
		var consumed *time.Time
		if err = e.pool.QueryRow(t.Context(), "SELECT consumed_at FROM app_oauth_states WHERE state_hash=$1", crypto.HashToken(state)).Scan(&consumed); err != nil || consumed != nil {
			t.Fatal("foreign browser consumed state", err)
		}
		expectStatusT(t, caller.req("POST", complete, callback), 409)
		expectStatusT(t, caller.req("POST", complete, map[string]any{"state": state, "code": "unused"}), 409)
		// Resume creates a new state, invalidates older states, and keeps the instance.
		resumed := caller.req("POST", start, map[string]any{"organization": "orglane-test", "key": key, "instanceId": reply.InstanceID})
		expectStatusT(t, resumed, 200)
		var next struct {
			URL string `json:"url"`
		}
		resumed.json(t, &next)
		nu, _ := url.Parse(next.URL)
		nextState := nu.Query().Get("state")
		if _, err = e.pool.Exec(t.Context(), "UPDATE app_oauth_states SET expires_at=now()-interval '1 second',created_at=now()-interval '2 hours' WHERE state_hash=$1", crypto.HashToken(nextState)); err != nil {
			t.Fatal(err)
		}
		expectStatusT(t, caller.req("POST", complete, map[string]any{"state": nextState, "code": "unused"}), 409)
		expectStatusT(t, caller.req("POST", start, map[string]any{"organization": "another-org", "key": key, "instanceId": reply.InstanceID}), 409)
		expectStatusT(t, caller.req("PATCH", "/api/v1/apps/instances/"+reply.InstanceID.String(), map[string]any{"settings": map[string]any{"app_id": "unverified"}}), 400)
		expectStatusT(t, caller.req("PUT", "/api/v1/apps/instances/"+uuid.NewString()+"/credentials/app_private_key", map[string]any{"value": "fake"}), 404)
	}
	expectStatusT(t, owner.req("POST", start, map[string]any{"organization": "https://evil.test", "key": "github-bad"}), 400)
}

// This checks the core's encrypted upload staging and permissions against real
// PostgreSQL. No background worker starts; an explicit real-adapter call rejects
// invalid PEM locally, without substituting or contacting a GitHub API. Successful
// provider-side verification still belongs to real-GitHub acceptance.
func TestGitHubPrivateKeyUploadStaysStaged(t *testing.T) {
	e := newEnv(t)
	owner := e.browser()
	e.setupOwner(owner)
	adminPerson := e.createMember("key-admin")
	if err := e.stack.Identity.AssignRole(t.Context(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.signIn("key-admin@acme.test")
	rc, err := river.NewClient(riverpgxv5.New(e.pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.stack.Apps.SetQueue(rc) // insert-only; provider calls never run in this staging test
	oldValue := []byte("old-private-key-staging-fixture")
	replacement := "replacement-private-key-staging-fixture"
	for i, caller := range []*browser{owner, admin} {
		key := []string{"github-owner-key", "github-admin-key"}[i]
		inst, err := e.stack.Apps.CreateInstance(t.Context(), audit.System, apps.NewInstance{Provider: "github", Key: key, State: apps.StateActive, Settings: map[string]any{"organization": "orglane-test", "organization_id": "123", "app_id": "456"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = e.stack.Apps.StoreCredential(t.Context(), audit.System, inst.ID, "app_private_key", oldValue); err != nil {
			t.Fatal(err)
		}
		endpoint := "/api/v1/apps/instances/" + inst.ID.String() + "/credentials/app_private_key"
		expectStatusT(t, caller.req("PUT", endpoint, map[string]any{"value": replacement}), 202)
		expectStatusT(t, caller.req("PUT", endpoint, map[string]any{"value": replacement}), 409)
		var active, pending int
		var ciphertext []byte
		if err = e.pool.QueryRow(t.Context(), "SELECT count(*) FILTER(WHERE state='active'),count(*) FILTER(WHERE state='pending') FROM app_credentials WHERE instance_id=$1 AND name='app_private_key'", inst.ID).Scan(&active, &pending); err != nil {
			t.Fatal(err)
		}
		if active != 1 || pending != 1 {
			t.Fatalf("upload retired the active key before verification: active=%d pending=%d", active, pending)
		}
		if err = e.pool.QueryRow(t.Context(), "SELECT ciphertext FROM app_credentials WHERE instance_id=$1 AND name='app_private_key' AND state='pending'", inst.ID).Scan(&ciphertext); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ciphertext, []byte(replacement)) {
			t.Fatal("replacement key was stored as plaintext")
		}
		detail := caller.req("GET", "/api/v1/apps/instances/"+inst.ID.String(), nil)
		expectStatusT(t, detail, 200)
		if bytes.Contains(detail.body, oldValue) || bytes.Contains(detail.body, []byte(replacement)) {
			t.Fatal("credentials leaked through instance detail")
		}
		assertRotation := func(want string) {
			t.Helper()
			r := caller.req("GET", "/api/v1/apps/instances/"+inst.ID.String(), nil)
			expectStatusT(t, r, 200)
			var result struct {
				Rotations []struct {
					Name, State string
					Version     int
				} `json:"credentialRotations"`
			}
			if err := json.Unmarshal(r.body, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Rotations) != 1 || result.Rotations[0].Name != "app_private_key" || result.Rotations[0].Version != 2 || result.Rotations[0].State != want {
				t.Fatalf("safe rotation outcome after reload = %s, want %s", r.body, want)
			}
			if bytes.Contains(r.body, oldValue) || bytes.Contains(r.body, []byte(replacement)) {
				t.Fatal("rotation result leaked a key")
			}
		}
		assertRotation("accepted")
		if _, err = e.pool.Exec(t.Context(), "UPDATE app_credentials SET state='uncertain' WHERE instance_id=$1 AND version=2", inst.ID); err != nil {
			t.Fatal(err)
		}
		assertRotation("verifying")
		// Real adapter rejects invalid PEM locally; no provider transport is mocked.
		if err = e.stack.Apps.RunRotateCredential(t.Context(), apps.RotateCredentialArgs{InstanceID: inst.ID, Name: "app_private_key", Version: 2}); err != nil {
			t.Fatal(err)
		}
		assertRotation("failed")
		// The terminal outcome must survive ciphertext retention cleanup and reload.
		if _, err = e.pool.Exec(t.Context(), "DELETE FROM app_credentials WHERE instance_id=$1 AND state='discarded'", inst.ID); err != nil {
			t.Fatal(err)
		}
		assertRotation("failed")
		if err = e.pool.QueryRow(t.Context(), "SELECT count(*) FROM app_credentials WHERE instance_id=$1 AND version=1 AND state='active'", inst.ID).Scan(&active); err != nil || active != 1 {
			t.Fatalf("rejection replaced the working key: %d %v", active, err)
		}
		// Cleanup permits version reuse; a new acceptance must supersede the old failure.
		expectStatusT(t, caller.req("PUT", endpoint, map[string]any{"value": replacement}), 202)
		assertRotation("accepted")

	}
	var args string
	if err = e.pool.QueryRow(t.Context(), "SELECT string_agg(args::text,',') FROM river_job WHERE kind='orglane.apps.rotate_credential'").Scan(&args); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(args, replacement) || strings.Contains(args, string(oldValue)) {
		t.Fatal("credential value was put in queue arguments")
	}
}
