package oidcop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
)

// RefreshTokenPrefix starts every refresh token.
const RefreshTokenPrefix = "olrt_"

// authRequestTTL bounds the time from /authorize to the code exchange,
// including sign-in. Codes live at most this long (RFC 6749 recommends at
// most 10 minutes).
const authRequestTTL = 10 * time.Minute

// Lifetimes configure tokens.
type Lifetimes struct {
	AccessToken  time.Duration
	IDToken      time.Duration
	RefreshToken time.Duration
}

// storage implements op.Storage over the core database.
type storage struct {
	pool      *pgxpool.Pool
	keys      *KeyManager
	lifetimes Lifetimes
	loginURL  string
	logger    *slog.Logger
}

var (
	_ op.Storage                        = (*storage)(nil)
	_ op.CanSetUserinfoFromRequest      = (*storage)(nil)
	_ op.CanTerminateSessionFromRequest = (*storage)(nil)
)

func (s *storage) q() *store.Queries { return store.New(s.pool) }

func (s *storage) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return fn(store.New(tx)) })
}

// errNotFound is returned for unknown IDs; zitadel maps plain errors to the
// right protocol error per endpoint.
var errNotFound = errors.New("oidcop: not found")

func parseID(id string) (uuid.UUID, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return uuid.Nil, errNotFound
	}
	return u, nil
}

// --- clients ---

// GetClientByClientID implements op.OPStorage.
func (s *storage) GetClientByClientID(ctx context.Context, clientID string) (op.Client, error) {
	c, err := s.q().GetOIDCClient(ctx, clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: client %q", errNotFound, clientID)
	} else if err != nil {
		return nil, oidc.ErrServerError().WithParent(err)
	}
	return &client{row: c, loginURL: s.loginURL, idTokenLifetime: s.lifetimes.IDToken}, nil
}

// AuthorizeClientIDSecret implements op.OPStorage.
func (s *storage) AuthorizeClientIDSecret(ctx context.Context, clientID, clientSecret string) error {
	c, err := s.q().GetOIDCClient(ctx, clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("oidcop: invalid client credentials")
	} else if err != nil {
		return oidc.ErrServerError().WithParent(err)
	}
	if c.SecretHash == nil || !crypto.Equal(c.SecretHash, crypto.HashToken(clientSecret)) {
		return errors.New("oidcop: invalid client credentials")
	}
	return nil
}

// --- authorization requests and codes ---

// CreateAuthRequest implements op.AuthStorage. PKCE "plain" is refused, and
// public clients must use PKCE with S256.
func (s *storage) CreateAuthRequest(ctx context.Context, req *oidc.AuthRequest, _ string) (op.AuthRequest, error) {
	c, err := s.q().GetOIDCClient(ctx, req.ClientID)
	if err != nil {
		return nil, oidc.ErrInvalidClient().WithParent(err)
	}
	switch {
	case req.CodeChallenge != "" && req.CodeChallengeMethod != oidc.CodeChallengeMethodS256:
		return nil, oidc.ErrInvalidRequest().WithDescription("code_challenge_method must be S256")
	case req.CodeChallenge == "" && c.AuthMethod == identity.ClientAuthNone:
		return nil, oidc.ErrInvalidRequest().WithDescription("public clients must use PKCE with S256 (code_challenge)")
	}
	if len(req.LoginHint) > 254 {
		req.LoginHint = ""
	}
	params := store.CreateOIDCAuthRequestParams{
		ClientID:            req.ClientID,
		RedirectUri:         req.RedirectURI,
		Scopes:              nonNil(req.Scopes),
		State:               req.State,
		Nonce:               req.Nonce,
		ResponseType:        string(req.ResponseType),
		ResponseMode:        string(req.ResponseMode),
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: string(req.CodeChallengeMethod),
		Prompt:              nonNil(req.Prompt),
		LoginHint:           req.LoginHint,
		TtlSeconds:          authRequestTTL.Seconds(),
	}
	if req.MaxAge != nil {
		maxAge := int64(min(*req.MaxAge, 1<<31)) //nolint:gosec // bounded
		params.MaxAgeSeconds = &maxAge
	}
	row, err := s.q().CreateOIDCAuthRequest(ctx, params)
	if err != nil {
		return nil, oidc.ErrInvalidRequest().WithDescription("the authorization request could not be stored").WithParent(err)
	}
	return &AuthRequest{row: row}, nil
}

