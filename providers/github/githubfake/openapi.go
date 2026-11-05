package githubfake

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

//go:embed openapi/api.github.com.subset.json openapi/oauth.json
var descriptions embed.FS

// description is one loaded OpenAPI description with its router.
type description struct {
	doc    *openapi3.T
	router routers.Router
}

var loadDescriptions = sync.OnceValues(func() ([2]description, error) {
	var out [2]description
	for i, name := range []string{"openapi/api.github.com.subset.json", "openapi/oauth.json"} {
		raw, err := descriptions.ReadFile(name)
		if err != nil {
			return out, err
		}
		doc, err := openapi3.NewLoader().LoadFromData(raw)
		if err != nil {
			return out, fmt.Errorf("githubfake: load %s: %w", name, err)
		}
		// GitHub's own examples do not all satisfy its schemas; the
		// schemas are what requests and responses are checked against.
		if err := doc.Validate(context.Background(), openapi3.DisableExamplesValidation()); err != nil {
			return out, fmt.Errorf("githubfake: invalid %s: %w", name, err)
		}
		router, err := gorillamux.NewRouter(doc)
		if err != nil {
			return out, fmt.Errorf("githubfake: route %s: %w", name, err)
		}
		out[i] = description{doc: doc, router: router}
	}
	return out, nil
})

// example returns a deep copy of a named example of the REST description,
// the base of the fake's response objects.
func example(name string) map[string]any {
	d, err := loadDescriptions()
	if err != nil {
		panic(err)
	}
	ex, ok := d[0].doc.Components.Examples[name]
	if !ok || ex.Value == nil {
		panic("githubfake: no example " + name)
	}
	raw, err := json.Marshal(ex.Value.Value)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// exchange is a request with its response, checked against the
// descriptions.
type exchange struct {
	route  *routers.Route
	params map[string]string
	in     *openapi3filter.RequestValidationInput
}

// genericErrors are the statuses GitHub documents for every endpoint
// (authentication, rate limits, server errors) even where an operation
// does not list them.
func genericError(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status >= 500
}

// validateRequest finds the documented operation of r and checks the
// request against it. host is the GitHub host the request was meant for.
func validateRequest(r *http.Request, host string, body []byte) (*exchange, error) {
	d, err := loadDescriptions()
	if err != nil {
		return nil, err
	}
	desc := d[0]
	if host == "github.com" {
		desc = d[1]
	}
	u := *r.URL
	u.Scheme, u.Host = "https", host
	req := r.Clone(r.Context())
	req.URL = &u
	req.Host = host
	req.Body = io.NopCloser(bytes.NewReader(body))
	route, params, err := desc.router.FindRoute(req)
	if err != nil {
		return nil, fmt.Errorf("undocumented operation %s %s: %w", r.Method, r.URL.Path, err)
	}
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, MultiError: true},
	}
	if err := openapi3filter.ValidateRequest(r.Context(), in); err != nil {
		return nil, fmt.Errorf("request %s %s does not match GitHub's description: %w", r.Method, r.URL.Path, err)
	}
	return &exchange{route: route, params: params, in: in}, nil
}

// validateResponse checks a response against the operation. A status the
// operation does not document must be one of GitHub's generic errors with
// a basic-error body.
func validateResponse(x *exchange, status int, header http.Header, body []byte) error {
	documented := x.route.Operation.Responses.Status(status) != nil || x.route.Operation.Responses.Default() != nil
	if !documented {
		if !genericError(status) {
			return fmt.Errorf("status %d is not documented for %s %s", status, x.route.Method, x.route.Path)
		}
		return validateBasicError(body)
	}
	out := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: x.in, Status: status, Header: header,
		Body:    io.NopCloser(bytes.NewReader(body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
		return fmt.Errorf("response %d to %s %s does not match GitHub's description: %w", status, x.route.Method, x.route.Path, err)
	}
	return nil
}

func validateBasicError(body []byte) error {
	d, err := loadDescriptions()
	if err != nil {
		return err
	}
	ref := d[0].doc.Components.Schemas["basic-error"]
	if ref == nil || ref.Value == nil {
		return errors.New("githubfake: basic-error schema missing")
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("generic error body is not JSON: %w", err)
	}
	if err := ref.Value.VisitJSON(v); err != nil {
		return fmt.Errorf("generic error body is not a basic-error: %w", err)
	}
	return nil
}

// pathOf returns r's path for logging (no query: it can hold codes).
func pathOf(r *http.Request) string { return (&url.URL{Path: r.URL.Path}).String() }

// host returns the GitHub host a request was meant for: test transports
// keep it in the Host header; the browser reaches the fake directly for the
// authorization page, which lives on github.com.
func host(r *http.Request) string {
	h := strings.ToLower(r.Host)
	if h == "api.github.com" || h == "github.com" {
		return h
	}
	if strings.HasPrefix(r.URL.Path, "/login/oauth/") {
		return "github.com"
	}
	return "api.github.com"
}
