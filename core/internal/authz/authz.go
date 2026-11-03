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

// Authenticated marks an operation any signed-in caller may use, without a
// granted permission: self-service on the caller's own account (their
// sessions, passkeys, sign-out). It is not a grantable permission either.
const Authenticated Permission = "authenticated"

// Demo marks an operation of demo mode (one-click sign-in as a person of the
// demo dataset). Like Public it needs no authentication, but its route is
// registered only when the server runs in demo mode: otherwise it does not
// exist at all (404). It is not a grantable permission either.
const Demo Permission = "demo"

// The permission registry: the single source of truth for permission names.
// Add a permission here (constant and registry entry) before using it in the
// OpenAPI contract, in a role bundle or in a module scope; the contract tests
// reject unknown names so typos fail the build. Names are lower case,
// dot-separated "<area>.<action>" and never change once released, because
// role bundles reference them by name in the database.
const (
	// InstanceRead allows reading how the instance is addressed and configured.
	InstanceRead Permission = "instance.read"
	// InstanceManage allows changing instance-wide settings.
	InstanceManage Permission = "instance.manage"
	// PeopleManage allows adding, editing, suspending and offboarding people
	// and assigning their roles.
	PeopleManage Permission = "people.manage"
	// PeopleSensitiveRead allows reading sensitive profile fields (ADR 0015).
	PeopleSensitiveRead Permission = "people.sensitive.read"
	// AppsManage allows connecting and configuring providers and modules.
	AppsManage Permission = "apps.manage"
	// SecurityManage allows managing security settings, other people's
	// devices and sessions, and reading the audit log.
	SecurityManage Permission = "security.manage"
	// BillingManage allows managing the subscription and payments.
	BillingManage Permission = "billing.manage"
)

// registry lists every known permission, in a stable order.
var registry = []Permission{
	InstanceRead,
	InstanceManage,
	PeopleManage,
	PeopleSensitiveRead,
	AppsManage,
	SecurityManage,
	BillingManage,
}

// Known reports whether p is a permission from the registry. Public is not a
// permission and is not known.
func Known(p Permission) bool {
	return slices.Contains(registry, p)
}

// All returns every known permission, in a stable order.
func All() []Permission {
	return slices.Clone(registry)
}

// Principal is an authenticated caller: a person, a module or an agent.
type Principal struct {
	// Subject identifies the caller (a person ID for people).
	Subject string
	// Permissions are the permissions granted to the caller, already resolved
	// from roles and scopes.
	Permissions []Permission
	// SessionID is the browser session the caller authenticated with; empty
	// for bearer tokens.
	SessionID string
	// ClientID is the OIDC client a bearer token was issued to; empty for
	// sessions.
	ClientID string
}

// Has reports whether the principal holds permission p. Every principal
// holds Authenticated.
func (p *Principal) Has(perm Permission) bool {
	if p == nil {
		return false
	}
	return perm == Authenticated || slices.Contains(p.Permissions, perm)
}

// ErrUnauthenticated means the request carries no valid credentials.
var ErrUnauthenticated = errors.New("authz: unauthenticated")

// ErrCrossSite means a cookie-authenticated, state-changing request failed
// the CSRF checks (Origin and CSRF token). It is answered with 403.
var ErrCrossSite = errors.New("authz: cross-site request rejected")

// Authenticator resolves the caller of a request. It returns
// ErrUnauthenticated or ErrCrossSite (possibly wrapped) when the request has
// no valid credentials or fails the CSRF checks; any other error is treated
// as a server error.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// DenyAll rejects every request as unauthenticated. It is the default when
// no authenticator is configured, so non-public operations fail closed; the
// server uses the identity authenticator (package identity/web).
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
