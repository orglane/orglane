//go:build integration

package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/tests/auditfault"
)

// auditRollbackCase is a mutation whose audit entry fails on the database
// side.
type auditRollbackCase struct {
	name string
	// unseeded skips seeding the built-in roles.
	unseeded bool
	// prepare creates what the mutation needs and returns the mutation. It
	// must be repeatable: after the failed attempt it runs again without the
	// fault and must then change the database.
	prepare func(t *testing.T, f *rollbackFixture) func() error
	// reject selects the audit entries that can fail (the trigger's WHEN
	// condition, NEW being the entry); the nth of them in the mutation's
	// transaction fails (default 1).
	reject string
	nth    int
	// tables is how many tables the mutation changes, audit_entries
	// included (default 2).
	tables int
	// allowed are errors the mutation returns when it succeeds, for
	// mutations that record a failure (a failed sign-in).
	allowed []error
}

type rollbackFixture struct {
	ctx  context.Context
	svc  *identity.Service
	pool *pgxpool.Pool
}

// rollbackHasher keeps password hashing quick.
var rollbackHasher = crypto.NewPasswordHasher(crypto.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}, 4)

const rollbackPassword = "correct horse battery staple"

func (f *rollbackFixture) person(t *testing.T, username string, status identity.Status) uuid.UUID {
	t.Helper()
	return createPerson(t, f.svc, username, status).ID
}

// team creates the fixture's team and returns its ID.
func (f *rollbackFixture) team(t *testing.T) uuid.UUID {
	t.Helper()
	team, err := f.svc.CreateTeam(f.ctx, admin, identity.NewTeam{Slug: "platform", Name: "Platform"})
	if err != nil {
		t.Fatal(err)
	}
	return team.ID
}

