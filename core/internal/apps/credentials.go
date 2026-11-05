package apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// Credential states (docs/specs/providers.md "Credentials").
const (
	credentialPending   = "pending"
	credentialUncertain = "uncertain"
	// credentialAbandoning: an admin abandoned the candidate; it still
	// verifies incoming deliveries until the active value is re-applied.
	credentialAbandoning = "abandoning"
	credentialActive     = "active"
	credentialRetiring   = "retiring"
	credentialDiscarded  = "discarded"
)

// defaultOverlap is how long a replaced version stays valid when the spec
// declares no overlap.
const defaultOverlap = time.Hour

// stuckAfter is when an uncertain candidate is reported to admins.
const stuckAfter = 24 * time.Hour

// credentialAD binds a sealed credential to its row.
func credentialAD(instanceID uuid.UUID, name string, version int32) []byte {
	return []byte("app-credential\x00" + instanceID.String() + "\x00" + name + "\x00" + strconv.Itoa(int(version)))
}

// storeCredential seals and stores a new version of a credential. The
// caller holds the instance row lock.
func (s *Service) storeCredential(ctx context.Context, q *store.Queries, actor audit.Actor, inst Instance, name string, value []byte, state string) (store.AppCredential, error) {
	version, err := q.NextAppCredentialVersion(ctx, store.NextAppCredentialVersionParams{InstanceID: inst.ID, Name: name})
	if err != nil {
		return store.AppCredential{}, fmt.Errorf("apps: next credential version: %w", err)
	}
	row, err := q.InsertAppCredentialVersion(ctx, store.InsertAppCredentialVersionParams{
		InstanceID: inst.ID,
		Name:       name,
		Version:    version,
		Ciphertext: s.creds.Seal(value, credentialAD(inst.ID, name, version)),
		State:      state,
	})
	if err != nil {
		return store.AppCredential{}, fmt.Errorf("apps: store credential: %w", err)
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    actor,
		Action:   audit.AppCredentialCreated,
		Target:   instanceTarget(inst.ID),
		Metadata: credentialMetadata(inst, row, "state", state),
	})
	return row, err
}

// credentialMetadata is the audit metadata of a credential row: provider,
// name, version and the row's durable ID, never a value.
func credentialMetadata(inst Instance, row store.AppCredential, extra ...string) map[string]any {
	m := map[string]any{"provider": inst.Provider, "name": row.Name, "version": row.Version, "credential_id": row.ID.String()}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

// StoreCredential stores a credential obtained from the provider or an
// admin as the new active version, without handing it to the provider; the
// previous active version retires after the spec's overlap. It is refused
// with ErrRotationInProgress while a rotation candidate exists.
func (s *Service) StoreCredential(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, name string, value []byte) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, instanceID, true)
		if err != nil {
			return err
		}
		if inst.Adapter == nil {
			return ErrProviderUnavailable
		}
		spec, ok := inst.Adapter.Credential(name)
		if !ok {
			return fmt.Errorf("%w: credential", ErrNotFound)
		}
		return s.replaceActiveCredential(ctx, q, actor, inst, spec, value)
	})
}

// credentialLockID is the per-credential session lock that serializes
// rotation workers (concurrency model, lock 1).
func credentialLockID(instanceID uuid.UUID, name string) int64 {
	return lockID("credential", instanceID.String(), name)
}

// Provider call leases (concurrency model, "Provider calls in flight").
const (
	// credentialCallTimeout bounds one ApplyCredential call, all of its
	// requests included: the call's context ends then.
	credentialCallTimeout = 2 * time.Minute
	// credentialCallMargin is how much longer the in-flight marker lasts
	// than the call may: a request whose answer was lost can still be
	// processed by the provider shortly after the client gave up.
	credentialCallMargin = time.Minute
)

// errCallInFlight makes a job retry: an execution of the credential may
// still be in flight.
var errCallInFlight = errors.New("apps: a provider call for the credential is in flight")

// errLeaseLost makes a job retry: an execution got a definite answer but its
// lease ended before it could record it, so it recorded nothing.
var errLeaseLost = errors.New("apps: the provider call's lease ended before its result was recorded")

