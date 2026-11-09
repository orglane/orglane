package github

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orglane/orglane/providers/provider"
)

// Normalized event types (docs/specs/providers.md "Normalized events").
const (
	EventPullRequestOpened = "code.pull_request.opened"
	EventReviewRequested   = "code.review.requested"
	EventMention           = "code.mention.created"
	EventMembershipChanged = "code.membership.changed"
)

// Delivery error codes.
const (
	// CodeInvalidPayload: the body is not the JSON of the event.
	CodeInvalidPayload = "invalid_payload"
	// CodeForeignInstallation: the delivery is for another installation or
	// organization.
	CodeForeignInstallation = "foreign_installation"
	// CodeNoInstallation: an App-level delivery without an installation
	// that is not handled explicitly.
	CodeNoInstallation = "no_installation"
)

// Health codes the webhook effects set (app_instances.health_code).
const (
	CodeInstallationDeleted   = "installation_deleted"
	CodeInstallationSuspended = "installation_suspended"
	CodeWebhookDeleted        = "webhook_deleted"
)

// Limits of normalized strings (untrusted input rules).
const (
	maxTitle    = 256
	maxExcerpt  = 280
	maxRef      = 255
	maxTargets  = 50
	maxRepoName = 100
)

// The X-GitHub-Event values the adapter reads. Others are ignored.
var handledEvents = []string{
	"pull_request", "pull_request_review", "pull_request_review_comment", "issue_comment", "issues",
	"member", "membership", "organization", "installation", "meta", "github_app_authorization",
}

var (
	// botLoginPattern: GitHub App bots ("dependabot[bot]") as actors and
	// reviewers. Mentions never match it.
	botLoginPattern = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,37}[a-zA-Z0-9])?\[bot\]$`)
	repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

// payloadUser is an account in a webhook payload.
type payloadUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type payloadTeam struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
}

type repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

type pullRequest struct {
	ID                 int64         `json:"id"`
	Number             int           `json:"number"`
	Title              string        `json:"title"`
	Body               string        `json:"body"`
	HTMLURL            string        `json:"html_url"`
	Draft              bool          `json:"draft"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
	RequestedReviewers []payloadUser `json:"requested_reviewers"`
	Assignees          []payloadUser `json:"assignees"`
	Base               struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

