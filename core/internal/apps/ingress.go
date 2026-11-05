package apps

import (
	"context"
	"fmt"
	"net/url"

	"github.com/orglane/orglane/core/internal/ingress"
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
	if u, err := url.Parse(*row.BaseUrl); err != nil || u.Hostname() == "" {
		return ingress.AppRoute{}, false
	}
	pkg, ok := s.InstancePackage(row)
	upstream := s.Upstream(row)
	if !ok || upstream == nil {
		return ingress.AppRoute{}, false
	}
	// The same hosts the agent's spec carries (ServiceHosts, agent.go).
	route := pkg.Manifest.Routes[0].Name
	r := ingress.AppRoute{Key: row.Key, Route: route, Host: s.ServiceHosts(row, pkg)[route]}
	if row.State == StateUpgrading {
		r.Maintenance = true
		return r, true
	}
	r.Upstream = upstream.String()
	return r, true
}
