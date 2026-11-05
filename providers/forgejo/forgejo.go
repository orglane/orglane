// Package forgejo is the adapter of the managed Forgejo code forge
// (docs/specs/providers.md "Provider specifics → Forgejo"). Orglane runs
// Forgejo through the agent from the package in managed/; this adapter
// gives the core the package inputs, sets up a fresh install (the
// organization) and checks its health through the Forgejo API.
//
// Single sign-on and provisioning (#62) and webhooks (#63) build on it.
package forgejo

import (
	"time"

	"github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
)

// ID is the provider ID.
const ID provider.ID = "forgejo"

// Credential names (docs/specs/providers.md "Credentials").
const (
	// CredentialDBPassword is the password of the per-service database role.
	CredentialDBPassword = "db_password"
	// CredentialSecretKey is Forgejo's SECRET_KEY. It encrypts secrets in
	// Forgejo's database and is never rotated.
	CredentialSecretKey = "secret_key"
	// CredentialAdminPassword is the password of the service admin, which
	// the package's configure hook maintains. The adapter uses it for basic
	// authentication against the admin API.
	CredentialAdminPassword = "admin_password"
	// CredentialAdminToken is an API token of the service admin, minted by
	// the adapter (ApplyCredential) and named after its version.
	CredentialAdminToken = "admin_token"
)

// Setting names.
const (
	// SettingSSHPort is the host port of Git over SSH.
	SettingSSHPort = "ssh_port"
	// SettingOrganization is the Forgejo organization of the company. Fixed
	// once the instance exists.
	SettingOrganization = "organization"
)

// AdminUsername is the service admin's login, the same as in the package's
// compose template (ORGLANE_ADMIN_USERNAME).
const AdminUsername = "orglane-admin"

// DefaultSSHPort is the default host port of Git over SSH (22 is normally
// the host's own SSH daemon).
const DefaultSSHPort = 2222

// organizationPattern is the setting's pattern: Forgejo's rules for user
// and organization names, at most 40 characters.
const organizationPattern = `[A-Za-z0-9][A-Za-z0-9_.-]{0,38}`

// planned is the milestone of what #62 and #63 add in M2.
const planned = "M2"

// Adapter is the Forgejo adapter. It holds no state; all methods are safe
// for concurrent use.
type Adapter struct{}

// New returns the Forgejo adapter.
func New() *Adapter { return &Adapter{} }

// Package returns the package version new instances install.
func Package() string { return managed.Current }

// Manifest implements provider.CapabilityManifest.
func (*Adapter) Manifest() provider.Manifest {
	return provider.Manifest{
		ID:           ID,
		Name:         "Forgejo",
		Capabilities: []provider.Capability{"code"},
		Management:   provider.Managed,
		Package:      "forgejo",
		// The matrix says what this adapter does today: single sign-on,
		// accounts and teams come with #62, webhooks with #63.
		Features: []provider.FeatureSupport{
			{Feature: provider.FeatureSSO, Support: provider.Planned, Milestone: planned},
			{Feature: provider.FeatureDeepLinks, Support: provider.Planned, Milestone: planned},
			{Feature: provider.FeatureAccountsCreate, Support: provider.Planned, Milestone: planned},
			{Feature: provider.FeatureAccountsInvite, Support: provider.Unsupported},
			{Feature: provider.FeatureAccountsLink, Support: provider.Planned, Milestone: planned},
			{Feature: provider.FeatureTeamsSync, Support: provider.Planned, Milestone: planned},
			{Feature: provider.FeatureDeprovisionTeamRemoval, Support: provider.Planned, Milestone: planned},
			{Feature: provider.FeatureDeprovisionOrgRemoval, Support: provider.Unsupported},
			{Feature: provider.FeatureDeprovisionSuspend, Support: provider.Planned, Milestone: "M5"},
			{Feature: provider.FeatureDeprovisionRevokeTokens, Support: provider.Planned, Milestone: "M5"},
			{Feature: provider.FeatureEventsWebhooks, Support: provider.Planned, Milestone: planned},
			{Feature: provider.FeatureLifecycleInstall, Support: provider.Supported},
			{Feature: provider.FeatureLifecycleUpgrade, Support: provider.Supported},
			{Feature: provider.FeatureLifecycleSnapshot, Support: provider.Supported},
			{Feature: provider.FeatureLifecycleBackup, Support: provider.Planned, Milestone: "M7"},
		},
		Credentials: []provider.CredentialSpec{
			{Name: CredentialDBPassword, Source: provider.SourceGenerated, Agent: true},
			{Name: CredentialSecretKey, Source: provider.SourceGenerated, Agent: true},
			{Name: CredentialAdminPassword, Source: provider.SourceGenerated, Agent: true},
			{Name: CredentialAdminToken, Source: provider.SourceProvider, Rotatable: true, Overlap: time.Hour},
		},
		Settings: []provider.SettingSpec{
			{Name: SettingSSHPort, Type: provider.SettingInt, Min: 1024, Max: 65535, Default: DefaultSSHPort, Mutable: true},
			{Name: SettingOrganization, Type: provider.SettingString, Pattern: organizationPattern},
		},
		DefaultKey:    "git",
		DefaultPolicy: provider.AllPeople,
	}
}

var (
	_ provider.Lifecycle         = (*Adapter)(nil)
	_ provider.CredentialRotator = (*Adapter)(nil)
)
