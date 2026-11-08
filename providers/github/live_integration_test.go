//go:build integration

package github

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
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

// The link flow's GitHub side against the real API: the authorization URL
// GitHub accepts for the dedicated App, and the account lookup by user
// token that binds a link to the immutable user ID (the bot's token stands
// in for the token the browser authorization would yield; the interactive
// authorization page itself stays manual acceptance).
func TestGitHubLiveAccountLink(t *testing.T) {
	requireGitHubLive(t)
	env := &liveEnv{client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	inst := provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{"app_client_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_CLIENT_ID")}}
	u, err := New().BeginVerify(t.Context(), env, inst, "ols_live", strings.Repeat("c", 43))
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode >= 400 {
		t.Fatalf("GitHub refused the authorization URL: %d", res.StatusCode)
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := request(t.Context(), env, http.MethodGet, "/user", os.Getenv("ORGLANE_TEST_GITHUB_USER_TOKEN"), nil, &user); err != nil {
		t.Fatal(err)
	}
	if strconv.FormatInt(user.ID, 10) != os.Getenv("ORGLANE_TEST_GITHUB_USER_ID") || !strings.EqualFold(user.Login, os.Getenv("ORGLANE_TEST_GITHUB_USER_LOGIN")) || user.Type != "User" {
		t.Fatal("the bot user's token does not resolve to the configured user ID")
	}
	t.Log("Verified the authorization URL and the user lookup by ID; the interactive authorization and code exchange remain manual acceptance.")
}

