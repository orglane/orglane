//go:build integration

package github

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

// Live tests contact only the explicitly supplied dedicated test organization.
// No provider API is mocked, and this connection test changes no org resources.
func TestGitHubLiveConnection(t *testing.T) {
	requireGitHubLive(t)
	org := os.Getenv("ORGLANE_TEST_GITHUB_ORG")
	if !ValidOrganization(org) {
		t.Fatal("invalid dedicated test organization login")
	}
	env := &liveEnv{client: &http.Client{Timeout: 30 * time.Second}, key: []byte(os.Getenv("ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY"))}
	defer clear(env.key)
	inst := provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{"organization": org, "app_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_ID"), "app_client_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_CLIENT_ID"), "installation_id": os.Getenv("ORGLANE_TEST_GITHUB_INSTALLATION_ID")}}
	jwt, err := jwtFor(t.Context(), env, inst)
	if err != nil {
		t.Fatal(err)
	}
	var app struct {
		ID       int64  `json:"id"`
		ClientID string `json:"client_id"`
	}
	if err = request(t.Context(), env, http.MethodGet, "/app", jwt, nil, &app); err != nil {
		t.Fatal(err)
	}
	if app.ClientID != setting(inst, "app_client_id") {
		t.Fatal("App client ID mismatch")
	}
	a := New()
	result, err := a.CompleteConnect(t.Context(), env, inst, "install_app", url.Values{"installation_id": {setting(inst, "installation_id")}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Done || result.BaseURL.Host != "github.com" || !strings.EqualFold(result.Settings["organization"].(string), org) || result.Settings["organization_id"] == "" {
		t.Fatal("connection did not verify expected org")
	}
	for k, v := range result.Settings {
		inst.Settings[k] = v
	}
	token, err := a.InstallationToken(t.Context(), env, inst)
	if err != nil {
		t.Fatal(err)
	}
	var repositories struct {
		Total int `json:"total_count"`
	}
	if err = request(t.Context(), env, http.MethodGet, "/installation/repositories", token, nil, &repositories); err != nil {
		t.Fatal(err)
	}
	cached, err := a.InstallationToken(t.Context(), env, inst)
	if err != nil || cached != token {
		t.Fatal("installation token cache failed")
	}
	t.Log("Verified real App identity, expected organization, installation permissions, token issuance and authenticated repository read; browser manifest/installation and OAuth authorization remain manual acceptance.")
}

// This Env uses the production protocol with a real HTTP client.
type liveEnv struct {
	client *http.Client
	key    []byte
}

func (e *liveEnv) Credential(_ context.Context, _ string) ([]byte, error) {
	return append([]byte(nil), e.key...), nil
}
func (e *liveEnv) Credentials(ctx context.Context, name string) ([][]byte, error) {
	v, err := e.Credential(ctx, name)
	return [][]byte{v}, err
}
func (e *liveEnv) HTTPClient() *http.Client { return e.client }
func (*liveEnv) Logger() *slog.Logger       { return slog.Default() }
func (*liveEnv) Now() time.Time             { return time.Now() }
func (*liveEnv) WorkspaceURL() *url.URL     { return &url.URL{Scheme: "https", Host: "work.example.test"} }
func (e *liveEnv) IssuerURL() *url.URL      { return e.WorkspaceURL() }
func (e *liveEnv) WebhookURL(id uuid.UUID) *url.URL {
	u := e.WorkspaceURL()
	u.Path = "/api/v1/webhooks/" + id.String()
	return u
}

func requireGitHubLive(t *testing.T) {
	t.Helper()
	names := []string{"ORGLANE_TEST_GITHUB_ORG", "ORGLANE_TEST_GITHUB_APP_ID", "ORGLANE_TEST_GITHUB_APP_CLIENT_ID", "ORGLANE_TEST_GITHUB_INSTALLATION_ID", "ORGLANE_TEST_GITHUB_USER_LOGIN", "ORGLANE_TEST_GITHUB_USER_ID", "ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY", "ORGLANE_TEST_GITHUB_WEBHOOK_SECRET", "ORGLANE_TEST_GITHUB_USER_TOKEN"}
	missing := []string{}
	for _, name := range names {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		const message = "GitHub live tests need ORGLANE_TEST_GITHUB_* (see docs/specs/providers.md#github-live-tests)"
		if os.Getenv("ORGLANE_REQUIRE_GITHUB_LIVE") == "1" {
			t.Fatalf("%s; missing: %s", message, strings.Join(missing, ", "))
		}
		t.Skip(message)
	}
}
