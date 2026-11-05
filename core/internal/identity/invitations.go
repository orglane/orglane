package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/mail"
	"github.com/orglane/orglane/core/internal/store"
)

// Invitations.
//
// Inviting a person adds them to the directory in the invited state, with
// their role, and issues a single-use, expiring token. The token is only in
// the invitation link (the database keeps its SHA-256 hash): it is emailed
// to the person's contact email when the channel can safely carry its grant, or handed
// to the inviting admin as a link to share otherwise. Accepting the
// invitation sets the person's password and makes them active. Resending
// issues a new token (the old link stops working); revoking invalidates the
// current one. Every step is audited, including expiry, which identity
// maintenance records once.

// InvitationTokenPrefix starts every invitation token.
const InvitationTokenPrefix = "oli_"

// InvitationPath is the web UI page that accepts invitations. The token is
// in the fragment, so it reaches neither server logs nor Referer headers.
const InvitationPath = "/invite"

// Mailer queues system email in a transaction (package mail).
type Mailer interface {
	// SettingsSource reports who controls the effective email channel.
	SettingsSource(ctx context.Context) (mail.Source, error)
	// EnqueueTx queues a message in tx and returns its outbox ID.
	EnqueueTx(ctx context.Context, tx pgx.Tx, kind, to string, m mail.Message) (uuid.UUID, error)
}

// InvitationConfig configures invitation links.
type InvitationConfig struct {
	// WorkspaceURL is the base of invitation links, e.g.
	// https://work.acme.com.
	WorkspaceURL string
	// Lifetime is how long a link stays valid.
	Lifetime time.Duration
}

// DefaultInvitationConfig is used without WithInvitations (tests). The
// workspace URL is supplied by the instance configuration; without it,
// invitation links are relative.
var DefaultInvitationConfig = InvitationConfig{Lifetime: 7 * 24 * time.Hour}

// Delivery is how an invitation reaches the person.
type Delivery string

// Deliveries.
const (
	// DeliveryEmail: sent to the contact email.
	DeliveryEmail Delivery = "email"
	// DeliveryLink: no email is configured; the inviting admin shares the
	// link.
	DeliveryLink Delivery = "link"
)

// NewInvitation describes a person to invite.
type NewInvitation struct {
	DisplayName string
	// ContactEmail is where the invitation goes. Required.
	ContactEmail string
	Username     string
	// WorkEmail is the person's identity in the workspace, by default built
	// from the username (domain.work_email_template).
	WorkEmail string
	Title     string
	// Role is the role the person gets.
	Role authz.RoleKey
	// TeamIDs are the initial active teams; memberships are created as
	// members in the same transaction as the person and invitation.
	TeamIDs []uuid.UUID
	// Locale is the language of the invitation email (a BCP 47 tag or an
	// Accept-Language header).
	Locale string
}

// IssuedInvitation is an invitation with its delivery.
type IssuedInvitation struct {
	Invitation store.Invitation
	Person     store.Person
	Delivery   Delivery
	// Link is the invitation link, returned only for DeliveryLink: with
	// email it reaches only the invited person.
	Link string
}

// InvitationPreview is what the accept page shows before the person sets a
// password.
type InvitationPreview struct {
	Person    store.Person
	ExpiresAt time.Time
}

