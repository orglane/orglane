package api

import (
	"strings"
	"testing"

	specpkg "github.com/orglane/orglane/core/api"
	"github.com/orglane/orglane/core/internal/authz"
)

// TestContractDeclaresPermissions is the build gate from ADR 0004: every
// operation in core/api/openapi.yaml must declare x-orglane-permission with a
// permission from the registry in package authz, "public", "authenticated"
// or "demo".
func TestContractDeclaresPermissions(t *testing.T) {
	ops, err := ParseOperations(specpkg.Spec)
	if err != nil {
		t.Fatalf("core/api/openapi.yaml violates the API conventions:\n%v", err)
	}
	if len(ops) == 0 {
		t.Fatal("no operations found in core/api/openapi.yaml")
	}
	for _, op := range ops {
		if op.Permission != authz.Public && op.Permission != authz.Authenticated && op.Permission != authz.Demo && !authz.Known(op.Permission) {
			t.Errorf("%s: permission %q is not in the authz registry", op.Pattern(), op.Permission)
		}
	}
}

func TestContractPermissions(t *testing.T) {
	ops, err := ParseOperations(specpkg.Spec)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]authz.Permission{
		"GET /healthz":         authz.Public,
		"GET /readyz":          authz.Public,
		"GET /api/v1/version":  authz.Public,
		"GET /api/v1/instance": authz.InstanceRead,
		// Instance discovery and sign-in are public; self-service needs a
		// signed-in caller.
		"GET /.well-known/orglane":               authz.Public,
		"GET /api/v1/auth/setup":                 authz.Public,
		"POST /api/v1/auth/setup":                authz.Public,
		"POST /api/v1/auth/sign-in":              authz.Public,
		"POST /api/v1/auth/passkey/options":      authz.Public,
		"POST /api/v1/auth/passkey":              authz.Public,
		"POST /api/v1/auth/sign-out":             authz.Authenticated,
		"GET /api/v1/me":                         authz.Authenticated,
		"GET /api/v1/me/sessions":                authz.Authenticated,
		"DELETE /api/v1/me/sessions/{sessionId}": authz.Authenticated,
		"GET /api/v1/me/passkeys":                authz.Authenticated,
		"POST /api/v1/me/passkeys":               authz.Authenticated,
		"POST /api/v1/me/passkeys/options":       authz.Authenticated,
		"DELETE /api/v1/me/passkeys/{passkeyId}": authz.Authenticated,
		"POST /api/v1/me/passkey-offer/dismiss":  authz.Authenticated,
		// Every signed-in person can read the directory.
		"GET /api/v1/people/{personId}": authz.Authenticated,
		// Base edits and raster uploads allow self-service; handlers enforce
		// self or people.manage, with admin-only identity/reporting fields.
		"PATCH /api/v1/people/{personId}":      authz.Authenticated,
		"PUT /api/v1/people/{personId}/avatar": authz.Authenticated,
		"GET /api/v1/people":                   authz.Authenticated,
		"GET /api/v1/people/{personId}/avatar": authz.Authenticated,
		// Every signed-in person can read teams; managing them is people
		// management.
		"GET /api/v1/people/{personId}/teams":              authz.Authenticated,
		"GET /api/v1/teams":                                authz.Authenticated,
		"GET /api/v1/teams/{teamId}":                       authz.Authenticated,
		"GET /api/v1/teams/{teamId}/members":               authz.Authenticated,
		"POST /api/v1/teams":                               authz.PeopleManage,
		"PATCH /api/v1/teams/{teamId}":                     authz.PeopleManage,
		"POST /api/v1/teams/{teamId}/archive":              authz.PeopleManage,
		"PUT /api/v1/teams/{teamId}/members/{personId}":    authz.PeopleManage,
		"DELETE /api/v1/teams/{teamId}/members/{personId}": authz.PeopleManage,
		// Profiles: package profile applies the self, team and admin rules
		// to every caller; definitions are managed with people.manage.
		"GET /api/v1/people/{personId}/profile":           authz.Authenticated,
		"PATCH /api/v1/people/{personId}/profile":         authz.Authenticated,
		"GET /api/v1/profile-fields":                      authz.PeopleManage,
		"POST /api/v1/profile-fields":                     authz.PeopleManage,
		"PATCH /api/v1/profile-fields/{fieldId}":          authz.PeopleManage,
		"GET /api/v1/profile-field-library":               authz.PeopleManage,
		"POST /api/v1/profile-field-library/{libraryKey}": authz.PeopleManage,
		// Accepting an invitation is how invited people get credentials.
		"POST /api/v1/auth/invitation":        authz.Public,
		"POST /api/v1/auth/invitation/accept": authz.Public,
		// Inviting adds people; email settings are instance settings.
		"GET /api/v1/invitations":                        authz.PeopleManage,
		"POST /api/v1/invitations":                       authz.PeopleManage,
		"GET /api/v1/invitations/options":                authz.PeopleManage,
		"POST /api/v1/invitations/{invitationId}/resend": authz.PeopleManage,
		"DELETE /api/v1/invitations/{invitationId}":      authz.PeopleManage,
		"GET /api/v1/settings/email":                     authz.InstanceManage,
		"PUT /api/v1/settings/email":                     authz.InstanceManage,
		"DELETE /api/v1/settings/email":                  authz.InstanceManage,
		"POST /api/v1/settings/email/test":               authz.InstanceManage,
		// Demo mode only: not served at all otherwise.
		"GET /api/v1/demo":          authz.Demo,
		"POST /api/v1/demo/sign-in": authz.Demo,
	}
	got := map[string]authz.Permission{}
	for _, op := range ops {
		got[op.Pattern()] = op.Permission
	}
	if len(got) != len(want) {
		t.Errorf("the contract has %d operations, this test knows %d: add the new ones here", len(got), len(want))
	}
	for pattern, perm := range want {
		if got[pattern] != perm {
			t.Errorf("%s: permission = %q, want %q", pattern, got[pattern], perm)
		}
	}
}

