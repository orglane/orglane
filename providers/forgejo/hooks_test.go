package forgejo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

const hookTarget = "https://work.acme.test/api/v1/webhooks/x"

// hookEnv is env() with a webhook secret and webhook URL.
type hookEnv struct {
	testEnv
	target string
}

func (e hookEnv) WebhookURL(uuid.UUID) *url.URL { u, _ := url.Parse(e.target); return u }

func newHookEnv(secrets ...string) hookEnv {
	e := env()
	if len(secrets) > 0 {
		e.creds[CredentialWebhookSecret] = []byte(secrets[0])
	}
	return hookEnv{testEnv: e, target: hookTarget}
}

// fakeHooks is an httptest stand-in for Forgejo's organization hook API
// (unit tests only; the integration test runs the real Forgejo).
type fakeHooks struct {
	mu       sync.Mutex
	hooks    []fakeHook
	nextID   int64
	fail     map[string][]int
	requests []string
	// loseCreate commits the next creation, then answers 502 (the answer
	// was lost on the way).
	loseCreate bool
}

type fakeHook struct {
	ID     int64
	URL    string
	Type   string
	Secret string
	Events []string
	Active bool
}

func newFakeHooks(t *testing.T) (*fakeHooks, provider.Instance) {
	t.Helper()
	f := &fakeHooks{fail: map[string][]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return f, provider.Instance{ID: uuid.New(), Key: "git", Provider: ID, Upstream: u, Settings: map[string]any{SettingOrganization: "acme"}}
}

func (f *fakeHooks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if u, p, ok := r.BasicAuth(); !ok || u != AdminUsername || p != testPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	base := "/api/v1/orgs/acme/hooks"
	var in struct {
		Type   string            `json:"type"`
		Config map[string]string `json:"config"`
		Events []string          `json:"events"`
		Active bool              `json:"active"`
	}
	switch {
	case key == "GET "+base:
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		from, to := min((page-1)*limit, len(f.hooks)), min(page*limit, len(f.hooks))
		out := []hook{}
		for _, h := range f.hooks[from:to] {
			// Forgejo never returns the secret.
			out = append(out, hook{ID: h.ID, Type: h.Type, Active: h.Active, Config: map[string]string{"url": h.URL, "content_type": "json"}})
		}
		_ = json.NewEncoder(w).Encode(out)
	case key == "POST "+base:
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.nextID++
		f.hooks = append(f.hooks, fakeHook{ID: f.nextID, URL: in.Config["url"], Type: in.Type, Secret: in.Config["secret"], Events: in.Events, Active: in.Active})
		if f.loseCreate {
			f.loseCreate = false
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(r.URL.Path, base+"/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, base+"/"), 10, 64)
		for i, h := range f.hooks {
			if h.ID != id {
				continue
			}
			switch r.Method {
			case http.MethodPatch:
				_ = json.NewDecoder(r.Body).Decode(&in)
				// Like Forgejo 15: an update never changes the secret.
				h.URL, h.Events, h.Active = in.Config["url"], in.Events, in.Active
				f.hooks[i] = h
				w.WriteHeader(http.StatusOK)
			case http.MethodDelete:
				f.hooks = append(f.hooks[:i], f.hooks[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeHooks) ours() []fakeHook {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeHook
	for _, h := range f.hooks {
		if isOurs(h.URL, hookTarget) {
			out = append(out, h)
		}
	}
	return out
}

func TestEnsureWebhooks(t *testing.T) {
	f, inst := newFakeHooks(t)
	a := New()
	marked := func(secret string) string {
		u, err := markedURL(hookTarget, []byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	// Missing: created, active, JSON, with the active secret and its mark.
	if err := a.EnsureWebhooks(t.Context(), newHookEnv("s1"), inst); err != nil {
		t.Fatal(err)
	}
	h := f.ours()
	if len(h) != 1 || h[0].Secret != "s1" || h[0].URL != marked("s1") || h[0].Type != "forgejo" || !h[0].Active ||
		strings.Join(h[0].Events, ",") != "push,pull_request,issue_comment" {
		t.Fatalf("created %+v", h)
	}
	if strings.Contains(h[0].URL, "s1") {
		t.Fatal("the URL carries the secret")
	}
	// Changed in Forgejo (deactivated, other events): repaired in place;
	// another hook of the organization is never touched.
	f.mu.Lock()
	f.hooks[0].Active, f.hooks[0].Events = false, []string{"push"}
	f.nextID++
	f.hooks = append(f.hooks, fakeHook{ID: f.nextID, URL: "https://ci.acme.test/hook", Secret: "theirs", Active: true})
	f.mu.Unlock()
	if err := a.EnsureWebhooks(t.Context(), newHookEnv("s1"), inst); err != nil {
		t.Fatal(err)
	}
	if h := f.ours(); len(h) != 1 || h[0].ID != 1 || h[0].Secret != "s1" || !h[0].Active || len(h[0].Events) != 3 {
		t.Fatalf("repaired %+v", h)
	}
	// Another active secret (a restored snapshot brought back an old hook):
	// a new hook is created, then the old one deleted.
	if err := a.EnsureWebhooks(t.Context(), newHookEnv("s2"), inst); err != nil {
		t.Fatal(err)
	}
	if h := f.ours(); len(h) != 1 || h[0].ID == 1 || h[0].Secret != "s2" || h[0].URL != marked("s2") {
		t.Fatalf("after another secret %+v", h)
	}
	if i := slices.Index(f.requests, "POST /api/v1/orgs/acme/hooks"); i < 0 || slices.Index(f.requests[i:], "DELETE /api/v1/orgs/acme/hooks/1") < 0 {
		t.Fatalf("not created before the old one was deleted: %v", f.requests)
	}
	if other := f.hooks[0]; other.Secret != "theirs" || other.URL != "https://ci.acme.test/hook" {
		t.Fatalf("another hook was changed: %+v", other)
	}
	// Duplicates (a creation whose answer was lost, then repeated) and an
	// unmarked hook of ours: one is kept, the rest deleted.
	f.mu.Lock()
	f.nextID++
	f.hooks = append(f.hooks, fakeHook{ID: f.nextID, URL: marked("s2"), Secret: "s2", Active: true})
	f.nextID++
	f.hooks = append(f.hooks, fakeHook{ID: f.nextID, URL: hookTarget, Secret: "?", Active: true})
	f.mu.Unlock()
	if err := a.EnsureWebhooks(t.Context(), newHookEnv("s2"), inst); err != nil {
		t.Fatal(err)
	}
	if h := f.ours(); len(h) != 1 || h[0].Secret != "s2" {
		t.Fatalf("after duplicates %+v", h)
	}
	// A creation whose answer is lost: the retry keeps the created hook.
	f.mu.Lock()
	f.loseCreate = true
	f.mu.Unlock()
	if code, retryable, _ := provider.Classify(a.EnsureWebhooks(t.Context(), newHookEnv("s3"), inst)); code != CodeUnavailable || !retryable {
		t.Fatalf("lost creation: %s %v", code, retryable)
	}
	if h := f.ours(); len(h) != 2 {
		t.Fatalf("after a lost creation (the old hook stays until the new one is confirmed): %+v", h)
	}
	if err := a.EnsureWebhooks(t.Context(), newHookEnv("s3"), inst); err != nil {
		t.Fatal(err)
	}
	if h := f.ours(); len(h) != 1 || h[0].Secret != "s3" {
		t.Fatalf("after the retry %+v", h)
	}
	// Failures are classified: a server error retries, a refusal does not.
	f.mu.Lock()
	f.fail["GET /api/v1/orgs/acme/hooks"] = []int{http.StatusServiceUnavailable, http.StatusUnprocessableEntity}
	f.mu.Unlock()
	if code, retryable, _ := provider.Classify(a.EnsureWebhooks(t.Context(), newHookEnv("s3"), inst)); code != CodeUnavailable || !retryable {
		t.Fatalf("503: %s %v", code, retryable)
	}
	if code, retryable, _ := provider.Classify(a.EnsureWebhooks(t.Context(), newHookEnv("s3"), inst)); code != CodeRejected || retryable {
		t.Fatalf("422: %s %v", code, retryable)
	}
	// Without a webhook secret nothing is sent.
	before := len(f.requests)
	if err := a.EnsureWebhooks(t.Context(), newHookEnv(), inst); !errors.Is(err, provider.ErrNotFound) || len(f.requests) != before {
		t.Fatalf("no secret: %v, %d requests", err, len(f.requests)-before)
	}
}

func TestApplyWebhookSecret(t *testing.T) {
	f, inst := newFakeHooks(t)
	a := New()
	e := newHookEnv("active")
	if err := a.EnsureWebhooks(t.Context(), e, inst); err != nil {
		t.Fatal(err)
	}
	// Forward: the hook is replaced by one with the candidate.
	res, err := a.ApplyCredential(t.Context(), e, inst, CredentialWebhookSecret, 2, []byte("candidate"))
	if h := f.ours(); err != nil || res.Outcome != provider.RotationAcknowledged || len(h) != 1 || h[0].Secret != "candidate" {
		t.Fatalf("forward: %+v, %v, %+v", res, err, h)
	}
	// Again (an attempt whose answer was lost): nothing changes.
	id := f.ours()[0].ID
	res, err = a.ApplyCredential(t.Context(), e, inst, CredentialWebhookSecret, 2, []byte("candidate"))
	if h := f.ours(); err != nil || res.Outcome != provider.RotationAcknowledged || len(h) != 1 || h[0].ID != id {
		t.Fatalf("re-apply: %+v, %v, %+v", res, err, h)
	}
	// Abandon: the active value is set back.
	res, err = a.ApplyCredential(t.Context(), e, inst, CredentialWebhookSecret, 1, []byte("active"))
	if h := f.ours(); err != nil || res.Outcome != provider.RotationAcknowledged || len(h) != 1 || h[0].Secret != "active" {
		t.Fatalf("abandon: %+v, %v, %+v", res, err, h)
	}
	// No proof either way (Forgejo never returns the secret): every failure,
	// a refusal included, is unknown.
	for _, code := range []int{http.StatusBadGateway, http.StatusUnprocessableEntity} {
		f.mu.Lock()
		f.fail["POST /api/v1/orgs/acme/hooks"] = []int{code}
		f.mu.Unlock()
		res, err = a.ApplyCredential(t.Context(), e, inst, CredentialWebhookSecret, 3, []byte("third"))
		if err == nil || res.Outcome != provider.RotationUnknown {
			t.Fatalf("%d: %+v, %v", code, res, err)
		}
	}
	// Unknown credentials stay rejected.
	if res, _ := a.ApplyCredential(t.Context(), e, inst, CredentialDBPassword, 1, []byte("x")); res.Outcome != provider.RotationRejected {
		t.Fatalf("db_password: %+v", res)
	}
}

// verifyBody is the body of the Verify cases.
const verifyBody = `{"ref":"refs/heads/main"}`

func sign(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(verifyBody))
	return hex.EncodeToString(mac.Sum(nil))
}

// multiEnv returns several webhook secrets (a rotation in progress).
type multiEnv struct {
	testEnv
	secrets []string
}

func (e multiEnv) Credentials(_ context.Context, name string) ([][]byte, error) {
	if name != CredentialWebhookSecret {
		return nil, provider.ErrNotFound
	}
	out := [][]byte{}
	for _, s := range e.secrets {
		out = append(out, []byte(s))
	}
	return out, nil
}

func TestVerify(t *testing.T) {
	a := New()
	body := verifyBody
	e := multiEnv{testEnv: env(), secrets: []string{"old", "new"}}
	header := func(sig string) http.Header {
		h := http.Header{}
		if sig != "" {
			h.Set(SignatureHeader, sig)
		}
		return h
	}
	for name, c := range map[string]struct {
		sig  string
		body string
		ok   bool
	}{
		"active secret":       {sign("old"), body, true},
		"candidate secret":    {sign("new"), body, true},
		"uppercase hex":       {strings.ToUpper(sign("new")), body, true},
		"another secret":      {sign("other"), body, false},
		"changed body":        {sign("old"), body + " ", false},
		"missing":             {"", body, false},
		"not hex":             {"zz" + sign("old")[2:], body, false},
		"truncated":           {sign("old")[:62], body, false},
		"sha1 length":         {sign("old")[:40], body, false},
		"github-style prefix": {"sha256=" + sign("old"), body, false},
	} {
		err := a.Verify(t.Context(), e, provider.Instance{}, header(c.sig), []byte(c.body))
		if c.ok && err != nil || !c.ok && !errors.Is(err, provider.ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Without any secret nothing verifies.
	if err := a.Verify(t.Context(), multiEnv{testEnv: env()}, provider.Instance{}, header(sign("")), []byte(body)); !errors.Is(err, provider.ErrBadSignature) {
		t.Errorf("no secrets: %v", err)
	}
}

func TestDeliveryID(t *testing.T) {
	a := New()
	h := http.Header{}
	h.Set(DeliveryHeader, "4439d2f3-00bd-4bbc-a05d-825b1206b013")
	if id, err := a.DeliveryID(h); err != nil || id != "4439d2f3-00bd-4bbc-a05d-825b1206b013" {
		t.Fatalf("%q, %v", id, err)
	}
	for _, bad := range []string{"", "1", "4439d2f3-00bd-4bbc-a05d-825b1206b013x", strings.Repeat("a", 200)} {
		h.Set(DeliveryHeader, bad)
		if _, err := a.DeliveryID(h); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
