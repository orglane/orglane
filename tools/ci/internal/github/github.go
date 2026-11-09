// Package github is a minimal GitHub REST and GraphQL client for the CI
// tools. It uses only the standard library, so the tools can be built from
// the base branch without downloading modules.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Client talks to the GitHub API of one repository.
type Client struct {
	Repo       string // "owner/name"
	Token      string
	APIURL     string // REST base URL, e.g. https://api.github.com
	GraphQLURL string
	HTTP       *http.Client
}

// NewFromEnv builds a client from the standard GitHub Actions environment
// (GITHUB_REPOSITORY, GITHUB_TOKEN, GITHUB_API_URL, GITHUB_GRAPHQL_URL).
func NewFromEnv() (*Client, error) {
	c := &Client{
		Repo:       os.Getenv("GITHUB_REPOSITORY"),
		Token:      os.Getenv("GITHUB_TOKEN"),
		APIURL:     envOr("GITHUB_API_URL", "https://api.github.com"),
		GraphQLURL: envOr("GITHUB_GRAPHQL_URL", "https://api.github.com/graphql"),
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
	if c.Repo == "" || !strings.Contains(c.Repo, "/") {
		return nil, errors.New("GITHUB_REPOSITORY must be set to owner/name")
	}
	if c.Token == "" {
		return nil, errors.New("GITHUB_TOKEN must be set")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// PullRequest is the subset of a pull request the tools need.
type PullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   struct {
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// FromFork reports whether the head branch lives outside the base repository
// (an external contribution). A deleted head repository counts as a fork.
func (pr *PullRequest) FromFork() bool {
	return pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName)
}

// LabelNames returns the names of the pull request's labels.
func (pr *PullRequest) LabelNames() []string {
	out := make([]string, len(pr.Labels))
	for i, l := range pr.Labels {
		out[i] = l.Name
	}
	return out
}

// PullRequest fetches pull request n.
func (c *Client) PullRequest(ctx context.Context, n int) (*PullRequest, error) {
	var pr PullRequest
	err := c.get(ctx, c.repoURL("pulls/%d", n), &pr)
	return &pr, err
}

// Comment is an issue (conversation) comment on a pull request.
type Comment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// Comments lists every conversation comment of issue or pull request n.
func (c *Client) Comments(ctx context.Context, n int) ([]Comment, error) {
	var all []Comment
	next := c.repoURL("issues/%d/comments?per_page=100", n)
	for next != "" {
		var page []Comment
		var err error
		if next, err = c.getPage(ctx, next, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
	}
	return all, nil
}

// CanWrite reports whether a user has write (or higher) access to the
// repository.
func (c *Client) CanWrite(ctx context.Context, user string) (bool, error) {
	var resp struct {
		Permission string `json:"permission"` // admin, write, read or none
	}
	err := c.get(ctx, c.repoURL("collaborators/%s/permission", url.PathEscape(user)), &resp)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return false, nil // not a collaborator
	}
	if err != nil {
		return false, err
	}
	return resp.Permission == "admin" || resp.Permission == "write", nil
}

// CheckRun is a check run attached to a commit.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// CheckRuns lists the latest check runs of a commit.
func (c *Client) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var all []CheckRun
	next := c.repoURL("commits/%s/check-runs?filter=latest&per_page=100", url.PathEscape(sha))
	for next != "" {
		var page struct {
			CheckRuns []CheckRun `json:"check_runs"`
		}
		var err error
		if next, err = c.getPage(ctx, next, &page); err != nil {
			return nil, err
		}
		all = append(all, page.CheckRuns...)
	}
	return all, nil
}

// Status is a commit status (the pre-Checks API).
type Status struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

// Statuses lists the latest commit status per context.
func (c *Client) Statuses(ctx context.Context, sha string) ([]Status, error) {
	var combined struct {
		Statuses []Status `json:"statuses"`
	}
	err := c.get(ctx, c.repoURL("commits/%s/status?per_page=100", url.PathEscape(sha)), &combined)
	return combined.Statuses, err
}

// UnresolvedThreads counts the unresolved review threads of pull request n.
func (c *Client) UnresolvedThreads(ctx context.Context, n int) (int, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	const query = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes { isResolved }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`
	unresolved := 0
	var after *string
	for {
		var resp struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "name": name, "number": n, "after": after}
		if err := c.graphql(ctx, query, vars, &resp); err != nil {
			return 0, err
		}
		threads := resp.Repository.PullRequest.ReviewThreads
		for _, t := range threads.Nodes {
			if !t.IsResolved {
				unresolved++
			}
		}
		if !threads.PageInfo.HasNextPage {
			return unresolved, nil
		}
		cursor := threads.PageInfo.EndCursor
		after = &cursor
	}
}

// AddLabel adds a label to issue or pull request n.
func (c *Client) AddLabel(ctx context.Context, n int, label string) error {
	body := map[string][]string{"labels": {label}}
	return c.do(ctx, http.MethodPost, c.repoURL("issues/%d/labels", n), body, nil)
}

// RemoveLabel removes a label from issue or pull request n. Removing a label
// that is not set is not an error.
func (c *Client) RemoveLabel(ctx context.Context, n int, label string) error {
	err := c.do(ctx, http.MethodDelete, c.repoURL("issues/%d/labels/%s", n, url.PathEscape(label)), nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

// OpenPullsForHead lists the open pull requests whose head is
// "owner:branch". GitHub leaves workflow_run.pull_requests empty for forks.
func (c *Client) OpenPullsForHead(ctx context.Context, ownerBranch string) ([]int, error) {
	var pulls []struct {
		Number int `json:"number"`
	}
	q := url.Values{"state": {"open"}, "head": {ownerBranch}, "per_page": {"100"}}
	if err := c.get(ctx, c.repoURL("pulls?%s", q.Encode()), &pulls); err != nil {
		return nil, err
	}
	out := make([]int, len(pulls))
	for i, p := range pulls {
		out[i] = p.Number
	}
	return out, nil
}

// APIError is a non-2xx response.
type APIError struct {
	StatusCode int
	Method     string
	URL        string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub API %s %s: %d %s", e.Method, e.URL, e.StatusCode, e.Message)
}

func (c *Client) repoURL(format string, args ...any) string {
	return strings.TrimSuffix(c.APIURL, "/") + "/repos/" + c.Repo + "/" + fmt.Sprintf(format, args...)
}

func (c *Client) get(ctx context.Context, u string, out any) error {
	return c.do(ctx, http.MethodGet, u, nil, out)
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// getPage fetches one page and returns the URL of the next one ("" if none).
func (c *Client) getPage(ctx context.Context, u string, out any) (string, error) {
	resp, err := c.request(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", fmt.Errorf("decoding %s: %w", u, err)
	}
	if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		return m[1], nil
	}
	return "", nil
}

func (c *Client) do(ctx context.Context, method, u string, body, out any) error {
	resp, err := c.request(ctx, method, u, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s: %w", u, err)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, u string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		var msg struct {
			Message string `json:"message"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if json.Unmarshal(data, &msg) != nil || msg.Message == "" {
			msg.Message = strings.TrimSpace(string(data))
		}
		return nil, &APIError{StatusCode: resp.StatusCode, Method: method, URL: u, Message: msg.Message}
	}
	return resp, nil
}

func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": query, "variables": vars}
	if err := c.do(ctx, http.MethodPost, c.GraphQLURL, body, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("GitHub GraphQL: %s", strings.Join(msgs, "; "))
	}
	return json.Unmarshal(resp.Data, out)
}
