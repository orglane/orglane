//go:build integration

package github

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

// TestGitHubLiveJanitor is the workflow's separately invoked always-after-tests
// cleanup. Merely listing tests cannot execute it. It never touches accounts or
// memberships, and unknown ownership/age never authorizes a deletion.
func TestGitHubLiveJanitor(t *testing.T) {
	requireGitHubLive(t)
	repository := os.Getenv("GITHUB_REPOSITORY")
	workflowToken := os.Getenv("GITHUB_TOKEN")
	currentRun := os.Getenv("GITHUB_RUN_ID")
	if repository != "orglane/orglane" || !idPattern.MatchString(currentRun) || workflowToken == "" {
		t.Fatal("janitor needs GITHUB_REPOSITORY and ephemeral GITHUB_TOKEN with actions:read to verify resource ages")
	}
	env := &liveEnv{client: &http.Client{Timeout: 30 * time.Second}, key: []byte(os.Getenv("ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY"))}
	defer clear(env.key)
	inst := provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{"organization": os.Getenv("ORGLANE_TEST_GITHUB_ORG"), "app_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_ID"), "app_client_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_CLIENT_ID"), "installation_id": os.Getenv("ORGLANE_TEST_GITHUB_INSTALLATION_ID")}}
	if !ValidOrganization(setting(inst, "organization")) {
		t.Fatal("invalid dedicated organization")
	}
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
	if strconv.FormatInt(app.ID, 10) != setting(inst, "app_id") || app.ClientID != setting(inst, "app_client_id") {
		t.Fatal("janitor App identity mismatch")
	}
	a := New()
	installation, err := a.VerifyInstallation(t.Context(), env, inst, setting(inst, "installation_id"))
	if err != nil {
		t.Fatal(err)
	}
	org := installation.Account.Login
	orgID := strconv.FormatInt(installation.Account.ID, 10)
	inst.Settings["organization_id"] = orgID
	token, err := a.InstallationToken(t.Context(), env, inst)
	if err != nil {
		t.Fatal(err)
	}
	// The bot token is existing shared test configuration; no broader App
	// permission or additional stored secret is requested for cleanup.
	botToken := os.Getenv("ORGLANE_TEST_GITHUB_USER_TOKEN")
	var bot struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err = request(t.Context(), env, http.MethodGet, "/user", botToken, nil, &bot); err != nil {
		t.Fatal("janitor bot identity:", err)
	}
	if strconv.FormatInt(bot.ID, 10) != os.Getenv("ORGLANE_TEST_GITHUB_USER_ID") || !strings.EqualFold(bot.Login, os.Getenv("ORGLANE_TEST_GITHUB_USER_LOGIN")) {
		t.Fatal("janitor token does not belong to the configured dedicated bot")
	}

	origins := map[string]janitorOrigin{}
	originFor := func(resource janitorResource) (janitorOrigin, bool) {
		match := janitorNamePattern.FindStringSubmatch(resource.Name)
		if len(match) != 4 || match[1] == currentRun {
			return janitorOrigin{}, false
		}
		candidate := janitorOrigin{Repository: repository, RunID: match[1], Attempt: match[2]}
		if resource.Description != "orglane-test:"+repository+":"+candidate.RunID+":"+candidate.Attempt {
			return janitorOrigin{}, false
		}
		key := candidate.RunID + "/" + candidate.Attempt
		if known, ok := origins[key]; ok {
			return known, true
		}
		var run janitorAttempt
		path := "/repos/" + repository + "/actions/runs/" + candidate.RunID + "/attempts/" + candidate.Attempt
		if err := request(t.Context(), env, http.MethodGet, path, workflowToken, nil, &run); err != nil {
			if apiNotFound(err) {
				t.Log("Skipped a resource whose originating workflow attempt no longer exists; ownership/age cannot be proven")
				return janitorOrigin{}, false
			}
			t.Fatal("janitor workflow age lookup:", err)
		}
		verified, ok := run.origin(candidate)
		if !ok {
			t.Log("Skipped a resource with unverifiable originating workflow metadata")
			return janitorOrigin{}, false
		}
		origins[key] = verified
		return verified, true
	}
	teams := livePages[liveTeam](t, env, "/orgs/"+org+"/teams", token)
	deletedTeams := 0
	for _, team := range teams {
		resource := janitorResource{Name: team.Name, Description: team.Description, CreatedAt: team.CreatedAt}
		origin, ok := originFor(resource)
		if !ok || !janitorOwned(resource, origin, time.Now(), currentRun) {
			continue
		}
		if team.ID <= 0 {
			t.Fatal("invalid team ID in janitor response")
		}
		path := "/organizations/" + orgID + "/team/" + strconv.FormatInt(team.ID, 10)
		var current liveTeam
		if err = request(t.Context(), env, http.MethodGet, path, token, nil, &current); err != nil {
			t.Fatal(err)
		}
		if current.ID != team.ID || current.Organization.ID != installation.Account.ID {
			t.Fatal("team identity changed before cleanup")
		}
		resource = janitorResource{Name: current.Name, Description: current.Description, CreatedAt: current.CreatedAt}
		if !janitorOwned(resource, origin, time.Now(), currentRun) {
			continue
		}
		if !slugPattern.MatchString(current.Slug) {
			t.Fatal("invalid team slug")
		}
		var children []liveTeam
		if err = request(t.Context(), env, http.MethodGet, "/orgs/"+org+"/teams/"+current.Slug+"/teams?per_page=1", token, nil, &children); err != nil {
			t.Fatal(err)
		}
		// Deleting a parent team can cascade to child teams whose ownership is
		// unknown. Never do that, even for an old, correctly marked parent.
		if len(children) > 0 {
			t.Log("Skipped an owned parent team to preserve child teams")
			continue
		}
		if err = request(t.Context(), env, http.MethodDelete, path, token, nil, nil); err != nil {
			t.Fatal("janitor team deletion:", err)
		}
		deletedTeams++
	}
	repos := livePages[liveRepository](t, env, "/orgs/"+org+"/repos?type=all", botToken)
	deletedRepos := 0
	for _, repo := range repos {
		if repo.Owner.ID != installation.Account.ID {
			continue
		}
		resource := janitorResource{Name: repo.Name, Description: repo.Description, CreatedAt: repo.CreatedAt}
		origin, ok := originFor(resource)
		if !ok || !janitorOwned(resource, origin, time.Now(), currentRun) {
			continue
		}
		// Repositories must have their own creation timestamp as well as the
		// originating attempt's age. Unlike teams, missing created_at is invalid.
		if repo.CreatedAt.IsZero() {
			t.Log("Skipped a repository with unknown creation age")
			continue
		}
		if repo.ID <= 0 {
			t.Fatal("invalid repository ID in janitor response")
		}
		var current liveRepository
		if err = request(t.Context(), env, http.MethodGet, "/repositories/"+strconv.FormatInt(repo.ID, 10), botToken, nil, &current); err != nil {
			t.Fatal(err)
		}
		resource = janitorResource{Name: current.Name, Description: current.Description, CreatedAt: current.CreatedAt}
		if current.ID != repo.ID || current.Owner.ID != installation.Account.ID || current.CreatedAt.IsZero() || !janitorOwned(resource, origin, time.Now(), currentRun) {
			continue
		}
		if !strings.EqualFold(current.FullName, org+"/"+current.Name) {
			t.Fatal("repository identity mismatch")
		}
		if err = request(t.Context(), env, http.MethodDelete, "/repos/"+org+"/"+current.Name, botToken, nil, nil); err != nil {
			t.Fatal("janitor repository deletion (existing test bot must be authorized; cleanup fails closed):", err)
		}
		deletedRepos++
	}
	t.Logf("Janitor deleted %d old owned test teams and %d old owned test repositories; unmarked, unknown-age, recent and foreign resources were preserved", deletedTeams, deletedRepos)
}

type liveTeam struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	Description  string    `json:"description"`
	CreatedAt    time.Time `json:"created_at"`
	Organization struct {
		ID int64 `json:"id"`
	} `json:"organization"`
}
type liveRepository struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	FullName    string    `json:"full_name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	Owner       struct {
		ID int64 `json:"id"`
	} `json:"owner"`
}

func apiNotFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}
func livePages[T any](t *testing.T, env provider.Env, path, token string) []T {
	t.Helper()
	var all []T
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	for page := 1; page <= 100; page++ {
		var items []T
		if err := request(t.Context(), env, http.MethodGet, fmt.Sprintf("%s%sper_page=100&page=%d", path, separator, page), token, nil, &items); err != nil {
			t.Fatal("janitor resource listing:", err)
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all
		}
	}
	t.Fatal("janitor resource listing exceeded 100 pages; no partial list is treated as complete")
	return nil
}
