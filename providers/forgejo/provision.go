package forgejo

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/orglane/orglane/providers/provider"
)

// Provisioning (docs/specs/providers.md "Provisioning", "Provider specifics
// → Forgejo"). Every person with access gets an account that Orglane
// creates, bound to the Orglane sign-in source with the person's OIDC
// subject (the person ID) as login name, so their first sign-in matches it.
// That binding is also the marker that proves the account is Orglane's, and
// every action finds the account by it when it runs, never by a login saved
// in the plan (a login can be renamed and reused meanwhile).
//
// Teams: Orglane creates one organization team per "create" mapping, named
// after the Orglane team slug and marked by its description (teamMarker),
// and one base team (baseTeamName) that every account Orglane manages is
// in. Only marked teams are Orglane's. The adapter adds people to the teams
// they are mapped to and removes managed accounts from marked teams they
// are no longer mapped to, re-checking the mark when the removal runs. It
// never touches other teams and never the Owners team (whatever its
// description says), so it never makes anyone an owner. "existing" mappings
// are refused: Forgejo teams are all Orglane's or not at all.
//
// The base team keeps organization membership: in Forgejo 15 removing an
// account from its last team of an organization also removes it from the
// organization (models/org_team.go, removeInvalidOrgUser). Every managed
// account is in the base team before any removal runs, so a removal is never
// from the last team, and nobody is removed from the organization
// (deprovision.org_removal is unsupported). No account is deleted
// (suspension is M5).

// Action parameters.
const (
	paramPersonID    = "person_id"
	paramUsername    = "username"      // create and update: the login the account must have
	paramAccountID   = "account_id"    // team removals: the account seen when planning (informational)
	paramCurrent     = "current_login" // update_account: the login seen when planning (informational)
	paramDisplayName = "display_name"
	paramEmail       = "email"
	paramTeam        = "team" // Forgejo team ID
)

// Error codes of provisioning.
const (
	// CodeUsernameTaken: another Forgejo account (not Orglane's for this
	// person) has the username. An admin resolves it.
	CodeUsernameTaken = "username_taken"
	// CodeInvalidLogin: Forgejo does not allow the username (reserved, or
	// characters it refuses), and no account has it.
	CodeInvalidLogin = "invalid_login"
	// CodeAccountRefused: Forgejo refused the account's data (e.g. an email
	// that another account uses).
	CodeAccountRefused = "account_refused"
	// CodeTeamNameTaken: a team with the mapped name exists but is not
	// Orglane's.
	CodeTeamNameTaken = "team_name_taken"
	// CodeNoAccount: the account an action is for does not exist (yet).
	CodeNoAccount = "no_account"
	// CodeTeamNotSynced: a team the person is mapped to (or the base team)
	// does not exist in Forgejo yet; the next team sync creates it.
	// Retryable.
	CodeTeamNotSynced = "team_not_synced"
	// CodeTeamNotManaged: a team an action is for is not Orglane's (no mark,
	// the Owners team, another organization). Permanent.
	CodeTeamNotManaged = "team_not_managed"
)

// baseTeamName is the team every managed account is in (organization
// membership, no repository access).
const baseTeamName = "orglane-members"

// teamMarker starts the description of the teams Orglane creates and
// manages.
const teamMarker = "Managed by Orglane"

// teamUnits are the repository units a mapped team writes to; the team's
// repositories are chosen by the organization's owners in Forgejo.
var teamUnits = map[string]string{
	"repo.code": "write", "repo.issues": "write", "repo.pulls": "write", "repo.releases": "write",
	"repo.wiki": "write", "repo.projects": "write", "repo.packages": "read", "repo.actions": "read",
	"repo.ext_issues": "read", "repo.ext_wiki": "read",
}

// baseUnits: the base team grants no repository access.
var baseUnits = map[string]string{
	"repo.code": "none", "repo.issues": "none", "repo.pulls": "none", "repo.releases": "none",
	"repo.wiki": "none", "repo.projects": "none", "repo.packages": "none", "repo.actions": "none",
	"repo.ext_issues": "none", "repo.ext_wiki": "none",
}

