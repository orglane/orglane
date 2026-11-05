package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
)

// Package upgrades (docs/specs/agent.md "Upgrade", ADR 0019 §6):
//
//	preflight → pulling → stopping → snapshotting → switching (fenced) →
//	migrating → verifying → awaiting_confirmation → committing (unfenced)
//
// Any failure after the snapshot, an abort or a missing confirmation rolls
// back in two journaled phases: rolling_back restores the snapshot while
// everything stays fenced, restored exposes the restored version and never
// restores data again. The journal (upgrade.json) is synced before each
// phase; a restarted agent never resumes an upgrade forward
// (recoverJournal).

// defaultConfirmTimeout applies when a manifest sets no confirm_timeout.
const defaultConfirmTimeout = 10 * time.Minute

// errCrashed is returned by checkpoint when the test crash hook fires.
var errCrashed = errors.New("agent: test crash")

// checkpoint is the test hook ORGLANE_AGENT_TEST_CRASH_AT=<name> (only in
// builds with the agenttest tag, and Options.crashAt in tests): at the
// named point the agent stops at once, as if the process died there.
// Names are the journal phases (right after the phase's journal entry is
// durable) and "restored:exposed" (after the restored version was exposed,
// before the rollback is recorded).
func (a *Agent) checkpoint(name string) error {
	if a.crashAt == "" || a.crashAt != name {
		return nil
	}
	if a.crashed.CompareAndSwap(false, true) {
		a.log.Warn("test crash", "at", name)
		a.mu.Lock()
		stop := a.stop
		a.mu.Unlock()
		if stop != nil {
			stop()
		}
	}
	return errCrashed
}

// interrupted reports whether a step ended because the agent stops
// (shutdown or test crash): the journal stays as it is and the next start
// recovers from it.
func interrupted(ctx context.Context, err error) bool {
	return err != nil && (errors.Is(err, errCrashed) || ctx.Err() != nil)
}

// isUpgrade reports whether applying the desired spec changes the package
// version (state absent is a plain removal).
func isUpgrade(st *instanceState) bool {
	return st.current != nil && st.desired != nil && st.desired.State != agentapi.StateAbsent &&
		st.desired.Package.Version != st.current.Package.Version
}

// enter records a journal phase durably, then shows it on the operation.
func (a *Agent) enter(in *instance, j *journal, phase string) error {
	j.Phase = phase
	in.files.Lock()
	err := writeJournal(a.instanceDir(in.key), j)
	in.files.Unlock()
	if err != nil {
		return failed(agentapi.CodeInternal, err)
	}
	a.opPhase(j.Operation, phase)
	return a.checkpoint(phase)
}

func (a *Agent) opPhase(id uuid.UUID, phase string) {
	state := agentapi.OpRunning
	if phase == agentapi.PhaseAwaitingConfirmation {
		state = agentapi.OpAwaitingConfirmation
	}
	if _, err := a.ops.update(id, func(o *agentapi.Operation) {
		if !o.Terminal() {
			o.State, o.Phase = state, phase
		}
	}); err != nil {
		a.log.Error("update operation", "operation", id, "error", err)
	}
}

// finalize records the end of an upgrade or restore, in this order:
// blocked.json (with the generation blocked if block), current.json (if
// current is set), the operation's result, and only then the journal is
// deleted. A crash in between repeats the journal's phase, which records
// the same result again.
func (a *Agent) finalize(in *instance, j *journal, current *agentapi.Spec, block bool, state, code, rule string) error {
	dir := a.instanceDir(in.key)
	in.files.Lock()
	st, err := loadState(dir)
	if err == nil {
		b := st.blocked
		b.Failures = nil
		if block && !b.has(j.ToGeneration) {
			b.Generations = append(b.Generations, blockedGeneration{Generation: j.ToGeneration, ErrorCode: code, At: a.now().UTC()})
		}
		err = writeJSON(filepath.Join(dir, blockedFile), b)
		if err == nil && current != nil && (st.desired == nil || st.desired.Generation < current.Generation) {
			// A restore whose request was cut off after its journal.
			err = writeSpec(filepath.Join(dir, desiredFile), current)
		}
		if err == nil && current != nil {
			err = writeSpec(filepath.Join(dir, currentFile), current)
		}
	}
	in.files.Unlock()
	if err != nil {
		return err
	}
	if _, err := a.ops.update(j.Operation, func(o *agentapi.Operation) {
		if o.Terminal() {
			return
		}
		o.State, o.Phase, o.ErrorCode, o.Rule = state, "", code, rule
		if j.SnapshotID != uuid.Nil {
			o.SnapshotID = j.SnapshotID.String()
		}
	}); err != nil {
		return fmt.Errorf("record operation result: %w", err)
	}
	in.files.Lock()
	defer in.files.Unlock()
	return removeJournal(dir)
}

