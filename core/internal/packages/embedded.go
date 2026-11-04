package packages

import "io/fs"

// sources are the managed/ directories of the compiled-in packages. The
// test package agenttest is added only in builds with the "agenttest" tag
// (embedded_agenttest.go), so a production agent cannot install it.
var sources []fs.FS

// Embedded returns the registry of the package versions embedded into this
// binary.
func Embedded() (*Registry, error) { return NewRegistry(sources...) }
