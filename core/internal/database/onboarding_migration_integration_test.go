//go:build integration

package database_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/tests/pgtest"
)

func TestOnboardingMigrationGrandfathersAcceptedInvitations(t *testing.T) {
	ctx := t.Context()
	pool := connect(t, pgtest.NewDatabase(t))
	migrations := newGoose(t, pool)
	// Exercise a real upgrade with identity records created on the old schema.
	for _, source := range migrations.ListSources() {
		if source.Version >= 11 {
			break
		}
		if _, err := migrations.UpByOne(ctx); err != nil {
			t.Fatalf("before onboarding migration: %v", err)
		}
	}
	hasher := crypto.NewPasswordHasher(crypto.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}, 1)
	svc := identity.NewService(pool, identity.WithPasswordHasher(hasher))
	if err := svc.SeedBuiltinRoles(ctx); err != nil {
		t.Fatal(err)
	}
	invite := func(username string) identity.IssuedInvitation {
		t.Helper()
		issued, err := svc.Invite(ctx, audit.System, identity.NewInvitation{
			Username: username, DisplayName: username, WorkEmail: username + "@acme.test", ContactEmail: username + "@example.test", Role: authz.RoleMember,
		})
		if err != nil {
			t.Fatal(err)
		}
		return issued
	}
	accept := func(issued identity.IssuedInvitation) {
		t.Helper()
		_, token, ok := strings.Cut(issued.Link, "#")
		if !ok {
			t.Fatalf("missing invitation token: %q", issued.Link)
		}
		if _, err := svc.AcceptInvitation(ctx, audit.System, token, "correct horse battery staple"); err != nil {
			t.Fatal(err)
		}
	}
	pending := func(id uuid.UUID, want bool) {
		t.Helper()
		got, err := svc.WelcomePending(ctx, id)
		if err != nil || got != want {
			t.Fatalf("welcome pending for %s = %v, %v; want %v", id, got, err, want)
		}
	}
	oldA, oldB := invite("old-a"), invite("old-b")
	accept(oldA)
	accept(oldB)
	waiting := invite("waiting")
	ordinary, err := svc.CreatePerson(ctx, audit.System, identity.NewPerson{Username: "ordinary", DisplayName: "Ordinary", WorkEmail: "ordinary@acme.test", Status: identity.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetAvatar(ctx, audit.System, oldB.Person.ID, identity.Avatar{ContentType: identity.AvatarSVG, Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><circle r="1"/></svg>`)}); err != nil {
		t.Fatal(err)
	}
	result, err := migrations.UpByOne(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.Version != 11 {
		t.Fatalf("applied migration %d, want onboarding 11", result.Source.Version)
	}
	pending(oldA.Person.ID, false)
	pending(oldB.Person.ID, false)
	pending(waiting.Person.ID, false)
	pending(ordinary.ID, false)
	var markers int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM person_onboarding").Scan(&markers); err != nil || markers != 2 {
		t.Fatalf("backfilled markers = %d, %v; want 2", markers, err)
	}
	// Finishing welcome does not pretend that optional account steps are done.
	for _, tc := range []struct {
		id     uuid.UUID
		avatar bool
	}{{oldA.Person.ID, false}, {oldB.Person.ID, true}} {
		avatar, passkey, err := svc.OnboardingAccountCompletion(ctx, tc.id)
		if err != nil || avatar != tc.avatar || passkey {
			t.Fatalf("optional completion = %v,%v,%v", avatar, passkey, err)
		}
	}
	if err := svc.RemoveAvatar(ctx, audit.System, oldB.Person.ID); err != nil {
		t.Fatal(err)
	}
	avatar, _, err := svc.OnboardingAccountCompletion(ctx, oldB.Person.ID)
	if err != nil || avatar {
		t.Fatalf("removed avatar completion = %v,%v", avatar, err)
	}
	pending(oldB.Person.ID, false)
	// Existing but unaccepted invitations and newly issued invitations both
	// start welcome only when accepted after migration 11.
	accept(waiting)
	pending(waiting.Person.ID, true)
	fresh := invite("fresh")
	accept(fresh)
	pending(fresh.Person.ID, true)
	if _, err := migrations.UpTo(ctx, 11); err != nil {
		t.Fatal(err)
	}
	pending(waiting.Person.ID, true)
	pending(fresh.Person.ID, true)
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM person_onboarding").Scan(&markers); err != nil || markers != 2 {
		t.Fatalf("markers after repeated migrate = %d,%v; want 2", markers, err)
	}
	if _, err := migrations.Down(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('person_onboarding') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("down left onboarding table: %v,%v", exists, err)
	}
	var accepted int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM invitations WHERE accepted_at IS NOT NULL").Scan(&accepted); err != nil || accepted != 4 {
		t.Fatalf("down changed accepted invitations: %d,%v", accepted, err)
	}
}
