// Command avatars regenerates the SVG initials avatars of the demo datasets
// from their people files: `go generate ./demo` (run from the repository
// root). The generated files are committed; a test fails when they are
// stale.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/orglane/orglane/demo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "avatars:", err)
		os.Exit(1)
	}
}

func run() error {
	// go generate runs in the package directory (demo/). The people file is
	// read directly, not with demo.Load, which requires the avatar files to
	// exist already.
	for _, name := range []string{demo.Acme} {
		raw, err := os.ReadFile(filepath.Join(name, "people.yaml")) //nolint:gosec // fixed dataset path
		if err != nil {
			return err
		}
		var file struct {
			People []demo.Person `yaml:"people"`
		}
		if err := yaml.Unmarshal(raw, &file); err != nil {
			return fmt.Errorf("%s/people.yaml: %w", name, err)
		}
		for _, p := range file.People {
			if filepath.Ext(p.Avatar) != ".svg" || !filepath.IsLocal(p.Avatar) {
				continue
			}
			out := filepath.Join(name, filepath.FromSlash(p.Avatar))
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
				return err
			}
			if err := os.WriteFile(out, demo.InitialsAvatar(p.DisplayName, p.Username), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}