type issue struct {
	ID          int64           `json:"id"`
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	HTMLURL     string          `json:"html_url"`
	CreatedAt   time.Time       `json:"created_at"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

type review struct {
	ID          int64     `json:"id"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// payload is the union of the fields the adapter reads; unknown fields are
// ignored, and nothing else is decoded (GitHub's payloads differ in the
// types of fields the adapter does not need, such as timestamps).
type payload struct {
	Action       string      `json:"action"`
	Sender       payloadUser `json:"sender"`
	Installation *struct {
		ID      int64        `json:"id"`
		Account *payloadUser `json:"account"`
	} `json:"installation"`
	Organization      *payloadUser `json:"organization"`
	Repository        *repository  `json:"repository"`
	PullRequest       *pullRequest `json:"pull_request"`
	RequestedReviewer *payloadUser `json:"requested_reviewer"`
	RequestedTeam     *payloadTeam `json:"requested_team"`
	Review            *review      `json:"review"`
	Issue             *issue       `json:"issue"`
	Comment           *comment     `json:"comment"`
	// member and membership
	Member *payloadUser `json:"member"`
	Team   *payloadTeam `json:"team"`
	Scope  string       `json:"scope"`
	// organization
	Membership *struct {
		Role string       `json:"role"`
		User *payloadUser `json:"user"`
	} `json:"membership"`
	User       *payloadUser `json:"user"`
	Invitation *struct {
		Role string `json:"role"`
	} `json:"invitation"`
}

// delivery is a parsed delivery that passed the installation check.
type delivery struct {
	inst provider.Instance
	d    provider.Delivery
	p    payload
	// app: an App-level delivery without an installation that is handled
	// explicitly (meta, github_app_authorization); nothing else is read.
	app bool
}

// parse decodes a handled delivery and checks that it is for this
// instance, from signed body fields only (docs/specs/providers.md
// "Webhooks" -> "Processing"): installation.id must be the instance's
// installation and organization.id (or an installation event's account),
// when present, its organization. Event names the adapter does not handle
// give nil.
func parse(inst provider.Instance, d provider.Delivery) (*delivery, error) {
	if !slices.Contains(handledEvents, d.EventName) {
		return nil, nil
	}
	var p payload
	if err := json.Unmarshal(d.Body, &p); err != nil {
		return nil, invalidPayload("not the JSON of a %s event", d.EventName)
	}
	if p.Installation == nil {
		switch d.EventName {
		case "meta", "github_app_authorization":
			return &delivery{inst: inst, d: d, p: p, app: true}, nil
		}
		return nil, foreign(CodeNoInstallation)
	}
	if strconv.FormatInt(p.Installation.ID, 10) != setting(inst, "installation_id") {
		return nil, foreign(CodeForeignInstallation)
	}
	orgID := setting(inst, "organization_id")
	if p.Organization != nil && strconv.FormatInt(p.Organization.ID, 10) != orgID {
		return nil, foreign(CodeForeignInstallation)
	}
	if d.EventName == "installation" {
		// Installation events always name the account; requiring it keeps a
		// spoofed X-GitHub-Event header (the headers are not signed) from
		// turning another event's body into an installation change.
		acct := p.Installation.Account
		if acct == nil || acct.ID <= 0 || strconv.FormatInt(acct.ID, 10) != orgID {
			return nil, foreign(CodeForeignInstallation)
		}
	}
	return &delivery{inst: inst, d: d, p: p}, nil
}

func invalidPayload(format string, args ...any) error {
	return &provider.Error{Code: CodeInvalidPayload, Err: fmt.Errorf("github: "+format, args...)}
}

func foreign(code string) error {
	return &provider.Error{Code: code, Err: provider.ErrForeignDelivery}
}

// Effects implements provider.DeliveryEffects: installation events that
// delete or suspend the App's installation disconnect or degrade the
// instance (unsuspend restores it), the deletion of the App's webhook
// (meta, App-level) degrades it, and organization and team membership
// changes ask provisioning to compare desired and actual access again.
// Any other delivery for the installation proves that the App's webhook
// works again, so it restores an instance degraded by its deletion.
func (*Adapter) Effects(inst provider.Instance, d provider.Delivery) (provider.Effect, error) {
	dl, err := parse(inst, d)
	if err != nil || dl == nil {
		return provider.Effect{}, err
	}
	action := dl.p.Action
	if !dl.app && d.EventName != "installation" {
		e := provider.Effect{State: provider.EffectRestored, Code: CodeWebhookDeleted}
		switch {
		case d.EventName == "organization" && (action == "member_added" || action == "member_removed"),
			d.EventName == "membership" && (action == "added" || action == "removed"):
			e.Resync = true
		}
		return e, nil
	}
	switch d.EventName {
	case "installation":
		switch action {
		case "deleted":
			return provider.Effect{State: provider.EffectDisconnected, Code: CodeInstallationDeleted}, nil
		case "suspend":
			return provider.Effect{State: provider.EffectDegraded, Code: CodeInstallationSuspended}, nil
		case "unsuspend":
			return provider.Effect{State: provider.EffectRestored, Code: CodeInstallationSuspended}, nil
		}
	case "meta":
		if action == "deleted" {
			return provider.Effect{State: provider.EffectDegraded, Code: CodeWebhookDeleted}, nil
		}
	}
	return provider.Effect{}, nil
}

// normalizer turns one parsed delivery into events.
type normalizer struct {
	*delivery
	sha   string // hex SHA-256 of the raw body
	actor provider.ExternalRef
	repo  string // owner/name, for repository events
}

// Normalize implements provider.EventSource (docs/specs/providers.md
// "Normalized events" and "Untrusted input rules"). It is a pure function
// of the delivery: the payload is untrusted even though it is signed (a
// GitHub user controls titles, bodies and branch names), so it is decoded
// into typed structs, strings are validated or truncated, and URLs are kept
// only on github.com. Deliveries for another installation or organization
// are ErrForeignDelivery. Event names it does not normalize give nil (the
// delivery is ignored, unless an effect applied); actions it does not
// normalize give no events.
func (*Adapter) Normalize(inst provider.Instance, d provider.Delivery) ([]provider.Event, error) {
	dl, err := parse(inst, d)
	if err != nil || dl == nil || dl.app || d.EventName == "installation" {
		// App-level deliveries and installation events have only effects.
		return nil, err
	}
	sum := sha256.Sum256(d.Body)
	n := &normalizer{delivery: dl, sha: hex.EncodeToString(sum[:])}
	actor, ok := payloadRef(dl.p.Sender)
	if !ok {
		return nil, invalidPayload("invalid sender")
	}
	n.actor = actor
	if r := dl.p.Repository; r != nil {
		owner, name, ok := strings.Cut(r.FullName, "/")
		if r.ID <= 0 || !ok || !loginPattern.MatchString(owner) || !validRepoName(name) {
			return nil, invalidPayload("invalid repository")
		}
		n.repo = r.FullName
	}
	action := dl.p.Action
	switch d.EventName {
	case "pull_request":
		switch action {
		case "opened":
			return n.opened()
		case "review_requested":
			return n.reviewRequested()
		}
	case "issues":
		if action == "opened" {
			return n.issueOpened()
		}
	case "issue_comment":
		if action == "created" {
			return n.issueComment()
		}
	case "pull_request_review_comment":
		if action == "created" {
			return n.reviewComment()
		}
	case "pull_request_review":
		if action == "submitted" {
			return n.reviewSubmitted()
		}
	case "member":
		return n.collaborator()
	case "membership":
		return n.teamMembership()
	case "organization":
		return n.orgMembership()
	}
	return []provider.Event{}, nil
}

func validRepoName(name string) bool {
	return len(name) <= maxRepoName && repoNamePattern.MatchString(name) && name != "." && name != ".."
}

func validLogin(login string) bool {
	return loginPattern.MatchString(login) || botLoginPattern.MatchString(login)
}

// payloadRef is an account with its ID; false if either is invalid.
func payloadRef(u payloadUser) (provider.ExternalRef, bool) {
	if !validLogin(u.Login) || u.ID <= 0 {
		return provider.ExternalRef{}, false
	}
	return provider.ExternalRef{ExternalID: strconv.FormatInt(u.ID, 10), Login: u.Login}, true
}

// targets turns accounts into targets with reason, dropping invalid ones
// and duplicates, at most maxTargets in all.
func targets(out []provider.Target, users []payloadUser, reason string, seen map[string]bool) []provider.Target {
	for _, u := range users {
		r, ok := payloadRef(u)
		if !ok || seen[r.ExternalID+"\x00"+reason] || len(out) >= maxTargets {
			continue
		}
		seen[r.ExternalID+"\x00"+reason] = true
		out = append(out, provider.Target{ExternalRef: r, Reason: reason})
	}
	return out
}

// safeURL keeps a payload URL only if it is https on github.com.
func safeURL(raw string) string {
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, webHost) {
		return ""
	}
	return u.String()
}

