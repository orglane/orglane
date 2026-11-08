package web

import (
	"testing"

	"github.com/orglane/orglane/core/internal/config"
)

// Plain-http redirect URIs on the local service domain are enabled only for
// a workspace that itself runs on http in local ingress mode. An https
// workspace never gets them, also in configurations that config.Load
// accepts without an internal listener (where it does not check the
// mode and scheme together).
func TestLocalServiceDomain(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"local http workspace", map[string]string{
			"ORGLANE_ENVIRONMENT": "development", "ORGLANE_DOMAIN_SCHEME": "http", "ORGLANE_INGRESS_MODE": "local",
			"ORGLANE_SERVER_INTERNAL_LISTEN": ":9090", "ORGLANE_INGRESS_UPSTREAM_URL": "http://orglane:8080",
		}, "orglane.localhost"},
		{"local mode, https workspace, no internal listener", map[string]string{
			"ORGLANE_ENVIRONMENT": "production", "ORGLANE_DOMAIN_SCHEME": "https", "ORGLANE_INGRESS_MODE": "local",
			"ORGLANE_SERVER_INTERNAL_LISTEN": "",
		}, ""},
		{"domain mode", map[string]string{
			"ORGLANE_ENVIRONMENT": "production", "ORGLANE_DOMAIN_SCHEME": "https", "ORGLANE_INGRESS_MODE": "domain",
			"ORGLANE_SERVER_INTERNAL_LISTEN": "",
		}, ""},
		{"http workspace in domain mode without a listener", map[string]string{
			"ORGLANE_ENVIRONMENT": "development", "ORGLANE_DOMAIN_SCHEME": "http", "ORGLANE_INGRESS_MODE": "domain",
			"ORGLANE_SERVER_INTERNAL_LISTEN": "",
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := map[string]string{
				"ORGLANE_DATABASE_URL":           "postgres://orglane@postgres:5432/orglane",
				"ORGLANE_DOMAIN_BASE":            "orglane.localhost",
				"ORGLANE_IDENTITY_MASTER_SECRET": "test-master-secret-0123456789abcdef",
			}
			for k, v := range tc.vars {
				vars[k] = v
			}
			cfg, err := config.Load("", func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
			if err != nil {
				t.Fatalf("config refused: %v", err)
			}
			if got := localServiceDomain(cfg); got != tc.want {
				t.Fatalf("local service domain = %q, want %q", got, tc.want)
			}
		})
	}
}