// Invite adds a person in the invited state with a role and issues their
// invitation, by email when trusted for the grant, otherwise as a link.
func (s *Service) Invite(ctx context.Context, actor audit.Actor, in NewInvitation) (IssuedInvitation, error) {
	params := store.CreatePersonParams{Status: string(StatusInvited)}
	var err error
	if params.DisplayName, err = normalizeDisplayName(in.DisplayName); err != nil {
		return IssuedInvitation{}, err
	}
	contact, err := normalizeAddress("contact_email", in.ContactEmail)
	if err != nil {
		return IssuedInvitation{}, err
	}
	params.ContactEmail = &contact
	if params.Username, err = normalizeUsername(in.Username); err != nil {
		return IssuedInvitation{}, err
	}
	if params.WorkEmail, err = normalizeEmail(in.WorkEmail); err != nil {
		return IssuedInvitation{}, err
	}
	if params.Title, err = normalizeTitle(in.Title); err != nil {
		return IssuedInvitation{}, err
	}
	if in.Role == "" {
		return IssuedInvitation{}, invalid("role", "required")
	}
	if len(in.TeamIDs) > 100 {
		return IssuedInvitation{}, invalid("teamIds", "at most 100 teams are allowed")
	}
	teamIDs := slices.Clone(in.TeamIDs)
	// Team locks precede person locks. Sort multiple teams consistently to
	// avoid deadlocks between invitations with overlapping team selections.
	slices.SortFunc(teamIDs, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for i, id := range teamIDs {
		if id == uuid.Nil || (i > 0 && id == teamIDs[i-1]) {
			return IssuedInvitation{}, invalid("teamIds", "must contain distinct, nonzero team IDs")
		}
	}
	locale := mail.Negotiate(in.Locale)
	source, err := s.invitationMailSource(ctx)
	if err != nil {
		return IssuedInvitation{}, err
	}

	var out IssuedInvitation
	err = s.inTxWith(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if err := LockGrantsTx(ctx, q); err != nil {
			return err
		}
		for _, id := range teamIDs {
			team, err := q.GetTeamForUpdate(ctx, id)
			if err != nil {
				return notFound(err, "team")
			}
			if team.ArchivedAt != nil {
				return ErrTeamArchived
			}
		}
		role, err := q.GetRoleByKey(ctx, string(in.Role))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return invalid("role", "unknown role %q", in.Role)
			}
			return notFound(err, "role")
		}
		permissions, err := q.ListRolePermissions(ctx, role.ID)
		if err != nil {
			return fmt.Errorf("identity: invitation role permissions: %w", err)
		}
		if permissions == nil {
			permissions = []string{}
		}
		if err := AuthorizeGrantTx(ctx, q, actor, uuid.Nil, permissions); err != nil {
			return err
		}
		channel := mail.InvitationChannelFor(source, permissions)
		delivery := invitationDelivery(channel)
		person, err := q.CreatePerson(ctx, params)
		if err != nil {
			return uniqueViolation(err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: audit.PersonCreated, Target: personTarget(person.ID),
			Metadata: map[string]any{"status": person.Status},
		}); err != nil {
			return err
		}
		if _, err := q.AssignPersonRole(ctx, store.AssignPersonRoleParams{PersonID: person.ID, RoleID: role.ID}); err != nil {
			return fmt.Errorf("identity: assign role: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: audit.PersonRoleAssigned, Target: personTarget(person.ID),
			Metadata: map[string]any{"role": role.Key, "role_id": role.ID},
		}); err != nil {
			return err
		}
		for _, id := range teamIDs {
			if _, err := q.InsertTeamMembership(ctx, store.InsertTeamMembershipParams{TeamID: id, PersonID: person.ID, Role: string(TeamRoleMember)}); err != nil {
				return fmt.Errorf("identity: add initial team membership: %w", err)
			}
			if _, err := audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.TeamMemberAdded, Target: teamTarget(id), Metadata: map[string]any{"person_id": person.ID, "role": TeamRoleMember}}); err != nil {
				return err
			}
		}
		token := crypto.RandomToken(InvitationTokenPrefix)
		emailID, err := s.sendInvitation(ctx, tx, q, actor, delivery, person, locale, token)
		if err != nil {
			return err
		}
		inv, err := q.CreateInvitation(ctx, store.CreateInvitationParams{
			PersonID: person.ID, TokenHash: crypto.HashToken(token), Locale: locale,
			InvitedBy: actorPerson(actor), EmailID: emailID, Channel: string(channel), GrantPermissions: permissions, TtlSeconds: s.invites.Lifetime.Seconds(),
		})
		if err != nil {
			return fmt.Errorf("identity: create invitation: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: audit.InvitationCreated, Target: invitationTarget(inv.ID),
			Metadata: map[string]any{"person_id": person.ID, "role": role.Key, "delivery": delivery, "expires_at": inv.ExpiresAt, "email_id": emailID},
		}); err != nil {
			return err
		}
		out = IssuedInvitation{Invitation: inv, Person: person, Delivery: delivery}
		if delivery == DeliveryLink {
			out.Link = s.invitationLink(token)
		}
		return nil
	})
	if err != nil {
		return IssuedInvitation{}, err
	}
	return out, nil
}

