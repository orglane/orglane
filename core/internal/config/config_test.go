package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

var minimalEnv = map[string]string{
	"ORGLANE_DATABASE_URL":           "postgres://orglane@localhost:5432/orglane",
	"ORGLANE_DOMAIN_BASE":            "acme.com",
	"ORGLANE_IDENTITY_MASTER_SECRET": "test-master-secret-0123456789abcdef",
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "orglane.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	cfg, err := Load("", env(minimalEnv))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Environment != Production || cfg.Log.Format != "json" || cfg.Log.Level != "info" {
		t.Errorf("environment/log = %q %+v", cfg.Environment, cfg.Log)
	}
	if cfg.Domain.WorkspaceHost != "acme.com" {
		t.Errorf("workspace host = %q, want the base domain", cfg.Domain.WorkspaceHost)
	}
	if got := cfg.ServiceHost("git"); got != "git.acme.com" {
		t.Errorf("ServiceHost(git) = %q, want git.acme.com", got)
	}
	if cfg.Endpoints.PushRelayURL != "https://push.orglane.app" || !strings.HasPrefix(cfg.Endpoints.DiscoveryURL, "https://orglane.app/") {
		t.Errorf("endpoints = %+v", cfg.Endpoints)
	}
	if cfg.Jobs.Noop.Enabled {
		t.Error("the example periodic job must be disabled by default (ADR 0009)")
	}
	if cfg.Domain.Scheme != "https" || cfg.WorkspaceURL() != "https://acme.com" {
		t.Errorf("scheme = %q, workspace URL = %q", cfg.Domain.Scheme, cfg.WorkspaceURL())
	}
	id := cfg.Identity
	if id.SigningKeyRotation != 90*24*time.Hour || id.SigningKeyGrace <= id.IDTokenLifetime ||
		id.SessionIdleTimeout >= id.SessionAbsoluteTimeout || id.SignInAccountLimit != 5 {
		t.Errorf("identity defaults = %+v", id)
	}
	// No proxy is trusted unless configured: X-Forwarded-For is ignored.
	proxies, err := cfg.Server.TrustedProxyPrefixes()
	if err != nil || len(proxies) != 0 {
		t.Errorf("default trusted proxies = %v, %v; want none", proxies, err)
	}
}

func TestMasterSecret(t *testing.T) {
	without := maps.Clone(minimalEnv)
	delete(without, "ORGLANE_IDENTITY_MASTER_SECRET")
	if _, err := Load("", env(without)); err == nil || !strings.Contains(err.Error(), "identity.master_secret (ORGLANE_IDENTITY_MASTER_SECRET): required in production") {
		t.Errorf("production without a master secret: %v", err)
	}

	short := maps.Clone(minimalEnv)
	short["ORGLANE_IDENTITY_MASTER_SECRET"] = "too-short-secret"
	if _, err := Load("", env(short)); err == nil || strings.Contains(err.Error(), "too-short-secret") || !strings.Contains(err.Error(), "at least 32") {
		t.Errorf("short master secret: %v (must fail without echoing the value)", err)
	}

	public := maps.Clone(minimalEnv)
	public["ORGLANE_IDENTITY_MASTER_SECRET"] = DevMasterSecret
	if _, err := Load("", env(public)); err == nil || !strings.Contains(err.Error(), "development secret") {
		t.Errorf("production with the development secret: %v", err)
	}

	dev := maps.Clone(without)
	dev["ORGLANE_ENVIRONMENT"] = "development"
	cfg, err := Load("", env(dev))
	if err != nil || cfg.Identity.MasterSecret != DevMasterSecret {
		t.Errorf("development without a master secret: %v", err)
	}
}