// at is t when the payload carries it, else the delivery's receipt.
func (n *normalizer) at(t time.Time) time.Time {
	if t.IsZero() {
		return n.d.ReceivedAt
	}
	return t.UTC()
}

func (n *normalizer) event(typ, key string, at time.Time, subject provider.Subject, ts []provider.Target, data map[string]any) provider.Event {
	if ts == nil {
		ts = []provider.Target{}
	}
	if data == nil {
		data = map[string]any{}
	}
	return provider.Event{
		Type: typ, SchemaVersion: 1, DedupeKey: key, OccurredAt: at,
		Actor: n.actor, Subject: subject, Targets: ts, Data: data,
	}
}

func (n *normalizer) needRepo() error {
	if n.repo == "" {
		return invalidPayload("no repository")
	}
	return nil
}

func (n *normalizer) pullRequest() (*pullRequest, provider.Subject, error) {
	pr := n.p.PullRequest
	if err := n.needRepo(); err != nil {
		return nil, provider.Subject{}, err
	}
	if pr == nil || pr.ID <= 0 || pr.Number <= 0 {
		return nil, provider.Subject{}, invalidPayload("no pull request")
	}
	return pr, provider.Subject{
		Kind:       "pull_request",
		ExternalID: strconv.FormatInt(pr.ID, 10),
		Repository: n.repo,
		Number:     pr.Number,
		Title:      truncate(pr.Title, maxTitle),
		URL:        safeURL(pr.HTMLURL),
	}, nil
}

