// Package api serves the Orglane HTTP API defined in core/api/openapi.yaml.
//
// The server interfaces are generated into package apigen (`make generate`).
// This package implements them, enforces the permission every operation
// declares (ADR 0004) and renders errors as RFC 9457 problem details.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	specpkg "github.com/orglane/orglane/core/api"
	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity/web"
	"github.com/orglane/orglane/core/internal/mail"
	"github.com/orglane/orglane/core/internal/profile"
	"github.com/orglane/orglane/demo"
)

func itoa(n int) string { return strconv.Itoa(n) }

// ReadinessCheck is one dependency checked by /readyz.
type ReadinessCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

// Options configure the API handler.
type Options struct {
	// Version is the server version.
	Version string
	// BaseDomain and WorkspaceHost describe how the instance is addressed
	// (ADR 0018).
	BaseDomain    string
	WorkspaceHost string
	// ReadinessChecks run on every /readyz request.
	ReadinessChecks []ReadinessCheck
	// ReadinessTimeout bounds all readiness checks together. Defaults to 3s.
	ReadinessTimeout time.Duration
	// WorkspaceURL is the workspace URL, e.g. https://work.acme.com.
	WorkspaceURL string
	// Authenticator resolves callers of non-public operations. Defaults to
	// Auth when set, otherwise to authz.DenyAll.
	Authenticator authz.Authenticator
	// Auth serves sign-in, sessions and passkeys. Without it those
	// operations fail with 500 and every non-public operation is denied.
	Auth *web.Auth
	// Profile serves profile fields (ADR 0015). Without it those operations
	// fail with 500.
	Profile *profile.Service
	// Mail serves the system email settings. Without it those operations
	// fail with 500.
	Mail *mail.Service
	// WorkEmail builds the default work email from a username
	// (config.Config.WorkEmail). Defaults to <username>@<BaseDomain>.
	WorkEmail func(username string) string
	// Demo is the dataset of demo mode, nil outside demo mode. Only with it
	// are the operations marked x-orglane-permission: demo served; without
	// it their routes are not registered at all.
	Demo *demo.Dataset
	// WebUI serves the web app for every path not handled otherwise (SPA
	// fallback). Without it such paths answer 404.
	WebUI http.Handler
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// NewHandler returns the HTTP handler for the whole contract: the health
// probes and everything under /api/v1, plus the OpenID Provider and the web
// UI when configured. It fails if any operation lacks a
// valid permission or if the generated routes and the contract disagree.
func NewHandler(opts Options) (http.Handler, error) {
	return newHandler(specpkg.Spec, opts)
}

func newHandler(spec []byte, opts Options) (http.Handler, error) {
	if opts.Authenticator == nil {
		if opts.Auth != nil {
			opts.Authenticator = opts.Auth
		} else {
			opts.Authenticator = authz.DenyAll{}
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ReadinessTimeout <= 0 {
		opts.ReadinessTimeout = 3 * time.Second
	}

	ops, err := ParseOperations(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid API contract: %w", err)
	}

	mux := http.NewServeMux()
	guard := newRouteGuard(mux, ops, opts.Authenticator, opts.Logger, opts.Demo != nil)
	badRequest := func(w http.ResponseWriter, r *http.Request, err error) {
		writeProblem(w, r, http.StatusBadRequest, err.Error())
	}
	strict := apigen.NewStrictHandlerWithOptions(&server{opts: opts}, []apigen.StrictMiddlewareFunc{withHTTP}, apigen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: badRequest,
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			var apiErr *apiError
			if errors.As(err, &apiErr) {
				if apiErr.retryAfter != "" {
					w.Header().Set("Retry-After", apiErr.retryAfter)
				}
				writeProblem(w, r, apiErr.status, apiErr.detail)
				return
			}
			opts.Logger.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
			writeProblem(w, r, http.StatusInternalServerError, "")
		},
	})
	apigen.HandlerWithOptions(strict, apigen.StdHTTPServerOptions{
		BaseRouter:       guard,
		ErrorHandlerFunc: badRequest,
	})
	if err := guard.err(); err != nil {
		return nil, fmt.Errorf("API routes do not match the contract: %w", err)
	}

	// Unknown API paths get problem details too, not the plain-text default.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, "")
	})

	if opts.WebUI != nil {
		mux.Handle("/", opts.WebUI)
	}

	var handler http.Handler = mux
	if opts.Auth != nil {
		// The OpenID Provider and its login page share the server.
		oidc := opts.Auth.OIDCHandler()
		mux.Handle("/oidc/", oidc)
		mux.Handle("/.well-known/openid-configuration", oidc)
		// Login CSRF: unauthenticated state-changing API requests must not
		// come from another origin (authenticated ones are checked by the
		// authenticator).
		handler = rejectCrossOriginAPI(opts.Auth, mux)
	}
	return recoverer(handler, opts.Logger), nil
}

func rejectCrossOriginAPI(a *web.Auth, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Method != http.MethodOptions && a.CrossOrigin(r) {
			writeProblem(w, r, http.StatusForbidden, "Cross-origin request rejected.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverer turns a handler panic into a logged 500 problem response.
func recoverer(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			logger.ErrorContext(r.Context(), "handler panic", "method", r.Method, "path", r.URL.Path, "panic", v)
			writeProblem(w, r, http.StatusInternalServerError, "")
		}()
		next.ServeHTTP(w, r)
	})
}

// server implements apigen.StrictServerInterface.
type server struct {
	opts Options
}

var _ apigen.StrictServerInterface = (*server)(nil)

func (s *server) GetHealthz(context.Context, apigen.GetHealthzRequestObject) (apigen.GetHealthzResponseObject, error) {
	return apigen.GetHealthz200JSONResponse{Status: apigen.HealthStatusOk}, nil
}

func (s *server) GetReadyz(ctx context.Context, _ apigen.GetReadyzRequestObject) (apigen.GetReadyzResponseObject, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.ReadinessTimeout)
	defer cancel()

	ready := true
	checks := make([]apigen.HealthCheck, 0, len(s.opts.ReadinessChecks))
	for _, c := range s.opts.ReadinessChecks {
		status := apigen.HealthCheckStatusOk
		if err := c.Check(ctx); err != nil {
			// Details stay in the log: the probe is public.
			s.opts.Logger.WarnContext(ctx, "readiness check failed", "check", c.Name, "error", err)
			status = apigen.HealthCheckStatusUnavailable
			ready = false
		}
		checks = append(checks, apigen.HealthCheck{Name: c.Name, Status: status})
	}
	if !ready {
		return apigen.GetReadyz503JSONResponse{Status: apigen.HealthStatusUnavailable, Checks: &checks}, nil
	}
	return apigen.GetReadyz200JSONResponse{Status: apigen.HealthStatusOk, Checks: &checks}, nil
}

func (s *server) GetVersion(context.Context, apigen.GetVersionRequestObject) (apigen.GetVersionResponseObject, error) {
	return apigen.GetVersion200JSONResponse{Version: s.opts.Version}, nil
}

func (s *server) GetInstance(context.Context, apigen.GetInstanceRequestObject) (apigen.GetInstanceResponseObject, error) {
	return apigen.GetInstance200JSONResponse{
		Version:       s.opts.Version,
		BaseDomain:    s.opts.BaseDomain,
		WorkspaceHost: s.opts.WorkspaceHost,
	}, nil
}
