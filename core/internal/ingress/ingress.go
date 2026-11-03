// Package ingress builds the Traefik dynamic configuration served by the
// core (ADR 0008). Traefik has no Docker socket: it polls the core's internal
// listener through its HTTP provider and routes requests from what it gets.
//
// The static side of the contract (entry point and certificate resolver
// names) lives in deploy/compose/compose.yaml.
package ingress

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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
)

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
	FrameDeny          bool   `json:"frameDeny,omitempty"`
	ContentTypeNosniff bool   `json:"contentTypeNosniff,omitempty"`
	ReferrerPolicy     string `json:"referrerPolicy,omitempty"`
	PermissionsPolicy  string `json:"permissionsPolicy,omitempty"`
	STSSeconds         int64  `json:"stsSeconds,omitempty"`
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

// Build returns the dynamic configuration for cfg: a router for the
// workspace host that forwards to the core, plus security headers. In domain
// mode the workspace is served on the HTTPS entry point with a certificate
// from the configured resolver and HSTS, and plain HTTP is redirected to
// HTTPS. In local mode it is served on the HTTP entry point only.
//
// cfg must be valid (config.Load validates it).
func Build(cfg *config.Config) Dynamic {
	// The workspace host is a validated hostname ([a-z0-9.-]), so it is safe
	// inside a rule literal. Traefik's Host matcher ignores the port.
	rule := fmt.Sprintf("Host(`%s`)", cfg.WorkspaceHostname())
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

	if cfg.Ingress.Mode == config.IngressLocal {
		d.HTTP.Routers[RouterWorkspace] = Router{
			EntryPoints: []string{EntryPointHTTP},
			Rule:        rule,
			Service:     ServiceCore,
			Middlewares: []string{MiddlewareHeaders},
		}
		return d
	}

	headers.STSSeconds = int64(cfg.Ingress.HSTSMaxAge / time.Second)
	d.HTTP.Routers[RouterWorkspace] = Router{
		EntryPoints: []string{EntryPointHTTPS},
		Rule:        rule,
		Service:     ServiceCore,
		Middlewares: []string{MiddlewareHeaders},
		TLS:         &RouterTLS{CertResolver: cfg.Ingress.CertResolver},
	}
	// ACME HTTP-01 challenges on the HTTP entry point are answered by
	// Traefik's own router, which takes precedence over this one.
	d.HTTP.Routers[RouterWorkspaceRedirect] = Router{
		EntryPoints: []string{EntryPointHTTP},
		Rule:        rule,
		Service:     noopService,
		Middlewares: []string{MiddlewareHTTPSRedirect},
	}
	d.HTTP.Middlewares[MiddlewareHTTPSRedirect] = Middleware{
		RedirectScheme: &RedirectScheme{Scheme: "https", Permanent: true},
	}
	return d
}

// NewHandler returns the handler of the internal listener: GET ConfigPath
// returns Build(cfg) as JSON. It must never be served on the public listener.
func NewHandler(cfg *config.Config, logger *slog.Logger) (http.Handler, error) {
	body, err := json.Marshal(Build(cfg))
	if err != nil {
		return nil, fmt.Errorf("encode traefik configuration: %w", err)
	}
	logger.Info("serving traefik dynamic configuration", "path", ConfigPath, "mode", cfg.Ingress.Mode,
		"workspace_host", cfg.WorkspaceHostname())

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ConfigPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	})
	return mux, nil
}
