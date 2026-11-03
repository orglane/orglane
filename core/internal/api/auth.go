package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/core/internal/store"
)

// apiError is returned by handlers for expected failures; the response
// error handler renders it as problem details with its status.
type apiError struct {
	status     int
	detail     string
	retryAfter string
}

func (e *apiError) Error() string { return e.detail }

func problemErr(status int, detail string) error { return &apiError{status: status, detail: detail} }

// httpKey carries the request and response writer into strict handlers,
// which need cookies, headers and the client address.
type httpKey struct{}

type httpPair struct {
	w http.ResponseWriter
	r *http.Request
}

func withHTTP(f apigen.StrictHandlerFunc, _ string) apigen.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		return f(context.WithValue(ctx, httpKey{}, httpPair{w: w, r: r}), w, r, req)
	}
}

func httpFrom(ctx context.Context) (http.ResponseWriter, *http.Request) {
	p, _ := ctx.Value(httpKey{}).(httpPair)
	return p.w, p.r
}

func (s *server) auth() (*web.Auth, error) {
	if s.opts.Auth == nil {
		return nil, errors.New("api: identity is not configured")
	}
	return s.opts.Auth, nil
}

// callerID returns the person ID of the authenticated caller.
func callerID(ctx context.Context) (uuid.UUID, error) {
	p := authz.PrincipalFrom(ctx)
	if p == nil {
		return uuid.Nil, problemErr(http.StatusUnauthorized, "")
	}
	id, err := uuid.Parse(p.Subject)
	if err != nil {
		return uuid.Nil, problemErr(http.StatusForbidden, "This operation is only available to people.")
	}
	return id, nil
}

func (s *server) GetOrglaneDiscovery(context.Context, apigen.GetOrglaneDiscoveryRequestObject) (apigen.GetOrglaneDiscoveryResponseObject, error) {
	issuer := s.opts.WorkspaceURL
	if s.opts.Auth != nil && s.opts.Auth.Issuer() != "" {
		issuer = s.opts.Auth.Issuer()
	}
	return apigen.GetOrglaneDiscovery200JSONResponse{
		WorkspaceUrl: s.opts.WorkspaceURL,
		ApiBaseUrl:   s.opts.WorkspaceURL + "/api/v1",
		Issuer:       issuer,
		Version:      s.opts.Version,
		Demo:         s.opts.Demo != nil,
	}, nil
}

func (s *server) GetSetupStatus(ctx context.Context, _ apigen.GetSetupStatusRequestObject) (apigen.GetSetupStatusResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	required, err := a.Identity().SetupRequired(ctx)
	if err != nil {
		return nil, err
	}
	return apigen.GetSetupStatus200JSONResponse{Required: required}, nil
}

func (s *server) CompleteSetup(ctx context.Context, req apigen.CompleteSetupRequestObject) (apigen.CompleteSetupResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	w, r := httpFrom(ctx)
	if err := a.AllowSignIn(ctx, r, identity.AuthPassword); err != nil {
		return nil, signInError(err)
	}
	b := req.Body
	person, err := a.Identity().CompleteSetup(ctx, a.Actor(r, uuid.Nil), b.SetupToken, identity.NewOwner{
		Username: b.Username, WorkEmail: b.WorkEmail, DisplayName: b.DisplayName, Password: b.Password,
	})
	switch {
	case errors.Is(err, identity.ErrInvalidSetupToken):
		return nil, problemErr(http.StatusForbidden, "The setup token is invalid. Use the token from the server log or run `orglane cli setup-token`.")
	case errors.Is(err, identity.ErrSetupDone):
		return nil, problemErr(http.StatusConflict, "Setup is already complete.")
	case err != nil:
		return nil, inputError(err)
	}
	result, err := s.signIn(ctx, a, w, r, person, identity.AuthPassword)
	if err != nil {
		return nil, err
	}
	return apigen.CompleteSetup201JSONResponse(result), nil
}

