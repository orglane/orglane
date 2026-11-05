// Package apps is the provider framework in the core
// (docs/specs/providers.md): the registry of compiled adapters, app
// instances and their bindings, sealed credentials, team mappings, desired
// access and provisioning with the removal guard, webhook ingestion and
// normalized events.
//
// Every mutation goes through Service and writes an audit entry in the same
// transaction. Jobs run on the River queue "apps" (QueueName); their
// arguments carry IDs only, never secrets.
package apps

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// Instance states (docs/specs/providers.md "app_instances").
const (
	StateConnecting   = "connecting"
	StateInstalling   = "installing"
	StateActive       = "active"
	StateDegraded     = "degraded"
	StateUpgrading    = "upgrading"
	StateFailed       = "failed"
	StateDisconnected = "disconnected"
	StateRemoving     = "removing"
	StateRemoved      = "removed"
)

// Health values.
const (
	HealthUnknown   = "unknown"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// CodeSetupFailed is the health code of a managed instance whose
// Lifecycle.Setup exhausted its retries.
const CodeSetupFailed = "setup_failed"

// provisionable are the states in which provisioning runs.
var provisionable = []string{StateActive, StateDegraded}

// transitions lists the allowed state changes.
var transitions = map[string][]string{
	StateConnecting:   {StateActive, StateRemoved, StateFailed},
	StateInstalling:   {StateActive, StateFailed},
	StateActive:       {StateDegraded, StateUpgrading, StateDisconnected, StateRemoving},
	StateDegraded:     {StateActive, StateUpgrading, StateDisconnected, StateRemoving},
	StateUpgrading:    {StateActive, StateDegraded, StateFailed},
	StateFailed:       {StateInstalling, StateRemoving},
	StateDisconnected: {StateActive, StateRemoving, StateRemoved},
	StateRemoving:     {StateRemoved},
	StateRemoved:      {StateInstalling},
}

// Queue inserts jobs; *river.Client is one.
type Queue interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Config configures a Service.
type Config struct {
	// WorkspaceURL is the workspace origin, e.g. https://work.acme.com.
	WorkspaceURL *url.URL
	// IssuerURL is the OpenID Provider's issuer.
	IssuerURL *url.URL
	// HTTPClient is handed to adapters. Defaults to NewHTTPClient().
	HTTPClient *http.Client
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger

	// Agent is the core's client of the agent; nil when managed services
	// are disabled. Pushes then wait for the agent.
	Agent AgentClient
	// OIDC registers the OIDC clients of relying-party instances
	// (provider.OIDCRelyingParty); *identity.Service is one. Nil: such
	// providers cannot be installed.
	OIDC OIDCClients
	// Packages are the embedded managed packages: the routes, hosts and
	// upstreams of managed instances come from them, and without one a
	// managed provider cannot be installed.
	Packages *packages.Registry
	// IngressMode is config.IngressDomain or config.IngressLocal (the
	// default).
	IngressMode string
	// ServiceHost returns the public host of a service label (ADR 0018,
	// config.ServiceHost). Defaults to "<label>.<workspace host>".
	ServiceHost func(label string) string
	// SyncInterval is how often orglane.apps.agent_sync runs. Defaults to
	// 5m (config.Agent.SyncInterval).
	SyncInterval time.Duration
	// AgentPollInterval is how long agent_apply snoozes between polls of an
	// operation. Defaults to 2s.
	AgentPollInterval time.Duration
	// AgentRetryMax caps the backoff while a push waits for the agent.
	// Defaults to a minute.
	AgentRetryMax time.Duration
	// AgentWatchInterval is how often WatchAgent asks the agent for its boot
	// ID. Defaults to 30s.
	AgentWatchInterval time.Duration
	// InstallPreflight, if set, checks an install before anything is
	// recorded (#61: host, DNS, agent health, resource hints); its error is
	// returned by Install.
	InstallPreflight func(ctx context.Context, plan InstallPlan) error
}

// OIDCClients registers OIDC clients in the caller's transaction.
type OIDCClients interface {
	CreateOIDCClientTx(ctx context.Context, q *store.Queries, actor audit.Actor, in identity.NewOIDCClient) (store.OidcClient, string, error)
}

// Service changes and reads everything the provider framework owns.
type Service struct {
	pool       *pgxpool.Pool
	reg        *Registry
	creds      *crypto.Sealer
	deliveries *crypto.Sealer
	oauth      *crypto.Sealer
	cfg        Config

	mu    sync.RWMutex
	queue Queue

	badSignatures rateLimitedLog

	// bootID is the agent boot ID seen last (uuid.UUID), for WatchAgent.
	bootID atomic.Value
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool, reg *Registry, keyring *crypto.Keyring, cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = NewHTTPClient()
	}
	if cfg.WorkspaceURL == nil {
		cfg.WorkspaceURL = &url.URL{Scheme: "https", Host: "localhost"}
	}
	if cfg.IssuerURL == nil {
		cfg.IssuerURL = cfg.WorkspaceURL
	}
	if cfg.IngressMode == "" {
		cfg.IngressMode = agentapi.IngressLocal
	}
	if cfg.ServiceHost == nil {
		host := cfg.WorkspaceURL.Hostname()
		cfg.ServiceHost = func(label string) string { return label + "." + host }
	}
	cfg.SyncInterval = cmp.Or(cfg.SyncInterval, defaultSyncInterval)
	cfg.AgentPollInterval = cmp.Or(cfg.AgentPollInterval, defaultAgentPollInterval)
	cfg.AgentRetryMax = cmp.Or(cfg.AgentRetryMax, defaultAgentRetryMax)
	cfg.AgentWatchInterval = cmp.Or(cfg.AgentWatchInterval, defaultAgentWatchInterval)
	return &Service{
		pool:          pool,
		reg:           reg,
		creds:         keyring.Sealer(crypto.PurposeAppCredentials),
		deliveries:    keyring.Sealer(crypto.PurposeWebhookDeliveries),
		oauth:         keyring.Sealer(crypto.PurposeAppOAuthState),
		cfg:           cfg,
		badSignatures: rateLimitedLog{every: time.Minute},
	}
}

