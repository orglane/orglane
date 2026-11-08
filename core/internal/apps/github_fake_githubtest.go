//go:build githubtest

package apps

import (
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/orglane/orglane/providers/github/githubfake"
)

// Only compiled into the UI acceptance binary (web/e2e), never production:
// with ORGLANE_TEST_GITHUB_FAKE_ADDR set, GitHub.com is the fake GitHub on
// that address, validated against GitHub's OpenAPI description. The browser
// reaches its authorization page there through Playwright routing.
//
// The fake starts with the first provider HTTP client, which only the
// server builds: CLI commands run with the same environment while the
// server holds the address, so they must not listen on it.
func init() {
	addr := os.Getenv("ORGLANE_TEST_GITHUB_FAKE_ADDR")
	if addr == "" {
		return
	}
	route := sync.OnceValue(func() http.RoundTripper {
		fake, err := githubfake.Listen(func(err error) { log.Printf("fake GitHub violation: %v", err) }, addr)
		if err != nil {
			log.Fatalf("fake GitHub: %v", err)
		}
		return fake.Transport()
	})
	testProviderTransport = func(next http.RoundTripper) http.RoundTripper {
		fake := route()
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "api.github.com" || r.URL.Host == "github.com" {
				return fake.RoundTrip(r)
			}
			return next.RoundTrip(r)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
