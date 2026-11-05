package identity

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
)

// OIDC clients are the relying parties allowed to sign people in with
// Orglane: company services (Forgejo, ...) and apps. Operators register them
// (`orglane cli oidc-client add`); there is no dynamic registration.

// ClientSecretPrefix starts every OIDC client secret.
const ClientSecretPrefix = "olcs_"

// Client authentication methods at the token endpoint.
const (
	ClientAuthBasic = "client_secret_basic"
	ClientAuthPost  = "client_secret_post"
	ClientAuthNone  = "none"
)

// OIDC grant types a client can be allowed.
const (
	GrantAuthorizationCode = "authorization_code"
	GrantRefreshToken      = "refresh_token"
)

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,100}$`)

// NewOIDCClient describes a client to register.
type NewOIDCClient struct {
	ID   string
	Name string
	// Public clients (native and browser apps) have no secret and must use
	// PKCE with S256. Confidential clients authenticate with a secret.
	Public bool
	// AuthMethod for confidential clients: ClientAuthBasic (default) or
	// ClientAuthPost.
	AuthMethod string
	// Native marks a public client as a native app (custom schemes and
	// loopback redirect URIs) rather than a browser app.
	Native                 bool
	RedirectURIs           []string
	PostLogoutRedirectURIs []string
	// RefreshTokens allows the refresh_token grant (offline_access).
	RefreshTokens bool
}

// CreateOIDCClient registers a client and returns it with its secret (empty
// for public clients). The secret is returned only here; the database keeps
// its SHA-256 hash.
func (s *Service) CreateOIDCClient(ctx context.Context, actor audit.Actor, in NewOIDCClient) (store.OidcClient, string, error) {
	var client store.OidcClient
	var secret string
	err := s.inTx(ctx, func(q *store.Queries) error {
		var err error
		client, secret, err = s.CreateOIDCClientTx(ctx, q, actor, in)
		return err
	})
	if err != nil {
		return store.OidcClient{}, "", err
	}
	return client, secret, nil
}

// CreateOIDCClientTx is CreateOIDCClient in the caller's transaction (q),
// so a client is registered together with what it belongs to (a managed
// app instance) or not at all. The audit entry is in the same transaction.
func (s *Service) CreateOIDCClientTx(ctx context.Context, q *store.Queries, actor audit.Actor, in NewOIDCClient) (store.OidcClient, string, error) {
	if !clientIDPattern.MatchString(in.ID) {
		return store.OidcClient{}, "", invalid("id", "must be 1-100 characters of A-Z, a-z, 0-9, '.', '_', '~' and '-'")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > 100 || !utf8.ValidString(name) {
		return store.OidcClient{}, "", invalid("name", "must be 1-100 characters")
	}
	params := store.CreateOIDCClientParams{
		ID:                     in.ID,
		Name:                   name,
		ApplicationType:        "web",
		AuthMethod:             in.AuthMethod,
		RedirectUris:           in.RedirectURIs,
		PostLogoutRedirectUris: in.PostLogoutRedirectURIs,
		GrantTypes:             []string{GrantAuthorizationCode},
	}
	if params.PostLogoutRedirectUris == nil {
		params.PostLogoutRedirectUris = []string{}
	}
	if in.RefreshTokens {
		params.GrantTypes = append(params.GrantTypes, GrantRefreshToken)
	}
	var secret string
	switch {
	case in.Public:
		if in.AuthMethod != "" && in.AuthMethod != ClientAuthNone {
			return store.OidcClient{}, "", invalid("auth_method", "public clients have no secret")
		}
		params.AuthMethod = ClientAuthNone
		params.ApplicationType = "user_agent"
		if in.Native {
			params.ApplicationType = "native"
		}
	default:
		if in.Native {
			return store.OidcClient{}, "", invalid("native", "native apps are public clients")
		}
		if params.AuthMethod == "" {
			params.AuthMethod = ClientAuthBasic
		}
		if params.AuthMethod != ClientAuthBasic && params.AuthMethod != ClientAuthPost {
			return store.OidcClient{}, "", invalid("auth_method", "must be %s or %s", ClientAuthBasic, ClientAuthPost)
		}
		secret = crypto.RandomToken(ClientSecretPrefix)
		params.SecretHash = crypto.HashToken(secret)
	}
	if len(in.RedirectURIs) == 0 {
		return store.OidcClient{}, "", invalid("redirect_uris", "at least one is required")
	}
	for _, u := range slices.Concat(in.RedirectURIs, in.PostLogoutRedirectURIs) {
		if err := validateRedirectURI(u, params.ApplicationType == "native", s.localDomain); err != nil {
			return store.OidcClient{}, "", invalid("redirect_uris", "%q: %v", u, err)
		}
	}

	client, err := q.CreateOIDCClient(ctx, params)
	if err != nil {
		if isUniqueViolation(err) {
			return store.OidcClient{}, "", invalid("id", "a client with this ID exists")
		}
		return store.OidcClient{}, "", fmt.Errorf("identity: create OIDC client: %w", err)
	}
	if _, err := audit.Record(ctx, q, audit.Entry{
		Actor:  actor,
		Action: audit.OIDCClientCreated,
		Target: audit.Target{Kind: audit.TargetOIDCClient, ID: client.ID},
		Metadata: map[string]any{
			"name": client.Name, "auth_method": client.AuthMethod, "redirect_uris": client.RedirectUris,
			"grant_types": client.GrantTypes,
		},
	}); err != nil {
		return store.OidcClient{}, "", err
	}
	return client, secret, nil
}

// validateRedirectURI accepts absolute URIs without fragments: https,
// http only for loopback hosts (local development and native apps, RFC
// 8252), and private-use schemes (com.example.app:/cb) for native apps.
// With a local domain (WithLocalServiceDomain), http is also accepted for
// that exact domain and its subdomains, written as lowercase DNS names:
// never user info, a trailing dot, another suffix or an IP address.
func validateRedirectURI(raw string, native bool, localDomain string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || len(raw) > 2048 {
		return fmt.Errorf("must be an absolute URI")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("must not contain a fragment")
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return fmt.Errorf("must have a host")
		}
	case "http":
		h := u.Hostname()
		switch {
		case h == "localhost" || h == "127.0.0.1" || h == "::1":
		case localDomain != "" && u.User == nil && validLocalHost(h) &&
			(h == localDomain || strings.HasSuffix(h, "."+localDomain)):
		default:
			return fmt.Errorf("http is only allowed for localhost")
		}
	default:
		if !native || !strings.Contains(u.Scheme, ".") {
			return fmt.Errorf("must use https (native apps may use a reverse-domain scheme)")
		}
	}
	return nil
}

// OIDCClient returns a client by ID.
func (s *Service) OIDCClient(ctx context.Context, id string) (store.OidcClient, error) {
	c, err := s.queries().GetOIDCClient(ctx, id)
	if err != nil {
		return store.OidcClient{}, notFound(err, "OIDC client")
	}
	return c, nil
}

// validLocalHost reports whether h is a lowercase DNS name of letters,
// digits and inner hyphens ending in the label "localhost".
func validLocalHost(h string) bool {
	if len(h) > 253 || !strings.HasSuffix(h, "localhost") {
		return false
	}
	labels := strings.Split(h, ".")
	if labels[len(labels)-1] != "localhost" {
		return false
	}
	for _, l := range labels {
		if !localLabel.MatchString(l) {
			return false
		}
	}
	return true
}

var localLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
