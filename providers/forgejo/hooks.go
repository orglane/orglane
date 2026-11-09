package forgejo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/orglane/orglane/providers/provider"
)

// hookEvents are the events of the organization hook. In Forgejo's API,
// "pull_request" subscribes to every pull request event (opened, review
// requested, reviews, comments on pull requests, ...) and "issue_comment"
// to comments on issues.
var hookEvents = []string{"push", "pull_request", "issue_comment"}

// hookPageSize is the page size when listing the organization's hooks.
const hookPageSize = 50

// maxHookPages bounds the hook listing (an organization has few hooks).
const maxHookPages = 20

type hook struct {
	ID     int64             `json:"id"`
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
	Active bool              `json:"active"`
}

func hooksPath(org string) string { return "/api/v1/orgs/" + url.PathEscape(org) + "/hooks" }

// secretMark is the query parameter of a hook's URL that carries the
// fingerprint of the hook's secret (secretFingerprint). The core's webhook
// endpoint ignores it.
const secretMark = "orglane_secret"

// secretFingerprint identifies a secret without revealing it: a truncated,
// domain-separated SHA-256 of a 256-bit random value.
func secretFingerprint(secret []byte) string {
	h := sha256.New()
	h.Write([]byte("orglane/forgejo/webhook-secret\x00"))
	h.Write(secret)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// markedURL is target with the fingerprint of secret.
func markedURL(target string, secret []byte) (string, error) {
	u, err := url.Parse(target)
	if err != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("forgejo: invalid webhook URL")
	}
	u.RawQuery = url.Values{secretMark: {secretFingerprint(secret)}}.Encode()
	return u.String(), nil
}

// isOurs reports whether a hook's URL is target, with or without a mark.
func isOurs(hookURL, target string) bool {
	u, err := url.Parse(hookURL)
	if err != nil {
		return false
	}
	u.RawQuery = ""
	return u.String() == target
}

// listHooks returns the organization's hooks that deliver to target.
func (c *client) listHooks(ctx context.Context, org, target string) ([]hook, error) {
	var ours []hook
	for page := 1; page <= maxHookPages; page++ {
		var list []hook
		q := url.Values{"limit": {strconv.Itoa(hookPageSize)}, "page": {strconv.Itoa(page)}}
		if err := c.doQuery(ctx, http.MethodGet, hooksPath(org), q, nil, &list); err != nil {
			return nil, err
		}
		for _, h := range list {
			if isOurs(h.Config["url"], target) {
				ours = append(ours, h)
			}
		}
		if len(list) < hookPageSize {
			return ours, nil
		}
	}
	return nil, errors.New("forgejo: too many organization hooks")
}

// hookConfig is the hook's configuration: the marked URL, JSON, the hook's
// events, active. The secret is set only at creation.
func hookConfig(marked string, secret []byte) map[string]any {
	cfg := map[string]string{"url": marked, "content_type": "json"}
	if secret != nil {
		cfg["secret"] = string(secret)
	}
	return map[string]any{"config": cfg, "events": hookEvents, "active": true, "branch_filter": "*"}
}

// ensureHook makes exactly one organization hook deliver to target, signed
// with secret, with the hook's events, active.
//
// A hook is Orglane's by its URL (target, the instance's webhook URL); the
// organization's other hooks are never touched. Forgejo neither returns a
// hook's secret nor changes it on an update, so the secret is known only
// from the mark in the hook's URL (secretMark), set when the hook was
// created with it:
//
//   - a hook marked with secret is kept and updated, which repairs changed
//     events, a changed content type or a deactivated hook;
//   - otherwise (no hook, or only hooks with another secret: a rotation, a
//     restored snapshot) a hook with secret is created first and the others
//     are deleted afterwards, so there is no moment without a hook;
//   - extra hooks (a creation whose answer was lost, then repeated) are
//     deleted, so retries converge on one hook.
//
// A secret changed in Forgejo's settings while the URL keeps its mark
// cannot be noticed; rotating the secret replaces the hook.
func ensureHook(ctx context.Context, c *client, org, target string, secret []byte) error {
	if len(secret) == 0 {
		return errors.New("forgejo: empty webhook secret")
	}
	marked, err := markedURL(target, secret)
	if err != nil {
		return err
	}
	hooks, err := c.listHooks(ctx, org, target)
	if err != nil {
		return err
	}
	keep := slices.IndexFunc(hooks, func(h hook) bool { return h.Config["url"] == marked })
	if keep < 0 {
		create := hookConfig(marked, secret)
		create["type"] = "forgejo"
		if err := c.do(ctx, http.MethodPost, hooksPath(org), create, nil); err != nil {
			return err
		}
	} else if err := c.do(ctx, http.MethodPatch, hooksPath(org)+"/"+strconv.FormatInt(hooks[keep].ID, 10), hookConfig(marked, nil), nil); err != nil {
		return err // deleted meanwhile (404): the next attempt creates it
	}
	for i, h := range hooks {
		if i == keep {
			continue
		}
		err := c.do(ctx, http.MethodDelete, hooksPath(org)+"/"+strconv.FormatInt(h.ID, 10), nil, nil)
		if err != nil && status(err) != http.StatusNotFound {
			return err
		}
	}
	return nil
}

// EnsureWebhooks implements provider.EventSource: the organization hook
// delivers to the instance's webhook URL, signed with the active
// webhook_secret (see ensureHook): created if missing, replaced if it has
// another secret (a restored snapshot), repaired if changed. The core calls
// it after every Setup and daily, serialized with rotations of the secret.
func (*Adapter) EnsureWebhooks(ctx context.Context, env provider.Env, inst provider.Instance) error {
	org, _ := inst.Settings[SettingOrganization].(string)
	if err := validOrganization(org); err != nil {
		return &provider.Error{Code: CodeInvalidSettings, Err: err}
	}
	secret, err := env.Credential(ctx, CredentialWebhookSecret)
	if errors.Is(err, provider.ErrNotFound) {
		// No active version: the first one is being rotated in (the core
		// creates a missing secret before it calls this). Retryable: the
		// rotation establishes it.
		return &provider.Error{Code: CodeNoWebhookSecret, Retryable: true, Err: err}
	} else if err != nil {
		return fmt.Errorf("forgejo: webhook secret: %w", err)
	}
	c, err := newClient(ctx, env, inst, true)
	if err != nil {
		return err
	}
	if err := ensureHook(ctx, c, org, env.WebhookURL(inst.ID).String(), secret); err != nil {
		return classify(err)
	}
	return nil
}

// applyWebhookSecret hands a webhook secret to Forgejo (ApplyCredential):
// forward with the candidate, abandon with the active value. The hook is
// replaced by one with value (ensureHook). Forgejo never returns a hook's
// secret, so there is no read-back: success is proven only by Forgejo's
// answers, and every failure is unknown (a refusal cannot prove that an
// earlier attempt's change did not commit); the core re-applies until it
// gets an answer, or proves the candidate by use.
func applyWebhookSecret(ctx context.Context, c *client, inst provider.Instance, target string, value []byte) (provider.Rotation, error) {
	org, _ := inst.Settings[SettingOrganization].(string)
	if err := validOrganization(org); err != nil {
		return provider.Rotation{Outcome: provider.RotationUnknown}, &provider.Error{Code: CodeInvalidSettings, Err: err}
	}
	if err := ensureHook(ctx, c, org, target, value); err != nil {
		return unknown(err)
	}
	return provider.Rotation{Outcome: provider.RotationAcknowledged}, nil
}
