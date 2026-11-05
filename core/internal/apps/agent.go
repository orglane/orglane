package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// This file is the core side of managed instances (docs/specs/agent.md
// "Desired state and reconciliation", providers.md "Managed instance
// lifecycle in the core"): the install flow, desired spec generations and
// their app_operations rows. agent_apply.go pushes them, agent_sync.go reads
// back what the agent runs.
//
// Invariants:
//
//   - app_instances.desired_generation is the highest app_desired_specs
//     generation of the instance; a generation is recorded only with the
//     instance row locked (recordDesired), together with its app_operations
//     row (state queued) and its agent_apply job, in one transaction.
//   - After that, only the agent_apply job of (instance, generation) writes
//     the operation row (River runs at most one such job at a time). Every
//     write is conditional on finished_at IS NULL, so an operation finishes
//     once and its consequences (setup, install_failed, ...) are applied
//     once, in the transaction that finishes it.
//   - The agent applies only the newest accepted generation. A generation
//     replaced before it was pushed ends failed with code superseded; one
//     that was pushed is mirrored until the agent reports it terminal.

// AgentClient is what the core needs from the agent (agentclient.Client).
// Part 2 of #75 (upgrades) adds Confirm and Abort.
type AgentClient interface {
	Health(ctx context.Context) (*agentapi.Health, error)
	Instances(ctx context.Context) ([]agentapi.InstanceStatus, error)
	Apply(ctx context.Context, key string, spec *agentapi.Spec) (*agentapi.Operation, bool, error)
	Operation(ctx context.Context, id uuid.UUID) (*agentapi.Operation, error)
}

var _ AgentClient = (*agentclient.Client)(nil)

// Reasons of desired spec generations (app_desired_specs.reason).
const (
	ReasonInstall   = "install"
	ReasonConfigure = "configure"
	ReasonUpgrade   = "upgrade"
	ReasonRetry     = "retry"
	ReasonRevert    = "revert"
	ReasonRemove    = "remove"
	ReasonReinstall = "reinstall"
	ReasonResync    = "resync"
)

// Codes of managed instances and operations.
const (
	// CodeInstallFailed: the install generation's operation failed.
	CodeInstallFailed = "install_failed"
	// Waiting for the agent (unfinished operations, AppInstance.waitingForAgent).
	CodeAgentUnavailable     = "agent_unavailable"
	CodeAgentVersionMismatch = "agent_version_mismatch"
	CodeAgentUntrusted       = "agent_untrusted"
	// Core-side ends of an operation before the agent ran it.
	CodePackageUnavailable = "package_unavailable"
	CodeCredentialMissing  = "credential_missing" //nolint:gosec // an error code, not a credential
	// CodeAgentStalled: the agent kept an operation queued for longer than
	// Config.AgentQueueTimeout.
	CodeAgentStalled = "agent_stalled"
	// Health codes the sync owns (and may clear again).
	CodeAgentUnhealthy       = "agent_unhealthy"
	CodeAgentInstanceMissing = "agent_instance_missing"
	CodeHealthCheckFailed    = "health_check_failed"
)

// waitingCodes are the codes of operations that wait for the agent.
var waitingCodes = []string{CodeAgentUnavailable, CodeAgentVersionMismatch, CodeAgentUntrusted}

// Defaults of Config.
const (
	defaultSyncInterval       = 5 * time.Minute
	defaultAgentPollInterval  = 2 * time.Second
	defaultAgentRetryMax      = time.Minute
	defaultAgentWatchInterval = 30 * time.Second
	defaultAgentQueueTimeout  = time.Hour
)

// DesiredChange is a new desired state of a managed instance.
type DesiredChange struct {
	RunState string // agentapi.StateRunning | StateStopped | StateAbsent
	// Purge, with StateAbsent, deletes the data, the database and the role.
	Purge bool
	// PackageVersion is the package version to run; empty keeps the current
	// desired generation's version (configure, remove, reinstall).
	PackageVersion string
	Reason         string // ReasonInstall, ReasonConfigure, ...
}