// callInFlight reports whether an execution of the credential (any row) may
// still be in flight. The caller holds the instance row lock, which every
// execution record change also takes.
func callInFlight(ctx context.Context, q *store.Queries, instanceID uuid.UUID, name string) (bool, error) {
	busy, err := q.AppCredentialCallInFlight(ctx, store.AppCredentialCallInFlightParams{InstanceID: instanceID, Name: name})
	if err != nil {
		return false, fmt.Errorf("apps: check provider calls: %w", err)
	}
	return busy, nil
}

// replaceActiveCredential is the direct replacement of the active version
// (StoreCredential, connect). The caller holds the instance row lock. It is
// refused with ErrRotationInProgress while a candidate exists or a provider
// call for the credential is in flight: superseding a pending or uncertain
// candidate would drop a value the provider may already use, letting the
// candidate finish later would retire the newer value, and a call still in
// flight (a retry of a candidate proof by use already activated, or a
// re-apply) could overwrite the new value at the provider after this commit
// (concurrency model, "Direct replacement" and "Provider calls in flight").
func (s *Service) replaceActiveCredential(ctx context.Context, q *store.Queries, actor audit.Actor, inst Instance, spec provider.CredentialSpec, value []byte) error {
	if _, err := q.GetAppCredentialCandidate(ctx, store.GetAppCredentialCandidateParams{InstanceID: inst.ID, Name: spec.Name}); err == nil {
		return ErrRotationInProgress
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("apps: load candidate: %w", err)
	}
	if busy, err := callInFlight(ctx, q, inst.ID, spec.Name); err != nil {
		return err
	} else if busy {
		return ErrRotationInProgress
	}
	if err := q.RetireActiveAppCredential(ctx, store.RetireActiveAppCredentialParams{InstanceID: inst.ID, Name: spec.Name, RetireAt: ptr(s.cfg.Now().Add(overlap(spec)))}); err != nil {
		return fmt.Errorf("apps: retire credential: %w", err)
	}
	_, err := s.storeCredential(ctx, q, actor, inst, spec.Name, value, credentialActive)
	return err
}

// lockCredentials takes the instance row lock, the second lock of the
// order, at the start of a transaction that changes credential rows outside
// the admin paths (which lock it by loading the instance for update).
func lockCredentials(ctx context.Context, q *store.Queries, instanceID uuid.UUID) error {
	if _, err := q.GetAppInstanceForUpdate(ctx, instanceID); errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: app instance", ErrNotFound)
	} else if err != nil {
		return fmt.Errorf("apps: lock instance: %w", err)
	}
	return nil
}

func (s *Service) openCredential(row store.AppCredential) ([]byte, error) {
	plain, err := s.creds.Open(row.Ciphertext, credentialAD(row.InstanceID, row.Name, row.Version))
	if err != nil {
		return nil, fmt.Errorf("apps: open credential %s v%d: %w", row.Name, row.Version, err)
	}
	return plain, nil
}

// Credential is a credential version without its value.
type Credential = store.ListAppCredentialsRow

// Credentials returns the credential versions of an instance, without
// values, discarded ones excluded.
func (s *Service) Credentials(ctx context.Context, instanceID uuid.UUID) ([]Credential, error) {
	rows, err := store.New(s.pool).ListAppCredentials(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("apps: list credentials: %w", err)
	}
	return rows, nil
}

// RotationStuck reports whether a candidate of the instance has been
// uncertain for longer than a day.
func RotationStuck(creds []Credential, now time.Time) bool {
	for _, c := range creds {
		if (c.State == credentialUncertain || c.State == credentialAbandoning) && now.Sub(c.CreatedAt) > stuckAfter {
			return true
		}
	}
	return false
}

// RotateCredential starts a staged rotation: the new version is stored as
// pending and handed to the provider by the rotate_credential job. The
// active version stays in use until the provider acknowledges the new one.
func (s *Service) RotateCredential(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, name string) (int32, error) {
	var version int32
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, spec, err := s.rotatable(ctx, q, instanceID, name)
		if err != nil {
			return err
		}
		if spec.Source == provider.SourceUpload {
			return ErrNotSupported
		}
		var value []byte // minted by the provider
		if spec.Source == provider.SourceGenerated || spec.GenerateRotation {
			value = []byte(crypto.RandomToken(""))
		}
		row, err := s.startRotation(ctx, tx, q, actor, inst, name, value)
		version = row.Version
		return err
	})
	return version, err
}

