package apps

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/orglane/orglane/core/internal/store"
)

// QueueName is the River queue of every apps job, so provider slowness
// never blocks email or identity jobs.
const QueueName = "apps"

// DefaultMaxWorkers is the default size of the apps queue.
const DefaultMaxWorkers = 2

// Reasons for provision_instance.
const (
	ReasonActivated = "activated"
	ReasonMapping   = "mapping"
	ReasonConfig    = "config"
	ReasonDrift     = "drift"
	ReasonLink      = "link"
)

// Attempt limits.
const (
	maxProvisionAttempts = 8
	maxSetupAttempts     = 8
	// Uncertain rotations are retried without a practical limit, backing
	// off to an hour.
	maxRotateAttempts  = 10000
	maxDefaultAttempts = 8
)

// removalDelay collapses a burst of directory changes into one
// apply_removals run.
const removalDelay = 60 * time.Second

// uniqueStates are the River states across which apps jobs are unique.
// Completed jobs do not block a new one.
var uniqueStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

func uniqueOpts(maxAttempts int) river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueName,
		MaxAttempts: maxAttempts,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByState: uniqueStates},
	}
}

// ProvisionPersonArgs make one person's access in one instance match their
// desired access.
type ProvisionPersonArgs struct {
	InstanceID uuid.UUID `json:"instance_id" river:"unique"`
	PersonID   uuid.UUID `json:"person_id" river:"unique"`
	// FollowUp marks the one job queued while another runs, so a change
	// during a run is never lost.
	FollowUp bool `json:"follow_up,omitempty" river:"unique"`
	// Drift forces a plan even when the desired state was applied. Unique,
	// so a queued ordinary job (which may skip planning) does not swallow
	// the daily drift check.
	Drift bool `json:"drift,omitempty" river:"unique"`
	// Retry also retries a failed result (an admin's Retry). Unique, so a
	// queued ordinary job does not swallow it.
	Retry bool `json:"retry,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (ProvisionPersonArgs) Kind() string { return "orglane.apps.provision_person" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ProvisionPersonArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxProvisionAttempts) }

// ProvisionInstanceArgs sync an instance's teams and queue provisioning for
// everyone it concerns.
type ProvisionInstanceArgs struct {
	InstanceID uuid.UUID `json:"instance_id" river:"unique"`
	// Reason is unique, so a queued job for another reason does not
	// swallow the daily drift check (runs are serialized per instance).
	Reason string `json:"reason" river:"unique"`
	// FollowUp marks the one job queued while another runs.
	FollowUp bool `json:"follow_up,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (ProvisionInstanceArgs) Kind() string { return "orglane.apps.provision_instance" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ProvisionInstanceArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxProvisionAttempts) }

// ApplyRemovalsArgs apply an instance's planned removals through the guard.
type ApplyRemovalsArgs struct {
	InstanceID uuid.UUID `json:"instance_id" river:"unique"`
	// FollowUp marks the one job queued while another runs.
	FollowUp bool `json:"follow_up,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (ApplyRemovalsArgs) Kind() string { return "orglane.apps.apply_removals" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ApplyRemovalsArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxProvisionAttempts) }

// ProcessDeliveryArgs normalize a stored webhook delivery into events.
type ProcessDeliveryArgs struct {
	DeliveryID uuid.UUID `json:"delivery_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (ProcessDeliveryArgs) Kind() string { return "orglane.apps.process_delivery" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (ProcessDeliveryArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxDefaultAttempts) }

// RotateCredentialArgs hand a credential candidate to its consumer, or
// abandon it.
type RotateCredentialArgs struct {
	InstanceID uuid.UUID `json:"instance_id" river:"unique"`
	Name       string    `json:"name" river:"unique"`
	Version    int32     `json:"version" river:"unique"`
	Abandon    bool      `json:"abandon,omitempty" river:"unique"`
}

// Kind implements river.JobArgs.
func (RotateCredentialArgs) Kind() string { return "orglane.apps.rotate_credential" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (RotateCredentialArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxRotateAttempts) }

// SetupArgs run Lifecycle.Setup of a managed instance.
type SetupArgs struct {
	InstanceID uuid.UUID `json:"instance_id" river:"unique"`
}

// Kind implements river.JobArgs.
func (SetupArgs) Kind() string { return "orglane.apps.setup" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (SetupArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxSetupAttempts) }

// CleanupArgs run the daily cleanup.
type CleanupArgs struct{}

// Kind implements river.JobArgs.
func (CleanupArgs) Kind() string { return "orglane.apps.cleanup" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (CleanupArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxDefaultAttempts) }

// DriftCheckArgs queue the daily drift check of every instance.
type DriftCheckArgs struct{}

// Kind implements river.JobArgs.
func (DriftCheckArgs) Kind() string { return "orglane.apps.drift_check" }

// InsertOpts implements river.JobArgsWithInsertOpts.
func (DriftCheckArgs) InsertOpts() river.InsertOpts { return uniqueOpts(maxDefaultAttempts) }

// DailyInterval is how often cleanup and drift checks run.
const DailyInterval = 24 * time.Hour

// Register adds the apps workers to workers.
func Register(workers *river.Workers, s *Service) error {
	for _, add := range []func() error{
		func() error { return river.AddWorkerSafely(workers, &provisionPersonWorker{s: s}) },
		func() error { return river.AddWorkerSafely(workers, &provisionInstanceWorker{s: s}) },
		func() error { return river.AddWorkerSafely(workers, &applyRemovalsWorker{s: s}) },
		func() error { return river.AddWorkerSafely(workers, &processDeliveryWorker{s: s}) },
		func() error { return river.AddWorkerSafely(workers, &rotateCredentialWorker{s: s}) },
		func() error { return river.AddWorkerSafely(workers, &setupWorker{s: s}) },
		func() error { return river.AddWorkerSafely(workers, &cleanupWorker{s: s}) },
		func() error { return river.AddWorkerSafely(workers, &driftCheckWorker{s: s}) },
	} {
		if err := add(); err != nil {
			return fmt.Errorf("apps: register worker: %w", err)
		}
	}
	return nil
}

// PeriodicJobs are the daily cleanup and drift check.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(DailyInterval),
			func() (river.JobArgs, *river.InsertOpts) { return CleanupArgs{}, nil }, &river.PeriodicJobOpts{RunOnStart: true}),
		river.NewPeriodicJob(river.PeriodicInterval(DailyInterval),
			func() (river.JobArgs, *river.InsertOpts) { return DriftCheckArgs{}, nil }, nil),
	}
}