// SetQueue attaches the job queue. Changes that queue jobs fail with
// ErrNoQueue until it is set.
func (s *Service) SetQueue(q Queue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = q
}

func (s *Service) getQueue() (Queue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.queue == nil {
		return nil, ErrNoQueue
	}
	return s.queue, nil
}

// Registry returns the adapters this service knows.
func (s *Service) Registry() *Registry { return s.reg }

// WebhookURL returns the URL providers deliver webhooks of an instance to.
func (s *Service) WebhookURL(instanceID uuid.UUID) *url.URL {
	u := *s.cfg.WorkspaceURL
	u.Path = "/api/v1/webhooks/" + instanceID.String()
	u.RawQuery, u.Fragment = "", ""
	return &u
}

func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx, q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(tx, store.New(tx))
	})
}

// Instance is an app instance with its decoded settings and adapter.
type Instance struct {
	store.AppInstance
	Settings map[string]any
	// Adapter is nil when the provider is not compiled into this binary.
	Adapter *Adapter
	// WaitingForAgent is set (Instance and Instances only) while a change
	// of a managed instance waits for the agent: agent_unavailable,
	// agent_version_mismatch or agent_untrusted.
	WaitingForAgent string
	// upstream is the direct URL of a managed instance on the edge network
	// (provider.Instance.Upstream); nil when its package is not embedded.
	upstream *url.URL
}

// View returns the read-only view adapters get.
func (i Instance) View() provider.Instance {
	v := provider.Instance{
		ID:         i.ID,
		Key:        i.Key,
		Provider:   provider.ID(i.Provider),
		Management: provider.Management(i.Management),
		Settings:   i.Settings,
		Upstream:   i.upstream,
	}
	if i.BaseUrl != nil {
		if u, err := url.Parse(*i.BaseUrl); err == nil {
			v.BaseURL = u
		}
	}
	return v
}

// Provisionable reports whether provisioning runs for the instance.
func (i Instance) Provisionable() bool { return slices.Contains(provisionable, i.State) }

func (s *Service) instance(row store.AppInstance) (Instance, error) {
	settings, err := decodeSettings(row.Settings)
	if err != nil {
		return Instance{}, err
	}
	inst := Instance{AppInstance: row, Settings: settings}
	if a, ok := s.reg.Adapter(provider.ID(row.Provider)); ok {
		inst.Adapter = a
		canonicalSettings(a.Manifest.Settings, settings)
	}
	inst.upstream = s.Upstream(row)
	return inst, nil
}

