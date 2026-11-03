package oidcop

import (
	"slices"
	"strings"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
)

// amr maps session authentication methods to RFC 8176 values.
func amr(methods []string) []string {
	out := make([]string, 0, len(methods))
	for _, m := range methods {
		switch identity.AuthMethod(m) {
		case identity.AuthPassword:
			out = append(out, "pwd")
		case identity.AuthPasskey:
			out = append(out, "hwk")
		}
	}
	return out
}

// AuthRequest is an authorization request in flight (op.AuthRequest).
type AuthRequest struct {
	row store.OidcAuthRequest
}

var _ op.AuthRequest = (*AuthRequest)(nil)

// GetID returns the request ID.
func (a *AuthRequest) GetID() string { return a.row.ID.String() }

// GetACR returns the authentication context class; none is used.
func (a *AuthRequest) GetACR() string { return "" }

// GetAMR returns the authentication methods (RFC 8176).
func (a *AuthRequest) GetAMR() []string { return amr(a.row.AuthMethods) }

// GetAudience returns the token audience: the client.
func (a *AuthRequest) GetAudience() []string { return []string{a.row.ClientID} }

// GetClientID returns the client ID.
func (a *AuthRequest) GetClientID() string { return a.row.ClientID }

// GetNonce returns the nonce.
func (a *AuthRequest) GetNonce() string { return a.row.Nonce }

// GetRedirectURI returns the redirect URI.
func (a *AuthRequest) GetRedirectURI() string { return a.row.RedirectUri }

// GetScopes returns the requested scopes.
func (a *AuthRequest) GetScopes() []string { return a.row.Scopes }

// GetState returns the client's state.
func (a *AuthRequest) GetState() string { return a.row.State }

// Done reports whether a person signed in for the request.
func (a *AuthRequest) Done() bool { return a.row.PersonID != nil }

// GetAuthTime returns when the person authenticated.
func (a *AuthRequest) GetAuthTime() time.Time {
	if a.row.AuthTime == nil {
		return time.Time{}
	}
	return *a.row.AuthTime
}

// GetCodeChallenge returns the PKCE challenge, if any.
func (a *AuthRequest) GetCodeChallenge() *oidc.CodeChallenge {
	if a.row.CodeChallenge == "" {
		return nil
	}
	return &oidc.CodeChallenge{Challenge: a.row.CodeChallenge, Method: oidc.CodeChallengeMethod(a.row.CodeChallengeMethod)}
}

// GetResponseType returns the response type.
func (a *AuthRequest) GetResponseType() oidc.ResponseType {
	return oidc.ResponseType(a.row.ResponseType)
}

// GetResponseMode returns the response mode.
func (a *AuthRequest) GetResponseMode() oidc.ResponseMode {
	return oidc.ResponseMode(a.row.ResponseMode)
}

// GetSubject returns the person ID once signed in.
func (a *AuthRequest) GetSubject() string {
	if a.row.PersonID == nil {
		return ""
	}
	return a.row.PersonID.String()
}

// PromptNone reports whether the client asked for no interaction.
func (a *AuthRequest) PromptNone() bool { return slices.Contains(a.row.Prompt, oidc.PromptNone) }

// PromptLogin reports whether the client asked for a fresh sign-in.
func (a *AuthRequest) PromptLogin() bool { return slices.Contains(a.row.Prompt, oidc.PromptLogin) }

// MaxAge returns the maximum authentication age the client accepts, if any.
func (a *AuthRequest) MaxAge() (time.Duration, bool) {
	if a.row.MaxAgeSeconds == nil {
		return 0, false
	}
	return time.Duration(*a.row.MaxAgeSeconds) * time.Second, true
}

// CreatedAt returns when the client started the request.
func (a *AuthRequest) CreatedAt() time.Time { return a.row.CreatedAt }

// LoginHint returns the login_hint parameter (an email, typically).
func (a *AuthRequest) LoginHint() string { return a.row.LoginHint }

// ClientID returns the ID of the requesting client.
func (a *AuthRequest) ClientID() string { return a.row.ClientID }

// refreshTokenRequest is a refresh token exchange (op.RefreshTokenRequest).
type refreshTokenRequest struct {
	row    store.OidcRefreshToken
	scopes []string
}

var _ op.RefreshTokenRequest = (*refreshTokenRequest)(nil)

func (r *refreshTokenRequest) GetAMR() []string                 { return amr(r.row.AuthMethods) }
func (r *refreshTokenRequest) GetAudience() []string            { return r.row.Audience }
func (r *refreshTokenRequest) GetAuthTime() time.Time           { return r.row.AuthTime }
func (r *refreshTokenRequest) GetClientID() string              { return r.row.ClientID }
func (r *refreshTokenRequest) GetScopes() []string              { return r.scopes }
func (r *refreshTokenRequest) GetSubject() string               { return r.row.PersonID.String() }
func (r *refreshTokenRequest) SetCurrentScopes(scopes []string) { r.scopes = scopes }

// client adapts a registered client to op.Client.
type client struct {
	row             store.OidcClient
	loginURL        string
	idTokenLifetime time.Duration
}

var _ op.Client = (*client)(nil)

func (c *client) GetID() string                    { return c.row.ID }
func (c *client) RedirectURIs() []string           { return c.row.RedirectUris }
func (c *client) PostLogoutRedirectURIs() []string { return c.row.PostLogoutRedirectUris }
func (c *client) LoginURL(id string) string        { return c.loginURL + "?authRequest=" + id }
func (c *client) AccessTokenType() op.AccessTokenType {
	// Opaque tokens: every use is checked against the database, so
	// revocation is immediate.
	return op.AccessTokenTypeBearer
}
func (c *client) IDTokenLifetime() time.Duration { return c.idTokenLifetime }
func (c *client) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}
func (c *client) RestrictAdditionalIdTokenScopes() func([]string) []string { //nolint:revive // name from op.Client
	return keepScopes
}
func (c *client) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return keepScopes
}
func (c *client) IsScopeAllowed(string) bool           { return false }
func (c *client) IDTokenUserinfoClaimsAssertion() bool { return false }
func (c *client) ClockSkew() time.Duration             { return 0 }

func keepScopes(scopes []string) []string { return scopes }

func (c *client) ApplicationType() op.ApplicationType {
	switch c.row.ApplicationType {
	case "native":
		return op.ApplicationTypeNative
	case "user_agent":
		return op.ApplicationTypeUserAgent
	default:
		return op.ApplicationTypeWeb
	}
}

func (c *client) AuthMethod() oidc.AuthMethod {
	switch c.row.AuthMethod {
	case identity.ClientAuthPost:
		return oidc.AuthMethodPost
	case identity.ClientAuthNone:
		return oidc.AuthMethodNone
	default:
		return oidc.AuthMethodBasic
	}
}

func (c *client) GrantTypes() []oidc.GrantType {
	out := make([]oidc.GrantType, 0, len(c.row.GrantTypes))
	for _, g := range c.row.GrantTypes {
		out = append(out, oidc.GrantType(g))
	}
	return out
}

// DevMode allows plain-http redirect URIs, which registration accepts only
// for loopback hosts (local development).
func (c *client) DevMode() bool {
	return slices.ContainsFunc(c.row.RedirectUris, func(u string) bool { return strings.HasPrefix(u, "http://") })
}
