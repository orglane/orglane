// Package testprovider is an in-memory fake provider for the framework's
// own tests (docs/specs/providers.md "Testing strategy"). It keeps its
// "provider-side" accounts, teams and webhook secrets in memory and lets
// tests inject failures, so jobs, results, retries, rotation and the
// removal guard can be exercised without a real service.
//
// It is not a mock of any real provider's API and must never be used to
// test a real adapter. It is registered only in tests and in test builds
// (build tag appstest).
package testprovider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

// ID is the default provider ID.
const ID provider.ID = "testprovider"

// Webhook headers. The stored-header allowlist is fixed in M2, so the test
// provider names its delivery and event headers like Forgejo's; its
// signature header is its own.
const (
	SignatureHeader = "X-Testprovider-Signature"
	DeliveryHeader  = "X-Forgejo-Delivery"
	EventHeader     = "X-Forgejo-Event"
)

// StepLinkAccount is the employee step of instances with require_link.
const StepLinkAccount = "link-account"

// Account is a provider-side account.
type Account struct {
	ID          string
	Login       string
	DisplayName string
	Email       string
	Member      bool
	Teams       map[string]bool // external team refs
	// CreatedFor is the provider-side marker of an account Orglane created:
	// the person it was created for (like Forgejo's auth-source binding).
	// Empty for accounts created outside Orglane.
	CreatedFor string
	// TeamsFor marks team memberships Orglane added, by team ref.
	TeamsFor map[string]string
}

type org struct {
	accounts map[string]*Account // by ID
	teams    map[string]string   // ref -> name
	secrets  map[string][]byte   // credential name -> value in use
	hooks    int
	setups   int
}

// Provider is the fake. The zero value is not usable; call New.
type Provider struct {
	id         provider.ID
	management provider.Management

	mu       sync.Mutex
	orgs     map[uuid.UUID]*org
	faults   map[string][]error
	outcomes map[string][]provider.RotationOutcome
	lose     map[string]int
	blocks   map[string]*block
	calls    []string
	nextID   int
	// packageSettings are what Inputs returns (managed variant).
	packageSettings map[string]any
	// openOrigin, if set, is the origin OpenURL links to instead of the
	// base URL's (to test the core's origin check).
	openOrigin *url.URL
}

// Option configures New.
type Option func(*Provider)

// WithID registers the provider under another ID (for tests with two
// providers).
func WithID(id provider.ID) Option { return func(p *Provider) { p.id = id } }

// Managed makes the provider a managed one with a Lifecycle.
func Managed() Option { return func(p *Provider) { p.management = provider.Managed } }

