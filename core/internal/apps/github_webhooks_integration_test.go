//go:build integration

package apps_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
)

// githubFixture is a recorded GitHub payload (octokit/webhooks examples,
// providers/github/testdata/webhooks/README.md), with fields replaced by
// dotted path before signing.
func githubFixture(t *testing.T, name string, fields map[string]any) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "providers", "github", "testdata", "webhooks", name+".json")) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) == 0 {
		return raw
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for path, v := range fields {
		parts := strings.Split(path, ".")
		m := doc
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func githubDelivery(secret, body []byte, deliveryID, event string) http.Header {
	h := http.Header{}
	h.Set(githubprovider.SignatureHeader, githubprovider.Sign(secret, body))
	if deliveryID != "" {
		h.Set(githubprovider.DeliveryHeader, deliveryID)
	}
	h.Set(githubprovider.EventHeader, event)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "GitHub-Hookshot/044aadd")
	return h
}

// TestGitHubWebhooks sends recorded GitHub App deliveries, signed with a
// test-only fixture secret, through the real core ingestion (verification,
// storage, dedupe, processing) with the real GitHub adapter and Postgres:
// events are stored once, foreign and App-level deliveries are ignored,
// and installation and hook events change the instance's state.
func TestGitHubWebhooks(t *testing.T) {
	h := newHarnessWith(t, apps.Config{}, githubprovider.New())
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "github", Key: "github", State: apps.StateActive, BaseURL: "https://github.com/Octocoders",
		Settings: map[string]any{"organization": "Octocoders", "organization_id": "38302899", "installation_id": "1", "app_id": "456"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", fixtureSecret); err != nil {
		t.Fatal(err)
	}
	h.drain()
	n := 0
	send := func(event string, body []byte) (apps.WebhookReceipt, string) {
		t.Helper()
		n++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		r, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, githubDelivery(fixtureSecret, body, id, event), body)
		if err != nil {
			t.Fatalf("%s: %v", event, err)
		}
		h.drain()
		return r, id
	}
	outcome := func(id string) string {
		t.Helper()
		var status, code string
		var count int
		if err := h.pool.QueryRow(h.ctx, `SELECT status, coalesce(error_code, ''), event_count FROM webhook_deliveries WHERE delivery_id = $1`, id).Scan(&status, &code, &count); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s/%s/%d", status, code, count)
	}
	state := func() string {
		t.Helper()
		var st, code string
		if err := h.pool.QueryRow(h.ctx, `SELECT state, coalesce(health_code, '') FROM app_instances WHERE id = $1`, inst.ID).Scan(&st, &code); err != nil {
			t.Fatal(err)
		}
		return st + "/" + code
	}

	opened := githubFixture(t, "pull_request.opened.with-organization", map[string]any{"pull_request.body": "Please look, @hubot."})
	// A bad signature is refused and nothing is stored.
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, githubDelivery([]byte("not-the-secret"), opened, "", "pull_request"), opened); !errors.Is(err, apps.ErrWebhookSignature) {
		t.Fatalf("bad signature = %v", err)
	}
	if h.count(`SELECT count(*) FROM webhook_deliveries`) != 0 {
		t.Fatal("a delivery with a bad signature was stored")
	}
	r, id := send("pull_request", opened)
	if r.Duplicate || outcome(id) != "processed//2" {
		t.Fatalf("pull request opened: %+v %s", r, outcome(id))
	}
	rows, err := h.pool.Query(h.ctx, `SELECT type, dedupe_key, provider, subject->>'repository', targets::text FROM events ORDER BY type`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var typ, key, prov, repo, targets string
		if err := rows.Scan(&typ, &key, &prov, &repo, &targets); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.Join([]string{typ, key, prov, repo, targets}, " "))
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	want := []string{
		`code.mention.created pr:279147437:body_mentions github Codertocat/Hello-World [{"login": "hubot", "reason": "mentioned"}]`,
		`code.pull_request.opened pr:279147437:opened github Codertocat/Hello-World [{"login": "octocat", "reason": "review_requested", "external_id": "5346"}, {"login": "Codertocat", "reason": "assigned", "external_id": "21031067"}]`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The same signed bytes under another delivery ID, or none, are
	// duplicates; a redelivery with other bytes (re-encoded) stores a
	// delivery but no second event.
	for _, did := range []string{"00000000-0000-4000-8000-999999999999", ""} {
		r, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, githubDelivery(fixtureSecret, opened, did, "pull_request"), opened)
		if err != nil || !r.Duplicate {
			t.Fatalf("replay = %+v, %v", r, err)
		}
	}
	reencoded := githubFixture(t, "pull_request.opened.with-organization", map[string]any{"pull_request.body": "Please look, @hubot.", "extra": true})
	if _, id := send("pull_request", reencoded); outcome(id) != "processed//0" {
		t.Fatalf("redelivery: %s", outcome(id))
	}
	// PostgreSQL's jsonb rejects NUL in text; a signed payload may still
	// carry it, so the adapter drops it rather than failing forever.
	nul := githubFixture(t, "pull_request.opened.with-organization", map[string]any{
		"pull_request.id": 6, "pull_request.title": "a\x00b", "pull_request.body": "Please look, @hubot\x00.",
	})
	if _, id := send("pull_request", nul); outcome(id) != "processed//2" {
		t.Fatalf("NUL in text: %s", outcome(id))
	}

	for _, c := range []struct {
		name, event string
		body        []byte
		want        string
	}{
		{"review requested", "pull_request", githubFixture(t, "pull_request.review_requested", nil), "processed//1"},
		{"comment with a mention", "issue_comment", githubFixture(t, "issue_comment.created.with-installation", map[string]any{"comment.body": "@octocat ping"}), "processed//1"},
		{"comment without a mention", "issue_comment", githubFixture(t, "issue_comment.created.with-installation", nil), "processed//0"},
		{"team membership removed", "membership", githubFixture(t, "membership.removed.with-installation", nil), "processed//1"},
		{"organization member added", "organization", githubFixture(t, "organization.member_added.with-installation", nil), "processed//1"},
		{"another installation", "pull_request", githubFixture(t, "pull_request.opened.with-organization", map[string]any{"installation.id": 2}), "ignored/foreign_installation/0"},
		{"another organization", "pull_request", githubFixture(t, "pull_request.opened.with-organization", map[string]any{"organization.id": 7}), "ignored/foreign_installation/0"},
		{"no installation", "issues", githubFixture(t, "issues.opened.with-organization", nil), "ignored/no_installation/0"},
		{"app authorization revoked", "github_app_authorization", githubFixture(t, "github_app_authorization.revoked", nil), "ignored/not_normalized/0"},
		{"not subscribed", "star", []byte(`{"action":"created"}`), "ignored/not_normalized/0"},
		{"malformed", "pull_request", []byte(`{"action":"opened","installation":{"id":1},"pull_request":"x"}`), "failed/invalid_payload/0"},
	} {
		if _, id := send(c.event, c.body); outcome(id) != c.want {
			t.Errorf("%s: %s, want %s", c.name, outcome(id), c.want)
		}
	}
	if st := state(); st != "active/" {
		t.Fatalf("state = %s", st)
	}

	// Installation and hook events change the instance's state, audited.
	audits := h.auditCount(audit.AppInstanceStateChanged)
	ours := map[string]any{"installation.id": 1, "installation.account.id": 38302899}
	if _, id := send("meta", githubFixture(t, "meta.deleted", nil)); outcome(id) != "processed//0" || state() != "degraded/webhook_deleted" {
		t.Fatalf("hook deleted: %s %s", outcome(id), state())
	}
	early := map[string]any{"installation.id": 1, "installation.account.id": 38302899, "installation.updated_at": "2021-04-28T22:32:40.000-04:00"}
	if _, id := send("installation", githubFixture(t, "installation.unsuspend", early)); outcome(id) != "ignored/not_normalized/0" || state() != "degraded/webhook_deleted" {
		t.Fatalf("an unsuspend cleared another degradation: %s %s", outcome(id), state())
	}
	// Any later delivery for the installation proves the hook works again.
	if _, id := send("issue_comment", githubFixture(t, "issue_comment.created.with-installation", map[string]any{"comment.id": 77})); outcome(id) != "processed//0" || state() != "active/" {
		t.Fatalf("a delivery after the hook's deletion: %s %s", outcome(id), state())
	}
	if _, id := send("installation", githubFixture(t, "installation.suspend", ours)); outcome(id) != "processed//0" || state() != "degraded/installation_suspended" {
		t.Fatalf("suspended: %s %s", outcome(id), state())
	}
	if _, id := send("installation", githubFixture(t, "installation.suspend", nil)); outcome(id) != "ignored/foreign_installation/0" {
		t.Fatalf("another installation's suspension: %s", outcome(id))
	}
	if _, id := send("installation", githubFixture(t, "installation.unsuspend", ours)); outcome(id) != "processed//0" || state() != "active/" {
		t.Fatalf("unsuspended: %s %s", outcome(id), state())
	}
	if _, id := send("installation", githubFixture(t, "installation.deleted", ours)); outcome(id) != "processed//0" || state() != "disconnected/installation_deleted" {
		t.Fatalf("deleted: %s %s", outcome(id), state())
	}
	if got := h.auditCount(audit.AppInstanceStateChanged) - audits; got != 5 {
		t.Fatalf("%d state changes audited, want 5", got)
	}
	// A disconnected instance stores deliveries but does not process them.
	if _, id := send("pull_request", githubFixture(t, "pull_request.opened.with-organization", map[string]any{"pull_request.id": 5})); outcome(id) != "ignored/instance_inactive/0" {
		t.Fatalf("after disconnect: %s", outcome(id))
	}
}

