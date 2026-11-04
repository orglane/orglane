//go:build githubimporttest

package peopleimport

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"
)

// Only compiled into the UI acceptance binary, never production. The fixture
// server supplies synthetic source data and requires a synthetic credential.
func init() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-github-token" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/orgs/fixture":
			_, _ = fmt.Fprint(w, `{"id":4242}`)
		case "/orgs/fixture/members":
			if r.URL.Query().Get("page") != "1" {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			_, _ = fmt.Fprint(w, `[{"id":101,"login":"fixture-person"},{"id":102,"login":"no-email"}]`)
		case "/user/101":
			_, _ = fmt.Fprint(w, `{"id":101,"login":"fixture-person","name":"GitHub Fixture Person","email":"github-fixture@example.test","avatar_url":"https://avatars.githubusercontent.com/u/101"}`)
		case "/user/102":
			_, _ = fmt.Fprint(w, `{"id":102,"login":"no-email","name":"Missing Public Email","email":null}`)
		default:
			w.WriteHeader(404)
		}
	}))
	githubHTTPClient = func() *http.Client {
		return &http.Client{Timeout: time.Second, Transport: githubFixtureTransport{server.URL}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
}

type githubFixtureTransport struct{ origin string }

func (t githubFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	u := *r.URL
	clone.URL = &u
	target, _ := http.NewRequest(http.MethodGet, t.origin, nil)
	clone.URL.Scheme = target.URL.Scheme
	clone.URL.Host = target.URL.Host
	return http.DefaultTransport.RoundTrip(clone)
}