// startRotation stores a pending candidate, records rotation_started and
// queues its job. The caller holds the instance row lock.
func (s *Service) startRotation(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, inst Instance, name string, value []byte) (store.AppCredential, error) {
	if _, err := q.GetAppCredentialCandidate(ctx, store.GetAppCredentialCandidateParams{InstanceID: inst.ID, Name: name}); err == nil {
		return store.AppCredential{}, ErrRotationInProgress
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.AppCredential{}, fmt.Errorf("apps: load candidate: %w", err)
	}
	// The previous rotation's call may still be in flight (its candidate
	// proven by use meanwhile): the new value must not be overtaken by it.
	if busy, err := callInFlight(ctx, q, inst.ID, name); err != nil {
		return store.AppCredential{}, err
	} else if busy {
		return store.AppCredential{}, ErrRotationInProgress
	}
	row, err := s.storeCredential(ctx, q, actor, inst, name, value, credentialPending)
	if err != nil {
		return store.AppCredential{}, mapCandidateConflict(err)
	}
	if _, err := audit.Record(ctx, q, audit.Entry{
		Actor:    actor,
		Action:   audit.AppCredentialRotationStarted,
		Target:   instanceTarget(inst.ID),
		Metadata: credentialMetadata(inst, row),
	}); err != nil {
		return store.AppCredential{}, err
	}
	return row, s.enqueue(ctx, tx, RotateCredentialArgs{InstanceID: inst.ID, Name: name, Version: row.Version, CredentialID: row.ID})
}

// AbandonRotation gives up an uncertain (or pending) candidate. The job
// first re-applies the active value to the provider until it is
// acknowledged, and only then discards the candidate.
func (s *Service) AbandonRotation(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, name string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, _, err := s.rotatable(ctx, q, instanceID, name)
		if err != nil {
			return err
		}
		// Instance, then candidate: a verification that won commits before
		// this lock is granted, and the candidate is then gone.
		cand, err := q.GetAppCredentialCandidateForUpdate(ctx, store.GetAppCredentialCandidateForUpdateParams{InstanceID: instanceID, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoRotation
		} else if err != nil {
			return fmt.Errorf("apps: load candidate: %w", err)
		}
		// The intent is persisted, so a forward attempt that runs after it
		// never installs the candidate again.
		n, err := q.SetAppCredentialStateFrom(ctx, store.SetAppCredentialStateFromParams{
			ID: cand.ID, State: credentialAbandoning, FromStates: []string{credentialPending, credentialUncertain},
		})
		if err != nil {
			return fmt.Errorf("apps: mark candidate abandoning: %w", err)
		}
		if n == 0 {
			// Already abandoning: no second intent, audit entry or job.
			return ErrNoRotation
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AppCredentialRotationAbandoned,
			Target:   instanceTarget(instanceID),
			Metadata: credentialMetadata(inst, cand, "stage", "requested"),
		}); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, RotateCredentialArgs{InstanceID: instanceID, Name: name, Version: cand.Version, CredentialID: cand.ID, Abandon: true})
	})
}

func (s *Service) rotatable(ctx context.Context, q *store.Queries, instanceID uuid.UUID, name string) (Instance, provider.CredentialSpec, error) {
	inst, err := s.loadInstance(ctx, q, instanceID, true)
	if err != nil {
		return Instance{}, provider.CredentialSpec{}, err
	}
	if inst.State == StateRemoved || inst.State == StateRemoving {
		return Instance{}, provider.CredentialSpec{}, ErrInvalidState
	}
	if inst.Adapter == nil {
		return Instance{}, provider.CredentialSpec{}, ErrProviderUnavailable
	}
	spec, ok := inst.Adapter.Credential(name)
	if !ok {
		return Instance{}, provider.CredentialSpec{}, fmt.Errorf("%w: credential", ErrNotFound)
	}
	if !spec.Rotatable {
		return Instance{}, provider.CredentialSpec{}, fmt.Errorf("%w: the credential is not rotated by Orglane", ErrNotSupported)
	}
	if spec.Agent {
		// The agent consumes it through a new desired generation (#59, #60).
		return Instance{}, provider.CredentialSpec{}, fmt.Errorf("%w: agent credentials rotate with the managed service", ErrNotSupported)
	}
	if _, ok := inst.Adapter.Rotator(); !ok {
		return Instance{}, provider.CredentialSpec{}, ErrNotSupported
	}
	return inst, spec, nil
}

