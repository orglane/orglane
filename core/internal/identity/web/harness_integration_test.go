//go:build integration

package web_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/api"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/dbtest"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/core/internal/jobs"
	"github.com/orglane/orglane/core/internal/store"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
	"github.com/orglane/orglane/tests/pgtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

const (
	workspaceHost = "work.acme.test"
	origin        = "https://" + workspaceHost
	ownerEmail    = "owner@acme.test"
	ownerPassword = "correct horse battery staple"
)

// fastHasher keeps tests quick; production uses crypto.DefaultPasswordParams.
var fastHasher = crypto.NewPasswordHasher(crypto.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}, 4)

// clock is a settable clock for signing key rotation.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// env is a complete identity stack behind a TLS test server reached as
// https://work.acme.test.
type env struct {
	t     *testing.T
	cfg   *config.Config
	pool  *pgxpool.Pool
	stack *web.Stack
	srv   *httptest.Server
	clock *clock
	// tp is the in-repo test provider, registered in every test stack.
	tp *testprovider.Provider
}

func newEnv(t *testing.T, mutate ...func(*config.Config)) *env {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Domain.Base = "acme.test"
	cfg.Domain.WorkspaceHost = workspaceHost
	cfg.Identity.MasterSecret = "integration-test-master-secret-0123456789"
	cfg.Database = config.Database{URL: pgtest.NewDatabase(t), MaxConns: 10, ConnectTimeout: 10 * time.Second}
	for _, m := range mutate {
		m(&cfg)
	}
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

	clk := &clock{t: time.Now()}
	tp := testprovider.New()
	stack, err := web.NewStack(pool, &cfg, logger, web.StackOptions{Hasher: fastHasher, Now: clk.Now, Adapters: []provider.CapabilityManifest{tp, testprovider.New(testprovider.WithID("testmanaged"), testprovider.Managed()), githubprovider.New()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Identity.SeedBuiltinRoles(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.Keys.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.Options{
		Version: "test", BaseDomain: cfg.Domain.Base, WorkspaceHost: workspaceHost, WorkspaceURL: cfg.WorkspaceURL(),
		Auth: stack.Auth, Profile: stack.Profile, Imports: stack.Imports, Mail: stack.Mail, WorkEmail: cfg.WorkEmail, Logger: logger,
		Apps: stack.Apps, EmployeeSteps: stack.Apps.EmployeeSteps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return &env{t: t, cfg: &cfg, pool: pool, stack: stack, srv: srv, clock: clk, tp: tp}
}

// startJobs starts the job queue, which delivers queued email, for the
// rest of the test.
func (e *env) startJobs() {
	e.t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := e.cfg.Jobs
	cfg.FetchPollInterval = 200 * time.Millisecond
	client, err := jobs.NewClient(e.pool, cfg, logger, jobs.Options{Mail: e.stack.Mail, Apps: e.stack.Apps})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.StopAndCancel(ctx)
	})
}

// browser is an HTTP client with a cookie jar that does not follow
// redirects, so tests can inspect them.
type browser struct {
	e      *env
	client *http.Client
	csrf   string
	// header is added to every request.
	header http.Header
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	addr := e.srv.Listener.Addr().String()
	transport := &http.Transport{
		// Every host resolves to the test server.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test server's self-signed certificate
	}
	return &browser{
		e:      e,
		client: &http.Client{Jar: jar, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		header: http.Header{},
	}
}

type response struct {
	*http.Response
	body []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v (status %d)", r.body, err, r.StatusCode)
	}
}

// req sends a request. Unsafe methods carry the workspace Origin and the
// CSRF token, like the web UI would, unless headers override them.
func (b *browser) req(method, target string, body any, headers ...string) response {
	b.e.t.Helper()
	var rd io.Reader
	contentType := ""
	switch v := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(v)
	case io.Reader:
		rd, contentType = v, "application/octet-stream"
	case url.Values:
		rd, contentType = strings.NewReader(v.Encode()), "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			b.e.t.Fatal(err)
		}
		rd, contentType = bytes.NewReader(raw), "application/json"
	}
	if !strings.HasPrefix(target, "http") {
		target = origin + target
	}
	r, err := http.NewRequestWithContext(b.e.t.Context(), method, target, rd)
	if err != nil {
		b.e.t.Fatal(err)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for k, v := range b.header {
		r.Header[k] = v
	}
	if method != http.MethodGet && method != http.MethodHead {
		r.Header.Set("Origin", origin)
		if b.csrf != "" {
			r.Header.Set(web.CSRFHeader, b.csrf)
		}
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			r.Header.Del(headers[i])
		} else {
			r.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := b.client.Do(r)
	if err != nil {
		b.e.t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		b.e.t.Fatal(err)
	}
	return response{Response: res, body: raw}
}

func expectStatus(t *testing.T, r response, want int) {
	t.Helper()
	if r.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", r.Request.Method, r.Request.URL, r.StatusCode, want, r.body)
	}
}

type signInResult struct {
	Person struct {
		ID        uuid.UUID `json:"id"`
		WorkEmail string    `json:"workEmail"`
	} `json:"person"`
	CsrfToken    string `json:"csrfToken"`
	PasskeyOffer bool   `json:"passkeyOffer"`
}

// setupOwner completes first-run setup in b and returns the result.
func (e *env) setupOwner(b *browser) signInResult {
	e.t.Helper()
	token, err := e.stack.Identity.ResetSetupToken(context.Background(), audit.System)
	if err != nil {
		e.t.Fatal(err)
	}
	r := b.req("POST", "/api/v1/auth/setup", map[string]string{
		"setupToken": token, "workEmail": ownerEmail, "username": "owner", "displayName": "Olivia Owner", "password": ownerPassword,
	})
	expectStatus(e.t, r, http.StatusCreated)
	var res signInResult
	r.json(e.t, &res)
	b.csrf = res.CsrfToken
	return res
}

// signIn signs in in b with a password (every test account has
// ownerPassword).
func (b *browser) signIn(email string) signInResult {
	b.e.t.Helper()
	r := b.req("POST", "/api/v1/auth/sign-in", map[string]string{"email": email, "password": ownerPassword})
	expectStatus(b.e.t, r, http.StatusOK)
	var res signInResult
	r.json(b.e.t, &res)
	b.csrf = res.CsrfToken
	return res
}

// createMember adds an active member with a password.
func (e *env) createMember(username string) store.Person {
	e.t.Helper()
	ctx := context.Background()
	p, err := e.stack.Identity.CreatePerson(ctx, audit.System, identity.NewPerson{
		Username: username, WorkEmail: username + "@acme.test", DisplayName: "Member " + username, Status: identity.StatusActive,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.stack.Identity.AssignRole(ctx, audit.System, p.ID, authz.RoleMember); err != nil {
		e.t.Fatal(err)
	}
	if err := e.stack.Identity.SetPassword(ctx, audit.System, p.ID, ownerPassword); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) auditCount(action audit.Action) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM audit_entries WHERE action = $1", string(action)).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func cookieNamed(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}