// New returns a fake provider.
func New(opts ...Option) *Provider {
	p := &Provider{
		id:         ID,
		management: provider.Connected,
		orgs:       map[uuid.UUID]*org{},
		faults:     map[string][]error{},
		outcomes:   map[string][]provider.RotationOutcome{},
		lose:       map[string]int{},
		blocks:     map[string]*block{},
		nextID:     1000,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Manifest implements provider.CapabilityManifest.
func (p *Provider) Manifest() provider.Manifest {
	m := provider.Manifest{
		ID:           p.id,
		Name:         "Test provider",
		Capabilities: []provider.Capability{"code"},
		Management:   p.management,
		Features: []provider.FeatureSupport{
			{Feature: provider.FeatureSSO, Support: provider.Unsupported},
			{Feature: provider.FeatureDeepLinks, Support: provider.Partial, Note: "providers.testprovider.features.deep_links_direct"},
			{Feature: provider.FeatureAccountsCreate, Support: provider.Supported},
			{Feature: provider.FeatureAccountsInvite, Support: provider.Unsupported},
			{Feature: provider.FeatureAccountsLink, Support: provider.Supported},
			{Feature: provider.FeatureTeamsSync, Support: provider.Supported},
			{Feature: provider.FeatureDeprovisionTeamRemoval, Support: provider.Supported},
			{Feature: provider.FeatureDeprovisionOrgRemoval, Support: provider.Supported},
			{Feature: provider.FeatureDeprovisionSuspend, Support: provider.Planned, Milestone: "M5"},
			{Feature: provider.FeatureDeprovisionRevokeTokens, Support: provider.Planned, Milestone: "M5"},
			{Feature: provider.FeatureEventsWebhooks, Support: provider.Supported},
			{Feature: provider.FeatureLifecycleInstall, Support: provider.Unsupported},
			{Feature: provider.FeatureLifecycleUpgrade, Support: provider.Unsupported},
			{Feature: provider.FeatureLifecycleSnapshot, Support: provider.Unsupported},
			{Feature: provider.FeatureLifecycleBackup, Support: provider.Unsupported},
		},
		Credentials: []provider.CredentialSpec{
			{Name: "webhook_secret", Source: provider.SourceGenerated, Rotatable: true, Overlap: 24 * time.Hour},
			{Name: "api_token", Source: provider.SourceProvider, Rotatable: true, Overlap: time.Hour},
		},
		EmployeeSteps: []provider.EmployeeStepSpec{{
			ID:          string(p.id) + "." + StepLinkAccount,
			Title:       "Link your test account",
			Description: "Prove which test provider account is yours.",
			ActionPath:  "/me/accounts/" + string(p.id),
		}},
		Settings: []provider.SettingSpec{
			{Name: "organization", Type: provider.SettingString, Pattern: `[a-z0-9-]{1,39}`, Default: "acme"},
			{Name: "require_link", Type: provider.SettingBool, Default: false, Mutable: true},
		},
		DefaultKey:    "test",
		DefaultPolicy: provider.AllPeople,
	}
	if p.management == provider.Managed {
		m.Name = "Managed test provider"
		m.Package = string(p.id)
		m.DefaultKey = "managed-test"
		// The package's database password (the agenttest package declares
		// it), passed to the agent as a secret.
		m.Credentials = append(m.Credentials, provider.CredentialSpec{Name: "db_password", Source: provider.SourceGenerated, Agent: true})
	}
	return m
}

// SetPackageSettings sets the package settings Inputs returns (managed
// variant), e.g. a free host port for the agenttest package.
func (p *Provider) SetPackageSettings(settings map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.packageSettings = maps.Clone(settings)
}

// Fail makes the next calls of op fail with errs, one per call. Ops:
// "plan", "apply:<action key>", "apply:<action kind>", "sync_teams",
// "members", "setup", "inputs", "health", "rotate:<credential name>".
func (p *Provider) Fail(op string, errs ...error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.faults[op] = append(p.faults[op], errs...)
}

func (p *Provider) fault(ops ...string) error {
	for _, op := range ops {
		if errs := p.faults[op]; len(errs) > 0 {
			p.faults[op] = errs[1:]
			return errs[0]
		}
	}
	return nil
}

type block struct {
	reached chan struct{}
	release chan struct{}
}

// Block makes the next call of op ("apply:<action key>", "rotate:<name>")
// wait: reached is closed when the call arrives, and the call continues
// once release is closed. Tests use it to interleave jobs.
func (p *Provider) Block(op string) (reached <-chan struct{}, release chan<- struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := &block{reached: make(chan struct{}), release: make(chan struct{})}
	p.blocks[op] = b
	return b.reached, b.release
}

// wait blocks the calling operation if a test asked for it.
func (p *Provider) wait(op string) {
	p.mu.Lock()
	b := p.blocks[op]
	delete(p.blocks, op)
	p.mu.Unlock()
	if b != nil {
		close(b.reached)
		<-b.release
	}
}

// Calls returns the calls made so far ("plan:<username>", "apply:<key>", ...).
func (p *Provider) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *Provider) org(inst provider.Instance) *org {
	o := p.orgs[inst.ID]
	if o == nil {
		o = &org{accounts: map[string]*Account{}, teams: map[string]string{}, secrets: map[string][]byte{}}
		p.orgs[inst.ID] = o
	}
	return o
}

// AddMember adds a pre-existing member that Orglane did not create.
func (p *Provider) AddMember(instanceID uuid.UUID, login string) Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	o := p.org(provider.Instance{ID: instanceID})
	p.nextID++
	a := &Account{ID: strconv.Itoa(p.nextID), Login: login, Member: true, Teams: map[string]bool{}}
	o.accounts[a.ID] = a
	return *a
}

// AddTeam adds a provider-side team (for "existing" mappings).
func (p *Provider) AddTeam(instanceID uuid.UUID, ref, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.org(provider.Instance{ID: instanceID}).teams[ref] = name
}

