package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/mail"
	"github.com/orglane/orglane/core/internal/store"
)

// ImportedPerson is one mapped row. Empty mapped cells leave existing values.
// Role is an additive bundle; imports never silently revoke existing roles.
// Existing GitHub matches preserve all local person fields and grants.
type ImportedPerson struct {
	GitHub                                                *PendingGitHubAccount
	ID                                                    uuid.UUID
	Username, WorkEmail, ContactEmail, DisplayName, Title string
	Role                                                  authz.RoleKey
	ManagerID                                             *uuid.UUID
	Teams                                                 []string
	CreateTeams, SendInvitation                           bool
}

// LockImportTx establishes the shared order: grants, reporting, team hierarchy,
// all existing teams, then people/definitions/values/roles/invitations. Taking
// all teams first also preserves that order across successful row savepoints.
func LockImportTx(ctx context.Context, q *store.Queries) error {
	if err := LockGrantsTx(ctx, q); err != nil {
		return err
	}
	if err := AcquireReportingLock(ctx, q); err != nil {
		return err
	}
	if err := q.AcquireXactLock(ctx, teamHierarchyLockID); err != nil {
		return err
	}
	_, err := q.LockPeopleImportTeams(ctx)
	return err
}

// ImportPersonTx writes one person's base fields, additive role/team grant and
// optional invitation in the caller's transaction. All validation and auditing
// use the identity domain. Caller takes LockImportTx first and rolls back the
// entire row on any error; no generated invitation token is returned.
func (s *Service) ImportPersonTx(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, in ImportedPerson) (store.Person, error) {
	// Existing GitHub matches only add/update pending evidence. Keep this
	// invariant in the identity domain as well as the shared import mapper.
	if in.GitHub != nil && in.ID != uuid.Nil {
		in = ImportedPerson{ID: in.ID, GitHub: in.GitHub}
	}
	roleKey := in.Role
	if in.ID == uuid.Nil && roleKey == "" {
		roleKey = authz.RoleMember
	}
	var role store.Role
	if roleKey != "" {
		var err error
		role, err = q.GetRoleByKey(ctx, string(roleKey))
		if err != nil {
			return store.Person{}, notFound(err, "role")
		}
		permissions, err := q.ListRolePermissions(ctx, role.ID)
		if err != nil {
			return store.Person{}, err
		}
		if err := AuthorizeGrantTx(ctx, q, actor, in.ID, permissions); err != nil {
			return store.Person{}, err
		}
	} else if err := AuthorizeGrantTx(ctx, q, actor, in.ID, nil); err != nil {
		return store.Person{}, err
	}
	teamIDs := []uuid.UUID{}
	if len(in.Teams) > 100 {
		return store.Person{}, invalid("teams", "too many teams")
	}
	for _, name := range in.Teams {
		name, err := normalizeTeamName(name)
		if err != nil {
			return store.Person{}, err
		}
		found, err := q.FindPeopleImportTeams(ctx, name)
		if err != nil {
			return store.Person{}, err
		}
		var team store.Team
		switch len(found) {
		case 0:
			if !in.CreateTeams {
				return store.Person{}, ErrNotFound
			}
			sum := sha256.Sum256([]byte(strings.ToLower(name)))
			team, err = q.CreateTeam(ctx, store.CreateTeamParams{Slug: "import-" + hex.EncodeToString(sum[:12]), Name: name})
			if err != nil {
				return store.Person{}, teamUniqueViolation(err)
			}
			if _, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.TeamCreated, Target: teamTarget(team.ID), Metadata: map[string]any{"source": "import"}}); err != nil {
				return store.Person{}, err
			}
		case 1:
			team = found[0]
		default:
			return store.Person{}, invalid("teams", "ambiguous team")
		}
		if team.ArchivedAt != nil {
			return store.Person{}, ErrTeamArchived
		}
		if !slices.Contains(teamIDs, team.ID) {
			teamIDs = append(teamIDs, team.ID)
		}
	}
	var person store.Person
	var err error
	created := in.ID == uuid.Nil
	if created {
		person, err = s.CreatePersonTx(ctx, q, actor, NewPerson{Username: in.Username, WorkEmail: in.WorkEmail, DisplayName: in.DisplayName, Title: in.Title, ContactEmail: in.ContactEmail, Status: StatusInvited})
		if err != nil {
			return store.Person{}, err
		}
		in.ID = person.ID
	} else {
		person, err = q.GetPersonForUpdate(ctx, in.ID)
		if err != nil {
			return store.Person{}, notFound(err, "person")
		}
		if person.Status == string(StatusOffboarded) {
			return store.Person{}, ErrPersonOffboarded
		}
	}
	update := PersonUpdate{ManagerID: in.ManagerID}
	if in.Username != "" {
		update.Username = &in.Username
	}
	if in.WorkEmail != "" {
		update.WorkEmail = &in.WorkEmail
	}
	if in.DisplayName != "" {
		update.DisplayName = &in.DisplayName
	}
	if in.Title != "" {
		update.Title = &in.Title
	}
	person, err = s.UpdatePersonTx(ctx, q, actor, in.ID, update)
	if err != nil {
		return store.Person{}, err
	}
	if !created && in.ContactEmail != "" {
		contact, err := normalizeAddress("contact_email", in.ContactEmail)
		if err != nil {
			return store.Person{}, err
		}
		if person.ContactEmail == nil || !strings.EqualFold(contact, *person.ContactEmail) {
			// An existing invitation destination cannot be redirected by import.
			if person.Status == string(StatusInvited) {
				return store.Person{}, ErrForbidden
			}
			if err = q.UpdatePeopleImportContact(ctx, store.UpdatePeopleImportContactParams{ID: in.ID, ContactEmail: &contact}); err != nil {
				return store.Person{}, err
			}
			if _, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.PersonUpdated, Target: personTarget(in.ID), Metadata: map[string]any{"fields": []string{"contact_email"}}}); err != nil {
				return store.Person{}, err
			}
			person.ContactEmail = &contact
		}
	}
	if roleKey != "" {
		n, err := q.AssignPersonRole(ctx, store.AssignPersonRoleParams{PersonID: in.ID, RoleID: role.ID})
		if err != nil {
			return store.Person{}, err
		}
		if n > 0 {
			if _, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.PersonRoleAssigned, Target: personTarget(in.ID), Metadata: map[string]any{"role_id": role.ID, "role": role.Key}}); err != nil {
				return store.Person{}, err
			}
		}
	}
	for _, id := range teamIDs {
		_, err := q.GetTeamMembershipForUpdate(ctx, store.GetTeamMembershipForUpdateParams{TeamID: id, PersonID: in.ID})
		if err == nil {
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return store.Person{}, err
		}
		if _, err = q.InsertTeamMembership(ctx, store.InsertTeamMembershipParams{TeamID: id, PersonID: in.ID, Role: string(TeamRoleMember)}); err != nil {
			return store.Person{}, err
		}
		if _, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.TeamMemberAdded, Target: teamTarget(id), Metadata: map[string]any{"person_id": in.ID, "role": TeamRoleMember}}); err != nil {
			return store.Person{}, err
		}
	}
	if created {
		if err := s.issueImportedInvitation(ctx, tx, q, actor, person, in.SendInvitation); err != nil {
			return store.Person{}, err
		}
	}

	if in.GitHub != nil {
		g := in.GitHub
		n, e := q.PutPendingGitHubAccount(ctx, store.PutPendingGitHubAccountParams{OrganizationID: g.OrganizationID, ExternalID: g.UserID, PersonID: person.ID, Login: g.Login, AvatarUrl: g.AvatarURL})
		if e != nil {
			return store.Person{}, e
		}
		if n != 1 {
			return store.Person{}, ErrInvalid
		}
		if _, e = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.PersonUpdated, Target: personTarget(person.ID), Metadata: map[string]any{"fields": []string{"pending_github_account"}}}); e != nil {
			return store.Person{}, e
		}
	}
	return person, nil
}

