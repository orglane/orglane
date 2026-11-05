// Package identity is the domain layer for people, roles, teams, devices and
// sessions (ADR 0003, 0004, 0012).
//
// Every change to these entities goes through Service, which runs it in a
// transaction together with an audit entry (package audit): a change is
// never committed without its entry. Callers pass the audit.Actor performing
// the change. Methods that would not change anything (assigning a role a
// person already holds, revoking a revoked session) succeed without writing.
//
// The same holds for sign-in state: passwords, passkeys, WebAuthn ceremonies,
// sign-in throttling, the first-run setup token and OIDC clients (sign-in.go,
// setup.go, passkeys.go, oidc_clients.go). HTTP handlers and the OIDC
// provider (subpackages web and oidcop) build on this package; they never
// write these tables through package store directly, except for the OIDC
// protocol state (authorization requests, tokens, signing keys), which
// package oidcop owns.
package identity

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/crypto"
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
	// ErrInvalidCredentials: wrong email, password or passkey. Callers must
	// not tell these cases apart in responses.
	ErrInvalidCredentials = errors.New("identity: invalid credentials")
	ErrSetupDone          = errors.New("identity: setup is already complete")
	ErrInvalidSetupToken  = errors.New("identity: invalid setup token")
	ErrWeakPassword       = errors.New("identity: password does not meet the policy")
	ErrPasskeysDisabled   = errors.New("identity: passkeys are not configured")
	// ErrInvitationInvalid: the invitation token is unknown, revoked or
	// replaced by a newer one, or the person is no longer invited.
	ErrInvitationInvalid = errors.New("identity: the invitation is not valid")
	// ErrInvitationExpired: the invitation token expired.
	ErrInvitationExpired = errors.New("identity: the invitation has expired")
	// ErrInvitationUsed: the invitation was already accepted.
	ErrInvitationUsed = errors.New("identity: the invitation was already accepted")
)

// LockedError is returned when an account is locked after too many failed
// sign-ins.
type LockedError struct {
	RetryAfter time.Duration
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("identity: account is locked, retry after %s", e.RetryAfter)
}

// Service changes and reads people, roles, teams, devices and sessions.
//
// Concurrent changes are serialized with row locks taken in one order:
// the person, then the device, then sessions; the owner role row is locked
// after the person when the last-owner rule applies. Keeping that order
// avoids deadlocks between, for example, suspending a person and revoking
// one of their devices. Team changes lock the team before anything else;
// changes that can affect the team hierarchy take an advisory lock first
// (teams.go).
type Service struct {
	pool     *pgxpool.Pool
	hasher   *crypto.PasswordHasher
	limits   SignInLimits
	webAuthn *webauthn.WebAuthn
	mailer   Mailer
	invites  InvitationConfig
	observer DirectoryObserver
	// localDomain, when set, is the local-mode base domain (localhost or a
	// *.localhost name): plain-http redirect URIs may use it and its
	// subdomains (validateRedirectURI).
	localDomain string
	// dummyHash is verified for unknown accounts, so a sign-in takes as long
	// whether or not the account exists.
	dummyHash func() (string, error)
}

// SignInLimits configure the per-account lockout of password sign-in.
type SignInLimits struct {
	// Failures is how many failed attempts in a row lock the account.
	Failures int
	// Lockout is how long a locked account stays locked. It is also the
	// window after which a failure no longer counts.
	Lockout time.Duration
}

// DefaultSignInLimits lock an account for 15 minutes after 5 failures.
var DefaultSignInLimits = SignInLimits{Failures: 5, Lockout: 15 * time.Minute}

// Option configures a Service.
type Option func(*Service)

// WithPasswordHasher sets the password hasher. The default uses
// crypto.DefaultPasswordParams.
func WithPasswordHasher(h *crypto.PasswordHasher) Option {
	return func(s *Service) { s.hasher = h }
}

// WithSignInLimits sets the account lockout limits.
func WithSignInLimits(l SignInLimits) Option {
	return func(s *Service) { s.limits = l }
}

// WithWebAuthn enables passkeys with the given relying party.
func WithWebAuthn(w *webauthn.WebAuthn) Option {
	return func(s *Service) { s.webAuthn = w }
}

// WithMailer uses the mail channel authority to choose invitation delivery;
// without it they are always issued as links to share.
func WithMailer(m Mailer) Option {
	return func(s *Service) { s.mailer = m }
}

// WithInvitations configures invitation links.
func WithInvitations(c InvitationConfig) Option {
	return func(s *Service) { s.invites = c }
}

// WithLocalServiceDomain allows plain-http OIDC redirect URIs on a
// local-mode base domain and its subdomains, e.g. orglane.localhost and
// http://git.orglane.localhost/... for managed services in local mode
// (docs/specs/agent.md "Local mode"). Browsers resolve every *.localhost
// name to loopback (RFC 6761), so these are loopback redirects like
// http://localhost. Anything else, or a domain that is neither localhost
// nor below it, leaves the rule at literal loopback hosts.
func WithLocalServiceDomain(base string) Option {
	return func(s *Service) {
		if base == "localhost" || (strings.HasSuffix(base, ".localhost") && validLocalHost(base)) {
			s.localDomain = base
		}
	}
}

// NewService returns a Service using pool.
func NewService(pool *pgxpool.Pool, opts ...Option) *Service {
	s := &Service{pool: pool, limits: DefaultSignInLimits, invites: DefaultInvitationConfig}
	for _, opt := range opts {
		opt(s)
	}
	if s.hasher == nil {
		s.hasher = crypto.NewPasswordHasher(crypto.DefaultPasswordParams, runtime.GOMAXPROCS(0))
	}
	s.dummyHash = sync.OnceValues(func() (string, error) {
		return s.hasher.Hash(context.Background(), "orglane-dummy-password")
	})
	return s
}

// inTx runs fn in a transaction. fn's changes and audit entries commit
// together or not at all.
func (s *Service) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(store.New(tx))
	})
}

// inTxWith is inTx for changes that also queue work in the transaction
// (system email).
func (s *Service) inTxWith(ctx context.Context, fn func(tx pgx.Tx, q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(tx, store.New(tx))
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

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// DirectoryObserver is told, inside the transaction of a directory change,
// which people and teams it affects, so it can queue work atomically with
// the change (the provider framework queues provisioning). Every member of
// a listed team is affected.
type DirectoryObserver interface {
	DirectoryChanged(ctx context.Context, q *store.Queries, people, teams []uuid.UUID) error
}

// SetDirectoryObserver attaches the observer of directory changes. Set it
// before serving requests.
func (s *Service) SetDirectoryObserver(o DirectoryObserver) {
	s.observer = o
}

// directoryChanged reports a change to the observer, if any.
func (s *Service) directoryChanged(ctx context.Context, q *store.Queries, people []uuid.UUID, teams ...uuid.UUID) error {
	if s.observer == nil || (len(people) == 0 && len(teams) == 0) {
		return nil
	}
	return s.observer.DirectoryChanged(ctx, q, people, teams)
}
