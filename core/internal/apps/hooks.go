package apps

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// errStateChanged: the instance left the states webhooks were ensured for
// while ensureWebhooks waited for its lock.
var errStateChanged = errors.New("apps: the instance state changed")

// errWebhooksDeferred: a rotation of webhook_secret is in progress, so the
// webhooks were left as they are (see ensureWebhooks).
var errWebhooksDeferred = errors.New("apps: webhooks deferred while the webhook secret is rotated")

// ensureWebhooks makes the provider's webhooks deliver to the instance
// (EventSource.EnsureWebhooks): after every Setup and daily
// (RunEnsureWebhooks). Adapters install the active webhook_secret on the
// provider's hook, so an ensure is a provider execution for that credential
// like a rotation attempt, under the same concurrency model
// (docs/specs/providers.md "Credentials"):
//
//   - It holds lock 1, webhook_secret's credential lock, and does all
//     database work on that connection (heldConn): it never acquires a
//     second pool connection.
//   - Under the instance row lock it rechecks the instance (one that left
//     states gets nothing: errStateChanged) and creates a missing
//     webhook_secret (backfillWebhookSecret).
//   - While a candidate of webhook_secret exists (pending, uncertain or
//     abandoning) it installs nothing (errWebhooksDeferred): the provider
//     may already use the candidate (an attempt whose answer was lost), and
//     putting the active value back would let a later proof by use activate
//     a value the provider no longer uses. The rotation job sets the hook
//     with the value that wins.
//   - The adapter call is an execution on the active version's row
//     (beginCall): deferred while any execution of the credential holds a
//     lease (errCallInFlight), and bounded by its own lease. It ends after
//     success; after a failure its lease expires, as for an unknown answer.
func (s *Service) ensureWebhooks(ctx context.Context, inst Instance, states ...string) error {
	if inst.Adapter == nil {
		return nil
	}
	if _, ok := inst.Adapter.EventSource(); !ok {
		return nil
	}
	held, err := s.holdLock(ctx, credentialLockID(inst.ID, webhookSecret))
	if err != nil {
		return err
	}
	defer held.release()
	// recheck runs under the instance row lock: the instance is still in
	// states, and no candidate exists.
	recheck := func(q *store.Queries, cand *store.AppCredential) (Instance, error) {
		cur, err := s.loadInstance(ctx, q, inst.ID, false)
		if errors.Is(err, ErrNotFound) {
			return Instance{}, errStateChanged
		} else if err != nil {
			return Instance{}, err
		}
		if !slices.Contains(states, cur.State) || cur.Adapter == nil {
			return Instance{}, errStateChanged
		}
		if cand != nil {
			return Instance{}, errWebhooksDeferred
		}
		return cur, nil
	}
	var cur Instance
	var active store.AppCredential
	err = held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if err := lockCredentials(ctx, q, inst.ID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return errStateChanged
			}
			return err
		}
		var cand *store.AppCredential
		if c, err := q.GetAppCredentialCandidate(ctx, store.GetAppCredentialCandidateParams{InstanceID: inst.ID, Name: webhookSecret}); err == nil {
			cand = &c
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("apps: load webhook secret candidate: %w", err)
		}
		var err error
		if cur, err = recheck(q, cand); err != nil {
			return err
		}
		if err := s.backfillWebhookSecret(ctx, q, cur); err != nil {
			return err
		}
		active, err = q.GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: cur.ID, Name: webhookSecret})
		if errors.Is(err, pgx.ErrNoRows) {
			return errWebhooksDeferred // only a candidate: its rotation sets the hook
		} else if err != nil {
			return fmt.Errorf("apps: load webhook secret: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var why error
	call, err := s.beginCall(ctx, held, cur, webhookSecret, active.ID, func(q *store.Queries, cand *store.AppCredential) (bool, error) {
		c, err := recheck(q, cand)
		if errors.Is(err, errStateChanged) || errors.Is(err, errWebhooksDeferred) {
			why = err
			return false, nil
		} else if err != nil {
			return false, err
		}
		cur = c
		return true, nil
	})
	if err != nil {
		return err
	}
	if call == nil {
		return why
	}
	src, ok := cur.Adapter.EventSource()
	if !ok {
		return nil
	}
	callCtx, cancel := call.context(ctx)
	err = src.EnsureWebhooks(callCtx, held.env(cur), cur.View())
	cancel()
	if err != nil {
		return err // the execution's lease expires: the provider may still act
	}
	return held.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		return call.end(ctx, q, cur.ID)
	})
}

// backfillWebhookSecret creates the active webhook_secret of an instance
// whose adapter generates it but that has no version of it: generated
// credentials are created with the instance, so an instance created before
// its adapter declared the credential has none, and its webhooks could
// never be set up. An instance with any usable version is left alone. q is
// in the caller's transaction, which holds the instance row lock.
func (s *Service) backfillWebhookSecret(ctx context.Context, q *store.Queries, inst Instance) error {
	spec, ok := inst.Adapter.Credential(webhookSecret)
	if !ok || spec.Source != provider.SourceGenerated || spec.Agent {
		return nil
	}
	usable, err := q.ListUsableAppCredentials(ctx, store.ListUsableAppCredentialsParams{InstanceID: inst.ID, Name: webhookSecret})
	if err != nil {
		return fmt.Errorf("apps: list webhook secrets: %w", err)
	}
	if len(usable) > 0 {
		return nil
	}
	_, err = s.storeCredential(ctx, q, audit.System, inst, webhookSecret, []byte(crypto.RandomToken("")), credentialActive)
	return err
}

// RunEnsureWebhooks works one attempt of the daily ensure_webhooks job:
// a webhook that was deleted or changed in the provider is repaired.
// Only active and degraded instances: an installing one gets its webhooks
// from Setup, an upgrading one from the Setup after the upgrade.
func (s *Service) RunEnsureWebhooks(ctx context.Context, args EnsureWebhooksArgs) error {
	inst, err := s.Instance(ctx, args.InstanceID)
	if errors.Is(err, ErrNotFound) {
		return nil // removed meanwhile
	} else if err != nil {
		return err
	}
	if !inst.Provisionable() {
		return nil
	}
	err = s.ensureWebhooks(ctx, inst, provisionable...)
	if errors.Is(err, errStateChanged) || errors.Is(err, errWebhooksDeferred) {
		return nil // left active and degraded meanwhile, or a rotation sets the hook
	}
	if err != nil {
		s.env(inst).Logger().WarnContext(ctx, "ensuring webhooks failed", "error", err)
		return retryOrCancel(err)
	}
	return nil
}

// enqueueEnsureWebhooks queues ensure_webhooks for every active or
// degraded instance whose provider takes webhooks (the daily drift check).
// A job of the instance that already waits or runs is enough: the repair
// is periodic, not a reaction to a change.
func (s *Service) enqueueEnsureWebhooks(ctx context.Context, tx pgx.Tx, rows []store.AppInstance) error {
	for _, row := range rows {
		a, ok := s.reg.Adapter(provider.ID(row.Provider))
		if !ok {
			continue
		}
		if _, ok := a.EventSource(); !ok {
			continue
		}
		if err := s.enqueue(ctx, tx, EnsureWebhooksArgs{InstanceID: row.ID}); err != nil {
			return err
		}
	}
	return nil
}
