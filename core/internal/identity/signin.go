package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
)

// AuthMethod is how a person proved their identity for a session.
type AuthMethod string

// Authentication methods.
const (
	AuthPassword AuthMethod = "password"
	AuthPasskey  AuthMethod = "passkey"
)

// Password policy (OWASP ASVS: length, not composition rules).
const (
	MinPasswordLength = 12
	MaxPasswordLength = crypto.MaxPasswordLength
)

// CheckPasswordPolicy reports whether password is acceptable for a new
// password: 12 to 1024 bytes of valid UTF-8, counted in characters for the
// minimum.
func CheckPasswordPolicy(password string) error {
	if !utf8.ValidString(password) || strings.ContainsRune(password, 0) {
		return fmt.Errorf("%w: must be valid text", ErrWeakPassword)
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("%w: must be at least %d characters", ErrWeakPassword, MinPasswordLength)
	}
	if len(password) > MaxPasswordLength {
		return fmt.Errorf("%w: must be at most %d bytes", ErrWeakPassword, MaxPasswordLength)
	}
	return nil
}

// signInAccount normalizes a work email as typed into the key used for
// throttling.
func signInAccount(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// VerifyPassword checks a work email and password and returns the person.
//
// Failures return ErrInvalidCredentials whatever the cause (unknown email,
// no password, wrong password, inactive person), take about as long, and are
// counted per account (the email as typed, existing or not): after
// SignInLimits.Failures failures in a row the account is locked and
// VerifyPassword returns a *LockedError until the lockout ends, even for
// the right password. Every failure and lockout is audited; actor carries
// the client IP and User-Agent. A success clears the failure count; the
// caller records it when it starts the session (StartSession).
func (s *Service) VerifyPassword(ctx context.Context, actor audit.Actor, email, password string) (store.Person, error) {
	account := signInAccount(email)
	if account == "" || len(account) > maxEmailLength {
		return store.Person{}, ErrInvalidCredentials
	}
	q := s.queries()
	if retry, err := q.GetSignInLock(ctx, account); err == nil {
		person, _ := q.GetPersonByWorkEmail(ctx, account)
		if err := s.recordSignInFailure(ctx, actor, account, person.ID, AuthPassword, "locked", false); err != nil {
			return store.Person{}, err
		}
		return store.Person{}, &LockedError{RetryAfter: time.Duration(max(retry, 1)) * time.Second}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.Person{}, fmt.Errorf("identity: check sign-in lock: %w", err)
	}

	person, err := q.GetPersonByWorkEmail(ctx, account)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return store.Person{}, fmt.Errorf("identity: load person: %w", err)
	}
	var hash string
	if err == nil {
		creds, err := q.GetPersonCredentials(ctx, person.ID)
		switch {
		case err == nil && creds.PasswordHash != nil:
			hash = *creds.PasswordHash
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return store.Person{}, fmt.Errorf("identity: load credentials: %w", err)
		}
	}

	ok, rehash, err := s.verifyHash(ctx, password, hash)
	if err != nil {
		return store.Person{}, err
	}
	reason := ""
	switch {
	case person.ID == uuid.Nil:
		reason = "unknown_account"
	case hash == "":
		reason = "no_password"
	case !ok:
		reason = "wrong_password"
	case Status(person.Status) != StatusActive:
		reason = "inactive"
	}
	if reason != "" {
		if err := s.recordSignInFailure(ctx, actor, account, person.ID, AuthPassword, reason, true); err != nil {
			return store.Person{}, err
		}
		return store.Person{}, ErrInvalidCredentials
	}

	if err := q.ClearSignInThrottle(ctx, account); err != nil {
		return store.Person{}, fmt.Errorf("identity: clear sign-in failures: %w", err)
	}
	if rehash {
		// Upgrade the hash to the current parameters. Not audited: the
		// password itself did not change.
		if newHash, err := s.hasher.Hash(ctx, password); err == nil {
			_, _ = q.SetPasswordHash(ctx, store.SetPasswordHashParams{PersonID: person.ID, PasswordHash: &newHash})
		}
	}
	return person, nil
}

// verifyHash checks password against hash. Without a hash it verifies
// against a dummy hash, so unknown accounts cost the same time.
func (s *Service) verifyHash(ctx context.Context, password, hash string) (ok, rehash bool, err error) {
	if hash == "" {
		dummy, err := s.dummyHash()
		if err != nil {
			return false, false, err
		}
		_, _, err = s.hasher.Verify(ctx, password, dummy)
		return false, false, err
	}
	ok, rehash, err = s.hasher.Verify(ctx, password, hash)
	if errors.Is(err, crypto.ErrMalformedHash) {
		return false, false, fmt.Errorf("identity: stored password hash: %w", err)
	}
	return ok, rehash, err
}

