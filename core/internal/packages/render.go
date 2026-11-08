package packages

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"unicode"
	"unicode/utf8"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// RenderContext is what the agent adds to a spec to render it.
type RenderContext struct {
	// Key is the instance key.
	Key string
	// InstanceDir is the absolute host path of instances/<key>.
	InstanceDir string
	// ComposeProject is the bundle's Compose project name; the instance
	// project is "<ComposeProject>-<Key>".
	ComposeProject string
	// PublishAddress is the host address published ports bind to.
	PublishAddress string
	// EdgeNetwork and ServicesNetwork are the external network names.
	EdgeNetwork, ServicesNetwork string
	// DatabaseHost is the bundle's PostgreSQL service name.
	DatabaseHost string
	// Fenced renders an upgrade candidate: fenced aliases, no host ports.
	Fenced bool
	// MemoryBytes and CPUs describe the host, for tuning (never limits).
	MemoryBytes int64
	CPUs        int
}

// Project returns the instance's Compose project name.
func (rc RenderContext) Project() string { return InstanceProject(rc.ComposeProject, rc.Key) }

// InstanceProject returns the Compose project name of an instance.
func InstanceProject(composeProject, key string) string { return composeProject + "-" + key }

// DataPath returns the host path of a data directory.
func (rc RenderContext) DataPath(name string) string {
	return filepath.Join(rc.InstanceDir, "data", name)
}

// FilesPath returns the host path of the rendered files directory.
func (rc RenderContext) FilesPath() string { return filepath.Join(rc.InstanceDir, FilesDir) }

// DatabaseName returns the name of an instance's database and role.
func DatabaseName(key string) string { return "svc_" + strings.ReplaceAll(key, "-", "_") }

// SecretEnvName returns the .env variable of a secret.
func SecretEnvName(name string) string { return "ORGLANE_SECRET_" + strings.ToUpper(name) }

// FencedSuffix marks the aliases of a fenced render.
const FencedSuffix = "-fenced"

// FencedName reports whether a service name would give an unfenced alias
// ending in FencedSuffix ("fenced" or "<name>-fenced"). Such names are
// invalid, so routed aliases and fenced aliases are disjoint namespaces: no
// route of any instance or version can reach a fenced project.
func FencedName(service string) bool {
	return service == strings.TrimPrefix(FencedSuffix, "-") || strings.HasSuffix(service, FencedSuffix)
}