// Account returns an account by login.
func (p *Provider) Account(instanceID uuid.UUID, login string) (Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.org(provider.Instance{ID: instanceID}).accounts {
		if a.Login == login {
			c := *a
			c.Teams = map[string]bool{}
			for k, v := range a.Teams {
				c.Teams[k] = v
			}
			return c, true
		}
	}
	return Account{}, false
}

// Teams returns the provider-side teams by ref.
func (p *Provider) Teams(instanceID uuid.UUID) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]string{}
	for k, v := range p.org(provider.Instance{ID: instanceID}).teams {
		out[k] = v
	}
	return out
}

// Secret returns the value the "provider" uses for a credential.
func (p *Provider) Secret(instanceID uuid.UUID, name string) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.org(provider.Instance{ID: instanceID}).secrets[name])
}

// Setups returns how often Setup succeeded for an instance.
func (p *Provider) Setups(instanceID uuid.UUID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.org(provider.Instance{ID: instanceID}).setups
}

func (p *Provider) find(o *org, d provider.DesiredAccess) *Account {
	if d.Link != nil {
		return o.accounts[d.Link.Account.ExternalID]
	}
	for _, a := range o.accounts {
		if a.Login == d.Username {
			return a
		}
	}
	return nil
}

// Plan implements provider.UserProvisioner.
func (p *Provider) Plan(_ context.Context, _ provider.Env, inst provider.Instance, d provider.DesiredAccess) (provider.Plan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "plan:"+d.Username)
	if err := p.fault("plan"); err != nil {
		return provider.Plan{}, err
	}
	o := p.org(inst)
	if requireLink, _ := inst.Settings["require_link"].(bool); requireLink && d.Access && d.Link == nil {
		return provider.Plan{EmployeeStep: string(p.id) + "." + StepLinkAccount}, nil
	}
	acct := p.find(o, d)
	var plan provider.Plan
	params := map[string]string{"person_id": d.PersonID.String(), "username": d.Username, "display_name": d.DisplayName, "email": d.WorkEmail}
	if !d.Access {
		if acct == nil || !acct.Member || !d.Managed {
			return plan, nil
		}
		for _, ref := range sortedKeys(acct.Teams) {
			plan.Actions = append(plan.Actions, provider.Action{Kind: provider.ActionRemoveFromTeam, Key: "remove_from_team:" + ref, Params: map[string]string{"account": acct.ID, "team": ref}, Removal: true})
		}
		plan.Actions = append(plan.Actions, provider.Action{Kind: provider.ActionRemoveFromOrg, Key: "remove_from_org", Params: map[string]string{"account": acct.ID}, Removal: true})
		return plan, nil
	}
	if acct == nil || !acct.Member {
		plan.Actions = append(plan.Actions, provider.Action{Kind: provider.ActionCreateAccount, Key: "create_account", Params: params})
	} else if acct.DisplayName != d.DisplayName || acct.Email != d.WorkEmail {
		params["account"] = acct.ID
		plan.Actions = append(plan.Actions, provider.Action{Kind: provider.ActionUpdateAccount, Key: "update_account", Params: params})
	}
	want := map[string]bool{}
	for _, t := range d.Teams {
		want[t.ExternalRef] = true
		if acct == nil || !acct.Teams[t.ExternalRef] {
			plan.Actions = append(plan.Actions, provider.Action{Kind: provider.ActionAddToTeam, Key: "add_to_team:" + t.ExternalRef, Params: map[string]string{"person_id": d.PersonID.String(), "username": d.Username, "team": t.ExternalRef}})
		}
	}
	if acct != nil && d.Managed {
		for _, ref := range sortedKeys(acct.Teams) {
			if !want[ref] {
				plan.Actions = append(plan.Actions, provider.Action{Kind: provider.ActionRemoveFromTeam, Key: "remove_from_team:" + ref, Params: map[string]string{"account": acct.ID, "team": ref}, Removal: true})
			}
		}
	}
	return plan, nil
}

