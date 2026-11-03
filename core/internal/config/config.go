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
		Domain:      Domain{ServiceHostTemplate: DefaultServiceHostTemplate},
		Endpoints:   Endpoints{PushRelayURL: DefaultPushRelayURL, DiscoveryURL: DefaultDiscoveryURL},
		Jobs: Jobs{
			MaxWorkers:        5,
			FetchPollInterval: 5 * time.Second,
			Noop:              PeriodicJob{Interval: time.Hour},
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
	return errors.Join(errs...)
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
