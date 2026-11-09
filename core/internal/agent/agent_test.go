package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

const testSecret = "test-agent-secret-0123456789abcdef0123456789" //nolint:gosec // a test secret

// recordingRunner records Docker invocations and fails them: unit tests run
// without Docker.
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingRunner) run(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	return nil, []byte("no docker in unit tests"), errors.New("no docker")
}

func (r *recordingRunner) tail(ctx context.Context, _ int, args ...string) ([]byte, bool, []byte, error) {
	out, stderr, err := r.run(ctx, nil, args...)
	return out, false, stderr, err
}

func (r *recordingRunner) pipe(ctx context.Context, _ io.Reader, _ io.Writer, args ...string) ([]byte, error) {
	_, stderr, err := r.run(ctx, nil, args...)
	return stderr, err
}

func (r *recordingRunner) stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("no docker")
}

func (r *recordingRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func testConfig(t *testing.T) config.Agent {
	t.Helper()
	cfg := config.Default().Agent
	cfg.Secret = testSecret
	cfg.DataDir = t.TempDir()
	cfg.PeerUID = -1
	cfg.EdgeNetwork, cfg.ServicesNetwork = "orglane_edge", "orglane_services"
	return cfg
}

func testRegistry(t *testing.T) *packages.Registry {
	t.Helper()
	r, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newTestAgent(t *testing.T, now func() time.Time) (*Agent, *recordingRunner) {
	t.Helper()
	rr := &recordingRunner{}
	a, err := New(Options{Config: testConfig(t), Version: "1.2.3", Registry: testRegistry(t), Now: now, runner: rr})
	if err != nil {
		t.Fatal(err)
	}
	return a, rr
}

func testKey(t *testing.T) agentapi.Key {
	t.Helper()
	k, err := agentapi.DeriveKey(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testSpec(gen int64) *agentapi.Spec {
	return &agentapi.Spec{
		InstanceID: uuid.MustParse("0190f0f0-0000-7000-8000-000000000001"),
		Generation: gen,
		Package:    agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version},
		State:      agentapi.StateRunning,
		Hosts:      map[string]string{"workspace": "orglane.localhost", "web": "agenttest.orglane.localhost"},
		URLs:       agentapi.URLs{Workspace: "http://orglane.localhost", Issuer: "http://orglane.localhost"},
		Ingress:    agentapi.IngressLocal,
		Settings:   map[string]any{"http_port": 18080, "message": "hello"},
		Secrets:    map[string]string{"db_password": "c2VjcmV0LXBhc3N3b3Jk"}, //nolint:gosec // a test secret
	}
}

// signed builds a request signed with key at time now.
func signed(t *testing.T, key agentapi.Key, method, target string, body []byte, now time.Time) (*http.Request, []byte) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, bytes.NewReader(body))
	mac := agentapi.SignRequest(key, req, body, now)
	return req, mac
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func problemCode(t *testing.T, rec *httptest.ResponseRecorder) agentapi.Problem {
	t.Helper()
	var p agentapi.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem: %s", rec.Body.String())
	}
	return p
}

func TestRouteTable(t *testing.T) {
	want := []string{
		"GET /v1/health",
		"GET /v1/instances",
		"GET /v1/instances/{key}",
		"PUT /v1/instances/{key}",
		"POST /v1/instances/{key}/snapshots",
		"GET /v1/instances/{key}/snapshots",
		"POST /v1/instances/{key}/restore",
		"GET /v1/operations/{id}",
		"POST /v1/operations/{id}/confirm",
		"POST /v1/operations/{id}/abort",
		"GET /v1/instances/{key}/logs",
	}
	if got := Routes(); !slices.Equal(got, want) {
		t.Fatalf("Routes() = %v, want exactly %v", got, want)
	}
}

// TestNoCommandEndpoints sends authenticated requests to endpoints that would
// run something: none exists, and Docker is never invoked.
func TestNoCommandEndpoints(t *testing.T) {
	now := time.Now()
	a, rr := newTestAgent(t, func() time.Time { return now })
	h, key := a.Handler(), testKey(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/exec"},
		{http.MethodPost, "/v1/run"},
		{http.MethodPost, "/v1/compose"},
		{http.MethodPost, "/v1/instances/git"},
		{http.MethodPost, "/v1/instances/git/exec"},
		{http.MethodPut, "/v1/instances/git/compose"},
		{http.MethodPut, "/v1/instances/git/snapshots"},
		{http.MethodDelete, "/v1/instances/git/snapshots"},
		{http.MethodPost, "/v1/instances/git/snapshots/x/exec"},
		{http.MethodDelete, "/v1/instances/git"},
		{http.MethodPut, "/v1/images/busybox"},
		{http.MethodPost, "/v1/operations/" + uuid.NewString() + "/confirm"},
		{http.MethodGet, "/v2/health"},
	} {
		req, _ := signed(t, key, c.method, c.path, []byte(`{"command":["sh","-c","id"],"image":"alpine"}`), now)
		rec := serve(h, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 404 or 405", c.method, c.path, rec.Code)
		}
	}
	// The snapshot and restore endpoints take no command, image or path:
	// such fields are refused.
	for _, path := range []string{"/v1/instances/git/snapshots", "/v1/instances/git/restore"} {
		req, _ := signed(t, key, http.MethodPost, path, []byte(`{"reason":"manual","command":["sh","-c","id"],"image":"alpine"}`), now)
		if rec := serve(h, req); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s with a command = %d, want 400", path, rec.Code)
		}
	}
	if n := rr.count(); n != 0 {
		t.Fatalf("Docker was invoked %d times", n)
	}
}

