package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// SessionTokenPrefix starts every session token, so leaked tokens are easy
// to recognize (secret scanners, logs).
const SessionTokenPrefix = "ols_"

// sessionTokenBytes is the entropy of a session token. With 256 random bits
// a fast hash is enough to store it: there is nothing to brute-force.
const sessionTokenBytes = 32

// CreateSession starts a session on a device and returns it with its token.
// The token is returned only here; the database stores its SHA-256 hash.
// The device must not be revoked and its person must be active.
func (s *Service) CreateSession(ctx context.Context, actor audit.Actor, deviceID uuid.UUID, ttl time.Duration) (store.Session, string, error) {
	if ttl <= 0 {
		return store.Session{}, "", invalid("ttl", "must be positive")
	}
	token, err := newSessionToken()
	if err != nil {
		return store.Session{}, "", err
	}

	var session store.Session
	err = s.inTx(ctx, func(q *store.Queries) error {
		device, err := q.GetDevice(ctx, deviceID)
		if err != nil {
			return notFound(err, "device")
		}
		// Lock order: person, device, sessions (see Service). Holding both
		// locks means a concurrent suspension or device revocation, which
		// revoke sessions, cannot miss this one.
		person, err := q.GetPersonForUpdate(ctx, device.PersonID)
		if err != nil {
			return notFound(err, "person")
		}
		if Status(person.Status) != StatusActive {
			return fmt.Errorf("%w: %s", ErrInactive, person.Status)
		}
		if device, err = q.GetDeviceForUpdate(ctx, deviceID); err != nil {
			return notFound(err, "device")
		}
		if device.RevokedAt != nil {
			return ErrRevoked
		}
		session, err = q.CreateSession(ctx, store.CreateSessionParams{
			PersonID:   device.PersonID,
			DeviceID:   deviceID,
			TokenHash:  hashSessionToken(token),
			TtlSeconds: ttl.Seconds(),
		})
		if err != nil {
			return fmt.Errorf("identity: create session: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:  actor,
			Action: audit.SessionCreated,
			Target: sessionTarget(session.ID),
			Metadata: map[string]any{
				"person_id":  session.PersonID,
				"device_id":  session.DeviceID,
				"expires_at": session.ExpiresAt,
			},
		})
		return err
	})
	if err != nil {
		return store.Session{}, "", err
	}
	return session, token, nil
}

// SessionByToken returns the active session for a token: not revoked, not
// expired, on a device that is not revoked, of an active person. Any other
// token returns ErrNotFound.
func (s *Service) SessionByToken(ctx context.Context, token string) (store.Session, error) {
	session, err := s.queries().GetActiveSessionByTokenHash(ctx, hashSessionToken(token))
	if err != nil {
		return store.Session{}, notFound(err, "session")
	}
	return session, nil
}

// RevokeSession revokes a session. Revoking a revoked session is a no-op.
func (s *Service) RevokeSession(ctx context.Context, actor audit.Actor, id uuid.UUID) (store.Session, error) {
	var session store.Session
	err := s.inTx(ctx, func(q *store.Queries) error {
		current, err := q.GetSession(ctx, id)
		if err != nil {
			return notFound(err, "session")
		}
		// Lock order: person, device, sessions (see Service).
		if _, err := q.GetPersonForUpdate(ctx, current.PersonID); err != nil {
			return notFound(err, "person")
		}
		if session, err = q.GetSessionForUpdate(ctx, id); err != nil {
			return notFound(err, "session")
		}
		if session.RevokedAt != nil {
			return nil
		}
		if session, err = q.RevokeSession(ctx, id); err != nil {
			return fmt.Errorf("identity: revoke session: %w", err)
		}
		return recordSessionsRevoked(ctx, q, actor, []store.Session{session}, "revoked")
	})
	if err != nil {
		return store.Session{}, err
	}
	return session, nil
}

// RevokePersonSessions revokes every active session of a person ("sign out
// everywhere") and returns how many were revoked.
func (s *Service) RevokePersonSessions(ctx context.Context, actor audit.Actor, personID uuid.UUID) (int, error) {
	var n int
	err := s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		sessions, err := q.RevokePersonSessions(ctx, personID)
		if err != nil {
			return fmt.Errorf("identity: revoke sessions: %w", err)
		}
		n = len(sessions)
		return recordSessionsRevoked(ctx, q, actor, sessions, "revoked")
	})
	return n, err
}

// PersonSessions returns the sessions of a person, oldest first, including
// revoked and expired ones.
func (s *Service) PersonSessions(ctx context.Context, personID uuid.UUID) ([]store.Session, error) {
	return s.queries().ListPersonSessions(ctx, personID)
}

// recordSessionsRevoked writes one audit entry per revoked session. reason
// says why: "revoked" for a direct revocation, otherwise the change that
// caused it (device_revoked, person_suspended, ...).
func recordSessionsRevoked(ctx context.Context, q *store.Queries, actor audit.Actor, sessions []store.Session, reason string) error {
	for _, session := range sessions {
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:  actor,
			Action: audit.SessionRevoked,
			Target: sessionTarget(session.ID),
			Metadata: map[string]any{
				"person_id": session.PersonID,
				"device_id": session.DeviceID,
				"reason":    reason,
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func newSessionToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: generate session token: %w", err)
	}
	return SessionTokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func hashSessionToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func sessionTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetSession, ID: id.String()}
}
