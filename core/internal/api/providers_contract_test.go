package api

import (
	"testing"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
)

// Provider metadata stays inside its contract: every enum value the
// providers endpoint emits for the compiled adapters (and the test
// provider's variants) is one the OpenAPI document declares, so strict
// clients can represent it.
func TestAppProviderMetadataMatchesContract(t *testing.T) {
	adapters := append(apps.Compiled(), testprovider.New(testprovider.WithID("testmanaged"), testprovider.Managed()))
	reg, err := apps.NewRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]bool{}
	for _, a := range reg.Adapters() {
		p := appProvider(a)
		if !p.Management.Valid() || !p.DefaultPolicy.Valid() {
			t.Errorf("%s: management %q, policy %q", p.Id, p.Management, p.DefaultPolicy)
		}
		for _, f := range p.Features {
			if !f.Support.Valid() {
				t.Errorf("%s: feature %s support %q", p.Id, f.Feature, f.Support)
			}
		}
		for _, c := range p.Credentials {
			sources[string(c.Source)] = true
			if !c.Source.Valid() {
				t.Errorf("%s: credential %s source %q is not in the contract", p.Id, c.Name, c.Source)
			}
		}
		for _, s := range p.Settings {
			if !s.Type.Valid() {
				t.Errorf("%s: setting %s type %q", p.Id, s.Name, s.Type)
			}
		}
	}
	if !sources[provider.SourceIdentity] {
		t.Error("no compiled adapter has an identity credential: the check above did not cover it")
	}
}
