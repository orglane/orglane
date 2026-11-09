package apps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// Result statuses (docs/specs/providers.md "Results").
const (
	StatusQueued                = "queued"
	StatusDone                  = "done"
	StatusPendingEmployeeAction = "pending_employee_action"
	StatusFailed                = "failed"
)

// Action outcomes in results.
const (
	OutcomeChanged      = "changed"
	OutcomeUnchanged    = "unchanged"
	OutcomeFailed       = "failed"
	OutcomeNotAttempted = "not_attempted"
	OutcomeHeld         = "held"
)

// Removal states.
const (
	removalPending = "pending"
	removalHeld    = "held"
	removalApplied = "applied"
	removalDropped = "dropped"
	removalFailed  = "failed"
)

// Removal batch states.
const (
	batchHeld       = "held"
	batchConfirmed  = "confirmed"
	batchApplied    = "applied"
	batchSuperseded = "superseded"
)

// ActionResult is one action of the last plan in a result.
type ActionResult struct {
	Kind    provider.ActionKind `json:"kind"`
	Key     string              `json:"key"`
	Outcome string              `json:"outcome"`
	Removal bool                `json:"removal,omitempty"`
	Error   string              `json:"error_code,omitempty"`
}

// grants are the action kinds by which Orglane creates the account (and so
// owns it, given proof). Adding an existing account to a team proves only
// the team membership, not that the account is Orglane's, so it never makes
// the person's access managed (which would allow removing the account).
var grants = []provider.ActionKind{provider.ActionCreateAccount, provider.ActionInvite}

// sessionLock takes a session-level advisory lock on a dedicated
// connection and returns it with a release function. A dying process
// releases it with its connection.
func sessionLock(ctx context.Context, pool *pgxpool.Pool, id int64) (*pgxpool.Conn, func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("apps: acquire connection: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", id); err != nil {
		conn.Release()
		return nil, nil, fmt.Errorf("apps: lock: %w", err)
	}
	return conn, func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", id); err != nil {
			// Never hand a connection holding the lock back to the pool.
			_ = conn.Conn().Close(ctx)
		}
		conn.Release()
	}, nil
}

// RunProvisionPerson works one attempt of a provision_person job: compute
// desired access, plan, apply the non-removal actions in order (stopping
// at the first failure), route removals to the guard and write the result.
func (s *Service) RunProvisionPerson(ctx context.Context, args ProvisionPersonArgs, attempt, maxAttempts int) error {
	held, err := s.holdLock(ctx, lockID("provision", args.InstanceID.String(), args.PersonID.String()))
	if err != nil {
		return err
	}
	defer held.release()
	conn := held.conn
	var startedAt time.Time
	if err := conn.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&startedAt); err != nil {
		return fmt.Errorf("apps: read clock: %w", err)
	}
	q := store.New(conn)
	inst, err := s.loadInstance(ctx, q, args.InstanceID, false)
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	} else if err != nil {
		return err
	}
	if !inst.Provisionable() || inst.Adapter == nil {
		return nil // provisioning resumes when the instance becomes active again
	}
	prov, ok := inst.Adapter.Provisioner()
	if !ok {
		return nil
	}
	d, err := s.computeDesired(ctx, q, inst, args.PersonID)
	if errors.Is(err, ErrNotFound) {
		return nil // the person is gone (their rows cascade)
	} else if err != nil {
		return err
	}
	if d.result == nil && !d.access.Access && d.access.Link == nil {
		return nil // never had access: nothing to record or remove
	}
	var recovered []ActionResult
	if d.result != nil && d.result.PendingGrant != nil {
		// An earlier run sent a grant whose outcome was never recorded.
		rep, err := s.resolvePendingGrant(ctx, held, inst, prov, d)
		if err != nil {
			return err
		}
		switch {
		case rep.providerErr != nil:
			// Same policy as any failed action: retry, snooze, exhaust or
			// fail permanently, and stop this person's run.
			actions, err := json.Marshal([]ActionResult{rep.result})
			if err != nil {
				return fmt.Errorf("apps: encode actions: %w", err)
			}
			return s.failed(ctx, conn, inst, d, actions, rep.providerErr, attempt, maxAttempts, startedAt)
		case rep.unproven:
			// The access is there, but nothing proves Orglane made it: an
			// admin reviews it. Not adopted, never removed automatically.
			actions, err := json.Marshal([]ActionResult{rep.result})
			if err != nil {
				return fmt.Errorf("apps: encode actions: %w", err)
			}
			code := CodeOwnershipUnproven
			attempts := d.result.Attempts + 1
			err = s.writeResult(ctx, conn, inst, d, StatusFailed, nil, actions, &code, false, nil, attempts, startedAt)
			return s.followUp(err, conn, inst.ID, args.PersonID, startedAt)
		}
		recovered = append(recovered, rep.result)
		if d, err = s.computeDesired(ctx, store.New(conn), inst, args.PersonID); err != nil {
			return err
		}
	}
	if r := d.result; r != nil && r.Status == StatusFailed && !args.Retry && bytes.Equal(r.DesiredHash, d.hash) {
		// A permanent failure is retried when desired access changes or an
		// admin retries, never by drift checks or unrelated changes.
		return s.followUp(nil, conn, inst.ID, args.PersonID, startedAt)
	}
	if r := d.result; r != nil && !args.Drift && len(recovered) == 0 && bytes.Equal(r.AppliedHash, d.hash) && r.ErrorCode == nil && r.EmployeeStep == nil && r.Status != StatusFailed {
		err := s.writeResult(ctx, conn, inst, d, StatusDone, nil, r.Actions, nil, false, r.AppliedHash, 0, startedAt)
		return s.followUp(err, conn, inst.ID, args.PersonID, startedAt)
	}

	env := held.env(inst)
	view := inst.View()
	plan, err := prov.Plan(ctx, env, view, d.access)
	if err != nil {
		return s.failed(ctx, conn, inst, d, nil, err, attempt, maxAttempts, startedAt)
	}
	if plan.EmployeeStep != "" {
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			_, err := store.New(tx).DropPendingRemovals(ctx, store.DropPendingRemovalsParams{InstanceID: inst.ID, PersonID: d.person.ID, KeepKeys: []string{}})
			return err
		}); err != nil {
			return fmt.Errorf("apps: drop removals: %w", err)
		}
		step := plan.EmployeeStep
		err := s.writeResult(ctx, conn, inst, d, StatusPendingEmployeeAction, &step, []byte("[]"), nil, false, nil, 0, startedAt)
		return s.followUp(err, conn, inst.ID, args.PersonID, startedAt)
	}

	// Ownership is recovered on reruns: a link this instance provisioned
	// makes the access Orglane's (computeDesired's Managed). The result's
	// own flag is not passed on: the upsert keeps it, and a link revocation
	// may have cleared it while this run worked (revokeLink). Verifier
	// providers store ownership only in recordApplied (under the link lock);
	// carrying an in-memory flag could restore it after a revocation.
	_, verifies := inst.Adapter.Verifier()
	granted := !verifies && d.access.Link != nil && d.access.Link.Method == provider.LinkProvisioned && d.access.Managed
	results := make([]ActionResult, 0, len(recovered)+len(plan.Actions))
	results = append(results, recovered...)
	var removals []provider.Action
	var applyErr error
	for _, a := range plan.Actions {
		ar := ActionResult{Kind: a.Kind, Key: a.Key, Removal: a.Removal}
		switch {
		case applyErr != nil:
			// Plan order: nothing after the first failure runs, removals
			// included (they would take away access the failed step was
			// to replace).
			ar.Outcome = OutcomeNotAttempted
		case a.Removal:
			if !d.access.Managed {
				// Adapters plan removals only for managed accounts; refuse
				// anything else rather than take access Orglane did not grant.
				env.Logger().WarnContext(ctx, "adapter planned a removal for an unmanaged account; ignored", "kind", a.Kind, "key", a.Key)
				ar.Outcome = OutcomeNotAttempted
			} else {
				removals = append(removals, a)
				ar.Outcome = OutcomeHeld
			}
		default:
			grant := slices.Contains(grants, a.Kind)
			if grant {
				// Durable intent before the provider is touched, kept apart
				// from ownership: if the process dies between the provider
				// call and its bookkeeping, the next run replays the action
				// and records the outcome. A failed write stops here, before
				// any provider change.
				if err := s.setPendingGrant(ctx, conn, inst.ID, d.person.ID, a); err != nil {
					return err
				}
			}
			out, err := prov.Apply(ctx, env, view, a)
			if err != nil {
				applyErr = err
				ar.Outcome = OutcomeFailed
				ar.Error, _, _ = provider.Classify(err)
				if _, retryable, _ := provider.Classify(err); grant && !retryable {
					// A definite rejection changed nothing: no ownership.
					if err := store.New(conn).ClearPendingGrant(ctx, store.ClearPendingGrantParams{InstanceID: inst.ID, PersonID: d.person.ID}); err != nil {
						return fmt.Errorf("apps: clear grant intent: %w", err)
					}
				}
				// A retryable (uncertain) failure keeps the intent: the next
				// run replays the action and resolves it.
				break
			}
			ar.Outcome = OutcomeUnchanged
			if out.Changed {
				ar.Outcome = OutcomeChanged
			}
			owned, err := s.recordApplied(ctx, conn, inst, d.person.ID, a, out, false)
			if err != nil {
				return err
			}
			if owned && !verifies {
				granted = true // only provider proof makes access Orglane's
			}
		}
		results = append(results, ar)
	}
	actions, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("apps: encode actions: %w", err)
	}
	if err := s.planRemovals(ctx, conn, inst, d, removals); err != nil {
		return err
	}
	if applyErr != nil {
		return s.failed(ctx, conn, inst, d, actions, applyErr, attempt, maxAttempts, startedAt, granted)
	}
	status, applied := StatusDone, d.hash
	if len(removals) > 0 {
		// Not done until the guard applied the removals; apply_removals
		// queues the person again afterwards.
		status, applied = StatusQueued, nil
	}
	err = s.writeResult(ctx, conn, inst, d, status, nil, actions, nil, granted, applied, 0, startedAt)
	return s.followUp(err, conn, inst.ID, args.PersonID, startedAt)
}

