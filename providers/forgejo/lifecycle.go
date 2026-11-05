package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/orglane/orglane/providers/provider"
)

// adminTokenScopes are the scopes of the admin token: accounts (#62),
// organization teams and hooks (#62, #63), and reading its own user.
var adminTokenScopes = []string{"write:admin", "write:organization", "read:user"}

// tokenPageSize is the page size when listing the admin's tokens.
const tokenPageSize = 50

var organizationRE = regexp.MustCompile(`^(?:` + organizationPattern + `)$`)

// Inputs implements provider.Lifecycle: the package settings and the
// credentials the agent renders into the project.
func (*Adapter) Inputs(_ context.Context, _ provider.Env, inst provider.Instance) (provider.Inputs, error) {
	port := int64(DefaultSSHPort)
	if v, ok := inst.Settings[SettingSSHPort]; ok {
		n, ok := v.(int64)
		if !ok {
			return provider.Inputs{}, &provider.Error{Code: CodeInvalidSettings, Err: fmt.Errorf("forgejo: %s is %T, want int64", SettingSSHPort, v)}
		}
		port = n
	}
	return provider.Inputs{
		Settings: map[string]any{SettingSSHPort: port, settingOIDCClientID: provider.OIDCClientID(inst.Key)},
		Secrets:  []string{CredentialDBPassword, CredentialSecretKey, CredentialAdminPassword, CredentialOIDCClientSecret},
	}, nil
}

