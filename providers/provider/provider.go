// Package provider holds the contracts between the core and provider
// adapters (docs/specs/providers.md "Adapter contracts"): the manifest every
// adapter declares, the optional capability interfaces the core discovers
// with type assertions, and the types they exchange.
//
// Adapters live under providers/ and cannot import core/internal/...; the
// core hands them its services through Env. This package is not a public
// SDK: it may change in any release until the provider SDK (v0.2).
//
// All methods of an adapter must be safe for concurrent use and honor
// context cancellation.
package provider

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// ID identifies a provider, e.g. "forgejo" or "github". IDs match
// ^[a-z][a-z0-9-]{1,30}$ and are unique among the compiled adapters.
type ID string

// Capability is what a provider offers, e.g. "code".
type Capability string

// Management says who runs the service of a provider.
type Management string

// Management kinds. A provider is either managed or connected in M2.
const (
	// Managed: Orglane runs the service through the agent.
	Managed Management = "managed"
	// Connected: an external service Orglane talks to.
	Connected Management = "connected"
)

// AccessPolicy says who gets access to an app instance.
type AccessPolicy string

// Access policies.
const (
	// AllPeople gives every active person access.
	AllPeople AccessPolicy = "all_people"
	// MappedTeams gives access to members of mapped teams only.
	MappedTeams AccessPolicy = "mapped_teams"
)

// CapabilityManifest is the only interface every adapter implements.
type CapabilityManifest interface {
	Manifest() Manifest
}

// Manifest describes an adapter.
type Manifest struct {
	ID ID
	// Name is the English display name; the UI translates it by
	// "providers.<id>.name".
	Name          string
	Capabilities  []Capability
	Management    Management
	Package       string // managed package ID (agent.md); "" for connected providers
	Features      []FeatureSupport
	Credentials   []CredentialSpec
	EmployeeSteps []EmployeeStepSpec
	// Settings are non-secret, provider-specific instance settings,
	// validated by the core.
	Settings      []SettingSpec
	DefaultKey    string       // default instance key, e.g. "git"
	DefaultPolicy AccessPolicy // all_people | mapped_teams
}

// Instance is the read-only view of an app instance passed to adapters.
type Instance struct {
	ID         uuid.UUID
	Key        string
	Provider   ID
	Management Management
	// BaseURL is the public base URL: https://git.acme.com,
	// https://github.com/acme. Nil until known.
	BaseURL *url.URL
	// Upstream is the direct URL on the edge network (managed only), e.g.
	// http://git-forgejo:3000.
	Upstream *url.URL
	// Settings are validated against Manifest.Settings.
	Settings map[string]any
}

// Env gives adapters the core services they may use. Nothing else is
// reachable.
type Env interface {
	// Credential returns the current plaintext of a credential of this
	// instance. Plaintext must not be logged, stored, returned in errors or
	// put in results.
	Credential(ctx context.Context, name string) ([]byte, error)
	// Credentials returns current and not yet retired previous versions,
	// newest first (webhook secret rotation).
	Credentials(ctx context.Context, name string) ([][]byte, error)
	// HTTPClient has timeouts, a response size limit, no cookies and
	// refuses redirects to another host.
	HTTPClient() *http.Client
	Logger() *slog.Logger
	Now() time.Time
	// URLs built from the core configuration (ADR 0018); adapters never
	// build hosts.
	WorkspaceURL() *url.URL
	IssuerURL() *url.URL
	WebhookURL(instanceID uuid.UUID) *url.URL
}

// Credential sources.
const (
	// SourceGenerated: the core makes 32 random bytes.
	SourceGenerated = "generated"
	// SourceProvider: from Connect, Setup or the provider itself.
	SourceProvider = "provider"
	// SourceUpload: uploaded by an admin, who installed it at the provider.
	// CredentialRotator.ApplyCredential only verifies an upload and never
	// changes provider state, so its rejection is always definitive.
	SourceUpload = "upload"
	// SourceIdentity: the secret of the instance's OIDC client, issued by
	// Orglane's OpenID Provider when the instance is created (see
	// OIDCRelyingParty). Not rotatable in M2.
	SourceIdentity = "identity"
)

