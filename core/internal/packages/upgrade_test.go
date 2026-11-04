package packages

import (
	"errors"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// pathRegistry builds a registry of minimal package versions with the
// given min_from ("" for none).
func pathRegistry(t *testing.T, versions map[string]string) *Registry {
	t.Helper()
	fsys := fstest.MapFS{}
	for v, minFrom := range versions {
		manifest := "format: 1\nid: demo\nversion: " + v + "\nprovider: demo\n" +
			"images: {app: docker.io/library/busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec}\n" +
			"services: [app]\nhealth: {start_timeout: 1m}\n"
		if minFrom != "" {
			manifest += "upgrade: {min_from: " + minFrom + "}\n"
		}
		fsys[v+"/manifest.yaml"] = &fstest.MapFile{Data: []byte(manifest)}
		fsys[v+"/compose.yaml.tmpl"] = &fstest.MapFile{Data: []byte("services: {}\n")}
	}
	r, err := NewRegistry(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUpgradePath(t *testing.T) {
	r := pathRegistry(t, map[string]string{
		"12.0.0-1": "11.5.0-1",
		"13.0.0-1": "12.0.0-1",
		"13.1.0-1": "12.0.0-1",
		"14.0.0-1": "13.1.0-1",
		"15.0.0-1": "14.0.0-1",
	})
	for _, c := range []struct {
		from, to string
		want     []string
	}{
		{"12.0.0-1", "13.0.0-1", []string{"13.0.0-1"}},
		{"12.0.0-1", "15.0.0-1", []string{"13.1.0-1", "14.0.0-1", "15.0.0-1"}},
		// A running version that is not embedded.
		{"12.5.0-3", "14.0.0-1", []string{"13.1.0-1", "14.0.0-1"}},
		{"13.0.0-1", "13.1.0-1", []string{"13.1.0-1"}},
		{"14.0.0-1", "15.0.0-1", []string{"15.0.0-1"}},
	} {
		got, err := r.UpgradePath("demo", c.from, c.to)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("UpgradePath(%s, %s) = %v, %v; want %v", c.from, c.to, got, err, c.want)
		}
	}
	if got := r.RequiredStop("demo", "12.0.0-1", "15.0.0-1"); got != "13.1.0-1" {
		t.Errorf("RequiredStop = %q", got)
	}
	if got := r.RequiredStop("demo", "14.0.0-1", "15.0.0-1"); got != "" {
		t.Errorf("RequiredStop of a direct upgrade = %q", got)
	}

	var ue *UpgradeError
	if _, err := r.UpgradePath("demo", "11.0.0-1", "13.0.0-1"); !errors.As(err, &ue) || ue.MinFrom != "12.0.0-1" || ue.Downgrade {
		t.Errorf("no path from 11: %v", err)
	}
	if _, err := r.UpgradePath("demo", "14.0.0-1", "13.0.0-1"); !errors.As(err, &ue) || !ue.Downgrade {
		t.Errorf("downgrade: %v", err)
	}
	if _, err := r.UpgradePath("demo", "13.0.0-1", "13.0.0-1"); !errors.As(err, &ue) || !ue.Downgrade {
		t.Errorf("same version: %v", err)
	}
	if _, err := r.UpgradePath("demo", "12.0.0-1", "16.0.0-1"); err == nil || errors.As(err, &ue) {
		t.Errorf("target not embedded: %v", err)
	}
	if latest, ok := r.Latest("demo"); !ok || latest.Manifest.Version != "15.0.0-1" {
		t.Errorf("Latest = %v", latest)
	}
	if _, ok := r.Latest("other"); ok {
		t.Error("Latest of an unknown package")
	}
}

func TestCheckUpgrade(t *testing.T) {
	r := testRegistry(t)
	get := func(v string) *Package {
		p, ok := r.Get(agentapi.PackageRef{ID: "agenttest", Version: v})
		if !ok {
			t.Fatalf("agenttest %s is not embedded", v)
		}
		return p
	}
	if err := get("2.0.0-1").CheckUpgrade("1.0.0-1"); err != nil {
		t.Errorf("1 -> 2: %v", err)
	}
	var ue *UpgradeError
	if err := get("3.0.0-1").CheckUpgrade("1.0.0-1"); !errors.As(err, &ue) || ue.MinFrom != "2.0.0-1" {
		t.Errorf("1 -> 3 skips the required stop: %v", err)
	}
	if err := get("1.0.0-1").CheckUpgrade("2.0.0-1"); !errors.As(err, &ue) || !ue.Downgrade {
		t.Errorf("2 -> 1: %v", err)
	}
	if got := r.RequiredStop("agenttest", "1.0.0-1", "3.0.0-1"); got != "2.0.0-1" {
		t.Errorf("RequiredStop(1, 3) = %q", got)
	}
}
