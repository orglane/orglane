package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
)

// opError is a failed reconcile step with its operation error code.
type opError struct {
	code string
	rule string // with CodePolicyViolation
	err  error
}

func (e *opError) Error() string { return e.code + ": " + e.err.Error() }
func (e *opError) Unwrap() error { return e.err }

func failed(code string, err error) error { return &opError{code: code, err: err} }

func errorCode(err error) (code, rule string) {
	var oe *opError
	if errors.As(err, &oe) {
		return oe.code, oe.rule
	}
	return agentapi.CodeInternal, ""
}

// compose returns the Compose runner of an instance.
func (a *Agent) compose(key string, secrets []string) compose {
	return compose{r: a.docker, project: packages.InstanceProject(a.cfg.ComposeProject, key), dir: a.instanceDir(key), secrets: secrets}
}

func (a *Agent) postgres() postgres {
	return postgres{r: a.docker, project: a.cfg.ComposeProject, service: a.cfg.PostgresService, user: a.cfg.PostgresUser}
}

// reconcile runs one reconcile of an instance (docs/specs/agent.md
// "Reconcile loop"). It always converges toward current.json; only an
// operation for a pending, unblocked desired spec moves current toward
// desired.
func (a *Agent) reconcile(ctx context.Context, in *instance) {
	if a.crashed.Load() {
		return
	}
	dir := a.instanceDir(in.key)
	in.files.Lock()
	st, err := loadState(dir)
	var j *journal
	if err == nil {
		j, err = readJournal(dir)
	}
	// Claim work before releasing the lock shared with restore acceptance.
	// Operation-store I/O may block; a request must not publish a journal
	// after this state read and before the worker claims its generation.
	var snapshotOp uuid.UUID
	if err == nil {
		switch {
		case j != nil:
			if !j.RollbackFailed {
				a.setBusyKind(in, j.Kind)
			}
		case st.pending():
			a.setBusyKind(in, opKind(st.desired, st.current))
		}
		if j == nil {
			snapshotOp = a.takeSnapshotRequest(in)
			if snapshotOp != uuid.Nil {
				a.setBusyKind(in, agentapi.KindSnapshot)
			}
		}
	}
	in.files.Unlock()
	if err != nil {
		a.log.Error("read instance state", "instance", in.key, "error", err)
		return
	}

	// An upgrade or restore journal comes first: recover it and stop
	// (docs/specs/agent.md "Reconcile loop").
	if j != nil {
		if j.RollbackFailed {
			a.recordRollbackFailed(j)
			a.setLastError(in, agentapi.CodeRollbackFailed)
			return
		}
		a.setBusyKind(in, j.Kind)
		code := a.recoverJournal(ctx, in, st, j)
		a.clearBusy(in, code)
		a.wakeIfSettled(in)
		return
	}
	if id := snapshotOp; id != uuid.Nil {
		a.setBusyKind(in, agentapi.KindSnapshot)
		a.clearBusy(in, a.manualSnapshot(ctx, in, st, id))
		a.wake(in.key) // pending work runs next
		return
	}

	var target *agentapi.Spec
	var op *agentapi.Operation
	switch {
	case st.pending():
		target = st.desired
		o, ok := a.ops.forGeneration(in.key, target.Generation)
		if !ok || o.Terminal() {
			// A retry after a failure, or an operation pruned meanwhile.
			if o, err = a.ops.create(in.key, opKind(target, st.current), target.Generation); err != nil {
				a.log.Error("create operation", "instance", in.key, "error", err)
				a.clearBusy(in, agentapi.CodeInternal)
				return
			}
		}
		if isUpgrade(st) {
			a.setBusyKind(in, agentapi.KindUpgrade)
			code := a.upgrade(ctx, in, st, o)
			a.clearBusy(in, code)
			a.wakeIfSettled(in)
			return
		}
		op = &o
	case st.current != nil:
		target = st.current
	default:
		return // nothing accepted, or only a blocked generation and nothing running
	}

	a.setBusy(in, op)
	setPhase := func(phase string) {
		if op == nil {
			return
		}
		if _, err := a.ops.update(op.ID, func(o *agentapi.Operation) { o.State, o.Phase = agentapi.OpRunning, phase }); err != nil {
			a.log.Error("update operation", "operation", op.ID, "error", err)
		}
	}
	setPhase("")
	start := a.now()
	err = a.converge(ctx, in, target, st.current, setPhase)
	if ctx.Err() != nil && err != nil {
		// Shutdown: the next start runs the operation again.
		a.clearBusy(in, "")
		return
	}
	if err != nil {
		code, rule := errorCode(err)
		a.log.Error("reconcile failed", "instance", in.key, "generation", target.Generation, "code", code, "rule", rule, "error", err)
	} else {
		a.log.Info("reconciled", "instance", in.key, "generation", target.Generation, "state", target.State, "duration", a.now().Sub(start).Round(time.Millisecond))
	}
	a.clearBusy(in, a.finish(in, st, target, op, err))
}

