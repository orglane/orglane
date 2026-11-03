package profile

import (
	"testing"

	"github.com/google/uuid"
)

// TestDecide covers the rule table of decide for every kind of viewer.
func TestDecide(t *testing.T) {
	member := Viewer{PersonID: uuid.New()}
	admin := Viewer{PersonID: uuid.New(), ManagePeople: true}
	sensitiveAdmin := Viewer{PersonID: uuid.New(), ManagePeople: true, ReadSensitive: true}
	self, teammate, other := relation{self: true}, relation{teammate: true}, relation{}

	type viewerCase struct {
		name string
		v    Viewer
		rel  relation
	}
	viewers := []viewerCase{
		{"self", member, self},
		{"teammate", member, teammate},
		{"other member", member, other},
		{"admin", admin, other},
		{"sensitive admin", sensitiveAdmin, other},
	}
	// Expected "see" per visibility, in the order of viewers.
	visibility := map[Visibility][]bool{
		VisibilityEveryone: {true, true, true, true, true},
		VisibilityTeam:     {true, true, false, true, true},
		VisibilityAdmins:   {true, false, false, true, true},
	}
	for vis, want := range visibility {
		for i, vc := range viewers {
			f := Field{Key: "f", Visibility: vis, EditableBy: EditableBySelf}
			if got := decide(f, vc.v, vc.rel).see; got != want[i] {
				t.Errorf("%s field, %s: see = %v, want %v", vis, vc.name, got, want[i])
			}
		}
	}

	sensitive := Field{Key: "s", Visibility: VisibilityEveryone, EditableBy: EditableBySelf, Sensitive: true}
	for i, want := range []bool{true, false, false, false, true} {
		vc := viewers[i]
		if got := decide(sensitive, vc.v, vc.rel).see; got != want {
			t.Errorf("sensitive field, %s: see = %v, want %v", vc.name, got, want)
		}
	}

	archived := Field{Key: "a", Visibility: VisibilityEveryone, EditableBy: EditableBySelf, Archived: true}
	for _, vc := range viewers {
		if got := decide(archived, vc.v, vc.rel); got != (access{}) {
			t.Errorf("archived field, %s: %+v", vc.name, got)
		}
	}

	hideable := Field{Key: "h", Visibility: VisibilityEveryone, EditableBy: EditableBySelf, UserCanHide: true}
	for i, want := range []bool{true, false, false, true, true} {
		vc := viewers[i]
		if got := decide(hideable, vc.v, vc.rel).seeHidden; got != want {
			t.Errorf("hidden values, %s: seeHidden = %v, want %v", vc.name, got, want)
		}
		if got := decide(hideable, vc.v, vc.rel).hide; got != (i == 0) {
			t.Errorf("hide, %s: %v", vc.name, got)
		}
	}
	if decide(Field{Key: "n", Visibility: VisibilityEveryone, EditableBy: EditableBySelf}, member, self).hide {
		t.Error("the person may hide a field that does not allow it")
	}

	birthday := Field{Key: "birthday", Library: true, Type: TypeDate, Visibility: VisibilityEveryone, EditableBy: EditableBySelf}
	for i, want := range []bool{true, false, false, false, true} {
		vc := viewers[i]
		if got := decide(birthday, vc.v, vc.rel).fullDate; got != want {
			t.Errorf("birthday, %s: fullDate = %v, want %v", vc.name, got, want)
		}
	}
	customDate := Field{Key: "anniversary", Type: TypeDate, Visibility: VisibilityEveryone, EditableBy: EditableBySelf}
	if !decide(customDate, member, other).fullDate {
		t.Error("only the library birthday hides its year")
	}

	for _, editor := range Editors {
		f := Field{Key: "e", Visibility: VisibilityEveryone, EditableBy: editor}
		for i, want := range []bool{editor == EditableBySelf, false, false, true, true} {
			vc := viewers[i]
			if got := decide(f, vc.v, vc.rel).edit; got != want {
				t.Errorf("editable by %s, %s: edit = %v, want %v", editor, vc.name, got, want)
			}
		}
	}
	// What one cannot see, one cannot edit.
	if decide(sensitive, admin, other).edit {
		t.Error("an admin without people.sensitive.read edits a sensitive field")
	}
	if got := decide(Field{Key: "x", Visibility: "unknown"}, member, teammate); got.see {
		t.Errorf("an unknown visibility does not fail closed: %+v", got)
	}
}
