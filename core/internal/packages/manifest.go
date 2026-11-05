package packages

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// ManifestFormat is the only supported manifest format.
const ManifestFormat = 1

// Setting types.
const (
	SettingInt    = "int"
	SettingString = "string"
	SettingBool   = "bool"
)

// Manifest describes one managed package version (docs/specs/agent.md
// "Manifest"). It is decoded strictly: unknown fields are errors.
type Manifest struct {
	Format          int               `yaml:"format"`
	ID              string            `yaml:"id"`
	Version         string            `yaml:"version"`
	Provider        string            `yaml:"provider"`
	Images          map[string]string `yaml:"images"`
	Services        []string          `yaml:"services"`
	Routes          []Route           `yaml:"routes"`
	Ports           []Port            `yaml:"ports"`
	Database        Database          `yaml:"database"`
	DataDirs        []DataDir         `yaml:"data_dirs"`
	Settings        []Setting         `yaml:"settings"`
	Secrets         []string          `yaml:"secrets"`
	CapabilitiesAdd []string          `yaml:"capabilities_add"`
	Resources       Resources         `yaml:"resources"`
	Health          Health            `yaml:"health"`
	Hooks           Hooks             `yaml:"hooks"`
	Backup          Backup            `yaml:"backup"`
	Upgrade         Upgrade           `yaml:"upgrade"`
}

// Route is a public HTTP route to a service.
type Route struct {
	Name      string `yaml:"name"`
	Service   string `yaml:"service"`
	Port      int    `yaml:"port"`
	HostLabel string `yaml:"host_label"`
}

// Port is a host port the project may publish.
type Port struct {
	Name      string `yaml:"name"`
	Service   string `yaml:"service"`
	Container int    `yaml:"container"`
	Setting   string `yaml:"setting"`
}

// Database declares a per-service database on the shared PostgreSQL.
type Database struct {
	Enabled bool `yaml:"enabled"`
}

// DataDir is a bind-mounted data directory under instances/<key>/data/.
type DataDir struct {
	Name string `yaml:"name"`
	// Owner is "<uid>:<gid>"; empty keeps the agent's user.
	Owner string `yaml:"owner"`
	// Mode is an octal permission string such as "0750".
	Mode string `yaml:"mode"`
}

// Setting is a typed input from the core.
type Setting struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"`
	Min     *int64 `yaml:"min"`
	Max     *int64 `yaml:"max"`
	Pattern string `yaml:"pattern"`
	Default any    `yaml:"default"`
}

// Resources are hints, not limits (ADR 0009).
type Resources struct {
	Memory struct {
		Min         Quantity `yaml:"min"`
		Recommended Quantity `yaml:"recommended"`
	} `yaml:"memory"`
	Disk struct {
		Min Quantity `yaml:"min"`
	} `yaml:"disk"`
}

// Health configures the start of the services.
type Health struct {
	StartTimeout time.Duration `yaml:"start_timeout"`
}

// Hook runs a fixed command in a service with `docker compose exec`.
type Hook struct {
	Service string        `yaml:"service"`
	Command []string      `yaml:"command"`
	Timeout time.Duration `yaml:"timeout"`
}

// Hooks are the package's lifecycle hooks.
type Hooks struct {
	Configure    []Hook `yaml:"configure"`
	PreSnapshot  []Hook `yaml:"pre_snapshot"`
	PostSnapshot []Hook `yaml:"post_snapshot"`
}

// Backup declares what a snapshot contains.
type Backup struct {
	Database     bool     `yaml:"database"`
	DataDirs     []string `yaml:"data_dirs"`
	StopServices []string `yaml:"stop_services"`
}

// Upgrade declares the upgrade path of the version.
type Upgrade struct {
	MinFrom        string        `yaml:"min_from"`
	Migrate        *Hook         `yaml:"migrate"`
	ConfirmTimeout time.Duration `yaml:"confirm_timeout"`
}

// Quantity is a size such as "256Mi" or "2Gi", in bytes.
type Quantity int64

var quantityPattern = regexp.MustCompile(`^([0-9]{1,12})(Ki|Mi|Gi|Ti)?$`)

// UnmarshalYAML parses binary size suffixes.
func (q *Quantity) UnmarshalYAML(n *yaml.Node) error {
	m := quantityPattern.FindStringSubmatch(n.Value)
	if n.Kind != yaml.ScalarNode || m == nil {
		return fmt.Errorf("line %d: invalid quantity %q (want e.g. 256Mi)", n.Line, n.Value)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return fmt.Errorf("line %d: invalid quantity %q", n.Line, n.Value)
	}
	shift := map[string]uint{"": 0, "Ki": 10, "Mi": 20, "Gi": 30, "Ti": 40}[m[2]]
	*q = Quantity(v << shift)
	return nil
}

