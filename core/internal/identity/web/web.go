// Package web is the browser side of identity: session and device cookies,
// CSRF protection, the client IP behind trusted proxies, per-IP sign-in rate
// limiting, the API authenticator (session cookie or OIDC bearer token) and
// the browser side of OIDC sign-in (the handoff to the web UI's sign-in
// screen).
//
// Cookies are host-only (no Domain attribute, ADR 0018), HttpOnly and
// SameSite=Lax. Over HTTPS they carry the __Host- prefix and Secure; in the
// local plain-HTTP demo mode (domain.scheme: http, development only) they
// are named without the prefix, which browsers reject without Secure:
//
//	session: __Host-orglane_session (https) / orglane_session (http)
//	device:  __Host-orglane_device  (https) / orglane_device  (http)
//
// The session cookie holds an opaque random token; the database stores only
// its hash and checks it on every request, so revocation is immediate. The
// device cookie holds the ID of the browser's device record, reused when the
// same person signs in again.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/oidcop"
	"github.com/orglane/orglane/core/internal/store"
)

// Cookie names (see the package documentation).
const (
	SecureSessionCookie   = "__Host-orglane_session"
	InsecureSessionCookie = "orglane_session"
	SecureDeviceCookie    = "__Host-orglane_device"
	InsecureDeviceCookie  = "orglane_device"
)

// CSRFHeader carries the CSRF token on state-changing requests
// authenticated with the session cookie.
const CSRFHeader = "X-CSRF-Token"

// deviceCookieLifetime is the longest cookie lifetime browsers accept.
const deviceCookieLifetime = 400 * 24 * time.Hour

// Config configures Auth.
type Config struct {
	// WorkspaceURL is the origin browsers use, e.g. https://work.acme.com.
	WorkspaceURL string
	// IdleTimeout and AbsoluteTimeout bound sessions.
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
	// IPLimit sign-in attempts are allowed per client IP and IPWindow.
	IPLimit  int
	IPWindow time.Duration
	// TrustedProxies may set X-Forwarded-For.
	TrustedProxies []netip.Prefix
}

// Auth ties identity to HTTP.
type Auth struct {
	cfg      Config
	origin   string
	secure   bool
	identity *identity.Service
	provider *oidcop.Provider
	csrfKey  [32]byte
	limiter  *Limiter
	logger   *slog.Logger
}

// New returns Auth. provider may be nil when OIDC is not served (tests).
func New(cfg Config, svc *identity.Service, provider *oidcop.Provider, keyring *crypto.Keyring, logger *slog.Logger) (*Auth, error) {
	u, err := url.Parse(cfg.WorkspaceURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("web: invalid workspace URL")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Auth{
		cfg:      cfg,
		origin:   u.Scheme + "://" + u.Host,
		secure:   u.Scheme == "https",
		identity: svc,
		provider: provider,
		csrfKey:  keyring.Key(crypto.PurposeCSRF),
		limiter:  NewLimiter(cfg.IPLimit, cfg.IPWindow),
		logger:   logger,
	}, nil
}

// Identity returns the identity service.
func (a *Auth) Identity() *identity.Service { return a.identity }

// Origin returns the workspace origin.
func (a *Auth) Origin() string { return a.origin }

// SessionCookieName returns the session cookie name for the scheme.
func (a *Auth) SessionCookieName() string {
	if a.secure {
		return SecureSessionCookie
	}
	return InsecureSessionCookie
}

// DeviceCookieName returns the device cookie name for the scheme.
func (a *Auth) DeviceCookieName() string {
	if a.secure {
		return SecureDeviceCookie
	}
	return InsecureDeviceCookie
}

func (a *Auth) cookie(name, value string, expires time.Time) *http.Cookie {
	// Secure is off only in the plain-HTTP demo mode (development only).
	c := &http.Cookie{ //nolint:gosec // see above
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	}
	if expires.IsZero() {
		c.MaxAge = -1
	} else {
		c.Expires = expires.UTC()
	}
	return c
}

// SetSessionCookies sets the session and device cookies after sign-in.
func (a *Auth) SetSessionCookies(w http.ResponseWriter, s identity.SignedIn) {
	http.SetCookie(w, a.cookie(a.SessionCookieName(), s.Token, s.Session.ExpiresAt))
	http.SetCookie(w, a.cookie(a.DeviceCookieName(), s.Device.ID.String(), time.Now().Add(deviceCookieLifetime)))
}

// ClearSessionCookie removes the session cookie (the device cookie stays,
// so the browser keeps its device record).
func (a *Auth) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, a.cookie(a.SessionCookieName(), "", time.Time{}))
}

