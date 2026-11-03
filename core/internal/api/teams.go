package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/core/internal/store"
)

func (s *server) ListTeams(ctx context.Context, req apigen.ListTeamsRequestObject) (apigen.ListTeamsResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	rows, err := a.Identity().Teams(ctx, req.Params.Archived != nil && *req.Params.Archived)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.Team, 0, len(rows))
	for _, row := range rows {
		items = append(items, team(row.Team, row.MemberCount))
	}
	return apigen.ListTeams200JSONResponse{Items: items}, nil
}

func (s *server) CreateTeam(ctx context.Context, req apigen.CreateTeamRequestObject) (apigen.CreateTeamResponseObject, error) {
	a, actor, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := identity.NewTeam{Slug: b.Slug, Name: b.Name, ParentID: b.ParentId}
	if b.Description != nil {
		in.Description = *b.Description
	}
	t, err := a.Identity().CreateTeam(ctx, actor, in)
	if err != nil {
		return nil, teamError(err)
	}
	return apigen.CreateTeam201JSONResponse(team(t, 0)), nil
}

func (s *server) GetTeam(ctx context.Context, req apigen.GetTeamRequestObject) (apigen.GetTeamResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	row, err := a.Identity().GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, teamError(err)
	}
	return apigen.GetTeam200JSONResponse(team(row.Team, row.MemberCount)), nil
}

func (s *server) UpdateTeam(ctx context.Context, req apigen.UpdateTeamRequestObject) (apigen.UpdateTeamResponseObject, error) {
	a, actor, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := identity.TeamUpdate{Slug: b.Slug, Name: b.Name, Description: b.Description}
	if b.ParentId != nil {
		parent := uuid.Nil
		if v := strings.TrimSpace(*b.ParentId); v != "" {
			if parent, err = uuid.Parse(v); err != nil {
				return nil, problemErr(http.StatusBadRequest, "parentId must be a team ID or an empty string.")
			}
		}
		in.ParentID = &parent
	}
	if _, err := a.Identity().UpdateTeam(ctx, actor, req.TeamId, in); err != nil {
		return nil, teamError(err)
	}
	row, err := a.Identity().GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, teamError(err)
	}
	return apigen.UpdateTeam200JSONResponse(team(row.Team, row.MemberCount)), nil
}

func (s *server) ArchiveTeam(ctx context.Context, req apigen.ArchiveTeamRequestObject) (apigen.ArchiveTeamResponseObject, error) {
	a, actor, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := a.Identity().ArchiveTeam(ctx, actor, req.TeamId); err != nil {
		return nil, teamError(err)
	}
	row, err := a.Identity().GetTeam(ctx, req.TeamId)
	if err != nil {
		return nil, teamError(err)
	}
	return apigen.ArchiveTeam200JSONResponse(team(row.Team, row.MemberCount)), nil
}

func (s *server) ListTeamMembers(ctx context.Context, req apigen.ListTeamMembersRequestObject) (apigen.ListTeamMembersResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	rows, err := a.Identity().TeamMembers(ctx, req.TeamId)
	if err != nil {
		return nil, teamError(err)
	}
	items := make([]apigen.TeamMember, 0, len(rows))
	for _, row := range rows {
		items = append(items, apigen.TeamMember{
			Person:   personCard(row.Person, row.HasAvatar),
			Role:     apigen.TeamRole(row.Role),
			JoinedAt: row.JoinedAt,
		})
	}
	return apigen.ListTeamMembers200JSONResponse{Items: items}, nil
}

func (s *server) SetTeamMember(ctx context.Context, req apigen.SetTeamMemberRequestObject) (apigen.SetTeamMemberResponseObject, error) {
	a, actor, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	m, err := a.Identity().SetTeamMember(ctx, actor, req.TeamId, req.PersonId, identity.TeamRole(req.Body.Role))
	if err != nil {
		return nil, teamError(err)
	}
	return apigen.SetTeamMember200JSONResponse{TeamId: m.TeamID, PersonId: m.PersonID, Role: apigen.TeamRole(m.Role), JoinedAt: m.CreatedAt}, nil
}

func (s *server) RemoveTeamMember(ctx context.Context, req apigen.RemoveTeamMemberRequestObject) (apigen.RemoveTeamMemberResponseObject, error) {
	a, actor, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.Identity().RemoveTeamMember(ctx, actor, req.TeamId, req.PersonId); err != nil {
		return nil, teamError(err)
	}
	return apigen.RemoveTeamMember204Response{}, nil
}

func (s *server) ListPersonTeams(ctx context.Context, req apigen.ListPersonTeamsRequestObject) (apigen.ListPersonTeamsResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	person, err := a.Identity().GetPerson(ctx, req.PersonId)
	if errors.Is(err, identity.ErrNotFound) || (err == nil && identity.Status(person.Status) == identity.StatusOffboarded) {
		return nil, problemErr(http.StatusNotFound, "")
	} else if err != nil {
		return nil, err
	}
	rows, err := a.Identity().PersonTeams(ctx, req.PersonId)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.PersonTeam, 0, len(rows))
	for _, row := range rows {
		items = append(items, apigen.PersonTeam{Team: team(row.Team, row.MemberCount), Role: apigen.TeamRole(row.Role)})
	}
	return apigen.ListPersonTeams200JSONResponse{Items: items}, nil
}

// manager returns the identity stack and the audit actor of a caller
// managing the directory. The route guard has checked the permission.
func (s *server) manager(ctx context.Context) (*web.Auth, audit.Actor, error) {
	a, err := s.auth()
	if err != nil {
		return nil, audit.Actor{}, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, audit.Actor{}, err
	}
	_, r := httpFrom(ctx)
	return a, a.Actor(r, personID), nil
}

// teamError maps team errors to problem details.
func teamError(err error) error {
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return problemErr(http.StatusNotFound, err.Error())
	case errors.Is(err, identity.ErrInvalid):
		return problemErr(http.StatusBadRequest, err.Error())
	case errors.Is(err, identity.ErrSlugTaken):
		return problemErr(http.StatusConflict, "Another team already uses this slug.")
	case errors.Is(err, identity.ErrTeamArchived):
		return problemErr(http.StatusConflict, "The team is archived and cannot be changed.")
	case errors.Is(err, identity.ErrTeamHasChildren):
		return problemErr(http.StatusConflict, "The team has active child teams; archive or move them first.")
	case errors.Is(err, identity.ErrPersonOffboarded):
		return problemErr(http.StatusConflict, "Offboarded people cannot join teams.")
	default:
		return err
	}
}

func team(t store.Team, memberCount int64) apigen.Team {
	return apigen.Team{
		Id:          t.ID,
		Slug:        t.Slug,
		Name:        t.Name,
		Description: t.Description,
		ParentId:    t.ParentID,
		MemberCount: int(memberCount),
		ArchivedAt:  t.ArchivedAt,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

func personCard(p store.Person, hasAvatar bool) apigen.PersonCard {
	c := apigen.PersonCard{
		Id:          p.ID,
		Username:    p.Username,
		DisplayName: p.DisplayName,
		Title:       p.Title,
		Status:      apigen.PersonStatus(p.Status),
	}
	if hasAvatar {
		url := avatarURL(p.ID)
		c.AvatarUrl = &url
	}
	return c
}

func avatarURL(id uuid.UUID) string {
	return VersionPrefix + "people/" + id.String() + "/avatar"
}