func mapCandidateConflict(err error) error {
	if isUnique(err, "app_credentials_pending_key") {
		return ErrRotationInProgress
	}
	return err
}

// errRotationUnknown makes the job retry: the provider may or may not have
// committed the change.
var errRotationUnknown = errors.New("apps: the provider's answer to a credential change is unknown")

// errRejectedReapply makes the abandon job retry: the provider rejected the
// re-applied active value.
var errRejectedReapply = errors.New("apps: the provider rejected the re-applied active value")

// errRejectedAfterUnknown makes the job retry: the provider rejected this
// attempt, but an earlier attempt's outcome is unknown, so the candidate
// may be in use and stays uncertain.
var errRejectedAfterUnknown = errors.New("apps: the provider rejected a retry after an unknown outcome")

// RunRotateCredential works one attempt of a rotate_credential job. Forward
// and abandon attempts for one credential are serialized by a session lock
// held across the provider call; everything else runs on the lock's
// connection (concurrency model in docs/specs/providers.md "Credentials").
func (s *Service) RunRotateCredential(ctx context.Context, args RotateCredentialArgs) error {
	held, err := s.holdLock(ctx, credentialLockID(args.InstanceID, args.Name))
	if err != nil {
		return err
	}
	defer held.release()
	q := held.queries()
	inst, err := s.loadInstance(ctx, q, args.InstanceID, false)
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	} else if err != nil {
		return err
	}
	if inst.Adapter == nil {
		return river.JobCancel(ErrProviderUnavailable)
	}
	rotator, ok := inst.Adapter.Rotator()
	spec, okSpec := inst.Adapter.Credential(args.Name)
	if !ok || !okSpec {
		return river.JobCancel(ErrNotSupported)
	}
	cand, err := q.GetAppCredential(ctx, store.GetAppCredentialParams{InstanceID: args.InstanceID, Name: args.Name, Version: args.Version})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // cleaned up
	} else if err != nil {
		return fmt.Errorf("apps: load candidate: %w", err)
	}
	if args.CredentialID != uuid.Nil && cand.ID != args.CredentialID {
		return nil // a later row reusing the version; this job's row is gone
	}
	env := held.env(inst)
	if args.Abandon {
		if cand.State != credentialAbandoning {
			return nil // resolved before the abandon request (acknowledged, discarded)
		}
		return s.abandon(ctx, held, inst, rotator, env, cand)
	}
	if cand.State != credentialPending && cand.State != credentialUncertain {
		return nil // resolved by another path (proof by use, abandoned, an earlier attempt)
	}
	var value []byte
	if spec.Source == provider.SourceGenerated || spec.Source == provider.SourceUpload || spec.GenerateRotation {
		if value, err = s.openCredential(cand); err != nil {
			return river.JobCancel(err)
		}
	}
	defer clear(value)
	// A retry after an unknown outcome repeats the same request only when the
	// core holds the value. A value the provider mints differs per attempt:
	// another attempt could invalidate the one the earlier attempt minted,
	// unless the adapter reads back what that attempt did (ReadBack).
	minted := value == nil
	unsafeRetry := false
	call, err := s.beginCall(ctx, held, inst, args.Name, cand.ID, func(_ *store.Queries, cur *store.AppCredential) (bool, error) {
		if cur == nil || cur.ID != cand.ID || (cur.State != credentialPending && cur.State != credentialUncertain) {
			return false, nil
		}
		unsafeRetry = cur.State == credentialUncertain && minted && !spec.ReadBack
		return !unsafeRetry, nil
	})
	if err != nil {
		return err
	}
	if call == nil {
		if unsafeRetry {
			// Stays uncertain until evidence, abandon or an admin's
			// resolution; the stuck report applies.
			env.Logger().WarnContext(ctx, "not retrying a provider-minted credential whose earlier answer was lost", "name", cand.Name, "version", cand.Version)
			return s.reportStuck(ctx, held.conn, inst, cand)
		}
		return nil // resolved meanwhile (abandoned, proven by use)
	}
	// An attempt that starts from uncertain follows one whose outcome is
	// unknown (a lost answer, a dying worker), which may have installed the
	// candidate. Uploads are never installed by Orglane (the admin did that
	// at the provider; ApplyCredential only verifies them), so every
	// rejection of an upload is definitive.
	afterUnknown := call.from == credentialUncertain
	definitive := !afterUnknown || spec.Source == provider.SourceUpload
	callCtx, cancel := call.context(ctx)
	res, err := rotator.ApplyCredential(callCtx, env, inst.View(), args.Name, int(args.Version), value)
	cancel()
	defer clear(res.Value)
	if err != nil {
		res.Outcome = provider.RotationUnknown
	}
	switch {
	case res.Outcome == provider.RotationAcknowledged:
		return held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
			if err := call.end(ctx, q, inst.ID); err != nil {
				return err // lease lost: record nothing, retry
			}
			return s.acknowledge(ctx, q, inst, spec, cand.ID, res.Value, "applied")
		})
	case res.Outcome == provider.RotationRejected && definitive:
		// Nothing was ever installed: the first handover, or an upload.
		return held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
			if err := call.end(ctx, q, inst.ID); err != nil {
				return err // lease lost: record nothing, retry
			}
			n, err := q.SetAppCredentialStateFrom(ctx, store.SetAppCredentialStateFromParams{
				ID: cand.ID, State: credentialDiscarded, FromStates: []string{credentialUncertain},
			})
			if err != nil {
				return fmt.Errorf("apps: discard candidate: %w", err)
			}
			if n == 0 {
				return nil // abandoned meanwhile: the abandon job re-applies the active value
			}
			_, err = audit.Record(ctx, q, audit.Entry{
				Actor:    audit.System,
				Action:   audit.AppCredentialRotationFailed,
				Target:   instanceTarget(inst.ID),
				Metadata: credentialMetadata(inst, cand),
			})
			return err
		})
	case res.Outcome == provider.RotationRejected:
		// A rejection that proves nothing about the earlier attempt: the
		// candidate stays uncertain and is retried, but this execution has
		// its definite answer and ends.
		if err := s.endRejected(ctx, held, inst, call); err != nil {
			return err
		}
		if err := s.reportStuck(ctx, held.conn, inst, cand); err != nil {
			return err
		}
		return errRejectedAfterUnknown
	default:
		// Unknown: the candidate stays uncertain and is retried. The
		// execution keeps its lease (the provider may still apply it); only
		// evidence resolves the candidate.
		if err := s.reportStuck(ctx, held.conn, inst, cand); err != nil {
			return err
		}
		return rotationRetry(err)
	}
}

