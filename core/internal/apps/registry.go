package apps

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/orglane/orglane/core/internal/onboarding"
	"github.com/orglane/orglane/providers/provider"
)

// compiled lists the provider adapters compiled into the core. There is no
// dynamic loading (docs/specs/providers.md "Code layout and registry"). The
// in-repo test provider is added only by test builds (registry_appstest.go)
// and by tests.
var compiled []provider.CapabilityManifest

// Compiled returns the adapters compiled into this binary.
func Compiled() []provider.CapabilityManifest { return slices.Clone(compiled) }

var (
	providerIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	namePattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,40}$`)
	keyPattern        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,22}[a-z0-9]$`)
	stepIDPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}\.[a-z][a-z0-9-]{0,62}$`)
)

// Adapter is a registered adapter with its validated manifest.
type Adapter struct {
	Manifest provider.Manifest
	impl     provider.CapabilityManifest
}

// Provisioner returns the adapter's UserProvisioner, if it has one.
func (a *Adapter) Provisioner() (provider.UserProvisioner, bool) {
	p, ok := a.impl.(provider.UserProvisioner)
	return p, ok
}

// EventSource returns the adapter's EventSource, if it has one.
func (a *Adapter) EventSource() (provider.EventSource, bool) {
	p, ok := a.impl.(provider.EventSource)
	return p, ok
}

// Lifecycle returns the adapter's Lifecycle, if it has one. Only managed
// providers have a lifecycle.
func (a *Adapter) Lifecycle() (provider.Lifecycle, bool) {
	p, ok := a.impl.(provider.Lifecycle)
	return p, ok && a.Manifest.Management == provider.Managed
}

// Rotator returns the adapter's CredentialRotator, if it has one.
func (a *Adapter) Rotator() (provider.CredentialRotator, bool) {
	p, ok := a.impl.(provider.CredentialRotator)
	return p, ok
}

// DeepLinker returns the adapter's DeepLinker, if it has one.
func (a *Adapter) DeepLinker() (provider.DeepLinker, bool) {
	p, ok := a.impl.(provider.DeepLinker)
	return p, ok
}

// Realm returns the account realm of an instance of this adapter.
func (a *Adapter) Realm(inst provider.Instance) string {
	if r, ok := a.impl.(provider.AccountRealmer); ok {
		return r.AccountRealm(inst)
	}
	return inst.ID.String()
}

// Credential returns the spec of a declared credential.
func (a *Adapter) Credential(name string) (provider.CredentialSpec, bool) {
	for _, c := range a.Manifest.Credentials {
		if c.Name == name {
			return c, true
		}
	}
	return provider.CredentialSpec{}, false
}

// Registry is the static set of adapters the core knows.
type Registry struct {
	adapters []*Adapter
	byID     map[provider.ID]*Adapter
}

// NewRegistry validates the manifests of the adapters and returns their
// registry. Invalid or duplicate manifests are an error: the server refuses
// to start rather than run a half-declared adapter.
func NewRegistry(adapters ...provider.CapabilityManifest) (*Registry, error) {
	r := &Registry{byID: make(map[provider.ID]*Adapter, len(adapters))}
	steps := map[string]bool{}
	for _, impl := range adapters {
		m := impl.Manifest()
		if err := validateManifest(m); err != nil {
			return nil, err
		}
		if r.byID[m.ID] != nil {
			return nil, fmt.Errorf("apps: provider %q is registered twice", m.ID)
		}
		for _, s := range m.EmployeeSteps {
			if steps[s.ID] {
				return nil, fmt.Errorf("apps: employee step %q is declared twice", s.ID)
			}
			steps[s.ID] = true
		}
		a := &Adapter{Manifest: m, impl: impl}
		r.adapters = append(r.adapters, a)
		r.byID[m.ID] = a
	}
	slices.SortFunc(r.adapters, func(a, b *Adapter) int { return strings.Compare(string(a.Manifest.ID), string(b.Manifest.ID)) })
	return r, nil
}

// Adapters returns the registered adapters by ID.
func (r *Registry) Adapters() []*Adapter { return slices.Clone(r.adapters) }

// Adapter returns the adapter of a provider.
func (r *Registry) Adapter(id provider.ID) (*Adapter, bool) {
	a, ok := r.byID[id]
	return a, ok
}

func validateManifest(m provider.Manifest) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("apps: manifest of provider %q: %s", m.ID, fmt.Sprintf(format, args...))
	}
	if !providerIDPattern.MatchString(string(m.ID)) {
		return fail("the ID must match %s", providerIDPattern)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fail("a name is required")
	}
	if len(m.Capabilities) == 0 {
		return fail("at least one capability is required")
	}
	for _, c := range m.Capabilities {
		if !capabilityPattern.MatchString(string(c)) {
			return fail("capability %q must match %s", c, capabilityPattern)
		}
	}
	switch m.Management {
	case provider.Managed:
		if m.Package == "" {
			return fail("managed providers name their package")
		}
	case provider.Connected:
		if m.Package != "" {
			return fail("connected providers have no package")
		}
	default:
		return fail("management must be %q or %q", provider.Managed, provider.Connected)
	}
	if m.DefaultPolicy != provider.AllPeople && m.DefaultPolicy != provider.MappedTeams {
		return fail("the default policy must be %q or %q", provider.AllPeople, provider.MappedTeams)
	}
	if !keyPattern.MatchString(m.DefaultKey) {
		return fail("the default key must match %s", keyPattern)
	}
	seen := map[provider.Feature]bool{}
	for _, f := range m.Features {
		if !slices.Contains(provider.Features, f.Feature) {
			return fail("unknown feature %q", f.Feature)
		}
		if seen[f.Feature] {
			return fail("feature %q is declared twice", f.Feature)
		}
		seen[f.Feature] = true
		switch f.Support {
		case provider.Supported, provider.Partial, provider.Unsupported:
		case provider.Planned:
			if f.Milestone == "" {
				return fail("planned feature %q names its milestone", f.Feature)
			}
		default:
			return fail("feature %q has unknown support %q", f.Feature, f.Support)
		}
	}
	// The matrix is honest: every feature is declared, none omitted.
	for _, f := range provider.Features {
		if !seen[f] {
			return fail("feature %q is not declared (use %q)", f, provider.Unsupported)
		}
	}
	names := map[string]bool{}
	for _, c := range m.Credentials {
		if !namePattern.MatchString(c.Name) || names[c.Name] {
			return fail("credential %q is invalid or declared twice", c.Name)
		}
		names[c.Name] = true
		switch c.Source {
		case provider.SourceGenerated, provider.SourceProvider, provider.SourceUpload:
		default:
			return fail("credential %q has unknown source %q", c.Name, c.Source)
		}
		if c.Overlap < 0 {
			return fail("credential %q has a negative overlap", c.Name)
		}
	}
	settings := map[string]bool{}
	for _, s := range m.Settings {
		if !namePattern.MatchString(s.Name) || settings[s.Name] {
			return fail("setting %q is invalid or declared twice", s.Name)
		}
		settings[s.Name] = true
		if err := validateSettingSpec(s); err != nil {
			return fail("setting %q: %v", s.Name, err)
		}
	}
	for _, s := range m.EmployeeSteps {
		if !stepIDPattern.MatchString(s.ID) || !strings.HasPrefix(s.ID, string(m.ID)+".") {
			return fail("employee step %q must be namespaced as %q", s.ID, string(m.ID)+".<step>")
		}
		if strings.TrimSpace(s.Title) == "" {
			return fail("employee step %q needs a title", s.ID)
		}
		if err := onboarding.ValidateActionURL(s.ActionPath); err != nil || s.ActionPath == "" {
			return fail("employee step %q needs a local action path", s.ID)
		}
	}
	return nil
}
