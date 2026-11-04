package apps

import (
	"net/url"
	"strings"
	"testing"

	"github.com/orglane/orglane/core/internal/onboarding"
)

// targetCorpus is shared with onboarding action URL validation: every
// target accepted here is a local path that onboarding.ValidateActionURL
// accepts too.
var targetCorpus = []struct {
	to string
	ok bool
}{
	{"/", true},
	{"/acme/robot/pulls/3", true},
	{"/acme/robot/pulls?state=open#top", true},
	{"/a%20b", true},
	{"", false},
	{"acme/robot", false},
	{"//evil.example/x", false},
	{"/\\evil.example", false},
	{"\\\\evil.example", false},
	{"https://evil.example/", false},
	{"javascript:alert(1)", false},
	{"/a b", false},
	{"/a\tb", false},
	{"/a\nb", false},
	{"/a\rb", false},
	{"/a\x00b", false},
	{"/a\x7fb", false},
	{"/" + strings.Repeat("a", 2047), true},
	{"/" + strings.Repeat("a", 2048), false},
}

func TestValidateTarget(t *testing.T) {
	for _, c := range targetCorpus {
		err := ValidateTarget(c.to)
		if (err == nil) != c.ok {
			t.Errorf("ValidateTarget(%q) = %v, want ok=%v", c.to, err, c.ok)
		}
		if err == nil && onboarding.ValidateActionURL(c.to) != nil {
			t.Errorf("%q is a valid target but not a valid onboarding action", c.to)
		}
	}
}

func TestCheckOpenURL(t *testing.T) {
	base, _ := url.Parse("https://git.acme.com")
	for raw, ok := range map[string]bool{
		"https://git.acme.com/user/oauth2/orglane?redirect_to=%2Fx": true,
		"https://GIT.acme.com/x":                                    true,
		"http://git.acme.com/x":                                     false,
		"https://evil.example/x":                                    false,
		"https://user@git.acme.com/x":                               false,
		"https://git.acme.com.evil/x":                               false,
	} {
		u, _ := url.Parse(raw)
		if err := CheckOpenURL(base, u); (err == nil) != ok {
			t.Errorf("CheckOpenURL(%s) = %v, want ok=%v", raw, err, ok)
		}
	}
}