// finish records the result of a reconcile durably and only then publishes
// it on the operation: an operation never reports success while
// current.json (or blocked.json) still holds the old state. Both happen
// under the instance's file lock, so a request never sees a recorded result
// without its operation outcome; for a purge that is the deletion of the
// directory. It returns the error code of the reconcile, "" on success.
func (a *Agent) finish(in *instance, st *instanceState, target *agentapi.Spec, op *agentapi.Operation, err error) string {
	code, rule := "", ""
	if err != nil {
		code, rule = errorCode(err)
	}
	in.files.Lock()
	trash, recordErr := a.record(in.key, st, target, op, err == nil, code)
	if recordErr != nil {
		a.log.Error("record reconcile result", "instance", in.key, "generation", target.Generation, "error", recordErr)
		if err == nil {
			// Converged, but the new state is not durable: the generation
			// stays pending and the next reconcile applies it again.
			code, rule = agentapi.CodeInternal, ""
		}
	}
	a.at("recorded")
	if op != nil {
		if _, uerr := a.ops.update(op.ID, func(o *agentapi.Operation) {
			o.Phase = ""
			if code == "" {
				o.State = agentapi.OpSucceeded
				return
			}
			o.State, o.ErrorCode, o.Rule = agentapi.OpFailed, code, rule
		}); uerr != nil {
			a.log.Error("update operation", "operation", op.ID, "error", uerr)
		}
	}
	in.files.Unlock()
	if trash != "" {
		if err := os.RemoveAll(trash); err != nil {
			a.log.Warn("delete the purged instance directory", "instance", in.key, "error", err)
		}
	}
	return code
}

// record persists the result of a reconcile: current.json on success,
// failure counts and blocked generations otherwise; a successful purge
// moves the directory to the trash, whose path it returns. The caller holds
// the file lock.
func (a *Agent) record(key string, st *instanceState, target *agentapi.Spec, op *agentapi.Operation, ok bool, code string) (string, error) {
	if op == nil {
		return "", nil // drift repair of current; on failure the next trigger tries again
	}
	dir := a.instanceDir(key)
	// The directory changes its binding only through a purge, which this
	// function records; never write another instance's state.
	if id, err := readBinding(dir); err != nil {
		return "", err
	} else if id != target.InstanceID {
		return "", fmt.Errorf("the instance directory is no longer bound to instance %s", target.InstanceID)
	}
	if ok && target.State == agentapi.StateAbsent && target.Purge {
		desired, err := readSpec(filepath.Join(dir, desiredFile))
		if err != nil {
			return "", err
		}
		if desired == nil || desired.Generation <= target.Generation {
			return a.purgeDir(key, target, op)
		}
		// A newer spec arrived during the purge: keep its binding and the
		// spec, and record the purge as current so the newer spec applies
		// next.
		if err := removeExcept(dir, instanceFile, desiredFile); err != nil {
			return "", err
		}
	}
	if ok {
		// blocked.json first: current.json advances only when every other
		// file is durable, so a failed write leaves the generation pending.
		blocked := st.blocked
		blocked.Failures = nil
		if err := writeJSON(filepath.Join(dir, blockedFile), blocked); err != nil {
			return "", err
		}
		return "", writeSpec(filepath.Join(dir, currentFile), target)
	}
	blocked := st.blocked
	if blocked.Failures == nil || blocked.Failures.Generation != target.Generation {
		blocked.Failures = &failureCount{Generation: target.Generation}
	}
	blocked.Failures.Count++
	if blocked.Failures.Count >= maxApplyAttempts {
		blocked.Generations = append(blocked.Generations, blockedGeneration{Generation: target.Generation, ErrorCode: code, At: a.now().UTC()})
		blocked.Failures = nil
		a.log.Error("generation blocked after repeated failures", "instance", key, "generation", target.Generation, "code", code)
	}
	return "", writeJSON(filepath.Join(dir, blockedFile), blocked)
}

