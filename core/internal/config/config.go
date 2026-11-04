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
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

// DefaultWorkEmailTemplate builds work emails such as maya@acme.com.
const DefaultWorkEmailTemplate = "{username}@{base}"

// Config is the core configuration.
type Config struct {
	// Environment is "production" (default) or "development".
	Environment string    `yaml:"environment"`
	Log         Log       `yaml:"log"`
	Server      Server    `yaml:"server"`
	Database    Database  `yaml:"database"`
	Domain      Domain    `yaml:"domain"`
	Endpoints   Endpoints `yaml:"endpoints"`
	Ingress     Ingress   `yaml:"ingress"`
	Jobs        Jobs      `yaml:"jobs"`
	Identity    Identity  `yaml:"identity"`
	Email       Email     `yaml:"email"`
	Demo        Demo      `yaml:"demo"`
	Agent       Agent     `yaml:"agent"`
}

// Agent configures managed services: the core's connection to the agent
// (socket, secret, sync_interval) and the agent itself (`orglane agent`, the
// other keys). See docs/specs/agent.md "Configuration".
type Agent struct {
	// Socket is the agent's socket path, as the core sees it. Empty, or an
	// empty secret, disables managed services.
	Socket string `yaml:"socket"`
	// Secret is the shared secret of the core and the agent (at least 32
	// characters). It is never the master secret (ADR 0019).
	Secret string `yaml:"secret"`
	// SyncInterval is how often the core syncs instance status and re-pushes
	// lagging desired state. Defaults to 5m.
	SyncInterval time.Duration `yaml:"sync_interval"`

	// Listen is the socket the agent listens on. Its directory must hold
	// nothing else: the agent sets its mode and group.
	Listen string `yaml:"listen"`
	// DataDir holds the instances (instances/<key>) and the agent state
	// (instances/.agent). Mounted at the same path as on the host.
	DataDir string `yaml:"data_dir"`
	// PeerUID is the uid allowed to connect (the core, 65532); -1 disables
	// the check (tests, development).
	PeerUID int `yaml:"peer_uid"`
	// ComposeProject is the bundle's Compose project name.
	ComposeProject string `yaml:"compose_project"`
	// PostgresService and PostgresUser find and use the bundle's PostgreSQL.
	PostgresService string `yaml:"postgres_service"`
	PostgresUser    string `yaml:"postgres_user"`
	// EdgeNetwork and ServicesNetwork default to <compose_project>_edge and
	// <compose_project>_services.
	EdgeNetwork     string `yaml:"edge_network"`
	ServicesNetwork string `yaml:"services_network"`
	// PublishAddress is the host address published instance ports bind to.
	PublishAddress string `yaml:"publish_address"`
	// ReconcileInterval is how often the agent reconciles every instance.
	ReconcileInterval time.Duration `yaml:"reconcile_interval"`
	// SnapshotRetention is how many upgrade snapshots are kept per instance.
	SnapshotRetention int `yaml:"snapshot_retention"`
}

// Enabled reports whether the core uses managed services.
func (a Agent) Enabled() bool { return a.Socket != "" && a.Secret != "" }

// MinAgentSecretLength is the minimum length of agent.secret.
const MinAgentSecretLength = 32

// Agent defaults.
const (
	DefaultAgentListen  = "/run/orglane-agent/agent.sock"
	DefaultAgentDataDir = "/var/lib/orglane"
	DefaultAgentPeerUID = 65532
)

// SMTP TLS modes (Email.SMTP.TLS).
const (
	// SMTPStartTLS upgrades a plain connection with STARTTLS and refuses to
	// continue without it (submission, port 587).
	SMTPStartTLS = "starttls"
	// SMTPImplicitTLS connects with TLS from the start (port 465).
	SMTPImplicitTLS = "tls"
	// SMTPNoTLS sends in the clear. Only for a relay on the same host or a
	// trusted network, such as Mailpit in development; authentication is
	// refused over it except to localhost.
	SMTPNoTLS = "none"
)

// DefaultSMTPPort returns the usual port of a TLS mode.
func DefaultSMTPPort(tlsMode string) int {
	switch tlsMode {
	case SMTPImplicitTLS:
		return 465
	case SMTPNoTLS:
		return 25
	default:
		return 587
	}
}