func (s *Service) loadInstance(ctx context.Context, q *store.Queries, id uuid.UUID, forUpdate bool) (Instance, error) {
	var row store.AppInstance
	var err error
	if forUpdate {
		row, err = q.GetAppInstanceForUpdate(ctx, id)
	} else {
		row, err = q.GetAppInstance(ctx, id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Instance{}, fmt.Errorf("%w: app instance", ErrNotFound)
	} else if err != nil {
		return Instance{}, fmt.Errorf("apps: load instance: %w", err)
	}
	return s.instance(row)
}

// Instance returns an app instance.
func (s *Service) Instance(ctx context.Context, id uuid.UUID) (Instance, error) {
	inst, err := s.loadInstance(ctx, store.New(s.pool), id, false)
	if err != nil {
		return Instance{}, err
	}
	waiting, err := s.waitingForAgent(ctx)
	if err != nil {
		return Instance{}, err
	}
	inst.WaitingForAgent = waiting[inst.ID]
	return inst, nil
}

// waitingForAgent maps instances with an operation that waits for the
// agent to the reason.
func (s *Service) waitingForAgent(ctx context.Context) (map[uuid.UUID]string, error) {
	rows, err := store.New(s.pool).ListAppInstancesWaitingForAgent(ctx, waitingCodes)
	if err != nil {
		return nil, fmt.Errorf("apps: list operations waiting for the agent: %w", err)
	}
	out := make(map[uuid.UUID]string, len(rows))
	for _, r := range rows {
		out[r.InstanceID] = r.ErrorCode
	}
	return out, nil
}

// Instances returns every app instance, removed ones included, oldest
// first.
func (s *Service) Instances(ctx context.Context) ([]Instance, error) {
	rows, err := store.New(s.pool).ListAppInstances(ctx)
	if err != nil {
		return nil, fmt.Errorf("apps: list instances: %w", err)
	}
	waiting, err := s.waitingForAgent(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(rows))
	for _, row := range rows {
		inst, err := s.instance(row)
		if err != nil {
			return nil, err
		}
		inst.WaitingForAgent = waiting[inst.ID]
		out = append(out, inst)
	}
	return out, nil
}

// NewInstance describes an app instance to create. Installing managed
// providers (#61) and connecting external ones (#64) build on it.
type NewInstance struct {
	Provider    provider.ID
	Key         string // defaults to the manifest's DefaultKey
	DisplayName string // defaults to the manifest's name
	BaseURL     string // optional until known
	Settings    map[string]any
	// State defaults to connecting (connected) or installing (managed).
	State string
}

// CreateInstance creates an app instance and its generated credentials.
// Installing a managed provider through the agent is Install.
func (s *Service) CreateInstance(ctx context.Context, actor audit.Actor, in NewInstance) (Instance, error) {
	return s.createInstance(ctx, actor, in, nil)
}

// createInstance creates an instance; then, if set, runs in the same
// transaction after the instance and its credentials exist.
func (s *Service) createInstance(ctx context.Context, actor audit.Actor, in NewInstance, then func(tx pgx.Tx, q *store.Queries, created Instance) error) (Instance, error) {
	adapter, ok := s.reg.Adapter(in.Provider)
	if !ok {
		return Instance{}, fmt.Errorf("%w: %s", ErrProviderUnavailable, in.Provider)
	}
	m := adapter.Manifest
	key := strings.TrimSpace(in.Key)
	if key == "" {
		key = m.DefaultKey
	}
	if !keyPattern.MatchString(key) {
		return Instance{}, invalid("key", "must be 2-24 characters of a-z, 0-9 and inner hyphens, starting with a letter")
	}
	name := strings.TrimSpace(in.DisplayName)
	if name == "" {
		name = m.Name
	}
	if err := validDisplayName(name); err != nil {
		return Instance{}, err
	}
	var baseURL *string
	if in.BaseURL != "" {
		u, err := parseBaseURL(in.BaseURL)
		if err != nil {
			return Instance{}, err
		}
		baseURL = &u
	}
	settings, err := mergeSettings(m.Settings, nil, in.Settings, true)
	if err != nil {
		return Instance{}, err
	}
	if err := adapter.validateSettings(settings); err != nil {
		return Instance{}, err
	}
	rawSettings, err := json.Marshal(settings)
	if err != nil {
		return Instance{}, fmt.Errorf("apps: encode settings: %w", err)
	}
	state := in.State
	if state == "" {
		state = StateConnecting
		if m.Management == provider.Managed {
			state = StateInstalling
		}
	}
	if _, ok := transitions[state]; !ok {
		return Instance{}, invalid("state", "unknown state %q", state)
	}
	var pkg *string
	if m.Management == provider.Managed {
		pkg = &m.Package
	}
	var created Instance
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		row, err := q.InsertAppInstance(ctx, store.InsertAppInstanceParams{
			Key:          key,
			Provider:     string(m.ID),
			Management:   string(m.Management),
			DisplayName:  name,
			State:        state,
			BaseUrl:      baseURL,
			Settings:     rawSettings,
			AccessPolicy: string(m.DefaultPolicy),
			PackageID:    pkg,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "app_instances_key_key" {
				return ErrKeyReserved
			}
			return fmt.Errorf("apps: create instance: %w", err)
		}
		if created, err = s.instance(row); err != nil {
			return err
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AppInstanceCreated,
			Target:   instanceTarget(row.ID),
			Metadata: map[string]any{"provider": row.Provider, "key": row.Key, "management": row.Management, "state": row.State},
		}); err != nil {
			return err
		}
		for _, c := range m.Credentials {
			if c.Source != provider.SourceGenerated {
				continue
			}
			if _, err := s.storeCredential(ctx, q, actor, created, c.Name, []byte(crypto.RandomToken("")), credentialActive); err != nil {
				return err
			}
		}
		// Before the desired spec (then): its credential versions include
		// the client secret.
		if err := s.registerOIDCClient(ctx, q, actor, created); err != nil {
			return err
		}
		if then != nil {
			if err := then(tx, q, created); err != nil {
				return err
			}
		}
		if created.Provisionable() {
			return s.activated(ctx, tx, q, created)
		}
		return nil
	})
	if err != nil {
		return Instance{}, err
	}
	return created, nil
}