func (s *server) SignInWithPassword(ctx context.Context, req apigen.SignInWithPasswordRequestObject) (apigen.SignInWithPasswordResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	w, r := httpFrom(ctx)
	if err := a.AllowSignIn(ctx, r, identity.AuthPassword); err != nil {
		return nil, signInError(err)
	}
	person, err := a.Identity().VerifyPassword(ctx, a.Actor(r, uuid.Nil), req.Body.Email, req.Body.Password)
	if err != nil {
		return nil, signInError(err)
	}
	result, err := s.signIn(ctx, a, w, r, person, identity.AuthPassword)
	if err != nil {
		return nil, err
	}
	return apigen.SignInWithPassword200JSONResponse(result), nil
}

func (s *server) BeginPasskeySignIn(ctx context.Context, _ apigen.BeginPasskeySignInRequestObject) (apigen.BeginPasskeySignInResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	_, r := httpFrom(ctx)
	if err := a.AllowSignIn(ctx, r, identity.AuthPasskey); err != nil {
		return nil, signInError(err)
	}
	id, options, err := a.Identity().BeginPasskeySignIn(ctx)
	if err != nil {
		return nil, err
	}
	publicKey, err := toObject(options.Response)
	if err != nil {
		return nil, err
	}
	return apigen.BeginPasskeySignIn200JSONResponse{CeremonyId: id, PublicKey: publicKey}, nil
}

func (s *server) SignInWithPasskey(ctx context.Context, req apigen.SignInWithPasskeyRequestObject) (apigen.SignInWithPasskeyResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	w, r := httpFrom(ctx)
	if err := a.AllowSignIn(ctx, r, identity.AuthPasskey); err != nil {
		return nil, signInError(err)
	}
	credential, err := json.Marshal(req.Body.Credential)
	if err != nil {
		return nil, problemErr(http.StatusBadRequest, "The credential is malformed.")
	}
	person, err := a.Identity().FinishPasskeySignIn(ctx, a.Actor(r, uuid.Nil), req.Body.CeremonyId, credential)
	if err != nil {
		return nil, signInError(err)
	}
	result, err := s.signIn(ctx, a, w, r, person, identity.AuthPasskey)
	if err != nil {
		return nil, err
	}
	return apigen.SignInWithPasskey200JSONResponse(result), nil
}

func (s *server) signIn(ctx context.Context, a *web.Auth, w http.ResponseWriter, r *http.Request, person store.Person, method identity.AuthMethod) (apigen.SignInResult, error) {
	signedIn, err := a.SignIn(w, r, person, method)
	if errors.Is(err, identity.ErrInactive) {
		return apigen.SignInResult{}, problemErr(http.StatusUnauthorized, "The email or password is incorrect.")
	} else if err != nil {
		return apigen.SignInResult{}, err
	}
	offer, err := a.Identity().PasskeyOffer(ctx, person.ID)
	if err != nil {
		return apigen.SignInResult{}, err
	}
	return apigen.SignInResult{
		Person:           personSummary(signedIn.Person),
		CsrfToken:        a.CSRFToken(signedIn.Session.ID),
		PasskeyOffer:     offer,
		SessionExpiresAt: signedIn.Session.ExpiresAt,
	}, nil
}

func (s *server) SignOut(ctx context.Context, _ apigen.SignOutRequestObject) (apigen.SignOutResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	sessionID, err := uuid.Parse(authz.PrincipalFrom(ctx).SessionID)
	if err != nil {
		return nil, problemErr(http.StatusBadRequest, "Only browser sessions can sign out; revoke access tokens at the OIDC revocation endpoint.")
	}
	w, r := httpFrom(ctx)
	if err := a.Identity().RevokeOwnSession(ctx, a.Actor(r, personID), personID, sessionID, "signed_out"); err != nil {
		return nil, err
	}
	a.ClearSessionCookie(w)
	return apigen.SignOut204Response{}, nil
}

