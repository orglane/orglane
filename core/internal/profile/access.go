package profile

import "github.com/google/uuid"

// Viewer is who reads or changes a profile: a signed-in person and the
// permissions they hold (ADR 0004).
type Viewer struct {
	PersonID uuid.UUID
	// ManagePeople is people.manage: admins see every non-sensitive value,
	// including hidden ones, and edit every field.
	ManagePeople bool
	// ReadSensitive is people.sensitive.read: sensitive values and the year
	// of birthdays.
	ReadSensitive bool
}

// relation is how the viewer relates to the person whose profile it is.
type relation struct {
	self     bool
	teammate bool
}

// access is what a viewer may do with one field of one person's profile.
type access struct {
	// see: the field and its value are shown (unless hidden, below).
	see bool
	// seeHidden: values the person hid are shown too.
	seeHidden bool
	// fullDate: a birthday is shown with its year.
	fullDate bool
	// edit: the value may be set or cleared.
	edit bool
	// hide: the value may be hidden from others.
	hide bool
}

// decide is the one place that applies the visibility and edit rules of
// profile fields (ADR 0015). Every view and change of values goes through
// it.
//
//   - Archived fields are nobody's business.
//   - Sensitive fields: only the person and holders of
//     people.sensitive.read; every read is audited (by the caller).
//   - Visibility: "everyone" is every signed-in person; "team" is people
//     sharing a team with the person; "admins" is nobody else. The person
//     and holders of people.manage always see the field.
//   - Hidden values (user can hide): only the person and holders of
//     people.manage still see them.
//   - Birthday: day and month for everyone; the year only for the person
//     and holders of people.sensitive.read.
//   - Editing: what one sees, by the person when the field is editable by
//     "self", and by holders of people.manage.
//   - Hiding: only the person, when the field allows it.
func decide(f Field, v Viewer, rel relation) access {
	if f.Archived {
		return access{}
	}
	if f.Sensitive && !rel.self && !v.ReadSensitive {
		return access{}
	}
	var see bool
	switch f.Visibility {
	case VisibilityEveryone:
		see = true
	case VisibilityTeam:
		see = rel.self || v.ManagePeople || rel.teammate
	default: // admins, and anything unknown fails closed
		see = rel.self || v.ManagePeople
	}
	if !see {
		return access{}
	}
	return access{
		see:       true,
		seeHidden: rel.self || v.ManagePeople,
		fullDate:  !f.birthday() || rel.self || v.ReadSensitive,
		edit:      (rel.self && f.EditableBy == EditableBySelf) || v.ManagePeople,
		hide:      rel.self && f.UserCanHide,
	}
}
