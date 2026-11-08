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
	base    int64
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
	f.teams[f.owners].Permission = "owner"
	f.base = f.addTeam(baseTeamName, teamMarker+": base")
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
	t := &team{ID: f.nextID, Name: name, Description: desc, Permission: "write"}
	t.Organization = &struct {
		UserName string `json:"username"`
	}{UserName: "acme"}
	f.teams[f.nextID] = t
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
		for _, t := range f.teams {
			if strings.EqualFold(t.Name, name) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				return
			}
		}
		id := f.addTeam(name, desc)
		writeJSON(w, 201, f.teams[id])
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/teams/") && !strings.Contains(path, "/members/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/api/v1/teams/"), 10, 64)
		t, ok := f.teams[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, 200, t)
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
	d := provider.DesiredAccess{PersonID: id, Username: username, DisplayName: username, WorkEmail: username + "@acme.test", Access: access, Managed: managed, Teams: []provider.TeamRef{}}
	for _, t := range teams {
		d.Teams = append(d.Teams, provider.TeamRef{ExternalRef: t})
	}
	return d
}

// keys returns the plan's action keys in plan order.
func keys(p provider.Plan) []string {
	var out []string
	for _, a := range p.Actions {
		out = append(out, a.Key)
	}
	return out
}

func ref(id int64) string { return strconv.FormatInt(id, 10) }

// linked returns d with a link to the account and its current data.
func linked(d provider.DesiredAccess, u *user) provider.DesiredAccess {
	d.Link = &provider.AccountLink{Account: provider.ExternalAccount{ExternalID: ref(u.ID), Login: u.Login}}
	d.Username, d.DisplayName, d.WorkEmail = u.Login, u.FullName, u.Email
	return d
}

