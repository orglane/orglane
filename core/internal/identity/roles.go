package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/store"
)

// seedLockID serializes role seeding, role mutations and invitation grant
// checks. Concurrent starts seed and audit once; grant checks remain valid
// until their transaction commits (see LockGrantsTx).
const seedLockID int64 = 0x6f72676c726f6c65 // "orglrole"

// SeedBuiltinRoles makes the built-in roles in the database match
// authz.BuiltinRoles: missing roles are created, and the name and permission
// set of existing ones are brought in line. It is idempotent; a run that
// changes nothing writes nothing. Changes are audited with the system actor.
// The server runs it on every start, after the migrations.
func (s *Service) SeedBuiltinRoles(ctx context.Context) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if err := q.AcquireXactLock(ctx, seedLockID); err != nil {
			return fmt.Errorf("identity: seed roles: lock: %w", err)
		}
		for _, want := range authz.BuiltinRoles() {
			if err := seedRole(ctx, q, want); err != nil {
				return fmt.Errorf("identity: seed role %q: %w", want.Key, err)
			}
		}
		return nil
	})
}

func seedRole(ctx context.Context, q *store.Queries, want authz.Role) error {
	role, err := q.InsertRoleIfMissing(ctx, store.InsertRoleIfMissingParams{Key: string(want.Key), Name: want.Name, Builtin: true})
	switch {
	case err == nil:
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    audit.System,
			Action:   audit.RoleCreated,
			Target:   roleTarget(role.ID),
			Metadata: map[string]any{"key": role.Key, "name": role.Name},
		}); err != nil {
			return err
		}
	case errors.Is(err, pgx.ErrNoRows):
		if role, err = q.GetRoleByKeyForUpdate(ctx, string(want.Key)); err != nil {
			return err
		}
		if role.Name != want.Name || !role.Builtin {
			if role, err = q.UpdateRole(ctx, store.UpdateRoleParams{ID: role.ID, Name: want.Name, Builtin: true}); err != nil {
				return err
			}
			if _, err := audit.Record(ctx, q, audit.Entry{
				Actor:    audit.System,
				Action:   audit.RoleUpdated,
				Target:   roleTarget(role.ID),
				Metadata: map[string]any{"key": role.Key, "name": role.Name},
			}); err != nil {
				return err
			}
		}
	default:
		return err
	}

	have, err := q.ListRolePermissions(ctx, role.ID)
	if err != nil {
		return err
	}
	added, removed := []string{}, []string{}
	for _, p := range want.Permissions {
		if !slices.Contains(have, string(p)) {
			if err := q.AddRolePermission(ctx, store.AddRolePermissionParams{RoleID: role.ID, Permission: string(p)}); err != nil {
				return err
			}
			added = append(added, string(p))
		}
	}
	for _, p := range have {
		if !slices.Contains(want.Permissions, authz.Permission(p)) {
			if err := q.RemoveRolePermission(ctx, store.RemoveRolePermissionParams{RoleID: role.ID, Permission: p}); err != nil {
				return err
			}
			removed = append(removed, p)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    audit.System,
		Action:   audit.RolePermissionsChanged,
		Target:   roleTarget(role.ID),
		Metadata: map[string]any{"key": role.Key, "added": added, "removed": removed},
	})
	return err
}

// AssignRole grants a role to a person.
func (s *Service) AssignRole(ctx context.Context, actor audit.Actor, personID uuid.UUID, key authz.RoleKey) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if err := LockGrantsTx(ctx, q); err != nil {
			return err
		}

		role, err := q.GetRoleByKey(ctx, string(key))
		if err != nil {
			return notFound(err, "role")
		}
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		permissions, err := q.ListRolePermissions(ctx, role.ID)
		if err != nil {
			return err
		}
		if err := AuthorizeGrantTx(ctx, q, actor, personID, permissions); err != nil {
			return err
		}
		n, err := q.AssignPersonRole(ctx, store.AssignPersonRoleParams{PersonID: personID, RoleID: role.ID})
		if err != nil {
			return fmt.Errorf("identity: assign role: %w", err)
		}
		if n == 0 {
			return nil // already assigned
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PersonRoleAssigned,
			Target:   personTarget(personID),
			Metadata: map[string]any{"role": role.Key, "role_id": role.ID},
		})
		return err
	})
}

// RemoveRole takes a role away from a person. The last active owner keeps
// the owner role.
func (s *Service) RemoveRole(ctx context.Context, actor audit.Actor, personID uuid.UUID, key authz.RoleKey) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if err := LockGrantsTx(ctx, q); err != nil {
			return err
		}

		person, err := q.GetPersonForUpdate(ctx, personID)
		if err != nil {
			return notFound(err, "person")
		}
		role, err := q.GetRoleByKey(ctx, string(key))
		if err != nil {
			return notFound(err, "role")
		}
		if err := AuthorizeGrantTx(ctx, q, actor, personID, nil); err != nil {
			return err
		}
		if key == authz.RoleOwner && Status(person.Status) == StatusActive {
			if err := ensureOwnerRemains(ctx, q, person); err != nil {
				return err
			}
		}
		n, err := q.UnassignPersonRole(ctx, store.UnassignPersonRoleParams{PersonID: personID, RoleID: role.ID})
		if err != nil {
			return fmt.Errorf("identity: remove role: %w", err)
		}
		if n == 0 {
			return nil // not assigned
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.PersonRoleRemoved,
			Target:   personTarget(personID),
			Metadata: map[string]any{"role": role.Key, "role_id": role.ID},
		})
		return err
	})
}

// ensureOwnerRemains returns ErrLastOwner if person is an owner and no other
// active person holds the owner role. It locks the owner role row, so
// concurrent changes that could remove the last owner are serialized.
func ensureOwnerRemains(ctx context.Context, q *store.Queries, person store.Person) error {
	owner, err := q.GetRoleByKeyForUpdate(ctx, string(authz.RoleOwner))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // roles not seeded yet
	}
	if err != nil {
		return fmt.Errorf("identity: load owner role: %w", err)
	}
	isOwner, err := q.PersonHasRole(ctx, store.PersonHasRoleParams{PersonID: person.ID, RoleID: owner.ID})
	if err != nil || !isOwner {
		return err
	}
	others, err := q.CountActiveHoldersExcept(ctx, store.CountActiveHoldersExceptParams{RoleID: owner.ID, PersonID: person.ID})
	if err != nil {
		return fmt.Errorf("identity: count owners: %w", err)
	}
	if others == 0 {
		return ErrLastOwner
	}
	return nil
}

// ListRoles returns every role.
func (s *Service) ListRoles(ctx context.Context) ([]store.Role, error) {
	return s.queries().ListRoles(ctx)
}

// PersonRoles returns the roles a person holds.
func (s *Service) PersonRoles(ctx context.Context, personID uuid.UUID) ([]store.Role, error) {
	return s.queries().ListPersonRoles(ctx, personID)
}

// PersonPermissions resolves the permissions a person holds through their
// roles, sorted by name. Names no longer in the authz registry are dropped.
// It does not check the person's status; authentication does.
func (s *Service) PersonPermissions(ctx context.Context, personID uuid.UUID) ([]authz.Permission, error) {
	names, err := s.queries().ListPersonPermissions(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("identity: resolve permissions: %w", err)
	}
	perms := make([]authz.Permission, 0, len(names))
	for _, n := range names {
		if p := authz.Permission(n); authz.Known(p) {
			perms = append(perms, p)
		}
	}
	return perms, nil
}

func roleTarget(id uuid.UUID) audit.Target {
	return audit.Target{Kind: audit.TargetRole, ID: id.String()}
}