const pageSize = 50

// user is a Forgejo account as the admin API returns it.
type user struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	LoginName string `json:"login_name"`
	SourceID  int64  `json:"source_id"`
	FullName  string `json:"full_name"`
	Email     string `json:"email"`
}

// team is a Forgejo organization team.
type team struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Permission   string `json:"permission"`
	Organization *struct {
		UserName string `json:"username"`
	} `json:"organization"`
}

// owners reports whether the team is the organization's Owners team, which
// Orglane never changes, whatever its description says.
func (t team) owners() bool {
	return t.Permission == "owner" || strings.EqualFold(t.Name, "owners")
}

// managed: Orglane's team (marked, never Owners).
func (t team) managed() bool { return strings.HasPrefix(t.Description, teamMarker) && !t.owners() }

// base: Orglane's base team.
func (t team) base() bool { return t.managed() && strings.EqualFold(t.Name, baseTeamName) }

// mappable: a team people are mapped to (Orglane's, not the base team).
func (t team) mappable() bool { return t.managed() && !t.base() }

func (t team) id() string { return strconv.FormatInt(t.ID, 10) }

func (inst instance) account(u user) *provider.ExternalAccount {
	return &provider.ExternalAccount{Realm: inst.ID.String(), ExternalID: strconv.FormatInt(u.ID, 10), Login: u.Login}
}

// instance wraps provider.Instance for small helpers.
type instance struct{ provider.Instance }

func (inst instance) organization() string {
	org, _ := inst.Settings[SettingOrganization].(string)
	return org
}

// get pages through a list endpoint.
func pages[T any](ctx context.Context, c *client, path string, q url.Values) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		v := url.Values{}
		for k, vs := range q {
			v[k] = vs
		}
		v.Set("limit", strconv.Itoa(pageSize))
		v.Set("page", strconv.Itoa(page))
		var list []T
		if err := c.doQuery(ctx, http.MethodGet, path, v, nil, &list); err != nil {
			return nil, err
		}
		all = append(all, list...)
		if len(list) < pageSize {
			return all, nil
		}
	}
}

// orglaneAccount returns the account bound to the Orglane source with the
// person's subject, nil if there is none.
func orglaneAccount(ctx context.Context, c *client, sourceID int64, subject string) (*user, error) {
	users, err := pages[user](ctx, c, "/api/v1/admin/users", url.Values{
		"source_id": {strconv.FormatInt(sourceID, 10)}, "login_name": {subject},
	})
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		// The API filters; checking again keeps a filter Forgejo ignores
		// from matching someone else.
		if u.SourceID == sourceID && u.LoginName == subject {
			return &u, nil
		}
	}
	return nil, nil
}

func orgTeams(ctx context.Context, c *client, org string) ([]team, error) {
	return pages[team](ctx, c, "/api/v1/orgs/"+url.PathEscape(org)+"/teams", nil)
}

func teamPath(id string) string { return "/api/v1/teams/" + url.PathEscape(id) }

func memberPath(id, login string) string {
	return teamPath(id) + "/members/" + url.PathEscape(login)
}

// isMember reports whether login is in the team.
func isMember(ctx context.Context, c *client, teamID, login string) (bool, error) {
	err := c.do(ctx, http.MethodGet, memberPath(teamID, login), nil, nil)
	switch {
	case err == nil:
		return true, nil
	case status(err) == http.StatusNotFound:
		return false, nil
	}
	return false, err
}

