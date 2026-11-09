//go:build integration

package apps_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/tests/pgtest"
)

// The M2 acceptance scenario (issue #67): a fresh company is set up, a
// person is invited, Forgejo is installed through the install API and the
// real agent, the person gets an account and a team through provisioning,
// a mention in a pull request becomes a normalized event for them, and the
// deep link opens the pull request through Forgejo's silent SSO. The last
// part demonstrates the agent's upgrade rollback with the real Forgejo
// package: a candidate that fails its health check restores the snapshot
// and the running version.
//
// The GitHub part of the scenario is TestM2AcceptanceGitHubLive: it runs
// only with the github-live environment and skips explicitly without it,
// because GitHub.com cannot run in a container (docs/specs/providers.md
// "GitHub live tests").

const m2Password = "correct horse battery staple 42" //nolint:gosec // a test password

// testLogWriter sends the core's log to the test log, shown on failure.
type testLogWriter struct{ t *testing.T }

// Write implements io.Writer for slog.
func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Logf("%s", bytes.TrimSpace(p))
	return len(p), nil
}

// forgejoCandidateVersion is the test-only upgrade candidate. The real
// package has one version; the acceptance scenario adds this one to the
// agent's registry at test time so the snapshot/rollback flow runs against
// the real Forgejo image, template and data.
const forgejoCandidateVersion = "15.0.12-1"

// m2Scenario is a real core and the real agent in front of a bundle.
type m2Scenario struct {
	t               *testing.T
	b               *managedtest.Bundle
	ctx             context.Context
	pool            *pgxpool.Pool
	stack           *web.Stack
	inst            apps.Instance
	agent           *agentclient.Client
	admin           managedtest.Auth
	workspace       string
	forgejoURL      string
	owner           browser
	person          browser
	personID        uuid.UUID
	teamID          uuid.UUID
	invitationToken string
}

func TestM2AcceptanceScenario(t *testing.T) {
	if managedtest.RunAsRoot(t) {
		return
	}
	s := startM2Scenario(t)

	// 1. Create the company: first-run setup creates the owner and signs
	// them in, as the installer's one-time setup token does.
	s.completeSetup()

	// 2. Invite a person into a team. The invitation link is the only
	// delivery without system email, as in a fresh install.
	s.invite("Platform", "platform", "Maya Okafor", "maya.okafor")

	// 3. Install Forgejo through the install API. The real agent pulls and
	// starts it, Setup creates the organization and activates the app.
	s.installForgejo()

	// 4. Map the team and accept the invitation: the person gets a Forgejo
	// account bound to Orglane's sign-in source and the mapped team.
	team := s.mapTeam("platform")
	f := s.forgejo()
	if f.account(s.personID.String()) != nil {
		s.t.Fatal("the invited person has a Forgejo account before accepting")
	}
	s.acceptInvitation()
	person := f.person("maya.okafor")
	managedtest.Eventually(t, 2*time.Minute, "maya's Forgejo access is provisioned", func() bool {
		result, err := store.New(s.pool).GetProvisioningResult(s.ctx, store.GetProvisioningResultParams{
			InstanceID: s.inst.ID, PersonID: person.ID,
		})
		return err == nil && result.Status == apps.StatusDone
	})
	account := f.account(person.ID.String())
	if account == nil {
		t.Fatal("the invited person has no Forgejo account after accepting")
	}
	if account.Login != person.Username || account.FullName != person.DisplayName || account.Email != person.WorkEmail {
		t.Fatalf("Forgejo account = %+v, want the person's profile", account)
	}
	if teamID := f.teamID("platform"); teamID == 0 || !f.inTeam(teamID, person.Username) {
		t.Fatal("the invited person is not in the mapped Forgejo team")
	}
	if team.ID == uuid.Nil {
		t.Fatal("the Orglane team disappeared")
	}

	// 5. A mention in a pull request is a normalized event for that person,
	// through a real Forgejo signature and the public webhook endpoint.
	start := forgejoLastTask(t, s.b)
	s.pullRequestWithMention(person)
	s.deliverWebhook(start)
	managedtest.Eventually(t, time.Minute, "the mention is a normalized event", func() bool {
		return s.hasMentionEvent(person.Username)
	})

	// 6. The pull request opens through the deep link with the person's
	// existing Orglane session: Forgejo signs them in silently.
	s.openPullRequestWithDeepLink(person)

	// 7. A Forgejo upgrade whose candidate fails its health check rolls
	// back to the snapshot; the running service and its data stay intact.
	s.forgejoUpgradeRollsBack()

	// The scenario never used the demo dataset or a fake provider: the
	// company, invitation, accounts, repositories and events are all real.
	if n := s.count(`SELECT count(*) FROM events WHERE instance_id = $1`, s.inst.ID); n == 0 {
		t.Fatal("the scenario produced no normalized events")
	}
}

