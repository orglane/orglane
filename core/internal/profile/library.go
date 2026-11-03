package profile

import "slices"

// LibraryEntry is a ready-made field admins can turn on (ADR 0015). Its
// defaults become the field's settings; admins can change visibility,
// section and the advanced settings afterwards, but not the key, label,
// type or options, which clients know by the key (translated labels).
type LibraryEntry struct {
	Key         string
	Label       string // English; clients translate by Key
	Type        Type
	HandleKind  HandleKind
	Options     []Option
	Section     Section
	Visibility  Visibility
	EditableBy  Editor
	UserCanHide bool
	Sensitive   bool
}

// library is the library of ready-made fields, in display order. Keys never
// change once released: fields reference their entry by key.
var library = []LibraryEntry{
	{Key: "telegram", Label: "Telegram", Type: TypeHandle, HandleKind: HandleTelegram, Section: SectionContacts, Visibility: VisibilityEveryone, EditableBy: EditableBySelf, UserCanHide: true},
	{Key: "github", Label: "GitHub", Type: TypeHandle, HandleKind: HandleGitHub, Section: SectionContacts, Visibility: VisibilityEveryone, EditableBy: EditableBySelf, UserCanHide: true},
	{Key: "phone", Label: "Phone", Type: TypePhone, Section: SectionContacts, Visibility: VisibilityTeam, EditableBy: EditableBySelf, UserCanHide: true},
	{Key: "pronouns", Label: "Pronouns", Type: TypeText, Section: SectionPersonal, Visibility: VisibilityEveryone, EditableBy: EditableBySelf, UserCanHide: true},
	{Key: "birthday", Label: "Birthday", Type: TypeDate, Section: SectionPersonal, Visibility: VisibilityEveryone, EditableBy: EditableBySelf, UserCanHide: true},
	{Key: "location", Label: "Location", Type: TypeText, Section: SectionPersonal, Visibility: VisibilityEveryone, EditableBy: EditableBySelf, UserCanHide: true},
	{Key: "tshirt_size", Label: "T-shirt size", Type: TypeSelect, Options: []Option{
		{Key: "xs", Label: "XS"}, {Key: "s", Label: "S"}, {Key: "m", Label: "M"}, {Key: "l", Label: "L"},
		{Key: "xl", Label: "XL"}, {Key: "xxl", Label: "XXL"}, {Key: "xxxl", Label: "3XL"},
	}, Section: SectionPersonal, Visibility: VisibilityAdmins, EditableBy: EditableBySelf},
	{Key: "start_date", Label: "Start date", Type: TypeDate, Section: SectionHR, Visibility: VisibilityEveryone, EditableBy: EditableByAdmins},
	{Key: "emergency_contact", Label: "Emergency contact", Type: TypeLongText, Section: SectionHR, Visibility: VisibilityAdmins, EditableBy: EditableBySelf, Sensitive: true},
}

// Library returns the library of ready-made fields, in display order.
func Library() []LibraryEntry {
	out := slices.Clone(library)
	for i := range out {
		out[i].Options = slices.Clone(out[i].Options)
	}
	return out
}

// libraryEntry returns the library entry with the given key.
func libraryEntry(key string) (LibraryEntry, bool) {
	i := slices.IndexFunc(library, func(e LibraryEntry) bool { return e.Key == key })
	if i < 0 {
		return LibraryEntry{}, false
	}
	return library[i], true
}
