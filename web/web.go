// Package web embeds the built web UI into the core binary (ADR 0001). The
// UI itself is the pnpm package in this directory; `make build` builds it
// into dist/ before compiling the binary. Without a build, dist/ holds only
// .gitkeep and the core serves a notice instead of the UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built UI: index.html, the hashed files under assets/ and
// the files copied from public/.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return sub
}
