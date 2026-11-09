package apps

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// syncOwnedCodes are the health codes the sync sets and clears. Other codes
// (setup_failed, webhook_deleted, ...) are reasons another path set; the
// sync leaves those instances' health and state alone.
var syncOwnedCodes = []string{CodeAgentUnhealthy, CodeAgentInstanceMissing, CodeHealthCheckFailed, CodeAgentUnavailable}

// RunAgentSync works orglane.apps.agent_sync (docs/specs/agent.md "Sync"),
// which runs every Config.SyncInterval, at core start and when the agent's
// boot ID changes (WatchAgent):
//
//  1. every unfinished push has an agent_apply job (one discarded after its
//     attempts is queued again);
//  2. the agent's statuses go into app_instances (observed generation,
//     running version);
//  3. a generation the agent lost (its accepted generation is behind one
//     whose operation succeeded) is pushed again as a resync generation;
//  4. health: the worse of the containers' health and Lifecycle.CheckHealth
//     for active and degraded instances, moving them between the two.
//
// When the agent cannot be reached, the health of instances the sync could
// not check for two intervals becomes unknown.
func (s *Service) RunAgentSync(ctx context.Context) error {
	q := store.New(s.pool)
	// Read the database before the agent: an operation that finished
	// before this snapshot was accepted by the agent before its statuses
	// are read, so a lower accepted generation means the agent lost it.
	rows, err := q.ListAppInstances(ctx)
	if err != nil {
		return fmt.Errorf("apps: list instances: %w", err)
	}
	unfinished, err := q.ListUnfinishedAppPushOperations(ctx)
	if err != nil {
		return fmt.Errorf("apps: list unfinished operations: %w", err)
	}
	pushing := map[uuid.UUID]bool{}
	if len(unfinished) > 0 {
		err := s.inTx(ctx, func(tx pgx.Tx, _ *store.Queries) error {
			for _, u := range unfinished {
				pushing[u.InstanceID] = true
				if err := s.enqueue(ctx, tx, AgentApplyArgs{InstanceID: u.InstanceID, Generation: u.Generation}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if s.cfg.Agent == nil {
		return s.markStale(ctx)
	}
	h, err := s.cfg.Agent.Health(ctx)
	if err != nil {
		// Nothing is planned while the agent is down: moving an instance
		// to upgrading would put its routes into maintenance even though
		// its containers are fine. The next reachable sync plans.
		s.cfg.Logger.WarnContext(ctx, "agent sync: the agent is not reachable", "error", err)
		return s.markStale(ctx)
	}
	s.noteBoot(h.BootID)
	// Drive one attempt of the unfinished pushes of an upgrading instance
	// here, independently of River's stuck-job rescue (one hour by default):
	// a confirmation the core was about to send when it crashed is recovered
	// at the next sync instead of being lost to the agent's confirm_timeout.
	// Only upgrading instances need this recovery (an upgrade is the only
	// operation that awaits confirmation, and the one-at-a-time rule allows
	// one of them), so the sync runs at most one candidate health check
	// bounded by UpgradeCheckTimeout. Every other push stays with its
	// agent_apply job and River's rescue. The normal job remains the primary
	// poller; every step is idempotent.
	s.recoverUpgradingApplies(ctx, unfinished, rows)
	// Automatic package upgrades after an Orglane release that embeds newer
	// package versions (agent.md "Upgrade"). Only once the agent answers,
	// and one managed instance at a time.
	if err := s.planUpgrades(ctx, rows, pushing); err != nil {
		s.cfg.Logger.WarnContext(ctx, "agent sync: plan upgrades", "error", err)
	}
	statuses, err := s.cfg.Agent.Instances(ctx)
	if err != nil {
		s.cfg.Logger.WarnContext(ctx, "agent sync: list instances", "error", err)
		return s.markStale(ctx)
	}
	byKey := make(map[string]*agentapi.InstanceStatus, len(statuses))
	for i := range statuses {
		byKey[statuses[i].Key] = &statuses[i]
	}
	var errs []error
	for _, row := range rows {
		if row.Management != string(provider.Managed) || row.State == StateRemoved {
			continue
		}
		if err := s.syncInstance(ctx, row, byKey[row.Key], pushing[row.ID]); err != nil {
			errs = append(errs, fmt.Errorf("apps: sync %s: %w", row.Key, err))
		}
	}
	return errors.Join(errs...)
}

// recoverUpgradingApplies runs one agent_apply attempt of the unfinished
// pushes of instances in upgrading. It is safe beside the normal job: the
// operation row says where the push is, the agent answers a repeated PUT
// with the same operation, and a confirmation or finish is idempotent and
// conditional on the unfinished row. The sync therefore recovers a
// confirmation after a core crash even while River still considers the
// crashed job running.
//
// Only upgrading instances are recovered: an upgrade is the one operation
// that awaits confirmation, and the one-at-a-time rule allows a single
// upgrading instance, so recovery costs at most one health check. Recovering
// every unfinished push would multiply UpgradeCheckTimeout by the number of
// installs, removes and applies waiting for their jobs.
func (s *Service) recoverUpgradingApplies(ctx context.Context, unfinished []store.ListUnfinishedAppPushOperationsRow, rows []store.AppInstance) {
	var upgrading map[uuid.UUID]bool
	for _, row := range rows {
		if row.State == StateUpgrading {
			if upgrading == nil {
				upgrading = make(map[uuid.UUID]bool, 1)
			}
			upgrading[row.ID] = true
		}
	}
	if len(upgrading) == 0 {
		return
	}
	for _, u := range unfinished {
		if !upgrading[u.InstanceID] {
			continue
		}
		err := s.RunAgentApply(ctx, AgentApplyArgs{InstanceID: u.InstanceID, Generation: u.Generation})
		var snooze *river.JobSnoozeError
		var cancel *river.JobCancelError
		if err != nil && !errors.As(err, &snooze) && !errors.As(err, &cancel) {
			s.cfg.Logger.WarnContext(ctx, "agent sync: recover an unfinished apply",
				"app_instance", u.InstanceID, "generation", u.Generation, "error", err)
		}
	}
}

func (s *Service) syncInstance(ctx context.Context, row store.AppInstance, st *agentapi.InstanceStatus, pushing bool) error {
	if st != nil && st.InstanceID != row.ID {
		// Keys are reserved while data is retained, so this is another
		// installation's directory: never touch it.
		s.cfg.Logger.WarnContext(ctx, "agent sync: the agent's directory belongs to another instance", "key", row.Key)
		return nil
	}
	q := store.New(s.pool)
	// observed is the generation the state consequences below (recovery,
	// health) are derived from; each of their writes is conditional on it
	// and on the state it expects.
	observed := row.ObservedGeneration
	stale := false
	if st != nil {
		var version *string
		if st.Package != nil {
			version = &st.Package.Version
		}
		n, err := q.SetAppInstanceObservation(ctx, store.SetAppInstanceObservationParams{ID: row.ID, ObservedGeneration: st.ObservedGeneration, PackageVersion: version})
		if err != nil {
			return fmt.Errorf("record observation: %w", err)
		}
		// Zero rows: the core recorded a newer generation as applied than
		// this status reports, either because a newer generation finished
		// after the status was read, or because the agent fell back to an
		// older state. Either way no state may be derived from it.
		stale = n == 0
		observed = st.ObservedGeneration
	}
	// The resync is decided on its own, from what the agent accepted, so a
	// stale observation does not hide a real loss: the status was read after
	// the database snapshot, and resyncLost re-checks under the instance lock
	// that the desired generation is unchanged and its operation succeeded,
	// so a delayed status cannot cause a spurious push either.
	var accepted int64
	if st != nil {
		accepted = st.AcceptedGeneration
	}
	if row.DesiredGeneration > accepted && !pushing {
		if err := s.resyncLost(ctx, row.ID, row.DesiredGeneration); err != nil {
			return err
		}
	}
	if stale {
		s.cfg.Logger.DebugContext(ctx, "agent sync: status older than the recorded generation; no state derived", "key", row.Key, "observed_generation", st.ObservedGeneration)
		return nil
	}
	if row.State == StateFailed && derefString(row.HealthCode) == CodeInstallFailed && st != nil &&
		st.Phase == agentapi.PhaseRunning && st.ObservedGeneration >= row.DesiredGeneration {
		// The agent's own retries of the failed install succeeded.
		return s.recoverInstall(ctx, row.ID, row.DesiredGeneration)
	}
	if row.State != StateActive && row.State != StateDegraded {
		return nil
	}
	inst, err := s.instance(row)
	if err != nil {
		return err
	}
	return s.syncHealth(ctx, inst, st, observed)
}

// resyncLost pushes the current desired state again as a new generation
// when the agent no longer has a generation it once applied.
func (s *Service) resyncLost(ctx context.Context, id uuid.UUID, gen int64) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if inst.DesiredGeneration != gen || inst.State == StateRemoved {
			return nil // changed since the snapshot; its own job pushes it
		}
		op, err := q.GetAppPushOperation(ctx, store.GetAppPushOperationParams{InstanceID: id, Generation: &gen})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("load operation: %w", err)
		}
		// Unfinished: its job pushes it. Failed or rolled back: the agent
		// never applied it, and pushing it again would fail the same way.
		if op.State != agentapi.OpSucceeded {
			return nil
		}
		ds, err := q.GetAppDesiredSpec(ctx, store.GetAppDesiredSpecParams{InstanceID: id, Generation: gen})
		if err != nil {
			return fmt.Errorf("load desired spec: %w", err)
		}
		if ds.RunState == agentapi.StateAbsent {
			return nil // removed: nothing to bring back
		}
		s.cfg.Logger.WarnContext(ctx, "the agent lost an applied generation; pushing it again", "app_instance", id, "generation", gen)
		// Keep a revert a revert: its version may no longer be embedded, and
		// only the revert reason may build the minimal acknowledgment spec.
		reason := ReasonResync
		if ds.Reason == ReasonRevert {
			reason = ReasonRevert
		}
		_, err = s.recordDesired(ctx, tx, q, audit.System, inst, DesiredChange{
			RunState: ds.RunState, PackageVersion: ds.PackageVersion, Reason: reason,
		})
		return err
	})
}

// recoverInstall moves an install_failed instance whose generation the
// agent did apply back to installing and runs Setup. It is conditional on
// the generation that justified it: desired is still that generation and
// the agent's observation of it is recorded; a newer generation recorded
// meanwhile (which may have failed) is left alone.
func (s *Service) recoverInstall(ctx context.Context, id uuid.UUID, desired int64) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if inst.State != StateFailed || derefString(inst.HealthCode) != CodeInstallFailed ||
			inst.DesiredGeneration != desired || inst.ObservedGeneration < desired {
			return nil // changed since the status was read: nothing to recover
		}
		if _, err := s.transition(ctx, tx, q, audit.System, id, StateInstalling, ""); err != nil {
			return err
		}
		return s.enqueueSetup(ctx, tx, id, desired)
	})
}

