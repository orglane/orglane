//go:build integration

package identity_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/dbtest"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/tests/pgtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// migratedPool returns a pool on a fresh, fully migrated database without
// seeded roles.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := database.Connect(ctx, config.Database{URL: pgtest.NewDatabase(t), MaxConns: 10, ConnectTimeout: 10 * time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	m, err := database.NewMigrator(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

// newService returns a service on a migrated database with seeded roles.
func newService(t *testing.T) (*identity.Service, *pgxpool.Pool) {
	t.Helper()
	pool := migratedPool(t)
	svc := identity.NewService(pool)
	if err := svc.SeedBuiltinRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	return svc, pool
}

var admin = audit.Actor{
	Kind:      audit.ActorPerson,
	ID:        "0190f3c4-0000-7000-8000-000000000001",
	IP:        netip.MustParseAddr("203.0.113.7"),
	UserAgent: "orglane-test/1.0",
}

func createPerson(t *testing.T, svc *identity.Service, username string, status identity.Status) store.Person {
	t.Helper()
	p, err := svc.CreatePerson(context.Background(), admin, identity.NewPerson{
		Username: username, WorkEmail: username + "@acme.test", DisplayName: "Person " + username, Status: status,
	})
	if err != nil {
		t.Fatalf("CreatePerson(%s): %v", username, err)
	}
	return p
}

// entry is the part of an audit entry the tests compare.
type entry struct {
	ActorKind, ActorID, Action, TargetKind, TargetID string
}

// auditLog returns every audit entry after the given one (nil for all), in
// insertion order.
func auditLog(t *testing.T, pool *pgxpool.Pool, after *uuid.UUID) []store.AuditEntry {
	t.Helper()
	cutoff := uuid.Nil
	if after != nil {
		cutoff = *after
	}
	rows, err := pool.Query(context.Background(), `
		SELECT id, occurred_at, actor_kind, actor_id, action, target_kind, target_id, metadata, ip, user_agent
		FROM audit_entries WHERE id > $1 ORDER BY id`, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var entries []store.AuditEntry
	for rows.Next() {
		var e store.AuditEntry
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.ActorKind, &e.ActorID, &e.Action, &e.TargetKind, &e.TargetID, &e.Metadata, &e.IP, &e.UserAgent); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func lastAuditID(t *testing.T, pool *pgxpool.Pool) *uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), "SELECT coalesce((SELECT id FROM audit_entries ORDER BY id DESC LIMIT 1), '00000000-0000-0000-0000-000000000000')").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return &id
}

func summarize(entries []store.AuditEntry) []entry {
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entry{e.ActorKind, deref(e.ActorID), e.Action, deref(e.TargetKind), deref(e.TargetID)})
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ---------------------------------------------------------------------------
// Roles

func TestSeedBuiltinRoles(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	svc := identity.NewService(pool)

	if err := svc.SeedBuiltinRoles(ctx); err != nil {
		t.Fatal(err)
	}
	assertRolesMatchRegistry(t, svc, pool)
	first := auditLog(t, pool, nil)
	var actions []string
	for _, e := range first {
		if e.ActorKind != "system" || e.ActorID != nil {
			t.Errorf("seeding entry %s has actor %s/%v, want system", e.Action, e.ActorKind, e.ActorID)
		}
		actions = append(actions, e.Action)
	}
	// Three roles created; owner and admin get permissions, member has none.
	want := []string{"role.created", "role.permissions_changed", "role.created", "role.permissions_changed", "role.created"}
	if !slices.Equal(actions, want) {
		t.Fatalf("first seed audit = %v, want %v", actions, want)
	}

	// Idempotent: a second run changes and records nothing.
	if err := svc.SeedBuiltinRoles(ctx); err != nil {
		t.Fatal(err)
	}
	assertRolesMatchRegistry(t, svc, pool)
	if got := auditLog(t, pool, &first[len(first)-1].ID); len(got) != 0 {
		t.Fatalf("second seed wrote %d audit entries, want none", len(got))
	}
	if n := count(t, pool, "SELECT count(*) FROM roles"); n != 3 {
		t.Fatalf("roles = %d after two seeds, want 3", n)
	}

	// Drift is repaired and audited.
	mark := lastAuditID(t, pool)
	if _, err := pool.Exec(ctx, `
		DELETE FROM role_permissions WHERE permission = 'people.manage' AND role_id = (SELECT id FROM roles WHERE key = 'admin');
		INSERT INTO role_permissions (role_id, permission) SELECT id, 'billing.manage' FROM roles WHERE key = 'admin';
		UPDATE roles SET name = 'Boss', builtin = false WHERE key = 'owner';`); err != nil {
		t.Fatal(err)
	}
	if err := svc.SeedBuiltinRoles(ctx); err != nil {
		t.Fatal(err)
	}
	assertRolesMatchRegistry(t, svc, pool)
	repaired := auditLog(t, pool, mark)
	if got := summarizeActions(repaired); !slices.Equal(got, []string{"role.updated", "role.permissions_changed"}) {
		t.Fatalf("repair audit = %v", got)
	}
	var meta struct{ Added, Removed []string }
	if err := json.Unmarshal(repaired[1].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(meta.Added, []string{"people.manage"}) || !slices.Equal(meta.Removed, []string{"billing.manage"}) {
		t.Fatalf("permissions_changed metadata = %s", repaired[1].Metadata)
	}
}

func TestSeedBuiltinRolesConcurrently(t *testing.T) {
	pool := migratedPool(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { errs[i] = identity.NewService(pool).SeedBuiltinRoles(context.Background()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("seed %d: %v", i, err)
		}
	}
	if n := count(t, pool, "SELECT count(*) FROM audit_entries WHERE action = 'role.created'"); n != 3 {
		t.Fatalf("role.created entries = %d after concurrent seeding, want 3", n)
	}
}

func summarizeActions(entries []store.AuditEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Action)
	}
	return out
}

