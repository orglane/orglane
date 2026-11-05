package forgejo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/providers/provider"
)

// The fixtures in testdata/webhooks are deliveries of a real Forgejo 15
// (the event header and the exact body), recorded by the integration test's
// scenario: user orglane-admin (1) opens pull request 1 of orglane/app with
// "Please look @bob and `@nobody`", requests carol's (4) review, comments
// "@carol ping", reviews with "thoughts @bob"; an issue comment "@bob issue
// ping" on issue 2; a push that creates branch feature.

var received = time.Date(2026, 11, 5, 6, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) provider.Delivery {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "webhooks", name+".json")) //nolint:gosec // fixed fixture names
	if err != nil {
		t.Fatal(err)
	}
	var f struct{ Event, Body string }
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return provider.Delivery{ID: "d-" + name, EventName: f.Event, Body: []byte(f.Body), ReceivedAt: received}
}

func eventsInstance() provider.Instance {
	base, _ := url.Parse("http://git.orglane.localhost")
	return provider.Instance{ID: uuid.New(), Key: "git", Provider: ID, BaseURL: base,
		Settings: map[string]any{SettingOrganization: "orglane"}}
}

func normalize(t *testing.T, d provider.Delivery) []provider.Event {
	t.Helper()
	events, err := New().Normalize(eventsInstance(), d)
	if err != nil {
		t.Fatalf("normalize %s: %v", d.ID, err)
	}
	return events
}

func logins(ts []provider.Target) []string {
	out := []string{}
	for _, tg := range ts {
		out = append(out, tg.Login+"/"+tg.Reason)
	}
	return out
}

func TestNormalizeRealDeliveries(t *testing.T) {
	admin := provider.ExternalRef{ExternalID: "1", Login: "orglane-admin"}
	pr := provider.Subject{Kind: "pull_request", ExternalID: "1", Repository: "orglane/app", Number: 1, Title: "Add feature",
		URL: "http://git.orglane.localhost/orglane/app/pulls/1"}

	events := normalize(t, fixture(t, "pull_request_opened"))
	if len(events) != 2 {
		t.Fatalf("opened: %d events, want the pull request and the description's mention", len(events))
	}
	opened, mention := events[0], events[1]
	if opened.Type != EventPullRequestOpened || opened.DedupeKey != "pr:1:opened" || opened.Actor != admin || opened.Subject != pr ||
		opened.SchemaVersion != 1 || !opened.OccurredAt.Equal(time.Date(2026, 11, 5, 5, 48, 15, 0, time.UTC)) {
		t.Fatalf("opened = %+v", opened)
	}
	if opened.Data["draft"] != false || opened.Data["base_ref"] != "main" || opened.Data["head_ref"] != "feature" || len(opened.Targets) != 0 {
		t.Fatalf("opened data %v targets %v", opened.Data, opened.Targets)
	}
	// "`@nobody`" is inline code.
	if mention.Type != EventMention || mention.DedupeKey != "pr:1:body_mentions" || !slices.Equal(logins(mention.Targets), []string{"bob/mentioned"}) ||
		mention.Targets[0].ExternalID != "" || mention.Data["excerpt"] != "Please look @bob and `@nobody`" {
		t.Fatalf("description mention = %+v", mention)
	}

	events = normalize(t, fixture(t, "pull_request_review_requested"))
	if len(events) != 1 || events[0].Type != EventReviewRequested || events[0].DedupeKey != "pr:1:review_requested:4:2026-11-05T05:48:15Z" ||
		!slices.Equal(logins(events[0].Targets), []string{"carol/review_requested"}) || events[0].Targets[0].ExternalID != "4" || events[0].Subject != pr {
		t.Fatalf("review requested = %+v", events)
	}

	events = normalize(t, fixture(t, "pull_request_comment_created"))
	want := pr
	want.URL = "http://git.orglane.localhost/orglane/app/pulls/1#issuecomment-3"
	if len(events) != 1 || events[0].Type != EventMention || events[0].DedupeKey != "comment:3:mentions" || events[0].Subject != want ||
		!slices.Equal(logins(events[0].Targets), []string{"carol/mentioned"}) || events[0].Data["comment_id"] != "3" || events[0].Data["excerpt"] != "@carol ping" {
		t.Fatalf("pull request comment = %+v", events)
	}

	d := fixture(t, "pull_request_reviewed")
	sum := sha256.Sum256(d.Body)
	events = normalize(t, d)
	if len(events) != 1 || events[0].Type != EventMention || events[0].DedupeKey != "pr:1:review:"+hex.EncodeToString(sum[:])+":mentions" ||
		!slices.Equal(logins(events[0].Targets), []string{"bob/mentioned"}) || events[0].Data["excerpt"] != "thoughts @bob" {
		t.Fatalf("review = %+v", events)
	}

	events = normalize(t, fixture(t, "issue_comment_created"))
	issue := provider.Subject{Kind: "issue", ExternalID: "2", Repository: "orglane/app", Number: 2, Title: "Bug",
		URL: "http://git.orglane.localhost/orglane/app/issues/2#issuecomment-5"}
	if len(events) != 1 || events[0].DedupeKey != "comment:5:mentions" || events[0].Subject != issue || !slices.Equal(logins(events[0].Targets), []string{"bob/mentioned"}) {
		t.Fatalf("issue comment = %+v", events)
	}

	d = fixture(t, "push")
	sum = sha256.Sum256(d.Body)
	events = normalize(t, d)
	if len(events) != 1 || events[0].Type != EventBranchPushed ||
		events[0].DedupeKey != "push:1:"+strings.Repeat("0", 40)+":1d9f409fe45e70424165d37894742742bc287a54:"+hex.EncodeToString(sum[:]) ||
		events[0].Subject.Kind != "branch" || events[0].Subject.ExternalID != "1:feature" || events[0].Subject.Repository != "orglane/app" ||
		events[0].Data["commit_count"] != 1 || events[0].Data["before"] != strings.Repeat("0", 40) || !events[0].OccurredAt.Equal(received) {
		t.Fatalf("push = %+v", events)
	}
	if _, ok := events[0].Data["forced"]; ok {
		t.Fatal("push data claims to know whether it was forced")
	}
}

