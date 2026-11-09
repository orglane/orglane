// Package ingress builds the Traefik dynamic configuration served by the
// core (ADR 0008). Traefik has no Docker socket: it polls the core's internal
// listener through its HTTP provider and routes requests from what it gets.
//
// The configuration has the workspace router and one router per route of
// every managed app instance that is reachable (docs/specs/providers.md
// "Ingress routes"). It is rebuilt from the database on every poll.
//
// The static side of the contract (entry point and certificate resolver
// names) lives in deploy/compose/compose.yaml.
package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/orglane/orglane/core/internal/config"
)

// Traefik entry points defined by the Compose bundle's static configuration.
const (
	EntryPointHTTP  = "web"       // :80
	EntryPointHTTPS = "websecure" // :443
)

// ConfigPath is where the internal listener serves the dynamic configuration
// (Traefik's providers.http.endpoint).
const ConfigPath = "/traefik/config"

// Names of the routers, services and middlewares the core generates. They are
// stable, so operators can refer to them from their own Traefik configuration.
const (
	RouterWorkspace         = "orglane-workspace"
	RouterWorkspaceRedirect = "orglane-workspace-redirect"
	ServiceCore             = "orglane-core"
	MiddlewareHeaders       = "orglane-security-headers"
	MiddlewareHTTPSRedirect = "orglane-https-redirect"
	MiddlewareMaintenance   = "orglane-maintenance"
)

// HeaderMaintenance marks requests that Traefik forwards to the core
// instead of an app instance that is being upgraded. The core answers them
// with 503 (Maintenance).
const HeaderMaintenance = "X-Orglane-Maintenance"

// MaintenanceRetryAfter is the Retry-After of maintenance answers, in
// seconds.
const MaintenanceRetryAfter = "30"

// AppRoute is a public route of a managed app instance.
type AppRoute struct {
	// Key is the instance key; Route the manifest's route name.
	Key, Route string
	// Host is the public hostname.
	Host string
	// Upstream is the service on the edge network, e.g.
	// http://git-forgejo:3000.
	Upstream string
	// Maintenance routes the host to the core, which answers 503 while the
	// instance is being upgraded.
	Maintenance bool
}

// AppRouter returns the name of an app route's router (and its service):
// orglane-app-<key>-<route>.
func AppRouter(key, route string) string { return "orglane-app-" + key + "-" + route }

// RouteSource returns the app routes at the moment of a poll.
type RouteSource func(ctx context.Context) ([]AppRoute, error)

// servedTimeout bounds a ServedFunc call.
const servedTimeout = 5 * time.Second

// ServedFunc is told the app routes of every configuration the handler
// served Traefik completely: the routes Build included, in its order
// (docs/specs/providers.md "Route publication").
type ServedFunc func(ctx context.Context, routes []AppRoute)

// noopService is Traefik's built-in service for routers that only redirect.
const noopService = "noop@internal"

// Dynamic is the subset of Traefik's dynamic configuration the core produces.
// Field names follow Traefik's JSON/YAML format.
type Dynamic struct {
	HTTP HTTP `json:"http"`
}

// HTTP holds the HTTP routers, services and middlewares.
type HTTP struct {
	Routers     map[string]Router     `json:"routers"`
	Services    map[string]Service    `json:"services"`
	Middlewares map[string]Middleware `json:"middlewares"`
}

// Router matches requests and hands them to a service.
type Router struct {
	EntryPoints []string   `json:"entryPoints"`
	Rule        string     `json:"rule"`
	Service     string     `json:"service"`
	Middlewares []string   `json:"middlewares,omitempty"`
	TLS         *RouterTLS `json:"tls,omitempty"`
}

// RouterTLS enables TLS on a router, with certificates from a resolver.
type RouterTLS struct {
	CertResolver string `json:"certResolver,omitempty"`
}

// Service forwards requests to upstream servers.
type Service struct {
	LoadBalancer LoadBalancer `json:"loadBalancer"`
}

// LoadBalancer lists the upstream servers of a service.
type LoadBalancer struct {
	Servers        []Server `json:"servers"`
	PassHostHeader bool     `json:"passHostHeader"`
}

// Server is one upstream server.
type Server struct {
	URL string `json:"url"`
}

// Middleware is a request or response transformation. Exactly one field is
// set.
type Middleware struct {
	Headers        *Headers        `json:"headers,omitempty"`
	RedirectScheme *RedirectScheme `json:"redirectScheme,omitempty"`
}