// purgeDir completes a purge: it writes the purge record, then moves the
// instance directory to the trash in one rename, which frees the key. A
// crash before the rename leaves the generation pending (the purge runs
// again); after it, the purge record settles the operation. The caller
// holds the file lock and deletes the returned trash path.
func (a *Agent) purgeDir(key string, target *agentapi.Spec, op *agentapi.Operation) (string, error) {
	rec := purgedRecord{
		InstanceID: target.InstanceID, Generation: target.Generation, SpecHash: target.Hash(),
		Operation: op.ID, At: a.now().UTC(),
	}
	if err := writeJSON(a.purgedPath(key), rec); err != nil {
		return "", err
	}
	a.at("purge-recorded")
	trash := filepath.Join(a.trashDir(), key+"-"+op.ID.String())
	if err := os.Rename(a.instanceDir(key), trash); err != nil {
		return "", err
	}
	// The rename is not known to be durable without the sync: the
	// operation fails (a crash could bring the directory back after a
	// success), and the trash is deleted all the same. Another purge
	// generation completes it.
	if err := syncDir(a.instancesDir); err != nil {
		return trash, err
	}
	a.at("purged")
	return trash, nil
}

func (a *Agent) setBusyKind(in *instance, kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in.busy, in.busyKind = true, kind
}

func (a *Agent) setLastError(in *instance, code string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in.lastError = code
}

// takeSnapshotRequest returns and clears a requested manual snapshot.
func (a *Agent) takeSnapshotRequest(in *instance) uuid.UUID {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := in.snapshotOp
	in.snapshotOp = uuid.Nil
	return id
}

// wakeIfSettled reconciles the instance again once its journal is gone, so
// a newer accepted spec applies after an upgrade or restore. A journal that
// stays (a commit or exposure to retry, a failed rollback) waits for the
// next trigger instead of looping.
func (a *Agent) wakeIfSettled(in *instance) {
	if a.crashed.Load() {
		return
	}
	in.files.Lock()
	j, err := readJournal(a.instanceDir(in.key))
	in.files.Unlock()
	if err == nil && j == nil {
		a.wake(in.key)
	}
}

func (a *Agent) setBusy(in *instance, op *agentapi.Operation) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in.busy = true
	in.busyKind = ""
	if op != nil {
		in.busyKind = op.Kind
	}
}

func (a *Agent) clearBusy(in *instance, code string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in.busy, in.busyKind, in.lastError = false, "", code
}