// ResendInvitation issues a new token for an open invitation (a revoked one
// becomes valid again) and sends it like Invite. The previous link stops
// working and a previous email still in the queue is canceled.
func (s *Service) ResendInvitation(ctx context.Context, actor audit.Actor, id uuid.UUID) (IssuedInvitation, error) {
	source, err := s.invitationMailSource(ctx)
	if err != nil {
		return IssuedInvitation{}, err
	}
	var out IssuedInvitation
	err = s.inTxWith(ctx, func(tx pgx.Tx, q *store.Queries) error {
		person, inv, err := lockInvitation(ctx, q, id)
		if err != nil {
			return err
		}
		if err := AuthorizeGrantTx(ctx, q, actor, person.ID, nil); err != nil {
			return err
		}
		if inv.AcceptedAt != nil || Status(person.Status) != StatusInvited {
			return ErrInvitationUsed
		}
		permissions, err := invitationPermissions(ctx, q, person.ID)
		if err != nil {
			return err
		}
		channel := mail.InvitationChannelFor(source, permissions)
		// Imported people may not have a reachable contact destination yet.
		// Their capability can still be shared through the normal link UI.
		if person.ContactEmail == nil {
			channel = mail.ChannelLink
		}
		delivery := invitationDelivery(channel)
		// Reissuing replaces the expiry timestamp. Preserve the old token's
		// expiry event even when hourly maintenance has not run yet.
		if inv.RevokedAt == nil && inv.ExpiryRecordedAt == nil && !time.Now().Before(inv.ExpiresAt) {
			if err := recordInvitationExpiry(ctx, q, inv); err != nil {
				return err
			}
		}
		if inv.EmailID != nil {
			if err := mail.CancelTx(ctx, q, *inv.EmailID); err != nil {
				return err
			}
		}
		token := crypto.RandomToken(InvitationTokenPrefix)
		emailID, err := s.sendInvitation(ctx, tx, q, actor, delivery, person, inv.Locale, token)
		if err != nil {
			return err
		}
		if inv, err = q.ReissueInvitation(ctx, store.ReissueInvitationParams{
			ID: id, TokenHash: crypto.HashToken(token), Locale: inv.Locale, EmailID: emailID, Channel: string(channel), GrantPermissions: permissions, TtlSeconds: s.invites.Lifetime.Seconds(),
		}); err != nil {
			return fmt.Errorf("identity: reissue invitation: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: audit.InvitationResent, Target: invitationTarget(inv.ID),
			Metadata: map[string]any{"person_id": person.ID, "delivery": delivery, "expires_at": inv.ExpiresAt, "email_id": emailID},
		}); err != nil {
			return err
		}
		out = IssuedInvitation{Invitation: inv, Person: person, Delivery: delivery}
		if delivery == DeliveryLink {
			out.Link = s.invitationLink(token)
		}
		return nil
	})
	if err != nil {
		return IssuedInvitation{}, err
	}
	return out, nil
}

// RevokeInvitation invalidates an open invitation's link and cancels its
// email if it is still queued. The person stays invited; resending issues a
// new link. Revoking a revoked invitation is a no-op.
func (s *Service) RevokeInvitation(ctx context.Context, actor audit.Actor, id uuid.UUID) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		person, inv, err := lockInvitation(ctx, q, id)
		if err != nil {
			return err
		}
		if err := AuthorizeGrantTx(ctx, q, actor, person.ID, nil); err != nil {
			return err
		}
		if inv.AcceptedAt != nil || Status(person.Status) != StatusInvited {
			return ErrInvitationUsed
		}
		if inv.RevokedAt != nil {
			return nil
		}
		if inv.ExpiryRecordedAt == nil && !time.Now().Before(inv.ExpiresAt) {
			if err := recordInvitationExpiry(ctx, q, inv); err != nil {
				return err
			}
		}
		if inv.EmailID != nil {
			if err := mail.CancelTx(ctx, q, *inv.EmailID); err != nil {
				return err
			}
		}
		if _, err := q.RevokeInvitation(ctx, id); err != nil {
			return fmt.Errorf("identity: revoke invitation: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: audit.InvitationRevoked, Target: invitationTarget(id),
			Metadata: map[string]any{"person_id": person.ID},
		})
		return err
	})
}

