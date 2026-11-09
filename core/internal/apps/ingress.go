package apps

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

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

// Defaults of Config.RouteSettle and Config.RouteWait. The settle time is
// Traefik's default providersThrottleDuration, which the bundle keeps; the
// wait covers the bundle's poll interval (10s) plus the settle time.
const (
	defaultRouteSettle = 2 * time.Second
	defaultRouteWait   = 15 * time.Second
)

// errRouteUnpublished fails a setup attempt whose instance's route Traefik
// has not been served yet. It is retryable, like any setup failure.
var errRouteUnpublished = errors.New("apps: the instance's route is not published to Traefik yet")

// servedRoutes is the record of the app routes in the configurations the
// internal handler served Traefik (docs/specs/providers.md "Route
// publication"). It lives in the process that serves them: a restart
// forgets it until Traefik's next poll.
type servedRoutes struct {
	mu sync.Mutex
	// routes are the routes of the last configuration served, by instance
	// key, each with the time of the first answer since which it has been
	// served unchanged.
	routes map[string]servedRoute
	// changed is closed (and replaced) whenever a configuration is served.
	changed chan struct{}
}

type servedRoute struct {
	route ingress.AppRoute
	since time.Time
}

func newServedRoutes() *servedRoutes {
	return &servedRoutes{routes: map[string]servedRoute{}, changed: make(chan struct{})}
}

// record replaces the routes with those of a configuration served at now.
func (r *servedRoutes) record(routes []ingress.AppRoute, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]servedRoute, len(routes))
	for _, route := range routes {
		since := now
		if prev, ok := r.routes[route.Key]; ok && prev.route == route {
			since = prev.since
		}
		next[route.Key] = servedRoute{route: route, since: since}
	}
	r.routes = next
	close(r.changed)
	r.changed = make(chan struct{})
}

// lookup returns since when want has been served unchanged (zero: it is
// not in the last configuration served) and a channel closed on the next
// serve.
func (r *servedRoutes) lookup(want ingress.AppRoute) (time.Time, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if got, ok := r.routes[want.Key]; ok && got.route == want {
		return got.since, r.changed
	}
	return time.Time{}, r.changed
}

// RoutesServed records the app routes of a configuration the internal
// handler served Traefik completely (ingress.ServedFunc). It does nothing
// unless Config.AwaitRoutes.
func (s *Service) RoutesServed(routes []ingress.AppRoute) {
	if s.served != nil {
		s.served.record(routes, time.Now())
	}
}

// awaitRoute waits until the route of an installing instance is published
// (servedRoutes.await). An instance in another state, one without a route,
// or a process that does not serve Traefik (no Config.AwaitRoutes) does not
// wait.
func (s *Service) awaitRoute(ctx context.Context, inst Instance) error {
	if s.served == nil || inst.State != StateInstalling {
		return nil
	}
	want, ok := s.instanceRoute(inst.AppInstance)
	if !ok {
		return nil
	}
	err := s.served.await(ctx, want, s.cfg.RouteSettle, s.cfg.RouteWait)
	if errors.Is(err, errRouteUnpublished) {
		s.cfg.Logger.WarnContext(ctx, "the instance's route is not published to Traefik yet",
			"app_instance", inst.ID, "code", "route_unpublished", "host", want.Host)
	}
	return err
}

// await waits until want is published: served to Traefik unchanged for at
// least settle. It returns errRouteUnpublished when that takes longer than
// wait.
func (r *servedRoutes) await(ctx context.Context, want ingress.AppRoute, settle, wait time.Duration) error {
	timeout := time.NewTimer(wait)
	defer timeout.Stop()
	for {
		since, changed := r.lookup(want)
		var settleTimer *time.Timer
		var settled <-chan time.Time
		if !since.IsZero() {
			left := time.Until(since.Add(settle))
			if left <= 0 {
				return nil
			}
			settleTimer = time.NewTimer(left)
			settled = settleTimer.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return errRouteUnpublished
		case <-changed:
		case <-settled:
		}
		if settleTimer != nil {
			settleTimer.Stop()
		}
	}
}
