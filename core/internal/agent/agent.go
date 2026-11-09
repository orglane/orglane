// Package agent implements `orglane agent`: the only Orglane component with
// Docker access. It listens on a Unix socket, accepts declarative operations
// from the core (HMAC-signed requests and responses), renders the embedded
// managed packages into plain Compose projects under
// <data dir>/instances/<key>/, checks them against the package policy and
// drives `docker compose` (ADR 0006, ADR 0019, docs/specs/agent.md).
//
// The agent never runs a command, image, Compose file or path that a client
// sends: the API takes only typed desired specs for embedded packages.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/packages"
)

// Options configure an agent.
type Options struct {
	Config   config.Agent
	Version  string
	Registry *packages.Registry
	Logger   *slog.Logger

	// DockerBinary is the Docker CLI; empty means "docker" from PATH.
	DockerBinary string

	// EventDebounce and EventMinInterval pace reconciles after Docker
	// events: a reconcile runs EventDebounce after an event, at most once
	// per EventMinInterval per instance. Default 5s and 1m.
	EventDebounce, EventMinInterval time.Duration

	// Now replaces the clock in tests.
	Now func() time.Time

	// runner replaces the Docker CLI in unit tests.
	runner runner
	// diskFree replaces the free-space check in unit tests.
	diskFree func(path string) uint64
	// crashAt names the point where the agent stops as if it crashed
	// (checkpoint); tests set it, agenttest builds read
	// ORGLANE_AGENT_TEST_CRASH_AT.
	crashAt string
	// hook, if set, is called at named points of a reconcile, so tests can
	// interleave requests or stop the process there (a crash).
	hook func(point string)
	// syncDir replaces directory syncing in tests, to inject writeback
	// failures.
	syncDir func(string) error
}

// Agent is a running agent.
type Agent struct {
	opts         Options
	cfg          config.Agent
	log          *slog.Logger
	key          agentapi.Key
	verifier     *agentapi.Verifier
	registry     *packages.Registry
	docker       runner
	ops          *opStore
	bootID       uuid.UUID
	now          func() time.Time
	instancesDir string
	stateDir     string
	diskFree     func(path string) uint64
	crashAt      string
	crashed      atomic.Bool

	mu        sync.Mutex
	instances map[string]*instance
	ctx       context.Context // set by Run; workers start only after it
	stop      context.CancelFunc
	wg        sync.WaitGroup
}

// instance is the runtime state of one instance key.
type instance struct {
	key string
	// files serializes access to the instance's state files.
	files sync.Mutex
	// wake triggers a reconcile; the worker serializes them.
	wake chan struct{}
	// decision wakes an upgrade that awaits its confirmation.
	decision chan struct{}

	// Guarded by Agent.mu.
	started      bool
	eventTimer   *time.Timer
	lastEventRun time.Time
	busy         bool      // a reconcile runs
	busyKind     string    // the operation kind that runs, if any
	lastError    string    // error code of the last failed reconcile
	snapshotOp   uuid.UUID // a requested manual snapshot, uuid.Nil if none
	// awaiting is the upgrade operation this process waits to have
	// confirmed. Only it can be confirmed: a journal in
	// awaiting_confirmation left by an earlier process rolls back.
	awaiting uuid.UUID
}