// recordSignInFailure audits a failed sign-in and, when count is set,
// counts it against the account, locking the account at the limit.
func (s *Service) recordSignInFailure(ctx context.Context, actor audit.Actor, account string, personID uuid.UUID, method AuthMethod, reason string, count bool) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		var target audit.Target
		if personID != uuid.Nil {
			target = personTarget(personID)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.SignInFailed,
			Target:   target,
			Metadata: map[string]any{"method": method, "reason": reason},
		}); err != nil {
			return err
		}
		if !count {
			return nil
		}
		throttle, err := q.RecordSignInFailure(ctx, store.RecordSignInFailureParams{Account: account, WindowSeconds: s.limits.Lockout.Seconds()})
		if err != nil {
			return fmt.Errorf("identity: count sign-in failure: %w", err)
		}
		if int(throttle.Failures) < s.limits.Failures {
			return nil
		}
		if err := q.LockSignInAccount(ctx, store.LockSignInAccountParams{Account: account, LockoutSeconds: s.limits.Lockout.Seconds()}); err != nil {
			return fmt.Errorf("identity: lock account: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.SignInLocked,
			Target:   target,
			Metadata: map[string]any{"failures": throttle.Failures, "lockout_seconds": int64(s.limits.Lockout.Seconds())},
		})
		return err
	})
}

// RecordSignInRateLimited audits a sign-in attempt rejected by the per-IP
// rate limit.
func (s *Service) RecordSignInRateLimited(ctx context.Context, actor audit.Actor, method AuthMethod) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		_, err := audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.SignInRateLimited, Metadata: map[string]any{"method": method}})
		return err
	})
}

// SetPassword sets or replaces a person's password after checking the
// policy.
func (s *Service) SetPassword(ctx context.Context, actor audit.Actor, personID uuid.UUID, password string) error {
	if err := CheckPasswordPolicy(password); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		return setPasswordHash(ctx, q, actor, personID, hash)
	})
}

func setPasswordHash(ctx context.Context, q *store.Queries, actor audit.Actor, personID uuid.UUID, hash string) error {
	if _, err := q.SetPasswordHash(ctx, store.SetPasswordHashParams{PersonID: personID, PasswordHash: &hash}); err != nil {
		return fmt.Errorf("identity: set password: %w", err)
	}
	_, err := audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.PersonPasswordChanged, Target: personTarget(personID)})
	return err
}

// SignInOptions describe the session StartSession creates.
type SignInOptions struct {
	// Method is how the person authenticated.
	Method AuthMethod
	// DeviceID is the device the client already has, if any (the web
	// device cookie). It is reused when it is a device of this person on
	// the same platform and not revoked; otherwise a new device is
	// registered.
	DeviceID uuid.UUID
	// DeviceName and Platform describe a new device.
	DeviceName string
	Platform   Platform
	// TTL is the absolute lifetime of the session.
	TTL time.Duration
}

// SignedIn is the result of StartSession.
type SignedIn struct {
	Person  store.Person
	Device  store.Device
	Session store.Session
	// Token is the session token, returned only here.
	Token string
}

