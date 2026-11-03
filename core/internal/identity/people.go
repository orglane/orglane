package identity

import (
	"context"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// Status is the lifecycle state of a person.
type Status string

// Person statuses.
const (
	// StatusInvited: added to the directory, has not signed in yet.
	StatusInvited Status = "invited"
	// StatusActive: can sign in.
	StatusActive Status = "active"
	// StatusSuspended: temporarily blocked; sessions are revoked.
	StatusSuspended Status = "suspended"
	// StatusOffboarded: left the company; final.
	StatusOffboarded Status = "offboarded"
)

// transitions lists the allowed status changes.
var transitions = map[Status][]Status{
	StatusInvited:    {StatusActive, StatusOffboarded},
	StatusActive:     {StatusSuspended, StatusOffboarded},
	StatusSuspended:  {StatusActive, StatusOffboarded},
	StatusOffboarded: {},
}

// Limits matching the database constraints (core/migrations/00002_identity.sql).
const (
	maxEmailLength       = 254
	maxDisplayNameLength = 200
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// NewPerson describes a person to add to the directory.
type NewPerson struct {
	// Username is normalized to lower case: 1-63 characters of a-z, 0-9,
	// '.', '_' and '-', starting with a letter or digit.
	Username string
	// WorkEmail is unique case-insensitively and stored as entered.
	WorkEmail   string
	DisplayName string
	// Status is StatusInvited (the default) or StatusActive.
	Status Status
}

// PersonUpdate changes base fields of a person. Nil fields stay unchanged.
type PersonUpdate struct {
	Username    *string
	WorkEmail   *string
	DisplayName *string
}

// CreatePerson adds a person to the directory.
func (s *Service) CreatePerson(ctx context.Context, actor audit.Actor, in NewPerson) (store.Person, error) {
	params := store.CreatePersonParams{
		Status: string(in.Status),
	}
	var err error
	if params.Username, err = normalizeUsername(in.Username); err != nil {
		return store.Person{}, err
	}
	if params.WorkEmail, err = normalizeEmail(in.WorkEmail); err != nil {
		return store.Person{}, err
	}
	if params.DisplayName, err = normalizeDisplayName(in.DisplayName); err != nil {
		return store.Person{}, err
	}
	switch in.Status {
	case "":
		params.Status = string(StatusInvited)
	case StatusInvited, StatusActive:
	default:
		return store.Person{}, invalid("status", "a new person must be %s or %s, not %q", StatusInvited, StatusActive, in.Status)
	}

	var person store.Person
	err = s.inTx(ctx, func(q *store.Queries) error {
		var err error
		if person, err = q.CreatePerson(ctx, params); err != nil {
			return uniqueViolation(err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PersonCreated,
			Target:   personTarget(person.ID),
			Metadata: map[string]any{"status": person.Status},
		})
		return err
	})
	if err != nil {
		return store.Person{}, err
	}
	return person, nil
}

// UpdatePerson changes the base fields of a person. The audit entry lists
// the changed fields, not their values.
func (s *Service) UpdatePerson(ctx context.Context, actor audit.Actor, id uuid.UUID, in PersonUpdate) (store.Person, error) {
	var person store.Person
	err := s.inTx(ctx, func(q *store.Queries) error {
		current, err := q.GetPersonForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "person")
		}
		params := store.UpdatePersonParams{
			ID:          id,
			Username:    current.Username,
			WorkEmail:   current.WorkEmail,
			DisplayName: current.DisplayName,
		}
		var changed []string
		if in.Username != nil {
			if params.Username, err = normalizeUsername(*in.Username); err != nil {
				return err
			}
			if params.Username != current.Username {
				changed = append(changed, "username")
			}
		}
		if in.WorkEmail != nil {
			if params.WorkEmail, err = normalizeEmail(*in.WorkEmail); err != nil {
				return err
			}
			if params.WorkEmail != current.WorkEmail {
				changed = append(changed, "work_email")
			}
		}
		if in.DisplayName != nil {
			if params.DisplayName, err = normalizeDisplayName(*in.DisplayName); err != nil {
				return err
			}
			if params.DisplayName != current.DisplayName {
				changed = append(changed, "display_name")
			}
		}
		if len(changed) == 0 {
			person = current
			return nil
		}
		if person, err = q.UpdatePerson(ctx, params); err != nil {
			return uniqueViolation(err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PersonUpdated,
			Target:   personTarget(id),
			Metadata: map[string]any{"fields": changed},
		})
		return err
	})
	if err != nil {
		return store.Person{}, err
	}
	return person, nil
}