// TestGitHubEffectReplayAfterCleanup reproduces the review finding on
// 2156e68: delivery retention deletes the dedupe rows after 30 days, and a
// replay of the exact signed bytes then reapplied the connection effect
// although GitHub had not suspended the installation again. The durable
// effect marks outlive the delivery rows, so an old suspension cannot
// degrade the active instance and an old delivery cannot restore one whose
// webhook was deleted afterwards.
func TestGitHubEffectReplayAfterCleanup(t *testing.T) {
	h := newHarnessWith(t, apps.Config{}, githubprovider.New())
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "github", Key: "github", State: apps.StateActive, BaseURL: "https://github.com/Octocoders",
		Settings: map[string]any{"organization": "Octocoders", "organization_id": "38302899", "installation_id": "1", "app_id": "456"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", fixtureSecret); err != nil {
		t.Fatal(err)
	}
	h.drain()
	n := 0
	send := func(event string, body []byte) string {
		t.Helper()
		n++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		r, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, githubDelivery(fixtureSecret, body, id, event), body)
		if err != nil || r.Duplicate {
			t.Fatalf("%s: receipt %+v, %v", event, r, err)
		}
		h.drain()
		return id
	}
	outcome := func(id string) string {
		t.Helper()
		var status, code string
		if err := h.pool.QueryRow(h.ctx, `SELECT status, coalesce(error_code, '') FROM webhook_deliveries WHERE delivery_id = $1`, id).Scan(&status, &code); err != nil {
			t.Fatal(err)
		}
		return status + "/" + code
	}
	state := func() string {
		t.Helper()
		var st, code string
		if err := h.pool.QueryRow(h.ctx, `SELECT state, coalesce(health_code, '') FROM app_instances WHERE id = $1`, inst.ID).Scan(&st, &code); err != nil {
			t.Fatal(err)
		}
		return st + "/" + code
	}

	ours := map[string]any{"installation.id": 1, "installation.account.id": 38302899}
	pr := githubFixture(t, "pull_request.opened.with-organization", map[string]any{"pull_request.body": "Please look, @hubot."})
	suspend := githubFixture(t, "installation.suspend", ours)
	unsuspend := githubFixture(t, "installation.unsuspend", ours)
	if id := send("pull_request", pr); outcome(id) != "processed/" || state() != "active/" {
		t.Fatalf("pull request: %s %s", outcome(id), state())
	}
	if id := send("installation", suspend); outcome(id) != "processed/" || state() != "degraded/installation_suspended" {
		t.Fatalf("suspended: %s %s", outcome(id), state())
	}
	if id := send("installation", unsuspend); outcome(id) != "processed/" || state() != "active/" {
		t.Fatalf("unsuspended: %s %s", outcome(id), state())
	}
	audits := h.auditCount(audit.AppInstanceStateChanged)

	// Retention removes the delivery rows (the replay key), not the effect
	// marks (the durable effect key).
	h.exec(`UPDATE webhook_deliveries SET received_at = now() - interval '31 days', processed_at = now() - interval '31 days'`)
	if _, err := h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM webhook_deliveries`) != 0 {
		t.Fatal("retention kept deliveries")
	}

	// The reviewer's reproduction: the exact original suspension bytes and
	// signature, under a fresh delivery ID after cleanup. The delivery is
	// accepted (its dedupe row is gone) but its effect was already
	// considered, so the healthy connection stays active and nothing is
	// audited.
	if id := send("installation", suspend); outcome(id) != "ignored/effect_replayed" || state() != "active/" {
		t.Fatalf("replayed suspension: %s %s", outcome(id), state())
	}
	if got := h.auditCount(audit.AppInstanceStateChanged) - audits; got != 0 {
		t.Fatalf("replayed suspension changed the state %d times", got)
	}
	if got := h.count(`SELECT count(*) FROM webhook_effect_marks WHERE instance_id = $1`, inst.ID); got != 3 {
		t.Fatalf("%d effect marks survived retention, want 3", got)
	}

	// A stale delivery cannot restore a degradation that happened after it
	// was processed: the old pull request proved the hook worked then, not
	// after the App's webhook was deleted.
	if id := send("meta", githubFixture(t, "meta.deleted", nil)); outcome(id) != "processed/" || state() != "degraded/webhook_deleted" {
		t.Fatalf("hook deleted: %s %s", outcome(id), state())
	}
	audits = h.auditCount(audit.AppInstanceStateChanged)
	if id := send("pull_request", pr); outcome(id) != "processed/" || state() != "degraded/webhook_deleted" {
		t.Fatalf("replayed delivery restored the deleted hook: %s %s", outcome(id), state())
	}
	if got := h.auditCount(audit.AppInstanceStateChanged) - audits; got != 0 {
		t.Fatalf("replayed delivery changed the state %d times", got)
	}
}

// effectsProvider is the test provider with fixed delivery effects, for the
// core's handling of provider.DeliveryEffects independent of GitHub.
type effectsProvider struct {
	*testprovider.Provider
	effect provider.Effect
	err    error
}

func (p *effectsProvider) Effects(provider.Instance, provider.Delivery) (provider.Effect, error) {
	return p.effect, p.err
}

// TestDeliveryEffects: effects apply in the delivery's transaction; a
// restore clears only its own code; transitions the state does not allow
// are skipped; resyncs queue a drift run of provisioning; foreign and
// failed effects end the delivery like Normalize's errors.
func TestDeliveryEffects(t *testing.T) {
	fx := &effectsProvider{Provider: testprovider.New(testprovider.WithID("testeffects"))}
	h := newHarnessWith(t, apps.Config{}, fx)
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "testeffects", State: apps.StateActive, BaseURL: "https://test.example", Settings: map[string]any{"organization": "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", fixtureSecret); err != nil {
		t.Fatal(err)
	}
	h.drain()
	id := 0
	run := func(e provider.Effect, ferr error) string {
		t.Helper()
		fx.effect, fx.err = e, ferr
		id++
		body := prBody("acme", 1000+id)
		if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, body, fmt.Sprint("fx-", id), "pull_request"), body); err != nil {
			t.Fatal(err)
		}
		h.exec(`DELETE FROM river_job WHERE kind = $1`, apps.ProvisionInstanceArgs{}.Kind())
		var job int64
		if err := h.pool.QueryRow(h.ctx, `SELECT id FROM river_job WHERE kind = $1`, apps.ProcessDeliveryArgs{}.Kind()).Scan(&job); err != nil {
			t.Fatal(err)
		}
		var a apps.ProcessDeliveryArgs
		if err := h.pool.QueryRow(h.ctx, `SELECT args->>'delivery_id' FROM river_job WHERE id = $1`, job).Scan(&a.DeliveryID); err != nil {
			t.Fatal(err)
		}
		if err := h.svc.RunProcessDelivery(h.ctx, a); err != nil {
			t.Fatal(err)
		}
		h.exec(`DELETE FROM river_job WHERE id = $1`, job)
		var st, code, status, dcode string
		resync := h.jobs(apps.ProvisionInstanceArgs{}.Kind())
		h.exec(`DELETE FROM river_job`)
		if err := h.pool.QueryRow(h.ctx, `SELECT i.state, coalesce(i.health_code, ''), d.status, coalesce(d.error_code, '') FROM app_instances i JOIN webhook_deliveries d ON d.instance_id = i.id WHERE d.id = $1`, a.DeliveryID).Scan(&st, &code, &status, &dcode); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s/%s %s/%s resync=%d", st, code, status, dcode, resync)
	}
	for _, c := range []struct {
		name   string
		effect provider.Effect
		err    error
		want   string
	}{
		{"none", provider.Effect{}, nil, "active/ processed/ resync=0"},
		{"resync", provider.Effect{Resync: true}, nil, "active/ processed/ resync=1"},
		{"restore while active", provider.Effect{State: provider.EffectRestored, Code: "x_down"}, nil, "active/ processed/ resync=0"},
		{"degrade", provider.Effect{State: provider.EffectDegraded, Code: "x_down"}, nil, "degraded/x_down processed/ resync=0"},
		{"restore another code", provider.Effect{State: provider.EffectRestored, Code: "y_down"}, nil, "degraded/x_down processed/ resync=0"},
		{"invalid code", provider.Effect{State: provider.EffectDegraded, Code: "Not A Code"}, nil, "degraded/x_down processed/ resync=0"},
		{"resync while degraded", provider.Effect{Resync: true}, nil, "degraded/x_down processed/ resync=1"},
		{"restore", provider.Effect{State: provider.EffectRestored, Code: "x_down"}, nil, "active/ processed/ resync=0"},
		{"foreign", provider.Effect{State: provider.EffectDegraded, Code: "x_down"}, &provider.Error{Code: "elsewhere", Err: provider.ErrForeignDelivery}, "active/ ignored/elsewhere resync=0"},
		{"failed", provider.Effect{}, &provider.Error{Code: "invalid_payload"}, "active/ failed/invalid_payload resync=0"},
		{"disconnect and resync", provider.Effect{State: provider.EffectDisconnected, Code: "gone", Resync: true}, nil, "disconnected/gone processed/ resync=0"},
	} {
		if got := run(c.effect, c.err); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// A transition the state does not allow is skipped, not an error.
	h.exec(`UPDATE app_instances SET state = 'upgrading', health_code = NULL WHERE id = $1`, inst.ID)
	if got := run(provider.Effect{State: provider.EffectDisconnected, Code: "gone"}, nil); got != "upgrading/ processed/ resync=0" {
		t.Errorf("not allowed: %s", got)
	}
}
