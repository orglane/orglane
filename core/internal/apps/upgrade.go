package apps

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// This file is the second part of #75: the core drives package upgrades
// (docs/specs/agent.md "Upgrade" and "Implementation notes (#75, part 2)").
//
// The core never takes a version change into its own hands: it records one
// upgrade spec per required stop, the agent runs it (snapshot, fenced
// candidate, journaled rollback) and the core confirms the candidate through
// its fenced alias or aborts it. A rolled-back or failed operation records a
// revert spec with the running version, so the agent's blocked generation is
// acknowledged; Setup runs after that apply (or after the last upgrade step)
// and activates the instance again.

// Upgrade states of AppInstance.upgrade (AppUpgradeStatus in the API).
const (
	UpgradeUpToDate   = "up_to_date"
	UpgradeAvailable  = "available"
	UpgradeInProgress = "upgrading"
	UpgradeFailed     = "failed"
	UpgradeRolledBack = "rolled_back"
	UpgradeBlocked    = "blocked"
)

// DefaultUpgradeCheckTimeout bounds Lifecycle.CheckHealth of an upgrade
// candidate; the agent's confirmation deadline may shorten it.
const DefaultUpgradeCheckTimeout = 30 * time.Second

// UpgradeStatus is the upgrade state of a managed instance, derived from
// its running version, the embedded packages and the latest upgrade
// operation. The zero value (State "") means no status: connected
// instances, an unavailable provider, no running version yet, or a failure
// that the upgrade retry cannot recover (a failed rollback, a failed setup
// or install).
type UpgradeStatus struct {
	State           string
	CurrentVersion  string
	TargetVersion   string
	RequiredVersion string
}

// UpgradeStatus returns the instance's upgrade status (docs/specs/agent.md
// "Upgrade"), for Settings → Apps.
func (s *Service) UpgradeStatus(ctx context.Context, inst Instance) (UpgradeStatus, bool, error) {
	if s.cfg.Packages == nil || inst.Adapter == nil || inst.PackageID == nil {
		return UpgradeStatus{}, false, nil
	}
	switch inst.State {
	case StateActive, StateDegraded, StateUpgrading:
	case StateFailed:
		// A failed revert is retryable through RetryUpgrade: report the
		// upgrade result it acknowledges so Settings → Apps can show the
		// retry action. A failed rollback, setup or install has no status.
		if _, _, ok, err := failedRevertState(ctx, store.New(s.pool), inst); err != nil {
			return UpgradeStatus{}, false, err
		} else if !ok {
			return UpgradeStatus{}, false, nil
		}
	default:
		return UpgradeStatus{}, false, nil
	}
	latest, ok := s.latestPackage(*inst.PackageID)
	if !ok {
		return UpgradeStatus{}, false, nil
	}
	current := derefString(inst.PackageVersion)
	if current == "" {
		// The agent has not reported a running version yet.
		return UpgradeStatus{}, false, nil
	}
	status := UpgradeStatus{CurrentVersion: current, TargetVersion: latest.Manifest.Version}
	// The last step's Setup still holds the instance (and its maintenance
	// routes) after the running version reached the newest one: report the
	// upgrade in progress until Setup activates it.
	if inst.State == StateUpgrading {
		if path, err := s.cfg.Packages.UpgradePath(*inst.PackageID, current, latest.Manifest.Version); err == nil && len(path) > 0 {
			status.TargetVersion = path[0]
		}
		status.State = UpgradeInProgress
		return status, true, nil
	}
	if inst.State == StateFailed {
		// A failed revert never reports up_to_date: the running version may
		// already be the newest one, but the instance still needs the retry.
		// The latest upgrade operation names the step the revert
		// acknowledged; a rolled-back step is what the UI explains.
		op, err := store.New(s.pool).GetLatestAppUpgradeOperation(ctx, inst.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return UpgradeStatus{}, false, fmt.Errorf("apps: load latest upgrade: %w", err)
		}
		if err == nil && op.ToVersion != nil {
			status.TargetVersion = *op.ToVersion
			if op.State == agentapi.OpRolledBack {
				status.State = UpgradeRolledBack
			} else {
				status.State = UpgradeFailed
			}
		} else {
			status.State = UpgradeFailed
		}
		return status, true, nil
	}
	if current == latest.Manifest.Version {
		status.State = UpgradeUpToDate
		return status, true, nil
	}
	path, err := s.cfg.Packages.UpgradePath(*inst.PackageID, current, latest.Manifest.Version)
	if err != nil {
		status.State = UpgradeBlocked
		var ue *packages.UpgradeError
		if errors.As(err, &ue) {
			status.RequiredVersion = cmp.Or(ue.Required, ue.MinFrom)
		}
		return status, true, nil
	}
	status.TargetVersion = path[0]
	op, err := store.New(s.pool).GetLatestAppUpgradeOperation(ctx, inst.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		status.State = UpgradeAvailable
	case err != nil:
		return UpgradeStatus{}, false, fmt.Errorf("apps: load latest upgrade: %w", err)
	case op.ToVersion != nil && *op.ToVersion == status.TargetVersion:
		switch {
		case op.FinishedAt == nil:
			status.State = UpgradeInProgress
		case op.State == agentapi.OpFailed:
			status.State = UpgradeFailed
		case op.State == agentapi.OpRolledBack:
			status.State = UpgradeRolledBack
		default:
			status.State = UpgradeAvailable
		}
	default:
		status.State = UpgradeAvailable
	}
	return status, true, nil
}