// recordApplied writes the bookkeeping of an applied action in one
// transaction: the audit entry of a change, ownership of a grant the adapter
// proves Orglane made (ActionOutcome.Proof; this also resolves the pending
// grant) and the verified link of that account. A grant without proof is
// resolved without ownership or link: its intent is cleared. recovered
// marks the replay of an earlier run's grant (recorded in the audit entry).
// It reports whether the grant made the access Orglane's.
//
// For providers whose accounts people prove themselves (AccountVerifier:
// GitHub), a grant is Orglane's only for the account the person's verified
// link names when the bookkeeping commits, checked under the realm's link
// lock: if the link was revoked or replaced while the action ran, the
// granted account is reported as unmanaged instead, so ownership never
// passes to another account of the person.
func (s *Service) recordApplied(ctx context.Context, conn *pgxpool.Conn, inst Instance, personID uuid.UUID, a provider.Action, out provider.ActionOutcome, recovered bool) (bool, error) {
	grant := slices.Contains(grants, a.Kind)
	owned := grant && out.Proof != nil
	if !grant && out.Link != nil {
		// An action that saw the account (e.g. a rename) refreshes the
		// login of the person's verified link of that same account. It never
		// creates, moves or verifies a link: only grants with proof do.
		if err := s.refreshLinkLogin(ctx, conn, inst, personID, *out.Link); err != nil {
			return false, err
		}
	}
	if !out.Changed && !grant {
		return false, nil
	}
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, verifies := inst.Adapter.Verifier(); owned && verifies {
			linked, err := s.linkedAccount(ctx, q, inst, personID, out.Link)
			if err != nil {
				return err
			}
			if !linked {
				owned = false
				if l := out.Link; l != nil && l.ExternalID != "" && len(l.ExternalID) <= 100 && l.Login != "" && len(l.Login) <= 100 {
					if err := q.InsertUnmanagedAccount(ctx, store.InsertUnmanagedAccountParams{InstanceID: inst.ID, ExternalID: l.ExternalID, Login: l.Login}); err != nil {
						return fmt.Errorf("apps: store unmanaged account: %w", err)
					}
				}
			}
		}
		if out.Changed || owned {
			meta := map[string]any{"provider": inst.Provider, "instance_id": inst.ID, "kind": a.Kind, "key": a.Key}
			if recovered {
				meta["recovered"] = true
			}
			if out.Proof != nil {
				meta["proof"] = out.Proof.Kind
				if out.Proof.Evidence != "" {
					meta["evidence"] = out.Proof.Evidence
				}
			}
			if _, err := audit.Record(ctx, q, audit.Entry{
				Actor:    audit.System,
				Action:   audit.ProvisioningActionApplied,
				Target:   audit.Target{Kind: audit.TargetPerson, ID: personID.String()},
				Metadata: meta,
			}); err != nil {
				return err
			}
		}
		if !owned {
			if grant {
				if err := q.ClearPendingGrant(ctx, store.ClearPendingGrantParams{InstanceID: inst.ID, PersonID: personID}); err != nil {
					return fmt.Errorf("apps: clear grant intent: %w", err)
				}
			}
			return nil
		}
		if err := q.MarkProvisioningGranted(ctx, store.MarkProvisioningGrantedParams{InstanceID: inst.ID, PersonID: personID}); err != nil {
			return fmt.Errorf("apps: record grant: %w", err)
		}
		if _, verifies := inst.Adapter.Verifier(); out.Link != nil && !verifies {
			return s.provisionedLink(ctx, tx, q, inst, personID, *out.Link)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return owned, nil
}

// linkedAccount reports whether acct is the account of the person's
// verified link in the instance's realm, under the realm's link lock (link
// changes take it too, so the answer holds until the transaction ends).
func (s *Service) linkedAccount(ctx context.Context, q *store.Queries, inst Instance, personID uuid.UUID, acct *provider.ExternalAccount) (bool, error) {
	if acct == nil {
		return false, nil
	}
	realm := inst.Adapter.Realm(inst.View())
	if err := q.AcquireXactLock(ctx, lockID("account-links", inst.Provider, realm)); err != nil {
		return false, fmt.Errorf("apps: lock links: %w", err)
	}
	cur, err := q.GetVerifiedAccountLink(ctx, store.GetVerifiedAccountLinkParams{PersonID: personID, Provider: inst.Provider, Realm: realm})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("apps: load link: %w", err)
	}
	return cur.ExternalID == acct.ExternalID && (acct.Realm == "" || acct.Realm == realm), nil
}

