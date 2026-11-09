//go:build integration

package apps_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/store"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
)

// TestM2AcceptanceGitHubLive is the GitHub part of the M2 acceptance
// scenario (issue #67): the real GitHub adapter in the core's provisioning
// pipeline connects the dedicated test organization and gives an invited
// person access through a mapped team. It contacts GitHub.com and therefore
// runs only with the github-live environment; without its variables it
// skips with an explicit message (docs/specs/providers.md "GitHub live
// tests"). The Forgejo part, including the mention event and silent SSO, is
// TestM2AcceptanceScenario.
func TestM2AcceptanceGitHubLive(t *testing.T) {
	requireGitHubLiveScenario(t)
	ctx := t.Context()
	org, appID, installationID := os.Getenv("ORGLANE_TEST_GITHUB_ORG"), os.Getenv("ORGLANE_TEST_GITHUB_APP_ID"), os.Getenv("ORGLANE_TEST_GITHUB_INSTALLATION_ID")
	clientID, botLogin, botID := os.Getenv("ORGLANE_TEST_GITHUB_APP_CLIENT_ID"), os.Getenv("ORGLANE_TEST_GITHUB_USER_LOGIN"), os.Getenv("ORGLANE_TEST_GITHUB_USER_ID")
	key, webhookSecret := []byte(os.Getenv("ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY")), []byte(os.Getenv("ORGLANE_TEST_GITHUB_WEBHOOK_SECRET"))
	// Cleanups run last-in-first-out: clear the key after the team cleanup
	// that still needs it.
	t.Cleanup(func() {
		clear(key)
		clear(webhookSecret)
	})
	client := &http.Client{Timeout: 30 * time.Second}
	// Resolving the organization and minting installation tokens use the
	// App's real credentials; every request goes to GitHub.com.
	env := &managedtest.Env{Client: client, Creds: map[string][]byte{"app_private_key": key}}
	_, orgID, err := githubprovider.ResolveOrganization(ctx, env, org)
	if err != nil {
		t.Fatalf("resolve the dedicated test organization: %v", err)
	}

	h := newHarnessWith(t, apps.Config{}, githubprovider.New())
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
		Provider: "github", Key: "github", State: apps.StateActive, BaseURL: "https://github.com",
		Settings: map[string]any{
			"organization": org, "organization_id": orgID, "app_id": appID,
			"app_client_id": clientID, "installation_id": installationID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{"app_private_key": key, "webhook_secret": webhookSecret} {
		if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, name, value); err != nil {
			t.Fatalf("store %s: %v", name, err)
		}
	}

	// The invited person's GitHub account is the dedicated bot. A verified
	// link is test setup: the browser authorization stays a manual
	// pre-release check (docs/specs/providers.md).
	person := h.person(strings.ToLower(botLogin))
	if _, err := store.New(h.pool).InsertAccountLink(h.ctx, store.InsertAccountLinkParams{
		PersonID: person.ID, Provider: "github", Realm: githubprovider.Realm,
		ExternalID: botID, Login: botLogin, Method: provider.LinkOAuth,
	}); err != nil {
		t.Fatalf("link the bot account: %v", err)
	}
	teamSlug, teamID := m2GitHubTeamSlug(), ""
	team := h.team(teamSlug, person)
	marker := "Managed by Orglane (" + inst.ID.String() + ")"
	// The team is Orglane's until the mapping sync creates it at GitHub;
	// deleting a team that was never created is a 404 and fine. Cleanup
	// discovers it by marker if the mapping is not recorded yet and deletes
	// it only by immutable organization and team IDs.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		token, err := githubprovider.New().InstallationToken(cleanupCtx, env, inst.View())
		if err != nil {
			t.Errorf("cleanup: installation token: %v", err)
			return
		}
		m2DeleteGitHubTeam(cleanupCtx, t, client, m2GitHubAPI, token, org, orgID, teamSlug, teamID, marker)
	})
	h.mapTeam(inst, team)
	h.drain()
	result := h.result(inst, person)
	if result.Status != apps.StatusDone {
		t.Fatalf("the person's GitHub provisioning = %+v, want done", result)
	}

	mapping, err := store.New(h.pool).GetAppTeamMapping(h.ctx, store.GetAppTeamMappingParams{InstanceID: inst.ID, TeamID: team.ID})
	if err != nil || mapping.ExternalRef == nil || mapping.ExternalName == nil {
		t.Fatalf("mapped team = %+v, %v", mapping, err)
	}
	// GitHub may have normalized the slug; the cleanup closes over the
	// actual one and the numeric ID once the mapping is known.
	teamSlug, teamID = *mapping.ExternalName, *mapping.ExternalRef

	// The real API confirms the access: the bot is an active member of the
	// team Orglane created for the mapped team.
	token, err := githubprovider.New().InstallationToken(ctx, h.svc.EnvForTest(inst), inst.View())
	if err != nil {
		t.Fatalf("installation token: %v", err)
	}
	status, body := m2GitHubRequest(t, client, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(teamSlug)+"/memberships/"+url.PathEscape(botLogin), token, nil)
	var membership struct{ State, Role string }
	if status != http.StatusOK || json.Unmarshal(body, &membership) != nil || membership.State != "active" {
		t.Fatalf("team membership = %d %s, want active", status, body)
	}
	t.Logf("Connected the real GitHub App to %s, added @%s (ID %s) to team %s through core provisioning.", org, botLogin, botID, teamSlug)
}

// requireGitHubLiveScenario follows the provider live tests' contract: the
// github-live environment turns missing configuration into a failure,
// everywhere else the scenario skips with a message naming what is missing.
func requireGitHubLiveScenario(t *testing.T) {
	t.Helper()
	names := []string{
		"ORGLANE_TEST_GITHUB_ORG", "ORGLANE_TEST_GITHUB_APP_ID", "ORGLANE_TEST_GITHUB_APP_CLIENT_ID",
		"ORGLANE_TEST_GITHUB_INSTALLATION_ID", "ORGLANE_TEST_GITHUB_USER_LOGIN", "ORGLANE_TEST_GITHUB_USER_ID",
		"ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY", "ORGLANE_TEST_GITHUB_WEBHOOK_SECRET", "ORGLANE_TEST_GITHUB_USER_TOKEN",
	}
	var missing []string
	for _, name := range names {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return
	}
	const message = "the GitHub part of the M2 acceptance scenario needs the github-live environment (ORGLANE_TEST_GITHUB_*, see docs/specs/providers.md#github-live-tests); GitHub.com is not mocked in integration tests"
	if os.Getenv("ORGLANE_REQUIRE_GITHUB_LIVE") == "1" {
		t.Fatalf("%s; missing: %s", message, strings.Join(missing, ", "))
	}
	t.Skipf("%s; missing: %s. Run it with the github-live workflow or the same environment locally.", message, strings.Join(missing, ", "))
}

// m2GitHubTeamSlug names the temporary team after the run and attempt, as
// the live-test conventions require.
func m2GitHubTeamSlug() string {
	suffix := os.Getenv("GITHUB_RUN_ID")
	if attempt := os.Getenv("GITHUB_RUN_ATTEMPT"); attempt != "" {
		suffix += "-" + attempt
	}
	if suffix == "" {
		suffix = strconv.FormatInt(time.Now().Unix(), 10)
	}
	var b strings.Builder
	for _, r := range strings.ToLower(suffix) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return "orglane-test-" + strings.Trim(b.String(), "-") + "-m2"
}
