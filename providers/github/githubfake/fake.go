// Package githubfake is a fake GitHub.com for tests and the UI acceptance
// build: an http.Handler with state that serves the endpoints Orglane uses.
// Every request it receives and every response it sends is validated against
// GitHub's OpenAPI description (openapi/), so it cannot drift from the real
// API; a mismatch is reported as a violation. Never compiled into production.
package githubfake

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Reporter receives violations: a request or response that does not match
// GitHub's description, or a call the fake does not implement.
type Reporter func(error)

// The fake's GitHub objects.
type (
	// Org is the organization the App is installed on.
	Org struct {
		ID    int64
		Login string
	}
	// App is the GitHub App.
	App struct {
		ID            int64
		Slug          string
		ClientID      string
		ClientSecret  string
		WebhookSecret string
	}
	// User is a GitHub user account.
	User struct {
		ID    int64
		Login string
	}
)

// Fake is a fake GitHub.com. Its zero value is not usable; call New.
type Fake struct {
	report Reporter
	srv    *httptest.Server
	key    *rsa.PrivateKey

	mu             sync.Mutex
	now            func() time.Time
	org            Org
	app            App
	installationID int64
	users          map[int64]User
	authorizeAs    int64
	codes          map[string]*grant
	userTokens     map[string]int64
	failures       []failure
	calls          []string
	webhookSecret  string
	// installationTokens are the installation tokens issued (and valid).
	installationTokens map[string]bool
	state              orgState
	seats              int // 0: unlimited
}

type grant struct {
	user                             int64
	clientID, redirectURI, challenge string
	expires                          time.Time
	used                             bool
}

type failure struct {
	method, path string
	status       int
	header       http.Header
	message      string
}

// New starts a fake GitHub with one organization, one App installed on it
// and no users. report receives violations (a test's t.Error); it must not
// be nil.
func New(report Reporter) *Fake {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	f := &Fake{
		report: report, key: key, now: time.Now,
		org:                Org{ID: 9001, Login: "orglane-test"},
		app:                App{ID: 456, Slug: "orglane-test-app", ClientID: "Iv1.fakeclientid", ClientSecret: token("fakesecret_"), WebhookSecret: token("")},
		installationID:     789,
		users:              map[int64]User{},
		codes:              map[string]*grant{},
		userTokens:         map[string]int64{},
		installationTokens: map[string]bool{},
		state:              newOrgState(),
	}
	f.srv = httptest.NewServer(f)
	return f
}

// Listen starts a fake GitHub like New, on addr (host:port) instead of a
// random local port: for the UI acceptance build, whose browser reaches the
// authorization page there.
func Listen(report Reporter, addr string) (*Fake, error) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, err
	}
	f := New(report)
	f.srv.Close()
	f.srv = httptest.NewUnstartedServer(f)
	_ = f.srv.Listener.Close()
	f.srv.Listener = ln
	f.srv.Start()
	return f, nil
}

// Close stops the fake.
func (f *Fake) Close() { f.srv.Close() }

// URL is the fake's base URL.
func (f *Fake) URL() string { return f.srv.URL }

// Org is the fake's organization.
func (f *Fake) Org() Org { return f.org }

// App is the fake's GitHub App.
func (f *Fake) App() App { return f.app }

// InstallationID is the App's installation on the organization.
func (f *Fake) InstallationID() int64 { return f.installationID }

// AppPrivateKeyPEM is the App's private key (generated for this fake).
func (f *Fake) AppPrivateKeyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(f.key)})
}

// AddUser adds a GitHub user account.
func (f *Fake) AddUser(u User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID] = u
}

// RenameUser changes a user's login (the ID stays).
func (f *Fake) RenameUser(id int64, login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[id]
	u.Login = login
	f.users[id] = u
}

// AuthorizeAs makes the authorization page approve as this user (0: the
// user denies access).
func (f *Fake) AuthorizeAs(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authorizeAs = id
}

// Fail answers the next request to method and path with status, header and
// a basic-error body (a rate limit or another error).
func (f *Fake) Fail(method, path string, status int, header http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, failure{method: method, path: path, status: status, header: header, message: http.StatusText(status)})
}