// setPendingGrant commits the intent of a granting action.
func (s *Service) setPendingGrant(ctx context.Context, conn *pgxpool.Conn, instanceID, personID uuid.UUID, a provider.Action) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("apps: encode grant intent: %w", err)
	}
	if err := store.New(conn).SetPendingGrant(ctx, store.SetPendingGrantParams{InstanceID: instanceID, PersonID: personID, PendingGrant: raw}); err != nil {
		return fmt.Errorf("apps: record grant intent: %w", err)
	}
	return nil
}

// CodeOwnershipUnproven is the code of a result whose replayed grant found the access in place, but
// the adapter cannot prove Orglane's call made it. The access is not
// adopted, never removed automatically, and waits for an admin.
const CodeOwnershipUnproven = "ownership_unproven"

// replay is how resolvePendingGrant settled an interrupted grant.
type replay struct {
	result ActionResult
	// providerErr: the replay failed; handled like any failed action.
	providerErr error
	// unproven: the access is there without proof of Orglane's call.
	unproven bool
}

// resolvePendingGrant settles a grant an earlier run sent without recording
// its outcome (the process died, its bookkeeping failed, the answer was
// lost). Ownership and a verified link come only from provider proof
// (ActionOutcome.Proof), never from the access being there now.
//   - The person still has desired access: the action is replayed as an
//     ensure. With proof, ownership, link and audit entries are recorded as
//     on the first successful path. Without proof but with the access in
//     place, the intent is cleared and the result needs an admin's review
//     (ownership_unproven; the account goes to the unmanaged report). A
//     failure is returned to the normal failure handling (a definite
//     rejection also clears the intent).
//   - The person must no longer have access: nothing is granted. A
//     read-only plan shows whether the access exists; if so it is left to
//     an admin like above (never adopted, never removed), else the intent is
//     simply cleared.
func (s *Service) resolvePendingGrant(ctx context.Context, held *heldConn, inst Instance, prov provider.UserProvisioner, d desired) (replay, error) {
	var a provider.Action
	if err := json.Unmarshal(d.result.PendingGrant, &a); err != nil {
		return replay{}, fmt.Errorf("apps: decode grant intent: %w", err)
	}
	conn := held.conn
	env, view := held.env(inst), inst.View()
	failure := func(err error) (replay, error) {
		code, retryable, _ := provider.Classify(err)
		if !retryable {
			if err := store.New(conn).ClearPendingGrant(ctx, store.ClearPendingGrantParams{InstanceID: inst.ID, PersonID: d.person.ID}); err != nil {
				return replay{}, fmt.Errorf("apps: clear grant intent: %w", err)
			}
		}
		return replay{result: ActionResult{Kind: a.Kind, Key: a.Key, Outcome: OutcomeFailed, Error: code}, providerErr: err}, nil
	}
	unproven := func(link *provider.ExternalAccount) (replay, error) {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			q := store.New(tx)
			if err := q.ClearPendingGrant(ctx, store.ClearPendingGrantParams{InstanceID: inst.ID, PersonID: d.person.ID}); err != nil {
				return fmt.Errorf("apps: clear grant intent: %w", err)
			}
			if link == nil || link.ExternalID == "" || len(link.ExternalID) > 100 || link.Login == "" || len(link.Login) > 100 {
				return nil
			}
			return q.InsertUnmanagedAccount(ctx, store.InsertUnmanagedAccountParams{InstanceID: inst.ID, ExternalID: link.ExternalID, Login: link.Login})
		})
		if err != nil {
			return replay{}, err
		}
		return replay{result: ActionResult{Kind: a.Kind, Key: a.Key, Outcome: OutcomeFailed, Error: CodeOwnershipUnproven}, unproven: true}, nil
	}
	if !d.access.Access {
		probe := d.access
		probe.Access = true
		plan, err := prov.Plan(ctx, env, view, probe)
		if err != nil {
			return failure(err)
		}
		if slices.ContainsFunc(plan.Actions, func(p provider.Action) bool { return p.Key == a.Key }) || plan.EmployeeStep != "" {
			// Not there: the interrupted call changed nothing.
			if err := store.New(conn).ClearPendingGrant(ctx, store.ClearPendingGrantParams{InstanceID: inst.ID, PersonID: d.person.ID}); err != nil {
				return replay{}, fmt.Errorf("apps: clear grant intent: %w", err)
			}
			return replay{result: ActionResult{Kind: a.Kind, Key: a.Key, Outcome: OutcomeNotAttempted}}, nil
		}
		return unproven(nil)
	}
	out, err := prov.Apply(ctx, env, view, a)
	if err != nil {
		return failure(err)
	}
	if out.Proof == nil {
		return unproven(out.Link)
	}
	owned, err := s.recordApplied(ctx, conn, inst, d.person.ID, a, out, true)
	if err != nil {
		return replay{}, err
	}
	if !owned {
		return replay{result: ActionResult{Kind: a.Kind, Key: a.Key, Outcome: OutcomeFailed, Error: CodeOwnershipUnproven}, unproven: true}, nil
	}
	return replay{result: ActionResult{Kind: a.Kind, Key: a.Key, Outcome: OutcomeChanged}}, nil
}

// provisionedLink stores the verified link of an account Orglane created:
// creation is the proof.
func (s *Service) provisionedLink(ctx context.Context, tx pgx.Tx, q *store.Queries, inst Instance, personID uuid.UUID, acct provider.ExternalAccount) error {
	realm := inst.Adapter.Realm(inst.View())
	if acct.Realm != "" && acct.Realm != realm {
		return fmt.Errorf("apps: adapter returned an account of realm %q for realm %q", acct.Realm, realm)
	}
	if acct.ExternalID == "" || len(acct.ExternalID) > 100 || acct.Login == "" || len(acct.Login) > 100 {
		return errors.New("apps: adapter returned an invalid account")
	}
	if cur, err := q.GetVerifiedAccountLink(ctx, store.GetVerifiedAccountLinkParams{PersonID: personID, Provider: inst.Provider, Realm: realm}); err == nil {
		if cur.ExternalID == acct.ExternalID {
			return nil
		}
		return fmt.Errorf("apps: person already has another verified account in realm %s", realm)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("apps: load link: %w", err)
	}
	link, err := q.InsertAccountLink(ctx, store.InsertAccountLinkParams{
		PersonID: personID, Provider: inst.Provider, Realm: realm, ExternalID: acct.ExternalID, Login: acct.Login,
		Method: provider.LinkProvisioned, InstanceID: &inst.ID, Evidence: ptr("none"),
	})
	if err != nil {
		return fmt.Errorf("apps: store link: %w", err)
	}
	if _, err := audit.Record(ctx, q, audit.Entry{
		Actor:    audit.System,
		Action:   audit.AccountLinkVerified,
		Target:   audit.Target{Kind: audit.TargetAccountLink, ID: link.ID.String()},
		Metadata: map[string]any{"provider": inst.Provider, "instance_id": inst.ID, "person_id": personID, "method": provider.LinkProvisioned},
	}); err != nil {
		return err
	}
	return s.linkVerified(ctx, tx, q, inst.Provider, realm, personID, inst.ID)
}

