//go:build integration

package managedtest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/ingress"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/providers/forgejo"
	forgejopkg "github.com/orglane/orglane/providers/forgejo/managed"
	"github.com/orglane/orglane/providers/provider"
)

// ForgejoHost is the Forgejo host in local mode (agent.md "Local mode").
const ForgejoHost = "git.orglane.localhost"

// ForgejoRoutes are the routes of a Forgejo instance with key: the public
// host, as the core builds it, and the upstream alias as a host of its own,
// so a test outside Docker reaches the core's Upstream URL through Traefik
// too (inside the bundle the core is on the edge network).
func ForgejoRoutes(key string) []ingress.AppRoute {
	upstream := "http://" + packages.ServiceAlias(key, "forgejo", false) + ":3000"
	return []ingress.AppRoute{
		{Key: key, Route: "web", Host: ForgejoHost, Upstream: upstream},
		{Key: key + "-upstream", Route: "web", Host: packages.ServiceAlias(key, "forgejo", false), Upstream: upstream},
		{Key: key + "-fenced", Route: "web", Host: packages.ServiceAlias(key, "forgejo", true),
			Upstream: "http://" + packages.ServiceAlias(key, "forgejo", true) + ":3000"},
	}
}

// ForgejoSecrets returns fresh values of the package's secrets.
func ForgejoSecrets() map[string]string {
	return map[string]string{
		forgejo.CredentialDBPassword:    crypto.RandomToken(""),
		forgejo.CredentialSecretKey:     crypto.RandomToken(""),
		forgejo.CredentialAdminPassword: crypto.RandomToken(""),
		// A placeholder: tests that sign in through Orglane pass the
		// secret of the instance's real OIDC client.
		forgejo.CredentialOIDCClientSecret: crypto.RandomToken(""),
	}
}

// ForgejoSpec is a desired spec of the current Forgejo package in local
// mode.
func ForgejoSpec(id uuid.UUID, gen int64, sshPort int, secrets map[string]string, state string) *agentapi.Spec {
	return &agentapi.Spec{
		InstanceID: id,
		Generation: gen,
		Package:    agentapi.PackageRef{ID: "forgejo", Version: forgejopkg.Current},
		State:      state,
		Hosts:      map[string]string{"workspace": "orglane.localhost", "web": ForgejoHost},
		URLs: agentapi.URLs{
			Workspace: "http://orglane.localhost", Issuer: "http://orglane.localhost",
			Instance: "http://" + ForgejoHost, Webhook: "http://orglane.localhost/api/v1/webhooks/" + id.String(),
		},
		Ingress:  agentapi.IngressLocal,
		Settings: map[string]any{forgejo.SettingSSHPort: sshPort, "oidc_client_id": "app-git"},
		Secrets:  secrets,
	}
}

// Env is the core's provider.Env for an adapter, with fixed credentials.
type Env struct {
	Creds  map[string][]byte
	Client *http.Client
}

// Credential implements provider.Env. It returns a copy: adapters may clear
// the value they receive, and a test's fixture must stay usable.
func (e *Env) Credential(_ context.Context, name string) ([]byte, error) {
	if v, ok := e.Creds[name]; ok {
		return append([]byte(nil), v...), nil
	}
	return nil, provider.ErrNotFound
}

// Credentials implements provider.Env.
func (e *Env) Credentials(ctx context.Context, name string) ([][]byte, error) {
	v, err := e.Credential(ctx, name)
	return [][]byte{v}, err
}

// HTTPClient implements provider.Env.
func (e *Env) HTTPClient() *http.Client { return e.Client }

// Logger implements provider.Env.
func (*Env) Logger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Now implements provider.Env.
func (*Env) Now() time.Time { return time.Now() }

// WorkspaceURL implements provider.Env.
func (*Env) WorkspaceURL() *url.URL { return &url.URL{Scheme: "http", Host: "orglane.localhost"} }

// IssuerURL implements provider.Env.
func (*Env) IssuerURL() *url.URL { return &url.URL{Scheme: "http", Host: "orglane.localhost"} }

// WebhookURL implements provider.Env.
func (*Env) WebhookURL(uuid.UUID) *url.URL {
	return &url.URL{Scheme: "http", Host: "orglane.localhost"}
}

// Faults is an HTTP transport in front of the real API that injects
// realistic failures into the next matching request.
type Faults struct {
	Next http.RoundTripper
	mu   sync.Mutex
	// drop: forward the request, then lose its answer (the provider
	// committed, the caller never hears).
	drop map[string]bool
	// before: run before forwarding the request (a concurrent change).
	before map[string]func()
}

// Drop loses the answer of the next request with this method and path.
func (f *Faults) Drop(method, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.drop == nil {
		f.drop = map[string]bool{}
	}
	f.drop[method+" "+path] = true
}

// Before runs fn before the next request with this method and path.
func (f *Faults) Before(method, path string, fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.before == nil {
		f.before = map[string]func(){}
	}
	f.before[method+" "+path] = fn
}

// RoundTrip implements http.RoundTripper.
func (f *Faults) RoundTrip(r *http.Request) (*http.Response, error) {
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	drop, before := f.drop[key], f.before[key]
	delete(f.drop, key)
	delete(f.before, key)
	f.mu.Unlock()
	if before != nil {
		before()
	}
	resp, err := f.Next.RoundTrip(r)
	if err == nil && drop {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, errors.New("connection reset (test)")
	}
	return resp, err
}

// WithFaults returns a copy of client whose transport injects faults.
func WithFaults(client *http.Client) (*http.Client, *Faults) {
	f := &Faults{Next: client.Transport}
	c := *client
	c.Transport = f
	return &c, f
}

// Auth authenticates a request: basic auth (User, Password) or a token.
type Auth struct {
	User, Password, Token string
}

// Request sends a request to host through c and returns the status and the
// body (at most 1 MiB).
func Request(t *testing.T, c *http.Client, method, host, path string, auth Auth, body any) (int, []byte) {
	t.Helper()
	var in io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		in = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+host+path, in)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case auth.Token != "":
		req.Header.Set("Authorization", "token "+auth.Token)
	case auth.User != "":
		req.SetBasicAuth(auth.User, auth.Password)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out
}

// CronSchedule returns the schedule of a Forgejo cron task ("-": off).
func CronSchedule(t *testing.T, c *http.Client, host string, admin Auth, task string) string {
	t.Helper()
	code, body := Request(t, c, http.MethodGet, host, "/api/v1/admin/cron?limit=50", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("cron tasks: %d", code)
	}
	var tasks []struct {
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
	}
	if err := json.Unmarshal(body, &tasks); err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		if tk.Name == task {
			return tk.Schedule
		}
	}
	t.Fatalf("no cron task %s", task)
	return ""
}

// SSHBanner reads the SSH identification line of a server on 127.0.0.1.
func SSHBanner(port int) (string, error) {
	c, err := (&net.Dialer{Timeout: 3 * time.Second}).Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return bufio.NewReader(c).ReadString('\n')
}

// FreePort returns a free TCP port on 127.0.0.1.
func FreePort(t *testing.T) int {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}