type provisionPersonWorker struct {
	river.WorkerDefaults[ProvisionPersonArgs]
	s *Service
}

func (w *provisionPersonWorker) Work(ctx context.Context, job *river.Job[ProvisionPersonArgs]) error {
	return w.s.RunProvisionPerson(ctx, job.Args, job.Attempt, job.MaxAttempts)
}

func (w *provisionPersonWorker) Timeout(*river.Job[ProvisionPersonArgs]) time.Duration {
	return 5 * time.Minute
}

type provisionInstanceWorker struct {
	river.WorkerDefaults[ProvisionInstanceArgs]
	s *Service
}

func (w *provisionInstanceWorker) Work(ctx context.Context, job *river.Job[ProvisionInstanceArgs]) error {
	return w.s.RunProvisionInstance(ctx, job.Args)
}

func (w *provisionInstanceWorker) Timeout(*river.Job[ProvisionInstanceArgs]) time.Duration {
	return 10 * time.Minute
}

type applyRemovalsWorker struct {
	river.WorkerDefaults[ApplyRemovalsArgs]
	s *Service
}

func (w *applyRemovalsWorker) Work(ctx context.Context, job *river.Job[ApplyRemovalsArgs]) error {
	return w.s.RunApplyRemovals(ctx, job.Args, job.Attempt, job.MaxAttempts)
}

func (w *applyRemovalsWorker) Timeout(*river.Job[ApplyRemovalsArgs]) time.Duration {
	return 10 * time.Minute
}

type processDeliveryWorker struct {
	river.WorkerDefaults[ProcessDeliveryArgs]
	s *Service
}

func (w *processDeliveryWorker) Work(ctx context.Context, job *river.Job[ProcessDeliveryArgs]) error {
	return w.s.RunProcessDelivery(ctx, job.Args)
}

type rotateCredentialWorker struct {
	river.WorkerDefaults[RotateCredentialArgs]
	s *Service
}

func (w *rotateCredentialWorker) Work(ctx context.Context, job *river.Job[RotateCredentialArgs]) error {
	return w.s.RunRotateCredential(ctx, job.Args)
}

// NextRetry backs off exponentially, capped at an hour.
func (w *rotateCredentialWorker) NextRetry(job *river.Job[RotateCredentialArgs]) time.Time {
	d := time.Hour
	if job.Attempt < 12 {
		d = min(time.Duration(1<<job.Attempt)*time.Second, time.Hour)
	}
	return time.Now().Add(d)
}

