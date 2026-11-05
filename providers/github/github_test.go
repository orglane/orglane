package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

type testEnv struct {
	client      *http.Client
	now         time.Time
	credentials map[string][]byte
}

func (e *testEnv) Credential(_ context.Context, n string) ([]byte, error) {
	return append([]byte(nil), e.credentials[n]...), nil
}
func (e *testEnv) Credentials(ctx context.Context, n string) ([][]byte, error) {
	v, err := e.Credential(ctx, n)
	return [][]byte{v}, err
}
func (e *testEnv) HTTPClient() *http.Client { return e.client }
func (*testEnv) Logger() *slog.Logger       { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (e *testEnv) Now() time.Time           { return e.now }
func (*testEnv) WorkspaceURL() *url.URL     { return &url.URL{Scheme: "https", Host: "work.example.test"} }
func (e *testEnv) IssuerURL() *url.URL      { return e.WorkspaceURL() }
func (e *testEnv) WebhookURL(id uuid.UUID) *url.URL {
	u := e.WorkspaceURL()
	u.Path = "/api/v1/webhooks/" + id.String()
	return u
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}
}

var keyOnce sync.Once
var testKey []byte

func privateKey(t *testing.T) []byte {
	t.Helper()
	keyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		testKey = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	})
	return append([]byte(nil), testKey...)
}
func fixture(t *testing.T) (*testEnv, provider.Instance) {
	t.Helper()
	return &testEnv{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), credentials: map[string][]byte{"app_private_key": privateKey(t)}, client: &http.Client{}}, provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{"organization": "orglane-test", "organization_id": "123", "app_id": "456", "app_slug": "orglane-ci", "app_client_id": "Iv1.test", "installation_id": "789"}}
}
func installation() map[string]any {
	return map[string]any{"id": 789, "app_id": 456, "account": map[string]any{"id": 123, "login": "orglane-test", "type": "Organization"}, "target_id": 123, "target_type": "Organization", "permissions": RequiredPermissions(), "suspended_at": nil}
}