// StartSession signs a person in after they authenticated: it reuses or
// registers their device and creates a session bound to it, with audit
// entries for the device, the session and the sign-in. The person must be
// active.
func (s *Service) StartSession(ctx context.Context, actor audit.Actor, personID uuid.UUID, opts SignInOptions) (SignedIn, error) {
	if opts.TTL <= 0 {
		return SignedIn{}, invalid("ttl", "must be positive")
	}
	switch opts.Method {
	case AuthPassword, AuthPasskey:
	default:
		return SignedIn{}, invalid("method", "unknown method %q", opts.Method)
	}
	name := strings.TrimSpace(opts.DeviceName)
	if name == "" {
		name = "Web browser"
	}
	if utf8.RuneCountInString(name) > maxDeviceNameLength {
		name = string([]rune(name)[:maxDeviceNameLength])
	}
	token, err := newSessionToken()
	if err != nil {
		return SignedIn{}, err
	}

	var out SignedIn
	err = s.inTx(ctx, func(q *store.Queries) error {
		// Lock order: person, device, sessions (see Service).
		person, err := q.GetPersonForUpdate(ctx, personID)
		if err != nil {
			return notFound(err, "person")
		}
		if Status(person.Status) != StatusActive {
			return fmt.Errorf("%w: %s", ErrInactive, person.Status)
		}
		out.Person = person

		if opts.DeviceID != uuid.Nil {
			d, err := q.GetDeviceForUpdate(ctx, opts.DeviceID)
			if err == nil && d.PersonID == personID && d.RevokedAt == nil && d.Platform == string(opts.Platform) {
				out.Device = d
			} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("identity: load device: %w", err)
			}
		}
		if out.Device.ID == uuid.Nil {
			if out.Device, err = q.CreateDevice(ctx, store.CreateDeviceParams{PersonID: personID, Name: name, Platform: string(opts.Platform)}); err != nil {
				return fmt.Errorf("identity: register device: %w", err)
			}
			if _, err := audit.Record(ctx, q, audit.Entry{
				Actor:    actor,
				Action:   audit.DeviceRegistered,
				Target:   deviceTarget(out.Device.ID),
				Metadata: map[string]any{"person_id": personID, "platform": out.Device.Platform},
			}); err != nil {
				return err
			}
		} else if err := q.TouchDevice(ctx, out.Device.ID); err != nil {
			return fmt.Errorf("identity: touch device: %w", err)
		}

		out.Session, err = q.CreateSession(ctx, store.CreateSessionParams{
			PersonID:    personID,
			DeviceID:    out.Device.ID,
			TokenHash:   hashSessionToken(token),
			TtlSeconds:  opts.TTL.Seconds(),
			AuthMethods: []string{string(opts.Method)},
		})
		if err != nil {
			return fmt.Errorf("identity: create session: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:  actor,
			Action: audit.SessionCreated,
			Target: sessionTarget(out.Session.ID),
			Metadata: map[string]any{
				"person_id":  personID,
				"device_id":  out.Device.ID,
				"expires_at": out.Session.ExpiresAt,
				"method":     opts.Method,
			},
		}); err != nil {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.SignInSucceeded,
			Target:   personTarget(personID),
			Metadata: map[string]any{"method": opts.Method, "session_id": out.Session.ID, "device_id": out.Device.ID},
		})
		return err
	})
	if err != nil {
		return SignedIn{}, err
	}
	out.Token = token
	return out, nil
}

// ValidateSession returns the session for a token if it is valid: not
// revoked, within its absolute lifetime, used within idle, on a device that
// is not revoked, of an active person. It checks the database on every
// call, so revocation takes effect immediately, and records activity at
// most once a minute. Any other token returns ErrNotFound.
func (s *Service) ValidateSession(ctx context.Context, token string, idle time.Duration) (store.Session, error) {
	if !strings.HasPrefix(token, SessionTokenPrefix) {
		return store.Session{}, fmt.Errorf("%w: session", ErrNotFound)
	}
	q := s.queries()
	session, err := q.GetValidSessionByTokenHash(ctx, store.GetValidSessionByTokenHashParams{
		TokenHash:   hashSessionToken(token),
		IdleSeconds: idle.Seconds(),
	})
	if err != nil {
		return store.Session{}, notFound(err, "session")
	}
	if err := q.TouchSession(ctx, session.ID); err != nil {
		return store.Session{}, fmt.Errorf("identity: touch session: %w", err)
	}
	if err := q.TouchDevice(ctx, session.DeviceID); err != nil {
		return store.Session{}, fmt.Errorf("identity: touch device: %w", err)
	}
	return session, nil
}

// ValidSessions returns the valid sessions of a person (see
// ValidateSession), oldest first, with their devices.
func (s *Service) ValidSessions(ctx context.Context, personID uuid.UUID, idle time.Duration) ([]store.ListValidPersonSessionsRow, error) {
	return s.queries().ListValidPersonSessions(ctx, store.ListValidPersonSessionsParams{PersonID: personID, IdleSeconds: idle.Seconds()})
}

// RevokeOwnSession revokes a session of personID. Sessions of other people
// are reported as not found. reason is recorded in the audit entry, e.g.
// "signed_out".
func (s *Service) RevokeOwnSession(ctx context.Context, actor audit.Actor, personID, sessionID uuid.UUID, reason string) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		session, err := q.GetSessionForUpdate(ctx, sessionID)
		if err != nil || session.PersonID != personID {
			return fmt.Errorf("%w: session", ErrNotFound)
		}
		if session.RevokedAt != nil {
			return nil
		}
		if session, err = q.RevokeSession(ctx, sessionID); err != nil {
			return fmt.Errorf("identity: revoke session: %w", err)
		}
		return recordSessionsRevoked(ctx, q, actor, []store.Session{session}, reason)
	})
}
