package apps

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// errWebhooksDeferred: a rotation of webhook_secret is in progress, so the
// webhooks were left as they are (see ensureWebhooks).
var errWebhooksDeferred = errors.New("apps: webhooks deferred while the webhook secret is rotated")

// ensureWebhooks makes the provider's webhooks deliver to the instance
// (EventSource.EnsureWebhooks): after every Setup and daily
// (RunEnsureWebhooks). Adapters install the active webhook_secret on the
// provider's hook, so an ensure is a provider execution for that
// credential, like a rotation attempt:
//
//   - It holds the session lock of webhook_secret's rotation job, so it
//     never runs while a rotation attempt does, and does all database work
//     on that lock's connection (the reload, one short transaction, the
//     credential reads of the adapter through heldEnv): it never acquires a
//     second pool connection while holding one, which would never end with
//     database.max_conns=1 or a pool exhausted by other lock holders.
//   - In that transaction, under the instance row lock (which rotations,
//     abandons and direct replacements take to change credentials), it
//     rechecks the instance: one that left states gets nothing
//     (errStateChanged). While any candidate of webhook_secret exists
//     (pending, uncertain or abandoning) it installs nothing
//     (errWebhooksDeferred): the provider may already use the candidate
//     (an attempt whose answer was lost), and putting the active value back
//     would make a later proof by use activate a value the provider no
//     longer uses. The rotation job establishes the hook with the value
//     that wins. A candidate created after this check has had no attempt
//     yet (attempts need the lock this holds), so the ensure runs before
//     the rotation's first attempt.
//   - It creates a missing webhook_secret first (backfillWebhookSecret).
//
// #80's credential model adds execution leases (at most one provider
// execution per credential, including executions whose answer was lost):
// once it lands, the adapter call here becomes an execution on the active
// version's row (beginCall/end) and heldEnv becomes heldConn.env.
func (s *Service) ensureWebhooks(ctx context.Context, inst Instance, states ...string) error {
	if inst.Adapter == nil {
		return nil
	}
	if _, ok := inst.Adapter.EventSource(); !ok {
		return nil
	}
	conn, release, err := sessionLock(ctx, s.pool, lockID("credential", inst.ID.String(), webhookSecret))
	if err != nil {
		return err
	}
	defer release()
	var cur Instance
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		cur, err = s.loadInstance(ctx, q, inst.ID, true)
		if errors.Is(err, ErrNotFound) {
			return errStateChanged
		} else if err != nil {
			return err
		}
		if !slices.Contains(states, cur.State) || cur.Adapter == nil {
			return errStateChanged
		}
		if _, err := q.GetAppCredentialCandidate(ctx, store.GetAppCredentialCandidateParams{InstanceID: cur.ID, Name: webhookSecret}); err == nil {
			return errWebhooksDeferred
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("apps: load webhook secret candidate: %w", err)
		}
		return s.backfillWebhookSecret(ctx, q, cur)
	})
	if err != nil {
		return err
	}
	src, ok := cur.Adapter.EventSource()
	if !ok {
		return nil
	}
	return src.EnsureWebhooks(ctx, s.heldEnv(cur, conn), cur.View())
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

// heldEnv is the Env of an adapter call made while a lock connection is
// held: its credential reads go through that connection, never the pool.
// A connection is not safe for concurrent use and adapters may call Env
// from several goroutines, so every read holds mu. The holder does not use
// the connection while the adapter call runs. (#80's heldConn.env
// replaces it.)
type heldEnv struct {
	provider.Env
	s    *Service
	inst Instance
	mu   *sync.Mutex
	q    *store.Queries
}

func (s *Service) heldEnv(inst Instance, conn *pgxpool.Conn) provider.Env {
	return &heldEnv{Env: s.env(inst), s: s, inst: inst, mu: &sync.Mutex{}, q: store.New(conn)}
}

// Credential implements provider.Env.
func (e *heldEnv) Credential(ctx context.Context, name string) ([]byte, error) {
	e.mu.Lock()
	row, err := e.q.GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: e.inst.ID, Name: name})
	e.mu.Unlock()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, provider.ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("apps: load credential: %w", err)
	}
	return e.s.openCredential(row)
}

// Credentials implements provider.Env.
func (e *heldEnv) Credentials(ctx context.Context, name string) ([][]byte, error) {
	e.mu.Lock()
	rows, err := e.q.ListUsableAppCredentials(ctx, store.ListUsableAppCredentialsParams{InstanceID: e.inst.ID, Name: name})
	e.mu.Unlock()
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