// nextUpgradeVersion is the next required stop from the instance's running
// version (packages.Registry.UpgradePath), if one is embedded and the
// instance is not already on the newest one.
func (s *Service) nextUpgradeVersion(inst Instance) (string, bool) {
	if s.cfg.Packages == nil || inst.PackageID == nil {
		return "", false
	}
	latest, ok := s.latestPackage(*inst.PackageID)
	if !ok {
		return "", false
	}
	current := derefString(inst.PackageVersion)
	if current == "" || current == latest.Manifest.Version {
		return "", false
	}
	path, err := s.cfg.Packages.UpgradePath(*inst.PackageID, current, latest.Manifest.Version)
	if err != nil || len(path) == 0 {
		return "", false
	}
	return path[0], true
}

// planUpgrades records the next automatic upgrade step of one active or
// degraded instance whose running version is behind the newest embedded
// package version (docs/specs/agent.md "Upgrade"). It runs from agent_sync,
// so it runs at core start and every sync interval.
//
// One instance at a time: while any managed instance is upgrading (or has an
// unfinished upgrade operation), nothing is planned. The database enforces
// the same invariant with a partial unique index, so two concurrent syncs
// (or an admin retry) can never start a second upgrade. Each instance is
// planned at most one step per call; operationFinished records the next
// step after a successful one.
//
// Instances with an unfinished push, with the desired generation not yet
// observed, or whose newest upgrade operation for the same target failed or
// rolled back are left alone: a failed or rolled-back upgrade is retried
// only through the admin's POST /upgrade.
func (s *Service) planUpgrades(ctx context.Context, rows []store.AppInstance, pushing map[uuid.UUID]bool) error {
	if s.cfg.Packages == nil {
		return nil
	}
	var errs []error
	for _, row := range rows {
		if row.Management != string(provider.Managed) || pushing[row.ID] {
			continue
		}
		planned, err := s.planUpgrade(ctx, row.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("apps: plan upgrade of %s: %w", row.Key, err))
			continue
		}
		switch planned {
		case planRecorded:
			return errors.Join(errs...)
		case planBusy:
			// Another instance is upgrading: it must settle first.
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

// planResult is what planUpgrade did.
type planResult int

const (
	planSkipped  planResult = iota // no step for this instance now
	planRecorded                   // one step was recorded
	planBusy                       // another managed upgrade is in progress
)

// planUpgrade records one automatic upgrade step under the instance lock.
func (s *Service) planUpgrade(ctx context.Context, id uuid.UUID) (planResult, error) {
	result := planSkipped
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if inst.State == StateUpgrading {
			result = planBusy
			return nil
		}
		if inst.Adapter == nil || (inst.State != StateActive && inst.State != StateDegraded) {
			return nil
		}
		if inst.DesiredGeneration != inst.ObservedGeneration {
			return nil
		}
		target, ok := s.nextUpgradeVersion(inst)
		if !ok {
			return nil
		}
		unfinished, err := q.ListUnfinishedAppPushOperations(ctx)
		if err != nil {
			return fmt.Errorf("load unfinished operations: %w", err)
		}
		for _, u := range unfinished {
			if u.InstanceID == id {
				return nil
			}
		}
		last, err := q.GetLatestAppUpgradeOperation(ctx, id)
		switch {
		case err == nil:
			if last.FinishedAt == nil {
				return nil
			}
			if last.ToVersion != nil && *last.ToVersion == target &&
				(last.State == agentapi.OpFailed || last.State == agentapi.OpRolledBack) {
				return nil
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("load latest upgrade: %w", err)
		}
		// The row lock only serializes plans of the same instance. Two
		// concurrent syncs can pick different instances; this check and
		// the partial unique index (app_instances_upgrading_key) make the
		// second one stop instead of starting a parallel upgrade.
		busy, err := q.AppUpgradeInProgress(ctx, id)
		if err != nil {
			return fmt.Errorf("check upgrading instances: %w", err)
		}
		if busy != nil && *busy {
			result = planBusy
			return nil
		}
		upgrading, err := s.transition(ctx, tx, q, audit.System, id, StateUpgrading, "")
		if err != nil {
			if isUpgradingConflict(err) {
				// The row update already aborted the transaction; rolling
				// it back with a sentinel lets the caller report busy
				// instead of a commit failure.
				return errPlanBusy
			}
			return err
		}
		result = planRecorded
		return s.recordUpgradeStep(ctx, tx, q, audit.System, upgrading, target, ReasonUpgrade, false)
	})
	if errors.Is(err, errPlanBusy) {
		return planBusy, nil
	}
	return result, err
}

// errPlanBusy rolls back a plan whose transition lost to the partial unique
// index of a concurrent plan or admin retry (app_instances_upgrading_key).
var errPlanBusy = errors.New("apps: another managed instance is upgrading")

// recordUpgradeStep records the next desired generation of an upgrade with
// its operation and job, and audits the start.
func (s *Service) recordUpgradeStep(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, inst Instance, version, reason string, retry bool) error {
	from := derefString(inst.PackageVersion)
	ds, err := s.recordDesired(ctx, tx, q, actor, inst, DesiredChange{
		RunState: agentapi.StateRunning, PackageVersion: version, Reason: reason,
	})
	if err != nil {
		return err
	}
	meta := map[string]any{"provider": inst.Provider, "from": from, "to": version, "generation": ds.Generation}
	if retry {
		meta["retry"] = true
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor: actor, Action: audit.AppInstanceUpgradeStarted, Target: instanceTarget(inst.ID), Metadata: meta,
	})
	return err
}

