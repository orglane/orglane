package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

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
		Settings: map[string]any{SettingSSHPort: port},
		Secrets:  []string{CredentialDBPassword, CredentialSecretKey, CredentialAdminPassword},
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
	if !organizationRE.MatchString(org) {
		return &provider.Error{Code: CodeInvalidSettings, Err: errors.New("forgejo: the organization setting is missing or invalid")}
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
	return ensureOrganization(ctx, c, org)
}

// ensureOrganization creates the organization unless it exists. An
// organization that appears between the check and the creation (another
// attempt) counts as created.
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
	if err == nil {
		return nil
	}
	if status(err) == http.StatusUnprocessableEntity || status(err) == http.StatusConflict {
		if again := c.do(ctx, http.MethodGet, path, nil, nil); again == nil {
			return nil
		}
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
// token, which Forgejo mints (value is nil). The token is named after the
// version (TokenName), so the read-back is the token list: a token of this
// version found there was minted by an earlier attempt whose answer was
// lost. Its value is unknown, so it is deleted and minted again.
//
// Outcomes: acknowledged with the minted value; rejected when Forgejo
// refuses definitely (authentication, validation); unknown otherwise (no
// answer, server errors, a name conflict that the next attempt resolves).
func (*Adapter) ApplyCredential(ctx context.Context, env provider.Env, inst provider.Instance, name string, version int, _ []byte) (provider.Rotation, error) {
	if name != CredentialAdminToken {
		return provider.Rotation{Outcome: provider.RotationRejected}, nil
	}
	c, err := newClient(ctx, env, inst, true)
	if err != nil {
		return provider.Rotation{Outcome: provider.RotationUnknown}, err
	}
	tokens := "/api/v1/users/" + url.PathEscape(AdminUsername) + "/tokens"
	tokenName := TokenName(version)

	for page := 1; ; page++ {
		var list []accessToken
		q := url.Values{"limit": {strconv.Itoa(tokenPageSize)}, "page": {strconv.Itoa(page)}}
		if err := c.doQuery(ctx, http.MethodGet, tokens, q, nil, &list); err != nil {
			return rotationFailure(err)
		}
		for _, t := range list {
			if t.Name != tokenName {
				continue
			}
			err := c.do(ctx, http.MethodDelete, tokens+"/"+strconv.FormatInt(t.ID, 10), nil, nil)
			if err != nil && status(err) != http.StatusNotFound {
				return rotationFailure(err)
			}
		}
		if len(list) < tokenPageSize {
			break
		}
	}

	var minted accessToken
	err = c.do(ctx, http.MethodPost, tokens, map[string]any{"name": tokenName, "scopes": adminTokenScopes}, &minted)
	if err != nil {
		return rotationFailure(err)
	}
	if minted.SHA1 == "" || minted.Name != tokenName {
		return provider.Rotation{Outcome: provider.RotationUnknown}, &provider.Error{
			Code: CodeUnavailable, Retryable: true, Err: errors.New("forgejo: the token answer has no value"),
		}
	}
	return provider.Rotation{Outcome: provider.RotationAcknowledged, Value: []byte(minted.SHA1)}, nil
}

// rotationFailure classifies a failed rotation call: a definite refusal is
// rejected (nothing was minted), anything else unknown.
func rotationFailure(err error) (provider.Rotation, error) {
	switch s := status(err); {
	case s == http.StatusConflict || s == http.StatusUnprocessableEntity:
		// A token with the name exists after all (an attempt still in flight
		// when this one listed): the next attempt deletes and mints again.
		return provider.Rotation{Outcome: provider.RotationUnknown}, classify(err)
	case s >= 400 && s < 500 && s != http.StatusRequestTimeout && s != http.StatusTooManyRequests:
		return provider.Rotation{Outcome: provider.RotationRejected}, nil
	default:
		return provider.Rotation{Outcome: provider.RotationUnknown}, classify(err)
	}
}
