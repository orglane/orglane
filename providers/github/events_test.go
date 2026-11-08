package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

// fixtureSecret signs the recorded payloads; no real App uses it.
var fixtureSecret = []byte("test-only-fixture-webhook-secret-github")

var receivedAt = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

// octocoders is an instance of the installation and organization the
// octokit payload examples were recorded for (testdata/webhooks/README.md).
func octocoders() provider.Instance {
	return provider.Instance{ID: uuid.New(), Provider: "github", BaseURL: &url.URL{Scheme: "https", Host: "github.com", Path: "/Octocoders"},
		Settings: map[string]any{"organization": "Octocoders", "organization_id": "38302899", "installation_id": "1", "app_id": "456"}}
}

func payloadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "webhooks", name+".json")) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// withFields sets fields of a JSON object, addressed by dotted paths, and
// re-encodes it (before signing).
func withFields(t *testing.T, raw []byte, fields map[string]any) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for path, v := range fields {
		parts := strings.Split(path, ".")
		m := doc
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = v
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func deliveryOf(event string, body []byte) provider.Delivery {
	return provider.Delivery{ID: "72d3162e-cc78-11e3-81ab-4c9367dc0958", EventName: event, Body: body, ReceivedAt: receivedAt}
}

type secretsEnv struct {
	testEnv
	secrets [][]byte
}

func (e *secretsEnv) Credentials(context.Context, string) ([][]byte, error) { return e.secrets, nil }

func TestVerify(t *testing.T) {
	body := payloadFixture(t, "pull_request.opened.with-organization")
	env := &secretsEnv{secrets: [][]byte{[]byte("new-secret"), fixtureSecret}}
	h := func(sig string) http.Header {
		hd := http.Header{}
		if sig != "" {
			hd.Set(SignatureHeader, sig)
		}
		return hd
	}
	good := Sign(fixtureSecret, body)
	// A known vector: GitHub's documented example (secret "It's a Secret to
	// Everybody", payload "Hello, World!"), from
	// https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries#testing-the-webhook-payload-validation
	if got := Sign([]byte("It's a Secret to Everybody"), []byte("Hello, World!")); got != "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17" {
		t.Fatalf("Sign does not match GitHub's example: %s", got)
	}
	if err := New().Verify(t.Context(), env, octocoders(), h(good), body); err != nil {
		t.Fatalf("a rotation's previous secret did not verify: %v", err)
	}
	for name, sig := range map[string]string{
		"missing":       "",
		"wrong secret":  Sign([]byte("wrong"), body),
		"no prefix":     strings.TrimPrefix(good, "sha256="),
		"sha1":          "sha1=" + strings.TrimPrefix(good, "sha256=")[:40],
		"not hex":       "sha256=" + strings.Repeat("z", 64),
		"short":         good[:len(good)-2],
		"upper prefix":  "SHA256=" + strings.TrimPrefix(good, "sha256="),
		"changed body?": Sign(fixtureSecret, append([]byte(nil), body[:len(body)-1]...)),
	} {
		if err := New().Verify(t.Context(), env, octocoders(), h(sig), body); !errors.Is(err, provider.ErrBadSignature) {
			t.Errorf("%s: Verify = %v", name, err)
		}
	}
	// A body changed after signing.
	tampered := append([]byte(nil), body...)
	tampered[10] ^= 1
	if err := New().Verify(t.Context(), env, octocoders(), h(good), tampered); !errors.Is(err, provider.ErrBadSignature) {
		t.Errorf("tampered body: %v", err)
	}
	if err := New().Verify(t.Context(), &secretsEnv{}, octocoders(), h(good), body); !errors.Is(err, provider.ErrBadSignature) {
		t.Errorf("no secrets: %v", err)
	}
}

func TestDeliveryID(t *testing.T) {
	h := http.Header{}
	h.Set(DeliveryHeader, "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	if id, err := New().DeliveryID(h); err != nil || id != "72d3162e-cc78-11e3-81ab-4c9367dc0958" {
		t.Fatalf("DeliveryID = %q, %v", id, err)
	}
	for _, v := range []string{"", "1", "72d3162e-cc78-11e3-81ab-4c9367dc0958x", strings.Repeat("a", 200)} {
		h.Set(DeliveryHeader, v)
		if _, err := New().DeliveryID(h); err == nil {
			t.Errorf("DeliveryID(%q) accepted", v)
		}
	}
}

type wantEvent struct {
	typ, key, subjectKind, subjectID string
	targets                          []string // "<login>/<reason>"
	data                             map[string]any
}

func checkEvents(t *testing.T, got []provider.Event, want []wantEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		e := got[i]
		if e.Type != w.typ || e.DedupeKey != w.key || e.SchemaVersion != 1 || e.Subject.Kind != w.subjectKind || e.Subject.ExternalID != w.subjectID {
			t.Errorf("event %d = %s %s %s/%s, want %s %s %s/%s", i, e.Type, e.DedupeKey, e.Subject.Kind, e.Subject.ExternalID, w.typ, w.key, w.subjectKind, w.subjectID)
		}
		var ts []string
		for _, tg := range e.Targets {
			ts = append(ts, tg.Login+"/"+tg.Reason)
		}
		if strings.Join(ts, ",") != strings.Join(w.targets, ",") {
			t.Errorf("event %d targets = %v, want %v", i, ts, w.targets)
		}
		for k, v := range w.data {
			if e.Data[k] != v {
				t.Errorf("event %d data[%s] = %v, want %v", i, k, e.Data[k], v)
			}
		}
		if len(e.Data) != len(w.data) {
			t.Errorf("event %d data = %v, want only %v", i, e.Data, w.data)
		}
		if e.Actor.ExternalID == "" || e.Actor.Login == "" {
			t.Errorf("event %d has no actor: %+v", i, e.Actor)
		}
	}
}

