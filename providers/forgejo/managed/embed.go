// Package managed embeds the versions of the Forgejo managed package
// (docs/specs/agent.md "Package format"): one directory per version with
// its manifest, Compose template and files. The core and the agent load
// them through core/internal/packages.
package managed

import "embed"

// FS holds the package versions, one directory per version.
//
//go:embed all:15.0.11-1
var FS embed.FS

// Current is the newest package version: the one new instances install.
const Current = "15.0.11-1"
