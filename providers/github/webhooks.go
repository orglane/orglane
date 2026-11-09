package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/orglane/orglane/providers/provider"
)

// Webhook headers GitHub sends with App deliveries
// (https://docs.github.com/en/webhooks/webhook-events-and-payloads#delivery-headers).
const (
	// SignatureHeader carries "sha256=" and the hex HMAC-SHA256 of the raw
	// body under the App's webhook secret.
	SignatureHeader = "X-Hub-Signature-256"
	// DeliveryHeader carries the delivery's GUID.
	DeliveryHeader = "X-GitHub-Delivery"
	// EventHeader names the event.
	EventHeader = "X-GitHub-Event"
)

// CredentialWebhookSecret is the App's webhook secret.
const CredentialWebhookSecret = "webhook_secret"

var deliveryIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DeliveryID implements provider.EventSource: GitHub's delivery GUID. The
// header is not signed; the core dedupes on the signed body too.
func (*Adapter) DeliveryID(h http.Header) (string, error) {
	id := h.Get(DeliveryHeader)
	if !deliveryIDPattern.MatchString(id) {
		return "", fmt.Errorf("github: no valid %s header", DeliveryHeader)
	}
	return id, nil
}

// Verify implements provider.EventSource: X-Hub-Signature-256 must be
// "sha256=" and the hex HMAC-SHA256 of the raw body under one of the
// instance's webhook secrets (during a rotation, every version the App may
// use), compared in constant time.
func (*Adapter) Verify(ctx context.Context, env provider.Env, _ provider.Instance, h http.Header, body []byte) error {
	hexSum, ok := strings.CutPrefix(h.Get(SignatureHeader), "sha256=")
	if !ok {
		return provider.ErrBadSignature
	}
	got, err := hex.DecodeString(hexSum)
	if err != nil || len(got) != sha256.Size {
		return provider.ErrBadSignature
	}
	secrets, err := env.Credentials(ctx, CredentialWebhookSecret)
	if err != nil {
		return fmt.Errorf("github: webhook secrets: %w", err)
	}
	matched := false
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		// Every candidate is checked, so the time does not tell which one
		// matched.
		if hmac.Equal(mac.Sum(nil), got) {
			matched = true
		}
	}
	if !matched {
		return provider.ErrBadSignature
	}
	return nil
}

// Sign returns the X-Hub-Signature-256 value of body under secret, as
// GitHub computes it (tests and fixtures).
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// EnsureWebhooks implements provider.EventSource. The App carries its own
// webhook (created by the manifest, its secret set by the webhook_secret
// rotation), so there is nothing to create per instance.
func (*Adapter) EnsureWebhooks(context.Context, provider.Env, provider.Instance) error { return nil }

var _ provider.EventSource = (*Adapter)(nil)
var _ provider.DeliveryEffects = (*Adapter)(nil)
