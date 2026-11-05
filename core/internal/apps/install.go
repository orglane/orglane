package apps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// This file is the install API's side of managed instances (#61,
// docs/specs/providers.md "Managed instance lifecycle in the core"): the
// checks before an install, the agent's status for Settings → Apps, and
// removal, purge and reinstall. Every desired state goes through
// RecordDesired; the consequences of a removal's operation are applied by
// operationFinished (agent_apply.go), fenced by generation.

// Codes of refused installs.
const (
	// CodeHostTaken: another instance (or retained data) uses the host.
	CodeHostTaken = "host_taken"
	// CodeLocalhostWorkspace: in local mode the workspace host is the bare
	// "localhost", which a container cannot reach (agent.md "Local mode").
	CodeLocalhostWorkspace = "localhost_workspace"
	// CodeHostNotLocal: in local mode a service host must be a *.localhost
	// name, which browsers resolve to this machine.
	CodeHostNotLocal = "host_not_local"
	// CodeInsufficientMemory: the host's memory is below the sum of the
	// packages' minimums plus the bundle's share.
	CodeInsufficientMemory = "insufficient_memory"
	// CodeInsufficientDisk: free disk is below the package's minimum.
	CodeInsufficientDisk = "insufficient_disk"
	// CodeRemoveFailed: the removal's operation failed.
	CodeRemoveFailed = "remove_failed"
)

// BundleMemory is the memory the Compose bundle itself (core, PostgreSQL,
// Traefik, agent) is assumed to need beside the managed services
// (agent.md "Status, health and logs").
const BundleMemory = 1 << 30

// CodedError is a refusal with a stable problem code (Code) and an English
// detail without secrets.
type CodedError struct {
	Code   string
	Detail string
}

func (e *CodedError) Error() string { return "apps: " + e.Detail }

