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
	PersonCreated       Action = "person.created"
	PersonUpdated       Action = "person.updated"
	PersonStatusChanged Action = "person.status_changed"
	PersonRoleAssigned  Action = "person.role_assigned"
	PersonRoleRemoved   Action = "person.role_removed"

	RoleCreated            Action = "role.created"
	RoleUpdated            Action = "role.updated"
	RolePermissionsChanged Action = "role.permissions_changed"

	DeviceRegistered Action = "device.registered"
	DeviceRevoked    Action = "device.revoked"

	SessionCreated Action = "session.created"
	SessionRevoked Action = "session.revoked"
)

// TargetKind is the kind of object an action applies to.
type TargetKind string

// Target kinds.
const (
	TargetPerson  TargetKind = "person"
	TargetRole    TargetKind = "role"
	TargetDevice  TargetKind = "device"
	TargetSession TargetKind = "session"
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
