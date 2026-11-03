package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// fakeRegistry serves /v2/<repo>/tags/list like GHCR: a bearer challenge,
// a token endpoint with basic auth, and Link pagination.
func fakeRegistry(t *testing.T, pages [][]string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			user, pass, ok := r.BasicAuth()
			if !ok || user != "bot" || pass != "secret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("scope") != "repository:orglane/orglane:pull" || r.URL.Query().Get("service") != "test" {
				t.Errorf("token request %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		case "/v2/orglane/orglane/tags/list":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="`+srv.URL+`/token",service="test",scope="repository:orglane/orglane:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			page := 0
			if last := r.URL.Query().Get("last"); last != "" {
				page = slices.IndexFunc(pages, func(p []string) bool { return p[len(p)-1] == last }) + 1
			}
			if page+1 < len(pages) {
				w.Header().Set("Link", `</v2/orglane/orglane/tags/list?n=1000&last=`+pages[page][len(pages[page])-1]+`>; rel="next"`)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "orglane/orglane", "tags": pages[page]})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestListTags(t *testing.T) {
	srv := fakeRegistry(t, [][]string{{"0.1.0", "0.1"}, {"latest", "0.2.0"}, {"0.2"}})
	reg := Registry{URL: srv.URL, Username: "bot", Password: "secret", Client: srv.Client()}
	got, err := reg.ListTags(t.Context(), "orglane/orglane")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0.1.0", "0.1", "latest", "0.2.0", "0.2"}; !slices.Equal(got, want) {
		t.Errorf("tags = %q, want %q", got, want)
	}
}

func TestListTagsErrors(t *testing.T) {
	srv := fakeRegistry(t, [][]string{{"0.1.0"}})

	wrongPassword := Registry{URL: srv.URL, Username: "bot", Password: "wrong", Client: srv.Client()}
	if _, err := wrongPassword.ListTags(t.Context(), "orglane/orglane"); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("wrong password: err = %v, want a token error", err)
	}

	unknown := Registry{URL: srv.URL, Username: "bot", Password: "secret", Client: srv.Client()}
	if _, err := unknown.ListTags(t.Context(), "orglane/other"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unknown repository: err = %v, want 404", err)
	}

	// Credentials are never sent to a realm on another host.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			t.Errorf("credentials sent to %s", r.Host)
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://evil.example/token",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer foreign.Close()
	evil := Registry{URL: foreign.URL, Username: "bot", Password: "secret", Client: foreign.Client()}
	if _, err := evil.ListTags(t.Context(), "orglane/orglane"); err == nil || !strings.Contains(err.Error(), "realm") {
		t.Errorf("foreign realm: err = %v, want a realm error", err)
	}

	// Pagination must stay on the registry.
	leaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://evil.example/v2/x/tags/list?last=a>; rel="next"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"a"}})
	}))
	defer leaky.Close()
	if _, err := (Registry{URL: leaky.URL, Client: leaky.Client()}).ListTags(t.Context(), "x"); err == nil {
		t.Error("pagination to another host succeeded, want an error")
	}

	// A registry that always returns a next page is cut off.
	endless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `</v2/x/tags/list?last=a>; rel="next"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"a"}})
	}))
	defer endless.Close()
	if _, err := (Registry{URL: endless.URL, Client: endless.Client()}).ListTags(t.Context(), "x"); err == nil ||
		!strings.Contains(err.Error(), "pages") {
		t.Errorf("endless pagination: err = %v, want a page limit error", err)
	}
}

// redirectingRegistry is a registry on origin that answers the challenge
// normally but redirects at one step: "token" (the token endpoint, which
// receives Basic credentials) or "list" (the authenticated tag list, which
// receives the bearer token). Requests whose Host starts with "evil." are the
// redirect target: they must never carry credentials.
func redirectingRegistry(t *testing.T, at string, target func(r *http.Request) string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "evil.") {
			t.Errorf("redirect target %s%s was requested", r.Host, r.URL.Path)
			if r.Header.Get("Authorization") != "" {
				t.Errorf("credentials reached the redirect target %s: %q", r.Host, r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "stolen", "tags": []string{"9.9.9"}})
			return
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		switch r.URL.Path {
		case "/token":
			if at == "token" {
				http.Redirect(w, r, target(r)+"/token", http.StatusFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		case "/v2/orglane/orglane/tags/list":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+scheme+`://`+r.Host+`/token",service="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if at == "list" {
				http.Redirect(w, r, target(r)+r.URL.RequestURI(), http.StatusTemporaryRedirect) //nolint:gosec // test registry that redirects on purpose
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"0.1.0"}})
		default:
			http.NotFound(w, r)
		}
	})
}