// Plan implements provider.UserProvisioner. It only reads. Actions come in
// this order: the account (create, update), then every addition (the base
// team first), then the removals, so a removal never runs before the grant
// that replaces it (the core stops at the first failure). Removals are
// planned only for managed accounts, only from mappable teams, and only when
// every team the person is mapped to exists; a missing one is a retryable
// error (CodeTeamNotSynced), never a plan that ends without it.
func (*Adapter) Plan(ctx context.Context, env provider.Env, pi provider.Instance, d provider.DesiredAccess) (provider.Plan, error) {
	inst := instance{pi}
	c, err := newClient(ctx, env, pi, true)
	if err != nil {
		return provider.Plan{}, err
	}
	sourceID, err := authSourceID(ctx, c)
	if err != nil {
		return provider.Plan{}, err
	}
	acct, err := orglaneAccount(ctx, c, sourceID, d.PersonID.String())
	if err != nil {
		return provider.Plan{}, classify(err)
	}
	var plan provider.Plan
	if acct == nil && !d.Access {
		return plan, nil
	}
	teams, err := orgTeams(ctx, c, inst.organization())
	if err != nil {
		return provider.Plan{}, classify(err)
	}
	var base *team
	byID := map[string]team{}
	for _, t := range teams {
		byID[t.id()] = t
		if t.base() {
			base = &t
		}
	}
	if base == nil {
		return provider.Plan{}, &provider.Error{Code: CodeTeamNotSynced, Retryable: true, Err: errors.New("forgejo: the base team does not exist yet")}
	}
	var wanted []team
	want := map[string]bool{}
	for _, ref := range d.Teams {
		t, ok := byID[ref.ExternalRef]
		switch {
		case ref.ExternalRef == "" || !ok:
			return provider.Plan{}, &provider.Error{Code: CodeTeamNotSynced, Retryable: true, Err: fmt.Errorf("forgejo: mapped team %q does not exist", ref.ExternalRef)}
		case !t.mappable():
			return provider.Plan{}, &provider.Error{Code: CodeTeamNotManaged, Err: fmt.Errorf("forgejo: mapped team %s is not Orglane's", ref.ExternalRef)}
		}
		if !want[t.id()] {
			want[t.id()] = true
			wanted = append(wanted, t)
		}
	}
	slices.SortFunc(wanted, func(a, b team) int { return cmp.Compare(a.ID, b.ID) })

	if acct == nil {
		plan.Actions = append(plan.Actions, createAction(d), addAction(d, *base))
		for _, t := range wanted {
			plan.Actions = append(plan.Actions, addAction(d, t))
		}
		return plan, nil
	}
	accountID := strconv.FormatInt(acct.ID, 10)
	if d.Access {
		if d.Link == nil || d.Link.Account.ExternalID != accountID {
			// The account is there but its link is not recorded (an answer
			// lost): the ensure finds it and proves it by the marker.
			plan.Actions = append(plan.Actions, createAction(d))
		}
		staleLink := d.Link != nil && d.Link.Account.ExternalID == accountID && d.Link.Account.Login != acct.Login
		if acct.Login != d.Username || acct.FullName != d.DisplayName || !strings.EqualFold(acct.Email, d.WorkEmail) || staleLink {
			plan.Actions = append(plan.Actions, provider.Action{
				Kind: provider.ActionUpdateAccount, Key: "update_account",
				Params: map[string]string{
					paramPersonID: d.PersonID.String(), paramCurrent: acct.Login, paramUsername: d.Username,
					paramDisplayName: d.DisplayName, paramEmail: d.WorkEmail,
				},
			})
		}
	}

	var additions, removals []provider.Action
	for _, t := range teams {
		if !t.managed() {
			continue
		}
		in, err := isMember(ctx, c, t.id(), acct.Login)
		if err != nil {
			return provider.Plan{}, classify(err)
		}
		switch {
		case t.base():
			// Base membership: for everyone with access, and before any
			// removal (so that removal is not from the last team).
			if !in && (d.Access || d.Managed) {
				additions = append(additions, addAction(d, t))
			}
		case want[t.id()] && !in:
			additions = append(additions, addAction(d, t))
		case !want[t.id()] && in && d.Managed:
			removals = append(removals, provider.Action{
				Kind: provider.ActionRemoveFromTeam, Key: "remove_from_team:" + t.id(), Removal: true,
				Params: map[string]string{paramPersonID: d.PersonID.String(), paramAccountID: accountID, paramTeam: t.id()},
			})
		}
	}
	if len(removals) == 0 && !d.Access {
		// Nothing to take away: do not touch a person without access.
		additions = nil
	}
	plan.Actions = append(plan.Actions, additions...)
	plan.Actions = append(plan.Actions, removals...)
	return plan, nil
}