// CredentialSpec declares a credential of an instance.
type CredentialSpec struct {
	Name      string // ^[a-z][a-z0-9_]{0,40}$, unique per provider
	Source    string // SourceGenerated | SourceProvider | SourceUpload
	Rotatable bool
	// GenerateRotation makes the core generate replacement values even when
	// the initial value came from the provider (GitHub webhook secrets).
	GenerateRotation bool
	// ReadBack: for a value the provider mints, ApplyCredential first reads
	// back what an earlier attempt for the same version did (Forgejo tokens
	// named after the version) and reconciles it. Without it, an attempt
	// after an unknown outcome is not made: another mint could invalidate
	// the one the earlier attempt published, so the candidate stays
	// uncertain until evidence, abandon or an admin's resolution.
	ReadBack bool
	// Overlap is how long the previous version stays valid after the new
	// one is acknowledged.
	Overlap time.Duration
	// Agent: passed to the agent as a secret input (managed packages).
	Agent bool
}

// EmployeeStepSpec declares an onboarding step the provider needs from
// employees.
type EmployeeStepSpec struct {
	ID          string // namespaced: "<provider>.<step>", e.g. "github.link-account"
	Title       string // English fallback; the UI translates by "onboarding.steps.<id>.title"
	Description string // English fallback; "onboarding.steps.<id>.description"
	ActionPath  string // local path, e.g. "/me/accounts/github" (onboarding.Registry rules)
	Required    bool   // M2: false for every provider step
}

// Setting types.
const (
	SettingString = "string"
	SettingInt    = "int"
	SettingBool   = "bool"
)

// SettingSpec declares a non-secret instance setting. Values are JSON
// scalars.
type SettingSpec struct {
	Name     string // ^[a-z][a-z0-9_]{0,40}$
	Type     string // SettingString | SettingInt | SettingBool
	Pattern  string // strings: RE2, anchored
	Enum     []string
	Min, Max int64 // ints
	Default  any
	Mutable  bool // false: fixed after the instance is created (e.g. organization)
	// Required: the instance cannot work without it. The core refuses to
	// create an instance without it (unless Default fills it) and never
	// removes it. An immutable setting the adapter needs must be required,
	// or an instance created without it could never be repaired.
	Required bool
}

// Team mapping modes.
const (
	// MappingCreate: Orglane creates a provider team named after the
	// Orglane team slug.
	MappingCreate = "create"
	// MappingExisting: the admin picks an existing provider team.
	MappingExisting = "existing"
)

// TeamMapping is one row of app_team_mappings as adapters see it.
type TeamMapping struct {
	TeamID      uuid.UUID
	TeamSlug    string // Orglane team slug; name for teams the adapter creates
	TeamName    string
	Mode        string // MappingCreate | MappingExisting
	ExternalRef string // provider team ID; empty until created (mode "create")
}

// TeamRef identifies a provider-side team.
type TeamRef struct {
	TeamID      uuid.UUID // the Orglane team it is mapped from
	ExternalRef string    // provider team ID
	Name        string    // provider slug or name, informational
}

// Account link methods.
const (
	LinkOAuth       = "oauth"
	LinkProvisioned = "provisioned"
)

// AccountLink is a verified link as adapters see it.
type AccountLink struct {
	ID         uuid.UUID
	Account    ExternalAccount
	Method     string // LinkOAuth | LinkProvisioned
	VerifiedAt time.Time
}

// Inputs are what Lifecycle.Inputs adds to the desired spec (agent.md
// "Desired spec").
type Inputs struct {
	// Settings are package settings (validated by the agent against the
	// package manifest).
	Settings map[string]any
	// Secrets are credential names sent as secrets; they must be
	// CredentialSpec entries with Agent = true.
	Secrets []string
}