// linkVerified queues provisioning for a person in every other instance of
// the realm once a link is verified (the employee step is complete).
func (s *Service) linkVerified(ctx context.Context, tx pgx.Tx, q *store.Queries, providerID, realm string, personID, except uuid.UUID) error {
	rows, err := q.ListAppInstancesInState(ctx, provisionable)
	if err != nil {
		return fmt.Errorf("apps: list instances: %w", err)
	}
	for _, row := range rows {
		if row.ID == except || row.Provider != providerID {
			continue
		}
		inst, err := s.instance(row)
		if err != nil {
			return err
		}
		if inst.Adapter == nil || inst.Adapter.Realm(inst.View()) != realm {
			continue
		}
		if err := s.enqueuePerson(ctx, tx, q, inst.ID, personID, false); err != nil {
			return err
		}
	}
	return nil
}

// planRemovals upserts the planned removals as pending, drops pending ones
// the plan no longer contains, and queues the guard.
func (s *Service) planRemovals(ctx context.Context, conn *pgxpool.Conn, inst Instance, d desired, removals []provider.Action) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		keep := make([]string, 0, len(removals))
		for _, a := range removals {
			raw, err := json.Marshal(a)
			if err != nil {
				return fmt.Errorf("apps: encode removal: %w", err)
			}
			if err := q.UpsertPendingRemoval(ctx, store.UpsertPendingRemovalParams{
				InstanceID: inst.ID, PersonID: d.person.ID, Action: raw, ActionKey: a.Key, DesiredHash: d.hash,
			}); err != nil {
				return fmt.Errorf("apps: plan removal: %w", err)
			}
			keep = append(keep, a.Key)
		}
		if _, err := q.DropPendingRemovals(ctx, store.DropPendingRemovalsParams{InstanceID: inst.ID, PersonID: d.person.ID, KeepKeys: keep}); err != nil {
			return fmt.Errorf("apps: drop removals: %w", err)
		}
		if len(removals) == 0 {
			return nil
		}
		return s.enqueueRemovals(ctx, tx, inst.ID, removalDelay)
	})
}

// failed records a failed attempt. Retryable errors retry (or snooze until
// the provider's reset without spending an attempt) until the attempts are
// exhausted; others fail the result at once.
func (s *Service) failed(ctx context.Context, conn *pgxpool.Conn, inst Instance, d desired, actions []byte, cause error, attempt, maxAttempts int, startedAt time.Time, granted ...bool) error {
	code, retryable, retryAfter := provider.Classify(cause)
	if actions == nil {
		actions = []byte("[]")
		if d.result != nil {
			actions = d.result.Actions
		}
	}
	g := false // the upsert keeps the result's own flag
	if len(granted) > 0 {
		g = granted[0]
	}
	attempts := int32(1)
	if d.result != nil {
		attempts = d.result.Attempts + 1
	}
	var applied []byte
	if d.result != nil {
		applied = d.result.AppliedHash
	}
	status := StatusFailed
	if retryable && (retryAfter > 0 || attempt < maxAttempts) {
		status = StatusQueued
	}
	if err := s.writeResult(ctx, conn, inst, d, status, nil, actions, &code, g, applied, attempts, startedAt); err != nil {
		return err
	}
	s.env(inst).Logger().WarnContext(ctx, "provisioning failed", "person_id", d.person.ID, "code", code, "retryable", retryable, "attempt", attempt)
	switch {
	case retryable && retryAfter > 0:
		return river.JobSnooze(retryAfter)
	case status == StatusQueued:
		return fmt.Errorf("apps: provisioning failed (%s)", code)
	}
	return nil
}

func (s *Service) writeResult(ctx context.Context, conn *pgxpool.Conn, inst Instance, d desired, status string, step *string, actions []byte, code *string, granted bool, applied []byte, attempts int32, startedAt time.Time) error {
	_, err := store.New(conn).UpsertProvisioningResult(ctx, store.UpsertProvisioningResultParams{
		InstanceID:       inst.ID,
		PersonID:         d.person.ID,
		Status:           status,
		Access:           d.access.Access,
		EmployeeStep:     step,
		GrantedByOrglane: granted,
		DesiredHash:      d.hash,
		AppliedHash:      applied,
		Actions:          actions,
		ErrorCode:        code,
		Attempts:         attempts,
		StartedAt:        &startedAt,
	})
	if err != nil {
		return fmt.Errorf("apps: write result: %w", err)
	}
	return nil
}

// followUp runs the job again when a change was queued while it worked
// (the result's marker is newer than the run's start).
func (s *Service) followUp(err error, conn *pgxpool.Conn, instanceID, personID uuid.UUID, startedAt time.Time) error {
	if err != nil {
		return err
	}
	row, err := store.New(conn).GetProvisioningResult(context.Background(), store.GetProvisioningResultParams{InstanceID: instanceID, PersonID: personID})
	if err != nil {
		return nil //nolint:nilerr // no row: nothing was queued for it
	}
	if row.NextAttemptAt != nil && row.NextAttemptAt.After(startedAt) {
		return river.JobSnooze(time.Second)
	}
	return nil
}

// RunProvisionInstance syncs the instance's provider teams, refreshes the
// unmanaged accounts report and queues provision_person for everyone the
// instance concerns.
func (s *Service) RunProvisionInstance(ctx context.Context, args ProvisionInstanceArgs) error {
	// One run per instance at a time (a follow-up may be queued while one
	// runs): team syncs must not race.
	held, err := s.holdLock(ctx, lockID("instance", args.InstanceID.String()))
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
	if !inst.Provisionable() || inst.Adapter == nil {
		return nil
	}
	prov, ok := inst.Adapter.Provisioner()
	if !ok {
		return nil
	}
	env, view := held.env(inst), inst.View()
	rows, err := q.ListAppTeamMappings(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("apps: list mappings: %w", err)
	}
	mappings := make([]provider.TeamMapping, 0, len(rows))
	for _, m := range rows {
		if m.TeamArchivedAt != nil {
			continue // archived teams keep their mapping but grant nothing
		}
		mappings = append(mappings, provider.TeamMapping{TeamID: m.TeamID, TeamSlug: m.TeamSlug, TeamName: m.TeamName, Mode: m.Mode, ExternalRef: derefString(m.ExternalRef)})
	}
	refs, syncErr := prov.SyncTeams(ctx, env, view, mappings)
	// The teams synced so far are stored first, also when the sync or the
	// member listing fails afterwards: a team created at the provider and
	// not recorded could only be found again by its name.
	if err := held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		for _, ref := range refs {
			i := slices.IndexFunc(mappings, func(m provider.TeamMapping) bool { return m.TeamID == ref.TeamID })
			if i < 0 || ref.ExternalRef == "" || len(ref.ExternalRef) > 100 {
				continue
			}
			name := truncateRunes(strings.ToValidUTF8(ref.Name, ""), 100)
			m := mappings[i]
			var synced *string
			if m.ExternalRef != "" {
				synced = &m.ExternalRef
			}
			if err := q.SetAppTeamMappingExternal(ctx, store.SetAppTeamMappingExternalParams{InstanceID: inst.ID, TeamID: ref.TeamID, ExternalRef: &ref.ExternalRef, ExternalName: &name, Mode: m.Mode, SyncedRef: synced}); err != nil {
				return fmt.Errorf("apps: store provider team: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if syncErr != nil {
		return retryOrCancel(syncErr)
	}
	members, err := prov.Members(ctx, env, view)
	if err != nil {
		return retryOrCancel(err)
	}
	realm := inst.Adapter.Realm(view)
	return held.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if err := s.refreshUnmanaged(ctx, q, inst, realm, members); err != nil {
			return err
		}
		people, err := q.ListProvisioningCandidates(ctx, store.ListProvisioningCandidatesParams{InstanceID: inst.ID, Provider: inst.Provider, Realm: realm})
		if err != nil {
			return fmt.Errorf("apps: list people: %w", err)
		}
		for _, id := range people {
			if err := s.enqueuePerson(ctx, tx, q, inst.ID, id, args.Reason == ReasonDrift); err != nil {
				return err
			}
		}
		return nil
	})
}

