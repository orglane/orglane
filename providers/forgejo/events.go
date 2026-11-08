package forgejo

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
	"unicode"
	"unicode/utf8"

	"github.com/orglane/orglane/providers/provider"
)

// Normalized event types (docs/specs/providers.md "Normalized events").
const (
	EventPullRequestOpened = "code.pull_request.opened"
	EventReviewRequested   = "code.review.requested"
	EventMention           = "code.mention.created"
	EventBranchPushed      = "code.branch.pushed"
)

// Delivery error codes.
const (
	// CodeInvalidPayload: the body is not the JSON of the event.
	CodeInvalidPayload = "invalid_payload"
	// CodeForeignOrganization: the delivery is about another organization.
	CodeForeignOrganization = "foreign_organization"
)

// Limits of normalized strings (untrusted input rules).
const (
	maxTitle    = 256
	maxExcerpt  = 280
	maxRef      = 255
	maxMentions = 50
	maxLogin    = 40
	maxRepoName = 100
)

// The X-Forgejo-Event values Normalize handles. Forgejo sends a pull
// request's opening and review requests as "pull_request" (told apart by
// the action), comments on issues and pull requests as "issue_comment",
// and submitted reviews as "pull_request_comment", "pull_request_approved"
// or "pull_request_rejected" (action "reviewed").
var handledEvents = []string{"push", "pull_request", "issue_comment", "pull_request_comment", "pull_request_approved", "pull_request_rejected"}

