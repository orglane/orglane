package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/orglane/orglane/core/internal/config"
)

func load(t *testing.T, vars map[string]string) *config.Config {
	t.Helper()
	all := map[string]string{
		"ORGLANE_DATABASE_URL":           "postgres://orglane@postgres:5432/orglane",
		"ORGLANE_SERVER_INTERNAL_LISTEN": ":9090",
		"ORGLANE_INGRESS_UPSTREAM_URL":   "http://orglane:8080",
		"ORGLANE_IDENTITY_MASTER_SECRET": "test-master-secret-0123456789abcdef",
	}
	maps.Copy(all, vars)
	cfg, err := config.Load("", func(k string) (string, bool) { v, ok := all[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// assertJSON compares the encoded configuration with the exact document
// Traefik is expected to receive.
func assertJSON(t *testing.T, got Dynamic, want string) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(want)); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	if !bytes.Equal(gotJSON, buf.Bytes()) {
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, gotJSON, "", "  ")
		t.Errorf("configuration mismatch.\ngot:\n%s\nwant:\n%s", pretty.String(), want)
	}
}

func TestBuildDomainMode(t *testing.T) {
	cfg := load(t, map[string]string{
		"ORGLANE_DOMAIN_BASE":           "acme.com",
		"ORGLANE_DOMAIN_WORKSPACE_HOST": "Work.Acme.com.",
	})
	assertJSON(t, Build(cfg, nil), `{
  "http": {
    "routers": {
      "orglane-workspace": {
        "entryPoints": ["websecure"],
        "rule": "Host(`+"`work.acme.com`"+`)",
        "service": "orglane-core",
        "middlewares": ["orglane-security-headers"],
        "tls": {"certResolver": "letsencrypt"}
      },
      "orglane-workspace-redirect": {
        "entryPoints": ["web"],
        "rule": "Host(`+"`work.acme.com`"+`)",
        "service": "noop@internal",
        "middlewares": ["orglane-https-redirect"]
      }
    },
    "services": {
      "orglane-core": {"loadBalancer": {"servers": [{"url": "http://orglane:8080"}], "passHostHeader": true}}
    },
    "middlewares": {
      "orglane-https-redirect": {"redirectScheme": {"scheme": "https", "permanent": true}},
      "orglane-security-headers": {"headers": {
        "frameDeny": true,
        "contentTypeNosniff": true,
        "referrerPolicy": "strict-origin-when-cross-origin",
        "permissionsPolicy": "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
        "stsSeconds": 31536000
      }}
    }
  }
}`)
}

func TestBuildDomainModeCustomResolverAndHSTS(t *testing.T) {
	cfg := load(t, map[string]string{
		"ORGLANE_DOMAIN_BASE":           "acme.com",
		"ORGLANE_INGRESS_CERT_RESOLVER": "pebble",
		"ORGLANE_INGRESS_HSTS_MAX_AGE":  "0s",
	})
	d := Build(cfg, nil)
	r := d.HTTP.Routers[RouterWorkspace]
	if r.Rule != "Host(`acme.com`)" || r.TLS == nil || r.TLS.CertResolver != "pebble" {
		t.Errorf("workspace router = %+v", r)
	}
	if h := d.HTTP.Middlewares[MiddlewareHeaders].Headers; h.STSSeconds != 0 {
		t.Errorf("stsSeconds = %d, want 0 (HSTS disabled)", h.STSSeconds)
	}
}

func TestBuildLocalMode(t *testing.T) {
	cfg := load(t, map[string]string{
		"ORGLANE_DOMAIN_BASE":           "localhost",
		"ORGLANE_DOMAIN_WORKSPACE_HOST": "localhost:8000",
		"ORGLANE_INGRESS_MODE":          "local",
		"ORGLANE_ENVIRONMENT":           "development",
		"ORGLANE_DOMAIN_SCHEME":         "http",
	})
	// No TLS, no redirect and no HSTS: the browser would pin HTTPS for
	// localhost otherwise.
	assertJSON(t, Build(cfg, nil), `{
  "http": {
    "routers": {
      "orglane-workspace": {
        "entryPoints": ["web"],
        "rule": "Host(`+"`localhost`"+`)",
        "service": "orglane-core",
        "middlewares": ["orglane-security-headers"]
      }
    },
    "services": {
      "orglane-core": {"loadBalancer": {"servers": [{"url": "http://orglane:8080"}], "passHostHeader": true}}
    },
    "middlewares": {
      "orglane-security-headers": {"headers": {
        "frameDeny": true,
        "contentTypeNosniff": true,
        "referrerPolicy": "strict-origin-when-cross-origin",
        "permissionsPolicy": "camera=(), microphone=(), geolocation=(), payment=(), usb=()"
      }}
    }
  }
}`)
}

func TestHandler(t *testing.T) {
	cfg := load(t, map[string]string{"ORGLANE_DOMAIN_BASE": "acme.com"})
	h, err := NewHandler(cfg, nil, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	get := func(method, path string) (code int, header http.Header, body []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, body
	}

	code, header, body := get(http.MethodGet, ConfigPath)
	if code != http.StatusOK || header.Get("Content-Type") != "application/json" || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET %s = %d %v", ConfigPath, code, header)
	}
	var got Dynamic
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := got.HTTP.Routers[RouterWorkspace]; !ok {
		t.Errorf("no workspace router in %s", body)
	}

	if code, _, _ := get(http.MethodPost, ConfigPath); code != http.StatusMethodNotAllowed {
		t.Errorf("POST %s = %d, want 405", ConfigPath, code)
	}
	for _, path := range []string{"/", "/healthz", "/api/v1/instance"} {
		if code, _, _ := get(http.MethodGet, path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: the internal listener serves nothing else", path, code)
		}
	}
}

// TestHandlerAllowedPeers: with server.internal_allowed_peers set, only those
// peers get the configuration; every other peer gets 403, whatever its
// headers say.
func TestHandlerAllowedPeers(t *testing.T) {
	for _, tc := range []struct {
		name, allowed, peer string
		want                int
	}{
		{"allowed peer", "10.231.0.2/32", "10.231.0.2:41234", http.StatusOK},
		{"other peer on the network", "10.231.0.2/32", "10.231.0.130:41234", http.StatusForbidden},
		{"one of several", "10.231.0.2/32, fd00::2/128", "[fd00::2]:41234", http.StatusOK},
		{"IPv4-mapped peer", "10.231.0.2/32", "[::ffff:10.231.0.2]:41234", http.StatusOK},
		{"unparsable peer", "10.231.0.2/32", "pipe", http.StatusForbidden},
		{"default allows any", "", "192.0.2.7:41234", http.StatusOK},
		{"any", "any", "192.0.2.7:41234", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := load(t, map[string]string{"ORGLANE_DOMAIN_BASE": "acme.com", "ORGLANE_SERVER_INTERNAL_ALLOWED_PEERS": tc.allowed})
			h, err := NewHandler(cfg, nil, nil, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, ConfigPath, http.NoBody)
			req.RemoteAddr = tc.peer
			// A forwarded header never makes a peer allowed.
			req.Header.Set("X-Forwarded-For", "10.231.0.2")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("GET %s from %s = %d, want %d", ConfigPath, tc.peer, rec.Code, tc.want)
			}
			if tc.want == http.StatusForbidden && bytes.Contains(rec.Body.Bytes(), []byte("orglane-workspace")) {
				t.Errorf("refused answer carries the configuration: %s", rec.Body)
			}
		})
	}
}

