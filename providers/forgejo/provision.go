package forgejo

import (
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
// That binding is also the marker that proves the account is Orglane's.
//
// Teams: Orglane creates one organization team per "create" mapping, named
// after the Orglane team slug and marked by its description (teamMarker).
// Only marked teams are Orglane's: the adapter adds people to the teams they
// are mapped to and removes managed accounts from marked teams they are no
// longer mapped to. It never touches other teams (Owners included), never
// removes anyone from the organization (deprovision.org_removal is
// unsupported) and never deletes an account (suspension is M5).

// Action parameters.
const (
	paramPersonID    = "person_id"
	paramUsername    = "username"      // the account's current login (team actions) or the target login
	paramCurrent     = "current_login" // update_account: the login before a rename
	paramDisplayName = "display_name"
	paramEmail       = "email"
	paramTeam        = "team" // Forgejo team ID
)

// Error codes of provisioning.
const (
	// CodeUsernameTaken: another Forgejo account (not Orglane's for this
	// person) has the username. An admin resolves it.
	CodeUsernameTaken = "username_taken"
	// CodeAccountRefused: Forgejo refused the account's data (e.g. an email
	// that another account uses).
	CodeAccountRefused = "account_refused"
	// CodeTeamNameTaken: a team with the mapped name exists but is not
	// Orglane's.
	CodeTeamNameTaken = "team_name_taken"
	// CodeNoAccount: the account an action is for does not exist (yet).
	CodeNoAccount = "no_account"
)

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
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (t team) managed() bool { return strings.HasPrefix(t.Description, teamMarker) }

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

// Plan implements provider.UserProvisioner. It only reads.
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
	if acct == nil {
		if !d.Access {
			return plan, nil
		}
		// A new account: create it, then add it to its teams (the core runs
		// them in order and stops at the first failure).
		plan.Actions = append(plan.Actions, createAction(d))
		for _, t := range d.Teams {
			if t.ExternalRef != "" {
				plan.Actions = append(plan.Actions, addAction(d, d.Username, t.ExternalRef))
			}
		}
		return plan, nil
	}
	if d.Access {
		if d.Link == nil || d.Link.Account.ExternalID != strconv.FormatInt(acct.ID, 10) {
			// The account is there but its link is not recorded (an answer
			// lost): the ensure finds it and proves it by the marker.
			plan.Actions = append(plan.Actions, createAction(d))
		}
		if acct.Login != d.Username || acct.FullName != d.DisplayName || !strings.EqualFold(acct.Email, d.WorkEmail) {
			plan.Actions = append(plan.Actions, provider.Action{
				Kind: provider.ActionUpdateAccount, Key: "update_account",
				Params: map[string]string{
					paramPersonID: d.PersonID.String(), paramCurrent: acct.Login, paramUsername: d.Username,
					paramDisplayName: d.DisplayName, paramEmail: d.WorkEmail,
				},
			})
		}
	}

	teams, err := orgTeams(ctx, c, inst.organization())
	if err != nil {
		return provider.Plan{}, classify(err)
	}
	want, settled := map[string]bool{}, true
	for _, t := range d.Teams {
		if t.ExternalRef == "" {
			settled = false // mapped, not synced yet
			continue
		}
		want[t.ExternalRef] = true
	}
	// Team actions use the login the account has after this plan's update.
	login := acct.Login
	if d.Access {
		login = d.Username
	}
	for _, t := range teams {
		id := strconv.FormatInt(t.ID, 10)
		if !t.managed() && !want[id] {
			continue
		}
		in, err := isMember(ctx, c, id, acct.Login)
		if err != nil {
			return provider.Plan{}, classify(err)
		}
		switch {
		case want[id] && !in:
			plan.Actions = append(plan.Actions, addAction(d, login, id))
		case !want[id] && in && t.managed() && d.Managed && settled:
			// Only managed accounts, only Orglane's teams, and only once every
			// mapping is synced (a mapping without its team yet could be
			// this one).
			plan.Actions = append(plan.Actions, provider.Action{
				Kind: provider.ActionRemoveFromTeam, Key: "remove_from_team:" + id, Removal: true,
				Params: map[string]string{paramPersonID: d.PersonID.String(), paramUsername: login, paramTeam: id},
			})
		}
	}
	return plan, nil
}

