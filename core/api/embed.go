// Package api holds the Orglane OpenAPI contract (openapi.yaml).
//
// The contract is the source of truth: server interfaces are generated from it
// (see oapi-codegen.yaml and `make generate`), and the server reads the
// permission each operation declares straight from the embedded document.
package api

import _ "embed"

// Spec is the OpenAPI document, embedded at build time.
//
//go:embed openapi.yaml
var Spec []byte