func assertRolesMatchRegistry(t *testing.T, svc *identity.Service, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	roles, err := svc.ListRoles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]store.Role{}
	for _, r := range roles {
		byKey[r.Key] = r
	}
	for _, want := range authz.BuiltinRoles() {
		r, ok := byKey[string(want.Key)]
		if !ok {
			t.Errorf("role %q missing", want.Key)
			continue
		}
		if r.Name != want.Name || !r.Builtin {
			t.Errorf("role %q = %+v", want.Key, r)
		}
		var got []string
		if err := pool.QueryRow(ctx, "SELECT coalesce(array_agg(permission ORDER BY permission), '{}') FROM role_permissions WHERE role_id = $1", r.ID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		var wantPerms []string
		for _, p := range want.Permissions {
			wantPerms = append(wantPerms, string(p))
		}
		slices.Sort(wantPerms)
		if !slices.Equal(got, wantPerms) && (len(got) != 0 || len(wantPerms) != 0) {
			t.Errorf("role %q permissions = %v, want %v", want.Key, got, wantPerms)
		}
	}
}

func TestPersonPermissions(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	owner := createPerson(t, svc, "owner", identity.StatusActive)
	adminP := createPerson(t, svc, "admin", identity.StatusActive)
	member := createPerson(t, svc, "member", identity.StatusActive)
	nobody := createPerson(t, svc, "nobody", identity.StatusActive)
	for p, key := range map[uuid.UUID]authz.RoleKey{owner.ID: authz.RoleOwner, adminP.ID: authz.RoleAdmin, member.ID: authz.RoleMember} {
		if err := svc.AssignRole(ctx, admin, p, key); err != nil {
			t.Fatal(err)
		}
	}
	// Admin also holds member: permissions are the union, without duplicates.
	if err := svc.AssignRole(ctx, admin, adminP.ID, authz.RoleMember); err != nil {
		t.Fatal(err)
	}
	// A permission name no longer in the registry is ignored.
	if _, err := pool.Exec(ctx, "INSERT INTO role_permissions (role_id, permission) SELECT id, 'legacy.removed' FROM roles WHERE key = 'member'"); err != nil {
		t.Fatal(err)
	}

	sorted := func(ps []authz.Permission) []authz.Permission {
		ps = slices.Clone(ps)
		slices.Sort(ps)
		return ps
	}
	ownerRole, _ := authz.BuiltinRole(authz.RoleOwner)
	adminRole, _ := authz.BuiltinRole(authz.RoleAdmin)
	for _, tc := range []struct {
		person uuid.UUID
		want   []authz.Permission
	}{
		{owner.ID, sorted(ownerRole.Permissions)},
		{adminP.ID, sorted(adminRole.Permissions)},
		{member.ID, nil},
		{nobody.ID, nil},
	} {
		got, err := svc.PersonPermissions(ctx, tc.person)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
			t.Errorf("PersonPermissions(%s) = %v, want %v", tc.person, got, tc.want)
		}
	}
}

func TestAssignUnknownRole(t *testing.T) {
	svc, _ := newService(t)
	p := createPerson(t, svc, "alice", identity.StatusActive)
	if err := svc.AssignRole(context.Background(), admin, p.ID, "root"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("AssignRole(root) error = %v, want ErrNotFound", err)
	}
	if err := svc.AssignRole(context.Background(), admin, uuid.New(), authz.RoleMember); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("AssignRole(unknown person) error = %v, want ErrNotFound", err)
	}
}

