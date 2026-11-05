package github

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/github/githubfake"
	"github.com/orglane/orglane/providers/provider"
)

// fakeFixture is the adapter against the OpenAPI-validated fake GitHub.
func fakeFixture(t *testing.T) (*githubfake.Fake, *testEnv, provider.Instance) {
	t.Helper()
	f := githubfake.New(func(err error) { t.Error(err) })
	t.Cleanup(f.Close)
	app, org := f.App(), f.Org()
	env := &testEnv{now: time.Now(), client: f.Client(), credentials: map[string][]byte{
		"app_private_key": f.AppPrivateKeyPEM(), "app_client_secret": []byte(app.ClientSecret),
	}}
	inst := provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{
		"organization": org.Login, "organization_id": strconv.FormatInt(org.ID, 10), "app_id": strconv.FormatInt(app.ID, 10),
		"app_slug": app.Slug, "app_client_id": app.ClientID, "installation_id": strconv.FormatInt(f.InstallationID(), 10),
	}}
	return f, env, inst
}

func pkce(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeInBrowser follows the authorization URL as a browser would and
// returns the callback's query.
func authorizeInBrowser(t *testing.T, env *testEnv, u *url.URL) url.Values {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("authorization page = %d", res.StatusCode)
	}
	back, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Path != "/me/accounts/github/callback" {
		t.Fatalf("GitHub redirected to %s", back)
	}
	return back.Query()
}

// The web flow proves the GitHub account by its numeric ID, uses PKCE and
// revokes the user's authorization afterwards.
func TestVerifyAccountAgainstFakeGitHub(t *testing.T) {
	f, env, inst := fakeFixture(t)
	f.AddUser(githubfake.User{ID: 583231, Login: "octo-person"})
	f.AuthorizeAs(583231)
	verifier, challenge := pkce(t)
	u, err := New().BeginVerify(t.Context(), env, inst, "ols_state", challenge)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "github.com" || u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("redirect_uri") != "https://work.example.test/me/accounts/github/callback" {
		t.Fatalf("authorization URL %s", u)
	}
	back := authorizeInBrowser(t, env, u)
	if back.Get("state") != "ols_state" {
		t.Fatal("state was not returned")
	}
	acct, err := New().CompleteVerify(t.Context(), env, inst, back.Get("code"), verifier)
	if err != nil {
		t.Fatal(err)
	}
	if acct != (provider.ExternalAccount{Realm: "github.com", ExternalID: "583231", Login: "octo-person"}) {
		t.Fatalf("account = %+v", acct)
	}
	if n := f.LiveUserTokens(); n != 0 {
		t.Fatalf("%d user tokens were left unrevoked", n)
	}
	// A code is single-use.
	if _, err := New().CompleteVerify(t.Context(), env, inst, back.Get("code"), verifier); !hasCode(err, "invalid_code") {
		t.Fatalf("replayed code = %v", err)
	}
}

func TestVerifyAccountFailures(t *testing.T) {
	f, env, inst := fakeFixture(t)
	f.AddUser(githubfake.User{ID: 7, Login: "someone"})
	verifier, challenge := pkce(t)
	start := func() url.Values {
		u, err := New().BeginVerify(t.Context(), env, inst, "ols_state", challenge)
		if err != nil {
			t.Fatal(err)
		}
		return authorizeInBrowser(t, env, u)
	}
	// The person denies the authorization: no code.
	if back := start(); back.Get("error") != "access_denied" || back.Get("code") != "" {
		t.Fatalf("denied authorization = %v", back)
	}
	f.AuthorizeAs(7)
	// A verifier that does not match the challenge (PKCE) is refused.
	if _, err := New().CompleteVerify(t.Context(), env, inst, start().Get("code"), strings.Repeat("x", 43)); !hasCode(err, "invalid_code") {
		t.Fatalf("PKCE mismatch = %v", err)
	}
	// A wrong client secret is the App's problem, not the person's.
	env.credentials["app_client_secret"] = []byte("wrong")
	if _, err := New().CompleteVerify(t.Context(), env, inst, start().Get("code"), verifier); !hasCode(err, "invalid_app") {
		t.Fatalf("wrong client secret = %v", err)
	}
	env.credentials["app_client_secret"] = []byte(f.App().ClientSecret)
	// A rate limit on the user lookup is retryable with its delay, and the
	// token obtained is still revoked.
	f.Fail(http.MethodGet, "/user", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10)}})
	_, err := New().CompleteVerify(t.Context(), env, inst, start().Get("code"), verifier)
	if code, retryable, after := provider.Classify(err); code != CodeRateLimited || !retryable || after < time.Minute {
		t.Fatalf("rate-limited lookup = %s %v %s", code, retryable, after)
	}
	if n := f.LiveUserTokens(); n != 0 {
		t.Fatalf("%d user tokens were left unrevoked after a failed lookup", n)
	}
	// No client ID: the App is not connected.
	inst.Settings["app_client_id"] = ""
	if _, err := New().BeginVerify(t.Context(), env, inst, "ols_state", challenge); !hasCode(err, "invalid_app") {
		t.Fatalf("missing client ID = %v", err)
	}
}

// The fake also serves the connect and rotation calls the adapter makes;
// they validate against GitHub's description too.
func TestConnectAndRotationAgainstFakeGitHub(t *testing.T) {
	f, env, inst := fakeFixture(t)
	a := New()
	login, id, err := ResolveOrganization(t.Context(), env, f.Org().Login)
	if err != nil || login != f.Org().Login || id != strconv.FormatInt(f.Org().ID, 10) {
		t.Fatalf("organization = %s %s %v", login, id, err)
	}
	manual := url.Values{"app_id": {strconv.FormatInt(f.App().ID, 10)}, "client_id": {f.App().ClientID}, "client_secret": {f.App().ClientSecret}, "webhook_secret": {f.App().WebhookSecret}, "private_key": {string(f.AppPrivateKeyPEM())}, "state": {"ols_next"}}
	if _, err := a.CompleteConnect(t.Context(), env, inst, "manual", manual); err != nil {
		t.Fatal(err)
	}
	result, err := a.CompleteConnect(t.Context(), env, inst, "install_app", url.Values{"installation_id": {strconv.FormatInt(f.InstallationID(), 10)}})
	if err != nil || !result.Done {
		t.Fatalf("installation = %+v, %v", result, err)
	}
	if _, err := a.InstallationToken(t.Context(), env, inst); err != nil {
		t.Fatal(err)
	}
	out, err := a.ApplyCredential(t.Context(), env, inst, "webhook_secret", 2, []byte(strings.Repeat("s", 32)))
	if err != nil || out.Outcome != provider.RotationAcknowledged {
		t.Fatalf("webhook secret rotation = %+v, %v", out, err)
	}
}

// Huge Retry-After or reset values never overflow into a short delay.
func TestRateLimitHeaderOverflow(t *testing.T) {
	now := time.Now()
	for _, h := range []http.Header{
		{"Retry-After": {"99999999999999999999"}},
		{"Retry-After": {strconv.FormatInt(1<<62, 10)}},
		{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(1<<62, 10)}},
		{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"99999999999999999999"}},
	} {
		res := &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: http.NoBody}
		var pe *provider.Error
		if err := rateLimited(now, res); !errors.As(err, &pe) || pe.RetryAfter != time.Hour {
			t.Errorf("%v: after = %v, want an hour", h, err)
		}
	}
}

func hasCode(err error, code string) bool {
	var pe *provider.Error
	return errors.As(err, &pe) && pe.Code == code
}