// AuthRequestByID implements op.AuthStorage.
func (s *storage) AuthRequestByID(ctx context.Context, id string) (op.AuthRequest, error) {
	return s.authRequest(ctx, id)
}

func (s *storage) authRequest(ctx context.Context, id string) (*AuthRequest, error) {
	uid, err := parseID(id)
	if err != nil {
		return nil, err
	}
	row, err := s.q().GetOIDCAuthRequest(ctx, uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: authorization request", errNotFound)
	} else if err != nil {
		return nil, err
	}
	return &AuthRequest{row: row}, nil
}

// SaveAuthCode implements op.AuthStorage. Only the code's hash is stored.
func (s *storage) SaveAuthCode(ctx context.Context, id, code string) error {
	uid, err := parseID(id)
	if err != nil {
		return err
	}
	n, err := s.q().SetOIDCAuthCode(ctx, store.SetOIDCAuthCodeParams{ID: uid, CodeHash: crypto.HashToken(code)})
	if err != nil {
		return err
	}
	if n == 0 {
		return oidc.ErrInvalidRequest().WithDescription("the authorization request is not complete or already has a code")
	}
	return nil
}

// AuthRequestByCode implements op.AuthStorage. A code can be exchanged once:
// claiming it creates its grant in the same transaction, and presenting it
// again revokes the grant with every token issued for it, including tokens
// an exchange still in flight is about to issue (see lockGrant).
func (s *storage) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	hash := crypto.HashToken(code)
	var row store.OidcAuthRequest
	err := s.inTx(ctx, func(q *store.Queries) error {
		var err error
		if row, err = q.ClaimOIDCAuthCode(ctx, hash); err != nil {
			return err
		}
		if row.PersonID == nil {
			return oidc.ErrInvalidGrant().WithDescription("the authorization request is not complete")
		}
		return q.CreateOIDCGrant(ctx, store.CreateOIDCGrantParams{ID: row.ID, ClientID: row.ClientID, PersonID: *row.PersonID})
	})
	if err == nil {
		return &AuthRequest{row: row}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if used, err := s.q().GetOIDCAuthRequestByCodeHash(ctx, hash); err == nil && used.CodeUsedAt != nil {
		if err := s.revokeGrant(ctx, used.ID, "code_reused"); err != nil {
			return nil, err
		}
		return nil, oidc.ErrInvalidGrant().WithDescription("the code was already used")
	}
	return nil, oidc.ErrInvalidGrant().WithDescription("invalid or expired code")
}

// DeleteAuthRequest implements op.AuthStorage. Requests whose code was
// exchanged are kept until they expire, for code reuse detection.
func (s *storage) DeleteAuthRequest(ctx context.Context, id string) error {
	uid, err := parseID(id)
	if err != nil {
		return err
	}
	return s.q().DeletePendingOIDCAuthRequest(ctx, uid)
}

// --- tokens ---

// tokenGrant collects what a token is issued for.
type tokenGrant struct {
	grantID     uuid.UUID
	clientID    string
	personID    uuid.UUID
	scopes      []string
	audience    []string
	authTime    time.Time
	authMethods []string
}

func grantOf(req op.TokenRequest) (tokenGrant, error) {
	switch r := req.(type) {
	case *AuthRequest:
		if r.row.PersonID == nil || r.row.AuthTime == nil {
			return tokenGrant{}, oidc.ErrInvalidGrant().WithDescription("the authorization request is not complete")
		}
		return tokenGrant{
			grantID: r.row.ID, clientID: r.row.ClientID, personID: *r.row.PersonID,
			scopes: r.row.Scopes, audience: r.GetAudience(), authTime: *r.row.AuthTime, authMethods: r.row.AuthMethods,
		}, nil
	case *refreshTokenRequest:
		return tokenGrant{
			grantID: r.row.GrantID, clientID: r.row.ClientID, personID: r.row.PersonID,
			scopes: r.scopes, audience: r.row.Audience, authTime: r.row.AuthTime, authMethods: r.row.AuthMethods,
		}, nil
	default:
		return tokenGrant{}, oidc.ErrUnsupportedGrantType()
	}
}

