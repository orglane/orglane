package githubfake

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// collect records violations instead of failing the test.
type collect struct {
	mu   sync.Mutex
	errs []error
}

func (c *collect) report(err error) { c.mu.Lock(); c.errs = append(c.errs, err); c.mu.Unlock() }
func (c *collect) take() []error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.errs
	c.errs = nil
	return e
}

// The fake reports requests and responses that do not match GitHub's
// description, so it cannot silently drift from the real API.
func TestFakeReportsDrift(t *testing.T) {
	var c collect
	f := New(c.report)
	defer f.Close()
	client := f.Client()
	// A token request without the required code is not a valid request.
	send(t, client, http.MethodPost, "https://github.com/login/oauth/access_token", url.Values{"client_id": {f.App().ClientID}, "client_secret": {f.App().ClientSecret}})
	if errs := c.take(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "does not match GitHub's description") {
		t.Fatalf("invalid request reported as %v", errs)
	}
	// An operation GitHub does not document.
	send(t, client, http.MethodGet, "https://api.github.com/not/an/endpoint", nil)
	if errs := c.take(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "undocumented operation") {
		t.Fatalf("undocumented operation reported as %v", errs)
	}
	// An injected status the operation does not document must still be a
	// generic GitHub error; a 418 is not.
	f.Fail(http.MethodGet, "/orgs/"+f.Org().Login, http.StatusTeapot, nil)
	send(t, client, http.MethodGet, "https://api.github.com/orgs/"+f.Org().Login, nil)
	if errs := c.take(); len(errs) != 1 || !strings.Contains(errs[0].Error(), "not documented") {
		t.Fatalf("undocumented status reported as %v", errs)
	}
	// A generic rate limit is fine.
	f.Fail(http.MethodGet, "/orgs/"+f.Org().Login, http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}})
	res := send(t, client, http.MethodGet, "https://api.github.com/orgs/"+f.Org().Login, nil)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") != "30" {
		t.Fatalf("rate limit = %d %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if errs := c.take(); len(errs) != 0 {
		t.Fatalf("a documented generic error was reported: %v", errs)
	}
	// Only GitHub hosts are reachable through the fake's transport.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := client.Do(req); err == nil {
		_ = res.Body.Close()
		t.Fatal("the fake's transport reached another host")
	}
}

// sent is the status and header of a response whose body was closed.
type sent struct {
	StatusCode int
	Header     http.Header
}

// send makes one request through the fake.
func send(t *testing.T, client *http.Client, method, target string, form url.Values) sent {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	return sent{StatusCode: res.StatusCode, Header: res.Header}
}
