//go:build integration

package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

// TestGitHubLiveWebhookDeliveries reads recent deliveries from the test
// App's delivery log (docs/specs/providers.md "Webhooks in live tests"):
//
//  1. Payload semantics: each payload of an event the adapter reads is
//     re-signed with the test-only fixture secret and goes through Verify,
//     Effects and Normalize for the test installation, which must accept
//     it (real App payloads, including installation).
//  2. Real signature over raw bytes, path (a): HMAC-SHA256 of the logged
//     payload bytes with the App's real webhook secret must equal the
//     logged X-Hub-Signature-256. If GitHub's log does not return the
//     signed bytes, the test fails loudly; it never falls back.
//
// Read only: it changes nothing in the organization. Deliveries come from
// what happens in the test organization (other live tests invite, add to
// teams and remove the bot user).
func TestGitHubLiveWebhookDeliveries(t *testing.T) {
	requireGitHubLive(t)
	env := &liveEnv{client: &http.Client{Timeout: 30 * time.Second}, key: []byte(os.Getenv("ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY"))}
	defer clear(env.key)
	inst := provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{
		"organization": os.Getenv("ORGLANE_TEST_GITHUB_ORG"), "app_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_ID"),
		"installation_id": os.Getenv("ORGLANE_TEST_GITHUB_INSTALLATION_ID"),
	}}
	_, orgID, err := ResolveOrganization(t.Context(), env, setting(inst, "organization"))
	if err != nil {
		t.Fatal(err)
	}
	inst.Settings["organization_id"] = orgID
	jwt, err := jwtFor(t.Context(), env, inst)
	if err != nil {
		t.Fatal(err)
	}
	var list []struct {
		ID    int64  `json:"id"`
		GUID  string `json:"guid"`
		Event string `json:"event"`
	}
	if err := request(t.Context(), env, http.MethodGet, "/app/hook/deliveries?per_page=100", jwt, nil, &list); err != nil {
		t.Fatal(err)
	}
	secret := []byte(os.Getenv("ORGLANE_TEST_GITHUB_WEBHOOK_SECRET"))
	defer clear(secret)
	fixtureEnv := &secretsEnv{secrets: [][]byte{fixtureSecret}}
	checked := 0
	for _, d := range list {
		if !slices.Contains(handledEvents, d.Event) || checked == 25 {
			continue
		}
		var full struct {
			Request struct {
				Headers map[string]string `json:"headers"`
				Payload json.RawMessage   `json:"payload"`
			} `json:"request"`
		}
		if err := request(t.Context(), env, http.MethodGet, "/app/hook/deliveries/"+strconv.FormatInt(d.ID, 10), jwt, nil, &full); err != nil {
			t.Fatal(err)
		}
		logged := http.Header{}
		for k, v := range full.Request.Headers {
			logged.Set(k, v)
		}
		payload := []byte(full.Request.Payload)
		if got, want := Sign(secret, payload), logged.Get(SignatureHeader); got != want {
			t.Errorf("delivery %d (%s): HMAC of the logged payload does not equal the logged %s; the delivery log is not a byte-exact real-signature path (docs/specs/providers.md \"Real signature over raw bytes\")", d.ID, d.Event, SignatureHeader)
		}

		h := http.Header{}
		h.Set(SignatureHeader, Sign(fixtureSecret, payload))
		h.Set(DeliveryHeader, d.GUID)
		h.Set(EventHeader, d.Event)
		a := New()
		if err := a.Verify(t.Context(), fixtureEnv, inst, h, payload); err != nil {
			t.Fatalf("delivery %d: re-signed payload does not verify: %v", d.ID, err)
		}
		if _, err := a.DeliveryID(h); err != nil {
			t.Errorf("delivery %d: %v", d.ID, err)
		}
		dl := provider.Delivery{ID: d.GUID, EventName: d.Event, Headers: h, Body: payload, ReceivedAt: time.Now().UTC()}
		if _, err := a.Effects(inst, dl); err != nil && !errors.Is(err, provider.ErrForeignDelivery) {
			t.Errorf("delivery %d (%s): Effects = %v", d.ID, d.Event, err)
		}
		events, err := a.Normalize(inst, dl)
		var pe *provider.Error
		switch {
		case errors.Is(err, provider.ErrForeignDelivery) && errors.As(err, &pe) && pe.Code == CodeNoInstallation && d.Event != "meta" && d.Event != "github_app_authorization":
			t.Errorf("delivery %d (%s): an App delivery without installation", d.ID, d.Event)
		case errors.Is(err, provider.ErrForeignDelivery):
			// Another installation of the test App; nothing to check.
		case err != nil:
			t.Errorf("delivery %d (%s): Normalize = %v", d.ID, d.Event, err)
		}
		for _, e := range events {
			if e.DedupeKey == "" || e.Actor.ExternalID == "" || e.Subject.ExternalID == "" {
				t.Errorf("delivery %d (%s): incomplete event %+v", d.ID, d.Event, e)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Skip("the test App's delivery log has no deliveries of events the adapter reads yet")
	}
	t.Logf("Checked %d real App deliveries: payload semantics with the fixture secret, and the logged signatures over the logged bytes.", checked)
}
