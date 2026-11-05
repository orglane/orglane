package apps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// RunAgentApply works one attempt of orglane.apps.agent_apply: it pushes
// the generation to the agent if the operation has no agent operation yet,
// else polls that one, and mirrors it into app_operations. It snoozes
// between polls (snoozes are not attempts) and returns an error, retried
// with backoff, while the agent is unavailable.
//
// Every step is safe to repeat (job retries, a core restart between any two
// steps): the operation row says where the push is, the agent answers a
// repeated PUT of an accepted generation with the operation that applies it,
// and finishing is conditional on the row being unfinished.
func (s *Service) RunAgentApply(ctx context.Context, args AgentApplyArgs) error {
	q := store.New(s.pool)
	op, err := q.GetAppPushOperation(ctx, store.GetAppPushOperationParams{InstanceID: args.InstanceID, Generation: &args.Generation})
	if errors.Is(err, pgx.ErrNoRows) {
		return river.JobCancel(fmt.Errorf("%w: operation of generation %d", ErrNotFound, args.Generation))
	} else if err != nil {
		return fmt.Errorf("apps: load operation: %w", err)
	}
	if op.FinishedAt != nil {
		return nil
	}
	inst, err := s.loadInstance(ctx, q, args.InstanceID, false)
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	} else if err != nil {
		return err
	}
	ds, err := q.GetAppDesiredSpec(ctx, store.GetAppDesiredSpecParams{InstanceID: args.InstanceID, Generation: args.Generation})
	if err != nil {
		return fmt.Errorf("apps: load desired spec: %w", err)
	}
	if op.AgentOperationID == nil {
		return s.push(ctx, inst, ds, op)
	}
	return s.poll(ctx, inst, ds, op)
}

func (s *Service) push(ctx context.Context, inst Instance, ds store.AppDesiredSpec, op store.AppOperation) error {
	if ds.Generation < inst.DesiredGeneration {
		// Replaced before it reached the agent, which applies only the
		// newest generation anyway.
		return s.finishOp(ctx, ds, op, nil, agentapi.OpFailed, agentapi.CodeSuperseded)
	}
	agent := s.cfg.Agent
	if agent == nil {
		return s.waitForAgent(ctx, op, CodeAgentUnavailable, errors.New("managed services are not configured (agent.socket, agent.secret)"))
	}
	// Ask first, so a missing agent waits before the spec with its secrets
	// is assembled.
	if _, err := agent.Health(ctx); err != nil {
		return s.agentError(ctx, ds, op, err)
	}
	// The agent answers: whatever fails next, the push no longer waits for it.
	if op.ErrorCode != nil {
		if _, err := store.New(s.pool).SetAppOperationWaiting(ctx, store.SetAppOperationWaitingParams{ID: op.ID}); err != nil {
			return fmt.Errorf("apps: clear waiting operation: %w", err)
		}
	}
	spec, err := s.buildSpec(ctx, inst, ds)
	var se *specError
	if errors.As(err, &se) {
		return s.finishOp(ctx, ds, op, nil, agentapi.OpFailed, se.code)
	} else if err != nil {
		return err
	}
	aop, _, err := agent.Apply(ctx, inst.Key, spec)
	if err != nil {
		var pe *agentclient.ProblemError
		if errors.As(err, &pe) && pe.Code == agentapi.CodeGenerationConflict {
			return s.resyncConflict(ctx, ds, op)
		}
		return s.agentError(ctx, ds, op, err)
	}
	return s.mirror(ctx, ds, op, aop)
}

func (s *Service) poll(ctx context.Context, inst Instance, ds store.AppDesiredSpec, op store.AppOperation) error {
	agent := s.cfg.Agent
	if agent == nil {
		return s.waitForAgent(ctx, op, CodeAgentUnavailable, errors.New("managed services are not configured"))
	}
	aop, err := agent.Operation(ctx, *op.AgentOperationID)
	var pe *agentclient.ProblemError
	if errors.As(err, &pe) && pe.Code == agentapi.CodeNotFound {
		// The agent lost the operation (its state directory was lost, or
		// the operation expired). Push again: a still accepted generation
		// answers with the operation that applies it, a lost one is
		// accepted anew.
		s.cfg.Logger.WarnContext(ctx, "the agent no longer knows an operation; pushing again", "app_instance", inst.ID, "generation", ds.Generation)
		if _, err := store.New(s.pool).MirrorAppOperation(ctx, store.MirrorAppOperationParams{ID: op.ID, State: agentapi.OpQueued}); err != nil {
			return fmt.Errorf("apps: reset operation: %w", err)
		}
		return river.JobSnooze(0)
	} else if err != nil {
		if code, kind := classifyAgentError(err); kind == agentRefused {
			// A 4xx answer to a poll is no verdict on the operation.
			return fmt.Errorf("apps: poll agent operation (%s): %w", code, err)
		}
		return s.agentError(ctx, ds, op, err)
	}
	if aop.ID != *op.AgentOperationID {
		return fmt.Errorf("apps: the agent answered operation %s for %s", aop.ID, *op.AgentOperationID)
	}
	return s.mirror(ctx, ds, op, aop)
}