// Email configures system email: invitations and notifications, sent
// through SMTP (ADR 0014), separate from employee mailboxes.
//
// The SMTP server is normally set by an Owner or Admin in the web UI
// (Settings → Email). Setting email.smtp.host here instead manages it from
// the configuration (useful for installs and development): the UI then
// shows it read-only. Without either, invitations produce a link to share.
type Email struct {
	SMTP SMTP `yaml:"smtp"`
	// From is the sender, e.g. "Acme <no-reply@acme.com>". Required with
	// email.smtp.host.
	From string `yaml:"from"`
	// MaxAttempts is how often a message is tried before it is given up.
	// Defaults to 6.
	MaxAttempts int `yaml:"max_attempts"`
	// RetryDelay is the wait after the first failed attempt; it doubles
	// after every further failure (30s, 1m, 2m, ... by default).
	RetryDelay time.Duration `yaml:"retry_delay"`
}

// SMTP is an SMTP server for system email.
type SMTP struct {
	// Host is the SMTP server. Empty (the default) leaves email to the
	// settings in the web UI.
	Host string `yaml:"host"`
	// Port defaults to 587 for starttls, 465 for tls and 25 for none.
	Port int `yaml:"port"`
	// TLS is starttls (default), tls or none.
	TLS      string `yaml:"tls"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Configured reports whether the configuration sets the SMTP server.
func (e Email) Configured() bool { return e.SMTP.Host != "" }

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
	// Listen is the TCP address of the public listener (API, web app).
	// Defaults to ":8080".
	Listen string `yaml:"listen"`
	// InternalListen is the TCP address of the internal listener, which
	// serves the Traefik dynamic configuration (ADR 0008). It must only be
	// reachable from Traefik, never published. Empty (the default) disables
	// it.
	InternalListen string `yaml:"internal_listen"`
	// InternalAllowedPeers is a comma-separated list of the addresses
	// (CIDRs) that may use the internal listener, e.g. the Traefik
	// container, "172.30.0.2/32"; requests from other peers get 403. Empty
	// (the default) or "any" allows every peer that can reach the listener.
	// The Compose bundle sets Traefik's fixed address: managed services
	// share the network with the core (docs/specs/agent.md).
	InternalAllowedPeers string `yaml:"internal_allowed_peers"`
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
	return parsePrefixes(raw)
}

// InternalAllowedPeerPrefixes parses Server.InternalAllowedPeers. No
// prefixes means every peer is allowed.
func (s Server) InternalAllowedPeerPrefixes() ([]netip.Prefix, error) {
	raw := strings.TrimSpace(s.InternalAllowedPeers)
	if raw == "" || strings.EqualFold(raw, "any") {
		return nil, nil
	}
	return parsePrefixes(raw)
}

// parsePrefixes parses a comma-separated list of CIDRs.
func parsePrefixes(raw string) ([]netip.Prefix, error) {
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
	// WorkEmailTemplate builds the default work email of a new person from
	// {username} and {base}. Defaults to "{username}@{base}"; admins can
	// change the address of each person.
	WorkEmailTemplate string `yaml:"work_email_template"`
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

// Ingress modes.
const (
	// IngressDomain serves the workspace over HTTPS with certificates from
	// an ACME certificate resolver and redirects HTTP to HTTPS.
	IngressDomain = "domain"
	// IngressLocal serves the workspace over plain HTTP, for trying Orglane
	// on localhost (--demo) without a domain.
	IngressLocal = "local"
)

// DefaultCertResolver is the Traefik certificate resolver used in domain
// mode (deploy/compose defines it for Let's Encrypt).
const DefaultCertResolver = "letsencrypt"

// Ingress configures the routes the core hands to Traefik through the
// internal listener (ADR 0008). It only takes effect when
// server.internal_listen is set.
type Ingress struct {
	// Mode is "domain" (default: HTTPS, HTTP redirected to HTTPS, HSTS) or
	// "local" (plain HTTP on localhost, no TLS).
	Mode string `yaml:"mode"`
	// UpstreamURL is where Traefik reaches the core's public listener, e.g.
	// http://orglane:8080. Required when server.internal_listen is set.
	UpstreamURL string `yaml:"upstream_url"`
	// CertResolver names the Traefik certificate resolver used in domain
	// mode. Defaults to "letsencrypt".
	CertResolver string `yaml:"cert_resolver"`
	// HSTSMaxAge is the Strict-Transport-Security max-age in domain mode.
	// Defaults to one year; 0 disables the header.
	HSTSMaxAge time.Duration `yaml:"hsts_max_age"`
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
	// InvitationLifetime is how long an invitation link stays valid.
	// Resending an invitation issues a new link. Defaults to 7 days.
	InvitationLifetime time.Duration `yaml:"invitation_lifetime"`
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
		Domain:      Domain{ServiceHostTemplate: DefaultServiceHostTemplate, WorkEmailTemplate: DefaultWorkEmailTemplate, Scheme: "https"},
		Endpoints:   Endpoints{PushRelayURL: DefaultPushRelayURL, DiscoveryURL: DefaultDiscoveryURL},
		Ingress:     Ingress{Mode: IngressDomain, CertResolver: DefaultCertResolver, HSTSMaxAge: 365 * 24 * time.Hour},
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
			InvitationLifetime:     7 * 24 * time.Hour,
		},
		Email: Email{
			SMTP:        SMTP{TLS: SMTPStartTLS},
			MaxAttempts: 6,
			RetryDelay:  30 * time.Second,
		},
		Agent: Agent{
			SyncInterval:      5 * time.Minute,
			Listen:            DefaultAgentListen,
			DataDir:           DefaultAgentDataDir,
			PeerUID:           DefaultAgentPeerUID,
			ComposeProject:    "orglane",
			PostgresService:   "postgres",
			PostgresUser:      "orglane",
			PublishAddress:    "127.0.0.1",
			ReconcileInterval: 10 * time.Minute,
			SnapshotRetention: 2,
		},
	}
}

// Load builds the configuration from the defaults, the YAML file at path (if
// path is not empty) and the environment, then validates it. lookupEnv is
// usually os.LookupEnv.
func Load(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	return load(path, lookupEnv, (*Config).Validate)
}

// LoadAgent is Load for `orglane agent`: it validates only the log and
// agent settings, so the agent needs no database or domain settings.
func LoadAgent(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	return load(path, lookupEnv, (*Config).ValidateAgent)
}

func load(path string, lookupEnv func(string) (string, bool), validate func(*Config) error) (*Config, error) {
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
	if err := validate(&cfg); err != nil {
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
	c.Domain.WorkEmailTemplate = strings.ToLower(strings.TrimSpace(c.Domain.WorkEmailTemplate))
	c.Email.SMTP.Host = normalizeHost(c.Email.SMTP.Host)
	c.Email.SMTP.TLS = strings.ToLower(strings.TrimSpace(c.Email.SMTP.TLS))
	c.Email.From = strings.TrimSpace(c.Email.From)
	if c.Email.SMTP.Port == 0 {
		c.Email.SMTP.Port = DefaultSMTPPort(c.Email.SMTP.TLS)
	}
	c.Domain.Scheme = strings.ToLower(strings.TrimSpace(c.Domain.Scheme))
	if c.Identity.MasterSecret == "" && c.Environment == Development {
		c.Identity.MasterSecret = DevMasterSecret
	}
	c.Server.InternalListen = strings.TrimSpace(c.Server.InternalListen)
	c.Ingress.Mode = strings.ToLower(strings.TrimSpace(c.Ingress.Mode))
	c.Ingress.UpstreamURL = strings.TrimSpace(c.Ingress.UpstreamURL)
	c.Ingress.CertResolver = strings.TrimSpace(c.Ingress.CertResolver)
	c.Agent.Socket = strings.TrimSpace(c.Agent.Socket)
	c.Agent.Listen = strings.TrimSpace(c.Agent.Listen)
	c.Agent.DataDir = strings.TrimSpace(c.Agent.DataDir)
	c.Agent.ComposeProject = strings.TrimSpace(c.Agent.ComposeProject)
	if c.Agent.EdgeNetwork == "" {
		c.Agent.EdgeNetwork = c.Agent.ComposeProject + "_edge"
	}
	if c.Agent.ServicesNetwork == "" {
		c.Agent.ServicesNetwork = c.Agent.ComposeProject + "_services"
	}
	c.Agent.PublishAddress = strings.TrimSpace(c.Agent.PublishAddress)
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
	if c.Server.InternalListen != "" {
		if _, _, err := net.SplitHostPort(c.Server.InternalListen); err != nil {
			fail("server.internal_listen", "must be host:port or :port, got %q", c.Server.InternalListen)
		} else if c.Server.InternalListen == c.Server.Listen {
			fail("server.internal_listen", "must differ from server.listen")
		}
	}
	if c.Server.ShutdownTimeout <= 0 {
		fail("server.shutdown_timeout", "must be positive")
	}
	if _, err := c.Server.TrustedProxyPrefixes(); err != nil {
		fail("server.trusted_proxies", "%v (a comma-separated list of CIDRs, or none)", err)
	}
	if _, err := c.Server.InternalAllowedPeerPrefixes(); err != nil {
		fail("server.internal_allowed_peers", "%v (a comma-separated list of CIDRs, or any)", err)
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
	if err := c.validateWorkEmailTemplate(); err != nil {
		fail("domain.work_email_template", "%v", err)
	}
	switch {
	case c.Domain.Scheme == "https":
	case c.Domain.Scheme == "http" && c.Environment == Development:
	default:
		fail("domain.scheme", "must be https (http is allowed in development), got %q", c.Domain.Scheme)
	}

	c.validateIngress(fail)

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
	c.validateEmail(fail)
	if c.Agent.Secret != "" && len(c.Agent.Secret) < MinAgentSecretLength {
		fail("agent.secret", "must be at least %d characters (empty disables managed services)", MinAgentSecretLength)
	}
	if c.Agent.Socket != "" && !filepath.IsAbs(c.Agent.Socket) {
		fail("agent.socket", "must be an absolute path")
	}
	if c.Agent.SyncInterval < 10*time.Second {
		fail("agent.sync_interval", "must be at least 10s")
	}
	return errors.Join(errs...)
}

// composeNamePattern is the pattern of Compose project and network names.
var composeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// postgresNamePattern restricts the PostgreSQL service and user names the
// agent passes to docker exec.
var postgresNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,62}$`)