type setupWorker struct {
	river.WorkerDefaults[SetupArgs]
	s *Service
}

func (w *setupWorker) Work(ctx context.Context, job *river.Job[SetupArgs]) error {
	return w.s.RunSetup(ctx, job.Args, job.Attempt, job.MaxAttempts)
}

type cleanupWorker struct {
	river.WorkerDefaults[CleanupArgs]
	s *Service
}

func (w *cleanupWorker) Work(ctx context.Context, _ *river.Job[CleanupArgs]) error {
	_, err := w.s.Cleanup(ctx)
	return err
}

type driftCheckWorker struct {
	river.WorkerDefaults[DriftCheckArgs]
	s *Service
}

func (w *driftCheckWorker) Work(ctx context.Context, _ *river.Job[DriftCheckArgs]) error {
	return w.s.RunDriftCheck(ctx)
}

// enqueue inserts a job in tx.
func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, args river.JobArgs) error {
	queue, err := s.getQueue()
	if err != nil {
		return err
	}
	if _, err := queue.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("apps: queue %s: %w", args.Kind(), err)
	}
	return nil
}

// enqueueOnce inserts a unique job; when its twin is already running (and
// may have read the state before this change), it inserts the follow-up
// variant instead, so the change is never lost.
func (s *Service) enqueueOnce(ctx context.Context, tx pgx.Tx, args, followUp river.JobArgs, opts *river.InsertOpts) error {
	queue, err := s.getQueue()
	if err != nil {
		return err
	}
	res, err := queue.InsertTx(ctx, tx, args, opts)
	if err != nil {
		return fmt.Errorf("apps: queue %s: %w", args.Kind(), err)
	}
	if res.UniqueSkippedAsDuplicate && res.Job != nil && res.Job.State == rivertype.JobStateRunning {
		if _, err := queue.InsertTx(ctx, tx, followUp, opts); err != nil {
			return fmt.Errorf("apps: queue %s follow-up: %w", args.Kind(), err)
		}
	}
	return nil
}

// enqueueInstance queues provision_instance for an instance.
func (s *Service) enqueueInstance(ctx context.Context, tx pgx.Tx, instanceID uuid.UUID, reason string) error {
	return s.enqueueOnce(ctx, tx,
		ProvisionInstanceArgs{InstanceID: instanceID, Reason: reason},
		ProvisionInstanceArgs{InstanceID: instanceID, Reason: reason, FollowUp: true}, nil)
}

// enqueuePerson marks a person's result queued and queues provision_person.
// At most one job per (instance, person) waits; when one is already
// running, a single follow-up is queued so a change during the run is not
// lost.
func (s *Service) enqueuePerson(ctx context.Context, tx pgx.Tx, q *store.Queries, instanceID, personID uuid.UUID, drift bool, retry ...bool) error {
	queue, err := s.getQueue()
	if err != nil {
		return err
	}
	if err := q.MarkProvisioningQueued(ctx, store.MarkProvisioningQueuedParams{InstanceID: instanceID, PersonID: personID}); err != nil {
		return fmt.Errorf("apps: mark result queued: %w", err)
	}
	args := ProvisionPersonArgs{InstanceID: instanceID, PersonID: personID, Drift: drift, Retry: len(retry) > 0 && retry[0]}
	res, err := queue.InsertTx(ctx, tx, args, nil)
	if err != nil {
		return fmt.Errorf("apps: queue provisioning: %w", err)
	}
	if res.UniqueSkippedAsDuplicate && res.Job != nil && res.Job.State == rivertype.JobStateRunning {
		args.FollowUp = true
		if _, err := queue.InsertTx(ctx, tx, args, nil); err != nil {
			return fmt.Errorf("apps: queue provisioning follow-up: %w", err)
		}
	}
	return nil
}

// enqueueRemovals queues apply_removals for an instance after delay.
func (s *Service) enqueueRemovals(ctx context.Context, tx pgx.Tx, instanceID uuid.UUID, delay time.Duration) error {
	opts := ApplyRemovalsArgs{}.InsertOpts()
	if delay > 0 {
		opts.ScheduledAt = s.cfg.Now().Add(delay)
	}
	return s.enqueueOnce(ctx, tx, ApplyRemovalsArgs{InstanceID: instanceID}, ApplyRemovalsArgs{InstanceID: instanceID, FollowUp: true}, &opts)
}