// startM2Scenario starts the bundle, the real agent and a fresh core with
// managed services enabled. Traefik polls the core's routes, and the core's
// job workers drive the install and provisioning jobs.
func startM2Scenario(t *testing.T) *m2Scenario {
	t.Helper()
	public, internal := managedtest.FreePort(t), managedtest.FreePort(t)
	b := managedtest.StartBundleWithCore(t, internal)
	registry, err := packages.NewRegistry(forgejopkg.FS, forgejoCandidateFS(t))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(testLogWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	agentClient, socket := startAgentAtLogger(t, b, registry, logger)
	cfg := startCoreWithLogger(t, b, pgtest.NewDatabase(t), public, internal, false, map[string]string{
		"ORGLANE_AGENT_SOCKET": socket,
		"ORGLANE_AGENT_SECRET": agentSecret,
	}, logger)
	s := &m2Scenario{
		t: t, b: b, ctx: context.Background(), agent: agentClient,
		workspace: "http://" + managedtest.WorkspaceHost, forgejoURL: "http://" + managedtest.ForgejoHost,
	}
	s.owner = newBrowser(t, b)
	managedtest.Eventually(t, time.Minute, "the core answers through Traefik", func() bool {
		resp, _ := s.owner.do(t, http.MethodGet, s.workspace+"/healthz", nil)
		return resp.StatusCode == http.StatusOK
	})
	if s.pool, err = database.Connect(s.ctx, cfg.Database, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.pool.Close)
	if s.stack, err = web.NewStack(s.pool, cfg, slog.New(slog.DiscardHandler), web.StackOptions{AppsHTTPClient: b.Client()}); err != nil {
		t.Fatal(err)
	}
	return s
}

// completeSetup creates the first owner through the public setup endpoint.
func (s *m2Scenario) completeSetup() {
	s.t.Helper()
	token, err := s.stack.Identity.ResetSetupToken(s.ctx, audit.System)
	if err != nil {
		s.t.Fatal(err)
	}
	resp, body := s.owner.do(s.t, http.MethodPost, s.workspace+"/api/v1/auth/setup", map[string]string{
		"setupToken": token, "workEmail": "owner@acme.test", "username": "alex",
		"displayName": "Alex Morgan", "password": m2Password,
	})
	if resp.StatusCode != http.StatusCreated {
		s.t.Fatalf("setup = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Person struct{ ID uuid.UUID } `json:"person"`
		Csrf   string                 `json:"csrfToken"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Person.ID == uuid.Nil || out.Csrf == "" {
		s.t.Fatalf("setup result = %s (%v)", body, err)
	}
	s.owner = s.owner.withCSRF(out.Csrf)
	if n := s.count(`SELECT count(*) FROM people WHERE status = 'active'`); n != 1 {
		s.t.Fatalf("%d active people after setup, want the owner", n)
	}
}

// invite creates a team through the API and invites a person into it.
func (s *m2Scenario) invite(name, slug, displayName, username string) {
	s.t.Helper()
	resp, body := s.owner.do(s.t, http.MethodPost, s.workspace+"/api/v1/teams", map[string]string{"name": name, "slug": slug})
	if resp.StatusCode != http.StatusCreated {
		s.t.Fatalf("create team = %d %s", resp.StatusCode, body)
	}
	var team struct{ ID uuid.UUID }
	if err := json.Unmarshal(body, &team); err != nil || team.ID == uuid.Nil {
		s.t.Fatalf("team = %s (%v)", body, err)
	}
	s.teamID = team.ID
	resp, body = s.owner.do(s.t, http.MethodPost, s.workspace+"/api/v1/invitations", map[string]any{
		"displayName": displayName, "contactEmail": "maya@example.test", "username": username,
		"role": "member", "teamIds": []string{team.ID.String()},
	})
	if resp.StatusCode != http.StatusCreated {
		s.t.Fatalf("invite = %d %s", resp.StatusCode, body)
	}
	var invited struct {
		Invitation struct {
			PersonID uuid.UUID `json:"personId"`
		} `json:"invitation"`
		Link string `json:"link"`
	}
	if err := json.Unmarshal(body, &invited); err != nil || invited.Invitation.PersonID == uuid.Nil || !strings.Contains(invited.Link, "#") {
		s.t.Fatalf("invitation = %s (%v)", body, err)
	}
	s.personID = invited.Invitation.PersonID
	s.invitationToken = invited.Link[strings.Index(invited.Link, "#")+1:]
	if n := s.count(`SELECT count(*) FROM people WHERE status = 'invited'`); n != 1 {
		s.t.Fatalf("%d invited people, want one", n)
	}
}

// installForgejo installs the managed provider through the install API, the
// same path Settings → Apps will use, and waits until Setup activated it.
func (s *m2Scenario) installForgejo() {
	s.t.Helper()
	// The core reaches a managed service by its alias on the edge network;
	// in this test process that alias goes through Traefik too.
	s.b.Routes(s.t, managedtest.ForgejoRoutes("git")[1])
	resp, body := s.owner.do(s.t, http.MethodPost, s.workspace+"/api/v1/apps/instances", map[string]any{
		"provider": "forgejo", "key": "git",
		"settings": map[string]any{"organization": "acme", "ssh_port": managedtest.FreePort(s.t)},
	})
	if resp.StatusCode != http.StatusAccepted {
		s.t.Fatalf("install Forgejo = %d %s", resp.StatusCode, body)
	}
	var created struct{ ID uuid.UUID }
	if err := json.Unmarshal(body, &created); err != nil || created.ID == uuid.Nil {
		s.t.Fatalf("install result = %s (%v)", body, err)
	}
	key := "git"
	project := packages.InstanceProject(s.b.Project, key)
	s.t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(s.t.Context()), "docker", "compose", "-p", project, "down", "--remove-orphans").Run() //nolint:gosec // test cleanup
	})
	deadline := time.Now().Add(5 * time.Minute)
	var last []byte
	for {
		resp, body := s.owner.do(s.t, http.MethodGet, s.workspace+"/api/v1/apps/instances/"+created.ID.String(), nil)
		last = body
		if resp.StatusCode == http.StatusOK {
			var detail struct {
				Instance struct {
					State      string `json:"state"`
					HealthCode string `json:"healthCode"`
				} `json:"instance"`
			}
			if json.Unmarshal(body, &detail) == nil {
				switch detail.Instance.State {
				case apps.StateActive:
					goto installed
				case apps.StateFailed:
					s.diagnoseInstall(created.ID)
					s.t.Fatalf("Forgejo installation failed: %s", body)
				}
			}
		}
		if time.Now().After(deadline) {
			s.diagnoseInstall(created.ID)
			s.t.Fatalf("timed out waiting for Forgejo: last %s", last)
		}
		time.Sleep(500 * time.Millisecond)
	}
installed:
	inst, err := s.stack.Apps.Instance(s.ctx, created.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	if inst.State != apps.StateActive || inst.Key != key {
		s.t.Fatalf("installed instance = %+v", inst)
	}
	s.inst = inst
	env := s.stack.Apps.EnvForTest(inst)
	lifecycle, _ := inst.Adapter.Lifecycle()
	inputs, err := lifecycle.Inputs(s.ctx, env, inst.View())
	if err != nil {
		s.t.Fatal(err)
	}
	secrets := map[string]string{}
	for _, name := range inputs.Secrets {
		value, err := env.Credential(s.ctx, name)
		if err != nil {
			s.t.Fatal(err)
		}
		secrets[name] = string(value)
	}
	s.admin = managedtest.Auth{User: forgejo.AdminUsername, Password: secrets[forgejo.CredentialAdminPassword]}
}

// diagnoseInstall logs the instance's operations, River jobs and agent
// state when an install does not finish, so a CI failure is actionable.
func (s *m2Scenario) diagnoseInstall(id uuid.UUID) {
	s.t.Helper()
	rows, err := s.pool.Query(s.ctx, `SELECT generation, kind, state, coalesce(error_code, ''), coalesce(phase, '')
		FROM app_operations WHERE instance_id = $1 ORDER BY generation`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var generation int64
			var kind, state, code, phase string
			if err := rows.Scan(&generation, &kind, &state, &code, &phase); err == nil {
				s.t.Logf("operation: generation=%d kind=%s state=%s code=%s phase=%s", generation, kind, state, code, phase)
			}
		}
	}
	jobs, err := s.pool.Query(s.ctx, `SELECT kind, state, coalesce(error::text, ''), attempt FROM river_job
		WHERE kind LIKE 'orglane.apps.%' ORDER BY id DESC LIMIT 10`)
	if err == nil {
		defer jobs.Close()
		for jobs.Next() {
			var kind, state, jobErr string
			var attempt int
			if err := jobs.Scan(&kind, &state, &jobErr, &attempt); err == nil {
				s.t.Logf("job: %s %s attempt=%d error=%s", kind, state, attempt, jobErr)
			}
		}
	}
	if statuses, err := s.agent.Instances(s.ctx); err == nil {
		for _, status := range statuses {
			s.t.Logf("agent instance: key=%s state=%s phase=%s generation=%d", status.Key, status.State, status.Phase, status.ObservedGeneration)
		}
	}
	var deliveries int
	if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FROM webhook_deliveries`).Scan(&deliveries); err == nil {
		s.t.Logf("webhook deliveries: %d", deliveries)
	}
}

// mapTeam changes the access policy to mapped teams and maps the team, as an
// admin does in Settings → Apps.
func (s *m2Scenario) mapTeam(slug string) store.Team {
	s.t.Helper()
	resp, body := s.owner.do(s.t, http.MethodPatch, s.workspace+"/api/v1/apps/instances/"+s.inst.ID.String(), map[string]any{
		"accessPolicy": "mapped_teams",
	})
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("access policy = %d %s", resp.StatusCode, body)
	}
	resp, body = s.owner.do(s.t, http.MethodPut, s.workspace+"/api/v1/apps/instances/"+s.inst.ID.String()+"/team-mappings/"+s.teamID.String(), map[string]any{"mode": "create"})
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("team mapping = %d %s", resp.StatusCode, body)
	}
	resp, body = s.owner.do(s.t, http.MethodGet, s.workspace+"/api/v1/apps/instances/"+s.inst.ID.String()+"/provisioning", nil)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("provisioning status = %d %s", resp.StatusCode, body)
	}
	team, err := store.New(s.pool).GetTeamBySlug(s.ctx, slug)
	if err != nil {
		s.t.Fatal(err)
	}
	return team
}

