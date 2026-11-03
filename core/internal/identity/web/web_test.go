package web

import (
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/crypto"
)

func testAuth(t *testing.T, workspaceURL string, proxies ...string) *Auth {
	t.Helper()
	keyring, err := crypto.NewKeyring("unit-test-master-secret-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	var prefixes []netip.Prefix
	for _, p := range proxies {
		prefixes = append(prefixes, netip.MustParsePrefix(p))
	}
	a, err := New(Config{WorkspaceURL: workspaceURL, IPLimit: 2, IPWindow: time.Minute, TrustedProxies: prefixes}, nil, nil, keyring, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRejectsBadWorkspaceURL(t *testing.T) {
	keyring, _ := crypto.NewKeyring("unit-test-master-secret-0123456789abcdef")
	for _, u := range []string{"", "work.acme.com", "ftp://work.acme.com", "https://"} {
		if _, err := New(Config{WorkspaceURL: u}, nil, nil, keyring, nil); err == nil {
			t.Errorf("New(%q) succeeded", u)
		}
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	for i := range 2 {
		if ok, _, _ := l.Allow("a"); !ok {
			t.Fatalf("attempt %d rejected", i+1)
		}
	}
	ok, retry, first := l.Allow("a")
	if ok || !first || retry != time.Minute {
		t.Fatalf("third attempt = %v, %v, %v", ok, retry, first)
	}
	if _, _, first := l.Allow("a"); first {
		t.Error("the second rejection in a window is reported as first")
	}
	if ok, _, _ := l.Allow("b"); !ok {
		t.Error("another key is limited")
	}
	now = now.Add(time.Minute)
	if ok, _, _ := l.Allow("a"); !ok {
		t.Error("the limit did not reset after the window")
	}
}

func TestClientIP(t *testing.T) {
	a := testAuth(t, "https://work.acme.com", "10.0.0.0/8")
	tests := []struct {
		remote, xff, want string
	}{
		{"203.0.113.7:4321", "", "203.0.113.7"},
		{"203.0.113.7:4321", "198.51.100.1", "203.0.113.7"},       // untrusted peers cannot spoof
		{"10.0.0.2:80", "198.51.100.1", "198.51.100.1"},           // through the proxy
		{"10.0.0.2:80", "198.51.100.1, 10.0.0.3", "198.51.100.1"}, // through two proxies
		{"10.0.0.2:80", "6.6.6.6, 198.51.100.1", "198.51.100.1"},  // client-supplied hops are ignored
		{"10.0.0.2:80", "", "10.0.0.2"},                           // no header
		{"10.0.0.2:80", "garbage", "10.0.0.2"},                    // malformed header
		{"[::ffff:203.0.113.7]:443", "", "203.0.113.7"},           // mapped IPv4
	}
	for _, tt := range tests {
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
		r.RemoteAddr = tt.remote
		if tt.xff != "" {
			r.Header.Set("X-Forwarded-For", tt.xff)
		}
		if got := a.ClientIP(r).String(); got != tt.want {
			t.Errorf("ClientIP(%s, XFF %q) = %s, want %s", tt.remote, tt.xff, got, tt.want)
		}
	}
}

func TestOriginChecks(t *testing.T) {
	a := testAuth(t, "https://work.acme.com")
	req := func(headers ...string) bool {
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/", nil)
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		return a.SameOrigin(r)
	}
	if !req("Origin", "https://work.acme.com") || !req("Referer", "https://work.acme.com/people?x=1") {
		t.Error("same-origin requests rejected")
	}
	for _, h := range [][]string{
		{}, {"Origin", "https://evil.com"}, {"Origin", "null"}, {"Origin", "http://work.acme.com"},
		{"Origin", "https://work.acme.com.evil.com"}, {"Referer", "https://evil.com/"},
		{"Origin", "https://evil.com", "Referer", "https://work.acme.com/"},
	} {
		if req(h...) {
			t.Errorf("SameOrigin(%v) = true", h)
		}
	}
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/", nil)
	if a.CrossOrigin(r) {
		t.Error("a request without Origin is cross-origin")
	}
	r.Header.Set("Origin", "https://evil.com")
	if !a.CrossOrigin(r) {
		t.Error("a foreign Origin is not cross-origin")
	}
}

func TestCSRFToken(t *testing.T) {
	a := testAuth(t, "https://work.acme.com")
	s1, s2 := uuid.New(), uuid.New()
	first, again := a.CSRFToken(s1), a.CSRFToken(s1)
	if first != again || first == a.CSRFToken(s2) {
		t.Fatal("CSRF tokens must be stable per session and differ between sessions")
	}
	other := testAuth(t, "https://work.acme.com")
	other.csrfKey[0] ^= 1
	if other.CSRFToken(s1) == a.CSRFToken(s1) {
		t.Fatal("CSRF tokens do not depend on the key")
	}
	r := httptest.NewRequestWithContext(t.Context(), "POST", "/", nil)
	r.Header.Set(CSRFHeader, a.CSRFToken(s2))
	if a.validCSRF(r, s1) {
		t.Error("another session's token is accepted")
	}
	r.Header.Set(CSRFHeader, a.CSRFToken(s1))
	if !a.validCSRF(r, s1) {
		t.Error("the session's token is rejected")
	}
}

func TestCookieNames(t *testing.T) {
	if a := testAuth(t, "https://work.acme.com"); a.SessionCookieName() != SecureSessionCookie || !a.secure {
		t.Error("https must use __Host- cookies")
	}
	if a := testAuth(t, "http://localhost:8080"); a.SessionCookieName() != InsecureSessionCookie || a.secure {
		t.Error("http must use unprefixed cookies")
	}
}

func TestDeviceName(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5; rv:128.0) Gecko/20100101 Firefox/128.0":                                                     "Firefox on macOS",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36 Edg/126.0":                   "Edge on Windows",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1": "Safari on iOS",
		"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Mobile Safari/537.36":                                "Chrome on Android",
		"curl/8.7.1": "Web browser",
		"":           "Web browser",
	} {
		if got := DeviceName(ua); got != want {
			t.Errorf("DeviceName(%q) = %q, want %q", ua, got, want)
		}
	}
}