// ConnectStep tells the UI what to do next during Connector flows.
type ConnectStep struct {
	Name   string            // provider-defined step name, e.g. "create_app", "install_app"
	Method string            // "GET" (redirect) | "POST" (auto-submitted form)
	URL    *url.URL          // provider URL (host checked against the provider's allowlist)
	Form   map[string]string // form fields for POST
}

// UserProvisioner gives people access to an app instance.
type UserProvisioner interface {
	// Plan reads the provider's current state for one person and returns
	// the actions that make it match desired. Plan must not change provider
	// state.
	Plan(ctx context.Context, env Env, inst Instance, d DesiredAccess) (Plan, error)
	// Apply performs one action. It must be idempotent ("ensure"
	// semantics): applying an already satisfied action succeeds without
	// changes.
	Apply(ctx context.Context, env Env, inst Instance, a Action) (ActionOutcome, error)
	// SyncTeams makes the provider-side teams for the instance's team
	// mappings exist and returns their external references. Membership is
	// per person.
	SyncTeams(ctx context.Context, env Env, inst Instance, teams []TeamMapping) ([]TeamRef, error)
	// Members lists the provider-side accounts of the instance (for the
	// "unmanaged accounts" report and drift checks). Paged by the adapter.
	Members(ctx context.Context, env Env, inst Instance) ([]ExternalAccount, error)
}

// DesiredAccess is what one person should have in an app instance.
type DesiredAccess struct {
	PersonID    uuid.UUID
	Username    string // people.username
	DisplayName string
	WorkEmail   string
	Access      bool      // false: the person must not have access
	Teams       []TeamRef // provider-side teams the person must be in (exactly these, among mapped teams)
	// Mapped are the provider-side teams of all the instance's mappings
	// (archived Orglane teams included), whether or not the person is in
	// them: the teams Orglane manages membership of. Adapters that cannot
	// mark the teams they manage (GitHub teams picked with mode "existing")
	// plan team removals only from these.
	Mapped []TeamRef
	Link   *AccountLink // verified link in this provider realm, if any
	// Managed: removal allowed (Orglane granted the access or the instance
	// manages existing members).
	Managed bool
}

// RemovalChecker is optional. It reports whether a queued removal still
// targets access the adapter manages, given the person's current desired
// access. The core checks it when it tests a removal's freshness and again
// before applying it. GitHub manages team membership only of the instance's
// current mappings, so deleting a mapping cancels pending and held removals
// from that team; a Forgejo team stays Orglane's without a mapping. Pure: no
// provider calls.
type RemovalChecker interface {
	RemovalStillManaged(inst Instance, d DesiredAccess, a Action) bool
}

// Plan is the result of UserProvisioner.Plan.
type Plan struct {
	Actions []Action
	// EmployeeStep is set when nothing can be done until the person acts
	// (GitHub: no verified link). Status becomes pending_employee_action.
	EmployeeStep string
}

// ActionKind names an action.
type ActionKind string

// Action kinds in M2. Offboarding kinds (suspend_account, revoke_tokens,
// remove_ssh_keys) are reserved for M5.
const (
	ActionCreateAccount    ActionKind = "create_account"
	ActionUpdateAccount    ActionKind = "update_account"
	ActionInvite           ActionKind = "invite"
	ActionCancelInvitation ActionKind = "cancel_invitation"
	ActionAddToTeam        ActionKind = "add_to_team"
	ActionRemoveFromTeam   ActionKind = "remove_from_team"
	ActionRemoveFromOrg    ActionKind = "remove_from_org"
)

// Action is one step of a plan.
type Action struct {
	Kind   ActionKind        `json:"kind"`
	Key    string            `json:"key"`              // stable idempotency key, e.g. "add_to_team:<team-id>"
	Params map[string]string `json:"params,omitempty"` // non-secret
	// Removal is true for actions that take access away (removal guard).
	Removal bool `json:"removal,omitempty"`
	// ConfirmOnly asks a removal the provider accepted earlier to be confirmed
	// by a read only, never by issuing the removal again: the target may be a
	// new grant since the desired access changed. Adapters that return
	// CodeRemovalPending honor it; others never receive it.
	ConfirmOnly bool `json:"confirm_only,omitempty"`
}