// Headers is Traefik's headers middleware (security headers).
type Headers struct {
	FrameDeny            bool              `json:"frameDeny,omitempty"`
	ContentTypeNosniff   bool              `json:"contentTypeNosniff,omitempty"`
	ReferrerPolicy       string            `json:"referrerPolicy,omitempty"`
	PermissionsPolicy    string            `json:"permissionsPolicy,omitempty"`
	STSSeconds           int64             `json:"stsSeconds,omitempty"`
	CustomRequestHeaders map[string]string `json:"customRequestHeaders,omitempty"`
}

// RedirectScheme redirects requests to another scheme.
type RedirectScheme struct {
	Scheme    string `json:"scheme"`
	Permanent bool   `json:"permanent"`
}

// Security header values sent in every mode.
const (
	referrerPolicy    = "strict-origin-when-cross-origin"
	permissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=()"
)

// Build returns the dynamic configuration for cfg and the app routes: a
// router for the workspace host that forwards to the core, one router per
// app route that forwards to its upstream (or, during maintenance, to the
// core), plus security headers. In domain mode every host is served on the
// HTTPS entry point with a certificate from the configured resolver and
// HSTS, and plain HTTP is redirected to HTTPS. In local mode hosts are
// served on the HTTP entry point only.
//
// cfg must be valid (config.Load validates it). App routes whose host is
// not a valid hostname, equals the workspace host or repeats an earlier
// route's host, and routes without a valid upstream, are left out: a bad
// row must never take over another host.
func Build(cfg *config.Config, apps []AppRoute) Dynamic {
	d, _ := build(cfg, apps)
	return d
}

// build is Build that also returns the app routes it included.
func build(cfg *config.Config, apps []AppRoute) (Dynamic, []AppRoute) {
	headers := &Headers{
		FrameDeny:          true,
		ContentTypeNosniff: true,
		ReferrerPolicy:     referrerPolicy,
		PermissionsPolicy:  permissionsPolicy,
	}
	d := Dynamic{HTTP: HTTP{
		Routers: map[string]Router{},
		Services: map[string]Service{
			ServiceCore: {LoadBalancer: LoadBalancer{
				Servers:        []Server{{URL: cfg.Ingress.UpstreamURL}},
				PassHostHeader: true,
			}},
		},
		Middlewares: map[string]Middleware{MiddlewareHeaders: {Headers: headers}},
	}}
	domain := cfg.Ingress.Mode != config.IngressLocal
	if domain {
		headers.STSSeconds = int64(cfg.Ingress.HSTSMaxAge / time.Second)
		d.HTTP.Middlewares[MiddlewareHTTPSRedirect] = Middleware{
			RedirectScheme: &RedirectScheme{Scheme: "https", Permanent: true},
		}
	}
	// route adds the routers of one host. The workspace host is a validated
	// hostname ([a-z0-9.-]), and app hosts are checked below, so they are
	// safe inside a rule literal. Traefik's Host matcher ignores the port.
	route := func(name, host, service string, middlewares ...string) {
		rule := fmt.Sprintf("Host(`%s`)", host)
		mws := append([]string{MiddlewareHeaders}, middlewares...)
		if !domain {
			d.HTTP.Routers[name] = Router{EntryPoints: []string{EntryPointHTTP}, Rule: rule, Service: service, Middlewares: mws}
			return
		}
		d.HTTP.Routers[name] = Router{
			EntryPoints: []string{EntryPointHTTPS},
			Rule:        rule,
			Service:     service,
			Middlewares: mws,
			TLS:         &RouterTLS{CertResolver: cfg.Ingress.CertResolver},
		}
		// ACME HTTP-01 challenges on the HTTP entry point are answered by
		// Traefik's own router, which takes precedence over this one.
		d.HTTP.Routers[name+"-redirect"] = Router{
			EntryPoints: []string{EntryPointHTTP},
			Rule:        rule,
			Service:     noopService,
			Middlewares: []string{MiddlewareHTTPSRedirect},
		}
	}
	route(RouterWorkspace, cfg.WorkspaceHostname(), ServiceCore)

	hosts := map[string]bool{cfg.WorkspaceHostname(): true}
	var included []AppRoute
	apps = slices.Clone(apps)
	slices.SortFunc(apps, func(a, b AppRoute) int {
		return strings.Compare(a.Key+"\x00"+a.Route, b.Key+"\x00"+b.Route)
	})
	for _, a := range apps {
		name := AppRouter(a.Key, a.Route)
		if !validName(a.Key) || !validName(a.Route) || !validHostname(a.Host) || hosts[a.Host] || d.HTTP.Routers[name].Rule != "" {
			continue
		}
		if a.Maintenance {
			if _, ok := d.HTTP.Middlewares[MiddlewareMaintenance]; !ok {
				d.HTTP.Middlewares[MiddlewareMaintenance] = Middleware{Headers: &Headers{
					CustomRequestHeaders: map[string]string{HeaderMaintenance: "1"},
				}}
			}
			hosts[a.Host] = true
			route(name, a.Host, ServiceCore, MiddlewareMaintenance)
			included = append(included, a)
			continue
		}
		if !validUpstream(a.Upstream) {
			continue
		}
		hosts[a.Host] = true
		d.HTTP.Services[name] = Service{LoadBalancer: LoadBalancer{
			Servers:        []Server{{URL: a.Upstream}},
			PassHostHeader: true,
		}}
		route(name, a.Host, name)
		included = append(included, a)
	}
	return d, included
}