// RetryUpgrade retries a rolled-back or failed upgrade (providers.md
// "Managed instance lifecycle in the core", agent.md "Upgrade"). It records
// the next required stop as the reason retry; the agent never retries by
// itself. Only a settled, active or degraded instance whose latest upgrade
// to that stop ended failed or rolled back can be retried.
//
// A failed instance is retried when its durable desired generation is a
// revert whose apply failed (`failedRevertState`): a failure while applying the
// revert re-applies that generation's spec, so a transient Docker failure
// is recoverable once its cause is gone. A revert that could not be
// assembled or resolved (package_unavailable, unknown_package_version) is
// instead replaced by the next upgrade step from the running version, as
// older releases recorded it. A failed rollback is never retried here.
func (s *Service) RetryUpgrade(ctx context.Context, actor audit.Actor, id uuid.UUID) (Instance, error) {
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if inst.Adapter == nil {
			return ErrProviderUnavailable
		}
		if _, ok := inst.Adapter.Lifecycle(); !ok {
			return ErrNotSupported
		}
		revertSpec, revertOp, failedRevert, err := failedRevertState(ctx, q, inst)
		if err != nil {
			return err
		}
		if inst.State != StateActive && inst.State != StateDegraded && !failedRevert {
			return fmt.Errorf("%w: only an active, degraded or failed-revert instance can retry an upgrade", ErrInvalidState)
		}
		if failedRevert && retryRevertApply(revertOp) {
			return s.retryRevert(ctx, tx, q, actor, inst, revertSpec)
		}
		if !failedRevert && inst.DesiredGeneration != inst.ObservedGeneration {
			return fmt.Errorf("%w: the instance has not applied its current change", ErrInvalidState)
		}
		unfinished, err := q.ListUnfinishedAppPushOperations(ctx)
		if err != nil {
			return fmt.Errorf("load unfinished operations: %w", err)
		}
		for _, u := range unfinished {
			if u.InstanceID == id {
				return fmt.Errorf("%w: an operation of the instance is still running", ErrInvalidState)
			}
		}
		target, ok := s.nextUpgradeVersion(inst)
		if !ok {
			return fmt.Errorf("%w: no newer package version is embedded", ErrInvalidState)
		}
		last, err := q.GetLatestAppUpgradeOperation(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: there is no failed or rolled-back upgrade to retry", ErrInvalidState)
		} else if err != nil {
			return fmt.Errorf("load latest upgrade: %w", err)
		}
		if last.ToVersion == nil || *last.ToVersion != target || (last.State != agentapi.OpFailed && last.State != agentapi.OpRolledBack) {
			return fmt.Errorf("%w: there is no failed or rolled-back upgrade to retry", ErrInvalidState)
		}
		upgrading, err := s.transition(ctx, tx, q, actor, id, StateUpgrading, "")
		if err != nil {
			if isUpgradingConflict(err) {
				return fmt.Errorf("%w: another managed instance is upgrading", ErrInvalidState)
			}
			return err
		}
		return s.recordUpgradeStep(ctx, tx, q, actor, upgrading, target, ReasonRetry, true)
	})
	if err != nil {
		return Instance{}, err
	}
	return s.Instance(ctx, id)
}

