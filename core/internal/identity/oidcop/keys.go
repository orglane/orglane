package oidcop

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
)

// Signing keys.
//
// The server generates its token signing keys (RSA 2048, RS256: the
// algorithm every OIDC client supports) and stores them in signing_keys,
// the private key sealed with a key derived from the instance master secret
// and bound to the key ID. One key is active and signs; when it is older
// than the rotation period a new key replaces it, and the old one stays in
// the JWKS for the grace period so tokens it signed still verify. Rotation
// runs on start, from a periodic job and lazily when a token is signed, so
// it happens on schedule even if jobs are delayed.

const rsaKeyBits = 2048

// keyLockID serializes key creation and rotation across instances.
const keyLockID int64 = 0x6f72676c6b657973 // "orglkeys"

// KeyManager owns the signing keys.
type KeyManager struct {
	pool     *pgxpool.Pool
	sealer   *crypto.Sealer
	rotation time.Duration
	grace    time.Duration
	now      func() time.Time
	logger   *slog.Logger

	mu      sync.Mutex
	private map[uuid.UUID]*rsa.PrivateKey
}

// KeyManagerOptions configure a KeyManager.
type KeyManagerOptions struct {
	// Rotation is how long a key signs before it is replaced.
	Rotation time.Duration
	// Grace is how long a replaced key stays published.
	Grace time.Duration
	// Now returns the current time; defaults to time.Now. Tests move it to
	// trigger rotation.
	Now    func() time.Time
	Logger *slog.Logger
}

// NewKeyManager returns a key manager that seals private keys with the
// signing-key purpose key of keyring.
func NewKeyManager(pool *pgxpool.Pool, keyring *crypto.Keyring, opts KeyManagerOptions) *KeyManager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &KeyManager{
		pool:     pool,
		sealer:   keyring.Sealer(crypto.PurposeSigningKeys),
		rotation: opts.Rotation,
		grace:    opts.Grace,
		now:      opts.Now,
		logger:   opts.Logger,
		private:  map[uuid.UUID]*rsa.PrivateKey{},
	}
}

// Ensure creates the first key or rotates the active key when it is due,
// and deletes keys past their grace period. It reports whether a new key was
// created.
func (m *KeyManager) Ensure(ctx context.Context) (bool, error) {
	return m.rotate(ctx, false)
}

// Rotate replaces the active key now, whether or not it is due.
func (m *KeyManager) Rotate(ctx context.Context) error {
	_, err := m.rotate(ctx, true)
	return err
}

func (m *KeyManager) due(k store.SigningKey) bool {
	return !m.now().Before(k.CreatedAt.Add(m.rotation))
}

func (m *KeyManager) rotate(ctx context.Context, force bool) (bool, error) {
	// Generate outside the transaction: RSA key generation takes a while.
	var fresh *rsa.PrivateKey
	// What the transaction did is logged after it commits: a failure later
	// in the transaction rolls it back.
	created, deleted := false, int64(0)
	err := pgx.BeginFunc(ctx, m.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.AcquireXactLock(ctx, keyLockID); err != nil {
			return fmt.Errorf("oidcop: key lock: %w", err)
		}
		now := m.now().UTC()
		n, err := q.DeleteExpiredSigningKeys(ctx, now)
		if err != nil {
			return fmt.Errorf("oidcop: delete expired keys: %w", err)
		}
		deleted = n
		active, err := q.GetActiveSigningKey(ctx)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("oidcop: load active key: %w", err)
		case !force && !m.due(active):
			return nil
		default:
			retired, err := q.RetireSigningKey(ctx, store.RetireSigningKeyParams{ID: active.ID, RetiredAt: now, ExpiresAt: now.Add(m.grace)})
			if err != nil {
				return fmt.Errorf("oidcop: retire key: %w", err)
			}
			if _, err := audit.Record(ctx, q, audit.Entry{
				Actor: audit.System, Action: audit.SigningKeyRetired, Target: keyTarget(retired.ID),
				Metadata: map[string]any{"published_until": retired.ExpiresAt, "forced": force},
			}); err != nil {
				return err
			}
		}
		if fresh == nil {
			if fresh, err = rsa.GenerateKey(rand.Reader, rsaKeyBits); err != nil {
				return fmt.Errorf("oidcop: generate key: %w", err)
			}
		}
		key, err := m.storeKey(ctx, q, fresh, now)
		if err != nil {
			return err
		}
		created = true
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor: audit.System, Action: audit.SigningKeyCreated, Target: keyTarget(key.ID),
			Metadata: map[string]any{"algorithm": key.Algorithm},
		})
		return err
	})
	if err != nil {
		return false, err
	}
	if deleted > 0 {
		m.logger.InfoContext(ctx, "deleted expired signing keys", "count", deleted)
	}
	if created {
		m.logger.InfoContext(ctx, "created a new token signing key")
	}
	return created, nil
}