// device registers a web device with n sessions and returns the device and
// session IDs.
func (f *rollbackFixture) device(t *testing.T, personID uuid.UUID, sessions int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	d, err := f.svc.RegisterDevice(f.ctx, admin, personID, identity.NewDevice{Name: "Browser", Platform: identity.PlatformWeb})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, 0, sessions)
	for range sessions {
		s, _, err := f.svc.CreateSession(f.ctx, admin, d.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	return d.ID, ids
}

func (f *rollbackFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func rejectAction(action audit.Action) string {
	return "NEW.action = '" + string(action) + "'"
}

func auditRollbackCases() []auditRollbackCase {
	return []auditRollbackCase{
		// People.
		{
			name:   "create person",
			reject: rejectAction(audit.PersonCreated),
			prepare: func(_ *testing.T, f *rollbackFixture) func() error {
				return func() error {
					_, err := f.svc.CreatePerson(f.ctx, admin, identity.NewPerson{Username: "bob", WorkEmail: "bob@acme.test", DisplayName: "Bob"})
					return err
				}
			},
		},
		{
			name:   "update person",
			reject: rejectAction(audit.PersonUpdated),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				name, title := "Alice A.", "Engineer"
				return func() error {
					_, err := f.svc.UpdatePerson(f.ctx, admin, alice, identity.PersonUpdate{DisplayName: &name, Title: &title})
					return err
				}
			},
		},
		{
			name:   "activate person",
			reject: rejectAction(audit.PersonStatusChanged),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusInvited)
				return func() error { _, err := f.svc.SetPersonStatus(f.ctx, admin, alice, identity.StatusActive); return err }
			},
		},
		{
			// The status change and its entry, the revocation of both
			// sessions and the first session's entry are written before the
			// second session's entry fails.
			name:   "suspend person",
			tables: 3,
			reject: rejectAction(audit.SessionRevoked),
			nth:    2,
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				f.device(t, alice, 2)
				return func() error {
					_, err := f.svc.SetPersonStatus(f.ctx, admin, alice, identity.StatusSuspended)
					return err
				}
			},
		},
		{
			name: "set avatar",
			// Includes the public directory timestamp invalidated by migration 00009.
			tables: 3,
			reject: rejectAction(audit.PersonAvatarSet),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				return func() error {
					return f.svc.SetAvatar(f.ctx, admin, alice, identity.Avatar{ContentType: identity.AvatarPNG, Data: []byte("\x89PNG\r\n\x1a\nrest")})
				}
			},
		},
		{
			name: "remove avatar",
			// Includes the public directory timestamp invalidated by migration 00009.
			tables: 3,
			reject: rejectAction(audit.PersonAvatarRemoved),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				if err := f.svc.SetAvatar(f.ctx, admin, alice, identity.Avatar{ContentType: identity.AvatarPNG, Data: []byte("\x89PNG\r\n\x1a\nrest")}); err != nil {
					t.Fatal(err)
				}
				return func() error { return f.svc.RemoveAvatar(f.ctx, admin, alice) }
			},
		},

		{
			name:   "upload processed avatar",
			tables: 3,
			reject: rejectAction(audit.PersonAvatarSet),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				data := rasterJPEG(t)
				return func() error { return f.svc.UploadAvatar(f.ctx, admin, alice, "image/jpeg", data) }
			},
		},
		{
			name:   "update manager",
			reject: rejectAction(audit.PersonUpdated),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				manager := f.person(t, "manager", identity.StatusActive)
				return func() error {
					_, err := f.svc.UpdatePerson(f.ctx, admin, alice, identity.PersonUpdate{ManagerID: &manager})
					return err
				}
			},
		},

		// Roles and person roles.
		{
			// The owner and admin roles, their permissions and entries are
			// written before the member role's entry fails.
			name:     "seed built-in roles",
			tables:   3,
			unseeded: true,
			reject:   rejectAction(audit.RoleCreated) + " AND NEW.metadata->>'key' = 'member'",
			prepare: func(_ *testing.T, f *rollbackFixture) func() error {
				return func() error { return f.svc.SeedBuiltinRoles(f.ctx) }
			},
		},
		{
			// Repairing a drifted role: the rename and its entry and the
			// re-added permission are written before the permission entry
			// fails.
			name:   "repair built-in roles",
			tables: 3,
			reject: rejectAction(audit.RolePermissionsChanged) + " AND NEW.metadata->>'key' = 'admin'",
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				f.exec(t, "UPDATE roles SET name = 'Administrators' WHERE key = 'admin'")
				f.exec(t, "DELETE FROM role_permissions WHERE permission = $1 AND role_id = (SELECT id FROM roles WHERE key = 'admin')", string(authz.PeopleManage))
				return func() error { return f.svc.SeedBuiltinRoles(f.ctx) }
			},
		},
		{
			name: "assign role",
			// Includes the public directory timestamp invalidated by migration 00009.
			tables: 3,
			reject: rejectAction(audit.PersonRoleAssigned),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				return func() error { return f.svc.AssignRole(f.ctx, admin, alice, authz.RoleAdmin) }
			},
		},
		{
			name: "remove role",
			// Includes the public directory timestamp invalidated by migration 00009.
			tables: 3,
			reject: rejectAction(audit.PersonRoleRemoved),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				bob := f.person(t, "bob", identity.StatusActive)
				for _, id := range []uuid.UUID{alice, bob} {
					if err := f.svc.AssignRole(f.ctx, admin, id, authz.RoleOwner); err != nil {
						t.Fatal(err)
					}
				}
				// Removing the owner role also takes the last-owner check's
				// lock on the owner role.
				return func() error { return f.svc.RemoveRole(f.ctx, admin, alice, authz.RoleOwner) }
			},
		},

		// Teams.
		{
			name:   "create team",
			reject: rejectAction(audit.TeamCreated),
			prepare: func(_ *testing.T, f *rollbackFixture) func() error {
				return func() error {
					_, err := f.svc.CreateTeam(f.ctx, admin, identity.NewTeam{Slug: "platform", Name: "Platform"})
					return err
				}
			},
		},
		{
			name:   "update team",
			reject: rejectAction(audit.TeamUpdated),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				team := f.team(t)
				name := "Platform Engineering"
				return func() error {
					_, err := f.svc.UpdateTeam(f.ctx, admin, team, identity.TeamUpdate{Name: &name})
					return err
				}
			},
		},
		{
			name:   "archive team",
			reject: rejectAction(audit.TeamArchived),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				team := f.team(t)
				return func() error { _, err := f.svc.ArchiveTeam(f.ctx, admin, team); return err }
			},
		},
		{
			name: "add team member",
			// Includes the public directory timestamp invalidated by migration 00009.
			tables: 3,
			reject: rejectAction(audit.TeamMemberAdded),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				team := f.team(t)
				alice := f.person(t, "alice", identity.StatusActive)
				return func() error {
					_, err := f.svc.SetTeamMember(f.ctx, admin, team, alice, identity.TeamRoleMember)
					return err
				}
			},
		},
		{
			name: "make team lead",
			// Includes the public directory timestamp invalidated by migration 00009.
			tables: 3,
			reject: rejectAction(audit.TeamMemberRoleChanged),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				team := f.team(t)
				alice := f.person(t, "alice", identity.StatusActive)
				if _, err := f.svc.SetTeamMember(f.ctx, admin, team, alice, identity.TeamRoleMember); err != nil {
					t.Fatal(err)
				}
				return func() error {
					_, err := f.svc.SetTeamMember(f.ctx, admin, team, alice, identity.TeamRoleLead)
					return err
				}
			},
		},
		{
			name: "remove team member",
			// Includes the public directory timestamp invalidated by migration 00009.
			tables: 3,
			reject: rejectAction(audit.TeamMemberRemoved),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				team := f.team(t)
				alice := f.person(t, "alice", identity.StatusActive)
				if _, err := f.svc.SetTeamMember(f.ctx, admin, team, alice, identity.TeamRoleLead); err != nil {
					t.Fatal(err)
				}
				return func() error { return f.svc.RemoveTeamMember(f.ctx, admin, team, alice) }
			},
		},

		// Devices.
		{
			name:   "register device",
			reject: rejectAction(audit.DeviceRegistered),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				return func() error {
					_, err := f.svc.RegisterDevice(f.ctx, admin, alice, identity.NewDevice{Name: "Phone", Platform: identity.PlatformIOS})
					return err
				}
			},
		},
		{
			// The device revocation and its entry, the revocation of both
			// sessions and the first session's entry are written before the
			// second session's entry fails.
			name:   "revoke device",
			tables: 3,
			reject: rejectAction(audit.SessionRevoked),
			nth:    2,
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				device, _ := f.device(t, alice, 2)
				return func() error { _, err := f.svc.RevokeDevice(f.ctx, admin, device); return err }
			},
		},

		// Sessions.
		{
			name:   "create session",
			reject: rejectAction(audit.SessionCreated),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				device, _ := f.device(t, alice, 0)
				return func() error { _, _, err := f.svc.CreateSession(f.ctx, admin, device, time.Hour); return err }
			},
		},
		{
			name:   "revoke session",
			reject: rejectAction(audit.SessionRevoked),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				_, sessions := f.device(t, alice, 1)
				return func() error { _, err := f.svc.RevokeSession(f.ctx, admin, sessions[0]); return err }
			},
		},
		{
			name:   "revoke own session",
			reject: rejectAction(audit.SessionRevoked),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				_, sessions := f.device(t, alice, 1)
				return func() error { return f.svc.RevokeOwnSession(f.ctx, admin, alice, sessions[0], "signed_out") }
			},
		},
		{
			// Sessions on two devices: both are revoked and the first is
			// audited before the second's entry fails.
			name:   "revoke all sessions of a person",
			reject: rejectAction(audit.SessionRevoked),
			nth:    2,
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				f.device(t, alice, 1)
				f.device(t, alice, 1)
				return func() error { _, err := f.svc.RevokePersonSessions(f.ctx, admin, alice); return err }
			},
		},
		{
			// Sign-in on a new device: the device, the session and their
			// entries are written before the sign-in entry fails.
			name:   "start session on a new device",
			tables: 3,
			reject: rejectAction(audit.SignInSucceeded),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				return func() error {
					_, err := f.svc.StartSession(f.ctx, admin, alice, identity.SignInOptions{Method: identity.AuthPassword, Platform: identity.PlatformWeb, TTL: time.Hour})
					return err
				}
			},
		},
		{
			// Sign-in on a known device: touching the device and the new
			// session roll back too.
			name:   "start session on a known device",
			tables: 3,
			reject: rejectAction(audit.SignInSucceeded),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				device, _ := f.device(t, alice, 0)
				f.exec(t, "UPDATE devices SET last_seen_at = now() - interval '1 day' WHERE id = $1", device)
				return func() error {
					_, err := f.svc.StartSession(f.ctx, admin, alice, identity.SignInOptions{Method: identity.AuthPasskey, DeviceID: device, Platform: identity.PlatformWeb, TTL: time.Hour})
					return err
				}
			},
		},

		// Sign-in state.
		{
			// The failure that locks the account: its sign_in.failed entry,
			// the failure count and the lock are written before the lock's
			// entry fails.
			name:    "failed sign-in locking the account",
			reject:  rejectAction(audit.SignInLocked),
			allowed: []error{identity.ErrInvalidCredentials},
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				if err := f.svc.SetPassword(f.ctx, admin, alice, rollbackPassword); err != nil {
					t.Fatal(err)
				}
				wrong := func() error {
					_, err := f.svc.VerifyPassword(f.ctx, admin, "alice@acme.test", "not the password at all")
					return err
				}
				// The first failure of rollbackLimits.Failures.
				if err := wrong(); !errors.Is(err, identity.ErrInvalidCredentials) {
					t.Fatalf("first failed sign-in: %v", err)
				}
				return wrong
			},
		},
		{
			name:   "rate-limited sign-in",
			reject: rejectAction(audit.SignInRateLimited),
			tables: 1,
			prepare: func(_ *testing.T, f *rollbackFixture) func() error {
				return func() error { return f.svc.RecordSignInRateLimited(f.ctx, admin, identity.AuthPassword) }
			},
		},
		{
			name:   "set password",
			reject: rejectAction(audit.PersonPasswordChanged),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				if err := f.svc.SetPassword(f.ctx, admin, alice, rollbackPassword); err != nil {
					t.Fatal(err)
				}
				return func() error { return f.svc.SetPassword(f.ctx, admin, alice, "a different long password") }
			},
		},
		{
			name:   "dismiss passkey offer",
			reject: rejectAction(audit.PasskeyOfferDismissed),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				return func() error { return f.svc.DismissPasskeyOffer(f.ctx, admin, alice) }
			},
		},
		{
			name:   "create setup token",
			reject: rejectAction(audit.SetupTokenCreated),
			prepare: func(_ *testing.T, f *rollbackFixture) func() error {
				return func() error {
					token, err := f.svc.EnsureSetupToken(f.ctx)
					if err == nil && token == "" {
						return errors.New("no setup token was created")
					}
					return err
				}
			},
		},
		{
			name:   "reset setup token",
			reject: rejectAction(audit.SetupTokenCreated),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				if _, err := f.svc.EnsureSetupToken(f.ctx); err != nil {
					t.Fatal(err)
				}
				return func() error { _, err := f.svc.ResetSetupToken(f.ctx, admin); return err }
			},
		},
		{
			// The owner, its role, its password, their entries and the
			// deletion of the setup token are written before the setup entry
			// fails.
			name:   "complete setup",
			tables: 5,
			reject: rejectAction(audit.InstanceSetupCompleted),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				token, err := f.svc.EnsureSetupToken(f.ctx)
				if err != nil || token == "" {
					t.Fatalf("EnsureSetupToken() = %q, %v", token, err)
				}
				return func() error {
					_, err := f.svc.CompleteSetup(f.ctx, admin, token, identity.NewOwner{
						Username: "owner", WorkEmail: "owner@acme.test", DisplayName: "Owner", Password: rollbackPassword,
					})
					return err
				}
			},
		},

		// Passkeys. Registering one needs a WebAuthn ceremony; package web
		// covers it.
		{
			name:   "remove passkey",
			reject: rejectAction(audit.PasskeyRemoved),
			prepare: func(t *testing.T, f *rollbackFixture) func() error {
				alice := f.person(t, "alice", identity.StatusActive)
				var passkey uuid.UUID
				if err := f.pool.QueryRow(f.ctx, `INSERT INTO passkeys (person_id, credential_id, name, credential)
					VALUES ($1, '\x0102', 'Laptop', '{}') RETURNING id`, alice).Scan(&passkey); err != nil {
					t.Fatal(err)
				}
				return func() error { return f.svc.RemovePasskey(f.ctx, admin, alice, passkey) }
			},
		},

		// OIDC clients. Grants and signing keys (package oidcop) are covered
		// in package web.
		{
			name:   "create OIDC client",
			reject: rejectAction(audit.OIDCClientCreated),
			prepare: func(_ *testing.T, f *rollbackFixture) func() error {
				return func() error {
					_, _, err := f.svc.CreateOIDCClient(f.ctx, admin, identity.NewOIDCClient{ID: "forgejo", Name: "Forgejo", RedirectURIs: []string{"https://git.acme.test/callback"}})
					return err
				}
			},
		},
	}
}

