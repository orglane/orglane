package apps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// desired is a person's desired access in an instance with the state it was
// computed from.
type desired struct {
	access provider.DesiredAccess
	hash   []byte
	// result is the person's current result row, nil if none.
	result *store.ProvisioningResult
	person store.Person
}

// computeDesired computes a person's desired access in an instance from
// the directory (docs/specs/providers.md "Desired access and team
// mappings"). It is never stored as truth; only its hash is.
func (s *Service) computeDesired(ctx context.Context, q *store.Queries, inst Instance, personID uuid.UUID) (desired, error) {
	person, err := q.GetPerson(ctx, personID)
	if errors.Is(err, pgx.ErrNoRows) {
		return desired{}, fmt.Errorf("%w: person", ErrNotFound)
	} else if err != nil {
		return desired{}, fmt.Errorf("apps: load person: %w", err)
	}
	var result *store.ProvisioningResult
	if row, err := q.GetProvisioningResult(ctx, store.GetProvisioningResultParams{InstanceID: inst.ID, PersonID: personID}); err == nil {
		result = &row
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return desired{}, fmt.Errorf("apps: load result: %w", err)
	}
	mapped, err := q.ListPersonMappedTeams(ctx, store.ListPersonMappedTeamsParams{InstanceID: inst.ID, PersonID: personID})
	if err != nil {
		return desired{}, fmt.Errorf("apps: load mapped teams: %w", err)
	}
	eligible := person.Status == "active"
	access := eligible && (inst.AccessPolicy == string(provider.AllPeople) || len(mapped) > 0)
	d := provider.DesiredAccess{
		PersonID:    person.ID,
		Username:    person.Username,
		DisplayName: person.DisplayName,
		WorkEmail:   person.WorkEmail,
		Access:      access,
		Teams:       []provider.TeamRef{},
		Managed:     (result != nil && result.GrantedByOrglane) || inst.ManageExistingMembers,
	}
	if access { // never plan team additions for someone without access
		for _, m := range mapped {
			d.Teams = append(d.Teams, provider.TeamRef{TeamID: m.TeamID, ExternalRef: derefString(m.ExternalRef), Name: m.ExternalName})
		}
	}
	if inst.Adapter != nil {
		link, err := q.GetVerifiedAccountLink(ctx, store.GetVerifiedAccountLinkParams{PersonID: personID, Provider: inst.Provider, Realm: inst.Adapter.Realm(inst.View())})
		if err == nil {
			d.Link = &provider.AccountLink{
				ID:         link.ID,
				Account:    provider.ExternalAccount{Realm: link.Realm, ExternalID: link.ExternalID, Login: link.Login},
				Method:     link.Method,
				VerifiedAt: link.VerifiedAt,
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return desired{}, fmt.Errorf("apps: load link: %w", err)
		}
	}
	return desired{access: d, hash: hashDesired(d), result: result, person: person}, nil
}

// hashDesired is the SHA-256 of the canonical DesiredAccess. Logins are
// informational and left out (they change; links are keyed by ID).
func hashDesired(d provider.DesiredAccess) []byte {
	type team struct {
		TeamID uuid.UUID `json:"team_id"`
		Ref    string    `json:"ref"`
	}
	type link struct {
		ID         uuid.UUID `json:"id"`
		Realm      string    `json:"realm"`
		ExternalID string    `json:"external_id"`
		Method     string    `json:"method"`
	}
	c := struct {
		PersonID    uuid.UUID `json:"person_id"`
		Username    string    `json:"username"`
		DisplayName string    `json:"display_name"`
		WorkEmail   string    `json:"work_email"`
		Access      bool      `json:"access"`
		Teams       []team    `json:"teams"`
		Link        *link     `json:"link"`
		Managed     bool      `json:"managed"`
	}{PersonID: d.PersonID, Username: d.Username, DisplayName: d.DisplayName, WorkEmail: d.WorkEmail, Access: d.Access, Teams: []team{}, Managed: d.Managed}
	for _, t := range d.Teams {
		c.Teams = append(c.Teams, team{TeamID: t.TeamID, Ref: t.ExternalRef})
	}
	slices.SortFunc(c.Teams, func(a, b team) int {
		if c := strings.Compare(a.TeamID.String(), b.TeamID.String()); c != 0 {
			return c
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	if d.Link != nil {
		c.Link = &link{ID: d.Link.ID, Realm: d.Link.Account.Realm, ExternalID: d.Link.Account.ExternalID, Method: d.Link.Method}
	}
	raw, _ := json.Marshal(c) // only plain types: cannot fail
	sum := sha256.Sum256(raw)
	return sum[:]
}