// edit decodes a fixture's body, applies change and re-encodes it.
func edit(t *testing.T, d provider.Delivery, change func(map[string]any)) provider.Delivery {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(d.Body, &body); err != nil {
		t.Fatal(err)
	}
	change(body)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	d.Body = raw
	return d
}

func TestNormalizeEdgeCases(t *testing.T) {
	a := New()
	inst := eventsInstance()

	// Event names it does not normalize: ignored (nil, nil).
	for _, name := range []string{"repository", "create", "issues", ""} {
		d := fixture(t, "push")
		d.EventName = name
		if events, err := a.Normalize(inst, d); events != nil || err != nil {
			t.Errorf("event %q: %v, %v; want ignored", name, events, err)
		}
	}
	// Handled events with other actions: processed, no events.
	for _, c := range []struct{ fixture, action string }{
		{"pull_request_opened", "closed"}, {"pull_request_opened", "synchronized"}, {"pull_request_comment_created", "edited"},
		{"pull_request_reviewed", "dismissed"},
	} {
		d := edit(t, fixture(t, c.fixture), func(b map[string]any) { b["action"] = c.action })
		if events, err := a.Normalize(inst, d); err != nil || events == nil || len(events) != 0 {
			t.Errorf("%s %s: %v, %v; want no events", c.fixture, c.action, events, err)
		}
	}
	// Another organization: foreign, with its code.
	foreign := edit(t, fixture(t, "push"), func(b map[string]any) {
		b["repository"].(map[string]any)["owner"].(map[string]any)["login"] = "other"
	})
	_, err := a.Normalize(inst, foreign)
	code, _, _ := provider.Classify(err)
	if !errors.Is(err, provider.ErrForeignDelivery) || code != CodeForeignOrganization {
		t.Fatalf("foreign = %v (%s)", err, code)
	}
	// Organization names are case-insensitive.
	upper := edit(t, fixture(t, "push"), func(b map[string]any) {
		b["repository"].(map[string]any)["owner"].(map[string]any)["login"] = "Orglane"
	})
	if _, err := a.Normalize(inst, upper); err != nil {
		t.Fatalf("organization in another case: %v", err)
	}
	// Broken payloads: permanent invalid_payload.
	for name, d := range map[string]provider.Delivery{
		"not json":        {EventName: "push", Body: []byte("{"), ReceivedAt: received},
		"no repository":   edit(t, fixture(t, "push"), func(b map[string]any) { delete(b, "repository") }),
		"bad repo name":   edit(t, fixture(t, "push"), func(b map[string]any) { b["repository"].(map[string]any)["full_name"] = "orglane/a b" }),
		"bad sender":      edit(t, fixture(t, "push"), func(b map[string]any) { b["sender"].(map[string]any)["login"] = "-x" }),
		"bad sha":         edit(t, fixture(t, "push"), func(b map[string]any) { b["after"] = "xyz" }),
		"no pull request": edit(t, fixture(t, "pull_request_opened"), func(b map[string]any) { delete(b, "pull_request") }),
		"no comment":      edit(t, fixture(t, "issue_comment_created"), func(b map[string]any) { delete(b, "comment") }),
	} {
		_, err := a.Normalize(inst, d)
		if code, retryable, _ := provider.Classify(err); code != CodeInvalidPayload || retryable {
			t.Errorf("%s: %v, want permanent %s", name, err, CodeInvalidPayload)
		}
	}
	// Tags and deleted branches: no events.
	for name, d := range map[string]provider.Delivery{
		"tag":     edit(t, fixture(t, "push"), func(b map[string]any) { b["ref"] = "refs/tags/v1" }),
		"deleted": edit(t, fixture(t, "push"), func(b map[string]any) { b["after"] = strings.Repeat("0", 40) }),
	} {
		if events, err := a.Normalize(inst, d); err != nil || len(events) != 0 {
			t.Errorf("%s: %v, %v", name, events, err)
		}
	}
	// A team review request has no requested reviewer: no event.
	team := edit(t, fixture(t, "pull_request_review_requested"), func(b map[string]any) { b["requested_reviewer"] = nil })
	if events, err := a.Normalize(inst, team); err != nil || len(events) != 0 {
		t.Fatalf("team request: %v, %v", events, err)
	}
	// Untrusted strings: long titles truncated, foreign URLs dropped,
	// invalid and duplicate logins dropped, self-mentions dropped.
	long := strings.Repeat("ä", 300)
	d := edit(t, fixture(t, "pull_request_opened"), func(b map[string]any) {
		pr := b["pull_request"].(map[string]any)
		pr["title"] = long
		pr["html_url"] = "https://evil.example/orglane/app/pulls/1"
		pr["body"] = "@orglane-admin @dave @Dave " + strings.Repeat("x", 400)
		pr["requested_reviewers"] = []any{map[string]any{"id": 5, "login": "dave"}, map[string]any{"id": 6, "login": "bad login"}, map[string]any{"id": 5, "login": "dave"}}
		pr["assignees"] = []any{map[string]any{"id": 5, "login": "dave"}}
	})
	events, err := a.Normalize(inst, d)
	if err != nil || len(events) != 2 {
		t.Fatalf("untrusted: %v, %v", events, err)
	}
	if got := events[0].Subject.Title; got != strings.Repeat("ä", 256) {
		t.Errorf("title has %d runes", len([]rune(got)))
	}
	if events[0].Subject.URL != "" {
		t.Errorf("foreign URL kept: %s", events[0].Subject.URL)
	}
	if got := logins(events[0].Targets); !slices.Equal(got, []string{"dave/review_requested", "dave/assigned"}) {
		t.Errorf("targets = %v", got)
	}
	if got := logins(events[1].Targets); !slices.Equal(got, []string{"dave/mentioned"}) {
		t.Errorf("mentions = %v", got)
	}
	if ex := events[1].Data["excerpt"].(string); len([]rune(ex)) != 280 {
		t.Errorf("excerpt has %d runes", len([]rune(ex)))
	}
	// A comment without mentions: no event.
	quiet := edit(t, fixture(t, "issue_comment_created"), func(b map[string]any) { b["comment"].(map[string]any)["body"] = "no one here" })
	if events, err := a.Normalize(inst, quiet); err != nil || len(events) != 0 {
		t.Fatalf("quiet comment: %v, %v", events, err)
	}
	// Normalize is a pure function: the same delivery, the same events.
	first, second := normalize(t, fixture(t, "pull_request_opened")), normalize(t, fixture(t, "pull_request_opened"))
	f, _ := json.Marshal(first)
	s, _ := json.Marshal(second)
	if string(f) != string(s) {
		t.Fatal("normalizing twice differs")
	}
}

