package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
)

// First-run setup.
//
// A fresh instance has no owner. Until one exists, the instance can be
// claimed only with the one-time setup token: generated on first start and
// printed once to the log, or regenerated with `orglane cli setup-token`.
// Only its hash is stored (instance_settings key "setup_token"). Completing
// setup creates the first owner and deletes the token, so it cannot be used
// twice; afterwards setup is closed for good.

// SetupTokenPrefix starts every setup token.
const SetupTokenPrefix = "ost_"

const setupTokenSetting = "setup_token"

// setupLockID serializes setup token changes and setup completion.
const setupLockID int64 = 0x6f72676c73657475 // "orglsetu"

type setupTokenValue struct {
	Hash []byte `json:"hash"`
}

// SetupRequired reports whether the instance still needs its first owner:
// no active person holds the owner role.
func (s *Service) SetupRequired(ctx context.Context) (bool, error) {
	n, err := s.queries().CountActiveRoleHolders(ctx, string(authz.RoleOwner))
	if err != nil {
		return false, fmt.Errorf("identity: count owners: %w", err)
	}
	return n == 0, nil
}

// EnsureSetupToken creates a setup token if setup is required and none
// exists. It returns the new token, or "" when no token was created (setup
// is done, or a token was created earlier: it was shown then and only its
// hash is kept). The server calls it on start and logs a new token once.
func (s *Service) EnsureSetupToken(ctx context.Context) (string, error) {
	return s.newSetupToken(ctx, audit.System, false)
}

// ResetSetupToken replaces the setup token with a new one and returns it.
// It returns ErrSetupDone once an owner exists.
func (s *Service) ResetSetupToken(ctx context.Context, actor audit.Actor) (string, error) {
	return s.newSetupToken(ctx, actor, true)
}

func (s *Service) newSetupToken(ctx context.Context, actor audit.Actor, replace bool) (string, error) {
	token := crypto.RandomToken(SetupTokenPrefix)
	created := false
	err := s.inTx(ctx, func(q *store.Queries) error {
		if err := q.AcquireXactLock(ctx, setupLockID); err != nil {
			return fmt.Errorf("identity: setup lock: %w", err)
		}
		owners, err := q.CountActiveRoleHolders(ctx, string(authz.RoleOwner))
		if err != nil {
			return fmt.Errorf("identity: count owners: %w", err)
		}
		if owners > 0 {
			if replace {
				return ErrSetupDone
			}
			return nil
		}
		if !replace {
			if _, err := q.GetInstanceSetting(ctx, setupTokenSetting); err == nil {
				return nil
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("identity: load setup token: %w", err)
			}
		}
		value, err := json.Marshal(setupTokenValue{Hash: crypto.HashToken(token)})
		if err != nil {
			return err
		}
		if err := q.PutInstanceSetting(ctx, store.PutInstanceSettingParams{Key: setupTokenSetting, Value: value}); err != nil {
			return fmt.Errorf("identity: store setup token: %w", err)
		}
		created = true
		_, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.SetupTokenCreated, Metadata: map[string]any{"replaced": replace}})
		return err
	})
	if err != nil || !created {
		return "", err
	}
	return token, nil
}

// NewOwner describes the first owner created by setup.
type NewOwner struct {
	Username    string
	WorkEmail   string
	DisplayName string
	Password    string
}

// CompleteSetup creates the first owner with a password, if the setup token
// is valid and no owner exists yet, and deletes the token. actor carries the
// client's IP and User-Agent; entries are attributed to the new owner.
func (s *Service) CompleteSetup(ctx context.Context, actor audit.Actor, token string, in NewOwner) (store.Person, error) {
	params := store.CreatePersonParams{Status: string(StatusActive)}
	var err error
	if params.Username, err = normalizeUsername(in.Username); err != nil {
		return store.Person{}, err
	}
	if params.WorkEmail, err = normalizeEmail(in.WorkEmail); err != nil {
		return store.Person{}, err
	}
	if params.DisplayName, err = normalizeDisplayName(in.DisplayName); err != nil {
		return store.Person{}, err
	}
	if err := CheckPasswordPolicy(in.Password); err != nil {
		return store.Person{}, err
	}
	if !strings.HasPrefix(token, SetupTokenPrefix) {
		return store.Person{}, ErrInvalidSetupToken
	}
	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return store.Person{}, err
	}

	var person store.Person
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := q.AcquireXactLock(ctx, setupLockID); err != nil {
			return fmt.Errorf("identity: setup lock: %w", err)
		}
		owners, err := q.CountActiveRoleHolders(ctx, string(authz.RoleOwner))
		if err != nil {
			return fmt.Errorf("identity: count owners: %w", err)
		}
		if owners > 0 {
			return ErrSetupDone
		}
		raw, err := q.GetInstanceSetting(ctx, setupTokenSetting)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidSetupToken
		} else if err != nil {
			return fmt.Errorf("identity: load setup token: %w", err)
		}
		var stored setupTokenValue
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("identity: decode setup token: %w", err)
		}
		if !crypto.Equal(stored.Hash, crypto.HashToken(token)) {
			return ErrInvalidSetupToken
		}
		ownerRole, err := q.GetRoleByKey(ctx, string(authz.RoleOwner))
		if err != nil {
			return notFound(err, "owner role")
		}

		if person, err = q.CreatePerson(ctx, params); err != nil {
			return uniqueViolation(err)
		}
		owner := actor
		owner.Kind, owner.ID = audit.ActorPerson, person.ID.String()
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: owner, Action: audit.PersonCreated, Target: personTarget(person.ID),
			Metadata: map[string]any{"status": person.Status},
		}); err != nil {
			return err
		}
		if _, err := q.AssignPersonRole(ctx, store.AssignPersonRoleParams{PersonID: person.ID, RoleID: ownerRole.ID}); err != nil {
			return fmt.Errorf("identity: assign owner role: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor: owner, Action: audit.PersonRoleAssigned, Target: personTarget(person.ID),
			Metadata: map[string]any{"role": ownerRole.Key, "role_id": ownerRole.ID},
		}); err != nil {
			return err
		}
		if err := setPasswordHash(ctx, q, owner, person.ID, hash); err != nil {
			return err
		}
		if _, err := q.DeleteInstanceSetting(ctx, setupTokenSetting); err != nil {
			return fmt.Errorf("identity: delete setup token: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{Actor: owner, Action: audit.InstanceSetupCompleted, Target: personTarget(person.ID)})
		return err
	})
	if err != nil {
		return store.Person{}, err
	}
	return person, nil
}
