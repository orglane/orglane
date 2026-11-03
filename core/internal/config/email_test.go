package config

import (
	"maps"
	"strings"
	"testing"
)

func TestEmailConfiguration(t *testing.T) {
	vars := maps.Clone(minimalEnv)
	vars["ORGLANE_EMAIL_SMTP_HOST"] = "SMTP.EXAMPLE.TEST"
	vars["ORGLANE_EMAIL_SMTP_TLS"] = "tls"
	vars["ORGLANE_EMAIL_FROM"] = "Orglane <no-reply@acme.test>"
	vars["ORGLANE_EMAIL_SMTP_USERNAME"] = "mailer"
	vars["ORGLANE_EMAIL_SMTP_PASSWORD"] = "test-only-password"
	vars["ORGLANE_DOMAIN_WORK_EMAIL_TEMPLATE"] = "{username}@staff.{base}"
	cfg, err := Load("", env(vars))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Email.Configured() || cfg.Email.SMTP.Host != "smtp.example.test" || cfg.Email.SMTP.Port != 465 || cfg.WorkEmail("maya") != "maya@staff.acme.com" {
		t.Fatal("email configuration not applied")
	}
	for key, value := range map[string]string{
		"ORGLANE_EMAIL_SMTP_TLS": "optional", "ORGLANE_EMAIL_SMTP_PORT": "65536", "ORGLANE_EMAIL_MAX_ATTEMPTS": "0", "ORGLANE_EMAIL_RETRY_DELAY": "0s", "ORGLANE_EMAIL_FROM": "broken", "ORGLANE_DOMAIN_WORK_EMAIL_TEMPLATE": "{unknown}@{base}",
	} {
		t.Run(key, func(t *testing.T) {
			bad := maps.Clone(vars)
			bad[key] = value
			if _, err := Load("", env(bad)); err == nil || strings.Contains(err.Error(), "test-only-password") {
				t.Fatalf("expected safe validation error: %v", err)
			}
		})
	}
}
