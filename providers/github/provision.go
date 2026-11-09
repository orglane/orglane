package github

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
// → GitHub"). Orglane never creates GitHub accounts: a person proves their
// own account once (the employee step, link.go), and everything here acts
// on that account's immutable numeric user ID. Every action resolves the
// account's current login from the ID when it runs (GET /user/{id}) and
// checks that the organization membership it reads belongs to that ID.
// GitHub's membership mutations take a login, so immediately before one the
// action resolves the login from the ID again: a rename that released the
// login to another account is the retryable login_changed, and nothing is
// added or removed for the new holder.
//
// Access: an organization invitation by user ID (POST /orgs/{org}/invitations
// with invitee_id), then team memberships. Removal: canceling the
// invitation, or removing the organization membership, which also ends the
// team memberships. Organization owners and billing managers are never
// removed (owner_protected, role_protected), and nobody is made an owner or
// a team maintainer.
//
// Ownership (provider.ActionOutcome.Proof): the confirmed answer to
// Orglane's invitation, or a pending invitation whose inviter is the App's
// bot account ("<slug>[bot]"), which only calls with the App's installation
// token make. A membership that is already active proves nothing.
//
// Teams: "create" mappings get a team Orglane creates, named after the
// Orglane team slug and marked by its description (teamMarker plus the
// instance ID); "existing" mappings name a team the admin picked by its
// numeric ID. A team with the mapped name that does not carry the instance's
// mark is never taken over. Team removals happen only from the instance's
// mapped teams (DesiredAccess.Mapped) and only for managed accounts; team
// maintainers are never removed from their team (maintainer_protected).
// Additions to a team only run for people with an organization membership or
// a pending invitation, because GitHub invites anyone else who is added to a
// team, which would grant without the invitation's proof.

// Action parameters.
const (
	paramAccountID = "account_id" // the GitHub user ID (the verified link's)
	paramTeam      = "team"       // GitHub team ID
)

// Error codes of provisioning.
const (
	// CodeAccountNotFound: the linked GitHub account no longer exists.
	CodeAccountNotFound = "account_not_found"
	// CodeOwnerProtected: an organization owner is never removed.
	CodeOwnerProtected = "owner_protected"
	// CodeRoleProtected: a billing manager is never removed.
	CodeRoleProtected = "role_protected"
	// CodeMaintainerProtected: a team maintainer is never removed from the
	// team.
	CodeMaintainerProtected = "maintainer_protected"
	// CodeSeatLimit: the organization has no seat for another member.
	CodeSeatLimit = "seat_limit"
	// CodeInviteRefused: GitHub refused the invitation (e.g. the account
	// blocked the organization).
	CodeInviteRefused = "invite_refused"
	// CodeNotInvited: a team addition before the person has an organization
	// membership or invitation. Retryable: the next plan invites first.
	CodeNotInvited = "not_invited"
	// CodeTeamNotSynced: a mapped team does not exist on GitHub (yet).
	// Retryable: the next team sync creates "create" teams.
	CodeTeamNotSynced = "team_not_synced"
	// CodeTeamNameTaken: a team with a "create" mapping's name exists but is
	// not Orglane's (logged by SyncTeams; the mapping stays unsynced).
	CodeTeamNameTaken = "team_name_taken"
	// CodeLoginChanged: the account's login moved while an action ran.
	// Retryable.
	CodeLoginChanged = "login_changed"
	// CodeForbidden: GitHub refused the App (403), e.g. teams synced from
	// an identity provider.
	CodeForbidden = "github_forbidden"
	// CodeRejected: GitHub refused the request (another 4xx).
	CodeRejected = "github_rejected"
	// CodeUnauthorized: GitHub refused the installation token (401).
	// Retryable: the next token is minted after the installation is
	// verified again.
	CodeUnauthorized = "github_unauthorized"
)

// teamMarker starts the description of the teams Orglane creates; the
// instance ID follows it (teamMark).
const teamMarker = "Managed by Orglane"

func teamMark(inst provider.Instance) string { return teamMarker + " (" + inst.ID.String() + ")" }

// Bounds of paged lists (GitHub allows 100 items per page).
const (
	perPage  = 100
	maxPages = 100
)

// org is a client for the instance's organization, authenticated with an
// installation token.
type org struct {
	env   provider.Env
	inst  provider.Instance
	login string
	token string
}