// Apply implements provider.UserProvisioner. Every action is an "ensure".
func (p *Provider) Apply(_ context.Context, _ provider.Env, inst provider.Instance, a provider.Action) (provider.ActionOutcome, error) {
	p.wait("apply:" + a.Key)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "apply:"+a.Key)
	if err := p.fault("apply:"+a.Key, "apply:"+string(a.Kind)); err != nil {
		return provider.ActionOutcome{}, err
	}
	o := p.org(inst)
	byLogin := func(login string) *Account {
		for _, acct := range o.accounts {
			if acct.Login == login {
				return acct
			}
		}
		return nil
	}
	switch a.Kind {
	case provider.ActionCreateAccount:
		acct := byLogin(a.Params["username"])
		if acct != nil && acct.Member {
			// Already there: unchanged. The marker proves whether Orglane's
			// earlier call created it for this person.
			out := provider.ActionOutcome{Link: &provider.ExternalAccount{Realm: inst.ID.String(), ExternalID: acct.ID, Login: acct.Login}}
			if acct.CreatedFor != "" && acct.CreatedFor == a.Params["person_id"] {
				out.Proof = &provider.Proof{Kind: provider.ProofMarker, Evidence: "created_for"}
			}
			return out, nil
		}
		if acct == nil {
			p.nextID++
			acct = &Account{ID: strconv.Itoa(p.nextID), Login: a.Params["username"], Teams: map[string]bool{}}
			o.accounts[acct.ID] = acct
		}
		acct.Member, acct.DisplayName, acct.Email, acct.CreatedFor = true, a.Params["display_name"], a.Params["email"], a.Params["person_id"]
		return provider.ActionOutcome{Changed: true, Link: &provider.ExternalAccount{Realm: inst.ID.String(), ExternalID: acct.ID, Login: acct.Login},
			Proof: &provider.Proof{Kind: provider.ProofResponse, Evidence: "create_account"}}, nil
	case provider.ActionUpdateAccount:
		acct := o.accounts[a.Params["account"]]
		if acct == nil {
			return provider.ActionOutcome{}, provider.ErrNotFound
		}
		changed := acct.DisplayName != a.Params["display_name"] || acct.Email != a.Params["email"]
		acct.DisplayName, acct.Email = a.Params["display_name"], a.Params["email"]
		return provider.ActionOutcome{Changed: changed}, nil
	case provider.ActionAddToTeam:
		acct := byLogin(a.Params["username"])
		if acct == nil {
			return provider.ActionOutcome{}, &provider.Error{Code: "no_account", Retryable: true}
		}
		if _, ok := o.teams[a.Params["team"]]; !ok {
			return provider.ActionOutcome{}, &provider.Error{Code: "unknown_team"}
		}
		if acct.Teams[a.Params["team"]] {
			out := provider.ActionOutcome{}
			if by := acct.TeamsFor[a.Params["team"]]; by != "" && by == a.Params["person_id"] {
				out.Proof = &provider.Proof{Kind: provider.ProofMarker, Evidence: "team_added_for"}
			}
			return out, nil
		}
		acct.Teams[a.Params["team"]] = true
		if acct.TeamsFor == nil {
			acct.TeamsFor = map[string]string{}
		}
		acct.TeamsFor[a.Params["team"]] = a.Params["person_id"]
		return provider.ActionOutcome{Changed: true, Proof: &provider.Proof{Kind: provider.ProofResponse, Evidence: "add_to_team"}}, nil
	case provider.ActionRemoveFromTeam:
		acct := o.accounts[a.Params["account"]]
		if acct == nil || !acct.Teams[a.Params["team"]] {
			return provider.ActionOutcome{}, nil
		}
		delete(acct.Teams, a.Params["team"])
		delete(acct.TeamsFor, a.Params["team"])
		return provider.ActionOutcome{Changed: true}, nil
	case provider.ActionRemoveFromOrg:
		acct := o.accounts[a.Params["account"]]
		if acct == nil || !acct.Member {
			return provider.ActionOutcome{}, nil
		}
		acct.Member = false
		acct.Teams = map[string]bool{}
		acct.TeamsFor = nil
		return provider.ActionOutcome{Changed: true}, nil
	}
	return provider.ActionOutcome{}, &provider.Error{Code: "unknown_action"}
}