func TestLastOwnerIsKept(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	bob := createPerson(t, svc, "bob", identity.StatusActive)
	for _, p := range []uuid.UUID{alice.ID, bob.ID} {
		if err := svc.AssignRole(ctx, admin, p, authz.RoleOwner); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.RemoveRole(ctx, admin, bob.ID, authz.RoleOwner); err != nil {
		t.Fatalf("removing one of two owners: %v", err)
	}
	if err := svc.RemoveRole(ctx, admin, alice.ID, authz.RoleOwner); !errors.Is(err, identity.ErrLastOwner) {
		t.Fatalf("removing the last owner: error = %v, want ErrLastOwner", err)
	}
	for _, to := range []identity.Status{identity.StatusSuspended, identity.StatusOffboarded} {
		if _, err := svc.SetPersonStatus(ctx, admin, alice.ID, to); !errors.Is(err, identity.ErrLastOwner) {
			t.Fatalf("%s the last owner: error = %v, want ErrLastOwner", to, err)
		}
	}
	// With a second active owner, the first can go.
	if err := svc.AssignRole(ctx, admin, bob.ID, authz.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPersonStatus(ctx, admin, alice.ID, identity.StatusSuspended); err != nil {
		t.Fatalf("suspending one of two owners: %v", err)
	}
	// A suspended owner does not count: bob is now the last active owner.
	if err := svc.RemoveRole(ctx, admin, bob.ID, authz.RoleOwner); !errors.Is(err, identity.ErrLastOwner) {
		t.Fatalf("removing the last active owner: error = %v, want ErrLastOwner", err)
	}
	// Removing the role from a suspended owner is fine.
	if err := svc.RemoveRole(ctx, admin, alice.ID, authz.RoleOwner); err != nil {
		t.Fatalf("removing owner from a suspended person: %v", err)
	}
}

func TestLastOwnerConcurrentRemoval(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	bob := createPerson(t, svc, "bob", identity.StatusActive)
	for _, p := range []uuid.UUID{alice.ID, bob.ID} {
		if err := svc.AssignRole(ctx, admin, p, authz.RoleOwner); err != nil {
			t.Fatal(err)
		}
	}
	for range 10 {
		var wg sync.WaitGroup
		var errA, errB error
		wg.Go(func() { _, errA = svc.SetPersonStatus(ctx, admin, alice.ID, identity.StatusSuspended) })
		wg.Go(func() { _, errB = svc.SetPersonStatus(ctx, admin, bob.ID, identity.StatusSuspended) })
		wg.Wait()
		if (errA == nil) == (errB == nil) {
			t.Fatalf("concurrent suspension of both owners: errors %v and %v, want exactly one ErrLastOwner", errA, errB)
		}
		for _, p := range []uuid.UUID{alice.ID, bob.ID} {
			if _, err := svc.SetPersonStatus(ctx, admin, p, identity.StatusActive); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// People

func TestWorkEmailUniqueCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	createPerson(t, svc, "alice", identity.StatusInvited) // alice@acme.test

	_, err := svc.CreatePerson(ctx, admin, identity.NewPerson{Username: "alice2", WorkEmail: "  ALICE@Acme.Test ", DisplayName: "Alice"})
	if !errors.Is(err, identity.ErrEmailTaken) {
		t.Fatalf("CreatePerson with the same email in another case: error = %v, want ErrEmailTaken", err)
	}
	// The database enforces it too, not only the service.
	_, err = pool.Exec(ctx, "INSERT INTO people (username, work_email, display_name) VALUES ('alice3', 'Alice@ACME.test', 'Alice')")
	if pgCode(err) != "23505" {
		t.Fatalf("direct insert of a duplicate email: error = %v, want unique violation", err)
	}

	_, err = svc.CreatePerson(ctx, admin, identity.NewPerson{Username: "ALICE", WorkEmail: "other@acme.test", DisplayName: "Alice"})
	if !errors.Is(err, identity.ErrUsernameTaken) {
		t.Fatalf("CreatePerson with a taken username: error = %v, want ErrUsernameTaken", err)
	}

	bob := createPerson(t, svc, "bob", identity.StatusInvited)
	email := "ALICE@acme.test"
	if _, err := svc.UpdatePerson(ctx, admin, bob.ID, identity.PersonUpdate{WorkEmail: &email}); !errors.Is(err, identity.ErrEmailTaken) {
		t.Fatalf("UpdatePerson to a taken email: error = %v, want ErrEmailTaken", err)
	}

	got, err := svc.GetPersonByWorkEmail(ctx, "Alice@Acme.TEST")
	if err != nil || got.Username != "alice" {
		t.Fatalf("GetPersonByWorkEmail() = %+v, %v", got, err)
	}
	if n := count(t, pool, "SELECT count(*) FROM audit_entries WHERE action = 'person.created'"); n != 2 {
		t.Fatalf("person.created entries = %d, want 2 (failed creations are not audited)", n)
	}
}

func TestCreatePersonValidation(t *testing.T) {
	svc, pool := newService(t)
	valid := identity.NewPerson{Username: "alice", WorkEmail: "alice@acme.test", DisplayName: "Alice"}
	tests := map[string]func(p *identity.NewPerson){
		"empty username":         func(p *identity.NewPerson) { p.Username = "" },
		"username with space":    func(p *identity.NewPerson) { p.Username = "al ice" },
		"username starts with .": func(p *identity.NewPerson) { p.Username = ".alice" },
		"email without @":        func(p *identity.NewPerson) { p.WorkEmail = "alice" },
		"email with name":        func(p *identity.NewPerson) { p.WorkEmail = "Alice <alice@acme.test>" },
		"email with quoted @":    func(p *identity.NewPerson) { p.WorkEmail = `"a@b"@acme.test` },
		"blank display name":     func(p *identity.NewPerson) { p.DisplayName = "   " },
		"offboarded status":      func(p *identity.NewPerson) { p.Status = identity.StatusOffboarded },
		"unknown status":         func(p *identity.NewPerson) { p.Status = "deleted" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := valid
			mutate(&p)
			if _, err := svc.CreatePerson(context.Background(), admin, p); !errors.Is(err, identity.ErrInvalid) {
				t.Fatalf("CreatePerson() error = %v, want ErrInvalid", err)
			}
		})
	}
	if n := count(t, pool, "SELECT count(*) FROM people"); n != 0 {
		t.Fatalf("people = %d after invalid input, want 0", n)
	}

	p, err := svc.CreatePerson(context.Background(), admin, identity.NewPerson{Username: " Alice.Smith ", WorkEmail: " Alice@Acme.test ", DisplayName: " Alice Smith "})
	if err != nil {
		t.Fatal(err)
	}
	if p.Username != "alice.smith" || p.WorkEmail != "Alice@Acme.test" || p.DisplayName != "Alice Smith" || p.Status != "invited" {
		t.Fatalf("normalized person = %+v", p)
	}
	if p.ID.Version() != 7 {
		t.Errorf("person id %s is UUID version %d, want 7", p.ID, p.ID.Version())
	}
	if p.CreatedAt.Location() != time.UTC || p.UpdatedAt.Location() != time.UTC {
		t.Errorf("timestamps are in %v/%v, want UTC", p.CreatedAt.Location(), p.UpdatedAt.Location())
	}
}

func TestStatusTransitions(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	p := createPerson(t, svc, "alice", identity.StatusInvited)
	steps := []struct {
		to      identity.Status
		wantErr error
	}{
		{identity.StatusSuspended, identity.ErrInvalidTransition}, // invited cannot be suspended
		{identity.StatusActive, nil},
		{identity.StatusInvited, identity.ErrInvalidTransition},
		{identity.StatusSuspended, nil},
		{identity.StatusActive, nil},
		{identity.StatusOffboarded, nil},
		{identity.StatusActive, identity.ErrInvalidTransition}, // offboarded is final
		{"deleted", identity.ErrInvalid},
	}
	for _, s := range steps {
		_, err := svc.SetPersonStatus(ctx, admin, p.ID, s.to)
		if !errors.Is(err, s.wantErr) {
			t.Fatalf("SetPersonStatus(%s) error = %v, want %v", s.to, err, s.wantErr)
		}
	}
	if _, err := svc.SetPersonStatus(ctx, admin, uuid.New(), identity.StatusActive); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("SetPersonStatus(unknown) error = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Devices and sessions

func TestSessions(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	device, err := svc.RegisterDevice(ctx, admin, alice.ID, identity.NewDevice{Name: "Firefox on macOS", Platform: identity.PlatformWeb})
	if err != nil {
		t.Fatal(err)
	}

	session, token, err := svc.CreateSession(ctx, admin, device.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 40 || token[:len(identity.SessionTokenPrefix)] != identity.SessionTokenPrefix {
		t.Fatalf("token %q has the wrong shape", token)
	}
	if session.PersonID != alice.ID || session.DeviceID != device.ID {
		t.Fatalf("session = %+v", session)
	}
	if d := session.ExpiresAt.Sub(session.CreatedAt); d != time.Hour {
		t.Fatalf("session lifetime = %s, want 1h", d)
	}

	// Only the SHA-256 of the token is stored; the token appears nowhere.
	sum := sha256.Sum256([]byte(token))
	var stored []byte
	if err := pool.QueryRow(ctx, "SELECT token_hash FROM sessions WHERE id = $1", session.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stored, sum[:]) {
		t.Fatal("stored token hash is not the SHA-256 of the token")
	}
	if n := count(t, pool, `SELECT count(*) FROM sessions s WHERE position(convert_to($1, 'UTF8') IN s.token_hash) > 0 OR s::text LIKE '%' || $1 || '%'`, token); n != 0 {
		t.Fatal("the raw token is stored in sessions")
	}
	if n := count(t, pool, `SELECT count(*) FROM audit_entries WHERE metadata::text LIKE '%' || $1 || '%'`, token); n != 0 {
		t.Fatal("the raw token is stored in the audit log")
	}

	got, err := svc.SessionByToken(ctx, token)
	if err != nil || got.ID != session.ID {
		t.Fatalf("SessionByToken() = %+v, %v", got, err)
	}
	if _, err := svc.SessionByToken(ctx, token+"x"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("SessionByToken(wrong) error = %v, want ErrNotFound", err)
	}

	// Revoked session.
	if _, err := svc.RevokeSession(ctx, admin, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SessionByToken(ctx, token); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("SessionByToken(revoked) error = %v, want ErrNotFound", err)
	}

	// Expired session.
	_, short, err := svc.CreateSession(ctx, admin, device.ID, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := svc.SessionByToken(ctx, short); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("SessionByToken(expired) error = %v, want ErrNotFound", err)
	}

	// Revoking the device revokes its sessions and blocks new ones.
	_, onDevice, err := svc.CreateSession(ctx, admin, device.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := svc.RevokeDevice(ctx, admin, device.ID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("RevokeDevice() = %+v, %v", revoked, err)
	}
	if _, err := svc.SessionByToken(ctx, onDevice); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("SessionByToken(on revoked device) error = %v, want ErrNotFound", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions WHERE device_id = $1 AND revoked_at IS NULL", device.ID); n != 0 {
		t.Fatalf("%d sessions of a revoked device are still active", n)
	}
	if _, _, err := svc.CreateSession(ctx, admin, device.ID, time.Hour); !errors.Is(err, identity.ErrRevoked) {
		t.Fatalf("CreateSession(revoked device) error = %v, want ErrRevoked", err)
	}

	// Suspending the person revokes their sessions and blocks new ones.
	phone, err := svc.RegisterDevice(ctx, admin, alice.ID, identity.NewDevice{Name: "iPhone", Platform: identity.PlatformIOS})
	if err != nil {
		t.Fatal(err)
	}
	_, onPhone, err := svc.CreateSession(ctx, admin, phone.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPersonStatus(ctx, admin, alice.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SessionByToken(ctx, onPhone); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("SessionByToken(suspended person) error = %v, want ErrNotFound", err)
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions WHERE person_id = $1 AND revoked_at IS NULL", alice.ID); n != 0 {
		t.Fatalf("%d sessions of a suspended person are still active", n)
	}
	if _, _, err := svc.CreateSession(ctx, admin, phone.ID, time.Hour); !errors.Is(err, identity.ErrInactive) {
		t.Fatalf("CreateSession(suspended person) error = %v, want ErrInactive", err)
	}
	if _, err := svc.RegisterDevice(ctx, admin, alice.ID, identity.NewDevice{Name: "Pixel", Platform: identity.PlatformAndroid}); !errors.Is(err, identity.ErrInactive) {
		t.Fatalf("RegisterDevice(suspended person) error = %v, want ErrInactive", err)
	}

	sessions, err := svc.PersonSessions(ctx, alice.ID)
	if err != nil || len(sessions) != 4 {
		t.Fatalf("PersonSessions() = %d sessions, %v; want 4", len(sessions), err)
	}
	for _, s := range sessions {
		if s.CreatedAt.Location() != time.UTC || s.ExpiresAt.Location() != time.UTC || (s.RevokedAt != nil && s.RevokedAt.Location() != time.UTC) {
			t.Errorf("session %s timestamps are not UTC", s.ID)
		}
	}
}

func TestSessionsRejectInvalidInput(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusInvited)
	device, err := svc.RegisterDevice(ctx, admin, alice.ID, identity.NewDevice{Name: "Browser", Platform: identity.PlatformWeb})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateSession(ctx, admin, device.ID, time.Hour); !errors.Is(err, identity.ErrInactive) {
		t.Fatalf("CreateSession(invited person) error = %v, want ErrInactive", err)
	}
	if _, _, err := svc.CreateSession(ctx, admin, device.ID, 0); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("CreateSession(ttl 0) error = %v, want ErrInvalid", err)
	}
	if _, _, err := svc.CreateSession(ctx, admin, uuid.New(), time.Hour); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("CreateSession(unknown device) error = %v, want ErrNotFound", err)
	}
	for _, in := range []identity.NewDevice{{Name: "", Platform: identity.PlatformWeb}, {Name: "Toaster", Platform: "fridge"}} {
		if _, err := svc.RegisterDevice(ctx, admin, alice.ID, in); !errors.Is(err, identity.ErrInvalid) {
			t.Fatalf("RegisterDevice(%+v) error = %v, want ErrInvalid", in, err)
		}
	}
}

func TestSessionDeviceMustBelongToPerson(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	bob := createPerson(t, svc, "bob", identity.StatusActive)
	device, err := svc.RegisterDevice(ctx, admin, alice.ID, identity.NewDevice{Name: "Browser", Platform: identity.PlatformWeb})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sessions (person_id, device_id, token_hash, expires_at)
		VALUES ($1, $2, $3, now() + interval '1 hour')`, bob.ID, device.ID, make([]byte, 32))
	if pgCode(err) != "23503" {
		t.Fatalf("session for bob on alice's device: error = %v, want foreign key violation", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sessions (person_id, device_id, token_hash, expires_at)
		VALUES ($1, $2, $3, now() + interval '1 hour')`, alice.ID, device.ID, []byte("raw-token"))
	if pgCode(err) != "23514" {
		t.Fatalf("session with a non-hash token: error = %v, want check violation", err)
	}
}

// ---------------------------------------------------------------------------
// Audit

func TestAuditEntriesAppendOnly(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	createPerson(t, svc, "alice", identity.StatusInvited)
	before := count(t, pool, "SELECT count(*) FROM audit_entries")
	if before == 0 {
		t.Fatal("no audit entries to tamper with")
	}

	for _, sql := range []string{
		"UPDATE audit_entries SET action = 'person.deleted'",
		"UPDATE audit_entries SET metadata = '{}' WHERE action = 'person.created'",
		"DELETE FROM audit_entries",
		"DELETE FROM audit_entries WHERE action = 'person.created'",
		"TRUNCATE audit_entries",
	} {
		_, err := pool.Exec(ctx, sql)
		if err == nil {
			t.Fatalf("%s succeeded, want it rejected", sql)
		}
		if pgCode(err) != "42501" {
			t.Fatalf("%s: error = %v, want insufficient_privilege from the append-only trigger", sql, err)
		}
	}
	if n := count(t, pool, "SELECT count(*) FROM audit_entries"); n != before {
		t.Fatalf("audit entries = %d after tampering, want %d", n, before)
	}
	if n := count(t, pool, "SELECT count(*) FROM audit_entries WHERE action = 'person.created'"); n != 1 {
		t.Fatalf("person.created entries = %d, want 1", n)
	}
}

// TestEveryMutationIsAudited runs every mutation of the service and checks
// that each one wrote exactly the expected audit entry, with actor, target
// and request details.
func TestEveryMutationIsAudited(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	mark := lastAuditID(t, pool)

	type step struct {
		name string
		run  func() (want []entry)
	}
	var alice, bob store.Person
	var device store.Device
	var session store.Session
	p := func(id uuid.UUID) string { return id.String() }
	who := func(action, kind, id string) entry {
		return entry{string(admin.Kind), admin.ID, action, kind, id}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	steps := []step{
		{"create person", func() []entry {
			var err error
			alice, err = svc.CreatePerson(ctx, admin, identity.NewPerson{Username: "alice", WorkEmail: "alice@acme.test", DisplayName: "Alice", Status: identity.StatusActive})
			must(err)
			return []entry{who("person.created", "person", p(alice.ID))}
		}},
		{"create second person", func() []entry {
			var err error
			bob, err = svc.CreatePerson(ctx, admin, identity.NewPerson{Username: "bob", WorkEmail: "bob@acme.test", DisplayName: "Bob"})
			must(err)
			return []entry{who("person.created", "person", p(bob.ID))}
		}},
		{"update person", func() []entry {
			name := "Alice Smith"
			_, err := svc.UpdatePerson(ctx, admin, alice.ID, identity.PersonUpdate{DisplayName: &name})
			must(err)
			return []entry{who("person.updated", "person", p(alice.ID))}
		}},
		{"assign role", func() []entry {
			must(svc.AssignRole(ctx, admin, alice.ID, authz.RoleOwner))
			return []entry{who("person.role_assigned", "person", p(alice.ID))}
		}},
		{"assign second role", func() []entry {
			must(svc.AssignRole(ctx, admin, bob.ID, authz.RoleAdmin))
			return []entry{who("person.role_assigned", "person", p(bob.ID))}
		}},
		{"remove role", func() []entry {
			must(svc.RemoveRole(ctx, admin, bob.ID, authz.RoleAdmin))
			return []entry{who("person.role_removed", "person", p(bob.ID))}
		}},
		{"activate person", func() []entry {
			_, err := svc.SetPersonStatus(ctx, admin, bob.ID, identity.StatusActive)
			must(err)
			return []entry{who("person.status_changed", "person", p(bob.ID))}
		}},
		{"register device", func() []entry {
			var err error
			device, err = svc.RegisterDevice(ctx, admin, bob.ID, identity.NewDevice{Name: "Pixel", Platform: identity.PlatformAndroid})
			must(err)
			return []entry{who("device.registered", "device", p(device.ID))}
		}},
		{"create session", func() []entry {
			var err error
			session, _, err = svc.CreateSession(ctx, admin, device.ID, time.Hour)
			must(err)
			return []entry{who("session.created", "session", p(session.ID))}
		}},
		{"revoke session", func() []entry {
			_, err := svc.RevokeSession(ctx, admin, session.ID)
			must(err)
			return []entry{who("session.revoked", "session", p(session.ID))}
		}},
		{"revoke device with sessions", func() []entry {
			s1, _, err := svc.CreateSession(ctx, admin, device.ID, time.Hour)
			must(err)
			s2, _, err := svc.CreateSession(ctx, admin, device.ID, time.Hour)
			must(err)
			_, err = svc.RevokeDevice(ctx, admin, device.ID)
			must(err)
			return []entry{
				who("session.created", "session", p(s1.ID)),
				who("session.created", "session", p(s2.ID)),
				who("device.revoked", "device", p(device.ID)),
				who("session.revoked", "session", p(s1.ID)),
				who("session.revoked", "session", p(s2.ID)),
			}
		}},
		{"sign out everywhere", func() []entry {
			d, err := svc.RegisterDevice(ctx, admin, bob.ID, identity.NewDevice{Name: "Browser", Platform: identity.PlatformWeb})
			must(err)
			s, _, err := svc.CreateSession(ctx, admin, d.ID, time.Hour)
			must(err)
			n, err := svc.RevokePersonSessions(ctx, admin, bob.ID)
			must(err)
			if n != 1 {
				t.Fatalf("RevokePersonSessions() = %d, want 1", n)
			}
			return []entry{
				who("device.registered", "device", p(d.ID)),
				who("session.created", "session", p(s.ID)),
				who("session.revoked", "session", p(s.ID)),
			}
		}},
		{"offboard person with a session", func() []entry {
			d, err := svc.RegisterDevice(ctx, admin, bob.ID, identity.NewDevice{Name: "iPad", Platform: identity.PlatformIOS})
			must(err)
			s, _, err := svc.CreateSession(ctx, admin, d.ID, time.Hour)
			must(err)
			_, err = svc.SetPersonStatus(ctx, admin, bob.ID, identity.StatusOffboarded)
			must(err)
			return []entry{
				who("device.registered", "device", p(d.ID)),
				who("session.created", "session", p(s.ID)),
				who("person.status_changed", "person", p(bob.ID)),
				who("session.revoked", "session", p(s.ID)),
			}
		}},
	}
	for _, s := range steps {
		before := lastAuditID(t, pool)
		want := s.run()
		got := summarize(auditLog(t, pool, before))
		if !slices.Equal(got, want) {
			t.Fatalf("%s: audit entries\n got %v\nwant %v", s.name, got, want)
		}
	}

	all := auditLog(t, pool, mark)
	for _, e := range all {
		if e.IP == nil || e.IP.String() != "203.0.113.7" || deref(e.UserAgent) != "orglane-test/1.0" {
			t.Errorf("%s: ip %v, user agent %q; want the request details", e.Action, e.IP, deref(e.UserAgent))
		}
		if e.OccurredAt.Location() != time.UTC || e.ID.Version() != 7 {
			t.Errorf("%s: occurred_at in %v, id version %d", e.Action, e.OccurredAt.Location(), e.ID.Version())
		}
	}

	// Metadata carries the details, never secrets or field values.
	var meta map[string]any
	for _, e := range all {
		if e.Action == "person.updated" {
			must(json.Unmarshal(e.Metadata, &meta))
		}
	}
	if fields, _ := meta["fields"].([]any); len(fields) != 1 || fields[0] != "display_name" || len(meta) != 1 {
		t.Fatalf("person.updated metadata = %v, want only the changed field names", meta)
	}
}

// TestNoOpsAreNotAudited checks that calls which change nothing write
// nothing.
func TestNoOpsAreNotAudited(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	if err := svc.AssignRole(ctx, admin, alice.ID, authz.RoleMember); err != nil {
		t.Fatal(err)
	}
	device, err := svc.RegisterDevice(ctx, admin, alice.ID, identity.NewDevice{Name: "Browser", Platform: identity.PlatformWeb})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := svc.CreateSession(ctx, admin, device.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevokeSession(ctx, admin, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RevokeDevice(ctx, admin, device.ID); err != nil {
		t.Fatal(err)
	}
	mark := lastAuditID(t, pool)

	same := alice.DisplayName
	upper := "ALICE"
	noops := []func() error{
		func() error { return svc.AssignRole(ctx, admin, alice.ID, authz.RoleMember) },
		func() error { return svc.RemoveRole(ctx, admin, alice.ID, authz.RoleAdmin) },
		func() error {
			_, err := svc.UpdatePerson(ctx, admin, alice.ID, identity.PersonUpdate{DisplayName: &same, Username: &upper})
			return err
		},
		func() error { _, err := svc.UpdatePerson(ctx, admin, alice.ID, identity.PersonUpdate{}); return err },
		func() error { _, err := svc.SetPersonStatus(ctx, admin, alice.ID, identity.StatusActive); return err },
		func() error { _, err := svc.RevokeSession(ctx, admin, session.ID); return err },
		func() error { _, err := svc.RevokeDevice(ctx, admin, device.ID); return err },
		func() error { _, err := svc.RevokePersonSessions(ctx, admin, alice.ID); return err },
		func() error { return svc.SeedBuiltinRoles(ctx) },
	}
	for i, op := range noops {
		if err := op(); err != nil {
			t.Fatalf("no-op %d: %v", i, err)
		}
	}
	if got := auditLog(t, pool, mark); len(got) != 0 {
		t.Fatalf("no-ops wrote audit entries: %v", summarize(got))
	}
}

// TestChangeRollsBackWithoutAudit proves the change and its audit entry share
// one transaction: when the entry cannot be written, the change is not
// committed either.
func TestChangeRollsBackWithoutAudit(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	device, err := svc.RegisterDevice(ctx, admin, alice.ID, identity.NewDevice{Name: "Browser", Platform: identity.PlatformWeb})
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := svc.CreateSession(ctx, admin, device.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mark := lastAuditID(t, pool)

	// A person actor without an ID is rejected by the audit writer, after the
	// change itself has run inside the transaction.
	broken := audit.Actor{Kind: audit.ActorPerson}
	name := "Mallory"
	ops := map[string]func() error{
		"create person": func() error {
			_, err := svc.CreatePerson(ctx, broken, identity.NewPerson{Username: "mallory", WorkEmail: "m@acme.test", DisplayName: "M"})
			return err
		},
		"update person": func() error {
			_, err := svc.UpdatePerson(ctx, broken, alice.ID, identity.PersonUpdate{DisplayName: &name})
			return err
		},
		"set status": func() error {
			_, err := svc.SetPersonStatus(ctx, broken, alice.ID, identity.StatusSuspended)
			return err
		},
		"assign role": func() error { return svc.AssignRole(ctx, broken, alice.ID, authz.RoleAdmin) },
		"register device": func() error {
			_, err := svc.RegisterDevice(ctx, broken, alice.ID, identity.NewDevice{Name: "Phone", Platform: identity.PlatformIOS})
			return err
		},
		"create session": func() error { _, _, err := svc.CreateSession(ctx, broken, device.ID, time.Hour); return err },
		"revoke session": func() error { _, err := svc.RevokeSession(ctx, broken, session.ID); return err },
		"revoke device":  func() error { _, err := svc.RevokeDevice(ctx, broken, device.ID); return err },
	}
	for name, op := range ops {
		if err := op(); !errors.Is(err, audit.ErrInvalidEntry) {
			t.Fatalf("%s with a broken actor: error = %v, want audit.ErrInvalidEntry", name, err)
		}
	}

	if got := auditLog(t, pool, mark); len(got) != 0 {
		t.Fatalf("failed changes wrote audit entries: %v", summarize(got))
	}
	for sql, want := range map[string]int{
		"SELECT count(*) FROM people": 1,
		"SELECT count(*) FROM people WHERE display_name = 'Person alice' AND status = 'active'": 1,
		"SELECT count(*) FROM person_roles":                                                     0,
		"SELECT count(*) FROM devices":                                                          1,
		"SELECT count(*) FROM devices WHERE revoked_at IS NULL":                                 1,
		"SELECT count(*) FROM sessions":                                                         1,
		"SELECT count(*) FROM sessions WHERE revoked_at IS NULL":                                1,
	} {
		if n := count(t, pool, sql); n != want {
			t.Errorf("%s = %d, want %d (the failed change was committed)", sql, n, want)
		}
	}
}

// TestConcurrentRevocations runs the paths that revoke sessions at the same
// time. They must neither deadlock nor audit a session twice.
func TestConcurrentRevocations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc, pool := newService(t)
	alice := createPerson(t, svc, "alice", identity.StatusActive)
	var sessions int
	for range 20 {
		device, err := svc.RegisterDevice(ctx, admin, alice.ID, identity.NewDevice{Name: "Browser", Platform: identity.PlatformWeb})
		if err != nil {
			t.Fatal(err)
		}
		session, _, err := svc.CreateSession(ctx, admin, device.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := svc.CreateSession(ctx, admin, device.ID, time.Hour); err != nil {
			t.Fatal(err)
		}
		sessions += 2
		var wg sync.WaitGroup
		errs := make([]error, 3)
		wg.Go(func() { _, errs[0] = svc.RevokeDevice(ctx, admin, device.ID) })
		wg.Go(func() { _, errs[1] = svc.RevokePersonSessions(ctx, admin, alice.ID) })
		wg.Go(func() { _, errs[2] = svc.RevokeSession(ctx, admin, session.ID) })
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent revocation %d: %v", i, err)
			}
		}
	}
	if n := count(t, pool, "SELECT count(*) FROM sessions WHERE revoked_at IS NULL"); n != 0 {
		t.Fatalf("%d sessions still active", n)
	}
	if n := count(t, pool, "SELECT count(*) FROM audit_entries WHERE action = 'session.revoked'"); n != sessions {
		t.Fatalf("session.revoked entries = %d, want one per session (%d)", n, sessions)
	}
	if n := count(t, pool, "SELECT count(DISTINCT target_id) FROM audit_entries WHERE action = 'session.revoked'"); n != sessions {
		t.Fatalf("revoked sessions audited = %d, want %d", n, sessions)
	}
}