func TestPlan(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	platform := f.addTeam("platform", teamMarker+" (team Platform).")
	old := f.addTeam("platform-old", teamMarker+" (team Old).")
	manual := f.addTeam("contractors", "")
	person := uuid.New()
	base := "add_to_team:" + ref(f.base)

	// No account: create it, the base team, then its teams.
	p, err := a.Plan(t.Context(), env(), inst, desiredFor(person, "maya", true, false, ref(platform)))
	if err != nil || !slices.Equal(keys(p), []string{"create_account", base, "add_to_team:" + ref(platform)}) {
		t.Fatalf("new account plan = %v, %v", keys(p), err)
	}
	if p, _ := a.Plan(t.Context(), env(), inst, desiredFor(person, "maya", false, true)); len(p.Actions) != 0 {
		t.Fatalf("no access, no account: %v", keys(p))
	}

	// An account without its link recorded (a lost answer), renamed and in
	// the old team, Owners and a manual team: create ensure, update, then
	// every addition, then the removal. Owners and the manual team are
	// never touched.
	u := f.addUser("maya.old", fakeSource, person.String())
	for _, id := range []int64{old, manual, f.owners} {
		f.members[id]["maya.old"] = true
	}
	p, err = a.Plan(t.Context(), env(), inst, desiredFor(person, "maya", true, true, ref(platform)))
	want := []string{"create_account", "update_account", base, "add_to_team:" + ref(platform), "remove_from_team:" + ref(old)}
	if err != nil || !slices.Equal(keys(p), want) {
		t.Fatalf("existing account plan = %v, %v; want %v", keys(p), err, want)
	}

	// Grants come before removals whatever the teams' order: moving from a
	// team listed first to one listed later still adds first.
	f.members[f.base]["maya.old"] = true
	later := f.addTeam("product", teamMarker+" (team Product).")
	f.members[platform]["maya.old"] = true
	move := linked(desiredFor(person, "", true, true, ref(later)), u)
	got := keys(must(a.Plan(t.Context(), env(), inst, move)))
	if !slices.Equal(got, []string{"add_to_team:" + ref(later), "remove_from_team:" + ref(platform), "remove_from_team:" + ref(old)}) {
		t.Fatalf("move plan = %v: removals must follow the additions", got)
	}
	for _, act := range must(a.Plan(t.Context(), env(), inst, move)).Actions {
		if act.Removal != (act.Kind == provider.ActionRemoveFromTeam) {
			t.Errorf("removal flag of %s", act.Key)
		}
	}

	// Not managed: no removal, and the base team is never left.
	unmanaged := move
	unmanaged.Managed = false
	if got := keys(must(a.Plan(t.Context(), env(), inst, unmanaged))); !slices.Equal(got, []string{"add_to_team:" + ref(later)}) {
		t.Fatalf("unmanaged plan = %v", got)
	}
	// No access: removals from Orglane's mappable teams only.
	gone := move
	gone.Access, gone.Teams = false, []provider.TeamRef{}
	if got := keys(must(a.Plan(t.Context(), env(), inst, gone))); !slices.Equal(got, []string{"remove_from_team:" + ref(platform), "remove_from_team:" + ref(old)}) {
		t.Fatalf("no access plan = %v", got)
	}

	// A destination that is not synced (empty or deleted) or not Orglane's
	// (Owners, a manual team, the base team): no plan at all, so nothing is
	// removed before the person has their new team.
	for name, c := range map[string]struct {
		ref  string
		code string
	}{
		"unsynced": {"", CodeTeamNotSynced}, "deleted": {"999", CodeTeamNotSynced},
		"owners": {ref(f.owners), CodeTeamNotManaged}, "manual": {ref(manual), CodeTeamNotManaged}, "base": {ref(f.base), CodeTeamNotManaged},
	} {
		d := move
		d.Teams = []provider.TeamRef{{ExternalRef: c.ref}}
		_, err := a.Plan(t.Context(), env(), inst, d)
		if got, retry, _ := provider.Classify(err); got != c.code || retry != (c.code == CodeTeamNotSynced) {
			t.Errorf("%s destination: %v", name, err)
		}
	}
	// Owners stays protected even if its description carries the mark.
	f.teams[f.owners].Description = teamMarker
	if got := keys(must(a.Plan(t.Context(), env(), inst, gone))); slices.Contains(got, "remove_from_team:"+ref(f.owners)) {
		t.Fatalf("a removal from Owners was planned: %v", got)
	}
	// Without the base team nothing is planned (the next sync creates it).
	delete(f.teams, f.base)
	if _, err := a.Plan(t.Context(), env(), inst, move); code(err) != CodeTeamNotSynced {
		t.Fatalf("no base team: %v", err)
	}

	// Plan failures keep their classification.
	f2, inst2 := newFakeAdmin(t)
	f2.fail["GET /api/v1/orgs/acme/teams"] = []int{http.StatusTooManyRequests}
	_, err = a.Plan(t.Context(), env(), inst2, desiredFor(uuid.New(), "x", true, false))
	if c, retry, after := provider.Classify(err); c != CodeRateLimited || !retry || after != 7*time.Second {
		t.Fatalf("rate limited plan: %v (%s, %v, %v)", err, c, retry, after)
	}
}

// A stale link login (a rename whose answer was lost) gets an update that
// reports the account, so the core refreshes the link.
func TestPlanRefreshesStaleLink(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	person := uuid.New()
	u := f.addUser("maya", fakeSource, person.String())
	f.members[f.base]["maya"] = true
	d := linked(desiredFor(person, "", true, true), u)
	d.Link.Account.Login = "maya.old"
	if got := keys(must(a.Plan(t.Context(), env(), inst, d))); !slices.Equal(got, []string{"update_account"}) {
		t.Fatalf("stale link plan = %v", got)
	}
	out, err := a.Apply(t.Context(), env(), inst, must(a.Plan(t.Context(), env(), inst, d)).Actions[0])
	if err != nil || out.Changed || out.Link == nil || out.Link.Login != "maya" || out.Link.ExternalID != ref(u.ID) {
		t.Fatalf("update = %+v, %v", out, err)
	}
}