func TestSchemeAndIdentitySettings(t *testing.T) {
	plain := maps.Clone(minimalEnv)
	plain["ORGLANE_DOMAIN_SCHEME"] = "http"
	if _, err := Load("", env(plain)); err == nil || !strings.Contains(err.Error(), "domain.scheme") {
		t.Errorf("http in production: %v", err)
	}
	plain["ORGLANE_ENVIRONMENT"] = "development"
	plain["ORGLANE_DOMAIN_WORKSPACE_HOST"] = "localhost:8080"
	cfg, err := Load("", env(plain))
	if err != nil || cfg.WorkspaceURL() != "http://localhost:8080" {
		t.Errorf("http in development: %v, %v", cfg, err)
	}

	bad := maps.Clone(minimalEnv)
	maps.Copy(bad, map[string]string{
		"ORGLANE_IDENTITY_SIGNING_KEY_ROTATION": "1m",
		"ORGLANE_IDENTITY_SIGNING_KEY_GRACE":    "30m",
		"ORGLANE_IDENTITY_SIGN_IN_IP_LIMIT":     "0",
		"ORGLANE_IDENTITY_SESSION_IDLE_TIMEOUT": "0s",
		"ORGLANE_SERVER_TRUSTED_PROXIES":        "10.0.0.0/8,nope",
	})
	_, err = Load("", env(bad))
	for _, want := range []string{"identity.signing_key_rotation", "identity.signing_key_grace", "identity.sign_in_ip_limit", "identity.session_idle_timeout", "server.trusted_proxies"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}

	traefik := maps.Clone(minimalEnv)
	traefik["ORGLANE_SERVER_TRUSTED_PROXIES"] = " 172.30.0.2/32, fd00::2/128 "
	cfg, err = Load("", env(traefik))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := cfg.Server.TrustedProxyPrefixes(); err != nil || len(p) != 2 || p[0].String() != "172.30.0.2/32" {
		t.Errorf("trusted proxies = %v, %v", p, err)
	}

	none := maps.Clone(minimalEnv)
	none["ORGLANE_SERVER_TRUSTED_PROXIES"] = "none"
	cfg, err = Load("", env(none))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := cfg.Server.TrustedProxyPrefixes(); err != nil || len(p) != 0 {
		t.Errorf("trusted proxies none = %v, %v", p, err)
	}
}

