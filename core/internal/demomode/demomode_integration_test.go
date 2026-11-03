//go:build integration

package demomode_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/api"
	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/demomode"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/demo"
	"github.com/orglane/orglane/tests/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var fastHasher = crypto.NewPasswordHasher(crypto.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}, 4)

type env struct {
	t       *testing.T
	cfg     *config.Config
	pool    *pgxpool.Pool
	stack   *web.Stack
	dataset *demo.Dataset
}

// newEnv returns the identity stack on a fresh, migrated database with
// seeded roles, served over plain HTTP (development scheme).
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Environment = config.Development
	cfg.Domain = config.Domain{Base: "localhost", WorkspaceHost: "localhost", ServiceHostTemplate: config.DefaultServiceHostTemplate, Scheme: "http"}
	cfg.Identity.MasterSecret = "integration-test-master-secret-0123456789"
	cfg.Database = config.Database{URL: pgtest.NewDatabase(t), MaxConns: 10, ConnectTimeout: 10 * time.Second}
	pool, err := database.Connect(ctx, cfg.Database, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrator, err := database.NewMigrator(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrator.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = migrator.Close()
	stack, err := web.NewStack(pool, &cfg, logger, web.StackOptions{Hasher: fastHasher})
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Identity.SeedBuiltinRoles(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.Keys.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := demo.Load(demo.Acme)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, cfg: &cfg, pool: pool, stack: stack, dataset: d}
}

func (e *env) prepare() demomode.Result {
	e.t.Helper()
	res, err := demomode.Prepare(context.Background(), e.pool, e.stack.Identity, e.dataset)
	if err != nil {
		e.t.Fatalf("Prepare: %v", err)
	}
	return res
}

// serve starts the API, in demo mode when demoMode is set.
func (e *env) serve(demoMode bool) *httptest.Server {
	e.t.Helper()
	opts := api.Options{
		Version: "test", BaseDomain: e.cfg.Domain.Base, WorkspaceHost: e.cfg.Domain.WorkspaceHost, WorkspaceURL: e.cfg.WorkspaceURL(),
		Auth: e.stack.Auth, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if demoMode {
		opts.Demo = e.dataset
	}
	h, err := api.NewHandler(opts)
	if err != nil {
		e.t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	return srv
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func client(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func request(t *testing.T, c *http.Client, method, url, body string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, b
}

func TestPrepareLoadsEmptyDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := e.prepare()
	n := len(e.dataset.People)
	if !res.Loaded || res.People != n {
		t.Fatalf("Prepare = %+v, want loaded with %d people", res, n)
	}
	if got := e.count("SELECT count(*) FROM people WHERE status = 'active'"); got != n {
		t.Fatalf("%d active people, want %d", got, n)
	}
	for _, p := range e.dataset.People {
		person, err := e.stack.Identity.GetPersonByUsername(ctx, p.Username)
		if err != nil {
			t.Fatalf("%s: %v", p.Username, err)
		}
		if person.DisplayName != p.DisplayName || person.WorkEmail != p.WorkEmail || person.Title == nil || *person.Title != p.Title {
			t.Errorf("%s: loaded as %+v", p.Username, person)
		}
		roles, err := e.stack.Identity.PersonRoles(ctx, person.ID)
		if err != nil || len(roles) != 1 || roles[0].Key != p.Role {
			t.Errorf("%s: roles = %+v, %v, want %s", p.Username, roles, err, p.Role)
		}
		avatar, err := e.stack.Identity.GetAvatar(ctx, person.ID)
		if err != nil || avatar.ContentType != identity.AvatarSVG {
			t.Errorf("%s: avatar = %q, %v", p.Username, avatar.ContentType, err)
		}
	}
	// Demo mode replaces first-run setup: the dataset brings its owner.
	if required, err := e.stack.Identity.SetupRequired(ctx); err != nil || required {
		t.Fatalf("SetupRequired = %v, %v; want false", required, err)
	}
	marker, ok, err := demomode.IsDemo(ctx, e.pool)
	if err != nil || !ok || marker.Dataset != demo.Acme || marker.State != "loaded" {
		t.Fatalf("IsDemo = %+v, %v, %v", marker, ok, err)
	}

	// Everything was written through the identity service, with audit
	// entries by the demo actor.
	for action, want := range map[audit.Action]int{
		audit.PersonCreated:      n,
		audit.PersonRoleAssigned: n,
		audit.PersonAvatarSet:    n,
		audit.InstanceDemoLoaded: 1,
	} {
		if got := e.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND actor_kind = 'system' AND user_agent = 'orglane-demo'`, string(action)); got != want {
			t.Errorf("%d %s entries by the demo actor, want %d", got, action, want)
		}
	}
	if got := e.count(`SELECT count(*) FROM audit_entries WHERE action = 'instance.demo_loaded' AND metadata @> '{"dataset": "acme", "people": 14}'`); got != 1 {
		t.Errorf("instance.demo_loaded metadata does not name the dataset and people count")
	}

	// A restart in demo mode keeps the data and writes nothing.
	before := e.count("SELECT count(*) FROM audit_entries")
	if res := e.prepare(); res.Loaded {
		t.Fatal("second Prepare loaded the dataset again")
	}
	if after := e.count("SELECT count(*) FROM audit_entries"); after != before {
		t.Fatalf("second Prepare wrote %d audit entries", after-before)
	}
}

func TestPrepareRefusesNonDemoDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.stack.Identity.CreatePerson(ctx, audit.System, identity.NewPerson{
		Username: "real", WorkEmail: "real@acme.com", DisplayName: "Real Person", Status: identity.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	before := e.count("SELECT count(*) FROM audit_entries")
	_, err := demomode.Prepare(ctx, e.pool, e.stack.Identity, e.dataset)
	if !errors.Is(err, demomode.ErrNotEmpty) {
		t.Fatalf("Prepare on a database with people = %v, want ErrNotEmpty", err)
	}
	if n := e.count("SELECT count(*) FROM people"); n != 1 {
		t.Fatalf("Prepare changed the people: %d", n)
	}
	if n := e.count("SELECT count(*) FROM instance_settings WHERE key = 'demo'"); n != 0 {
		t.Fatal("Prepare marked a non-demo database as demo")
	}
	if after := e.count("SELECT count(*) FROM audit_entries"); after != before {
		t.Fatalf("a refused Prepare wrote %d audit entries", after-before)
	}
}

func TestPrepareRefusesOtherDataset(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO instance_settings (key, value) VALUES ('demo', '{"dataset": "globex", "version": 1, "state": "loaded"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := demomode.Prepare(context.Background(), e.pool, e.stack.Identity, e.dataset); !errors.Is(err, demomode.ErrOtherDataset) {
		t.Fatalf("Prepare = %v, want ErrOtherDataset", err)
	}
}

// TestPrepareResumesInterruptedLoad: a load that stopped half-way (marker
// "loading", some people created) is completed on the next start.
func TestPrepareResumesInterruptedLoad(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO instance_settings (key, value) VALUES ('demo', '{"dataset": "acme", "version": 1, "state": "loading"}')`); err != nil {
		t.Fatal(err)
	}
	first := e.dataset.People[0]
	if _, err := e.stack.Identity.CreatePerson(ctx, demomode.Actor, identity.NewPerson{
		Username: first.Username, WorkEmail: first.WorkEmail, DisplayName: first.DisplayName, Title: first.Title, Status: identity.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if res := e.prepare(); !res.Loaded {
		t.Fatal("Prepare did not finish the interrupted load")
	}
	if n := e.count("SELECT count(*) FROM people"); n != len(e.dataset.People) {
		t.Fatalf("%d people after resuming, want %d", n, len(e.dataset.People))
	}
	if n := e.count("SELECT count(*) FROM audit_entries WHERE action = 'person.created'"); n != len(e.dataset.People) {
		t.Fatalf("%d person.created entries, want one per person", n)
	}
}

// TestOneClickSignIn: in demo mode, one request signs in as the Owner (or a
// chosen demo person) with a normal session, and the directory shows the
// Acme people.
func TestOneClickSignIn(t *testing.T) {
	e := newEnv(t)
	e.prepare()
	srv := e.serve(true)
	c := client(t)

	code, _, body := request(t, c, http.MethodGet, srv.URL+"/.well-known/orglane", "")
	var discovery apigen.OrglaneDiscovery
	if err := json.Unmarshal(body, &discovery); code != http.StatusOK || err != nil || !discovery.Demo {
		t.Fatalf("discovery = %d %s", code, body)
	}

	// Unauthenticated: no directory.
	if code, _, _ := request(t, c, http.MethodGet, srv.URL+"/api/v1/people", ""); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/people without a session = %d, want 401", code)
	}

	// No body: the Owner.
	code, _, body = request(t, c, http.MethodPost, srv.URL+"/api/v1/demo/sign-in", "")
	var result apigen.SignInResult
	if err := json.Unmarshal(body, &result); code != http.StatusOK || err != nil {
		t.Fatalf("one-click sign-in = %d %s", code, body)
	}
	owner := e.dataset.Owner()
	if result.Person.Username != owner.Username || result.CsrfToken == "" {
		t.Fatalf("signed in as %+v", result.Person)
	}

	code, _, body = request(t, c, http.MethodGet, srv.URL+"/api/v1/me", "")
	var me apigen.Me
	if err := json.Unmarshal(body, &me); code != http.StatusOK || err != nil {
		t.Fatalf("GET /api/v1/me = %d %s", code, body)
	}
	if me.Session == nil || !slices.Equal(me.Session.AuthMethods, []apigen.SessionAuthMethods{"demo"}) || !slices.Contains(me.Permissions, "billing.manage") {
		t.Fatalf("me = %+v (session %+v)", me, me.Session)
	}

	code, _, body = request(t, c, http.MethodGet, srv.URL+"/api/v1/people", "")
	var people apigen.PersonList
	if err := json.Unmarshal(body, &people); code != http.StatusOK || err != nil {
		t.Fatalf("GET /api/v1/people = %d %s", code, body)
	}
	if len(people.Items) != len(e.dataset.People) {
		t.Fatalf("%d people in the directory, want %d", len(people.Items), len(e.dataset.People))
	}
	var avatarURL string
	for _, p := range people.Items {
		want, ok := e.dataset.Person(p.Username)
		if !ok || p.DisplayName != want.DisplayName || p.Title == nil || *p.Title != want.Title ||
			!slices.Equal(p.Roles, []string{want.Role}) || p.AvatarUrl == nil || p.Status != apigen.Active {
			t.Errorf("directory entry %+v does not match the dataset", p)
		}
		if p.Username == owner.Username {
			avatarURL = *p.AvatarUrl
		}
	}

	// The avatar is served inert: fixed content type, nosniff, sandboxed.
	code, header, body := request(t, c, http.MethodGet, srv.URL+avatarURL, "")
	if code != http.StatusOK || header.Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(string(body), "<svg") {
		t.Fatalf("avatar = %d %q", code, header.Get("Content-Type"))
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "default-src 'none'; style-src 'unsafe-inline'; sandbox",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("avatar %s = %q, want %q", name, got, want)
		}
	}
	if code, _, _ := request(t, client(t), http.MethodGet, srv.URL+avatarURL, ""); code != http.StatusUnauthorized {
		t.Errorf("avatar without a session = %d, want 401", code)
	}

	// A chosen member: a normal session without admin permissions.
	member := e.dataset.People[len(e.dataset.People)-1]
	mc := client(t)
	code, _, body = request(t, mc, http.MethodPost, srv.URL+"/api/v1/demo/sign-in", `{"username": "`+member.Username+`"}`)
	if code != http.StatusOK || !strings.Contains(string(body), member.Username) {
		t.Fatalf("sign-in as %s = %d %s", member.Username, code, body)
	}
	if code, _, _ := request(t, mc, http.MethodGet, srv.URL+"/api/v1/instance", ""); code != http.StatusForbidden {
		t.Fatalf("member GET /api/v1/instance = %d, want 403", code)
	}

	// Sign-ins are audited like any other, with the demo method.
	if n := e.count(`SELECT count(*) FROM audit_entries WHERE action = 'sign_in.succeeded' AND metadata->>'method' = 'demo'`); n != 2 {
		t.Fatalf("%d demo sign-in entries, want 2", n)
	}
}

func TestOneClickSignInOnlyForActiveDatasetPeople(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.prepare()
	srv := e.serve(true)

	if code, _, _ := request(t, client(t), http.MethodPost, srv.URL+"/api/v1/demo/sign-in", `{"username": "nobody"}`); code != http.StatusNotFound {
		t.Fatalf("unknown username = %d, want 404", code)
	}
	// A person added to the demo instance later is not a demo person.
	if _, err := e.stack.Identity.CreatePerson(ctx, audit.System, identity.NewPerson{
		Username: "real", WorkEmail: "real@acme.com", DisplayName: "Real Person", Status: identity.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, client(t), http.MethodPost, srv.URL+"/api/v1/demo/sign-in", `{"username": "real"}`); code != http.StatusNotFound {
		t.Fatalf("non-dataset person = %d, want 404", code)
	}
	// A demo person whose work email was changed no longer matches.
	member := e.dataset.People[3]
	p, err := e.stack.Identity.GetPersonByUsername(ctx, member.Username)
	if err != nil {
		t.Fatal(err)
	}
	other := "someone@acme.com"
	if _, err := e.stack.Identity.UpdatePerson(ctx, audit.System, p.ID, identity.PersonUpdate{WorkEmail: &other}); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, client(t), http.MethodPost, srv.URL+"/api/v1/demo/sign-in", `{"username": "`+member.Username+`"}`); code != http.StatusNotFound {
		t.Fatalf("demo person with another email = %d, want 404", code)
	}
	// A suspended demo person cannot sign in.
	suspended := e.dataset.People[4]
	p, err = e.stack.Identity.GetPersonByUsername(ctx, suspended.Username)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.stack.Identity.SetPersonStatus(ctx, audit.System, p.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := request(t, client(t), http.MethodPost, srv.URL+"/api/v1/demo/sign-in", `{"username": "`+suspended.Username+`"}`); code != http.StatusForbidden {
		t.Fatalf("suspended demo person = %d, want 403", code)
	}
	// Cross-origin browser requests are rejected (login CSRF).
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/api/v1/demo/sign-in", nil)
	req.Header.Set("Origin", "https://evil.example")
	res, err := client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin sign-in = %d, want 403", res.StatusCode)
	}
}

// TestDemoRoutesAbsentOutsideDemoMode: on the same demo database, a server
// not in demo mode has no demo routes at all.
func TestDemoRoutesAbsentOutsideDemoMode(t *testing.T) {
	e := newEnv(t)
	e.prepare()
	srv := e.serve(false)
	c := client(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/demo"},
		{http.MethodPost, "/api/v1/demo/sign-in"},
	} {
		code, header, _ := request(t, c, tc.method, srv.URL+tc.path, "")
		if code != http.StatusNotFound || header.Get("Content-Type") != api.ProblemContentType {
			t.Errorf("%s %s outside demo mode = %d, want a 404 problem", tc.method, tc.path, code)
		}
	}
	if n := e.count("SELECT count(*) FROM sessions"); n != 0 {
		t.Fatalf("%d sessions created outside demo mode", n)
	}
	_, _, body := request(t, c, http.MethodGet, srv.URL+"/.well-known/orglane", "")
	var discovery apigen.OrglaneDiscovery
	if err := json.Unmarshal(body, &discovery); err != nil || discovery.Demo {
		t.Fatalf("discovery outside demo mode = %s", body)
	}
}
