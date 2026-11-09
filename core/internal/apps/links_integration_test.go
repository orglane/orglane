//go:build integration

package apps_test

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/github/githubfake"
	"github.com/orglane/orglane/providers/provider"
)

// Account linking with the real GitHub adapter against the fake GitHub,
// whose every request and response is validated against GitHub's OpenAPI
// description (no provider API is mocked).

type linkHarness struct {
	*harness
	gh   *githubfake.Fake
	inst apps.Instance
}

func newLinkHarness(t *testing.T) *linkHarness {
	t.Helper()
	return newLinkHarnessWithTransport(t, nil)
}

// newLinkHarnessWithTransport wraps the fake GitHub's transport, so a test can
// inject a fault or observe requests without changing the adapter.
func newLinkHarnessWithTransport(t *testing.T, wrap func(http.RoundTripper) http.RoundTripper) *linkHarness {
	t.Helper()
	gh := githubfake.New(func(err error) { t.Error(err) })
	t.Cleanup(gh.Close)
	var transport = gh.Transport()
	if wrap != nil {
		transport = wrap(transport)
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	h := newHarnessWith(t, apps.Config{HTTPClient: client}, githubprovider.New())
	app, org := gh.App(), gh.Org()
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: "github", Key: "github", State: apps.StateActive, Settings: map[string]any{
		"organization": org.Login, "organization_id": strconv.FormatInt(org.ID, 10), "app_id": strconv.FormatInt(app.ID, 10),
		"app_slug": app.Slug, "app_client_id": app.ClientID, "installation_id": strconv.FormatInt(gh.InstallationID(), 10),
	}})
	if err != nil {
		t.Fatal(err)
	}
	policy := provider.AllPeople
	if inst, err = h.svc.UpdateInstance(h.ctx, audit.System, inst.ID, apps.InstanceUpdate{AccessPolicy: &policy}); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{"app_private_key": gh.AppPrivateKeyPEM(), "app_client_secret": []byte(app.ClientSecret)} {
		if err := h.svc.StoreCredential(h.ctx, audit.System, inst.ID, name, value); err != nil {
			t.Fatal(err)
		}
	}
	return &linkHarness{harness: h, gh: gh, inst: inst}
}

// authorize starts linking for the person in the session and follows
// GitHub's authorization page as the browser would, returning the callback
// query (state and code).
func (h *linkHarness) authorize(person store.Person, session uuid.UUID) url.Values {
	h.t.Helper()
	u, err := h.svc.StartAccountLink(h.ctx, person.ID, session, "github", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := h.gh.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = res.Body.Close()
	back, err := url.Parse(res.Header.Get("Location"))
	if err != nil || res.StatusCode != http.StatusFound {
		h.t.Fatalf("authorization page = %d %v", res.StatusCode, err)
	}
	return back.Query()
}

func (h *linkHarness) complete(person store.Person, session uuid.UUID, back url.Values) (store.AccountLink, error) {
	return h.svc.CompleteAccountLink(h.ctx, personActor(person.ID), person.ID, session, "github", back.Get("state"), back.Get("code"))
}

func (h *linkHarness) link(person store.Person) (store.AccountLink, error) {
	session := uuid.UUID(h.session(person))
	return h.complete(person, session, h.authorize(person, session))
}

// The person's own authorization proves their GitHub account by its ID,
// queues their provisioning, and leaves no GitHub token behind.
func TestAccountLinkVerifiesByID(t *testing.T) {
	h := newLinkHarness(t)
	alice := h.person("alice")
	h.gh.AddUser(githubfake.User{ID: 1001, Login: "alice-gh"})
	h.gh.AuthorizeAs(1001)
	// Creating the person already queued provisioning (the instance has a
	// provisioner now); the link must queue another run.
	h.drain()
	jobs := h.personJobs(alice.ID)
	link, err := h.link(alice)
	if err != nil {
		t.Fatal(err)
	}
	if link.ExternalID != "1001" || link.Login != "alice-gh" || link.Realm != "github.com" || link.Method != provider.LinkOAuth || link.Evidence == nil || *link.Evidence != apps.EvidenceNone {
		t.Fatalf("link = %+v", link)
	}
	if n := h.gh.LiveUserTokens(); n != 0 {
		t.Fatalf("%d GitHub user tokens left", n)
	}
	if h.auditCount(audit.AccountLinkVerified) != 1 || h.personJobs(alice.ID) <= jobs {
		t.Fatal("verification was not audited or did not queue provisioning")
	}
	// Linking again after a rename keeps the link (keyed by ID) and
	// refreshes the informational login.
	h.gh.RenameUser(1001, "alice-renamed")
	again, err := h.link(alice)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != link.ID || again.Login != "alice-renamed" || h.auditCount(audit.AccountLinkVerified) != 1 {
		t.Fatalf("re-verification = %+v", again)
	}
	links, err := h.svc.MyAccountLinks(h.ctx, alice.ID)
	if err != nil || len(links.Verified) != 1 || len(links.Linkable) != 1 || links.Linkable[0].ID != h.inst.ID {
		t.Fatalf("my links = %+v, %v", links, err)
	}
}

// The state is bound to the person and the session, single-use, short-lived,
// and consumed before GitHub is contacted, also when the exchange fails.
func TestAccountLinkStateBinding(t *testing.T) {
	h := newLinkHarness(t)
	alice, mallory := h.person("alice"), h.person("mallory")
	h.gh.AddUser(githubfake.User{ID: 1001, Login: "alice-gh"})
	h.gh.AuthorizeAs(1001)
	session := uuid.UUID(h.session(alice))
	back := h.authorize(alice, session)
	// Another person, or another session of the same person, cannot use it.
	if _, err := h.complete(mallory, uuid.UUID(h.session(mallory)), back); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("another person's completion = %v", err)
	}
	if _, err := h.complete(alice, uuid.UUID(h.session(alice)), back); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("another session's completion = %v", err)
	}
	if _, err := h.complete(alice, session, back); err != nil {
		t.Fatalf("the right session's completion: %v", err)
	}
	if _, err := h.complete(alice, session, back); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("replayed state = %v", err)
	}
	// Expired.
	back = h.authorize(alice, session)
	h.exec(`UPDATE app_oauth_states SET expires_at = now() - interval '1 second', created_at = now() - interval '11 minutes' WHERE kind = 'link' AND consumed_at IS NULL`)
	if _, err := h.complete(alice, session, back); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("expired state = %v", err)
	}
	// A new start invalidates the older state.
	older := h.authorize(alice, session)
	newer := h.authorize(alice, session)
	if _, err := h.complete(alice, session, older); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("superseded state = %v", err)
	}
	// A refused exchange still consumes the state.
	newer.Set("code", "not-a-code")
	if _, err := h.complete(alice, session, newer); err == nil {
		t.Fatal("a refused code linked an account")
	}
	newer.Del("code")
	if _, err := h.complete(alice, session, newer); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("state after a refused exchange = %v", err)
	}
	if _, err := h.svc.CompleteAccountLink(h.ctx, personActor(alice.ID), alice.ID, session, "github", "not-a-state", "x"); !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("malformed state = %v", err)
	}
}