func TestBuildAppRoutesDomainMode(t *testing.T) {
	cfg := load(t, map[string]string{"ORGLANE_DOMAIN_BASE": "acme.com"})
	routes := []AppRoute{
		{Key: "git", Route: "web", Host: "git.acme.com", Upstream: "http://git-forgejo:3000"},
		{Key: "files", Route: "web", Host: "files.acme.com", Maintenance: true},
	}
	d := Build(cfg, routes)
	gotJSON, _ := json.Marshal(d.HTTP.Routers[AppRouter("git", "web")])
	assertRaw(t, gotJSON, `{"entryPoints":["websecure"],"rule":"Host(`+"`git.acme.com`"+`)","service":"orglane-app-git-web",
		"middlewares":["orglane-security-headers"],"tls":{"certResolver":"letsencrypt"}}`)
	gotJSON, _ = json.Marshal(d.HTTP.Routers[AppRouter("git", "web")+"-redirect"])
	assertRaw(t, gotJSON, `{"entryPoints":["web"],"rule":"Host(`+"`git.acme.com`"+`)","service":"noop@internal",
		"middlewares":["orglane-https-redirect"]}`)
	gotJSON, _ = json.Marshal(d.HTTP.Services[AppRouter("git", "web")])
	assertRaw(t, gotJSON, `{"loadBalancer":{"servers":[{"url":"http://git-forgejo:3000"}],"passHostHeader":true}}`)

	// Maintenance: the host goes to the core, marked by the header.
	m := d.HTTP.Routers[AppRouter("files", "web")]
	if m.Service != ServiceCore || !slices.Equal(m.Middlewares, []string{MiddlewareHeaders, MiddlewareMaintenance}) || m.TLS == nil {
		t.Errorf("maintenance router = %+v", m)
	}
	if _, ok := d.HTTP.Services[AppRouter("files", "web")]; ok {
		t.Error("a maintenance route has its own service")
	}
	if h := d.HTTP.Middlewares[MiddlewareMaintenance].Headers; h == nil || h.CustomRequestHeaders[HeaderMaintenance] != "1" {
		t.Errorf("maintenance middleware = %+v", d.HTTP.Middlewares[MiddlewareMaintenance])
	}
	if len(d.HTTP.Routers) != 6 || len(d.HTTP.Services) != 2 {
		t.Errorf("routers %d, services %d", len(d.HTTP.Routers), len(d.HTTP.Services))
	}
}

