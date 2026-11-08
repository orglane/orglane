package apps

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// EnvForTest returns the Env adapters get for an instance, so tests can read
// credential plaintext the way an adapter does.
func (s *Service) EnvForTest(inst Instance) provider.Env { return s.env(inst) }

// CredentialADForTest exposes the additional data a credential row is
// sealed with.
func CredentialADForTest(instanceID [16]byte, name string, version int32) []byte {
	return credentialAD(instanceID, name, version)
}

// ResyncForTest records the instance's current desired state again as a
// new generation (reason resync), as the sync does after the agent lost it.
func (s *Service) ResyncForTest(ctx context.Context, id uuid.UUID) (int64, error) {
	var gen int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := store.New(tx).GetCurrentAppDesiredSpec(ctx, id)
		if err != nil {
			return err
		}
		ds, err := s.RecordDesired(ctx, tx, audit.System, id, DesiredChange{RunState: cur.RunState, Reason: ReasonResync})
		gen = ds.Generation
		return err
	})
	return gen, err
}

// CheckBootForTest runs one WatchAgent tick.
func (s *Service) CheckBootForTest(ctx context.Context) { s.checkBoot(ctx) }

// CredentialLockIDForTest is the session lock a rotation worker holds, so
// tests can find (and terminate) the worker's database backend.
func CredentialLockIDForTest(instanceID [16]byte, name string) int64 {
	return credentialLockID(instanceID, name)
}

// LockIDForTest is the advisory lock ID of parts.
func LockIDForTest(parts ...string) int64 { return lockID(parts...) }