// syncHealth records the worse of the containers' and the provider's health
// of an active or degraded instance.
//
// observed is the generation the status describes: the health is recorded
// only while it is still the observed generation.
func (s *Service) syncHealth(ctx context.Context, inst Instance, st *agentapi.InstanceStatus, observed int64) error {
	health, code := containerHealth(st)
	if health == HealthHealthy && inst.Adapter != nil {
		if lc, ok := inst.Adapter.Lifecycle(); ok {
			if err := lc.CheckHealth(ctx, s.env(inst), inst.View()); err != nil {
				pcode, _, _ := provider.Classify(err)
				s.env(inst).Logger().WarnContext(ctx, "health check failed", "code", pcode)
				health, code = HealthUnhealthy, CodeHealthCheckFailed
			}
		}
	}
	switch {
	case inst.State == StateActive && health == HealthUnhealthy:
		return s.transitionFrom(ctx, inst.ID, observed, StateActive, StateDegraded, code)
	case inst.State == StateDegraded && health == HealthHealthy:
		return s.transitionFrom(ctx, inst.ID, observed, StateDegraded, StateActive, "")
	}
	if inst.State == StateDegraded && code == "" {
		code = derefString(inst.HealthCode) // keep the reason it is degraded
	}
	n, err := store.New(s.pool).SetAppInstanceHealth(ctx, store.SetAppInstanceHealthParams{
		ID: inst.ID, Health: health, HealthCode: nilIfEmpty(code), State: inst.State,
		ObservedGeneration: observed, OwnedCodes: syncOwnedCodes,
	})
	if err != nil {
		return fmt.Errorf("record health: %w", err)
	} else if n == 0 {
		// The state, the observed generation or the reason changed since
		// the status was read: this health is no longer about the instance.
		s.cfg.Logger.DebugContext(ctx, "agent sync: stale health ignored", "app_instance", inst.ID)
	}
	return nil
}

