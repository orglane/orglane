package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/store"
)

// DirectoryOptions selects one bounded page. Cursor is the last ID of the
// previous page. UpdatedSince includes offboarded cache tombstones, and must
// not combine with filters (which cannot represent removal from a result set).
type DirectoryOptions struct {
	Search       string
	TeamID       *uuid.UUID
	Role         string
	Limit        int
	Cursor       *uuid.UUID
	UpdatedSince *time.Time
}

// DirectoryTeam is a public team label, without membership or private fields.
type DirectoryTeam struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
}

// DirectoryPerson holds only the directory's public base data.
type DirectoryPerson struct {
	Person    store.Person
	Roles     []string
	HasAvatar bool
	Teams     []DirectoryTeam
}

// DirectoryPage is a keyset page in stable UUID order.
type DirectoryPage struct {
	Items      []DirectoryPerson
	NextCursor *uuid.UUID
}

// DirectoryPage returns current public rows. It does not read profile values.
func (s *Service) DirectoryPage(ctx context.Context, in DirectoryOptions) (DirectoryPage, error) {
	if in.Limit == 0 {
		in.Limit = 50
	}
	in.Search = strings.TrimSpace(in.Search)
	in.Role = strings.TrimSpace(in.Role)
	if in.Limit < 1 || in.Limit > 100 || !utf8.ValidString(in.Search) || utf8.RuneCountInString(in.Search) > 200 || strings.ContainsRune(in.Search, 0) || !utf8.ValidString(in.Role) || len(in.Role) > 63 || strings.ContainsRune(in.Role, 0) {
		return DirectoryPage{}, invalid("directory", "limit must be 1-100, search at most 200 characters and role at most 63 bytes")
	}
	if in.UpdatedSince != nil && (in.Search != "" || in.TeamID != nil || in.Role != "") {
		return DirectoryPage{}, invalid("updated_since", "cannot combine incremental updates with search, team or role filters")
	}
	search := ""
	if in.Search != "" {
		search = "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(in.Search)) + "%"
	}
	rows, err := s.queries().ListDirectoryPage(ctx, store.ListDirectoryPageParams{Search: search, TeamID: in.TeamID, RoleKey: in.Role, Cursor: in.Cursor, UpdatedSince: in.UpdatedSince, PageSize: int32(in.Limit + 1)})
	if err != nil {
		return DirectoryPage{}, fmt.Errorf("identity: directory page: %w", err)
	}
	out := DirectoryPage{Items: make([]DirectoryPerson, 0, min(len(rows), in.Limit))}
	if len(rows) > in.Limit {
		next := rows[in.Limit-1].Person.ID
		out.NextCursor = &next
		rows = rows[:in.Limit]
	}
	for _, r := range rows {
		p, err := directoryPerson(r.Person, r.RoleKeys, r.HasAvatar, r.Teams)
		if err != nil {
			return DirectoryPage{}, err
		}
		out.Items = append(out.Items, p)
	}
	return out, nil
}

// DirectoryPerson returns the public header and contacts of a visible person.
func (s *Service) DirectoryPerson(ctx context.Context, id uuid.UUID) (DirectoryPerson, error) {
	r, err := s.queries().GetDirectoryPerson(ctx, id)
	if err != nil {
		return DirectoryPerson{}, notFound(err, "person")
	}
	return directoryPerson(r.Person, r.RoleKeys, r.HasAvatar, r.Teams)
}

func directoryPerson(person store.Person, roles []string, hasAvatar bool, teams []byte) (DirectoryPerson, error) {
	// Contact addresses belong to invitation delivery, never to a public
	// directory projection, including trusted higher-level readers.
	person.ContactEmail = nil
	person.UpdatedAt = person.DirectoryUpdatedAt
	out := DirectoryPerson{Person: person, Roles: roles, HasAvatar: hasAvatar, Teams: []DirectoryTeam{}}
	if err := json.Unmarshal(teams, &out.Teams); err != nil {
		return DirectoryPerson{}, fmt.Errorf("identity: decode directory teams: %w", err)
	}
	return out, nil
}
