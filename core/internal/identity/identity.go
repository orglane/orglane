// Package identity is the domain layer for people, roles, devices and
// sessions (ADR 0003, 0004, 0012).
//
// Every change to these entities goes through Service, which runs it in a
// transaction together with an audit entry (package audit): a change is
// never committed without its entry. Callers pass the audit.Actor performing
// the change. Methods that would not change anything (assigning a role a
// person already holds, revoking a revoked session) succeed without writing.
//
// HTTP handlers and the sign-in flows (#6) build on this package; they never
// write these tables through package store directly.
package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/store"
)

// Errors returned by Service. Validation errors wrap ErrInvalid with the
// offending field.
var (
	ErrNotFound          = errors.New("identity: not found")
	ErrInvalid           = errors.New("identity: invalid input")
	ErrUsernameTaken     = errors.New("identity: username is taken")
	ErrEmailTaken        = errors.New("identity: work email is taken")
	ErrInvalidTransition = errors.New("identity: status transition not allowed")
	ErrLastOwner         = errors.New("identity: the last active owner cannot lose the owner role")
	ErrInactive          = errors.New("identity: person is not active")
	ErrRevoked           = errors.New("identity: device is revoked")
)

// Service changes and reads people, roles, devices and sessions.
//
// Concurrent changes are serialized with row locks taken in one order:
// the person, then the device, then sessions; the owner role row is locked
// after the person when the last-owner rule applies. Keeping that order
// avoids deadlocks between, for example, suspending a person and revoking
// one of their devices.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service using pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// inTx runs fn in a transaction. fn's changes and audit entries commit
// together or not at all.
func (s *Service) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(store.New(tx))
	})
}

func (s *Service) queries() *store.Queries {
	return store.New(s.pool)
}

func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(format, args...))
}

// notFound maps "no rows" to ErrNotFound and wraps other errors.
func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return fmt.Errorf("identity: load %s: %w", what, err)
}

// uniqueViolation maps unique constraint violations on people to their
// errors and returns other errors unchanged.
func uniqueViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "people_username_key":
		return ErrUsernameTaken
	case "people_work_email_key":
		return ErrEmailTaken
	}
	return err
}
