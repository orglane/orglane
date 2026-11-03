// Package profile is the domain layer for profile fields (ADR 0015): the
// definitions admins manage (custom fields and the library of ready-made
// fields) and the values people and admins fill in.
//
// Base fields (name, work email, title, ...) stay on the person in package
// identity; profile fields add to them. Every change runs in a transaction
// together with its audit entry (package audit), and so does every read of
// a sensitive value: a sensitive value is never returned without an entry.
//
// Who may see, change or hide which value is decided in one place, decide
// in access.go. Every read and write of values goes through it: callers get
// already filtered views (Service.Profile) and never filter themselves.
package profile

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
)

// Errors returned by Service. Validation errors wrap ErrInvalid with the
// offending field.
var (
	ErrNotFound = errors.New("profile: not found")
	ErrInvalid  = errors.New("profile: invalid input")
	// ErrForbidden: the viewer may not make this change.
	ErrForbidden = errors.New("profile: not allowed")
	// ErrKeyTaken: another field (possibly archived) has this key, or it is
	// reserved for a library field.
	ErrKeyTaken = errors.New("profile: field key is taken")
	// ErrOptionInUse: a select option to remove is someone's value.
	ErrOptionInUse = errors.New("profile: option is in use")
)

// Type is the type of a field's values.
type Type string

// Field types.
const (
	TypeText     Type = "text"
	TypeLongText Type = "long_text"
	TypeDate     Type = "date"
	TypeSelect   Type = "select"
	TypePhone    Type = "phone"
	TypeURL      Type = "url"
	TypeHandle   Type = "handle"
)

// Types lists every field type.
var Types = []Type{TypeText, TypeLongText, TypeDate, TypeSelect, TypePhone, TypeURL, TypeHandle}

// HandleKind is the service a handle belongs to.
type HandleKind string

// Handle kinds.
const (
	HandleTelegram HandleKind = "telegram"
	HandleGitHub   HandleKind = "github"
	HandleGitLab   HandleKind = "gitlab"
	HandleX        HandleKind = "x"
	HandleMastodon HandleKind = "mastodon"
)

// HandleKinds lists every handle kind.
var HandleKinds = []HandleKind{HandleTelegram, HandleGitHub, HandleGitLab, HandleX, HandleMastodon}

// Section groups fields on a profile.
type Section string

// Sections, in display order.
const (
	SectionContacts Section = "contacts"
	SectionPersonal Section = "personal"
	SectionHR       Section = "hr"
	SectionCustom   Section = "custom"
)

// Sections lists every section in display order.
var Sections = []Section{SectionContacts, SectionPersonal, SectionHR, SectionCustom}

// Visibility says who sees a field's values besides the person and admins.
type Visibility string

// Visibilities.
const (
	// VisibilityEveryone: every signed-in person.
	VisibilityEveryone Visibility = "everyone"
	// VisibilityTeam: people who share at least one team with the person.
	VisibilityTeam Visibility = "team"
	// VisibilityAdmins: nobody else.
	VisibilityAdmins Visibility = "admins"
)

// Visibilities lists every visibility.
var Visibilities = []Visibility{VisibilityEveryone, VisibilityTeam, VisibilityAdmins}

// Editor says who may change a field's values.
type Editor string

// Editors.
const (
	// EditableBySelf: the person and admins.
	EditableBySelf Editor = "self"
	// EditableByAdmins: admins only.
	EditableByAdmins Editor = "admins"
)

// Editors lists every editor setting.
var Editors = []Editor{EditableBySelf, EditableByAdmins}

// Option is a choice of a select field. The key is stored as the value.
type Option struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Field is a profile field definition.
type Field struct {
	ID  uuid.UUID
	Key string
	// Library is true for fields from the library; their key is the library
	// key, and clients translate their label by it.
	Library              bool
	Label                string
	Type                 Type
	HandleKind           HandleKind // only for TypeHandle
	Options              []Option   // only for TypeSelect
	Section              Section
	Visibility           Visibility
	RequiredAtOnboarding bool
	EditableBy           Editor
	UserCanHide          bool
	Sensitive            bool
	Position             int
	Archived             bool
	CreatedAt            time.Time
	UpdatedAt            time.Time

	archivedAt time.Time
}

// birthday reports whether the field is the library birthday, whose year
// only the person and readers of sensitive fields see.
func (f Field) birthday() bool { return f.Library && f.Key == "birthday" }

// Teams tells whether two people share a team, for VisibilityTeam. The
// identity service implements it for active team memberships. Without an
// injected resolver, NoTeams treats nobody as a teammate.
type Teams interface {
	SharesTeam(ctx context.Context, a, b uuid.UUID) (bool, error)
}

// NoTeams is a Teams without any team.
type NoTeams struct{}

// SharesTeam implements Teams.
func (NoTeams) SharesTeam(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return false, nil }

// Service manages profile field definitions and values.
//
// Row locks are taken in one order: the person, then field definitions,
// then values. Changes to a definition lock it, then its values; changes
// to a profile lock the person, then share-lock the definitions, so a
// definition cannot change (become sensitive, lose an option, be archived)
// under a profile change.
type Service struct {
	pool   *pgxpool.Pool
	sealer *crypto.Sealer
	teams  Teams
}

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// WithTeams sets how team visibility is resolved. The default is NoTeams.
func WithTeams(t Teams) ServiceOption {
	return func(s *Service) { s.teams = t }
}

// NewService returns a Service. Sensitive values are encrypted with the key
// derived from keyring for crypto.PurposeProfileFields.
func NewService(pool *pgxpool.Pool, keyring *crypto.Keyring, opts ...ServiceOption) *Service {
	s := &Service{pool: pool, sealer: keyring.Sealer(crypto.PurposeProfileFields), teams: NoTeams{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(store.New(tx))
	})
}

func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(format, args...))
}

func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return fmt.Errorf("profile: load %s: %w", what, err)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// sortFields sorts fields by section, then position.
func sortFields(fields []Field) {
	slices.SortStableFunc(fields, func(a, b Field) int {
		if d := slices.Index(Sections, a.Section) - slices.Index(Sections, b.Section); d != 0 {
			return d
		}
		return a.Position - b.Position
	})
}
