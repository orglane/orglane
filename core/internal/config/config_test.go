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
	"ORGLANE_DATABASE_URL": "postgres://orglane@localhost:5432/orglane",
	"ORGLANE_DOMAIN_BASE":  "acme.com",
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