// InstanceUpdate changes an app instance. Nil fields stay unchanged.
type InstanceUpdate struct {
	DisplayName           *string
	AccessPolicy          *provider.AccessPolicy
	ManageExistingMembers *bool
	// Settings are merged into the current settings; a nil value removes a
	// setting. Immutable settings cannot change.
	Settings map[string]any
}

// UpdateInstance changes the display name, access policy, whether existing
// members are managed, and provider settings. Changes that affect access
// queue provisioning for the instance.
func (s *Service) UpdateInstance(ctx context.Context, actor audit.Actor, id uuid.UUID, in InstanceUpdate) (Instance, error) {
	var updated Instance
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		current, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if current.State == StateRemoved || current.State == StateRemoving {
			return ErrInvalidState
		}
		params := store.UpdateAppInstanceConfigParams{
			ID:                    id,
			DisplayName:           current.DisplayName,
			AccessPolicy:          current.AccessPolicy,
			ManageExistingMembers: current.ManageExistingMembers,
			Settings:              current.AppInstance.Settings,
		}
		var changed []string
		if in.DisplayName != nil {
			name := strings.TrimSpace(*in.DisplayName)
			if err := validDisplayName(name); err != nil {
				return err
			}
			if name != current.DisplayName {
				params.DisplayName = name
				changed = append(changed, "display_name")
			}
		}
		if in.AccessPolicy != nil {
			p := *in.AccessPolicy
			if p != provider.AllPeople && p != provider.MappedTeams {
				return invalid("access_policy", "must be %s or %s", provider.AllPeople, provider.MappedTeams)
			}
			if string(p) != current.AccessPolicy {
				params.AccessPolicy = string(p)
				changed = append(changed, "access_policy")
			}
		}
		if in.ManageExistingMembers != nil && *in.ManageExistingMembers != current.ManageExistingMembers {
			params.ManageExistingMembers = *in.ManageExistingMembers
			changed = append(changed, "manage_existing_members")
		}
		var settingNames []string
		if len(in.Settings) > 0 {
			if current.Adapter == nil {
				return ErrProviderUnavailable
			}
			merged, err := mergeSettings(current.Adapter.Manifest.Settings, current.Settings, in.Settings, false)
			if err != nil {
				return err
			}
			if err := current.Adapter.validateSettings(merged); err != nil {
				return err
			}
			raw, err := json.Marshal(merged)
			if err != nil {
				return fmt.Errorf("apps: encode settings: %w", err)
			}
			for name := range in.Settings {
				settingNames = append(settingNames, name)
			}
			slices.Sort(settingNames)
			if !jsonEqual(raw, current.AppInstance.Settings) {
				params.Settings = raw
				changed = append(changed, "settings")
			}
		}
		if len(changed) == 0 {
			updated = current
			return nil
		}
		row, err := q.UpdateAppInstanceConfig(ctx, params)
		if err != nil {
			return fmt.Errorf("apps: update instance: %w", err)
		}
		if updated, err = s.instance(row); err != nil {
			return err
		}
		meta := map[string]any{"provider": row.Provider, "fields": changed}
		if len(settingNames) > 0 {
			meta["settings"] = settingNames
		}
		if _, err := audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.AppInstanceUpdated, Target: instanceTarget(id), Metadata: meta}); err != nil {
			return err
		}
		if updated.Provisionable() && (slices.Contains(changed, "access_policy") || slices.Contains(changed, "manage_existing_members") || slices.Contains(changed, "settings")) {
			return s.enqueueInstance(ctx, tx, id, ReasonConfig)
		}
		return nil
	})
	if err != nil {
		return Instance{}, err
	}
	return updated, nil
}

