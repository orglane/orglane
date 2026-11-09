//go:build integration

package github

import (
	"context"
	"errors"
	"fmt"
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
	inst := provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{"organization": org, "app_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_ID"), "app_client_id": os.Getenv("ORGLANE_TEST_GITHUB_APP_CLIENT_ID"), "installation_id": os.Getenv("ORGLANE_TEST_GITHUB_INSTALLATION_ID")}}
	a := New()
	var ref provider.TeamRef
	removal := &liveRemoval{}
	liveProvisioningCleanups(t, a, env, inst, botToken, org, botID, botLogin, &ref, removal)
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
	// removes and restores it. A previous failed run can have left it removed
	// because that run's cleanup still saw the queued membership as active and
	// the removal finished afterwards; restore it first, so the test repairs
	// exactly the state its own removal can leave.
	role, state, err := liveMembership(ctx, env, token, org, botLogin)
	restore := false
	switch {
	case err != nil:
		var api *APIError
		if !errors.As(err, &api) || api.Status != http.StatusNotFound {
			t.Fatalf("read the bot's membership: %v", err)
		}
		restore = true
	case state != "active":
		restore = true
	case role != "member":
		t.Fatalf("bot membership = %q/%q; the live-test bot must be a non-owner member", state, role)
	}
	if restore {
		t.Logf("restoring the dedicated bot's membership: %q/%q, %v", role, state, err)
		if _, err := a.Apply(ctx, env, inst, accountAction(provider.ActionInvite, botID, false)); err != nil {
			t.Fatalf("restore the dedicated bot: invite: %v", err)
		}
		if role, state, err = liveMembership(ctx, env, token, org, botLogin); err != nil || state != "active" || role != "member" {
			liveAcceptInvitation(ctx, t, env, botToken, org)
			role, err = liveAwaitMembershipActive(ctx, env, token, org, botLogin)
			if err != nil || role != "member" {
				t.Fatalf("restore the dedicated bot: role %q, %v", role, err)
			}
		}
	}
	// The member list is a separate read and can lag the membership; wait a
	// moment after a restoration instead of failing the setup on that lag.
	for deadline := time.Now().Add(15 * time.Second); ; {
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
		if member {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the dedicated bot @%s (ID %s) is not an organization member; the github-live setup requires one", botLogin, botID)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatal(ctx.Err())
		case <-timer.C:
		}
	}
	role, state, err = liveMembership(ctx, env, token, org, botLogin)
	if err != nil || state != "active" || role != "member" {
		t.Fatalf("bot membership = %q/%q, %v; the live-test bot must be a non-owner member", state, role, err)
	}

	teamSlug := liveTeamName("provisioning")
	refs, err := a.SyncTeams(ctx, env, inst, []provider.TeamMapping{{TeamID: uuid.New(), TeamSlug: teamSlug, TeamName: teamSlug, Mode: provider.MappingCreate}})
	if err != nil || len(refs) != 1 || refs[0].ExternalRef == "" || !strings.HasPrefix(refs[0].Name, "orglane-test-") {
		t.Fatalf("synced team = %+v, %v", refs, err)
	}
	ref = refs[0]
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
			liveApplyRemoval(ctx, t, a, env, inst, act, nil)
		}
	}
	if err := liveAwaitTeamMembershipGone(ctx, env, token, org, ref.Name, botLogin); err != nil {
		t.Fatal(err)
	}

	// Organization removal, invitation and its cancellation, then restore.
	d.Access, d.Managed = false, true
	plan, err = a.Plan(ctx, env, inst, d)
	if err != nil || !slices.ContainsFunc(plan.Actions, func(act provider.Action) bool { return act.Kind == provider.ActionRemoveFromOrg }) {
		t.Fatalf("organization removal plan = %v, %v", plan.Actions, err)
	}
	for _, act := range plan.Actions {
		if act.Kind == provider.ActionRemoveFromOrg {
			liveApplyRemoval(ctx, t, a, env, inst, act, removal)
		}
	}
	// GitHub may process the removal after its 204, so the membership is
	// awaited rather than read once; on failure the read's non-secret state
	// says what GitHub still serves.
	if err := liveAwaitMembershipGone(ctx, env, token, org, botLogin); err != nil {
		t.Fatal(err)
	}
	removal.pending = false
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
	liveApplyRemoval(ctx, t, a, env, inst, accountAction(provider.ActionCancelInvitation, botID, true), nil)
	if err := liveAwaitMembershipGone(ctx, env, token, org, botLogin); err != nil {
		t.Fatal(err)
	}
	// Orglane's invitation by ID, accepted by the bot itself. GitHub may
	// answer the acceptance synchronously (200) or process it asynchronously
	// (202), so the active membership is awaited too.
	if _, err := a.Apply(ctx, env, inst, invite); err != nil {
		t.Fatal("re-invite:", err)
	}
	liveAcceptInvitation(ctx, t, env, botToken, org)
	role, err = liveAwaitMembershipActive(ctx, env, token, org, botLogin)
	if err != nil || role != "member" {
		t.Fatalf("restored membership role = %q, %v", role, err)
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

// liveMutationTimeout bounds every wait for GitHub to finish a queued
// membership change. GitHub accepts the mutation before it propagates it, so
// a single read right after it is not proof.
const liveMutationTimeout = 60 * time.Second

// liveMembershipInfo is the non-secret membership state a diagnostic shows:
// enough to tell a queued removal from an indirect (enterprise team)
// membership without ever logging a token or a response body.
type liveMembershipInfo struct {
	State            string   `json:"state"`
	Role             string   `json:"role"`
	DirectMembership *bool    `json:"direct_membership"`
	EnterpriseTeams  []string `json:"enterprise_teams_providing_indirect_membership"`
}

func (m liveMembershipInfo) detail() string {
	direct := "absent"
	if m.DirectMembership != nil {
		direct = strconv.FormatBool(*m.DirectMembership)
	}
	return fmt.Sprintf("state=%q role=%q direct_membership=%s enterprise_teams=%v", m.State, m.Role, direct, m.EnterpriseTeams)
}

// liveMembershipRead reads a membership and keeps GitHub's status, so a test
// can tell the documented 404 (gone) from a still-served membership. Only a
// 200 carries a membership to show.
func liveMembershipRead(ctx context.Context, env *liveEnv, token, org, login string) (int, liveMembershipInfo, error) {
	var m liveMembershipInfo
	err := request(ctx, env, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/memberships/"+url.PathEscape(login), token, nil, &m)
	if err == nil {
		return http.StatusOK, m, nil
	}
	var api *APIError
	if errors.As(err, &api) {
		return api.Status, liveMembershipInfo{}, nil
	}
	return 0, liveMembershipInfo{}, err
}

type liveTeamMembershipInfo struct {
	State string `json:"state"`
	Role  string `json:"role"`
}

func (m liveTeamMembershipInfo) detail() string {
	return fmt.Sprintf("state=%q role=%q", m.State, m.Role)
}

func liveTeamMembershipRead(ctx context.Context, env *liveEnv, token, org, team, login string) (int, liveTeamMembershipInfo, error) {
	var m liveTeamMembershipInfo
	path := "/orgs/" + url.PathEscape(org) + "/teams/" + url.PathEscape(team) + "/memberships/" + url.PathEscape(login)
	err := request(ctx, env, http.MethodGet, path, token, nil, &m)
	if err == nil {
		return http.StatusOK, m, nil
	}
	var api *APIError
	if errors.As(err, &api) {
		return api.Status, liveTeamMembershipInfo{}, nil
	}
	return 0, liveTeamMembershipInfo{}, err
}

// liveAwaitGone waits until read reports 404. GitHub may serve a membership
// for a moment after the mutation that removed it; a failure names the last
// status and non-secret membership state rather than hiding the lag.
func liveAwaitGone(ctx context.Context, what string, read func(context.Context) (int, string, error)) error {
	deadline := time.Now().Add(liveMutationTimeout)
	delay := 500 * time.Millisecond
	for {
		status, detail, err := read(ctx)
		switch {
		case err != nil:
			detail = "read failed: " + err.Error()
		case status == http.StatusNotFound:
			return nil
		case status == http.StatusOK:
			detail = "status 200, " + detail
		case status == http.StatusTooManyRequests || status >= 500:
			detail = fmt.Sprintf("status %d", status)
		default:
			return fmt.Errorf("%s: GitHub answered %d", what, status)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s did not disappear within %s; last read: %s", what, liveMutationTimeout, detail)
		}
		timer := time.NewTimer(min(delay, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, 5*time.Second)
	}
}

func liveAwaitMembershipGone(ctx context.Context, env *liveEnv, token, org, login string) error {
	return liveAwaitGone(ctx, fmt.Sprintf("the organization membership of @%s", login), func(ctx context.Context) (int, string, error) {
		status, m, err := liveMembershipRead(ctx, env, token, org, login)
		return status, m.detail(), err
	})
}

func liveAwaitTeamMembershipGone(ctx context.Context, env *liveEnv, token, org, team, login string) error {
	return liveAwaitGone(ctx, fmt.Sprintf("@%s's membership of team %s", login, team), func(ctx context.Context) (int, string, error) {
		status, m, err := liveTeamMembershipRead(ctx, env, token, org, team, login)
		return status, m.detail(), err
	})
}

// liveAwaitMembershipActive waits for an accepted invitation to become an
// active membership: GitHub documents a 200 answer for a synchronous
// acceptance and a 202 when it is processed asynchronously.
func liveAwaitMembershipActive(ctx context.Context, env *liveEnv, token, org, login string) (string, error) {
	deadline := time.Now().Add(liveMutationTimeout)
	delay := 500 * time.Millisecond
	for {
		status, m, err := liveMembershipRead(ctx, env, token, org, login)
		switch {
		case err != nil:
		case status == http.StatusOK && m.State == "active":
			return m.Role, nil
		case status != http.StatusOK && status != http.StatusNotFound && status != http.StatusTooManyRequests && status < 500:
			return "", fmt.Errorf("the organization membership of @%s: GitHub answered %d", login, status)
		}
		detail := "read failed"
		if err != nil {
			detail += ": " + err.Error()
		} else {
			detail = fmt.Sprintf("status %d, %s", status, m.detail())
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("the organization membership of @%s did not become active within %s; last read: %s", login, liveMutationTimeout, detail)
		}
		timer := time.NewTimer(min(delay, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, 5*time.Second)
	}
}

// liveRemoval tracks an organization removal whose DELETE GitHub accepted
// but whose completion the test has not observed yet. A queued removal can
// still land after a failed test, so cleanup lets it finish before restoring
// the bot; restoring first would let the late removal remove the bot again.
type liveRemoval struct{ pending bool }

// liveApplyRemoval applies a removal action until GitHub confirms it. The
// adapter answers the retryable removal_pending while GitHub still serves the
// membership its DELETE accepted; the test retries like the core's guard.
func liveApplyRemoval(ctx context.Context, t *testing.T, a *Adapter, env *liveEnv, inst provider.Instance, act provider.Action, removal *liveRemoval) provider.ActionOutcome {
	t.Helper()
	if removal != nil {
		removal.pending = true
	}
	deadline := time.Now().Add(liveMutationTimeout)
	delay := time.Second
	for {
		out, err := a.Apply(ctx, env, inst, act)
		if err == nil {
			return out
		}
		code, retryable, retryAfter := provider.Classify(err)
		if !retryable {
			if removal != nil {
				removal.pending = false
			}
			t.Fatalf("%s failed: %v", act.Kind, err)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s is still retryable (%s) after %s: %v", act.Kind, code, liveMutationTimeout, err)
		}
		wait := delay
		if retryAfter > 0 {
			wait = min(retryAfter, time.Until(deadline))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("%s: %v", act.Kind, ctx.Err())
		case <-timer.C:
		}
		delay = min(2*delay, 5*time.Second)
	}
}

func liveAcceptInvitation(ctx context.Context, t *testing.T, env *liveEnv, token, org string) {
	t.Helper()
	// GitHub answers 200 for a synchronous acceptance and 202 while the
	// acceptance is processed asynchronously; both carry the membership, and
	// the caller awaits its active state.
	var res struct {
		State string `json:"state"`
	}
	if err := request(ctx, env, http.MethodPatch, "/user/memberships/orgs/"+url.PathEscape(org), token, map[string]any{"state": "active"}, &res); err != nil {
		t.Fatalf("accept invitation = %v", err)
	}
}

// liveProvisioningCleanups registers a live provisioning run's cleanups.
// t.Cleanup runs last-in-first-out, so clearing the App key is registered
// first: the bot restoration and the team deletion still mint installation
// tokens after a failed test. team is filled in once the temporary team
// exists; removal records an accepted organization removal still in flight.
func liveProvisioningCleanups(t *testing.T, a *Adapter, env *liveEnv, inst provider.Instance, botToken, org, botID, botLogin string, team *provider.TeamRef, removal *liveRemoval) {
	t.Helper()
	t.Cleanup(func() { clear(env.key) })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		token, err := a.InstallationToken(ctx, env, inst)
		if err != nil {
			t.Errorf("cleanup: installation token: %v", err)
			return
		}
		if removal != nil && removal.pending {
			// The test failed while GitHub was still processing its removal;
			// wait for it to settle, or it could remove the restored bot.
			if err := liveAwaitMembershipGone(ctx, env, token, org, botLogin); err != nil {
				t.Errorf("cleanup: wait for the accepted organization removal: %v", err)
			}
			removal.pending = false
		}
		if team.Name != "" {
			liveDeleteTeam(ctx, t, env, token, org, team.Name)
		}
		liveEnsureMember(ctx, t, a, env, inst, token, botToken, org, botID, botLogin)
	})
}

// liveEnsureMember restores the bot's organization membership if a failed
// test left it removed; the bot is an ordinary member again when it returns.
func liveEnsureMember(ctx context.Context, t *testing.T, a *Adapter, env *liveEnv, inst provider.Instance, token, botToken, org, botID, botLogin string) {
	t.Helper()
	if status, m, err := liveMembershipRead(ctx, env, token, org, botLogin); err == nil && status == http.StatusOK && m.State == "active" {
		return
	}
	if _, err := a.Apply(ctx, env, inst, accountAction(provider.ActionInvite, botID, false)); err != nil {
		var api *APIError
		if !errors.As(err, &api) || (api.Status != http.StatusUnprocessableEntity && api.Status != http.StatusNotFound) {
			t.Errorf("cleanup: invite the bot back: %v", err)
			return
		}
	}
	// The invite ensure may have found the bot already active (someone
	// accepted meanwhile); do not send an acceptance without an invitation.
	if status, m, err := liveMembershipRead(ctx, env, token, org, botLogin); err == nil && status == http.StatusOK && m.State == "active" {
		return
	}
	liveAcceptInvitation(ctx, t, env, botToken, org)
	role, err := liveAwaitMembershipActive(ctx, env, token, org, botLogin)
	if err != nil {
		t.Errorf("cleanup: restore the bot's membership: %v", err)
	} else if role != "member" {
		t.Errorf("cleanup: restored bot role = %q, want an ordinary member", role)
	}
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
