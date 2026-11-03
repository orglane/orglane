package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// TeamRole is a person's role in a team.
type TeamRole string

// Team roles. A team may have any number of leads, including none.
const (
	TeamRoleMember TeamRole = "member"
	TeamRoleLead   TeamRole = "lead"
)

// Team errors.
var (
	ErrSlugTaken = errors.New("identity: team slug is taken")
	// ErrTeamArchived: archived teams are read-only.
	ErrTeamArchived = errors.New("identity: team is archived")
	// ErrTeamHasChildren: a team with active child teams cannot be archived.
	ErrTeamHasChildren = errors.New("identity: team has active child teams")
	// ErrPersonOffboarded: offboarded people cannot join teams.
	ErrPersonOffboarded = errors.New("identity: person is offboarded")
)

// Limits matching the database constraints (core/migrations/00006_teams.sql).
const (
	maxTeamNameLength        = 100
	maxTeamDescriptionLength = 1000
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// teamHierarchyLockID is the transaction-scoped advisory lock taken first by
// every change that can affect the team hierarchy or set of teams (creating,
// setting a parent, archiving). Besides preventing cycles, it freezes the set
// of teams while an import takes its ordered team locks.
const teamHierarchyLockID int64 = 0x6f72676c7465616d // "orglteam"

// NewTeam describes a team to create.
type NewTeam struct {
	// Slug is normalized to lower case: 1-63 characters of a-z, 0-9 and
	// inner hyphens. It is unique across all teams, archived ones included.
	Slug string
	Name string
	// Description is optional.
	Description string
	// ParentID is the parent team, if any. It must be an active team.
	ParentID *uuid.UUID
}

// TeamUpdate changes base fields of a team. Nil fields stay unchanged.
type TeamUpdate struct {
	Slug *string
	Name *string
	// Description: an empty string clears it.
	Description *string
	// ParentID: uuid.Nil removes the parent.
	ParentID *uuid.UUID
}

// CreateTeam creates a team.
func (s *Service) CreateTeam(ctx context.Context, actor audit.Actor, in NewTeam) (store.Team, error) {
	params := store.CreateTeamParams{ParentID: in.ParentID}
	var err error
	if params.Slug, err = normalizeSlug(in.Slug); err != nil {
		return store.Team{}, err
	}
	if params.Name, err = normalizeTeamName(in.Name); err != nil {
		return store.Team{}, err
	}
	if params.Description, err = normalizeTeamDescription(in.Description); err != nil {
		return store.Team{}, err
	}
	if params.ParentID != nil && *params.ParentID == uuid.Nil {
		params.ParentID = nil
	}
	var team store.Team
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := q.AcquireXactLock(ctx, teamHierarchyLockID); err != nil {
			return fmt.Errorf("identity: lock team hierarchy: %w", err)
		}
		if params.ParentID != nil {
			if err := checkParent(ctx, q, uuid.Nil, *params.ParentID); err != nil {
				return err
			}
		}
		var err error
		if team, err = q.CreateTeam(ctx, params); err != nil {
			return teamUniqueViolation(err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.TeamCreated,
			Target:   teamTarget(team.ID),
			Metadata: map[string]any{"slug": team.Slug, "name": team.Name, "parent_id": team.ParentID},
		})
		return err
	})
	if err != nil {
		return store.Team{}, err
	}
	return team, nil
}

// UpdateTeam changes the base fields of a team. Archived teams cannot be
// changed. The audit entry lists the changed fields, not their values.
func (s *Service) UpdateTeam(ctx context.Context, actor audit.Actor, id uuid.UUID, in TeamUpdate) (store.Team, error) {
	var team store.Team
	err := s.inTx(ctx, func(q *store.Queries) error {
		if in.ParentID != nil {
			if err := q.AcquireXactLock(ctx, teamHierarchyLockID); err != nil {
				return fmt.Errorf("identity: lock team hierarchy: %w", err)
			}
		}
		current, err := q.GetTeamForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "team")
		}
		if current.ArchivedAt != nil {
			return ErrTeamArchived
		}
		params := store.UpdateTeamParams{
			ID:          id,
			Slug:        current.Slug,
			Name:        current.Name,
			Description: current.Description,
			ParentID:    current.ParentID,
		}
		var changed []string
		if in.Slug != nil {
			if params.Slug, err = normalizeSlug(*in.Slug); err != nil {
				return err
			}
			if params.Slug != current.Slug {
				changed = append(changed, "slug")
			}
		}
		if in.Name != nil {
			if params.Name, err = normalizeTeamName(*in.Name); err != nil {
				return err
			}
			if params.Name != current.Name {
				changed = append(changed, "name")
			}
		}
		if in.Description != nil {
			if params.Description, err = normalizeTeamDescription(*in.Description); err != nil {
				return err
			}
			if !equalPtr(params.Description, current.Description) {
				changed = append(changed, "description")
			}
		}
		if in.ParentID != nil {
			params.ParentID = nil
			if *in.ParentID != uuid.Nil {
				parent := *in.ParentID
				params.ParentID = &parent
			}
			if !equalUUIDPtr(params.ParentID, current.ParentID) {
				if params.ParentID != nil {
					if err := checkParent(ctx, q, id, *params.ParentID); err != nil {
						return err
					}
				}
				changed = append(changed, "parent_id")
			}
		}
		if len(changed) == 0 {
			team = current
			return nil
		}
		if team, err = q.UpdateTeam(ctx, params); err != nil {
			return teamUniqueViolation(err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.TeamUpdated,
			Target:   teamTarget(id),
			Metadata: map[string]any{"fields": changed},
		})
		return err
	})
	if err != nil {
		return store.Team{}, err
	}
	return team, nil
}

