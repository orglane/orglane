//go:build integration

package apps_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/providers/testprovider"
)

var fixtureSecret = []byte("test-only-fixture-webhook-secret")

func delivery(secret, body []byte, deliveryID, event string) http.Header {
	h := http.Header{}
	h.Set(testprovider.SignatureHeader, testprovider.Sign(secret, body))
	if deliveryID != "" {
		h.Set(testprovider.DeliveryHeader, deliveryID)
	}
	if event != "" {
		h.Set(testprovider.EventHeader, event)
	}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer must-never-be-stored")
	return h
}

func prBody(org string, id int) []byte {
	return []byte(fmt.Sprintf(`{"organization":%q,"action":"opened","id":%d,"number":7,"title":"Add arm controller","actor":"alice","actor_id":1001,"reviewers":["bob"]}`, org, id))
}

// TestWebhookIngestion covers verification, storage, dedupe and replay by
// the signed bytes, processing into events and the untrusted-input rules.
func TestWebhookIngestion(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", fixtureSecret); err != nil {
		t.Fatal(err)
	}
	h.drain()
	body := prBody("acme", 42)

	// Bad signature: 401, nothing stored.
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery([]byte("wrong"), body, "d-1", "pull_request"), body); !errors.Is(err, apps.ErrWebhookSignature) {
		t.Fatalf("bad signature = %v", err)
	}
	// Unknown instance: 404.
	if _, err := h.svc.ReceiveWebhook(h.ctx, newID(), delivery(fixtureSecret, body, "d-1", "pull_request"), body); !errors.Is(err, apps.ErrWebhookUnknown) {
		t.Fatalf("unknown instance = %v", err)
	}
	if h.count(`SELECT count(*) FROM webhook_deliveries`) != 0 {
		t.Fatal("a rejected delivery was stored")
	}

	r, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, body, "d-1", "pull_request"), body)
	if err != nil || r.Duplicate {
		t.Fatalf("receive = %+v, %v", r, err)
	}
	if h.jobs(apps.ProcessDeliveryArgs{}.Kind()) != 1 {
		t.Fatal("no processing job was queued")
	}
	var stored []byte
	var headers string
	if err := h.pool.QueryRow(h.ctx, `SELECT body, headers::text FROM webhook_deliveries`).Scan(&stored, &headers); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("arm controller")) {
		t.Fatal("the delivery body is stored in plaintext")
	}
	if bytes.Contains([]byte(headers), []byte("must-never")) || bytes.Contains([]byte(headers), []byte(testprovider.SignatureHeader)) {
		t.Fatalf("headers outside the allowlist were stored: %s", headers)
	}

	// Replays of the same signed bytes are duplicates under any headers.
	for _, hd := range []http.Header{
		delivery(fixtureSecret, body, "d-2", "pull_request"), // changed delivery header
		delivery(fixtureSecret, body, "d-1", "ping"),         // changed event header
		delivery(fixtureSecret, body, "", "pull_request"),    // no delivery header
	} {
		r, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, hd, body)
		if err != nil || !r.Duplicate {
			t.Fatalf("replay = %+v, %v", r, err)
		}
	}
	// A different body with a reused delivery ID is a duplicate too.
	if r, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, prBody("acme", 43), "d-1", "pull_request"), prBody("acme", 43)); err != nil || !r.Duplicate {
		t.Fatalf("reused delivery ID = %+v, %v", r, err)
	}
	if h.count(`SELECT count(*) FROM webhook_deliveries`) != 1 || h.jobs(apps.ProcessDeliveryArgs{}.Kind()) != 1 {
		t.Fatal("a duplicate was stored or queued")
	}

	h.drain()
	var typ, dedupe, status string
	var count int
	if err := h.pool.QueryRow(h.ctx, `SELECT e.type, e.dedupe_key, d.status, d.event_count FROM events e JOIN webhook_deliveries d ON d.id = e.delivery_id`).Scan(&typ, &dedupe, &status, &count); err != nil {
		t.Fatal(err)
	}
	if typ != "code.pull_request.opened" || dedupe != "pr:42:opened" || status != "processed" || count != 1 {
		t.Fatalf("event %s %s, delivery %s %d", typ, dedupe, status, count)
	}

	// Foreign, not normalized and malformed deliveries.
	cases := []struct {
		body   []byte
		event  string
		status string
		code   string
	}{
		{prBody("other-org", 50), "pull_request", "ignored", "foreign_delivery"},
		{[]byte(`{"zen":"hi"}`), "ping", "ignored", "not_normalized"},
		{[]byte(`not json`), "pull_request", "failed", "invalid_payload"},
	}
	for i, c := range cases {
		id := fmt.Sprintf("case-%d", i)
		if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, c.body, id, c.event), c.body); err != nil {
			t.Fatal(err)
		}
		h.drain()
		var status, code string
		if err := h.pool.QueryRow(h.ctx, `SELECT status, coalesce(error_code, '') FROM webhook_deliveries WHERE delivery_id = $1`, id).Scan(&status, &code); err != nil {
			t.Fatal(err)
		}
		if status != c.status || code != c.code {
			t.Errorf("%s: %s/%s, want %s/%s", c.event, status, code, c.status, c.code)
		}
	}
	if h.count(`SELECT count(*) FROM events`) != 1 {
		t.Fatal("ignored deliveries created events")
	}

	// Events are append-only.
	if _, err := h.pool.Exec(h.ctx, `UPDATE events SET dedupe_key = 'x'`); err == nil {
		t.Fatal("events can be updated")
	}
	if _, err := h.pool.Exec(h.ctx, `DELETE FROM events`); err == nil {
		t.Fatal("events can be deleted")
	}

	// Retention: bodies go after 7 days, deliveries after 30; events stay.
	h.exec(`UPDATE webhook_deliveries SET processed_at = now() - interval '8 days' WHERE delivery_id = 'd-1'`)
	if _, err := h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM webhook_deliveries WHERE delivery_id = 'd-1' AND body IS NULL`) != 1 {
		t.Fatal("an old body was kept")
	}
	if h.count(`SELECT count(*) FROM webhook_deliveries WHERE delivery_id = 'case-2' AND body IS NOT NULL`) != 1 {
		t.Fatal("a recent failed body was cleared")
	}
	h.exec(`UPDATE webhook_deliveries SET received_at = now() - interval '31 days'`)
	if _, err := h.svc.Cleanup(h.ctx); err != nil {
		t.Fatal(err)
	}
	if h.count(`SELECT count(*) FROM webhook_deliveries`) != 0 || h.count(`SELECT count(*) FROM events`) != 1 {
		t.Fatal("retention deleted events or kept deliveries")
	}
	// A replay older than the delivery rows creates a delivery but no event.
	if r, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, body, "d-1", "pull_request"), body); err != nil || r.Duplicate {
		t.Fatalf("old replay = %+v, %v", r, err)
	}
	h.drain()
	if h.count(`SELECT count(*) FROM events`) != 1 {
		t.Fatal("an old replay created a second event")
	}
	if h.count(`SELECT count(*) FROM webhook_deliveries WHERE status = 'processed' AND event_count = 0`) != 1 {
		t.Fatal("the old replay was not processed without events")
	}

	// A secret that cannot be loaded is a server error, not a bad signature.
	h.exec(`UPDATE app_credentials SET ciphertext = '\x00' WHERE instance_id = $1 AND name = 'webhook_secret' AND state = 'active'`, inst.ID)
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, prBody("acme", 98), "y", "pull_request"), prBody("acme", 98)); err == nil || errors.Is(err, apps.ErrWebhookSignature) {
		t.Fatalf("unreadable secret = %v, want a server error", err)
	}
	if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, "webhook_secret", fixtureSecret); err != nil {
		t.Fatal(err)
	}
	h.exec(`DELETE FROM app_credentials WHERE instance_id = $1 AND name = 'webhook_secret' AND state = 'retiring'`, inst.ID)

	// Connecting and removed instances answer 404; inactive ones are kept
	// but not processed.
	h.exec(`UPDATE app_instances SET state = 'connecting' WHERE id = $1`, inst.ID)
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, prBody("acme", 99), "x", "pull_request"), prBody("acme", 99)); !errors.Is(err, apps.ErrWebhookUnknown) {
		t.Fatalf("connecting instance = %v", err)
	}
	h.exec(`UPDATE app_instances SET state = 'failed' WHERE id = $1`, inst.ID)
	if _, err := h.svc.ReceiveWebhook(h.ctx, inst.ID, delivery(fixtureSecret, prBody("acme", 99), "x", "pull_request"), prBody("acme", 99)); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if h.count(`SELECT count(*) FROM webhook_deliveries WHERE delivery_id = 'x' AND status = 'ignored' AND error_code = 'instance_inactive'`) != 1 {
		t.Fatal("a delivery to an inactive instance was processed")
	}
}
