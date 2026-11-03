//go:build integration

// Package integration runs the orglane binary against a real PostgreSQL in a
// container. Run with `make test` (when Docker is available) or
// `make test-integration`.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/tests/pgtest"
)

func TestMain(m *testing.M) {
	code := pgtest.Run(m)
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

var (
	buildOnce sync.Once
	binDir    string
	binPath   string
	buildErr  error
)

// binary builds the orglane binary once per test run, without the race
// detector so memory numbers are representative.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "orglane-integration-")
		if buildErr != nil {
			return
		}
		binPath = filepath.Join(binDir, "orglane")
		args := []string{"build", "-trimpath", "-ldflags", "-X main.version=integration-test",
			"-o", binPath, "github.com/orglane/orglane/core/cmd/orglane"}
		out, err := exec.CommandContext(context.Background(), "go", args...).CombinedOutput() //nolint:gosec // builds this repository's binary
		if err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// syncBuffer collects process output from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type serverProc struct {
	cmd     *exec.Cmd
	baseURL string
	logs    *syncBuffer
	done    chan struct{} // closed when the process has exited
	waitErr error         // valid after done is closed
}

func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// testMasterSecret is the instance master secret of the test servers.
const testMasterSecret = "integration-test-master-secret-0123456789" //nolint:gosec // test-only value

func serverEnv(dbURL, addr string) []string {
	return append(os.Environ(),
		"ORGLANE_DATABASE_URL="+dbURL,
		"ORGLANE_DOMAIN_BASE=acme.test",
		"ORGLANE_DOMAIN_WORKSPACE_HOST=work.acme.test",
		"ORGLANE_SERVER_LISTEN="+addr,
		"ORGLANE_LOG_FORMAT=json",
		"ORGLANE_IDENTITY_MASTER_SECRET="+testMasterSecret,
	)
}

// startServer runs `orglane server` and waits until /readyz answers 200.
// extraEnv adds or overrides environment variables.
func startServer(t *testing.T, dbURL string, extraEnv ...string) *serverProc {
	t.Helper()
	return startServerArgs(t, dbURL, nil, extraEnv...)
}

// startServerArgs is startServer with extra `orglane server` arguments.
func startServerArgs(t *testing.T, dbURL string, args []string, extraEnv ...string) *serverProc {
	t.Helper()
	addr := freeAddr(t)
	p := &serverProc{
		cmd:     exec.CommandContext(context.Background(), binary(t), append([]string{"server"}, args...)...), //nolint:gosec // the binary built by binary()
		baseURL: "http://" + addr,
		logs:    &syncBuffer{},
		done:    make(chan struct{}),
	}
	p.cmd.Env = append(serverEnv(dbURL, addr), extraEnv...)
	p.cmd.Stdout, p.cmd.Stderr = p.logs, p.logs
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
		if t.Failed() {
			t.Logf("server logs:\n%s", p.logs)
		}
	})

	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-p.done:
			t.Fatalf("server exited before becoming ready: %v\n%s", p.waitErr, p.logs)
		case <-deadline:
			t.Fatalf("server not ready within 60s\n%s", p.logs)
		case <-time.After(200 * time.Millisecond):
		}
		if code, _, _ := tryGet(t, p.baseURL+"/readyz"); code == http.StatusOK {
			return p
		}
	}
}

// stop sends SIGTERM and returns the exit error.
func (p *serverProc) stop(t *testing.T) error {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		return p.waitErr
	case <-time.After(30 * time.Second):
		t.Fatal("server did not stop within 30s of SIGTERM")
		return nil
	}
}

// tryGet performs a GET request; code is 0 if the request failed.
func tryGet(t *testing.T, url string) (code int, header http.Header, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, nil
	}
	defer func() { _ = res.Body.Close() }()
	body, err = io.ReadAll(res.Body)
	if err != nil {
		return 0, nil, nil
	}
	return res.StatusCode, res.Header, body
}

func get(t *testing.T, url string) (code int, header http.Header, body []byte) {
	t.Helper()
	code, header, body = tryGet(t, url)
	if code == 0 {
		t.Fatalf("GET %s failed", url)
	}
	return code, header, body
}

