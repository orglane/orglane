package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agent"
	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

const secret = "client-test-secret-0123456789abcdef0123456789" //nolint:gosec // a test secret

func socketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths are limited to about 100 bytes: not under t.TempDir.
	dir, err := os.MkdirTemp("", "agentclient-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "agent.sock")
}

// startAgent runs a real agent on a socket. Docker is unavailable to it, so
// accepted specs are never applied.
func startAgent(t *testing.T, version string) (string, string) {
	t.Helper()
	reg, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default().Agent
	cfg.Secret, cfg.DataDir, cfg.PeerUID = secret, t.TempDir(), -1
	cfg.Listen = socketPath(t)
	a, err := agent.New(agent.Options{Config: cfg, Version: version, Registry: reg, DockerBinary: "/nonexistent/docker"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("agent: %v", err)
		}
	})
	waitForSocket(t, cfg.Listen)
	return cfg.Listen, cfg.DataDir
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	for range 100 {
		if c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the socket does not accept connections")
}

func testSpec() *agentapi.Spec {
	return &agentapi.Spec{
		InstanceID: uuid.MustParse("0190f0f0-0000-7000-8000-000000000001"),
		Generation: 1,
		Package:    agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version},
		State:      agentapi.StateRunning,
		Hosts:      map[string]string{"workspace": "orglane.localhost", "web": "agenttest.orglane.localhost"},
		Ingress:    agentapi.IngressLocal,
		Settings:   map[string]any{"message": "hello"},
		Secrets:    map[string]string{"db_password": "c2VjcmV0LXBhc3N3b3Jk"}, //nolint:gosec // a test secret
	}
}

func newClient(t *testing.T, socket, sec, version string) *Client {
	t.Helper()
	c, err := New(socket, sec, version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestClientWithAgent(t *testing.T) {
	socket, _ := startAgent(t, "1.2.3")
	c := newClient(t, socket, secret, "1.2.3")
	ctx := t.Context()

	h, err := c.Health(ctx)
	if err != nil || h.Version != "1.2.3" || h.Protocol != agentapi.ProtocolVersion || len(h.Packages) != 1 {
		t.Fatalf("health = %+v, %v", h, err)
	}
	op, accepted, err := c.Apply(ctx, "git", testSpec())
	if err != nil || !accepted || op.Generation != 1 {
		t.Fatalf("apply = %+v, %v, %v", op, accepted, err)
	}
	again, accepted, err := c.Apply(ctx, "git", testSpec())
	if err != nil || accepted || again.ID != op.ID {
		t.Fatalf("retry = %+v, %v, %v", again, accepted, err)
	}
	if got, err := c.Operation(ctx, op.ID); err != nil || got.ID != op.ID {
		t.Fatalf("operation = %+v, %v", got, err)
	}
	st, err := c.Instance(ctx, "git")
	if err != nil || st.AcceptedGeneration != 1 {
		t.Fatalf("instance = %+v, %v", st, err)
	}
	list, err := c.Instances(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("instances = %+v, %v", list, err)
	}

	_, _, err = c.Apply(ctx, "git", testSpec()) // the same generation and content again
	if err != nil {
		t.Fatal(err)
	}
	stale := testSpec()
	stale.Settings["message"] = "changed"
	_, _, err = c.Apply(ctx, "git", stale)
	var pe *ProblemError
	if !errors.As(err, &pe) || pe.Code != agentapi.CodeGenerationConflict || pe.Status != http.StatusConflict {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := c.Instance(ctx, "nope"); !errors.As(err, &pe) || pe.Code != agentapi.CodeNotFound {
		t.Fatalf("unknown instance error = %v", err)
	}
}

func TestClientWrongSecret(t *testing.T) {
	socket, dataDir := startAgent(t, "dev")
	c := newClient(t, socket, strings.Repeat("w", 40), "dev")
	if _, err := c.Health(t.Context()); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("health with a wrong secret: %v", err)
	}
	if _, _, err := c.Apply(t.Context(), "git", testSpec()); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("apply with a wrong secret: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "instances", "git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a client with a wrong secret created an instance")
	}
}

func TestClientVersionMismatch(t *testing.T) {
	socket, dataDir := startAgent(t, "1.2.3")
	c := newClient(t, socket, secret, "1.2.4")
	if _, _, err := c.Apply(t.Context(), "git", testSpec()); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("apply to another version: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "instances", "git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a spec was sent to an agent of another version")
	}
	// Reading status is allowed.
	if _, err := c.Instances(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !CompatibleVersions("dev", "1.0.0") || !CompatibleVersions("1.0.0", "dev") || CompatibleVersions("1.0.0", "1.0.1") {
		t.Fatal("CompatibleVersions")
	}
}

func TestClientUnavailable(t *testing.T) {
	c := newClient(t, socketPath(t), secret, "dev")
	if _, err := c.Health(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("health without an agent: %v", err)
	}
}

// TestClientRefusesImpostor runs a process on the socket that does not hold
// the key: unsigned answers and answers signed with another key. The client
// refuses them and never sends it a spec, so no secret reaches it.
func TestClientRefusesImpostor(t *testing.T) {
	otherKey, err := agentapi.DeriveKey(strings.Repeat("o", 40))
	if err != nil {
		t.Fatal(err)
	}
	for name, sign := range map[string]bool{"unsigned": false, "signed with another key": true} {
		t.Run(name, func(t *testing.T) {
			socket := socketPath(t)
			ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var seen []string
			srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Method+" "+r.URL.Path)
				mu.Unlock()
				body, _ := json.Marshal(agentapi.Health{Protocol: 1, Version: "dev"})
				if sign {
					w.Header().Set(agentapi.HeaderSignature, agentapi.SignResponse(otherKey, agentapi.ParseMAC(r.Header), http.StatusOK, body))
				}
				_, _ = w.Write(body)
			})}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			c := newClient(t, socket, secret, "dev")
			if _, err := c.Health(t.Context()); !errors.Is(err, ErrUntrusted) {
				t.Fatalf("health from an impostor: %v", err)
			}
			if _, _, err := c.Apply(t.Context(), "git", testSpec()); !errors.Is(err, ErrUntrusted) {
				t.Fatalf("apply to an impostor: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, s := range seen {
				if s != "GET "+agentapi.PathHealth {
					t.Fatalf("the impostor received %s", s)
				}
			}
		})
	}
}

func TestFromConfig(t *testing.T) {
	cfg := config.Default().Agent
	if c, err := FromConfig(cfg, "dev"); c != nil || err != nil {
		t.Fatalf("disabled: %v, %v", c, err)
	}
	cfg.Socket, cfg.Secret = "/run/orglane-agent/agent.sock", "short"
	if _, err := FromConfig(cfg, "dev"); err == nil {
		t.Fatal("a short secret was accepted")
	}
	cfg.Secret = secret
	if c, err := FromConfig(cfg, "dev"); c == nil || err != nil {
		t.Fatalf("enabled: %v, %v", c, err)
	}
}
