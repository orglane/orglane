package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/oidcop"
	"github.com/orglane/orglane/core/internal/store"
)

// OIDC sign-in in the browser.
//
// /oidc/authorize sends the browser to /oidc/login?authRequest=<id>. With a
// valid session that satisfies the request (no prompt=login, within
// max_age) the request completes at once and the browser continues to the
// client. With prompt=none the client gets login_required instead.
// Otherwise the browser goes to the web UI's sign-in screen
// (SignInPath?authRequest=<id>), which signs the person in through the API
// (password or passkey) and then returns to /oidc/login, where the new
// session completes the request.
//
// Browsers that cannot run the web UI (no JavaScript modules, such as the
// HtmlUnit browser of the OpenID Conformance Suite) get a basic form on the
// same screen that posts to /oidc/login: a password sign-in that completes
// the request directly, or returns to the sign-in screen with an error code
// (SignInError*).

// SignInPath is the web UI's sign-in route.
const SignInPath = "/sign-in"

// Error codes of the basic sign-in form, in the error query parameter of
// SignInPath.
const (
	SignInErrorCredentials = "credentials"
	SignInErrorLimited     = "limited"
	SignInErrorFailed      = "failed"
)

// OIDCHandler serves the OIDC protocol endpoints, the sign-in handoff and
// the session-ending wrapper around the end_session endpoint.
func (a *Auth) OIDCHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(oidcop.PathDiscovery, a.provider)
	mux.Handle(oidcop.PathPrefix, a.provider)
	// Authorization errors that cannot go back to the client (an unknown
	// client or redirect URI) are shown to the person as a page.
	mux.Handle(oidcop.PathAuthorize, htmlErrors(a.provider))
	mux.Handle(oidcop.PathCallback, htmlErrors(a.provider))
	// Responses carrying tokens or personal data must not be cached
	// (RFC 6749, section 5.1).
	for _, p := range []string{oidcop.PathToken, oidcop.PathUserinfo, oidcop.PathIntrospect} {
		mux.Handle(p, noStore(a.provider))
	}
	mux.Handle("GET "+oidcop.PathLogin, http.HandlerFunc(a.loginPage))
	mux.Handle("POST "+oidcop.PathLogin, a.RejectCrossOrigin(http.HandlerFunc(a.loginSubmit)))
	mux.Handle(oidcop.PathEndSession, http.HandlerFunc(a.endSession))
	mux.Handle("GET "+oidcop.PathLogout, http.HandlerFunc(a.logoutPage))
	mux.Handle("POST "+oidcop.PathLogout, http.HandlerFunc(a.logoutSubmit))
	return securityHeaders(mux)
}

// securityHeaders keeps protocol and login responses out of frames and
// caches.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *Auth) loginPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.URL.Query().Get("authRequest")
	req, err := a.provider.AuthRequest(ctx, id)
	if err != nil {
		a.renderError(w, http.StatusBadRequest, "This sign-in link has expired. Go back to the application and try again.")
		return
	}
	session, ok, err := a.Session(r)
	if err != nil {
		a.logger.ErrorContext(ctx, "validate session", "error", err)
		a.renderError(w, http.StatusInternalServerError, "Something went wrong. Try again later.")
		return
	}
	if ok && sessionSatisfies(session, req) {
		a.complete(w, r, req, session)
		return
	}
	if req.PromptNone() {
		a.provider.AuthRequestError(w, r, req, oidcop.ErrLoginRequired())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, a.SignInURL(id, req.LoginHint()), http.StatusSeeOther) //nolint:gosec // the workspace origin, not an open redirect
}

// SignInURL returns the web UI's sign-in screen for an auth request, with
// the client's login_hint to prefill the email.
func (a *Auth) SignInURL(authRequest, loginHint string) string {
	return a.signInURL(authRequest, loginHint, "")
}

func (a *Auth) signInURL(authRequest, email, errorCode string) string {
	v := url.Values{"authRequest": {authRequest}}
	if email != "" {
		v.Set("email", email)
	}
	if errorCode != "" {
		v.Set("error", errorCode)
	}
	return a.origin + SignInPath + "?" + v.Encode()
}