func TestMentions(t *testing.T) {
	many := ""
	for i := range 60 {
		many += " @user" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	for _, c := range []struct {
		name, text string
		want       []string
	}{
		{"plain", "hi @bob and @carol", []string{"bob", "carol"}},
		{"start and punctuation", "@bob, (@carol) @dave. @eve! @frank's", []string{"bob", "carol", "dave", "eve", "frank"}},
		{"separators inside", "@a.b-c_d", []string{"a.b-c_d"}},
		{"trailing separators", "@bob.. @carol-", []string{"bob", "carol"}},
		{"email is not a mention", "mail bob@acme.test or x@y", nil},
		{"after a slash or @", "path/@bob @@carol", nil},
		{"team mention", "@orglane/backend and @bob", []string{"bob"}},
		{"inline code", "`@bob` and ``a ` @carol`` and @dave", []string{"dave"}},
		{"unmatched backtick", "a ` @bob", []string{"bob"}},
		{"fenced", "before @a\n```go\n@bob\n```\nafter @c", []string{"a", "c"}},
		{"tilde fence and longer close", "~~~\n@bob\n~~~~\n@carol", []string{"carol"}},
		{"unclosed fence", "@a\n```\n@bob", []string{"a"}},
		{"fence of another kind does not close", "```\n~~~\n@bob\n```\n@carol", []string{"carol"}},
		{"indented fence", "   ```\n@bob\n   ```\n@carol", []string{"carol"}},
		{"dedupe ignores case", "@Bob @bob @BOB", []string{"Bob"}},
		{"too long", "@" + strings.Repeat("a", 41), nil},
		{"not ascii", "@bøb @bob", []string{"bob"}},
		{"bare @", "@ @. @-", nil},
		{"code span across lines", "`first line\n@bob\nlast line` and @carol", []string{"carol"}},
		{"code span ends at a paragraph", "`open\n\n@bob", []string{"bob"}},
		{"indented code block", "intro\n\n    @bob\n\n    @eve\nafter @carol", []string{"carol"}},
		{"indented code at the start", "    @bob\n@carol", []string{"carol"}},
		{"tab-indented code", "\t@bob\n\n@carol", []string{"carol"}},
		{"indented continuation of a paragraph", "please ask\n    @bob", []string{"bob"}},
		{"fence interrupts a paragraph", "text @a\n```\n@bob\n```", []string{"a"}},
		{"double backtick span across lines", "``a `\n@bob`` @carol", []string{"carol"}},
	} {
		if got := Mentions(c.text); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q -> %v, want %v", c.name, c.text, got, c.want)
		}
	}
	if got := Mentions(many); len(got) != maxMentions {
		t.Errorf("%d mentions, want at most %d", len(got), maxMentions)
	}
}