func addAction(d provider.DesiredAccess, t team) provider.Action {
	return provider.Action{
		Kind: provider.ActionAddToTeam, Key: "add_to_team:" + t.id(),
		Params: map[string]string{paramPersonID: d.PersonID.String(), paramTeam: t.id()},
	}
}

func createAction(d provider.DesiredAccess) provider.Action {
	return provider.Action{
		Kind: provider.ActionCreateAccount, Key: "create_account",
		Params: map[string]string{
			paramPersonID: d.PersonID.String(), paramUsername: d.Username, paramDisplayName: d.DisplayName, paramEmail: d.WorkEmail,
		},
	}
}

// Apply implements provider.UserProvisioner. Every action is an ensure: an
// already satisfied action succeeds unchanged.
func (*Adapter) Apply(ctx context.Context, env provider.Env, pi provider.Instance, a provider.Action) (provider.ActionOutcome, error) {
	inst := instance{pi}
	c, err := newClient(ctx, env, pi, true)
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	switch a.Kind {
	case provider.ActionCreateAccount:
		return createAccount(ctx, c, inst, a.Params)
	case provider.ActionUpdateAccount:
		return updateAccount(ctx, c, inst, a.Params)
	case provider.ActionAddToTeam:
		return teamMember(ctx, c, inst, a.Params, true)
	case provider.ActionRemoveFromTeam:
		return teamMember(ctx, c, inst, a.Params, false)
	}
	return provider.ActionOutcome{}, &provider.Error{Code: "unsupported_action", Err: fmt.Errorf("forgejo: %s", a.Kind)}
}

// createAccount makes the person's account exist. Proof that it is
// Orglane's: the confirmed creation (response) or, for an account that is
// already there, its binding to the Orglane source with the person's
// subject (marker), which only Orglane's admin calls make.
func createAccount(ctx context.Context, c *client, inst instance, p map[string]string) (provider.ActionOutcome, error) {
	sourceID, err := authSourceID(ctx, c)
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	subject := p[paramPersonID]
	if acct, err := orglaneAccount(ctx, c, sourceID, subject); err != nil {
		return provider.ActionOutcome{}, classify(err)
	} else if acct != nil {
		return provider.ActionOutcome{Link: inst.account(*acct), Proof: &provider.Proof{Kind: provider.ProofMarker, Evidence: "auth_source"}}, nil
	}
	var created user
	err = c.do(ctx, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username": p[paramUsername], "email": p[paramEmail], "full_name": p[paramDisplayName],
		"source_id": sourceID, "login_name": subject, "must_change_password": false, "send_notify": false,
	}, &created)
	if err == nil {
		if created.ID == 0 || created.SourceID != sourceID || created.LoginName != subject {
			return provider.ActionOutcome{}, &provider.Error{Code: CodeUnavailable, Retryable: true, Err: errors.New("forgejo: the created account is not bound to the Orglane source")}
		}
		return provider.ActionOutcome{Changed: true, Link: inst.account(created), Proof: &provider.Proof{Kind: provider.ProofResponse, Evidence: "create_account"}}, nil
	}
	if !definite(err) {
		return provider.ActionOutcome{}, classify(err)
	}
	// Refused: maybe because an attempt whose answer was lost created it.
	// Read back before the refusal counts.
	if acct, rerr := orglaneAccount(ctx, c, sourceID, subject); rerr != nil {
		return provider.ActionOutcome{}, classify(rerr)
	} else if acct != nil {
		return provider.ActionOutcome{Link: inst.account(*acct), Proof: &provider.Proof{Kind: provider.ProofMarker, Evidence: "auth_source"}}, nil
	}
	if s := status(err); s == http.StatusUnauthorized || s == http.StatusForbidden {
		return provider.ActionOutcome{}, classify(err)
	}
	// Only a successful lookup tells a taken username from refused data;
	// a failed one keeps its own classification (rate limit, outage).
	taken, terr := usernameExists(ctx, c, p[paramUsername])
	switch {
	case terr != nil:
		return provider.ActionOutcome{}, classify(terr)
	case taken:
		return provider.ActionOutcome{}, &provider.Error{Code: CodeUsernameTaken, Err: err}
	}
	return provider.ActionOutcome{}, &provider.Error{Code: CodeAccountRefused, Err: err}
}