// failedRevertState reports the durable desired spec and operation of an instance
// that failed while acknowledging a rollback: the current desired reason is
// revert and that generation's operation is failed. It never covers a failed
// rollback (rollback_failed), which only an agent restore can recover, or a
// setup that failed after a successful revert (RetrySetup handles that).
func failedRevertState(ctx context.Context, q *store.Queries, inst Instance) (store.AppDesiredSpec, store.AppOperation, bool, error) {
	if inst.State != StateFailed || inst.PackageVersion == nil ||
		derefString(inst.HealthCode) == agentapi.CodeRollbackFailed {
		return store.AppDesiredSpec{}, store.AppOperation{}, false, nil
	}
	gen := inst.DesiredGeneration
	ds, err := q.GetAppDesiredSpec(ctx, store.GetAppDesiredSpecParams{InstanceID: inst.ID, Generation: gen})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AppDesiredSpec{}, store.AppOperation{}, false, nil
	} else if err != nil {
		return store.AppDesiredSpec{}, store.AppOperation{}, false, fmt.Errorf("load desired spec: %w", err)
	}
	if ds.Reason != ReasonRevert {
		return store.AppDesiredSpec{}, store.AppOperation{}, false, nil
	}
	op, err := q.GetAppPushOperation(ctx, store.GetAppPushOperationParams{InstanceID: inst.ID, Generation: &gen})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AppDesiredSpec{}, store.AppOperation{}, false, nil
	} else if err != nil {
		return store.AppDesiredSpec{}, store.AppOperation{}, false, fmt.Errorf("load revert operation: %w", err)
	}
	if op.State != agentapi.OpFailed {
		return store.AppDesiredSpec{}, store.AppOperation{}, false, nil
	}
	return ds, op, true, nil
}