// TestNormalizeFixtures normalizes recorded GitHub payloads (octokit/webhooks
// examples, see testdata/webhooks/README.md).
func TestNormalizeFixtures(t *testing.T) {
	const mention = "Thanks @octocat and @hubot, see `@notme` and @Codertocat."
	pr := func(extra map[string]any) []byte {
		return withFields(t, payloadFixture(t, "pull_request.opened.with-organization"), extra)
	}
	for _, tc := range []struct {
		name, event string
		body        []byte
		want        []wantEvent
	}{
		{"pull request opened", "pull_request", payloadFixture(t, "pull_request.opened.with-organization"), []wantEvent{
			{typ: EventPullRequestOpened, key: "pr:279147437:opened", subjectKind: "pull_request", subjectID: "279147437",
				targets: []string{"octocat/review_requested", "Codertocat/assigned"},
				data:    map[string]any{"draft": false, "base_ref": "master", "head_ref": "changes"}},
		}},
		{"pull request opened with reviewers, assignees and mentions", "pull_request", pr(map[string]any{
			"pull_request.body":                mention,
			"pull_request.requested_reviewers": []any{map[string]any{"id": 5346, "login": "octocat"}, map[string]any{"id": 5346, "login": "octocat"}},
			"pull_request.assignees":           []any{map[string]any{"id": 1, "login": "hubot"}, map[string]any{"id": 0, "login": "nobody"}, map[string]any{"id": 2, "login": "-bad"}},
		}), []wantEvent{
			{typ: EventPullRequestOpened, key: "pr:279147437:opened", subjectKind: "pull_request", subjectID: "279147437",
				targets: []string{"octocat/review_requested", "hubot/assigned"},
				data:    map[string]any{"draft": false, "base_ref": "master", "head_ref": "changes"}},
			{typ: EventMention, key: "pr:279147437:body_mentions", subjectKind: "pull_request", subjectID: "279147437",
				targets: []string{"octocat/mentioned", "hubot/mentioned"},
				data:    map[string]any{"excerpt": mention}},
		}},
		{"review requested", "pull_request", payloadFixture(t, "pull_request.review_requested"), []wantEvent{
			{typ: EventReviewRequested, key: "pr:279147437:review_requested:5346:2019-05-15T15:20:33Z", subjectKind: "pull_request", subjectID: "279147437",
				targets: []string{"octocat/review_requested"}},
		}},
		{"team review requested", "pull_request", withFields(t, payloadFixture(t, "pull_request.review_requested"), map[string]any{
			"requested_reviewer": nil, "requested_team": map[string]any{"id": 9, "slug": "robotics", "name": "Robotics"},
		}), []wantEvent{
			{typ: EventReviewRequested, key: "pr:279147437:review_requested:team:9:2019-05-15T15:20:33Z", subjectKind: "pull_request", subjectID: "279147437",
				data: map[string]any{"requested_team": "robotics"}},
		}},
		{"review without mentions", "pull_request_review", payloadFixture(t, "pull_request_review.submitted.with-organization"), []wantEvent{}},
		{"review with mentions", "pull_request_review", withFields(t, payloadFixture(t, "pull_request_review.submitted.with-organization"), map[string]any{"review.body": "LGTM @octocat"}), []wantEvent{
			{typ: EventMention, key: "review:237895671:mentions", subjectKind: "pull_request", subjectID: "279147437",
				targets: []string{"octocat/mentioned"}, data: map[string]any{"excerpt": "LGTM @octocat", "review_id": "237895671"}},
		}},
		{"issue comment without mentions", "issue_comment", payloadFixture(t, "issue_comment.created.with-installation"), []wantEvent{}},
		{"issue comment with mentions", "issue_comment", withFields(t, payloadFixture(t, "issue_comment.created.with-installation"), map[string]any{"comment.body": "@octocat ping"}), []wantEvent{
			{typ: EventMention, key: "comment:492700400:mentions", subjectKind: "issue", subjectID: "444500041",
				targets: []string{"octocat/mentioned"}, data: map[string]any{"excerpt": "@octocat ping", "comment_id": "492700400"}},
		}},
		{"comment on a pull request's conversation", "issue_comment", withFields(t, payloadFixture(t, "issue_comment.created.with-installation"), map[string]any{
			"comment.body": "@octocat ping", "issue.pull_request": map[string]any{"url": "https://api.github.com/repos/Octocoders/Hello-World/pulls/1"},
		}), []wantEvent{
			{typ: EventMention, key: "comment:492700400:mentions", subjectKind: "pull_request", subjectID: "444500041",
				targets: []string{"octocat/mentioned"}, data: map[string]any{"excerpt": "@octocat ping", "comment_id": "492700400"}},
		}},
		{"review comment with mentions", "pull_request_review_comment", withFields(t, payloadFixture(t, "pull_request_review_comment.created.with-organization"), map[string]any{"comment.body": "@hubot nit"}), []wantEvent{
			{typ: EventMention, key: "review_comment:284312630:mentions", subjectKind: "pull_request", subjectID: "279147437",
				targets: []string{"hubot/mentioned"}, data: map[string]any{"excerpt": "@hubot nit", "comment_id": "284312630"}},
		}},
		{"issue opened with mentions", "issues", withFields(t, payloadFixture(t, "issues.opened.with-organization"), map[string]any{"installation.id": 1, "issue.body": "cc @octocat"}), []wantEvent{
			{typ: EventMention, key: "issue:444500041:body_mentions", subjectKind: "issue", subjectID: "444500041",
				targets: []string{"octocat/mentioned"}, data: map[string]any{"excerpt": "cc @octocat"}},
		}},
		{"organization member added", "organization", payloadFixture(t, "organization.member_added.with-installation"), []wantEvent{
			{typ: EventMembershipChanged, subjectKind: "organization", subjectID: "38302899",
				targets: []string{"hacktocat/member_added"}, data: map[string]any{"change": "added", "role": "member"}},
		}},
		{"organization member invited", "organization", payloadFixture(t, "organization.member_invited"), []wantEvent{
			{typ: EventMembershipChanged, subjectKind: "organization", subjectID: "38302899",
				targets: []string{"hacktocat/member_invited"}, data: map[string]any{"change": "invited", "role": "member"}},
		}},
		{"team membership removed", "membership", payloadFixture(t, "membership.removed.with-installation"), []wantEvent{
			{typ: EventMembershipChanged, subjectKind: "team", subjectID: "3253328",
				targets: []string{"Codertocat/member_removed"}, data: map[string]any{"change": "removed", "team": "github"}},
		}},
		{"removed from a deleted team", "membership", withFields(t, payloadFixture(t, "membership.removed.with-deleted-team"), map[string]any{"installation.id": 1}), []wantEvent{
			{typ: EventMembershipChanged, subjectKind: "team", subjectID: "3253328",
				targets: []string{"Codertocat/member_removed"}, data: map[string]any{"change": "removed"}},
		}},
		{"repository collaborator added", "member", payloadFixture(t, "member.added.with-installation"), []wantEvent{
			{typ: EventMembershipChanged, subjectKind: "repository", subjectID: "186853002",
				targets: []string{"hacktocat/member_added"}, data: map[string]any{"change": "added"}},
		}},
		{"unhandled action", "pull_request", withFields(t, payloadFixture(t, "pull_request.opened.with-organization"), map[string]any{"action": "labeled"}), []wantEvent{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New().Normalize(octocoders(), deliveryOf(tc.event, tc.body))
			if err != nil {
				t.Fatal(err)
			}
			for i := range tc.want {
				if tc.want[i].typ == EventMembershipChanged {
					tc.want[i].key = got[i].DedupeKey
					if !strings.HasPrefix(got[i].DedupeKey, "body:") || !strings.HasSuffix(got[i].DedupeKey, "/0") || len(got[i].DedupeKey) != len("body:")+64+2 {
						t.Errorf("membership key = %q", got[i].DedupeKey)
					}
					if !got[i].OccurredAt.Equal(receivedAt) {
						t.Errorf("membership time = %v", got[i].OccurredAt)
					}
				}
			}
			checkEvents(t, got, tc.want)
			again, _ := New().Normalize(octocoders(), deliveryOf(tc.event, tc.body))
			for i := range got {
				if again[i].DedupeKey != got[i].DedupeKey {
					t.Errorf("dedupe key is not stable: %s != %s", again[i].DedupeKey, got[i].DedupeKey)
				}
			}
		})
	}
}