var (
	loginRE    = regexp.MustCompile(`^(?:` + organizationPattern + `)$`)
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	shaRE      = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// payloadUser is an account in a webhook payload.
type payloadUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type repository struct {
	ID       int64       `json:"id"`
	FullName string      `json:"full_name"`
	HTMLURL  string      `json:"html_url"`
	Owner    payloadUser `json:"owner"`
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
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
}

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

// payload is the union of the fields Normalize reads; unknown fields are
// ignored.
type payload struct {
	Action            string       `json:"action"`
	Sender            payloadUser  `json:"sender"`
	Repository        *repository  `json:"repository"`
	PullRequest       *pullRequest `json:"pull_request"`
	RequestedReviewer *payloadUser `json:"requested_reviewer"`
	Review            *struct {
		Content string `json:"content"`
	} `json:"review"`
	Issue   *issue   `json:"issue"`
	Comment *comment `json:"comment"`
	IsPull  bool     `json:"is_pull"`
	// push
	Ref          string `json:"ref"`
	Before       string `json:"before"`
	After        string `json:"after"`
	CompareURL   string `json:"compare_url"`
	TotalCommits int    `json:"total_commits"`
}

// normalizer turns one verified delivery into events.
type normalizer struct {
	inst  provider.Instance
	d     provider.Delivery
	p     payload
	sha   string // hex SHA-256 of the raw body
	actor provider.ExternalRef
	repo  string // owner/name
}

// Normalize implements provider.EventSource (docs/specs/providers.md
// "Normalized events" and "Untrusted input rules"). It is a pure function
// of the delivery: the payload is untrusted even though it is signed (a
// Forgejo user controls titles, bodies and branch names), so it is decoded
// into typed structs, strings are validated or truncated, and URLs are kept
// only on the instance's own host. Deliveries about another organization
// are ErrForeignDelivery. Event names it does not handle give nil (the
// delivery is ignored); actions it does not handle give no events.
func (*Adapter) Normalize(inst provider.Instance, d provider.Delivery) ([]provider.Event, error) {
	if !slices.Contains(handledEvents, d.EventName) {
		return nil, nil
	}
	var p payload
	if err := json.Unmarshal(d.Body, &p); err != nil {
		return nil, invalidPayload("not the JSON of a %s event", d.EventName)
	}
	sum := sha256.Sum256(d.Body)
	n := &normalizer{inst: inst, d: d, p: p, sha: hex.EncodeToString(sum[:])}
	if err := n.common(); err != nil {
		return nil, err
	}
	switch d.EventName {
	case "push":
		return n.push()
	case "pull_request":
		switch p.Action {
		case "opened":
			return n.opened()
		case "review_requested":
			return n.reviewRequested()
		}
	case "issue_comment":
		if p.Action == "created" {
			return n.commentMentions()
		}
	default: // a submitted review
		if p.Action == "reviewed" {
			return n.reviewMentions()
		}
	}
	return []provider.Event{}, nil
}

func invalidPayload(format string, args ...any) error {
	return &provider.Error{Code: CodeInvalidPayload, Err: fmt.Errorf("forgejo: "+format, args...)}
}

// common checks what every handled payload carries: the repository of the
// instance's organization and a valid sender.
func (n *normalizer) common() error {
	r := n.p.Repository
	if r == nil || r.ID <= 0 {
		return invalidPayload("no repository")
	}
	org, _ := n.inst.Settings[SettingOrganization].(string)
	if !strings.EqualFold(r.Owner.Login, org) {
		// Forgejo names are case-insensitive.
		return fmt.Errorf("%w: %w", provider.ErrForeignDelivery, &provider.Error{Code: CodeForeignOrganization})
	}
	owner, name, ok := strings.Cut(r.FullName, "/")
	if !ok || !strings.EqualFold(owner, org) || !validRepoName(name) {
		return invalidPayload("invalid repository name")
	}
	n.repo = r.FullName
	actor, ok := payloadRef(n.p.Sender)
	if !ok {
		return invalidPayload("invalid sender")
	}
	n.actor = actor
	return nil
}

func validRepoName(name string) bool {
	return len(name) <= maxRepoName && repoNameRE.MatchString(name) && name != "." && name != ".."
}

func validLogin(login string) bool { return len(login) <= maxLogin && loginRE.MatchString(login) }

// payloadRef is an account with its ID; false if the login is invalid.
func payloadRef(u payloadUser) (provider.ExternalRef, bool) {
	if !validLogin(u.Login) || u.ID == 0 {
		return provider.ExternalRef{}, false
	}
	return provider.ExternalRef{ExternalID: strconv.FormatInt(u.ID, 10), Login: u.Login}, true
}

// targets turns accounts into targets with reason, dropping invalid ones
// and duplicates.
func targets(users []payloadUser, reason string, seen map[string]bool) []provider.Target {
	out := []provider.Target{}
	for _, u := range users {
		r, ok := payloadRef(u)
		if !ok || seen[strings.ToLower(r.Login)+"\x00"+reason] {
			continue
		}
		seen[strings.ToLower(r.Login)+"\x00"+reason] = true
		out = append(out, provider.Target{ExternalRef: r, Reason: reason})
	}
	return out
}

// url keeps a payload URL only if its scheme and host are the instance's.
func (n *normalizer) url(raw string) string {
	base := n.inst.BaseURL
	if base == nil || raw == "" || len(raw) > 2048 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || !strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
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

func (n *normalizer) pullRequest() (*pullRequest, provider.Subject, error) {
	pr := n.p.PullRequest
	if pr == nil || pr.ID <= 0 || pr.Number <= 0 {
		return nil, provider.Subject{}, invalidPayload("no pull request")
	}
	return pr, provider.Subject{
		Kind:       "pull_request",
		ExternalID: strconv.FormatInt(pr.ID, 10),
		Repository: n.repo,
		Number:     pr.Number,
		Title:      truncate(pr.Title, maxTitle),
		URL:        n.url(pr.HTMLURL),
	}, nil
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

// opened: code.pull_request.opened, and the mentions in the description.
func (n *normalizer) opened() ([]provider.Event, error) {
	pr, subject, err := n.pullRequest()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	ts := append(targets(pr.RequestedReviewers, "review_requested", seen), targets(pr.Assignees, "assigned", seen)...)
	data := map[string]any{"draft": pr.Draft, "base_ref": truncate(pr.Base.Ref, maxRef), "head_ref": truncate(pr.Head.Ref, maxRef)}
	at := n.at(pr.CreatedAt)
	events := []provider.Event{n.event(EventPullRequestOpened, fmt.Sprintf("pr:%d:opened", pr.ID), at, subject, ts, data)}
	if m := n.mentions(pr.Body); len(m) > 0 {
		events = append(events, n.event(EventMention, fmt.Sprintf("pr:%d:body_mentions", pr.ID), at, subject, m,
			map[string]any{"excerpt": excerpt(pr.Body)}))
	}
	return events, nil
}

// reviewRequested: one event for the requested reviewer. A team request
// (no requested_reviewer) has no target in M2 and gives no event.
func (n *normalizer) reviewRequested() ([]provider.Event, error) {
	pr, subject, err := n.pullRequest()
	if err != nil {
		return nil, err
	}
	if n.p.RequestedReviewer == nil {
		return []provider.Event{}, nil
	}
	reviewer, ok := payloadRef(*n.p.RequestedReviewer)
	if !ok {
		return nil, invalidPayload("invalid requested reviewer")
	}
	if pr.UpdatedAt.IsZero() {
		return nil, invalidPayload("no updated_at")
	}
	key := fmt.Sprintf("pr:%d:review_requested:%s:%s", pr.ID, reviewer.ExternalID, pr.UpdatedAt.UTC().Format(time.RFC3339))
	ts := []provider.Target{{ExternalRef: reviewer, Reason: "review_requested"}}
	return []provider.Event{n.event(EventReviewRequested, key, n.at(pr.UpdatedAt), subject, ts, nil)}, nil
}

// commentMentions: the mentions in a new comment on an issue or a pull
// request.
func (n *normalizer) commentMentions() ([]provider.Event, error) {
	c := n.p.Comment
	if c == nil || c.ID <= 0 {
		return nil, invalidPayload("no comment")
	}
	var subject provider.Subject
	if n.p.IsPull {
		var err error
		if _, subject, err = n.pullRequest(); err != nil {
			return nil, err
		}
	} else {
		is := n.p.Issue
		if is == nil || is.ID <= 0 || is.Number <= 0 {
			return nil, invalidPayload("no issue")
		}
		subject = provider.Subject{Kind: "issue", ExternalID: strconv.FormatInt(is.ID, 10), Repository: n.repo,
			Number: is.Number, Title: truncate(is.Title, maxTitle), URL: n.url(is.HTMLURL)}
	}
	m := n.mentions(c.Body)
	if len(m) == 0 {
		return []provider.Event{}, nil
	}
	if u := n.url(c.HTMLURL); u != "" {
		subject.URL = u // the comment itself
	}
	data := map[string]any{"excerpt": excerpt(c.Body), "comment_id": strconv.FormatInt(c.ID, 10)}
	return []provider.Event{n.event(EventMention, fmt.Sprintf("comment:%d:mentions", c.ID), n.at(c.CreatedAt), subject, m, data)}, nil
}

// reviewMentions: the mentions in a submitted review's text. Forgejo's
// review payload has no review ID, so the key is the signed body's hash: a
// redelivery of the same bytes is the same event.
func (n *normalizer) reviewMentions() ([]provider.Event, error) {
	pr, subject, err := n.pullRequest()
	if err != nil {
		return nil, err
	}
	if n.p.Review == nil {
		return []provider.Event{}, nil
	}
	m := n.mentions(n.p.Review.Content)
	if len(m) == 0 {
		return []provider.Event{}, nil
	}
	key := fmt.Sprintf("pr:%d:review:%s:mentions", pr.ID, n.sha)
	return []provider.Event{n.event(EventMention, key, n.at(pr.UpdatedAt), subject, m,
		map[string]any{"excerpt": excerpt(n.p.Review.Content)})}, nil
}

// push: code.branch.pushed for a branch that was created or moved. Tags
// and deleted branches give no event; Forgejo's payload does not say
// whether a push was forced, so Data has no "forced".
func (n *normalizer) push() ([]provider.Event, error) {
	p := n.p
	branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	if !ok {
		return []provider.Event{}, nil // a tag
	}
	if branch == "" || len(p.Ref) > maxRef || !utf8.ValidString(p.Ref) || !shaRE.MatchString(p.After) || !shaRE.MatchString(p.Before) {
		return nil, invalidPayload("invalid ref or commit")
	}
	if strings.Trim(p.After, "0") == "" {
		return []provider.Event{}, nil // the branch was deleted
	}
	if p.TotalCommits < 0 {
		return nil, invalidPayload("invalid commit count")
	}
	subject := provider.Subject{Kind: "branch", ExternalID: fmt.Sprintf("%d:%s", p.Repository.ID, branch), Repository: n.repo,
		Title: truncate(branch, maxTitle), URL: n.url(p.CompareURL)}
	data := map[string]any{"ref": p.Ref, "before": p.Before, "after": p.After, "commit_count": p.TotalCommits}
	// The transition and the signed body: two pushes can move a ref along
	// the same commits (A→B, B→C, C→B, or a force push back and forth), and
	// only a redelivery of the same bytes is the same push.
	key := fmt.Sprintf("push:%d:%s:%s:%s", p.Repository.ID, p.Before, p.After, n.sha)
	return []provider.Event{n.event(EventBranchPushed, key, n.d.ReceivedAt, subject, nil, data)}, nil
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

// Mentions returns the logins mentioned as @login in Markdown text, in
// order, deduplicated (case-insensitively), at most 50. Only text is
// scanned, as CommonMark parses it (see markdown.go): code blocks, code
// spans and HTML blocks are skipped, and code spans end with their
// paragraph or heading. A mention starts at "@" that does not follow a
// letter, digit or one of "-._/@`" (so e-mail addresses are no mentions);
// the login is Forgejo's user name pattern, without trailing separators
// ("@bob." mentions bob), not followed by another letter or digit.
//
// The work is linear in the length of text, so a large adversarial body
// cannot occupy a worker.
func Mentions(text string) []string {
	var out []string
	seen := map[string]bool{}
	inlineContent(text, func(content string) bool {
		for _, login := range lineMentions(inlineText(content)) {
			key := strings.ToLower(login)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, login)
			if len(out) == maxMentions {
				return false
			}
		}
		return true
	})
	return out
}

func lineMentions(line string) []string {
	var out []string
	for i := 0; i < len(line); i++ {
		if line[i] != '@' {
			continue
		}
		if i > 0 {
			prev, _ := utf8.DecodeLastRuneInString(line[:i])
			if isLoginRune(prev) || strings.ContainsRune("/@`", prev) {
				continue
			}
		}
		j := i + 1
		for j < len(line) && isLoginRune(rune(line[j])) {
			j++
		}
		login := strings.TrimRight(line[i+1:j], "-._")
		next, _ := utf8.DecodeRuneInString(line[j:])
		switch {
		case !validLogin(login):
		case j < len(line) && (unicode.IsLetter(next) || unicode.IsDigit(next)):
			// "@bøb": the word goes on, it is no login.
		case len(login) == j-i-1 && next == '/':
			// "@org/team" mentions a team, which has no target in M2.
		default:
			out = append(out, login)
		}
		i = j - 1
	}
	return out
}

func isLoginRune(r rune) bool {
	return r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
}

// truncate makes s valid UTF-8 and at most n runes.
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// excerpt is the plain text of s: whitespace collapsed, at most 280 runes.
// Nothing is rendered; Markdown stays as text.
func excerpt(s string) string {
	return truncate(strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " "), maxExcerpt)
}
