//go:build integration

package apps_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
)

// forgejoSite is a real Forgejo installed by the real agent, set up by the
// core (RunSetup: Setup and the organization hook).
type forgejoSite struct {
	h     *harness
	b     *managedtest.Bundle
	inst  apps.Instance
	admin managedtest.Auth
	// faults injects failures into the core's requests to Forgejo.
	faults *managedtest.Faults
}

func startForgejoSite(t *testing.T) *forgejoSite {
	t.Helper()
	b := managedtest.StartBundle(t)
	registry, err := packages.NewRegistry(forgejopkg.FS)
	if err != nil {
		t.Fatal(err)
	}
	agentClient := startAgent(t, b, registry)
	httpClient, faults := managedtest.WithFaults(b.Client())
	h := newHarnessWith(t, apps.Config{HTTPClient: httpClient, Packages: registry}, forgejo.New())
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, BaseURL: "http://" + managedtest.ForgejoHost,
		Settings: map[string]any{forgejo.SettingOrganization: "orglane"},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := h.svc.EnvForTest(inst)
	secrets := map[string]string{}
	lifecycle, _ := inst.Adapter.Lifecycle()
	inputs, err := lifecycle.Inputs(h.ctx, env, inst.View())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range inputs.Secrets {
		v, err := env.Credential(h.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		secrets[name] = string(v)
	}
	project := packages.InstanceProject(b.Project, inst.Key)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", project, "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
	b.Routes(t, managedtest.ForgejoRoutes(inst.Key)...)
	spec := managedtest.ForgejoSpec(inst.ID, 1, managedtest.FreePort(t), secrets, agentapi.StateRunning)
	for k, v := range inputs.Settings {
		spec.Settings[k] = v
	}
	op, _, err := agentClient.Apply(t.Context(), inst.Key, spec)
	if err != nil {
		t.Fatal(err)
	}
	managedtest.Eventually(t, 5*time.Minute, "the install finishes", func() bool {
		op, err = agentClient.Operation(t.Context(), op.ID)
		return err == nil && op.Terminal()
	})
	if op.State != agentapi.OpSucceeded {
		t.Fatalf("install: %+v", op)
	}
	if err := h.svc.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatalf("setup: %v", err)
	}
	inst, err = h.svc.Instance(h.ctx, inst.ID)
	if err != nil || inst.State != apps.StateActive {
		t.Fatalf("after setup: %v, %v", inst.State, err)
	}
	return &forgejoSite{h: h, b: b, inst: inst, faults: faults,
		admin: managedtest.Auth{User: forgejo.AdminUsername, Password: secrets[forgejo.CredentialAdminPassword]}}
}

// call sends an API request as auth and fails on a non-2xx answer.
func (s *forgejoSite) call(t *testing.T, auth managedtest.Auth, method, path string, body any) []byte {
	t.Helper()
	code, out := managedtest.Request(t, s.b.Client(), method, managedtest.ForgejoHost, path, auth, body)
	if code < 200 || code > 299 {
		t.Fatalf("%s %s: %d %s", method, path, code, out)
	}
	return out
}

type orgHook struct {
	ID     int64             `json:"id"`
	Active bool              `json:"active"`
	Config map[string]string `json:"config"`
	Events []string          `json:"events"`
}

// ourHooks are the organization's hooks to the instance's webhook URL.
func (s *forgejoSite) ourHooks(t *testing.T) []orgHook {
	t.Helper()
	var hooks []orgHook
	if err := json.Unmarshal(s.call(t, s.admin, http.MethodGet, "/api/v1/orgs/orglane/hooks", nil), &hooks); err != nil {
		t.Fatal(err)
	}
	target := s.h.svc.WebhookURL(s.inst.ID).String()
	return slices.DeleteFunc(hooks, func(h orgHook) bool {
		base, _, _ := strings.Cut(h.Config["url"], "?") // the secret's mark
		return base != target
	})
}