// acceptInvitation accepts the invitation as the person, which signs them
// in and makes them active.
func (s *m2Scenario) acceptInvitation() {
	s.t.Helper()
	s.person = newBrowser(s.t, s.b)
	resp, body := s.person.do(s.t, http.MethodPost, s.workspace+"/api/v1/auth/invitation/accept", map[string]string{
		"token": s.invitationToken, "password": m2Password,
	})
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("accept invitation = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Person struct{ ID uuid.UUID } `json:"person"`
		Csrf   string                 `json:"csrfToken"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Person.ID != s.personID || out.Csrf == "" {
		s.t.Fatalf("accept result = %s (%v)", body, err)
	}
	s.person = s.person.withCSRF(out.Csrf)
}

// forgejo returns the helpers of the installed instance.
func (s *m2Scenario) forgejo() *forgejoEnv {
	return &forgejoEnv{
		t: s.t, b: s.b, ctx: s.ctx, pool: s.pool, stack: s.stack, inst: s.inst,
		admin: s.admin, workspace: s.workspace, forgejoURL: s.forgejoURL,
	}
}

// pullRequestWithMention creates a repository and a pull request that
// mentions the person.
func (s *m2Scenario) pullRequestWithMention(person store.Person) {
	s.t.Helper()
	s.forgejoCall(s.admin, http.MethodPost, "/api/v1/orgs/acme/repos", map[string]any{
		"name": "app", "auto_init": true, "default_branch": "main",
	})
	s.forgejoCall(s.admin, http.MethodPost, "/api/v1/repos/acme/app/contents/feature.txt", map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte("hello from the acceptance scenario\n")),
		"branch":  "main", "new_branch": "feature", "message": "add feature",
	})
	s.forgejoCall(s.admin, http.MethodPost, "/api/v1/repos/acme/app/pulls", map[string]any{
		"head": "feature", "base": "main", "title": "Add the acceptance feature",
		"body": "Please review this, @" + person.Username + ".",
	})
}

// forgejoCall sends an authenticated request to Forgejo and fails on a
// non-2xx answer.
func (s *m2Scenario) forgejoCall(auth managedtest.Auth, method, path string, body any) []byte {
	s.t.Helper()
	code, out := s.forgejoStatus(auth, method, path, body)
	if code < 200 || code > 299 {
		s.t.Fatalf("%s %s: %d %s", method, path, code, out)
	}
	return out
}

// forgejoStatus sends an authenticated request to Forgejo and returns its
// status and body, for assertions that expect a non-2xx answer.
func (s *m2Scenario) forgejoStatus(auth managedtest.Auth, method, path string, body any) (int, []byte) {
	s.t.Helper()
	return managedtest.Request(s.t, s.b.Client(), method, managedtest.ForgejoHost, path, auth, body)
}

// deliverWebhook posts the delivery Forgejo recorded to the public webhook
// endpoint, as Traefik would deliver it. On CI Forgejo can reach the
// workspace through Traefik itself and may have delivered the same signed
// body already, in which case this is a duplicate (200) and the event is
// already queued; the recorded request still makes the scenario independent
// of container DNS, and a fresh delivery answers 202.
func (s *m2Scenario) deliverWebhook(start int) {
	s.t.Helper()
	var deliveries []recorded
	managedtest.Eventually(s.t, time.Minute, "Forgejo records the pull request delivery", func() bool {
		deliveries = forgejoRecorded(s.t, s.b, start)
		for _, d := range deliveries {
			if d.event == "pull_request" {
				return true
			}
		}
		return false
	})
	var mention *recorded
	for i := range deliveries {
		if deliveries[i].event == "pull_request" {
			mention = &deliveries[i]
		}
	}
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.workspace+"/api/v1/webhooks/"+s.inst.ID.String(), bytes.NewReader(mention.body))
	if err != nil {
		s.t.Fatal(err)
	}
	for name, values := range mention.headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := s.b.Client().Do(req)
	if err != nil {
		s.t.Fatalf("deliver webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		s.t.Fatalf("webhook = %d %s", resp.StatusCode, body)
	}
}

// hasMentionEvent reports whether the person's username is a mention target
// of a normalized event.
func (s *m2Scenario) hasMentionEvent(login string) bool {
	s.t.Helper()
	rows, err := s.pool.Query(s.ctx, `SELECT type, targets FROM events WHERE instance_id = $1`, s.inst.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var raw []byte
		if err := rows.Scan(&typ, &raw); err != nil {
			s.t.Fatal(err)
		}
		if typ != forgejo.EventMention {
			continue
		}
		var targets []provider.Target
		if err := json.Unmarshal(raw, &targets); err != nil {
			s.t.Fatal(err)
		}
		for _, target := range targets {
			if target.Login == login && target.Reason == "mentioned" {
				return true
			}
		}
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return false
}

// openPullRequestWithDeepLink follows the instance's deep link in the
// person's browser, which already has an Orglane session, and lands on the
// pull request signed in to Forgejo.
func (s *m2Scenario) openPullRequestWithDeepLink(person store.Person) {
	s.t.Helper()
	resp, body := s.person.do(s.t, http.MethodGet, s.workspace+"/api/v1/apps/instances/"+s.inst.ID.String()+"/open?to="+url.QueryEscape("/acme/app/pulls/1"), nil)
	var link struct{ URL string }
	if err := json.Unmarshal(body, &link); resp.StatusCode != http.StatusOK || err != nil {
		s.t.Fatalf("deep link = %d %s (%v)", resp.StatusCode, body, err)
	}
	if !strings.HasPrefix(link.URL, s.forgejoURL+"/user/oauth2/orglane?") {
		s.t.Fatalf("deep link = %s, want Forgejo's Orglane sign-in", link.URL)
	}
	if signedInToForgejo(s.t, s.person, s.forgejoURL) {
		s.t.Fatal("the person already had a Forgejo session before the deep link")
	}
	resp, html := s.person.do(s.t, http.MethodGet, link.URL, nil)
	if resp.StatusCode != http.StatusOK || resp.URL.String() != s.forgejoURL+"/acme/app/pulls/1" || !strings.Contains(string(html), person.Username) {
		s.t.Fatalf("the deep link ended at %s (%d), want the pull request signed in as %s", resp.URL, resp.StatusCode, person.Username)
	}
	if !signedInToForgejo(s.t, s.person, s.forgejoURL) {
		s.t.Fatal("not signed in to Forgejo after the deep link")
	}
}

// forgejoUpgradeRollsBack pushes a test-only candidate version of the real
// package whose health check fails, and checks that the agent rolls back to
// the snapshot: the operation is rolled_back, the running version and its
// repository data are intact, and the snapshot is recorded.
func (s *m2Scenario) forgejoUpgradeRollsBack() {
	s.t.Helper()
	env := s.stack.Apps.EnvForTest(s.inst)
	lifecycle, _ := s.inst.Adapter.Lifecycle()
	inputs, err := lifecycle.Inputs(s.ctx, env, s.inst.View())
	if err != nil {
		s.t.Fatal(err)
	}
	secrets := map[string]string{}
	for _, name := range inputs.Secrets {
		value, err := env.Credential(s.ctx, name)
		if err != nil {
			s.t.Fatal(err)
		}
		secrets[name] = string(value)
	}
	spec := managedtest.ForgejoSpec(s.inst.ID, 2, managedtest.FreePort(s.t), secrets, agentapi.StateRunning)
	spec.Package = agentapi.PackageRef{ID: "forgejo", Version: forgejoCandidateVersion}
	for name, value := range inputs.Settings {
		spec.Settings[name] = value
	}
	op, _, err := s.agent.Apply(s.ctx, s.inst.Key, spec)
	if err != nil {
		s.t.Fatalf("push the upgrade candidate: %v", err)
	}
	managedtest.Eventually(s.t, 5*time.Minute, "the upgrade candidate rolls back", func() bool {
		current, err := s.agent.Operation(s.ctx, op.ID)
		return err == nil && current.Terminal()
	})
	op, err = s.agent.Operation(s.ctx, op.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	// Docker Compose reports an unhealthy candidate either as the health
	// timeout or, when its ps output no longer names the health state, as
	// compose_failed; both are the same failed verification and must roll
	// back.
	if op.State != agentapi.OpRolledBack || op.SnapshotID == "" ||
		(op.ErrorCode != agentapi.CodeHealthTimeout && op.ErrorCode != agentapi.CodeComposeFailed) {
		s.t.Fatalf("upgrade operation = %+v, want rolled_back with a failed health check and the snapshot", op)
	}
	status, err := s.agent.Instance(s.ctx, s.inst.Key)
	if err != nil {
		s.t.Fatal(err)
	}
	if status.Package == nil || status.Package.Version != forgejopkg.Current || status.ObservedGeneration != 1 ||
		status.Phase != agentapi.PhaseRunning || len(status.BlockedGenerations) != 1 || status.BlockedGenerations[0] != 2 {
		s.t.Fatalf("agent status after the rollback = %+v, want %s generation 1, running and generation 2 blocked", status, forgejopkg.Current)
	}
	if code, _ := s.forgejoStatus(s.admin, http.MethodGet, "/api/v1/repos/acme/app", nil); code != http.StatusOK {
		s.t.Fatalf("the repository is gone after the rollback: %d", code)
	}
	if code, _ := s.forgejoStatus(s.admin, http.MethodGet, "/api/v1/repos/acme/app/pulls/1", nil); code != http.StatusOK {
		s.t.Fatalf("the pull request is gone after the rollback: %d", code)
	}
}

// count runs a count query.
func (s *m2Scenario) count(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(s.ctx, query, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// forgejoCandidateFS builds a test-only Forgejo package version in a
// temporary directory. It is the real manifest and template, with a newer
// version that accepts the embedded current version and a Forgejo that
// listens on another port, so its health check fails and the agent rolls
// back. Every rewrite is checked, so a change to the real package fails the
// candidate at the replacement site, not as a mysterious successful upgrade.
func forgejoCandidateFS(t *testing.T) fs.FS {
	t.Helper()
	from, err := packages.ParseVersion(forgejopkg.Current)
	if err != nil {
		t.Fatal(err)
	}
	to, err := packages.ParseVersion(forgejoCandidateVersion)
	if err != nil {
		t.Fatal(err)
	}
	if to.Compare(from) <= 0 {
		t.Fatalf("the candidate version %s must be newer than the embedded %s", to, from)
	}
	root := t.TempDir()
	err = fs.WalkDir(forgejopkg.FS, forgejopkg.Current, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, forgejopkg.Current), "/")
		data := []byte(nil)
		if !d.IsDir() {
			data, err = fs.ReadFile(forgejopkg.FS, path)
			if err != nil {
				return err
			}
			switch rel {
			case "manifest.yaml":
				data = forgejoCandidateManifest(t, data)
			case "compose.yaml.tmpl":
				data = forgejoCandidateCompose(t, data)
			}
		}
		dst := filepath.Join(root, forgejoCandidateVersion, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return os.DirFS(root)
}

// forgejoCandidateManifest rewrites the real manifest: the candidate
// version, and the current version as the oldest version it upgrades from.
func forgejoCandidateManifest(t *testing.T, raw []byte) []byte {
	t.Helper()
	manifest := mustReplace(t, string(raw), "version: "+forgejopkg.Current, "version: "+forgejoCandidateVersion)
	return []byte(mustReplace(t, manifest, "upgrade:\n", "upgrade:\n  min_from: "+forgejopkg.Current+"\n"))
}

// forgejoCandidateCompose makes the candidate's health check fail: Forgejo
// listens on 3001 while the package health check probes 3000, and the
// shortened start period and retries make the failure quick.
func forgejoCandidateCompose(t *testing.T, raw []byte) []byte {
	t.Helper()
	compose := mustReplace(t, string(raw), `FORGEJO__server__HTTP_PORT: "3000"`, `FORGEJO__server__HTTP_PORT: "3001"`)
	compose = mustReplace(t, compose, "start_period: 5m", "start_period: 5s")
	return []byte(mustReplace(t, compose, "retries: 3", "retries: 1"))
}

// mustReplace replaces old exactly once and fails the test when the real
// package no longer contains it.
func mustReplace(t *testing.T, s, old, replacement string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("candidate package: %q not found in the real package", old)
	}
	return strings.Replace(s, old, replacement, 1)
}