// New prepares an agent: it derives the transport key and creates the data
// directories. Run starts it.
func New(opts Options) (*Agent, error) {
	key, err := agentapi.DeriveKey(opts.Config.Secret)
	if err != nil {
		return nil, err
	}
	if opts.Registry == nil {
		return nil, errors.New("agent: no package registry")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.EventDebounce <= 0 {
		opts.EventDebounce = 5 * time.Second
	}
	if opts.EventMinInterval <= 0 {
		opts.EventMinInterval = time.Minute
	}
	if opts.diskFree == nil {
		opts.diskFree = diskFree
	}
	if opts.crashAt == "" {
		opts.crashAt = testCrashAt()
	}
	if opts.runner == nil {
		bin := opts.DockerBinary
		if bin == "" {
			bin = "docker"
		}
		opts.runner = newDockerCLI(bin)
	}
	dataDir, err := filepath.Abs(opts.Config.DataDir)
	if err != nil {
		return nil, err
	}
	bootID, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	a := &Agent{
		opts:         opts,
		cfg:          opts.Config,
		log:          opts.Logger,
		key:          key,
		verifier:     agentapi.NewVerifier(key, opts.Now(), opts.Now),
		registry:     opts.Registry,
		docker:       opts.runner,
		bootID:       bootID,
		now:          opts.Now,
		diskFree:     opts.diskFree,
		crashAt:      opts.crashAt,
		instancesDir: filepath.Join(dataDir, "instances"),
		instances:    map[string]*instance{},
	}
	a.stateDir = filepath.Join(a.instancesDir, ".agent")
	for _, dir := range []string{a.instancesDir, a.stateDir, filepath.Join(a.stateDir, "operations"), a.purgedDir(), a.trashDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
	}
	if err := os.Chmod(a.stateDir, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
		return nil, fmt.Errorf("agent: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(a.stateDir, "boot_id"), []byte(bootID.String()+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	a.ops = newOpStore(filepath.Join(a.stateDir, "operations"), a.instancesDir, opts.Now)
	if err := a.ops.load(); err != nil {
		return nil, fmt.Errorf("agent: load operations: %w", err)
	}
	if err := a.assignLegacyOwners(); err != nil {
		return nil, fmt.Errorf("agent: assign operation owners: %w", err)
	}
	if err := a.removeAllPartialSnapshots(); err != nil {
		return nil, fmt.Errorf("agent: remove partial snapshots: %w", err)
	}
	if err := a.recoverOperations(); err != nil {
		return nil, fmt.Errorf("agent: recover operations: %w", err)
	}
	if a.crashAt != "" {
		a.log.Warn("test crash hook armed", "at", a.crashAt)
	}
	a.emptyTrash()
	return a, nil
}

// Run listens on the configured socket, reconciles every instance and
// serves until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	ln, err := listen(ctx, a.cfg.Listen, a.cfg.PeerUID, a.log)
	if err != nil {
		return err
	}
	return a.Serve(ctx, ln)
}

// Serve is Run on an existing listener.
func (a *Agent) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.mu.Lock()
	a.ctx, a.stop = ctx, cancel
	a.mu.Unlock()

	srv := &http.Server{
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	a.log.Info("agent started", "version", a.opts.Version, "boot_id", a.bootID, "listen", ln.Addr().String(),
		"packages", len(a.registry.List()), "peer_uid", a.cfg.PeerUID)

	a.wakeAll()
	a.wg.Add(2)
	go func() { defer a.wg.Done(); a.watchEvents(ctx) }()
	go func() { defer a.wg.Done(); a.periodic(ctx) }()

	var err error
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		cancel()
	}
	shutdownCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	err = errors.Join(err, ignoreClosed(srv.Shutdown(shutdownCtx)))
	a.wg.Wait()
	if a.crashed.Load() {
		a.log.Warn("agent stopped by the test crash hook")
		return errors.Join(err, errCrashed)
	}
	a.log.Info("agent stopped")
	return err
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// instanceDir returns the directory of a validated key.
func (a *Agent) instanceDir(key string) string { return filepath.Join(a.instancesDir, key) }

// purgedDir holds the purge records (purgedRecord) of the keys.
func (a *Agent) purgedDir() string { return filepath.Join(a.stateDir, "purged") }

func (a *Agent) purgedPath(key string) string { return filepath.Join(a.purgedDir(), key+".json") }

// trashDir holds purged instance directories until they are deleted: a
// purge moves the directory there in one rename.
func (a *Agent) trashDir() string { return filepath.Join(a.stateDir, "trash") }

// emptyTrash deletes what purges moved to the trash and could not delete
// (or a crash left).
func (a *Agent) emptyTrash() {
	entries, err := os.ReadDir(a.trashDir())
	if err != nil {
		a.log.Warn("read trash", "error", err)
		return
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(a.trashDir(), e.Name())); err != nil {
			a.log.Warn("delete a purged instance directory", "path", e.Name(), "error", err)
		}
	}
}

// at calls the test hook.
func (a *Agent) at(point string) {
	if a.opts.hook != nil {
		a.opts.hook(point)
	}
}

// instance returns the runtime state of a key, creating it.
func (a *Agent) instance(key string) *instance {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.instanceLocked(key)
}

func (a *Agent) instanceLocked(key string) *instance {
	in, ok := a.instances[key]
	if !ok {
		in = &instance{key: key, wake: make(chan struct{}, 1), decision: make(chan struct{}, 1)}
		a.instances[key] = in
	}
	return in
}

// wake triggers a reconcile of the instance, starting its worker once Run
// has started.
func (a *Agent) wake(key string) {
	a.mu.Lock()
	in := a.instanceLocked(key)
	if a.ctx != nil && !in.started {
		in.started = true
		a.wg.Add(1)
		go func() { defer a.wg.Done(); a.worker(a.ctx, in) }()
	}
	a.mu.Unlock()
	select {
	case in.wake <- struct{}{}:
	default: // a reconcile is already pending
	}
}

// keys lists the instance directories.
func (a *Agent) keys() ([]string, error) {
	entries, err := os.ReadDir(a.instancesDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && agentapi.KeyPattern.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (a *Agent) wakeAll() {
	keys, err := a.keys()
	if err != nil {
		a.log.Error("list instances", "error", err)
		return
	}
	for _, k := range keys {
		a.wake(k)
	}
}

func (a *Agent) worker(ctx context.Context, in *instance) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-in.wake:
			a.reconcile(ctx, in)
		}
	}
}

