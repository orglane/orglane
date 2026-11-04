package main

import (
	"bytes"
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func noEnv(string) (string, bool) { return "", false }

func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut, noEnv)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"cli", "version"}, {"--version"}} {
		code, out, _ := runArgs(t, args...)
		if code != 0 || out != "orglane "+version+"\n" {
			t.Errorf("%v: code %d, output %q", args, code, out)
		}
	}
}

func TestAgentRequiresSecret(t *testing.T) {
	code, _, errOut := runArgs(t, "agent")
	if code != 1 || !strings.Contains(errOut, "agent.secret") {
		t.Fatalf("agent: code %d, stderr %q", code, errOut)
	}
}

// TestAgentRunsAndHealthcheck starts `orglane agent` and checks it with
// `orglane agent healthcheck`, and that a wrong secret fails the check.
func TestAgentRunsAndHealthcheck(t *testing.T) {
	sockDir, err := os.MkdirTemp("", "orglane-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	env := map[string]string{
		"ORGLANE_AGENT_SECRET":   strings.Repeat("s", 40),
		"ORGLANE_AGENT_LISTEN":   filepath.Join(sockDir, "agent.sock"),
		"ORGLANE_AGENT_DATA_DIR": t.TempDir(),
		"ORGLANE_AGENT_PEER_UID": "-1",
		"ORGLANE_LOG_FORMAT":     "text",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var agentErr syncBuffer
	go func() { done <- run(ctx, []string{"agent"}, io.Discard, &agentErr, lookup) }()
	defer func() {
		cancel()
		if code := <-done; code != 0 {
			t.Errorf("agent exited with %d: %s", code, agentErr.String())
		}
	}()

	var out, errOut bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	for {
		out.Reset()
		errOut.Reset()
		if run(context.Background(), []string{"agent", "healthcheck"}, &out, &errOut, lookup) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthcheck failed: %s\nagent: %s", errOut.String(), agentErr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "ready") {
		t.Fatalf("healthcheck output %q", out.String())
	}

	wrong := maps.Clone(env)
	wrong["ORGLANE_AGENT_SECRET"] = strings.Repeat("x", 40)
	errOut.Reset()
	if code := run(context.Background(), []string{"agent", "healthcheck"}, io.Discard, &errOut, func(k string) (string, bool) { v, ok := wrong[k]; return v, ok }); code != 1 || !strings.Contains(errOut.String(), "agent_untrusted") {
		t.Fatalf("healthcheck with a wrong secret: code %d, stderr %q", code, errOut.String())
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestHelp(t *testing.T) {
	code, out, _ := runArgs(t, "help")
	if code != 0 || !strings.Contains(out, "Usage: orglane") {
		t.Fatalf("help: code %d, output %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"serve"},
		{"cli"},
		{"cli", "frobnicate"},
		{"server", "-bogus"},
		{"cli", "migrate", "extra"},
	} {
		code, _, errOut := runArgs(t, args...)
		if code != 2 || !strings.Contains(errOut, "Usage: orglane") {
			t.Errorf("%v: code %d, stderr %q; want 2 with usage", args, code, errOut)
		}
	}
}

func TestInvalidConfigIsReported(t *testing.T) {
	for _, args := range [][]string{{"server"}, {"cli", "migrate"}, {"cli", "healthcheck"}} {
		code, _, errOut := runArgs(t, args...)
		if code != 1 || !strings.Contains(errOut, "ORGLANE_DATABASE_URL") || !strings.Contains(errOut, "ORGLANE_DOMAIN_BASE") {
			t.Errorf("%v: code %d, stderr %q; want 1 with config errors", args, code, errOut)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	vars := map[string]string{
		"ORGLANE_DATABASE_URL":  "postgres://orglane@postgres/orglane",
		"ORGLANE_DOMAIN_BASE":   "acme.com",
		"ORGLANE_SERVER_LISTEN": srv.Listener.Addr().String(),
		// Required in production; the healthcheck itself never uses it.
		"ORGLANE_IDENTITY_MASTER_SECRET": "test-master-secret-0123456789abcdef",
	}
	check := func() (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(t.Context(), []string{"cli", "healthcheck"}, &out, &errOut, func(k string) (string, bool) {
			v, ok := vars[k]
			return v, ok
		})
		return code, out.String(), errOut.String()
	}

	if code, out, errOut := check(); code != 0 || out != "ready\n" {
		t.Errorf("ready server: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	status = http.StatusServiceUnavailable
	if code, _, errOut := check(); code != 1 || !strings.Contains(errOut, "503") {
		t.Errorf("unready server: code %d, stderr %q; want 1", code, errOut)
	}
	srv.Close()
	if code, _, errOut := check(); code != 1 || !strings.Contains(errOut, "healthcheck") {
		t.Errorf("stopped server: code %d, stderr %q; want 1", code, errOut)
	}
}

func TestLoopbackAddr(t *testing.T) {
	for listen, want := range map[string]string{
		":8080":          "127.0.0.1:8080",
		"0.0.0.0:8080":   "127.0.0.1:8080",
		"[::]:8080":      "[::1]:8080",
		"10.0.0.5:8080":  "10.0.0.5:8080",
		"localhost:8080": "localhost:8080",
	} {
		if got := loopbackAddr(listen); got != want {
			t.Errorf("loopbackAddr(%q) = %q, want %q", listen, got, want)
		}
	}
}