var (
	namePattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func validName(s string) bool { return namePattern.MatchString(s) }

func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if !labelPattern.MatchString(label) {
			return false
		}
	}
	return true
}

// validUpstream accepts http://<hostname>:<port> without anything else.
func validUpstream(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host, port, err := net.SplitHostPort(u.Host)
	return err == nil && validHostname(host) && port != ""
}

// Maintenance answers requests that Traefik routed to the core for an app
// instance under maintenance (HeaderMaintenance on a host other than the
// workspace) with 503 and Retry-After. Everything else goes to next. The
// header alone is not enough: a client can send it, but only Traefik's
// maintenance routers send the core a foreign host.
func Maintenance(workspaceHost string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if r.Header.Get(HeaderMaintenance) == "" || strings.EqualFold(host, workspaceHost) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", MaintenanceRetryAfter)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("This service is being upgraded. Please try again in a moment.\n"))
	})
}

// NewHandler returns the handler of the internal listener: GET ConfigPath
// returns Build(cfg, routes) as JSON, with the app routes read from source
// on every request (nil: none). It must never be served on the public
// listener. After an answer was written and flushed, served (if not nil)
// gets the app routes it contained, with a context bounded by
// servedTimeout.
//
// If source fails, the answer is 503: Traefik then keeps the configuration
// of its last successful poll instead of dropping the app routes.
func NewHandler(cfg *config.Config, source RouteSource, served ServedFunc, logger *slog.Logger) (http.Handler, error) {
	if _, err := json.Marshal(Build(cfg, nil)); err != nil {
		return nil, fmt.Errorf("encode traefik configuration: %w", err)
	}
	logger.Info("serving traefik dynamic configuration", "path", ConfigPath, "mode", cfg.Ingress.Mode,
		"workspace_host", cfg.WorkspaceHostname())

	allowed, err := cfg.Server.InternalAllowedPeerPrefixes()
	if err != nil {
		return nil, fmt.Errorf("server.internal_allowed_peers: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ConfigPath, func(w http.ResponseWriter, r *http.Request) {
		var routes []AppRoute
		if source != nil {
			var err error
			if routes, err = source(r.Context()); err != nil {
				logger.ErrorContext(r.Context(), "traefik configuration: app routes unavailable", "error", err)
				http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
		}
		d, included := build(cfg, routes)
		body, err := json.Marshal(d)
		if err != nil {
			logger.ErrorContext(r.Context(), "traefik configuration: encode", "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if _, err := w.Write(body); err != nil || served == nil {
			return
		}
		// Only an answer that left the core counts as served: a write into
		// the response buffer would not fail when Traefik went away.
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), servedTimeout)
		defer cancel()
		served(ctx, included)
	})
	if len(allowed) == 0 {
		return mux, nil
	}
	return allowPeers(mux, allowed, logger), nil
}

// allowPeers answers 403 to requests whose peer address is not in allowed.
// The peer is the TCP connection's address, never a forwarded header.
func allowPeers(next http.Handler, allowed []netip.Prefix, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !peerAllowed(r.RemoteAddr, allowed) {
			logger.WarnContext(r.Context(), "internal listener: request from a peer that is not allowed", "peer", r.RemoteAddr)
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func peerAllowed(remoteAddr string, allowed []netip.Prefix) bool {
	addrPort, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return false
	}
	addr := addrPort.Addr().Unmap()
	for _, p := range allowed {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
