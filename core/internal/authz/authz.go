// Package authz holds the permission vocabulary and the authentication seam of
// the core (ADR 0004).
//
// Code checks granular permissions, never role names. Every API operation
// declares the permission it requires in the OpenAPI contract with the
// `x-orglane-permission` extension; the API layer enforces it per operation.
package authz

import (
	"context"
	"errors"
	"net/http"
	"slices"
)

// Permission is a granular permission such as "people.manage".
type Permission string

// Public marks an operation that needs no authentication (health probes, the
// version endpoint). It is not a grantable permission.
const Public Permission = "public"

// Known permissions. Add a permission here before using it in the OpenAPI
// contract; the contract tests reject unknown names so typos fail the build.
const (
	// InstanceRead allows reading how the instance is addressed and configured.
	InstanceRead Permission = "instance.read"
)

var known = []Permission{
	InstanceRead,
}

// Known reports whether p is a permission from the vocabulary above. Public is
// not a permission and is not known.
func Known(p Permission) bool {
	return slices.Contains(known, p)
}

// Principal is an authenticated caller: a person, a module or an agent.
type Principal struct {
	// Subject identifies the caller.
	Subject string
	// Permissions are the permissions granted to the caller, already resolved
	// from roles and scopes.
	Permissions []Permission
}

// Has reports whether the principal holds permission p.
func (p *Principal) Has(perm Permission) bool {
	return p != nil && slices.Contains(p.Permissions, perm)
}

// ErrUnauthenticated means the request carries no valid credentials.
var ErrUnauthenticated = errors.New("authz: unauthenticated")

// Authenticator resolves the caller of a request. It returns
// ErrUnauthenticated (possibly wrapped) when the request has no valid
// credentials; any other error is treated as a server error.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// DenyAll rejects every request as unauthenticated. It is the authenticator
// until the embedded identity provider lands (#6), so every non-public
// operation is closed by default.
type DenyAll struct{}

// Authenticate implements Authenticator.
func (DenyAll) Authenticate(*http.Request) (*Principal, error) {
	return nil, ErrUnauthenticated
}

type principalKey struct{}

// WithPrincipal returns a copy of ctx carrying the principal.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored in ctx, or nil for public
// operations and unauthenticated contexts.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}