// InstallPlan is what Install is about to create, for Config.InstallPreflight.
type InstallPlan struct {
	Provider provider.ID
	Key      string
	// Host is the first route's public host, BaseURL the instance's URL.
	Host    string
	BaseURL string
	Package *packages.Package
}

// RecordDesired records the next desired generation of a managed instance
// in tx (with its operation and agent_apply job), locking the instance row.
// It is the only way desired generations are recorded: the removal and
// reinstall flows (#61) and upgrades (#75 part 2) call it in the
// transaction of their state change.
func (s *Service) RecordDesired(ctx context.Context, tx pgx.Tx, actor audit.Actor, instanceID uuid.UUID, d DesiredChange) (store.AppDesiredSpec, error) {
	q := store.New(tx)
	inst, err := s.loadInstance(ctx, q, instanceID, true)
	if err != nil {
		return store.AppDesiredSpec{}, err
	}
	if d.PackageVersion == "" {
		cur, err := q.GetCurrentAppDesiredSpec(ctx, instanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.AppDesiredSpec{}, fmt.Errorf("%w: %s has no desired state yet", ErrInvalidState, inst.Key)
		} else if err != nil {
			return store.AppDesiredSpec{}, fmt.Errorf("apps: load desired spec: %w", err)
		}
		d.PackageVersion = cur.PackageVersion
	}
	return s.recordDesired(ctx, tx, q, actor, inst, d)
}

// Install creates a managed instance and installs it through the agent
// (providers.md "Managed instance lifecycle in the core", steps 2-5): in one
// transaction the instance (installing), its generated credentials, desired
// spec generation 1 with the newest embedded package version, its operation
// and the agent_apply job. When the operation succeeds, Lifecycle.Setup runs
// and activates the instance; when it fails, the instance fails with
// install_failed. It is provider-agnostic: the install API (#61) brings the
// host, DNS, agent health and resource checks as Config.InstallPreflight.
func (s *Service) Install(ctx context.Context, actor audit.Actor, in NewInstance) (Instance, error) {
	adapter, ok := s.reg.Adapter(in.Provider)
	if !ok {
		return Instance{}, fmt.Errorf("%w: %s", ErrProviderUnavailable, in.Provider)
	}
	if _, ok := adapter.Lifecycle(); !ok {
		return Instance{}, fmt.Errorf("%w: %s is not a managed provider", ErrNotSupported, in.Provider)
	}
	pkg, ok := s.latestPackage(adapter.Manifest.Package)
	if !ok {
		return Instance{}, fmt.Errorf("%w: the package %s is not embedded in this build", ErrNotSupported, adapter.Manifest.Package)
	}
	in.State = StateInstalling
	if in.BaseURL == "" && len(pkg.Manifest.Routes) > 0 {
		in.BaseURL = s.serviceURL(pkg.Manifest.Routes[0].HostLabel)
	}
	if in.BaseURL == "" {
		return Instance{}, invalid("host", "is required")
	}
	// Installation-dependent defaults (provider.SettingDefaulter) fill
	// settings the request leaves out.
	settings := maps.Clone(in.Settings)
	if settings == nil {
		settings = map[string]any{}
	}
	for name, v := range adapter.settingDefaults(s.cfg.DomainBase) {
		if settings[name] == nil {
			settings[name] = v
		}
	}
	in.Settings = settings
	host, err := s.validHost(in.BaseURL)
	if err != nil {
		return Instance{}, err
	}
	var budget InstallBudget
	if s.cfg.InstallPreflight != nil {
		key := in.Key
		if key == "" {
			key = adapter.Manifest.DefaultKey
		}
		if budget, err = s.cfg.InstallPreflight(ctx, InstallPlan{Provider: in.Provider, Key: key, Host: host, BaseURL: in.BaseURL, Package: pkg}); err != nil {
			return Instance{}, err
		}
	}
	created, err := s.createInstance(ctx, actor, in, func(tx pgx.Tx, q *store.Queries, created Instance) error {
		if err := s.reserve(ctx, q, host, created.ID, budget, pkg); err != nil {
			return err
		}
		_, err := s.recordDesired(ctx, tx, q, actor, created, DesiredChange{
			RunState: agentapi.StateRunning, PackageVersion: pkg.Manifest.Version, Reason: ReasonInstall,
		})
		return err
	})
	if err != nil {
		return Instance{}, err
	}
	return s.Instance(ctx, created.ID)
}

