// Package config loads and validates the core configuration.
//
// Sources, from lowest to highest precedence: built-in defaults, a YAML file,
// environment variables. Every setting has an environment variable named
// ORGLANE_ followed by its YAML path in upper case, joined with underscores:
// domain.workspace_host is ORGLANE_DOMAIN_WORKSPACE_HOST.
//
// This package is the only place that knows default hosts (ADR 0018).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// EnvPrefix prefixes every environment variable.
const EnvPrefix = "ORGLANE_"

// EnvConfigFile names the environment variable holding the config file path.
const EnvConfigFile = EnvPrefix + "CONFIG"

// Environments.
const (
	Production  = "production"
	Development = "development"
)

// Default project endpoints (ADR 0018). Instances can point them elsewhere.
const (
	DefaultPushRelayURL = "https://push.orglane.app"
	DefaultDiscoveryURL = "https://orglane.app/api/discovery"
)

// DefaultServiceHostTemplate builds service hosts such as git.acme.com.
const DefaultServiceHostTemplate = "{service}.{base}"

// Config is the core configuration.
type Config struct {
	// Environment is "production" (default) or "development".
	Environment string    `yaml:"environment"`
	Log         Log       `yaml:"log"`
	Server      Server    `yaml:"server"`
	Database    Database  `yaml:"database"`
	Domain      Domain    `yaml:"domain"`
	Endpoints   Endpoints `yaml:"endpoints"`
	Jobs        Jobs      `yaml:"jobs"`
	Identity    Identity  `yaml:"identity"`
	Demo        Demo      `yaml:"demo"`
}

// Demo configures demo mode (`orglane server --demo`): on an empty database
// the server loads the demo dataset (demo/) instead of offering first-run
// setup, and anyone who can reach the instance may sign in as a demo person
// with one click. The server refuses to start in demo mode on a database
// that holds other data.
type Demo struct {
	// Enabled turns demo mode on. Defaults to false. `orglane server --demo`
	// sets it too.
	Enabled bool `yaml:"enabled"`
}

// Log configures logging.
type Log struct {
	// Level is debug, info (default), warn or error.
	Level string `yaml:"level"`
	// Format is json or text. Defaults to json in production and text in
	// development.
	Format string `yaml:"format"`
}

// Server configures the HTTP server.
type Server struct {
	// Listen is the TCP address to listen on. Defaults to ":8080".
	Listen string `yaml:"listen"`
	// ShutdownTimeout bounds graceful shutdown. Defaults to 15s.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// TrustedProxies is a comma-separated list of the addresses (CIDRs) of
	// the reverse proxies in front of the core, e.g. the Traefik container,
	// "172.30.0.2/32". Only requests from these peers may name the client IP
	// in X-Forwarded-For, which sign-in rate limiting and the audit log use.
	// Empty (the default) or "none" trusts no proxy: the peer address is the
	// client. List exactly the proxies; anything listed can spoof client
	// addresses.
	TrustedProxies string `yaml:"trusted_proxies"`
}