// sessionToken returns the session cookie value, if any.
func (a *Auth) sessionToken(r *http.Request) string {
	c, err := r.Cookie(a.SessionCookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

// deviceID returns the device cookie value, if it is a UUID.
func (a *Auth) deviceID(r *http.Request) uuid.UUID {
	c, err := r.Cookie(a.DeviceCookieName())
	if err != nil {
		return uuid.Nil
	}
	id, err := uuid.Parse(c.Value)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// CSRFToken returns the CSRF token of a session: an HMAC of the session ID
// under a key derived from the master secret. It is stable for the
// session's lifetime and useless for any other session.
func (a *Auth) CSRFToken(sessionID uuid.UUID) string {
	mac := hmac.New(sha256.New, a.csrfKey[:])
	mac.Write([]byte("csrf:"))
	mac.Write(sessionID[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Auth) validCSRF(r *http.Request, sessionID uuid.UUID) bool {
	got := r.Header.Get(CSRFHeader)
	return got != "" && hmac.Equal([]byte(got), []byte(a.CSRFToken(sessionID)))
}

// SameOrigin reports whether a request comes from the workspace origin,
// by its Origin header or, without one, its Referer. Requests with neither
// are not same-origin.
func (a *Auth) SameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == a.origin
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && u.Scheme+"://"+u.Host == a.origin
	}
	return false
}

// CrossOrigin reports whether a request carries an Origin header for
// another origin. Browsers send Origin on every cross-origin request and on
// same-origin POSTs; requests without it come from non-browser clients.
func (a *Auth) CrossOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o != "" && o != a.origin
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// ClientIP returns the client address: the connection's peer, or, when the
// peer is a trusted proxy, the last untrusted address in X-Forwarded-For.
func (a *Auth) ClientIP(r *http.Request) netip.Addr {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		addr, _ := netip.ParseAddr(r.RemoteAddr)
		return addr.Unmap()
	}
	ip := peer.Addr().Unmap()
	if !a.trusted(ip) {
		return ip
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		hop = hop.Unmap()
		if !a.trusted(hop) {
			return hop
		}
		ip = hop
	}
	return ip
}

func (a *Auth) trusted(ip netip.Addr) bool {
	for _, p := range a.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Actor returns the audit actor for a request: the given person, or the
// system when personID is nil, with the client IP and User-Agent.
func (a *Auth) Actor(r *http.Request, personID uuid.UUID) audit.Actor {
	actor := audit.Actor{Kind: audit.ActorSystem, IP: a.ClientIP(r), UserAgent: r.UserAgent()}
	if personID != uuid.Nil {
		actor.Kind, actor.ID = audit.ActorPerson, personID.String()
	}
	return actor
}

// ErrRateLimited is returned when a client IP made too many sign-in
// attempts.
type ErrRateLimited struct {
	RetryAfter time.Duration
}

func (e *ErrRateLimited) Error() string { return "web: too many sign-in attempts from this address" }

// AllowSignIn counts a sign-in attempt (password, passkey or setup) against
// the client IP. Rejections are audited once per window.
func (a *Auth) AllowSignIn(ctx context.Context, r *http.Request, method identity.AuthMethod) error {
	ip := a.ClientIP(r)
	ok, retry, first := a.limiter.Allow(ip.String())
	if ok {
		return nil
	}
	if first {
		if err := a.identity.RecordSignInRateLimited(ctx, a.Actor(r, uuid.Nil), method); err != nil {
			a.logger.WarnContext(ctx, "audit rate-limited sign-in", "error", err)
		}
	}
	return &ErrRateLimited{RetryAfter: retry}
}

// SignIn starts a browser session for a person who authenticated with
// method and sets the cookies.
func (a *Auth) SignIn(w http.ResponseWriter, r *http.Request, person store.Person, method identity.AuthMethod) (identity.SignedIn, error) {
	signedIn, err := a.identity.StartSession(r.Context(), a.Actor(r, person.ID), person.ID, identity.SignInOptions{
		Method:     method,
		DeviceID:   a.deviceID(r),
		DeviceName: DeviceName(r.UserAgent()),
		Platform:   identity.PlatformWeb,
		TTL:        a.cfg.AbsoluteTimeout,
	})
	if err != nil {
		return identity.SignedIn{}, err
	}
	a.SetSessionCookies(w, signedIn)
	return signedIn, nil
}

// Session returns the valid session of the request's cookie, if any.
func (a *Auth) Session(r *http.Request) (store.Session, bool, error) {
	token := a.sessionToken(r)
	if token == "" {
		return store.Session{}, false, nil
	}
	s, err := a.identity.ValidateSession(r.Context(), token, a.cfg.IdleTimeout)
	if errors.Is(err, identity.ErrNotFound) {
		return store.Session{}, false, nil
	}
	if err != nil {
		return store.Session{}, false, err
	}
	return s, true, nil
}

// Authenticate implements authz.Authenticator: an OIDC access token in the
// Authorization header, or the session cookie. Cookie-authenticated
// requests with an unsafe method must come from the workspace origin and
// carry the session's CSRF token. Permissions come from the person's roles,
// resolved on every request.
func (a *Auth) Authenticate(r *http.Request) (*authz.Principal, error) {
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || a.provider == nil {
			return nil, authz.ErrUnauthenticated
		}
		t, err := a.provider.ValidateAccessToken(ctx, strings.TrimSpace(token))
		if err != nil {
			return nil, authz.ErrUnauthenticated
		}
		perms, err := a.identity.PersonPermissions(ctx, t.PersonID)
		if err != nil {
			return nil, err
		}
		return &authz.Principal{Subject: t.PersonID.String(), Permissions: perms, ClientID: t.ClientID}, nil
	}

	session, ok, err := a.Session(r)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, authz.ErrUnauthenticated
	}
	if !safeMethod(r.Method) && (!a.SameOrigin(r) || !a.validCSRF(r, session.ID)) {
		return nil, authz.ErrCrossSite
	}
	perms, err := a.identity.PersonPermissions(ctx, session.PersonID)
	if err != nil {
		return nil, err
	}
	return &authz.Principal{Subject: session.PersonID.String(), Permissions: perms, SessionID: session.ID.String()}, nil
}

// RejectCrossOrigin wraps next and answers 403 to state-changing requests
// whose Origin header names another origin. It guards the unauthenticated
// sign-in form against login CSRF; authenticated requests are checked more
// strictly by Authenticate.
func (a *Auth) RejectCrossOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safeMethod(r.Method) && a.CrossOrigin(r) {
			http.Error(w, "Cross-origin request rejected.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IdleTimeout returns the session idle timeout.
func (a *Auth) IdleTimeout() time.Duration { return a.cfg.IdleTimeout }

// Issuer returns the OIDC issuer, or "" without a provider.
func (a *Auth) Issuer() string {
	if a.provider == nil {
		return ""
	}
	return a.provider.Issuer()
}
