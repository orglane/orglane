//go:build integration

package peopleimport_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"

	"github.com/orglane/orglane/core/internal/peopleimport"
)

func TestGitHubPipelinePendingLinkAndRepeat(t *testing.T) {
	f := newFixture(t)
	login := "github-fixture"
	email := "fixture@example.test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/fixture":
			_, _ = fmt.Fprint(w, `{"id":4242}`)
		case "/orgs/fixture/members":
			if r.URL.Query().Get("page") != "1" {
				_, _ = fmt.Fprint(w, `[]`)
				return
			}
			_, _ = fmt.Fprint(w, `[{"id":101},{"id":102}]`)
		case "/user/101":
			_, _ = fmt.Fprintf(w, `{"id":101,"login":%q,"name":"Fixture Person","email":%q}`, login, email)
		case "/user/102":
			_, _ = fmt.Fprint(w, `{"id":102,"login":"no-email","name":"Missing Email"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	upload := func() peopleimport.Run {
		t.Helper()
		r, e := f.imports.Upload(f.ctx, f.actor, peopleimport.GitHubTestSource(f.ctx, server.URL, server.Client()), nil)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	m := peopleimport.Mapping{Columns: []peopleimport.Column{{Column: 0, Target: "username"}, {Column: 1, Target: "display_name"}, {Column: 2, Target: "work_email"}, {Column: 3, Target: "external_id"}}, SkipRows: []int{}}
	r := upload()
	r, e := f.imports.Preview(f.ctx, f.actor, r.ID, m)
	if e != nil {
		t.Fatal(e)
	}
	if r.Rows[0].Decision != "create" || r.Rows[1].Decision != "conflict" {
		t.Fatalf("unexpected decisions: %+v", r.Rows)
	}
	r, e = f.imports.Apply(f.ctx, f.actor, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if r.Rows[0].Result != "created" {
		t.Fatalf("unexpected result: %+v", r.Rows)
	}
	var state, storedLogin string
	if e = f.pool.QueryRow(f.ctx, "SELECT state,login FROM pending_account_links WHERE organization_id=4242 AND external_id=101").Scan(&state, &storedLogin); e != nil {
		t.Fatal(e)
	}
	if state != "pending" || storedLogin != login {
		t.Fatal("pending link missing")
	}
	var invitations int
	if e = f.pool.QueryRow(f.ctx, "SELECT count(*) FROM invitations WHERE person_id=(SELECT person_id FROM pending_account_links WHERE external_id=101)").Scan(&invitations); e != nil || invitations != 1 {
		t.Fatalf("invitations=%d err=%v", invitations, e)
	}
	login = "renamed-login"
	repeated := upload()
	repeated, e = f.imports.Preview(f.ctx, f.actor, repeated.ID, m)
	if e != nil {
		t.Fatal(e)
	}
	if repeated.Rows[0].Decision != "update" {
		t.Fatalf("rename lost stable identity: %+v", repeated.Rows)
	}
	repeated, e = f.imports.Apply(f.ctx, f.actor, repeated.ID)
	if e != nil {
		t.Fatal(e)
	}
	if repeated.Rows[0].Result != "updated" {
		t.Fatalf("rename failed: %+v", repeated.Rows)
	}

	// A supplied email pointing elsewhere must not override the stable pending account.
	other, err := f.ids.CreatePerson(f.ctx, f.actor, identity.NewPerson{Username: "unrelated-person", WorkEmail: "unrelated@example.test", DisplayName: "Unrelated Person", Status: identity.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	email = other.WorkEmail
	conflict := upload()
	conflict, err = f.imports.Preview(f.ctx, f.actor, conflict.ID, m)
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Rows[0].Decision != "conflict" || conflict.Rows[0].Error != "identity_conflict" {
		t.Fatalf("stable account/email conflict lost: %+v", conflict.Rows)
	}
	conflict, err = f.imports.Apply(f.ctx, f.actor, conflict.ID)
	if err != nil {
		t.Fatal(err)
	}
	if conflict.Rows[0].Result != "failed" {
		t.Fatal("conflicting source overwrote a person")
	}
	var reassigned bool
	if err = f.pool.QueryRow(f.ctx, "SELECT person_id=$1 FROM pending_account_links WHERE organization_id=4242 AND external_id=101", other.ID).Scan(&reassigned); err != nil || reassigned {
		t.Fatal("pending association reassigned", err)
	}
	var leaked bool
	if e = f.pool.QueryRow(f.ctx, `SELECT EXISTS(SELECT 1 FROM audit_entries WHERE metadata::text LIKE '%test-only-token%')`).Scan(&leaked); e != nil || leaked {
		t.Fatalf("audit credential leak err=%v", e)
	}
	var plaintext string
	if e = f.pool.QueryRow(f.ctx, `SELECT string_agg(encode(payload_sealed,'escape'),'') FROM people_import_rows`).Scan(&plaintext); e != nil || strings.Contains(plaintext, "test-only-token") || strings.Contains(plaintext, "fixture@example.test") {
		t.Fatal("staging plaintext leak", e)
	}
}

func TestGitHubFetchChecksCurrentGrantBeforeNetwork(t *testing.T) {
	calls := 0
	restore := peopleimport.SetGitHubTestClient(&http.Client{Transport: countGitHubRequests(func() { calls++ })})
	defer restore()
	defer func() {
		if calls != 0 {
			t.Fatal("outbound request before current permission check")
		}
	}()
	f := newFixture(t)
	person, err := f.ids.CreatePerson(f.ctx, f.actor, identity.NewPerson{Username: "former-admin", WorkEmail: "former-admin@example.test", DisplayName: "Former Admin", Status: identity.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ids.AssignRole(f.ctx, f.actor, person.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	stale := authz.WithPrincipal(f.ctx, &authz.Principal{Subject: person.ID.String(), Permissions: authz.All()})
	if err = f.ids.RemoveRole(f.ctx, f.actor, person.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	_, err = f.imports.FetchGitHub(stale, audit.Actor{Kind: audit.ActorPerson, ID: person.ID.String()}, "fixture", "synthetic-token")
	if !errors.Is(err, peopleimport.ErrForbidden) {
		t.Fatalf("stale principal reached source: %v", err)
	}
}

func TestGitHubRunBeyondFileBoundResumesAndSkips(t *testing.T) {
	f := newFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orgs/fixture" {
			_, _ = fmt.Fprint(w, `{"id":99}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/members") {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			members := []map[string]any{}
			for id := (page-1)*100 + 1; id <= 1001 && id <= page*100; id++ {
				members = append(members, map[string]any{"id": id})
			}
			_ = json.NewEncoder(w).Encode(members)
			return
		}
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/user/"))
		_, _ = fmt.Fprintf(w, `{"id":%d,"login":"member-%d","name":"Example"}`, id, id)
	}))
	defer server.Close()
	run, err := f.imports.Upload(f.ctx, f.actor, peopleimport.GitHubTestSource(f.ctx, server.URL, server.Client()), nil)
	if err != nil {
		t.Fatal(err)
	}
	run, err = f.imports.Get(f.ctx, f.actor, run.ID)
	if err != nil || len(run.Rows) != 1001 {
		t.Fatalf("resume rows=%d err=%v", len(run.Rows), err)
	}
	m := peopleimport.Mapping{Columns: []peopleimport.Column{{Column: 3, Target: "external_id"}}, SkipRows: []int{}}
	for _, r := range run.Rows {
		m.SkipRows = append(m.SkipRows, r.Row)
	}
	run, err = f.imports.Preview(f.ctx, f.actor, run.ID, m)
	if err != nil {
		t.Fatal(err)
	}
	run, err = f.imports.Apply(f.ctx, f.actor, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range run.Rows {
		if row.Result != "skipped" {
			t.Fatalf("skip failed %+v", row)
		}
	}
}

type countGitHubRequests func()

func (f countGitHubRequests) RoundTrip(*http.Request) (*http.Response, error) {
	f()
	return nil, fmt.Errorf("unexpected outbound request")
}
