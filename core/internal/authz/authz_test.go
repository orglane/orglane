package authz

import (
	"context"
	"errors"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"
)

func TestKnown(t *testing.T) {
	if !Known(InstanceRead) {
		t.Errorf("Known(%q) = false, want true", InstanceRead)
	}
	for _, p := range []Permission{Public, Authenticated, Demo, "", "instance.write", "INSTANCE.READ", "people"} {
		if Known(p) {
			t.Errorf("Known(%q) = true, want false", p)
		}
	}
}

func TestPrincipalHas(t *testing.T) {
	p := &Principal{Subject: "alice", Permissions: []Permission{InstanceRead}}
	if !p.Has(InstanceRead) {
		t.Error("Has(InstanceRead) = false, want true")
	}
	if p.Has("people.manage") {
		t.Error("Has(people.manage) = true, want false")
	}
	var nilPrincipal *Principal
	if nilPrincipal.Has(InstanceRead) {
		t.Error("nil principal Has() = true, want false")
	}
	if !p.Has(Authenticated) || !(&Principal{Subject: "bob"}).Has(Authenticated) {
		t.Error("a principal without permissions must hold Authenticated")
	}
	if nilPrincipal.Has(Authenticated) {
		t.Error("nil principal Has(Authenticated) = true, want false")
	}
	for _, marker := range []Permission{Authenticated, Demo} {
		if Known(marker) || permissionName.MatchString(string(marker)) {
			t.Errorf("%q must not be a grantable permission", marker)
		}
	}
	if (&Principal{Subject: "bob", Permissions: All()}).Has(Demo) {
		t.Error("no principal holds Demo: it marks demo-mode routes, it is not granted")
	}
}

func TestDenyAll(t *testing.T) {
	p, err := DenyAll{}.Authenticate(httptest.NewRequestWithContext(t.Context(), "GET", "/", nil))
	if p != nil || !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("DenyAll.Authenticate() = %v, %v; want nil, ErrUnauthenticated", p, err)
	}
}

func TestPrincipalContext(t *testing.T) {
	if got := PrincipalFrom(context.Background()); got != nil {
		t.Fatalf("PrincipalFrom(empty) = %v, want nil", got)
	}
	p := &Principal{Subject: "alice"}
	if got := PrincipalFrom(WithPrincipal(context.Background(), p)); got != p {
		t.Fatalf("PrincipalFrom() = %v, want %v", got, p)
	}
}

// permissionName is the naming rule for permissions; the database enforces
// the same pattern on role_permissions.permission.
var permissionName = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

func TestRegistry(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("All() is empty")
	}
	seen := map[Permission]bool{}
	for _, p := range all {
		if !permissionName.MatchString(string(p)) {
			t.Errorf("permission %q does not match %s", p, permissionName)
		}
		if seen[p] {
			t.Errorf("permission %q registered twice", p)
		}
		seen[p] = true
		if !Known(p) {
			t.Errorf("Known(%q) = false for a registered permission", p)
		}
	}
	if permissionName.MatchString(string(Public)) {
		t.Errorf("%q must not be a valid permission name", Public)
	}
	all[0] = "mutated"
	if All()[0] == "mutated" {
		t.Error("All() returns the registry itself, want a copy")
	}
}

func TestBuiltinRoles(t *testing.T) {
	roles := BuiltinRoles()
	var keys []RoleKey
	for _, r := range roles {
		keys = append(keys, r.Key)
		if r.Name == "" {
			t.Errorf("role %q has no name", r.Key)
		}
		for _, p := range r.Permissions {
			if !Known(p) {
				t.Errorf("role %q bundles unknown permission %q", r.Key, p)
			}
		}
		if got, ok := BuiltinRole(r.Key); !ok || got.Key != r.Key {
			t.Errorf("BuiltinRole(%q) = %v, %v", r.Key, got, ok)
		}
	}
	if want := []RoleKey{RoleOwner, RoleAdmin, RoleMember}; !slices.Equal(keys, want) {
		t.Fatalf("built-in roles = %v, want %v", keys, want)
	}
	if _, ok := BuiltinRole("root"); ok {
		t.Error(`BuiltinRole("root") found a role`)
	}

	owner, _ := BuiltinRole(RoleOwner)
	if !slices.Equal(owner.Permissions, All()) {
		t.Errorf("owner permissions = %v, want every permission", owner.Permissions)
	}
	admin, _ := BuiltinRole(RoleAdmin)
	for _, p := range []Permission{BillingManage, PeopleSensitiveRead} {
		if slices.Contains(admin.Permissions, p) {
			t.Errorf("admin holds %q, which is reserved for the owner (ADR 0004)", p)
		}
	}
	for _, p := range []Permission{PeopleManage, AppsManage, SecurityManage} {
		if !slices.Contains(admin.Permissions, p) {
			t.Errorf("admin lacks %q", p)
		}
	}
	member, _ := BuiltinRole(RoleMember)
	if len(member.Permissions) != 0 {
		t.Errorf("member permissions = %v, want none", member.Permissions)
	}
}