func refused(code, format string, args ...any) error {
	return &CodedError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// AgentStatus is what Settings → Apps shows about the agent: whether
// managed services can be installed and the host's resources.
type AgentStatus struct {
	// Enabled: managed services are configured (agent.socket and secret).
	Enabled bool
	// Connected: the agent answered, with a compatible version.
	Connected bool
	// Code says why it is not connected: agent_unavailable,
	// agent_version_mismatch or agent_untrusted.
	Code    string
	Version string
	// Host resources the agent reports (zero when not connected).
	MemTotal, MemAvailable, DiskFree uint64
	// MemoryReserved is the bundle's share plus the minimum memory of every
	// managed instance that holds its key.
	MemoryReserved uint64
}

// AgentStatus asks the agent for its health.
func (s *Service) AgentStatus(ctx context.Context) (AgentStatus, error) {
	st := AgentStatus{Enabled: s.cfg.Agent != nil, Code: CodeAgentUnavailable}
	if s.cfg.Agent == nil {
		return st, nil
	}
	h, err := s.cfg.Agent.Health(ctx)
	if err != nil {
		if code, kind := classifyAgentError(err); kind == agentWaiting {
			st.Code = code
		}
		return st, nil
	}
	reserved, err := s.reservedMemory(ctx, uuid.Nil)
	if err != nil {
		return st, err
	}
	st.Version, st.MemTotal, st.MemAvailable, st.DiskFree, st.MemoryReserved = h.Version, h.MemTotal, h.MemAvailable, h.DiskFree, reserved
	if !agentclient.CompatibleVersions(s.cfg.CoreVersion, h.Version) {
		st.Code = CodeAgentVersionMismatch
		return st, nil
	}
	st.Connected, st.Code = true, ""
	return st, nil
}

// reservedMemory is BundleMemory plus the minimum memory of the packages
// of every managed instance that holds its key, except one.
func (s *Service) reservedMemory(ctx context.Context, except uuid.UUID) (uint64, error) {
	rows, err := store.New(s.pool).ListAppInstances(ctx)
	if err != nil {
		return 0, fmt.Errorf("apps: list instances: %w", err)
	}
	total := uint64(BundleMemory)
	for _, row := range rows {
		if row.ID == except || row.PackageID == nil || (row.State == StateRemoved && !row.DataRetained) {
			continue
		}
		if pkg, ok := s.InstancePackage(row); ok {
			total += uint64(pkg.Manifest.Resources.Memory.Min) //nolint:gosec // quantities are non-negative
		}
	}
	return total, nil
}

// InstallDefaults are what an install of a managed provider uses when the
// request leaves them out.
type InstallDefaults struct {
	Key      string
	Host     string
	Settings map[string]any
	Package  *packages.Package
}

// InstallDefaults returns the defaults of an install of a managed provider,
// or false when it cannot be installed in this build.
func (s *Service) InstallDefaults(a *Adapter) (InstallDefaults, bool) {
	if _, ok := a.Lifecycle(); !ok {
		return InstallDefaults{}, false
	}
	pkg, ok := s.latestPackage(a.Manifest.Package)
	if !ok {
		return InstallDefaults{}, false
	}
	d := InstallDefaults{Key: a.Manifest.DefaultKey, Settings: a.settingDefaults(s.cfg.DomainBase), Package: pkg}
	if len(pkg.Manifest.Routes) > 0 {
		d.Host = s.cfg.ServiceHost(pkg.Manifest.Routes[0].HostLabel)
	}
	return d, true
}

// HostURL is the base URL of a service host: the workspace's scheme and port.
func (s *Service) HostURL(host string) string {
	u := url.URL{Scheme: s.cfg.WorkspaceURL.Scheme, Host: host}
	if port := s.cfg.WorkspaceURL.Port(); port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	return u.String()
}

// CheckInstall is the production InstallPreflight (wired by the server):
// local mode needs *.localhost names, the agent must be connected with a
// compatible version, the host's memory and disk must cover the package's
// minimums, and in domain mode the host must resolve to the workspace's
// addresses. Checks that need no network come first.
func (s *Service) CheckInstall(ctx context.Context, plan InstallPlan) error {
	if s.cfg.IngressMode == agentapi.IngressLocal {
		ws := s.cfg.WorkspaceURL.Hostname()
		if !strings.HasSuffix(ws, ".localhost") {
			return refused(CodeLocalhostWorkspace, "in local mode managed services need a workspace host under localhost such as orglane.localhost, not %s (run the installer's enable-managed)", ws)
		}
		if !strings.HasSuffix(plan.Host, ".localhost") {
			return refused(CodeHostNotLocal, "in local mode the host must end in .localhost, not %s", plan.Host)
		}
	}
	st, err := s.AgentStatus(ctx)
	if err != nil {
		return err
	}
	if !st.Connected {
		return refused(st.Code, "the agent that runs managed services is not usable")
	}
	if err := checkResources(st, plan.Package); err != nil {
		return err
	}
	if s.cfg.IngressMode == agentapi.IngressDomain {
		return CheckServiceHost(ctx, s.cfg.Resolver, s.cfg.WorkspaceURL.Hostname(), plan.Host)
	}
	return nil
}

// checkResources refuses an install whose package does not fit: total
// memory below what is reserved plus the package's minimum, or free disk
// below its minimum (agent.md "Status, health and logs").
func checkResources(st AgentStatus, pkg *packages.Package) error {
	r := pkg.Manifest.Resources
	need := st.MemoryReserved + uint64(r.Memory.Min) //nolint:gosec // quantities are non-negative
	if st.MemTotal < need {
		return refused(CodeInsufficientMemory, "the host has %d MiB of memory, the bundle and the installed apps need at least %d MiB", st.MemTotal>>20, need>>20)
	}
	if st.DiskFree < uint64(r.Disk.Min) { //nolint:gosec // quantities are non-negative
		return refused(CodeInsufficientDisk, "%d MiB of disk are free, the app needs at least %d MiB", st.DiskFree>>20, uint64(r.Disk.Min)>>20) //nolint:gosec // quantities are non-negative
	}
	return nil
}

// validHost returns the host of a managed instance's base URL, refusing
// one that is not a host name or is the workspace host, and a URL that is
// more than a host: the service is reached through the workspace's Traefik,
// so the URL has the workspace's scheme and port, and no credentials, path,
// query or fragment. It needs nothing but the input, so Install runs it
// before the preflight: an invalid host is invalid input, never a DNS or
// agent refusal.
func (s *Service) validHost(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !packages.ValidHostname(u.Hostname()) {
		return "", invalid("host", "must be a valid host name")
	}
	ws := s.cfg.WorkspaceURL
	if u.Scheme != ws.Scheme || u.Port() != ws.Port() || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", invalid("host", "must be a host name only, served like the workspace (%s, port %q)", ws.Scheme, ws.Port())
	}
	host := u.Hostname()
	if strings.EqualFold(host, s.cfg.WorkspaceURL.Hostname()) {
		return "", invalid("host", "must differ from the workspace host")
	}
	return host, nil
}

// checkHost refuses a host held by an instance other than self. It runs in
// the install's transaction under a lock, so two concurrent installs cannot
// take the same host.
func (s *Service) checkHost(ctx context.Context, q *store.Queries, host string, self uuid.UUID) error {
	if err := q.AcquireXactLock(ctx, lockID("app-host")); err != nil {
		return fmt.Errorf("apps: lock hosts: %w", err)
	}
	rows, err := q.ListReservedAppInstanceHosts(ctx)
	if err != nil {
		return fmt.Errorf("apps: list hosts: %w", err)
	}
	for _, r := range rows {
		if r.ID == self {
			continue
		}
		if other, err := url.Parse(r.BaseUrl); err == nil && strings.EqualFold(other.Hostname(), host) {
			return refused(CodeHostTaken, "another app uses %s", host)
		}
	}
	return nil
}

