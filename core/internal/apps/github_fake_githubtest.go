//go:build githubtest

package apps

import (
	"log"
	"net/http"
	"os"

	"github.com/orglane/orglane/providers/github/githubfake"
)

// Only compiled into the UI acceptance binary (web/e2e), never production:
// with ORGLANE_TEST_GITHUB_FAKE_ADDR set, GitHub.com is the fake GitHub on
// that address, validated against GitHub's OpenAPI description. The browser
// reaches its authorization page there through Playwright routing.
func init() {
	addr := os.Getenv("ORGLANE_TEST_GITHUB_FAKE_ADDR")
	if addr == "" {
		return
	}
	fake, err := githubfake.Listen(func(err error) { log.Printf("fake GitHub violation: %v", err) }, addr)
	if err != nil {
		log.Fatalf("fake GitHub: %v", err)
	}
	route := fake.Transport()
	testProviderTransport = func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "api.github.com" || r.URL.Host == "github.com" {
				return route.RoundTrip(r)
			}
			return next.RoundTrip(r)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