// ArchiveTeam archives a team. The team keeps its slug, memberships and
// history and becomes read-only; it no longer appears in team lists or a
// person's teams. Archiving an archived team changes nothing. A team with
// active child teams cannot be archived.
func (s *Service) ArchiveTeam(ctx context.Context, actor audit.Actor, id uuid.UUID) (store.Team, error) {
	var team store.Team
	err := s.inTx(ctx, func(q *store.Queries) error {
		if err := q.AcquireXactLock(ctx, teamHierarchyLockID); err != nil {
			return fmt.Errorf("identity: lock team hierarchy: %w", err)
		}
		current, err := q.GetTeamForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "team")
		}
		if current.ArchivedAt != nil {
			team = current
			return nil
		}
		children, err := q.CountActiveChildTeams(ctx, &id)
		if err != nil {
			return fmt.Errorf("identity: count child teams: %w", err)
		}
		if children > 0 {
			return ErrTeamHasChildren
		}
		if team, err = q.ArchiveTeam(ctx, id); err != nil {
			return fmt.Errorf("identity: archive team: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.TeamArchived,
			Target:   teamTarget(id),
			Metadata: map[string]any{"slug": team.Slug},
		})
		return err
	})
	if err != nil {
		return store.Team{}, err
	}
	return team, nil
}

// SetTeamMember adds a person to a team with a role, or changes the role of
// a member (for example to make them a lead). Setting the role a member
// already has changes nothing. Offboarded people cannot join, and archived
// teams cannot change.
//
// The team row is locked before the person's, so membership changes of one
// team are serialized and cannot race with archiving it.
func (s *Service) SetTeamMember(ctx context.Context, actor audit.Actor, teamID, personID uuid.UUID, role TeamRole) (store.TeamMembership, error) {
	if role != TeamRoleMember && role != TeamRoleLead {
		return store.TeamMembership{}, invalid("role", "must be %s or %s, not %q", TeamRoleMember, TeamRoleLead, role)
	}
	var membership store.TeamMembership
	err := s.inTx(ctx, func(q *store.Queries) error {
		team, err := q.GetTeamForUpdate(ctx, teamID)
		if err != nil {
			return notFound(err, "team")
		}
		if team.ArchivedAt != nil {
			return ErrTeamArchived
		}
		person, err := q.GetPersonForUpdate(ctx, personID)
		if err != nil {
			return notFound(err, "person")
		}
		current, err := q.GetTeamMembershipForUpdate(ctx, store.GetTeamMembershipForUpdateParams{TeamID: teamID, PersonID: personID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if Status(person.Status) == StatusOffboarded {
				return ErrPersonOffboarded
			}
			if membership, err = q.InsertTeamMembership(ctx, store.InsertTeamMembershipParams{TeamID: teamID, PersonID: personID, Role: string(role)}); err != nil {
				return fmt.Errorf("identity: add team member: %w", err)
			}
			_, err = audit.Record(ctx, q, audit.Entry{
				Actor:    actor,
				Action:   audit.TeamMemberAdded,
				Target:   teamTarget(teamID),
				Metadata: map[string]any{"person_id": personID, "role": role},
			})
			return err
		case err != nil:
			return fmt.Errorf("identity: load team membership: %w", err)
		case current.Role == string(role):
			membership = current
			return nil
		}
		if Status(person.Status) == StatusOffboarded {
			return ErrPersonOffboarded
		}
		if membership, err = q.SetTeamMembershipRole(ctx, store.SetTeamMembershipRoleParams{TeamID: teamID, PersonID: personID, Role: string(role)}); err != nil {
			return fmt.Errorf("identity: set team role: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.TeamMemberRoleChanged,
			Target:   teamTarget(teamID),
			Metadata: map[string]any{"person_id": personID, "from": current.Role, "to": role},
		})
		return err
	})
	if err != nil {
		return store.TeamMembership{}, err
	}
	return membership, nil
}

// RemoveTeamMember removes a person from a team. Removing someone who is not
// a member changes nothing. Archived teams cannot change.
func (s *Service) RemoveTeamMember(ctx context.Context, actor audit.Actor, teamID, personID uuid.UUID) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		team, err := q.GetTeamForUpdate(ctx, teamID)
		if err != nil {
			return notFound(err, "team")
		}
		if team.ArchivedAt != nil {
			return ErrTeamArchived
		}
		current, err := q.GetTeamMembershipForUpdate(ctx, store.GetTeamMembershipForUpdateParams{TeamID: teamID, PersonID: personID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not a member
		} else if err != nil {
			return fmt.Errorf("identity: load team membership: %w", err)
		}
		if _, err := q.DeleteTeamMembership(ctx, store.DeleteTeamMembershipParams{TeamID: teamID, PersonID: personID}); err != nil {
			return fmt.Errorf("identity: remove team member: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.TeamMemberRemoved,
			Target:   teamTarget(teamID),
			Metadata: map[string]any{"person_id": personID, "role": current.Role},
		})
		return err
	})
}