func (m *KeyManager) storeKey(ctx context.Context, q *store.Queries, key *rsa.PrivateKey, now time.Time) (store.SigningKey, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return store.SigningKey{}, err
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return store.SigningKey{}, err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return store.SigningKey{}, err
	}
	row, err := q.CreateSigningKey(ctx, store.CreateSigningKeyParams{
		ID:         id,
		Algorithm:  string(jose.RS256),
		PublicKey:  public,
		PrivateKey: m.sealer.Seal(private, id[:]),
		CreatedAt:  now,
	})
	if err != nil {
		return store.SigningKey{}, fmt.Errorf("oidcop: store key: %w", err)
	}
	return row, nil
}

// SigningKey returns the active key, creating or rotating it first if
// needed.
func (m *KeyManager) SigningKey(ctx context.Context) (op.SigningKey, error) {
	q := store.New(m.pool)
	active, err := q.GetActiveSigningKey(ctx)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.due(active)) {
		if _, err := m.Ensure(ctx); err != nil {
			return nil, err
		}
		active, err = q.GetActiveSigningKey(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("oidcop: load active key: %w", err)
	}
	private, err := m.decrypt(active)
	if err != nil {
		return nil, err
	}
	return &signingKey{id: active.ID.String(), key: private}, nil
}

func (m *KeyManager) decrypt(k store.SigningKey) (*rsa.PrivateKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key, ok := m.private[k.ID]; ok {
		return key, nil
	}
	der, err := m.sealer.Open(k.PrivateKey, k.ID[:])
	if err != nil {
		return nil, fmt.Errorf("oidcop: decrypt signing key %s (was identity.master_secret changed?): %w", k.ID, err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, fmt.Errorf("oidcop: parse signing key %s: %w", k.ID, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("oidcop: signing key %s is not an RSA key", k.ID)
	}
	m.private[k.ID] = key
	return key, nil
}

// PublicKeys returns the published keys (active and in grace), newest
// first.
func (m *KeyManager) PublicKeys(ctx context.Context) ([]op.Key, error) {
	rows, err := store.New(m.pool).ListPublishedSigningKeys(ctx, m.now().UTC())
	if err != nil {
		return nil, fmt.Errorf("oidcop: list keys: %w", err)
	}
	keys := make([]op.Key, 0, len(rows))
	for _, r := range rows {
		pub, err := x509.ParsePKIXPublicKey(r.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("oidcop: parse public key %s: %w", r.ID, err)
		}
		keys = append(keys, &publicKey{id: r.ID.String(), key: pub})
	}
	return keys, nil
}

type signingKey struct {
	id  string
	key *rsa.PrivateKey
}

func (k *signingKey) SignatureAlgorithm() jose.SignatureAlgorithm { return jose.RS256 }
func (k *signingKey) Key() any                                    { return k.key }
func (k *signingKey) ID() string                                  { return k.id }

type publicKey struct {
	id  string
	key any
}

func (k *publicKey) ID() string                         { return k.id }
func (k *publicKey) Algorithm() jose.SignatureAlgorithm { return jose.RS256 }
func (k *publicKey) Use() string                        { return "sig" }
func (k *publicKey) Key() any                           { return k.key }

func keyTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetSigningKey, ID: id.String()}
}