func usernameExists(ctx context.Context, c *client, login string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/api/v1/users/"+url.PathEscape(login), nil, nil)
	switch {
	case err == nil:
		return true, nil
	case status(err) == http.StatusNotFound:
		return false, nil
	}
	return false, err
}

// updateAccount makes the account's login, name and email the person's.
// The account is found by its binding, never by login, so an update cannot
// reach another account.
func updateAccount(ctx context.Context, c *client, inst instance, p map[string]string) (provider.ActionOutcome, error) {
	sourceID, err := authSourceID(ctx, c)
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	acct, err := orglaneAccount(ctx, c, sourceID, p[paramPersonID])
	if err != nil {
		return provider.ActionOutcome{}, classify(err)
	}
	if acct == nil {
		return provider.ActionOutcome{}, &provider.Error{Code: CodeNoAccount, Retryable: true, Err: errors.New("forgejo: the account to update does not exist")}
	}
	changed := false
	if acct.Login != p[paramUsername] {
		err := c.do(ctx, http.MethodPost, "/api/v1/admin/users/"+url.PathEscape(acct.Login)+"/rename", map[string]any{"new_username": p[paramUsername]}, nil)
		if err != nil {
			if !refused(err) {
				return provider.ActionOutcome{}, classify(err)
			}
			// Only a successful lookup tells a taken username from one
			// Forgejo does not allow (reserved, invalid characters).
			taken, terr := usernameExists(ctx, c, p[paramUsername])
			switch {
			case terr != nil:
				return provider.ActionOutcome{}, classify(terr)
			case taken:
				return provider.ActionOutcome{}, &provider.Error{Code: CodeUsernameTaken, Err: err}
			}
			return provider.ActionOutcome{}, &provider.Error{Code: CodeInvalidLogin, Err: err}
		}
		acct.Login, changed = p[paramUsername], true
	}
	if acct.FullName != p[paramDisplayName] || !strings.EqualFold(acct.Email, p[paramEmail]) {
		err := c.do(ctx, http.MethodPatch, "/api/v1/admin/users/"+url.PathEscape(acct.Login), map[string]any{
			"source_id": sourceID, "login_name": p[paramPersonID], "full_name": p[paramDisplayName], "email": p[paramEmail],
		}, nil)
		if err != nil {
			return provider.ActionOutcome{}, refusal(err, CodeAccountRefused)
		}
		changed = true
	}
	return provider.ActionOutcome{Changed: changed, Link: inst.account(*acct)}, nil
}

// refused reports whether Forgejo refused a change's data (validation).
func refused(err error) bool {
	s := status(err)
	return s == http.StatusUnprocessableEntity || s == http.StatusBadRequest || s == http.StatusConflict
}

// refusal classifies a failed update: a validation refusal is permanent
// with code, anything else as usual.
func refusal(err error, code string) error {
	if refused(err) {
		return &provider.Error{Code: code, Err: err}
	}
	return classify(err)
}