// ErrImportDelivery means email cannot safely carry this grant. The normal
// invitation UI remains the only place to retrieve manual invitation links.
var ErrImportDelivery = errors.New("identity: import email delivery unavailable for grant")

func (s *Service) issueImportedInvitation(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, person store.Person, send bool) error {
	permissions, err := invitationPermissions(ctx, q, person.ID)
	if err != nil {
		return err
	}
	source := mail.SourceNone
	if send {
		source, err = s.invitationMailSource(ctx)
		if err != nil {
			return err
		}
	}
	channel := mail.InvitationChannelFor(source, permissions)
	delivery := invitationDelivery(channel)
	if send && delivery != DeliveryEmail {
		return ErrImportDelivery
	}
	token := crypto.RandomToken(InvitationTokenPrefix)
	locale := mail.Negotiate("")
	emailID, err := s.sendInvitation(ctx, tx, q, actor, delivery, person, locale, token)
	if err != nil {
		return err
	}
	inv, err := q.CreateInvitation(ctx, store.CreateInvitationParams{PersonID: person.ID, TokenHash: crypto.HashToken(token), Locale: locale, InvitedBy: actorPerson(actor), EmailID: emailID, Channel: string(channel), GrantPermissions: permissions, TtlSeconds: s.invites.Lifetime.Seconds()})
	if err != nil {
		return fmt.Errorf("identity: imported invitation: %w", err)
	}
	_, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.InvitationCreated, Target: invitationTarget(inv.ID), Metadata: map[string]any{"person_id": person.ID, "delivery": delivery, "source": "import"}})
	return err
}

// PendingGitHubAccount records imported evidence, never verified ownership.
type PendingGitHubAccount struct {
	OrganizationID, UserID int64
	Login, AvatarURL       string
}
