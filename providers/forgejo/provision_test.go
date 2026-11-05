package forgejo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

// fakeAdmin is an httptest stand-in for the Forgejo admin, team and
// membership endpoints provisioning uses (unit tests only; the acceptance
// test runs the real Forgejo).
type fakeAdmin struct {
	mu      sync.Mutex
	users   map[int64]*user
	teams   map[int64]*team
	members map[int64]map[string]bool // team -> logins
	nextID  int64
	owners  int64
	fail    map[string][]int // "METHOD path" -> statuses in turn (0: answer)
	// beforeCreate runs before an account is created (a concurrent or lost
	// attempt).
	beforeCreate func(f *fakeAdmin)
}

const fakeSource = 3

func newFakeAdmin(t *testing.T) (*fakeAdmin, provider.Instance) {
	t.Helper()
	f := &fakeAdmin{users: map[int64]*user{}, teams: map[int64]*team{}, members: map[int64]map[string]bool{}, fail: map[string][]int{}, nextID: 100}
	f.users[1] = &user{ID: 1, Login: AdminUsername}
	f.owners = f.addTeam("Owners", "")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return f, provider.Instance{
		ID: uuid.New(), Key: "git", Provider: ID, Management: provider.Managed, Upstream: u,
		Settings: map[string]any{SettingOrganization: "acme"},
	}
}

func (f *fakeAdmin) addTeam(name, desc string) int64 {
	f.nextID++
	f.teams[f.nextID] = &team{ID: f.nextID, Name: name, Description: desc}
	f.members[f.nextID] = map[string]bool{}
	return f.nextID
}

func (f *fakeAdmin) addUser(login string, source int64, loginName string) *user {
	f.nextID++
	u := &user{ID: f.nextID, Login: login, SourceID: source, LoginName: loginName, FullName: login, Email: login + "@acme.test"}
	f.users[u.ID] = u
	return u
}