// mirror records an agent operation: progress with a snooze until the next
// poll, or its end with its consequences.
func (s *Service) mirror(ctx context.Context, ds store.AppDesiredSpec, op store.AppOperation, aop *agentapi.Operation) error {
	if !slices.Contains(operationStates, aop.State) {
		return fmt.Errorf("apps: the agent reported an unknown operation state %q", aop.State)
	}
	if aop.Terminal() {
		return s.finishOp(ctx, ds, op, aop, aop.State, aop.ErrorCode)
	}
	// Part 2 of #75 hooks in here: an upgrade in awaiting_confirmation is
	// checked with Lifecycle.CheckHealth through the fenced upstream and
	// confirmed or aborted (agentclient Confirm and Abort); until then it is
	// polled like any other unfinished operation and rolls back after the
	// package's confirm_timeout.
	if _, err := store.New(s.pool).MirrorAppOperation(ctx, store.MirrorAppOperationParams{
		ID: op.ID, AgentOperationID: &aop.ID, State: aop.State, Phase: nilIfEmpty(aop.Phase),
	}); err != nil {
		return fmt.Errorf("apps: mirror operation: %w", err)
	}
	return river.JobSnooze(s.cfg.AgentPollInterval)
}

var operationStates = []string{
	agentapi.OpQueued, agentapi.OpRunning, agentapi.OpAwaitingConfirmation,
	agentapi.OpSucceeded, agentapi.OpFailed, agentapi.OpRolledBack,
}

// finishOp ends an operation (aop is nil when the agent never saw it) and
// applies its consequences in the same transaction. A second finish of the
// same operation changes nothing.
func (s *Service) finishOp(ctx context.Context, ds store.AppDesiredSpec, op store.AppOperation, aop *agentapi.Operation, state, code string) error {
	finished := s.cfg.Now().UTC()
	params := store.MirrorAppOperationParams{ID: op.ID, State: state, ErrorCode: nilIfEmpty(code), FinishedAt: &finished}
	if aop != nil {
		params.AgentOperationID, params.Phase = &aop.ID, nilIfEmpty(aop.Phase)
		if aop.FinishedAt != nil {
			params.FinishedAt = aop.FinishedAt
		}
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		// Instance first, as every transition locks it: one lock order.
		inst, err := s.loadInstance(ctx, q, op.InstanceID, true)
		if err != nil {
			return err
		}
		n, err := q.MirrorAppOperation(ctx, params)
		if err != nil {
			return fmt.Errorf("apps: finish operation: %w", err)
		} else if n == 0 {
			return nil
		}
		return s.operationFinished(ctx, tx, q, inst, ds, state, code)
	})
}

// operationFinished applies what the end of a generation's operation means
// for its instance. Only the instance's current generation changes its state.
//
// Extension points: part 2 of #75 handles upgrades here (a rolled_back
// operation records the revert spec and re-runs Setup; a succeeded upgrade is
// audited as app_instance.upgraded), and the removal flow (#61) moves a
// removing instance to removed when its absent generation succeeded.
func (s *Service) operationFinished(ctx context.Context, tx pgx.Tx, q *store.Queries, inst Instance, ds store.AppDesiredSpec, state, code string) error {
	current := ds.Generation == inst.DesiredGeneration
	switch state {
	case agentapi.OpSucceeded:
		var version *string
		if ds.RunState != agentapi.StateAbsent {
			version = &ds.PackageVersion
		}
		if err := q.SetAppInstanceApplied(ctx, store.SetAppInstanceAppliedParams{ID: inst.ID, Generation: ds.Generation, PackageVersion: version}); err != nil {
			return fmt.Errorf("apps: record applied generation: %w", err)
		}
		if current && inst.State == StateInstalling && ds.RunState == agentapi.StateRunning {
			// Installed: Setup runs and activates the instance (step 4).
			return s.enqueue(ctx, tx, SetupArgs{InstanceID: inst.ID})
		}
	case agentapi.OpFailed:
		if current && inst.State == StateInstalling && code != agentapi.CodeSuperseded {
			_, err := s.transition(ctx, tx, q, audit.System, inst.ID, StateFailed, CodeInstallFailed)
			return err
		}
	}
	return nil
}