// PreviewInvitation returns the invited person for a valid token. It fails
// with ErrInvitationInvalid, ErrInvitationExpired or ErrInvitationUsed.
func (s *Service) PreviewInvitation(ctx context.Context, token string) (InvitationPreview, error) {
	var preview InvitationPreview
	// The invitation and its person are read from one snapshot. With a
	// snapshot per statement, an acceptance committing between the two
	// reads shows an open invitation of an already active person, which
	// reads as invalid instead of already accepted.
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := store.New(tx)
		inv, err := invitationByToken(ctx, q, token)
		if err != nil {
			return err
		}
		person, err := q.GetPerson(ctx, inv.PersonID)
		if err != nil {
			return notFound(err, "person")
		}
		if err := checkInvitation(inv, person); err != nil {
			return err
		}
		if err := checkInvitationGrant(ctx, q, inv); err != nil {
			return err
		}
		preview = InvitationPreview{Person: person, ExpiresAt: inv.ExpiresAt}
		return nil
	})
	if err != nil {
		return InvitationPreview{}, err
	}
	return preview, nil
}

// AcceptInvitation accepts a valid invitation: it sets the person's password
// (checked against the policy) and makes them active. The token cannot be
// used again. The caller signs the person in (StartSession).
func (s *Service) AcceptInvitation(ctx context.Context, actor audit.Actor, token, password string) (store.Person, error) {
	if !strings.HasPrefix(token, InvitationTokenPrefix) {
		return store.Person{}, ErrInvitationInvalid
	}
	if err := CheckPasswordPolicy(password); err != nil {
		return store.Person{}, err
	}
	// Reject unusable tokens before paying for the password hash.
	if _, err := s.PreviewInvitation(ctx, token); err != nil {
		return store.Person{}, err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return store.Person{}, err
	}
	var person store.Person
	err = s.inTx(ctx, func(q *store.Queries) error {
		found, err := invitationByToken(ctx, q, token)
		if err != nil {
			return err
		}
		p, inv, err := lockInvitation(ctx, q, found.ID)
		if err != nil {
			return err
		}
		if !crypto.Equal(inv.TokenHash, crypto.HashToken(token)) {
			return ErrInvitationInvalid // reissued meanwhile
		}
		if err := checkInvitation(inv, p); err != nil {
			return err
		}
		if err := checkInvitationGrant(ctx, q, inv); err != nil {
			return err
		}
		// The person acts for themselves from here on.
		self := actor
		self.Kind, self.ID = audit.ActorPerson, p.ID.String()
		if _, err := q.AcceptInvitation(ctx, inv.ID); err != nil {
			return fmt.Errorf("identity: accept invitation: %w", err)
		}
		if person, err = q.SetPersonStatus(ctx, store.SetPersonStatusParams{ID: p.ID, Status: string(StatusActive)}); err != nil {
			return fmt.Errorf("identity: set status: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: self, Action: audit.PersonStatusChanged, Target: personTarget(p.ID),
			Metadata: map[string]any{"from": StatusInvited, "to": StatusActive},
		}); err != nil {
			return err
		}
		if err := s.directoryChanged(ctx, q, []uuid.UUID{p.ID}); err != nil {
			return err
		}
		if err := setPasswordHash(ctx, q, self, p.ID, hash); err != nil {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor: self, Action: audit.InvitationAccepted, Target: invitationTarget(inv.ID),
			Metadata: map[string]any{"person_id": p.ID},
		})
		return err
	})
	if err != nil {
		return store.Person{}, err
	}
	return person, nil
}

// Invitations lists the open invitations (not accepted) of people who are
// still invited, newest first, with the delivery state of their email.
func (s *Service) Invitations(ctx context.Context) ([]store.ListInvitationsRow, error) {
	rows, err := s.queries().ListInvitations(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: list invitations: %w", err)
	}
	return rows, nil
}

