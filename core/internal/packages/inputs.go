package packages

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// SecretDatabasePassword is the secret that holds the password of the
// per-service database role.
const SecretDatabasePassword = "db_password"

// HostWorkspace is the Hosts key of the workspace host.
const HostWorkspace = "workspace"

// maxStringSetting is the longest string setting.
const maxStringSetting = 4096

// secretPattern restricts secret values to characters that need no quoting
// anywhere (docs/specs/agent.md "Template rendering"): no quotes, "$" or
// newlines. Generated secrets are base64url.
var secretPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

// maxSecret is the longest secret value.
const maxSecret = 4096

// ValidSecret reports whether a secret value may be passed to a package.
func ValidSecret(v string) bool { return len(v) <= maxSecret && secretPattern.MatchString(v) }

// InputError lists the invalid inputs of a spec by field name, never by
// value: values may be secrets.
type InputError struct {
	Fields []string
}

func (e *InputError) Error() string {
	return "invalid input: " + strings.Join(e.Fields, ", ")
}

// Inputs are a spec's validated inputs.
type Inputs struct {
	// Settings holds every declared setting, with defaults applied and values
	// of the declared type (int64, string or bool).
	Settings map[string]any
}

// Inputs validates the spec's inputs against the manifest: hosts, URLs,
// settings by declared type and bounds, and secrets by name and value. The
// spec's shape (agentapi.Spec.Check) is checked too.
func (p *Package) Inputs(spec *agentapi.Spec) (Inputs, error) {
	m := p.Manifest
	fields := spec.Check()
	if spec.Package.ID != m.ID || spec.Package.Version != m.Version {
		fields = append(fields, "package")
	}

	want := []string{HostWorkspace}
	for _, r := range m.Routes {
		want = append(want, r.Name)
	}
	for name, host := range spec.Hosts {
		if !slices.Contains(want, name) || !ValidHostname(host) {
			fields = append(fields, "hosts."+name)
		}
	}
	for _, name := range want {
		if _, ok := spec.Hosts[name]; !ok {
			fields = append(fields, "hosts."+name)
		}
	}

	for name, raw := range map[string]string{
		"urls.workspace": spec.URLs.Workspace, "urls.issuer": spec.URLs.Issuer,
		"urls.instance": spec.URLs.Instance, "urls.webhook": spec.URLs.Webhook,
	} {
		if raw != "" && !validURL(raw) {
			fields = append(fields, name)
		}
	}

	settings := make(map[string]any, len(m.Settings))
	for name := range spec.Settings {
		if _, ok := m.Setting(name); !ok {
			fields = append(fields, "settings."+name)
		}
	}
	for _, s := range m.Settings {
		raw, ok := spec.Settings[s.Name]
		if !ok || raw == nil {
			if s.Default == nil {
				fields = append(fields, "settings."+s.Name)
				continue
			}
			raw = s.Default
		}
		v, err := s.coerce(raw)
		if err != nil {
			fields = append(fields, "settings."+s.Name)
			continue
		}
		settings[s.Name] = v
	}

	for name, v := range spec.Secrets {
		if !m.HasSecret(name) || !ValidSecret(v) {
			fields = append(fields, "secrets."+name)
		}
	}
	for _, name := range m.Secrets {
		if _, ok := spec.Secrets[name]; !ok {
			fields = append(fields, "secrets."+name)
		}
	}

	if len(fields) > 0 {
		slices.Sort(fields)
		return Inputs{}, &InputError{Fields: slices.Compact(fields)}
	}
	return Inputs{Settings: settings}, nil
}

// coerce converts a decoded value to the setting's type and checks its
// bounds or pattern.
func (s Setting) coerce(raw any) (any, error) {
	switch s.Type {
	case SettingInt:
		n, err := toInt(raw)
		if err != nil {
			return nil, err
		}
		if s.Min != nil && n < *s.Min || s.Max != nil && n > *s.Max {
			return nil, errors.New("out of range")
		}
		return n, nil
	case SettingString:
		v, ok := raw.(string)
		if !ok {
			return nil, errors.New("not a string")
		}
		if len(v) > maxStringSetting || !utf8.ValidString(v) || strings.ContainsFunc(v, isControl) {
			return nil, errors.New("too long, invalid UTF-8 or control characters")
		}
		if s.Pattern != "" && !regexp.MustCompile(`^(?:`+s.Pattern+`)$`).MatchString(v) {
			return nil, errors.New("does not match the pattern")
		}
		return v, nil
	case SettingBool:
		v, ok := raw.(bool)
		if !ok {
			return nil, errors.New("not a bool")
		}
		return v, nil
	}
	return nil, fmt.Errorf("unknown type %q", s.Type)
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }

func toInt(raw any) (int64, error) {
	switch v := raw.(type) {
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > 1<<53 {
			return 0, errors.New("not an integer")
		}
		return int64(v), nil
	case json.Number:
		return strconv.ParseInt(v.String(), 10, 64)
	}
	return 0, errors.New("not an integer")
}

// ValidHostname reports whether h is a lowercase RFC 1123 hostname.
func ValidHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if !hostLabelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// validURL accepts absolute http and https URLs without user info or
// fragment, with a valid hostname and optional port.
func validURL(raw string) bool {
	if len(raw) > 2048 || strings.ContainsFunc(raw, isControl) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if !ValidHostname(u.Hostname()) {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}