func TestParseOperationsRejects(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{
			name: "missing permission",
			spec: `
paths:
  /api/v1/things:
    get:
      operationId: listThings
`,
			wantErr: "GET /api/v1/things: missing x-orglane-permission",
		},
		{
			name: "unknown permission",
			spec: `
paths:
  /api/v1/things:
    post:
      operationId: createThing
      x-orglane-permission: things.manage
`,
			wantErr: `unknown permission "things.manage"`,
		},
		{
			name: "permission not in the registry next to a valid one",
			spec: `
paths:
  /api/v1/people:
    get:
      operationId: listPeople
      x-orglane-permission: people.manage
    delete:
      operationId: deletePeople
      x-orglane-permission: people.delete
`,
			wantErr: `DELETE /api/v1/people: unknown permission "people.delete"`,
		},
		{
			name: "permission with the wrong case",
			spec: `
paths:
  /api/v1/people:
    get:
      operationId: listPeople
      x-orglane-permission: People.Manage
`,
			wantErr: `unknown permission "People.Manage"`,
		},
		{
			name: "empty permission",
			spec: `
paths:
  /api/v1/people:
    get:
      operationId: listPeople
      x-orglane-permission: ""
`,
			wantErr: `unknown permission ""`,
		},
		{
			name: "missing operationId",
			spec: `
paths:
  /api/v1/things:
    get:
      x-orglane-permission: public
`,
			wantErr: "missing operationId",
		},
		{
			name: "unversioned path",
			spec: `
paths:
  /things:
    get:
      operationId: listThings
      x-orglane-permission: public
`,
			wantErr: "path /things: must be under /api/v1/",
		},
		{
			name: "authenticated outside the registry is fine, unversioned paths are not",
			spec: `
paths:
  /api/v1/me:
    get:
      operationId: getMe
      x-orglane-permission: authenticated
  /me:
    get:
      operationId: getMeUnversioned
      x-orglane-permission: authenticated
`,
			wantErr: "path /me: must be under /api/v1/",
		},
		{
			name:    "invalid YAML",
			spec:    "paths: [",
			wantErr: "parse OpenAPI document",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseOperations([]byte(tt.spec))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseOperations() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseOperationsIgnoresPathItemFields(t *testing.T) {
	ops, err := ParseOperations([]byte(`
paths:
  /api/v1/things/{id}:
    summary: A thing
    parameters:
      - name: id
        in: path
        required: true
        schema: {type: string}
    get:
      operationId: getThing
      x-orglane-permission: instance.read
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Pattern() != "GET /api/v1/things/{id}" || ops[0].Permission != authz.InstanceRead {
		t.Fatalf("ParseOperations() = %+v", ops)
	}
}