// A move whose destination grant fails keeps the old membership: the plan
// lists the addition before the removal, and the core stops at the first
// failure (emulated here), so the removal never runs. The old team is
// listed before the destination, so plan order follows the action kind,
// not the teams' order.
func TestGrantsPrecedeRemovals(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	person := uuid.New()
	u := f.addUser("maya", fakeSource, person.String())
	old := f.addTeam("platform", teamMarker+" (team Platform).")
	dest := f.addTeam("product", teamMarker+" (team Product).")
	f.members[old]["maya"], f.members[f.base]["maya"] = true, true
	plan := must(a.Plan(t.Context(), env(), inst, linked(desiredFor(person, "", true, true, ref(dest)), u)))
	if !slices.Equal(keys(plan), []string{"add_to_team:" + ref(dest), "remove_from_team:" + ref(old)}) {
		t.Fatalf("move plan = %v", keys(plan))
	}
	f.fail["PUT /api/v1/teams/"+ref(dest)+"/members/maya"] = []int{http.StatusBadGateway}
	var failed error
	for _, act := range plan.Actions {
		if _, failed = a.Apply(t.Context(), env(), inst, act); failed != nil {
			break
		}
	}
	if code(failed) != CodeUnavailable || !f.members[old]["maya"] || f.members[dest]["maya"] {
		t.Fatalf("failed grant: %v; old team %v, destination %v", failed, f.members[old]["maya"], f.members[dest]["maya"])
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

	// The username belongs to another account (local, or another
	// person's): permanent, never adopted.
	f.addUser("chen", 0, "")
	if _, err := a.Apply(t.Context(), env(), inst, create(uuid.New(), "chen")); code(err) != CodeUsernameTaken {
		t.Fatalf("taken: %v", err)
	}
	f.addUser("aiko", fakeSource, uuid.NewString())
	if _, err := a.Apply(t.Context(), env(), inst, create(uuid.New(), "aiko")); code(err) != CodeUsernameTaken {
		t.Fatalf("another person's account: %v", err)
	}

	for name, c := range map[string]struct {
		fail  map[string][]int
		code  string
		retry bool
		after time.Duration
	}{
		"server error":  {map[string][]int{"POST /api/v1/admin/users": {502}}, CodeUnavailable, true, 0},
		"rate limited":  {map[string][]int{"POST /api/v1/admin/users": {429}}, CodeRateLimited, true, 7 * time.Second},
		"auth":          {map[string][]int{"POST /api/v1/admin/users": {401}}, CodeAdminAuth, true, 0},
		"refused data":  {map[string][]int{"POST /api/v1/admin/users": {422}}, CodeAccountRefused, false, 0},
		"lookup fails":  {map[string][]int{"GET /api/v1/admin/users": {503}}, CodeUnavailable, true, 0},
		"readback fail": {map[string][]int{"POST /api/v1/admin/users": {422}, "GET /api/v1/admin/users": {0, 503}}, CodeUnavailable, true, 0},
		// The username lookup after a refusal keeps its classification.
		"username lookup rate limited": {map[string][]int{"POST /api/v1/admin/users": {422}, "GET /api/v1/users/lucas": {429}}, CodeRateLimited, true, 7 * time.Second},
		"username lookup outage":       {map[string][]int{"POST /api/v1/admin/users": {422}, "GET /api/v1/users/lucas": {502}}, CodeUnavailable, true, 0},
		"username lookup auth":         {map[string][]int{"POST /api/v1/admin/users": {422}, "GET /api/v1/users/lucas": {401}}, CodeAdminAuth, true, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f, inst := newFakeAdmin(t)
			f.fail = c.fail
			_, err := a.Apply(t.Context(), env(), inst, create(uuid.New(), "lucas"))
			got, retry, after := provider.Classify(err)
			if err == nil || got != c.code || retry != c.retry || after != c.after {
				t.Fatalf("error = %v (%s, %v, %v), want %s, %v, %v", err, got, retry, after, c.code, c.retry, c.after)
			}
		})
	}
}

