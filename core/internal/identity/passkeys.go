package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// Passkeys (WebAuthn, go-webauthn). The relying party ID is the workspace
// host (ADR 0018); the WebAuthn user handle is the person ID.
//
// A ceremony has two steps. Begin* returns the options for the browser
// (navigator.credentials.create or .get) and a ceremony ID; the challenge
// state is kept in the database, bound to the person for registrations.
// Finish* takes the ceremony ID and the browser's response; each ceremony
// can be finished once, within CeremonyTTL.

// CeremonyTTL bounds the time between Begin* and Finish*.
const CeremonyTTL = 5 * time.Minute

// PasskeyOfferInterval is how long a dismissed passkey offer stays hidden
// before it is shown again.
const PasskeyOfferInterval = 30 * 24 * time.Hour

const (
	ceremonyRegistration = "registration"
	ceremonyLogin        = "login"
)

// DefaultPasskeyName names passkeys registered without a name.
const DefaultPasskeyName = "Passkey"

// webAuthnUser adapts a person to webauthn.User.
type webAuthnUser struct {
	person      store.Person
	credentials []webauthn.Credential
}

func (u *webAuthnUser) WebAuthnID() []byte                         { return u.person.ID[:] }
func (u *webAuthnUser) WebAuthnName() string                       { return u.person.WorkEmail }
func (u *webAuthnUser) WebAuthnDisplayName() string                { return u.person.DisplayName }
func (u *webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

func loadWebAuthnUser(ctx context.Context, q *store.Queries, person store.Person) (*webAuthnUser, error) {
	passkeys, err := q.ListPersonPasskeys(ctx, person.ID)
	if err != nil {
		return nil, fmt.Errorf("identity: list passkeys: %w", err)
	}
	u := &webAuthnUser{person: person}
	for _, pk := range passkeys {
		var c webauthn.Credential
		if err := json.Unmarshal(pk.Credential, &c); err != nil {
			return nil, fmt.Errorf("identity: decode passkey %s: %w", pk.ID, err)
		}
		u.credentials = append(u.credentials, c)
	}
	return u, nil
}

// BeginPasskeyRegistration starts registering a passkey for an active
// person. The options exclude the person's existing passkeys and require a
// discoverable credential, so it can sign in without a username.
func (s *Service) BeginPasskeyRegistration(ctx context.Context, personID uuid.UUID) (uuid.UUID, *protocol.CredentialCreation, error) {
	if s.webAuthn == nil {
		return uuid.Nil, nil, ErrPasskeysDisabled
	}
	q := s.queries()
	person, err := q.GetPerson(ctx, personID)
	if err != nil {
		return uuid.Nil, nil, notFound(err, "person")
	}
	if Status(person.Status) != StatusActive {
		return uuid.Nil, nil, fmt.Errorf("%w: %s", ErrInactive, person.Status)
	}
	user, err := loadWebAuthnUser(ctx, q, person)
	if err != nil {
		return uuid.Nil, nil, err
	}
	exclude := make([]protocol.CredentialDescriptor, 0, len(user.credentials))
	for _, c := range user.credentials {
		exclude = append(exclude, c.Descriptor())
	}
	options, session, err := s.webAuthn.BeginRegistration(user,
		webauthn.WithExclusions(exclude),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("identity: begin passkey registration: %w", err)
	}
	id, err := s.storeCeremony(ctx, q, ceremonyRegistration, &personID, session)
	if err != nil {
		return uuid.Nil, nil, err
	}
	return id, options, nil
}

// FinishPasskeyRegistration verifies the browser's attestation response for
// a registration ceremony of the same person and stores the passkey.
// Invalid responses return ErrInvalid.
func (s *Service) FinishPasskeyRegistration(ctx context.Context, actor audit.Actor, personID, ceremonyID uuid.UUID, name string, response []byte) (store.Passkey, error) {
	if s.webAuthn == nil {
		return store.Passkey{}, ErrPasskeysDisabled
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultPasskeyName
	}
	if utf8.RuneCountInString(name) > maxDeviceNameLength || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return store.Passkey{}, invalid("name", "must be 1-%d characters", maxDeviceNameLength)
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return store.Passkey{}, invalid("credential", "malformed registration response")
	}

	var passkey store.Passkey
	err = s.inTx(ctx, func(q *store.Queries) error {
		session, err := takeCeremony(ctx, q, ceremonyID, ceremonyRegistration)
		if err != nil {
			return err
		}
		person, err := q.GetPersonForUpdate(ctx, personID)
		if err != nil {
			return notFound(err, "person")
		}
		if !bytes.Equal(session.UserID, person.ID[:]) {
			return invalid("ceremony", "belongs to someone else")
		}
		if Status(person.Status) != StatusActive {
			return fmt.Errorf("%w: %s", ErrInactive, person.Status)
		}
		user, err := loadWebAuthnUser(ctx, q, person)
		if err != nil {
			return err
		}
		credential, err := s.webAuthn.CreateCredential(user, session, parsed)
		if err != nil {
			return invalid("credential", "registration could not be verified")
		}
		raw, err := json.Marshal(credential)
		if err != nil {
			return err
		}
		passkey, err = q.CreatePasskey(ctx, store.CreatePasskeyParams{
			PersonID: personID, CredentialID: credential.ID, Name: name, Credential: raw,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return invalid("credential", "this passkey is already registered")
			}
			return fmt.Errorf("identity: store passkey: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PasskeyAdded,
			Target:   passkeyTarget(passkey.ID),
			Metadata: map[string]any{"person_id": personID, "name": name, "aaguid": uuidOrEmpty(credential.Authenticator.AAGUID)},
		})
		return err
	})
	if err != nil {
		return store.Passkey{}, err
	}
	return passkey, nil
}