func TestServerLifecycle(t *testing.T) {
	dbURL := pgtest.NewDatabase(t)
	p := startServer(t, dbURL) // a fresh database: the server applies migrations itself

	if code, _, body := get(t, p.baseURL+"/healthz"); code != 200 || !strings.Contains(string(body), `"ok"`) {
		t.Errorf("/healthz = %d %s", code, body)
	}
	if code, _, body := get(t, p.baseURL+"/readyz"); code != 200 || !strings.Contains(string(body), `"migrations"`) {
		t.Errorf("/readyz = %d %s", code, body)
	}
	var version struct{ Version string }
	code, _, body := get(t, p.baseURL+"/api/v1/version")
	if err := json.Unmarshal(body, &version); code != 200 || err != nil || version.Version != "integration-test" {
		t.Errorf("/api/v1/version = %d %s", code, body)
	}
	// Non-public operations need credentials.
	code, header, body := get(t, p.baseURL+"/api/v1/instance")
	if code != 401 || header.Get("Content-Type") != "application/problem+json" {
		t.Errorf("/api/v1/instance = %d %s %s, want 401 problem", code, header.Get("Content-Type"), body)
	}
	if code, _, _ := get(t, p.baseURL+"/api/v1/nope"); code != 404 {
		t.Errorf("/api/v1/nope = %d, want 404", code)
	}

	// Readiness reflects the migration state.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "DELETE FROM goose_db_version WHERE version_id = 1"); err != nil {
		t.Fatal(err)
	}
	if code, _, body := get(t, p.baseURL+"/readyz"); code != 503 || strings.Contains(string(body), "goose") {
		t.Errorf("/readyz with a pending migration = %d %s, want 503 without details", code, body)
	}

	if err := p.stop(t); err != nil {
		t.Fatalf("exit after SIGTERM: %v\n%s", err, p.logs)
	}
	logs := p.logs.String()
	for _, want := range []string{`"msg":"applied migration"`, `"msg":"server listening"`, `"msg":"shutdown complete"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s", want)
		}
	}
	if strings.Contains(logs, "password=") || strings.Contains(logs, "orglane:orglane@") {
		t.Error("logs leak database credentials")
	}
}

// TestSingleConnectionPool is a regression test: with the smallest accepted
// pool (database.max_conns: 1) migrations must not deadlock on start.
func TestSingleConnectionPool(t *testing.T) {
	const oneConn = "ORGLANE_DATABASE_MAX_CONNS=1"
	dbURL := pgtest.NewDatabase(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary(t), "cli", "migrate") //nolint:gosec // the binary built by binary()
	cmd.Env = append(serverEnv(dbURL, "127.0.0.1:0"), oneConn)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "migrations are up to date") {
		t.Fatalf("cli migrate with max_conns=1: %v (context: %v)\n%s", err, ctx.Err(), out)
	}

	// Already migrated by the CLI: the server still migrates (a no-op) on start.
	p := startServer(t, dbURL, oneConn)
	for range 3 {
		if code, _, body := get(t, p.baseURL+"/readyz"); code != 200 {
			t.Fatalf("/readyz with max_conns=1 = %d %s", code, body)
		}
	}
	if err := p.stop(t); err != nil {
		t.Fatalf("exit after SIGTERM: %v\n%s", err, p.logs)
	}
}

func TestCLIMigrate(t *testing.T) {
	dbURL := pgtest.NewDatabase(t)
	for range 2 { // idempotent
		cmd := exec.CommandContext(t.Context(), binary(t), "cli", "migrate") //nolint:gosec // the binary built by binary()
		cmd.Env = serverEnv(dbURL, "127.0.0.1:0")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "migrations are up to date") {
			t.Fatalf("cli migrate: %v\n%s", err, out)
		}
	}

	// The built-in roles are seeded once, with one audit entry per change.
	conn, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var roles, created int
	if err := conn.QueryRow(t.Context(), `SELECT
		(SELECT count(*) FROM roles WHERE builtin AND key IN ('owner', 'admin', 'member')),
		(SELECT count(*) FROM audit_entries WHERE action = 'role.created' AND actor_kind = 'system')`).Scan(&roles, &created); err != nil {
		t.Fatal(err)
	}
	if roles != 3 || created != 3 {
		t.Fatalf("after two migrate runs: %d built-in roles, %d role.created entries; want 3 and 3", roles, created)
	}
}

func TestServerRequiresMasterSecretInProduction(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), binary(t), "server") //nolint:gosec // the binary built by binary()
	cmd.Env = append(serverEnv("postgres://localhost/x", "127.0.0.1:0"), "ORGLANE_IDENTITY_MASTER_SECRET=")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(out), "identity.master_secret") {
		t.Fatalf("server without a master secret: %v\n%s", err, out)
	}
}

func TestServerRejectsInvalidConfig(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), binary(t), "server") //nolint:gosec // the binary built by binary()
	cmd.Env = append(os.Environ(), "ORGLANE_DATABASE_URL=postgres://localhost/x", "ORGLANE_DOMAIN_SERVICE_HOST_TEMPLATE=git")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 ||
		!strings.Contains(string(out), "domain.base") || !strings.Contains(string(out), "domain.service_host_template") {
		t.Fatalf("server with invalid config: %v\n%s", err, out)
	}
}
