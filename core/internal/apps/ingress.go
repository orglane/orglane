package apps

import (
	"context"
	"fmt"
	"net"
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
// Traefik: the first route of the instance's package (InstancePackage), on
// the host of the instance's base URL, to its Upstream, or to the core's
// maintenance answer while the instance is upgrading.
//
// In M2 a package has one route; further routes would need their own hosts
// and are left out.
func (s *Service) IngressRoutes(ctx context.Context) ([]ingress.AppRoute, error) {
	rows, err := store.New(s.pool).ListAppInstancesInState(ctx, routedStates)
	if err != nil {
		return nil, fmt.Errorf("apps: list routed instances: %w", err)
	}
	var out []ingress.AppRoute
	for _, row := range rows {
		if r, ok := s.instanceRoute(row); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// instanceRoute returns the route of one instance, ok = false if it has
// none (yet).
func (s *Service) instanceRoute(row store.AppInstance) (ingress.AppRoute, bool) {
	if row.Management != "managed" || row.BaseUrl == nil {
		return ingress.AppRoute{}, false
	}
	if row.State == StateInstalling && (row.DesiredGeneration == 0 || row.ObservedGeneration < row.DesiredGeneration) {
		return ingress.AppRoute{}, false
	}
	u, err := url.Parse(*row.BaseUrl)
	if err != nil || u.Hostname() == "" {
		return ingress.AppRoute{}, false
	}
	pkg, ok := s.InstancePackage(row)
	upstream := s.Upstream(row)
	if !ok || upstream == nil {
		return ingress.AppRoute{}, false
	}
	r := ingress.AppRoute{Key: row.Key, Route: pkg.Manifest.Routes[0].Name, Host: u.Hostname()}
	if row.State == StateUpgrading {
		r.Maintenance = true
		return r, true
	}
	r.Upstream = upstream.String()
	return r, true
}

// The helpers below have the names and behavior of #75's (core/internal/apps
// agent.go there); whichever merges second drops its copy.

// Upstream is a managed instance's first route on the edge network, the
// alias Traefik's routes use (agent.md "Networking"), from the package
// InstancePackage returns; nil without one.
func (s *Service) Upstream(row store.AppInstance) *url.URL {
	pkg, ok := s.InstancePackage(row)
	if !ok || len(pkg.Manifest.Routes) == 0 {
		return nil
	}
	r := pkg.Manifest.Routes[0]
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(packages.ServiceAlias(row.Key, r.Service, false), strconv.Itoa(r.Port))}
}

// InstancePackage is the package version whose routes and hosts apply to
// a managed instance: the running version, else (not reported yet, or no
// longer embedded) the newest embedded one.
func (s *Service) InstancePackage(row store.AppInstance) (*packages.Package, bool) {
	if row.PackageID == nil || s.cfg.Packages == nil {
		return nil, false
	}
	if p, ok := s.cfg.Packages.Get(agentapi.PackageRef{ID: *row.PackageID, Version: derefString(row.PackageVersion)}); ok {
		return p, true
	}
	return s.latestPackage(*row.PackageID)
}

// latestPackage returns the newest embedded version of a package.
func (s *Service) latestPackage(id string) (*packages.Package, bool) {
	if s.cfg.Packages == nil {
		return nil, false
	}
	var newest *packages.Package
	for _, ref := range s.cfg.Packages.List() { // sorted by ID, then version
		if ref.ID == id {
			newest, _ = s.cfg.Packages.Get(ref)
		}
	}
	return newest, newest != nil
}