// TestPushKeys: different pushes never share a dedupe key, even when they
// move a ref to the same commit (A→B, B→C, C→B) or repeat a transition (a
// force push back and forth); a redelivery of the same bytes does.
func TestPushKeys(t *testing.T) {
	push := func(before, after, at string) provider.Delivery {
		return edit(t, fixture(t, "push"), func(b map[string]any) {
			b["before"], b["after"] = before, after
			b["compare_url"] = "http://git.orglane.localhost/orglane/app/compare/" + before + "..." + after
			b["head_commit"].(map[string]any)["timestamp"] = at
		})
	}
	a, b, c := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	keys := map[string]string{}
	for name, d := range map[string]provider.Delivery{
		"A→B":       push(a, b, "2026-11-05T05:00:00Z"),
		"B→C":       push(b, c, "2026-11-05T05:01:00Z"),
		"C→B":       push(c, b, "2026-11-05T05:02:00Z"),
		"A→B again": push(a, b, "2026-11-05T05:03:00Z"),
	} {
		key := normalize(t, d)[0].DedupeKey
		if other, ok := keys[key]; ok {
			t.Fatalf("%s and %s share the key %s", name, other, key)
		}
		keys[key] = name
	}
	first := push(a, b, "2026-11-05T05:00:00Z")
	if normalize(t, first)[0].DedupeKey != normalize(t, push(a, b, "2026-11-05T05:00:00Z"))[0].DedupeKey {
		t.Fatal("a redelivery of the same bytes got another key")
	}
}

// adversarialMarkdown is a body of unmatched backtick runs of lengths 1 to
// n, each followed by " x ": every run needs a search for a closing run.
func adversarialMarkdown(n int) string {
	var b strings.Builder
	b.WriteString("prefix ")
	for i := 1; i <= n; i++ {
		b.WriteString(strings.Repeat("`", i))
		b.WriteString(" x ")
	}
	b.WriteString("@bob")
	return b.String()
}

// TestMentionsScaleLinearly bounds the time of the largest adversarial
// body below the webhook size limit (4.5 MB). Matching code spans by
// rescanning took seconds; the bound is generous for slow runners and the
// race detector.
func TestMentionsScaleLinearly(t *testing.T) {
	text := adversarialMarkdown(3000)
	if len(text) > 5<<20 {
		t.Fatalf("the body is %d bytes, over the webhook limit", len(text))
	}
	bound := time.Second
	if raceEnabled {
		bound *= 10
	}
	start := time.Now()
	got := Mentions(text)
	if d := time.Since(start); d > bound {
		t.Fatalf("Mentions of %d bytes took %s, over %s", len(text), d, bound)
	}
	if !slices.Equal(got, []string{"bob"}) {
		t.Fatalf("mentions = %v", got)
	}
}

func BenchmarkMentionsAdversarial(b *testing.B) {
	for _, n := range []int{1000, 2000, 3000} {
		text := adversarialMarkdown(n)
		b.Run(strconv.Itoa(len(text)>>10)+"KiB", func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			for b.Loop() {
				Mentions(text)
			}
		})
	}
}