// upgrade runs the upgrade flow from the running spec to the desired one.
func (a *Agent) upgrade(ctx context.Context, in *instance, st *instanceState, op agentapi.Operation) string {
	from, to := st.current, st.desired
	ref := from.Package
	j := &journal{
		Operation: op.ID, Kind: agentapi.KindUpgrade, From: &ref, To: to.Package,
		FromGeneration: from.Generation, ToGeneration: to.Generation, Spec: to,
	}
	a.log.Info("upgrade started", "instance", in.key, "from", from.Package.String(), "to", to.Package.String(),
		"generation", to.Generation, "operation", op.ID)
	code, err := a.upgradeForward(ctx, in, j, from)
	if err == nil {
		return code
	}
	if interrupted(ctx, err) {
		return ""
	}
	code, rule := errorCode(err)
	a.log.Error("upgrade failed", "instance", in.key, "phase", j.Phase, "code", code, "rule", rule, "error", err)
	switch j.Phase {
	case agentapi.PhasePreflight, agentapi.PhasePulling:
		// Nothing had changed.
		return a.endFailed(ctx, in, j, code, rule)
	case agentapi.PhaseStopping, agentapi.PhaseSnapshotting:
		a.discardSnapshotAndRestart(ctx, in, from)
		return a.endFailed(ctx, in, j, code, rule)
	default:
		j.ErrorCode = code
		return a.rollback(ctx, in, j)
	}
}

func (a *Agent) endFailed(ctx context.Context, in *instance, j *journal, code, rule string) string {
	if ctx.Err() != nil {
		return ""
	}
	// The result goes into the journal first: finalize blocks the
	// generation with it before it records the operation, and a crash in
	// between must recover the same result, not "interrupted".
	if j.ErrorCode != code || j.Rule != rule {
		j.ErrorCode, j.Rule = code, rule
		in.files.Lock()
		err := writeJournal(a.instanceDir(in.key), j)
		in.files.Unlock()
		if err != nil {
			a.log.Error("record the failed upgrade", "instance", in.key, "error", err)
			return agentapi.CodeInternal
		}
	}
	if err := a.finalize(in, j, nil, true, agentapi.OpFailed, code, rule); err != nil {
		a.log.Error("record the failed upgrade", "instance", in.key, "error", err)
		return agentapi.CodeInternal
	}
	return code
}

// discardSnapshotAndRestart undoes an upgrade that stopped before the
// switch: the partial snapshot goes and the running version starts again
// (its files were not changed).
func (a *Agent) discardSnapshotAndRestart(ctx context.Context, in *instance, from *agentapi.Spec) {
	if err := a.removePartialSnapshots(in.key, uuid.Nil); err != nil {
		a.log.Warn("remove the partial snapshot", "instance", in.key, "error", err)
	}
	if from == nil || from.State != agentapi.StateRunning {
		return
	}
	c := a.compose(in.key, from.SecretValues())
	if err := a.upWait(ctx, c, a.startTimeout(from)); err != nil {
		a.log.Error("start the running version again", "instance", in.key, "error", err)
	}
}

func secretsOf(specs ...*agentapi.Spec) []string {
	var out []string
	for _, s := range specs {
		if s != nil {
			out = append(out, s.SecretValues()...)
		}
	}
	return out
}

func (a *Agent) startTimeout(spec *agentapi.Spec) time.Duration {
	if pkg, ok := a.registry.Get(spec.Package); ok {
		return pkg.Manifest.Health.StartTimeout
	}
	return defaultStartTimeout
}

// upWait starts a project and waits for its healthchecks. A service that
// turned unhealthy, or the timeout, is health_timeout.
func (a *Agent) upWait(ctx context.Context, c compose, timeout time.Duration) error {
	upCtx, cancel := context.WithTimeout(ctx, timeout+2*time.Minute)
	started := a.now()
	err := c.up(upCtx, timeout)
	cancel()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	if a.now().Sub(started) >= timeout {
		return failed(agentapi.CodeHealthTimeout, err)
	}
	if services, perr := c.ps(ctx); perr == nil {
		for _, s := range services {
			if s.Health == "unhealthy" {
				return failed(agentapi.CodeHealthTimeout, err)
			}
		}
	}
	return failed(agentapi.CodeComposeFailed, err)
}

