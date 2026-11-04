//go:build agenttest

package packages

import (
	"slices"
	"testing"

	"github.com/orglane/orglane/core/internal/agentapi"
)

func TestEmbeddedRegistryHasTestPackage(t *testing.T) {
	r, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.List(), agentapi.PackageRef{ID: "agenttest", Version: "1.0.0-1"}) {
		t.Fatalf("packages = %v, want agenttest with the agenttest build tag", r.List())
	}
}
