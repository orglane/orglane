package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	specpkg "github.com/orglane/orglane/core/api"
	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/authz"
)

// stubAuthenticator maps bearer tokens to principals. Real identity arrives
// with the embedded OIDC provider (#6).
type stubAuthenticator map[string]*authz.Principal

func (s stubAuthenticator) Authenticate(r *http.Request) (*authz.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, authz.ErrUnauthenticated
	}
	if token == "broken" {
		return nil, errors.New("identity store unavailable")
	}
	if token == "cross-site" {
		return nil, authz.ErrCrossSite
	}
	p, ok := s[token]
	if !ok {
		return nil, authz.ErrUnauthenticated
	}
	return p, nil
}

var testAuth = stubAuthenticator{
	"admin":  {Subject: "admin", Permissions: []authz.Permission{authz.InstanceRead}},
	"member": {Subject: "member"},
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestHandler(t *testing.T, opts Options) http.Handler {
	t.Helper()
	if opts.Authenticator == nil {
		opts.Authenticator = testAuth
	}
	opts.Logger = discardLogger()
	h, err := NewHandler(opts)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return h
}

func do(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) apigen.Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Fatalf("Content-Type = %q, want %q (body %s)", ct, ProblemContentType, rec.Body)
	}
	var p apigen.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %s)", err, rec.Body)
	}
	if p.Status != rec.Code || p.Title != http.StatusText(rec.Code) || p.Type != "about:blank" {
		t.Fatalf("problem = %+v, want status %d", p, rec.Code)
	}
	return p
}

func TestPermissionEnforcement(t *testing.T) {
	h := newTestHandler(t, Options{Version: "v1.2.3", BaseDomain: "acme.com", WorkspaceHost: "work.acme.com"})

	tests := []struct {
		name   string
		path   string
		token  string
		status int
	}{
		{"public without credentials", "/api/v1/version", "", http.StatusOK},
		{"public with credentials", "/api/v1/version", "member", http.StatusOK},
		{"liveness is public", "/healthz", "", http.StatusOK},
		{"readiness is public", "/readyz", "", http.StatusOK},
		{"allowed with permission", "/api/v1/instance", "admin", http.StatusOK},
		{"denied without credentials", "/api/v1/instance", "", http.StatusUnauthorized},
		{"denied with unknown token", "/api/v1/instance", "nope", http.StatusUnauthorized},
		{"denied without permission", "/api/v1/instance", "member", http.StatusForbidden},
		{"authenticator failure", "/api/v1/instance", "broken", http.StatusInternalServerError},
		{"failed CSRF checks", "/api/v1/instance", "cross-site", http.StatusForbidden},
		{"self-service without credentials", "/api/v1/me", "", http.StatusUnauthorized},
		{"discovery is public", "/.well-known/orglane", "", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, h, tt.path, tt.token)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.status, rec.Body)
			}
			if rec.Code >= 400 {
				decodeProblem(t, rec)
			}
		})
	}
}

func TestUnauthorizedResponse(t *testing.T) {
	h := newTestHandler(t, Options{})
	rec := do(t, h, "/api/v1/instance", "")
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}
	p := decodeProblem(t, rec)
	if p.Instance == nil || *p.Instance != "/api/v1/instance" {
		t.Errorf("problem instance = %v, want /api/v1/instance", p.Instance)
	}
}

func TestForbiddenNamesPermission(t *testing.T) {
	h := newTestHandler(t, Options{})
	p := decodeProblem(t, do(t, h, "/api/v1/instance", "member"))
	if p.Detail == nil || !strings.Contains(*p.Detail, `"instance.read"`) {
		t.Errorf("problem detail = %v, want it to name the permission", p.Detail)
	}
}

func TestDefaultAuthenticatorDeniesAll(t *testing.T) {
	h, err := NewHandler(Options{Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "/api/v1/instance", "admin"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestPrincipalReachesHandler(t *testing.T) {
	var seen *authz.Principal
	g := newRouteGuard(http.NewServeMux(), nil, testAuth, discardLogger())
	h := g.enforce(Operation{OperationID: "getInstance", Permission: authz.InstanceRead},
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = authz.PrincipalFrom(r.Context()) }))
	do(t, h, "/api/v1/instance", "admin")
	if seen == nil || seen.Subject != "admin" {
		t.Fatalf("principal in handler = %+v, want admin", seen)
	}
}

func TestResponses(t *testing.T) {
	h := newTestHandler(t, Options{Version: "v1.2.3", BaseDomain: "acme.com", WorkspaceHost: "work.acme.com"})

	var v apigen.Version
	rec := do(t, h, "/api/v1/version", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Version != "v1.2.3" {
		t.Fatalf("version = %+v, %v (body %s)", v, err, rec.Body)
	}

	var inst apigen.Instance
	rec = do(t, h, "/api/v1/instance", "admin")
	if err := json.Unmarshal(rec.Body.Bytes(), &inst); err != nil {
		t.Fatal(err)
	}
	want := apigen.Instance{Version: "v1.2.3", BaseDomain: "acme.com", WorkspaceHost: "work.acme.com"}
	if inst != want {
		t.Fatalf("instance = %+v, want %+v", inst, want)
	}
}

func TestReadiness(t *testing.T) {
	ok := ReadinessCheck{Name: "database", Check: func(context.Context) error { return nil }}
	failing := ReadinessCheck{Name: "migrations", Check: func(context.Context) error { return errors.New("2 pending at postgres://secret@db") }}

	h := newTestHandler(t, Options{ReadinessChecks: []ReadinessCheck{ok}})
	if rec := do(t, h, "/readyz", ""); rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", rec.Code)
	}

	h = newTestHandler(t, Options{ReadinessChecks: []ReadinessCheck{ok, failing}})
	rec := do(t, h, "/readyz", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("readiness leaks check errors: %s", rec.Body)
	}
	var health apigen.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health.Status != apigen.HealthStatusUnavailable || health.Checks == nil || len(*health.Checks) != 2 ||
		(*health.Checks)[1] != (apigen.HealthCheck{Name: "migrations", Status: apigen.HealthCheckStatusUnavailable}) {
		t.Fatalf("health = %+v", health)
	}
}

func TestUnknownAPIPathIsProblem(t *testing.T) {
	h := newTestHandler(t, Options{})
	rec := do(t, h, "/api/v1/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	decodeProblem(t, rec)
}

func TestHandlerFailsClosed(t *testing.T) {
	t.Run("operation without permission", func(t *testing.T) {
		spec := strings.Replace(string(specpkg.Spec), "x-orglane-permission: instance.read", "", 1)
		if _, err := newHandler([]byte(spec), Options{Logger: discardLogger()}); err == nil ||
			!strings.Contains(err.Error(), "GET /api/v1/instance: missing x-orglane-permission") {
			t.Fatalf("newHandler() error = %v, want missing permission", err)
		}
	})
	t.Run("generated route missing from contract", func(t *testing.T) {
		spec := strings.Replace(string(specpkg.Spec), "  /api/v1/instance:\n", "  /api/v1/renamed:\n", 1)
		_, err := newHandler([]byte(spec), Options{Logger: discardLogger()})
		if err == nil || !strings.Contains(err.Error(), `route "GET /api/v1/instance" has no operation`) ||
			!strings.Contains(err.Error(), `operation "GET /api/v1/renamed" is not served`) {
			t.Fatalf("newHandler() error = %v, want route/contract mismatch", err)
		}
	})
}

func TestPanicIsProblem(t *testing.T) {
	h := recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), discardLogger())
	rec := do(t, h, "/api/v1/anything", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	decodeProblem(t, rec)
}
