package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

// testEnv is the core's Env as the adapter sees it, with fixed credentials.
type testEnv struct {
	creds map[string][]byte
}

func (e testEnv) Credential(_ context.Context, name string) ([]byte, error) {
	if v, ok := e.creds[name]; ok {
		return v, nil
	}
	return nil, provider.ErrNotFound
}

func (e testEnv) Credentials(ctx context.Context, name string) ([][]byte, error) {
	v, err := e.Credential(ctx, name)
	return [][]byte{v}, err
}

func (testEnv) HTTPClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }
func (testEnv) Logger() *slog.Logger     { return slog.New(slog.DiscardHandler) }
func (testEnv) Now() time.Time           { return time.Now() }
func (testEnv) WorkspaceURL() *url.URL   { return &url.URL{Scheme: "https", Host: "work.acme.test"} }
func (testEnv) IssuerURL() *url.URL      { return &url.URL{Scheme: "https", Host: "work.acme.test"} }
func (testEnv) WebhookURL(uuid.UUID) *url.URL {
	return &url.URL{Scheme: "https", Host: "work.acme.test"}
}

const testPassword = "YWRtaW4tcGFzc3dvcmQ" //nolint:gosec // a test secret

func env() testEnv {
	return testEnv{creds: map[string][]byte{CredentialAdminPassword: []byte(testPassword)}}
}

// fakeForgejo is an httptest stand-in for the Forgejo API endpoints the
// adapter uses (unit tests only; integration tests run the real Forgejo).
type fakeForgejo struct {
	mu     sync.Mutex
	orgs   map[string]bool
	tokens []accessToken
	nextID int64
	health int
	admin  bool
	// fail answers the next requests of "METHOD path" with these statuses
	// in turn (0: answer normally).
	fail     map[string][]int
	requests []string
	values   map[string]int64 // token value -> token ID
	// beforeMint runs before a token is minted (a concurrent attempt).
	beforeMint func(f *fakeForgejo)
}

// mintLocked adds a token; f.mu is held.
func (f *fakeForgejo) mintLocked(name string) accessToken {
	f.nextID++
	t := accessToken{ID: f.nextID, Name: name, SHA1: fmt.Sprintf("token-%d", f.nextID)}
	f.tokens = append(f.tokens, accessToken{ID: t.ID, Name: t.Name})
	f.values[t.SHA1] = t.ID
	return t
}

func (f *fakeForgejo) hasToken(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tokens {
		if t.ID == id {
			return true
		}
	}
	return false
}