func TestAuthentication(t *testing.T) {
	now := time.Now()
	clock := now
	a, _ := newTestAgent(t, func() time.Time { return clock })
	h, key := a.Handler(), testKey(t)
	wrongKey, err := agentapi.DeriveKey(strings.Repeat("w", 40))
	if err != nil {
		t.Fatal(err)
	}

	// A correctly signed request gets a signed answer.
	req, mac := signed(t, key, http.MethodGet, agentapi.PathHealth, nil, now)
	rec := serve(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed request: %d %s", rec.Code, rec.Body)
	}
	if !agentapi.VerifyResponse(key, mac, rec.Code, rec.Header(), rec.Body.Bytes()) {
		t.Fatal("the answer is not signed")
	}
	var health agentapi.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil || health.Version != "1.2.3" || health.Protocol != 1 {
		t.Fatalf("health = %+v, %v", health, err)
	}

	// Replaying it is refused.
	replay := httptest.NewRequestWithContext(t.Context(), http.MethodGet, agentapi.PathHealth, nil)
	replay.Header = req.Header.Clone()
	if rec := serve(h, replay); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay: %d", rec.Code)
	}

	body := []byte(`{}`)
	cases := map[string]func() *http.Request{
		"unsigned": func() *http.Request {
			return httptest.NewRequestWithContext(t.Context(), http.MethodGet, agentapi.PathHealth, nil)
		},
		"wrong key": func() *http.Request {
			r, _ := signed(t, wrongKey, http.MethodGet, agentapi.PathHealth, nil, now)
			return r
		},
		"tampered body": func() *http.Request {
			r, _ := signed(t, key, http.MethodPut, "/v1/instances/git", body, now)
			r.Body = io.NopCloser(strings.NewReader(`{"x":1}`))
			return r
		},
		"tampered path": func() *http.Request {
			r, _ := signed(t, key, http.MethodGet, "/v1/instances/git", nil, now)
			r.URL.Path = "/v1/instances/other"
			return r
		},
		"tampered query": func() *http.Request {
			r, _ := signed(t, key, http.MethodGet, "/v1/instances/git/logs?service=web&tail=10", nil, now)
			r.URL.RawQuery = "service=worker&tail=10"
			return r
		},
		"tampered method": func() *http.Request {
			r, _ := signed(t, key, http.MethodGet, "/v1/instances/git", nil, now)
			r.Method = http.MethodPut
			return r
		},
		"stale timestamp": func() *http.Request {
			r, _ := signed(t, key, http.MethodGet, agentapi.PathHealth, nil, now.Add(-61*time.Second))
			return r
		},
		"future timestamp": func() *http.Request {
			r, _ := signed(t, key, http.MethodGet, agentapi.PathHealth, nil, now.Add(61*time.Second))
			return r
		},
		"timestamp before agent start": func() *http.Request {
			r, _ := signed(t, key, http.MethodGet, agentapi.PathHealth, nil, now.Add(-30*time.Second))
			return r
		},
		"malformed nonce": func() *http.Request {
			r, _ := signed(t, key, http.MethodGet, agentapi.PathHealth, nil, now)
			r.Header.Set(agentapi.HeaderNonce, "short")
			return r
		},
		"other scheme": func() *http.Request {
			r, _ := signed(t, key, http.MethodGet, agentapi.PathHealth, nil, now)
			r.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(r.Header.Get("Authorization"), "Orglane-HMAC "))
			return r
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			rec := serve(h, mk())
			if rec.Code != http.StatusUnauthorized || problemCode(t, rec).Code != agentapi.CodeUnauthenticated {
				t.Fatalf("got %d %s, want 401 unauthenticated", rec.Code, rec.Body)
			}
			if rec.Header().Get(agentapi.HeaderSignature) != "" {
				t.Fatal("an answer to an unauthenticated request is signed")
			}
		})
	}

	// A request signed 50 s ago is fresh 50 s later; one signed after the
	// agent started and replayed within the nonce window is refused.
	clock = now.Add(50 * time.Second)
	req, _ = signed(t, key, http.MethodGet, agentapi.PathHealth, nil, now)
	if rec := serve(h, req); rec.Code != http.StatusOK {
		t.Fatalf("request within the skew: %d", rec.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	body := bytes.Repeat([]byte("a"), agentapi.MaxRequestBytes+1)
	req, _ := signed(t, testKey(t), http.MethodPut, "/v1/instances/git", body, now)
	if rec := serve(a.Handler(), req); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", rec.Code)
	}
}