// endRejected ends an execution after a definite rejection that changes no
// credential row (a retry after an unknown outcome, an abandon re-apply): the
// call is over, so its lease must not defer the next attempt or an admin's
// resolution. If its lease already ended, nothing is changed either way.
func (s *Service) endRejected(ctx context.Context, held *heldConn, inst Instance, call *providerCall) error {
	err := held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		return call.end(ctx, q, inst.ID)
	})
	if errors.Is(err, errLeaseLost) {
		return nil
	}
	return err
}

// rotationRetry is how an attempt whose outcome is unknown retries: a
// provider's RetryAfter snoozes the job until the reset without spending an
// attempt; anything else retries with backoff.
func rotationRetry(err error) error {
	if err == nil {
		return errRotationUnknown
	}
	code, retryable, retryAfter := provider.Classify(err)
	if retryable && retryAfter > 0 {
		return river.JobSnooze(retryAfter)
	}
	return fmt.Errorf("%w (%s)", errRotationUnknown, code)
}

// providerCall is one execution of a provider call, recorded with its own
// lease on the row whose value it writes (concurrency model, "Provider calls
// in flight").
type providerCall struct {
	row   uuid.UUID
	token uuid.UUID
	// deadline ends the call strictly before its lease expires: the lease
	// was taken after it was computed, by database time, and lasts
	// credentialCallMargin longer (local intervals only, no clock compare).
	deadline time.Time
	// from is the candidate's state when the call began ("" without one).
	from string
}