// loginSubmit handles the basic sign-in form (see above).
func (a *Auth) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		a.renderError(w, http.StatusBadRequest, "The form could not be read.")
		return
	}
	id, email := r.PostForm.Get("authRequest"), r.PostForm.Get("email")
	req, err := a.provider.AuthRequest(ctx, id)
	if err != nil {
		a.renderError(w, http.StatusBadRequest, "This sign-in link has expired. Go back to the application and try again.")
		return
	}
	back := func(code string) {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, a.signInURL(id, email, code), http.StatusSeeOther) //nolint:gosec // the workspace origin, not an open redirect
	}
	if err := a.AllowSignIn(ctx, r, identity.AuthPassword); err != nil {
		back(SignInErrorLimited)
		return
	}
	person, err := a.identity.VerifyPassword(ctx, a.Actor(r, uuid.Nil), email, r.PostForm.Get("password"))
	var locked *identity.LockedError
	switch {
	case errors.As(err, &locked):
		back(SignInErrorLimited)
		return
	case errors.Is(err, identity.ErrInvalidCredentials):
		back(SignInErrorCredentials)
		return
	case err != nil:
		a.logger.ErrorContext(ctx, "password sign-in", "error", err)
		back(SignInErrorFailed)
		return
	}
	signedIn, err := a.SignIn(w, r, person, identity.AuthPassword)
	if errors.Is(err, identity.ErrInactive) {
		back(SignInErrorCredentials)
		return
	} else if err != nil {
		a.logger.ErrorContext(ctx, "start session", "error", err)
		back(SignInErrorFailed)
		return
	}
	a.complete(w, r, req, signedIn.Session)
}

// sessionSatisfies reports whether an existing session may answer the
// request without a new sign-in. A session started after the request is
// the fresh authentication the request asked for: the sign-in screen
// creates one and comes back here. An older one is enough unless the
// client asked for a new sign-in (prompt=login, which zitadel also
// expresses as max_age=0) or the session is older than max_age.
func sessionSatisfies(s store.Session, req *oidcop.AuthRequest) bool {
	if s.CreatedAt.After(req.CreatedAt()) {
		return true
	}
	if req.PromptLogin() {
		return false
	}
	if maxAge, ok := req.MaxAge(); ok && time.Since(s.CreatedAt) > maxAge {
		return false
	}
	return true
}

// complete records the person for the request and continues the flow. It
// is the only place an authorization request completes, so the client
// access check here covers every sign-in to a client: a person it refuses
// goes back to the client with access_denied and no code.
func (a *Auth) complete(w http.ResponseWriter, r *http.Request, req *oidcop.AuthRequest, session store.Session) {
	if check := a.clientAccess.Load(); check != nil {
		ok, err := (*check)(r.Context(), req.GetClientID(), session.PersonID)
		if err != nil {
			a.logger.ErrorContext(r.Context(), "client access check", "client", req.GetClientID(), "error", err)
			a.renderError(w, http.StatusInternalServerError, "Something went wrong. Try again later.")
			return
		}
		if !ok {
			a.provider.AuthRequestError(w, r, req, oidcop.ErrAccessDenied())
			return
		}
	}
	if err := a.provider.CompleteAuthRequest(r.Context(), req.GetID(), session); err != nil {
		a.renderError(w, http.StatusBadRequest, "This sign-in link has expired. Go back to the application and try again.")
		return
	}
	// The callback is on the issuer; the request ID is query-escaped.
	http.Redirect(w, r, a.provider.CallbackURL(req.GetID()), http.StatusSeeOther) //nolint:gosec // not an open redirect
}

// RP-initiated logout.
//
// The provider validates the end_session request first (id_token_hint
// signature, client, post_logout_redirect_uri) and only then calls logout.
// An id_token_hint for the signed-in person proves the request comes from
// a client that person signed in to, so the session ends at once. Any other
// request (no hint, or a hint for someone else) could be a cross-site link,
// so the person confirms on /oidc/logout with a same-origin POST that
// carries the session's CSRF token (like every other cookie-authenticated
// change); the validated redirect target travels signed.

// logoutCSRFField is the confirmation form field that carries the
// session's CSRF token.
const logoutCSRFField = "csrf_token"

func (a *Auth) endSession(w http.ResponseWriter, r *http.Request) {
	a.provider.ServeHTTP(w, r.WithContext(oidcop.WithLogout(r.Context(), func(ctx context.Context, req *op.EndSessionRequest) (string, error) {
		session, ok, err := a.Session(r)
		if err != nil {
			return "", err
		}
		if !ok {
			return req.RedirectURI, nil // nothing to end
		}
		if req.IDTokenHintClaims != nil && req.IDTokenHintClaims.GetSubject() == session.PersonID.String() {
			if err := a.identity.RevokeOwnSession(ctx, a.Actor(r, session.PersonID), session.PersonID, session.ID, "oidc_logout"); err != nil {
				return "", err
			}
			a.ClearSessionCookie(w)
			return req.RedirectURI, nil
		}
		v := url.Values{"next": {req.RedirectURI}, "sig": {a.logoutSig(req.RedirectURI)}}
		return a.Origin() + oidcop.PathLogout + "?" + v.Encode(), nil
	})))
}