// GitHub provisioning against the real API: the dedicated bot user is
// invited by ID, added to and removed from a temporary team, removed from
// the organization and restored. Every temporary team follows the
// github-live naming and description convention and is deleted here (the
// janitor removes only leftovers of older runs).
func TestGitHubLiveProvisioning(t *testing.T) {
	requireGitHubLive(t)
	ctx := t.Context()
	org := os.Getenv("ORGLANE_TEST_GITHUB_ORG")
	if !ValidOrganization(org) {
		t.Fatal("invalid dedicated test organization login")
	}
	botID := os.Getenv("ORGLANE_TEST_GITHUB_USER_ID")
	botLogin := os.Getenv("ORGLANE_TEST_GITHUB_USER_LOGIN")
	botToken := os.Getenv("ORGLANE_TEST_GITHUB_USER_TOKEN")
	if !idPattern.MatchString(botID) || !loginPattern.MatchString(botLogin) || botToken == "" {
		t.Fatal("invalid dedicated bot configuration")
	}
	env := &liveEnv{client: &http.Client{Timeout: 30 * time.Second}, key: []byte(os.Getenv("ORGLANE_TEST_GITHUB_APP_PRIVATE_KEY"))}
	defer clear(env.key)
	inst := provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{"organization": org, "app_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_ID"), "app_client_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_CLIENT_ID"), "installation_id": os.Getenv("ORGLANE_TEST_GITHUB_INSTALLATION_ID")}}
	a := New()
	// The App slug names the bot that GitHub attributes installation-token
	// invitations to; read it rather than adding another required setting.
	jwt, err := jwtFor(ctx, env, inst)
	if err != nil {
		t.Fatal(err)
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := request(ctx, env, http.MethodGet, "/app", jwt, nil, &app); err != nil || !slugPattern.MatchString(app.Slug) {
		t.Fatalf("App identity = %+v, %v", app, err)
	}
	inst.Settings["app_slug"] = app.Slug
	token, err := a.InstallationToken(ctx, env, inst)
	if err != nil {
		t.Fatal(err)
	}

	// The setup requires the bot to be an ordinary member: the test invites,
	// removes and restores it.
	members, err := a.Members(ctx, env, inst)
	if err != nil {
		t.Fatal(err)
	}
	member := false
	for _, m := range members {
		if m.ExternalID == botID && strings.EqualFold(m.Login, botLogin) {
			member = true
		}
	}
	if !member {
		t.Fatalf("the dedicated bot @%s (ID %s) is not an organization member; the github-live setup requires one", botLogin, botID)
	}
	role, state, err := liveMembership(ctx, env, token, org, botLogin)
	if err != nil || state != "active" || role != "member" {
		t.Fatalf("bot membership = %q/%q, %v; the live-test bot must be a non-owner member", state, role, err)
	}

	teamSlug := liveTeamName("provisioning")
	refs, err := a.SyncTeams(ctx, env, inst, []provider.TeamMapping{{TeamID: uuid.New(), TeamSlug: teamSlug, TeamName: teamSlug, Mode: provider.MappingCreate}})
	if err != nil || len(refs) != 1 || refs[0].ExternalRef == "" || !strings.HasPrefix(refs[0].Name, "orglane-test-") {
		t.Fatalf("synced team = %+v, %v", refs, err)
	}
	ref := refs[0]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		liveDeleteTeam(ctx, t, env, token, org, ref.Name)
		liveEnsureMember(ctx, t, a, env, inst, token, botToken, org, botID, botLogin)
	})
	// The test deletes the team itself; also give it the janitor's provenance
	// so a crashed run leaves something the janitor can remove.
	if err := request(ctx, env, http.MethodPatch, "/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(ref.Name), token, map[string]any{"description": liveResourceDescription()}, nil); err != nil {
		t.Fatal("describe temporary team:", err)
	}
	teams, err := a.ProviderTeams(ctx, env, inst)
	if err != nil || !slices.ContainsFunc(teams, func(team provider.Team) bool { return team.ExternalRef == ref.ExternalRef }) {
		t.Fatalf("provider teams = %+v, %v", teams, err)
	}

	// The bot is added to and removed from the temporary team.
	d := provider.DesiredAccess{
		PersonID: uuid.New(), Username: botLogin, DisplayName: botLogin, WorkEmail: "bot@example.test",
		Access: true, Teams: []provider.TeamRef{ref}, Mapped: []provider.TeamRef{ref}, Managed: true,
		Link: &provider.AccountLink{ID: uuid.New(), Method: provider.LinkOAuth, Account: provider.ExternalAccount{Realm: Realm, ExternalID: botID, Login: botLogin}},
	}
	plan, err := a.Plan(ctx, env, inst, d)
	if err != nil || !slices.ContainsFunc(plan.Actions, func(act provider.Action) bool { return act.Kind == provider.ActionAddToTeam }) {
		t.Fatalf("team plan = %v, %v", plan.Actions, err)
	}
	for _, act := range plan.Actions {
		if act.Kind == provider.ActionAddToTeam {
			if out, err := a.Apply(ctx, env, inst, act); err != nil || !out.Changed {
				t.Fatalf("add to team = %+v, %v", out, err)
			}
		}
	}
	if state, err := liveTeamState(ctx, env, token, org, ref.Name, botLogin); err != nil || state != "active" {
		t.Fatalf("team membership = %q, %v", state, err)
	}
	d.Teams = nil
	plan, err = a.Plan(ctx, env, inst, d)
	if err != nil || !slices.ContainsFunc(plan.Actions, func(act provider.Action) bool { return act.Kind == provider.ActionRemoveFromTeam }) {
		t.Fatalf("team removal plan = %v, %v", plan.Actions, err)
	}
	for _, act := range plan.Actions {
		if act.Kind == provider.ActionRemoveFromTeam {
			if out, err := a.Apply(ctx, env, inst, act); err != nil || !out.Changed {
				t.Fatalf("remove from team = %+v, %v", out, err)
			}
		}
	}
	if status, err := liveStatus(ctx, env, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(ref.Name)+"/memberships/"+url.PathEscape(botLogin), token); status != http.StatusNotFound {
		t.Fatalf("team membership after removal = %d, %v", status, err)
	}

	// Organization removal, invitation and its cancellation, then restore.
	d.Access, d.Managed = false, true
	plan, err = a.Plan(ctx, env, inst, d)
	if err != nil || !slices.ContainsFunc(plan.Actions, func(act provider.Action) bool { return act.Kind == provider.ActionRemoveFromOrg }) {
		t.Fatalf("organization removal plan = %v, %v", plan.Actions, err)
	}
	for _, act := range plan.Actions {
		if act.Kind == provider.ActionRemoveFromOrg {
			if out, err := a.Apply(ctx, env, inst, act); err != nil || !out.Changed {
				t.Fatalf("remove from organization = %+v, %v", out, err)
			}
		}
	}
	if status, err := liveStatus(ctx, env, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/memberships/"+url.PathEscape(botLogin), token); status != http.StatusNotFound {
		t.Fatalf("membership after removal = %d, %v", status, err)
	}
	invite := accountAction(provider.ActionInvite, botID, false)
	out, err := a.Apply(ctx, env, inst, invite)
	if err != nil || out.Proof == nil || out.Proof.Kind != provider.ProofResponse {
		t.Fatalf("invite = %+v, %v", out, err)
	}
	if _, state, err := liveMembership(ctx, env, token, org, botLogin); err != nil || state != "pending" {
		t.Fatalf("invited membership = %q, %v", state, err)
	}
	// GitHub attributes the installation token's invitation to the App's bot;
	// the ensure proves the earlier call again with that marker (the recovery
	// path for an invitation whose answer was lost).
	out, err = a.Apply(ctx, env, inst, invite)
	if err != nil || out.Changed || out.Proof == nil || out.Proof.Kind != provider.ProofMarker {
		t.Fatalf("invite ensure (bot inviter) = %+v, %v", out, err)
	}
	out, err = a.Apply(ctx, env, inst, accountAction(provider.ActionCancelInvitation, botID, true))
	if err != nil || !out.Changed {
		t.Fatalf("cancel invitation = %+v, %v", out, err)
	}
	if status, err := liveStatus(ctx, env, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/memberships/"+url.PathEscape(botLogin), token); status != http.StatusNotFound {
		t.Fatalf("membership after cancellation = %d, %v", status, err)
	}
	// Orglane's invitation by ID, accepted by the bot itself.
	if _, err := a.Apply(ctx, env, inst, invite); err != nil {
		t.Fatal("re-invite:", err)
	}
	liveAcceptInvitation(ctx, t, env, botToken, org)
	if role, state, err := liveMembership(ctx, env, token, org, botLogin); err != nil || role != "member" || state != "active" {
		t.Fatalf("restored membership = %q/%q, %v", role, state, err)
	}
	t.Log("Invited, team-synced, removed and restored the dedicated bot against real GitHub; the interactive OAuth authorization stays manual acceptance.")
}

