package apps

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// webhookSecret is the credential that signs an instance's webhooks.
const webhookSecret = "webhook_secret"

// ensureWebhooks makes the provider's webhooks deliver to the instance
// (EventSource.EnsureWebhooks): after every Setup and daily
// (RunEnsureWebhooks). Adapters set the active webhook_secret on the
// provider's hook, so it holds the lock of webhook_secret's rotation job:
// an ensure never interleaves with a rotation attempt, and so never puts
// back a value that a rotation just replaced. Adapters without an event
// source have nothing to ensure.
func (s *Service) ensureWebhooks(ctx context.Context, inst Instance) error {
	if inst.Adapter == nil {
		return nil
	}
	src, ok := inst.Adapter.EventSource()
	if !ok {
		return nil
	}
	_, release, err := sessionLock(ctx, s.pool, lockID("credential", inst.ID.String(), webhookSecret))
	if err != nil {
		return err
	}
	defer release()
	// Reload under the lock: the instance may have left its state.
	cur, err := s.loadInstance(ctx, store.New(s.pool), inst.ID, false)
	if err != nil {
		return err
	}
	return src.EnsureWebhooks(ctx, s.env(cur), cur.View())
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
	if err := s.ensureWebhooks(ctx, inst); err != nil {
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
