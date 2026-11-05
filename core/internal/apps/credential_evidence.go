package apps

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// Evidence about which credential value the provider uses, and the paths
// that act on it (docs/specs/providers.md "Credentials", concurrency model,
// "Evidence, override and drift"): proof by use, an admin's forced
// resolution, and drift detection (reported, never repaired automatically).

// webhookSecret is the credential incoming deliveries are signed with.
const webhookSecret = "webhook_secret"

// Rotation resolutions an admin may force (ResolveRotation).
const (
	ResolveActive    = "active"
	ResolveDiscarded = "discarded"
)

// ResolveRotation forces an uncertain or abandoning candidate to active or
// discarded without evidence from the provider: the way out of a rotation
// whose outcome cannot be established. The admin asserts which value the
// provider uses; a wrong assertion breaks the provider's use of the
// credential until it is rotated again (drift detection reports it for
// webhook secrets). It is refused while an execution's lease holds, and
// activation is refused when the candidate has no value (a mint whose
// answer was lost): an assertion cannot recover the provider's bytes.
func (s *Service) ResolveRotation(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, name, resolution string) error {
	if resolution != ResolveActive && resolution != ResolveDiscarded {
		return invalid("resolution", "must be active or discarded")
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		inst, spec, err := s.rotatable(ctx, q, instanceID, name)
		if err != nil {
			return err
		}
		cand, err := q.GetAppCredentialCandidateForUpdate(ctx, store.GetAppCredentialCandidateForUpdateParams{InstanceID: instanceID, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoRotation
		} else if err != nil {
			return fmt.Errorf("apps: lock candidate: %w", err)
		}
		if cand.State == credentialPending {
			// Never handed over: nothing is uncertain (abandon it instead).
			return ErrInvalidState
		}
		if busy, err := callInFlight(ctx, q, instanceID, name); err != nil {
			return err
		} else if busy {
			return ErrRotationInProgress
		}
		if resolution == ResolveActive {
			value, err := s.openCredential(cand)
			if err != nil {
				return err
			}
			known := len(value) > 0
			clear(value)
			if !known {
				return ErrCandidateValueUnknown
			}
			if err := s.activate(ctx, q, inst, spec, cand, nil, "admin"); err != nil {
				return err
			}
		} else {
			n, err := q.SetAppCredentialStateFrom(ctx, store.SetAppCredentialStateFromParams{
				ID: cand.ID, State: credentialDiscarded, FromStates: []string{credentialUncertain, credentialAbandoning},
			})
			if err != nil {
				return fmt.Errorf("apps: discard candidate: %w", err)
			}
			if n != 1 {
				// Unreachable: the candidate is locked and was checked above.
				return fmt.Errorf("apps: discard candidate %s v%d: not a candidate", cand.Name, cand.Version)
			}
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AppCredentialRotationResolved,
			Target:   instanceTarget(inst.ID),
			Metadata: credentialMetadata(inst, cand, "resolution", resolution),
		})
		return err
	})
}

// classifyDelivery runs in the transaction that stores a delivery whose
// signature verified with some usable version, and acts on which one: the
// active version proves nothing; only an uncertain candidate is proof by use
// and activates it; only a retiring version is drift (reported). Signatures
// are checked without locks; acknowledge and reportDrift take the instance
// lock and recheck.
func (s *Service) classifyDelivery(ctx context.Context, tx pgx.Tx, q *store.Queries, inst Instance, src provider.EventSource, h http.Header, body []byte) error {
	spec, ok := inst.Adapter.Credential(webhookSecret)
	if !ok {
		return nil
	}
	verifies := s.signatureCheck(ctx, inst, s.envOn(inst, tx), src, h, body)
	active, err := q.GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: inst.ID, Name: webhookSecret})
	if err == nil {
		if ok, err := verifies(active); err != nil || ok {
			return err // the provider uses the active version, or it may
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("apps: load active credential: %w", err)
	}
	cand, err := q.GetAppCredentialCandidate(ctx, store.GetAppCredentialCandidateParams{InstanceID: inst.ID, Name: webhookSecret})
	if err == nil {
		if ok, err := verifies(cand); err != nil {
			return err
		} else if ok {
			if cand.State == credentialAbandoning {
				return nil // being abandoned: proof of use must not activate it
			}
			return s.acknowledge(ctx, q, inst, spec, cand.ID, nil, "use")
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("apps: load candidate: %w", err)
	}
	rows, err := q.ListDriftAppCredentials(ctx, store.ListDriftAppCredentialsParams{InstanceID: inst.ID, Name: webhookSecret})
	if err != nil {
		return fmt.Errorf("apps: load retiring credentials: %w", err)
	}
	for _, row := range rows {
		if ok, err := verifies(row); err != nil {
			return err
		} else if ok {
			return s.reportDrift(ctx, q, inst, row)
		}
	}
	return nil
}

// detectRetainedDrift runs after a signature verified with no usable
// version: if it verifies with a retained retired or discarded version, the
// provider uses an older value. The delivery stays rejected (an older value
// is never accepted); the drift is reported. It reads without a transaction
// and writes only on a match, as rejected deliveries are untrusted.
func (s *Service) detectRetainedDrift(ctx context.Context, inst Instance, src provider.EventSource, h http.Header, body []byte) error {
	if _, ok := inst.Adapter.Credential(webhookSecret); !ok {
		return nil
	}
	rows, err := store.New(s.pool).ListRetainedAppCredentials(ctx, store.ListRetainedAppCredentialsParams{InstanceID: inst.ID, Name: webhookSecret})
	if err != nil || len(rows) == 0 {
		return err
	}
	verifies := s.signatureCheck(ctx, inst, s.env(inst), src, h, body)
	for _, row := range rows {
		if ok, err := verifies(row); err != nil {
			return err
		} else if ok {
			return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
				return s.reportDrift(ctx, q, inst, row)
			})
		}
	}
	return nil
}

// signatureCheck returns whether a delivery verifies with exactly one
// version's value.
func (s *Service) signatureCheck(ctx context.Context, inst Instance, base provider.Env, src provider.EventSource, h http.Header, body []byte) func(store.AppCredential) (bool, error) {
	return func(row store.AppCredential) (bool, error) {
		value, err := s.openCredential(row)
		if err != nil {
			return false, err
		}
		defer clear(value)
		if len(value) == 0 {
			return false, nil // a candidate the provider has not minted yet
		}
		return src.Verify(ctx, fixedSecrets{base, webhookSecret, [][]byte{value}}, inst.View(), h, body) == nil, nil
	}
}

// reportDrift records, once per version, that a delivery was signed with an
// older version after it ended (past the grace period for deliveries signed
// just before the change): an audit entry, and the version's
// drift_detected_at, which the instance detail shows until the credential is
// rotated again. Nothing is queued or re-applied: rotating is the admin's
// remedy. Replays of the same delivery and repeated deliveries change
// nothing (zero rows).
func (s *Service) reportDrift(ctx context.Context, q *store.Queries, inst Instance, row store.AppCredential) error {
	if err := lockCredentials(ctx, q, inst.ID); err != nil {
		return err
	}
	n, err := q.MarkAppCredentialDrift(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("apps: mark drift: %w", err)
	}
	if n == 0 {
		return nil // reported before, within the grace period, or gone
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    audit.System,
		Action:   audit.AppCredentialDrift,
		Target:   instanceTarget(inst.ID),
		Metadata: credentialMetadata(inst, row, "state", row.State),
	})
	return err
}