func (s *storage) createAccessToken(ctx context.Context, q *store.Queries, g tokenGrant) (store.OidcAccessToken, error) {
	return q.CreateOIDCAccessToken(ctx, store.CreateOIDCAccessTokenParams{
		ClientID:   g.clientID,
		PersonID:   g.personID,
		GrantID:    g.grantID,
		Scopes:     nonNil(g.scopes),
		Audience:   nonNil(g.audience),
		TtlSeconds: s.lifetimes.AccessToken.Seconds(),
	})
}

// lockGrant locks the grant row for the rest of the transaction and fails
// if the grant was revoked. Every token is issued under this lock, and
// revokeGrant takes the same lock, so issuance and revocation of a grant
// are serialized.
func lockGrant(ctx context.Context, q *store.Queries, id uuid.UUID) error {
	g, err := q.LockOIDCGrant(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return oidc.ErrInvalidGrant().WithDescription("unknown grant")
	} else if err != nil {
		return err
	}
	if g.RevokedAt != nil {
		return oidc.ErrInvalidGrant().WithDescription("the grant was revoked")
	}
	return nil
}

// CreateAccessToken implements op.AuthStorage.
func (s *storage) CreateAccessToken(ctx context.Context, req op.TokenRequest) (string, time.Time, error) {
	g, err := grantOf(req)
	if err != nil {
		return "", time.Time{}, err
	}
	var t store.OidcAccessToken
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := lockGrant(ctx, q, g.grantID); err != nil {
			return err
		}
		t, err = s.createAccessToken(ctx, q, g)
		return err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return t.ID.String(), t.ExpiresAt, nil
}

// CreateAccessAndRefreshTokens implements op.AuthStorage. On a refresh, the
// presented token is marked used and replaced (rotation); the new token
// keeps the grant's absolute expiry.
func (s *storage) CreateAccessAndRefreshTokens(ctx context.Context, req op.TokenRequest, current string) (string, string, time.Time, error) {
	g, err := grantOf(req)
	if err != nil {
		return "", "", time.Time{}, err
	}
	refresh := crypto.RandomToken(RefreshTokenPrefix)
	var access store.OidcAccessToken
	reused := false
	err = s.inTx(ctx, func(q *store.Queries) error {
		if err := lockGrant(ctx, q, g.grantID); err != nil {
			return err
		}
		expires := time.Now().Add(s.lifetimes.RefreshToken)
		if current != "" {
			r, ok := req.(*refreshTokenRequest)
			if !ok {
				return oidc.ErrInvalidGrant()
			}
			used, err := q.UseOIDCRefreshToken(ctx, r.row.ID)
			if errors.Is(err, pgx.ErrNoRows) {
				reused = true // lost a race with another exchange of the same token
				return oidc.ErrInvalidGrant().WithDescription("the refresh token was already used")
			} else if err != nil {
				return err
			}
			expires = used.ExpiresAt
		}
		if _, err := q.CreateOIDCRefreshToken(ctx, store.CreateOIDCRefreshTokenParams{
			TokenHash:   crypto.HashToken(refresh),
			GrantID:     g.grantID,
			ClientID:    g.clientID,
			PersonID:    g.personID,
			Scopes:      nonNil(g.scopes),
			Audience:    nonNil(g.audience),
			AuthTime:    g.authTime,
			AuthMethods: nonNil(g.authMethods),
			ExpiresAt:   expires,
		}); err != nil {
			return err
		}
		access, err = s.createAccessToken(ctx, q, g)
		return err
	})
	if reused {
		if rerr := s.revokeGrant(ctx, g.grantID, "refresh_token_reused"); rerr != nil {
			return "", "", time.Time{}, rerr
		}
	}
	if err != nil {
		return "", "", time.Time{}, err
	}
	return access.ID.String(), refresh, access.ExpiresAt, nil
}