// InvitationDelivery reports how a new invitation would be delivered now.
func (s *Service) InvitationDelivery(ctx context.Context) (Delivery, error) {
	return s.delivery(ctx)
}

// InvitationLifetime is how long a new invitation link stays valid.
func (s *Service) InvitationLifetime() time.Duration { return s.invites.Lifetime }

// RecordExpiredInvitations audits invitations that expired unused, once
// each. Identity maintenance runs it hourly; it returns how many it
// recorded.
func (s *Service) RecordExpiredInvitations(ctx context.Context) (int, error) {
	var n int
	err := s.inTx(ctx, func(q *store.Queries) error {
		rows, err := q.ListUnrecordedExpiredInvitations(ctx)
		if err != nil {
			return fmt.Errorf("identity: list expired invitations: %w", err)
		}
		for _, inv := range rows {
			if err := recordInvitationExpiry(ctx, q, inv); err != nil {
				return err
			}
		}
		n = len(rows)
		return nil
	})
	return n, err
}

func recordInvitationExpiry(ctx context.Context, q *store.Queries, inv store.Invitation) error {
	if err := q.MarkInvitationExpiryRecorded(ctx, inv.ID); err != nil {
		return fmt.Errorf("identity: record expiry: %w", err)
	}
	_, err := audit.Record(ctx, q, audit.Entry{
		Actor: audit.System, Action: audit.InvitationExpired, Target: invitationTarget(inv.ID),
		Metadata: map[string]any{"person_id": inv.PersonID, "expires_at": inv.ExpiresAt},
	})
	return err
}

// RolePermissions returns the permissions of a role, for checking that
// whoever grants it holds them all.
func (s *Service) RolePermissions(ctx context.Context, key authz.RoleKey) ([]authz.Permission, error) {
	q := s.queries()
	role, err := q.GetRoleByKey(ctx, string(key))
	if err != nil {
		return nil, notFound(err, "role")
	}
	names, err := q.ListRolePermissions(ctx, role.ID)
	if err != nil {
		return nil, fmt.Errorf("identity: role permissions: %w", err)
	}
	perms := make([]authz.Permission, 0, len(names))
	for _, n := range names {
		perms = append(perms, authz.Permission(n))
	}
	return perms, nil
}

func (s *Service) invitationMailSource(ctx context.Context) (mail.Source, error) {
	if s.mailer == nil {
		return mail.SourceNone, nil
	}
	return s.mailer.SettingsSource(ctx)
}

func (s *Service) delivery(ctx context.Context) (Delivery, error) {
	source, err := s.invitationMailSource(ctx)
	if err != nil {
		return "", err
	}
	if source == mail.SourceNone {
		return DeliveryLink, nil
	}
	return DeliveryEmail, nil
}

// InvitationRoleDelivery describes the channel for a particular role grant.
func (s *Service) InvitationRoleDelivery(ctx context.Context, role authz.RoleKey) (Delivery, error) {
	permissions, err := s.RolePermissions(ctx, role)
	if err != nil {
		return "", err
	}
	source, err := s.invitationMailSource(ctx)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(permissions))
	for _, p := range permissions {
		names = append(names, string(p))
	}
	return invitationDelivery(mail.InvitationChannelFor(source, names)), nil
}

func invitationDelivery(channel mail.InvitationChannel) Delivery {
	if channel == mail.ChannelLink {
		return DeliveryLink
	}
	return DeliveryEmail
}

func invitationPermissions(ctx context.Context, q *store.Queries, personID uuid.UUID) ([]string, error) {
	permissions, err := q.ListPersonPermissions(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("identity: invitation permissions: %w", err)
	}
	if permissions == nil {
		permissions = []string{}
	}
	return permissions, nil
}

func checkInvitationGrant(ctx context.Context, q *store.Queries, inv store.Invitation) error {
	permissions, err := invitationPermissions(ctx, q, inv.PersonID)
	if err != nil {
		return err
	}
	if !mail.InvitationChannel(inv.Channel).AllowsGrant(inv.GrantPermissions, permissions) {
		return ErrInvitationInvalid
	}
	return nil
}

