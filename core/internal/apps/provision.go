package apps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

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

// grants are the action kinds by which Orglane grants access.
var grants = []provider.ActionKind{provider.ActionCreateAccount, provider.ActionInvite, provider.ActionAddToTeam}

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
	conn, release, err := sessionLock(ctx, s.pool, lockID("provision", args.InstanceID.String(), args.PersonID.String()))
	if err != nil {
		return err
	}
	defer release()
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
	if r := d.result; r != nil && !args.Drift && bytes.Equal(r.AppliedHash, d.hash) && r.ErrorCode == nil && r.EmployeeStep == nil && r.Status != StatusFailed {
		err := s.writeResult(ctx, conn, inst, d, StatusDone, nil, r.Actions, nil, r.GrantedByOrglane, r.AppliedHash, 0, startedAt)
		return s.followUp(err, conn, inst.ID, args.PersonID, startedAt)
	}

	env := s.env(inst)
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
		err := s.writeResult(ctx, conn, inst, d, StatusPendingEmployeeAction, &step, []byte("[]"), nil, d.result != nil && d.result.GrantedByOrglane, nil, 0, startedAt)
		return s.followUp(err, conn, inst.ID, args.PersonID, startedAt)
	}

	granted := d.result != nil && d.result.GrantedByOrglane
	results := make([]ActionResult, 0, len(plan.Actions))
	var removals []provider.Action
	var applyErr error
	for _, a := range plan.Actions {
		ar := ActionResult{Kind: a.Kind, Key: a.Key, Removal: a.Removal}
		switch {
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
		case applyErr != nil:
			ar.Outcome = OutcomeNotAttempted
		default:
			out, err := prov.Apply(ctx, env, view, a)
			if err != nil {
				applyErr = err
				ar.Outcome = OutcomeFailed
				ar.Error, _, _ = provider.Classify(err)
				break
			}
			ar.Outcome = OutcomeUnchanged
			if out.Changed {
				ar.Outcome = OutcomeChanged
				if slices.Contains(grants, a.Kind) {
					granted = true
				}
			}
			if err := s.recordApplied(ctx, conn, inst, d.person.ID, a, out); err != nil {
				return err
			}
			if out.Link != nil {
				granted = true
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

// recordApplied writes the audit entry of a changed action and the link of
// a created account.
func (s *Service) recordApplied(ctx context.Context, conn *pgxpool.Conn, inst Instance, personID uuid.UUID, a provider.Action, out provider.ActionOutcome) error {
	if !out.Changed && out.Link == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		if out.Changed {
			if _, err := audit.Record(ctx, q, audit.Entry{
				Actor:    audit.System,
				Action:   audit.ProvisioningActionApplied,
				Target:   audit.Target{Kind: audit.TargetPerson, ID: personID.String()},
				Metadata: map[string]any{"provider": inst.Provider, "instance_id": inst.ID, "kind": a.Kind, "key": a.Key},
			}); err != nil {
				return err
			}
		}
		if out.Link != nil {
			return s.provisionedLink(ctx, tx, q, inst, personID, *out.Link)
		}
		return nil
	})
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
	g := d.result != nil && d.result.GrantedByOrglane
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
	conn, release, err := sessionLock(ctx, s.pool, lockID("instance", args.InstanceID.String()))
	if err != nil {
		return err
	}
	defer release()
	q := store.New(conn)
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
	env, view := s.env(inst), inst.View()
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
	refs, err := prov.SyncTeams(ctx, env, view, mappings)
	if err != nil {
		return retryOrCancel(err)
	}
	members, err := prov.Members(ctx, env, view)
	if err != nil {
		return retryOrCancel(err)
	}
	realm := inst.Adapter.Realm(view)
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		for _, ref := range refs {
			i := slices.IndexFunc(mappings, func(m provider.TeamMapping) bool { return m.TeamID == ref.TeamID })
			if i < 0 || ref.ExternalRef == "" || len(ref.ExternalRef) > 100 {
				continue
			}
			name := ref.Name
			if len(name) > 100 {
				name = name[:100]
			}
			if err := q.SetAppTeamMappingExternal(ctx, store.SetAppTeamMappingExternalParams{InstanceID: inst.ID, TeamID: ref.TeamID, ExternalRef: &ref.ExternalRef, ExternalName: &name}); err != nil {
				return fmt.Errorf("apps: store provider team: %w", err)
			}
		}
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
func (s *Service) RunApplyRemovals(ctx context.Context, args ApplyRemovalsArgs) error {
	conn, release, err := sessionLock(ctx, s.pool, lockID("removals", args.InstanceID.String()))
	if err != nil {
		return err
	}
	defer release()
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

	var toApply []store.ProvisioningRemoval
	var batches []uuid.UUID
	replan := map[uuid.UUID]bool{}
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		fresh := func(r store.ProvisioningRemoval) (bool, error) {
			d, err := s.computeDesired(ctx, q, inst, r.PersonID)
			if errors.Is(err, ErrNotFound) {
				return false, nil
			} else if err != nil {
				return false, err
			}
			if bytes.Equal(d.hash, r.DesiredHash) {
				return true, nil
			}
			replan[r.PersonID] = true
			return false, q.SetRemovalState(ctx, store.SetRemovalStateParams{ID: r.ID, State: removalDropped})
		}
		// Confirmed batches: apply what is still fresh.
		confirmed, err := q.ListConfirmedRemovalBatches(ctx, inst.ID)
		if err != nil {
			return fmt.Errorf("apps: list confirmed batches: %w", err)
		}
		for _, b := range confirmed {
			items, err := q.ListBatchRemovals(ctx, &b.ID)
			if err != nil {
				return fmt.Errorf("apps: list batch: %w", err)
			}
			for _, r := range items {
				if r.State != removalHeld {
					continue
				}
				ok, err := fresh(r)
				if err != nil {
					return err
				}
				if ok {
					toApply = append(toApply, r)
				}
			}
			batches = append(batches, b.ID)
		}
		// Pending removals: freshness, then the threshold.
		pending, err := q.ListPendingRemovals(ctx, inst.ID)
		if err != nil {
			return fmt.Errorf("apps: list removals: %w", err)
		}
		var freshPending []store.ProvisioningRemoval
		for _, r := range pending {
			ok, err := fresh(r)
			if err != nil {
				return err
			}
			if ok {
				freshPending = append(freshPending, r)
			}
		}
		if len(freshPending) == 0 {
			return nil
		}
		recent, err := q.CountAppliedRemovalsSince(ctx, store.CountAppliedRemovalsSinceParams{InstanceID: inst.ID, Since: startedAt.Add(-24 * time.Hour)})
		if err != nil {
			return fmt.Errorf("apps: count removals: %w", err)
		}
		managed, err := q.CountManagedAccounts(ctx, inst.ID)
		if err != nil {
			return fmt.Errorf("apps: count managed accounts: %w", err)
		}
		// While a batch is held, new removals join it instead of applying.
		_, err = q.GetHeldRemovalBatch(ctx, inst.ID)
		heldExists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("apps: load held batch: %w", err)
		}
		if !heldExists && int64(len(freshPending))+recent <= removalLimit(managed) {
			toApply = append(toApply, freshPending...)
			return nil
		}
		return s.hold(ctx, q, inst, freshPending)
	})
	if err != nil {
		return err
	}

	env, view := s.env(inst), inst.View()
	var firstErr error
	for _, r := range toApply {
		var a provider.Action
		if err := json.Unmarshal(r.Action, &a); err != nil {
			return fmt.Errorf("apps: decode removal: %w", err)
		}
		out, applyErr := prov.Apply(ctx, env, view, a)
		state, code := removalApplied, (*string)(nil)
		if applyErr != nil {
			c, retryable, _ := provider.Classify(applyErr)
			if retryable && firstErr == nil {
				firstErr = applyErr
				continue // stays as it is; the job retries
			} else if retryable {
				continue
			}
			state, code = removalFailed, &c
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			q := store.New(tx)
			if err := q.SetRemovalState(ctx, store.SetRemovalStateParams{ID: r.ID, State: state, ErrorCode: code}); err != nil {
				return fmt.Errorf("apps: record removal: %w", err)
			}
			if applyErr == nil && out.Changed {
				if _, err := audit.Record(ctx, q, audit.Entry{
					Actor:    audit.System,
					Action:   audit.ProvisioningActionApplied,
					Target:   audit.Target{Kind: audit.TargetPerson, ID: r.PersonID.String()},
					Metadata: map[string]any{"provider": inst.Provider, "instance_id": inst.ID, "kind": a.Kind, "key": a.Key},
				}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		replan[r.PersonID] = true
	}
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		if firstErr == nil {
			for _, id := range batches {
				if err := q.SetRemovalBatchState(ctx, store.SetRemovalBatchStateParams{ID: id, State: batchApplied}); err != nil {
					return fmt.Errorf("apps: finish batch: %w", err)
				}
			}
		}
		ids := make([]uuid.UUID, 0, len(replan))
		for id := range replan {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		for _, id := range ids {
			if err := s.enqueuePerson(ctx, tx, q, inst.ID, id, false); err != nil {
				return err
			}
		}
		if firstErr != nil {
			return fmt.Errorf("apps: removal failed: %w", firstErr)
		}
		// Removals planned while this run worked wait for the next one.
		var later bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM provisioning_removals WHERE instance_id = $1 AND state = 'pending' AND updated_at >= $2)`, inst.ID, startedAt).Scan(&later); err != nil {
			return fmt.Errorf("apps: check pending removals: %w", err)
		}
		if later {
			return s.enqueueRemovals(ctx, tx, inst.ID, removalDelay)
		}
		return nil
	})
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
			if err := s.enqueuePerson(ctx, tx, q, instanceID, id, true); err != nil {
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
// instance.
func (s *Service) RunDriftCheck(ctx context.Context) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		rows, err := q.ListAppInstancesInState(ctx, provisionable)
		if err != nil {
			return fmt.Errorf("apps: list instances: %w", err)
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