func (a *Adapter) org(ctx context.Context, env provider.Env, inst provider.Instance) (*org, error) {
	login := setting(inst, "organization")
	if !loginPattern.MatchString(login) {
		return nil, protocolError("invalid_organization")
	}
	token, err := a.InstallationToken(ctx, env, inst)
	if err != nil {
		return nil, classify(err)
	}
	return &org{env: env, inst: inst, login: login, token: token}, nil
}

func (o *org) do(ctx context.Context, method, path string, body, out any) error {
	return request(ctx, o.env, method, path, o.token, body, out)
}

func (o *org) path(parts ...string) string {
	p := "/orgs/" + url.PathEscape(o.login)
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// bot is the login GitHub records for the App's own actions.
func (o *org) bot() string { return setting(o.inst, "app_slug") + "[bot]" }

// classify turns GitHub's answers into provider errors: server errors and
// 401 are retryable, other refusals permanent. Rate limits and missing
// answers are classified where they happen (do).
func classify(err error) error {
	var pe *provider.Error
	if err == nil || errors.As(err, &pe) {
		return err
	}
	var api *APIError
	if !errors.As(err, &api) {
		return err
	}
	switch {
	case api.Status >= 500 || api.Status == http.StatusRequestTimeout:
		return &provider.Error{Code: CodeUnavailable, Retryable: true, Err: err}
	case api.Status == http.StatusUnauthorized:
		return &provider.Error{Code: CodeUnauthorized, Retryable: true, Err: err}
	case api.Status == http.StatusForbidden:
		return &provider.Error{Code: CodeForbidden, Err: err}
	case api.SeatLimit:
		return &provider.Error{Code: CodeSeatLimit, Err: err}
	}
	return &provider.Error{Code: CodeRejected, Err: err}
}

func notFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}

// pages reads a paged list endpoint.
func pages[T any](ctx context.Context, o *org, path string) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		var list []T
		q := url.Values{"per_page": {strconv.Itoa(perPage)}, "page": {strconv.Itoa(page)}}
		if err := o.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &list); err != nil {
			return nil, classify(err)
		}
		all = append(all, list...)
		if len(list) < perPage {
			return all, nil
		}
	}
	return nil, &provider.Error{Code: CodeRejected, Err: fmt.Errorf("github: %s has more than %d pages", path, maxPages)}
}

// ghUser is a GitHub account.
type ghUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func (u ghUser) account() *provider.ExternalAccount {
	return &provider.ExternalAccount{Realm: Realm, ExternalID: strconv.FormatInt(u.ID, 10), Login: u.Login}
}

// user reads the account with the ID now; nil if it no longer exists.
func (o *org) user(ctx context.Context, id string) (*ghUser, error) {
	if !idPattern.MatchString(id) {
		return nil, protocolError("invalid_account")
	}
	var u ghUser
	if err := o.do(ctx, http.MethodGet, "/user/"+id, nil, &u); notFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, classify(err)
	}
	if strconv.FormatInt(u.ID, 10) != id || !loginPattern.MatchString(u.Login) {
		return nil, protocolError("invalid_response")
	}
	return &u, nil
}

// current reads the account with the ID now, immediately before a mutation
// addressed by login: GitHub's organization and team membership endpoints
// take a login, and a rename can release a login to another account while an
// action runs. The fresh login must still be the one the action's reads used,
// else the action is retryable (login_changed) and nothing is changed. A nil
// user means the account is gone.
func (o *org) current(ctx context.Context, u ghUser) (*ghUser, error) {
	v, err := o.user(ctx, strconv.FormatInt(u.ID, 10))
	if err != nil || v == nil {
		return v, err
	}
	if !strings.EqualFold(v.Login, u.Login) {
		return nil, &provider.Error{Code: CodeLoginChanged, Retryable: true, Err: errors.New("github: the account's login moved while the action ran")}
	}
	return v, nil
}

// membership is an organization membership (state active, or pending for
// an invitation).
type membership struct {
	State string  `json:"state"`
	Role  string  `json:"role"` // admin | member | billing_manager
	User  *ghUser `json:"user"`
}