// BeginPasskeySignIn starts a passkey sign-in without a username
// (discoverable credentials).
func (s *Service) BeginPasskeySignIn(ctx context.Context) (uuid.UUID, *protocol.CredentialAssertion, error) {
	if s.webAuthn == nil {
		return uuid.Nil, nil, ErrPasskeysDisabled
	}
	options, session, err := s.webAuthn.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return uuid.Nil, nil, fmt.Errorf("identity: begin passkey sign-in: %w", err)
	}
	id, err := s.storeCeremony(ctx, s.queries(), ceremonyLogin, nil, session)
	if err != nil {
		return uuid.Nil, nil, err
	}
	return id, options, nil
}

// FinishPasskeySignIn verifies the browser's assertion for a sign-in
// ceremony and returns the person it belongs to. Every failure returns
// ErrInvalidCredentials and is audited. A sign counter that went backwards
// (a possibly cloned authenticator) fails the sign-in.
func (s *Service) FinishPasskeySignIn(ctx context.Context, actor audit.Actor, ceremonyID uuid.UUID, response []byte) (store.Person, error) {
	if s.webAuthn == nil {
		return store.Person{}, ErrPasskeysDisabled
	}
	fail := func(personID uuid.UUID, reason string) (store.Person, error) {
		if err := s.recordSignInFailure(ctx, actor, "", personID, AuthPasskey, reason, false); err != nil {
			return store.Person{}, err
		}
		return store.Person{}, ErrInvalidCredentials
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return fail(uuid.Nil, "malformed_response")
	}
	q := s.queries()
	session, err := takeCeremony(ctx, q, ceremonyID, ceremonyLogin)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fail(uuid.Nil, "unknown_ceremony")
		}
		return store.Person{}, err
	}

	var (
		person  store.Person
		passkey store.Passkey
	)
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		pk, err := q.GetPasskeyByCredentialID(ctx, rawID)
		if err != nil {
			return nil, fmt.Errorf("unknown passkey: %w", err)
		}
		if !bytes.Equal(userHandle, pk.PersonID[:]) {
			return nil, errors.New("user handle does not match the passkey")
		}
		p, err := q.GetPerson(ctx, pk.PersonID)
		if err != nil {
			return nil, err
		}
		user, err := loadWebAuthnUser(ctx, q, p)
		if err != nil {
			return nil, err
		}
		person, passkey = p, pk
		return user, nil
	}
	credential, err := s.webAuthn.ValidateDiscoverableLogin(handler, session, parsed)
	if err != nil {
		return fail(person.ID, "verification_failed")
	}
	if credential.Authenticator.CloneWarning {
		return fail(person.ID, "sign_counter_regressed")
	}
	if Status(person.Status) != StatusActive {
		return fail(person.ID, "inactive")
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return store.Person{}, err
	}
	if err := q.RecordPasskeyUse(ctx, store.RecordPasskeyUseParams{ID: passkey.ID, Credential: raw}); err != nil {
		return store.Person{}, fmt.Errorf("identity: record passkey use: %w", err)
	}
	return person, nil
}