// teamMember ensures membership (add) or its absence (remove) of the
// person's account, found by its binding now (never by a login saved in the
// plan), in a team that is Orglane's now (re-read, never trusted from the
// plan). A removal first makes sure the account is in the base team, so it
// is never from the account's last team (which would take it out of the
// organization). A removal whose team is no longer Orglane's is dropped
// unchanged; an addition to such a team is refused.
func teamMember(ctx context.Context, c *client, inst instance, p map[string]string, add bool) (provider.ActionOutcome, error) {
	sourceID, err := authSourceID(ctx, c)
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	acct, err := orglaneAccount(ctx, c, sourceID, p[paramPersonID])
	switch {
	case err != nil:
		return provider.ActionOutcome{}, classify(err)
	case acct == nil && add:
		return provider.ActionOutcome{}, &provider.Error{Code: CodeNoAccount, Retryable: true, Err: errors.New("forgejo: the account does not exist")}
	case acct == nil:
		return provider.ActionOutcome{}, nil // no account, no membership
	}
	id := p[paramTeam]
	var t team
	if err := c.do(ctx, http.MethodGet, teamPath(id), nil, &t); err != nil {
		switch {
		case status(err) == http.StatusNotFound && add:
			return provider.ActionOutcome{}, &provider.Error{Code: CodeTeamNotSynced, Retryable: true, Err: err}
		case status(err) == http.StatusNotFound:
			return provider.ActionOutcome{Link: inst.account(*acct)}, nil // the team is gone, and the membership with it
		}
		return provider.ActionOutcome{}, classify(err)
	}
	ours := t.managed() && t.Organization != nil && strings.EqualFold(t.Organization.UserName, inst.organization())
	if !add && (!ours || t.base()) {
		return provider.ActionOutcome{Link: inst.account(*acct)}, nil // not Orglane's (any more): dropped
	}
	if add && !ours {
		return provider.ActionOutcome{}, &provider.Error{Code: CodeTeamNotManaged, Err: fmt.Errorf("forgejo: team %s is not Orglane's", id)}
	}
	if !add {
		if err := ensureBase(ctx, c, inst, acct.Login); err != nil {
			return provider.ActionOutcome{}, err
		}
	}
	in, err := isMember(ctx, c, id, acct.Login)
	if err != nil {
		return provider.ActionOutcome{}, classify(err)
	}
	out := provider.ActionOutcome{Link: inst.account(*acct)}
	if in == add {
		return out, nil
	}
	method := http.MethodPut
	if !add {
		method = http.MethodDelete
	}
	if err := c.do(ctx, method, memberPath(id, acct.Login), nil, nil); err != nil {
		if status(err) == http.StatusNotFound && !add {
			return out, nil // gone meanwhile
		}
		return provider.ActionOutcome{}, classify(err)
	}
	out.Changed = true
	return out, nil
}

// ensureBase makes the account a member of the base team.
func ensureBase(ctx context.Context, c *client, inst instance, login string) error {
	teams, err := orgTeams(ctx, c, inst.organization())
	if err != nil {
		return classify(err)
	}
	for _, t := range teams {
		if !t.base() {
			continue
		}
		in, err := isMember(ctx, c, t.id(), login)
		if err != nil {
			return classify(err)
		}
		if in {
			return nil
		}
		if err := c.do(ctx, http.MethodPut, memberPath(t.id(), login), nil, nil); err != nil {
			return classify(err)
		}
		return nil
	}
	return &provider.Error{Code: CodeTeamNotSynced, Retryable: true, Err: errors.New("forgejo: the base team does not exist yet")}
}