// Transition moves an app instance to another state, with a stable reason
// code for degraded and failed states. Becoming active creates the
// instance's capability bindings and queues provisioning in the same
// transaction (docs/specs/providers.md "Managed instance lifecycle").
func (s *Service) Transition(ctx context.Context, actor audit.Actor, id uuid.UUID, to, code string) (Instance, error) {
	var out Instance
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		var err error
		out, err = s.transition(ctx, tx, q, actor, id, to, code)
		return err
	})
	return out, err
}

func (s *Service) transition(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, id uuid.UUID, to, code string) (Instance, error) {
	current, err := s.loadInstance(ctx, q, id, true)
	if err != nil {
		return Instance{}, err
	}
	if current.State == to && derefString(current.HealthCode) == code {
		return current, nil
	}
	if current.State != to && !slices.Contains(transitions[current.State], to) {
		return Instance{}, fmt.Errorf("%w: %s -> %s", ErrInvalidState, current.State, to)
	}
	health := HealthUnknown
	switch to {
	case StateActive:
		health = HealthHealthy
	case StateDegraded, StateFailed, StateDisconnected:
		health = HealthUnhealthy
	}
	var healthCode *string
	if code != "" {
		healthCode = &code
	}
	row, err := q.SetAppInstanceState(ctx, store.SetAppInstanceStateParams{ID: id, State: to, Health: health, HealthCode: healthCode})
	if err != nil {
		return Instance{}, fmt.Errorf("apps: set instance state: %w", err)
	}
	next, err := s.instance(row)
	if err != nil {
		return Instance{}, err
	}
	meta := map[string]any{"provider": row.Provider, "from": current.State, "to": to}
	if code != "" {
		meta["code"] = code
	}
	if _, err := audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.AppInstanceStateChanged, Target: instanceTarget(id), Metadata: meta}); err != nil {
		return Instance{}, err
	}
	if next.Provisionable() && !current.Provisionable() {
		if err := s.activated(ctx, tx, q, next); err != nil {
			return Instance{}, err
		}
	}
	return next, nil
}

// activated creates the bindings of a newly active instance (the first per
// capability becomes the default) and queues provisioning.
func (s *Service) activated(ctx context.Context, tx pgx.Tx, q *store.Queries, inst Instance) error {
	if inst.Adapter != nil {
		for _, c := range inst.Adapter.Manifest.Capabilities {
			if err := q.AcquireXactLock(ctx, lockID("binding", string(c))); err != nil {
				return fmt.Errorf("apps: lock binding: %w", err)
			}
			if _, err := q.InsertAppBinding(ctx, store.InsertAppBindingParams{Capability: string(c), InstanceID: inst.ID}); err != nil {
				return fmt.Errorf("apps: create binding: %w", err)
			}
		}
		if _, ok := inst.Adapter.Provisioner(); !ok {
			return nil
		}
	}
	// Removals an earlier run left (confirmed batches interrupted by an
	// inactive instance) resume.
	if err := s.enqueueRemovals(ctx, tx, inst.ID, 0); err != nil {
		return err
	}
	return s.enqueueInstance(ctx, tx, inst.ID, ReasonActivated)
}