// periodic reconciles every instance every reconcile interval and prunes
// old operations.
func (a *Agent) periodic(ctx context.Context) {
	t := time.NewTicker(a.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.ops.prune(); err != nil {
				a.log.Warn("prune operations", "error", err)
			}
			a.cleanAllMovedAside()
			a.wakeAll()
		}
	}
}

// watchEvents reconciles instances whose containers die, are destroyed or
// change health, debounced (docs/specs/agent.md "Reconcile loop").
func (a *Agent) watchEvents(ctx context.Context) {
	prefix := a.cfg.ComposeProject + "-"
	for ctx.Err() == nil {
		err := events(ctx, a.docker, func(project string) {
			key, ok := cutPrefix(project, prefix)
			if ok && agentapi.KeyPattern.MatchString(key) {
				a.onEvent(key)
			}
		})
		if ctx.Err() != nil {
			return
		}
		a.log.Warn("docker events stream ended; restarting", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) <= len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}

func (a *Agent) onEvent(key string) {
	if _, err := os.Stat(a.instanceDir(key)); err != nil {
		return // not an instance of this agent
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	in := a.instanceLocked(key)
	if in.eventTimer != nil {
		return // a reconcile is already scheduled
	}
	delay := a.opts.EventDebounce
	if next := in.lastEventRun.Add(a.opts.EventMinInterval).Sub(a.now()); next > delay {
		delay = next
	}
	in.eventTimer = time.AfterFunc(delay, func() {
		a.mu.Lock()
		in.eventTimer = nil
		in.lastEventRun = a.now()
		a.mu.Unlock()
		a.wake(key)
	})
}

// health answers GET /v1/health.
func (a *Agent) health() agentapi.Health {
	total, available := memory()
	return agentapi.Health{
		Protocol:     agentapi.ProtocolVersion,
		Version:      a.opts.Version,
		BootID:       a.bootID,
		Packages:     a.registry.List(),
		MemTotal:     total,
		MemAvailable: available,
		DiskFree:     diskFree(a.instancesDir),
	}
}

// renderContext returns the context packages are rendered with.
func (a *Agent) renderContext(key string) packages.RenderContext {
	total, _ := memory()
	return packages.RenderContext{
		Key:             key,
		InstanceDir:     a.instanceDir(key),
		ComposeProject:  a.cfg.ComposeProject,
		PublishAddress:  a.cfg.PublishAddress,
		EdgeNetwork:     a.cfg.EdgeNetwork,
		ServicesNetwork: a.cfg.ServicesNetwork,
		DatabaseHost:    a.cfg.PostgresService,
		MemoryBytes:     int64(min(total, 1<<62)), //nolint:gosec // bounded
		CPUs:            runtime.NumCPU(),
	}
}

// assignLegacyOwners assigns the operations a previous agent version
// persisted without an instance ID when, and only when, the key's purge
// record proves the current binding owns them: the binding was created after
// that record, so an operation that started after it cannot belong to the
// purged instance. Without a purge record an older agent version may have
// reused the key without recording the purge, so absence of a record is not
// evidence: the operation stays unowned and is recovered as interrupted
// rather than attributed to the current binding.
func (a *Agent) assignLegacyOwners() error {
	type binding struct {
		id     uuid.UUID
		purged *purgedRecord
	}
	states := map[string]binding{}
	for _, op := range a.ops.unowned() {
		if !agentapi.KeyPattern.MatchString(op.Instance) {
			continue
		}
		b, ok := states[op.Instance]
		if !ok {
			purged, err := readPurged(a.purgedPath(op.Instance))
			if err != nil {
				return err
			}
			var id uuid.UUID
			if purged != nil {
				st, err := loadState(a.instanceDir(op.Instance))
				if err != nil {
					return err
				}
				id = st.id
			}
			b = binding{id: id, purged: purged}
			states[op.Instance] = b
		}
		if b.id != uuid.Nil && b.purged != nil && op.StartedAt.After(b.purged.At) {
			a.ops.setOwner(op.ID, b.id)
		}
	}
	return nil
}

// recoverOperations settles the operations a previous process left
// unfinished. An upgrade or restore with a journal is left to journal
// recovery (recoverJournal); a durable failed rollback is recorded from it.
// An apply, remove or upgrade without a journal is settled from what is
// durable on disk: it succeeded if its generation is current.json (the
// process stopped between the state write and the operation update), as
// did a purge whose purge record names that operation; a pending generation
// runs again; a generation replaced by a newer accepted spec is superseded;
// a blocked one fails with its blocking code. Anything else fails as
// interrupted, as does an operation that belongs to another instance.
func (a *Agent) recoverOperations() error {
	var errs []error
	for _, op := range a.ops.unfinished() {
		state, code := agentapi.OpFailed, agentapi.CodeInterrupted
		if agentapi.KeyPattern.MatchString(op.Instance) {
			dir := a.instanceDir(op.Instance)
			j, err := readJournal(dir)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if j != nil && j.Operation == op.ID {
				// The journal owns the operation's result. A durable failed
				// rollback has one already: record it, not "interrupted".
				if j.RollbackFailed {
					a.recordRollbackFailed(j)
				}
				continue
			}
			if j == nil && (op.Kind == agentapi.KindApply || op.Kind == agentapi.KindRemove || op.Kind == agentapi.KindUpgrade) {
				st, err := loadState(dir)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				purged, err := readPurged(a.purgedPath(op.Instance))
				if err != nil {
					errs = append(errs, err)
					continue
				}
				state, code = recoveredOutcome(st, purged, a.ops.ownerOf(op.ID), op)
				if state == agentapi.OpSucceeded && st.id == uuid.Nil && op.Kind == agentapi.KindRemove {
					// The purge record is written before the rename, so it
					// alone does not prove the directory stays gone. Sync the
					// rename before reporting success; a failure is recorded
					// like one from the purge itself.
					if err := a.syncDirectory(a.instancesDir); err != nil {
						a.log.Error("sync a recovered purge", "instance", op.Instance, "error", err)
						state, code = agentapi.OpFailed, agentapi.CodeInternal
					}
				}
			}
		}
		_, err := a.ops.update(op.ID, func(o *agentapi.Operation) {
			o.State, o.Phase, o.ErrorCode = state, "", code
		})
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// removeAllPartialSnapshots deletes every incomplete snapshot on start.
func (a *Agent) removeAllPartialSnapshots() error {
	keys, err := a.keys()
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range keys {
		errs = append(errs, a.removePartialSnapshots(k, uuid.Nil))
	}
	return errors.Join(errs...)
}

// cleanAllMovedAside deletes data moved aside by rollbacks and restores
// more than 7 days ago, and restore staging directories a failed restore
// left, except for instances with a journal (a running or failed rollback
// still needs them).
func (a *Agent) cleanAllMovedAside() {
	keys, err := a.keys()
	if err != nil {
		a.log.Warn("list instances", "error", err)
		return
	}
	for _, k := range keys {
		in := a.instance(k)
		in.files.Lock()
		j, err := readJournal(a.instanceDir(k))
		if err == nil && j == nil {
			// Under the lock: a restore accepted now would create its own
			// staging directory.
			if err := a.cleanRestoreStaging(k); err != nil {
				a.log.Warn("remove restore staging", "instance", k, "error", err)
			}
		}
		in.files.Unlock()
		if err != nil || j != nil {
			continue
		}
		if err := a.cleanMovedAside(k, false); err != nil {
			a.log.Warn("remove moved-aside data", "instance", k, "error", err)
		}
	}
}

// recoveredOutcome is the state and error code of an unfinished apply,
// remove or upgrade operation without a journal after a restart. owner is
// the instance the operation belongs to, uuid.Nil when its file predates
// recorded ownership and the key's purge record does not prove otherwise.
func recoveredOutcome(st *instanceState, purged *purgedRecord, owner uuid.UUID, op agentapi.Operation) (string, string) {
	gen := op.Generation
	// Never settle an operation from another instance's directory: a key
	// may have been reused after a purge at the same generation. An
	// ownerless operation that startup could not attribute to the current
	// binding (assignLegacyOwners) is not evidence about it either.
	if owner == uuid.Nil && st.id != uuid.Nil {
		return agentapi.OpFailed, agentapi.CodeInterrupted
	}
	if owner != uuid.Nil && st.id != uuid.Nil && owner != st.id {
		return agentapi.OpFailed, agentapi.CodeInterrupted
	}
	switch {
	case st.id == uuid.Nil:
		// No directory: the key was purged. Only the operation the purge
		// record names may be settled from it, so a remove of a new
		// instance that reused the key at the same generation is not
		// reported as succeeded from the previous instance's purge.
		if purged != nil && purged.Operation == op.ID && purged.Generation == gen && op.Kind == agentapi.KindRemove {
			return agentapi.OpSucceeded, ""
		}
	case st.current != nil && st.current.Generation == gen:
		return agentapi.OpSucceeded, ""
	case st.desired != nil && st.desired.Generation > gen:
		return agentapi.OpFailed, agentapi.CodeSuperseded
	case st.desired != nil && st.desired.Generation == gen:
		if code, ok := st.blocked.code(gen); ok {
			return agentapi.OpFailed, code
		}
		if st.pending() {
			return agentapi.OpQueued, ""
		}
	}
	return agentapi.OpFailed, agentapi.CodeInterrupted
}