// SyncTeams implements provider.UserProvisioner. The base team exists
// first; then every "create" mapping gets an organization team named after
// the Orglane team slug, marked as Orglane's. A mapping keeps its team while
// that team exists and is still Orglane's; otherwise it is found by name
// (if that team is Orglane's) or created. A team with the name that is not
// Orglane's (or the base team's name) is never taken over: that mapping
// stays unsynced (logged with CodeTeamNameTaken) and the others sync,
// because an error would hold back the whole instance's provisioning.
// "existing" mappings are refused when they are set (ValidateTeamMapping)
// and never synced.
func (*Adapter) SyncTeams(ctx context.Context, env provider.Env, pi provider.Instance, mappings []provider.TeamMapping) ([]provider.TeamRef, error) {
	inst := instance{pi}
	c, err := newClient(ctx, env, pi, true)
	if err != nil {
		return nil, err
	}
	org := inst.organization()
	teams, err := orgTeams(ctx, c, org)
	if err != nil {
		return nil, classify(err)
	}
	byID, byName := map[string]team{}, map[string]team{}
	hasBase := false
	for _, t := range teams {
		byID[t.id()] = t
		byName[strings.ToLower(t.Name)] = t
		hasBase = hasBase || t.base()
	}
	var conflicts []string
	create := func(name, description string, units map[string]string) (team, error) {
		var created team
		err := c.do(ctx, http.MethodPost, "/api/v1/orgs/"+url.PathEscape(org)+"/teams", map[string]any{
			"name": name, "description": description,
			"includes_all_repositories": false, "can_create_org_repo": false, "units_map": units,
		}, &created)
		return created, err
	}
	if !hasBase {
		if _, taken := byName[baseTeamName]; taken {
			conflicts = append(conflicts, baseTeamName)
		} else if _, err := create(baseTeamName, teamMarker+": every account Orglane manages (organization membership, no repositories).", baseUnits); err != nil {
			return nil, classify(err)
		}
	}
	var refs []provider.TeamRef
	for _, m := range mappings {
		if m.Mode != provider.MappingCreate {
			continue
		}
		if t, ok := byID[m.ExternalRef]; ok && m.ExternalRef != "" && t.mappable() {
			refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: m.ExternalRef, Name: t.Name})
			continue
		}
		if t, ok := byName[strings.ToLower(m.TeamSlug)]; ok || strings.EqualFold(m.TeamSlug, baseTeamName) {
			if !ok || !t.mappable() {
				conflicts = append(conflicts, m.TeamSlug)
				continue
			}
			refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: t.id(), Name: t.Name})
			continue
		}
		created, err := create(m.TeamSlug, teamMarker+" (team "+m.TeamName+"). Members change through Orglane.", teamUnits)
		if err != nil {
			// A refusal may mean another run created it meanwhile: the next
			// sync finds it by name. Report what is synced so far.
			return refs, classify(err)
		}
		byName[strings.ToLower(created.Name)] = created
		refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: created.id(), Name: created.Name})
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		env.Logger().WarnContext(ctx, "forgejo: team names are taken by teams that are not Orglane's; those teams stay unsynced",
			"code", CodeTeamNameTaken, "teams", strings.Join(conflicts, ","))
	}
	return refs, nil
}

// ValidateTeamMapping implements provider.TeamMappingValidator: Forgejo
// teams are created by Orglane ("create"); picking an existing team could
// hand out a team Orglane does not control, such as Owners.
func (*Adapter) ValidateTeamMapping(_ provider.Instance, mode, _ string) error {
	if mode != provider.MappingCreate {
		return errors.New("only mode create is supported: Orglane creates the Forgejo teams")
	}
	return nil
}

// Members implements provider.UserProvisioner: every account of the
// instance except the service admin.
func (*Adapter) Members(ctx context.Context, env provider.Env, pi provider.Instance) ([]provider.ExternalAccount, error) {
	inst := instance{pi}
	c, err := newClient(ctx, env, pi, true)
	if err != nil {
		return nil, err
	}
	users, err := pages[user](ctx, c, "/api/v1/admin/users", nil)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]provider.ExternalAccount, 0, len(users))
	for _, u := range users {
		if u.Login == AdminUsername {
			continue
		}
		out = append(out, *inst.account(u))
	}
	return out, nil
}
