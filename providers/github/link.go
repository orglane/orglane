package github

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/orglane/orglane/providers/provider"
)

// StepLinkAccount is the employee step that links a person's GitHub account.
const StepLinkAccount = "github.link-account"

// Realm is the account realm of every GitHub.com instance: a GitHub user ID
// is the same account in every organization.
const Realm = "github.com"

var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// BeginVerify returns GitHub's authorization URL for the instance's App
// (user authorization of a GitHub App, with PKCE S256). GitHub redirects
// back to the account callback route with code and state.
func (*Adapter) BeginVerify(_ context.Context, env provider.Env, inst provider.Instance, state, codeChallenge string) (*url.URL, error) {
	clientID := setting(inst, "app_client_id")
	if !clientIDPattern.MatchString(clientID) {
		return nil, protocolError("invalid_app")
	}
	if state == "" || codeChallenge == "" {
		return nil, protocolError("invalid_request")
	}
	u := &url.URL{Scheme: "https", Host: webHost, Path: "/login/oauth/authorize"}
	u.RawQuery = url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {accountCallback(env)},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	return u, nil
}

// CompleteVerify exchanges the authorization code (with the PKCE verifier)
// for a user token, reads the authenticated user with it and revokes the
// authorization, so the token is used exactly once and never kept. The
// account is identified by GitHub's immutable numeric user ID.
func (*Adapter) CompleteVerify(ctx context.Context, env provider.Env, inst provider.Instance, code, codeVerifier string) (provider.ExternalAccount, error) {
	clientID := setting(inst, "app_client_id")
	if !clientIDPattern.MatchString(clientID) {
		return provider.ExternalAccount{}, protocolError("invalid_app")
	}
	if !codePattern.MatchString(code) || codeVerifier == "" {
		return provider.ExternalAccount{}, protocolError("invalid_code")
	}
	secret, err := env.Credential(ctx, "app_client_secret")
	if err != nil {
		return provider.ExternalAccount{}, err
	}
	defer clear(secret)
	token, err := exchangeCode(ctx, env, clientID, secret, code, codeVerifier)
	if err != nil {
		return provider.ExternalAccount{}, err
	}
	defer revokeGrant(ctx, env, clientID, secret, token)
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := request(ctx, env, http.MethodGet, "/user", token, nil, &user); err != nil {
		return provider.ExternalAccount{}, err
	}
	if user.ID <= 0 || !loginPattern.MatchString(user.Login) || user.Type != "User" {
		return provider.ExternalAccount{}, protocolError("invalid_user")
	}
	return provider.ExternalAccount{Realm: Realm, ExternalID: strconv.FormatInt(user.ID, 10), Login: user.Login}, nil
}

// exchangeCode is the token step of GitHub's web application flow
// (https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app).
// GitHub answers errors with 200 and an error field; they are definite.
func exchangeCode(ctx context.Context, env provider.Env, clientID string, secret []byte, code, verifier string) (string, error) {
	var res struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	err := do(ctx, env, call{
		host: webHost, method: http.MethodPost, path: "/login/oauth/access_token", accept: "application/json",
		body: url.Values{
			"client_id": {clientID}, "client_secret": {string(secret)}, "code": {code},
			"redirect_uri": {accountCallback(env)}, "code_verifier": {verifier},
		},
		out: &res,
	})
	if err != nil {
		return "", err
	}
	switch {
	case res.Error == "bad_verification_code":
		return "", protocolError("invalid_code") // expired, used, or a PKCE mismatch
	case res.Error == "incorrect_client_credentials" || res.Error == "redirect_uri_mismatch":
		return "", protocolError("invalid_app")
	case res.Error != "":
		return "", protocolError("oauth_failed")
	case res.AccessToken == "" || res.TokenType != "bearer":
		return "", protocolError("invalid_response")
	}
	return res.AccessToken, nil
}

// revokeGrant deletes the user's authorization of the App, which revokes
// the access token and any refresh token issued with it. It runs even when
// the caller's request was canceled; a failure is logged, never returned:
// the token was never stored and expires on its own.
func revokeGrant(ctx context.Context, env provider.Env, clientID string, secret []byte, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := do(ctx, env, call{
		host: apiHost, method: http.MethodDelete, path: "/applications/" + url.PathEscape(clientID) + "/grant",
		authorization: basicAuth(clientID, secret), body: map[string]string{"access_token": token},
	})
	var api *APIError
	if err != nil && (!errors.As(err, &api) || api.Status != http.StatusNotFound) {
		code, _, _ := provider.Classify(err)
		env.Logger().WarnContext(ctx, "github: revoking a user authorization failed", "code", code)
	}
}

// basicAuth is the App's client credentials as an Authorization header.
func basicAuth(clientID string, secret []byte) string {
	raw := make([]byte, 0, len(clientID)+1+len(secret))
	raw = append(append(append(raw, clientID...), ':'), secret...)
	defer clear(raw)
	return "Basic " + base64.StdEncoding.EncodeToString(raw)
}

var _ provider.AccountVerifier = (*Adapter)(nil)

// AccountRealm is github.com for every instance: one GitHub user ID is the
// same account in every organization, so a link proven through one App
// holds for all of them.
func (*Adapter) AccountRealm(provider.Instance) string { return Realm }

var _ provider.AccountRealmer = (*Adapter)(nil)