// ValidateAgent checks the settings of `orglane agent` and reports every
// problem at once.
func (c *Config) ValidateAgent() error {
	var errs []error
	fail := func(setting, format string, args ...any) {
		env := EnvPrefix + strings.ToUpper(strings.ReplaceAll(setting, ".", "_"))
		errs = append(errs, fmt.Errorf("  %s (%s): %s", setting, env, fmt.Sprintf(format, args...)))
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		fail("log.level", "must be debug, info, warn or error, got %q", c.Log.Level)
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		fail("log.format", "must be json or text, got %q", c.Log.Format)
	}
	a := c.Agent
	if len(a.Secret) < MinAgentSecretLength {
		fail("agent.secret", "required, at least %d characters (the same value as the core's)", MinAgentSecretLength)
	}
	if !filepath.IsAbs(a.Listen) {
		fail("agent.listen", "must be an absolute path")
	}
	if !filepath.IsAbs(a.DataDir) {
		fail("agent.data_dir", "must be an absolute path")
	}
	if a.PeerUID < -1 {
		fail("agent.peer_uid", "must be a uid, or -1 to disable the check")
	}
	if !composeNamePattern.MatchString(a.ComposeProject) {
		fail("agent.compose_project", "must be a Compose project name (lowercase letters, digits, - and _)")
	}
	if !composeNamePattern.MatchString(a.EdgeNetwork) {
		fail("agent.edge_network", "must be a Docker network name")
	}
	if !composeNamePattern.MatchString(a.ServicesNetwork) {
		fail("agent.services_network", "must be a Docker network name")
	}
	if !postgresNamePattern.MatchString(a.PostgresService) {
		fail("agent.postgres_service", "must be a Compose service name")
	}
	if !postgresNamePattern.MatchString(a.PostgresUser) {
		fail("agent.postgres_user", "must be a PostgreSQL role name")
	}
	if addr, err := netip.ParseAddr(a.PublishAddress); err != nil || addr.Zone() != "" {
		fail("agent.publish_address", "must be an IP address, got %q", a.PublishAddress)
	}
	if a.ReconcileInterval < time.Second {
		fail("agent.reconcile_interval", "must be at least 1s")
	}
	if a.SnapshotRetention < 1 {
		fail("agent.snapshot_retention", "must be at least 1")
	}
	return errors.Join(errs...)
}

