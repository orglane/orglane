package apps

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"

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
	// routeCheckInterval is how often awaitRoute reads the served routes.
	routeCheckInterval = 250 * time.Millisecond
	// setupFinishReserve is the part of a setup attempt's deadline kept
	// for recording its outcome (finishSetup): the route wait never eats
	// into it.
	setupFinishReserve = 5 * time.Second
)

// errRouteUnpublished fails a setup attempt whose instance's route Traefik
// has not been served yet. It is retryable, like any setup failure.
var errRouteUnpublished = errors.New("apps: the instance's route is not published to Traefik yet")

// PublishRoutes observes a configuration the internal handler serves
// Traefik (ingress.Publisher) in the database, where the setup job reads
// it from whichever core process runs it (docs/specs/providers.md "Route
// publication"). It fails closed. Before the answer is written it deletes
// every recorded route the configuration does not contain identically; if
// that fails, the handler answers 503 and Traefik keeps the configuration
// the remaining records describe. Once the answer was flushed, served
// records its new routes, unless a later serve has begun meanwhile; a
// failure there only leaves them unpublished until the next poll. Without
// Config.AwaitRoutes it records nothing.
func (s *Service) PublishRoutes(ctx context.Context, routes []ingress.AppRoute) (served func(context.Context), err error) {
	if !s.cfg.AwaitRoutes {
		return nil, nil
	}
	// Empty, never nil: a NULL array would match nothing.
	keys, hosts := make([]string, 0, len(routes)), make([]string, 0, len(routes))
	upstreams, maintenance := make([]string, 0, len(routes)), make([]bool, 0, len(routes))
	for _, r := range routes {
		keys, hosts = append(keys, r.Key), append(hosts, r.Host)
		upstreams, maintenance = append(upstreams, r.Upstream), append(maintenance, r.Maintenance)
	}
	var seq int64
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		// The sequence first: its row lock orders this serve after any
		// recording still in flight, whose rows the delete then sees.
		var err error
		if seq, err = q.BeginRouteServe(ctx); err != nil {
			return err
		}
		return q.DropUnservedRoutes(ctx, store.DropUnservedRoutesParams{
			Keys: keys, Hosts: hosts, Upstreams: upstreams, Maintenance: maintenance,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("apps: record the routes to serve: %w", err)
	}
	return func(ctx context.Context) {
		if err := store.New(s.pool).RecordServedRoutes(ctx, store.RecordServedRoutesParams{
			Keys: keys, Hosts: hosts, Upstreams: upstreams, Maintenance: maintenance, Seq: seq,
		}); err != nil {
			s.cfg.Logger.ErrorContext(ctx, "record the routes served to Traefik", "error", err)
		}
	}, nil
}

// routePublished reports whether want has been served to Traefik unchanged
// for at least Config.RouteSettle.
func (s *Service) routePublished(ctx context.Context, want ingress.AppRoute) (bool, error) {
	got, err := store.New(s.pool).GetServedRoute(ctx, store.GetServedRouteParams{
		InstanceKey: want.Key, SettleSeconds: s.cfg.RouteSettle.Seconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("apps: read the served route: %w", err)
	}
	return got.Settled && got.Host == want.Host && got.Upstream == want.Upstream && got.Maintenance == want.Maintenance, nil
}

// awaitRoute waits until the route of an installing instance is published.
// It returns errRouteUnpublished after Config.RouteWait, or earlier when
// the attempt's deadline (less setupFinishReserve, kept for recording the
// outcome) comes first. An instance in another state, one without a route,
// or a deployment without Traefik (no Config.AwaitRoutes) does not wait.
func (s *Service) awaitRoute(ctx context.Context, inst Instance) error {
	if !s.cfg.AwaitRoutes || inst.State != StateInstalling {
		return nil
	}
	want, ok := s.instanceRoute(inst.AppInstance)
	if !ok {
		return nil
	}
	until := time.Now().Add(s.cfg.RouteWait)
	if d, ok := ctx.Deadline(); ok && d.Add(-setupFinishReserve).Before(until) {
		until = d.Add(-setupFinishReserve)
	}
	for {
		published, err := s.routePublished(ctx, want)
		if err != nil || published {
			return err
		}
		left := time.Until(until)
		if left <= 0 {
			s.cfg.Logger.WarnContext(ctx, "the instance's route is not published to Traefik yet",
				"app_instance", inst.ID, "code", "route_unpublished", "host", want.Host)
			return errRouteUnpublished
		}
		t := time.NewTimer(min(left, routeCheckInterval))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