// TrustedProxyPrefixes parses Server.TrustedProxies.
func (s Server) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	raw := strings.TrimSpace(s.TrustedProxies)
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil, nil
	}
	var out []netip.Prefix
	for part := range strings.SplitSeq(raw, ",") {
		p, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("%q is not a CIDR", strings.TrimSpace(part))
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// Database configures PostgreSQL.
type Database struct {
	// URL is a PostgreSQL connection string (URL or key=value). Required.
	URL string `yaml:"url"`
	// MaxConns caps the connection pool. Defaults to 10.
	MaxConns int32 `yaml:"max_conns"`
	// ConnectTimeout is how long startup waits for the database. Defaults
	// to 30s.
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
}

// Domain is the instance domain model (ADR 0018).
type Domain struct {
	// Base is the company base domain, e.g. acme.com. Required.
	Base string `yaml:"base"`
	// WorkspaceHost is the host serving the workspace, e.g. work.acme.com.
	// It may carry a port for local development. Defaults to Base.
	WorkspaceHost string `yaml:"workspace_host"`
	// ServiceHostTemplate builds company service hosts from {service} and
	// {base}. Defaults to "{service}.{base}".
	ServiceHostTemplate string `yaml:"service_host_template"`
	// Scheme is the scheme the workspace is reached with by browsers and
	// clients: https (default) or http. TLS is usually terminated in front of
	// the core (Traefik). http is only allowed in development, for local
	// plain-HTTP demos; it changes the session cookie name (see package
	// identity/web) and allows a non-https OIDC issuer.
	Scheme string `yaml:"scheme"`
}

// Endpoints are the project service endpoints (ADR 0018).
type Endpoints struct {
	// PushRelayURL is the encrypted push relay (ADR 0011).
	PushRelayURL string `yaml:"push_relay_url"`
	// DiscoveryURL is the discovery API for cloud instances.
	DiscoveryURL string `yaml:"discovery_url"`
}

// Jobs configures the in-process, Postgres-backed job queue (ADR 0009).
type Jobs struct {
	// MaxWorkers caps concurrently running jobs. Defaults to 5.
	MaxWorkers int `yaml:"max_workers"`
	// FetchPollInterval is the fallback poll interval for new jobs; new jobs
	// are normally picked up through LISTEN/NOTIFY. Defaults to 5s.
	FetchPollInterval time.Duration `yaml:"fetch_poll_interval"`
	// Noop is an example periodic job. Disabled by default.
	Noop PeriodicJob `yaml:"noop"`
}

// Identity configures the embedded identity provider: sign-in, sessions and
// OIDC (ADR 0003).
type Identity struct {
	// MasterSecret is the instance master secret. Keys for encrypting data at
	// rest (OIDC signing keys), opaque OIDC access tokens and CSRF tokens are
	// derived from it. At least 32 characters, e.g. the output of
	// `openssl rand -base64 32`. Required in production; development falls
	// back to a fixed, publicly known secret with a warning. Never change it
	// on a running instance: encrypted keys become unreadable.
	MasterSecret string `yaml:"master_secret"`
	// SessionIdleTimeout ends a browser session after this long without a
	// request. Defaults to 7 days.
	SessionIdleTimeout time.Duration `yaml:"session_idle_timeout"`
	// SessionAbsoluteTimeout ends a browser session this long after sign-in,
	// active or not. Defaults to 30 days.
	SessionAbsoluteTimeout time.Duration `yaml:"session_absolute_timeout"`
	// AccessTokenLifetime is the lifetime of OIDC access tokens. Defaults to
	// 15 minutes.
	AccessTokenLifetime time.Duration `yaml:"access_token_lifetime"`
	// IDTokenLifetime is the lifetime of OIDC ID tokens. Defaults to 1 hour.
	IDTokenLifetime time.Duration `yaml:"id_token_lifetime"`
	// RefreshTokenLifetime is the absolute lifetime of an OIDC refresh token
	// chain; every refresh rotates the token. Defaults to 30 days.
	RefreshTokenLifetime time.Duration `yaml:"refresh_token_lifetime"`
	// SigningKeyRotation is how long a signing key signs tokens before a new
	// one replaces it. Defaults to 90 days.
	SigningKeyRotation time.Duration `yaml:"signing_key_rotation"`
	// SigningKeyGrace is how long a replaced signing key stays in the JWKS so
	// tokens it signed can still be verified. Must exceed the ID token
	// lifetime. Defaults to 7 days.
	SigningKeyGrace time.Duration `yaml:"signing_key_grace"`
	// SignInIPLimit is how many sign-in attempts one client IP may make per
	// SignInIPWindow. Defaults to 20 per 5 minutes.
	SignInIPLimit  int           `yaml:"sign_in_ip_limit"`
	SignInIPWindow time.Duration `yaml:"sign_in_ip_window"`
	// SignInAccountLimit is how many failed password sign-ins one account
	// (work email) may have in a row before it is locked for
	// SignInLockout. Defaults to 5 and 15 minutes.
	SignInAccountLimit int           `yaml:"sign_in_account_limit"`
	SignInLockout      time.Duration `yaml:"sign_in_lockout"`
}

// DevMasterSecret is the master secret used in development when none is
// configured. It is public, so it protects nothing.
const DevMasterSecret = "orglane-development-master-secret-do-not-use-in-production"

// MinMasterSecretLength is the minimum length of identity.master_secret.
const MinMasterSecretLength = 32

// PeriodicJob configures a periodic job.
type PeriodicJob struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
}

