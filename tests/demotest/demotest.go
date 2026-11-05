// Package demotest lets Go end-to-end tests use the demo dataset against a
// running server in demo mode (`orglane server --demo`, see demo/README.md):
// read the dataset the server loaded and sign in as a demo person with one
// click.
//
//	d := demotest.Dataset(t)
//	owner := demotest.SignIn(t, baseURL, "")             // the Owner
//	member := demotest.SignIn(t, baseURL, "priya.raman") // anyone else
//	code, _, body := owner.Get(t, "/api/v1/people")
//
// Sessions carry their cookies by hand rather than in a cookie jar, so they
// work against a server reached over plain HTTP whose workspace URL is
// https (the __Host- cookies are Secure). In-process tests in package core
// load the dataset with core/internal/demomode.Prepare instead.
package demotest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/demo"
)

// Dataset returns the "Acme Robotics" dataset, the one `--demo` loads.
func Dataset(t testing.TB) *demo.Dataset {
	t.Helper()
	d, err := demo.Load(demo.Acme)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Person is the signed-in person, as returned by the sign-in.
type Person struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	WorkEmail   string `json:"workEmail"`
	DisplayName string `json:"displayName"`
}

// Session is a browser-like session of a demo person.
type Session struct {
	// BaseURL is the address the server is reached at, e.g.
	// http://127.0.0.1:8080.
	BaseURL string
	Person  Person
	// CSRFToken is sent with state-changing requests.
	CSRFToken string
	// Origin is the workspace origin (from /.well-known/orglane), sent as
	// the Origin of state-changing requests.
	Origin  string
	cookies []*http.Cookie
	client  *http.Client
}

// SignIn signs in as the demo person with the given username ("" for the
// dataset's default person, the Owner) through POST /api/v1/demo/sign-in.
// It fails the test if the server is not in demo mode.
func SignIn(t testing.TB, baseURL, username string) *Session {
	t.Helper()
	return SignInWith(t, &http.Client{Timeout: 30 * time.Second}, baseURL, username)
}

// SignInWith is SignIn through client, e.g. one that dials Traefik's
// published port for a *.localhost workspace that the host cannot resolve.
func SignInWith(t testing.TB, client *http.Client, baseURL, username string) *Session {
	t.Helper()
	s := &Session{BaseURL: strings.TrimSuffix(baseURL, "/"), client: client}

	code, _, body := s.do(t, http.MethodGet, "/.well-known/orglane", nil)
	var discovery struct {
		WorkspaceURL string `json:"workspaceUrl"`
		Demo         bool   `json:"demo"`
	}
	if err := json.Unmarshal(body, &discovery); code != http.StatusOK || err != nil {
		t.Fatalf("demotest: GET /.well-known/orglane = %d %s", code, body)
	}
	if !discovery.Demo {
		t.Fatal("demotest: the server is not in demo mode (start it with `orglane server --demo`)")
	}
	s.Origin = discovery.WorkspaceURL

	var req any
	if username != "" {
		req = map[string]string{"username": username}
	}
	code, header, body := s.do(t, http.MethodPost, "/api/v1/demo/sign-in", req)
	if code != http.StatusOK {
		t.Fatalf("demotest: sign in as %q = %d %s", username, code, body)
	}
	var result struct {
		Person    Person `json:"person"`
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("demotest: decode sign-in result: %v", err)
	}
	s.Person, s.CSRFToken = result.Person, result.CSRFToken
	s.cookies = (&http.Response{Header: header}).Cookies()
	return s
}

// Get sends an authenticated GET request to path (e.g. "/api/v1/people").
func (s *Session) Get(t testing.TB, path string) (int, http.Header, []byte) {
	t.Helper()
	return s.do(t, http.MethodGet, path, nil)
}

// Do sends an authenticated request with an optional JSON body. Unsafe
// methods carry the CSRF token and the workspace Origin.
func (s *Session) Do(t testing.TB, method, path string, body any) (int, http.Header, []byte) {
	t.Helper()
	return s.do(t, method, path, body)
}

func (s *Session) do(t testing.TB, method, path string, body any) (int, http.Header, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.BaseURL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range s.cookies {
		// Only name and value go into a Cookie request header.
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value}) //nolint:gosec // a request cookie, not a Set-Cookie

	}
	if method != http.MethodGet && method != http.MethodHead && s.CSRFToken != "" {
		req.Header.Set("X-CSRF-Token", s.CSRFToken)
		req.Header.Set("Origin", s.Origin)
	}
	res, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("demotest: %s %s: %v", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, b
}