func (c *Config) validateEmail(fail func(string, string, ...any)) {
	e := c.Email
	if e.MaxAttempts < 1 || e.MaxAttempts > 25 {
		fail("email.max_attempts", "must be between 1 and 25")
	}
	if e.RetryDelay < time.Second {
		fail("email.retry_delay", "must be at least 1s")
	}
	if !slices.Contains([]string{SMTPStartTLS, SMTPImplicitTLS, SMTPNoTLS}, e.SMTP.TLS) {
		fail("email.smtp.tls", "must be %s, %s or %s, got %q", SMTPStartTLS, SMTPImplicitTLS, SMTPNoTLS, e.SMTP.TLS)
	}
	if e.SMTP.Port < 1 || e.SMTP.Port > 65535 {
		fail("email.smtp.port", "must be between 1 and 65535")
	}
	if !e.Configured() {
		if e.From != "" || e.SMTP.Username != "" || e.SMTP.Password != "" {
			fail("email.smtp.host", "required when other email settings are set")
		}
		return
	}
	if !validHostname(e.SMTP.Host) {
		if _, err := netip.ParseAddr(e.SMTP.Host); err != nil {
			fail("email.smtp.host", "%q is not a valid host name or IP address", e.SMTP.Host)
		}
	}
	if (e.SMTP.Username == "") != (e.SMTP.Password == "") {
		// Never echo the values.
		fail("email.smtp.username", "username and password go together: set both or neither")
	}
	if e.From == "" {
		fail("email.from", "required with email.smtp.host, e.g. \"Acme <no-reply@acme.com>\"")
	} else if _, err := mail.ParseAddress(e.From); err != nil || strings.ContainsAny(e.From, "\r\n") {
		fail("email.from", "%q is not an email address such as \"Acme <no-reply@acme.com>\"", e.From)
	}
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
	if id.InvitationLifetime < time.Hour || id.InvitationLifetime > 90*24*time.Hour {
		fail("identity.invitation_lifetime", "must be between 1h and 90 days")
	}
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

func (c *Config) validateWorkEmailTemplate() error {
	tmpl := c.Domain.WorkEmailTemplate
	if !strings.Contains(tmpl, "{username}") {
		return fmt.Errorf("%q must contain {username}", tmpl)
	}
	rest := strings.NewReplacer("{username}", "", "{base}", "").Replace(tmpl)
	if strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("%q: only {username} and {base} are supported", tmpl)
	}
	if c.Domain.Base != "" {
		email := c.WorkEmail("maya.okafor")
		local, domain, ok := strings.Cut(email, "@")
		if !ok || local == "" || strings.Contains(domain, "@") || !validHostname(domain) {
			return fmt.Errorf("%q produces the invalid address %q", tmpl, email)
		}
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
			return fmt.Errorf("%q produces the invalid address %q", tmpl, email)
		}
	}
	return nil
}