// resyncConflict handles generation_conflict: the agent accepted this
// generation with another spec (the configuration that builds hosts or URLs
// changed between pushes). The operation ends and, if the generation is
// still current, the same desired state is recorded as a new generation.
func (s *Service) resyncConflict(ctx context.Context, ds store.AppDesiredSpec, op store.AppOperation) error {
	finished := s.cfg.Now().UTC()
	code := agentapi.CodeGenerationConflict
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, op.InstanceID, true)
		if err != nil {
			return err
		}
		n, err := q.MirrorAppOperation(ctx, store.MirrorAppOperationParams{ID: op.ID, State: agentapi.OpFailed, ErrorCode: &code, FinishedAt: &finished})
		if err != nil {
			return fmt.Errorf("apps: finish operation: %w", err)
		}
		if n == 0 || ds.Generation != inst.DesiredGeneration {
			return nil
		}
		_, err = s.recordDesired(ctx, tx, q, audit.System, inst, DesiredChange{
			RunState: ds.RunState, Purge: ds.Purge, PackageVersion: ds.PackageVersion, Reason: ReasonResync,
		})
		return err
	})
}

// agentError handles a failed agent call: wait for the agent (retry with
// backoff, the UI shows "waiting for the agent"), retry a transient
// failure, or end the operation with the agent's refusal.
func (s *Service) agentError(ctx context.Context, ds store.AppDesiredSpec, op store.AppOperation, err error) error {
	code, kind := classifyAgentError(err)
	switch kind {
	case agentWaiting:
		return s.waitForAgent(ctx, op, code, err)
	case agentRefused:
		return s.finishOp(ctx, ds, op, nil, agentapi.OpFailed, code)
	}
	return fmt.Errorf("apps: agent (%s): %w", code, err)
}

// waitForAgent notes why the operation waits and returns an error, so River
// retries the job with backoff.
func (s *Service) waitForAgent(ctx context.Context, op store.AppOperation, code string, cause error) error {
	if _, err := store.New(s.pool).SetAppOperationWaiting(ctx, store.SetAppOperationWaitingParams{ID: op.ID, ErrorCode: &code}); err != nil {
		return fmt.Errorf("apps: note waiting operation: %w", err)
	}
	return fmt.Errorf("apps: waiting for the agent (%s): %w", code, cause)
}

type agentErrorKind int

const (
	agentRetry   agentErrorKind = iota // transient: retry
	agentWaiting                       // the agent is not usable: wait for it
	agentRefused                       // the agent refused the request: final
)

// classifyAgentError maps agentclient errors to a stable code and what to
// do about them.
func classifyAgentError(err error) (string, agentErrorKind) {
	var pe *agentclient.ProblemError
	switch {
	case errors.Is(err, agentclient.ErrUnavailable):
		return CodeAgentUnavailable, agentWaiting
	case errors.Is(err, agentclient.ErrUntrusted):
		return CodeAgentUntrusted, agentWaiting
	case errors.Is(err, agentclient.ErrVersionMismatch), errors.Is(err, agentclient.ErrProtocol):
		return CodeAgentVersionMismatch, agentWaiting
	case errors.As(err, &pe):
		if pe.Status >= http.StatusInternalServerError || pe.Code == agentapi.CodeAgentBusy {
			return pe.Code, agentRetry
		}
		return pe.Code, agentRefused
	}
	return "", agentRetry
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// QueueAgentSync queues orglane.apps.agent_sync (a follow-up when one is
// running, so a trigger during a run is not lost).
func (s *Service) QueueAgentSync(ctx context.Context) error {
	return s.inTx(ctx, func(tx pgx.Tx, _ *store.Queries) error {
		return s.enqueueOnce(ctx, tx, AgentSyncArgs{}, AgentSyncArgs{FollowUp: true}, nil)
	})
}

// WatchAgent asks the agent for its boot ID every Config.AgentWatchInterval
// until ctx ends, and queues agent_sync when it changed: the agent restarted,
// perhaps without its state. The first boot ID seen is only noted (the sync
// at core start covers it).
func (s *Service) WatchAgent(ctx context.Context) {
	if s.cfg.Agent == nil {
		return
	}
	t := time.NewTicker(s.cfg.AgentWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.checkBoot(ctx)
		}
	}
}

func (s *Service) checkBoot(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	h, err := s.cfg.Agent.Health(ctx)
	if err != nil {
		return
	}
	if s.noteBoot(h.BootID) {
		s.cfg.Logger.InfoContext(ctx, "the agent restarted; syncing", "boot_id", h.BootID)
		if err := s.QueueAgentSync(ctx); err != nil {
			s.cfg.Logger.WarnContext(ctx, "queue agent sync", "error", err)
		}
	}
}
