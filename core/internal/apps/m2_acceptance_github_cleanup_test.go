package apps_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// m2GitHubAPI is GitHub.com's REST API. The cleanup helper takes the base
// URL as a parameter so its immutable-ID behavior is tested against a local
// server without contacting GitHub.
const m2GitHubAPI = "https://api.github.com"

// m2GitHubRequest sends a request to the real GitHub.com API.
func m2GitHubRequest(t *testing.T, client *http.Client, method, path, token string, body any) (int, []byte) {
	t.Helper()
	return m2GitHubRequestAt(t.Context(), t, client, m2GitHubAPI, method, path, token, body)
}

// m2GitHubRequestAt sends a request to a GitHub API at baseURL. The base URL
// is a test constant or an httptest server; the path is a test constant or a
// URL-escaped provider value, never user input.
func m2GitHubRequestAt(ctx context.Context, t *testing.T, client *http.Client, baseURL, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var in io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		in = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, in) //nolint:gosec // test-only base URL
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "token "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req) //nolint:gosec // test-only base URL
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out
}

// m2DeleteGitHubTeam removes the temporary team after the live test; a 404
// is fine (it may have been removed already). It never deletes by slug:
//
//   - a team ID recorded by the mapping is read and deleted by the
//     immutable organization/team IDs;
//   - without a recorded ID, the organization's teams are listed, the one
//     with this test's slug and Orglane instance marker is identified, and
//     only its numeric ID is deleted. If the slug is held by another team,
//     nothing is deleted.
//
// A team renamed between discovery and deletion is still the team whose ID
// was discovered; a foreign team that takes the old slug is never touched.
func m2DeleteGitHubTeam(ctx context.Context, t *testing.T, client *http.Client, baseURL, token, org, orgID, slug, teamID, marker string) {
	t.Helper()
	if teamID == "" {
		found, ok := m2FindGitHubTeam(ctx, t, client, baseURL, token, org, slug, marker)
		if !ok {
			return
		}
		teamID = found
	} else {
		status, body := m2GitHubRequestAt(ctx, t, client, baseURL, http.MethodGet,
			"/organizations/"+url.PathEscape(orgID)+"/team/"+url.PathEscape(teamID), token, nil)
		switch {
		case status == http.StatusNotFound:
			return
		case status != http.StatusOK:
			t.Errorf("cleanup: read team %s by ID = %d %s", teamID, status, body)
			return
		}
		var team struct {
			ID          int64  `json:"id"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(body, &team); err != nil || strconv.FormatInt(team.ID, 10) != teamID || !strings.HasPrefix(team.Description, marker) {
			t.Errorf("cleanup: team %s is not this test's (ID %d, description %q)", teamID, team.ID, team.Description)
			return
		}
	}
	status, body := m2GitHubRequestAt(ctx, t, client, baseURL, http.MethodDelete,
		"/organizations/"+url.PathEscape(orgID)+"/team/"+url.PathEscape(teamID), token, nil)
	if status != http.StatusNoContent && status != http.StatusNotFound {
		t.Errorf("cleanup: delete team %s = %d %s", teamID, status, body)
	}
}

// m2FindGitHubTeam finds a test team by slug and instance marker and returns
// its immutable numeric ID. It never trusts the slug alone.
func m2FindGitHubTeam(ctx context.Context, t *testing.T, client *http.Client, baseURL, token, org, slug, marker string) (string, bool) {
	t.Helper()
	for page := 1; page <= 10; page++ {
		path := "/orgs/" + url.PathEscape(org) + "/teams?per_page=100&page=" + strconv.Itoa(page)
		status, body := m2GitHubRequestAt(ctx, t, client, baseURL, http.MethodGet, path, token, nil)
		if status != http.StatusOK {
			t.Errorf("cleanup: list teams = %d %s", status, body)
			return "", false
		}
		var teams []struct {
			ID          int64  `json:"id"`
			Slug        string `json:"slug"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(body, &teams); err != nil {
			t.Errorf("cleanup: decode teams: %v", err)
			return "", false
		}
		for _, team := range teams {
			if strings.EqualFold(team.Slug, slug) && strings.HasPrefix(team.Description, marker) {
				return strconv.FormatInt(team.ID, 10), true
			}
		}
		if len(teams) < 100 {
			return "", false
		}
	}
	return "", false
}

// TestM2GitHubCleanupUsesImmutableTeamID is the regression for the team-slug
// cleanup race: the owned team is renamed and a foreign team takes its slug
// between discovery and deletion. The cleanup must delete the discovered
// numeric ID, never the team that now holds the slug.
func TestM2GitHubCleanupUsesImmutableTeamID(t *testing.T) {
	const (
		org    = "acme"
		orgID  = "9001"
		slug   = "orglane-test-123-1-m2"
		marker = "Managed by Orglane (instance)"
	)
	t.Run("owned team renamed before deletion", func(t *testing.T) {
		owned := &m2CleanupTeam{ID: 5001, Slug: slug, Description: marker}
		foreign := &m2CleanupTeam{ID: 5002, Slug: "renamed-owned", Description: "not this test's team"}
		var deleted []string
		srv := newM2CleanupServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/orgs/"+org+"/teams":
				// The list is the snapshot the cleanup sees; then the race
				// happens: the owned team is renamed and a foreign team takes
				// its old slug.
				snapshot := *owned
				owned.Slug = "renamed-owned"
				foreign.Slug = slug
				writeM2CleanupJSON(t, w, []m2CleanupTeam{snapshot})
			case r.Method == http.MethodDelete && r.URL.Path == "/organizations/"+orgID+"/team/5001":
				deleted = append(deleted, r.URL.Path)
				owned.Deleted = true
				w.WriteHeader(http.StatusNoContent)
			case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/orgs/"+org+"/teams/"):
				deleted = append(deleted, r.URL.Path)
				foreign.Deleted = true
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected cleanup request %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		})
		m2DeleteGitHubTeam(t.Context(), t, srv.Client(), srv.URL, "test-token", org, orgID, slug, "", marker)
		if !owned.Deleted {
			t.Fatal("the discovered team was not deleted by its immutable ID")
		}
		if foreign.Deleted {
			t.Fatal("cleanup deleted the foreign team that took the old slug")
		}
		if len(deleted) != 1 || deleted[0] != "/organizations/"+orgID+"/team/5001" {
			t.Fatalf("cleanup deletes = %v, want only the immutable-ID route", deleted)
		}
	})

	t.Run("no recorded ID and the slug is foreign", func(t *testing.T) {
		var deleted []string
		srv := newM2CleanupServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/orgs/"+org+"/teams":
				writeM2CleanupJSON(t, w, []m2CleanupTeam{{ID: 5002, Slug: slug, Description: "not this test's team"}})
			case r.Method == http.MethodDelete:
				deleted = append(deleted, r.URL.Path)
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected cleanup request %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		})
		m2DeleteGitHubTeam(t.Context(), t, srv.Client(), srv.URL, "test-token", org, orgID, slug, "", marker)
		if len(deleted) != 0 {
			t.Fatalf("cleanup deleted a foreign team: %v", deleted)
		}
	})
}

type m2CleanupTeam struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Deleted     bool   `json:"-"`
}

func newM2CleanupServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func writeM2CleanupJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("write cleanup response: %v", err)
	}
}