// WorkEmail returns the default work email of a person, e.g.
// WorkEmail("maya") is maya@acme.com with the default template.
func (c *Config) WorkEmail(username string) string {
	return strings.NewReplacer("{username}", username, "{base}", c.Domain.Base).Replace(c.Domain.WorkEmailTemplate)
}

func (c *Config) validateIngress(fail func(string, string, ...any)) {
	if c.Ingress.Mode != IngressDomain && c.Ingress.Mode != IngressLocal {
		fail("ingress.mode", "must be %q or %q, got %q", IngressDomain, IngressLocal, c.Ingress.Mode)
	}
	if c.Ingress.HSTSMaxAge < 0 {
		fail("ingress.hsts_max_age", "must not be negative")
	}
	if c.Server.InternalListen == "" {
		// Nothing is handed to Traefik, so the rest does not apply.
		return
	}
	u, err := url.Parse(c.Ingress.UpstreamURL)
	switch {
	case c.Ingress.UpstreamURL == "":
		fail("ingress.upstream_url", "required when server.internal_listen is set, e.g. http://orglane:8080")
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		fail("ingress.upstream_url", "%q must be an http:// or https:// URL without a path, e.g. http://orglane:8080", c.Ingress.UpstreamURL)
	}
	if c.Ingress.Mode == IngressLocal {
		// Traefik serves plain HTTP: browsers reach http://<workspace host>.
		if c.Domain.Scheme != "http" {
			fail("domain.scheme", "must be http in local ingress mode (no TLS), got %q", c.Domain.Scheme)
		}
		return
	}
	if c.Ingress.Mode != IngressDomain {
		return
	}
	if c.Domain.Scheme != "https" {
		fail("domain.scheme", "must be https in domain ingress mode, got %q", c.Domain.Scheme)
	}
	if c.Environment != Production {
		fail("environment", "must be %q in domain ingress mode: a public workspace is a production instance", Production)
	}
	if c.Ingress.CertResolver == "" {
		fail("ingress.cert_resolver", "required in domain mode")
	}
	host := c.Domain.WorkspaceHost
	switch {
	case strings.Contains(host, ":"):
		fail("domain.workspace_host", "%q must not carry a port in domain ingress mode (HTTPS is served on 443)", host)
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		fail("domain.workspace_host", "%q cannot get a public certificate: use ingress.mode %q on localhost", host, IngressLocal)
	}
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

// WorkspaceHostname returns the workspace host without a port.
func (c *Config) WorkspaceHostname() string {
	if host, _, err := net.SplitHostPort(c.Domain.WorkspaceHost); err == nil {
		return host
	}
	return c.Domain.WorkspaceHost
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