// issueSubject is the issue, or the pull request an issue_comment is on.
// GitHub's issue_comment payload carries the issue's ID, not the pull
// request's, so such a pull request subject has the issue ID as its
// ExternalID; Repository and Number identify it across events.
func (n *normalizer) issueSubject() (*issue, provider.Subject, error) {
	is := n.p.Issue
	if err := n.needRepo(); err != nil {
		return nil, provider.Subject{}, err
	}
	if is == nil || is.ID <= 0 || is.Number <= 0 {
		return nil, provider.Subject{}, invalidPayload("no issue")
	}
	kind := "issue"
	if len(is.PullRequest) > 0 && string(is.PullRequest) != "null" {
		kind = "pull_request"
	}
	return is, provider.Subject{Kind: kind, ExternalID: strconv.FormatInt(is.ID, 10), Repository: n.repo,
		Number: is.Number, Title: truncate(is.Title, maxTitle), URL: safeURL(is.HTMLURL)}, nil
}

// opened: code.pull_request.opened, and the mentions in the description.
func (n *normalizer) opened() ([]provider.Event, error) {
	pr, subject, err := n.pullRequest()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	ts := targets(nil, pr.RequestedReviewers, "review_requested", seen)
	ts = targets(ts, pr.Assignees, "assigned", seen)
	data := map[string]any{"draft": pr.Draft, "base_ref": truncate(pr.Base.Ref, maxRef), "head_ref": truncate(pr.Head.Ref, maxRef)}
	at := n.at(pr.CreatedAt)
	events := []provider.Event{n.event(EventPullRequestOpened, fmt.Sprintf("pr:%d:opened", pr.ID), at, subject, ts, data)}
	if m := n.mentions(pr.Body); len(m) > 0 {
		events = append(events, n.event(EventMention, fmt.Sprintf("pr:%d:body_mentions", pr.ID), at, subject, m,
			map[string]any{"excerpt": excerpt(pr.Body)}))
	}
	return events, nil
}

// reviewRequested: one event for the requested reviewer, or for the
// requested team (no targets in M2).
func (n *normalizer) reviewRequested() ([]provider.Event, error) {
	pr, subject, err := n.pullRequest()
	if err != nil {
		return nil, err
	}
	if pr.UpdatedAt.IsZero() {
		return nil, invalidPayload("no updated_at")
	}
	updated := pr.UpdatedAt.UTC().Format(time.RFC3339)
	if t := n.p.RequestedTeam; t != nil && n.p.RequestedReviewer == nil {
		if t.ID <= 0 || !slugPattern.MatchString(t.Slug) {
			return nil, invalidPayload("invalid requested team")
		}
		key := fmt.Sprintf("pr:%d:review_requested:team:%d:%s", pr.ID, t.ID, updated)
		return []provider.Event{n.event(EventReviewRequested, key, n.at(pr.UpdatedAt), subject, nil, map[string]any{"requested_team": t.Slug})}, nil
	}
	if n.p.RequestedReviewer == nil {
		return nil, invalidPayload("no requested reviewer")
	}
	reviewer, ok := payloadRef(*n.p.RequestedReviewer)
	if !ok {
		return nil, invalidPayload("invalid requested reviewer")
	}
	key := fmt.Sprintf("pr:%d:review_requested:%s:%s", pr.ID, reviewer.ExternalID, updated)
	ts := []provider.Target{{ExternalRef: reviewer, Reason: "review_requested"}}
	return []provider.Event{n.event(EventReviewRequested, key, n.at(pr.UpdatedAt), subject, ts, nil)}, nil
}