// ActionOutcome is the result of UserProvisioner.Apply.
type ActionOutcome struct {
	Changed bool // false: already satisfied
	// Link is the account the action concerns (created, or found by an
	// ensure that finds it already there).
	Link *ExternalAccount
	// Proof is set only when the adapter can prove that Orglane's call made
	// the grant: the confirmed response of this call (ProofResponse), or a
	// provider-side marker an ensure checks (ProofMarker: e.g. Forgejo, an
	// account bound to Orglane's auth source with this person's subject;
	// GitHub, an invitation or membership created by Orglane's App
	// installation). The core grants ownership (granted_by_orglane) and
	// stores Link as a verified "provisioned" link only with a Proof. It
	// never infers ownership from access being there now.
	Proof *Proof
}

// Proof kinds.
const (
	// ProofResponse: the provider confirmed that this call made the change.
	ProofResponse = "response"
	// ProofMarker: a provider-side marker shows that Orglane's earlier call
	// for this person made the change.
	ProofMarker = "marker"
)

// Proof is provider-verifiable evidence that Orglane's call made a grant.
type Proof struct {
	Kind string // ProofResponse | ProofMarker
	// Evidence names what was checked, without secrets or personal data,
	// e.g. "auth_source", "invitation:12345". Recorded in the audit entry.
	Evidence string
}

// AccountVerifier proves that a person owns an account at the provider.
// Providers whose accounts Orglane creates (Forgejo) do not implement it:
// creation is the proof.
type AccountVerifier interface {
	// BeginVerify returns the provider URL the person's browser is sent to.
	// state and the PKCE verifier are generated and stored by the core.
	BeginVerify(ctx context.Context, env Env, inst Instance, state, codeChallenge string) (*url.URL, error)
	// CompleteVerify exchanges the callback parameters for the account that
	// authorized. Any token obtained is used once and revoked; it is never
	// stored.
	CompleteVerify(ctx context.Context, env Env, inst Instance, code, codeVerifier string) (ExternalAccount, error)
}

// ExternalAccount is an account at a provider.
type ExternalAccount struct {
	Realm      string // "github.com"; for Forgejo the instance ID
	ExternalID string // provider's immutable numeric/opaque user ID, as text
	Login      string // current login, informational (logins change)
}

// EventSource receives webhooks.
type EventSource interface {
	// DeliveryID returns the provider's unique delivery ID from the headers.
	DeliveryID(h http.Header) (string, error)
	// Verify checks the signature over the raw body with each candidate
	// secret (Env.Credentials("webhook_secret")). Constant-time comparison.
	// It returns ErrBadSignature when no candidate verifies.
	Verify(ctx context.Context, env Env, inst Instance, h http.Header, body []byte) error
	// Normalize turns a verified delivery into events. It must be a pure
	// function of its input: no provider API calls, no clock except
	// d.ReceivedAt. Every Event.DedupeKey is derived from the signed body
	// only. ErrForeignDelivery marks an authentic delivery that is not for
	// this instance. A nil slice with a nil error means the event name is
	// not normalized (the delivery is ignored).
	Normalize(inst Instance, d Delivery) ([]Event, error)
	// EnsureWebhooks creates or updates provider-side webhooks pointing at
	// env.WebhookURL(inst.ID).
	EnsureWebhooks(ctx context.Context, env Env, inst Instance) error
}

// Delivery is a verified webhook delivery.
type Delivery struct {
	ID         string
	EventName  string      // X-GitHub-Event / X-Forgejo-Event
	Headers    http.Header // allowlisted headers only
	Body       []byte
	ReceivedAt time.Time
}