func TestUpdateAccount(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	person := uuid.New()
	f.addUser("maya.old", fakeSource, person.String())
	f.addUser("local", 0, "") // never reached by an update
	update := provider.Action{Kind: provider.ActionUpdateAccount, Key: "update_account", Params: map[string]string{
		paramPersonID: person.String(), paramCurrent: "maya.old", paramUsername: "maya", paramDisplayName: "Maya O.", paramEmail: "maya@acme.test",
	}}
	out, err := a.Apply(t.Context(), env(), inst, update)
	if err != nil || !out.Changed || f.byLogin("maya") == nil || f.byLogin("maya").FullName != "Maya O." || out.Link == nil || out.Link.Login != "maya" {
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
}

func teamAction(kind provider.ActionKind, person uuid.UUID, team int64) provider.Action {
	k := "add_to_team:"
	if kind == provider.ActionRemoveFromTeam {
		k = "remove_from_team:"
	}
	return provider.Action{Kind: kind, Key: k + ref(team), Removal: kind == provider.ActionRemoveFromTeam,
		Params: map[string]string{paramPersonID: person.String(), paramTeam: ref(team)}}
}

func TestTeamActions(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	person := uuid.New()
	f.addUser("maya", fakeSource, person.String())
	tm := f.addTeam("platform", teamMarker)
	add, remove := teamAction(provider.ActionAddToTeam, person, tm), teamAction(provider.ActionRemoveFromTeam, person, tm)
	for _, step := range []struct {
		a       provider.Action
		changed bool
		member  bool
	}{{add, true, true}, {add, false, true}, {remove, true, false}, {remove, false, false}} {
		out, err := a.Apply(t.Context(), env(), inst, step.a)
		if err != nil || out.Changed != step.changed || f.members[tm]["maya"] != step.member {
			t.Fatalf("%s = %+v, %v (member %v)", step.a.Key, out, err, f.members[tm]["maya"])
		}
	}
	// A removal keeps the account in the base team, so it is never from the
	// last team (Forgejo would take the account out of the organization).
	if !f.members[f.base]["maya"] {
		t.Fatal("a removal left the account without the base team")
	}

	// Additions only to Orglane's teams, never Owners or a manual team.
	for _, id := range []int64{f.owners, f.addTeam("contractors", "")} {
		if _, err := a.Apply(t.Context(), env(), inst, teamAction(provider.ActionAddToTeam, person, id)); code(err) != CodeTeamNotManaged {
			t.Fatalf("add to team %d: %v", id, err)
		}
	}
	if f.members[f.owners]["maya"] {
		t.Fatal("added to Owners")
	}
	if _, err := a.Apply(t.Context(), env(), inst, teamAction(provider.ActionAddToTeam, person, 999)); code(err) != CodeTeamNotSynced {
		t.Fatalf("add to a deleted team: %v", err)
	}
	if _, err := a.Apply(t.Context(), env(), inst, teamAction(provider.ActionAddToTeam, uuid.New(), tm)); code(err) != CodeNoAccount {
		t.Fatalf("add without an account: %v", err)
	}
	f.fail["PUT /api/v1/teams/"+ref(tm)+"/members/maya"] = []int{http.StatusInternalServerError}
	if _, err := a.Apply(t.Context(), env(), inst, add); code(err) != CodeUnavailable {
		t.Fatalf("add fails: %v", err)
	}
}

// A removal planned earlier and applied later (held by the guard) acts on
// the account bound to the person now, not on a login saved in the plan:
// the bound account was renamed and its old login reused by another
// account in the team meanwhile.
func TestDelayedRemovalResolvesBinding(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	person := uuid.New()
	u := f.addUser("maya", fakeSource, person.String())
	tm := f.addTeam("platform", teamMarker)
	f.members[tm]["maya"], f.members[f.base]["maya"] = true, true
	plan := must(a.Plan(t.Context(), env(), inst, linked(desiredFor(person, "", true, true), u)))
	if !slices.Equal(keys(plan), []string{"remove_from_team:" + ref(tm)}) {
		t.Fatalf("plan = %v", keys(plan))
	}
	// Meanwhile: renamed, and the old login reused by an unbound account.
	u.Login = "maya.renamed"
	delete(f.members[tm], "maya")
	f.members[tm]["maya.renamed"], f.members[f.base]["maya.renamed"] = true, true
	f.addUser("maya", 0, "")
	f.members[tm]["maya"] = true
	if _, err := a.Apply(t.Context(), env(), inst, plan.Actions[0]); err != nil {
		t.Fatal(err)
	}
	if f.members[tm]["maya.renamed"] || !f.members[tm]["maya"] {
		t.Fatalf("the removal hit the wrong account: %v", f.members[tm])
	}
}

// A removal re-checks the team when it runs: a team that is no longer
// Orglane's keeps the membership (the removal is dropped).
func TestDelayedRemovalRechecksTeamMarker(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	person := uuid.New()
	f.addUser("maya", fakeSource, person.String())
	tm := f.addTeam("platform", teamMarker)
	f.members[tm]["maya"], f.members[f.base]["maya"] = true, true
	remove := teamAction(provider.ActionRemoveFromTeam, person, tm)
	f.teams[tm].Description = "maintained by hand now"
	if out, err := a.Apply(t.Context(), env(), inst, remove); err != nil || out.Changed || !f.members[tm]["maya"] {
		t.Fatalf("removal from an unmarked team = %+v, %v (member %v)", out, err, f.members[tm]["maya"])
	}
	// The base team and Owners are never left through a removal.
	f.members[f.owners]["maya"] = true
	f.teams[f.owners].Description = teamMarker
	for _, id := range []int64{f.base, f.owners} {
		if out, err := a.Apply(t.Context(), env(), inst, teamAction(provider.ActionRemoveFromTeam, person, id)); err != nil || out.Changed || !f.members[id]["maya"] {
			t.Fatalf("removal from team %d = %+v, %v", id, out, err)
		}
	}
}

func TestSyncTeamsAndMembers(t *testing.T) {
	a := New()
	f, inst := newFakeAdmin(t)
	delete(f.teams, f.base) // a fresh organization
	existing := f.addTeam("platform", teamMarker)
	f.addTeam("sales", "") // somebody else's team with the mapped name
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	mappings := []provider.TeamMapping{
		{TeamID: ids[0], TeamSlug: "platform", TeamName: "Platform", Mode: provider.MappingCreate},
		{TeamID: ids[1], TeamSlug: "product", TeamName: "Product", Mode: provider.MappingCreate},
		{TeamID: ids[2], TeamSlug: "sales", TeamName: "Sales", Mode: provider.MappingCreate},
		{TeamID: ids[3], TeamSlug: "hardware", Mode: provider.MappingCreate, ExternalRef: "999"},      // its team was deleted
		{TeamID: ids[4], TeamSlug: "leads", Mode: provider.MappingCreate, ExternalRef: ref(f.owners)}, // a reference to Owners
		{TeamID: ids[5], TeamSlug: baseTeamName, Mode: provider.MappingCreate},                        // the base team's name
	}
	refs, err := a.SyncTeams(t.Context(), env(), inst, mappings)
	if err != nil {
		t.Fatal(err)
	}
	byTeam := map[uuid.UUID]string{}
	for _, r := range refs {
		byTeam[r.TeamID] = r.ExternalRef
	}
	if byTeam[ids[0]] != ref(existing) || byTeam[ids[1]] == "" || byTeam[ids[3]] == "" || byTeam[ids[3]] == "999" {
		t.Fatalf("refs = %+v", refs)
	}
	if r := byTeam[ids[4]]; r == ref(f.owners) || r == "" {
		t.Fatalf("a mapping kept its reference to Owners: %q", r)
	}
	for _, i := range []int{2, 5} {
		if _, ok := byTeam[ids[i]]; ok {
			t.Fatalf("mapping %s took over a team that is not its own", mappings[i].TeamSlug)
		}
	}
	var base *team
	for _, tm := range f.teams {
		if tm.base() {
			base = tm
		}
	}
	if base == nil {
		t.Fatal("the base team was not created")
	}
	created, _ := strconv.ParseInt(byTeam[ids[1]], 10, 64)
	if tm := f.teams[created]; tm == nil || tm.Name != "product" || !tm.mappable() {
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
	// Existing mappings are refused when they are set.
	if err := a.ValidateTeamMapping(inst, provider.MappingExisting, ref(f.owners)); err == nil {
		t.Fatal("an existing mapping was accepted")
	}
	if err := a.ValidateTeamMapping(inst, provider.MappingCreate, ""); err != nil {
		t.Fatal(err)
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