// recorded is a delivery as Forgejo sent it: its request headers and the
// exact signed body.
type recorded struct {
	id      int
	event   string
	headers http.Header
	body    []byte
}

// deliveries returns the hook deliveries after id, once Forgejo has
// attempted all want of them and recorded their requests. Forgejo records every attempt with its request
// (headers, signature and body); the attempts themselves fail, as the
// workspace of this test is not reachable from Forgejo. The test hands the
// recorded requests to the core as the endpoint would.
func (s *forgejoSite) deliveries(t *testing.T, after, want int) []recorded {
	t.Helper()
	var out []recorded
	managedtest.Eventually(t, time.Minute, "Forgejo attempts the deliveries", func() bool {
		// Forgejo marks a task delivered before it sends it, and records the
		// request afterwards: only tasks with a recorded request count.
		rows := s.b.Psql(t, "svc_git", "select row_to_json(t) from (select id, event_type, request_content from hook_task where id > "+
			strconv.Itoa(after)+" and is_delivered and coalesce(request_content, '') <> '' order by id) t")
		out = nil
		for _, line := range strings.Split(rows, "\n") {
			if line == "" {
				continue
			}
			var row struct {
				ID             int    `json:"id"`
				EventType      string `json:"event_type"`
				RequestContent string `json:"request_content"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatal(err)
			}
			var req struct {
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
			}
			if err := json.Unmarshal([]byte(row.RequestContent), &req); err != nil {
				t.Fatal(err)
			}
			h := http.Header{}
			for k, v := range req.Headers {
				h.Set(k, v)
			}
			out = append(out, recorded{id: row.ID, event: row.EventType, headers: h, body: []byte(req.Body)})
		}
		return len(out) >= want
	})
	if len(out) != want {
		t.Fatalf("%d deliveries, want %d", len(out), want)
	}
	return out
}

func (s *forgejoSite) receive(t *testing.T, d recorded) apps.WebhookReceipt {
	t.Helper()
	r, err := s.h.svc.ReceiveWebhook(s.h.ctx, s.inst.ID, d.headers, d.body)
	if err != nil {
		t.Fatalf("delivery %d (%s): %v", d.id, d.event, err)
	}
	return r
}

type storedEvent struct {
	Type, DedupeKey string
	Targets         []provider.Target
}

func (s *forgejoSite) events(t *testing.T) []storedEvent {
	t.Helper()
	rows, err := s.h.pool.Query(s.h.ctx, `SELECT type, dedupe_key, targets FROM events WHERE instance_id = $1 ORDER BY dedupe_key`, s.inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []storedEvent
	for rows.Next() {
		var e storedEvent
		var raw []byte
		if err := rows.Scan(&e.Type, &e.DedupeKey, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &e.Targets); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (s *forgejoSite) lastTask(t *testing.T) int {
	t.Helper()
	n, err := strconv.Atoi(s.b.Psql(t, "svc_git", "select coalesce(max(id), 0) from hook_task"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestForgejoWebhooks: the core creates the organization hook during Setup;
// a pull request opened with a mention, a review request, comments and
// reviews with mentions, and a push in a real Forgejo become normalized
// events, each stored once; redeliveries are duplicates; bad signatures are
// refused; a deleted or deactivated hook is repaired by the daily job; a
// rotated webhook secret signs new deliveries.
func TestForgejoWebhooks(t *testing.T) {
	if managedtest.RunAsRoot(t) {
		return
	}
	s := startForgejoSite(t)
	h := s.h

	// Setup created exactly one active hook for the instance.
	hooks := s.ourHooks(t)
	if len(hooks) != 1 || !hooks[0].Active || hooks[0].Config["content_type"] != "json" {
		t.Fatalf("hooks after setup: %+v", hooks)
	}

	// Activity in a repository of the organization.
	people := map[string]managedtest.Auth{}
	for _, u := range []string{"bob", "carol"} {
		password := "test-password-" + u
		s.call(t, s.admin, http.MethodPost, "/api/v1/admin/users", map[string]any{"username": u, "email": u + "@acme.test", "password": password, "must_change_password": false})
		people[u] = managedtest.Auth{User: u, Password: password}
	}
	s.call(t, s.admin, http.MethodPost, "/api/v1/orgs/orglane/repos", map[string]any{"name": "app", "auto_init": true, "default_branch": "main"})
	for _, u := range []string{"bob", "carol"} {
		s.call(t, s.admin, http.MethodPut, "/api/v1/repos/orglane/app/collaborators/"+u, map[string]any{"permission": "write"})
	}
	start := s.lastTask(t)
	s.call(t, s.admin, http.MethodPost, "/api/v1/repos/orglane/app/contents/feature.txt", map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte("hello\n")), "branch": "main", "new_branch": "feature", "message": "add feature",
	})
	s.call(t, s.admin, http.MethodPost, "/api/v1/repos/orglane/app/pulls", map[string]any{
		"head": "feature", "base": "main", "title": "Add feature", "body": "Please look @bob, not `@nobody`\n```\n@carol\n```",
	})
	s.call(t, s.admin, http.MethodPost, "/api/v1/repos/orglane/app/pulls/1/requested_reviewers", map[string]any{"reviewers": []string{"carol"}})
	s.call(t, s.admin, http.MethodPost, "/api/v1/repos/orglane/app/issues/1/comments", map[string]any{"body": "@carol ping, and @orglane-admin"})
	s.call(t, people["carol"], http.MethodPost, "/api/v1/repos/orglane/app/pulls/1/reviews", map[string]any{"event": "APPROVED", "body": "LGTM @bob"})
	s.call(t, s.admin, http.MethodPost, "/api/v1/repos/orglane/app/issues", map[string]any{"title": "Bug", "body": "cc @carol"})
	s.call(t, s.admin, http.MethodPost, "/api/v1/repos/orglane/app/issues/2/comments", map[string]any{"body": "@bob issue ping"})

	// push, opened, review request, comment, approval, issue comment (issue
	// openings are not subscribed).
	deliveries := s.deliveries(t, start, 6)
	for _, d := range deliveries {
		if r := s.receive(t, d); r.Duplicate {
			t.Fatalf("delivery %d (%s) taken as a duplicate", d.id, d.event)
		}
	}
	h.drain()
	if n := h.count(`SELECT count(*) FROM webhook_deliveries WHERE instance_id = $1 AND status = 'processed'`, s.inst.ID); n != len(deliveries) {
		t.Fatalf("%d processed deliveries, want %d: %v", n, len(deliveries),
			h.count(`SELECT count(*) FROM webhook_deliveries WHERE status <> 'processed'`))
	}
	got := map[string][]string{}
	for _, e := range s.events(t) {
		var ts []string
		for _, tg := range e.Targets {
			ts = append(ts, tg.Login+"/"+tg.Reason)
		}
		got[e.Type] = append(got[e.Type], strings.Join(ts, ","))
	}
	want := map[string][]string{
		forgejo.EventBranchPushed:      {""},
		forgejo.EventPullRequestOpened: {""},
		forgejo.EventReviewRequested:   {"carol/review_requested"},
		// the description, the approval and the issue comment mention bob
		// (not the inline code or the fenced block), the pull request comment
		// carol (not its author)
		forgejo.EventMention: {"bob/mentioned", "bob/mentioned", "bob/mentioned", "carol/mentioned"},
	}
	for typ, w := range want {
		slices.Sort(got[typ])
		if !slices.Equal(got[typ], w) {
			t.Errorf("%s: %q, want %q", typ, got[typ], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("event types %v", got)
	}
	stored := len(s.events(t))

	// Redeliveries: the same signed bytes, with Forgejo's headers or another
	// delivery ID, are duplicates: no delivery, no job, no event.
	for i, d := range deliveries {
		again := d.headers.Clone()
		if i%2 == 1 {
			again.Set(forgejo.DeliveryHeader, "00000000-0000-4000-8000-00000000000"+strconv.Itoa(i))
		}
		if r := s.receive(t, recorded{id: d.id, event: d.event, headers: again, body: d.body}); !r.Duplicate {
			t.Fatalf("redelivery of %d (%s) not a duplicate", d.id, d.event)
		}
	}
	h.drain()
	if n := len(s.events(t)); n != stored {
		t.Fatalf("%d events after redeliveries, want %d", n, stored)
	}

	// Bad signatures are refused and nothing is stored.
	before := h.count(`SELECT count(*) FROM webhook_deliveries`)
	push := deliveries[0]
	forged := append([]byte(nil), push.body...)
	forged[len(forged)-2] ^= 1
	wrongKey := push.headers.Clone()
	wrongKey.Set(forgejo.SignatureHeader, strings.Repeat("ab", 32))
	noSig := push.headers.Clone()
	noSig.Del(forgejo.SignatureHeader)
	for name, c := range map[string]struct {
		h    http.Header
		body []byte
	}{"changed body": {push.headers, forged}, "other key": {wrongKey, push.body}, "no signature": {noSig, push.body}} {
		if _, err := h.svc.ReceiveWebhook(h.ctx, s.inst.ID, c.h, c.body); !errors.Is(err, apps.ErrWebhookSignature) {
			t.Errorf("%s: %v, want a refused signature", name, err)
		}
	}
	if n := h.count(`SELECT count(*) FROM webhook_deliveries`); n != before {
		t.Fatalf("%d deliveries stored from bad signatures", n-before)
	}

	// Repair: a deleted hook comes back with the daily job, and so does a
	// deactivated one; the repaired hook signs with the active secret.
	s.call(t, s.admin, http.MethodDelete, "/api/v1/orgs/orglane/hooks/"+strconv.FormatInt(hooks[0].ID, 10), nil)
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	hooks = s.ourHooks(t)
	if len(hooks) != 1 || !hooks[0].Active {
		t.Fatalf("after deleting the hook: %+v", hooks)
	}
	s.call(t, s.admin, http.MethodPatch, "/api/v1/orgs/orglane/hooks/"+strconv.FormatInt(hooks[0].ID, 10), map[string]any{"active": false})
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if hooks = s.ourHooks(t); len(hooks) != 1 || !hooks[0].Active {
		t.Fatalf("after deactivating the hook: %+v", hooks)
	}
	start = s.lastTask(t)
	s.call(t, people["bob"], http.MethodPost, "/api/v1/repos/orglane/app/issues/1/comments", map[string]any{"body": "after repair @carol"})
	if r := s.receive(t, s.deliveries(t, start, 1)[0]); r.Duplicate {
		t.Fatal("the delivery after the repair is a duplicate")
	}

	// Rotation: the new secret is set on the hook and signs what follows.
	version, err := h.svc.RotateCredential(h.ctx, audit.System, s.inst.ID, forgejo.CredentialWebhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: s.inst.ID, Name: forgejo.CredentialWebhookSecret, Version: version}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if state := h.count(`SELECT count(*) FROM app_credentials WHERE instance_id = $1 AND name = $2 AND version = $3 AND state = 'active'`,
		s.inst.ID, forgejo.CredentialWebhookSecret, version); state != 1 {
		t.Fatal("the rotated secret is not active")
	}
	// The hook was replaced, not added to.
	if after := s.ourHooks(t); len(after) != 1 || after[0].ID == hooks[0].ID || !after[0].Active {
		t.Fatalf("hooks after the rotation: %+v (before: %d)", after, hooks[0].ID)
	}
	start = s.lastTask(t)
	s.call(t, people["carol"], http.MethodPost, "/api/v1/repos/orglane/app/issues/1/comments", map[string]any{"body": "rotated @bob"})
	rotated := s.deliveries(t, start, 1)[0]
	newOnly, err := h.svc.EnvForTest(s.inst).Credential(h.ctx, forgejo.CredentialWebhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := forgejo.New().Verify(h.ctx, fixedSecret{Env: h.svc.EnvForTest(s.inst), value: newOnly}, s.inst.View(), rotated.headers, rotated.body); err != nil {
		t.Fatalf("the delivery after the rotation is not signed with the new secret: %v", err)
	}
	if r := s.receive(t, rotated); r.Duplicate {
		t.Fatal("the delivery after the rotation is a duplicate")
	}

	// A rotation whose answer is lost: Forgejo created the candidate's hook,
	// the core never heard. The daily repair leaves both hooks alone while
	// the candidate is uncertain (it would otherwise put the active secret
	// back and delete the candidate's hook); a delivery signed with the
	// candidate proves it by use; the next repair keeps only its hook.
	lost, err := h.svc.RotateCredential(h.ctx, audit.System, s.inst.ID, forgejo.CredentialWebhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`DELETE FROM river_job WHERE kind = $1`, apps.RotateCredentialArgs{}.Kind()) // no retry in this scenario
	s.faults.Drop(http.MethodPost, "/api/v1/orgs/orglane/hooks")
	if err := h.svc.RunRotateCredential(h.ctx, apps.RotateCredentialArgs{InstanceID: s.inst.ID, Name: forgejo.CredentialWebhookSecret, Version: lost}); err == nil {
		t.Fatal("a lost answer was taken as an outcome")
	}
	credState := func(v int32) string {
		var st string
		if err := h.pool.QueryRow(h.ctx, `SELECT state FROM app_credentials WHERE instance_id = $1 AND name = $2 AND version = $3`,
			s.inst.ID, forgejo.CredentialWebhookSecret, v).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if credState(lost) != "uncertain" || len(s.ourHooks(t)) != 2 {
		t.Fatalf("after the lost answer: %s, %d hooks", credState(lost), len(s.ourHooks(t)))
	}
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if n := len(s.ourHooks(t)); n != 2 {
		t.Fatalf("the repair changed the hooks during an uncertain rotation: %d hooks", n)
	}
	start = s.lastTask(t)
	s.call(t, people["bob"], http.MethodPost, "/api/v1/repos/orglane/app/issues/1/comments", map[string]any{"body": "both hooks @carol"})
	for _, d := range s.deliveries(t, start, 2) {
		s.receive(t, d) // the same body from both hooks: the second is a duplicate
	}
	if credState(lost) != "active" {
		t.Fatalf("candidate = %s, want active (proven by use)", credState(lost))
	}
	// The lost execution's lease still holds (Forgejo may still act on it):
	// the repair waits for it, at most one execution per credential.
	if err := h.svc.RunEnsureWebhooks(h.ctx, apps.EnsureWebhooksArgs{InstanceID: s.inst.ID}); err == nil || len(s.ourHooks(t)) != 2 {
		t.Fatalf("a repair ran while the lost execution's lease held: %v, %d hooks", err, len(s.ourHooks(t)))
	}
	expireLeases(h, s.inst)
	if err := h.svc.RunDriftCheck(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if n := len(s.ourHooks(t)); n != 1 {
		t.Fatalf("%d hooks after the repair, want the candidate's only", n)
	}
	start = s.lastTask(t)
	s.call(t, people["carol"], http.MethodPost, "/api/v1/repos/orglane/app/issues/1/comments", map[string]any{"body": "settled @bob"})
	settled := s.deliveries(t, start, 1)[0]
	activeNow, err := h.svc.EnvForTest(s.inst).Credential(h.ctx, forgejo.CredentialWebhookSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := forgejo.New().Verify(h.ctx, fixedSecret{Env: h.svc.EnvForTest(s.inst), value: activeNow}, s.inst.View(), settled.headers, settled.body); err != nil {
		t.Fatalf("Forgejo does not sign with the core's active secret: %v", err)
	}
}

// fixedSecret is an Env whose only webhook secret is value.
type fixedSecret struct {
	provider.Env
	value []byte
}

func (f fixedSecret) Credentials(context.Context, string) ([][]byte, error) {
	return [][]byte{f.value}, nil
}
