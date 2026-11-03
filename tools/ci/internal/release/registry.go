package release

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// errRedirect is returned for any redirect from the registry or its token
// endpoint. Neither GHCR endpoint redirects, and Go's client would forward the
// Authorization header to another port, a subdomain or plain HTTP.
var errRedirect = errors.New("registry redirects are not followed: credentials stay on the registry origin")

// noRedirects returns a copy of the client (or of http.DefaultClient) that
// refuses every redirect, for the token request and every page alike.
func noRedirects(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	clone := *c
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	return &clone
}

// maxPages bounds tag list pagination, so a misbehaving registry cannot keep
// the job busy forever.
const maxPages = 100

// Registry lists image tags through the OCI distribution API
// (GET /v2/<repository>/tags/list), as needed to compute the aliases from
// the published versions.
type Registry struct {
	// URL is the registry base URL, for example https://ghcr.io.
	URL string
	// Username and Password are exchanged for a bearer token when the
	// registry asks for one (on GHCR: any user name and the workflow's
	// GITHUB_TOKEN). They and the token are only ever sent to the registry's
	// own origin (scheme and host:port): the token realm and pagination links
	// must be on it, and redirects are refused.
	Username, Password string
	Client             *http.Client
}

// ListTags returns every tag of the repository, following pagination.
func (r Registry) ListTags(ctx context.Context, repository string) ([]string, error) {
	base, err := url.Parse(r.URL)
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("invalid registry URL %q", r.URL)
	}
	client := noRedirects(r.Client)

	next := base.JoinPath("v2", repository, "tags", "list")
	next.RawQuery = "n=1000"
	var token string
	var tags []string
	for page := 0; next != nil; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("tag list of %s has more than %d pages", repository, maxPages)
		}
		resp, err := r.get(ctx, client, next.String(), token)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && token == "" {
			challenge := resp.Header.Get("WWW-Authenticate")
			drain(resp)
			if token, err = r.token(ctx, client, base, challenge); err != nil {
				return nil, err
			}
			page--
			continue
		}
		if resp.StatusCode != http.StatusOK {
			drain(resp)
			return nil, fmt.Errorf("list tags of %s: %s", repository, resp.Status)
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&body)
		link := resp.Header.Get("Link")
		drain(resp)
		if err != nil {
			return nil, fmt.Errorf("list tags of %s: %w", repository, err)
		}
		tags = append(tags, body.Tags...)
		if next, err = nextPage(base, link); err != nil {
			return nil, err
		}
	}
	return tags, nil
}

func (r Registry) get(ctx context.Context, client *http.Client, u, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return client.Do(req) //nolint:gosec // the URL is built from the registry URL the caller chose
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// token answers a "Bearer realm=...,service=...,scope=..." challenge.
func (r Registry) token(ctx context.Context, client *http.Client, base *url.URL, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("registry asks for unsupported authentication %q", scheme)
	}
	p := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(params, -1) {
		p[m[1]] = m[2]
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Host != base.Host || realm.Scheme != base.Scheme {
		// Credentials go to the registry itself only.
		return "", fmt.Errorf("registry token realm %q is not on %s", p["realm"], base.Host)
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	realm.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if r.Password != "" {
		req.SetBasicAuth(r.Username, r.Password)
	}
	resp, err := client.Do(req) //nolint:gosec // the realm is checked to be on the registry host
	if err != nil {
		return "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token: %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("registry token: %w", err)
	}
	if t := cmp.Or(body.Token, body.AccessToken); t != "" {
		return t, nil
	}
	return "", errors.New("registry token: empty response")
}

var nextLink = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// nextPage resolves the Link header's next page against the registry; it
// must stay on the registry host.
func nextPage(base *url.URL, link string) (*url.URL, error) {
	m := nextLink.FindStringSubmatch(link)
	if m == nil {
		return nil, nil
	}
	u, err := base.Parse(m[1])
	if err != nil || u.Host != base.Host || u.Scheme != base.Scheme {
		return nil, fmt.Errorf("registry pagination link %q leaves %s", m[1], base.Host)
	}
	return u, nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}
