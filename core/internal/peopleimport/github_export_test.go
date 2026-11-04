package peopleimport

import (
	"context"
	"net/http"
)

// GitHubTestSource injects an httptest endpoint only into test binaries.
func GitHubTestSource(ctx context.Context, origin string, client *http.Client) Source {
	return &githubSource{ctx: ctx, organization: "fixture", token: "test-only-token", origin: origin, client: client}
}

// SetGitHubTestClient prevents production network access in fetch authorization tests.
func SetGitHubTestClient(client *http.Client) func() {
	previous := githubHTTPClient
	githubHTTPClient = func() *http.Client { return client }
	return func() { githubHTTPClient = previous }
}
