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
		"over the limit":        strings.Repeat("x ", 4*n),
	} {
		start := time.Now()
		Mentions(body)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %v", name, d)
		}
	}
}