// issueOpened: the mentions in a new issue's description.
func (n *normalizer) issueOpened() ([]provider.Event, error) {
	is, subject, err := n.issueSubject()
	if err != nil {
		return nil, err
	}
	m := n.mentions(is.Body)
	if len(m) == 0 {
		return []provider.Event{}, nil
	}
	return []provider.Event{n.event(EventMention, fmt.Sprintf("issue:%d:body_mentions", is.ID), n.at(is.CreatedAt), subject, m,
		map[string]any{"excerpt": excerpt(is.Body)})}, nil
}

// commentEvent is the mention event of a new comment, if it mentions
// anyone.
func (n *normalizer) commentEvent(key string, c *comment, subject provider.Subject) []provider.Event {
	m := n.mentions(c.Body)
	if len(m) == 0 {
		return []provider.Event{}
	}
	if u := safeURL(c.HTMLURL); u != "" {
		subject.URL = u // the comment itself
	}
	data := map[string]any{"excerpt": excerpt(c.Body), "comment_id": strconv.FormatInt(c.ID, 10)}
	return []provider.Event{n.event(EventMention, key, n.at(c.CreatedAt), subject, m, data)}
}

// issueComment: the mentions in a new comment on an issue or a pull
// request's conversation.
func (n *normalizer) issueComment() ([]provider.Event, error) {
	_, subject, err := n.issueSubject()
	if err != nil {
		return nil, err
	}
	c := n.p.Comment
	if c == nil || c.ID <= 0 {
		return nil, invalidPayload("no comment")
	}
	return n.commentEvent(fmt.Sprintf("comment:%d:mentions", c.ID), c, subject), nil
}

// reviewComment: the mentions in a new comment on a pull request's diff.
// Review comments have their own IDs, apart from issue comments.
func (n *normalizer) reviewComment() ([]provider.Event, error) {
	_, subject, err := n.pullRequest()
	if err != nil {
		return nil, err
	}
	c := n.p.Comment
	if c == nil || c.ID <= 0 {
		return nil, invalidPayload("no comment")
	}
	return n.commentEvent(fmt.Sprintf("review_comment:%d:mentions", c.ID), c, subject), nil
}

// reviewSubmitted: the mentions in a submitted review's text.
func (n *normalizer) reviewSubmitted() ([]provider.Event, error) {
	_, subject, err := n.pullRequest()
	if err != nil {
		return nil, err
	}
	r := n.p.Review
	if r == nil || r.ID <= 0 {
		return nil, invalidPayload("no review")
	}
	m := n.mentions(r.Body)
	if len(m) == 0 {
		return []provider.Event{}, nil
	}
	if u := safeURL(r.HTMLURL); u != "" {
		subject.URL = u
	}
	data := map[string]any{"excerpt": excerpt(r.Body), "review_id": strconv.FormatInt(r.ID, 10)}
	return []provider.Event{n.event(EventMention, fmt.Sprintf("review:%d:mentions", r.ID), n.at(r.SubmittedAt), subject, m, data)}, nil
}

// membership is a code.membership.changed event. These payloads carry no
// stable event ID or timestamp, so the key is the signed body's hash and
// the event's index, and the time is the receipt.
func (n *normalizer) membership(subject provider.Subject, account payloadUser, change string, data map[string]any) ([]provider.Event, error) {
	ref, ok := payloadRef(account)
	if !ok {
		return nil, invalidPayload("invalid member")
	}
	data["change"] = change
	ts := []provider.Target{{ExternalRef: ref, Reason: "member_" + change}}
	return []provider.Event{n.event(EventMembershipChanged, fmt.Sprintf("body:%s/0", n.sha), n.d.ReceivedAt, subject, ts, data)}, nil
}

