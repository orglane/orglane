// Package audit writes the append-only audit log: who did what, to what and
// from where (docs/architecture.md "Core domain model").
//
// Record must be called in the same transaction as the change it describes,
// so a change is never committed without its entry and an entry never
// outlives a rolled-back change. The table rejects UPDATE, DELETE and
// TRUNCATE (core/migrations/00003_audit_entries.sql).
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/orglane/orglane/core/internal/store"
)

// ActorKind is the kind of caller performing an action.
type ActorKind string

// Actor kinds.
const (
	// ActorPerson is a signed-in person.
	ActorPerson ActorKind = "person"
	// ActorSystem is the core itself: startup routines and background jobs.
	ActorSystem ActorKind = "system"
	// ActorModule is an installed module acting with granted scopes.
	ActorModule ActorKind = "module"
	// ActorAgent is an AI agent acting with delegated scopes (ADR 0013).
	ActorAgent ActorKind = "agent"
)

// Actor is who performed an action, and from where.
type Actor struct {
	Kind ActorKind
	// ID identifies the actor (a person ID, a module ID, ...). Required for
	// every kind except ActorSystem.
	ID string
	// IP is the client address of the request, if any.
	IP netip.Addr
	// UserAgent is the client's User-Agent header, if any. Longer values are
	// truncated.
	UserAgent string
}

// System is the actor for actions the core takes on its own.
var System = Actor{Kind: ActorSystem}

// Action names what happened, e.g. "person.created".
type Action string

// Actions. Names are "<target kind>.<past tense verb>" and never change once
// released, because entries reference them by name.
const (
	// OnboardingWelcomeFinished records finishing welcome, without marking optional steps complete.
	OnboardingWelcomeFinished Action = "person.onboarding_welcome_finished"
	PersonCreated             Action = "person.created"
	PersonUpdated             Action = "person.updated"
	PersonStatusChanged       Action = "person.status_changed"
	PersonRoleAssigned        Action = "person.role_assigned"
	PersonRoleRemoved         Action = "person.role_removed"
	// PersonAvatarSet: a person's avatar was set or replaced.
	PersonAvatarSet Action = "person.avatar_set"
	// PersonAvatarRemoved: a person's avatar was removed.
	PersonAvatarRemoved Action = "person.avatar_removed"

	// PersonProfileUpdated: values of a person's profile fields were set,
	// cleared, hidden or shown. The entry lists the field keys, never the
	// values.
	PersonProfileUpdated Action = "person.profile_updated"
	// PersonSensitiveFieldsRead: values of sensitive profile fields of a
	// person were read (ADR 0015). The entry lists the field keys.
	PersonSensitiveFieldsRead Action = "person.sensitive_fields_read"

	// ProfileFieldCreated: a profile field was defined (custom, or enabled
	// from the library).
	ProfileFieldCreated Action = "profile_field.created"
	// ProfileFieldUpdated: a profile field definition changed, including
	// archiving and restoring it. The entry lists the changed settings.
	ProfileFieldUpdated Action = "profile_field.updated"

	RoleCreated            Action = "role.created"
	RoleUpdated            Action = "role.updated"
	RolePermissionsChanged Action = "role.permissions_changed"

	DeviceRegistered Action = "device.registered"
	DeviceRevoked    Action = "device.revoked"

	SessionCreated Action = "session.created"
	SessionRevoked Action = "session.revoked"

	// InstanceSetupCompleted: the first owner was created with the setup
	// token.
	InstanceSetupCompleted Action = "instance.setup_completed"
	// SetupTokenCreated: a first-run setup token was generated (on first
	// start or with `orglane cli setup-token`).
	SetupTokenCreated Action = "setup_token.created" //nolint:gosec // an action name, not a credential

	PersonPasswordChanged Action = "person.password_changed" //nolint:gosec // an action name, not a credential

	SignInSucceeded   Action = "sign_in.succeeded"
	SignInFailed      Action = "sign_in.failed"
	SignInLocked      Action = "sign_in.locked"
	SignInRateLimited Action = "sign_in.rate_limited"

	PasskeyAdded          Action = "passkey.added"
	PasskeyRemoved        Action = "passkey.removed"
	PasskeyOfferDismissed Action = "passkey.offer_dismissed"

	SigningKeyCreated Action = "signing_key.created"
	SigningKeyRetired Action = "signing_key.retired"

	OIDCClientCreated Action = "oidc_client.created"
	// OIDCGrantRevoked: every token of an OIDC grant was revoked, for
	// example because a used refresh token or code was presented again.
	OIDCGrantRevoked Action = "oidc_grant.revoked"

	TeamCreated Action = "team.created"
	// TeamUpdated: base fields of a team changed; the entry lists the fields.
	TeamUpdated Action = "team.updated"
	// TeamArchived: a team was archived. It keeps its memberships and
	// history and becomes read-only.
	TeamArchived Action = "team.archived"
	// TeamMemberAdded: a person joined a team with a role (member or lead).
	TeamMemberAdded Action = "team.member_added"
	// TeamMemberRoleChanged: a member's role in a team changed, e.g. to lead.
	TeamMemberRoleChanged Action = "team.member_role_changed"
	// TeamMemberRemoved: a person left a team.
	TeamMemberRemoved Action = "team.member_removed"

	// InstanceDemoLoaded: the demo dataset was loaded into an empty instance
	// (demo mode).
	InstanceDemoLoaded Action = "instance.demo_loaded"

	// InvitationCreated: a person was invited (sent by email or issued as a
	// link to share).
	InvitationCreated Action = "invitation.created"
	// InvitationResent: an invitation was reissued with a new link; the
	// previous link stops working.
	InvitationResent Action = "invitation.resent"
	// InvitationRevoked: an invitation link was revoked before it was used.
	InvitationRevoked Action = "invitation.revoked"
	// InvitationAccepted: the invited person accepted and became active.
	InvitationAccepted Action = "invitation.accepted"
	// InvitationExpired: an invitation expired unused (recorded by identity
	// maintenance).
	InvitationExpired Action = "invitation.expired"

	// EmailSettingsUpdated: the SMTP settings for system email were set or
	// changed in the web UI.
	EmailSettingsUpdated Action = "email_settings.updated"
	// EmailSettingsRemoved: the SMTP settings were removed; invitations
	// produce links to share.
	EmailSettingsRemoved Action = "email_settings.removed"
	// EmailSettingsTested: a test email was queued.
	EmailSettingsTested Action = "email_settings.tested"
)