// SyncTeams implements provider.UserProvisioner: "create" mappings get a
// team named after the Orglane team slug.
func (p *Provider) SyncTeams(_ context.Context, _ provider.Env, inst provider.Instance, teams []provider.TeamMapping) ([]provider.TeamRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "sync_teams")
	if err := p.fault("sync_teams"); err != nil {
		return nil, err
	}
	o := p.org(inst)
	refs := make([]provider.TeamRef, 0, len(teams))
	for _, t := range teams {
		ref := t.ExternalRef
		switch {
		case ref != "":
			if _, ok := o.teams[ref]; !ok && t.Mode == provider.MappingCreate {
				o.teams[ref] = t.TeamSlug
			}
		case t.Mode == provider.MappingCreate:
			ref = "team-" + t.TeamSlug
			o.teams[ref] = t.TeamSlug
		}
		name, ok := o.teams[ref]
		if !ok {
			continue // an existing team that is gone
		}
		refs = append(refs, provider.TeamRef{TeamID: t.TeamID, ExternalRef: ref, Name: name})
	}
	return refs, nil
}

// Members implements provider.UserProvisioner.
func (p *Provider) Members(_ context.Context, _ provider.Env, inst provider.Instance) ([]provider.ExternalAccount, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fault("members"); err != nil {
		return nil, err
	}
	var out []provider.ExternalAccount
	for _, a := range p.org(inst).accounts {
		if a.Member {
			out = append(out, provider.ExternalAccount{Realm: inst.ID.String(), ExternalID: a.ID, Login: a.Login})
		}
	}
	slices.SortFunc(out, func(a, b provider.ExternalAccount) int { return strings.Compare(a.ExternalID, b.ExternalID) })
	return out, nil
}

// Sign returns the signature header value of body under secret.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// DeliveryID implements provider.EventSource.
func (p *Provider) DeliveryID(h http.Header) (string, error) {
	id := h.Get(DeliveryHeader)
	if id == "" {
		return "", errors.New("testprovider: no delivery ID")
	}
	return id, nil
}

// Verify implements provider.EventSource.
func (p *Provider) Verify(ctx context.Context, env provider.Env, _ provider.Instance, h http.Header, body []byte) error {
	got, err := hex.DecodeString(h.Get(SignatureHeader))
	if err != nil || len(got) != sha256.Size {
		return provider.ErrBadSignature
	}
	secrets, err := env.Credentials(ctx, "webhook_secret")
	if err != nil {
		return err
	}
	for _, secret := range secrets {
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		if hmac.Equal(mac.Sum(nil), got) {
			return nil
		}
	}
	return provider.ErrBadSignature
}

// Payload is the JSON body of test deliveries.
type Payload struct {
	Organization string   `json:"organization"`
	Action       string   `json:"action"`
	ID           int64    `json:"id"`
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	Actor        string   `json:"actor"`
	ActorID      int64    `json:"actor_id"`
	Reviewers    []string `json:"reviewers"`
}

// Normalize implements provider.EventSource: "pull_request" deliveries
// with action "opened" become code.pull_request.opened; "ping" and other
// events are not normalized.
func (p *Provider) Normalize(inst provider.Instance, d provider.Delivery) ([]provider.Event, error) {
	if d.EventName != "pull_request" {
		return nil, nil
	}
	var pl Payload
	if err := json.Unmarshal(d.Body, &pl); err != nil {
		return nil, &provider.Error{Code: "invalid_payload", Err: errors.New("testprovider: payload is not JSON")}
	}
	if org, _ := inst.Settings["organization"].(string); pl.Organization != org {
		return nil, fmt.Errorf("%w", provider.ErrForeignDelivery)
	}
	if pl.Action != "opened" || pl.ID <= 0 {
		return []provider.Event{}, nil
	}
	title := pl.Title
	if r := []rune(title); len(r) > 256 {
		title = string(r[:256])
	}
	e := provider.Event{
		Type:          "code.pull_request.opened",
		SchemaVersion: 1,
		DedupeKey:     fmt.Sprintf("pr:%d:opened", pl.ID),
		OccurredAt:    d.ReceivedAt,
		Actor:         provider.ExternalRef{ExternalID: strconv.FormatInt(pl.ActorID, 10), Login: pl.Actor},
		Subject:       provider.Subject{Kind: "pull_request", ExternalID: strconv.FormatInt(pl.ID, 10), Repository: pl.Organization + "/repo", Number: pl.Number, Title: title},
		Targets:       []provider.Target{},
		Data:          map[string]any{"draft": false},
	}
	for _, r := range pl.Reviewers {
		e.Targets = append(e.Targets, provider.Target{ExternalRef: provider.ExternalRef{Login: r}, Reason: "review_requested"})
	}
	return []provider.Event{e}, nil
}

