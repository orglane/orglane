//go:build integration

package github

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/github/githubfake"
	"github.com/orglane/orglane/providers/provider"
)

// The live provisioning cleanup must restore the bot before the App key is
// cleared. t.Cleanup runs last-in-first-out, so registering the key clear
// first keeps it available to the restoration after a failed test. This
// runs the actual cleanup helper against the fake GitHub, without secrets.
func TestGitHubLiveCleanupRestoresBotBeforeClearingKey(t *testing.T) {
	gh := githubfake.New(func(err error) { t.Error(err) })
	t.Cleanup(gh.Close)
	const botID int64 = 7001
	const botLogin = "orglane-live-bot"
	gh.AddUser(githubfake.User{ID: botID, Login: botLogin})
	gh.AddMember(botID, githubfake.RoleMember)
	app, org := gh.App(), gh.Org()
	inst := provider.Instance{ID: uuid.New(), Provider: "github", Settings: map[string]any{
		"organization":    org.Login,
		"app_id":          strconv.FormatInt(app.ID, 10),
		"app_client_id":   app.ClientID,
		"installation_id": strconv.FormatInt(gh.InstallationID(), 10),
	}}
	a := New()
	// The fake has no route for the bot accepting its invitation (the
	// adapter's live test uses GitHub's user-token endpoint): accept it and
	// answer as GitHub would.
	base := gh.Transport()
	env := &liveEnv{key: gh.AppPrivateKeyPEM(), client: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/user/memberships/orgs/") {
			gh.Accept(botID)
			return response(http.StatusOK, map[string]any{"state": "active"}), nil
		}
		return base.RoundTrip(r)
	})}}
	var team provider.TeamRef
	t.Run("the run fails after removing the bot", func(t *testing.T) {
		liveProvisioningCleanups(t, a, env, inst, "ghu_test", org.Login, strconv.FormatInt(botID, 10), botLogin, &team, nil)
		// The test body removed the bot and then failed before its own
		// restoration, the state the cleanup has to repair.
		if _, err := a.Apply(t.Context(), env, inst, accountAction(provider.ActionRemoveFromOrg, strconv.FormatInt(botID, 10), true)); err != nil {
			t.Fatal(err)
		}
		if _, ok := gh.Members()[botID]; ok {
			t.Fatal("the bot is still a member")
		}
	})
	// The subtest's cleanups ran: restoration first, then the key clear.
	if _, ok := gh.Members()[botID]; !ok {
		t.Fatal("the cleanup did not restore the bot: the App key was cleared before restoration")
	}
	if len(bytes.Trim(env.key, "\x00")) != 0 {
		t.Fatal("the App key was not cleared after restoration")
	}
}