// TokenRequestByRefreshToken implements op.AuthStorage. A refresh token
// that was already used means it leaked or a client misbehaves: the whole
// grant is revoked.
func (s *storage) TokenRequestByRefreshToken(ctx context.Context, token string) (op.RefreshTokenRequest, error) {
	row, err := s.q().GetOIDCRefreshTokenByHash(ctx, crypto.HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, oidc.ErrInvalidGrant().WithDescription("invalid refresh token")
	} else if err != nil {
		return nil, err
	}
	switch {
	case row.RevokedAt != nil:
		return nil, oidc.ErrInvalidGrant().WithDescription("the refresh token was revoked")
	case row.UsedAt != nil:
		if err := s.revokeGrant(ctx, row.GrantID, "refresh_token_reused"); err != nil {
			return nil, err
		}
		return nil, oidc.ErrInvalidGrant().WithDescription("the refresh token was already used")
	case !time.Now().Before(row.ExpiresAt):
		return nil, oidc.ErrInvalidGrant().WithDescription("the refresh token expired")
	}
	if grant, err := s.q().GetOIDCGrant(ctx, row.GrantID); err != nil || grant.RevokedAt != nil {
		return nil, oidc.ErrInvalidGrant().WithDescription("the grant was revoked")
	}
	person, err := s.q().GetPerson(ctx, row.PersonID)
	if err != nil || identity.Status(person.Status) != identity.StatusActive {
		return nil, oidc.ErrInvalidGrant().WithDescription("the account is not active")
	}
	return &refreshTokenRequest{row: row, scopes: row.Scopes}, nil
}

// revokeGrant marks a grant revoked and revokes its tokens, under the grant
// lock: an exchange issuing tokens for the grant at the same time either
// finishes first (and its tokens are revoked here) or sees the revoked grant
// and fails.
func (s *storage) revokeGrant(ctx context.Context, grantID uuid.UUID, reason string) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.LockOIDCGrant(ctx, grantID); errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		grants, err := q.RevokeOIDCGrant(ctx, grantID)
		if err != nil {
			return err
		}
		refresh, err := q.RevokeOIDCGrantRefreshTokens(ctx, grantID)
		if err != nil {
			return err
		}
		access, err := q.RevokeOIDCGrantAccessTokens(ctx, grantID)
		if err != nil {
			return err
		}
		if grants+refresh+access == 0 {
			return nil
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:    audit.System,
			Action:   audit.OIDCGrantRevoked,
			Target:   audit.Target{Kind: audit.TargetOIDCGrant, ID: grantID.String()},
			Metadata: map[string]any{"reason": reason, "refresh_tokens": refresh, "access_tokens": access},
		})
		return err
	})
}

// TerminateSession implements op.AuthStorage. It is not called:
// TerminateSessionFromRequest takes precedence.
func (s *storage) TerminateSession(context.Context, string, string) error {
	return nil
}

// LogoutFunc ends the browser session of a validated RP-initiated logout
// request and returns where to send the browser. Package web supplies it
// through the request context (WithLogout), because only it sees the
// session cookie.
type LogoutFunc func(ctx context.Context, req *op.EndSessionRequest) (string, error)

type logoutKey struct{}

// WithLogout returns ctx carrying the logout function for the end_session
// endpoint.
func WithLogout(ctx context.Context, fn LogoutFunc) context.Context {
	return context.WithValue(ctx, logoutKey{}, fn)
}

// TerminateSessionFromRequest implements op.CanTerminateSessionFromRequest.
// zitadel calls it only after validating the request: the id_token_hint
// signature, the client and the post_logout_redirect_uri. Tokens held by
// the client stay valid until they expire or are revoked at the revocation
// endpoint.
func (s *storage) TerminateSessionFromRequest(ctx context.Context, req *op.EndSessionRequest) (string, error) {
	if fn, ok := ctx.Value(logoutKey{}).(LogoutFunc); ok {
		return fn(ctx, req)
	}
	return req.RedirectURI, nil
}