func put(t *testing.T, a *Agent, now time.Time, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req, mac := signed(t, testKey(t), http.MethodPut, "/v1/instances/"+key, body, now)
	rec := serve(a.Handler(), req)
	if !agentapi.VerifyResponse(testKey(t), mac, rec.Code, rec.Header(), rec.Body.Bytes()) {
		t.Fatal("unsigned answer")
	}
	return rec
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPutSemantics(t *testing.T) {
	now := time.Now()
	a, rr := newTestAgent(t, func() time.Time { return now })

	rec := put(t, a, now, "git", mustJSON(t, testSpec(1)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first spec: %d %s", rec.Code, rec.Body)
	}
	var op agentapi.Operation
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil || op.State != agentapi.OpQueued || op.Kind != agentapi.KindApply || op.Generation != 1 {
		t.Fatalf("operation = %+v, %v", op, err)
	}
	dir := filepath.Join(a.instancesDir, "git")
	for name, perm := range map[string]os.FileMode{"": 0o700, instanceFile: 0o600, desiredFile: 0o600} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != perm {
			t.Errorf("%s: mode %v, %v; want %v", name, fi.Mode().Perm(), err, perm)
		}
	}

	// The same generation again: 200 with the same operation.
	rec = put(t, a, now, "git", mustJSON(t, testSpec(1)))
	var again agentapi.Operation
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &again) != nil || again.ID != op.ID {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}

	// The same generation with other content.
	changed := testSpec(1)
	changed.Settings["message"] = "other"
	if rec = put(t, a, now, "git", mustJSON(t, changed)); rec.Code != http.StatusConflict || problemCode(t, rec).Code != agentapi.CodeGenerationConflict {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body)
	}

	// A newer generation supersedes the queued operation.
	rec = put(t, a, now, "git", mustJSON(t, testSpec(3)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("generation 3: %d %s", rec.Code, rec.Body)
	}
	if old, _ := a.ops.get(op.ID); old.State != agentapi.OpFailed || old.ErrorCode != agentapi.CodeSuperseded {
		t.Fatalf("superseded operation = %+v", old)
	}

	// An older generation is stale.
	rec = put(t, a, now, "git", mustJSON(t, testSpec(2)))
	if p := problemCode(t, rec); rec.Code != http.StatusConflict || p.Code != agentapi.CodeStaleGeneration || p.AcceptedGeneration != 3 {
		t.Fatalf("stale: %d %s", rec.Code, rec.Body)
	}

	// Another instance ID for the same key.
	other := testSpec(4)
	other.InstanceID = uuid.MustParse("0190f0f0-0000-7000-8000-000000000002")
	if rec = put(t, a, now, "git", mustJSON(t, other)); rec.Code != http.StatusConflict || problemCode(t, rec).Code != agentapi.CodeInstanceMismatch {
		t.Fatalf("mismatch: %d %s", rec.Code, rec.Body)
	}

	// Unknown package version.
	unknown := testSpec(5)
	unknown.Package.Version = "9.9.9-1"
	if rec = put(t, a, now, "git", mustJSON(t, unknown)); rec.Code != http.StatusUnprocessableEntity || problemCode(t, rec).Code != agentapi.CodeUnknownPackageVersion {
		t.Fatalf("unknown version: %d %s", rec.Code, rec.Body)
	}
	traversal := testSpec(5)
	traversal.Package.ID = "../../../etc"
	if rec = put(t, a, now, "git", mustJSON(t, traversal)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("package traversal: %d %s", rec.Code, rec.Body)
	}

	// Invalid inputs name the fields, never the values.
	bad := testSpec(5)
	bad.Settings["message"] = "line\nbreak-SECRETVALUE"
	bad.Secrets["db_password"] = "it's$SECRETVALUE"
	rec = put(t, a, now, "git", mustJSON(t, bad))
	p := problemCode(t, rec)
	if rec.Code != http.StatusUnprocessableEntity || p.Code != agentapi.CodeInvalidInput ||
		!slices.Equal(p.Fields, []string{"secrets.db_password", "settings.message"}) || strings.Contains(rec.Body.String(), "SECRETVALUE") {
		t.Fatalf("invalid inputs: %d %s", rec.Code, rec.Body)
	}

	// Fields the spec does not have (an image, a compose file, a command)
	// are refused.
	for _, field := range []string{`"image":"alpine"`, `"compose":"services: {}"`, `"command":["id"]`, `"path":"/"`} {
		raw := mustJSON(t, testSpec(5))
		raw = append(raw[:len(raw)-1], []byte(","+field+"}")...)
		if rec = put(t, a, now, "git", raw); rec.Code != http.StatusBadRequest {
			t.Fatalf("spec with %s: %d %s", field, rec.Code, rec.Body)
		}
	}

	// Invalid keys never reach the file system.
	for _, key := range []string{"..", "Git", "a_b", "-git", "x%2F..%2F..", strings.Repeat("a", 25)} {
		if rec = put(t, a, now, key, mustJSON(t, testSpec(1))); rec.Code < 300 {
			t.Fatalf("key %q: %d", key, rec.Code)
		}
	}
	entries, err := os.ReadDir(a.instancesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "git" && e.Name() != ".agent" {
			t.Fatalf("unexpected entry %s in the instances directory", e.Name())
		}
	}
	if n := rr.count(); n != 0 {
		t.Fatalf("accepting specs invoked Docker %d times", n)
	}
}