// retryRevertApply reports whether a failed revert got as far as applying
// its spec. package_unavailable and unknown_package_version mean the revert
// could not be assembled or resolved; re-applying the same durable spec
// would fail again, so the retry starts the next upgrade step instead.
func retryRevertApply(op store.AppOperation) bool {
	switch derefString(op.ErrorCode) {
	case CodePackageUnavailable, agentapi.CodeUnknownPackageVersion:
		return false
	}
	return true
}

// retryRevert re-applies the durable revert spec of a failed instance. It
// records the same version, run state, settings and credential versions as
// a new generation (the agent answers a repeated PUT of an accepted
// generation with the operation that failed) and moves the instance back to
// upgrading; the successful apply runs Setup and activates it.
func (s *Service) retryRevert(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, inst Instance, ds store.AppDesiredSpec) error {
	upgrading, err := s.transition(ctx, tx, q, actor, inst.ID, StateUpgrading, "")
	if err != nil {
		if isUpgradingConflict(err) {
			return fmt.Errorf("%w: another managed instance is upgrading", ErrInvalidState)
		}
		return err
	}
	_, err = s.recordDesiredSpec(ctx, tx, q, actor, upgrading, DesiredChange{
		RunState: ds.RunState, Purge: ds.Purge, PackageVersion: ds.PackageVersion, Reason: ReasonRevert,
	}, ds.Settings, ds.CredentialVersions)
	return err
}

// isUpgradingConflict reports the partial unique index that allows only one
// managed instance in the upgrading state (agent.md "Upgrade").
func isUpgradingConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "app_instances_upgrading_key"
}

// revertUpgrade acknowledges a rolled-back or failed upgrade with the
// running version, so the agent's blocked generation is replaced and the
// instance can run again. The revert apply runs Setup when it succeeds and
// then returns the instance to active; a failed upgrade that never changed
// the running version takes the same path.
func (s *Service) revertUpgrade(ctx context.Context, tx pgx.Tx, q *store.Queries, inst Instance, ds store.AppDesiredSpec, code string) error {
	version := derefString(inst.PackageVersion)
	if version == "" {
		_, err := s.transition(ctx, tx, q, audit.System, inst.ID, StateFailed, CodeUpgradeFailed)
		return err
	}
	if _, err := audit.Record(ctx, q, audit.Entry{
		Actor:  audit.System,
		Action: audit.AppInstanceRolledBack,
		Target: instanceTarget(inst.ID),
		Metadata: map[string]any{
			"provider": inst.Provider, "from": ds.PackageVersion, "to": version, "code": code,
		},
	}); err != nil {
		return err
	}
	_, err := s.recordDesired(ctx, tx, q, audit.System, inst, DesiredChange{
		RunState: agentapi.StateRunning, PackageVersion: version, Reason: ReasonRevert,
	})
	return err
}

// confirmCandidate is the core's confirmation loop for an agent operation
// in awaiting_confirmation: Lifecycle.CheckHealth through the candidate's
// fenced alias, then confirm on success or abort on failure (agent.md
// "Upgrade"). It never decides after the agent's confirmation deadline; the
// agent's own timeout then rolls back. A confirmation or abort the agent
// already moved past is not an error: the next poll reads its outcome.
func (s *Service) confirmCandidate(ctx context.Context, inst Instance, ds store.AppDesiredSpec, op store.AppOperation, aop *agentapi.Operation) (*agentapi.Operation, error) {
	if aop.Kind != agentapi.KindUpgrade {
		return aop, nil
	}
	timeout := cmp.Or(s.cfg.UpgradeCheckTimeout, DefaultUpgradeCheckTimeout)
	if aop.ConfirmBy != nil {
		left := aop.ConfirmBy.Sub(s.cfg.Now().UTC())
		if left <= 0 {
			return aop, nil
		}
		timeout = min(timeout, left)
	}
	view := inst.View()
	var healthErr error
	if inst.Adapter == nil {
		healthErr = ErrProviderUnavailable
	} else if lc, ok := inst.Adapter.Lifecycle(); !ok {
		healthErr = ErrNotSupported
	} else if view.Upstream = s.fencedUpstream(inst, ds.PackageVersion); view.Upstream == nil {
		healthErr = fmt.Errorf("the candidate's package version %s is not embedded", ds.PackageVersion)
	} else {
		hctx, cancel := context.WithTimeout(ctx, timeout)
		healthErr = lc.CheckHealth(hctx, s.env(inst), view)
		cancel()
	}
	confirm := healthErr == nil
	if !confirm {
		code, _, _ := provider.Classify(healthErr)
		s.cfg.Logger.WarnContext(ctx, "upgrade health check failed; rolling back",
			"app_instance", inst.ID, "generation", ds.Generation, "code", code)
	}
	next, err := s.decideUpgrade(ctx, op, aop.ID, confirm)
	if err != nil {
		return nil, err
	}
	if next != nil {
		if next.ID != aop.ID {
			return nil, fmt.Errorf("apps: the agent answered operation %s for %s", next.ID, aop.ID)
		}
		return next, nil
	}
	return aop, nil
}