func liveSuffix() string {
	clean := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				b.WriteRune(r)
			}
		}
		return strings.Trim(b.String(), "-")
	}
	run, attempt := clean(os.Getenv("GITHUB_RUN_ID")), clean(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if run == "" {
		run = strconv.FormatInt(time.Now().Unix(), 10)
	}
	if attempt == "" {
		attempt = "local"
	}
	return run + "-" + attempt
}

func liveTeamName(purpose string) string {
	return "orglane-test-" + liveSuffix() + "-" + purpose
}

func liveResourceDescription() string {
	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		repo = "local"
	}
	return "orglane-test:" + repo + ":" + liveSuffix()
}

func liveMembership(ctx context.Context, env *liveEnv, token, org, login string) (role, state string, err error) {
	var m struct {
		Role  string `json:"role"`
		State string `json:"state"`
		User  struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := request(ctx, env, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/memberships/"+url.PathEscape(login), token, nil, &m); err != nil {
		return "", "", err
	}
	return m.Role, m.State, nil
}

func liveTeamState(ctx context.Context, env *liveEnv, token, org, team, login string) (string, error) {
	var m struct {
		State string `json:"state"`
	}
	err := request(ctx, env, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(team)+"/memberships/"+url.PathEscape(login), token, nil, &m)
	return m.State, err
}

// liveStatus runs a request and returns its HTTP status; a documented error
// body is expected for 404s.
func liveStatus(ctx context.Context, env *liveEnv, method, path, token string) (int, error) {
	err := request(ctx, env, method, path, token, nil, nil)
	if err == nil {
		return http.StatusOK, nil
	}
	var api *APIError
	if errors.As(err, &api) {
		return api.Status, nil
	}
	return 0, err
}

func liveAcceptInvitation(ctx context.Context, t *testing.T, env *liveEnv, token, org string) {
	t.Helper()
	var res struct {
		State string `json:"state"`
	}
	if err := request(ctx, env, http.MethodPatch, "/user/memberships/orgs/"+url.PathEscape(org), token, map[string]any{"state": "active"}, &res); err != nil || res.State != "active" {
		t.Fatalf("accept invitation = %+v, %v", res, err)
	}
}

// liveEnsureMember restores the bot's organization membership if a failed
// test left it removed.
func liveEnsureMember(ctx context.Context, t *testing.T, a *Adapter, env *liveEnv, inst provider.Instance, token, botToken, org, botID, botLogin string) {
	t.Helper()
	if _, state, err := liveMembership(ctx, env, token, org, botLogin); err == nil && state == "active" {
		return
	}
	if _, err := a.Apply(ctx, env, inst, accountAction(provider.ActionInvite, botID, false)); err != nil {
		var api *APIError
		if !errors.As(err, &api) || (api.Status != http.StatusUnprocessableEntity && api.Status != http.StatusNotFound) {
			t.Errorf("cleanup: invite the bot back: %v", err)
			return
		}
	}
	liveAcceptInvitation(ctx, t, env, botToken, org)
}

// liveDeleteTeam deletes a temporary team; a 404 is fine (already gone).
func liveDeleteTeam(ctx context.Context, t *testing.T, env *liveEnv, token, org, team string) {
	t.Helper()
	status, err := liveStatus(ctx, env, http.MethodDelete, "/orgs/"+url.PathEscape(org)+"/teams/"+url.PathEscape(team), token)
	if err != nil || (status != http.StatusNoContent && status != http.StatusOK && status != http.StatusNotFound) {
		t.Errorf("cleanup: delete temporary team %s = %d, %v", team, status, err)
	}
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
