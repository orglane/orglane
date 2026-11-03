package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/orglane/orglane/core/internal/authz"
)

// routeGuard is the router handed to the generated code. Every route the
// generated code registers is wrapped with the permission its operation
// declares in the contract, before any parameter or body is parsed. A route
// without a contract operation is recorded as an error, so the server fails
// closed instead of serving an unguarded endpoint.
//
// Operations marked authz.Demo are registered only in demo mode; otherwise
// they are dropped here, so their paths answer 404 like any unknown path.
type routeGuard struct {
	mux        *http.ServeMux
	demo       bool
	ops        map[string]Operation
	auth       authz.Authenticator
	logger     *slog.Logger
	registered map[string]bool
	errs       []error
}

func newRouteGuard(mux *http.ServeMux, ops []Operation, auth authz.Authenticator, logger *slog.Logger, demo bool) *routeGuard {
	g := &routeGuard{
		mux:        mux,
		demo:       demo,
		ops:        make(map[string]Operation, len(ops)),
		auth:       auth,
		logger:     logger,
		registered: make(map[string]bool, len(ops)),
	}
	for _, op := range ops {
		g.ops[op.Pattern()] = op
	}
	return g
}

// HandleFunc implements apigen.ServeMux.
func (g *routeGuard) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	op, ok := g.ops[pattern]
	if !ok {
		g.errs = append(g.errs, fmt.Errorf("route %q has no operation in the OpenAPI contract", pattern))
		return
	}
	g.registered[pattern] = true
	if op.Permission == authz.Demo && !g.demo {
		return // demo mode only: not served at all
	}
	g.mux.Handle(pattern, g.enforce(op, http.HandlerFunc(handler)))
}

// ServeHTTP implements apigen.ServeMux.
func (g *routeGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mux.ServeHTTP(w, r)
}

// err reports routes without an operation and operations without a route
// (stale generated code).
func (g *routeGuard) err() error {
	errs := g.errs
	for pattern := range g.ops {
		if !g.registered[pattern] {
			errs = append(errs, fmt.Errorf("operation %q is not served (regenerate with `make generate`)", pattern))
		}
	}
	return errors.Join(errs...)
}

func (g *routeGuard) enforce(op Operation, next http.Handler) http.Handler {
	if op.Permission == authz.Public || op.Permission == authz.Demo {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := g.auth.Authenticate(r)
		switch {
		case errors.Is(err, authz.ErrUnauthenticated) || (err == nil && principal == nil):
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeProblem(w, r, http.StatusUnauthorized, "This operation requires authentication.")
			return
		case errors.Is(err, authz.ErrCrossSite):
			writeProblem(w, r, http.StatusForbidden, "Cross-site request rejected: send the X-CSRF-Token header from the same origin.")
			return
		case err != nil:
			g.logger.ErrorContext(r.Context(), "authentication failed", "operation", op.OperationID, "error", err)
			writeProblem(w, r, http.StatusInternalServerError, "")
			return
		case !principal.Has(op.Permission):
			writeProblem(w, r, http.StatusForbidden, fmt.Sprintf("This operation requires the %q permission.", op.Permission))
			return
		}
		if op.OperationID == "updatePerson" && !validatePersonUpdate(w, r) {
			return
		}
		next.ServeHTTP(w, r.WithContext(authz.WithPrincipal(r.Context(), principal)))
	})
}
