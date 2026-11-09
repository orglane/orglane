package githubfake

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The organization's people and teams: members with their role,
// invitations with their inviter, and teams with their memberships. Like
// GitHub, removing a member or canceling an invitation also ends the
// person's team memberships, and adding someone who is not in the
// organization to a team invites them.

// Org roles (GET /orgs/{org}/memberships/{username} "role").
const (
	RoleMember = "member"
	RoleOwner  = "admin"
)

// Team roles.
const (
	TeamMember     = "member"
	TeamMaintainer = "maintainer"
)

// Invitation is a pending organization invitation.
type Invitation struct {
	ID      int64
	Invitee int64
	// Inviter is the login of whoever invited: a person, or the App's bot
	// ("<slug>[bot]") for invitations made with an installation token.
	Inviter string
	Teams   []int64
}

// Team is an organization team.
type Team struct {
	ID          int64
	Name        string
	Slug        string
	Description string
	// Enterprise marks an enterprise team (GitHub lists them with the
	// organization's teams; they are not the organization's).
	Enterprise bool
}

type teamState struct {
	Team
	members map[int64]teamMembership
}

type teamMembership struct{ role, state string }

type orgState struct {
	members     map[int64]string // user ID -> role
	invitations map[int64]*Invitation
	teams       map[int64]*teamState
	nextID      int64
	// removals holds the reads left before a queued organization removal
	// completes: GitHub accepted the removal but still serves the
	// membership for a moment (QueueOrgRemovals).
	removals map[int64]int
	// indirect names the enterprise teams that grant a member's membership
	// instead of a direct one (GitHub keeps such a membership when the
	// direct one is removed).
	indirect map[int64][]string
}

func newOrgState() orgState {
	return orgState{members: map[int64]string{}, invitations: map[int64]*Invitation{}, teams: map[int64]*teamState{}, nextID: 5000, removals: map[int64]int{}, indirect: map[int64][]string{}}
}

// BotLogin is the login GitHub shows for the App's own actions.
func (f *Fake) BotLogin() string { return f.app.Slug + "[bot]" }

// AddMember makes a user a member of the organization with role
// (RoleMember or RoleOwner), as if they had joined outside Orglane.
func (f *Fake) AddMember(userID int64, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.members[userID] = role
}

// SetRole changes a member's role.
func (f *Fake) SetRole(userID int64, role string) { f.AddMember(userID, role) }

// AddIndirectMember makes an organization member whose membership is granted
// by enterprise teams instead of directly. GitHub reports direct_membership
// false and keeps the membership when the organization membership is
// removed (docs: "Remove organization membership for a user").
func (f *Fake) AddIndirectMember(userID int64, role string, enterpriseTeams []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.members[userID] = role
	f.state.indirect[userID] = append([]string(nil), enterpriseTeams...)
}

// Members returns the organization's members and their roles.
func (f *Fake) Members() map[int64]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[int64]string, len(f.state.members))
	for k, v := range f.state.members {
		out[k] = v
	}
	return out
}

// InviteAs records an invitation of user by inviter (a login), as if made
// outside Orglane.
func (f *Fake) InviteAs(userID int64, inviter string) Invitation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.invite(userID, inviter, nil)
}

func (f *Fake) invite(userID int64, inviter string, teams []int64) *Invitation {
	f.state.nextID++
	inv := &Invitation{ID: f.state.nextID, Invitee: userID, Inviter: inviter, Teams: teams}
	f.state.invitations[inv.ID] = inv
	return inv
}

// Invitations returns the pending invitations.
func (f *Fake) Invitations() []Invitation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Invitation, 0, len(f.state.invitations))
	for _, inv := range f.state.invitations {
		out = append(out, *inv)
	}
	slices.SortFunc(out, func(a, b Invitation) int { return int(a.ID - b.ID) })
	return out
}

// Accept accepts the user's pending invitation: they become a member and
// their pending team memberships become active.
func (f *Fake) Accept(userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, inv := range f.state.invitations {
		if inv.Invitee == userID {
			delete(f.state.invitations, id)
			f.state.members[userID] = RoleMember
		}
	}
	for _, t := range f.state.teams {
		if m, ok := t.members[userID]; ok && m.state == "pending" {
			t.members[userID] = teamMembership{role: m.role, state: "active"}
		}
	}
}