func (s *server) GetMe(ctx context.Context, _ apigen.GetMeRequestObject) (apigen.GetMeResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	person, err := a.Identity().GetPerson(ctx, personID)
	if err != nil {
		return nil, err
	}
	principal := authz.PrincipalFrom(ctx)
	me := apigen.Me{Person: personSummary(person), Permissions: make([]string, 0, len(principal.Permissions))}
	for _, p := range principal.Permissions {
		me.Permissions = append(me.Permissions, string(p))
	}
	if principal.SessionID != "" {
		sessions, err := s.mySessions(ctx, a, personID)
		if err != nil {
			return nil, err
		}
		if i := slices.IndexFunc(sessions, func(s apigen.Session) bool { return s.Current }); i >= 0 {
			me.Session = &sessions[i]
			token := a.CSRFToken(sessions[i].Id)
			me.CsrfToken = &token
		}
		if me.PasskeyOffer, err = a.Identity().PasskeyOffer(ctx, personID); err != nil {
			return nil, err
		}
	}
	pending, err := a.Identity().WelcomePending(ctx, personID)
	if err != nil {
		return nil, err
	}
	me.WelcomePending = &pending
	return apigen.GetMe200JSONResponse(me), nil
}

func (s *server) mySessions(ctx context.Context, a *web.Auth, personID uuid.UUID) ([]apigen.Session, error) {
	rows, err := a.Identity().ValidSessions(ctx, personID, a.IdleTimeout())
	if err != nil {
		return nil, err
	}
	current := authz.PrincipalFrom(ctx).SessionID
	out := make([]apigen.Session, 0, len(rows))
	for _, row := range rows {
		methods := make([]apigen.SessionAuthMethods, 0, len(row.AuthMethods))
		for _, m := range row.AuthMethods {
			methods = append(methods, apigen.SessionAuthMethods(m))
		}
		out = append(out, apigen.Session{
			Id:          row.ID,
			Device:      apigen.Device{Id: row.DeviceID, Name: row.DeviceName, Platform: apigen.DevicePlatform(row.DevicePlatform)},
			CreatedAt:   row.CreatedAt,
			LastSeenAt:  row.LastSeenAt,
			ExpiresAt:   row.ExpiresAt,
			AuthMethods: methods,
			Current:     row.ID.String() == current,
		})
	}
	return out, nil
}

func (s *server) ListMySessions(ctx context.Context, _ apigen.ListMySessionsRequestObject) (apigen.ListMySessionsResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	sessions, err := s.mySessions(ctx, a, personID)
	if err != nil {
		return nil, err
	}
	return apigen.ListMySessions200JSONResponse{Items: sessions}, nil
}

func (s *server) RevokeMySession(ctx context.Context, req apigen.RevokeMySessionRequestObject) (apigen.RevokeMySessionResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	w, r := httpFrom(ctx)
	err = a.Identity().RevokeOwnSession(ctx, a.Actor(r, personID), personID, req.SessionId, "revoked")
	if errors.Is(err, identity.ErrNotFound) {
		return nil, problemErr(http.StatusNotFound, "")
	} else if err != nil {
		return nil, err
	}
	if authz.PrincipalFrom(ctx).SessionID == req.SessionId.String() {
		a.ClearSessionCookie(w)
	}
	return apigen.RevokeMySession204Response{}, nil
}

func (s *server) ListMyPasskeys(ctx context.Context, _ apigen.ListMyPasskeysRequestObject) (apigen.ListMyPasskeysResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := a.Identity().Passkeys(ctx, personID)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.Passkey, 0, len(rows))
	for _, pk := range rows {
		items = append(items, passkey(pk))
	}
	return apigen.ListMyPasskeys200JSONResponse{Items: items}, nil
}

func (s *server) BeginPasskeyRegistration(ctx context.Context, _ apigen.BeginPasskeyRegistrationRequestObject) (apigen.BeginPasskeyRegistrationResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	id, options, err := a.Identity().BeginPasskeyRegistration(ctx, personID)
	if err != nil {
		return nil, err
	}
	publicKey, err := toObject(options.Response)
	if err != nil {
		return nil, err
	}
	return apigen.BeginPasskeyRegistration200JSONResponse{CeremonyId: id, PublicKey: publicKey}, nil
}

