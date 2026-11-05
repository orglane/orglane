// Package forgejo is the adapter of the managed Forgejo code forge
// (docs/specs/providers.md "Provider specifics → Forgejo"). Orglane runs
// Forgejo through the agent from the package in managed/; this adapter
// gives the core the package inputs, sets up a fresh install (the
// organization), checks its health through the Forgejo API, and turns the
// organization hook's deliveries into normalized events (webhooks.go,
// hooks.go, events.go).
//
// Single sign-on and provisioning (#62) build on it.
package forgejo

import (
	"slices"
	"strings"
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
	// CredentialWebhookSecret signs the organization hook's deliveries.
	CredentialWebhookSecret = "webhook_secret"
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

// organizationPattern is the setting's pattern, Forgejo's rule for user
// and organization names (15.0, modules/validation): letters and digits in
// runs separated by single '-', '.' or '_', so no punctuation at either end
// and no two in a row. ValidateSettings adds the length and reserved names.
const organizationPattern = `[A-Za-z0-9]+(?:[-._][A-Za-z0-9]+)*`

// maxOrganization is Forgejo's limit for organization names.
const maxOrganization = 40

// reservedNames are names Forgejo 15.0 refuses for users and organizations
// (models/user: reservedUsernames), compared case-insensitively, plus the
// service admin, which takes its name from the same namespace.
var reservedNames = []string{
	".", "..", "-", ".well-known", "api", "metrics", "v2", "assets", "attachments",
	"avatar", "avatars", "repo-avatars", "captcha", "login", "org", "repo", "user",
	"admin", "explore", "issues", "pulls", "milestones", "notifications", "report_abuse",
	"favicon.ico", "manifest.json", "robots.txt", "sitemap.xml", "ssh_info", "swagger.v1.json",
	"ghost", "gitea-actions", "forgejo-actions", AdminUsername,
}

// reservedSuffixes are Forgejo's reserved name patterns (*.keys, ...).
var reservedSuffixes = []string{".keys", ".gpg", ".rss", ".atom", ".png"}

// planned is the milestone of what #62 adds in M2.
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
		// accounts and teams come with #62.
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
			{Feature: provider.FeatureEventsWebhooks, Support: provider.Supported},
			{Feature: provider.FeatureLifecycleInstall, Support: provider.Supported},
			{Feature: provider.FeatureLifecycleUpgrade, Support: provider.Supported},
			{Feature: provider.FeatureLifecycleSnapshot, Support: provider.Supported},
			{Feature: provider.FeatureLifecycleBackup, Support: provider.Planned, Milestone: "M7"},
		},
		Credentials: []provider.CredentialSpec{
			{Name: CredentialDBPassword, Source: provider.SourceGenerated, Agent: true},
			{Name: CredentialSecretKey, Source: provider.SourceGenerated, Agent: true},
			{Name: CredentialAdminPassword, Source: provider.SourceGenerated, Agent: true},
			{Name: CredentialAdminToken, Source: provider.SourceProvider, Rotatable: true, ReadBack: true, Overlap: time.Hour},
			// No read-back: Forgejo never returns a hook's secret.
			{Name: CredentialWebhookSecret, Source: provider.SourceGenerated, Rotatable: true, Overlap: 24 * time.Hour},
		},
		Settings: []provider.SettingSpec{
			{Name: SettingSSHPort, Type: provider.SettingInt, Min: 1024, Max: 65535, Default: DefaultSSHPort, Mutable: true},
			// Required and immutable. Its default depends on the
			// configuration (the first label of domain.base), so the
			// install API fills it in, not the manifest.
			{Name: SettingOrganization, Type: provider.SettingString, Pattern: organizationPattern, Required: true},
		},
		DefaultKey:    "git",
		DefaultPolicy: provider.AllPeople,
	}
}

// ValidateSettings implements provider.SettingsValidator: the organization
// must be present and a name Forgejo accepts, because it is fixed once the
// instance exists and Setup would fail on every retry otherwise.
func (*Adapter) ValidateSettings(settings map[string]any) error {
	org, _ := settings[SettingOrganization].(string) // missing or null: ""
	return validOrganization(org)
}

func validOrganization(org string) error {
	fail := func(reason string) error { return &provider.SettingError{Setting: SettingOrganization, Reason: reason} }
	if org == "" {
		return fail("is required")
	}
	if !organizationRE.MatchString(org) {
		return fail("must be letters and digits, separated by single '-', '.' or '_'")
	}
	if len(org) > maxOrganization {
		return fail("must be at most 40 characters")
	}
	lower := strings.ToLower(org)
	if slices.Contains(reservedNames, lower) {
		return fail("is reserved by Forgejo")
	}
	for _, suffix := range reservedSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return fail("is reserved by Forgejo")
		}
	}
	return nil
}

var (
	_ provider.SettingsValidator = (*Adapter)(nil)
	_ provider.Lifecycle         = (*Adapter)(nil)
	_ provider.CredentialRotator = (*Adapter)(nil)
	_ provider.EventSource       = (*Adapter)(nil)
)