// AddTeam creates a team, as if made outside Orglane.
func (f *Fake) AddTeam(name, description string) Team {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addTeam(name, description).Team
}

// AddEnterpriseTeam creates an enterprise team that GitHub lists with the
// organization's teams.
func (f *Fake) AddEnterpriseTeam(name string) Team {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.addTeam(name, "")
	t.Enterprise = true
	return t.Team
}

var slugChars = regexp.MustCompile(`[^a-z0-9]+`)

func (f *Fake) addTeam(name, description string) *teamState {
	f.state.nextID++
	slug := strings.Trim(slugChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	t := &teamState{Team: Team{ID: f.state.nextID, Name: name, Slug: slug, Description: description}, members: map[int64]teamMembership{}}
	f.state.teams[t.ID] = t
	return t
}

// Teams returns the organization's teams.
func (f *Fake) Teams() []Team {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Team, 0, len(f.state.teams))
	for _, t := range f.state.teams {
		out = append(out, t.Team)
	}
	slices.SortFunc(out, func(a, b Team) int { return int(a.ID - b.ID) })
	return out
}

// TeamMembers returns a team's memberships: user ID -> role and state
// ("member/active", "maintainer/pending").
func (f *Fake) TeamMembers(teamID int64) map[int64]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]string{}
	if t := f.state.teams[teamID]; t != nil {
		for u, m := range t.members {
			out[u] = m.role + "/" + m.state
		}
	}
	return out
}

// SetTeamMember adds a user to a team with role, as if done outside
// Orglane (active if they are an organization member, else pending).
func (f *Fake) SetTeamMember(teamID, userID int64, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.state.teams[teamID]; t != nil {
		state := "pending"
		if _, ok := f.state.members[userID]; ok {
			state = "active"
		}
		t.members[userID] = teamMembership{role: role, state: state}
	}
}

// RenameTeam renames a team, as a person can on GitHub. Its slug follows the
// name, so a rename releases the old slug for another team to take.
func (f *Fake) RenameTeam(teamID int64, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.state.teams[teamID]; t != nil {
		t.Name = name
		t.Slug = strings.Trim(slugChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	}
}

// DeleteTeam deletes a team and its memberships.
func (f *Fake) DeleteTeam(teamID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.state.teams, teamID)
}

// DeleteUser deletes a user account (and everything it had).
func (f *Fake) DeleteUser(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.users, id)
	f.leave(id)
}

// RemoveMember ends a user's organization membership (and its invitations and
// team memberships) without deleting the account, as if GitHub completed an
// accepted removal.
func (f *Fake) RemoveMember(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leave(id)
}

// RemoveTeamMember removes a user's membership of a team without touching the
// account or the organization.
func (f *Fake) RemoveTeamMember(teamID, userID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.state.teams[teamID]; t != nil {
		delete(t.members, userID)
	}
}

// QueueOrgRemovals makes organization membership removals asynchronous for
// tests: the DELETE still answers 204, but the membership (and its team
// memberships) remains visible for the next readbacks membership reads. The
// read that consumes the last readback writes the 200 response and then ends
// the membership, so later reads get GitHub's 404. readbacks <= 0 restores
// synchronous removal.
func (f *Fake) QueueOrgRemovals(readbacks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.orgRemovalReadbacks = max(readbacks, 0)
}

// leave ends a user's membership, invitations and team memberships.
func (f *Fake) leave(id int64) {
	delete(f.state.members, id)
	delete(f.state.removals, id)
	delete(f.state.indirect, id)
	for k, inv := range f.state.invitations {
		if inv.Invitee == id {
			delete(f.state.invitations, k)
		}
	}
	for _, t := range f.state.teams {
		delete(t.members, id)
	}
}

func (f *Fake) userByLogin(login string) (User, bool) {
	for _, u := range f.users {
		if strings.EqualFold(u.Login, login) {
			return u, true
		}
	}
	return User{}, false
}

func (f *Fake) pendingInvitation(userID int64) *Invitation {
	for _, inv := range f.state.invitations {
		if inv.Invitee == userID {
			return inv
		}
	}
	return nil
}

func (f *Fake) teamBySlug(slug string) *teamState {
	for _, t := range f.state.teams {
		if strings.EqualFold(t.Slug, slug) && !t.Enterprise {
			return t
		}
	}
	return nil
}