// TargetKind is the kind of object an action applies to.
type TargetKind string

// Target kinds.
const (
	TargetPerson     TargetKind = "person"
	TargetRole       TargetKind = "role"
	TargetTeam       TargetKind = "team"
	TargetDevice     TargetKind = "device"
	TargetSession    TargetKind = "session"
	TargetPasskey    TargetKind = "passkey"
	TargetSigningKey TargetKind = "signing_key"
	TargetOIDCClient TargetKind = "oidc_client"
	TargetOIDCGrant  TargetKind = "oidc_grant"
	// TargetProfileField is a profile field definition.
	TargetProfileField TargetKind = "profile_field"
	TargetInvitation   TargetKind = "invitation"
)

// Target is the object an action applies to. The zero Target means the
// action has no target.
type Target struct {
	Kind TargetKind
	ID   string
}

// Entry is one audit entry to record.
type Entry struct {
	Actor  Actor
	Action Action
	Target Target
	// Metadata holds action details. It is stored as a JSON object, so it
	// must marshal to one (a struct or a map); nil stores {}. Never put
	// secrets or sensitive profile values here.
	Metadata any
}

// MaxUserAgentLength bounds the stored User-Agent.
const MaxUserAgentLength = 1024

// ErrInvalidEntry is returned for entries that cannot be recorded.
var ErrInvalidEntry = errors.New("audit: invalid entry")

var actionName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Record writes e using q, which must be bound to the transaction of the
// change e describes.
func Record(ctx context.Context, q *store.Queries, e Entry) (store.AuditEntry, error) {
	params, err := e.params()
	if err != nil {
		return store.AuditEntry{}, err
	}
	row, err := q.InsertAuditEntry(ctx, params)
	if err != nil {
		return store.AuditEntry{}, fmt.Errorf("audit: record %s: %w", e.Action, err)
	}
	return row, nil
}

func (e Entry) params() (store.InsertAuditEntryParams, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInvalidEntry, e.Action, fmt.Sprintf(format, args...))
	}
	switch e.Actor.Kind {
	case ActorPerson, ActorModule, ActorAgent:
		if e.Actor.ID == "" {
			return store.InsertAuditEntryParams{}, invalid("actor of kind %q needs an id", e.Actor.Kind)
		}
	case ActorSystem:
	default:
		return store.InsertAuditEntryParams{}, invalid("unknown actor kind %q", e.Actor.Kind)
	}
	if !actionName.MatchString(string(e.Action)) {
		return store.InsertAuditEntryParams{}, invalid("malformed action name")
	}
	if (e.Target.Kind == "") != (e.Target.ID == "") {
		return store.InsertAuditEntryParams{}, invalid("target needs both kind and id")
	}

	metadata := []byte("{}")
	if e.Metadata != nil {
		var err error
		metadata, err = json.Marshal(e.Metadata)
		if err != nil {
			return store.InsertAuditEntryParams{}, invalid("marshal metadata: %v", err)
		}
		if string(metadata) == "null" { // a nil map or pointer
			metadata = []byte("{}")
		}
		if metadata[0] != '{' {
			return store.InsertAuditEntryParams{}, invalid("metadata must be a JSON object")
		}
	}

	p := store.InsertAuditEntryParams{
		ActorKind: string(e.Actor.Kind),
		ActorID:   optional(e.Actor.ID),
		Action:    string(e.Action),
		Metadata:  metadata,
		UserAgent: optional(sanitize(e.Actor.UserAgent, MaxUserAgentLength)),
	}
	if e.Target.Kind != "" {
		p.TargetKind = optional(string(e.Target.Kind))
		p.TargetID = optional(e.Target.ID)
	}
	if e.Actor.IP.IsValid() {
		ip := e.Actor.IP.Unmap()
		p.IP = &ip
	}
	return p, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// sanitize drops what PostgreSQL rejects in text (invalid UTF-8, NUL) and
// cuts s to at most n bytes without splitting a character.
func sanitize(s string, n int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