func addAction(d provider.DesiredAccess, login, team string) provider.Action {
	return provider.Action{
		Kind: provider.ActionAddToTeam, Key: "add_to_team:" + team,
		Params: map[string]string{paramPersonID: d.PersonID.String(), paramUsername: login, paramTeam: team},
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
		return teamMember(ctx, c, a.Params, true)
	case provider.ActionRemoveFromTeam:
		return teamMember(ctx, c, a.Params, false)
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
	if taken, terr := usernameExists(ctx, c, p[paramUsername]); terr == nil && taken {
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
			return provider.ActionOutcome{}, refusal(err, CodeUsernameTaken)
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

// refusal classifies a failed update: a validation refusal is permanent
// with code, anything else as usual.
func refusal(err error, code string) error {
	if s := status(err); s == http.StatusUnprocessableEntity || s == http.StatusBadRequest || s == http.StatusConflict {
		return &provider.Error{Code: code, Err: err}
	}
	return classify(err)
}

// teamMember ensures membership (add) or its absence (remove).
func teamMember(ctx context.Context, c *client, p map[string]string, add bool) (provider.ActionOutcome, error) {
	id, login := p[paramTeam], p[paramUsername]
	in, err := isMember(ctx, c, id, login)
	if err != nil {
		if status(err) == http.StatusNotFound {
			return provider.ActionOutcome{}, &provider.Error{Code: CodeNoAccount, Retryable: true, Err: err}
		}
		return provider.ActionOutcome{}, classify(err)
	}
	if in == add {
		return provider.ActionOutcome{}, nil
	}
	method := http.MethodPut
	if !add {
		method = http.MethodDelete
	}
	if err := c.do(ctx, method, memberPath(id, login), nil, nil); err != nil {
		if !add && status(err) == http.StatusNotFound {
			return provider.ActionOutcome{}, nil // gone meanwhile
		}
		if add && status(err) == http.StatusNotFound {
			// The account (renamed or not created yet) or the team is gone.
			return provider.ActionOutcome{}, &provider.Error{Code: CodeNoAccount, Retryable: true, Err: err}
		}
		return provider.ActionOutcome{}, classify(err)
	}
	return provider.ActionOutcome{Changed: true}, nil
}

// SyncTeams implements provider.UserProvisioner: every "create" mapping
// gets an organization team named after the Orglane team slug, marked as
// Orglane's. A mapping keeps its team while that team exists; a missing one
// is found by name (if it is Orglane's) or created. A team with the name
// that is not Orglane's is never taken over: that mapping stays unsynced
// (logged with CodeTeamNameTaken) and the others sync, because an error
// would hold back the whole instance's provisioning.
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
	for _, t := range teams {
		byID[strconv.FormatInt(t.ID, 10)] = t
		byName[strings.ToLower(t.Name)] = t
	}
	var refs []provider.TeamRef
	var conflicts []string
	for _, m := range mappings {
		if m.Mode != provider.MappingCreate {
			continue // Forgejo has no "existing" mappings
		}
		if t, ok := byID[m.ExternalRef]; ok && m.ExternalRef != "" {
			refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: m.ExternalRef, Name: t.Name})
			continue
		}
		if t, ok := byName[strings.ToLower(m.TeamSlug)]; ok {
			if !t.managed() {
				conflicts = append(conflicts, m.TeamSlug)
				continue
			}
			refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: strconv.FormatInt(t.ID, 10), Name: t.Name})
			continue
		}
		var created team
		err := c.do(ctx, http.MethodPost, "/api/v1/orgs/"+url.PathEscape(org)+"/teams", map[string]any{
			"name": m.TeamSlug, "description": teamMarker + " (team " + m.TeamName + "). Members change through Orglane.",
			"includes_all_repositories": false, "can_create_org_repo": false, "units_map": teamUnits,
		}, &created)
		if err != nil {
			// A refusal may mean another run created it meanwhile: the next
			// sync finds it by name. Report what is synced so far.
			return refs, classify(err)
		}
		byName[strings.ToLower(created.Name)] = created
		refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: strconv.FormatInt(created.ID, 10), Name: created.Name})
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		env.Logger().WarnContext(ctx, "forgejo: mapped team names are taken by teams that are not Orglane's; those mappings stay unsynced",
			"code", CodeTeamNameTaken, "teams", strings.Join(conflicts, ","))
	}
	return refs, nil
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
