package api

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"

	specpkg "github.com/orglane/orglane/core/api"
	"github.com/orglane/orglane/core/internal/apps"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
)

func TestGitHubAuthenticationContract(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(specpkg.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path, method string
		want         []map[string][]string
	}{
		{"/api/v1/apps/github/connect", "post", []map[string][]string{{"sessionCookie": {}}}},
		{"/api/v1/apps/github/connect/complete", "post", []map[string][]string{{"sessionCookie": {}}}},
		{"/api/v1/apps/instances/{instanceId}/credentials/{credentialName}", "put", []map[string][]string{{"sessionCookie": {}}, {"bearerAuth": {}}}},
	} {
		var op struct {
			Security []map[string][]string `yaml:"security"`
		}
		node := doc.Paths[tc.path][tc.method]
		if err := node.Decode(&op); err != nil {
			t.Fatal(err)
		}
		if got := op.Security; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s authentication = %v, want %v", tc.path, got, tc.want)
		}
	}
}
func TestConnectErrorPreservesInternalFailures(t *testing.T) {
	internal := errors.New("database failure with internal diagnostics")
	if got := connectError(fmt.Errorf("connect persistence: %w", internal)); !errors.Is(got, internal) {
		t.Fatalf("internal error became a client error: %v", got)
	}
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&githubprovider.APIError{Status: 503}, 400, "github_unavailable"},
		{&provider.Error{Code: "invalid_private_key"}, 400, "invalid_private_key"},
		{apps.ErrKeyReserved, 409, "key_reserved"},
	} {
		var got *apiError
		if !errors.As(connectError(tc.err), &got) || got.status != tc.status || got.code != tc.code {
			t.Errorf("incorrect safe mapping for %T: %+v", tc.err, got)
		}
	}
}
