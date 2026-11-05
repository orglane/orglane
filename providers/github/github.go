// Package github connects GitHub.com organizations through GitHub Apps.
// Account linking/provisioning and event normalization are separate M2 work.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/orglane/orglane/providers/provider"
)

var loginPattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?$`)
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
var idPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
var codePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,256}$`)

// Adapter is safe for concurrent use. Installation tokens live only in this
// bounded in-memory cache, keyed by instance, installation and key digest.
type Adapter struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
}
type cachedToken struct {
	value   string
	expires time.Time
}

// New returns a GitHub.com adapter.
func New() *Adapter { return &Adapter{tokens: map[string]cachedToken{}} }

// Manifest declares only features implemented in this adapter.
func (*Adapter) Manifest() provider.Manifest {
	features := make([]provider.FeatureSupport, 0, len(provider.Features))
	for _, f := range provider.Features {
		support := provider.Unsupported
		if f == provider.FeatureAccountsLink {
			support = provider.Supported // the employee step (OAuth through the App)
		}
		features = append(features, provider.FeatureSupport{Feature: f, Support: support})
	}
	settings := []provider.SettingSpec{}
	for _, n := range []string{"organization", "organization_id", "app_id", "app_slug", "app_client_id", "installation_id"} {
		settings = append(settings, provider.SettingSpec{Name: n, Type: provider.SettingString})
	}
	return provider.Manifest{ID: "github", Name: "GitHub", Management: provider.Connected, Capabilities: []provider.Capability{"code"}, Features: features, DefaultKey: "github", DefaultPolicy: provider.MappedTeams, Settings: settings,
		EmployeeSteps: []provider.EmployeeStepSpec{{
			ID: StepLinkAccount, Title: "Link your GitHub account",
			Description: "Sign in to GitHub once so Orglane knows which GitHub account is yours.",
			ActionPath:  "/me/accounts/github",
		}},
		Credentials: []provider.CredentialSpec{
			{Name: "app_private_key", Source: provider.SourceUpload, Rotatable: true, Overlap: time.Hour},
			{Name: "app_client_secret", Source: provider.SourceProvider},
			{Name: "webhook_secret", Source: provider.SourceProvider, Rotatable: true, GenerateRotation: true, Overlap: 24 * time.Hour},
		}}
}