// SetPersonStatus moves a person to another status. Suspending or
// offboarding a person revokes all their sessions. The last active owner
// cannot be suspended or offboarded.
func (s *Service) SetPersonStatus(ctx context.Context, actor audit.Actor, id uuid.UUID, to Status) (store.Person, error) {
	if _, ok := transitions[to]; !ok {
		return store.Person{}, invalid("status", "unknown status %q", to)
	}
	var person store.Person
	err := s.inTx(ctx, func(q *store.Queries) error {
		current, err := q.GetPersonForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "person")
		}
		from := Status(current.Status)
		if from == to {
			person = current
			return nil
		}
		if !slices.Contains(transitions[from], to) {
			return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, from, to)
		}
		if from == StatusActive {
			if err := ensureOwnerRemains(ctx, q, current); err != nil {
				return err
			}
		}
		if person, err = q.SetPersonStatus(ctx, store.SetPersonStatusParams{ID: id, Status: string(to)}); err != nil {
			return fmt.Errorf("identity: set status: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PersonStatusChanged,
			Target:   personTarget(id),
			Metadata: map[string]any{"from": from, "to": to},
		}); err != nil {
			return err
		}
		if to == StatusSuspended || to == StatusOffboarded {
			sessions, err := q.RevokePersonSessions(ctx, id)
			if err != nil {
				return fmt.Errorf("identity: revoke sessions: %w", err)
			}
			return recordSessionsRevoked(ctx, q, actor, sessions, "person_"+string(to))
		}
		return nil
	})
	if err != nil {
		return store.Person{}, err
	}
	return person, nil
}

// GetPerson returns a person by ID.
func (s *Service) GetPerson(ctx context.Context, id uuid.UUID) (store.Person, error) {
	p, err := s.queries().GetPerson(ctx, id)
	if err != nil {
		return store.Person{}, notFound(err, "person")
	}
	return p, nil
}

// GetPersonByWorkEmail returns a person by work email, case-insensitively.
func (s *Service) GetPersonByWorkEmail(ctx context.Context, email string) (store.Person, error) {
	p, err := s.queries().GetPersonByWorkEmail(ctx, strings.TrimSpace(email))
	if err != nil {
		return store.Person{}, notFound(err, "person")
	}
	return p, nil
}

func personTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetPerson, ID: id.String()}
}

func normalizeUsername(u string) (string, error) {
	u = strings.ToLower(strings.TrimSpace(u))
	if !usernamePattern.MatchString(u) {
		return "", invalid("username", "must be 1-63 characters of a-z, 0-9, '.', '_' and '-', starting with a letter or digit")
	}
	return u, nil
}

func normalizeEmail(e string) (string, error) {
	e = strings.TrimSpace(e)
	if e == "" || len(e) > maxEmailLength {
		return "", invalid("work_email", "must be 1-%d characters", maxEmailLength)
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Name != "" || addr.Address != e || strings.Count(e, "@") != 1 || strings.ContainsAny(e, " \t\r\n") {
		return "", invalid("work_email", "must be a bare address such as name@example.com")
	}
	return e, nil
}

func normalizeDisplayName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || utf8.RuneCountInString(n) > maxDisplayNameLength || !utf8.ValidString(n) || strings.ContainsRune(n, 0) {
		return "", invalid("display_name", "must be 1-%d characters", maxDisplayNameLength)
	}
	return n, nil
}
