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
	"/app/installations/{installation_id}/access_tokens": {"post"},
	"/applications/{client_id}/grant":                    {"delete"},
	"/orgs/{org}":                                        {"get"},
	"/user":                                              {"get"},
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