// sendInvitation queues the invitation email for DeliveryEmail and returns
// its outbox ID (nil for DeliveryLink).
func (s *Service) sendInvitation(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, delivery Delivery, person store.Person, locale, token string) (*uuid.UUID, error) {
	if delivery != DeliveryEmail {
		return nil, nil
	}
	if person.ContactEmail == nil {
		return nil, invalid("contact_email", "required to send an invitation")
	}
	workspace := s.workspaceHost()
	inviter := workspace
	if id := actorPerson(actor); id != nil {
		if p, err := q.GetPerson(ctx, *id); err == nil {
			inviter = p.DisplayName
		}
	}
	msg, err := mail.RenderInvitation(locale, mail.Invitation{
		Name: person.DisplayName, Inviter: inviter, Workspace: workspace, WorkEmail: person.WorkEmail,
		Link: s.invitationLink(token), Days: max(1, int(math.Ceil(s.invites.Lifetime.Hours()/24))),
	})
	if err != nil {
		return nil, err
	}
	id, err := s.mailer.EnqueueTx(ctx, tx, mail.KindInvitation, *person.ContactEmail, msg)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *Service) invitationLink(token string) string {
	return strings.TrimSuffix(s.invites.WorkspaceURL, "/") + InvitationPath + "#" + token
}

func (s *Service) workspaceHost() string {
	if u, err := url.Parse(s.invites.WorkspaceURL); err == nil && u.Host != "" {
		return u.Host
	}
	return s.invites.WorkspaceURL
}

// lockInvitation takes the grant lock before the invited person and invitation.
// The locked grant is revalidated before reissue, revoke or acceptance.
func lockInvitation(ctx context.Context, q *store.Queries, id uuid.UUID) (store.Person, store.Invitation, error) {
	if err := LockGrantsTx(ctx, q); err != nil {
		return store.Person{}, store.Invitation{}, err
	}
	inv, err := q.GetInvitation(ctx, id)
	if err != nil {
		return store.Person{}, store.Invitation{}, notFound(err, "invitation")
	}
	person, err := q.GetPersonForUpdate(ctx, inv.PersonID)
	if err != nil {
		return store.Person{}, store.Invitation{}, notFound(err, "person")
	}
	if inv, err = q.GetInvitationForUpdate(ctx, id); err != nil {
		return store.Person{}, store.Invitation{}, notFound(err, "invitation")
	}
	return person, inv, nil
}

func invitationByToken(ctx context.Context, q *store.Queries, token string) (store.Invitation, error) {
	if !strings.HasPrefix(token, InvitationTokenPrefix) {
		return store.Invitation{}, ErrInvitationInvalid
	}
	inv, err := q.GetInvitationByTokenHash(ctx, crypto.HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Invitation{}, ErrInvitationInvalid
	} else if err != nil {
		return store.Invitation{}, fmt.Errorf("identity: load invitation: %w", err)
	}
	return inv, nil
}

// checkInvitation reports whether an invitation can be accepted now.
func checkInvitation(inv store.Invitation, person store.Person) error {
	switch {
	case inv.AcceptedAt != nil:
		return ErrInvitationUsed
	case inv.RevokedAt != nil || Status(person.Status) != StatusInvited:
		return ErrInvitationInvalid
	case !time.Now().Before(inv.ExpiresAt):
		return ErrInvitationExpired
	}
	return nil
}

// actorPerson returns the person ID of a person actor.
func actorPerson(actor audit.Actor) *uuid.UUID {
	if actor.Kind != audit.ActorPerson {
		return nil
	}
	id, err := uuid.Parse(actor.ID)
	if err != nil {
		return nil
	}
	return &id
}

func invitationTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetInvitation, ID: id.String()}
}

// InvitationDetails returns one invitation as Invitations lists it.
func (s *Service) InvitationDetails(ctx context.Context, id uuid.UUID) (store.ListInvitationsRow, error) {
	r, err := s.queries().GetInvitationDetails(ctx, id)
	if err != nil {
		return store.ListInvitationsRow{}, notFound(err, "invitation")
	}
	return store.ListInvitationsRow(r), nil
}
