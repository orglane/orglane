//go:build integration

package apps_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/ingress"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// TestIngressRoutes: managed instances get their package's route once the
// agent applied their generation, keep it while active or degraded, point
// it at the core's maintenance answer while upgrading, and lose it when
// they fail or are removed. Connected instances never get routes.
func TestIngressRoutes(t *testing.T) {
	h := newHarness(t)
	pkgs, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	h.instance(nil) // connected

	type row struct {
		key, state        string
		desired, observed int64
		version           *string
	}
	running := agenttest.Version
	rows := []row{
		{"pending", apps.StateInstalling, 1, 0, nil},        // the agent has not applied it yet
		{"applied", apps.StateInstalling, 1, 1, nil},        // applied: Setup can use the public name
		{"reinstall", apps.StateInstalling, 3, 2, &running}, // a new generation is not applied yet
		{"active", apps.StateActive, 2, 2, &running},
		{"degraded", apps.StateDegraded, 2, 2, &running},
		{"upgrading", apps.StateUpgrading, 3, 2, &running},
		{"failed", apps.StateFailed, 1, 1, &running},
		{"removed", apps.StateRemoved, 2, 2, &running},
	}
	for _, r := range rows {
		inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
			Provider: "testmanaged", Key: r.key, BaseURL: "https://" + r.key + ".acme.test",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.pool.Exec(h.ctx, `UPDATE app_instances SET package_id = 'agenttest', package_version = $2,
			desired_generation = $3, observed_generation = $4, state = $5 WHERE id = $1`,
			inst.ID, r.version, r.desired, r.observed, r.state); err != nil {
			t.Fatal(err)
		}
	}

	got, err := h.svc.IngressRoutes(h.ctx, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b ingress.AppRoute) int { return strings.Compare(a.Key, b.Key) })
	want := []ingress.AppRoute{
		{Key: "active", Route: "web", Host: "active.acme.test", Upstream: "http://active-web:8080"},
		{Key: "applied", Route: "web", Host: "applied.acme.test", Upstream: "http://applied-web:8080"},
		{Key: "degraded", Route: "web", Host: "degraded.acme.test", Upstream: "http://degraded-web:8080"},
		{Key: "upgrading", Route: "web", Host: "upgrading.acme.test", Maintenance: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("routes =\n%+v\nwant\n%+v", got, want)
	}

	// Without the package embedded there is nothing to route to.
	empty, err := packages.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := h.svc.IngressRoutes(h.ctx, empty); err != nil || len(got) != 0 {
		t.Fatalf("no package: %v, %v", got, err)
	}
}