// One verified link per GitHub account and per person: an account another
// person linked is refused; a different account of the same person
// replaces the old link.
func TestAccountLinkUniqueness(t *testing.T) {
	h := newLinkHarness(t)
	alice, bob := h.person("alice"), h.person("bob")
	h.gh.AddUser(githubfake.User{ID: 1001, Login: "alice-gh"})
	h.gh.AddUser(githubfake.User{ID: 1002, Login: "alice-second"})
	h.gh.AuthorizeAs(1001)
	first, err := h.link(alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.link(bob); !errors.Is(err, apps.ErrAccountAlreadyLinked) {
		t.Fatalf("linking alice's account as bob = %v", err)
	}
	if n := h.count(`SELECT count(*) FROM account_links WHERE person_id = $1`, bob.ID); n != 0 {
		t.Fatalf("bob got %d links", n)
	}
	h.gh.AuthorizeAs(1002)
	second, err := h.link(alice)
	if err != nil {
		t.Fatal(err)
	}
	if second.ExternalID != "1002" || second.ID == first.ID {
		t.Fatalf("replacement = %+v", second)
	}
	if n := h.count(`SELECT count(*) FROM account_links WHERE id = $1 AND state = 'revoked'`, first.ID); n != 1 {
		t.Fatal("the replaced link was not revoked")
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = $1 AND metadata->>'reason' = 'replaced'`, string(audit.AccountLinkRevoked)); n != 1 {
		t.Fatalf("replacement audits = %d", n)
	}
}

// M1 import evidence is compared, never trusted: matched, mismatched, and
// another person's evidence for the account shows as a conflict.
func TestAccountLinkEvidence(t *testing.T) {
	h := newLinkHarness(t)
	alice, bob, carol := h.person("alice"), h.person("bob"), h.person("carol")
	org := h.gh.Org().ID
	pending := func(person store.Person, id int64, login string) {
		h.exec(`INSERT INTO pending_account_links (provider, organization_id, external_id, person_id, login, avatar_url) VALUES ('github', $1, $2, $3, $4, '')`, org, id, person.ID, login)
	}
	pending(alice, 1001, "alice-gh")
	pending(bob, 4444, "bob-old")
	pending(carol, 1003, "bob-gh") // the import attributed bob's account to carol
	for _, tc := range []struct {
		person store.Person
		user   githubfake.User
		want   string
	}{
		{alice, githubfake.User{ID: 1001, Login: "alice-gh"}, apps.EvidenceMatched},
		{bob, githubfake.User{ID: 1003, Login: "bob-gh"}, apps.EvidenceMismatched},
	} {
		h.gh.AddUser(tc.user)
		h.gh.AuthorizeAs(tc.user.ID)
		link, err := h.link(tc.person)
		if err != nil {
			t.Fatal(err)
		}
		if link.Evidence == nil || *link.Evidence != tc.want {
			t.Fatalf("%s evidence = %v, want %s", tc.person.Username, link.Evidence, tc.want)
		}
	}
	// Carol's evidence names bob's verified account: a conflict, shown, not
	// resolved; carol has no link.
	links, err := h.svc.MyAccountLinks(h.ctx, carol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links.Verified) != 0 || len(links.Pending) != 1 || !links.Pending[0].Conflict {
		t.Fatalf("carol's links = %+v", links)
	}
}

// The person unlinks their own account; an admin revokes a wrong link.
// Both are audited and queue provisioning; nobody can revoke a link through
// another person.
func TestAccountLinkRevocation(t *testing.T) {
	h := newLinkHarness(t)
	alice, bob := h.person("alice"), h.person("bob")
	h.gh.AddUser(githubfake.User{ID: 1001, Login: "alice-gh"})
	h.gh.AuthorizeAs(1001)
	link, err := h.link(alice)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RevokeAccountLink(h.ctx, personActor(bob.ID), bob.ID, link.ID, apps.RevokedBySelf); !errors.Is(err, apps.ErrNotFound) {
		t.Fatalf("revoking through another person = %v", err)
	}
	h.exec(`DELETE FROM river_job WHERE kind = $1`, apps.ProvisionPersonArgs{}.Kind()) // the link's own job ran
	if err := h.svc.RevokeAccountLink(h.ctx, personActor(alice.ID), alice.ID, link.ID, apps.RevokedBySelf); err != nil {
		t.Fatal(err)
	}
	if h.personJobs(alice.ID) != 1 {
		t.Fatal("unlinking did not queue provisioning")
	}
	if err := h.svc.RevokeAccountLink(h.ctx, personActor(alice.ID), alice.ID, link.ID, apps.RevokedBySelf); !errors.Is(err, apps.ErrNotFound) {
		t.Fatalf("revoking twice = %v", err)
	}
	// Linked again, then revoked by an admin.
	link, err = h.link(alice)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RevokeAccountLink(h.ctx, audit.System, alice.ID, link.ID, apps.RevokedByAdmin); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM audit_entries WHERE action = $1`, string(audit.AccountLinkRevoked)); n != 2 {
		t.Fatalf("revocation audits = %d", n)
	}
}

// GitHub's rate limit on the user lookup is retryable with its delay; the
// state is spent and the token revoked.
func TestAccountLinkConcurrentStartsLeaveOneState(t *testing.T) {
	h := newLinkHarness(t)
	alice := h.person("alice")
	session := uuid.UUID(h.session(alice))
	const starts = 16
	start := make(chan struct{})
	errs := make(chan error, starts)
	var wg sync.WaitGroup
	for range starts {
		wg.Go(func() {
			<-start
			_, err := h.svc.StartAccountLink(h.ctx, alice.ID, session, "github", nil)
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := h.count(`SELECT count(*) FROM app_oauth_states WHERE kind = 'link' AND consumed_at IS NULL`); n != 1 {
		t.Fatalf("%d live link states after %d concurrent starts, want 1", n, starts)
	}
}

func TestAccountLinkRateLimit(t *testing.T) {
	h := newLinkHarness(t)
	alice := h.person("alice")
	h.gh.AddUser(githubfake.User{ID: 1001, Login: "alice-gh"})
	h.gh.AuthorizeAs(1001)
	session := uuid.UUID(h.session(alice))
	back := h.authorize(alice, session)
	h.gh.Fail(http.MethodGet, "/user", http.StatusTooManyRequests, http.Header{"Retry-After": {"90"}})
	_, err := h.complete(alice, session, back)
	if code, retryable, after := provider.Classify(err); code != githubprovider.CodeRateLimited || !retryable || after != 90*time.Second {
		t.Fatalf("rate-limited link = %s %v %s (%v)", code, retryable, after, err)
	}
	if n := h.gh.LiveUserTokens(); n != 0 {
		t.Fatalf("%d GitHub user tokens left", n)
	}
	if n := h.count(`SELECT count(*) FROM account_links`); n != 0 {
		t.Fatalf("%d links after a failed lookup", n)
	}
}

func personActor(id uuid.UUID) audit.Actor {
	return audit.Actor{Kind: audit.ActorPerson, ID: id.String()}
}