func TestPutVersionChange(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	if rec := put(t, a, now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	running := func(version string) {
		t.Helper()
		s := testSpec(1)
		s.Package.Version = version
		if err := writeSpec(filepath.Join(a.instancesDir, "git", currentFile), s); err != nil {
			t.Fatal(err)
		}
	}
	versioned := func(gen int64, version string) *agentapi.Spec {
		s := testSpec(gen)
		s.Package.Version = version
		if version != agenttest.Version {
			s.Settings["fault"] = "none"
		}
		return s
	}
	running(agenttest.Version)

	// 1.0.0-1 -> 3.0.0-1 skips the required stop 2.0.0-1.
	rec := put(t, a, now, "git", mustJSON(t, versioned(2, agenttest.Version3)))
	if p := problemCode(t, rec); rec.Code != http.StatusUnprocessableEntity || p.Code != agentapi.CodeUpgradePathBlocked || p.RequiredVersion != agenttest.Version2 {
		t.Fatalf("skipping a stop: %d %s", rec.Code, rec.Body)
	}
	// An upgrade needs the state running.
	stopped := versioned(2, agenttest.Version2)
	stopped.State = agentapi.StateStopped
	if rec = put(t, a, now, "git", mustJSON(t, stopped)); rec.Code != http.StatusUnprocessableEntity || problemCode(t, rec).Code != agentapi.CodeInvalidInput {
		t.Fatalf("stopped upgrade: %d %s", rec.Code, rec.Body)
	}
	// 1.0.0-1 -> 2.0.0-1 is an upgrade.
	rec = put(t, a, now, "git", mustJSON(t, versioned(2, agenttest.Version2)))
	var op agentapi.Operation
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &op) != nil || op.Kind != agentapi.KindUpgrade || op.Generation != 2 {
		t.Fatalf("upgrade: %d %s", rec.Code, rec.Body)
	}

	// A downgrade is refused without a stop to name.
	running(agenttest.Version2)
	rec = put(t, a, now, "git", mustJSON(t, versioned(3, agenttest.Version)))
	if p := problemCode(t, rec); rec.Code != http.StatusUnprocessableEntity || p.Code != agentapi.CodeUpgradePathBlocked || p.RequiredVersion != "" {
		t.Fatalf("downgrade: %d %s", rec.Code, rec.Body)
	}
	// The running version is a plain apply (the revert after a rollback).
	rec = put(t, a, now, "git", mustJSON(t, versioned(3, agenttest.Version2)))
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &op) != nil || op.Kind != agentapi.KindApply {
		t.Fatalf("revert: %d %s", rec.Code, rec.Body)
	}
	// Removal is accepted for any version.
	remove := testSpec(4)
	remove.State, remove.Purge = agentapi.StateAbsent, true
	if rec := put(t, a, now, "git", mustJSON(t, remove)); rec.Code != http.StatusAccepted {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
}