// Default returns the built-in defaults. Domain.Base and Database.URL have no
// default and must be configured.
func Default() Config {
	return Config{
		Environment: Production,
		Log:         Log{Level: "info"},
		Server:      Server{Listen: ":8080", ShutdownTimeout: 15 * time.Second},
		Database:    Database{MaxConns: 10, ConnectTimeout: 30 * time.Second},
		Domain:      Domain{ServiceHostTemplate: DefaultServiceHostTemplate, Scheme: "https"},
		Endpoints:   Endpoints{PushRelayURL: DefaultPushRelayURL, DiscoveryURL: DefaultDiscoveryURL},
		Jobs: Jobs{
			MaxWorkers:        5,
			FetchPollInterval: 5 * time.Second,
			Noop:              PeriodicJob{Interval: time.Hour},
		},
		Identity: Identity{
			SessionIdleTimeout:     7 * 24 * time.Hour,
			SessionAbsoluteTimeout: 30 * 24 * time.Hour,
			AccessTokenLifetime:    15 * time.Minute,
			IDTokenLifetime:        time.Hour,
			RefreshTokenLifetime:   30 * 24 * time.Hour,
			SigningKeyRotation:     90 * 24 * time.Hour,
			SigningKeyGrace:        7 * 24 * time.Hour,
			SignInIPLimit:          20,
			SignInIPWindow:         5 * time.Minute,
			SignInAccountLimit:     5,
			SignInLockout:          15 * time.Minute,
		},
	}
}

