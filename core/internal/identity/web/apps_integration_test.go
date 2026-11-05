//go:build integration

package web_test

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
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
		{"POST", base + "/credentials/api_token/abandon", nil, authz.AppsManage, 409}, // no rotation
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
		{"GET", base + "/open?to=%2Facme%2Frobot%3Ftab%3D1", nil, authz.Authenticated, 200},
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
	// Deep links: the target under the instance's origin, for every
	// signed-in person; anything but a safe relative path is refused, and
	// an adapter that links elsewhere is an error, never a redirect.
	var link struct {
		URL string `json:"url"`
	}
	r = member.req("GET", base+"/open?to=%2Facme%2Frobot%3Ftab%3D1", nil)
	r.json(t, &link)
	if link.URL != "https://code.acme.test/acme/robot?tab=1" {
		t.Fatalf("deep link = %s", r.body)
	}
	for _, bad := range []string{"https%3A%2F%2Fevil.example%2F", "%2F%2Fevil.example%2F", "%2Fa%5Cb", "%2Fa%20b", "acme", "%2Fa%0Ab"} {
		expectStatusT(t, member.req("GET", base+"/open?to="+bad, nil), http.StatusBadRequest)
	}
	expectStatusT(t, member.req("GET", "/api/v1/apps/instances/"+uuid.NewString()+"/open?to=%2F", nil), http.StatusNotFound)
	e.tp.LinkToOrigin(&url.URL{Scheme: "https", Host: "evil.example"})
	expectStatusT(t, member.req("GET", base+"/open?to=%2F", nil), http.StatusInternalServerError)
	e.tp.LinkToOrigin(nil)
	// Only active or degraded instances open.
	connecting, err := e.stack.Apps.CreateInstance(ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, Key: "pending", BaseURL: "https://pending.acme.test"})
	if err != nil {
		t.Fatal(err)
	}
	expectStatusT(t, member.req("GET", "/api/v1/apps/instances/"+connecting.ID.String()+"/open?to=%2F", nil), http.StatusConflict)
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