// EnsureWebhooks implements provider.EventSource.
func (p *Provider) EnsureWebhooks(_ context.Context, _ provider.Env, inst provider.Instance) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.org(inst).hooks++
	return nil
}

// LoseResponses makes the next n credential changes of name commit at the
// "provider" but answer RotationUnknown (the response was lost).
func (p *Provider) LoseResponses(name string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lose[name] += n
}

// RotationOutcomes makes the next credential changes of name answer these
// outcomes without committing (RotationAcknowledged still commits).
func (p *Provider) RotationOutcomes(name string, outcomes ...provider.RotationOutcome) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outcomes[name] = append(p.outcomes[name], outcomes...)
}

// ApplyCredential implements provider.CredentialRotator. Setting the same
// value again is idempotent. api_token is minted by the "provider". Like a
// real adapter authenticating its call, it first reads the active value
// through Env.
func (p *Provider) ApplyCredential(ctx context.Context, env provider.Env, inst provider.Instance, name string, version int, value []byte) (provider.Rotation, error) {
	current, err := env.Credential(ctx, name)
	if err != nil && !errors.Is(err, provider.ErrNotFound) {
		return provider.Rotation{Outcome: provider.RotationUnknown}, err
	}
	clear(current)
	p.wait("rotate:" + name)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fmt.Sprintf("rotate:%s:%d", name, version))
	if err := p.fault("rotate:" + name); err != nil {
		return provider.Rotation{}, err
	}
	o := p.org(inst)
	if out := p.outcomes[name]; len(out) > 0 {
		p.outcomes[name] = out[1:]
		if out[0] != provider.RotationAcknowledged {
			return provider.Rotation{Outcome: out[0]}, nil
		}
	}
	minted := value == nil
	if minted {
		value = []byte(fmt.Sprintf("minted-%d", version))
	}
	o.secrets[name] = slices.Clone(value)
	if p.lose[name] > 0 {
		p.lose[name]--
		return provider.Rotation{Outcome: provider.RotationUnknown}, nil
	}
	r := provider.Rotation{Outcome: provider.RotationAcknowledged}
	if minted {
		r.Value = value
	}
	return r, nil
}

// Inputs implements provider.Lifecycle (managed variant): the settings of
// SetPackageSettings and the database password.
func (p *Provider) Inputs(context.Context, provider.Env, provider.Instance) (provider.Inputs, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fault("inputs"); err != nil {
		return provider.Inputs{}, err
	}
	settings := maps.Clone(p.packageSettings)
	if settings == nil {
		settings = map[string]any{}
	}
	return provider.Inputs{Settings: settings, Secrets: []string{"db_password"}}, nil
}

// Setup implements provider.Lifecycle (managed variant).
func (p *Provider) Setup(_ context.Context, _ provider.Env, inst provider.Instance) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fault("setup"); err != nil {
		return err
	}
	p.org(inst).setups++
	return nil
}

// CheckHealth implements provider.Lifecycle (managed variant).
func (p *Provider) CheckHealth(context.Context, provider.Env, provider.Instance) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fault("health")
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// OpenURL implements provider.DeepLinker: the target under the instance's
// base URL.
func (p *Provider) OpenURL(inst provider.Instance, target string) (*url.URL, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if inst.BaseURL == nil {
		return nil, errors.New("testprovider: no base URL")
	}
	origin := inst.BaseURL
	if p.openOrigin != nil {
		origin = p.openOrigin
	}
	return url.Parse(origin.Scheme + "://" + origin.Host + target)
}

// LinkToOrigin makes OpenURL link to another origin, as a broken adapter
// would.
func (p *Provider) LinkToOrigin(origin *url.URL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.openOrigin = origin
}

var (
	_ provider.DeepLinker        = (*Provider)(nil)
	_ provider.UserProvisioner   = (*Provider)(nil)
	_ provider.EventSource       = (*Provider)(nil)
	_ provider.CredentialRotator = (*Provider)(nil)
	_ provider.Lifecycle         = (*Provider)(nil)
)