// Remove removes a managed instance: its services stop and their
// containers are removed (a desired generation with state absent). Without
// purge its data, database and key are retained, so Reinstall can bring it
// back; with purge they are deleted, which the caller must confirm with the
// instance key. A retained instance can be purged later. The instance is
// removing until the agent's operation ends: removed on success, failed
// (remove_failed) otherwise, from where it can be removed again.
func (s *Service) Remove(ctx context.Context, actor audit.Actor, id uuid.UUID, purge bool, confirm string) (Instance, error) {
	var out Instance
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		cur, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if cur.Management != string(provider.Managed) {
			return fmt.Errorf("%w: only managed apps are removed here", ErrNotSupported)
		}
		if purge && confirm != cur.Key {
			return invalid("confirm", "must be the instance key to purge its data")
		}
		switch {
		case cur.State == StateRemoved && cur.DataRetained && purge:
		case cur.State == StateActive, cur.State == StateDegraded, cur.State == StateFailed:
		default:
			return fmt.Errorf("%w: a %s app cannot be removed%s", ErrInvalidState, cur.State, map[bool]string{true: " or purged", false: ""}[purge])
		}
		if cur.State == StateRemoved {
			// The CHECK on data_retained requires it before leaving removed.
			if n, err := q.ClearAppInstanceRetained(ctx, id); err != nil {
				return fmt.Errorf("apps: purge: %w", err)
			} else if n != 1 {
				return errors.New("apps: the retained instance changed under its lock")
			}
		}
		bindings, err := q.DeleteAppInstanceBindings(ctx, id)
		if err != nil {
			return fmt.Errorf("apps: remove bindings: %w", err)
		}
		if _, err := s.transition(ctx, tx, q, actor, id, StateRemoving, ""); err != nil {
			return err
		}
		ds, err := s.RecordDesired(ctx, tx, actor, id, DesiredChange{RunState: agentapi.StateAbsent, Purge: purge, Reason: ReasonRemove})
		if err != nil {
			return err
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: audit.AppInstanceRemoved, Target: instanceTarget(id),
			Metadata: map[string]any{"provider": cur.Provider, "key": cur.Key, "purge": purge, "generation": ds.Generation, "bindings_removed": bindings},
		}); err != nil {
			return err
		}
		out, err = s.loadInstance(ctx, q, id, false)
		return err
	})
	return out, err
}

// Reinstall brings back a removed instance whose data is retained: the same
// ID, key, credentials and data, a new desired generation with state
// running (the version it ran). It installs again, and Setup activates it.
func (s *Service) Reinstall(ctx context.Context, actor audit.Actor, id uuid.UUID) (Instance, error) {
	var out Instance
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		cur, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if cur.Management != string(provider.Managed) {
			return fmt.Errorf("%w: only managed apps are reinstalled", ErrNotSupported)
		}
		n, err := q.SetAppInstanceReinstalling(ctx, id)
		if err != nil {
			return fmt.Errorf("apps: reinstall: %w", err)
		} else if n == 0 {
			return fmt.Errorf("%w: only a removed app whose data is retained can be reinstalled", ErrInvalidState)
		}
		ds, err := s.RecordDesired(ctx, tx, actor, id, DesiredChange{RunState: agentapi.StateRunning, Reason: ReasonReinstall})
		if err != nil {
			return err
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: audit.AppInstanceReinstalled, Target: instanceTarget(id),
			Metadata: map[string]any{"provider": cur.Provider, "key": cur.Key, "generation": ds.Generation, "package_version": ds.PackageVersion},
		}); err != nil {
			return err
		}
		out, err = s.loadInstance(ctx, q, id, false)
		return err
	})
	return out, err
}

// removalFinished applies the end of a removal's operation (an absent
// generation) in the operation's transaction. inst is locked; only the
// instance's current generation in state removing changes anything, and the
// update re-checks both, so a stale result never regresses the state.
func (s *Service) removalFinished(ctx context.Context, tx pgx.Tx, q *store.Queries, inst Instance, ds store.AppDesiredSpec, state string) error {
	if ds.Generation != inst.DesiredGeneration || inst.State != StateRemoving {
		return nil
	}
	if state != agentapi.OpSucceeded {
		_, err := s.transition(ctx, tx, q, audit.System, inst.ID, StateFailed, CodeRemoveFailed)
		return err
	}
	n, err := q.SetAppInstanceRemoved(ctx, store.SetAppInstanceRemovedParams{ID: inst.ID, DataRetained: !ds.Purge, Generation: ds.Generation})
	if err != nil {
		return fmt.Errorf("apps: set removed: %w", err)
	} else if n == 0 {
		return errors.New("apps: the removed instance changed under its lock")
	}
	if _, err := audit.Record(ctx, q, audit.Entry{
		Actor: audit.System, Action: audit.AppInstanceStateChanged, Target: instanceTarget(inst.ID),
		Metadata: map[string]any{"provider": inst.Provider, "from": StateRemoving, "to": StateRemoved, "data_retained": !ds.Purge},
	}); err != nil {
		return err
	}
	if !ds.Purge {
		return nil
	}
	// Purged: the agent deleted the data, database and role; the
	// credentials go too, and the key is free again.
	creds, err := q.DeleteAppInstanceCredentials(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("apps: delete credentials: %w", err)
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor: audit.System, Action: audit.AppInstancePurged, Target: instanceTarget(inst.ID),
		Metadata: map[string]any{"provider": inst.Provider, "key": inst.Key, "generation": ds.Generation, "credentials_deleted": creds},
	})
	return err
}