// Calls lists the requests received, as "METHOD path".
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// LimitSeats makes invitations fail with GitHub's seat-limit refusal once
// members and pending invitations reach n (0: unlimited).
func (f *Fake) LimitSeats(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seats = n
}

// LiveUserTokens counts user tokens that were issued and not revoked.
func (f *Fake) LiveUserTokens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.userTokens)
}

// Transport routes requests for api.github.com and github.com to the fake,
// keeping their intended host in the Host header.
func (f *Fake) Transport() http.RoundTripper { return transport{f: f} }

// Client is an HTTP client that reaches the fake through Transport.
func (f *Fake) Client() *http.Client {
	return &http.Client{Transport: f.Transport(), Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

type transport struct{ f *Fake }

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" && r.URL.Host != "github.com" {
		return nil, fmt.Errorf("githubfake: request to %s refused", r.URL.Host)
	}
	target, err := url.Parse(t.f.srv.URL)
	if err != nil {
		return nil, err
	}
	clone := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host = target.Scheme, target.Host
	clone.URL = &u
	clone.Host = r.URL.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// ServeHTTP validates the request, handles it and validates the response.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_fake/") {
		f.control(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	h := host(r)
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+pathOf(r))
	f.mu.Unlock()
	x, err := validateRequest(r, h, body)
	if err != nil {
		f.report(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rec := httptest.NewRecorder()
	if !f.failed(rec, r) {
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.handle(rec, r, h, x.route.Path, x.params, body)
	}
	res := rec.Result()
	out, _ := io.ReadAll(res.Body)
	if err := validateResponse(x, res.StatusCode, res.Header, out); err != nil {
		f.report(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(out)
}

func (f *Fake) failed(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, fl := range f.failures {
		if fl.method == r.Method && fl.path == r.URL.Path {
			f.failures = append(f.failures[:i], f.failures[i+1:]...)
			for k, v := range fl.header {
				w.Header()[k] = v
			}
			basicError(w, fl.status, fl.message)
			return true
		}
	}
	return false
}

// handle serves the documented operation route (its path template) that
// the request matched.
func (f *Fake) handle(w http.ResponseWriter, r *http.Request, h, route string, params map[string]string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op := r.Method + " " + route; {
	case h == "github.com" && r.URL.Path == "/login/oauth/authorize":
		f.authorize(w, r)
	case h == "github.com" && r.URL.Path == "/login/oauth/access_token":
		f.accessToken(w, body)
	case op == "GET /user":
		f.currentUser(w, r)
	case op == "GET /user/{account_id}":
		f.getUserByID(w, r, params["account_id"])
	case op == "DELETE /applications/{client_id}/grant":
		f.deleteGrant(w, r, params["client_id"], body)
	case op == "GET /orgs/{org}":
		f.getOrg(w, params["org"])
	case route == "/orgs/{org}/memberships/{username}":
		f.membership(w, r, params)
	case route == "/orgs/{org}/invitations":
		f.invitations(w, r, params, body)
	case op == "DELETE /orgs/{org}/invitations/{invitation_id}":
		f.cancelInvitation(w, r, params)
	case op == "GET /orgs/{org}/members":
		f.listMembers(w, r, params)
	case route == "/orgs/{org}/teams":
		f.teams(w, r, params, body)
	case route == "/orgs/{org}/teams/{team_slug}/memberships/{username}":
		f.teamMembership(w, r, params, body)
	case op == "GET /app":
		if f.appAuth(w, r) {
			writeJSON(w, http.StatusOK, f.integration())
		}
	case op == "GET /app/installations/{installation_id}":
		if f.appAuth(w, r) {
			f.getInstallation(w, params["installation_id"])
		}
	case op == "POST /app/installations/{installation_id}/access_tokens":
		if f.appAuth(w, r) {
			f.installationToken(w, params["installation_id"])
		}
	case op == "PATCH /app/hook/config":
		if f.appAuth(w, r) {
			f.hookConfig(w, body)
		}
	default:
		err := fmt.Errorf("githubfake: %s %s is documented but not implemented", r.Method, r.URL.Path)
		f.report(err)
		basicError(w, http.StatusInternalServerError, "not implemented by the fake")
	}
}

// authorize is GitHub's authorization page, approving as AuthorizeAs.
func (f *Fake) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if q.Get("client_id") != f.app.ClientID || err != nil || redirect.Scheme == "" {
		http.Error(w, "unknown application", http.StatusNotFound) // GitHub shows an error page
		f.report(fmt.Errorf("githubfake: authorization for unknown client or redirect"))
		return
	}
	back := redirect.Query()
	back.Set("state", q.Get("state"))
	if f.authorizeAs == 0 {
		back.Set("error", "access_denied")
	} else {
		code := token("")[:20]
		f.codes[code] = &grant{user: f.authorizeAs, clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), expires: f.now().Add(10 * time.Minute)}
		back.Set("code", code)
	}
	redirect.RawQuery = back.Encode()
	w.Header().Set("Location", redirect.String())
	w.WriteHeader(http.StatusFound)
}

// accessToken exchanges a code. GitHub answers errors with 200.
func (f *Fake) accessToken(w http.ResponseWriter, body []byte) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": "invalid_request"})
		return
	}
	if form.Get("client_id") != f.app.ClientID || form.Get("client_secret") != f.app.ClientSecret {
		writeJSON(w, http.StatusOK, map[string]any{"error": "incorrect_client_credentials", "error_description": "The client_id and/or client_secret passed are incorrect."})
		return
	}
	g, ok := f.codes[form.Get("code")]
	switch {
	case !ok || g.used || f.now().After(g.expires) || g.clientID != form.Get("client_id"):
		writeJSON(w, http.StatusOK, map[string]any{"error": "bad_verification_code", "error_description": "The code passed is incorrect or expired."})
		return
	case form.Get("redirect_uri") != "" && form.Get("redirect_uri") != g.redirectURI:
		writeJSON(w, http.StatusOK, map[string]any{"error": "redirect_uri_mismatch"})
		return
	case g.challenge != "" && challenge(form.Get("code_verifier")) != g.challenge:
		writeJSON(w, http.StatusOK, map[string]any{"error": "bad_verification_code", "error_description": "The code_verifier does not match the code_challenge."})
		return
	}
	g.used = true
	access := token("ghu_")
	f.userTokens[access] = g.user
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "bearer", "scope": "", "expires_in": 28800, "refresh_token": token("ghr_"), "refresh_token_expires_in": 15897600})
}