// TestListTagsRefusesRedirects checks that neither the Basic credentials nor
// the bearer token follow a redirect off the registry origin: to another
// port, a subdomain, or from HTTPS to HTTP.
func TestListTagsRefusesRedirects(t *testing.T) {
	for _, at := range []string{"token", "list"} {
		t.Run(at+"/cross-port", func(t *testing.T) {
			// The target is the same host on another port.
			target := httptest.NewServer(asRedirectTarget(t, at))
			defer target.Close()
			targetPort := target.Listener.Addr().(*net.TCPAddr).Port
			srv := httptest.NewServer(redirectingRegistry(t, at, func(*http.Request) string {
				return fmt.Sprintf("http://127.0.0.1:%d", targetPort)
			}))
			defer srv.Close()
			assertRedirectRefused(t, Registry{URL: srv.URL, Username: "bot", Password: "secret", Client: srv.Client()})
		})

		t.Run(at+"/cross-subdomain", func(t *testing.T) {
			srv := httptest.NewServer(redirectingRegistry(t, at, func(r *http.Request) string {
				return "http://evil." + r.Host
			}))
			defer srv.Close()
			// Resolve every host name to the test server, so that
			// registry.test and evil.registry.test are different hosts on it.
			addr := srv.Listener.Addr().String()
			client := &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				},
			}}
			_, port, _ := net.SplitHostPort(addr)
			assertRedirectRefused(t, Registry{URL: "http://registry.test:" + port, Username: "bot", Password: "secret", Client: client})
		})

		t.Run(at+"/https-to-http", func(t *testing.T) {
			plain := httptest.NewServer(asRedirectTarget(t, at))
			defer plain.Close()
			srv := httptest.NewTLSServer(redirectingRegistry(t, at, func(*http.Request) string { return plain.URL }))
			defer srv.Close()
			assertRedirectRefused(t, Registry{URL: srv.URL, Username: "bot", Password: "secret", Client: srv.Client()})
		})
	}
}

// asRedirectTarget serves as the redirect target on its own server: its
// requests are told apart by the "evil." Host prefix.
func asRedirectTarget(t *testing.T, at string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = "evil." + r.Host
		redirectingRegistry(t, at, nil).ServeHTTP(w, r)
	})
}

func assertRedirectRefused(t *testing.T, reg Registry) {
	t.Helper()
	tags, err := reg.ListTags(t.Context(), "orglane/orglane")
	if !errors.Is(err, errRedirect) {
		t.Errorf("ListTags = %q, %v; want errRedirect", tags, err)
	}
}

// TestListTagsDoesNotChangeTheCallersClient checks that refusing redirects
// does not leak into the client passed in.
func TestListTagsDoesNotChangeTheCallersClient(t *testing.T) {
	srv := fakeRegistry(t, [][]string{{"0.1.0"}})
	client := srv.Client()
	if _, err := (Registry{URL: srv.URL, Username: "bot", Password: "secret", Client: client}).ListTags(t.Context(), "orglane/orglane"); err != nil {
		t.Fatal(err)
	}
	if client.CheckRedirect != nil {
		t.Error("ListTags changed the caller's client")
	}
}
