package forgejo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"

	"github.com/orglane/orglane/providers/provider"
)

// Webhook headers Forgejo sends.
const (
	// SignatureHeader carries the hex HMAC-SHA256 of the raw body.
	SignatureHeader = "X-Forgejo-Signature"
	// DeliveryHeader carries the delivery's UUID.
	DeliveryHeader = "X-Forgejo-Delivery"
	// EventHeader names the event.
	EventHeader = "X-Forgejo-Event"
)

var deliveryIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DeliveryID implements provider.EventSource: Forgejo's delivery UUID. The
// header is not signed; the core dedupes on the signed body too.
func (*Adapter) DeliveryID(h http.Header) (string, error) {
	id := h.Get(DeliveryHeader)
	if !deliveryIDRE.MatchString(id) {
		return "", fmt.Errorf("forgejo: no valid %s header", DeliveryHeader)
	}
	return id, nil
}

// Verify implements provider.EventSource: X-Forgejo-Signature must be the
// hex HMAC-SHA256 of the raw body under one of the instance's webhook
// secrets (during a rotation, every version the hook may use), compared in
// constant time.
func (*Adapter) Verify(ctx context.Context, env provider.Env, _ provider.Instance, h http.Header, body []byte) error {
	got, err := hex.DecodeString(h.Get(SignatureHeader))
	if err != nil || len(got) != sha256.Size {
		return provider.ErrBadSignature
	}
	secrets, err := env.Credentials(ctx, CredentialWebhookSecret)
	if err != nil {
		return fmt.Errorf("forgejo: webhook secrets: %w", err)
	}
	ok := false
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		// Every candidate is checked, so the time does not tell which one
		// matched.
		if hmac.Equal(mac.Sum(nil), got) {
			ok = true
		}
	}
	if !ok {
		return provider.ErrBadSignature
	}
	return nil
}