// converge brings the instance to target. prev is the spec that ran before.
func (a *Agent) converge(ctx context.Context, in *instance, target, prev *agentapi.Spec, setPhase func(string)) error {
	pkg, ok := a.registry.Get(target.Package)
	if !ok {
		// Only the exact running spec may use the saved project. A new
		// desired spec still requires its embedded package and policy.
		if prev != nil && target.Hash() == prev.Hash() {
			return a.repairRestored(ctx, in.key, target)
		}
		return failed(agentapi.CodeUnknownPackageVersion, fmt.Errorf("package %s is not embedded", target.Package))
	}
	secrets := target.SecretValues()
	if prev != nil {
		secrets = append(secrets, prev.SecretValues()...)
	}
	c := a.compose(in.key, secrets)

	if target.State == agentapi.StateAbsent {
		return a.remove(ctx, in, target, prev, c, setPhase)
	}

	setPhase(agentapi.PhaseRendering)
	inputs, err := pkg.Inputs(target)
	if err != nil {
		return failed(agentapi.CodeInvalidInput, err)
	}
	rc := a.renderContext(in.key)
	rendered, err := pkg.Render(target, inputs, rc)
	if err != nil {
		return failed(agentapi.CodeInvalidInput, err)
	}

	setPhase(agentapi.PhasePolicyCheck)
	if _, err := a.checkPolicy(ctx, pkg, rendered, inputs, rc, c); err != nil {
		return err
	}

	setPhase(agentapi.PhasePreparing)
	if err := writeRendered(rc, rendered); err != nil {
		return failed(agentapi.CodeComposeFailed, err)
	}
	if err := a.ensureDataDirs(pkg, rc); err != nil {
		return failed(agentapi.CodeComposeFailed, err)
	}
	if pkg.Manifest.Database.Enabled {
		setPhase(agentapi.PhaseDatabase)
		if err := a.ensureDatabase(ctx, in.key, target.Secrets[packages.SecretDatabasePassword]); err != nil {
			return failed(agentapi.CodeDatabaseFailed, err)
		}
	}

	if target.State == agentapi.StateStopped {
		setPhase(agentapi.PhaseStopping)
		if err := c.stop(ctx); err != nil {
			return failed(agentapi.CodeComposeFailed, err)
		}
		return nil
	}

	setPhase(agentapi.PhaseStarting)
	if err := a.upWait(ctx, c, pkg.Manifest.Health.StartTimeout); err != nil {
		return err
	}

	// Configure hooks run whenever the running render differs from the last
	// one they configured successfully (configured.json): after the first
	// start, also when the instance was installed or reconfigured while
	// stopped, and after a reinstall. Never for a plain drift repair.
	hash := rendered.Hash()
	if configuredHash(c.dir) != hash {
		setPhase(agentapi.PhaseConfiguring)
		for _, h := range pkg.Manifest.Hooks.Configure {
			hctx, cancel := context.WithTimeout(ctx, h.Timeout)
			err := c.exec(hctx, h.Service, h.Command)
			cancel()
			if err != nil {
				return failed(agentapi.CodeHookFailed, err)
			}
		}
		if err := writeConfigured(c.dir, hash); err != nil {
			return failed(agentapi.CodeInternal, err)
		}
	}
	return nil
}

// configuredHash returns the render hash the configure hooks last ran for,
// "" if they never completed.
func configuredHash(dir string) string {
	var c struct {
		RenderHash string `json:"render_hash"`
	}
	if ok, err := readJSON(filepath.Join(dir, configuredFile), &c); !ok || err != nil {
		return ""
	}
	return c.RenderHash
}

func writeConfigured(dir, hash string) error {
	return writeJSON(filepath.Join(dir, configuredFile), struct {
		RenderHash string `json:"render_hash"`
	}{hash})
}

// checkPolicy writes the rendered project to the staging directory,
// normalizes it with `docker compose config` and checks it against the
// package policy, returning the normalized project. Nothing is started from
// staging.
func (a *Agent) checkPolicy(ctx context.Context, pkg *packages.Package, r *packages.Rendered, in packages.Inputs, rc packages.RenderContext, c compose) ([]byte, error) {
	var normalized []byte
	err := a.stage(c, r, func(file, env string) error {
		var err error
		normalized, err = c.config(ctx, file, env)
		return err
	})
	if err != nil {
		return nil, failed(agentapi.CodeComposeFailed, err)
	}
	if err := pkg.CheckPolicy(normalized, in, rc); err != nil {
		var pe *packages.PolicyError
		if errors.As(err, &pe) {
			return nil, &opError{code: agentapi.CodePolicyViolation, rule: pe.Rule, err: err}
		}
		return nil, failed(agentapi.CodePolicyViolation, err)
	}
	return normalized, nil
}

