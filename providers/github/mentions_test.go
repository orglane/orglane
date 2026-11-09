package github

import (
	"strings"
	"testing"
	"time"
)

func TestMentions(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"@octocat", []string{"octocat"}},
		{"hi @octocat, @hubot and @OctoCat.", []string{"octocat", "hubot"}},
		{"(@a-b) [@c] @d!", []string{"a-b", "c", "d"}},
		{"@bob-", []string{"bob"}},
		{"mail me@example.com or a.@b", nil},
		{`escaped \@bob`, nil},
		{"@org/team and @org", []string{"org"}},
		{"@bob_smith @bøb @-bob @bob--x", []string{"bob--x"}},
		{"@" + strings.Repeat("a", 39) + " @" + strings.Repeat("b", 40), []string{strings.Repeat("a", 39)}},
		{"`@code` @text ``@a ` @b`` @c", []string{"text", "c"}},
		{"unclosed ` @a", []string{"a"}},
		{"span across\nlines `@a\n@b` @c", []string{"c"}},
		{"para `@a\n\n@b` x", []string{"b"}},
		{"```\n@a\n```\n@b", []string{"b"}},
		{"~~~~go\n@a\n~~~\n@b\n~~~~\n@c", []string{"c"}},
		{"``` not `a` fence @a", []string{"a"}},
		{"```\n@a never closed", nil},
		{"    @indented\n\n@b", []string{"b"}},
		{"text\n    @continuation", []string{"continuation"}},
		{"\t@tab\n\n@b", []string{"b"}},
		{"@a\r\n@b\r\n", []string{"a", "b"}},
		{"@a\r@b", []string{"a", "b"}},
		// Fenced code in block quotes and list items, including a longer
		// closing fence and a list marker that is content inside the fence.
		{"> ~~~\n> @a\n> ~~~", nil},
		{"> ```\n> @a\n> ````", nil},
		{"> ```\n> @a\n> ```\n\n@b", []string{"b"}},
		{"> ```\n> @a\n\n@b", []string{"b"}},
		{"    <pre>\n\n@a", []string{"a"}},
		{"- ```\n  @a\n  ```\n- @b", []string{"b"}},
		{"1. ```\n   @a\n   ```\n2. @b", []string{"b"}},
		{"```\n- ```\n@a\n```\n@b", []string{"b"}},
		// Headings and list items are separate blocks: a code span cannot
		// pair a backtick across their boundary.
		{"# `heading\n@a`", []string{"a"}},
		{"- `one\n- @a`", []string{"a"}},
		{"Setext `heading\n===\n@a`", []string{"a"}},
		{"`heading\n***\n@a`", []string{"a"}},
		// Link destinations, titles and labels are not rendered.
		{"[x](/foo`) @a `", []string{"a"}},
		{"[x](url \"`title\") @a `", []string{"a"}},
		{"[x][@bad] @a", []string{"a"}},
		{"[@bad](https://example.com/@bad2) @a", []string{"a"}},
		{"[x] some @a ](https://example.com)", []string{"a"}},
		{"![@bad](https://example.com/@bad2) @a", []string{"a"}},
		{"[@bad]: https://example.com/@bad2\n@a", []string{"a"}},
		{"text\n[x]: @a", []string{"a"}},
		// Raw HTML tags, comments, autolinks and raw-text blocks.
		{"<a href=\"@bad\">@a</a>", []string{"a"}},
		{"<!-- @bad --> @a", nil},
		{"text <!-- @bad --> @a", []string{"a"}},
		{"<!-- @bad\n@c", nil},
		{"<!--\n@a\n\n@b\n-->\n@c", []string{"c"}},
		{"<https://example.com/@bad> @a", []string{"a"}},
		{"<pre>\n@a\n</pre>\n@b", []string{"b"}},
		{"> <pre>\n> @a\n> </pre>\n\n@b", []string{"b"}},
		{"<pre>@a</pre> @b", nil},
		{"<pre>@a</pre>\n@b", []string{"b"}},
		{"<pre>\n</prefix>\n@a\n</pre>\n@b", []string{"b"}},
		{"<textarea>\n@a\n", nil},
	} {
		got := Mentions(c.text)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("Mentions(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

// TestMentionsLinear runs adversarial bodies of the largest size that is
// scanned; a quadratic scanner takes minutes on them.
func TestMentionsLinear(t *testing.T) {
	n := maxMentionText
	for name, body := range map[string]string{
		"backtick runs of rising length": func() string {
			var b strings.Builder
			for i := 1; b.Len() < n; i++ {
				b.WriteString(strings.Repeat("`", i%700+1) + "@a ")
			}
			return b.String()
		}(),
		"single backticks":      strings.Repeat("` @a ", n/5),
		"at signs":              strings.Repeat("@", n),
		"long words":            strings.Repeat("@"+strings.Repeat("a", 5000)+" ", n/5000),
		"fences":                strings.Repeat("```\n", n/4),
		"one paragraph of runs": strings.Repeat("`` ` ``` ", n/9),
		"link destinations":     strings.Repeat("](", n/2),
		"reference labels":      strings.Repeat("][", n/2),
		"unclosed tags":         strings.Repeat("<a ", n/3),
		"unclosed comments":     strings.Repeat("<!-- ", n/5),
		"escaped brackets":      strings.Repeat("\\", n/2) + strings.Repeat("]", n/2),
		"over the limit":        strings.Repeat("x ", 4*n),
	} {
		start := time.Now()
		Mentions(body)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %v", name, d)
		}
	}
}