// Load builds the configuration from the defaults, the YAML file at path (if
// path is not empty) and the environment, then validates it. lookupEnv is
// usually os.LookupEnv.
func Load(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	cfg := Default()
	if path != "" {
		if err := cfg.loadFile(path); err != nil {
			return nil, err
		}
	}
	if err := applyEnv(reflect.ValueOf(&cfg).Elem(), EnvPrefix, lookupEnv); err != nil {
		return nil, err
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return &cfg, nil
}

func (c *Config) loadFile(path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // the operator chooses the config path
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	return nil
}

// applyEnv overrides fields from environment variables derived from the YAML
// field names.
func applyEnv(v reflect.Value, prefix string, lookupEnv func(string) (string, bool)) error {
	t := v.Type()
	var errs []error
	for i := range t.NumField() {
		field, fv := t.Field(i), v.Field(i)
		name := prefix + strings.ToUpper(strings.Split(field.Tag.Get("yaml"), ",")[0])
		if fv.Kind() == reflect.Struct {
			errs = append(errs, applyEnv(fv, name+"_", lookupEnv))
			continue
		}
		raw, ok := lookupEnv(name)
		if !ok {
			continue
		}
		if err := setFromString(fv, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func setFromString(v reflect.Value, raw string) error {
	if v.Type() == reflect.TypeFor[time.Duration]() {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid duration %q", raw)
		}
		v.SetInt(int64(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid boolean %q", raw)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		v.SetInt(n)
	default:
		return fmt.Errorf("unsupported setting type %s", v.Type())
	}
	return nil
}

func (c *Config) normalize() {
	c.Environment = strings.ToLower(strings.TrimSpace(c.Environment))
	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	c.Log.Format = strings.ToLower(strings.TrimSpace(c.Log.Format))
	if c.Log.Format == "" {
		c.Log.Format = "json"
		if c.Environment == Development {
			c.Log.Format = "text"
		}
	}
	c.Domain.Base = normalizeHost(c.Domain.Base)
	c.Domain.WorkspaceHost = normalizeHost(c.Domain.WorkspaceHost)
	if c.Domain.WorkspaceHost == "" {
		c.Domain.WorkspaceHost = c.Domain.Base
	}
	c.Domain.ServiceHostTemplate = strings.ToLower(strings.TrimSpace(c.Domain.ServiceHostTemplate))
	c.Domain.Scheme = strings.ToLower(strings.TrimSpace(c.Domain.Scheme))
	if c.Identity.MasterSecret == "" && c.Environment == Development {
		c.Identity.MasterSecret = DevMasterSecret
	}
}

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// Validate checks the configuration and reports every problem at once, each
// prefixed with the setting and its environment variable.
func (c *Config) Validate() error {
	var errs []error
	fail := func(setting, format string, args ...any) {
		env := EnvPrefix + strings.ToUpper(strings.ReplaceAll(setting, ".", "_"))
		errs = append(errs, fmt.Errorf("  %s (%s): %s", setting, env, fmt.Sprintf(format, args...)))
	}

	if c.Environment != Production && c.Environment != Development {
		fail("environment", "must be %q or %q, got %q", Production, Development, c.Environment)
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		fail("log.level", "must be debug, info, warn or error, got %q", c.Log.Level)
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		fail("log.format", "must be json or text, got %q", c.Log.Format)
	}

	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		fail("server.listen", "must be host:port or :port, got %q", c.Server.Listen)
	}
	if c.Server.ShutdownTimeout <= 0 {
		fail("server.shutdown_timeout", "must be positive")
	}
	if _, err := c.Server.TrustedProxyPrefixes(); err != nil {
		fail("server.trusted_proxies", "%v (a comma-separated list of CIDRs, or none)", err)
	}

	switch {
	case strings.TrimSpace(c.Database.URL) == "":
		fail("database.url", "required, e.g. postgres://orglane:password@localhost:5432/orglane")
	case strings.Contains(c.Database.URL, "://"):
		// Never echo the value: it usually contains a password.
		if u, err := url.Parse(c.Database.URL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			fail("database.url", "must be a postgres:// or postgresql:// URL")
		}
	}
	if c.Database.MaxConns < 1 {
		fail("database.max_conns", "must be at least 1")
	}
	if c.Database.ConnectTimeout <= 0 {
		fail("database.connect_timeout", "must be positive")
	}

	switch {
	case c.Domain.Base == "":
		fail("domain.base", "required: the company base domain, e.g. acme.com (localhost for local development)")
	case !validHostname(c.Domain.Base):
		fail("domain.base", "%q is not a valid domain name", c.Domain.Base)
	}
	if !validHostPort(c.Domain.WorkspaceHost) {
		fail("domain.workspace_host", "%q is not a valid host (a domain name, optionally with :port)", c.Domain.WorkspaceHost)
	}
	if err := c.validateServiceTemplate(); err != nil {
		fail("domain.service_host_template", "%v", err)
	}
	switch {
	case c.Domain.Scheme == "https":
	case c.Domain.Scheme == "http" && c.Environment == Development:
	default:
		fail("domain.scheme", "must be https (http is allowed in development), got %q", c.Domain.Scheme)
	}

	c.validateEndpoint("endpoints.push_relay_url", c.Endpoints.PushRelayURL, fail)
	c.validateEndpoint("endpoints.discovery_url", c.Endpoints.DiscoveryURL, fail)

	if c.Jobs.MaxWorkers < 1 {
		fail("jobs.max_workers", "must be at least 1")
	}
	if c.Jobs.FetchPollInterval < 100*time.Millisecond {
		fail("jobs.fetch_poll_interval", "must be at least 100ms")
	}
	if c.Jobs.Noop.Enabled && c.Jobs.Noop.Interval < time.Second {
		fail("jobs.noop.interval", "must be at least 1s")
	}
	c.validateIdentity(fail)
	return errors.Join(errs...)
}

func (c *Config) validateIdentity(fail func(string, string, ...any)) {
	id := c.Identity
	switch {
	case id.MasterSecret == "":
		fail("identity.master_secret", "required in production: at least %d random characters, e.g. the output of `openssl rand -base64 32`", MinMasterSecretLength)
	case len(id.MasterSecret) < MinMasterSecretLength:
		// Never echo the value.
		fail("identity.master_secret", "must be at least %d characters", MinMasterSecretLength)
	case id.MasterSecret == DevMasterSecret && c.Environment != Development:
		fail("identity.master_secret", "the public development secret cannot be used in production")
	}
	positive := func(setting string, d time.Duration) {
		if d <= 0 {
			fail(setting, "must be positive")
		}
	}
	positive("identity.session_idle_timeout", id.SessionIdleTimeout)
	positive("identity.session_absolute_timeout", id.SessionAbsoluteTimeout)
	positive("identity.access_token_lifetime", id.AccessTokenLifetime)
	positive("identity.id_token_lifetime", id.IDTokenLifetime)
	positive("identity.refresh_token_lifetime", id.RefreshTokenLifetime)
	positive("identity.sign_in_ip_window", id.SignInIPWindow)
	positive("identity.sign_in_lockout", id.SignInLockout)
	if id.SigningKeyRotation < time.Hour {
		fail("identity.signing_key_rotation", "must be at least 1h")
	}
	if id.SigningKeyGrace <= id.IDTokenLifetime || id.SigningKeyGrace <= id.AccessTokenLifetime {
		fail("identity.signing_key_grace", "must be longer than the ID and access token lifetimes")
	}
	if id.SignInIPLimit < 1 {
		fail("identity.sign_in_ip_limit", "must be at least 1")
	}
	if id.SignInAccountLimit < 1 {
		fail("identity.sign_in_account_limit", "must be at least 1")
	}
}

// WorkspaceURL returns the URL of the workspace, e.g. https://work.acme.com.
// It is the OIDC issuer and the WebAuthn origin.
func (c *Config) WorkspaceURL() string {
	return c.Domain.Scheme + "://" + c.Domain.WorkspaceHost
}

func (c *Config) validateServiceTemplate() error {
	tmpl := c.Domain.ServiceHostTemplate
	if !strings.Contains(tmpl, "{service}") {
		return fmt.Errorf("%q must contain {service}", tmpl)
	}
	rest := strings.NewReplacer("{service}", "", "{base}", "").Replace(tmpl)
	if strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("%q: only {service} and {base} are supported", tmpl)
	}
	if c.Domain.Base != "" {
		if host := c.ServiceHost("git"); !validHostname(host) {
			return fmt.Errorf("%q produces the invalid host %q", tmpl, host)
		}
	}
	return nil
}

func (c *Config) validateEndpoint(setting, raw string, fail func(string, string, ...any)) {
	u, err := url.Parse(raw)
	switch {
	case raw == "":
		fail(setting, "required")
	case err != nil || u.Host == "":
		fail(setting, "%q is not an absolute URL", raw)
	case u.Scheme == "https":
	case u.Scheme == "http" && c.Environment == Development:
	default:
		fail(setting, "%q must use https (http is allowed in development)", raw)
	}
}

// ServiceHost returns the host of a company service, e.g. ServiceHost("git")
// is git.acme.com with the default template.
func (c *Config) ServiceHost(service string) string {
	return strings.NewReplacer("{service}", service, "{base}", c.Domain.Base).Replace(c.Domain.ServiceHostTemplate)
}

func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

func validHostPort(hp string) bool {
	host, port, err := net.SplitHostPort(hp)
	if err != nil {
		return validHostname(hp)
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536 && validHostname(host)
}