// beginCall records an execution writing row's value of credential name, in
// one transaction that rechecks the situation under the lock order: valid
// gets the locked candidate (nil without one) and decides whether the call
// is still to be made; beginCall returns nil if not. At most one execution
// per credential holds a lease: while any does, this one is deferred
// (errCallInFlight), the same row included. A pending candidate becomes
// uncertain here, from the moment it is handed over: a worker dying mid-call
// must not leave it looking untried.
func (s *Service) beginCall(ctx context.Context, held *heldConn, inst Instance, name string, row uuid.UUID, valid func(q *store.Queries, cand *store.AppCredential) (bool, error)) (*providerCall, error) {
	call := &providerCall{row: row, token: uuid.New()}
	start := time.Now() // before the lease is taken, so the call ends first
	begun := false
	err := held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if err := lockCredentials(ctx, q, inst.ID); err != nil {
			return err
		}
		var cand *store.AppCredential
		if c, err := q.GetAppCredentialCandidateForUpdate(ctx, store.GetAppCredentialCandidateForUpdateParams{InstanceID: inst.ID, Name: name}); err == nil {
			cand = &c
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("apps: lock candidate: %w", err)
		}
		if ok, err := valid(q, cand); err != nil || !ok {
			return err
		}
		if busy, err := callInFlight(ctx, q, inst.ID, name); err != nil {
			return err
		} else if busy {
			return errCallInFlight
		}
		if cand != nil {
			call.from = cand.State
			if cand.State == credentialPending && cand.ID == row {
				if _, err := q.SetAppCredentialStateFrom(ctx, store.SetAppCredentialStateFromParams{
					ID: cand.ID, State: credentialUncertain, FromStates: []string{credentialPending},
				}); err != nil {
					return fmt.Errorf("apps: mark candidate uncertain: %w", err)
				}
			}
		}
		if err := q.InsertAppCredentialCall(ctx, store.InsertAppCredentialCallParams{
			ID: call.token, CredentialID: row, LeaseSeconds: (credentialCallTimeout + credentialCallMargin).Seconds(),
		}); err != nil {
			return fmt.Errorf("apps: record provider call: %w", err)
		}
		begun = true
		return nil
	})
	if err != nil || !begun {
		return nil, err
	}
	call.deadline = start.Add(credentialCallTimeout)
	return call, nil
}

// context bounds the provider call by its deadline.
func (c *providerCall) context(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, c.deadline)
}

// end ends this execution after a definite answer, in the transaction
// recording it. When its lease no longer holds it returns errLeaseLost: the
// result is not recorded, and the job retries rather than complete as if
// the candidate were resolved.
func (c *providerCall) end(ctx context.Context, q *store.Queries, instanceID uuid.UUID) error {
	if err := lockCredentials(ctx, q, instanceID); err != nil {
		return err
	}
	n, err := q.EndAppCredentialCall(ctx, c.token)
	if err != nil {
		return fmt.Errorf("apps: end provider call: %w", err)
	}
	if n != 1 {
		return errLeaseLost
	}
	return nil
}

// acknowledge makes candidate id the active version, unless it was resolved
// meanwhile; the old active version retires after the overlap. It takes the
// instance, then the candidate lock. Because a direct replacement is refused
// while a candidate exists, the active version it retires is the one the
// rotation started from.
func (s *Service) acknowledge(ctx context.Context, q *store.Queries, inst Instance, spec provider.CredentialSpec, id uuid.UUID, minted []byte, how string) error {
	if err := lockCredentials(ctx, q, inst.ID); err != nil {
		return err
	}
	cur, err := q.GetAppCredentialCandidateForUpdate(ctx, store.GetAppCredentialCandidateForUpdateParams{InstanceID: inst.ID, Name: spec.Name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // resolved meanwhile
	} else if err != nil {
		return fmt.Errorf("apps: lock candidate: %w", err)
	}
	if cur.ID != id || (cur.State != credentialPending && cur.State != credentialUncertain) {
		return nil // another candidate, or abandoning
	}
	return s.activate(ctx, q, inst, spec, cur, minted, how)
}

// activate makes the locked candidate cur the active version; the old
// active version retires after the overlap. The caller holds the instance
// and candidate locks and has checked cur.
func (s *Service) activate(ctx context.Context, q *store.Queries, inst Instance, spec provider.CredentialSpec, cur store.AppCredential, minted []byte, how string) error {
	if minted != nil {
		if err := q.SetAppCredentialCiphertext(ctx, store.SetAppCredentialCiphertextParams{
			ID: cur.ID, Ciphertext: s.creds.Seal(minted, credentialAD(cur.InstanceID, cur.Name, cur.Version)),
		}); err != nil {
			return fmt.Errorf("apps: store minted credential: %w", err)
		}
	}
	if err := q.RetireActiveAppCredential(ctx, store.RetireActiveAppCredentialParams{InstanceID: inst.ID, Name: spec.Name, RetireAt: ptr(s.cfg.Now().Add(overlap(spec)))}); err != nil {
		return fmt.Errorf("apps: retire credential: %w", err)
	}
	n, err := q.ActivateAppCredential(ctx, cur.ID)
	if err != nil {
		return fmt.Errorf("apps: activate credential: %w", err)
	}
	if n != 1 {
		// Unreachable: the candidate is locked and was checked by the caller.
		return fmt.Errorf("apps: activate credential %s v%d: not a candidate", cur.Name, cur.Version)
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    audit.System,
		Action:   audit.AppCredentialRotated,
		Target:   instanceTarget(inst.ID),
		Metadata: credentialMetadata(inst, cur, "proof", how),
	})
	return err
}