func TestStatusAndOperation(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	key := testKey(t)
	get := func(target string) *httptest.ResponseRecorder {
		req, _ := signed(t, key, http.MethodGet, target, nil, now)
		return serve(a.Handler(), req)
	}
	if rec := get("/v1/instances/git"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown instance: %d", rec.Code)
	}
	rec := put(t, a, now, "git", mustJSON(t, testSpec(1)))
	var op agentapi.Operation
	_ = json.Unmarshal(rec.Body.Bytes(), &op)

	rec = get("/v1/instances/git")
	var st agentapi.InstanceStatus
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	if st.Phase != agentapi.PhasePending || st.AcceptedGeneration != 1 || st.ObservedGeneration != 0 || st.Operation == nil || st.Operation.ID != op.ID {
		t.Fatalf("status = %+v", st)
	}
	if strings.Contains(rec.Body.String(), "c2VjcmV0LXBhc3N3b3Jk") {
		t.Fatal("status contains a secret")
	}
	rec = get("/v1/instances")
	var list agentapi.InstanceList
	if json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Instances) != 1 || list.Instances[0].Key != "git" {
		t.Fatalf("list: %s", rec.Body)
	}
	if rec = get("/v1/operations/" + op.ID.String()); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), op.ID.String()) {
		t.Fatalf("operation: %d %s", rec.Code, rec.Body)
	}
	if rec = get("/v1/operations/" + uuid.NewString()); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown operation: %d", rec.Code)
	}
	if rec = get("/v1/instances/git/logs?service=web"); rec.Code != http.StatusNotFound {
		t.Fatalf("logs before install: %d", rec.Code)
	}
}

