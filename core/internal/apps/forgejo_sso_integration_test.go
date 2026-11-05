//go:build integration

package apps_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/core/internal/managedtest"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/server"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/tests/pgtest"
)

// browser is an HTTP client with cookies that reaches every host through
// Traefik, as a browser that resolves *.localhost to loopback.
type browser struct {
	*http.Client
}

func newBrowser(t *testing.T, b *managedtest.Bundle) browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := b.Client()
	c.Jar = jar
	c.CheckRedirect = nil // follow redirects, at most 10
	return browser{c}
}

// page is what a request ended with, after redirects.
type page struct {
	StatusCode int
	URL        *url.URL
}

func (br browser) do(t *testing.T, method, rawURL string, body any) (page, []byte) {
	t.Helper()
	var in io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		in = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, in)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := br.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return page{StatusCode: resp.StatusCode, URL: resp.Request.URL}, out
}

// startCore runs `orglane server` in-process on the database in local mode
// (http://orglane.localhost, demo mode for one-click sign-in), behind the
// bundle's Traefik, which polls the core's routes. Its job workers run the
// provisioning jobs; adapters reach managed services through Traefik (in
// the bundle the core is on the edge network). It returns the config.
func startCore(t *testing.T, b *managedtest.Bundle, dbURL string, public, internal int) *config.Config {
	t.Helper()
	host := managedtest.CoreListenHost()
	vars := map[string]string{
		"ORGLANE_DATABASE_URL":           dbURL,
		"ORGLANE_DOMAIN_BASE":            "orglane.localhost",
		"ORGLANE_DOMAIN_WORKSPACE_HOST":  managedtest.WorkspaceHost,
		"ORGLANE_DOMAIN_SCHEME":          "http",
		"ORGLANE_ENVIRONMENT":            "development",
		"ORGLANE_INGRESS_MODE":           "local",
		"ORGLANE_SERVER_LISTEN":          net.JoinHostPort(host, strconv.Itoa(public)),
		"ORGLANE_SERVER_INTERNAL_LISTEN": net.JoinHostPort(host, strconv.Itoa(internal)),
		"ORGLANE_INGRESS_UPSTREAM_URL":   managedtest.CoreURL(public),
		"ORGLANE_IDENTITY_MASTER_SECRET": masterSecret,
		"ORGLANE_DEMO_ENABLED":           "true",
	}
	cfg, err := config.Load("", func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.RunWithStack(ctx, cfg, "dev", slog.New(slog.DiscardHandler), web.StackOptions{AppsHTTPClient: b.Client()})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server: %v", err)
		}
	})
	return cfg
}

// forgejoEnv is Forgejo installed by the real agent next to a real core,
// both behind the real Traefik.
type forgejoEnv struct {
	t          *testing.T
	b          *managedtest.Bundle
	ctx        context.Context
	pool       *pgxpool.Pool
	stack      *web.Stack // the test's view of the core: changes go through it, the server's jobs act on them
	inst       apps.Instance
	admin      managedtest.Auth
	workspace  string
	forgejoURL string
}