func (s *Service) abandon(ctx context.Context, held *heldConn, inst Instance, rotator provider.CredentialRotator, env provider.Env, cand store.AppCredential) error {
	isCand := func(_ *store.Queries, cur *store.AppCredential) (bool, error) {
		return cur != nil && cur.ID == cand.ID && cur.State == credentialAbandoning, nil
	}
	// The candidate stays abandoning (and verifies deliveries) until the
	// provider acknowledged the active value again. The active version cannot
	// change while a candidate exists.
	active, err := held.queries().GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: cand.InstanceID, Name: cand.Name})
	if errors.Is(err, pgx.ErrNoRows) {
		// No active version to go back to (the first value of a credential
		// was being rotated in): nothing to re-apply. The candidate is
		// discarded once no call writing it can still be in flight.
		return held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
			if err := lockCredentials(ctx, q, inst.ID); err != nil {
				return err
			}
			if busy, err := callInFlight(ctx, q, inst.ID, cand.Name); err != nil {
				return err
			} else if busy {
				return errCallInFlight
			}
			return s.discardAbandoned(ctx, q, inst, cand)
		})
	} else if err != nil {
		return fmt.Errorf("apps: load active credential: %w", err)
	}
	value, err := s.openCredential(active)
	if err != nil {
		return river.JobCancel(err)
	}
	defer clear(value)
	// A forward call still in flight could install the candidate after the
	// re-apply: beginCall defers the re-apply until its lease expired.
	call, err := s.beginCall(ctx, held, inst, cand.Name, active.ID, isCand)
	if err != nil || call == nil {
		return err
	}
	callCtx, cancel := call.context(ctx)
	res, err := rotator.ApplyCredential(callCtx, env, inst.View(), cand.Name, int(active.Version), value)
	cancel()
	clear(res.Value)
	if err != nil || res.Outcome != provider.RotationAcknowledged {
		if err == nil && res.Outcome == provider.RotationRejected {
			// A definite answer: this re-apply is over. The candidate stays
			// abandoning, and the job re-applies again.
			if err := s.endRejected(ctx, held, inst, call); err != nil {
				return err
			}
		}
		if err := s.reportStuck(ctx, held.conn, inst, cand); err != nil {
			return err
		}
		if err == nil && res.Outcome == provider.RotationRejected {
			return errRejectedReapply
		}
		return rotationRetry(err) // keep re-applying the active value
	}
	return held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if err := call.end(ctx, q, inst.ID); err != nil {
			return err // lease lost: record nothing, retry
		}
		return s.discardAbandoned(ctx, q, inst, cand)
	})
}

// discardAbandoned discards an abandoning candidate. The caller holds the
// instance row lock.
func (s *Service) discardAbandoned(ctx context.Context, q *store.Queries, inst Instance, cand store.AppCredential) error {
	n, err := q.SetAppCredentialStateFrom(ctx, store.SetAppCredentialStateFromParams{
		ID: cand.ID, State: credentialDiscarded, FromStates: []string{credentialAbandoning},
	})
	if err != nil {
		return fmt.Errorf("apps: discard candidate: %w", err)
	}
	if n == 0 {
		return nil
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    audit.System,
		Action:   audit.AppCredentialRotationAbandoned,
		Target:   instanceTarget(inst.ID),
		Metadata: credentialMetadata(inst, cand, "stage", "discarded"),
	})
	return err
}