// collaborator: a repository collaborator was added or removed (member).
func (n *normalizer) collaborator() ([]provider.Event, error) {
	change := map[string]string{"added": "added", "removed": "removed"}[n.p.Action]
	if change == "" {
		return []provider.Event{}, nil
	}
	if err := n.needRepo(); err != nil {
		return nil, err
	}
	if n.p.Member == nil {
		return nil, invalidPayload("no member")
	}
	r := n.p.Repository
	subject := provider.Subject{Kind: "repository", ExternalID: strconv.FormatInt(r.ID, 10), Repository: n.repo,
		Title: truncate(n.repo, maxTitle), URL: safeURL(r.HTMLURL)}
	return n.membership(subject, *n.p.Member, change, map[string]any{})
}

// teamMembership: an account was added to or removed from a team
// (membership). A deleted team's payload has no slug.
func (n *normalizer) teamMembership() ([]provider.Event, error) {
	change := map[string]string{"added": "added", "removed": "removed"}[n.p.Action]
	if change == "" || n.p.Scope != "team" {
		return []provider.Event{}, nil
	}
	t := n.p.Team
	if t == nil || t.ID <= 0 || n.p.Member == nil {
		return nil, invalidPayload("no team or member")
	}
	subject := provider.Subject{Kind: "team", ExternalID: strconv.FormatInt(t.ID, 10), Title: truncate(t.Name, maxTitle), URL: safeURL(t.HTMLURL)}
	data := map[string]any{}
	if slugPattern.MatchString(t.Slug) {
		data["team"] = t.Slug
	}
	return n.membership(subject, *n.p.Member, change, data)
}

// orgMembership: an account was added to, removed from or invited to the
// organization (organization). An invitation by e-mail address has no
// account and gives no event.
func (n *normalizer) orgMembership() ([]provider.Event, error) {
	org := n.p.Organization
	if org == nil || org.ID <= 0 || !loginPattern.MatchString(org.Login) {
		if n.p.Action == "member_added" || n.p.Action == "member_removed" || n.p.Action == "member_invited" {
			return nil, invalidPayload("no organization")
		}
		return []provider.Event{}, nil
	}
	subject := provider.Subject{Kind: "organization", ExternalID: strconv.FormatInt(org.ID, 10), Title: org.Login,
		URL: "https://" + webHost + "/" + org.Login}
	data := map[string]any{}
	switch n.p.Action {
	case "member_added", "member_removed":
		m := n.p.Membership
		if m == nil || m.User == nil {
			return nil, invalidPayload("no membership")
		}
		if m.Role == "admin" || m.Role == "member" {
			data["role"] = m.Role
		}
		return n.membership(subject, *m.User, strings.TrimPrefix(n.p.Action, "member_"), data)
	case "member_invited":
		if n.p.User == nil {
			return []provider.Event{}, nil
		}
		if inv := n.p.Invitation; inv != nil {
			switch inv.Role {
			case "admin":
				data["role"] = "admin"
			case "direct_member":
				data["role"] = "member"
			}
		}
		return n.membership(subject, *n.p.User, "invited", data)
	}
	return []provider.Event{}, nil
}

// mentions returns the accounts mentioned in text, without the actor.
func (n *normalizer) mentions(text string) []provider.Target {
	out := []provider.Target{}
	for _, login := range Mentions(text) {
		if strings.EqualFold(login, n.actor.Login) {
			continue // a self-mention
		}
		out = append(out, provider.Target{ExternalRef: provider.ExternalRef{Login: login}, Reason: "mentioned"})
	}
	return out
}

// truncate makes s valid UTF-8 without NUL and at most n runes. NUL is
// removed because PostgreSQL's jsonb rejects \u0000; the payload is
// untrusted even after signing.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\x00", "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// excerpt is the plain text of s: whitespace collapsed, at most 280 runes.
// Nothing is rendered; Markdown stays as text. Only a bounded prefix is
// read.
func excerpt(s string) string {
	if len(s) > 4*maxExcerpt*4 {
		s = s[:4*maxExcerpt*4]
	}
	return truncate(strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " "), maxExcerpt)
}