// decideUpgrade confirms or aborts an awaiting upgrade. The agent is the
// authority: a decision it already passed (the timeout won, or the upgrade
// was decided) is left to the next poll. Errors other than "not awaiting"
// are retried; they never finish the operation from the core side.
func (s *Service) decideUpgrade(ctx context.Context, op store.AppOperation, id uuid.UUID, confirm bool) (*agentapi.Operation, error) {
	var (
		next *agentapi.Operation
		err  error
	)
	if confirm {
		next, err = s.cfg.Agent.Confirm(ctx, id)
	} else {
		next, err = s.cfg.Agent.Abort(ctx, id)
	}
	if err == nil {
		return next, nil
	}
	var pe *agentclient.ProblemError
	if errors.As(err, &pe) && (pe.Code == agentapi.CodeNotAwaitingConfirmation || pe.Code == agentapi.CodeNotFound) {
		s.cfg.Logger.InfoContext(ctx, "the agent's upgrade is already decided", "agent_operation", id, "confirmed", confirm)
		return nil, nil
	}
	code, kind := classifyAgentError(err)
	if kind == agentWaiting {
		return nil, s.waitForAgent(ctx, op, code, err)
	}
	action := "abort"
	if confirm {
		action = "confirm"
	}
	return nil, fmt.Errorf("apps: %s agent upgrade (%s): %w", action, code, err)
}

// fencedUpstream is the candidate's first route on the edge network while
// it is fenced: the alias Traefik cannot route to (agent.md "Write fence").
func (s *Service) fencedUpstream(inst Instance, version string) *url.URL {
	pkg, ok := s.pkg(inst.PackageID, version)
	if !ok || len(pkg.Manifest.Routes) == 0 {
		return nil
	}
	r := pkg.Manifest.Routes[0]
	return &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(packages.ServiceAlias(inst.Key, r.Service, true), strconv.Itoa(r.Port)),
	}
}

// finishSetup activates an instance whose setup (or upgrade) finished, or
// fails it with setup_failed. generation > 0 fences the result: a setup
// that a newer desired generation superseded changes nothing.
func (s *Service) finishSetup(ctx context.Context, id uuid.UUID, generation int64, setupErr error) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if generation > 0 {
			if inst.DesiredGeneration != generation {
				return nil
			}
		} else if inst.State != StateInstalling || inst.DesiredGeneration != inst.ObservedGeneration {
			// A legacy setup job (no generation) may only finish an install
			// that is still the applied generation; it never activates an
			// upgrading instance.
			return nil
		}
		if inst.State != StateInstalling && inst.State != StateUpgrading {
			return nil
		}
		if inst.DesiredGeneration != inst.ObservedGeneration {
			// The current generation was not applied: a setup must neither
			// activate nor fail the instance while it is pending.
			return nil
		}
		if setupErr != nil {
			_, err := s.transition(ctx, tx, q, audit.System, id, StateFailed, CodeSetupFailed)
			return err
		}
		_, err = s.transition(ctx, tx, q, audit.System, id, StateActive, "")
		return err
	})
}