// RetrySetup retries Lifecycle.Setup of a managed instance whose setup
// failed: the instance goes back to installing and a setup job is queued in
// the same transaction (enqueueSetup: the failed attempt's job, which River
// may still show running after RunSetup committed setup_failed, never takes
// its place).
func (s *Service) RetrySetup(ctx context.Context, actor audit.Actor, id uuid.UUID) (Instance, error) {
	var out Instance
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		current, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if current.Adapter == nil {
			return ErrProviderUnavailable
		}
		if _, ok := current.Adapter.Lifecycle(); !ok {
			return ErrNotSupported
		}
		if current.State != StateFailed || derefString(current.HealthCode) != CodeSetupFailed {
			return fmt.Errorf("%w: only instances whose setup failed can retry it", ErrInvalidState)
		}
		if out, err = s.transition(ctx, tx, q, actor, id, StateInstalling, ""); err != nil {
			return err
		}
		return s.enqueueSetup(ctx, tx, id)
	})
	return out, err
}

// Binding is a capability binding with its instance.
type Binding = store.ListAppBindingsRow

// Bindings returns every capability binding.
func (s *Service) Bindings(ctx context.Context) ([]Binding, error) {
	rows, err := store.New(s.pool).ListAppBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("apps: list bindings: %w", err)
	}
	return rows, nil
}

// InstanceBindings returns the bindings of an instance.
func (s *Service) InstanceBindings(ctx context.Context, id uuid.UUID) ([]store.AppBinding, error) {
	rows, err := store.New(s.pool).ListAppInstanceBindings(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("apps: list bindings: %w", err)
	}
	return rows, nil
}

// SetDefaultBinding makes an instance the default for a capability. The
// instance must already be bound to it.
func (s *Service) SetDefaultBinding(ctx context.Context, actor audit.Actor, capability string, instanceID uuid.UUID) error {
	if !capabilityPattern.MatchString(capability) {
		return invalid("capability", "unknown capability")
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if err := q.AcquireXactLock(ctx, lockID("binding", capability)); err != nil {
			return fmt.Errorf("apps: lock binding: %w", err)
		}
		inst, err := s.loadInstance(ctx, q, instanceID, false)
		if err != nil {
			return err
		}
		if err := q.ClearDefaultAppBinding(ctx, capability); err != nil {
			return fmt.Errorf("apps: clear default binding: %w", err)
		}
		n, err := q.SetDefaultAppBinding(ctx, store.SetDefaultAppBindingParams{Capability: capability, InstanceID: instanceID})
		if err != nil {
			return fmt.Errorf("apps: set default binding: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: the instance is not bound to %s", ErrNotFound, capability)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AppBindingSet,
			Target:   instanceTarget(instanceID),
			Metadata: map[string]any{"provider": inst.Provider, "capability": capability, "default": true},
		})
		return err
	})
}

// TeamMapping is a team mapping with its Orglane team.
type TeamMapping = store.ListAppTeamMappingsRow

// TeamMappings returns the team mappings of an instance.
func (s *Service) TeamMappings(ctx context.Context, instanceID uuid.UUID) ([]TeamMapping, error) {
	q := store.New(s.pool)
	if _, err := s.loadInstance(ctx, q, instanceID, false); err != nil {
		return nil, err
	}
	rows, err := q.ListAppTeamMappings(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("apps: list team mappings: %w", err)
	}
	return rows, nil
}

// TeamMappingInput creates or changes a team mapping.
type TeamMappingInput struct {
	Mode string // provider.MappingCreate | provider.MappingExisting
	// ExternalRef and ExternalName pick the provider team (mode existing).
	ExternalRef  string
	ExternalName string
}