// membership reads u's organization membership; nil if there is neither a
// membership nor an invitation. It belongs to u's ID or the read fails.
func (o *org) membership(ctx context.Context, u ghUser) (*membership, error) {
	var m membership
	if err := o.do(ctx, http.MethodGet, o.path("memberships", u.Login), nil, &m); notFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, classify(err)
	}
	if m.User == nil || m.User.ID != u.ID {
		return nil, &provider.Error{Code: CodeLoginChanged, Retryable: true, Err: errors.New("github: the membership read is not the account's")}
	}
	return &m, nil
}

// invitation is a pending organization invitation.
type invitation struct {
	ID      int64  `json:"id"`
	Login   string `json:"login"` // the invitee's login (empty for email invitations)
	Inviter *struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"inviter"`
}

// invitation finds u's pending invitation; nil if there is none.
func (o *org) invitation(ctx context.Context, u ghUser) (*invitation, error) {
	list, err := pages[invitation](ctx, o, o.path("invitations"))
	if err != nil {
		return nil, err
	}
	for _, inv := range list {
		if inv.Login != "" && strings.EqualFold(inv.Login, u.Login) {
			return &inv, nil
		}
	}
	return nil, nil
}

// ours reports whether the App's installation made the invitation.
func (o *org) ours(inv *invitation) bool {
	return inv.Inviter != nil && inv.Inviter.Type == "Bot" && strings.EqualFold(inv.Inviter.Login, o.bot())
}

// ghTeam is an organization team.
type ghTeam struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Type        string `json:"type"` // organization | enterprise
}

func (t ghTeam) id() string { return strconv.FormatInt(t.ID, 10) }

// teams lists the organization's own teams (not enterprise teams).
func (o *org) teams(ctx context.Context) ([]ghTeam, error) {
	list, err := pages[ghTeam](ctx, o, o.path("teams"))
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list, func(t ghTeam) bool { return t.Type != "organization" || t.ID <= 0 || !slugPattern.MatchString(t.Slug) }), nil
}

// team finds an organization team by its ID now; nil if it is gone.
func (o *org) team(ctx context.Context, id string) (*ghTeam, error) {
	teams, err := o.teams(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		if t.id() == id {
			return &t, nil
		}
	}
	return nil, nil
}

// teamMembership is a team membership (pending until the invitation is
// accepted).
type teamMembership struct {
	Role  string `json:"role"` // member | maintainer
	State string `json:"state"`
}

func (o *org) teamMembership(ctx context.Context, t ghTeam, u ghUser) (*teamMembership, error) {
	var m teamMembership
	if err := o.do(ctx, http.MethodGet, o.path("teams", t.Slug, "memberships", u.Login), nil, &m); notFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, classify(err)
	}
	return &m, nil
}

// Plan implements provider.UserProvisioner. It only reads. Without a
// verified link nothing can be done: a person with desired access waits for
// the employee step. Actions come in this order: the link's login refresh,
// the invitation, the team additions, then the removals, so a removal never
// runs before a grant (the core stops at the first failure). Removals are
// planned only for managed accounts.
func (a *Adapter) Plan(ctx context.Context, env provider.Env, inst provider.Instance, d provider.DesiredAccess) (provider.Plan, error) {
	if d.Link == nil || d.Link.Account.Realm != Realm {
		if d.Access {
			return provider.Plan{EmployeeStep: StepLinkAccount}, nil
		}
		return provider.Plan{}, nil
	}
	o, err := a.org(ctx, env, inst)
	if err != nil {
		return provider.Plan{}, err
	}
	id := d.Link.Account.ExternalID
	u, err := o.user(ctx, id)
	if err != nil {
		return provider.Plan{}, err
	}
	if u == nil {
		if d.Access {
			return provider.Plan{}, &provider.Error{Code: CodeAccountNotFound, Err: errors.New("github: the linked account does not exist")}
		}
		return provider.Plan{}, nil // a deleted account has no access left
	}
	mem, err := o.membership(ctx, *u)
	if err != nil {
		return provider.Plan{}, err
	}
	var plan provider.Plan
	if !d.Access {
		switch {
		case mem == nil || !d.Managed:
		case mem.State == "pending":
			plan.Actions = append(plan.Actions, accountAction(provider.ActionCancelInvitation, id, true))
		default:
			// Removing the membership ends the team memberships too.
			plan.Actions = append(plan.Actions, accountAction(provider.ActionRemoveFromOrg, id, true))
		}
		return plan, nil
	}
	if u.Login != d.Link.Account.Login {
		plan.Actions = append(plan.Actions, accountAction(provider.ActionUpdateAccount, id, false))
	}

	teams, err := o.teams(ctx)
	if err != nil {
		return provider.Plan{}, err
	}
	byID := map[string]ghTeam{}
	for _, t := range teams {
		byID[t.id()] = t
	}
	want := map[string]bool{}
	for _, ref := range d.Teams {
		if _, ok := byID[ref.ExternalRef]; ref.ExternalRef == "" || !ok {
			return provider.Plan{}, &provider.Error{Code: CodeTeamNotSynced, Retryable: true, Err: fmt.Errorf("github: mapped team %q does not exist", ref.ExternalRef)}
		}
		want[ref.ExternalRef] = true
	}
	var mapped []ghTeam
	seen := map[string]bool{}
	for _, ref := range append(slices.Clone(d.Teams), d.Mapped...) {
		if t, ok := byID[ref.ExternalRef]; ok && !seen[t.id()] {
			seen[t.id()] = true
			mapped = append(mapped, t)
		}
	}
	slices.SortFunc(mapped, func(a, b ghTeam) int { return cmp.Compare(a.ID, b.ID) })

	if mem == nil {
		plan.Actions = append(plan.Actions, accountAction(provider.ActionInvite, id, false))
		for _, t := range mapped {
			if want[t.id()] {
				plan.Actions = append(plan.Actions, teamAction(provider.ActionAddToTeam, id, t))
			}
		}
		return plan, nil
	}
	var removals []provider.Action
	for _, t := range mapped {
		tm, err := o.teamMembership(ctx, t, *u)
		if err != nil {
			return provider.Plan{}, err
		}
		switch {
		case want[t.id()] && tm == nil:
			plan.Actions = append(plan.Actions, teamAction(provider.ActionAddToTeam, id, t))
		case !want[t.id()] && tm != nil && d.Managed:
			// Planned for maintainers too: Apply refuses it
			// (maintainer_protected), so an admin sees why.
			removals = append(removals, teamAction(provider.ActionRemoveFromTeam, id, t))
		}
	}
	plan.Actions = append(plan.Actions, removals...)
	return plan, nil
}

func accountAction(kind provider.ActionKind, id string, removal bool) provider.Action {
	return provider.Action{Kind: kind, Key: string(kind), Params: map[string]string{paramAccountID: id}, Removal: removal}
}

func teamAction(kind provider.ActionKind, id string, t ghTeam) provider.Action {
	return provider.Action{
		Kind: kind, Key: string(kind) + ":" + t.id(), Removal: kind == provider.ActionRemoveFromTeam,
		Params: map[string]string{paramAccountID: id, paramTeam: t.id()},
	}
}

// RemovalStillManaged implements provider.RemovalChecker. Team removals are
// planned only from the instance's mapped teams; deleting a mapping stops
// managing its team, so a queued removal from it is stale and must not run.
// Account removals (the organization membership, a pending invitation) do
// not depend on mappings.
func (*Adapter) RemovalStillManaged(_ provider.Instance, d provider.DesiredAccess, a provider.Action) bool {
	if a.Kind != provider.ActionRemoveFromTeam {
		return true
	}
	for _, t := range d.Mapped {
		if t.ExternalRef == a.Params[paramTeam] {
			return true
		}
	}
	return false
}

// Apply implements provider.UserProvisioner. Every action is an ensure that
// reads the account (by ID), its membership and the team now; nothing saved
// in the plan but the IDs is trusted.
func (a *Adapter) Apply(ctx context.Context, env provider.Env, inst provider.Instance, act provider.Action) (provider.ActionOutcome, error) {
	o, err := a.org(ctx, env, inst)
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	u, err := o.user(ctx, act.Params[paramAccountID])
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	if u == nil {
		if act.Removal {
			return provider.ActionOutcome{}, nil // a deleted account has no access left
		}
		return provider.ActionOutcome{}, &provider.Error{Code: CodeAccountNotFound, Err: errors.New("github: the linked account does not exist")}
	}
	switch act.Kind {
	case provider.ActionUpdateAccount:
		// Nothing changes at GitHub: the core refreshes the link's login.
		return provider.ActionOutcome{Link: u.account()}, nil
	case provider.ActionInvite:
		return o.invite(ctx, *u)
	case provider.ActionCancelInvitation:
		return o.cancelInvitation(ctx, *u)
	case provider.ActionRemoveFromOrg:
		return o.removeFromOrg(ctx, *u)
	case provider.ActionAddToTeam, provider.ActionRemoveFromTeam:
		return o.teamMember(ctx, *u, act.Params[paramTeam], act.Kind == provider.ActionAddToTeam)
	}
	return provider.ActionOutcome{}, &provider.Error{Code: "unsupported_action", Err: fmt.Errorf("github: %s", act.Kind)}
}

// invite makes u a member or invitee of the organization. Proof that
// Orglane granted it: the confirmed answer to its invitation, or a pending
// invitation by the App's bot (an earlier attempt whose answer was lost).
func (o *org) invite(ctx context.Context, u ghUser) (provider.ActionOutcome, error) {
	out := provider.ActionOutcome{Link: u.account()}
	marker := func() (provider.ActionOutcome, bool, error) {
		mem, err := o.membership(ctx, u)
		if err != nil || mem == nil {
			return out, false, err
		}
		if mem.State == "pending" {
			inv, err := o.invitation(ctx, u)
			if err != nil {
				return out, false, err
			}
			if inv != nil && o.ours(inv) {
				// The invitation list is matched by login: prove that the
				// login still belongs to the bound ID before claiming it.
				fresh, err := o.current(ctx, u)
				if err != nil {
					return out, false, err
				}
				if fresh == nil {
					return out, false, &provider.Error{Code: CodeAccountNotFound, Err: errors.New("github: the linked account does not exist")}
				}
				return provider.ActionOutcome{Link: out.Link, Proof: &provider.Proof{Kind: provider.ProofMarker, Evidence: "invitation:" + strconv.FormatInt(inv.ID, 10)}}, true, nil
			}
		}
		return out, true, nil // there, but nothing proves Orglane made it
	}
	if res, there, err := marker(); err != nil || there {
		return res, err
	}
	var inv invitation
	err := o.do(ctx, http.MethodPost, o.path("invitations"), map[string]any{"invitee_id": u.ID, "role": "direct_member"}, &inv)
	if err == nil {
		if inv.ID <= 0 {
			return provider.ActionOutcome{}, protocolError("invalid_response")
		}
		return provider.ActionOutcome{Changed: true, Link: out.Link, Proof: &provider.Proof{Kind: provider.ProofResponse, Evidence: "invitation:" + strconv.FormatInt(inv.ID, 10)}}, nil
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusUnprocessableEntity {
		return provider.ActionOutcome{}, classify(err)
	}
	// Refused: maybe because an attempt whose answer was lost invited them.
	// Read back before the refusal counts.
	if res, there, rerr := marker(); rerr != nil || there {
		return res, rerr
	}
	if api.SeatLimit {
		return provider.ActionOutcome{}, &provider.Error{Code: CodeSeatLimit, Err: err}
	}
	return provider.ActionOutcome{}, &provider.Error{Code: CodeInviteRefused, Err: err}
}

func (o *org) cancelInvitation(ctx context.Context, u ghUser) (provider.ActionOutcome, error) {
	out := provider.ActionOutcome{Link: u.account()}
	inv, err := o.invitation(ctx, u)
	if err != nil || inv == nil {
		return out, err
	}
	// The invitation was found by login; never cancel one that a reused
	// login points at now.
	if fresh, err := o.current(ctx, u); err != nil {
		return provider.ActionOutcome{}, err
	} else if fresh == nil {
		return out, nil
	}
	if err := o.do(ctx, http.MethodDelete, o.path("invitations", strconv.FormatInt(inv.ID, 10)), nil, nil); notFound(err) {
		return out, nil // accepted or canceled meanwhile
	} else if err != nil {
		return provider.ActionOutcome{}, classify(err)
	}
	out.Changed = true
	return out, nil
}

// removeFromOrg removes u's membership (or invitation), never an owner's or
// a billing manager's.
func (o *org) removeFromOrg(ctx context.Context, u ghUser) (provider.ActionOutcome, error) {
	out := provider.ActionOutcome{Link: u.account()}
	mem, err := o.membership(ctx, u)
	if err != nil || mem == nil {
		return out, err
	}
	switch mem.Role {
	case "member":
	case "admin":
		return provider.ActionOutcome{}, &provider.Error{Code: CodeOwnerProtected, Err: errors.New("github: organization owners are never removed")}
	default:
		return provider.ActionOutcome{}, &provider.Error{Code: CodeRoleProtected, Err: fmt.Errorf("github: role %q is never removed", mem.Role)}
	}
	// The membership DELETE takes a login: re-resolve the bound ID now, so a
	// login released and reused while the membership was read cannot remove
	// the new holder.
	fresh, err := o.current(ctx, u)
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	if fresh == nil {
		return out, nil // gone meanwhile
	}
	if err := o.do(ctx, http.MethodDelete, o.path("memberships", fresh.Login), nil, nil); notFound(err) {
		return out, nil // gone meanwhile
	} else if err != nil {
		return provider.ActionOutcome{}, classify(err)
	}
	out.Changed = true
	return out, nil
}

// teamMember ensures u's membership (add) or its absence (remove) in the
// team with the ID, read now. An addition needs an organization membership
// or invitation first; a removal never takes a maintainer out.
func (o *org) teamMember(ctx context.Context, u ghUser, teamID string, add bool) (provider.ActionOutcome, error) {
	out := provider.ActionOutcome{Link: u.account()}
	if add {
		mem, err := o.membership(ctx, u)
		if err != nil {
			return provider.ActionOutcome{}, err
		}
		if mem == nil {
			return provider.ActionOutcome{}, &provider.Error{Code: CodeNotInvited, Retryable: true, Err: errors.New("github: not in the organization yet")}
		}
	}
	t, err := o.team(ctx, teamID)
	switch {
	case err != nil:
		return provider.ActionOutcome{}, err
	case t == nil && add:
		return provider.ActionOutcome{}, &provider.Error{Code: CodeTeamNotSynced, Retryable: true, Err: fmt.Errorf("github: team %s does not exist", teamID)}
	case t == nil:
		return out, nil // the team is gone, and the membership with it
	}
	tm, err := o.teamMembership(ctx, *t, u)
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	// Team membership mutations take a login: re-resolve the bound ID now,
	// after the reads above, so a login released and reused meanwhile cannot
	// add or remove the new holder.
	fresh, err := o.current(ctx, u)
	if err != nil {
		return provider.ActionOutcome{}, err
	}
	if fresh == nil {
		if add {
			return provider.ActionOutcome{}, &provider.Error{Code: CodeAccountNotFound, Err: errors.New("github: the linked account does not exist")}
		}
		return out, nil // gone meanwhile, and the membership with it
	}
	if (tm != nil) == add {
		return out, nil
	}
	path := o.path("teams", t.Slug, "memberships", fresh.Login)
	if add {
		err = o.do(ctx, http.MethodPut, path, map[string]string{"role": "member"}, nil)
	} else {
		if tm.Role == "maintainer" {
			return provider.ActionOutcome{}, &provider.Error{Code: CodeMaintainerProtected, Err: errors.New("github: team maintainers are never removed")}
		}
		err = o.do(ctx, http.MethodDelete, path, nil, nil)
		if notFound(err) {
			return out, nil // gone meanwhile
		}
	}
	if err != nil {
		return provider.ActionOutcome{}, classify(err)
	}
	out.Changed = true
	return out, nil
}

// SyncTeams implements provider.UserProvisioner. "existing" mappings keep
// the team the admin picked while it exists. "create" mappings keep the team
// Orglane created while it exists; otherwise a team with the mapped name is
// used only if it carries this instance's mark, and else one is created
// (closed, no repositories: the organization's owners grant those on
// GitHub). A team with the name that is not marked is never taken over: that
// mapping stays unsynced (logged with CodeTeamNameTaken) and the others
// sync.
func (a *Adapter) SyncTeams(ctx context.Context, env provider.Env, inst provider.Instance, mappings []provider.TeamMapping) ([]provider.TeamRef, error) {
	o, err := a.org(ctx, env, inst)
	if err != nil {
		return nil, err
	}
	teams, err := o.teams(ctx)
	if err != nil {
		return nil, err
	}
	byID, bySlug := map[string]ghTeam{}, map[string]ghTeam{}
	for _, t := range teams {
		byID[t.id()] = t
		bySlug[strings.ToLower(t.Slug)] = t
	}
	mark := teamMark(inst)
	var refs []provider.TeamRef
	var conflicts, missing []string
	for _, m := range mappings {
		if t, ok := byID[m.ExternalRef]; ok && m.ExternalRef != "" {
			refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: t.id(), Name: t.Slug})
			continue
		}
		if m.Mode != provider.MappingCreate {
			missing = append(missing, m.ExternalRef)
			continue
		}
		if t, ok := bySlug[strings.ToLower(m.TeamSlug)]; ok {
			if !strings.HasPrefix(t.Description, mark) {
				conflicts = append(conflicts, m.TeamSlug)
				continue
			}
			refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: t.id(), Name: t.Slug})
			continue
		}
		var created ghTeam
		err := o.do(ctx, http.MethodPost, o.path("teams"), map[string]any{
			"name": m.TeamSlug, "description": mark + ": team " + m.TeamName + ". Members change through Orglane.",
			"privacy": "closed",
		}, &created)
		if err != nil {
			var api *APIError
			if errors.As(err, &api) && api.Status == http.StatusUnprocessableEntity {
				// Taken meanwhile (another run, or someone else): the next
				// sync finds it by name and checks its mark.
				conflicts = append(conflicts, m.TeamSlug)
				continue
			}
			// Report what is synced so far.
			return refs, classify(err)
		}
		if created.ID <= 0 || !slugPattern.MatchString(created.Slug) {
			return refs, protocolError("invalid_response")
		}
		bySlug[strings.ToLower(created.Slug)] = created
		refs = append(refs, provider.TeamRef{TeamID: m.TeamID, ExternalRef: created.id(), Name: created.Slug})
	}
	if len(conflicts) > 0 {
		slices.Sort(conflicts)
		env.Logger().WarnContext(ctx, "github: team names are taken by teams that are not Orglane's; those mappings stay unsynced",
			"code", CodeTeamNameTaken, "teams", strings.Join(conflicts, ","))
	}
	if len(missing) > 0 {
		env.Logger().WarnContext(ctx, "github: mapped teams do not exist", "code", CodeTeamNotSynced, "teams", strings.Join(missing, ","))
	}
	return refs, nil
}

// ValidateTeamMapping implements provider.TeamMappingValidator: an
// "existing" mapping names a GitHub team by its numeric ID.
func (*Adapter) ValidateTeamMapping(_ provider.Instance, mode, ref string) error {
	if mode == provider.MappingExisting && !idPattern.MatchString(ref) {
		return errors.New("an existing GitHub team is named by its numeric ID")
	}
	return nil
}

// ProviderTeams implements provider.TeamLister: the organization's teams,
// those Orglane created for this instance marked as managed.
func (a *Adapter) ProviderTeams(ctx context.Context, env provider.Env, inst provider.Instance) ([]provider.Team, error) {
	o, err := a.org(ctx, env, inst)
	if err != nil {
		return nil, err
	}
	teams, err := o.teams(ctx)
	if err != nil {
		return nil, err
	}
	mark := teamMark(inst)
	out := make([]provider.Team, 0, len(teams))
	for _, t := range teams {
		out = append(out, provider.Team{ExternalRef: t.id(), Name: t.Name, Slug: t.Slug, Managed: strings.HasPrefix(t.Description, mark)})
	}
	return out, nil
}

// Members implements provider.UserProvisioner: the organization's members
// (pending invitations are not members).
func (a *Adapter) Members(ctx context.Context, env provider.Env, inst provider.Instance) ([]provider.ExternalAccount, error) {
	o, err := a.org(ctx, env, inst)
	if err != nil {
		return nil, err
	}
	users, err := pages[ghUser](ctx, o, o.path("members"))
	if err != nil {
		return nil, err
	}
	out := make([]provider.ExternalAccount, 0, len(users))
	for _, u := range users {
		if u.ID > 0 && loginPattern.MatchString(u.Login) {
			out = append(out, *u.account())
		}
	}
	return out, nil
}

var (
	_ provider.UserProvisioner      = (*Adapter)(nil)
	_ provider.RemovalChecker       = (*Adapter)(nil)
	_ provider.TeamMappingValidator = (*Adapter)(nil)
	_ provider.TeamLister           = (*Adapter)(nil)
)