// TestRecoverOperations checks that a restarted agent runs a pending
// generation again and fails other unfinished operations as interrupted.
func TestRecoverOperations(t *testing.T) {
	now := time.Now()
	cfg := testConfig(t)
	newAgent := func() *Agent {
		a, err := New(Options{Config: cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := newAgent()
	rec := put(t, a, now, "git", mustJSON(t, testSpec(1)))
	var pending agentapi.Operation
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)
	if _, err := a.ops.update(pending.ID, func(o *agentapi.Operation) { o.State, o.Phase = agentapi.OpRunning, agentapi.PhaseStarting }); err != nil {
		t.Fatal(err)
	}
	orphan, err := a.ops.create("other", agentapi.KindApply, 7, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}

	b := newAgent()
	if op, _ := b.ops.get(pending.ID); op.State != agentapi.OpQueued {
		t.Fatalf("pending operation after restart = %+v", op)
	}
	if op, _ := b.ops.get(orphan.ID); op.State != agentapi.OpFailed || op.ErrorCode != agentapi.CodeInterrupted {
		t.Fatalf("orphan operation after restart = %+v", op)
	}
	if b.bootID == a.bootID {
		t.Fatal("the boot ID did not change")
	}
}

func TestPeerUIDCheck(t *testing.T) {
	dir, err := os.MkdirTemp("", "agent-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	ln, err := listen(t.Context(), path, os.Getuid(), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode %v, %v", fi.Mode().Perm(), err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o750 {
		t.Fatalf("socket directory mode %v, %v", fi.Mode().Perm(), err)
	}
	pl, ok := ln.(*peerListener)
	if !ok {
		t.Fatal("no peer check")
	}
	echo := func(allowed []int) error {
		pl.allowed = allowed
		accepted := make(chan net.Conn, 1)
		go func() {
			c, err := pl.Accept()
			if err == nil {
				accepted <- c
			}
		}()
		c, err := (&net.Dialer{}).DialContext(t.Context(), "unix", path)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		select {
		case s := <-accepted:
			_ = s.Close()
			return nil
		case <-time.After(500 * time.Millisecond):
			// Refused: the listener closed the connection.
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			_, err := c.Read(make([]byte, 1))
			return err
		}
	}
	if err := echo([]int{os.Getuid()}); err != nil {
		t.Fatalf("own uid refused: %v", err)
	}
	if err := echo([]int{os.Getuid() + 1}); err == nil {
		t.Fatal("a connection from another uid was accepted")
	}
	_ = ln.Close()

	// A stale socket is replaced; another file at the path is refused.
	if ln, err = listen(t.Context(), path, -1, testLogger(t)); err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	_ = ln.Close()
	other := filepath.Join(dir, "file")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listen(t.Context(), other, -1, testLogger(t)); err == nil {
		t.Fatal("listen replaced a regular file")
	}

	// A directory that holds anything else is not used for the socket: the
	// agent would change its mode and group.
	if _, err := listen(t.Context(), filepath.Join(dir, "agent2.sock"), -1, testLogger(t)); err == nil {
		t.Fatal("listen used a shared directory")
	}
}

// testLogger discards the agent's log; ORGLANE_TEST_AGENT_LOG=1 writes it
// to the test output.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	if os.Getenv("ORGLANE_TEST_AGENT_LOG") != "" {
		return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

func TestRedact(t *testing.T) {
	got := string(redact([]byte("pw=abcdef and abc"), []string{"abc", "abcdef", ""}))
	if got != "pw=[redacted] and [redacted]" {
		t.Fatalf("redact = %q", got)
	}
}

func TestDockerEnvExcludesSecret(t *testing.T) {
	t.Setenv("ORGLANE_AGENT_SECRET", testSecret)
	t.Setenv("COMPOSE_FILE", "/etc/evil.yaml")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	d := newDockerCLI("docker")
	for _, e := range d.env {
		if strings.HasPrefix(e, "ORGLANE_") || strings.HasPrefix(e, "COMPOSE_") {
			t.Fatalf("docker env has %s", e)
		}
	}
	if !slices.Contains(d.env, "DOCKER_HOST=unix:///var/run/docker.sock") {
		t.Fatal("DOCKER_HOST is not passed")
	}
}

func TestComposePSFormats(t *testing.T) {
	for _, out := range []string{
		`[{"Service":"web","State":"running","Health":"healthy"},{"Service":"worker","State":"exited","Health":""}]`,
		"{\"Service\":\"worker\",\"State\":\"exited\",\"Health\":\"\"}\n{\"Service\":\"web\",\"State\":\"running\",\"Health\":\"healthy\"}\n",
	} {
		c := compose{r: fixedRunner(out)}
		got, err := c.ps(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want := []agentapi.ServiceStatus{{Name: "web", State: "running", Health: "healthy"}, {Name: "worker", State: "exited"}}
		if !slices.Equal(got, want) {
			t.Fatalf("ps(%s) = %v", out, got)
		}
		if servicesHealthy([]string{"web", "worker"}, got) || !servicesHealthy([]string{"web"}, got) {
			t.Fatal("servicesHealthy")
		}
	}
}

type fixedRunner string

func (f fixedRunner) run(context.Context, []byte, ...string) ([]byte, []byte, error) {
	return []byte(f), nil, nil
}

func (f fixedRunner) tail(context.Context, int, ...string) ([]byte, bool, []byte, error) {
	return []byte(f), false, nil, nil
}

func (f fixedRunner) pipe(_ context.Context, _ io.Reader, w io.Writer, _ ...string) ([]byte, error) {
	if w != nil {
		_, _ = io.WriteString(w, string(f))
	}
	return nil, nil
}

func (f fixedRunner) stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return io.NopCloser(strings.NewReader(string(f))), func() error { return nil }, nil
}

func TestEvents(t *testing.T) {
	stream := strings.Join([]string{
		`{"Type":"container","Action":"die","Actor":{"Attributes":{"com.docker.compose.project":"orglane-git"}}}`,
		`{"Type":"container","Action":"start","Actor":{"Attributes":{"com.docker.compose.project":"orglane-git"}}}`,
		`{"Type":"container","Action":"health_status: unhealthy","Actor":{"Attributes":{"com.docker.compose.project":"orglane-wiki"}}}`,
		`not json`,
		`{"Type":"container","Action":"destroy","Actor":{"Attributes":{"com.docker.compose.project":"orglane"}}}`,
	}, "\n")
	var got []string
	if err := events(context.Background(), fixedRunner(stream), func(p string) { got = append(got, p) }); err != nil {
		t.Fatal(err)
	}
	if want := []string{"orglane-git", "orglane-wiki", "orglane"}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for in, want := range map[string]string{"orglane-git": "git", "orglane": "", "orglane-": "", "other-git": ""} {
		if k, _ := cutPrefix(in, "orglane-"); k != want {
			t.Errorf("cutPrefix(%q) = %q", in, k)
		}
	}
}

func TestQueryParsing(t *testing.T) {
	// The log query is validated before Docker sees it.
	now := time.Now()
	a, rr := newTestAgent(t, func() time.Time { return now })
	_ = put(t, a, now, "git", mustJSON(t, testSpec(1)))
	if err := writeSpec(filepath.Join(a.instancesDir, "git", currentFile), testSpec(1)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"service=postgres", "service=web&tail=0", "service=web&tail=1001", "service=web&since=yesterday", "service=web;id", "service=web&since=-5m"} {
		req, _ := signed(t, testKey(t), http.MethodGet, "/v1/instances/git/logs?"+q, nil, now)
		if rec := serve(a.Handler(), req); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("logs?%s = %d, want 422", q, rec.Code)
		}
	}
	if n := rr.count(); n != 0 {
		t.Fatalf("Docker was invoked %d times", n)
	}
	req, _ := signed(t, testKey(t), http.MethodGet, "/v1/instances/git/logs?service=web&tail=5&since=10m", nil, now)
	_ = serve(a.Handler(), req)
	want := []string{"logs", "--no-color", "--timestamps", "--tail", "5", "--since", "10m0s", "web"}
	if rr.count() != 1 || !slices.Equal(rr.calls[0][len(rr.calls[0])-len(want):], want) {
		t.Fatalf("logs call = %v", rr.calls)
	}
}