func (f *Fake) currentUser(w http.ResponseWriter, r *http.Request) {
	id, ok := f.userTokens[bearer(r)]
	if !ok {
		basicError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	u := f.users[id]
	body := example("private-user-response-with-public-and-private-profile-information")
	body["id"], body["login"], body["type"], body["user_view_type"] = u.ID, u.Login, "User", "private"
	body["node_id"] = base64.StdEncoding.EncodeToString([]byte("U_" + strconv.FormatInt(u.ID, 10)))
	body["html_url"] = "https://github.com/" + u.Login
	body["url"] = "https://api.github.com/users/" + u.Login
	writeJSON(w, http.StatusOK, body)
}

// deleteGrant revokes the authorization of the token's user: every token
// of that user for this App.
func (f *Fake) deleteGrant(w http.ResponseWriter, r *http.Request, clientID string, body []byte) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != f.app.ClientID || pass != f.app.ClientSecret || clientID != f.app.ClientID {
		basicError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	var in struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.AccessToken == "" {
		validationFailed(w)
		return
	}
	id, ok := f.userTokens[in.AccessToken]
	if !ok {
		validationFailed(w)
		return
	}
	for t, u := range f.userTokens {
		if u == id {
			delete(f.userTokens, t)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) getOrg(w http.ResponseWriter, login string) {
	if !strings.EqualFold(login, f.org.Login) {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	body := example("organization-full")
	body["id"], body["login"], body["type"] = f.org.ID, f.org.Login, "Organization"
	body["archived_at"] = nil
	writeJSON(w, http.StatusOK, body)
}

func (f *Fake) integration() map[string]any {
	body := example("integration")
	body["id"], body["slug"], body["client_id"] = f.app.ID, f.app.Slug, f.app.ClientID
	return body
}

func (f *Fake) getInstallation(w http.ResponseWriter, id string) {
	if id != strconv.FormatInt(f.installationID, 10) {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	body := example("base-installation")
	body["id"], body["app_id"], body["app_slug"] = f.installationID, f.app.ID, f.app.Slug
	account, _ := body["account"].(map[string]any)
	if account == nil {
		account = map[string]any{}
	}
	account["id"], account["login"], account["type"] = f.org.ID, f.org.Login, "Organization"
	body["account"] = account
	body["target_id"], body["target_type"], body["suspended_at"], body["suspended_by"] = f.org.ID, "Organization", nil, nil
	body["permissions"] = map[string]any{"members": "write", "metadata": "read", "pull_requests": "read", "issues": "read"}
	writeJSON(w, http.StatusOK, body)
}

func (f *Fake) installationToken(w http.ResponseWriter, id string) {
	if id != strconv.FormatInt(f.installationID, 10) {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	body := example("installation-token")
	body["token"] = token("ghs_")
	f.installationTokens[body["token"].(string)] = true
	body["expires_at"] = f.now().Add(time.Hour).UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusCreated, body)
}

func (f *Fake) hookConfig(w http.ResponseWriter, body []byte) {
	var in struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(body, &in); err == nil && in.Secret != "" {
		f.webhookSecret = in.Secret
	}
	out := example("webhook-config")
	out["secret"] = "********"
	writeJSON(w, http.StatusOK, out)
}

// appAuth verifies an App JWT (RS256 with the App's key, iss = App ID, at
// most ten minutes valid).
func (f *Fake) appAuth(w http.ResponseWriter, r *http.Request) bool {
	signed, err := jose.ParseSignedCompact(bearer(r), []jose.SignatureAlgorithm{jose.RS256})
	if err == nil {
		var payload []byte
		if payload, err = signed.Verify(&f.key.PublicKey); err == nil {
			var claims struct {
				Iss string `json:"iss"`
				Iat int64  `json:"iat"`
				Exp int64  `json:"exp"`
			}
			now := f.now().Unix()
			if json.Unmarshal(payload, &claims) == nil && claims.Iss == strconv.FormatInt(f.app.ID, 10) && claims.Iat <= now && now < claims.Exp && claims.Exp-claims.Iat <= 600 {
				return true
			}
		}
	}
	basicError(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
	return false
}

// control serves the fake's own endpoints (not GitHub's) for the UI
// acceptance build: the App's credentials and the authorizing user.
func (f *Fake) control(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/_fake/app":
		writeJSON(w, http.StatusOK, map[string]any{
			"app_id": strconv.FormatInt(f.app.ID, 10), "client_id": f.app.ClientID, "client_secret": f.app.ClientSecret,
			"webhook_secret": f.app.WebhookSecret, "private_key": string(f.AppPrivateKeyPEM()),
			"installation_id": strconv.FormatInt(f.installationID, 10), "organization": f.org.Login,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/_fake/authorize-as":
		var u User
		if err := json.NewDecoder(r.Body).Decode(&struct {
			ID    *int64  `json:"id"`
			Login *string `json:"login"`
		}{&u.ID, &u.Login}); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if u.ID != 0 {
			f.AddUser(u)
		}
		f.AuthorizeAs(u.ID)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(v, "Bearer "); ok {
		return t
	}
	if t, ok := strings.CutPrefix(v, "token "); ok {
		return t
	}
	return ""
}

// anyAuth reports whether r carries a token the fake issued (installation
// or user).
func (f *Fake) anyAuth(r *http.Request) bool {
	t := bearer(r)
	_, user := f.userTokens[t]
	return user || f.installationTokens[t]
}

// exampleItem returns the first object of a list example.
func exampleItem(name string) map[string]any {
	d, err := loadDescriptions()
	if err != nil {
		panic(err)
	}
	ex, ok := d[0].doc.Components.Examples[name]
	if !ok || ex.Value == nil {
		panic("githubfake: no example " + name)
	}
	raw, err := json.Marshal(ex.Value.Value)
	if err != nil {
		panic(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || len(out) == 0 {
		panic("githubfake: example " + name + " is not a list of objects")
	}
	return out[0]
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func token(prefix string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func basicError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"message": message, "documentation_url": "https://docs.github.com/rest", "status": strconv.Itoa(status)})
}

func validationFailed(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed", "documentation_url": "https://docs.github.com/rest"})
}
