package profile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// Definition limits.
const (
	maxLabelLength = 100
	maxKeyLength   = 63
	maxOptions     = 100
)

var (
	keyPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	optionKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,62}$`)
)

// NewField describes a custom field.
type NewField struct {
	// Key identifies the field in the API (lower case, a-z, 0-9 and '_',
	// starting with a letter). Derived from Label when empty. Library keys
	// are reserved.
	Key   string
	Label string
	Type  Type
	// HandleKind is required for TypeHandle.
	HandleKind HandleKind
	// Options are required for TypeSelect; option keys are derived from
	// their labels when empty.
	Options []Option
	// Section defaults to SectionCustom, Visibility to VisibilityEveryone and
	// EditableBy to EditableBySelf.
	Section              Section
	Visibility           Visibility
	RequiredAtOnboarding bool
	EditableBy           Editor
	UserCanHide          bool
	Sensitive            bool
}

// FieldUpdate changes a field definition. Nil fields stay unchanged. The
// key and type never change; library fields also keep their label and
// options.
type FieldUpdate struct {
	Label *string
	// Options replaces the options of a select field. Options in use by a
	// value cannot be removed (ErrOptionInUse).
	Options              *[]Option
	Section              *Section
	Visibility           *Visibility
	RequiredAtOnboarding *bool
	EditableBy           *Editor
	UserCanHide          *bool
	// Sensitive moves the existing values into or out of encryption.
	Sensitive *bool
	Position  *int
	// Archived hides the field and its values everywhere; false restores
	// them.
	Archived *bool
}

// Fields returns the field definitions by section and position, without
// archived ones unless includeArchived.
func (s *Service) Fields(ctx context.Context, includeArchived bool) ([]Field, error) {
	rows, err := store.New(s.pool).ListProfileFields(ctx)
	if err != nil {
		return nil, fmt.Errorf("profile: list fields: %w", err)
	}
	out := make([]Field, 0, len(rows))
	for _, row := range rows {
		f, err := fieldFrom(row)
		if err != nil {
			return nil, err
		}
		if includeArchived || !f.Archived {
			out = append(out, f)
		}
	}
	sortFields(out)
	return out, nil
}

// CreateField defines a custom field.
func (s *Service) CreateField(ctx context.Context, actor audit.Actor, in NewField) (Field, error) {
	f := Field{
		Key: strings.TrimSpace(in.Key), Type: in.Type, HandleKind: in.HandleKind,
		Section: orDefault(in.Section, SectionCustom), Visibility: orDefault(in.Visibility, VisibilityEveryone),
		EditableBy: orDefault(in.EditableBy, EditableBySelf), RequiredAtOnboarding: in.RequiredAtOnboarding,
		UserCanHide: in.UserCanHide, Sensitive: in.Sensitive,
	}
	var err error
	if f.Label, err = normalizeLabel(in.Label); err != nil {
		return Field{}, err
	}
	if !slices.Contains(Types, f.Type) {
		return Field{}, invalid("type", "must be one of %v", Types)
	}
	switch {
	case f.Type == TypeHandle && !slices.Contains(HandleKinds, f.HandleKind):
		return Field{}, invalid("handle_kind", "must be one of %v", HandleKinds)
	case f.Type != TypeHandle && f.HandleKind != "":
		return Field{}, invalid("handle_kind", "is only for handles")
	}
	if f.Options, err = normalizeOptions(f.Type, in.Options); err != nil {
		return Field{}, err
	}
	if err := f.validateSettings(); err != nil {
		return Field{}, err
	}
	if f.Key != "" {
		if !keyPattern.MatchString(f.Key) {
			return Field{}, invalid("key", "must be 1-63 characters of a-z, 0-9 and '_', starting with a letter")
		}
		if _, ok := libraryEntry(f.Key); ok {
			return Field{}, fmt.Errorf("%w: %q is reserved for the library field", ErrKeyTaken, f.Key)
		}
	}

	err = s.inTx(ctx, func(q *store.Queries) error {
		if f.Key == "" {
			rows, err := q.ListProfileFields(ctx)
			if err != nil {
				return fmt.Errorf("profile: list fields: %w", err)
			}
			taken := make(map[string]bool, len(rows))
			for _, r := range rows {
				taken[r.Key] = true
			}
			f.Key = deriveKey(f.Label, taken)
		}
		created, err := s.insert(ctx, q, actor, f)
		f = created
		return err
	})
	if err != nil {
		return Field{}, err
	}
	return f, nil
}

// EnableLibraryField turns on a field from the library with its defaults,
// or restores it when it was archived. created is false when the field
// already existed.
func (s *Service) EnableLibraryField(ctx context.Context, actor audit.Actor, key string) (f Field, created bool, err error) {
	entry, ok := libraryEntry(key)
	if !ok {
		return Field{}, false, fmt.Errorf("%w: library field %q", ErrNotFound, key)
	}
	err = s.inTx(ctx, func(q *store.Queries) error {
		row, err := q.GetProfileFieldByKeyForUpdate(ctx, key)
		if errors.Is(err, pgx.ErrNoRows) {
			created = true
			f, err = s.insert(ctx, q, actor, Field{
				Key: entry.Key, Library: true, Label: entry.Label, Type: entry.Type, HandleKind: entry.HandleKind,
				Options: entry.Options, Section: entry.Section, Visibility: entry.Visibility, EditableBy: entry.EditableBy,
				UserCanHide: entry.UserCanHide, Sensitive: entry.Sensitive,
			})
			return err
		} else if err != nil {
			return notFound(err, "field")
		}
		current, err := fieldFrom(row)
		if err != nil {
			return err
		}
		if !current.Library {
			return fmt.Errorf("%w: %q", ErrKeyTaken, key)
		}
		restore := false
		f, err = s.update(ctx, q, actor, current, FieldUpdate{Archived: &restore})
		return err
	})
	if err != nil {
		return Field{}, false, err
	}
	return f, created, nil
}

// UpdateField changes a field definition.
func (s *Service) UpdateField(ctx context.Context, actor audit.Actor, viewer Viewer, id uuid.UUID, in FieldUpdate) (Field, error) {
	var f Field
	var validationErr error
	err := s.inTx(ctx, func(q *store.Queries) error {
		row, err := q.GetProfileFieldForUpdate(ctx, id)
		if err != nil {
			return notFound(err, "field")
		}
		current, err := fieldFrom(row)
		if err != nil {
			return err
		}
		// Changing sensitivity or removing select options reads existing
		// values. Definition management alone must not reveal or declassify
		// sensitive values.
		if current.Sensitive && !viewer.ReadSensitive && ((in.Sensitive != nil && !*in.Sensitive) || in.Options != nil) {
			return fmt.Errorf("%w: changing sensitive values requires people.sensitive.read", ErrForbidden)
		}
		f, err = s.update(ctx, q, actor, current, in)
		if errors.Is(err, ErrOptionInUse) {
			// No definition or value was changed: commit the read audits even
			// when the result is a conflict. Whether an option is in use is
			// information about a sensitive value too.
			validationErr = err
			return nil
		}
		return err
	})
	if err != nil {
		return Field{}, err
	}
	if validationErr != nil {
		return Field{}, validationErr
	}
	return f, nil
}

func (s *Service) insert(ctx context.Context, q *store.Queries, actor audit.Actor, f Field) (Field, error) {
	options, err := marshalOptions(f.Options)
	if err != nil {
		return Field{}, err
	}
	row, err := q.CreateProfileField(ctx, store.CreateProfileFieldParams{
		Key: f.Key, Library: f.Library, Label: f.Label, Type: string(f.Type), HandleKind: optional(string(f.HandleKind)),
		Options: options, Section: string(f.Section), Visibility: string(f.Visibility),
		RequiredAtOnboarding: f.RequiredAtOnboarding, EditableBy: string(f.EditableBy),
		UserCanHide: f.UserCanHide, Sensitive: f.Sensitive,
	})
	if isUniqueViolation(err) {
		return Field{}, fmt.Errorf("%w: %q", ErrKeyTaken, f.Key)
	} else if err != nil {
		return Field{}, fmt.Errorf("profile: create field: %w", err)
	}
	created, err := fieldFrom(row)
	if err != nil {
		return Field{}, err
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:  actor,
		Action: audit.ProfileFieldCreated,
		Target: fieldTarget(created.ID),
		Metadata: map[string]any{
			"key": created.Key, "library": created.Library, "type": created.Type,
			"visibility": created.Visibility, "sensitive": created.Sensitive,
		},
	})
	return created, err
}

// update applies in to current (locked by the caller) and records which
// settings changed.
func (s *Service) update(ctx context.Context, q *store.Queries, actor audit.Actor, current Field, in FieldUpdate) (Field, error) {
	next := current
	next.Options = slices.Clone(current.Options)
	var changed []string
	if in.Label != nil {
		label, err := normalizeLabel(*in.Label)
		if err != nil {
			return Field{}, err
		}
		if label != current.Label {
			if current.Library {
				return Field{}, invalid("label", "library fields keep their label")
			}
			next.Label = label
			changed = append(changed, "label")
		}
	}
	var removedOptions []string
	if in.Options != nil {
		options, err := normalizeOptions(current.Type, *in.Options)
		if err != nil {
			return Field{}, err
		}
		if !slices.Equal(options, current.Options) {
			if current.Library {
				return Field{}, invalid("options", "library fields keep their options")
			}
			for _, o := range current.Options {
				if !slices.ContainsFunc(options, func(n Option) bool { return n.Key == o.Key }) {
					removedOptions = append(removedOptions, o.Key)
				}
			}
			next.Options = options
			changed = append(changed, "options")
		}
	}
	set := func(name string, dst *bool, v *bool) {
		if v != nil && *v != *dst {
			*dst = *v
			changed = append(changed, name)
		}
	}
	if in.Section != nil && *in.Section != current.Section {
		next.Section = *in.Section
		changed = append(changed, "section")
	}
	if in.Visibility != nil && *in.Visibility != current.Visibility {
		next.Visibility = *in.Visibility
		changed = append(changed, "visibility")
	}
	if in.EditableBy != nil && *in.EditableBy != current.EditableBy {
		next.EditableBy = *in.EditableBy
		changed = append(changed, "editable_by")
	}
	set("required_at_onboarding", &next.RequiredAtOnboarding, in.RequiredAtOnboarding)
	set("user_can_hide", &next.UserCanHide, in.UserCanHide)
	set("sensitive", &next.Sensitive, in.Sensitive)
	set("archived", &next.Archived, in.Archived)
	if in.Position != nil && *in.Position != current.Position {
		if *in.Position < 0 || *in.Position > 1_000_000 {
			return Field{}, invalid("position", "must be between 0 and 1000000")
		}
		next.Position = *in.Position
		changed = append(changed, "position")
	}
	if err := next.validateSettings(); err != nil {
		return Field{}, err
	}
	if len(changed) == 0 {
		return current, nil
	}

	if next.Sensitive != current.Sensitive || len(removedOptions) > 0 {
		if err := s.rewriteValues(ctx, q, actor, current, next, removedOptions); err != nil {
			return Field{}, err
		}
	}
	options, err := marshalOptions(next.Options)
	if err != nil {
		return Field{}, err
	}
	params := store.UpdateProfileFieldParams{
		ID: current.ID, Label: next.Label, Options: options, Section: string(next.Section),
		Visibility: string(next.Visibility), RequiredAtOnboarding: next.RequiredAtOnboarding,
		EditableBy: string(next.EditableBy), UserCanHide: next.UserCanHide, Sensitive: next.Sensitive,
		Position: int32(next.Position), //nolint:gosec // bounded above
	}
	if next.Archived {
		archivedAt := time.Now().UTC()
		if current.Archived {
			archivedAt = current.archivedAt
		}
		params.ArchivedAt = &archivedAt
	}
	row, err := q.UpdateProfileField(ctx, params)
	if err != nil {
		return Field{}, fmt.Errorf("profile: update field: %w", err)
	}
	updated, err := fieldFrom(row)
	if err != nil {
		return Field{}, err
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    actor,
		Action:   audit.ProfileFieldUpdated,
		Target:   fieldTarget(current.ID),
		Metadata: map[string]any{"key": current.Key, "changes": changed},
	})
	return updated, err
}

// rewriteValues moves the field's values into or out of encryption when its
// sensitivity changes, and refuses to remove options that are in use. The
// field row is locked by the caller, so no value changes meanwhile.
func (s *Service) rewriteValues(ctx context.Context, q *store.Queries, actor audit.Actor, current, next Field, removedOptions []string) error {
	rows, err := q.ListValuesOfFieldForUpdate(ctx, current.ID)
	if err != nil {
		return fmt.Errorf("profile: load values: %w", err)
	}
	values := make([]string, len(rows))
	optionInUse := false
	for i, row := range rows {
		value, err := s.open(row)
		if err != nil {
			return err
		}
		values[i] = value
		if current.Sensitive {
			if _, err := audit.Record(ctx, q, audit.Entry{
				Actor: actor, Action: audit.PersonSensitiveFieldsRead,
				Target:   audit.Target{Kind: audit.TargetPerson, ID: row.PersonID.String()},
				Metadata: map[string]any{"fields": []string{current.Key}},
			}); err != nil {
				return err
			}
		}
		optionInUse = optionInUse || slices.Contains(removedOptions, value)
	}
	// Validate every value before writing any of them. UpdateField can then
	// commit just the read audits when an option is in use.
	if optionInUse {
		return ErrOptionInUse
	}
	if next.Sensitive != current.Sensitive {
		for i, row := range rows {
			if err := s.put(ctx, q, next, row.PersonID, values[i], row.Hidden); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f Field) validateSettings() error {
	switch {
	case !slices.Contains(Sections, f.Section):
		return invalid("section", "must be one of %v", Sections)
	case !slices.Contains(Visibilities, f.Visibility):
		return invalid("visibility", "must be one of %v", Visibilities)
	case !slices.Contains(Editors, f.EditableBy):
		return invalid("editable_by", "must be one of %v", Editors)
	}
	return nil
}

func normalizeLabel(l string) (string, error) {
	l = strings.TrimSpace(l)
	if l == "" || utf8.RuneCountInString(l) > maxLabelLength || hasControl(l, false) {
		return "", invalid("label", "must be 1-%d characters on one line", maxLabelLength)
	}
	return l, nil
}

// normalizeOptions checks the options of a select field and derives
// missing keys from labels. Other types have no options.
func normalizeOptions(t Type, in []Option) ([]Option, error) {
	if t != TypeSelect {
		if len(in) > 0 {
			return nil, invalid("options", "are only for select fields")
		}
		return []Option{}, nil
	}
	if len(in) == 0 || len(in) > maxOptions {
		return nil, invalid("options", "a select field needs 1-%d options", maxOptions)
	}
	out := make([]Option, 0, len(in))
	taken := make(map[string]bool, len(in))
	for _, o := range in {
		label, err := normalizeLabel(o.Label)
		if err != nil {
			return nil, invalid("options", "every option needs a label of 1-%d characters", maxLabelLength)
		}
		key := strings.TrimSpace(o.Key)
		if key == "" {
			key = deriveOptionKey(label, taken)
		}
		if !optionKeyPattern.MatchString(key) {
			return nil, invalid("options", "option keys are 1-63 characters of a-z, 0-9 and '_'")
		}
		if taken[key] {
			return nil, invalid("options", "option key %q is used twice", key)
		}
		taken[key] = true
		out = append(out, Option{Key: key, Label: label})
	}
	return out, nil
}

// slug turns a label into a-z, 0-9 and '_' (other characters become '_').
func slug(label string) string {
	var b strings.Builder
	underscore := false
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			underscore = false
		} else if !underscore && b.Len() > 0 {
			b.WriteByte('_')
			underscore = true
		}
	}
	return strings.TrimRight(b.String(), "_")
}

// deriveKey makes a free field key from a label: "Office location" →
// office_location, office_location_2, ...
func deriveKey(label string, taken map[string]bool) string {
	base := slug(label)
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = strings.TrimRight("field_"+base, "_")
	}
	return unique(base, func(k string) bool {
		_, reserved := libraryEntry(k)
		return taken[k] || reserved
	})
}

func deriveOptionKey(label string, taken map[string]bool) string {
	base := slug(label)
	if base == "" {
		base = "option"
	}
	return unique(base, func(k string) bool { return taken[k] })
}

func unique(base string, taken func(string) bool) string {
	if len(base) > maxKeyLength {
		base = strings.TrimRight(base[:maxKeyLength], "_")
	}
	for i := 1; ; i++ {
		k := base
		if i > 1 {
			suffix := "_" + strconv.Itoa(i)
			k = strings.TrimRight(base[:min(len(base), maxKeyLength-len(suffix))], "_") + suffix
		}
		if !taken(k) {
			return k
		}
	}
}

func orDefault[T ~string](v, def T) T {
	if v == "" {
		return def
	}
	return v
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func fieldTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetProfileField, ID: id.String()}
}

// marshalOptions stores options as a JSON array, empty for none.
func marshalOptions(options []Option) ([]byte, error) {
	if options == nil {
		options = []Option{}
	}
	return json.Marshal(options)
}

func fieldFrom(row store.ProfileField) (Field, error) {
	f := Field{
		ID: row.ID, Key: row.Key, Library: row.Library, Label: row.Label, Type: Type(row.Type),
		Section: Section(row.Section), Visibility: Visibility(row.Visibility),
		RequiredAtOnboarding: row.RequiredAtOnboarding, EditableBy: Editor(row.EditableBy),
		UserCanHide: row.UserCanHide, Sensitive: row.Sensitive, Position: int(row.Position),
		Archived: row.ArchivedAt != nil, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.ArchivedAt != nil {
		f.archivedAt = *row.ArchivedAt
	}
	if row.HandleKind != nil {
		f.HandleKind = HandleKind(*row.HandleKind)
	}
	f.Options = []Option{}
	if err := json.Unmarshal(row.Options, &f.Options); err != nil {
		return Field{}, fmt.Errorf("profile: field %s: options: %w", row.Key, err)
	}
	return f, nil
}
