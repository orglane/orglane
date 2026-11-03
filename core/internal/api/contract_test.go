package api

import (
	"strings"
	"testing"

	specpkg "github.com/orglane/orglane/core/api"
	"github.com/orglane/orglane/core/internal/authz"
)

// TestContractDeclaresPermissions is the build gate from ADR 0004: every
// operation in core/api/openapi.yaml must declare x-orglane-permission with a
// known permission or "public".
func TestContractDeclaresPermissions(t *testing.T) {
	ops, err := ParseOperations(specpkg.Spec)
	if err != nil {
		t.Fatalf("core/api/openapi.yaml violates the API conventions:\n%v", err)
	}
	if len(ops) == 0 {
		t.Fatal("no operations found in core/api/openapi.yaml")
	}
	for _, op := range ops {
		if op.Permission == "" {
			t.Errorf("%s: empty permission", op.Pattern())
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
	}
	got := map[string]authz.Permission{}
	for _, op := range ops {
		got[op.Pattern()] = op.Permission
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
