package apps

import (
	"net/url"
	"testing"

	"github.com/orglane/orglane/providers/provider"
)

func TestConnectStepOriginBoundary(t *testing.T) {
	valid := func(raw string, method string) *provider.ConnectStep {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		step := &provider.ConnectStep{Name: "install_app", Method: method, URL: u}
		return step
	}
	if !validConnectStep(valid("https://github.com/apps/orglane-ci/installations/new?state=ols_test", "GET")) {
		t.Fatal("valid installation redirect rejected")
	}
	for _, raw := range []string{"http://github.com/apps/orglane-ci/installations/new", "https://github.com.evil.test/apps/orglane-ci/installations/new", "https://github.com:443/apps/orglane-ci/installations/new", "https://evil@github.com/apps/orglane-ci/installations/new", "https://github.com/apps/orglane-ci/installations/new#secret", "https://api.github.com/apps/orglane-ci/installations/new"} {
		if validConnectStep(valid(raw, "GET")) {
			t.Errorf("accepted unsafe provider redirect %s", raw)
		}
	}
	if validConnectStep(nil) || validConnectStep(valid("https://github.com/apps/orglane-ci/installations/new", "POST")) {
		t.Fatal("invalid step accepted")
	}
}