// RevokeToken implements op.AuthStorage (RFC 7009). Revoking a refresh
// token revokes its whole grant.
//
// zitadel passes a refresh token as GetRefreshTokenInfo returned it: the
// token itself (it has no other ID here), with the person as userID. An
// access token arrives as its ID, decrypted from the token. Anything else is
// a token this provider did not issue.
func (s *storage) RevokeToken(ctx context.Context, tokenOrID, userID, clientID string) *oidc.Error {
	if missing, _ := ctx.Value(missingTokenKey{}).(bool); missing {
		return oidc.ErrInvalidRequest().WithDescription("token missing")
	}
	if strings.HasPrefix(tokenOrID, RefreshTokenPrefix) {
		return s.revokeRefreshToken(ctx, tokenOrID, clientID)
	}
	if userID == "" {
		return nil // RFC 7009: invalid tokens are not an error
	}
	id, err := parseID(tokenOrID)
	if err != nil {
		return nil //nolint:nilerr // RFC 7009: invalid tokens are not an error
	}
	t, err := s.q().GetOIDCAccessToken(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return oidc.ErrServerError().WithParent(err)
	}
	if t.ClientID != clientID {
		return oidc.ErrInvalidClient().WithDescription("the token was issued to another client")
	}
	if _, err := s.q().RevokeOIDCAccessToken(ctx, id); err != nil {
		return oidc.ErrServerError().WithParent(err)
	}
	return nil
}

// zitadel's revocation endpoint (op.Revoke) does not check that a request
// has a token, which RFC 7009 (section 2.1) requires, and answers 200 as for
// an unknown token. Its newer op.Server does check, after authenticating the
// client. checkRevocationToken does the same for op.Revoke: it marks a
// request without a token, and RevokeToken, which zitadel calls only once
// the client is authenticated, answers invalid_request (RFC 6749, section
// 5.2). Client authentication errors keep their answers.
type missingTokenKey struct{}