// orgRoute checks that a request under /orgs/{org}/... is for the fake's
// organization and is made with an installation token.
func (f *Fake) orgRoute(w http.ResponseWriter, r *http.Request, org string) bool {
	if !f.installationAuth(w, r) {
		return false
	}
	if !strings.EqualFold(org, f.org.Login) {
		basicError(w, http.StatusNotFound, "Not Found")
		return false
	}
	return true
}

// orgIDRoute checks that a request under /organizations/{org_id}/... is for
// the fake's organization (by its immutable ID) and is made with an
// installation token.
func (f *Fake) orgIDRoute(w http.ResponseWriter, r *http.Request, orgID string) bool {
	if !f.installationAuth(w, r) {
		return false
	}
	if orgID != strconv.FormatInt(f.org.ID, 10) {
		basicError(w, http.StatusNotFound, "Not Found")
		return false
	}
	return true
}

func (f *Fake) installationAuth(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(bearer(r), "ghs_") || !f.installationTokens[bearer(r)] {
		basicError(w, http.StatusUnauthorized, "Bad credentials")
		return false
	}
	return true
}

// page applies GitHub's per_page/page parameters to n items.
func page(r *http.Request, n int) (lo, hi int) {
	per, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || per <= 0 {
		per = 30
	}
	per = min(per, 100)
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p <= 0 {
		p = 1
	}
	lo = min((p-1)*per, n)
	return lo, min(lo+per, n)
}

// userObject builds a simple-user or public-user from GitHub's examples.
func (f *Fake) userObject(u User, kind string) map[string]any {
	var body map[string]any
	if kind == "public" {
		body = example("public-user-default-response")
		body["user_view_type"] = "public"
		body["name"], body["email"], body["bio"], body["company"], body["location"], body["blog"], body["twitter_username"] = nil, nil, nil, nil, nil, "", nil
	} else {
		body = exampleItem("simple-user-items")
	}
	typ := "User"
	if strings.HasSuffix(u.Login, "[bot]") {
		typ = "Bot"
	}
	body["id"], body["login"], body["type"] = u.ID, u.Login, typ
	body["node_id"] = "U_" + strconv.FormatInt(u.ID, 10)
	body["html_url"] = "https://github.com/" + u.Login
	body["url"] = "https://api.github.com/users/" + u.Login
	return body
}

