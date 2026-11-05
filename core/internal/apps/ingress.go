package apps

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/ingress"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/store"
)

// routedStates are the states in which a managed instance has routes
// (docs/specs/providers.md "Ingress routes"). An installing instance has
// them only once the agent has applied its current generation, so Setup
// can reach it by its public name.
var routedStates = []string{StateInstalling, StateActive, StateDegraded, StateUpgrading}

// IngressRoutes returns the public routes of the managed instances for
// Traefik: one per route of the instance's package, to the service's alias
// on the edge network, or to the core's maintenance answer while the
// instance is upgrading.
//
// In M2 a package has one route, and its host is the host of the
// instance's base URL; further routes of a package would need their own
// hosts and are left out.
func (s *Service) IngressRoutes(ctx context.Context, pkgs *packages.Registry) ([]ingress.AppRoute, error) {
	rows, err := store.New(s.pool).ListAppInstancesInState(ctx, routedStates)
	if err != nil {
		return nil, fmt.Errorf("apps: list routed instances: %w", err)
	}
	var out []ingress.AppRoute
	for _, row := range rows {
		r, ok := instanceRoute(row, pkgs)
		if !ok {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// instanceRoute returns the route of one instance, ok = false if it has
// none (yet).
func instanceRoute(row store.AppInstance, pkgs *packages.Registry) (ingress.AppRoute, bool) {
	if row.Management != "managed" || row.PackageID == nil || row.BaseUrl == nil {
		return ingress.AppRoute{}, false
	}
	if row.State == StateInstalling && (row.DesiredGeneration == 0 || row.ObservedGeneration < row.DesiredGeneration) {
		return ingress.AppRoute{}, false
	}
	u, err := url.Parse(*row.BaseUrl)
	if err != nil || u.Hostname() == "" {
		return ingress.AppRoute{}, false
	}
	m, ok := routeManifest(pkgs, *row.PackageID, row.PackageVersion)
	if !ok || len(m.Routes) == 0 {
		return ingress.AppRoute{}, false
	}
	route := m.Routes[0]
	r := ingress.AppRoute{Key: row.Key, Route: route.Name, Host: u.Hostname()}
	if row.State == StateUpgrading {
		r.Maintenance = true
		return r, true
	}
	r.Upstream = "http://" + packages.ServiceAlias(row.Key, route.Service, false) + ":" + strconv.Itoa(route.Port)
	return r, true
}

// routeManifest returns the manifest whose routes apply: the running
// version's, else (not reported yet, or no longer embedded) the newest
// embedded version's.
func routeManifest(pkgs *packages.Registry, id string, running *string) (*packages.Manifest, bool) {
	if pkgs == nil {
		return nil, false
	}
	if running != nil {
		if p, ok := pkgs.Get(agentapi.PackageRef{ID: id, Version: *running}); ok {
			return p.Manifest, true
		}
	}
	var newest *packages.Package
	for _, ref := range pkgs.List() { // sorted by ID and version
		if ref.ID == id {
			newest, _ = pkgs.Get(ref)
		}
	}
	if newest == nil {
		return nil, false
	}
	return newest.Manifest, true
}