// checkRevocationToken is an interceptor of the provider's router, so it
// runs inside its CORS handling. It parses the form of a revocation request,
// so a form that cannot be read is answered here, as op.Revoke would.
func checkRevocationToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathRevoke {
			if err := r.ParseForm(); err != nil {
				op.RevocationRequestError(w, r, oidc.ErrInvalidRequest().WithDescription("unable to parse request").WithParent(err))
				return
			}
			if r.Form.Get("token") == "" {
				r = r.WithContext(context.WithValue(r.Context(), missingTokenKey{}, true))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *storage) revokeRefreshToken(ctx context.Context, token, clientID string) *oidc.Error {
	row, err := s.q().GetOIDCRefreshTokenByHash(ctx, crypto.HashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return oidc.ErrServerError().WithParent(err)
	}
	if row.ClientID != clientID {
		return oidc.ErrInvalidClient().WithDescription("the token was issued to another client")
	}
	if err := s.revokeGrant(ctx, row.GrantID, "revoked"); err != nil {
		return oidc.ErrServerError().WithParent(err)
	}
	return nil
}

// GetRefreshTokenInfo implements op.AuthStorage. It returns the token itself
// as its ID, so RevokeToken can tell it from an access token ID.
func (s *storage) GetRefreshTokenInfo(ctx context.Context, _ string, token string) (string, string, error) {
	if !strings.HasPrefix(token, RefreshTokenPrefix) {
		return "", "", op.ErrInvalidRefreshToken
	}
	row, err := s.q().GetOIDCRefreshTokenByHash(ctx, crypto.HashToken(token))
	if err != nil {
		return "", "", op.ErrInvalidRefreshToken
	}
	return row.PersonID.String(), token, nil
}

// --- keys ---

// SigningKey implements op.AuthStorage.
func (s *storage) SigningKey(ctx context.Context) (op.SigningKey, error) {
	return s.keys.SigningKey(ctx)
}

// SignatureAlgorithms implements op.AuthStorage.
func (s *storage) SignatureAlgorithms(context.Context) ([]jose.SignatureAlgorithm, error) {
	return []jose.SignatureAlgorithm{jose.RS256}, nil
}

// KeySet implements op.AuthStorage.
func (s *storage) KeySet(ctx context.Context) ([]op.Key, error) {
	return s.keys.PublicKeys(ctx)
}

// GetKeyByIDAndClientID implements op.OPStorage. JWT client authentication
// (private_key_jwt) and the JWT profile grant are not supported.
func (s *storage) GetKeyByIDAndClientID(context.Context, string, string) (*jose.JSONWebKey, error) {
	return nil, errors.New("oidcop: JWT client authentication is not supported")
}

// ValidateJWTProfileScopes implements op.OPStorage.
func (s *storage) ValidateJWTProfileScopes(context.Context, string, []string) ([]string, error) {
	return nil, errors.New("oidcop: the JWT profile grant is not supported")
}

// --- claims ---

// SetUserinfoFromScopes implements op.OPStorage; superseded by
// SetUserinfoFromRequest.
func (s *storage) SetUserinfoFromScopes(context.Context, *oidc.UserInfo, string, string, []string) error {
	return nil
}

// SetUserinfoFromRequest implements op.CanSetUserinfoFromRequest (claims in
// the ID token, for clients that ask for them there).
func (s *storage) SetUserinfoFromRequest(ctx context.Context, info *oidc.UserInfo, req op.IDTokenRequest, scopes []string) error {
	id, err := parseID(req.GetSubject())
	if err != nil {
		return err
	}
	return s.setUserinfo(ctx, info, id, scopes)
}

// SetUserinfoFromToken implements op.OPStorage (the userinfo endpoint).
func (s *storage) SetUserinfoFromToken(ctx context.Context, info *oidc.UserInfo, tokenID, subject, _ string) error {
	t, err := s.validAccessToken(ctx, tokenID, subject)
	if err != nil {
		return err
	}
	return s.setUserinfo(ctx, info, t.PersonID, t.Scopes)
}

// SetIntrospectionFromToken implements op.OPStorage.
func (s *storage) SetIntrospectionFromToken(ctx context.Context, resp *oidc.IntrospectionResponse, tokenID, subject, clientID string) error {
	t, err := s.validAccessToken(ctx, tokenID, subject)
	if err != nil {
		return err
	}
	if t.ClientID != clientID && !slices.Contains(t.Audience, clientID) {
		return errors.New("oidcop: the token was issued to another client")
	}
	info := new(oidc.UserInfo)
	if err := s.setUserinfo(ctx, info, t.PersonID, t.Scopes); err != nil {
		return err
	}
	resp.SetUserInfo(info)
	resp.Scope = t.Scopes
	resp.ClientID = t.ClientID
	resp.Audience = t.Audience
	resp.Expiration = oidc.FromTime(t.ExpiresAt)
	resp.IssuedAt = oidc.FromTime(t.CreatedAt)
	resp.TokenType = oidc.BearerToken
	resp.Active = true
	return nil
}

func (s *storage) validAccessToken(ctx context.Context, tokenID, subject string) (store.OidcAccessToken, error) {
	id, err := parseID(tokenID)
	if err != nil {
		return store.OidcAccessToken{}, err
	}
	t, err := s.q().GetValidOIDCAccessToken(ctx, id)
	if err != nil || t.PersonID.String() != subject {
		return store.OidcAccessToken{}, errors.New("oidcop: invalid access token")
	}
	return t, nil
}

func (s *storage) setUserinfo(ctx context.Context, info *oidc.UserInfo, personID uuid.UUID, scopes []string) error {
	p, err := s.q().GetPerson(ctx, personID)
	if err != nil {
		return err
	}
	info.Subject = p.ID.String()
	for _, scope := range scopes {
		switch scope {
		case oidc.ScopeProfile:
			info.Name = p.DisplayName
			info.PreferredUsername = p.Username
			info.UpdatedAt = oidc.FromTime(p.UpdatedAt)
		case oidc.ScopeEmail:
			info.Email = p.WorkEmail
			// Work emails are set by the company's admins, not
			// self-asserted.
			info.EmailVerified = true
		}
	}
	return nil
}

// GetPrivateClaimsFromScopes implements op.OPStorage.
func (s *storage) GetPrivateClaimsFromScopes(context.Context, string, string, []string) (map[string]any, error) {
	return nil, nil
}

// Health implements op.Storage.
func (s *storage) Health(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
