// Package oidcop is the embedded OpenID Provider (ADR 0003), built on the
// OpenID-certified zitadel/oidc library: discovery, authorization code flow
// with PKCE, refresh tokens with rotation, userinfo, introspection,
// revocation, RP-initiated logout and the JWKS.
//
// The issuer is the workspace URL (ADR 0018). Protocol endpoints live under
// /oidc/ so they never collide with web UI routes; discovery is at
// /.well-known/openid-configuration. Sign-in itself is not part of the
// protocol: /authorize redirects to the login URL (package web), which
// completes the request for the signed-in person (CompleteAuthRequest) and
// sends the browser to CallbackURL.
//
// Protocol state (authorization requests, codes, tokens, signing keys) is
// stored in the core database and owned by this package. Access tokens are
// opaque: an encrypted reference to a database row, so revoking a token, a
// grant or the person takes effect on the next request.
package oidcop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
	"golang.org/x/text/language"

	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
)

// Endpoint paths, relative to the issuer.
const (
	PathPrefix      = "/oidc/"
	PathDiscovery   = "/.well-known/openid-configuration"
	PathAuthorize   = "/oidc/authorize"
	PathCallback    = "/oidc/authorize/callback"
	PathToken       = "/oidc/token"
	PathUserinfo    = "/oidc/userinfo"
	PathIntrospect  = "/oidc/introspect"
	PathRevoke      = "/oidc/revoke"
	PathEndSession  = "/oidc/end_session"
	PathKeys        = "/oidc/keys"
	PathDeviceAuthz = "/oidc/device_authorization"
	// PathLogin is where /authorize sends the browser to sign in. It is
	// served by package web, not by the protocol handler.
	PathLogin = "/oidc/login"
	// PathLogout asks the person to confirm signing out when a logout
	// request does not prove it comes from their own session (package web).
	PathLogout = "/oidc/logout"
)

// Options configure the provider.
type Options struct {
	// Issuer is the workspace URL, e.g. https://work.acme.com.
	Issuer string
	// Keyring provides the token encryption key.
	Keyring   *crypto.Keyring
	Keys      *KeyManager
	Lifetimes Lifetimes
	Logger    *slog.Logger
}

// Provider is the OpenID Provider.
type Provider struct {
	op      *op.Provider
	storage *storage
	issuer  string
	crypto  op.Crypto
}

// New builds the provider. An http issuer is allowed (local plain-HTTP
// demos); configuration validation only permits it in development.
func New(pool *pgxpool.Pool, opts Options) (*Provider, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	u, err := url.Parse(opts.Issuer)
	if err != nil || u.Host == "" || u.Path != "" {
		return nil, fmt.Errorf("oidcop: invalid issuer %q", opts.Issuer)
	}
	st := &storage{pool: pool, keys: opts.Keys, lifetimes: opts.Lifetimes, loginURL: opts.Issuer + PathLogin, logger: opts.Logger}
	cfg := &op.Config{
		// Without a valid post_logout_redirect_uri, logout ends on the
		// workspace.
		DefaultLogoutRedirectURI: opts.Issuer + "/",
		CodeMethodS256:           true,
		AuthMethodPost:           true,
		GrantTypeRefreshToken:    true,
		SupportedUILocales:       []language.Tag{language.English},
		SupportedScopes:          []string{oidc.ScopeOpenID, oidc.ScopeProfile, oidc.ScopeEmail, oidc.ScopeOfflineAccess},
		SupportedClaims: []string{
			"sub", "aud", "exp", "iat", "iss", "auth_time", "nonce", "amr", "azp", "at_hash", "c_hash",
			"name", "preferred_username", "updated_at", "email", "email_verified",
		},
	}
	options := []op.Option{
		op.WithCustomEndpoints(
			op.NewEndpoint(PathAuthorize), op.NewEndpoint(PathToken), op.NewEndpoint(PathUserinfo),
			op.NewEndpoint(PathRevoke), op.NewEndpoint(PathEndSession), op.NewEndpoint(PathKeys),
		),
		op.WithCustomIntrospectionEndpoint(op.NewEndpoint(PathIntrospect)),
		op.WithCustomDeviceAuthorizationEndpoint(op.NewEndpoint(PathDeviceAuthz)),
		op.WithLogger(opts.Logger.With("component", "oidc")),
		// Access tokens are encrypted with AES-256-GCM, and only GCM
		// decrypts them. zitadel's default also falls back to AES-CFB for
		// tokens of older zitadel versions; CFB is not authenticated, so it
		// "decrypts" any long enough base64url string, a refresh token
		// included, into garbage that may parse as a token ID and subject.
		// This provider never issued CFB tokens.
		op.WithCrypto(op.NewAES256GCMCrypto(opts.Keyring.Key(crypto.PurposeOIDCTokens), "1")),
	}
	if u.Scheme == "http" {
		options = append(options, op.WithAllowInsecure())
	}
	provider, err := op.NewProvider(cfg, st, op.StaticIssuer(opts.Issuer), options...)
	if err != nil {
		return nil, fmt.Errorf("oidcop: %w", err)
	}
	return &Provider{op: provider, storage: st, issuer: opts.Issuer, crypto: provider.Crypto()}, nil
}