// transitionFrom transitions an instance that is still in from, at the
// observed generation the health was derived from, with no health code or
// one the sync owns. Otherwise it changes nothing.
func (s *Service) transitionFrom(ctx context.Context, id uuid.UUID, observed int64, from, to, code string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		cur, err := s.loadInstance(ctx, q, id, true)
		if err != nil {
			return err
		}
		if cur.State != from || cur.ObservedGeneration != observed ||
			(cur.HealthCode != nil && !slices.Contains(syncOwnedCodes, *cur.HealthCode)) {
			return nil // changed since the status was read
		}
		_, err = s.transition(ctx, tx, q, audit.System, id, to, code)
		return err
	})
}

// containerHealth is the agent's view: every service running and not
// unhealthy. Phases in between (installing, upgrading, ...) are unknown.
func containerHealth(st *agentapi.InstanceStatus) (health, code string) {
	if st == nil {
		return HealthUnknown, CodeAgentInstanceMissing
	}
	switch st.Phase {
	case agentapi.PhaseRunning:
		for _, svc := range st.Services {
			if svc.State != "running" || svc.Health == "unhealthy" {
				return HealthUnhealthy, CodeAgentUnhealthy
			}
		}
		return HealthHealthy, ""
	case agentapi.PhaseDegraded, agentapi.PhaseFailed:
		return HealthUnhealthy, CodeAgentUnhealthy
	}
	return HealthUnknown, ""
}

// markStale makes health unknown where the sync missed two intervals.
func (s *Service) markStale(ctx context.Context) error {
	_, err := store.New(s.pool).MarkStaleManagedHealth(ctx, store.MarkStaleManagedHealthParams{
		Before: s.cfg.Now().Add(-2 * s.cfg.SyncInterval), OwnedCodes: syncOwnedCodes,
	})
	if err != nil {
		return fmt.Errorf("apps: mark stale health: %w", err)
	}
	return nil
}

// noteBoot records the agent's boot ID and reports whether it changed from
// a known one.
func (s *Service) noteBoot(id uuid.UUID) bool {
	prev, _ := s.bootID.Swap(id).(uuid.UUID)
	return prev != uuid.Nil && prev != id
}