// refreshUnmanaged replaces the unmanaged accounts report: provider
// accounts no managed person is linked to. They are reported, never
// removed.
func (s *Service) refreshUnmanaged(ctx context.Context, q *store.Queries, inst Instance, realm string, members []provider.ExternalAccount) error {
	links, err := q.ListRealmAccountLinks(ctx, store.ListRealmAccountLinksParams{Provider: inst.Provider, Realm: realm})
	if err != nil {
		return fmt.Errorf("apps: list links: %w", err)
	}
	managed := map[string]bool{}
	for _, l := range links {
		r, err := q.GetProvisioningResult(ctx, store.GetProvisioningResultParams{InstanceID: inst.ID, PersonID: l.PersonID})
		if err == nil && (r.GrantedByOrglane || inst.ManageExistingMembers) {
			managed[l.ExternalID] = true
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("apps: load result: %w", err)
		}
	}
	if err := q.DeleteUnmanagedAccounts(ctx, inst.ID); err != nil {
		return fmt.Errorf("apps: clear unmanaged accounts: %w", err)
	}
	for _, m := range members {
		if managed[m.ExternalID] || m.ExternalID == "" || len(m.ExternalID) > 100 || m.Login == "" || len(m.Login) > 100 {
			continue
		}
		if err := q.InsertUnmanagedAccount(ctx, store.InsertUnmanagedAccountParams{InstanceID: inst.ID, ExternalID: m.ExternalID, Login: m.Login}); err != nil {
			return fmt.Errorf("apps: store unmanaged account: %w", err)
		}
	}
	return nil
}

// retryOrCancel retries retryable provider errors and gives up on others.
func retryOrCancel(err error) error {
	code, retryable, retryAfter := provider.Classify(err)
	switch {
	case retryAfter > 0:
		return river.JobSnooze(retryAfter)
	case retryable:
		return fmt.Errorf("apps: provider call failed (%s)", code)
	}
	return river.JobCancel(fmt.Errorf("apps: provider call failed (%s)", code))
}

// RunApplyRemovals applies an instance's planned removals through the
// guard (docs/specs/providers.md "Removal guard"): stale removals are
// dropped, small sets apply, large ones are held for confirmation, and
// confirmed batches apply after a freshness check per item.
//
// Durability: every state change of a removal commits together with what
// follows from it (a dropped or applied removal queues its person's
// provision_person job in the same transaction), so a failure of another
// person, a crash or a retry never loses that work. Any error stops only
// the person it concerns; errors are returned only after everything this
// run did is committed, and a RetryAfter snooze takes precedence over them.
//
// Retries: a retryable failure leaves the removal open and retries the job.
// A provider's RetryAfter (a rate limit) snoozes the job until the reset
// without spending an attempt, also on the last attempt, and ends the run's
// provider calls: the provider asked to wait.
func (s *Service) RunApplyRemovals(ctx context.Context, args ApplyRemovalsArgs, attempt, maxAttempts int) error {
	held, err := s.holdLock(ctx, lockID("removals", args.InstanceID.String()))
	if err != nil {
		return err
	}
	defer held.release()
	conn := held.conn
	var startedAt time.Time
	if err := conn.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&startedAt); err != nil {
		return fmt.Errorf("apps: read clock: %w", err)
	}
	inst, err := s.loadInstance(ctx, store.New(conn), args.InstanceID, false)
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	} else if err != nil {
		return err
	}
	if !inst.Provisionable() || inst.Adapter == nil {
		return nil
	}
	prov, ok := inst.Adapter.Provisioner()
	if !ok {
		return nil
	}

	var toApply []queuedRemoval
	var batches []uuid.UUID
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		// People whose removals are dropped as stale are planned again in
		// the same transaction as the drop.
		replan := map[uuid.UUID]bool{}
		var err error
		if toApply, batches, err = s.selectRemovals(ctx, q, inst, startedAt, replan); err != nil {
			return err
		}
		return s.enqueuePeople(ctx, tx, q, inst.ID, replan)
	})
	if err != nil {
		return err
	}

	final := attempt >= maxAttempts
	// retry is the first retryable failure (a provider's or the framework's
	// own, such as a database error); snooze the provider's RetryAfter.
	var retry, failure error
	var snooze time.Duration
	// A person's removals apply in plan order and stop at their first
	// failure; other people go on independently, so one person's broken
	// removal never blocks everyone after it.
	stopped := map[uuid.UUID]bool{}