// DeliveryEffects is implemented by event sources whose deliveries report
// changes of the connection itself (GitHub: the App's installation was
// deleted or suspended, its webhook was deleted) or of provider-side access
// that provisioning should look at again (GitHub: organization and team
// membership changed outside Orglane).
type DeliveryEffects interface {
	// Effects returns what a verified delivery changes besides its events.
	// Like Normalize it is a pure function of its input, decides from
	// signed body fields only, and returns ErrForeignDelivery for a
	// delivery that is not for this instance. The core calls it before
	// Normalize, in the transaction that stores the delivery's events.
	Effects(inst Instance, d Delivery) (Effect, error)
}

// Instance states an Effect may ask for.
const (
	EffectDegraded     = "degraded"
	EffectDisconnected = "disconnected"
	// EffectRestored returns a degraded instance to active, only if its
	// health code is Effect.Code (it clears what an earlier effect set and
	// nothing else).
	EffectRestored = "restored"
)

// Effect is what a delivery changes besides its events. The zero Effect
// changes nothing.
type Effect struct {
	// State is "", EffectDegraded, EffectDisconnected or EffectRestored.
	State string
	// Code is the health code (lower_snake_case) State sets, or for
	// EffectRestored the one it clears.
	Code string
	// Resync asks for a drift run of the instance's provisioning: access
	// changed at the provider, so desired and actual access are compared
	// again (plans are idempotent; nothing is removed outside the guard).
	Resync bool
}

// DeepLinker builds links into the provider.
type DeepLinker interface {
	// OpenURL returns the URL that opens target in the provider, signed in
	// through Orglane when the provider supports it. target is a validated
	// relative path. The result's scheme and host must equal inst.BaseURL's;
	// the core checks it again.
	OpenURL(inst Instance, target string) (*url.URL, error)
}

// Lifecycle is implemented by managed providers.
type Lifecycle interface {
	// Inputs returns the provider-specific template settings and the names
	// of the credentials to pass as secrets to the agent.
	Inputs(ctx context.Context, env Env, inst Instance) (Inputs, error)
	// Setup runs once after the first successful install. Idempotent;
	// retried on failure.
	Setup(ctx context.Context, env Env, inst Instance) error
	// CheckHealth is the provider-level health check through
	// inst.Upstream. The core calls it to confirm upgrades and on every
	// health sync. (Not named Verify: EventSource.Verify has another
	// signature and a managed provider implements both.)
	CheckHealth(ctx context.Context, env Env, inst Instance) error
}

// Connector connects an external provider (GitHub App). Steps are provider
// specific; the core stores the state token and the instance row between
// them.
type Connector interface {
	BeginConnect(ctx context.Context, env Env, inst Instance, state string) (ConnectStep, error)
	CompleteConnect(ctx context.Context, env Env, inst Instance, step string, params url.Values) (ConnectResult, error)
}

// ConnectResult is the result of one Connector step.
type ConnectResult struct {
	Done     bool
	Next     *ConnectStep   // e.g. "install the App on the organization"
	Settings map[string]any // e.g. app_id, installation_id, organization id and login
	// Credentials are stored encrypted by the core, then zeroed.
	Credentials map[string][]byte
	BaseURL     *url.URL
}

// RotationOutcome classifies one attempt to hand a credential version to
// its consumer (docs/specs/providers.md "Credentials", step 3).
type RotationOutcome string

// Rotation outcomes. Only a proven rejection discards a candidate.
const (
	// RotationAcknowledged: the provider answered success, or a read-back
	// proves the new value is in use.
	RotationAcknowledged RotationOutcome = "acknowledged"
	// RotationRejected: a definite answer that this attempt did not make
	// the change. It says nothing about an earlier attempt whose outcome was
	// unknown, so the core discards only a candidate whose first handover
	// was rejected, or an upload (SourceUpload).
	RotationRejected RotationOutcome = "rejected"
	// RotationUnknown: the provider may or may not have committed the
	// change (timeout, connection reset, 5xx).
	RotationUnknown RotationOutcome = "unknown"
)