func TestManifestAndUntrustedURLs(t *testing.T) {
	env, inst := fixture(t)
	delete(inst.Settings, "app_slug")
	step, err := New().BeginConnect(t.Context(), env, inst, "ols_test")
	if err != nil {
		t.Fatal(err)
	}
	if step.Method != "POST" || step.URL.Host != "github.com" || step.URL.Path != "/organizations/orglane-test/settings/apps/new" || step.URL.Query().Get("state") != "ols_test" {
		t.Fatalf("unsafe manifest step: %+v", step)
	}
	var m map[string]any
	if err = json.Unmarshal([]byte(step.Form["manifest"]), &m); err != nil {
		t.Fatal(err)
	}
	if m["redirect_url"] != "https://work.example.test/settings/apps/github/callback" || m["setup_url"] != m["redirect_url"] || m["public"] != false {
		t.Fatalf("callback config: %v", m)
	}
	if callbacks := m["callback_urls"].([]any); len(callbacks) != 1 || callbacks[0] != "https://work.example.test/me/accounts/github/callback" {
		t.Fatal("wrong employee callback", callbacks)
	}
	permissions := m["default_permissions"].(map[string]any)
	if len(permissions) != 4 || permissions["members"] != "write" || permissions["metadata"] != "read" {
		t.Fatal(permissions)
	}
	if m["hook_attributes"].(map[string]any)["url"] != env.WebhookURL(inst.ID).String() {
		t.Fatal("webhook not bound to instance")
	}
	for _, bad := range []string{"../evil", "https://evil.test", "a/b", "a?state=bad", "a#bad", "a%2Fb", "-org", "org-", strings.Repeat("a", 40)} {
		inst.Settings["organization"] = bad
		if _, err = New().BeginConnect(t.Context(), env, inst, "s"); err == nil {
			t.Errorf("accepted organization %q", bad)
		}
	}
}
func TestJWT(t *testing.T) {
	env, inst := fixture(t)
	jwt, err := jwtFor(t.Context(), env, inst)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatal("jwt parts")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if claims["iss"] != "456" || int64(claims["iat"].(float64)) != env.now.Add(-time.Minute).Unix() || int64(claims["exp"].(float64)) != env.now.Add(9*time.Minute).Unix() {
		t.Fatal(claims)
	}
	key, _ := parseKey(privateKey(t))
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err = rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hash[:], signature); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationVerification(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong app", func(v map[string]any) { v["app_id"] = 999 }},
		{"wrong id", func(v map[string]any) { v["id"] = 999 }},
		{"wrong org", func(v map[string]any) { v["account"].(map[string]any)["id"] = 999 }},
		{"user account", func(v map[string]any) { v["account"].(map[string]any)["type"] = "User" }},
		{"wrong target", func(v map[string]any) { v["target_id"] = 999 }},
		{"suspended", func(v map[string]any) { v["suspended_at"] = "2026-11-04T12:00:00Z" }},
		{"insufficient permissions", func(v map[string]any) { v["permissions"].(map[string]string)["members"] = "read" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, inst := fixture(t)
			v := installation()
			tc.change(v)
			env.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" || r.URL.Path != "/app/installations/789" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
					t.Fatalf("unexpected request %s", r.URL)
				}
				return response(200, v), nil
			})
			if _, err := New().VerifyInstallation(t.Context(), env, inst, "789"); err == nil {
				t.Fatal("accepted foreign/unusable installation")
			}
		})
	}
	t.Run("expected immutable org resolved", func(t *testing.T) {
		env, inst := fixture(t)
		delete(inst.Settings, "organization_id")
		calls := 0
		env.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Path == "/orgs/orglane-test" {
				return response(200, map[string]any{"id": 123, "login": "orglane-test", "type": "Organization"}), nil
			}
			return response(200, installation()), nil
		})
		got, err := New().VerifyInstallation(t.Context(), env, inst, "789")
		if err != nil || got.Account.ID != 123 || calls != 2 {
			t.Fatalf("verify: %+v %v calls=%d", got, err, calls)
		}
	})
}
func TestManifestConversionAndManual(t *testing.T) {
	env, inst := fixture(t)
	delete(inst.Settings, "app_id")
	delete(inst.Settings, "app_slug")
	calls := 0
	env.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/app-manifests/valid-code/conversions" {
			t.Fatalf("path %s", r.URL.Path)
		}
		return response(201, map[string]any{"id": 456, "slug": "orglane-ci", "client_id": "Iv1.test", "client_secret": "secret", "webhook_secret": strings.Repeat("w", 32), "pem": string(privateKey(t))}), nil
	})
	got, err := New().CompleteConnect(t.Context(), env, inst, "create_app", url.Values{"code": {"valid-code"}, "state": {"ols_next"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Done || len(got.Credentials) != 3 || got.Next.URL.Query().Get("state") != "ols_next" || got.Next.Name != "install_app" || got.Settings["app_id"] != "456" {
		t.Fatalf("conversion %+v", got)
	}
	for _, bad := range []string{"../evil", "x?token=secret", "a%2Fb", "", "x/y"} {
		_, err = New().CompleteConnect(t.Context(), env, inst, "create_app", url.Values{"code": {bad}})
		if err == nil {
			t.Errorf("accepted code %q", bad)
		}
	}
	if calls != 1 {
		t.Fatalf("bad codes reached API: %d", calls)
	}
	for _, value := range got.Credentials {
		clear(value)
	}
}
func TestTokenCacheAndExpiry(t *testing.T) {
	env, inst := fixture(t)
	a := New()
	minted := 0
	env.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/app/installations/789" {
			return response(200, installation()), nil
		}
		if r.URL.Path != "/app/installations/789/access_tokens" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		minted++
		return response(201, map[string]any{"token": fmt.Sprintf("test-%d", minted), "expires_at": env.now.Add(time.Hour)}), nil
	})
	first, err := a.InstallationToken(t.Context(), env, inst)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.InstallationToken(t.Context(), env, inst)
	if err != nil || first != second || minted != 1 {
		t.Fatal("cache missed", err, minted)
	}
	env.now = env.now.Add(55 * time.Minute)
	third, err := a.InstallationToken(t.Context(), env, inst)
	if err != nil || third == first || minted != 2 {
		t.Fatal("token was not refreshed", err, minted)
	}
	inst.Settings["installation_id"] = "../bad"
	if _, err = a.InstallationToken(t.Context(), env, inst); err == nil {
		t.Fatal("accepted token path injection")
	}
}
func TestRotationClassificationAndSecretSafety(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 404, 422, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			env, inst := fixture(t)
			env.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if r.Method != "PATCH" || r.URL.Path != "/app/hook/config" {
					t.Fatal(r.Method, r.URL.Path)
				}
				return response(status, map[string]string{"message": "private-sensitive-response"}), nil
			})
			out, err := New().ApplyCredential(t.Context(), env, inst, "webhook_secret", 2, []byte(strings.Repeat("w", 32)))
			expected := provider.RotationRejected
			if status == 200 {
				expected = provider.RotationAcknowledged
			} else if status >= 500 || status == 429 {
				expected = provider.RotationUnknown
			}
			if out.Outcome != expected {
				t.Fatalf("outcome %s expected %s (%v)", out.Outcome, expected, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-sensitive-response") {
				t.Fatal("response leaked")
			}
		})
	}
	env, inst := fixture(t)
	env.client.Transport = transport(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("transport https://api.github.com/app-manifests/private-code/conversions")
	})
	_, err := New().CompleteConnect(t.Context(), env, inst, "create_app", url.Values{"code": {"private-code"}})
	if err == nil || strings.Contains(err.Error(), "private-code") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}