// Issuer returns the issuer URL.
func (p *Provider) Issuer() string { return p.issuer }

// ServeHTTP serves the protocol endpoints (discovery and /oidc/*).
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.op.ServeHTTP(w, r)
}

// AuthRequest returns a pending, unexpired authorization request.
func (p *Provider) AuthRequest(ctx context.Context, id string) (*AuthRequest, error) {
	return p.storage.authRequest(ctx, id)
}

// ErrAuthRequestGone is returned when a request expired, was completed
// already or does not exist.
var ErrAuthRequestGone = errors.New("oidcop: the authorization request is no longer pending")

// CompleteAuthRequest records the signed-in person for a pending request.
// session is the browser session the person signed in with; its creation
// time is the authentication time.
func (p *Provider) CompleteAuthRequest(ctx context.Context, id string, session store.Session) error {
	uid, err := parseID(id)
	if err != nil {
		return ErrAuthRequestGone
	}
	_, err = p.storage.q().CompleteOIDCAuthRequest(ctx, store.CompleteOIDCAuthRequestParams{
		ID:          uid,
		PersonID:    &session.PersonID,
		SessionID:   &session.ID,
		AuthTime:    &session.CreatedAt,
		AuthMethods: nonNil(session.AuthMethods),
	})
	if err != nil {
		return ErrAuthRequestGone
	}
	return nil
}

// CallbackURL is where the browser continues after CompleteAuthRequest:
// the provider issues the code and redirects to the client.
func (p *Provider) CallbackURL(id string) string {
	return p.issuer + PathCallback + "?id=" + url.QueryEscape(id)
}

// AuthRequestError sends the browser back to the client with an OAuth
// error, e.g. login_required for prompt=none without a session.
func (p *Provider) AuthRequestError(w http.ResponseWriter, r *http.Request, req *AuthRequest, err error) {
	op.AuthRequestError(w, r, req, err, p.op)
}

// ErrLoginRequired is the OAuth error for prompt=none without a usable
// session.
func ErrLoginRequired() error { return oidc.ErrLoginRequired() }

// AccessToken is a valid OIDC access token presented to the API.
type AccessToken struct {
	ID       uuid.UUID
	PersonID uuid.UUID
	ClientID string
	Scopes   []string
}

// ErrInvalidAccessToken is returned for tokens that are malformed, expired,
// revoked or belong to an inactive person.
var ErrInvalidAccessToken = errors.New("oidcop: invalid access token")

// ValidateAccessToken checks an opaque access token against the database.
func (p *Provider) ValidateAccessToken(ctx context.Context, token string) (AccessToken, error) {
	plain, err := p.crypto.Decrypt(token)
	if err != nil {
		return AccessToken{}, ErrInvalidAccessToken
	}
	tokenID, subject, ok := strings.Cut(plain, ":")
	if !ok {
		return AccessToken{}, ErrInvalidAccessToken
	}
	t, err := p.storage.validAccessToken(ctx, tokenID, subject)
	if err != nil {
		return AccessToken{}, ErrInvalidAccessToken
	}
	return AccessToken{ID: t.ID, PersonID: t.PersonID, ClientID: t.ClientID, Scopes: t.Scopes}, nil
}

// Cleanup deletes expired authorization requests and tokens. The periodic
// identity job calls it.
func (p *Provider) Cleanup(ctx context.Context) error {
	q := p.storage.q()
	if _, err := q.DeleteExpiredOIDCAuthRequests(ctx); err != nil {
		return err
	}
	if _, err := q.DeleteExpiredOIDCAccessTokens(ctx); err != nil {
		return err
	}
	if _, err := q.DeleteExpiredOIDCRefreshTokens(ctx); err != nil {
		return err
	}
	_, err := q.DeleteExpiredOIDCGrants(ctx)
	return err
}
