package apps

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// txStarter is what a transaction can be started on: the pool, a held
// connection or a transaction (a savepoint).
type txStarter interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// inTxOn runs fn in a transaction on db.
func inTxOn(ctx context.Context, db txStarter, fn func(tx pgx.Tx, q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		return fn(tx, store.New(tx))
	})
}

// heldConn is a pooled connection that holds a session advisory lock, the
// first lock of the order in docs/specs/providers.md ("Credentials",
// concurrency model). Its holder does all database work through it: short
// transactions, the credential reads of adapters (Env) and reports. It never
// acquires a second pool connection while holding it: with
// database.max_conns=1, or a pool exhausted by other lock holders, that wait
// would never end.
type heldConn struct {
	s       *Service
	conn    *pgxpool.Conn
	release func()
	// reads serves the Env of adapter calls made under the lock. The holder
	// itself never uses the connection while an adapter call runs.
	reads *credentialReader
}

// holdLock takes session advisory lock id on a dedicated connection.
// Release it with release.
func (s *Service) holdLock(ctx context.Context, id int64) (*heldConn, error) {
	conn, release, err := sessionLock(ctx, s.pool, id)
	if err != nil {
		return nil, err
	}
	return &heldConn{s: s, conn: conn, release: release, reads: newCredentialReader(conn)}, nil
}

// queries runs single statements on the held connection, outside a
// transaction.
func (h *heldConn) queries() *store.Queries { return store.New(h.conn) }

// inTx runs fn in a short transaction on the held connection.
func (h *heldConn) inTx(ctx context.Context, fn func(tx pgx.Tx, q *store.Queries) error) error {
	return inTxOn(ctx, h.conn, fn)
}

// env is the Env for adapter calls made while the lock is held: its
// credential reads use the held connection.
func (h *heldConn) env(inst Instance) provider.Env {
	return &env{s: h.s, inst: inst, reads: h.reads}
}

// credentialReader reads credential rows for Env. A connection or a
// transaction is not safe for concurrent use, while adapters may call Env
// from several goroutines (a verifier checking several secrets at once), so
// every read holds mu: an Env is safe for concurrent use whatever it reads
// through, by construction.
type credentialReader struct {
	mu sync.Mutex
	q  *store.Queries
}

func newCredentialReader(db store.DBTX) *credentialReader {
	return &credentialReader{q: store.New(db)}
}

func (r *credentialReader) active(ctx context.Context, instanceID uuid.UUID, name string) (store.AppCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.q.GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: instanceID, Name: name})
}

func (r *credentialReader) usable(ctx context.Context, instanceID uuid.UUID, name string) ([]store.AppCredential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.q.ListUsableAppCredentials(ctx, store.ListUsableAppCredentialsParams{InstanceID: instanceID, Name: name})
}