// upgradeForward runs the phases up to the confirmation and then commits or
// rolls back, returning the result's error code. It returns an error only
// for a failure in a forward phase (j.Phase tells which) or an
// interruption.
func (a *Agent) upgradeForward(ctx context.Context, in *instance, j *journal, from *agentapi.Spec) (string, error) {
	err := a.forward(ctx, in, j, from)
	if err != nil {
		return "", err
	}
	a.log.Info("upgrade awaits confirmation", "instance", in.key, "operation", j.Operation, "deadline", j.ConfirmBy)
	confirmed, err := a.awaitConfirmation(ctx, in, j)
	if err != nil {
		return "", err
	}
	if confirmed {
		return a.commit(ctx, in, j), nil
	}
	return a.rollback(ctx, in, j), nil
}

// forward runs the phases from preflight to awaiting_confirmation.
func (a *Agent) forward(ctx context.Context, in *instance, j *journal, from *agentapi.Spec) error {
	key := in.key
	if err := a.enter(in, j, agentapi.PhasePreflight); err != nil {
		return err
	}
	pkg, ok := a.registry.Get(j.To)
	if !ok {
		return failed(agentapi.CodeUnknownPackageVersion, fmt.Errorf("package %s is not embedded", j.To))
	}
	if from.Package.ID != j.To.ID {
		return failed(agentapi.CodeInvalidInput, errors.New("the instance runs another package"))
	}
	if err := pkg.CheckUpgrade(from.Package.Version); err != nil {
		return failed(agentapi.CodeUpgradePathBlocked, err)
	}
	inputs, err := pkg.Inputs(j.Spec)
	if err != nil {
		return failed(agentapi.CodeInvalidInput, err)
	}
	plan, err := a.backupPlan(ctx, key, from)
	if err != nil {
		return failed(agentapi.CodeDatabaseFailed, err)
	}
	if err := a.checkDisk(ctx, key, plan); err != nil {
		return err
	}

	c := a.compose(key, secretsOf(from, j.Spec))
	fencedRC := a.renderContext(key)
	fencedRC.Fenced = true
	candidate, err := pkg.Render(j.Spec, inputs, fencedRC)
	if err != nil {
		return failed(agentapi.CodeInvalidInput, err)
	}
	// The commit renders unfenced: check that render now, not after the
	// switch.
	rc := a.renderContext(key)
	final, err := pkg.Render(j.Spec, inputs, rc)
	if err != nil {
		return failed(agentapi.CodeInvalidInput, err)
	}
	// No route may reach the unconfirmed candidate, nor the running version
	// once a rollback restores it fenced (fence.go). The routed names are
	// those of the running deployment and of the target's unfenced render;
	// the journal keeps them for the rollback, because a route of either
	// version may be active until the operation ends. All of this is
	// checked before anything is pulled, stopped or switched.
	candidateJSON, err := a.checkPolicy(ctx, pkg, candidate, inputs, fencedRC, c)
	if err != nil {
		return err
	}
	finalJSON, err := a.checkPolicy(ctx, pkg, final, inputs, rc, c)
	if err != nil {
		return err
	}
	routed, err := routedNames(key, finalJSON)
	if err != nil {
		return failed(agentapi.CodeInvalidInput, err)
	}
	var running *dnsProject
	if _, err := os.Stat(filepath.Join(c.dir, composeFile)); err == nil {
		runningJSON, err := c.config(ctx, filepath.Join(c.dir, composeFile), filepath.Join(c.dir, envFile))
		if err != nil {
			return failed(agentapi.CodeComposeFailed, err)
		}
		names, err := routedNames(key, runningJSON)
		if err != nil {
			return failed(agentapi.CodeComposeFailed, err)
		}
		routed = mergeNames(routed, names)
		if running, err = parseDNSProject(runningJSON); err != nil {
			return failed(agentapi.CodeComposeFailed, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) { // no deployment: nothing is routed to it
		return failed(agentapi.CodeComposeFailed, err)
	}
	if err := checkFenced(candidateJSON, c.project, routed); err != nil {
		return &opError{code: agentapi.CodePolicyViolation, rule: packages.RuleAlias, err: err}
	}
	if running != nil {
		if err := checkImplicitNames(running, c.project, routed); err != nil {
			return &opError{code: agentapi.CodePolicyViolation, rule: packages.RuleAlias, err: fmt.Errorf("the running version could not be restored fenced: %w", err)}
		}
	}
	j.Routed, j.RoutedKnown = routed, true
	if err := a.enter(in, j, agentapi.PhasePulling); err != nil {
		return err
	}
	if err := a.stage(c, candidate, func(file, env string) error { return c.pull(ctx, file, env) }); err != nil {
		return failed(agentapi.CodePullFailed, err)
	}

	if err := a.enter(in, j, agentapi.PhaseStopping); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(c.dir, composeFile)); err == nil {
		// The whole project stops: the candidate replaces every service.
		if err := c.stop(ctx); err != nil {
			return failed(agentapi.CodeComposeFailed, err)
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return failed(agentapi.CodeInternal, err)
	}
	j.SnapshotID = id
	if err := a.enter(in, j, agentapi.PhaseSnapshotting); err != nil {
		return err
	}
	if err := a.takeSnapshot(ctx, key, from, plan, agentapi.SnapshotReasonUpgrade, id); err != nil {
		if interrupted(ctx, err) {
			return err
		}
		return failed(agentapi.CodeSnapshotFailed, err)
	}
	if _, err := a.ops.update(j.Operation, func(o *agentapi.Operation) { o.SnapshotID = id.String() }); err != nil {
		a.log.Error("update operation", "operation", j.Operation, "error", err)
	}

	if err := a.enter(in, j, agentapi.PhaseSwitching); err != nil {
		return err
	}
	if err := writeRendered(fencedRC, candidate); err != nil {
		return failed(agentapi.CodeComposeFailed, err)
	}
	if err := a.ensureDataDirs(pkg, fencedRC); err != nil {
		return failed(agentapi.CodeComposeFailed, err)
	}
	if pkg.Manifest.Database.Enabled {
		if err := a.ensureDatabase(ctx, key, j.Spec.Secrets[packages.SecretDatabasePassword]); err != nil {
			return failed(agentapi.CodeDatabaseFailed, err)
		}
	}
	if err := c.start(ctx); err != nil {
		return failed(agentapi.CodeComposeFailed, err)
	}

	if err := a.enter(in, j, agentapi.PhaseMigrating); err != nil {
		return err
	}
	if h := pkg.Manifest.Upgrade.Migrate; h != nil {
		hctx, cancel := context.WithTimeout(ctx, h.Timeout)
		err := c.exec(hctx, h.Service, h.Command)
		cancel()
		if err != nil {
			return failed(agentapi.CodeHookFailed, err)
		}
	}

	if err := a.enter(in, j, agentapi.PhaseVerifying); err != nil {
		return err
	}
	if err := a.upWait(ctx, c, pkg.Manifest.Health.StartTimeout); err != nil {
		return err
	}
	for _, h := range pkg.Manifest.Hooks.Configure {
		hctx, cancel := context.WithTimeout(ctx, h.Timeout)
		err := c.exec(hctx, h.Service, h.Command)
		cancel()
		if err != nil {
			return failed(agentapi.CodeHookFailed, err)
		}
	}

	timeout := pkg.Manifest.Upgrade.ConfirmTimeout
	if timeout <= 0 {
		timeout = defaultConfirmTimeout
	}
	deadline := a.now().Add(timeout).UTC()
	j.ConfirmBy = &deadline
	return a.enter(in, j, agentapi.PhaseAwaitingConfirmation)
}

// awaitConfirmation waits until the core confirms or aborts the upgrade, or
// the confirmation deadline passes. Decisions are taken under the file lock
// and recorded in the journal, so a confirmation and the timeout cannot
// both win.
func (a *Agent) awaitConfirmation(ctx context.Context, in *instance, j *journal) (bool, error) {
	dir := a.instanceDir(in.key)
	a.mu.Lock()
	in.awaiting = j.Operation
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		in.awaiting = uuid.Nil
		a.mu.Unlock()
	}()
	for {
		in.files.Lock()
		cur, err := readJournal(dir)
		if err == nil && cur == nil {
			err = errors.New("the upgrade journal disappeared")
		}
		if err == nil {
			switch {
			case cur.Phase == agentapi.PhaseCommitting:
				*j = *cur
				in.files.Unlock()
				return true, nil
			case cur.Decision == decisionAbort:
				*j = *cur
				in.files.Unlock()
				return false, nil
			case cur.ConfirmBy == nil || !a.now().Before(*cur.ConfirmBy):
				cur.Decision, cur.ErrorCode = decisionAbort, agentapi.CodeConfirmTimeout
				if err = writeJournal(dir, cur); err == nil {
					*j = *cur
					in.files.Unlock()
					a.log.Warn("upgrade not confirmed in time", "instance", in.key, "operation", j.Operation)
					return false, nil
				}
			}
		}
		in.files.Unlock()
		if err != nil {
			return false, failed(agentapi.CodeInternal, err)
		}
		wait := max(cur.ConfirmBy.Sub(a.now()), 10*time.Millisecond)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-in.decision:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// decide handles POST /v1/operations/{id}/confirm and /abort. Only an
// upgrade that this process waits on can be decided, and a confirmation
// only before its deadline: a journal in awaiting_confirmation left by an
// earlier process rolls back on recovery, whatever arrives first. A
// repeated call with the same decision answers the operation again.
func (a *Agent) decide(id uuid.UUID, confirm bool) (agentapi.Operation, string) {
	op, ok := a.ops.get(id)
	if !ok {
		return op, agentapi.CodeNotFound
	}
	if op.Kind != agentapi.KindUpgrade {
		return op, agentapi.CodeNotAwaitingConfirmation
	}
	in := a.instance(op.Instance)
	dir := a.instanceDir(op.Instance)
	in.files.Lock()
	defer in.files.Unlock()
	j, err := readJournal(dir)
	if err != nil {
		a.log.Error("read the upgrade journal", "instance", op.Instance, "error", err)
		return op, agentapi.CodeInternal
	}
	if j == nil || j.Operation != id {
		// Finished: only the same decision is answered again.
		if (confirm && op.State == agentapi.OpSucceeded) ||
			(!confirm && op.State == agentapi.OpRolledBack && op.ErrorCode == agentapi.CodeAborted) {
			return op, ""
		}
		return op, agentapi.CodeNotAwaitingConfirmation
	}
	a.mu.Lock()
	waiting := in.awaiting == id
	a.mu.Unlock()
	switch {
	case j.Phase == agentapi.PhaseAwaitingConfirmation && j.Decision == "" && waiting:
		if confirm && (j.ConfirmBy == nil || !a.now().Before(*j.ConfirmBy)) {
			// Too late: the timeout rolls it back.
			return op, agentapi.CodeNotAwaitingConfirmation
		}
		if confirm {
			j.Phase = agentapi.PhaseCommitting
		} else {
			j.Decision, j.ErrorCode = decisionAbort, agentapi.CodeAborted
		}
		if err := writeJournal(dir, j); err != nil {
			a.log.Error("record the decision", "instance", op.Instance, "error", err)
			return op, agentapi.CodeInternal
		}
		if confirm {
			a.opPhase(id, agentapi.PhaseCommitting)
		}
		select {
		case in.decision <- struct{}{}:
		default:
		}
		a.log.Info("upgrade decided", "instance", op.Instance, "operation", id, "confirmed", confirm)
		op, _ = a.ops.get(id)
		return op, ""
	case confirm && j.Phase == agentapi.PhaseCommitting:
		return op, ""
	case !confirm && j.Decision == decisionAbort && j.ErrorCode == agentapi.CodeAborted:
		return op, ""
	}
	return op, agentapi.CodeNotAwaitingConfirmation
}

// commit makes the confirmed candidate the running version: rendered
// unfenced (real aliases and ports), started, recorded. A failure leaves
// the journal in committing; the next reconcile finishes the commit (the
// candidate may have been reachable already, so it never rolls back).
func (a *Agent) commit(ctx context.Context, in *instance, j *journal) string {
	if err := a.enter(in, j, agentapi.PhaseCommitting); err != nil {
		if !interrupted(ctx, err) {
			a.log.Error("commit", "instance", in.key, "error", err)
		}
		return agentapi.CodeInternal
	}
	if err := a.commitWork(ctx, in, j); err != nil {
		if interrupted(ctx, err) {
			return ""
		}
		code, _ := errorCode(err)
		a.log.Error("commit failed; it is retried", "instance", in.key, "code", code, "error", err)
		return code
	}
	if err := a.finalize(in, j, j.Spec, false, agentapi.OpSucceeded, "", ""); err != nil {
		a.log.Error("record the commit", "instance", in.key, "error", err)
		return agentapi.CodeInternal
	}
	if err := a.pruneSnapshots(in.key); err != nil {
		a.log.Warn("prune snapshots", "instance", in.key, "error", err)
	}
	if err := a.cleanMovedAside(in.key, true); err != nil {
		a.log.Warn("remove moved-aside data", "instance", in.key, "error", err)
	}
	a.log.Info("upgrade committed", "instance", in.key, "package", j.To.String(), "generation", j.ToGeneration)
	return ""
}

func (a *Agent) commitWork(ctx context.Context, in *instance, j *journal) error {
	pkg, ok := a.registry.Get(j.To)
	if !ok {
		return failed(agentapi.CodeUnknownPackageVersion, fmt.Errorf("package %s is not embedded", j.To))
	}
	inputs, err := pkg.Inputs(j.Spec)
	if err != nil {
		return failed(agentapi.CodeInvalidInput, err)
	}
	rc := a.renderContext(in.key)
	rendered, err := pkg.Render(j.Spec, inputs, rc)
	if err != nil {
		return failed(agentapi.CodeInvalidInput, err)
	}
	c := a.compose(in.key, secretsOf(j.Spec))
	if _, err := a.checkPolicy(ctx, pkg, rendered, inputs, rc, c); err != nil {
		return err
	}
	if err := writeRendered(rc, rendered); err != nil {
		return failed(agentapi.CodeComposeFailed, err)
	}
	if err := a.ensureDataDirs(pkg, rc); err != nil {
		return failed(agentapi.CodeComposeFailed, err)
	}
	if err := a.upWait(ctx, c, pkg.Manifest.Health.StartTimeout); err != nil {
		return err
	}
	// The configure hooks ran for the candidate of the same spec; unfencing
	// does not run them again.
	if err := writeConfigured(c.dir, rendered.Hash()); err != nil {
		return failed(agentapi.CodeInternal, err)
	}
	return nil
}

// rollback restores the snapshot fenced (phase rolling_back), records
// restored durably, and exposes the restored version.
func (a *Agent) rollback(ctx context.Context, in *instance, j *journal) string {
	if err := a.enter(in, j, agentapi.PhaseRollingBack); err != nil {
		if !interrupted(ctx, err) {
			a.log.Error("rollback", "instance", in.key, "error", err)
		}
		return agentapi.CodeInternal
	}
	a.log.Warn("rolling back", "instance", in.key, "kind", j.Kind, "snapshot", j.SnapshotID, "reason", j.ErrorCode)
	if err := a.restoreFenced(ctx, in, j); err != nil {
		if interrupted(ctx, err) {
			return ""
		}
		a.log.Error("rollback failed", "instance", in.key, "snapshot", j.SnapshotID, "error", err)
		// The journal records the failed rollback before the operation
		// does: until it is durable, the phase stays rolling_back and the
		// next trigger repeats the fenced restore.
		j.RollbackFailed = true
		in.files.Lock()
		werr := writeJournal(a.instanceDir(in.key), j)
		in.files.Unlock()
		if werr != nil {
			j.RollbackFailed = false
			a.log.Error("record the failed rollback", "instance", in.key, "error", werr)
			return agentapi.CodeInternal
		}
		a.recordRollbackFailed(j)
		return agentapi.CodeRollbackFailed
	}
	if err := a.enter(in, j, agentapi.PhaseRestored); err != nil {
		if !interrupted(ctx, err) {
			a.log.Error("rollback", "instance", in.key, "error", err)
		}
		return agentapi.CodeInternal
	}
	return a.expose(ctx, in, j)
}

// recordRollbackFailed records a durable failed rollback (the journal has
// rollback_failed) on its operation: failed with rollback_failed and the
// snapshot to restore. It is repeated on start and by every reconcile of
// the instance, so a crash or a failed write between the journal and the
// operation still ends with this result.
func (a *Agent) recordRollbackFailed(j *journal) {
	if _, err := a.ops.update(j.Operation, func(o *agentapi.Operation) {
		if o.Terminal() {
			return
		}
		o.State, o.Phase, o.ErrorCode, o.Rule = agentapi.OpFailed, "", agentapi.CodeRollbackFailed, ""
		if j.SnapshotID != uuid.Nil {
			o.SnapshotID = j.SnapshotID.String()
		}
	}); err != nil {
		a.log.Error("record the failed rollback", "operation", j.Operation, "error", err)
	}
}

// restoreFenced is rollback phase 1: the candidate goes down, the data
// directories and the database are restored from the snapshot, and the
// snapshot's rendered files start fenced. Every step is safe to repeat,
// and nothing is reachable through a client entry point.
func (a *Agent) restoreFenced(ctx context.Context, in *instance, j *journal) error {
	if j.SnapshotID == uuid.Nil {
		return errors.New("the journal names no snapshot")
	}
	snap, spec, err := a.loadSnapshot(in.key, j.SnapshotID)
	if err != nil {
		return err
	}
	if err := a.verifySnapshot(in.key, snap); err != nil {
		return err
	}
	c := a.compose(in.key, secretsOf(spec, j.Spec))
	deployed := false
	if _, err := os.Stat(filepath.Join(c.dir, composeFile)); err == nil {
		deployed = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !j.RoutedKnown {
		// A restore: what runs now is routed until the restore ends. Its
		// names go into the journal before anything changes, so a repeated
		// run (the deployment is fenced by then) still knows them.
		if deployed {
			current, err := c.config(ctx, filepath.Join(c.dir, composeFile), filepath.Join(c.dir, envFile))
			if err != nil {
				return err
			}
			names, err := routedNames(in.key, current)
			if err != nil {
				return err
			}
			j.Routed = mergeNames(j.Routed, names)
		}
		j.RoutedKnown = true
		in.files.Lock()
		err := writeJournal(a.instanceDir(in.key), j)
		in.files.Unlock()
		if err != nil {
			return err
		}
	}
	if deployed {
		if err := c.down(ctx); err != nil {
			return err
		}
	}
	if err := a.restoreDataDirs(in.key, snap, j.Operation); err != nil {
		return err
	}
	if snap.Database != nil {
		if err := a.restoreDatabase(ctx, in.key, snap, spec.Secrets[packages.SecretDatabasePassword]); err != nil {
			return err
		}
	}
	if err := a.writeSnapshotRendered(ctx, in.key, snap.ID, true, c, j.Routed); err != nil {
		return err
	}
	if spec.State == agentapi.StateRunning {
		return a.upWait(ctx, c, a.startTimeout(spec))
	}
	return nil
}

// expose is rollback phase 2 (restored): the snapshot's rendered files are
// written unfenced and started. It never touches data, so repeating it
// after a crash keeps every write the restored version accepted. A
// failure leaves the journal in restored; the next reconcile repeats it.
func (a *Agent) expose(ctx context.Context, in *instance, j *journal) string {
	_, spec, err := a.loadSnapshot(in.key, j.SnapshotID)
	if err == nil {
		c := a.compose(in.key, secretsOf(spec, j.Spec))
		if err = a.writeSnapshotRendered(ctx, in.key, j.SnapshotID, false, c, nil); err == nil {
			switch spec.State {
			case agentapi.StateRunning:
				err = a.upWait(ctx, c, a.startTimeout(spec))
			case agentapi.StateStopped:
				err = c.stop(ctx)
			default:
				err = c.down(ctx)
			}
		}
	}
	if err == nil {
		err = a.checkpoint("restored:exposed")
	}
	if err != nil {
		if interrupted(ctx, err) {
			return ""
		}
		code, _ := errorCode(err)
		a.log.Error("exposing the restored version failed; it is retried", "instance", in.key, "error", err)
		return code
	}
	restored := spec
	state, code, block := agentapi.OpRolledBack, j.ErrorCode, true
	if j.Kind == agentapi.KindRestore {
		restored, state, code, block = j.Spec, agentapi.OpSucceeded, "", false
	}
	// Keep a verified copy of the saved project before removing
	// the journal. Ordinary drift repair uses it without touching live data.
	if err := a.saveRestoredProject(in.key, j.SnapshotID, restored); err != nil {
		a.log.Error("save restored project", "instance", in.key, "error", err)
		return agentapi.CodeInternal
	}
	if err := a.finalize(in, j, restored, block, state, code, ""); err != nil {
		a.log.Error("record the rollback", "instance", in.key, "error", err)
		return agentapi.CodeInternal
	}
	a.log.Info("restored", "instance", in.key, "kind", j.Kind, "package", restored.Package.String(), "generation", restored.Generation)
	if block {
		return code
	}
	return ""
}

// recoverJournal continues an upgrade or restore that a previous process
// (or a failed commit or exposure) left in its journal, as the table in
// docs/specs/agent.md "Interrupted upgrades" says. It never resumes an
// upgrade forward.
func (a *Agent) recoverJournal(ctx context.Context, in *instance, st *instanceState, j *journal) string {
	if j.RollbackFailed {
		return agentapi.CodeRollbackFailed // waits for a restore request
	}
	if _, ok := a.ops.get(j.Operation); !ok {
		// Without its operation, finalize could never record the result
		// and the journal would stay for good.
		a.log.Warn("the journal's operation is unknown; recreating it", "instance", in.key, "operation", j.Operation)
		if err := a.ops.adopt(j.Operation, in.key, j.Kind, j.Phase, j.ToGeneration); err != nil {
			a.log.Error("recreate the journal's operation", "instance", in.key, "operation", j.Operation, "error", err)
			return agentapi.CodeInternal
		}
	}
	a.log.Warn("recovering an interrupted operation", "instance", in.key, "kind", j.Kind, "phase", j.Phase, "operation", j.Operation)
	// A failure before the switch that was already decided keeps its code;
	// otherwise the upgrade was interrupted.
	code, rule := j.ErrorCode, j.Rule
	if code == "" {
		code, rule = agentapi.CodeInterrupted, ""
	}
	switch j.Phase {
	case agentapi.PhasePreflight, agentapi.PhasePulling:
		return a.endFailed(ctx, in, j, code, rule)
	case agentapi.PhaseStopping, agentapi.PhaseSnapshotting:
		a.discardSnapshotAndRestart(ctx, in, st.current)
		return a.endFailed(ctx, in, j, code, rule)
	case agentapi.PhaseSwitching, agentapi.PhaseMigrating, agentapi.PhaseVerifying, agentapi.PhaseAwaitingConfirmation:
		if j.ErrorCode == "" {
			j.ErrorCode = agentapi.CodeInterrupted
		}
		return a.rollback(ctx, in, j)
	case agentapi.PhaseCommitting:
		return a.commit(ctx, in, j)
	case agentapi.PhaseRestored:
		return a.expose(ctx, in, j)
	default: // rolling_back, or a phase this version does not know
		if j.ErrorCode == "" && j.Kind == agentapi.KindUpgrade {
			j.ErrorCode = agentapi.CodeInterrupted
		}
		return a.rollback(ctx, in, j)
	}
}

// manualSnapshot takes a snapshot on request: the manifest's
// backup.stop_services stop, the snapshot is taken, and they start again.
func (a *Agent) manualSnapshot(ctx context.Context, in *instance, st *instanceState, id uuid.UUID) string {
	err := a.manualSnapshotWork(ctx, in, st, id)
	if ctx.Err() != nil && err != nil {
		return ""
	}
	code := ""
	if err != nil {
		code, _ = errorCode(err)
		a.log.Error("snapshot failed", "instance", in.key, "code", code, "error", err)
	}
	if _, uerr := a.ops.update(id, func(o *agentapi.Operation) {
		o.Phase = ""
		if code == "" {
			o.State = agentapi.OpSucceeded
			return
		}
		o.State, o.ErrorCode = agentapi.OpFailed, code
	}); uerr != nil {
		a.log.Error("update operation", "operation", id, "error", uerr)
	}
	return code
}

func (a *Agent) manualSnapshotWork(ctx context.Context, in *instance, st *instanceState, opID uuid.UUID) error {
	cur := st.current
	if cur == nil || cur.State == agentapi.StateAbsent {
		return failed(agentapi.CodeNotFound, errors.New("the instance is not installed"))
	}
	pkg, ok := a.registry.Get(cur.Package)
	if !ok {
		return failed(agentapi.CodeUnknownPackageVersion, fmt.Errorf("package %s is not embedded", cur.Package))
	}
	plan, err := a.backupPlan(ctx, in.key, cur)
	if err != nil {
		return failed(agentapi.CodeDatabaseFailed, err)
	}
	if err := a.checkDisk(ctx, in.key, plan); err != nil {
		return err
	}
	c := a.compose(in.key, cur.SecretValues())
	running := cur.State == agentapi.StateRunning
	restart := func() {
		if running {
			if err := a.upWait(ctx, c, pkg.Manifest.Health.StartTimeout); err != nil {
				a.log.Error("start after the snapshot", "instance", in.key, "error", err)
			}
		}
	}
	a.opPhase(opID, agentapi.PhaseStopping)
	if running && len(pkg.Manifest.Backup.StopServices) > 0 {
		if err := c.stop(ctx, pkg.Manifest.Backup.StopServices...); err != nil {
			restart()
			return failed(agentapi.CodeComposeFailed, err)
		}
	}
	a.opPhase(opID, agentapi.PhaseSnapshotting)
	id, err := uuid.NewV7()
	if err != nil {
		restart()
		return failed(agentapi.CodeInternal, err)
	}
	if err := a.takeSnapshot(ctx, in.key, cur, plan, agentapi.SnapshotReasonManual, id); err != nil {
		if rerr := a.removePartialSnapshots(in.key, uuid.Nil); rerr != nil {
			a.log.Warn("remove the partial snapshot", "instance", in.key, "error", rerr)
		}
		restart()
		return failed(agentapi.CodeSnapshotFailed, err)
	}
	if _, err := a.ops.update(opID, func(o *agentapi.Operation) { o.SnapshotID = id.String() }); err != nil {
		a.log.Error("update operation", "operation", opID, "error", err)
	}
	a.opPhase(opID, agentapi.PhaseStarting)
	if running {
		if err := a.upWait(ctx, c, pkg.Manifest.Health.StartTimeout); err != nil {
			return err
		}
	}
	return nil
}