// SetTeamMapping creates or changes the mapping of an Orglane team to a
// provider team and queues provisioning for the instance.
func (s *Service) SetTeamMapping(ctx context.Context, actor audit.Actor, instanceID, teamID uuid.UUID, in TeamMappingInput) (store.AppTeamMapping, error) {
	var out store.AppTeamMapping
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, instanceID, true)
		if err != nil {
			return err
		}
		if err := mappable(inst); err != nil {
			return err
		}
		team, err := q.GetTeamForUpdate(ctx, teamID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: team", ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("apps: load team: %w", err)
		}
		if team.ArchivedAt != nil {
			return invalid("teamId", "archived teams cannot be mapped")
		}
		params := store.UpsertAppTeamMappingParams{InstanceID: instanceID, TeamID: teamID, Mode: in.Mode}
		switch in.Mode {
		case provider.MappingCreate:
			if in.ExternalRef != "" {
				return invalid("externalRef", "is only set for existing provider teams")
			}
			// Keep the provider team an earlier sync created.
			if cur, err := q.GetAppTeamMapping(ctx, store.GetAppTeamMappingParams{InstanceID: instanceID, TeamID: teamID}); err == nil && cur.Mode == provider.MappingCreate {
				params.ExternalRef, params.ExternalName = cur.ExternalRef, cur.ExternalName
			} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("apps: load team mapping: %w", err)
			}
		case provider.MappingExisting:
			ref := strings.TrimSpace(in.ExternalRef)
			if ref == "" || len(ref) > 100 || !utf8.ValidString(ref) {
				return invalid("externalRef", "names the provider team (1-100 characters)")
			}
			params.ExternalRef = &ref
			if name := strings.TrimSpace(in.ExternalName); name != "" {
				if len(name) > 100 || !utf8.ValidString(name) {
					return invalid("externalName", "must be at most 100 characters")
				}
				params.ExternalName = &name
			}
		default:
			return invalid("mode", "must be %s or %s", provider.MappingCreate, provider.MappingExisting)
		}
		if inst.Adapter != nil {
			if err := inst.Adapter.validateTeamMapping(inst.View(), in.Mode, strings.TrimSpace(in.ExternalRef)); err != nil {
				return err
			}
		}
		if out, err = q.UpsertAppTeamMapping(ctx, params); err != nil {
			return fmt.Errorf("apps: set team mapping: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AppTeamMappingSet,
			Target:   instanceTarget(instanceID),
			Metadata: map[string]any{"provider": inst.Provider, "team_id": teamID, "mode": in.Mode},
		}); err != nil {
			return err
		}
		if inst.Provisionable() {
			return s.enqueueInstance(ctx, tx, instanceID, ReasonMapping)
		}
		return nil
	})
	return out, err
}

// RemoveTeamMapping removes a team mapping. Members of the team lose what
// the mapping granted, through the removal guard.
func (s *Service) RemoveTeamMapping(ctx context.Context, actor audit.Actor, instanceID, teamID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, instanceID, true)
		if err != nil {
			return err
		}
		n, err := q.DeleteAppTeamMapping(ctx, store.DeleteAppTeamMappingParams{InstanceID: instanceID, TeamID: teamID})
		if err != nil {
			return fmt.Errorf("apps: remove team mapping: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: team mapping", ErrNotFound)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AppTeamMappingRemoved,
			Target:   instanceTarget(instanceID),
			Metadata: map[string]any{"provider": inst.Provider, "team_id": teamID},
		}); err != nil {
			return err
		}
		if inst.Provisionable() {
			return s.enqueueInstance(ctx, tx, instanceID, ReasonMapping)
		}
		return nil
	})
}

func mappable(inst Instance) error {
	if inst.State == StateRemoved || inst.State == StateRemoving {
		return ErrInvalidState
	}
	if inst.Adapter == nil {
		return ErrProviderUnavailable
	}
	if _, ok := inst.Adapter.Provisioner(); !ok {
		return ErrNotSupported
	}
	return nil
}

func validDisplayName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > 100 || !utf8.ValidString(name) {
		return invalid("displayName", "must be 1-100 characters")
	}
	return nil
}

func parseBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", invalid("baseUrl", "must be an http or https URL without credentials, query or fragment")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func instanceTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetAppInstance, ID: id.String()}
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// lockID derives a transaction advisory lock ID from its parts.
func lockID(parts ...string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("orglane.apps"))
	for _, p := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(p))
	}
	return int64(h.Sum64()) //nolint:gosec // wrapping is fine for a lock ID
}