// Patterns of manifest names.
var (
	providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	serviceNamePat    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	namePattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	hostLabelPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	ownerPattern      = regexp.MustCompile(`^[0-9]{1,10}:[0-9]{1,10}$`)
	modePattern       = regexp.MustCompile(`^0?[0-7]{3}$`)
	capabilityPattern = regexp.MustCompile(`^[A-Z][A-Z_]{1,30}$`)
	// imagePattern requires a tag and a SHA-256 digest.
	imagePattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]{1,5})?(/[a-z0-9]+([._-][a-z0-9]+)*)+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}@sha256:[a-f0-9]{64}$`)
)

// reservedServices are the bundle's service names. A managed service with
// one of these names would answer for it on a shared network.
var reservedServices = []string{"postgres", "traefik", "orglane", "agent"}

// ParseManifest decodes a manifest strictly and validates it.
func ParseManifest(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty manifest")
		}
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Setting returns the declared setting with the name.
func (m *Manifest) Setting(name string) (Setting, bool) {
	i := slices.IndexFunc(m.Settings, func(s Setting) bool { return s.Name == name })
	if i < 0 {
		return Setting{}, false
	}
	return m.Settings[i], true
}

// DataDir returns the declared data directory with the name.
func (m *Manifest) DataDir(name string) (DataDir, bool) {
	i := slices.IndexFunc(m.DataDirs, func(d DataDir) bool { return d.Name == name })
	if i < 0 {
		return DataDir{}, false
	}
	return m.DataDirs[i], true
}

// HasService reports whether the manifest declares the long-running service.
func (m *Manifest) HasService(name string) bool { return slices.Contains(m.Services, name) }

// HasSecret reports whether the manifest declares the secret.
func (m *Manifest) HasSecret(name string) bool { return slices.Contains(m.Secrets, name) }

// FileMode returns the permission bits of a data directory.
func (d DataDir) FileMode() uint32 {
	n, _ := strconv.ParseUint(d.Mode, 8, 32) // validated
	return uint32(n)
}

// UIDGID returns the owner of a data directory, ok = false if unset.
func (d DataDir) UIDGID() (uid, gid int, ok bool) {
	u, g, found := strings.Cut(d.Owner, ":")
	if !found {
		return 0, 0, false
	}
	uid, _ = strconv.Atoi(u) // validated
	gid, _ = strconv.Atoi(g)
	return uid, gid, true
}

func (m *Manifest) validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if m.Format != ManifestFormat {
		fail("format: must be %d", ManifestFormat)
	}
	if !providerIDPattern.MatchString(m.ID) {
		fail("id: invalid %q", m.ID)
	}
	if !providerIDPattern.MatchString(m.Provider) {
		fail("provider: invalid %q", m.Provider)
	}
	version, err := ParseVersion(m.Version)
	if err != nil {
		fail("version: %v", err)
	}
	if len(m.Images) == 0 {
		fail("images: at least one image is required")
	}
	for name, ref := range m.Images {
		if !serviceNamePat.MatchString(name) {
			fail("images: invalid name %q", name)
		}
		if !imagePattern.MatchString(ref) {
			fail("images.%s: %q must be a full reference with a tag and a sha256 digest", name, ref)
		}
	}
	if len(m.Services) == 0 {
		fail("services: at least one service is required")
	}
	if !unique(m.Services) {
		fail("services: duplicate names")
	}
	for _, s := range m.Services {
		if !serviceNamePat.MatchString(s) {
			fail("services: invalid name %q", s)
		}
		if slices.Contains(reservedServices, s) {
			fail("services: %q is reserved for the bundle", s)
		}
		if FencedName(s) {
			fail("services: %q ends in the fence suffix %q", s, FencedSuffix)
		}
	}
	routeNames := map[string]bool{}
	for i, r := range m.Routes {
		if !serviceNamePat.MatchString(r.Name) || r.Name == "workspace" || routeNames[r.Name] {
			fail("routes[%d].name: invalid or duplicate %q", i, r.Name)
		}
		routeNames[r.Name] = true
		if !m.HasService(r.Service) {
			fail("routes[%d].service: %q is not a declared service", i, r.Service)
		}
		if r.Port < 1 || r.Port > 65535 {
			fail("routes[%d].port: out of range", i)
		}
		if !hostLabelPattern.MatchString(r.HostLabel) {
			fail("routes[%d].host_label: invalid %q", i, r.HostLabel)
		}
	}
	settingNames := map[string]bool{}
	for i, s := range m.Settings {
		if !namePattern.MatchString(s.Name) || settingNames[s.Name] {
			fail("settings[%d].name: invalid or duplicate %q", i, s.Name)
		}
		settingNames[s.Name] = true
		if err := s.validateDecl(); err != nil {
			fail("settings.%s: %v", s.Name, err)
		}
	}
	portNames := map[string]bool{}
	for i, p := range m.Ports {
		if !serviceNamePat.MatchString(p.Name) || portNames[p.Name] {
			fail("ports[%d].name: invalid or duplicate %q", i, p.Name)
		}
		portNames[p.Name] = true
		if !m.HasService(p.Service) {
			fail("ports[%d].service: %q is not a declared service", i, p.Service)
		}
		if p.Container < 1 || p.Container > 65535 {
			fail("ports[%d].container: out of range", i)
		}
		s, ok := m.Setting(p.Setting)
		if !ok || s.Type != SettingInt || s.Min == nil || s.Max == nil || *s.Min < 1 || *s.Max > 65535 {
			fail("ports[%d].setting: %q must be an int setting with min >= 1 and max <= 65535", i, p.Setting)
		}
	}
	dirNames := map[string]bool{}
	for i, d := range m.DataDirs {
		if !serviceNamePat.MatchString(d.Name) || dirNames[d.Name] {
			fail("data_dirs[%d].name: invalid or duplicate %q", i, d.Name)
		}
		dirNames[d.Name] = true
		if d.Owner != "" && !ownerPattern.MatchString(d.Owner) {
			fail("data_dirs[%d].owner: want <uid>:<gid>", i)
		}
		if !modePattern.MatchString(d.Mode) {
			fail("data_dirs[%d].mode: want an octal mode such as \"0750\"", i)
		}
	}
	if !unique(m.Secrets) {
		fail("secrets: duplicate names")
	}
	for _, s := range m.Secrets {
		if !namePattern.MatchString(s) {
			fail("secrets: invalid name %q", s)
		}
	}
	if m.Database.Enabled && !m.HasSecret(SecretDatabasePassword) {
		fail("secrets: database.enabled needs the secret %q", SecretDatabasePassword)
	}
	for _, c := range m.CapabilitiesAdd {
		if !capabilityPattern.MatchString(c) || c == "ALL" || strings.HasPrefix(c, "CAP_") {
			fail("capabilities_add: invalid %q (write e.g. NET_BIND_SERVICE)", c)
		}
	}
	if m.Health.StartTimeout <= 0 {
		fail("health.start_timeout: must be positive")
	}
	m.validateHooks("hooks.configure", m.Hooks.Configure, fail)
	m.validateHooks("hooks.pre_snapshot", m.Hooks.PreSnapshot, fail)
	m.validateHooks("hooks.post_snapshot", m.Hooks.PostSnapshot, fail)
	if m.Upgrade.Migrate != nil {
		m.validateHooks("upgrade.migrate", []Hook{*m.Upgrade.Migrate}, fail)
	}
	for _, d := range m.Backup.DataDirs {
		if !dirNames[d] {
			fail("backup.data_dirs: %q is not a declared data directory", d)
		}
	}
	for _, s := range m.Backup.StopServices {
		if !m.HasService(s) {
			fail("backup.stop_services: %q is not a declared service", s)
		}
	}
	if m.Upgrade.MinFrom != "" {
		from, err := ParseVersion(m.Upgrade.MinFrom)
		switch {
		case err != nil:
			fail("upgrade.min_from: %v", err)
		case from.Compare(version) >= 0:
			fail("upgrade.min_from: must be older than the version")
		}
	}
	return errors.Join(errs...)
}

func (m *Manifest) validateHooks(field string, hooks []Hook, fail func(string, ...any)) {
	for i, h := range hooks {
		if !m.HasService(h.Service) {
			fail("%s[%d].service: %q is not a declared service", field, i, h.Service)
		}
		if len(h.Command) == 0 || h.Command[0] == "" {
			fail("%s[%d].command: required", field, i)
		}
		if h.Timeout <= 0 {
			fail("%s[%d].timeout: must be positive", field, i)
		}
	}
}

func (s Setting) validateDecl() error {
	switch s.Type {
	case SettingInt:
		if s.Pattern != "" {
			return errors.New("pattern is only allowed for strings")
		}
		if s.Min != nil && s.Max != nil && *s.Min > *s.Max {
			return errors.New("min is greater than max")
		}
	case SettingString:
		if s.Min != nil || s.Max != nil {
			return errors.New("min and max are only allowed for ints")
		}
		if s.Pattern != "" {
			if _, err := regexp.Compile(s.Pattern); err != nil {
				return fmt.Errorf("pattern: %w", err)
			}
		}
	case SettingBool:
		if s.Min != nil || s.Max != nil || s.Pattern != "" {
			return errors.New("min, max and pattern are not allowed for bools")
		}
	default:
		return fmt.Errorf("type: want %s, %s or %s", SettingInt, SettingString, SettingBool)
	}
	if s.Default != nil {
		if _, err := s.coerce(s.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

func unique(s []string) bool {
	seen := map[string]bool{}
	for _, v := range s {
		if seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
