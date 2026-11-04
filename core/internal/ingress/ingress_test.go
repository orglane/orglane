package ingress

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
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
	assertJSON(t, Build(cfg), `{
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
	d := Build(cfg)
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
	assertJSON(t, Build(cfg), `{
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
	h, err := NewHandler(cfg, slog.New(slog.DiscardHandler))
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
			h, err := NewHandler(cfg, slog.New(slog.DiscardHandler))
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