// startForgejo starts the core, creates the Forgejo instance as the install
// flow does (#61 phase 2 adds the API; mutate adjusts it first), installs it
// with the agent, runs Setup and activates it, which starts provisioning.
func startForgejo(t *testing.T, mutate func(*forgejoEnv)) *forgejoEnv {
	t.Helper()
	public, internal := managedtest.FreePort(t), managedtest.FreePort(t)
	b := managedtest.StartBundleWithCore(t, internal)
	cfg := startCore(t, b, pgtest.NewDatabase(t), public, internal)
	e := &forgejoEnv{t: t, b: b, ctx: context.Background(), workspace: "http://" + managedtest.WorkspaceHost, forgejoURL: "http://" + managedtest.ForgejoHost}
	br := newBrowser(t, b)
	managedtest.Eventually(t, time.Minute, "the core answers through Traefik", func() bool {
		resp, _ := br.do(t, http.MethodGet, e.workspace+"/healthz", nil)
		return resp.StatusCode == http.StatusOK
	})
	var err error
	if e.pool, err = database.Connect(e.ctx, cfg.Database, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.pool.Close)
	if e.stack, err = web.NewStack(e.pool, cfg, slog.New(slog.DiscardHandler), web.StackOptions{}); err != nil {
		t.Fatal(err)
	}
	rc, err := river.NewClient(riverpgxv5.New(e.pool), &river.Config{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	e.stack.Apps.SetQueue(rc) // insert only: the server's workers run the jobs

	// The instance: its OIDC client and credentials in one transaction.
	e.inst, err = e.stack.Apps.CreateInstance(e.ctx, audit.System, apps.NewInstance{
		Provider: forgejo.ID, BaseURL: e.forgejoURL, Settings: map[string]any{forgejo.SettingOrganization: "acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := e.stack.Identity.OIDCClient(e.ctx, provider.OIDCClientID(e.inst.Key))
	if err != nil {
		t.Fatalf("the OIDC client was not registered: %v", err)
	}
	if want := e.forgejoURL + "/user/oauth2/orglane/callback"; len(client.RedirectUris) != 1 || client.RedirectUris[0] != want {
		t.Fatalf("redirect URIs = %v, want [%s]", client.RedirectUris, want)
	}
	if mutate != nil {
		mutate(e)
	}

	// The agent installs it with the core's inputs and credentials.
	registry, err := packages.NewRegistry(forgejopkg.FS)
	if err != nil {
		t.Fatal(err)
	}
	agentClient := startAgent(t, b, registry)
	env := e.stack.Apps.EnvForTest(e.inst)
	lc, _ := e.inst.Adapter.Lifecycle()
	in, err := lc.Inputs(e.ctx, env, e.inst.View())
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{}
	for _, name := range in.Secrets {
		v, err := env.Credential(e.ctx, name)
		if err != nil {
			t.Fatalf("credential %s: %v", name, err)
		}
		secrets[name] = string(v)
	}
	e.admin = managedtest.Auth{User: forgejo.AdminUsername, Password: secrets[forgejo.CredentialAdminPassword]}
	spec := managedtest.ForgejoSpec(e.inst.ID, 1, managedtest.FreePort(t), secrets, agentapi.StateRunning)
	for k, v := range in.Settings {
		spec.Settings[k] = v
	}
	project := packages.InstanceProject(b.Project, e.inst.Key)
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(t.Context()), "docker", "compose", "-p", project, "down", "--remove-orphans").Run() //nolint:gosec // test commands
	})
	// The core reaches the service by its alias on the edge network; from
	// the test process that alias goes through Traefik too.
	b.Routes(t, managedtest.ForgejoRoutes(e.inst.Key)[1])
	op, _, err := agentClient.Apply(t.Context(), e.inst.Key, spec)
	if err != nil {
		t.Fatal(err)
	}
	managedtest.Eventually(t, 5*time.Minute, "the install finishes", func() bool {
		op, err = agentClient.Operation(t.Context(), op.ID)
		return err == nil && op.Terminal()
	})
	if op.State != agentapi.OpSucceeded {
		t.Fatalf("install: %+v (the configure hook needs Orglane's discovery document)", op)
	}
	// Applied: the core routes it (the install flow records this).
	if _, err := e.pool.Exec(e.ctx, `UPDATE app_instances SET package_version = $2, desired_generation = 1, observed_generation = 1 WHERE id = $1`,
		e.inst.ID, forgejopkg.Current); err != nil {
		t.Fatal(err)
	}
	managedtest.Eventually(t, time.Minute, "Traefik routes Forgejo from the core's configuration", func() bool {
		resp, _ := br.do(t, http.MethodGet, e.forgejoURL+"/api/healthz", nil)
		return resp.StatusCode == http.StatusOK
	})
	adminEnv := &managedtest.Env{Client: b.Client(), Creds: map[string][]byte{forgejo.CredentialAdminPassword: []byte(e.admin.Password)}}
	if err := lc.Setup(e.ctx, adminEnv, e.inst.View()); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if e.inst, err = e.stack.Apps.Transition(e.ctx, audit.System, e.inst.ID, apps.StateActive, ""); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return e
}

// forgejoUser is a Forgejo account as the admin API shows it.
type forgejoUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	LoginName string `json:"login_name"`
	SourceID  int64  `json:"source_id"`
	FullName  string `json:"full_name"`
	Email     string `json:"email"`
}

// account returns the Forgejo account bound to the person's subject.
func (e *forgejoEnv) account(personID string) *forgejoUser {
	e.t.Helper()
	code, body := managedtest.Request(e.t, e.b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/v1/admin/users?limit=50&login_name="+personID, e.admin, nil)
	if code != http.StatusOK {
		e.t.Fatalf("list accounts: %d", code)
	}
	var users []forgejoUser
	if err := json.Unmarshal(body, &users); err != nil {
		e.t.Fatal(err)
	}
	for _, u := range users {
		if u.LoginName == personID {
			return &u
		}
	}
	return nil
}

// teamID returns the ID of an organization team, 0 if there is none.
func (e *forgejoEnv) teamID(name string) int64 {
	e.t.Helper()
	code, body := managedtest.Request(e.t, e.b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/v1/orgs/acme/teams?limit=50", e.admin, nil)
	if code != http.StatusOK {
		e.t.Fatalf("list teams: %d", code)
	}
	var teams []struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &teams); err != nil {
		e.t.Fatal(err)
	}
	for _, t := range teams {
		if t.Name == name {
			if !strings.HasPrefix(t.Description, "Managed by Orglane") {
				e.t.Fatalf("team %s is not marked as Orglane's: %q", name, t.Description)
			}
			return t.ID
		}
	}
	return 0
}

// teamIDAny returns the ID of any organization team (marked or not).
func (e *forgejoEnv) teamIDAny(name string) int64 {
	e.t.Helper()
	code, body := managedtest.Request(e.t, e.b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/v1/orgs/acme/teams?limit=50", e.admin, nil)
	if code != http.StatusOK {
		e.t.Fatalf("list teams: %d", code)
	}
	var teams []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &teams); err != nil {
		e.t.Fatal(err)
	}
	for _, t := range teams {
		if t.Name == name {
			return t.ID
		}
	}
	e.t.Fatalf("no team %s", name)
	return 0
}

// orgMember reports whether the login is a member of the organization.
func (e *forgejoEnv) orgMember(login string) bool {
	e.t.Helper()
	code, _ := managedtest.Request(e.t, e.b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/v1/orgs/acme/members/"+login, e.admin, nil)
	return code == http.StatusNoContent
}

// inTeam reports whether the login is in the Forgejo team.
func (e *forgejoEnv) inTeam(team int64, login string) bool {
	e.t.Helper()
	code, _ := managedtest.Request(e.t, e.b.Client(), http.MethodGet, managedtest.ForgejoHost, "/api/v1/teams/"+strconv.FormatInt(team, 10)+"/members/"+login, e.admin, nil)
	return code == http.StatusOK
}

func (e *forgejoEnv) person(username string) store.Person {
	e.t.Helper()
	p, err := store.New(e.pool).GetPersonByUsername(e.ctx, username)
	if err != nil {
		e.t.Fatalf("person %s: %v", username, err)
	}
	return p
}

func (e *forgejoEnv) orglaneTeam(slug string) store.Team {
	e.t.Helper()
	tm, err := store.New(e.pool).GetTeamBySlug(e.ctx, slug)
	if err != nil {
		e.t.Fatalf("team %s: %v", slug, err)
	}
	return tm
}

func (e *forgejoEnv) result(personID uuid.UUID) store.ProvisioningResult {
	e.t.Helper()
	r, err := store.New(e.pool).GetProvisioningResult(e.ctx, store.GetProvisioningResultParams{InstanceID: e.inst.ID, PersonID: personID})
	if err != nil {
		e.t.Fatalf("result: %v", err)
	}
	return r
}

// TestForgejoProvisioningAndSignOn is the acceptance of #62 against a real
// Forgejo installed by the real agent and the real core with its job
// workers, all behind the real Traefik, in local mode (mapped teams only,
// so who gets an account is explicit): people of mapped teams get accounts
// bound to the Orglane sign-in source and their team; an invited person
// gets theirs once they accept and join; a membership change propagates;
// a small removal goes through the removal guard on its own, a mass removal
// waits for an admin's confirmation; the deep link lands signed in, through
// a real OIDC round trip, and nobody without an Orglane-created account gets
// in.
func TestForgejoProvisioningAndSignOn(t *testing.T) {
	if managedtest.RunAsRoot(t) {
		return
	}
	policy := provider.MappedTeams
	var platform, product store.Team
	e := startForgejo(t, func(e *forgejoEnv) {
		if _, err := e.stack.Apps.UpdateInstance(e.ctx, audit.System, e.inst.ID, apps.InstanceUpdate{AccessPolicy: &policy}); err != nil {
			t.Fatal(err)
		}
		platform, product = e.orglaneTeam("platform"), e.orglaneTeam("product")
		for _, tm := range []store.Team{platform, product} {
			if _, err := e.stack.Apps.SetTeamMapping(e.ctx, audit.System, e.inst.ID, tm.ID, apps.TeamMappingInput{Mode: provider.MappingCreate}); err != nil {
				t.Fatal(err)
			}
		}
	})
	br := newBrowser(t, e.b)

	// People of the mapped teams: an account each, bound to the source with
	// their subject, in their teams. Nobody else.
	olivia, elif := e.person("olivia.kowalski"), e.person("elif.yilmaz")
	managedtest.Eventually(t, 2*time.Minute, "the mapped teams' people have their accounts", func() bool {
		return e.account(olivia.ID.String()) != nil && e.account(e.person("lucas.ferreira").ID.String()) != nil
	})
	platformTeam, productTeam := e.teamID("platform"), e.teamID("product")
	if platformTeam == 0 || productTeam == 0 {
		t.Fatal("the mapped teams do not exist in Forgejo")
	}
	acct := e.account(olivia.ID.String())
	if acct.Login != olivia.Username || acct.FullName != olivia.DisplayName || acct.Email != olivia.WorkEmail || acct.SourceID < 1 {
		t.Fatalf("account = %+v", acct)
	}
	managedtest.Eventually(t, time.Minute, "olivia is in the product team", func() bool { return e.inTeam(productTeam, olivia.Username) })
	if e.inTeam(platformTeam, olivia.Username) || e.account(elif.ID.String()) != nil {
		t.Fatal("an account or membership nobody is mapped to")
	}
	if r := e.result(olivia.ID); r.Status != apps.StatusDone || !r.GrantedByOrglane {
		t.Fatalf("olivia's result = %s (granted %v)", r.Status, r.GrantedByOrglane)
	}
	links, err := store.New(e.pool).ListPersonAccountLinks(e.ctx, olivia.ID)
	if err != nil || len(links) != 1 || links[0].Method != provider.LinkProvisioned || links[0].ExternalID != strconv.FormatInt(acct.ID, 10) {
		t.Fatalf("olivia's link = %+v, %v", links, err)
	}

	// Invite: no account while invited; once the person accepts and is in
	// the platform team, the account follows.
	issued, err := e.stack.Identity.Invite(e.ctx, audit.System, identity.NewInvitation{
		DisplayName: "Nora Berg", ContactEmail: "nora@example.test", Username: "nora.berg", WorkEmail: "nora.berg@acme.test", Role: authz.RoleMember,
		TeamIDs: []uuid.UUID{platform.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	nora := issued.Person
	time.Sleep(3 * time.Second)
	if e.account(nora.ID.String()) != nil {
		t.Fatal("an invited person has an account before accepting")
	}
	_, token, _ := strings.Cut(issued.Link, "#")
	if _, err := e.stack.Identity.AcceptInvitation(e.ctx, audit.System, token, "correct horse battery staple 42"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	managedtest.Eventually(t, time.Minute, "nora's account exists and is in platform", func() bool {
		return e.account(nora.ID.String()) != nil && e.inTeam(platformTeam, "nora.berg")
	})

	// A membership change propagates: into product at once, out of
	// platform through the removal guard (one removal: below the limit, it
	// applies after the guard's delay without a confirmation).
	if _, err := e.stack.Identity.SetTeamMember(e.ctx, audit.System, product.ID, nora.ID, identity.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	if err := e.stack.Identity.RemoveTeamMember(e.ctx, audit.System, platform.ID, nora.ID); err != nil {
		t.Fatal(err)
	}
	managedtest.Eventually(t, time.Minute, "nora is in product", func() bool { return e.inTeam(productTeam, "nora.berg") })
	managedtest.Eventually(t, 3*time.Minute, "nora's platform membership is removed by the guard", func() bool {
		return !e.inTeam(platformTeam, "nora.berg")
	})
	managedtest.Eventually(t, time.Minute, "nora's result is done", func() bool { return e.result(nora.ID).Status == apps.StatusDone })
	if e.account(nora.ID.String()) == nil {
		t.Fatal("a team removal deleted the account")
	}

	// Forgejo teams are Orglane's only: a mapping to an existing team (here
	// Owners) is refused, so no mapping can make anyone an owner.
	owners := e.teamIDAny("Owners")
	if _, err := e.stack.Apps.SetTeamMapping(e.ctx, audit.System, e.inst.ID, e.orglaneTeam("leadership").ID, apps.TeamMappingInput{
		Mode: provider.MappingExisting, ExternalRef: strconv.FormatInt(owners, 10),
	}); apps.Code(err) != "invalid_input" {
		t.Fatalf("an existing mapping to Owners: %v", err)
	}

	// A mass removal: unmapping platform takes five people out of the team,
	// above the limit: held for an admin, nobody removed until confirmed.
	if err := e.stack.Apps.RemoveTeamMapping(e.ctx, audit.System, e.inst.ID, platform.ID); err != nil {
		t.Fatal(err)
	}
	var held *apps.HeldBatch
	managedtest.Eventually(t, 3*time.Minute, "the removals are held", func() bool {
		held, err = e.stack.Apps.HeldRemovals(e.ctx, e.inst.ID)
		return err == nil && held != nil && len(held.Items) >= 4
	})
	if !e.inTeam(platformTeam, "lucas.ferreira") {
		t.Fatal("a held removal was applied")
	}
	if err := e.stack.Apps.ConfirmRemovals(e.ctx, audit.System, e.inst.ID, held.Batch.ID, held.Batch.ItemsHash); err != nil {
		t.Fatal(err)
	}
	managedtest.Eventually(t, 3*time.Minute, "the confirmed removals are applied", func() bool {
		return !e.inTeam(platformTeam, "lucas.ferreira") && !e.inTeam(platformTeam, "jonas.lindqvist")
	})
	if !e.inTeam(productTeam, "sofia.petrova") {
		t.Fatal("unmapping platform took sofia out of product")
	}
	// Platform was the last mapped team of lucas and jonas. In Forgejo,
	// leaving the last team of an organization also leaves the
	// organization; the base team keeps them members (no organization
	// removal), and their accounts stay.
	baseTeam := e.teamID("orglane-members")
	for _, login := range []string{"lucas.ferreira", "jonas.lindqvist"} {
		if !e.inTeam(baseTeam, login) || !e.orgMember(login) {
			t.Fatalf("%s left the organization (base team %v)", login, e.inTeam(baseTeam, login))
		}
	}
	if e.inTeam(owners, "olivia.kowalski") || e.inTeam(owners, "nora.berg") {
		t.Fatal("somebody became an owner")
	}

	// Renames: a new username renames the Forgejo account and refreshes
	// the verified link's login. And when the rename committed but its
	// answer was lost (emulated: Forgejo already has the new login), the
	// next run still refreshes the link.
	sofia, fatima := e.person("sofia.petrova"), e.person("fatima.alsayed")
	for _, rename := range []struct {
		p       store.Person
		to      string
		already bool
	}{{sofia, "sofia.p", false}, {fatima, "fatima.a", true}} {
		if rename.already {
			acct := e.account(rename.p.ID.String())
			if code, body := managedtest.Request(t, e.b.Client(), http.MethodPost, managedtest.ForgejoHost, "/api/v1/admin/users/"+acct.Login+"/rename", e.admin,
				map[string]any{"new_username": rename.to}); code != http.StatusNoContent {
				t.Fatalf("rename in Forgejo: %d %s", code, body)
			}
		}
		to := rename.to
		if _, err := e.stack.Identity.UpdatePerson(e.ctx, audit.System, rename.p.ID, identity.PersonUpdate{Username: &to}); err != nil {
			t.Fatal(err)
		}
		managedtest.Eventually(t, 2*time.Minute, rename.to+": account and link follow the rename", func() bool {
			acct := e.account(rename.p.ID.String())
			links, err := store.New(e.pool).ListPersonAccountLinks(e.ctx, rename.p.ID)
			return acct != nil && acct.Login == rename.to && err == nil && len(links) == 1 && links[0].Login == rename.to
		})
	}

	// The deep link lands signed in for a provisioned person: Forgejo's
	// sign-in with Orglane, Orglane's existing session, back to Forgejo.
	if resp, body := br.do(t, http.MethodPost, e.workspace+"/api/v1/demo/sign-in", map[string]string{"username": olivia.Username}); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in to Orglane: %d %s", resp.StatusCode, body)
	}
	resp, body := br.do(t, http.MethodGet, e.workspace+"/api/v1/apps/instances/"+e.inst.ID.String()+"/open?to="+url.QueryEscape("/acme"), nil)
	var link struct{ URL string }
	if err := json.Unmarshal(body, &link); resp.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("deep link: %d %s", resp.StatusCode, body)
	}
	if want := e.forgejoURL + "/user/oauth2/orglane?redirect_to=%2Facme"; link.URL != want {
		t.Fatalf("deep link = %s, want %s", link.URL, want)
	}
	resp, html := br.do(t, http.MethodGet, link.URL, nil)
	if resp.StatusCode != http.StatusOK || resp.URL.String() != e.forgejoURL+"/acme" || !strings.Contains(string(html), olivia.Username) {
		t.Fatalf("the deep link ended at %s (%d), want %s/acme signed in", resp.URL, resp.StatusCode, e.forgejoURL)
	}
	if !signedInToForgejo(t, br, e.forgejoURL) {
		t.Fatal("not signed in to Forgejo after the deep link")
	}
	// People cannot sign in with a password; only through Orglane.
	resp, html = newBrowser(t, e.b).do(t, http.MethodGet, e.forgejoURL+"/user/login", nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(html), `name="password"`) || !strings.Contains(string(html), "/user/oauth2/orglane") {
		t.Fatalf("Forgejo's sign-in page (%d) offers a password form or no Orglane sign-in", resp.StatusCode)
	}
	// Without an Orglane session the deep link stops at Orglane's sign-in.
	anonymous := newBrowser(t, e.b)
	resp, _ = anonymous.do(t, http.MethodGet, link.URL, nil)
	if resp.URL.Host != managedtest.WorkspaceHost || signedInToForgejo(t, anonymous, e.forgejoURL) {
		t.Fatalf("without an Orglane session the deep link ended at %s", resp.URL)
	}
	// The same browser, still signed in to Forgejo as olivia, signs in to
	// Orglane as elif, whom Orglane has not provisioned, and follows the
	// deep link. Forgejo would link that subject to the open Forgejo
	// session's account, whatever ACCOUNT_LINKING says; Orglane refuses to
	// sign elif in to this client (access_denied), so Forgejo never gets
	// the subject (providers.md "Spec gaps decided in #62").
	if resp, body := br.do(t, http.MethodPost, e.workspace+"/api/v1/demo/sign-in", map[string]string{"username": elif.Username}); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in as elif: %d %s", resp.StatusCode, body)
	}
	br.do(t, http.MethodGet, link.URL, nil)
	if n := e.b.Psql(t, "svc_git", "select count(*) from external_login_user"); n != "0" {
		t.Fatalf("Forgejo linked a subject to an existing account (%s links)", n)
	}
	// Signed in to Orglane but without an account Orglane created (in no
	// mapped team), in a fresh browser: Forgejo neither creates one nor
	// links another to them, and the mixed session above gave them nothing.
	other := newBrowser(t, e.b)
	if resp, body := other.do(t, http.MethodPost, e.workspace+"/api/v1/demo/sign-in", map[string]string{"username": elif.Username}); resp.StatusCode != http.StatusOK {
		t.Fatalf("sign in as elif: %d %s", resp.StatusCode, body)
	}
	other.do(t, http.MethodGet, link.URL, nil)
	if signedInToForgejo(t, other, e.forgejoURL) || e.account(elif.ID.String()) != nil {
		t.Fatal("a person without a provisioned account got into Forgejo")
	}
}

// signedInToForgejo reports whether the browser has a Forgejo session: the
// settings page answers instead of sending it to sign in.
func signedInToForgejo(t *testing.T, br browser, forgejoURL string) bool {
	t.Helper()
	resp, _ := br.do(t, http.MethodGet, forgejoURL+"/user/settings", nil)
	return resp.StatusCode == http.StatusOK && resp.URL.Path == "/user/settings"
}