// serviceNamePattern is the pattern of service names: no "-", so that the
// names an instance's containers get on shared networks are injective (see
// ValidServiceName).
var serviceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,30}$`)

// ValidServiceName reports whether name may name a service of a package,
// declared or one-shot (docs/specs/agent.md "Instance names").
//
// Instance keys (agentapi.KeyPattern) start with a letter and end with a
// letter or digit; service names are letters and digits only, start with a
// letter and are not "fenced". Then the names Docker's DNS gives an
// instance's containers on the shared networks never coincide with those of
// another instance or service:
//
//   - the routed alias <key>-<service>: its last "-" separates the key from
//     the service, so it determines both;
//   - the fenced alias <key>-<service>-fenced: it ends in FencedSuffix, which
//     a routed alias never does (no service is "fenced"); without the
//     suffix it is a routed alias;
//   - the container name <project>-<key>-<service>-<n> (project fixed per
//     agent, n a replica number): it ends in "-<digits>", aliases end in "-"
//     and a letter;
//   - the service name itself: it contains no "-", every name above does.
//
// The longest fenced alias (24 + 1 + 31 + 7 characters) still fits a DNS
// label.
func ValidServiceName(name string) bool {
	return serviceNamePattern.MatchString(name) && !FencedName(name)
}

// TemplateData is the data of package templates (docs/specs/agent.md
// "Template rendering").
type TemplateData struct {
	Instance struct{ Key, ID, Project string }
	Package  struct{ ID, Version string }
	Hosts    map[string]string
	URLs     struct{ Workspace, Issuer, Instance, Webhook string }
	Ingress  struct{ Mode string }
	Fenced   bool
	Networks struct{ Edge, Services string }
	Database struct{ Address, Name, User string }
	Settings map[string]any
	Host     struct {
		MemoryBytes int64
		CPUs        int
	}
}

// Rendered is a rendered project.
type Rendered struct {
	Compose []byte
	// Env is the .env file: one ORGLANE_SECRET_<NAME>='<value>' per secret.
	Env []byte
	// Files are the rendered package files by slash-separated path.
	Files map[string]File
}

// File is a rendered package file.
type File struct {
	Data []byte
	Mode fs.FileMode
}

// Hash returns a hash over everything rendered.
func (r *Rendered) Hash() string {
	h := sha256.New()
	write := func(b []byte) {
		h.Write([]byte(strconv.Itoa(len(b)) + ":"))
		h.Write(b)
	}
	write(r.Compose)
	write(r.Env)
	names := make([]string, 0, len(r.Files))
	for n := range r.Files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		write([]byte(n))
		write([]byte(r.Files[n].Mode.String()))
		write(r.Files[n].Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// placeholderFuncs declares the template functions for parsing; Render binds
// the real ones. Templates get only these, no function library.
var placeholderFuncs = template.FuncMap{
	"quote":  func(any) string { return "" },
	"secret": func(string) (string, error) { return "", nil },
	"image":  func(string) (string, error) { return "", nil },
	"alias":  func(string) (string, error) { return "", nil },
	"port":   func(string) (string, error) { return "", nil },
	"dir":    func(string) (string, error) { return "", nil },
	"file":   func(string) (string, error) { return "", nil },
}

// Render renders the package for a spec with validated inputs.
func (p *Package) Render(spec *agentapi.Spec, in Inputs, rc RenderContext) (*Rendered, error) {
	m := p.Manifest
	if !agentapi.KeyPattern.MatchString(rc.Key) {
		return nil, fmt.Errorf("render: invalid instance key %q", rc.Key)
	}
	var data TemplateData
	data.Instance.Key, data.Instance.ID, data.Instance.Project = rc.Key, spec.InstanceID.String(), rc.Project()
	data.Package.ID, data.Package.Version = m.ID, m.Version
	data.Hosts = spec.Hosts
	data.URLs.Workspace, data.URLs.Issuer, data.URLs.Instance, data.URLs.Webhook = spec.URLs.Workspace, spec.URLs.Issuer, spec.URLs.Instance, spec.URLs.Webhook
	data.Ingress.Mode = spec.Ingress
	data.Fenced = rc.Fenced
	data.Networks.Edge, data.Networks.Services = rc.EdgeNetwork, rc.ServicesNetwork
	if m.Database.Enabled {
		data.Database.Address = net.JoinHostPort(rc.DatabaseHost, "5432")
		data.Database.Name, data.Database.User = DatabaseName(rc.Key), DatabaseName(rc.Key)
	}
	data.Settings = in.Settings
	data.Host.MemoryBytes, data.Host.CPUs = rc.MemoryBytes, rc.CPUs

	funcs := template.FuncMap{
		"quote": func(v any) string { return Quote(fmt.Sprint(v)) },
		"secret": func(name string) (string, error) {
			if !m.HasSecret(name) {
				return "", fmt.Errorf("secret %q is not declared", name)
			}
			return `"${` + SecretEnvName(name) + `}"`, nil
		},
		"image": func(name string) (string, error) {
			ref, ok := m.Images[name]
			if !ok {
				return "", fmt.Errorf("image %q is not declared", name)
			}
			return ref, nil
		},
		"alias": func(service string) (string, error) {
			if !m.HasService(service) {
				return "", fmt.Errorf("alias: service %q is not declared", service)
			}
			return ServiceAlias(rc.Key, service, rc.Fenced), nil
		},
		"port": func(name string) (string, error) {
			i := slices.IndexFunc(m.Ports, func(pt Port) bool { return pt.Name == name })
			if i < 0 {
				return "", fmt.Errorf("port %q is not declared", name)
			}
			if rc.Fenced {
				return "", nil
			}
			pt := m.Ports[i]
			host, ok := in.Settings[pt.Setting].(int64)
			if !ok {
				return "", fmt.Errorf("port %q: setting %q is not set", name, pt.Setting)
			}
			return net.JoinHostPort(rc.PublishAddress, strconv.FormatInt(host, 10)) + ":" + strconv.Itoa(pt.Container), nil
		},
		"dir": func(name string) (string, error) {
			if _, ok := m.DataDir(name); !ok {
				return "", fmt.Errorf("data directory %q is not declared", name)
			}
			return rc.DataPath(name), nil
		},
		"file": func(rel string) (string, error) {
			if !p.hasFileOrDir(rel) {
				return "", fmt.Errorf("file %q is not a package file", rel)
			}
			if rel == "." {
				return rc.FilesPath(), nil
			}
			return filepath.Join(rc.FilesPath(), filepath.FromSlash(rel)), nil
		},
	}

	out := &Rendered{Files: map[string]File{}}
	var err error
	if out.Compose, err = execute(p.compose, funcs, data); err != nil {
		return nil, err
	}
	for name, f := range p.files {
		content := f.data
		if f.template != nil {
			if content, err = execute(f.template, funcs, data); err != nil {
				return nil, err
			}
		}
		mode := fs.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		out.Files[name] = File{Data: content, Mode: mode}
	}
	var env bytes.Buffer
	names := make([]string, 0, len(spec.Secrets))
	for name := range spec.Secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		v := spec.Secrets[name]
		if !m.HasSecret(name) || !ValidSecret(v) {
			return nil, &InputError{Fields: []string{"secrets." + name}}
		}
		fmt.Fprintf(&env, "%s='%s'\n", SecretEnvName(name), v)
	}
	out.Env = env.Bytes()
	return out, nil
}

// ServiceAlias returns a service's alias on the edge network: the upstream
// host of its routes, or the fenced alias that routes never resolve. For a
// valid key and a valid service name (ValidServiceName) it is injective.
func ServiceAlias(key, service string, fenced bool) string {
	a := key + "-" + service
	if fenced {
		a += FencedSuffix
	}
	return a
}

func execute(t *template.Template, funcs template.FuncMap, data TemplateData) ([]byte, error) {
	c, err := t.Clone()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := c.Funcs(funcs).Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", t.Name(), err)
	}
	return buf.Bytes(), nil
}

// Quote returns s as a YAML double-quoted scalar that Compose does not
// interpolate: backslashes, quotes and non-printable characters are escaped,
// and "$" is doubled.
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '$':
			b.WriteString("$$")
		case r == utf8.RuneError:
			b.WriteString(`�`)
		case r == ' ' || (unicode.IsPrint(r) && r != '\u2028' && r != '\u2029' && r != '\ufeff'):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02X`, r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			fmt.Fprintf(&b, `\U%08X`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
