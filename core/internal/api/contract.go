package api

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/orglane/orglane/core/internal/authz"
)

// PermissionExtension is the OpenAPI vendor extension every operation uses to
// declare the permission it requires (ADR 0004).
const PermissionExtension = "x-orglane-permission"

// VersionPrefix is the path prefix of every product endpoint.
const VersionPrefix = "/api/v1/"

// unversionedPaths are the only paths allowed outside VersionPrefix: the
// health probes and the instance discovery document (ADR 0012, 0018).
var unversionedPaths = []string{"/healthz", "/readyz", "/.well-known/orglane"}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// Operation is one operation of the OpenAPI contract.
type Operation struct {
	Method      string // upper case, e.g. "GET"
	Path        string // e.g. "/api/v1/instance"
	OperationID string
	Permission  authz.Permission
}

// Pattern returns the net/http.ServeMux pattern the generated router
// registers for the operation, e.g. "GET /api/v1/instance".
func (o Operation) Pattern() string {
	return o.Method + " " + o.Path
}

// ParseOperations reads the operations of an OpenAPI document and validates
// the repository conventions: every operation has an operationId and a known
// permission (or "public", "authenticated" or "demo"), and every path is versioned
// under /api/v1 unless it is a health probe or discovery. All violations are
// reported together.
func ParseOperations(spec []byte) ([]Operation, error) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("parse OpenAPI document: %w", err)
	}

	var ops []Operation
	var errs []error
	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, VersionPrefix) && !slices.Contains(unversionedPaths, path) {
			errs = append(errs, fmt.Errorf("path %s: must be under %s (only %v may be unversioned)", path, VersionPrefix, unversionedPaths))
		}
		for method, node := range item {
			if !slices.Contains(httpMethods, method) {
				continue // summary, parameters, servers, ...
			}
			var raw struct {
				OperationID string  `yaml:"operationId"`
				Permission  *string `yaml:"x-orglane-permission"`
			}
			if err := node.Decode(&raw); err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", strings.ToUpper(method), path, err))
				continue
			}
			op := Operation{Method: strings.ToUpper(method), Path: path, OperationID: raw.OperationID}
			if raw.OperationID == "" {
				errs = append(errs, fmt.Errorf("%s: missing operationId", op.Pattern()))
			}
			switch {
			case raw.Permission == nil:
				errs = append(errs, fmt.Errorf("%s: missing %s (use %q for unauthenticated operations)", op.Pattern(), PermissionExtension, authz.Public))
			case !slices.Contains([]authz.Permission{authz.Public, authz.Authenticated, authz.Demo}, authz.Permission(*raw.Permission)) &&
				!authz.Known(authz.Permission(*raw.Permission)):
				errs = append(errs, fmt.Errorf("%s: unknown permission %q in %s (add it to package authz first)", op.Pattern(), *raw.Permission, PermissionExtension))
			default:
				op.Permission = authz.Permission(*raw.Permission)
			}
			ops = append(ops, op)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slices.SortFunc(ops, func(a, b Operation) int { return strings.Compare(a.Pattern(), b.Pattern()) })
	return ops, nil
}