// Teams returns the teams by name with their member counts. Archived teams
// are included only when includeArchived is set.
func (s *Service) Teams(ctx context.Context, includeArchived bool) ([]store.ListTeamsRow, error) {
	rows, err := s.queries().ListTeams(ctx, includeArchived)
	if err != nil {
		return nil, fmt.Errorf("identity: list teams: %w", err)
	}
	return rows, nil
}

// GetTeam returns a team, archived or not, with its member count.
func (s *Service) GetTeam(ctx context.Context, id uuid.UUID) (store.GetTeamWithMemberCountRow, error) {
	row, err := s.queries().GetTeamWithMemberCount(ctx, id)
	if err != nil {
		return store.GetTeamWithMemberCountRow{}, notFound(err, "team")
	}
	return row, nil
}

// GetTeamBySlug returns a team by slug (normalized to lower case).
func (s *Service) GetTeamBySlug(ctx context.Context, slug string) (store.Team, error) {
	t, err := s.queries().GetTeamBySlug(ctx, strings.ToLower(strings.TrimSpace(slug)))
	if err != nil {
		return store.Team{}, notFound(err, "team")
	}
	return t, nil
}

// TeamMembers returns the members of a team who are not offboarded, leads
// first. Archived teams keep their members.
func (s *Service) TeamMembers(ctx context.Context, teamID uuid.UUID) ([]store.ListTeamMembersRow, error) {
	if _, err := s.queries().GetTeamWithMemberCount(ctx, teamID); err != nil {
		return nil, notFound(err, "team")
	}
	rows, err := s.queries().ListTeamMembers(ctx, teamID)
	if err != nil {
		return nil, fmt.Errorf("identity: list team members: %w", err)
	}
	return rows, nil
}

// PersonTeams returns the active (not archived) teams of a person with
// their role in each.
func (s *Service) PersonTeams(ctx context.Context, personID uuid.UUID) ([]store.ListPersonTeamsRow, error) {
	rows, err := s.queries().ListPersonTeams(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("identity: list person teams: %w", err)
	}
	return rows, nil
}

// SharesTeam reports whether two non-offboarded people share an active team.
// It is used by profile visibility rules; archived memberships grant no access.
// Missing people return false.
func (s *Service) SharesTeam(ctx context.Context, a, b uuid.UUID) (bool, error) {
	shared, err := s.queries().SharesTeam(ctx, store.SharesTeamParams{PersonA: a, PersonB: b})
	if err != nil {
		return false, fmt.Errorf("identity: shared team: %w", err)
	}
	return shared, nil
}

// checkParent checks that parentID can be the parent of team id (uuid.Nil
// for a new team): it exists, is active and is not the team itself or below
// it. The caller holds the hierarchy lock.
func checkParent(ctx context.Context, q *store.Queries, id, parentID uuid.UUID) error {
	parent, err := q.GetTeamForUpdate(ctx, parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid("parent_id", "no such team")
	} else if err != nil {
		return fmt.Errorf("identity: load parent team: %w", err)
	}
	if parent.ArchivedAt != nil {
		return invalid("parent_id", "the parent team is archived")
	}
	if id == uuid.Nil {
		return nil
	}
	chain, err := q.ListTeamAncestors(ctx, parentID)
	if err != nil {
		return fmt.Errorf("identity: load team ancestors: %w", err)
	}
	if slices.Contains(chain, id) {
		return invalid("parent_id", "a team cannot be placed below itself")
	}
	return nil
}

func teamTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetTeam, ID: id.String()}
}

// teamUniqueViolation maps the slug's unique violation to ErrSlugTaken.
func teamUniqueViolation(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "teams_slug_key" {
		return ErrSlugTaken
	}
	return err
}

func normalizeSlug(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !slugPattern.MatchString(s) {
		return "", invalid("slug", "must be 1-63 characters of a-z, 0-9 and '-', starting and ending with a letter or digit")
	}
	return s, nil
}

func normalizeTeamName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || !utf8.ValidString(n) || utf8.RuneCountInString(n) > maxTeamNameLength || strings.ContainsRune(n, 0) {
		return "", invalid("name", "must be 1-%d characters", maxTeamNameLength)
	}
	return n, nil
}

// normalizeTeamDescription trims a description; an empty one is stored as
// NULL.
func normalizeTeamDescription(d string) (*string, error) {
	d = strings.TrimSpace(d)
	if d == "" {
		return nil, nil
	}
	if !utf8.ValidString(d) || utf8.RuneCountInString(d) > maxTeamDescriptionLength || strings.ContainsRune(d, 0) {
		return nil, invalid("description", "must be at most %d characters", maxTeamDescriptionLength)
	}
	return &d, nil
}

func equalUUIDPtr(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