func newFake(t *testing.T) (*fakeForgejo, provider.Instance) {
	t.Helper()
	f := &fakeForgejo{orgs: map[string]bool{}, health: http.StatusOK, admin: true, fail: map[string][]int{}, values: map[string]int64{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return f, provider.Instance{
		ID: uuid.New(), Key: "git", Provider: ID, Management: provider.Managed, Upstream: u,
		Settings: map[string]any{SettingOrganization: "acme", SettingSSHPort: int64(2222)},
	}
}

func (f *fakeForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	f.requests = append(f.requests, key)
	if codes := f.fail[key]; len(codes) > 0 {
		f.fail[key] = codes[1:]
		if codes[0] != 0 {
			w.WriteHeader(codes[0])
			return
		}
	}
	if r.URL.Path == "/api/healthz" {
		w.WriteHeader(f.health)
		status := "pass"
		if f.health != http.StatusOK {
			status = "fail"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
		return
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token "); ok {
		if _, valid := f.values[tok]; !valid || key != "GET /api/v1/user" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"login": AdminUsername, "is_admin": f.admin})
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != AdminUsername || p != testPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	tokens := "/api/v1/users/" + AdminUsername + "/tokens"
	switch {
	case key == "GET /api/v1/user":
		_ = json.NewEncoder(w).Encode(map[string]any{"login": AdminUsername, "is_admin": f.admin})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/orgs/"):
		if !f.orgs[strings.TrimPrefix(r.URL.Path, "/api/v1/orgs/")] {
			w.WriteHeader(http.StatusNotFound)
		}
	case key == "POST /api/v1/orgs":
		var in struct{ Username, Visibility string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if f.orgs[in.Username] {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		f.orgs[in.Username] = true
		w.WriteHeader(http.StatusCreated)
	case key == "GET "+tokens:
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		from, to := min((page-1)*limit, len(f.tokens)), min(page*limit, len(f.tokens))
		_ = json.NewEncoder(w).Encode(f.tokens[from:to])
	case key == "POST "+tokens:
		var in struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if f.beforeMint != nil {
			f.beforeMint(f)
			f.beforeMint = nil
		}
		for _, t := range f.tokens {
			if t.Name == in.Name {
				// Forgejo 15 answers 400 when the name is taken.
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		t := f.mintLocked(in.Name)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(t)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, tokens+"/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, tokens+"/"), 10, 64)
		for i, t := range f.tokens {
			if t.ID == id {
				f.tokens = append(f.tokens[:i], f.tokens[i+1:]...)
				for v, vid := range f.values {
					if vid == id {
						delete(f.values, v)
					}
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func code(err error) string {
	c, _, _ := provider.Classify(err)
	return c
}

func TestManifest(t *testing.T) {
	m := New().Manifest()
	if m.ID != ID || m.Management != provider.Managed || m.Package != "forgejo" || m.DefaultKey != "git" {
		t.Errorf("manifest = %+v", m)
	}
	seen := map[provider.Feature]bool{}
	for _, f := range m.Features {
		seen[f.Feature] = true
	}
	for _, f := range provider.Features {
		if !seen[f] {
			t.Errorf("feature %s is not declared", f)
		}
	}
	inputs, err := New().Inputs(t.Context(), env(), provider.Instance{Settings: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range inputs.Secrets {
		found := false
		for _, c := range m.Credentials {
			found = found || (c.Name == name && c.Agent)
		}
		if !found {
			t.Errorf("secret %s is not an agent credential of the manifest", name)
		}
	}
}

func TestInputs(t *testing.T) {
	a := New()
	in, err := a.Inputs(t.Context(), env(), provider.Instance{Settings: map[string]any{SettingSSHPort: int64(2422)}})
	if err != nil || in.Settings[SettingSSHPort] != int64(2422) {
		t.Fatalf("inputs = %+v, %v", in, err)
	}
	in, err = a.Inputs(t.Context(), env(), provider.Instance{Settings: map[string]any{}})
	if err != nil || in.Settings[SettingSSHPort] != int64(DefaultSSHPort) {
		t.Fatalf("default inputs = %+v, %v", in, err)
	}
	if _, err := a.Inputs(t.Context(), env(), provider.Instance{Settings: map[string]any{SettingSSHPort: "22"}}); code(err) != CodeInvalidSettings {
		t.Fatalf("string port: %v", err)
	}
}

func TestSetup(t *testing.T) {
	a := New()
	f, inst := newFake(t)
	if err := a.Setup(t.Context(), env(), inst); err != nil {
		t.Fatal(err)
	}
	if !f.orgs["acme"] {
		t.Fatal("the organization was not created")
	}
	// Idempotent.
	if err := a.Setup(t.Context(), env(), inst); err != nil {
		t.Fatal(err)
	}

	// The organization appears between the check and the creation.
	// The first check misses it, the creation conflicts, the second check
	// finds it.
	f2, inst2 := newFake(t)
	f2.orgs["acme"] = true
	f2.fail["GET /api/v1/orgs/acme"] = []int{http.StatusNotFound}
	if err := a.Setup(t.Context(), env(), inst2); err != nil {
		t.Fatalf("race: %v", err)
	}

	cases := map[string]struct {
		prepare func(*fakeForgejo, *provider.Instance, *testEnv)
		code    string
		retry   bool
	}{
		"no organization setting": {func(_ *fakeForgejo, i *provider.Instance, _ *testEnv) { delete(i.Settings, SettingOrganization) }, CodeInvalidSettings, false},
		"bad organization":        {func(_ *fakeForgejo, i *provider.Instance, _ *testEnv) { i.Settings[SettingOrganization] = "-x" }, CodeInvalidSettings, false},
		"reserved organization":   {func(_ *fakeForgejo, i *provider.Instance, _ *testEnv) { i.Settings[SettingOrganization] = "Explore" }, CodeInvalidSettings, false},
		// Retryable: the configure hook may still be setting the password.
		"wrong password": {func(_ *fakeForgejo, _ *provider.Instance, e *testEnv) {
			e.creds[CredentialAdminPassword] = []byte("wrong")
		}, CodeAdminAuth, true},
		"not an admin":    {func(f *fakeForgejo, _ *provider.Instance, _ *testEnv) { f.admin = false }, CodeAdminAuth, false},
		"server error":    {func(f *fakeForgejo, _ *provider.Instance, _ *testEnv) { f.fail["GET /api/v1/user"] = []int{502} }, CodeUnavailable, true},
		"rate limited":    {func(f *fakeForgejo, _ *provider.Instance, _ *testEnv) { f.fail["GET /api/v1/orgs/acme"] = []int{429} }, CodeRateLimited, true},
		"rejected create": {func(f *fakeForgejo, _ *provider.Instance, _ *testEnv) { f.fail["POST /api/v1/orgs"] = []int{400} }, CodeRejected, false},
		"no upstream":     {func(_ *fakeForgejo, i *provider.Instance, _ *testEnv) { i.Upstream = nil }, CodeNoUpstream, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, inst := newFake(t)
			e := env()
			c.prepare(f, &inst, &e)
			err := a.Setup(t.Context(), e, inst)
			got, retry, _ := provider.Classify(err)
			if err == nil || got != c.code || retry != c.retry {
				t.Fatalf("error = %v (code %s, retryable %v), want %s, %v", err, got, retry, c.code, c.retry)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Fatal("the error contains the password")
			}
		})
	}
}

func TestCheckHealth(t *testing.T) {
	a := New()
	f, inst := newFake(t)
	if err := a.CheckHealth(t.Context(), env(), inst); err != nil {
		t.Fatal(err)
	}
	f.health = http.StatusServiceUnavailable
	if err := a.CheckHealth(t.Context(), env(), inst); code(err) != CodeUnhealthy {
		t.Fatalf("unhealthy: %v", err)
	}
	inst.Upstream = &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	if err := a.CheckHealth(t.Context(), env(), inst); code(err) != CodeUnavailable {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestApplyCredential(t *testing.T) {
	a := New()
	f, inst := newFake(t)

	r, err := a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 1, nil)
	if err != nil || r.Outcome != provider.RotationAcknowledged || string(r.Value) != "token-1" {
		t.Fatalf("mint = %+v, %v", r, err)
	}

	// An earlier attempt for version 2 committed but its answer was lost:
	// the token of that version is deleted and minted again. Version 1
	// stays valid (it is the active version during the overlap).
	f.tokens = append(f.tokens, accessToken{ID: 41, Name: TokenName(2)})
	r, err = a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 2, nil)
	if err != nil || r.Outcome != provider.RotationAcknowledged {
		t.Fatalf("re-mint = %+v, %v", r, err)
	}
	names := map[string]int{}
	for _, tok := range f.tokens {
		names[tok.Name]++
		if tok.ID == 41 {
			t.Error("the lost token of version 2 was not deleted")
		}
	}
	if names[TokenName(1)] != 1 || names[TokenName(2)] != 1 {
		t.Errorf("tokens = %v", names)
	}

	// The read-back pages through the token list.
	f.tokens = nil
	for i := range tokenPageSize + 3 {
		f.tokens = append(f.tokens, accessToken{ID: int64(100 + i), Name: fmt.Sprintf("other-%d", i)})
	}
	f.tokens = append(f.tokens, accessToken{ID: 999, Name: TokenName(3)})
	if r, err := a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 3, nil); err != nil || r.Outcome != provider.RotationAcknowledged {
		t.Fatalf("paged = %+v, %v", r, err)
	}
	for _, tok := range f.tokens {
		if tok.ID == 999 {
			t.Error("the lost token on the second page was not deleted")
		}
	}

	tokens := "/api/v1/users/" + AdminUsername + "/tokens"
	cases := map[string]struct {
		fail    map[string][]int
		outcome provider.RotationOutcome
	}{
		// A refusal backed by a read-back without the token is proof.
		"unauthorized mint": {map[string][]int{"POST " + tokens: {http.StatusUnauthorized}}, provider.RotationRejected},
		"invalid request":   {map[string][]int{"POST " + tokens: {http.StatusBadRequest}}, provider.RotationRejected},
		"server error":      {map[string][]int{"POST " + tokens: {http.StatusInternalServerError}}, provider.RotationUnknown},
		"rate limited mint": {map[string][]int{"POST " + tokens: {http.StatusTooManyRequests}}, provider.RotationUnknown},
		// Read-backs and cleanups that fail prove nothing, whatever the
		// status: an earlier attempt may have minted the token.
		"list unavailable":  {map[string][]int{"GET " + tokens: {http.StatusBadGateway}}, provider.RotationUnknown},
		"list rate limited": {map[string][]int{"GET " + tokens: {http.StatusTooManyRequests}}, provider.RotationUnknown},
		"list unauthorized": {map[string][]int{"GET " + tokens: {http.StatusForbidden}}, provider.RotationUnknown},
		"list not found":    {map[string][]int{"GET " + tokens: {http.StatusNotFound}}, provider.RotationUnknown},
		"refused, read-back fails": {map[string][]int{
			"POST " + tokens: {http.StatusBadRequest}, "GET " + tokens: {0, http.StatusUnauthorized},
		}, provider.RotationUnknown},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, inst := newFake(t)
			f.fail = c.fail
			r, err := a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 1, nil)
			if r.Outcome != c.outcome {
				t.Fatalf("outcome = %s (%v), want %s", r.Outcome, err, c.outcome)
			}
			if c.outcome == provider.RotationUnknown && err == nil {
				t.Fatal("an unknown outcome without an error")
			}
			if r.Value != nil {
				t.Fatal("a value without acknowledgment")
			}
		})
	}

	// A token of the version appears between the read-back and the mint
	// (an attempt still in flight): Forgejo refuses the name with 400. That
	// is no proof: unknown, and the next attempt deletes and mints again.
	f, inst = newFake(t)
	f.beforeMint = func(f *fakeForgejo) { f.mintLocked(TokenName(1)) }
	if r, err := a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 1, nil); r.Outcome != provider.RotationUnknown || err == nil {
		t.Fatalf("name taken after the read-back = %+v, %v", r, err)
	}
	if r, err := a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 1, nil); r.Outcome != provider.RotationAcknowledged || err != nil {
		t.Fatalf("next attempt = %+v, %v", r, err)
	}

	// A committed mint whose answer was lost, then an attempt that cannot
	// authenticate: unknown, never rejected (the token exists).
	f, inst = newFake(t)
	f.mu.Lock()
	f.mintLocked(TokenName(1))
	f.mu.Unlock()
	wrong := env()
	wrong.creds[CredentialAdminPassword] = []byte("wrong")
	if r, err := a.ApplyCredential(t.Context(), wrong, inst, CredentialAdminToken, 1, nil); r.Outcome != provider.RotationUnknown || err == nil {
		t.Fatalf("lost mint, then no authentication = %+v, %v", r, err)
	}

	// A timeout is unknown: Forgejo may have minted the token.
	_, inst = newFake(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r, err := a.ApplyCredential(ctx, env(), inst, CredentialAdminToken, 1, nil); r.Outcome != provider.RotationUnknown || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %+v, %v", r, err)
	}

	// Other credentials are not rotated through the adapter.
	if r, _ := a.ApplyCredential(t.Context(), env(), inst, CredentialSecretKey, 2, []byte("x")); r.Outcome != provider.RotationRejected {
		t.Fatalf("secret_key rotation = %+v", r)
	}
}

// Abandon: the core re-applies the active version it holds (value set).
// The held token stays valid and nothing is minted; newer tokens (the
// abandoned candidate, possibly minted) are deleted; older ones stay.
func TestReassertToken(t *testing.T) {
	a := New()
	f, inst := newFake(t)
	f.mu.Lock()
	old := f.mintLocked(TokenName(1))
	active := f.mintLocked(TokenName(2))
	candidate := f.mintLocked(TokenName(3))
	other := f.mintLocked("someone-else")
	f.mu.Unlock()

	r, err := a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 2, []byte(active.SHA1))
	if err != nil || r.Outcome != provider.RotationAcknowledged || r.Value != nil {
		t.Fatalf("re-apply = %+v, %v", r, err)
	}
	if !f.hasToken(active.ID) || !f.hasToken(old.ID) || !f.hasToken(other.ID) {
		t.Fatal("re-applying the active version deleted a token it must keep")
	}
	if f.hasToken(candidate.ID) {
		t.Fatal("the abandoned candidate's token is still valid")
	}
	if len(f.values) != 3 {
		t.Fatalf("tokens after re-apply: %d, want 3 (nothing minted)", len(f.values))
	}

	// A held value Forgejo no longer accepts is not in use: rejected.
	if r, err := a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 2, []byte("token-unknown")); err != nil || r.Outcome != provider.RotationRejected {
		t.Fatalf("revoked held value = %+v, %v", r, err)
	}
	tokens := "/api/v1/users/" + AdminUsername + "/tokens"
	for name, fail := range map[string]func(candidate int64) map[string][]int{
		"list fails": func(int64) map[string][]int { return map[string][]int{"GET " + tokens: {http.StatusBadGateway}} },
		"delete fails": func(id int64) map[string][]int {
			return map[string][]int{"DELETE " + tokens + "/" + strconv.FormatInt(id, 10): {http.StatusInternalServerError}}
		},
		"check fails": func(int64) map[string][]int {
			return map[string][]int{"GET /api/v1/user": {http.StatusServiceUnavailable}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, inst := newFake(t)
			f.mu.Lock()
			held := f.mintLocked(TokenName(2))
			candidate := f.mintLocked(TokenName(3))
			f.mu.Unlock()
			f.fail = fail(candidate.ID)
			if r, err := a.ApplyCredential(t.Context(), env(), inst, CredentialAdminToken, 2, []byte(held.SHA1)); r.Outcome != provider.RotationUnknown || err == nil {
				t.Fatalf("outcome = %+v, %v; want unknown", r, err)
			}
		})
	}
}

// A refused organization creation is read back; the read-back's own
// failure keeps its classification, and only a read-back that finds no
// organization makes the refusal final.
func TestSetupConflictReadBack(t *testing.T) {
	a := New()
	for name, c := range map[string]struct {
		readBack int // status of the second GET; 0: the organization exists
		code     string
		retry    bool
	}{
		"created meanwhile":   {0, "", false},
		"read-back 503":       {http.StatusServiceUnavailable, CodeUnavailable, true},
		"read-back 429":       {http.StatusTooManyRequests, CodeRateLimited, true},
		"read-back 401":       {http.StatusUnauthorized, CodeAdminAuth, true},
		"not there after all": {http.StatusNotFound, CodeRejected, false},
	} {
		for _, refusal := range []int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("%s/%d", name, refusal), func(t *testing.T) {
				f, inst := newFake(t)
				f.orgs["acme"] = c.readBack == 0
				f.fail["GET /api/v1/orgs/acme"] = []int{http.StatusNotFound, c.readBack}
				f.fail["POST /api/v1/orgs"] = []int{refusal}
				err := a.Setup(t.Context(), env(), inst)
				got, retry, _ := provider.Classify(err)
				if c.code == "" {
					if err != nil {
						t.Fatalf("Setup: %v", err)
					}
					return
				}
				if err == nil || got != c.code || retry != c.retry {
					t.Fatalf("error = %v (code %s, retryable %v), want %s, %v", err, got, retry, c.code, c.retry)
				}
			})
		}
	}
}

func TestValidateSettings(t *testing.T) {
	a := New()
	for _, ok := range []string{"acme", "Acme-Robotics", "a.b_c-d", "a", "x1", strings.Repeat("a", 40)} {
		if err := a.ValidateSettings(map[string]any{SettingOrganization: ok}); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "acme-", "acme..org", "a--b", "-acme", ".acme", "_x", "acme_", "a b", "über",
		strings.Repeat("a", 41), "admin", "Explore", "api", "orglane-admin", "acme.keys", "Acme.RSS", "ghost"} {
		var se *provider.SettingError
		if err := a.ValidateSettings(map[string]any{SettingOrganization: bad}); !errors.As(err, &se) || se.Setting != SettingOrganization {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if err := a.ValidateSettings(map[string]any{SettingSSHPort: int64(2222)}); err != nil {
		t.Errorf("without an organization: %v", err)
	}
}