func assertRaw(t *testing.T, got []byte, want string) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(want)); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	if !bytes.Equal(got, buf.Bytes()) {
		t.Errorf("got %s\nwant %s", got, buf.Bytes())
	}
}

func TestBuildAppRoutesLocalMode(t *testing.T) {
	cfg := load(t, map[string]string{
		"ORGLANE_DOMAIN_BASE":   "orglane.localhost",
		"ORGLANE_INGRESS_MODE":  "local",
		"ORGLANE_ENVIRONMENT":   "development",
		"ORGLANE_DOMAIN_SCHEME": "http",
	})
	d := Build(cfg, []AppRoute{{Key: "git", Route: "web", Host: "git.orglane.localhost", Upstream: "http://git-forgejo:3000"}})
	r := d.HTTP.Routers[AppRouter("git", "web")]
	if !slices.Equal(r.EntryPoints, []string{EntryPointHTTP}) || r.TLS != nil || r.Rule != "Host(`git.orglane.localhost`)" {
		t.Errorf("router = %+v", r)
	}
	if _, ok := d.HTTP.Routers[AppRouter("git", "web")+"-redirect"]; ok {
		t.Error("a redirect router in local mode")
	}
}

// Bad rows never take over a host or inject into a rule.
func TestBuildAppRoutesRejectsBadRoutes(t *testing.T) {
	cfg := load(t, map[string]string{"ORGLANE_DOMAIN_BASE": "acme.com"})
	ok := AppRoute{Key: "git", Route: "web", Host: "git.acme.com", Upstream: "http://git-forgejo:3000"}
	for name, bad := range map[string]AppRoute{
		"workspace host":  {Key: "evil", Route: "web", Host: "acme.com", Upstream: "http://evil:80"},
		"duplicate host":  {Key: "zzz", Route: "web", Host: "git.acme.com", Upstream: "http://zzz-x:80"},
		"rule injection":  {Key: "inj", Route: "web", Host: "x.acme.com`) || Host(`acme.com", Upstream: "http://inj:80"},
		"upper case host": {Key: "up", Route: "web", Host: "Up.acme.com", Upstream: "http://up:80"},
		"bad key":         {Key: "Bad_Key", Route: "web", Host: "bad.acme.com", Upstream: "http://bad:80"},
		"https upstream":  {Key: "tls", Route: "web", Host: "tls.acme.com", Upstream: "https://tls:443"},
		"upstream path":   {Key: "path", Route: "web", Host: "path.acme.com", Upstream: "http://path:80/x"},
		"no port":         {Key: "noport", Route: "web", Host: "noport.acme.com", Upstream: "http://noport"},
		"user info":       {Key: "user", Route: "web", Host: "user.acme.com", Upstream: "http://u:p@user:80"}, //nolint:gosec // a test URL
	} {
		t.Run(name, func(t *testing.T) {
			d := Build(cfg, []AppRoute{ok, bad})
			if r := d.HTTP.Routers[RouterWorkspace]; r.Service != ServiceCore {
				t.Fatalf("workspace router changed: %+v", r)
			}
			if r := d.HTTP.Routers[AppRouter("git", "web")]; r.Service != AppRouter("git", "web") {
				t.Fatalf("good route lost: %+v", r)
			}
			if _, found := d.HTTP.Routers[AppRouter(bad.Key, bad.Route)]; found {
				t.Fatalf("bad route kept: %+v", d.HTTP.Routers[AppRouter(bad.Key, bad.Route)])
			}
			if len(d.HTTP.Services) != 2 {
				t.Fatalf("services = %v", d.HTTP.Services)
			}
		})
	}
}