// Setup implements provider.Lifecycle. The package's configure hook has
// created the service admin; Setup checks that it can sign in as an admin
// and makes the company's organization exist. It is idempotent.
//
// The admin token is not minted here: Env cannot store credentials. It is
// minted by the core's staged rotation through ApplyCredential, which can
// tell from Forgejo whether an attempt committed.
func (*Adapter) Setup(ctx context.Context, env provider.Env, inst provider.Instance) error {
	org, _ := inst.Settings[SettingOrganization].(string)
	if err := validOrganization(org); err != nil {
		return &provider.Error{Code: CodeInvalidSettings, Err: err}
	}
	c, err := newClient(ctx, env, inst, true)
	if err != nil {
		return err
	}
	var me struct {
		Login   string `json:"login"`
		IsAdmin bool   `json:"is_admin"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/user", nil, &me); err != nil {
		return classify(err)
	}
	if me.Login != AdminUsername || !me.IsAdmin {
		return &provider.Error{Code: CodeAdminAuth, Err: errors.New("forgejo: the service account is not an admin")}
	}
	if _, err := authSourceID(ctx, c); err != nil {
		return err
	}
	return ensureOrganization(ctx, c, org)
}

// ensureOrganization creates the organization unless it exists. A refused
// creation is read back before it counts: the organization may have been
// created by another attempt in between (Forgejo then refuses the name),
// and a read-back that fails is classified on its own (a transient failure
// stays retryable). Only a read-back that finds no organization makes the
// refusal final.
func ensureOrganization(ctx context.Context, c *client, org string) error {
	path := "/api/v1/orgs/" + url.PathEscape(org)
	err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err == nil {
		return nil
	}
	if status(err) != http.StatusNotFound {
		return classify(err)
	}
	// "limited": visible to every signed-in person of the company, hidden
	// from anonymous visitors.
	create := map[string]any{"username": org, "visibility": "limited"}
	err = c.do(ctx, http.MethodPost, "/api/v1/orgs", create, nil)
	if err == nil || !definite(err) {
		// No answer or a server error: the next attempt reads it back.
		return classifyOrNil(err)
	}
	switch again := c.do(ctx, http.MethodGet, path, nil, nil); {
	case again == nil:
		return nil
	case status(again) == http.StatusNotFound:
		return classify(err)
	default:
		return classify(again)
	}
}

func classifyOrNil(err error) error {
	if err == nil {
		return nil
	}
	return classify(err)
}

// CheckHealth implements provider.Lifecycle: Forgejo's health endpoint
// through the upstream (the fenced alias while an upgrade candidate runs).
func (*Adapter) CheckHealth(ctx context.Context, env provider.Env, inst provider.Instance) error {
	c, err := newClient(ctx, env, inst, false)
	if err != nil {
		return err
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/healthz", nil, &health); err != nil {
		if status(err) == http.StatusServiceUnavailable {
			return &provider.Error{Code: CodeUnhealthy, Retryable: true, Err: err}
		}
		return classify(err)
	}
	if health.Status != "pass" && health.Status != "warn" {
		return &provider.Error{Code: CodeUnhealthy, Retryable: true, Err: errors.New("forgejo: health status is not pass")}
	}
	return nil
}

// TokenName returns the name of the admin token of a credential version.
func TokenName(version int) string { return "orglane-" + strconv.Itoa(version) }

type accessToken struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	SHA1 string `json:"sha1"`
}

// ApplyCredential implements provider.CredentialRotator for the admin
// token, which Forgejo mints. Tokens are named after their version
// (TokenName), so the admin's token list is the read-back.
//
// The core calls it in two ways (docs/specs/providers.md "Credentials"):
//
//   - value nil (forward rotation): mint the candidate version. A token of
//     this version already in the list was minted by an attempt whose answer
//     was lost; its value is unknown, so it is deleted and minted again.
//   - value set (abandon: the core re-applies the active version it holds):
//     keep that token. It is checked to still authenticate as the service
//     admin, and every newer token (an abandoned candidate that may have
//     been minted) is deleted, so only the held value stays in use. Nothing
//     is minted: the core keeps the value it has.
//
// Outcomes: acknowledged only when the result is proven. Rejected only when
// a definite refusal is backed by a read-back proving that no token of the
// version exists, from this attempt or an earlier one. Everything else is
// unknown with the error (no answer, server errors, a read-back or a
// cleanup that failed, a refused mint whose name is taken after all).
func (*Adapter) ApplyCredential(ctx context.Context, env provider.Env, inst provider.Instance, name string, version int, value []byte) (provider.Rotation, error) {
	if name != CredentialAdminToken {
		return provider.Rotation{Outcome: provider.RotationRejected}, nil
	}
	c, err := newClient(ctx, env, inst, true)
	if err != nil {
		return provider.Rotation{Outcome: provider.RotationUnknown}, err
	}
	if value != nil {
		return reassertToken(ctx, c, version, string(value))
	}
	return mintToken(ctx, c, version)
}

func tokensPath() string { return "/api/v1/users/" + url.PathEscape(AdminUsername) + "/tokens" }

// listTokens returns all of the admin's tokens.
func (c *client) listTokens(ctx context.Context) ([]accessToken, error) {
	var all []accessToken
	for page := 1; ; page++ {
		var list []accessToken
		q := url.Values{"limit": {strconv.Itoa(tokenPageSize)}, "page": {strconv.Itoa(page)}}
		if err := c.doQuery(ctx, http.MethodGet, tokensPath(), q, nil, &list); err != nil {
			return nil, err
		}
		all = append(all, list...)
		if len(list) < tokenPageSize {
			return all, nil
		}
	}
}

// deleteTokens deletes the listed tokens that match. A token that is gone
// already counts as deleted.
func (c *client) deleteTokens(ctx context.Context, tokens []accessToken, match func(accessToken) bool) error {
	for _, t := range tokens {
		if !match(t) {
			continue
		}
		err := c.do(ctx, http.MethodDelete, tokensPath()+"/"+strconv.FormatInt(t.ID, 10), nil, nil)
		if err != nil && status(err) != http.StatusNotFound {
			return err
		}
	}
	return nil
}

func unknown(err error) (provider.Rotation, error) {
	return provider.Rotation{Outcome: provider.RotationUnknown}, classify(err)
}

func mintToken(ctx context.Context, c *client, version int) (provider.Rotation, error) {
	tokenName := TokenName(version)
	tokens, err := c.listTokens(ctx)
	if err != nil {
		return unknown(err)
	}
	if err := c.deleteTokens(ctx, tokens, func(t accessToken) bool { return t.Name == tokenName }); err != nil {
		return unknown(err)
	}
	var minted accessToken
	err = c.do(ctx, http.MethodPost, tokensPath(), map[string]any{"name": tokenName, "scopes": adminTokenScopes}, &minted)
	if err == nil {
		if minted.SHA1 == "" || minted.Name != tokenName {
			return unknown(errors.New("forgejo: the token answer has no value"))
		}
		return provider.Rotation{Outcome: provider.RotationAcknowledged, Value: []byte(minted.SHA1)}, nil
	}
	if !definite(err) {
		return unknown(err)
	}
	// A refusal proves only that this request minted nothing. Forgejo also
	// refuses a name that exists (an attempt still in flight when this one
	// listed): read back before treating it as rejected.
	after, lerr := c.listTokens(ctx)
	if lerr != nil {
		return unknown(lerr)
	}
	for _, t := range after {
		if t.Name == tokenName {
			return unknown(err)
		}
	}
	return provider.Rotation{Outcome: provider.RotationRejected}, nil
}

func reassertToken(ctx context.Context, c *client, version int, value string) (provider.Rotation, error) {
	tokens, err := c.listTokens(ctx)
	if err != nil {
		return unknown(err)
	}
	newer := func(t accessToken) bool {
		v, ok := tokenVersion(t.Name)
		return ok && v > version
	}
	if err := c.deleteTokens(ctx, tokens, newer); err != nil {
		return unknown(err)
	}
	var me struct {
		Login string `json:"login"`
	}
	err = c.withToken(value).do(ctx, http.MethodGet, "/api/v1/user", nil, &me)
	switch {
	case err == nil && me.Login == AdminUsername:
		return provider.Rotation{Outcome: provider.RotationAcknowledged}, nil
	case err == nil:
		return provider.Rotation{Outcome: provider.RotationRejected}, nil
	case status(err) == http.StatusUnauthorized:
		// Forgejo does not accept the held value (deleted outside Orglane):
		// it is not in use, and re-applying cannot bring it back.
		return provider.Rotation{Outcome: provider.RotationRejected}, nil
	default:
		return unknown(err)
	}
}

// tokenVersion returns the version of a token named by TokenName.
func tokenVersion(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "orglane-")
	if !ok {
		return 0, false
	}
	v, err := strconv.Atoi(rest)
	if err != nil || v < 1 || strconv.Itoa(v) != rest {
		return 0, false
	}
	return v, true
}

// authSourcePath is where the configure hook publishes the sign-in
// source's ID (Forgejo has no API that lists sources).
const authSourcePath = "/assets/orglane/auth-source.json"

// authSourceID returns the ID of the Orglane sign-in source. Until the
// configure hook has published it, the error is retryable
// (CodeSSONotConfigured).
func authSourceID(ctx context.Context, c *client) (int64, error) {
	var src struct {
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, authSourcePath, nil, &src); err != nil {
		if status(err) == http.StatusNotFound {
			return 0, &provider.Error{Code: CodeSSONotConfigured, Retryable: true, Err: err}
		}
		return 0, classify(err)
	}
	if src.Name != AuthSourceName || src.ID < 1 {
		return 0, &provider.Error{Code: CodeSSONotConfigured, Retryable: true, Err: errors.New("forgejo: the published sign-in source is not Orglane's")}
	}
	return src.ID, nil
}
