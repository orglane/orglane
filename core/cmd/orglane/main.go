// Command orglane is the Orglane core binary.
//
// It is a placeholder for the monorepo tooling skeleton: it only reports its
// version. The server, agent and cli modes arrive with the core skeleton.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Stdout))
}

func run(w io.Writer) int {
	if _, err := fmt.Fprintf(w, "orglane %s\n", version); err != nil {
		return 1
	}
	return 0
}
