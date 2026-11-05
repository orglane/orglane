package apps

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// webhookSecret is the credential that signs an instance's webhooks.
const webhookSecret = "webhook_secret"

// errStateChanged: the instance left the states webhooks were ensured for
// while ensureWebhooks waited for its lock.
var errStateChanged = errors.New("apps: the instance state changed")

// ensureWebhooks makes the provider's webhooks deliver to the instance
// (EventSource.EnsureWebhooks): after every Setup and daily
// (RunEnsureWebhooks). Adapters set the active webhook_secret on the
// provider's hook, so it holds the lock of webhook_secret's rotation job:
// an ensure never interleaves with a rotation attempt, and so never puts
// back a value that a rotation just replaced. Under the lock it reloads the
// instance: one that is no longer in states (state transitions take another
// lock) gets nothing, errStateChanged. Adapters without an event source
// have nothing to ensure.
func (s *Service) ensureWebhooks(ctx context.Context, inst Instance, states ...string) error {
	if inst.Adapter == nil {
		return nil
	}
	if _, ok := inst.Adapter.EventSource(); !ok {
		return nil
	}
	_, release, err := sessionLock(ctx, s.pool, lockID("credential", inst.ID.String(), webhookSecret))
	if err != nil {
		return err
	}
	defer release()
	cur, err := s.loadInstance(ctx, store.New(s.pool), inst.ID, false)
	if errors.Is(err, ErrNotFound) {
		return errStateChanged
	} else if err != nil {
		return err
	}
	if !slices.Contains(states, cur.State) || cur.Adapter == nil {
		return errStateChanged
	}
	src, ok := cur.Adapter.EventSource()
	if !ok {
		return nil
	}
	if err := s.backfillWebhookSecret(ctx, cur.ID); err != nil {
		return err
	}
	return src.EnsureWebhooks(ctx, s.env(cur), cur.View())
}

// backfillWebhookSecret creates the active webhook_secret of an instance
// whose adapter generates it but that has no version of it: generated
// credentials are created with the instance, so an instance created before
// its adapter declared the credential has none, and its webhooks could
// never be set up. An instance with any usable version (active, a rotation
// candidate, retiring) is left alone: a rotation in progress establishes
// the active version. The caller holds the rotation lock of the credential;
// the instance row lock serializes it with RotateCredential and
// StoreCredential.
func (s *Service) backfillWebhookSecret(ctx context.Context, instanceID uuid.UUID) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, instanceID, true)
		if err != nil {
			return err
		}
		if inst.Adapter == nil {
			return nil
		}
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
	})
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
	if errors.Is(err, errStateChanged) {
		return nil // left active and degraded meanwhile
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