func setting(inst provider.Instance, name string) string {
	v, _ := inst.Settings[name].(string)
	return v
}
func callback(env provider.Env) string {
	u := env.WorkspaceURL()
	u.Path = "/settings/apps/github/callback"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func accountCallback(env provider.Env) string {
	u := env.WorkspaceURL()
	u.Path = "/me/accounts/github/callback"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// RequiredPermissions is the minimal permission set for M2 GitHub operations.
// Return a fresh map so callers cannot change the adapter's requirements.
func RequiredPermissions() map[string]string {
	return map[string]string{"members": "write", "metadata": "read", "pull_requests": "read", "issues": "read"}
}

// BeginConnect returns the manifest form or resumes installation of a stored App.
func (a *Adapter) BeginConnect(_ context.Context, env provider.Env, inst provider.Instance, state string) (provider.ConnectStep, error) {
	org := setting(inst, "organization")
	if !loginPattern.MatchString(org) {
		return provider.ConnectStep{}, protocolError("invalid_organization")
	}
	if setting(inst, "app_slug") != "" {
		return installationStep(inst.Settings, state)
	}
	manifest := map[string]any{
		"name": "Orglane " + org, "url": env.WorkspaceURL().String(), "public": false,
		"redirect_url": callback(env), "setup_url": callback(env), "callback_urls": []string{accountCallback(env)},
		"hook_attributes":     map[string]any{"url": env.WebhookURL(inst.ID).String(), "active": true},
		"default_permissions": RequiredPermissions(),
		"default_events":      []string{"pull_request", "pull_request_review", "issue_comment", "pull_request_review_comment", "member", "membership", "organization"},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return provider.ConnectStep{}, err
	}
	u := &url.URL{Scheme: "https", Host: "github.com", Path: "/organizations/" + org + "/settings/apps/new"}
	u.RawQuery = url.Values{"state": {state}}.Encode()
	return provider.ConnectStep{Name: "create_app", Method: "POST", URL: u, Form: map[string]string{"manifest": string(raw)}}, nil
}

func installationStep(settings map[string]any, state string) (provider.ConnectStep, error) {
	slug, _ := settings["app_slug"].(string)
	if !slugPattern.MatchString(slug) {
		return provider.ConnectStep{}, protocolError("invalid_app")
	}
	u := &url.URL{Scheme: "https", Host: "github.com", Path: "/apps/" + slug + "/installations/new"}
	u.RawQuery = url.Values{"state": {state}}.Encode()
	return provider.ConnectStep{Name: "install_app", Method: "GET", URL: u}, nil
}

// ValidOrganization validates a GitHub.com account login without network access.
func ValidOrganization(login string) bool { return loginPattern.MatchString(login) }

// ResolveOrganization reads the immutable organization account ID. Only
// GitHub.com is contacted; login input cannot supply a URL.
func ResolveOrganization(ctx context.Context, env provider.Env, login string) (string, string, error) {
	if !loginPattern.MatchString(login) {
		return "", "", protocolError("invalid_organization")
	}
	var org struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := request(ctx, env, http.MethodGet, "/orgs/"+login, "", nil, &org); err != nil {
		return "", "", err
	}
	if org.ID <= 0 || org.Type != "Organization" || !loginPattern.MatchString(org.Login) {
		return "", "", protocolError("invalid_organization")
	}
	return org.Login, strconv.FormatInt(org.ID, 10), nil
}

// CompleteConnect converts a manifest code, validates manual credentials, or
// verifies the installation with the App's JWT and immutable expected account.
func (a *Adapter) CompleteConnect(ctx context.Context, env provider.Env, inst provider.Instance, step string, params url.Values) (provider.ConnectResult, error) {
	switch step {
	case "create_app", "manual":
		var app struct {
			ID            int64  `json:"id"`
			Slug          string `json:"slug"`
			ClientID      string `json:"client_id"`
			ClientSecret  string `json:"client_secret"`
			WebhookSecret string `json:"webhook_secret"`
			PEM           string `json:"pem"`
		}
		if step == "create_app" {
			code := params.Get("code")
			if !codePattern.MatchString(code) {
				return provider.ConnectResult{}, protocolError("invalid_code")
			}
			if err := request(ctx, env, http.MethodPost, "/app-manifests/"+code+"/conversions", "", nil, &app); err != nil {
				return provider.ConnectResult{}, err
			}
		} else {
			key := []byte(params.Get("private_key"))
			defer clear(key)
			jwt, err := appJWT(env.Now(), params.Get("app_id"), key)
			if err != nil {
				return provider.ConnectResult{}, err
			}
			if err := request(ctx, env, http.MethodGet, "/app", jwt, nil, &app); err != nil {
				return provider.ConnectResult{}, err
			}
			if strconv.FormatInt(app.ID, 10) != params.Get("app_id") || app.ClientID != params.Get("client_id") {
				return provider.ConnectResult{}, protocolError("invalid_app")
			}
			app.PEM = params.Get("private_key")
			app.ClientSecret = params.Get("client_secret")
			app.WebhookSecret = params.Get("webhook_secret")
		}
		if app.ID <= 0 || !slugPattern.MatchString(app.Slug) || app.ClientID == "" || app.ClientSecret == "" || len(app.WebhookSecret) < 32 {
			return provider.ConnectResult{}, protocolError("invalid_app")
		}
		key := []byte(app.PEM)
		if _, err := parseKey(key); err != nil {
			clear(key)
			return provider.ConnectResult{}, err
		}
		settings := map[string]any{"app_id": strconv.FormatInt(app.ID, 10), "app_slug": app.Slug, "app_client_id": app.ClientID}
		next, err := installationStep(settings, params.Get("state"))
		if err != nil {
			clear(key)
			return provider.ConnectResult{}, err
		}
		return provider.ConnectResult{Next: &next, Settings: settings, Credentials: map[string][]byte{"app_private_key": key, "app_client_secret": []byte(app.ClientSecret), "webhook_secret": []byte(app.WebhookSecret)}}, nil
	case "install_app":
		id := params.Get("installation_id")
		if !idPattern.MatchString(id) {
			return provider.ConnectResult{}, protocolError("invalid_installation")
		}
		installation, err := a.VerifyInstallation(ctx, env, inst, id)
		if err != nil {
			return provider.ConnectResult{}, err
		}
		u := &url.URL{Scheme: "https", Host: "github.com", Path: "/" + installation.Account.Login}
		return provider.ConnectResult{Done: true, BaseURL: u, Settings: map[string]any{"installation_id": id, "organization": installation.Account.Login, "organization_id": strconv.FormatInt(installation.Account.ID, 10)}}, nil
	default:
		return provider.ConnectResult{}, protocolError("invalid_step")
	}
}

// Installation is the verified GitHub installation metadata.
type Installation struct {
	ID      int64 `json:"id"`
	AppID   int64 `json:"app_id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	TargetID    int64             `json:"target_id"`
	TargetType  string            `json:"target_type"`
	SuspendedAt *time.Time        `json:"suspended_at"`
	Permissions map[string]string `json:"permissions"`
}

// VerifyInstallation does not trust installation_id from the browser. It checks
// the authenticated App, expected immutable organization and usable permissions.
func (a *Adapter) VerifyInstallation(ctx context.Context, env provider.Env, inst provider.Instance, id string) (Installation, error) {
	var result Installation
	if !idPattern.MatchString(id) {
		return result, protocolError("invalid_installation")
	}
	jwt, err := jwtFor(ctx, env, inst)
	if err != nil {
		return result, err
	}
	if err = request(ctx, env, http.MethodGet, "/app/installations/"+id, jwt, nil, &result); err != nil {
		return result, err
	}
	expectedID := setting(inst, "organization_id")
	if expectedID == "" {
		_, expectedID, err = ResolveOrganization(ctx, env, setting(inst, "organization"))
		if err != nil {
			return Installation{}, err
		}
	}
	if strconv.FormatInt(result.ID, 10) != id || strconv.FormatInt(result.AppID, 10) != setting(inst, "app_id") || result.Account.Type != "Organization" || result.TargetType != "Organization" || result.Account.ID <= 0 || result.TargetID != result.Account.ID || strconv.FormatInt(result.Account.ID, 10) != expectedID || !loginPattern.MatchString(result.Account.Login) || result.SuspendedAt != nil {
		return Installation{}, protocolError("installation_mismatch")
	}
	for name, level := range RequiredPermissions() {
		actual := result.Permissions[name]
		if actual != level && (level != "read" || actual != "write") {
			return Installation{}, protocolError("missing_permissions")
		}
	}
	return result, nil
}

// InstallationToken mints a short-lived token for a verified installation. It is
// never persisted and is refreshed five minutes before GitHub's expiry.
func (a *Adapter) InstallationToken(ctx context.Context, env provider.Env, inst provider.Instance) (string, error) {
	key, err := env.Credential(ctx, "app_private_key")
	if err != nil {
		return "", err
	}
	defer clear(key)
	digest := sha256.Sum256(key)
	id := setting(inst, "installation_id")
	cacheKey := fmt.Sprintf("%s/%s/%s/%x", inst.ID, setting(inst, "app_id"), id, digest)
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.tokens[cacheKey]; ok && env.Now().Add(5*time.Minute).Before(t.expires) {
		return t.value, nil
	}
	delete(a.tokens, cacheKey)
	if _, err = a.VerifyInstallation(ctx, env, inst, id); err != nil {
		return "", err
	}
	jwt, err := appJWT(env.Now(), setting(inst, "app_id"), key)
	if err != nil {
		return "", err
	}
	var result struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	if err = request(ctx, env, http.MethodPost, "/app/installations/"+id+"/access_tokens", jwt, map[string]any{"permissions": RequiredPermissions()}, &result); err != nil {
		return "", err
	}
	if result.Token == "" || !result.Expires.After(env.Now().Add(5*time.Minute)) || result.Expires.After(env.Now().Add(65*time.Minute)) {
		return "", protocolError("invalid_token_expiry")
	}
	if len(a.tokens) >= 128 {
		clear(a.tokens)
	}
	a.tokens[cacheKey] = cachedToken{value: result.Token, expires: result.Expires}
	return result.Token, nil
}

func parseKey(value []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(value)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, protocolError("invalid_private_key")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, _ = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, _ := x509.ParsePKCS8PrivateKey(block.Bytes)
		key, _ = parsed.(*rsa.PrivateKey)
	}
	if key == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return nil, protocolError("invalid_private_key")
	}
	return key, nil
}
func appJWT(now time.Time, appID string, value []byte) (string, error) {
	if !idPattern.MatchString(appID) {
		return "", protocolError("invalid_app")
	}
	key, err := parseKey(value)
	if err != nil {
		return "", err
	}

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", protocolError("jwt_failed")
	}
	payload, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": appID})
	signed, err := signer.Sign(payload)
	if err != nil {
		return "", protocolError("jwt_failed")
	}
	compact, err := signed.CompactSerialize()
	if err != nil {
		return "", protocolError("jwt_failed")
	}
	return compact, nil
}
func jwtFor(ctx context.Context, env provider.Env, inst provider.Instance) (string, error) {
	key, err := env.Credential(ctx, "app_private_key")
	if err != nil {
		return "", err
	}
	defer clear(key)
	return appJWT(env.Now(), setting(inst, "app_id"), key)
}

// APIError contains only safe status metadata, never GitHub response bodies,
// request URLs (manifest codes) or transport errors (which may contain secrets).
type APIError struct{ Status int }

func (e *APIError) Error() string     { return fmt.Sprintf("github: API status %d", e.Status) }
func protocolError(code string) error { return &provider.Error{Code: code, Retryable: false} }

// GitHub hosts the adapter talks to; never anything else.
const (
	apiHost = "api.github.com"
	webHost = "github.com"
)

// call is one request to a fixed GitHub host.
type call struct {
	host, method, path string
	// authorization is the whole Authorization header value, or "".
	authorization string
	body          any         // JSON-encoded, or url.Values form-encoded
	accept        string      // default application/vnd.github+json
	out           interface{} // decoded JSON response, or nil
}

func request(ctx context.Context, env provider.Env, method, path, token string, body, out any) error {
	c := call{host: apiHost, method: method, path: path, body: body, out: out}
	if token != "" {
		c.authorization = "Bearer " + token
	}
	return do(ctx, env, c)
}

func do(ctx context.Context, env provider.Env, c call) error {
	path, method, out := c.path, c.method, c.out
	// Build the authority independently of caller-supplied path/query data.
	parsed, err := url.ParseRequestURI(path)
	if err != nil || (c.host != apiHost && c.host != webHost) || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" || strings.ContainsAny(path, "\\#") {
		return protocolError("invalid_request")
	}
	endpoint := url.URL{Scheme: "https", Host: c.host, Path: parsed.Path, RawPath: parsed.RawPath, RawQuery: parsed.RawQuery}
	var r io.Reader
	contentType := ""
	switch body := c.body.(type) {
	case nil:
	case url.Values:
		r, contentType = strings.NewReader(body.Encode()), "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(body)
		if err != nil {
			return protocolError("invalid_request")
		}
		r, contentType = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), r)
	if err != nil {
		return protocolError("invalid_request")
	}
	accept := c.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	if c.host == apiHost {
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	req.Header.Set("User-Agent", "Orglane")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.authorization != "" {
		req.Header.Set("Authorization", c.authorization)
	}
	// Do not follow even same-host redirects for credential-bearing requests.
	client := *env.HTTPClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return protocolError("github_unavailable")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if err := rateLimited(env.Now(), res); err != nil {
			return err
		}
		return &APIError{Status: res.StatusCode}
	}
	if out == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return protocolError("invalid_response")
	}
	defer clear(raw)
	if err = json.Unmarshal(raw, out); err != nil {
		return protocolError("invalid_response")
	}
	return nil
}

// CodeRateLimited is the provider.Error code of a GitHub rate limit.
const CodeRateLimited = "rate_limited"

// rateLimited recognizes GitHub's primary and secondary rate limits: a 429,
// or a 403 with Retry-After, X-RateLimit-Remaining: 0 or a rate-limit
// message (https://docs.github.com/en/rest/using-the-rest-api/troubleshooting-the-rest-api#rate-limit-errors).
// It returns a retryable *provider.Error whose RetryAfter is Retry-After,
// else the time until X-RateLimit-Reset, else one minute (GitHub's advice),
// at most an hour; nil for any other failure (a genuine permission denial
// stays a definite answer). It never keeps the response body.
func rateLimited(now time.Time, res *http.Response) error {
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	limited := res.StatusCode == http.StatusTooManyRequests
	var after time.Duration
	// Seconds are capped before they become a Duration, so huge header
	// values cannot overflow into a negative or tiny delay.
	const maxWait = int64(time.Hour / time.Second)
	if v := strings.TrimSpace(res.Header.Get("Retry-After")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			limited, after = true, time.Duration(min(n, maxWait))*time.Second
		} else if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(v, "-") {
			limited, after = true, time.Hour
		}
	}
	if strings.TrimSpace(res.Header.Get("X-RateLimit-Remaining")) == "0" {
		limited = true
		reset, err := strconv.ParseInt(strings.TrimSpace(res.Header.Get("X-RateLimit-Reset")), 10, 64)
		if errors.Is(err, strconv.ErrRange) && after == 0 {
			after = time.Hour
		} else if err == nil && after == 0 {
			if wait := reset - now.Unix(); wait > 0 {
				after = time.Duration(min(wait, maxWait)+1) * time.Second
			}
		}
	}
	if !limited {
		limited = rateLimitMessage(res.Body)
	}
	if !limited {
		return nil
	}
	if after <= 0 {
		after = time.Minute
	}
	if after > time.Hour {
		after = time.Hour
	}
	return &provider.Error{Code: CodeRateLimited, Retryable: true, RetryAfter: after}
}

// rateLimitMessage reports whether a 403 body's message names a rate limit
// (GitHub's secondary limits may come without headers).
func rateLimitMessage(body io.Reader) bool {
	raw, err := io.ReadAll(io.LimitReader(body, 64<<10))
	defer clear(raw)
	if err != nil {
		return false
	}
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return false
	}
	return strings.Contains(strings.ToLower(msg.Message), "rate limit")
}

// ApplyCredential verifies an uploaded key without replacing the working one,
// or idempotently updates the App webhook secret. Ambiguous results stay
// unknown; a rate limit is unknown too, and its error carries RetryAfter, so
// the core snoozes the job until GitHub's reset.
func (a *Adapter) ApplyCredential(ctx context.Context, env provider.Env, inst provider.Instance, name string, _ int, value []byte) (provider.Rotation, error) {
	var err error
	switch name {
	case "app_private_key":
		var jwt string
		jwt, err = appJWT(env.Now(), setting(inst, "app_id"), value)
		if err != nil {
			// A locally invalid key never reaches GitHub; the outcome is the error channel.
			return provider.Rotation{Outcome: provider.RotationRejected}, nil //nolint:nilerr // A definite rejection is represented by Outcome.
		}
		var app struct {
			ID int64 `json:"id"`
		}
		err = request(ctx, env, http.MethodGet, "/app", jwt, nil, &app)
		if err == nil && strconv.FormatInt(app.ID, 10) != setting(inst, "app_id") {
			return provider.Rotation{Outcome: provider.RotationRejected}, nil
		}
	case "webhook_secret":
		var jwt string
		jwt, err = jwtFor(ctx, env, inst)
		if err == nil {
			err = request(ctx, env, http.MethodPatch, "/app/hook/config", jwt, map[string]any{"secret": string(value)}, nil)
		}
	default:
		return provider.Rotation{Outcome: provider.RotationRejected}, nil
	}
	if err == nil {
		return provider.Rotation{Outcome: provider.RotationAcknowledged}, nil
	}
	var api *APIError
	if errors.As(err, &api) && (api.Status == 400 || api.Status == 401 || api.Status == 403 || api.Status == 404 || api.Status == 422) {
		return provider.Rotation{Outcome: provider.RotationRejected}, nil
	}
	return provider.Rotation{Outcome: provider.RotationUnknown}, err
}

var _ provider.Connector = (*Adapter)(nil)
var _ provider.CredentialRotator = (*Adapter)(nil)