// CredentialRotator hands new credential versions to the provider. It is
// implemented by adapters with Rotatable credentials that the provider (not
// the agent) consumes.
type CredentialRotator interface {
	// ApplyCredential makes the provider use value as version version of
	// credential name. It must be idempotent: setting the same value again
	// succeeds. value is nil for credentials the provider mints itself
	// (Source "provider"); the adapter then returns the minted value in
	// Rotation.Value, naming it after the version so a read-back can tell
	// whether an earlier attempt committed.
	ApplyCredential(ctx context.Context, env Env, inst Instance, name string, version int, value []byte) (Rotation, error)
}

// Rotation is the result of CredentialRotator.ApplyCredential.
type Rotation struct {
	Outcome RotationOutcome
	// Value is the minted value for credentials the provider mints; nil
	// otherwise.
	Value []byte
}

// OIDCRelyingParty is implemented by adapters whose service signs people in
// with Orglane (docs/specs/providers.md "Forgejo OIDC configured
// automatically"). When such an instance is created, the core registers a
// confidential OIDC client (client_secret_basic) with the ID
// OIDCClientID(key) in the same transaction and stores its secret as the
// named credential.
type OIDCRelyingParty interface {
	// OIDCClient returns the client to register for an instance. Redirect
	// URIs are built from inst.BaseURL, scheme and port included; adapters
	// never build hosts.
	OIDCClient(inst Instance) (OIDCClient, error)
}

// OIDCClient describes an instance's OIDC client.
type OIDCClient struct {
	RedirectURIs           []string
	PostLogoutRedirectURIs []string
	// Secret names the CredentialSpec (Source SourceIdentity) that holds
	// the client secret.
	Secret string
}

// OIDCClientID returns the ID of an instance's OIDC client: "app-<key>".
func OIDCClientID(key string) string { return "app-" + key }

// TeamMappingValidator refuses team mappings a provider cannot honor, e.g.
// mode "existing" where the provider only manages teams it creates. The
// core calls it before storing a mapping. Pure: no provider calls.
type TeamMappingValidator interface {
	ValidateTeamMapping(inst Instance, mode, externalRef string) error
}

// TeamLister lists the provider-side teams an admin can pick for a mapping
// of mode "existing" (GitHub). Read-only.
type TeamLister interface {
	ProviderTeams(ctx context.Context, env Env, inst Instance) ([]Team, error)
}

// Team is a provider-side team as TeamLister lists it.
type Team struct {
	ExternalRef string // provider team ID, the mapping's external reference
	Name        string // display name
	Slug        string // provider slug, informational
	// Managed: Orglane created the team for a "create" mapping (marked
	// so), so it is not offered for "existing" mappings.
	Managed bool
}

// SettingsValidator checks instance settings beyond what SettingSpec can
// express (e.g. a product's reserved names). The core calls it with the
// merged, typed settings when an instance is created and when its settings
// change, before anything is stored; an error refuses the change. It must
// be a pure function of its input.
type SettingsValidator interface {
	ValidateSettings(settings map[string]any) error
}

// SettingDefaulter gives defaults of settings that depend on the
// installation (e.g. an organization named after the company's domain).
// The core applies them when an instance is created without the setting
// and shows them in the install form. domainBase is the company's base
// domain (config domain.base, e.g. acme.com or orglane.localhost). It must
// be a pure function of its input; a default it cannot derive is left out.
type SettingDefaulter interface {
	SettingDefaults(domainBase string) map[string]any
}

// SettingError is a refused setting: Setting names it, Reason says why in
// English (no secrets: settings are not secret).
type SettingError struct {
	Setting string
	Reason  string
}

func (e *SettingError) Error() string { return "provider: setting " + e.Setting + ": " + e.Reason }

// AccountRealmer names the realm of a provider's accounts for an instance:
// accounts with the same realm are the same accounts, whichever instance
// they were linked through (GitHub: "github.com"). Without it the realm is
// the instance ID: accounts exist only in that instance (managed Forgejo).
type AccountRealmer interface {
	AccountRealm(inst Instance) string
}
