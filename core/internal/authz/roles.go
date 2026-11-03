package authz

import "slices"

// RoleKey identifies a role. Built-in roles have fixed keys; the UI shows
// them in the user's language by key.
type RoleKey string

// Built-in role keys (ADR 0004).
const (
	RoleOwner  RoleKey = "owner"
	RoleAdmin  RoleKey = "admin"
	RoleMember RoleKey = "member"
)

// Role is a bundle of permissions. Code checks permissions, never role keys
// (ADR 0004); the keys exist to seed, display and assign roles.
type Role struct {
	Key RoleKey
	// Name is the English display name, the fallback when the UI has no
	// translation for the key.
	Name        string
	Permissions []Permission
}

// BuiltinRoles returns the roles every instance has. They are seeded into the
// database on startup, and their permission sets in the database are kept in
// sync with this definition (see identity.Service.SeedBuiltinRoles).
//
//   - Owner holds every permission, including billing and sensitive profile
//     fields.
//   - Admin runs the instance: people, apps, security and instance settings,
//     but neither billing nor sensitive profile fields ("admins needn't see
//     personal data", ADR 0004).
//   - Member has no administrative permissions.
func BuiltinRoles() []Role {
	return []Role{
		{Key: RoleOwner, Name: "Owner", Permissions: All()},
		{Key: RoleAdmin, Name: "Admin", Permissions: []Permission{
			InstanceRead,
			InstanceManage,
			PeopleManage,
			AppsManage,
			SecurityManage,
		}},
		{Key: RoleMember, Name: "Member", Permissions: []Permission{}},
	}
}

// BuiltinRole returns the built-in role with the given key.
func BuiltinRole(key RoleKey) (Role, bool) {
	roles := BuiltinRoles()
	i := slices.IndexFunc(roles, func(r Role) bool { return r.Key == key })
	if i < 0 {
		return Role{}, false
	}
	return roles[i], true
}