// logoutSig binds a validated post-logout redirect target to this
// instance, so /oidc/logout cannot be turned into an open redirect.
func (a *Auth) logoutSig(next string) string {
	mac := hmac.New(sha256.New, a.csrfKey[:])
	mac.Write([]byte("logout:"))
	mac.Write([]byte(next))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// logoutTarget returns the signed redirect target of a logout
// confirmation, or the workspace.
func (a *Auth) logoutTarget(v url.Values) string {
	next := v.Get("next")
	if next != "" && hmac.Equal([]byte(v.Get("sig")), []byte(a.logoutSig(next))) {
		return next
	}
	return a.Origin() + "/"
}

func (a *Auth) logoutPage(w http.ResponseWriter, r *http.Request) {
	session, ok, err := a.Session(r)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, "Something went wrong. Try again later.")
		return
	}
	if !ok {
		http.Redirect(w, r, a.logoutTarget(r.URL.Query()), http.StatusFound) //nolint:gosec // signed target
		return
	}
	render(w, http.StatusOK, logoutTemplate, logoutForm{
		Next:      r.URL.Query().Get("next"),
		Sig:       r.URL.Query().Get("sig"),
		CSRFField: logoutCSRFField,
		CSRFToken: a.CSRFToken(session.ID),
	})
}

func (a *Auth) logoutSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		a.renderError(w, http.StatusBadRequest, "The form could not be read.")
		return
	}
	// The session cookie authenticates this request: it must come from the
	// workspace itself and carry the session's CSRF token from the
	// confirmation page.
	if !a.SameOrigin(r) {
		a.renderError(w, http.StatusForbidden, "Cross-origin request rejected.")
		return
	}
	session, ok, err := a.Session(r)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, "Something went wrong. Try again later.")
		return
	}
	if ok {
		if !a.validCSRFToken(r.PostForm.Get(logoutCSRFField), session.ID) {
			a.renderError(w, http.StatusForbidden, "This sign-out confirmation has expired. Go back to the application and try again.")
			return
		}
		if err := a.identity.RevokeOwnSession(r.Context(), a.Actor(r, session.PersonID), session.PersonID, session.ID, "oidc_logout"); err != nil {
			a.logger.ErrorContext(r.Context(), "end session", "error", err)
			a.renderError(w, http.StatusInternalServerError, "Something went wrong. Try again later.")
			return
		}
		a.ClearSessionCookie(w)
	}
	http.Redirect(w, r, a.logoutTarget(r.PostForm), http.StatusSeeOther) //nolint:gosec // signed target
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// htmlErrors renders plain-text error responses of next (zitadel's answer
// when it must not redirect to the client) as the HTML error page.
func htmlErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &errorCapture{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		if cw.status != 0 {
			msg := strings.TrimSpace(cw.body.String())
			if msg == "" {
				msg = "unknown error"
			}
			render(w, cw.status, errorTemplate, "The application sent an invalid sign-in request: "+msg)
		}
	})
}

// errorCapture holds back plain-text error responses.
type errorCapture struct {
	http.ResponseWriter
	status int // set when a plain-text error is captured
	body   bytes.Buffer
}

func (c *errorCapture) WriteHeader(code int) {
	if code >= 400 && strings.HasPrefix(c.Header().Get("Content-Type"), "text/plain") {
		c.status = code
		c.Header().Del("Content-Type")
		c.Header().Del("X-Content-Type-Options")
		return
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *errorCapture) Write(b []byte) (int, error) {
	if c.status != 0 {
		if c.body.Len() < 4096 {
			c.body.Write(b)
		}
		return len(b), nil
	}
	return c.ResponseWriter.Write(b)
}

type logoutForm struct {
	Next      string
	Sig       string
	CSRFField string
	CSRFToken string
}

func (a *Auth) renderError(w http.ResponseWriter, status int, message string) {
	render(w, status, errorTemplate, message)
}

func render(w http.ResponseWriter, status int, t *template.Template, data any) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = t.Execute(w, data)
}

const pageStyle = `body{font-family:system-ui,sans-serif;max-width:22rem;margin:4rem auto;padding:0 1rem;color:#1a1a1a}
label{display:block;margin:1rem 0 .25rem}input{width:100%;padding:.5rem;box-sizing:border-box}
button{margin-top:1.5rem;padding:.6rem 1.2rem}.error{color:#b00020}`

var logoutTemplate = template.Must(template.New("logout").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign out · Orglane</title><style>` + pageStyle + `</style></head>
<body><main>
<h1>Sign out?</h1>
<p>An application asked to sign you out of Orglane in this browser.</p>
<form method="post" action="/oidc/logout">
<input type="hidden" name="next" value="{{.Next}}">
<input type="hidden" name="sig" value="{{.Sig}}">
<input type="hidden" name="{{.CSRFField}}" value="{{.CSRFToken}}">
<button id="confirm" type="submit">Sign out</button>
</form>
</main></body></html>
`))

var errorTemplate = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in · Orglane</title><style>` + pageStyle + `</style></head>
<body><main><h1>Sign in</h1><p class="error" role="alert">{{.}}</p></main></body></html>
`))