func TestHandlerAppRoutes(t *testing.T) {
	cfg := load(t, map[string]string{"ORGLANE_DOMAIN_BASE": "acme.com"})
	var routes []AppRoute
	var fail error
	var served [][]AppRoute
	h, err := NewHandler(cfg, func(context.Context) ([]AppRoute, error) { return routes, fail },
		func(r []AppRoute) { served = append(served, r) }, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	poll := func() (int, Dynamic) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, ConfigPath, http.NoBody))
		var d Dynamic
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, d
	}
	if code, d := poll(); code != http.StatusOK || len(d.HTTP.Routers) != 2 {
		t.Fatalf("no apps: %d %v", code, d.HTTP.Routers)
	}
	// Every poll reads the routes again.
	routes = []AppRoute{{Key: "git", Route: "web", Host: "git.acme.com", Upstream: "http://git-forgejo:3000"}}
	if code, d := poll(); code != http.StatusOK || d.HTTP.Routers[AppRouter("git", "web")].Service == "" {
		t.Fatalf("with an app: %d %v", code, d.HTTP.Routers)
	}
	// The served hook gets the app routes of each answer, only those Build
	// included: a route that would take over the workspace host is not
	// published.
	routes = append(routes,
		AppRoute{Key: "evil", Route: "web", Host: cfg.WorkspaceHostname(), Upstream: "http://evil-web:80"},
		AppRoute{Key: "chat", Route: "web", Host: "chat.acme.com", Maintenance: true})
	if code, _ := poll(); code != http.StatusOK {
		t.Fatalf("with apps: %d", code)
	}
	want := [][]AppRoute{nil, routes[:1], {routes[2], routes[0]}}
	if !reflect.DeepEqual(served, want) {
		t.Fatalf("served =\n%+v\nwant\n%+v", served, want)
	}
	// A failing source is a 503, never a configuration without the apps,
	// and nothing is reported served.
	fail = errors.New("database down")
	if code, _ := poll(); code != http.StatusServiceUnavailable {
		t.Fatalf("failing source = %d, want 503", code)
	}
	if len(served) != len(want) {
		t.Fatalf("a failed poll was reported served: %+v", served[len(want):])
	}
}

func TestMaintenance(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := Maintenance("work.acme.com", next)
	for _, tc := range []struct {
		host   string
		header bool
		want   int
	}{
		{"git.acme.com", true, http.StatusServiceUnavailable},
		{"git.acme.com:443", true, http.StatusServiceUnavailable},
		{"git.acme.com", false, http.StatusTeapot},
		// A client sending the header to the workspace changes nothing.
		{"work.acme.com", true, http.StatusTeapot},
		{"Work.Acme.com:443", true, http.StatusTeapot},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		req.Host = tc.host
		if tc.header {
			req.Header.Set(HeaderMaintenance, "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s (header %v) = %d, want %d", tc.host, tc.header, rec.Code, tc.want)
		}
		if tc.want == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") != MaintenanceRetryAfter {
			t.Errorf("no Retry-After: %v", rec.Header())
		}
	}
}
