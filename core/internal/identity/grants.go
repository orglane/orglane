package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/store"
)

// ErrForbidden means the caller cannot manage the current or requested grant.
var ErrForbidden = errors.New("identity: insufficient grant authority")

// LockGrantsTx serializes permission definitions, caller/target role mutations,
// and invitation ceiling checks. Take it before team/person/invitation locks.
// Role seeding uses the same lock; holding only target person locks is not
// sufficient because either the caller or a role definition can change.
func LockGrantsTx(ctx context.Context, q *store.Queries) error {
	if err := q.AcquireXactLock(ctx, seedLockID); err != nil {
		return fmt.Errorf("identity: lock grants: %w", err)
	}
	return nil
}

// AuthorizeGrantTx checks a request principal against both its delegated scope
// and current database permissions, under LockGrantsTx. Internal domain callers
// without a principal remain trusted, as for other identity mutation methods.
func AuthorizeGrantTx(ctx context.Context, q *store.Queries, actor audit.Actor, target uuid.UUID, requested []string) error {
	p := authz.PrincipalFrom(ctx)
	if p == nil {
		return nil
	}
	id, err := uuid.Parse(p.Subject)
	if err != nil || actor.Kind != audit.ActorPerson || actor.ID != p.Subject {
		return ErrForbidden
	}
	person, err := q.GetPerson(ctx, id)
	if err != nil {
		return notFound(err, "caller")
	}
	if person.Status != string(StatusActive) {
		return ErrForbidden
	}
	have, err := q.ListPersonPermissions(ctx, id)
	if err != nil {
		return err
	}
	allows := func(name string) bool {
		return authz.Known(authz.Permission(name)) && p.Has(authz.Permission(name)) && slices.Contains(have, name)
	}
	if !allows(string(authz.PeopleManage)) {
		return ErrForbidden
	}
	if target != uuid.Nil {
		current, err := q.ListPersonPermissions(ctx, target)
		if err != nil {
			return err
		}
		requested = append(slices.Clone(requested), current...)
	}
	for _, name := range requested {
		if !allows(name) {
			return ErrForbidden
		}
	}
	return nil
}