func (f *fakeAdmin) byLogin(login string) *user {
	for _, u := range f.users {
		if u.Login == login {
			return u
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	if codes := f.fail[key]; len(codes) > 0 {
		f.fail[key] = codes[1:]
		if codes[0] != 0 {
			if codes[0] == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "7")
			}
			w.WriteHeader(codes[0])
			return
		}
	}
	if r.URL.Path == authSourcePath {
		writeJSON(w, 200, map[string]any{"name": AuthSourceName, "id": fakeSource})
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != AdminUsername || p != testPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := r.URL.Path
	switch {
	case key == "GET /api/v1/admin/users":
		var out []user
		src, name := r.URL.Query().Get("source_id"), r.URL.Query().Get("login_name")
		for _, u := range f.users {
			if (src == "" || strconv.FormatInt(u.SourceID, 10) == src) && (name == "" || u.LoginName == name) {
				out = append(out, *u)
			}
		}
		slices.SortFunc(out, func(a, b user) int { return int(a.ID - b.ID) })
		writeJSON(w, 200, page(out, r))
	case key == "POST /api/v1/admin/users":
		var in struct {
			Username, LoginName string
			SourceID            int64
		}
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		in.Username, _ = raw["username"].(string)
		in.LoginName, _ = raw["login_name"].(string)
		if s, ok := raw["source_id"].(float64); ok {
			in.SourceID = int64(s)
		}
		if f.beforeCreate != nil {
			f.beforeCreate(f)
			f.beforeCreate = nil
		}
		if f.byLogin(in.Username) != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		u := f.addUser(in.Username, in.SourceID, in.LoginName)
		u.Email, _ = raw["email"].(string)
		u.FullName, _ = raw["full_name"].(string)
		writeJSON(w, 201, u)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/rename"):
		old := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/admin/users/"), "/rename")
		var in struct {
			NewUsername string `json:"new_username"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		u := f.byLogin(old)
		switch {
		case u == nil:
			w.WriteHeader(http.StatusNotFound)
		case f.byLogin(in.NewUsername) != nil:
			w.WriteHeader(http.StatusUnprocessableEntity)
		default:
			for _, m := range f.members {
				if m[old] {
					delete(m, old)
					m[in.NewUsername] = true
				}
			}
			u.Login = in.NewUsername
			w.WriteHeader(http.StatusNoContent)
		}
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/api/v1/admin/users/"):
		u := f.byLogin(strings.TrimPrefix(path, "/api/v1/admin/users/"))
		if u == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		u.FullName, _ = raw["full_name"].(string)
		u.Email, _ = raw["email"].(string)
		writeJSON(w, 200, u)
	case strings.HasPrefix(path, "/api/v1/users/"):
		if f.byLogin(strings.TrimPrefix(path, "/api/v1/users/")) == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, 200, map[string]any{})
	case key == "GET /api/v1/orgs/acme/teams":
		var out []team
		for _, t := range f.teams {
			out = append(out, *t)
		}
		slices.SortFunc(out, func(a, b team) int { return int(a.ID - b.ID) })
		writeJSON(w, 200, page(out, r))
	case key == "POST /api/v1/orgs/acme/teams":
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		name, _ := raw["name"].(string)
		desc, _ := raw["description"].(string)
		id := f.addTeam(name, desc)
		writeJSON(w, 201, f.teams[id])
	case strings.HasPrefix(path, "/api/v1/teams/") && strings.Contains(path, "/members/"):
		rest := strings.TrimPrefix(path, "/api/v1/teams/")
		idStr, login, _ := strings.Cut(rest, "/members/")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		m, ok := f.members[id]
		if !ok || f.byLogin(login) == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			if !m[login] {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, 200, map[string]any{})
		case http.MethodPut:
			m[login] = true
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			delete(m, login)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func page[T any](all []T, r *http.Request) []T {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	l, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if p < 1 || l < 1 {
		return all
	}
	from, to := min((p-1)*l, len(all)), min(p*l, len(all))
	return all[from:to]
}

func desiredFor(id uuid.UUID, username string, access, managed bool, teams ...string) provider.DesiredAccess {
	d := provider.DesiredAccess{PersonID: id, Username: username, DisplayName: "Maya Okafor", WorkEmail: username + "@acme.test", Access: access, Managed: managed, Teams: []provider.TeamRef{}}
	for _, t := range teams {
		d.Teams = append(d.Teams, provider.TeamRef{ExternalRef: t})
	}
	return d
}

// kinds returns the plan's action keys: the first in plan order (account
// actions come first), team actions sorted.
func kinds(p provider.Plan) []string {
	var account, teams []string
	for _, a := range p.Actions {
		if strings.Contains(a.Key, "team:") {
			teams = append(teams, a.Key)
		} else {
			account = append(account, a.Key)
		}
	}
	slices.Sort(teams)
	return append(account, teams...)
}

func TestPlan(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	mapped := f.addTeam("platform", teamMarker+" (team Platform).")
	other := f.addTeam("platform-old", teamMarker+" (team Old).")
	manual := f.addTeam("contractors", "")
	ref, oldRef, manualRef := strconv.FormatInt(mapped, 10), strconv.FormatInt(other, 10), strconv.FormatInt(manual, 10)
	person := uuid.New()

	// No account: create it, then its teams.
	p, err := a.Plan(t.Context(), env(), inst, desiredFor(person, "maya", true, false, ref, ""))
	if err != nil || !slices.Equal(kinds(p), []string{"create_account", "add_to_team:" + ref}) {
		t.Fatalf("new account plan = %v, %v", kinds(p), err)
	}
	if p, _ := a.Plan(t.Context(), env(), inst, desiredFor(person, "maya", false, true)); len(p.Actions) != 0 {
		t.Fatalf("no access, no account: %v", kinds(p))
	}

	// An account bound to the source with the subject, but no link
	// recorded (a lost answer): the create ensure finds it; a changed name
	// and login are updated; Owners and manual teams are never touched.
	u := f.addUser("maya.old", fakeSource, person.String())
	for _, id := range []int64{other, manual, f.owners} {
		f.members[id]["maya.old"] = true
	}
	p, err = a.Plan(t.Context(), env(), inst, desiredFor(person, "maya", true, true, ref))
	want := []string{"create_account", "update_account", "add_to_team:" + ref, "remove_from_team:" + oldRef}
	got := kinds(p)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("existing account plan = %v, %v; want %v", kinds(p), err, want)
	}
	for _, act := range p.Actions {
		if act.Kind == provider.ActionAddToTeam && act.Params[paramUsername] != "maya" {
			t.Errorf("team action for the old login: %+v", act)
		}
		if act.Key == "remove_from_team:"+manualRef {
			t.Error("a manual team removal was planned")
		}
		if act.Removal != (act.Kind == provider.ActionRemoveFromTeam) {
			t.Errorf("removal flag of %s", act.Key)
		}
	}

	// Linked: no create; not managed: no removal; a mapping without its
	// team yet: no removal either.
	linked := desiredFor(person, "maya.old", true, true, ref)
	linked.DisplayName, linked.WorkEmail = u.FullName, u.Email
	linked.Link = &provider.AccountLink{Account: provider.ExternalAccount{ExternalID: strconv.FormatInt(u.ID, 10)}}
	if p, _ := a.Plan(t.Context(), env(), inst, linked); !slices.Equal(kinds(p), []string{"add_to_team:" + ref, "remove_from_team:" + oldRef}) {
		t.Fatalf("linked plan = %v", kinds(p))
	}
	unmanaged := linked
	unmanaged.Managed = false
	if p, _ := a.Plan(t.Context(), env(), inst, unmanaged); !slices.Equal(kinds(p), []string{"add_to_team:" + ref}) {
		t.Fatalf("unmanaged plan = %v", kinds(p))
	}
	unsettled := linked
	unsettled.Teams = append(unsettled.Teams, provider.TeamRef{})
	if p, _ := a.Plan(t.Context(), env(), inst, unsettled); !slices.Equal(kinds(p), []string{"add_to_team:" + ref}) {
		t.Fatalf("unsettled plan = %v", kinds(p))
	}

	// No access: removals from Orglane's teams only, the account stays.
	gone := linked
	gone.Access, gone.Teams = false, []provider.TeamRef{}
	f.members[mapped]["maya.old"] = true
	got = kinds(must(a.Plan(t.Context(), env(), inst, gone)))
	if want := []string{"remove_from_team:" + ref, "remove_from_team:" + oldRef}; !slices.Equal(got, want) {
		t.Fatalf("no access plan = %v, want %v", got, want)
	}

	// Plan failures are classified (the core retries or fails the person).
	f.fail["GET /api/v1/orgs/acme/teams"] = []int{http.StatusTooManyRequests}
	_, err = a.Plan(t.Context(), env(), inst, linked)
	if c, retry, after := provider.Classify(err); c != CodeRateLimited || !retry || after != 7*time.Second {
		t.Fatalf("rate limited plan: %v (%s, %v, %v)", err, c, retry, after)
	}
}

func must(p provider.Plan, err error) provider.Plan {
	if err != nil {
		panic(err)
	}
	return p
}

func TestCreateAccount(t *testing.T) {
	a := New()
	create := func(person uuid.UUID, login string) provider.Action {
		return createAction(desiredFor(person, login, true, false))
	}
	f, inst := newFakeAdmin(t)
	person := uuid.New()
	out, err := a.Apply(t.Context(), env(), inst, create(person, "maya"))
	if err != nil || !out.Changed || out.Proof == nil || out.Proof.Kind != provider.ProofResponse || out.Link == nil || out.Link.Login != "maya" {
		t.Fatalf("create = %+v, %v", out, err)
	}
	if u := f.byLogin("maya"); u.SourceID != fakeSource || u.LoginName != person.String() {
		t.Fatalf("account = %+v", u)
	}
	// Again (a retry): unchanged, proven by the marker.
	out, err = a.Apply(t.Context(), env(), inst, create(person, "maya"))
	if err != nil || out.Changed || out.Proof == nil || out.Proof.Kind != provider.ProofMarker {
		t.Fatalf("ensure = %+v, %v", out, err)
	}

	// Refused because an attempt whose answer was lost created it: the
	// read-back finds it.
	lost := uuid.New()
	f.beforeCreate = func(f *fakeAdmin) { f.addUser("nora", fakeSource, lost.String()) }
	if out, err := a.Apply(t.Context(), env(), inst, create(lost, "nora")); err != nil || out.Proof == nil || out.Proof.Kind != provider.ProofMarker {
		t.Fatalf("lost create = %+v, %v", out, err)
	}

	// The username belongs to another account (local, or another source):
	// permanent, never adopted.
	f.addUser("chen", 0, "")
	_, err = a.Apply(t.Context(), env(), inst, create(uuid.New(), "chen"))
	if c, retry, _ := provider.Classify(err); c != CodeUsernameTaken || retry {
		t.Fatalf("taken = %v (%s, %v)", err, c, retry)
	}
	someone := uuid.New()
	f.addUser("aiko", fakeSource, uuid.NewString()) // another person's
	if _, err := a.Apply(t.Context(), env(), inst, create(someone, "aiko")); code(err) != CodeUsernameTaken {
		t.Fatalf("another person's account: %v", err)
	}

	for name, c := range map[string]struct {
		fail  map[string][]int
		code  string
		retry bool
	}{
		"server error":  {map[string][]int{"POST /api/v1/admin/users": {502}}, CodeUnavailable, true},
		"rate limited":  {map[string][]int{"POST /api/v1/admin/users": {429}}, CodeRateLimited, true},
		"auth":          {map[string][]int{"POST /api/v1/admin/users": {401}}, CodeAdminAuth, true},
		"refused data":  {map[string][]int{"POST /api/v1/admin/users": {422}}, CodeAccountRefused, false},
		"lookup fails":  {map[string][]int{"GET /api/v1/admin/users": {503}}, CodeUnavailable, true},
		"readback fail": {map[string][]int{"POST /api/v1/admin/users": {422}, "GET /api/v1/admin/users": {0, 503}}, CodeUnavailable, true},
	} {
		t.Run(name, func(t *testing.T) {
			f, inst := newFakeAdmin(t)
			f.fail = c.fail
			_, err := a.Apply(t.Context(), env(), inst, create(uuid.New(), "lucas"))
			got, retry, _ := provider.Classify(err)
			if err == nil || got != c.code || retry != c.retry {
				t.Fatalf("error = %v (%s, %v), want %s, %v", err, got, retry, c.code, c.retry)
			}
		})
	}
}

func TestUpdateAccountAndTeams(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	person := uuid.New()
	f.addUser("maya.old", fakeSource, person.String())
	f.addUser("local", 0, "") // never reached by an update
	tm := f.addTeam("platform", teamMarker)
	f.members[tm]["maya.old"] = true
	update := provider.Action{Kind: provider.ActionUpdateAccount, Key: "update_account", Params: map[string]string{
		paramPersonID: person.String(), paramCurrent: "maya.old", paramUsername: "maya", paramDisplayName: "Maya O.", paramEmail: "maya@acme.test",
	}}
	out, err := a.Apply(t.Context(), env(), inst, update)
	if err != nil || !out.Changed || f.byLogin("maya") == nil || f.byLogin("maya").FullName != "Maya O." || !f.members[tm]["maya"] {
		t.Fatalf("update = %+v, %v", out, err)
	}
	if out, err := a.Apply(t.Context(), env(), inst, update); err != nil || out.Changed {
		t.Fatalf("update again = %+v, %v", out, err)
	}
	clash := update
	clash.Params = map[string]string{paramPersonID: person.String(), paramUsername: "local", paramDisplayName: "Maya O.", paramEmail: "maya@acme.test"}
	if _, err := a.Apply(t.Context(), env(), inst, clash); code(err) != CodeUsernameTaken {
		t.Fatalf("rename onto another account: %v", err)
	}
	missing := update
	missing.Params = map[string]string{paramPersonID: uuid.NewString(), paramUsername: "x"}
	if _, err := a.Apply(t.Context(), env(), inst, missing); code(err) != CodeNoAccount {
		t.Fatalf("no account: %v", err)
	}

	// Team membership ensures.
	id := strconv.FormatInt(tm, 10)
	remove := provider.Action{Kind: provider.ActionRemoveFromTeam, Key: "remove_from_team:" + id, Removal: true, Params: map[string]string{paramUsername: "maya", paramTeam: id}}
	add := provider.Action{Kind: provider.ActionAddToTeam, Key: "add_to_team:" + id, Params: map[string]string{paramUsername: "maya", paramTeam: id}}
	for _, step := range []struct {
		a       provider.Action
		changed bool
		member  bool
	}{{remove, true, false}, {remove, false, false}, {add, true, true}, {add, false, true}} {
		out, err := a.Apply(t.Context(), env(), inst, step.a)
		if err != nil || out.Changed != step.changed || f.members[tm]["maya"] != step.member {
			t.Fatalf("%s = %+v, %v (member %v)", step.a.Key, out, err, f.members[tm]["maya"])
		}
	}
	gone := add
	gone.Params = map[string]string{paramUsername: "nobody", paramTeam: id}
	if _, err := a.Apply(t.Context(), env(), inst, gone); code(err) != CodeNoAccount {
		t.Fatalf("add a missing account: %v", err)
	}
	f.fail["PUT /api/v1/teams/"+id+"/members/maya"] = []int{http.StatusInternalServerError}
	f.members[tm] = map[string]bool{}
	if _, err := a.Apply(t.Context(), env(), inst, add); code(err) != CodeUnavailable {
		t.Fatalf("add fails: %v", err)
	}
}

func TestSyncTeamsAndMembers(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	existing := f.addTeam("platform", teamMarker)
	f.addTeam("sales", "") // somebody else's team with the mapped name
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	mappings := []provider.TeamMapping{
		{TeamID: ids[0], TeamSlug: "platform", TeamName: "Platform", Mode: provider.MappingCreate},
		{TeamID: ids[1], TeamSlug: "product", TeamName: "Product", Mode: provider.MappingCreate},
		{TeamID: ids[2], TeamSlug: "sales", TeamName: "Sales", Mode: provider.MappingCreate},
		{TeamID: ids[3], TeamSlug: "hardware", Mode: provider.MappingCreate, ExternalRef: "999"}, // its team was deleted
	}
	refs, err := a.SyncTeams(t.Context(), env(), inst, mappings)
	if err != nil {
		t.Fatal(err)
	}
	byTeam := map[uuid.UUID]string{}
	for _, r := range refs {
		byTeam[r.TeamID] = r.ExternalRef
	}
	if byTeam[ids[0]] != strconv.FormatInt(existing, 10) || byTeam[ids[1]] == "" || byTeam[ids[3]] == "" || byTeam[ids[3]] == "999" {
		t.Fatalf("refs = %+v", refs)
	}
	if _, ok := byTeam[ids[2]]; ok {
		t.Fatal("a team that is not Orglane's was taken over")
	}
	created, _ := strconv.ParseInt(byTeam[ids[1]], 10, 64)
	if tm := f.teams[created]; tm == nil || tm.Name != "product" || !tm.managed() {
		t.Fatalf("created team = %+v", tm)
	}
	// Idempotent: the same refs, nothing new.
	n := len(f.teams)
	again, err := a.SyncTeams(t.Context(), env(), inst, mappings)
	if err != nil || len(again) != len(refs) || len(f.teams) != n {
		t.Fatalf("again: %+v, %v (%d teams, was %d)", again, err, len(f.teams), n)
	}
	// A failed creation reports what is synced and the error.
	f.fail["POST /api/v1/orgs/acme/teams"] = []int{http.StatusBadGateway}
	partial, err := a.SyncTeams(t.Context(), env(), inst, append(mappings, provider.TeamMapping{TeamID: uuid.New(), TeamSlug: "ops", Mode: provider.MappingCreate}))
	if code(err) != CodeUnavailable || len(partial) != len(refs) {
		t.Fatalf("partial = %+v, %v", partial, err)
	}

	f.addUser("maya", fakeSource, uuid.NewString())
	members, err := a.Members(t.Context(), env(), inst)
	if err != nil || len(members) != 1 || members[0].Login != "maya" || members[0].Realm != inst.ID.String() {
		t.Fatalf("members = %+v, %v (the service admin is no member)", members, err)
	}
	if _, err := a.Members(t.Context(), testEnv{creds: map[string][]byte{CredentialAdminPassword: []byte("wrong")}}, inst); code(err) != CodeAdminAuth {
		t.Fatalf("members with a wrong password: %v", err)
	}
}
