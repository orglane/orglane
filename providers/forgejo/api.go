package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/orglane/orglane/providers/provider"
)

// maxResponse bounds the API responses the adapter reads.
const maxResponse = 1 << 20

// Error codes of the adapter.
const (
	// CodeUnavailable: Forgejo did not answer, or answered with a server
	// error. Retryable.
	CodeUnavailable = "forgejo_unavailable"
	// CodeUnhealthy: Forgejo's health endpoint reports a failure.
	CodeUnhealthy = "forgejo_unhealthy"
	// CodeAdminAuth: the service admin cannot authenticate or is no admin.
	CodeAdminAuth = "service_admin_invalid"
	// CodeRejected: Forgejo refused a request (a 4xx other than auth).
	CodeRejected = "forgejo_rejected"
	// CodeRateLimited: Forgejo asked to slow down.
	CodeRateLimited = "rate_limited"
	// CodeNoUpstream: the instance has no upstream URL yet.
	CodeNoUpstream = "no_upstream"
	// CodeInvalidSettings: a required setting is missing or invalid.
	CodeInvalidSettings = "invalid_settings"
	// CodeSSONotConfigured: the sign-in source for Orglane does not exist
	// (yet). Retryable: the package's configure hook creates it.
	CodeSSONotConfigured = "sso_not_configured"
	// CodeNoWebhookSecret: the instance has no active webhook secret yet.
	CodeNoWebhookSecret = "webhook_secret_missing"
)

// statusError is a non-2xx answer. It carries only the status: response
// bodies can echo personal data and are never kept.
type statusError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *statusError) Error() string { return "forgejo: HTTP " + strconv.Itoa(e.Status) }

// client calls the Forgejo API of one instance through its upstream.
type client struct {
	http     *http.Client
	base     *url.URL
	user     string
	password []byte
	token    string // API token instead of basic auth (withToken)
}

// withToken returns a client that authenticates with an API token.
func (c *client) withToken(token string) *client {
	return &client{http: c.http, base: c.base, token: token}
}

func newClient(ctx context.Context, env provider.Env, inst provider.Instance, auth bool) (*client, error) {
	if inst.Upstream == nil {
		return nil, &provider.Error{Code: CodeNoUpstream, Retryable: true, Err: errors.New("forgejo: the instance has no upstream yet")}
	}
	c := &client{http: env.HTTPClient(), base: inst.Upstream}
	if auth {
		pw, err := env.Credential(ctx, CredentialAdminPassword)
		if err != nil {
			return nil, fmt.Errorf("forgejo: admin password: %w", err)
		}
		c.user, c.password = AdminUsername, pw
	}
	return c, nil
}

// do sends a request and decodes a JSON answer into out (if not nil). Every
// non-2xx answer is a *statusError.
func (c *client) do(ctx context.Context, method, path string, in, out any) error {
	return c.doQuery(ctx, method, path, nil, in, out)
}

// doQuery is do with a query string.
func (c *client) doQuery(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("forgejo: encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return fmt.Errorf("forgejo: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "token "+c.token)
	case c.user != "":
		req.SetBasicAuth(c.user, string(c.password))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The error may name the URL, never credentials (basic auth is in a
		// header).
		return fmt.Errorf("forgejo: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
		return &statusError{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(out); err != nil {
		return fmt.Errorf("forgejo: decode %s %s: %w", method, path, err)
	}
	return nil
}

func retryAfter(v string) time.Duration {
	if s, err := strconv.Atoi(v); err == nil && s > 0 && s <= 3600 {
		return time.Duration(s) * time.Second
	}
	return 0
}

// definite reports whether err is an answer that refuses the request for
// good: a 4xx other than a timeout or rate limit. Only such an answer can
// prove that a write was not made, and only for the request it answers.
func definite(err error) bool {
	s := status(err)
	return s >= 400 && s < 500 && s != http.StatusRequestTimeout && s != http.StatusTooManyRequests
}

// status returns the HTTP status of err, or 0 if Forgejo did not answer.
func status(err error) int {
	var se *statusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// classify turns an API failure into the *provider.Error the core shows and
// retries by: no answer and server errors are retryable, authentication
// failures and other refusals are not.
func classify(err error) error {
	var se *statusError
	if !errors.As(err, &se) {
		var pe *provider.Error
		if errors.As(err, &pe) {
			return err
		}
		return &provider.Error{Code: CodeUnavailable, Retryable: true, Err: err}
	}
	switch {
	case se.Status == http.StatusTooManyRequests:
		return &provider.Error{Code: CodeRateLimited, Retryable: true, RetryAfter: se.RetryAfter, Err: err}
	case se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden:
		// Retryable: right after an install, a reinstall or a new admin
		// password the configure hook may not have finished; Setup's
		// attempts are bounded.
		return &provider.Error{Code: CodeAdminAuth, Retryable: true, Err: err}
	case se.Status == http.StatusRequestTimeout || se.Status >= 500:
		return &provider.Error{Code: CodeUnavailable, Retryable: true, RetryAfter: se.RetryAfter, Err: err}
	default:
		return &provider.Error{Code: CodeRejected, Err: err}
	}
}
