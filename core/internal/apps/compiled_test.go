package apps

import (
	"slices"
	"testing"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/providers/provider"
)

// The compiled adapters form a valid registry, and every managed adapter
// agrees with its embedded package: the package exists for the provider,
// the secrets Lifecycle.Inputs names are exactly the package's secrets and
// agent credentials of the adapter, and its settings are package settings.
func TestCompiledAdaptersMatchPackages(t *testing.T) {
	reg, err := NewRegistry(Compiled()...)
	if err != nil {
		t.Fatal(err)
	}
	pkgs, err := packages.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Adapter("forgejo"); !ok {
		t.Fatal("the forgejo adapter is not compiled in")
	}
	for _, a := range reg.Adapters() {
		if a.Manifest.Management != provider.Managed {
			continue
		}
		lc, ok := a.Lifecycle()
		if !ok {
			t.Errorf("%s: a managed adapter without Lifecycle", a.Manifest.ID)
			continue
		}
		var versions []agentapi.PackageRef
		for _, ref := range pkgs.List() {
			if ref.ID == a.Manifest.Package {
				versions = append(versions, ref)
			}
		}
		if len(versions) == 0 {
			t.Errorf("%s: package %q is not embedded", a.Manifest.ID, a.Manifest.Package)
			continue
		}
		settings, err := mergeSettings(a.Manifest.Settings, nil, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		in, err := lc.Inputs(t.Context(), nil, provider.Instance{Key: a.Manifest.DefaultKey, Settings: settings})
		if err != nil {
			t.Fatalf("%s: inputs: %v", a.Manifest.ID, err)
		}
		for _, name := range in.Secrets {
			if c, ok := a.Credential(name); !ok || !c.Agent {
				t.Errorf("%s: secret %s is not an agent credential", a.Manifest.ID, name)
			}
		}
		for _, ref := range versions {
			p, _ := pkgs.Get(ref)
			if p.Manifest.Provider != string(a.Manifest.ID) {
				t.Errorf("%s: package %s belongs to provider %q", a.Manifest.ID, ref, p.Manifest.Provider)
			}
			got, want := slices.Sorted(slices.Values(in.Secrets)), slices.Sorted(slices.Values(p.Manifest.Secrets))
			if !slices.Equal(got, want) {
				t.Errorf("%s: inputs name secrets %v, package %s declares %v", a.Manifest.ID, got, ref, want)
			}
			for name := range in.Settings {
				if _, ok := p.Manifest.Setting(name); !ok {
					t.Errorf("%s: setting %s is not declared by package %s", a.Manifest.ID, name, ref)
				}
			}
		}
	}
}
