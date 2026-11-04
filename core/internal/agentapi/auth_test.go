package agentapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func key(t *testing.T, secret string) Key {
	t.Helper()
	k, err := DeriveKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestDeriveKey(t *testing.T) {
	if _, err := DeriveKey("short"); err == nil {
		t.Fatal("a short secret was accepted")
	}
	a, b := key(t, strings.Repeat("a", 32)), key(t, strings.Repeat("b", 32))
	if a == b || a != key(t, strings.Repeat("a", 32)) {
		t.Fatal("keys are not derived per secret")
	}
}

func TestResponseSignature(t *testing.T) {
	k := key(t, strings.Repeat("a", 32))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/v1/instances/git", nil)
	mac := SignRequest(k, req, []byte("{}"), time.Now())
	body := []byte(`{"state":"queued"}`)
	h := http.Header{}
	h.Set(HeaderSignature, SignResponse(k, mac, 202, body))
	if !VerifyResponse(k, mac, 202, h, body) {
		t.Fatal("a valid signature was refused")
	}
	other := SignRequest(k, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/health", nil), nil, time.Now())
	for name, ok := range map[string]bool{
		"status":      VerifyResponse(k, mac, 200, h, body),
		"body":        VerifyResponse(k, mac, 202, h, []byte(`{"state":"succeeded"}`)),
		"request mac": VerifyResponse(k, other, 202, h, body),
		"key":         VerifyResponse(key(t, strings.Repeat("b", 32)), mac, 202, h, body),
		"missing":     VerifyResponse(k, mac, 202, http.Header{}, body),
	} {
		if ok {
			t.Errorf("a response with a changed %s was accepted", name)
		}
	}
}

func TestVerifierNonceCache(t *testing.T) {
	k := key(t, strings.Repeat("a", 32))
	start := time.Unix(1_800_000_000, 0)
	now := start
	v := NewVerifier(k, start, func() time.Time { return now })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/health", nil)
	SignRequest(k, req, nil, now)
	if _, err := v.Verify(req.Method, req.URL, req.Header, nil); err != nil {
		t.Fatal(err)
	}
	// Replays are refused while the timestamp is acceptable, and the
	// timestamp check refuses them after the nonce expired.
	for _, d := range []time.Duration{0, 30 * time.Second, 59 * time.Second, 61 * time.Second, 3 * time.Minute} {
		now = start.Add(d)
		if _, err := v.Verify(req.Method, req.URL, req.Header, nil); err == nil {
			t.Fatalf("replay after %v accepted", d)
		}
	}
	if len(v.nonces) > 1 {
		t.Fatalf("nonce cache holds %d entries", len(v.nonces))
	}
}