func (s *server) AddMyPasskey(ctx context.Context, req apigen.AddMyPasskeyRequestObject) (apigen.AddMyPasskeyResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	credential, err := json.Marshal(req.Body.Credential)
	if err != nil {
		return nil, problemErr(http.StatusBadRequest, "The credential is malformed.")
	}
	name := ""
	if req.Body.Name != nil {
		name = *req.Body.Name
	}
	_, r := httpFrom(ctx)
	pk, err := a.Identity().FinishPasskeyRegistration(ctx, a.Actor(r, personID), personID, req.Body.CeremonyId, name, credential)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, problemErr(http.StatusBadRequest, "The passkey ceremony expired or was already used. Start again.")
	} else if err != nil {
		return nil, inputError(err)
	}
	return apigen.AddMyPasskey201JSONResponse(passkey(pk)), nil
}

func (s *server) RemoveMyPasskey(ctx context.Context, req apigen.RemoveMyPasskeyRequestObject) (apigen.RemoveMyPasskeyResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	_, r := httpFrom(ctx)
	err = a.Identity().RemovePasskey(ctx, a.Actor(r, personID), personID, req.PasskeyId)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, problemErr(http.StatusNotFound, "")
	} else if err != nil {
		return nil, err
	}
	return apigen.RemoveMyPasskey204Response{}, nil
}

func (s *server) DismissPasskeyOffer(ctx context.Context, _ apigen.DismissPasskeyOfferRequestObject) (apigen.DismissPasskeyOfferResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	_, r := httpFrom(ctx)
	if err := a.Identity().DismissPasskeyOffer(ctx, a.Actor(r, personID), personID); err != nil {
		return nil, err
	}
	return apigen.DismissPasskeyOffer204Response{}, nil
}

// signInError maps sign-in failures to responses. Wrong email, password or
// passkey are indistinguishable.
func signInError(err error) error {
	var limited *web.ErrRateLimited
	var locked *identity.LockedError
	switch {
	case errors.As(err, &limited):
		return &apiError{status: http.StatusTooManyRequests, detail: "Too many sign-in attempts. Try again later.", retryAfter: seconds(limited.RetryAfter.Seconds())}
	case errors.As(err, &locked):
		return &apiError{status: http.StatusTooManyRequests, detail: "Too many failed sign-in attempts for this account. Try again later.", retryAfter: seconds(locked.RetryAfter.Seconds())}
	case errors.Is(err, identity.ErrInvalidCredentials):
		return problemErr(http.StatusUnauthorized, "The email or password is incorrect.")
	case errors.Is(err, identity.ErrPasskeysDisabled):
		return problemErr(http.StatusBadRequest, "Passkeys are not available on this instance.")
	default:
		return err
	}
}

// inputError maps validation errors to 400 and conflicts to 409.
func inputError(err error) error {
	switch {
	case errors.Is(err, identity.ErrInvalid), errors.Is(err, identity.ErrWeakPassword):
		return problemErr(http.StatusBadRequest, err.Error())
	case errors.Is(err, identity.ErrEmailTaken), errors.Is(err, identity.ErrUsernameTaken):
		return problemErr(http.StatusConflict, err.Error())
	default:
		return err
	}
}

func seconds(s float64) string {
	n := int(s + 0.999)
	if n < 1 {
		n = 1
	}
	return itoa(n)
}

func personSummary(p store.Person) apigen.PersonSummary {
	return apigen.PersonSummary{Id: p.ID, Username: p.Username, WorkEmail: p.WorkEmail, DisplayName: p.DisplayName}
}

func passkey(pk store.Passkey) apigen.Passkey {
	return apigen.Passkey{Id: pk.ID, Name: pk.Name, CreatedAt: pk.CreatedAt, LastUsedAt: pk.LastUsedAt}
}

// toObject converts WebAuthn options to a JSON object.
func toObject(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
