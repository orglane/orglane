//go:build !agenttest

package packages

import "testing"

func TestEmbeddedRegistryHasNoTestPackage(t *testing.T) {
	r, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range r.List() {
		if ref.ID == "agenttest" {
			t.Fatal("the agenttest package is embedded without the agenttest build tag")
		}
	}
}