func (f *Fake) getUserByID(w http.ResponseWriter, r *http.Request, id string) {
	if !f.anyAuth(r) {
		basicError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	n, _ := strconv.ParseInt(id, 10, 64)
	u, ok := f.users[n]
	if !ok {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, f.userObject(u, "public"))
}

func (f *Fake) membership(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !f.orgRoute(w, r, p["org"]) {
		return
	}
	u, ok := f.userByLogin(p["username"])
	if !ok {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	role, member := f.state.members[u.ID]
	inv := f.pendingInvitation(u.ID)
	switch r.Method {
	case http.MethodGet:
		state := "active"
		switch {
		case member:
		case inv != nil:
			state, role = "pending", RoleMember
		default:
			basicError(w, http.StatusNotFound, "Not Found")
			return
		}
		body := example("org-membership-response-if-user-has-an-active-admin-membership-with-organization")
		body["state"], body["role"] = state, role
		body["direct_membership"] = true
		body["enterprise_teams_providing_indirect_membership"] = []string{}
		if teams, ok := f.state.indirect[u.ID]; ok {
			body["direct_membership"] = false
			body["enterprise_teams_providing_indirect_membership"] = teams
		}
		body["url"] = "https://api.github.com/orgs/" + f.org.Login + "/memberships/" + u.Login
		body["organization_url"] = "https://api.github.com/orgs/" + f.org.Login
		org, _ := body["organization"].(map[string]any)
		org["id"], org["login"] = f.org.ID, f.org.Login
		body["user"] = f.userObject(u, "simple")
		writeJSON(w, http.StatusOK, body)
		// A queued removal finishes after the last read that still sees the
		// membership; later reads get GitHub's 404.
		if n := f.state.removals[u.ID]; n > 0 {
			if n == 1 {
				f.leave(u.ID)
			} else {
				f.state.removals[u.ID] = n - 1
			}
		}
	case http.MethodDelete:
		if !member && inv == nil {
			basicError(w, http.StatusNotFound, "Not Found")
			return
		}
		if role == RoleOwner && f.owners() == 1 {
			basicError(w, http.StatusForbidden, "You cannot remove the last owner of an organization.")
			return
		}
		if _, indirect := f.state.indirect[u.ID]; indirect {
			// GitHub removes only the direct membership; the enterprise
			// teams keep the membership.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if f.orgRemovalReadbacks > 0 {
			f.state.removals[u.ID] = f.orgRemovalReadbacks
		} else {
			f.leave(u.ID)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *Fake) owners() int {
	n := 0
	for _, role := range f.state.members {
		if role == RoleOwner {
			n++
		}
	}
	return n
}

func (f *Fake) invitationObject(inv *Invitation) map[string]any {
	body := example("organization-invitation")
	u := f.users[inv.Invitee]
	body["id"], body["login"], body["email"], body["role"] = inv.ID, u.Login, nil, "direct_member"
	body["node_id"] = "OI_" + strconv.FormatInt(inv.ID, 10)
	body["team_count"] = len(inv.Teams)
	body["invitation_teams_url"] = "https://api.github.com/organizations/" + strconv.FormatInt(f.org.ID, 10) + "/invitations/" + strconv.FormatInt(inv.ID, 10) + "/teams"
	inviter := User{ID: 1, Login: inv.Inviter}
	if inv.Inviter == f.BotLogin() {
		inviter.ID = 41000 + f.app.ID
	} else if u, ok := f.userByLogin(inv.Inviter); ok {
		inviter = u
	}
	body["inviter"] = f.userObject(inviter, "simple")
	delete(body, "failed_at")
	delete(body, "failed_reason")
	return body
}

func (f *Fake) invitations(w http.ResponseWriter, r *http.Request, p map[string]string, body []byte) {
	if !f.orgRoute(w, r, p["org"]) {
		return
	}
	if r.Method == http.MethodGet {
		all := make([]*Invitation, 0, len(f.state.invitations))
		for _, inv := range f.state.invitations {
			all = append(all, inv)
		}
		slices.SortFunc(all, func(a, b *Invitation) int { return int(a.ID - b.ID) })
		lo, hi := page(r, len(all))
		out := make([]map[string]any, 0, hi-lo)
		for _, inv := range all[lo:hi] {
			out = append(out, f.invitationObject(inv))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var in struct {
		InviteeID int64   `json:"invitee_id"`
		Role      string  `json:"role"`
		TeamIDs   []int64 `json:"team_ids"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.InviteeID == 0 {
		validationFailed(w)
		return
	}
	if _, ok := f.users[in.InviteeID]; !ok {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	if _, member := f.state.members[in.InviteeID]; member || f.pendingInvitation(in.InviteeID) != nil || (in.Role != "" && in.Role != "direct_member") {
		validationFailed(w)
		return
	}
	if f.seats > 0 && len(f.state.members)+len(f.state.invitations) >= f.seats {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "You must purchase at least one more seat to add this user as a member.", "documentation_url": "https://docs.github.com/rest"})
		return
	}
	writeJSON(w, http.StatusCreated, f.invitationObject(f.invite(in.InviteeID, f.BotLogin(), in.TeamIDs)))
}

func (f *Fake) cancelInvitation(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !f.orgRoute(w, r, p["org"]) {
		return
	}
	id, _ := strconv.ParseInt(p["invitation_id"], 10, 64)
	inv, ok := f.state.invitations[id]
	if !ok {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	delete(f.state.invitations, id)
	for _, t := range f.state.teams {
		if m, ok := t.members[inv.Invitee]; ok && m.state == "pending" {
			delete(t.members, inv.Invitee)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) listMembers(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !f.orgRoute(w, r, p["org"]) {
		return
	}
	ids := make([]int64, 0, len(f.state.members))
	for id := range f.state.members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	lo, hi := page(r, len(ids))
	out := make([]map[string]any, 0, hi-lo)
	for _, id := range ids[lo:hi] {
		out = append(out, f.userObject(f.users[id], "simple"))
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) teamObject(t *teamState, full bool) map[string]any {
	var body map[string]any
	if full {
		body = example("team-full")
		org, _ := body["organization"].(map[string]any)
		org["id"], org["login"], org["archived_at"] = f.org.ID, f.org.Login, nil
		body["members_count"], body["repos_count"] = len(t.members), 0
		body["privacy"] = "closed"
	} else {
		body = exampleItem("team-items")
	}
	body["id"], body["name"], body["slug"], body["description"] = t.ID, t.Name, t.Slug, t.Description
	body["node_id"] = "T_" + strconv.FormatInt(t.ID, 10)
	body["permission"], body["parent"] = "pull", nil
	body["type"] = "organization"
	if t.Enterprise {
		body["type"] = "enterprise"
	}
	body["url"] = "https://api.github.com/teams/" + strconv.FormatInt(t.ID, 10)
	return body
}

// getTeamByID serves the documented team-ID alternate of the slug team read.
func (f *Fake) getTeamByID(w http.ResponseWriter, r *http.Request, p map[string]string) {
	if !f.orgIDRoute(w, r, p["org_id"]) {
		return
	}
	id, _ := strconv.ParseInt(p["team_id"], 10, 64)
	t := f.state.teams[id]
	if t == nil || t.Enterprise {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, f.teamObject(t, true))
}

func (f *Fake) teams(w http.ResponseWriter, r *http.Request, p map[string]string, body []byte) {
	if !f.orgRoute(w, r, p["org"]) {
		return
	}
	if r.Method == http.MethodGet {
		all := make([]*teamState, 0, len(f.state.teams))
		for _, t := range f.state.teams {
			all = append(all, t)
		}
		slices.SortFunc(all, func(a, b *teamState) int { return strings.Compare(a.Slug, b.Slug) })
		lo, hi := page(r, len(all))
		out := make([]map[string]any, 0, hi-lo)
		for _, t := range all[lo:hi] {
			out = append(out, f.teamObject(t, false))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		validationFailed(w)
		return
	}
	slug := strings.Trim(slugChars.ReplaceAllString(strings.ToLower(in.Name), "-"), "-")
	if f.teamBySlug(slug) != nil {
		validationFailed(w) // "Name must be unique for this org"
		return
	}
	writeJSON(w, http.StatusCreated, f.teamObject(f.addTeam(in.Name, in.Description), true))
}

func (f *Fake) teamMembership(w http.ResponseWriter, r *http.Request, p map[string]string, body []byte) {
	if !f.orgRoute(w, r, p["org"]) {
		return
	}
	f.teamMembershipFor(w, r, f.teamBySlug(p["team_slug"]), p["username"], body)
}

// teamMembershipByID serves the documented team-ID alternate of the slug
// membership route: the organization and team are named by their immutable
// IDs, so a renamed team whose old slug another team took is not confused.
func (f *Fake) teamMembershipByID(w http.ResponseWriter, r *http.Request, p map[string]string, body []byte) {
	if !f.orgIDRoute(w, r, p["org_id"]) {
		return
	}
	id, _ := strconv.ParseInt(p["team_id"], 10, 64)
	f.teamMembershipFor(w, r, f.state.teams[id], p["username"], body)
}

func (f *Fake) teamMembershipFor(w http.ResponseWriter, r *http.Request, t *teamState, username string, body []byte) {
	u, ok := f.userByLogin(username)
	if t == nil || t.Enterprise || !ok {
		basicError(w, http.StatusNotFound, "Not Found")
		return
	}
	url := "https://api.github.com/teams/" + strconv.FormatInt(t.ID, 10) + "/memberships/" + u.Login
	switch r.Method {
	case http.MethodGet:
		m, ok := t.members[u.ID]
		if !ok {
			basicError(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"role": m.role, "state": m.state, "url": url})
	case http.MethodPut:
		var in struct {
			Role string `json:"role"`
		}
		if len(body) > 0 && json.Unmarshal(body, &in) != nil {
			validationFailed(w)
			return
		}
		if in.Role == "" {
			in.Role = TeamMember
		}
		state := "active"
		if _, member := f.state.members[u.ID]; !member {
			// GitHub invites people who are not in the organization.
			state = "pending"
			if inv := f.pendingInvitation(u.ID); inv == nil {
				f.invite(u.ID, f.BotLogin(), []int64{t.ID})
			}
		}
		t.members[u.ID] = teamMembership{role: in.Role, state: state}
		writeJSON(w, http.StatusOK, map[string]any{"role": in.Role, "state": state, "url": url})
	case http.MethodDelete:
		if _, ok := t.members[u.ID]; !ok {
			basicError(w, http.StatusNotFound, "Not Found")
			return
		}
		delete(t.members, u.ID)
		w.WriteHeader(http.StatusNoContent)
	}
}