func TestNormalizeSubjectAndTime(t *testing.T) {
	got, err := New().Normalize(octocoders(), deliveryOf("pull_request", payloadFixture(t, "pull_request.opened.with-organization")))
	if err != nil {
		t.Fatal(err)
	}
	e := got[0]
	want := provider.Subject{Kind: "pull_request", ExternalID: "279147437", Repository: "Codertocat/Hello-World", Number: 2, Title: "Update the README with new information.", URL: "https://github.com/Codertocat/Hello-World/pull/2"}
	if e.Subject != want {
		t.Fatalf("subject = %+v", e.Subject)
	}
	if !e.OccurredAt.Equal(time.Date(2019, 5, 15, 15, 20, 33, 0, time.UTC)) {
		t.Fatalf("occurred = %v", e.OccurredAt)
	}
	if e.Actor != (provider.ExternalRef{ExternalID: "21031067", Login: "Codertocat"}) {
		t.Fatalf("actor = %+v", e.Actor)
	}
}

func TestNormalizeForeignAndAppLevel(t *testing.T) {
	body := payloadFixture(t, "pull_request.opened.with-organization")
	other := octocoders()
	other.Settings["installation_id"] = "2"
	otherOrg := octocoders()
	otherOrg.Settings["organization_id"] = "1"
	for name, c := range map[string]struct {
		inst  provider.Instance
		event string
		body  []byte
		code  string
	}{
		"another installation":    {other, "pull_request", body, CodeForeignInstallation},
		"another organization":    {otherOrg, "pull_request", body, CodeForeignInstallation},
		"no installation":         {octocoders(), "issues", payloadFixture(t, "issues.opened.with-organization"), CodeNoInstallation},
		"installation of another": {octocoders(), "installation", payloadFixture(t, "installation.deleted"), CodeForeignInstallation},
	} {
		_, err := New().Normalize(c.inst, deliveryOf(c.event, c.body))
		var pe *provider.Error
		if !errors.Is(err, provider.ErrForeignDelivery) || !errors.As(err, &pe) || pe.Code != c.code {
			t.Errorf("%s: Normalize = %v", name, err)
		}
		if _, err := New().Effects(c.inst, deliveryOf(c.event, c.body)); !errors.Is(err, provider.ErrForeignDelivery) {
			t.Errorf("%s: Effects = %v", name, err)
		}
	}
	for _, c := range []struct{ event, fixture string }{
		{"github_app_authorization", "github_app_authorization.revoked"},
		{"meta", "meta.deleted"},
		{"push", "pull_request.opened.with-organization"},
	} {
		got, err := New().Normalize(octocoders(), deliveryOf(c.event, payloadFixture(t, c.fixture)))
		if err != nil || got != nil {
			t.Errorf("%s: Normalize = %v, %v; want ignored", c.event, got, err)
		}
	}
}