// writeRendered writes the rendered files, .env and compose.yaml (last), each
// atomically. Package files are replaced one by one inside files/ (not by
// swapping the directory), so running containers that mount it see the new
// content; files the new render no longer has are deleted.
func writeRendered(rc packages.RenderContext, r *packages.Rendered) error {
	filesDir := rc.FilesPath()
	// Package files are mounted read-only into services that may run as any
	// uid, so they are world-readable; they never hold secrets.
	if err := os.MkdirAll(filesDir, 0o755); err != nil { //nolint:gosec // see above
		return err
	}
	names := make([]string, 0, len(r.Files))
	for name := range r.Files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		f := r.Files[name]
		p := filepath.Join(filesDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // see above
			return err
		}
		if err := writeFileAtomic(p, f.Data, f.Mode); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		return err
	}
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, ok := r.Files[p]; !ok {
			return root.Remove(p)
		}
		return nil
	})
	if err = errors.Join(err, root.Close()); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(rc.InstanceDir, envFile), r.Env, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(rc.InstanceDir, composeFile), r.Compose, 0o644)
}

// ensureDataDirs creates the data directories with the manifest's mode and,
// when the agent runs as root, owner. A data directory that is a symlink is
// refused: Docker would follow it on the host.
func (a *Agent) ensureDataDirs(pkg *packages.Package, rc packages.RenderContext) error {
	root := filepath.Join(rc.InstanceDir, dataDir)
	if err := os.MkdirAll(root, 0o755); err != nil { //nolint:gosec // traversed by service uids; each data directory has its own mode
		return err
	}
	for _, d := range pkg.Manifest.DataDirs {
		p := rc.DataPath(d.Name)
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(p, 0o700); err != nil {
				return err
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("data directory %s is not a directory", d.Name)
		}
		if err := os.Chmod(p, fs.FileMode(d.FileMode())); err != nil {
			return err
		}
		if uid, gid, ok := d.UIDGID(); ok {
			if os.Geteuid() != 0 {
				a.log.Warn("not running as root: data directory owner not set", "dir", d.Name)
				continue
			}
			if err := os.Lchown(p, uid, gid); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureDatabase creates or updates the instance's role and database
// (docs/specs/agent.md "Per-service database"). The identifier comes from
// the validated key and the password from a validated secret; the SQL goes
// to psql on standard input.
func (a *Agent) ensureDatabase(ctx context.Context, key, password string) error {
	if !packages.ValidSecret(password) {
		return errors.New("invalid database password")
	}
	name := packages.DatabaseName(key)
	sql := fmt.Sprintf(`SELECT format('CREATE ROLE %%I LOGIN', '%[1]s') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s')\gexec
ALTER ROLE %[1]s WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE CONNECTION LIMIT 20 PASSWORD %[2]s;
SELECT format('CREATE DATABASE %%I OWNER %%I ENCODING ''UTF8'' TEMPLATE template0', '%[1]s', '%[1]s') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '%[1]s')\gexec
REVOKE ALL ON DATABASE %[1]s FROM PUBLIC;
`, name, sqlString(password))
	return a.postgres().exec(ctx, sql, []string{password})
}

// dropDatabase removes the instance's database and role (purge).
func (a *Agent) dropDatabase(ctx context.Context, key string) error {
	name := packages.DatabaseName(key)
	sql := fmt.Sprintf("DROP DATABASE IF EXISTS %[1]s WITH (FORCE);\nDROP ROLE IF EXISTS %[1]s;\n", name)
	return a.postgres().exec(ctx, sql, nil)
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// remove stops and removes the project's containers; with purge it drops the
// database. finish then deletes the instance directory, which frees the key.
func (a *Agent) remove(ctx context.Context, in *instance, target, prev *agentapi.Spec, c compose, setPhase func(string)) error {
	setPhase(agentapi.PhaseRemoving)
	if _, err := os.Stat(filepath.Join(c.dir, composeFile)); err == nil {
		if err := c.down(ctx); err != nil {
			return failed(agentapi.CodeComposeFailed, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return failed(agentapi.CodeComposeFailed, err)
	}
	// A reinstall configures the new containers again.
	if err := os.Remove(filepath.Join(c.dir, configuredFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return failed(agentapi.CodeInternal, err)
	}
	if !target.Purge {
		return nil
	}
	setPhase(agentapi.PhasePurging)
	dbPkg := target.Package
	if prev != nil {
		dbPkg = prev.Package
	}
	if pkg, ok := a.registry.Get(dbPkg); !ok || pkg.Manifest.Database.Enabled {
		if err := a.dropDatabase(ctx, in.key); err != nil {
			return failed(agentapi.CodeDatabaseFailed, err)
		}
	}
	// finish deletes the directory, under the file lock that also
	// records the operation's result.
	return nil
}

func removeExcept(dir string, keep ...string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !slices.Contains(keep, e.Name()) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// status reports an instance; ok is false for an unknown key.
func (a *Agent) status(ctx context.Context, key string) (agentapi.InstanceStatus, bool, error) {
	in := a.instance(key)
	in.files.Lock()
	st, err := loadState(a.instanceDir(key))
	var j *journal
	var snapshots []agentapi.Snapshot
	if err == nil {
		j, err = readJournal(a.instanceDir(key))
	}
	if err == nil && st.id != uuid.Nil {
		snapshots, err = a.listSnapshots(key)
	}
	in.files.Unlock()
	if err != nil {
		return agentapi.InstanceStatus{}, false, err
	}
	if st.id == uuid.Nil {
		return agentapi.InstanceStatus{}, false, nil
	}
	out := agentapi.InstanceStatus{
		Key: key, InstanceID: st.id, Services: []agentapi.ServiceStatus{},
		BlockedGenerations: st.blocked.list(), Snapshots: len(snapshots),
	}
	if st.desired != nil {
		out.AcceptedGeneration = st.desired.Generation
	}
	if op, ok := a.ops.latest(key); ok {
		out.Operation = &op
	}
	a.mu.Lock()
	busy, busyKind, lastError := in.busy, in.busyKind, in.lastError
	a.mu.Unlock()
	out.ErrorCode = lastError

	cur := st.current
	if cur != nil {
		out.ObservedGeneration = cur.Generation
		ref := cur.Package
		out.Package = &ref
		out.State = cur.State
		out.DataRetained = cur.State == agentapi.StateAbsent
		if cur.State != agentapi.StateAbsent {
			services, err := a.compose(key, cur.SecretValues()).ps(ctx)
			if err != nil {
				a.log.Warn("compose ps failed", "instance", key, "error", err)
			}
			out.Services = append(out.Services, services...)
		}
	}

	switch {
	case j != nil && j.RollbackFailed:
		out.Phase = agentapi.PhaseFailed
	case j != nil && (j.Phase == agentapi.PhaseRollingBack || j.Phase == agentapi.PhaseRestored):
		out.Phase = agentapi.PhaseRollingBack
	case j != nil || (busy && busyKind == agentapi.KindUpgrade):
		out.Phase = agentapi.PhaseUpgrading
	case busy && busyKind == agentapi.KindRemove:
		out.Phase = agentapi.PhaseRemoving
	case busy && busyKind != "" && cur == nil:
		out.Phase = agentapi.PhaseInstalling
	case cur == nil:
		out.Phase = agentapi.PhasePending
		if lastError != "" || (st.desired != nil && st.blocked.has(st.desired.Generation)) {
			out.Phase = agentapi.PhaseFailed
		}
	case cur.State == agentapi.StateAbsent:
		out.Phase = agentapi.PhaseAbsent
	case cur.State == agentapi.StateStopped:
		out.Phase = agentapi.PhaseStopped
	default:
		out.Phase = agentapi.PhaseRunning
		if pkg, ok := a.registry.Get(cur.Package); !ok || !servicesHealthy(pkg.Manifest.Services, out.Services) {
			out.Phase = agentapi.PhaseDegraded
		}
	}
	return out, true, nil
}

// servicesHealthy reports whether every declared service runs and is not
// unhealthy.
func servicesHealthy(declared []string, services []agentapi.ServiceStatus) bool {
	for _, name := range declared {
		i := slices.IndexFunc(services, func(s agentapi.ServiceStatus) bool { return s.Name == name })
		if i < 0 || services[i].State != "running" || (services[i].Health != "" && services[i].Health != "healthy") {
			return false
		}
	}
	return true
}