// reportStuck records rotation_stuck once per candidate row uncertain or
// abandoning for a day. Nothing is retired or discarded automatically. db
// is the caller's held connection, or the pool when it holds none.
func (s *Service) reportStuck(ctx context.Context, db txStarter, inst Instance, cand store.AppCredential) error {
	if s.cfg.Now().Sub(cand.CreatedAt) <= stuckAfter {
		return nil
	}
	return inTxOn(ctx, db, func(_ pgx.Tx, q *store.Queries) error {
		if err := lockCredentials(ctx, q, inst.ID); err != nil {
			return err
		}
		n, err := q.MarkAppCredentialStuckReported(ctx, cand.ID)
		if err != nil {
			return fmt.Errorf("apps: mark stuck report: %w", err)
		}
		if n == 0 {
			return nil // reported before, or resolved meanwhile
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    audit.System,
			Action:   audit.AppCredentialRotationStuck,
			Target:   instanceTarget(inst.ID),
			Metadata: credentialMetadata(inst, cand),
		})
		return err
	})
}

func overlap(spec provider.CredentialSpec) time.Duration {
	if spec.Overlap > 0 {
		return spec.Overlap
	}
	return defaultOverlap
}

func ptr[T any](v T) *T { return &v }

// env is the Env handed to adapters for an instance. Its credential reads
// go through a credentialReader, so they are serialized whatever they run on.
type env struct {
	s     *Service
	inst  Instance
	reads *credentialReader
}

// env reads credentials through the pool: only for callers that hold no
// connection (heldConn.env otherwise).
func (s *Service) env(inst Instance) provider.Env { return s.envOn(inst, s.pool) }

// envOn reads credentials in a transaction the caller runs.
func (s *Service) envOn(inst Instance, db store.DBTX) provider.Env {
	return &env{s: s, inst: inst, reads: newCredentialReader(db)}
}

func (e *env) Credential(ctx context.Context, name string) ([]byte, error) {
	row, err := e.reads.active(ctx, e.inst.ID, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, provider.ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("apps: load credential: %w", err)
	}
	return e.s.openCredential(row)
}

func (e *env) Credentials(ctx context.Context, name string) ([][]byte, error) {
	rows, err := e.reads.usable(ctx, e.inst.ID, name)
	if err != nil {
		return nil, fmt.Errorf("apps: load credentials: %w", err)
	}
	out := make([][]byte, 0, len(rows))
	for _, row := range rows {
		v, err := e.s.openCredential(row)
		if err != nil {
			return nil, err
		}
		if len(v) > 0 { // a candidate the provider has not minted yet
			out = append(out, v)
		}
	}
	return out, nil
}

func (e *env) HTTPClient() *http.Client { return e.s.cfg.HTTPClient }

func (e *env) Logger() *slog.Logger {
	return e.s.cfg.Logger.With("provider", e.inst.Provider, "app_instance", e.inst.ID)
}

func (e *env) Now() time.Time { return e.s.cfg.Now() }

func (e *env) WorkspaceURL() *url.URL { u := *e.s.cfg.WorkspaceURL; return &u }

func (e *env) IssuerURL() *url.URL { u := *e.s.cfg.IssuerURL; return &u }

func (e *env) WebhookURL(instanceID uuid.UUID) *url.URL { return e.s.WebhookURL(instanceID) }

// fixedSecrets is an Env whose Credentials returns given values for one
// name (proof by use).
type fixedSecrets struct {
	provider.Env
	name   string
	values [][]byte
}

func (f fixedSecrets) Credentials(ctx context.Context, name string) ([][]byte, error) {
	if name == f.name {
		return f.values, nil
	}
	return f.Env.Credentials(ctx, name)
}

// UploadCredential stages an uploaded key for verification by the existing
// rotation worker. It cannot bypass pending/uncertain rotation protection.
func (s *Service) UploadCredential(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, name string, value []byte) error {
	if len(value) == 0 || len(value) > 16384 {
		return invalid("value", "must be 1-16384 bytes")
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, instanceID, true)
		if err != nil {
			return err
		}
		if inst.Adapter == nil {
			return ErrProviderUnavailable
		}
		spec, ok := inst.Adapter.Credential(name)
		if !ok {
			return ErrNotFound
		}
		if !spec.Rotatable || spec.Source != provider.SourceUpload || !inst.Provisionable() {
			return ErrNotSupported
		}
		if _, ok := inst.Adapter.Rotator(); !ok {
			return ErrNotSupported
		}
		_, err = s.startRotation(ctx, tx, q, actor, inst, name, value)
		return err
	})
}