func TestEffects(t *testing.T) {
	inst := func(installation, org string) provider.Instance {
		i := octocoders()
		i.Settings["installation_id"], i.Settings["organization_id"] = installation, org
		return i
	}
	for _, c := range []struct {
		name, event, fixture string
		inst                 provider.Instance
		want                 provider.Effect
	}{
		{"installation deleted", "installation", "installation.deleted", inst("2", "1"), provider.Effect{State: provider.EffectDisconnected, Code: CodeInstallationDeleted}},
		{"installation suspended", "installation", "installation.suspend", inst("16598467", "21031067"), provider.Effect{State: provider.EffectDegraded, Code: CodeInstallationSuspended}},
		{"installation unsuspended", "installation", "installation.unsuspend", inst("16598467", "21031067"), provider.Effect{State: provider.EffectRestored, Code: CodeInstallationSuspended}},
		{"webhook deleted", "meta", "meta.deleted", octocoders(), provider.Effect{State: provider.EffectDegraded, Code: CodeWebhookDeleted}},
		{"app authorization revoked", "github_app_authorization", "github_app_authorization.revoked", octocoders(), provider.Effect{}},
		{"organization member added", "organization", "organization.member_added.with-installation", octocoders(), provider.Effect{State: provider.EffectRestored, Code: CodeWebhookDeleted, Resync: true}},
		{"organization member invited", "organization", "organization.member_invited", octocoders(), provider.Effect{State: provider.EffectRestored, Code: CodeWebhookDeleted}},
		{"team membership removed", "membership", "membership.removed.with-installation", octocoders(), provider.Effect{State: provider.EffectRestored, Code: CodeWebhookDeleted, Resync: true}},
		{"repository collaborator", "member", "member.added.with-installation", octocoders(), provider.Effect{State: provider.EffectRestored, Code: CodeWebhookDeleted}},
		{"pull request", "pull_request", "pull_request.opened.with-organization", octocoders(), provider.Effect{State: provider.EffectRestored, Code: CodeWebhookDeleted}},
		{"unhandled event", "push", "pull_request.opened.with-organization", octocoders(), provider.Effect{}},
	} {
		got, err := New().Effects(c.inst, deliveryOf(c.event, payloadFixture(t, c.fixture)))
		if err != nil || got != c.want {
			t.Errorf("%s: Effects = %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	// An installation event of the right installation for another account.
	if _, err := New().Effects(inst("2", "99"), deliveryOf("installation", payloadFixture(t, "installation.deleted"))); !errors.Is(err, provider.ErrForeignDelivery) {
		t.Errorf("installation of another account: %v", err)
	}
	// Installation events create no events.
	if got, err := New().Normalize(inst("2", "1"), deliveryOf("installation", payloadFixture(t, "installation.deleted"))); err != nil || got != nil {
		t.Errorf("installation Normalize = %v, %v", got, err)
	}
}

// TestUntrustedInput: signed payloads are still untrusted (docs/specs/providers.md
// "Untrusted input rules").
func TestUntrustedInput(t *testing.T) {
	base := payloadFixture(t, "pull_request.opened.with-organization")
	long := strings.Repeat("ü", 400)
	got, err := New().Normalize(octocoders(), deliveryOf("pull_request", withFields(t, base, map[string]any{
		"pull_request.title":    long,
		"pull_request.html_url": "https://evil.example/Octocoders/Hello-World/pull/2",
		"pull_request.body":     "@octocat " + strings.Repeat("x ", 1000),
		"pull_request.base.ref": strings.Repeat("r", 300),
	})))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got[0].Subject.Title)); n != maxTitle {
		t.Errorf("title has %d runes", n)
	}
	if got[0].Subject.URL != "" {
		t.Errorf("a URL on another host was kept: %s", got[0].Subject.URL)
	}
	if n := len([]rune(got[0].Data["base_ref"].(string))); n != maxRef {
		t.Errorf("base_ref has %d runes", n)
	}
	if n := len([]rune(got[1].Data["excerpt"].(string))); n != maxExcerpt {
		t.Errorf("excerpt has %d runes", n)
	}
	for _, u := range []string{"http://github.com/x", "https://user@github.com/x", "javascript:alert(1)", "https://github.com.evil.example/x"} {
		if safeURL(u) != "" {
			t.Errorf("safeURL kept %s", u)
		}
	}

	// A bot actor is fine.
	if _, err := New().Normalize(octocoders(), deliveryOf("pull_request", withFields(t, base, map[string]any{"sender.login": "dependabot[bot]"}))); err != nil {
		t.Errorf("bot sender: %v", err)
	}
	for name, body := range map[string][]byte{
		"not JSON":           []byte("{"),
		"invalid sender":     withFields(t, base, map[string]any{"sender.login": "<script>"}),
		"invalid repository": withFields(t, base, map[string]any{"repository.full_name": "Octocoders/../x"}),
		"no pull request":    withFields(t, base, map[string]any{"pull_request": nil}),
		"wrong field type":   withFields(t, base, map[string]any{"pull_request.id": "279147437"}),
	} {
		_, err := New().Normalize(octocoders(), deliveryOf("pull_request", body))
		if code, retryable, _ := provider.Classify(err); code != CodeInvalidPayload || retryable {
			t.Errorf("%s: Normalize = %v", name, err)
		}
	}
	// At most 50 mention targets.
	var b strings.Builder
	for i := range 80 {
		b.WriteString("@user" + strings.Repeat("a", i%30) + string(rune('a'+i%26)) + " ")
	}
	got, err = New().Normalize(octocoders(), deliveryOf("pull_request", withFields(t, base, map[string]any{"pull_request.body": b.String()})))
	if err != nil || len(got[1].Targets) != maxTargets {
		t.Errorf("mention targets = %d, %v", len(got[1].Targets), err)
	}
}