// recordDesired records the next desired spec generation of a managed
// instance, its queued operation and its agent_apply job. inst must be the
// instance row as locked by the caller's transaction (or created in it).
// Lifecycle.Inputs runs here, so a generation's settings and credential
// versions are fixed and every push of it carries the same spec. Inputs must
// not read credential values: they may not be committed yet.
//
// Part 2 of #75 records upgrade, retry and revert generations with it; #61
// configure, remove and reinstall generations.
func (s *Service) recordDesired(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, inst Instance, d DesiredChange) (store.AppDesiredSpec, error) {
	if inst.Adapter == nil {
		return store.AppDesiredSpec{}, ErrProviderUnavailable
	}
	lc, ok := inst.Adapter.Lifecycle()
	if !ok || inst.PackageID == nil {
		return store.AppDesiredSpec{}, ErrNotSupported
	}
	inputs, err := lc.Inputs(ctx, s.env(inst), inst.View())
	if err != nil {
		code, _, _ := provider.Classify(err)
		return store.AppDesiredSpec{}, fmt.Errorf("apps: package inputs of %s (%s): %w", inst.Key, code, err)
	}
	versions := map[string]int32{}
	for _, name := range inputs.Secrets {
		if c, ok := inst.Adapter.Credential(name); !ok || !c.Agent {
			return store.AppDesiredSpec{}, fmt.Errorf("apps: %s is not an agent credential of %s", name, inst.Provider)
		}
		row, err := q.GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: inst.ID, Name: name})
		if err != nil {
			return store.AppDesiredSpec{}, fmt.Errorf("apps: credential %s of %s: %w", name, inst.Key, err)
		}
		versions[name] = row.Version
	}
	settings := inputs.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	rawSettings, err := json.Marshal(settings)
	if err != nil {
		return store.AppDesiredSpec{}, fmt.Errorf("apps: encode package settings: %w", err)
	}
	rawVersions, _ := json.Marshal(versions) // a map of ints: cannot fail
	gen := inst.DesiredGeneration + 1
	ds, err := q.InsertAppDesiredSpec(ctx, store.InsertAppDesiredSpecParams{
		InstanceID: inst.ID, Generation: gen, PackageVersion: d.PackageVersion, RunState: d.RunState,
		Purge: d.Purge, Settings: rawSettings, CredentialVersions: rawVersions, Reason: d.Reason,
	})
	if err != nil {
		return store.AppDesiredSpec{}, fmt.Errorf("apps: record desired spec: %w", err)
	}
	n, err := q.SetAppInstanceDesiredGeneration(ctx, store.SetAppInstanceDesiredGenerationParams{ID: inst.ID, Generation: gen, Previous: inst.DesiredGeneration})
	if err != nil {
		return store.AppDesiredSpec{}, fmt.Errorf("apps: set desired generation: %w", err)
	} else if n != 1 {
		// The caller did not hold the instance lock: refuse rather than
		// leave desired_generation behind the specs.
		return store.AppDesiredSpec{}, fmt.Errorf("apps: desired generation of %s changed concurrently", inst.Key)
	}
	kind := agentapi.KindApply
	switch {
	case d.RunState == agentapi.StateAbsent:
		kind = agentapi.KindRemove
	case inst.PackageVersion != nil && *inst.PackageVersion != d.PackageVersion:
		kind = agentapi.KindUpgrade
	}
	if _, err := q.InsertAppOperation(ctx, store.InsertAppOperationParams{
		InstanceID: inst.ID, Generation: &gen, Kind: kind, State: agentapi.OpQueued,
		FromVersion: inst.PackageVersion, ToVersion: &d.PackageVersion, StartedBy: actorPerson(actor),
	}); err != nil {
		return store.AppDesiredSpec{}, fmt.Errorf("apps: record operation: %w", err)
	}
	if err := s.enqueue(ctx, tx, AgentApplyArgs{InstanceID: inst.ID, Generation: gen}); err != nil {
		return store.AppDesiredSpec{}, err
	}
	return ds, nil
}

