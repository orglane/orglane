package profile

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// Entry is one field of a person's profile as a viewer sees it.
type Entry struct {
	Field Field
	// Value is the value in its stored form, "" when unset or hidden from
	// the viewer. A birthday whose year the viewer may not see is --MM-DD.
	Value string
	// Hidden: the person hid the value from others. Only viewers who still
	// see hidden values learn it.
	Hidden bool
	// YearHidden: Value is a birthday without its year.
	YearHidden bool
	// Editable: the viewer may set or clear the value.
	Editable bool
	// Hideable: the viewer (the person) may hide the value.
	Hideable bool
}

// Change sets, clears, hides or shows one value of a profile.
type Change struct {
	// Key is the field key.
	Key string
	// Value is the new value, "" to clear it; nil leaves it unchanged.
	Value *string
	// Hidden hides (true) or shows (false) the value; nil leaves it.
	Hidden *bool
}

// Profile returns the person's profile fields as the viewer may see them,
// by section and position: every field the viewer may see, with its value
// when set and visible to the viewer. Reads of sensitive values are
// audited in the same transaction. Offboarded people are not found except
// by holders of people.manage.
func (s *Service) Profile(ctx context.Context, actor audit.Actor, viewer Viewer, personID uuid.UUID) ([]Entry, error) {
	rel := relation{self: viewer.PersonID == personID}
	if !rel.self && !viewer.ManagePeople {
		// Resolved before the transaction: Teams may use its own
		// connection.
		shares, err := s.teams.SharesTeam(ctx, viewer.PersonID, personID)
		if err != nil {
			return nil, fmt.Errorf("profile: resolve teams: %w", err)
		}
		rel.teammate = shares
	}
	var entries []Entry
	err := s.inTx(ctx, func(q *store.Queries) error {
		person, err := q.GetPerson(ctx, personID)
		if err != nil {
			return notFound(err, "person")
		}
		if person.Status == "offboarded" && !viewer.ManagePeople {
			return fmt.Errorf("%w: person", ErrNotFound)
		}
		rows, err := q.ListProfileFields(ctx)
		if err != nil {
			return fmt.Errorf("profile: list fields: %w", err)
		}
		fields := make([]Field, 0, len(rows))
		for _, row := range rows {
			f, err := fieldFrom(row)
			if err != nil {
				return err
			}
			fields = append(fields, f)
		}
		sortFields(fields)
		values, err := s.values(ctx, q, personID)
		if err != nil {
			return err
		}

		var sensitive []string
		entries = make([]Entry, 0, len(fields))
		for _, f := range fields {
			acc := decide(f, viewer, rel)
			if !acc.see {
				continue
			}
			e := Entry{Field: f, Editable: acc.edit, Hideable: acc.hide}
			if row, ok := values[f.ID]; ok && (!row.Hidden || acc.seeHidden) {
				if e.Value, err = s.open(row); err != nil {
					return err
				}
				e.Hidden = row.Hidden
				if f.Sensitive {
					sensitive = append(sensitive, f.Key)
				}
				if f.Type == TypeDate && !acc.fullDate {
					e.Value, e.YearHidden = maskYear(e.Value), true
				}
			}
			entries = append(entries, e)
		}
		if len(sensitive) == 0 {
			return nil
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PersonSensitiveFieldsRead,
			Target:   audit.Target{Kind: audit.TargetPerson, ID: personID.String()},
			Metadata: map[string]any{"fields": sensitive},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// UpdateProfile applies changes to the person's profile at once, all or
// nothing, and returns the profile as the viewer sees it afterwards. The
// audit entry lists the changed field keys, never the values. Only the
// person and holders of people.manage change a profile; fields the viewer
// cannot see are unknown (ErrInvalid), and fields they see but may not
// change are ErrForbidden.
func (s *Service) UpdateProfile(ctx context.Context, actor audit.Actor, viewer Viewer, personID uuid.UUID, changes []Change) ([]Entry, error) {
	rel := relation{self: viewer.PersonID == personID}
	if !rel.self && !viewer.ManagePeople {
		return nil, fmt.Errorf("%w: only the person and admins change a profile", ErrForbidden)
	}
	err := s.inTx(ctx, func(q *store.Queries) error {
		person, err := q.GetPersonForUpdate(ctx, personID)
		if err != nil {
			return notFound(err, "person")
		}
		if person.Status == "offboarded" {
			return fmt.Errorf("%w: person", ErrNotFound)
		}
		rows, err := q.ListActiveProfileFieldsForShare(ctx)
		if err != nil {
			return fmt.Errorf("profile: list fields: %w", err)
		}
		byKey := make(map[string]Field, len(rows))
		for _, row := range rows {
			f, err := fieldFrom(row)
			if err != nil {
				return err
			}
			byKey[f.Key] = f
		}
		values, err := s.values(ctx, q, personID)
		if err != nil {
			return err
		}

		seen := make(map[string]bool, len(changes))
		var changed []string
		for _, c := range changes {
			f, ok := byKey[c.Key]
			acc := decide(f, viewer, rel)
			if !ok || !acc.see {
				return invalid("values", "unknown field %q", c.Key)
			}
			if seen[c.Key] {
				return invalid("values", "field %q appears twice", c.Key)
			}
			seen[c.Key] = true

			row, has := values[f.ID]
			current, hidden := "", false
			if has {
				if current, err = s.open(row); err != nil {
					return err
				}
				hidden = row.Hidden
			}
			next, nextHidden := current, hidden
			if c.Value != nil {
				if !acc.edit {
					return fmt.Errorf("%w: %s is not editable by you", ErrForbidden, c.Key)
				}
				if next, err = f.normalize(*c.Value); err != nil {
					return err
				}
			}
			if c.Hidden != nil && *c.Hidden != hidden {
				switch {
				case !rel.self:
					return fmt.Errorf("%w: only the person hides or shows their values", ErrForbidden)
				case *c.Hidden && !acc.hide:
					return fmt.Errorf("%w: %s cannot be hidden", ErrForbidden, c.Key)
				}
				nextHidden = *c.Hidden
			}
			switch {
			case next == "" && !has:
				continue // nothing to clear (hiding needs a value)
			case next == current && nextHidden == hidden:
				continue
			case next == "":
				if _, err := q.DeleteProfileFieldValue(ctx, store.DeleteProfileFieldValueParams{PersonID: personID, FieldID: f.ID}); err != nil {
					return fmt.Errorf("profile: clear value: %w", err)
				}
			default:
				if err := s.put(ctx, q, f, personID, next, nextHidden); err != nil {
					return err
				}
			}
			changed = append(changed, f.Key)
		}
		if len(changed) == 0 {
			return nil
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PersonProfileUpdated,
			Target:   audit.Target{Kind: audit.TargetPerson, ID: personID.String()},
			Metadata: map[string]any{"fields": changed},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Profile(ctx, actor, viewer, personID)
}

func (s *Service) values(ctx context.Context, q *store.Queries, personID uuid.UUID) (map[uuid.UUID]store.ProfileFieldValue, error) {
	rows, err := q.ListProfileFieldValues(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("profile: load values: %w", err)
	}
	out := make(map[uuid.UUID]store.ProfileFieldValue, len(rows))
	for _, row := range rows {
		out[row.FieldID] = row
	}
	return out, nil
}

// valueAD binds an encrypted value to its person and field, so ciphertexts
// cannot be moved between rows.
func valueAD(personID, fieldID uuid.UUID) []byte {
	ad := make([]byte, 0, 20+32)
	ad = append(ad, "profile_field_value:"...)
	ad = append(ad, personID[:]...)
	return append(ad, fieldID[:]...)
}

// open returns a stored value, decrypting it when sealed.
func (s *Service) open(row store.ProfileFieldValue) (string, error) {
	if row.Value != nil {
		return *row.Value, nil
	}
	plain, err := s.sealer.Open(row.ValueSealed, valueAD(row.PersonID, row.FieldID))
	if err != nil {
		return "", fmt.Errorf("profile: decrypt value of field %s: %w", row.FieldID, err)
	}
	return string(plain), nil
}

// put stores a value, encrypted when the field is sensitive.
func (s *Service) put(ctx context.Context, q *store.Queries, f Field, personID uuid.UUID, value string, hidden bool) error {
	if value == "" {
		return errors.New("profile: put an empty value")
	}
	p := store.PutProfileFieldValueParams{PersonID: personID, FieldID: f.ID, Hidden: hidden}
	if f.Sensitive {
		p.ValueSealed = s.sealer.Seal([]byte(value), valueAD(personID, f.ID))
	} else {
		p.Value = &value
	}
	if err := q.PutProfileFieldValue(ctx, p); err != nil {
		return fmt.Errorf("profile: store value: %w", err)
	}
	return nil
}
