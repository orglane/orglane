package peopleimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
)

// GitHubMaxRows applies only to internally constructed GitHub sources.
const GitHubMaxRows = 2000
const githubOrigin = "https://api.github.com"
const githubMaxResponse = 1 << 20
const githubMaxBytes = 32 << 20
const githubMaxRequests = 2022 // org + 21 member pages (overflow probe) + 2000 profiles
var githubSlug = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// ErrGitHub is a fixed, credential-free failure for incomplete source reads.
var ErrGitHub = errors.New("import: github_fetch_failed")

// githubSource has no public arbitrary-origin constructor. Tests in this package
// inject an httptest client; production always uses the fixed GitHub origin.
type githubSource struct {
	ctx                              context.Context
	organization, token, origin, key string
	client                           *http.Client
	requests, bytes                  int
	exhausted                        bool
}

func (s *githubSource) Key() string { return s.key }

// FetchGitHub uses credentials only within the synchronous source read. Source
// errors are deliberately value-free and never wrap HTTP errors or response bodies.
func (s *Service) FetchGitHub(ctx context.Context, actor audit.Actor, organization, token string) (Run, error) {
	if !authz.PrincipalFrom(ctx).Has(authz.PeopleManage) {
		return Run{}, ErrForbidden
	}
	// Recheck current database grants before doing any outbound work.
	if err := s.checkGitHubAuthority(ctx, actor); err != nil {
		return Run{}, err
	}
	if !githubSlug.MatchString(organization) || len(token) == 0 || len(token) > 255 || strings.ContainsAny(token, " \r\n\t") {
		return Run{}, ErrGitHub
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	source := &githubSource{ctx: ctx, organization: organization, token: token, origin: githubOrigin,
		client: githubHTTPClient()}
	defer func() { source.token = "" }()
	defer source.client.CloseIdleConnections()
	return s.Upload(ctx, actor, source, nil)
}

func (s *githubSource) get(path string, out any) error {
	if s.exhausted || s.ctx.Err() != nil || s.requests >= githubMaxRequests {
		return ErrGitHub
	}
	s.requests++
	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, s.origin+path, nil)
	if err != nil {
		return ErrGitHub
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	resp, err := s.client.Do(req)
	if err != nil {
		return ErrGitHub
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		s.exhausted = true
	}
	// No automatic retries: rate limits (including Retry-After) fail atomically.
	// The administrator may retry with a fresh token after the limit resets.
	if resp.StatusCode != http.StatusOK {
		return ErrGitHub
	}
	limit := min(githubMaxResponse, githubMaxBytes-s.bytes)
	if limit <= 0 {
		return ErrGitHub
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	s.bytes += len(b)
	if err != nil || len(b) > limit || s.bytes > githubMaxBytes {
		return ErrGitHub
	}
	if json.Unmarshal(b, out) != nil {
		return ErrGitHub
	}
	return nil
}

type githubUser struct {
	ID     int64  `json:"id"`
	Login  string `json:"login"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	Avatar string `json:"avatar_url"`
}

func (s *githubSource) Read(io.Reader) (Dataset, error) {
	defer func() { s.token = "" }()
	var org struct {
		ID int64 `json:"id"`
	}
	if s.get("/orgs/"+s.organization, &org) != nil || org.ID <= 0 {
		return Dataset{}, ErrGitHub
	}
	s.key = "github:" + strconv.FormatInt(org.ID, 10)
	out := Dataset{Headers: []string{"username", "display_name", "contact_email", "external_id"}, Encoding: "UTF-8"}
	seen := map[int64]bool{}
	// Construct numbered URLs ourselves. Link headers are never followed, so no
	// malicious, cyclic or cross-origin pagination can forward credentials.
	for page := 1; page <= 21; page++ {
		var members []githubUser
		if s.get(fmt.Sprintf("/orgs/%s/members?per_page=100&page=%d", s.organization, page), &members) != nil || members == nil || len(members) > 100 {
			return Dataset{}, ErrGitHub
		}
		for _, member := range members {
			if len(out.Rows) >= GitHubMaxRows || member.ID <= 0 || seen[member.ID] {
				return Dataset{}, ErrGitHub
			}
			seen[member.ID] = true
			var user githubUser
			if s.get("/user/"+strconv.FormatInt(member.ID, 10), &user) != nil || user.ID != member.ID || !githubSlug.MatchString(user.Login) {
				return Dataset{}, ErrGitHub
			}
			// Even a malformed upstream response must not reflect the credential
			// into staging or account metadata.
			for _, value := range []string{user.Login, user.Name, user.Email, user.Avatar} {
				if s.token != "" && strings.Contains(value, s.token) {
					return Dataset{}, ErrGitHub
				}
			}
			cells := []string{user.Login, user.Name, user.Email, strconv.FormatInt(user.ID, 10)}
			if !validCells(cells) || !validCells([]string{user.Avatar}) {
				return Dataset{}, ErrGitHub
			}
			out.Rows = append(out.Rows, Record{Number: len(out.Rows) + 2, Cells: cells, GitHub: &GitHubAccount{OrganizationID: org.ID, UserID: user.ID, Login: user.Login, AvatarURL: user.Avatar}})
		}
		if len(members) == 0 {
			if len(out.Rows) == 0 {
				return Dataset{}, ErrGitHub
			}
			return out, nil
		}
	}
	return Dataset{}, ErrGitHub
}

// GitHubAccount is unverified source metadata, encrypted with the staged row.
// Avatar URLs are evidence only: no remote avatar bytes are fetched or rendered.
type GitHubAccount struct {
	OrganizationID int64
	UserID         int64
	Login          string
	AvatarURL      string
}

var githubHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = 16 << 10
	transport.ResponseHeaderTimeout = 10 * time.Second
	transport.MaxConnsPerHost = 1
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