// specError ends an operation before the agent saw it, with a stable code.
type specError struct {
	code string
	err  error
}

func (e *specError) Error() string { return fmt.Sprintf("apps: %s: %v", e.code, e.err) }
func (e *specError) Unwrap() error { return e.err }

// buildSpec assembles the spec of a desired generation. Secrets are the
// credential versions the generation recorded, so every push of it is the
// same spec (the agent answers a changed spec with generation_conflict).
// The spec carries secrets: it is never logged or stored.
func (s *Service) buildSpec(ctx context.Context, inst Instance, ds store.AppDesiredSpec) (*agentapi.Spec, error) {
	pkg, ok := s.pkg(inst.PackageID, ds.PackageVersion)
	if !ok {
		return nil, &specError{code: CodePackageUnavailable, err: fmt.Errorf("package version %s is not embedded", ds.PackageVersion)}
	}
	settings, err := decodeSettings(ds.Settings)
	if err != nil {
		return nil, err
	}
	var versions map[string]int32
	if err := json.Unmarshal(ds.CredentialVersions, &versions); err != nil {
		return nil, fmt.Errorf("apps: decode credential versions: %w", err)
	}
	q := store.New(s.pool)
	secrets := make(map[string]string, len(versions))
	for name, version := range versions {
		row, err := q.GetAppCredential(ctx, store.GetAppCredentialParams{InstanceID: inst.ID, Name: name, Version: version})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &specError{code: CodeCredentialMissing, err: fmt.Errorf("credential %s version %d is gone", name, version)}
		} else if err != nil {
			return nil, fmt.Errorf("apps: load credential: %w", err)
		}
		plain, err := s.openCredential(row)
		if err != nil {
			return nil, err
		}
		secrets[name] = string(plain)
	}
	spec := &agentapi.Spec{
		InstanceID: inst.ID,
		Generation: ds.Generation,
		Package:    pkg.Ref(),
		State:      ds.RunState,
		Purge:      ds.Purge,
		Hosts:      s.ServiceHosts(inst.AppInstance, pkg),
		URLs: agentapi.URLs{
			Workspace: s.cfg.WorkspaceURL.String(),
			Issuer:    s.cfg.IssuerURL.String(),
			Instance:  derefString(inst.BaseUrl),
			Webhook:   s.WebhookURL(inst.ID).String(),
		},
		Ingress:  s.cfg.IngressMode,
		Settings: settings,
		Secrets:  secrets,
	}
	return spec, nil
}

// ServiceHosts maps the package's routes to public hosts, plus
// "workspace": the first route is the instance's base URL host, further
// routes config.ServiceHost of their label. It is the single source of
// service hosts: the spec's Hosts and the ingress routes (#61) use it.
func (s *Service) ServiceHosts(row store.AppInstance, pkg *packages.Package) map[string]string {
	hosts := map[string]string{packages.HostWorkspace: s.cfg.WorkspaceURL.Hostname()}
	for i, r := range pkg.Manifest.Routes {
		host := s.cfg.ServiceHost(r.HostLabel)
		if i == 0 && row.BaseUrl != nil {
			if u, err := url.Parse(*row.BaseUrl); err == nil && u.Hostname() != "" {
				host = u.Hostname()
			}
		}
		hosts[r.Name] = host
	}
	return hosts
}

// serviceURL is the public URL of a service host label.
func (s *Service) serviceURL(label string) string { return s.HostURL(s.cfg.ServiceHost(label)) }

// Upstream is a managed instance's first route on the edge network, the
// alias Traefik's routes use (agent.md "Networking"), from the package
// InstancePackage returns; nil without one. Part 2 of #75 points it to the
// fenced alias while an upgrade waits for its confirmation.
func (s *Service) Upstream(row store.AppInstance) *url.URL {
	pkg, ok := s.InstancePackage(row)
	if !ok || len(pkg.Manifest.Routes) == 0 {
		return nil
	}
	r := pkg.Manifest.Routes[0]
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(packages.ServiceAlias(row.Key, r.Service, false), strconv.Itoa(r.Port))}
}