apply:
	for _, item := range toApply {
		if stopped[item.removal.PersonID] {
			continue
		}
		retryErr, err := s.applyRemoval(ctx, held, prov, item, final)
		switch {
		case errors.Is(err, errRemovalFailed):
			stopped[item.removal.PersonID] = true
			continue
		case err != nil:
			stopped[item.removal.PersonID] = true
			if failure == nil {
				failure = err
			}
			continue
		case errors.Is(retryErr, errInstanceInactive):
			break apply // the rest waits until the instance is provisionable again (activation queues this job)
		case retryErr != nil:
			stopped[item.removal.PersonID] = true
			if retry == nil {
				retry = retryErr
			}
			if _, _, after := provider.Classify(retryErr); after > 0 {
				snooze = after
				break apply
			}
		}
	}
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		// A confirmed batch is applied only once none of its items waits;
		// an interrupted one stays confirmed and resumes with the next run.
		for _, id := range batches {
			n, err := q.CountHeldBatchRemovals(ctx, &id)
			if err != nil {
				return fmt.Errorf("apps: count batch items: %w", err)
			}
			if n > 0 {
				continue
			}
			if err := q.SetRemovalBatchState(ctx, store.SetRemovalBatchStateParams{ID: id, State: batchApplied}); err != nil {
				return fmt.Errorf("apps: finish batch: %w", err)
			}
		}
		// Removals planned while this run worked wait for the next one; a
		// retry picks them up itself.
		var later bool
		if retry == nil && failure == nil {
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM provisioning_removals WHERE instance_id = $1 AND state = 'pending' AND updated_at >= $2)`, inst.ID, startedAt).Scan(&later); err != nil {
				return fmt.Errorf("apps: check pending removals: %w", err)
			}
		}
		if later {
			return s.enqueueRemovals(ctx, tx, inst.ID, removalDelay)
		}
		return nil
	})
	switch {
	case err != nil:
		return err
	case snooze > 0:
		// Before a failure: the provider asked to wait, the snoozed rerun
		// retries everything still open (also after the last attempt, which
		// a returned error would end, stranding the rate-limited removal).
		return river.JobSnooze(snooze)
	case failure != nil:
		return failure
	case retry != nil:
		return fmt.Errorf("apps: removal failed: %w", retry)
	}
	return nil
}

// selectRemovals chooses, in q's transaction, the removals to apply: fresh
// items of confirmed batches and fresh pending removals within the
// threshold; above it, the fresh pending removals are held. Stale removals
// are dropped and their people added to replan. It returns the removals to
// apply and the confirmed batches they come from.
func (s *Service) selectRemovals(ctx context.Context, q *store.Queries, inst Instance, startedAt time.Time, replan map[uuid.UUID]bool) ([]queuedRemoval, []uuid.UUID, error) {
	var toApply []queuedRemoval
	var batches []uuid.UUID
	fresh := func(r store.ProvisioningRemoval) (bool, error) {
		d, err := s.computeDesired(ctx, q, inst, r.PersonID)
		if errors.Is(err, ErrNotFound) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		if bytes.Equal(d.hash, r.DesiredHash) && removalRowManaged(inst, d, r.Action) {
			return true, nil
		}
		replan[r.PersonID] = true
		return false, q.SetRemovalState(ctx, store.SetRemovalStateParams{ID: r.ID, State: removalDropped})
	}
	// Confirmed batches: apply what is still fresh.
	confirmed, err := q.ListConfirmedRemovalBatches(ctx, inst.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("apps: list confirmed batches: %w", err)
	}
	for _, b := range confirmed {
		items, err := q.ListBatchRemovals(ctx, &b.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("apps: list batch: %w", err)
		}
		for _, r := range items {
			if r.State != removalHeld {
				continue
			}
			ok, err := fresh(r)
			if err != nil {
				return nil, nil, err
			}
			if ok {
				toApply = append(toApply, queuedRemoval{removal: r, state: removalHeld})
			}
		}
		batches = append(batches, b.ID)
	}
	// Pending removals: freshness, then the threshold.
	pending, err := q.ListPendingRemovals(ctx, inst.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("apps: list removals: %w", err)
	}
	var freshPending []store.ProvisioningRemoval
	for _, r := range pending {
		ok, err := fresh(r)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			freshPending = append(freshPending, r)
		}
	}
	if len(freshPending) == 0 {
		return toApply, batches, nil
	}
	recent, err := q.CountAppliedRemovalsSince(ctx, store.CountAppliedRemovalsSinceParams{InstanceID: inst.ID, Since: startedAt.Add(-24 * time.Hour)})
	if err != nil {
		return nil, nil, fmt.Errorf("apps: count removals: %w", err)
	}
	managed, err := q.CountManagedAccounts(ctx, inst.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("apps: count managed accounts: %w", err)
	}
	// While a batch is held, new removals join it instead of applying.
	_, err = q.GetHeldRemovalBatch(ctx, inst.ID)
	heldExists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("apps: load held batch: %w", err)
	}
	if !heldExists && int64(len(freshPending))+recent <= removalLimit(managed) {
		for _, r := range freshPending {
			toApply = append(toApply, queuedRemoval{removal: r, state: removalPending})
		}
		return toApply, batches, nil
	}
	return toApply, batches, s.hold(ctx, q, inst, freshPending)
}

// removalManaged reports whether a queued removal still targets access the
// adapter manages. Adapters whose managed set follows the desired access
// implement provider.RemovalChecker (GitHub: only the instance's current
// mapped teams), so deleting a mapping cancels that removal while unrelated
// mapping changes leave it alone. For other adapters a planned removal stays
// valid as long as the person's desired hash does.
func removalManaged(inst Instance, d desired, a provider.Action) bool {
	if inst.Adapter == nil {
		return true
	}
	checker, ok := inst.Adapter.RemovalChecker()
	if !ok {
		return true
	}
	return checker.RemovalStillManaged(inst.View(), d.access, a)
}

// removalRowManaged is removalManaged for a stored removal. An action that
// cannot be decoded stays fresh: applyRemoval reports that as the person's
// failure, so one broken removal never blocks the others.
func removalRowManaged(inst Instance, d desired, raw []byte) bool {
	if inst.Adapter == nil {
		return true
	}
	if _, ok := inst.Adapter.RemovalChecker(); !ok {
		return true
	}
	var a provider.Action
	if err := json.Unmarshal(raw, &a); err != nil {
		return true
	}
	return removalManaged(inst, d, a)
}

// enqueuePeople queues provision_person for a set of people of an instance,
// in a stable order so concurrent transactions lock their results alike.
func (s *Service) enqueuePeople(ctx context.Context, tx pgx.Tx, q *store.Queries, instanceID uuid.UUID, people map[uuid.UUID]bool) error {
	ids := make([]uuid.UUID, 0, len(people))
	for id := range people {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for _, id := range ids {
		if err := s.enqueuePerson(ctx, tx, q, instanceID, id, false); err != nil {
			return err
		}
	}
	return nil
}

// truncateRunes cuts s to at most n bytes without splitting a character.
func truncateRunes(s string, n int) string {
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// queuedRemoval is a removal chosen to apply and the state it was in.
type queuedRemoval struct {
	removal store.ProvisioningRemoval
	state   string
}

// errInstanceInactive stops applying removals of an instance that is no
// longer provisionable.
var errInstanceInactive = errors.New("apps: the instance is not provisionable")

// errRemovalFailed reports that a removal failed for good: the person's
// later removals do not run, and their open removals were dropped (the
// result is failed and waits for a desired change or an admin's retry).
var errRemovalFailed = errors.New("apps: the removal failed")

// applyRemoval applies one removal under the person's provisioning lock,
// so it never interleaves with that person's own job, after rechecking
// everything it depends on: the removal is still in the state it was chosen
// in, the instance is provisionable and the person's desired access still
// hashes to what planned the removal. A stale removal is dropped and the
// person replanned, in one transaction; so is an applied removal and its
// person's follow-up job (and audit entry). A retryable provider error that
// asks for a delay (a rate limit, or a removal the provider accepted but has
// not confirmed) snoozes the job; provider.CodeRemovalPending is remembered
// on the pending row so the later attempt that confirms the removal audits
// the earlier accepted change even though it changes nothing itself. A
// permanent failure (or the last attempt's retryable one without RetryAfter)
// fails the person's result, which then waits for a desired-state change or
// an admin's retry. It returns the retryable provider error to retry the job
// with (the removal stays open), or errInstanceInactive.
//
// The person's lock is taken on held, the removals lock's connection (lock
// order: removals, then person), so the guard never waits for a second pool
// connection while it holds one.
func (s *Service) applyRemoval(ctx context.Context, held *heldConn, prov provider.UserProvisioner, item queuedRemoval, final bool) (error, error) {
	r := item.removal
	unlock, err := held.lockAlso(ctx, lockID("provision", r.InstanceID.String(), r.PersonID.String()))
	if err != nil {
		return nil, err
	}
	defer unlock()
	conn := held.conn
	q := held.queries()
	cur, err := q.GetRemoval(ctx, r.ID)
	if err != nil {
		return nil, fmt.Errorf("apps: reload removal: %w", err)
	}
	if cur.State != item.state {
		return nil, nil // dropped or resolved meanwhile, by a run that replans the person
	}
	inst, err := s.loadInstance(ctx, q, r.InstanceID, false)
	if err != nil {
		return nil, err
	}
	if !inst.Provisionable() || inst.Adapter == nil {
		return errInstanceInactive, nil
	}
	d, err := s.computeDesired(ctx, q, inst, r.PersonID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil // the person is gone (their rows cascade)
	} else if err != nil {
		return nil, err
	}
	var a provider.Action
	if err := json.Unmarshal(cur.Action, &a); err != nil {
		return nil, fmt.Errorf("apps: decode removal: %w", err)
	}
	if !bytes.Equal(d.hash, cur.DesiredHash) || !removalManaged(inst, d, a) {
		return nil, pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			q := store.New(tx)
			if _, err := q.SetRemovalStateFrom(ctx, store.SetRemovalStateFromParams{ID: r.ID, FromState: item.state, State: removalDropped}); err != nil {
				return fmt.Errorf("apps: drop removal: %w", err)
			}
			return s.enqueuePerson(ctx, tx, q, inst.ID, r.PersonID, false)
		})
	}
	out, applyErr := prov.Apply(ctx, held.env(inst), inst.View(), a)
	if applyErr != nil {
		code, retryable, retryAfter := provider.Classify(applyErr)
		if retryable && (retryAfter > 0 || !final) {
			if code == provider.CodeRemovalPending {
				// The provider accepted the removal but has not confirmed it
				// yet. Remember that, so the attempt that later confirms it
				// (which changes nothing itself) still records the audit
				// entry the accepted change deserves.
				if _, err := q.SetRemovalStateFrom(ctx, store.SetRemovalStateFromParams{ID: r.ID, FromState: item.state, State: item.state, ErrorCode: &code}); err != nil {
					return nil, fmt.Errorf("apps: record the accepted removal: %w", err)
				}
			}
			return applyErr, nil // stays as it is; the job retries or snoozes
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			q := store.New(tx)
			n, err := q.SetRemovalStateFrom(ctx, store.SetRemovalStateFromParams{ID: r.ID, FromState: item.state, State: removalFailed, ErrorCode: &code})
			if err != nil || n == 0 {
				return err
			}
			// The rest of this person's plan does not run after a failure.
			if err := q.DropOpenRemovals(ctx, store.DropOpenRemovalsParams{InstanceID: r.InstanceID, PersonID: r.PersonID}); err != nil {
				return fmt.Errorf("apps: drop later removals: %w", err)
			}
			return s.failResult(ctx, q, inst, d, a, code)
		})
		if err != nil {
			return nil, err
		}
		return nil, errRemovalFailed
	}
	// A removal the provider accepted in an earlier attempt is confirmed by
	// this one: the membership is gone now, so this attempt reports no change
	// of its own, but the earlier acceptance was the change to audit.
	acceptedEarlier := cur.ErrorCode != nil && *cur.ErrorCode == provider.CodeRemovalPending
	return nil, pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.SetRemovalStateFrom(ctx, store.SetRemovalStateFromParams{ID: r.ID, FromState: item.state, State: removalApplied})
		if err != nil {
			return fmt.Errorf("apps: record removal: %w", err)
		}
		if n > 0 && (out.Changed || acceptedEarlier) {
			meta := map[string]any{"provider": inst.Provider, "instance_id": inst.ID, "kind": a.Kind, "key": a.Key}
			if acceptedEarlier && !out.Changed {
				meta["confirmed"] = true
			}
			if _, err := audit.Record(ctx, q, audit.Entry{
				Actor:    audit.System,
				Action:   audit.ProvisioningActionApplied,
				Target:   audit.Target{Kind: audit.TargetPerson, ID: r.PersonID.String()},
				Metadata: meta,
			}); err != nil {
				return err
			}
		}
		// The provider changed: the person is planned again (their result
		// waits as queued until then), whatever happened to the row.
		return s.enqueuePerson(ctx, tx, q, inst.ID, r.PersonID, false)
	})
}

// failResult marks a person's result failed because a removal failed: the
// action shows as failed with the code, and Retry Failed selects it.
func (s *Service) failResult(ctx context.Context, q *store.Queries, inst Instance, d desired, a provider.Action, code string) error {
	var results []ActionResult
	if d.result != nil {
		_ = json.Unmarshal(d.result.Actions, &results) // written by the framework
	}
	i := slices.IndexFunc(results, func(r ActionResult) bool { return r.Key == a.Key })
	if i < 0 {
		results = append(results, ActionResult{Kind: a.Kind, Key: a.Key, Removal: true})
		i = len(results) - 1
	}
	results[i].Outcome, results[i].Error = OutcomeFailed, code
	raw, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("apps: encode actions: %w", err)
	}
	if err := q.FailProvisioningResult(ctx, store.FailProvisioningResultParams{
		InstanceID: inst.ID, PersonID: d.person.ID, ErrorCode: &code, Actions: raw, DesiredHash: d.hash,
	}); err != nil {
		return fmt.Errorf("apps: fail result: %w", err)
	}
	s.env(inst).Logger().WarnContext(ctx, "removal failed", "person_id", d.person.ID, "kind", a.Kind, "code", code)
	return nil
}

// removalLimit is how many removals may apply without confirmation within
// a day: max(3, 10 % of the managed accounts).
func removalLimit(managed int64) int64 { return max(3, managed/10) }

// hold puts fresh pending removals into one held batch. A batch already
// held is superseded; its items join the new one, so there is at most one
// held batch per instance.
func (s *Service) hold(ctx context.Context, q *store.Queries, inst Instance, items []store.ProvisioningRemoval) error {
	if old, err := q.GetHeldRemovalBatch(ctx, inst.ID); err == nil {
		prev, err := q.ListBatchRemovals(ctx, &old.ID)
		if err != nil {
			return fmt.Errorf("apps: list batch: %w", err)
		}
		for _, r := range prev {
			if r.State == removalHeld {
				items = append(items, r)
			}
		}
		if err := q.SetRemovalBatchState(ctx, store.SetRemovalBatchStateParams{ID: old.ID, State: batchSuperseded}); err != nil {
			return fmt.Errorf("apps: supersede batch: %w", err)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("apps: load held batch: %w", err)
	}
	batch, err := q.InsertRemovalBatch(ctx, store.InsertRemovalBatchParams{InstanceID: inst.ID, ItemCount: int32(len(items)), ItemsHash: itemsHash(items)}) //nolint:gosec // bounded by people
	if err != nil {
		return fmt.Errorf("apps: create batch: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(items))
	for _, r := range items {
		ids = append(ids, r.ID)
	}
	if err := q.HoldRemovals(ctx, store.HoldRemovalsParams{BatchID: &batch.ID, Ids: ids}); err != nil {
		return fmt.Errorf("apps: hold removals: %w", err)
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    audit.System,
		Action:   audit.ProvisioningRemovalsHeld,
		Target:   instanceTarget(inst.ID),
		Metadata: map[string]any{"provider": inst.Provider, "batch_id": batch.ID, "item_count": len(items)},
	})
	return err
}

// itemsHash is the SHA-256 over the sorted (removal id, desired_hash)
// pairs of a batch.
func itemsHash(items []store.ProvisioningRemoval) []byte {
	sorted := slices.Clone(items)
	slices.SortFunc(sorted, func(a, b store.ProvisioningRemoval) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	h := sha256.New()
	for _, r := range sorted {
		h.Write(r.ID[:])
		h.Write(r.DesiredHash)
	}
	return h.Sum(nil)
}

// HeldBatch is a held removal batch with its items.
type HeldBatch struct {
	Batch store.ProvisioningRemovalBatch
	Items []store.ListHeldRemovalsRow
}

// HeldRemovals returns the instance's held removal batch, if any.
func (s *Service) HeldRemovals(ctx context.Context, instanceID uuid.UUID) (*HeldBatch, error) {
	var out *HeldBatch
	err := s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		b, err := q.GetHeldRemovalBatch(ctx, instanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("apps: load held batch: %w", err)
		}
		items, err := q.ListHeldRemovals(ctx, &b.ID)
		if err != nil {
			return fmt.Errorf("apps: list held removals: %w", err)
		}
		out = &HeldBatch{Batch: b, Items: items}
		return nil
	})
	return out, err
}

// ConfirmRemovals releases a held batch. itemsHash must be the hash the
// admin saw; a batch that is no longer held or has other items is refused
// with ErrBatchChanged.
func (s *Service) ConfirmRemovals(ctx context.Context, actor audit.Actor, instanceID, batchID uuid.UUID, hash []byte) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, instanceID, false)
		if err != nil {
			return err
		}
		b, err := q.GetRemovalBatchForUpdate(ctx, store.GetRemovalBatchForUpdateParams{ID: batchID, InstanceID: instanceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: removal batch", ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("apps: load batch: %w", err)
		}
		if b.State != batchHeld || !crypto.Equal(b.ItemsHash, hash) {
			return ErrBatchChanged
		}
		personID, _ := uuid.Parse(actor.ID)
		var by *uuid.UUID
		if personID != uuid.Nil {
			by = &personID
		}
		if err := q.ConfirmRemovalBatch(ctx, store.ConfirmRemovalBatchParams{ID: batchID, ConfirmedBy: by}); err != nil {
			return fmt.Errorf("apps: confirm batch: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.ProvisioningRemovalsConfirmed,
			Target:   instanceTarget(instanceID),
			Metadata: map[string]any{"provider": inst.Provider, "batch_id": batchID, "item_count": b.ItemCount},
		}); err != nil {
			return err
		}
		return s.enqueueRemovals(ctx, tx, instanceID, 0)
	})
}

// RetryFailed queues provisioning again for failed results of an instance,
// all of them or one person's. It returns how many were queued.
func (s *Service) RetryFailed(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, personID *uuid.UUID) (int, error) {
	var n int
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, instanceID, false)
		if err != nil {
			return err
		}
		if !inst.Provisionable() {
			return ErrInvalidState
		}
		ids, err := q.ListFailedProvisioningResults(ctx, store.ListFailedProvisioningResultsParams{InstanceID: instanceID, PersonID: personID})
		if err != nil {
			return fmt.Errorf("apps: list failed results: %w", err)
		}
		for _, id := range ids {
			if err := s.enqueuePerson(ctx, tx, q, instanceID, id, true, true); err != nil {
				return err
			}
		}
		n = len(ids)
		meta := map[string]any{"provider": inst.Provider, "count": n}
		if personID != nil {
			meta["person_id"] = *personID
		}
		_, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.ProvisioningRetried, Target: instanceTarget(instanceID), Metadata: meta})
		return err
	})
	return n, err
}

// PlannedPerson is one person's dry-run plan.
type PlannedPerson struct {
	Person       store.Person
	Access       bool
	Actions      []provider.Action
	EmployeeStep string
	ErrorCode    string
}

// MaxDryRunPeople bounds a synchronous dry run.
const MaxDryRunPeople = 1000

// DryRun plans every person the instance concerns without changing
// anything: no provider state, no results, no jobs.
func (s *Service) DryRun(ctx context.Context, instanceID uuid.UUID) ([]PlannedPerson, bool, error) {
	q := store.New(s.pool)
	inst, err := s.loadInstance(ctx, q, instanceID, false)
	if err != nil {
		return nil, false, err
	}
	if !inst.Provisionable() {
		return nil, false, ErrInvalidState
	}
	if inst.Adapter == nil {
		return nil, false, ErrProviderUnavailable
	}
	prov, ok := inst.Adapter.Provisioner()
	if !ok {
		return nil, false, ErrNotSupported
	}
	view := inst.View()
	people, err := q.ListProvisioningCandidates(ctx, store.ListProvisioningCandidatesParams{InstanceID: inst.ID, Provider: inst.Provider, Realm: inst.Adapter.Realm(view)})
	if err != nil {
		return nil, false, fmt.Errorf("apps: list people: %w", err)
	}
	truncated := len(people) > MaxDryRunPeople
	if truncated {
		people = people[:MaxDryRunPeople]
	}
	env := s.env(inst)
	out := make([]PlannedPerson, 0, len(people))
	for _, id := range people {
		d, err := s.computeDesired(ctx, q, inst, id)
		if errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return nil, false, err
		}
		if d.result == nil && !d.access.Access && d.access.Link == nil {
			continue
		}
		p := PlannedPerson{Person: d.person, Access: d.access.Access, Actions: []provider.Action{}}
		plan, err := prov.Plan(ctx, env, view, d.access)
		if err != nil {
			p.ErrorCode, _, _ = provider.Classify(err)
		} else {
			p.EmployeeStep = plan.EmployeeStep
			for _, a := range plan.Actions {
				if a.Removal && !d.access.Managed {
					continue
				}
				p.Actions = append(p.Actions, a)
			}
		}
		out = append(out, p)
	}
	return out, truncated, nil
}

// RunDriftCheck queues provision_instance for every provisionable
// instance, and ensure_webhooks for those whose provider takes webhooks.
func (s *Service) RunDriftCheck(ctx context.Context) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		rows, err := q.ListAppInstancesInState(ctx, provisionable)
		if err != nil {
			return fmt.Errorf("apps: list instances: %w", err)
		}
		if err := s.enqueueEnsureWebhooks(ctx, tx, rows); err != nil {
			return err
		}
		for _, row := range rows {
			a, ok := s.reg.Adapter(provider.ID(row.Provider))
			if !ok {
				continue
			}
			if _, ok := a.Provisioner(); !ok {
				continue
			}
			if err := s.enqueueInstance(ctx, tx, row.ID, ReasonDrift); err != nil {
				return err
			}
		}
		return nil
	})
}

// refreshLinkLogin updates the informational login of the person's verified
// link of the account an action reported. Nothing happens for another
// account, another person or an unchanged login (zero rows).
func (s *Service) refreshLinkLogin(ctx context.Context, conn *pgxpool.Conn, inst Instance, personID uuid.UUID, acct provider.ExternalAccount) error {
	if acct.ExternalID == "" || acct.Login == "" || len(acct.Login) > 100 || inst.Adapter == nil {
		return nil
	}
	_, err := store.New(conn).RefreshAccountLinkLogin(ctx, store.RefreshAccountLinkLoginParams{
		PersonID: personID, Provider: inst.Provider, Realm: inst.Adapter.Realm(inst.View()), ExternalID: acct.ExternalID, Login: acct.Login,
	})
	if err != nil {
		return fmt.Errorf("apps: refresh link login: %w", err)
	}
	return nil
}