func TestRedirectAndMalformedResponse(t *testing.T) {
	for _, status := range []int{301, 302, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			env, inst := fixture(t)
			calls := 0
			env.client.Transport = transport(func(*http.Request) (*http.Response, error) {
				calls++
				r := response(status, nil)
				r.Header.Set("Location", "https://evil.test/secret")
				return r, nil
			})
			if _, err := New().VerifyInstallation(t.Context(), env, inst, "789"); err == nil || calls != 1 {
				t.Fatalf("redirect followed: %v %d", err, calls)
			}
		})
	}
}

func TestManualSetupValidatesAppIdentity(t *testing.T) {
	env, inst := fixture(t)
	params := url.Values{"app_id": {"456"}, "client_id": {"Iv1.test"}, "private_key": {string(privateKey(t))}, "client_secret": {"test-secret"}, "webhook_secret": {strings.Repeat("w", 32)}, "state": {"ols_install"}}
	env.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/app" || r.Header.Get("Authorization") == "" {
			t.Fatal("manual App verification missing")
		}
		return response(200, map[string]any{"id": 456, "slug": "orglane-ci", "client_id": "Iv1.test"}), nil
	})
	got, err := New().CompleteConnect(t.Context(), env, inst, "manual", params)
	if err != nil {
		t.Fatal(err)
	}
	if got.Done || got.Next.Name != "install_app" || got.Next.URL.Query().Get("state") != "ols_install" || len(got.Credentials) != 3 {
		t.Fatalf("manual result %+v", got)
	}
	params.Set("client_id", "another-app")
	if _, err = New().CompleteConnect(t.Context(), env, inst, "manual", params); err == nil {
		t.Fatal("accepted wrong client ID")
	}
	params.Set("private_key", "not-a-key")
	env.client.Transport = transport(func(*http.Request) (*http.Response, error) { t.Fatal("invalid key reached GitHub"); return nil, nil })
	if _, err = New().CompleteConnect(t.Context(), env, inst, "manual", params); err == nil {
		t.Fatal("accepted invalid key")
	}
}

func TestInvalidTokenExpiry(t *testing.T) {
	for _, duration := range []time.Duration{-time.Minute, 4 * time.Minute, 66 * time.Minute} {
		t.Run(duration.String(), func(t *testing.T) {
			env, inst := fixture(t)
			env.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/app/installations/789" {
					return response(200, installation()), nil
				}
				return response(201, map[string]any{"token": "test-token", "expires_at": env.now.Add(duration)}), nil
			})
			a := New()
			if _, err := a.InstallationToken(t.Context(), env, inst); err == nil {
				t.Fatal("accepted unsafe expiry")
			}
			if len(a.tokens) != 0 {
				t.Fatal("cached invalid token")
			}
		})
	}
}

func TestRequestCannotChangeAPIOrigin(t *testing.T) {
	env, _ := fixture(t)
	env.client.Transport = transport(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unsafe path reached transport: %s", r.URL)
		return nil, nil
	})
	for _, path := range []string{"@evil.test/x", ".evil.test/x", "//evil.test/x", "https://evil.test/x", "/app#fragment", "/app\\evil"} {
		if err := request(t.Context(), env, http.MethodGet, path, "", nil, nil); err == nil {
			t.Errorf("accepted unsafe API path %q", path)
		}
	}
}