// rollbackLimits lock an account after two failures.
var rollbackLimits = identity.SignInLimits{Failures: 2, Lockout: time.Hour}

// TestAuditFailureInDatabaseRollsBackMutation makes PostgreSQL fail the
// INSERT of an audit entry (tests/auditfault) for every mutation of
// package identity and checks that the whole mutation rolls back: its own
// writes and every audit entry it wrote before the failing one. Where a
// mutation writes several entries, a later one fails, so earlier writes have
// already succeeded inside the transaction. TestChangeRollsBackWithoutAudit
// covers entries rejected before they reach the database.
func TestAuditFailureInDatabaseRollsBackMutation(t *testing.T) {
	for _, tc := range auditRollbackCases() {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := migratedPool(t)
			f := &rollbackFixture{
				ctx:  ctx,
				pool: pool,
				svc:  identity.NewService(pool, identity.WithPasswordHasher(rollbackHasher), identity.WithSignInLimits(rollbackLimits)),
			}
			if !tc.unseeded {
				if err := f.svc.SeedBuiltinRoles(ctx); err != nil {
					t.Fatal(err)
				}
			}
			op := tc.prepare(t, f)
			auditfault.AssertRollsBack(t, pool, auditfault.Mutation{
				Reject: tc.reject,
				Nth:    tc.nth,
				Tables: tc.tables,
				Run: func(faulty bool) error {
					return auditfault.Expect(faulty, op(), tc.allowed...)
				},
			})
		})
	}
}
