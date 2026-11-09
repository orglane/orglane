package apps

import (
	"slices"
	"strings"
	"testing"

	"github.com/orglane/orglane/providers/forgejo"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
)

type manifestOnly struct{ m provider.Manifest }

func (a manifestOnly) Manifest() provider.Manifest { return a.m }

func TestRegistryValidatesManifests(t *testing.T) {
	good := testprovider.New().Manifest()
	if _, err := NewRegistry(testprovider.New(), testprovider.New(testprovider.WithID("other"), testprovider.Managed())); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRegistry(testprovider.New(), testprovider.New()); err == nil {
		t.Fatal("a provider registered twice")
	}
	for name, mutate := range map[string]func(*provider.Manifest){
		"bad id":          func(m *provider.Manifest) { m.ID = "Bad" },
		"no capability":   func(m *provider.Manifest) { m.Capabilities = nil },
		"managed, no pkg": func(m *provider.Manifest) { m.Management = provider.Managed },
		"connected, pkg":  func(m *provider.Manifest) { m.Package = "x" },
		"omitted feature": func(m *provider.Manifest) { m.Features = m.Features[1:] },
		"duplicate feat":  func(m *provider.Manifest) { m.Features = append(m.Features, m.Features[0]) },
		"planned, no ms": func(m *provider.Manifest) {
			m.Features[0] = provider.FeatureSupport{Feature: provider.FeatureSSO, Support: provider.Planned}
		},
		"unknown support": func(m *provider.Manifest) { m.Features[0].Support = "maybe" },
		"bad credential":  func(m *provider.Manifest) { m.Credentials[0].Name = "Bad-Name" },
		"bad source":      func(m *provider.Manifest) { m.Credentials[0].Source = "magic" },
		"step namespace":  func(m *provider.Manifest) { m.EmployeeSteps[0].ID = "github.link-account" },
		"step action":     func(m *provider.Manifest) { m.EmployeeSteps[0].ActionPath = "https://evil.example" },
		"setting pattern": func(m *provider.Manifest) { m.Settings[0].Pattern = "(" },
		"setting default": func(m *provider.Manifest) { m.Settings[0].Default = 3 },
		"default key":     func(m *provider.Manifest) { m.DefaultKey = "-x" },
		"default policy":  func(m *provider.Manifest) { m.DefaultPolicy = "everyone" },
	} {
		m := good
		m.Features = append([]provider.FeatureSupport(nil), good.Features...)
		m.Credentials = append([]provider.CredentialSpec(nil), good.Credentials...)
		m.EmployeeSteps = append([]provider.EmployeeStepSpec(nil), good.EmployeeSteps...)
		m.Settings = append([]provider.SettingSpec(nil), good.Settings...)
		mutate(&m)
		if _, err := NewRegistry(manifestOnly{m}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMergeSettings(t *testing.T) {
	specs := testprovider.New().Manifest().Settings
	specs = append(specs, provider.SettingSpec{Name: "seats", Type: provider.SettingInt, Min: 1, Max: 10, Mutable: true})
	created, err := mergeSettings(specs, nil, map[string]any{"organization": "robots"}, true)
	if err != nil || created["organization"] != "robots" || created["require_link"] != false {
		t.Fatalf("created = %v, %v", created, err)
	}
	for name, change := range map[string]map[string]any{
		"immutable": {"organization": "other"},
		"unknown":   {"nope": true},
		"type":      {"require_link": "yes"},
		"range":     {"seats": 11},
		"fraction":  {"seats": 1.5},
		"pattern":   {"organization": strings.Repeat("x", 40)},
	} {
		if _, err := mergeSettings(specs, created, change, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Setting an immutable value to what it already is is not a change.
	if _, err := mergeSettings(specs, created, map[string]any{"organization": "robots", "seats": float64(3)}, false); err != nil {
		t.Fatal(err)
	}
}

func TestHashDesiredIgnoresLoginAndTeamOrder(t *testing.T) {
	a := provider.DesiredAccess{Username: "a", Access: true, Teams: []provider.TeamRef{{ExternalRef: "1"}, {ExternalRef: "2"}},
		Link: &provider.AccountLink{Account: provider.ExternalAccount{ExternalID: "9", Login: "old"}}}
	b := a
	b.Teams = []provider.TeamRef{a.Teams[1], a.Teams[0]}
	b.Link = &provider.AccountLink{Account: provider.ExternalAccount{ExternalID: "9", Login: "new"}}
	if string(hashDesired(a)) != string(hashDesired(b)) {
		t.Fatal("hash depends on the login or team order")
	}
	b.Access = false
	if string(hashDesired(a)) == string(hashDesired(b)) {
		t.Fatal("hash ignores access")
	}
}

// Mapped is the instance-wide set of managed teams: an unrelated mapping
// change must not alter a person's hash (that would drop held removals
// nobody asked to cancel). A mapping that concerns the person changes Teams,
// which is hashed.
func TestHashDesiredIgnoresUnrelatedMappedTeams(t *testing.T) {
	a := provider.DesiredAccess{Username: "a", Access: true, Teams: []provider.TeamRef{{ExternalRef: "1"}},
		Mapped: []provider.TeamRef{{ExternalRef: "1"}}}
	b := a
	b.Mapped = []provider.TeamRef{{ExternalRef: "1"}, {ExternalRef: "2"}}
	if string(hashDesired(a)) != string(hashDesired(b)) {
		t.Fatal("hash depends on unrelated mapped teams")
	}
	b.Teams = []provider.TeamRef{{ExternalRef: "2"}}
	if string(hashDesired(a)) == string(hashDesired(b)) {
		t.Fatal("hash ignores the person's teams")
	}
}

func TestRemovalLimit(t *testing.T) {
	for managed, want := range map[int64]int64{0: 3, 29: 3, 40: 4, 1000: 100} {
		if got := removalLimit(managed); got != want {
			t.Errorf("removalLimit(%d) = %d, want %d", managed, got, want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	s := strings.Repeat("a", 99) + "ä" // 101 bytes
	if got := truncateRunes(s, 100); got != strings.Repeat("a", 99) {
		t.Fatalf("truncateRunes = %q", got)
	}
	if got := truncateRunes("äbc", 100); got != "äbc" {
		t.Fatalf("short string changed: %q", got)
	}
}

func TestSettingsDecodeCanonically(t *testing.T) {
	specs := []provider.SettingSpec{{Name: "seats", Type: provider.SettingInt, Min: 1, Max: 1 << 62, Mutable: true}, {Name: "org", Type: provider.SettingString}}
	got, err := decodeSettings([]byte(`{"seats": 9007199254740993, "org": "acme"}`))
	if err != nil {
		t.Fatal(err)
	}
	canonicalSettings(specs, got)
	if v, ok := got["seats"].(int64); !ok || v != 9007199254740993 {
		t.Fatalf("seats = %#v, want int64 9007199254740993", got["seats"])
	}
	if got["org"] != "acme" {
		t.Fatalf("org = %#v", got["org"])
	}
}

func TestIntSettingsRejectOutOfRangeFloats(t *testing.T) {
	spec := provider.SettingSpec{Name: "n", Type: provider.SettingInt}
	for _, v := range []float64{9223372036854775808.0, -9223372036854775809.0 * 2, 1.5} {
		if _, err := settingValue(spec, v); err == nil {
			t.Errorf("settingValue(%v) accepted", v)
		}
	}
	if n, err := settingValue(spec, float64(1<<53)); err != nil || n != int64(1<<53) {
		t.Fatalf("settingValue(2^53) = %v, %v", n, err)
	}
}

// relyingParty is the Forgejo adapter with another manifest.
type relyingParty struct {
	*forgejo.Adapter
	m provider.Manifest
}

func (r relyingParty) Manifest() provider.Manifest { return r.m }

// OIDC client secrets (Source identity) belong to relying parties and are
// not rotatable in M2.
func TestRegistryIdentityCredentials(t *testing.T) {
	good := forgejo.New().Manifest()
	if _, err := NewRegistry(forgejo.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRegistry(manifestOnly{good}); err == nil || !strings.Contains(err.Error(), "relying party") {
		t.Fatalf("identity credential without a relying party: %v", err)
	}
	rotatable := good
	rotatable.Credentials = slices.Clone(good.Credentials)
	for i, c := range rotatable.Credentials {
		if c.Source == provider.SourceIdentity {
			rotatable.Credentials[i].Rotatable = true
		}
	}
	if _, err := NewRegistry(relyingParty{forgejo.New(), rotatable}); err == nil || !strings.Contains(err.Error(), "not rotatable") {
		t.Fatalf("rotatable OIDC client secret: %v", err)
	}
}