func TestFileAndEnvPrecedence(t *testing.T) {
	path := writeFile(t, `
environment: development
log:
  level: debug
server:
  listen: 127.0.0.1:9000
  shutdown_timeout: 5s
database:
  url: postgres://file@db/orglane
domain:
  base: Acme.COM.
  workspace_host: work.acme.com
  service_host_template: "{service}-svc.{base}"
endpoints:
  push_relay_url: https://push.example.net
jobs:
  noop:
    enabled: true
    interval: 10m
`)
	cfg, err := Load(path, env(map[string]string{
		"ORGLANE_DOMAIN_WORKSPACE_HOST": "office.acme.com",
		"ORGLANE_JOBS_NOOP_INTERVAL":    "2h",
		"ORGLANE_DATABASE_MAX_CONNS":    "3",
	}))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"environment", cfg.Environment, Development},
		{"log.format default in development", cfg.Log.Format, "text"},
		{"log.level", cfg.Log.Level, "debug"},
		{"server.listen", cfg.Server.Listen, "127.0.0.1:9000"},
		{"server.shutdown_timeout", cfg.Server.ShutdownTimeout, 5 * time.Second},
		{"database.url", cfg.Database.URL, "postgres://file@db/orglane"},
		{"database.max_conns from env", cfg.Database.MaxConns, int32(3)},
		{"domain.base normalized", cfg.Domain.Base, "acme.com"},
		{"domain.workspace_host from env", cfg.Domain.WorkspaceHost, "office.acme.com"},
		{"service host", cfg.ServiceHost("git"), "git-svc.acme.com"},
		{"push relay from file", cfg.Endpoints.PushRelayURL, "https://push.example.net"},
		{"discovery default kept", cfg.Endpoints.DiscoveryURL, DefaultDiscoveryURL},
		{"jobs.noop.enabled", cfg.Jobs.Noop.Enabled, true},
		{"jobs.noop.interval from env", cfg.Jobs.Noop.Interval, 2 * time.Hour},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestEmptyFile(t *testing.T) {
	if _, err := Load(writeFile(t, ""), env(minimalEnv)); err != nil {
		t.Fatalf("empty file: %v", err)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		bare    bool // start from an empty environment instead of minimalEnv
		file    string
		env     map[string]string
		want    []string
		notWant string
	}{
		{
			name: "missing required settings",
			bare: true,
			want: []string{"database.url (ORGLANE_DATABASE_URL): required", "domain.base (ORGLANE_DOMAIN_BASE): required"},
		},
		{
			name: "unknown key in file",
			file: "domian:\n  base: acme.com\n",
			want: []string{"field domian not found"},
		},
		{
			name: "bad env values",
			env:  map[string]string{"ORGLANE_SERVER_SHUTDOWN_TIMEOUT": "soon", "ORGLANE_JOBS_NOOP_ENABLED": "maybe", "ORGLANE_JOBS_MAX_WORKERS": "x"},
			want: []string{"ORGLANE_SERVER_SHUTDOWN_TIMEOUT: invalid duration", "ORGLANE_JOBS_NOOP_ENABLED: invalid boolean", "ORGLANE_JOBS_MAX_WORKERS: invalid integer"},
		},
		{
			name: "invalid values",
			env: map[string]string{ //nolint:gosec // fake credentials to check they are not echoed
				"ORGLANE_ENVIRONMENT":                  "staging",
				"ORGLANE_LOG_LEVEL":                    "loud",
				"ORGLANE_LOG_FORMAT":                   "xml",
				"ORGLANE_SERVER_LISTEN":                "8080",
				"ORGLANE_DATABASE_URL":                 "mysql://root:hunter2@db/x",
				"ORGLANE_DOMAIN_BASE":                  "-acme.com",
				"ORGLANE_DOMAIN_WORKSPACE_HOST":        "https://work.acme.com",
				"ORGLANE_DOMAIN_SERVICE_HOST_TEMPLATE": "{base}",
				"ORGLANE_ENDPOINTS_PUSH_RELAY_URL":     "http://push.example.net",
				"ORGLANE_ENDPOINTS_DISCOVERY_URL":      "orglane.app",
				"ORGLANE_JOBS_NOOP_ENABLED":            "true",
				"ORGLANE_JOBS_NOOP_INTERVAL":           "1ms",
			},
			want: []string{
				"environment (ORGLANE_ENVIRONMENT)", "log.level", "log.format", "server.listen",
				"database.url (ORGLANE_DATABASE_URL): must be a postgres://", "domain.base", "domain.workspace_host",
				"domain.service_host_template", "endpoints.push_relay_url", "endpoints.discovery_url", "jobs.noop.interval",
			},
			notWant: "hunter2",
		},
		{
			name: "template with unknown placeholder",
			env:  map[string]string{"ORGLANE_DOMAIN_SERVICE_HOST_TEMPLATE": "{service}.{company}.{base}"},
			want: []string{"only {service} and {base} are supported"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{}
			if !tt.bare {
				maps.Copy(vars, minimalEnv)
			}
			maps.Copy(vars, tt.env)
			path := ""
			if tt.file != "" {
				path = writeFile(t, tt.file)
			}
			_, err := Load(path, env(vars))
			if err == nil {
				t.Fatal("Load() succeeded, want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q:\n%v", w, err)
				}
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("error leaks %q:\n%v", tt.notWant, err)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), env(minimalEnv))
	if err == nil || !strings.Contains(err.Error(), "read config file") {
		t.Fatalf("Load() error = %v, want read error", err)
	}
}

func TestDevelopmentAllowsHTTPEndpointsAndPorts(t *testing.T) {
	cfg, err := Load("", env(map[string]string{
		"ORGLANE_ENVIRONMENT":              "development",
		"ORGLANE_DATABASE_URL":             "host=localhost user=orglane",
		"ORGLANE_DOMAIN_BASE":              "localhost",
		"ORGLANE_DOMAIN_WORKSPACE_HOST":    "localhost:8080",
		"ORGLANE_ENDPOINTS_PUSH_RELAY_URL": "http://localhost:9090",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceHost("git") != "git.localhost" {
		t.Errorf("ServiceHost(git) = %q", cfg.ServiceHost("git"))
	}
}

// TestEveryFieldHasEnvVar guards the reflection-based env mapping: every leaf
// setting must be reachable from the environment.
func TestEveryFieldHasEnvVar(t *testing.T) {
	var seen []string
	_ = applyEnv(reflect.ValueOf(new(Config)).Elem(), EnvPrefix, func(k string) (string, bool) {
		seen = append(seen, k)
		return "", false
	})
	for _, want := range []string{
		"ORGLANE_ENVIRONMENT", "ORGLANE_LOG_LEVEL", "ORGLANE_LOG_FORMAT", "ORGLANE_SERVER_LISTEN",
		"ORGLANE_SERVER_SHUTDOWN_TIMEOUT", "ORGLANE_DATABASE_URL", "ORGLANE_DATABASE_MAX_CONNS",
		"ORGLANE_DATABASE_CONNECT_TIMEOUT", "ORGLANE_DOMAIN_BASE", "ORGLANE_DOMAIN_WORKSPACE_HOST",
		"ORGLANE_DOMAIN_SERVICE_HOST_TEMPLATE", "ORGLANE_ENDPOINTS_PUSH_RELAY_URL", "ORGLANE_ENDPOINTS_DISCOVERY_URL",
		"ORGLANE_JOBS_MAX_WORKERS", "ORGLANE_JOBS_FETCH_POLL_INTERVAL", "ORGLANE_JOBS_NOOP_ENABLED", "ORGLANE_JOBS_NOOP_INTERVAL",
		"ORGLANE_DOMAIN_SCHEME", "ORGLANE_SERVER_TRUSTED_PROXIES", "ORGLANE_IDENTITY_MASTER_SECRET",
		"ORGLANE_IDENTITY_SESSION_IDLE_TIMEOUT", "ORGLANE_IDENTITY_SESSION_ABSOLUTE_TIMEOUT",
		"ORGLANE_IDENTITY_ACCESS_TOKEN_LIFETIME", "ORGLANE_IDENTITY_ID_TOKEN_LIFETIME", "ORGLANE_IDENTITY_REFRESH_TOKEN_LIFETIME",
		"ORGLANE_IDENTITY_SIGNING_KEY_ROTATION", "ORGLANE_IDENTITY_SIGNING_KEY_GRACE",
		"ORGLANE_IDENTITY_SIGN_IN_IP_LIMIT", "ORGLANE_IDENTITY_SIGN_IN_IP_WINDOW",
		"ORGLANE_IDENTITY_SIGN_IN_ACCOUNT_LIMIT", "ORGLANE_IDENTITY_SIGN_IN_LOCKOUT",
	} {
		found := false
		for _, s := range seen {
			found = found || s == want
		}
		if !found {
			t.Errorf("no environment variable %s (saw %v)", want, seen)
		}
	}
}

func TestDemo(t *testing.T) {
	cfg, err := Load("", env(minimalEnv))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Demo.Enabled {
		t.Fatal("demo mode must be off by default")
	}
	vars := maps.Clone(minimalEnv)
	vars["ORGLANE_DEMO_ENABLED"] = "true"
	if cfg, err = Load("", env(vars)); err != nil || !cfg.Demo.Enabled {
		t.Fatalf("ORGLANE_DEMO_ENABLED=true: enabled = %v, err = %v", cfg != nil && cfg.Demo.Enabled, err)
	}
	if cfg, err = Load(writeFile(t, "demo:\n  enabled: true\n"), env(minimalEnv)); err != nil || !cfg.Demo.Enabled {
		t.Fatalf("demo.enabled in the file: err = %v", err)
	}
}