// InstancePackage is the package version whose routes and hosts apply to
// a managed instance: the running version, else (not reported yet, or no
// longer embedded) the newest embedded one.
func (s *Service) InstancePackage(row store.AppInstance) (*packages.Package, bool) {
	if row.PackageID == nil {
		return nil, false
	}
	if pkg, ok := s.pkg(row.PackageID, derefString(row.PackageVersion)); ok {
		return pkg, true
	}
	return s.latestPackage(*row.PackageID)
}

func (s *Service) pkg(id *string, version string) (*packages.Package, bool) {
	if id == nil || s.cfg.Packages == nil {
		return nil, false
	}
	return s.cfg.Packages.Get(agentapi.PackageRef{ID: *id, Version: version})
}

// latestPackage returns the newest embedded version of a package.
func (s *Service) latestPackage(id string) (*packages.Package, bool) {
	if s.cfg.Packages == nil {
		return nil, false
	}
	var newest *packages.Package
	for _, ref := range s.cfg.Packages.List() { // sorted by ID, then version
		if ref.ID == id {
			newest, _ = s.cfg.Packages.Get(ref)
		}
	}
	return newest, newest != nil
}

func actorPerson(a audit.Actor) *uuid.UUID {
	if a.Kind != audit.ActorPerson {
		return nil
	}
	id, err := uuid.Parse(a.ID)
	if err != nil {
		return nil
	}
	return &id
}

// AgentApplyArgs push one desired spec generation to the agent and mirror
// its operation until it is terminal.
type AgentApplyArgs struct {
	InstanceID uuid.UUID `json:"instance_id" river:"unique"`
	Generation int64     `json:"generation" river:"unique"`
}

// Kind implements river.JobArgs.
func (AgentApplyArgs) Kind() string { return "orglane.apps.agent_apply" }

// InsertOpts implements river.JobArgsWithInsertOpts. A push waits for the
// agent as long as it takes (backoff capped by Config.AgentRetryMax); a
// discarded job is queued again by the sync.
func (AgentApplyArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxAgentApplyAttempts) }

// AgentSyncArgs read the agent's statuses into app_instances, check health
// and re-push what the agent lost.
type AgentSyncArgs struct {
	// FollowUp marks the one job queued while another runs.
	FollowUp bool `json:"follow_up,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (AgentSyncArgs) Kind() string { return "orglane.apps.agent_sync" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (AgentSyncArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxDefaultAttempts) }

const maxAgentApplyAttempts = 10000

type agentApplyWorker struct {
	river.WorkerDefaults[AgentApplyArgs]
	s *Service
}

func (w *agentApplyWorker) Work(ctx context.Context, job *river.Job[AgentApplyArgs]) error {
	return w.s.RunAgentApply(ctx, job.Args)
}

// Timeout bounds one push or poll; waiting is snoozing, not working.
func (w *agentApplyWorker) Timeout(*river.Job[AgentApplyArgs]) time.Duration { return 3 * time.Minute }

// NextRetry backs off exponentially while the agent is unavailable.
func (w *agentApplyWorker) NextRetry(job *river.Job[AgentApplyArgs]) time.Time {
	return time.Now().Add(w.s.AgentBackoff(job.Attempt))
}

// AgentBackoff is the delay before attempt+1 of a push that waits for the
// agent: 1s, 2s, 4s, ... capped by Config.AgentRetryMax.
func (s *Service) AgentBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 20 {
		return s.cfg.AgentRetryMax
	}
	return min(time.Second<<(attempt-1), s.cfg.AgentRetryMax)
}

type agentSyncWorker struct {
	river.WorkerDefaults[AgentSyncArgs]
	s *Service
}

func (w *agentSyncWorker) Work(ctx context.Context, _ *river.Job[AgentSyncArgs]) error {
	return w.s.RunAgentSync(ctx)
}

func (w *agentSyncWorker) Timeout(*river.Job[AgentSyncArgs]) time.Duration { return 10 * time.Minute }