func (s *Service) storeCeremony(ctx context.Context, q *store.Queries, kind string, personID *uuid.UUID, session *webauthn.SessionData) (uuid.UUID, error) {
	data, err := json.Marshal(session)
	if err != nil {
		return uuid.Nil, err
	}
	// Opportunistic cleanup keeps the table small without a background job.
	if _, err := q.DeleteExpiredWebAuthnCeremonies(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("identity: delete expired ceremonies: %w", err)
	}
	c, err := q.CreateWebAuthnCeremony(ctx, store.CreateWebAuthnCeremonyParams{
		Kind: kind, PersonID: personID, SessionData: data, TtlSeconds: CeremonyTTL.Seconds(),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("identity: store ceremony: %w", err)
	}
	return c.ID, nil
}

func takeCeremony(ctx context.Context, q *store.Queries, id uuid.UUID, kind string) (webauthn.SessionData, error) {
	c, err := q.TakeWebAuthnCeremony(ctx, store.TakeWebAuthnCeremonyParams{ID: id, Kind: kind})
	if errors.Is(err, pgx.ErrNoRows) {
		return webauthn.SessionData{}, fmt.Errorf("%w: ceremony", ErrNotFound)
	} else if err != nil {
		return webauthn.SessionData{}, fmt.Errorf("identity: load ceremony: %w", err)
	}
	var session webauthn.SessionData
	if err := json.Unmarshal(c.SessionData, &session); err != nil {
		return webauthn.SessionData{}, fmt.Errorf("identity: decode ceremony: %w", err)
	}
	return session, nil
}

// Passkeys returns the passkeys of a person, oldest first.
func (s *Service) Passkeys(ctx context.Context, personID uuid.UUID) ([]store.Passkey, error) {
	return s.queries().ListPersonPasskeys(ctx, personID)
}

// RemovePasskey deletes a passkey of personID. Passkeys of other people are
// reported as not found.
func (s *Service) RemovePasskey(ctx context.Context, actor audit.Actor, personID, passkeyID uuid.UUID) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		pk, err := q.DeletePasskey(ctx, store.DeletePasskeyParams{ID: passkeyID, PersonID: personID})
		if err != nil {
			return notFound(err, "passkey")
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PasskeyRemoved,
			Target:   passkeyTarget(pk.ID),
			Metadata: map[string]any{"person_id": personID, "name": pk.Name},
		})
		return err
	})
}

// PasskeyOffer reports whether to offer a passkey after sign-in: the
// person has none and has not dismissed the offer within
// PasskeyOfferInterval.
func (s *Service) PasskeyOffer(ctx context.Context, personID uuid.UUID) (bool, error) {
	q := s.queries()
	n, err := q.CountPersonPasskeys(ctx, personID)
	if err != nil {
		return false, fmt.Errorf("identity: count passkeys: %w", err)
	}
	if n > 0 {
		return false, nil
	}
	creds, err := q.GetPersonCredentials(ctx, personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("identity: load credentials: %w", err)
	}
	return creds.PasskeyOfferDismissedAt == nil || time.Since(*creds.PasskeyOfferDismissedAt) > PasskeyOfferInterval, nil
}

// DismissPasskeyOffer remembers that the person skipped the passkey offer.
func (s *Service) DismissPasskeyOffer(ctx context.Context, actor audit.Actor, personID uuid.UUID) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		if err := q.DismissPasskeyOffer(ctx, personID); err != nil {
			return fmt.Errorf("identity: dismiss passkey offer: %w", err)
		}
		_, err := audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.PasskeyOfferDismissed, Target: personTarget(personID)})
		return err
	})
}

func passkeyTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetPasskey, ID: id.String()}
}

func uuidOrEmpty(b []byte) string {
	id, err := uuid.FromBytes(b)
	if err != nil {
		return ""
	}
	return id.String()
}
