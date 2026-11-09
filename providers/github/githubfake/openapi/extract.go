//go:build ignore

// Extract writes the subset of GitHub's OpenAPI description that the fake
// GitHub serves: the listed operations and every component they reference.
//
//	go run extract.go <api.github.com.2022-11-28.json> > api.github.com.subset.json
//
// The source is github/rest-api-description at the commit in README.md.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// operations are the endpoints the fake implements: path -> methods.
var operations = map[string][]string{
	"/app":                                 {"get"},
	"/app-manifests/{code}/conversions":    {"post"},
	"/app/hook/config":                     {"patch"},
	"/app/installations/{installation_id}": {"get"},
	"/app/installations/{installation_id}/access_tokens":   {"post"},
	"/applications/{client_id}/grant":                      {"delete"},
	"/orgs/{org}":                                          {"get"},
	"/orgs/{org}/invitations":                              {"get", "post"},
	"/orgs/{org}/invitations/{invitation_id}":              {"delete"},
	"/orgs/{org}/members":                                  {"get"},
	"/orgs/{org}/memberships/{username}":                   {"get", "delete"},
	"/orgs/{org}/teams":                                    {"get", "post"},
	"/orgs/{org}/teams/{team_slug}/memberships/{username}": {"get", "put", "delete"},
	"/user":              {"get"},
	"/user/{account_id}": {"get"},
}

// idParameter is one of the numeric path parameters of the team-ID routes
// GitHub documents as alternates of the slug routes.
func idParameter(name, description string) map[string]any {
	return map[string]any{
		"name": name, "in": "path", "required": true, "description": description,
		"schema": map[string]any{"type": "integer", "format": "int64"},
	}
}

// legacyTeamIDParameters replaces the slug route's org and team_slug path
// parameters with the numeric org_id and team_id of the documented alternate
// route. Other parameters (username, body and responses) are unchanged.
func legacyTeamIDParameters(parameters any) any {
	list, _ := parameters.([]any)
	out := make([]any, 0, len(list))
	for _, p := range list {
		ref := ""
		if m, ok := p.(map[string]any); ok {
			ref, _ = m["$ref"].(string)
		}
		switch {
		case strings.HasSuffix(ref, "/org"):
			out = append(out, idParameter("org_id", "The unique identifier of the organization."))
		case strings.HasSuffix(ref, "/team-slug"):
			out = append(out, idParameter("team_id", "The unique identifier of the team."))
		default:
			out = append(out, p)
		}
	}
	return out
}

// legacyTeamIDPaths derives the team-ID routes that GitHub's REST reference
// documents in the descriptions of the slug operations ("You can also
// specify a team by `org_id` and `team_id` using the route ...") but that its
// OpenAPI description does not list as paths. Each operation keeps the
// documented slug operation's request body and responses, so requests and
// responses are still checked against GitHub's own schemas.
func legacyTeamIDPaths(srcPaths map[string]any) map[string]any {
	routes := []struct {
		from, to string
		methods  []string
	}{
		{"/orgs/{org}/teams/{team_slug}", "/organizations/{org_id}/team/{team_id}", []string{"get"}},
		{"/orgs/{org}/teams/{team_slug}/memberships/{username}", "/organizations/{org_id}/team/{team_id}/memberships/{username}", []string{"get", "put", "delete"}},
	}
	paths := map[string]any{}
	for _, route := range routes {
		src, ok := srcPaths[route.from].(map[string]any)
		if !ok {
			panic("missing path " + route.from)
		}
		item := map[string]any{}
		for _, method := range route.methods {
			op, ok := src[method].(map[string]any)
			if !ok {
				panic("missing operation " + method + " " + route.from)
			}
			raw, err := json.Marshal(op)
			if err != nil {
				panic(err)
			}
			var copied map[string]any
			if err := json.Unmarshal(raw, &copied); err != nil {
				panic(err)
			}
			if parameters, ok := copied["parameters"]; ok {
				copied["parameters"] = legacyTeamIDParameters(parameters)
			}
			if id, ok := copied["operationId"].(string); ok {
				copied["operationId"] = id + "-by-id"
			}
			item[method] = copied
		}
		paths[route.to] = item
	}
	return paths
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run extract.go <description.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		panic(err)
	}
	srcPaths := doc["paths"].(map[string]any)
	paths := map[string]any{}
	for path, methods := range operations {
		item, ok := srcPaths[path].(map[string]any)
		if !ok {
			panic("missing path " + path)
		}
		out := map[string]any{}
		if p, ok := item["parameters"]; ok {
			out["parameters"] = p
		}
		for _, m := range methods {
			op, ok := item[m]
			if !ok {
				panic("missing operation " + m + " " + path)
			}
			out[m] = op
		}
		paths[path] = out
	}
	for path, item := range legacyTeamIDPaths(srcPaths) {
		paths[path] = item
	}
	components := doc["components"].(map[string]any)
	keep := map[string]map[string]any{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if ref, ok := t["$ref"].(string); ok && strings.HasPrefix(ref, "#/components/") {
				parts := strings.SplitN(strings.TrimPrefix(ref, "#/components/"), "/", 2)
				kind, name := parts[0], parts[1]
				if keep[kind] == nil {
					keep[kind] = map[string]any{}
				}
				if _, seen := keep[kind][name]; !seen {
					c := components[kind].(map[string]any)[name]
					keep[kind][name] = c
					walk(c)
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(paths)
	comps := map[string]any{}
	for kind, items := range keep {
		comps[kind] = items
	}
	subset := map[string]any{
		"openapi":    doc["openapi"],
		"info":       doc["info"],
		"servers":    doc["servers"],
		"paths":      paths,
		"components": comps,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(subset); err != nil {
		panic(err)
	}
}
