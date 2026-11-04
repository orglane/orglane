// Package packages is the registry of the managed package versions embedded
// into the binary, and renders them (docs/specs/agent.md "Package format").
// The core uses it for inputs, routes, resource hints and upgrade paths; the
// agent renders packages with it and checks the rendered project against
// the package policy before anything runs.
package packages

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"text/template"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// Package file names.
const (
	ManifestFile        = "manifest.yaml"
	ComposeTemplateFile = "compose.yaml.tmpl"
	FilesDir            = "files"
	templateSuffix      = ".tmpl"
)

// Package is one parsed, validated package version.
type Package struct {
	Manifest *Manifest

	compose *template.Template
	// files are the package files under files/, by slash-separated path.
	// Templates (*.tmpl, stored without the suffix) are rendered.
	files map[string]packageFile
}

type packageFile struct {
	data     []byte
	template *template.Template
}

// Ref returns the package's reference.
func (p *Package) Ref() agentapi.PackageRef {
	return agentapi.PackageRef{ID: p.Manifest.ID, Version: p.Manifest.Version}
}

// Registry holds package versions by ID and version.
type Registry struct {
	pkgs map[agentapi.PackageRef]*Package
}

// NewRegistry loads every package version from the sources. Each source is
// a package's managed/ directory: one subdirectory per version, named like
// the version.
func NewRegistry(sources ...fs.FS) (*Registry, error) {
	r := &Registry{pkgs: map[agentapi.PackageRef]*Package{}}
	for _, src := range sources {
		entries, err := fs.ReadDir(src, ".")
		if err != nil {
			return nil, fmt.Errorf("packages: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub, err := fs.Sub(src, e.Name())
			if err != nil {
				return nil, fmt.Errorf("packages: %w", err)
			}
			p, err := Load(sub)
			if err != nil {
				return nil, fmt.Errorf("packages: %s: %w", e.Name(), err)
			}
			if p.Manifest.Version != e.Name() {
				return nil, fmt.Errorf("packages: directory %s holds version %s", e.Name(), p.Manifest.Version)
			}
			if _, dup := r.pkgs[p.Ref()]; dup {
				return nil, fmt.Errorf("packages: duplicate %s", p.Ref())
			}
			r.pkgs[p.Ref()] = p
		}
	}
	return r, nil
}

// Get returns an embedded package version.
func (r *Registry) Get(ref agentapi.PackageRef) (*Package, bool) {
	p, ok := r.pkgs[ref]
	return p, ok
}

// List returns every package version, sorted by ID and version.
func (r *Registry) List() []agentapi.PackageRef {
	out := make([]agentapi.PackageRef, 0, len(r.pkgs))
	for ref := range r.pkgs {
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b agentapi.PackageRef) int {
		if c := cmp.Compare(a.ID, b.ID); c != 0 {
			return c
		}
		va, _ := ParseVersion(a.Version) // validated on load
		vb, _ := ParseVersion(b.Version)
		return va.Compare(vb)
	})
	return out
}

// Load parses one package version directory.
func Load(fsys fs.FS) (*Package, error) {
	raw, err := fs.ReadFile(fsys, ManifestFile)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	p := &Package{Manifest: m, files: map[string]packageFile{}}
	tmpl, err := fs.ReadFile(fsys, ComposeTemplateFile)
	if err != nil {
		return nil, err
	}
	if p.compose, err = parseTemplate(ComposeTemplateFile, tmpl); err != nil {
		return nil, err
	}
	err = fs.WalkDir(fsys, FilesDir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && name == FilesDir {
				return fs.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", name)
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(name, FilesDir+"/")
		f := packageFile{data: data}
		if strings.HasSuffix(rel, templateSuffix) {
			rel = strings.TrimSuffix(rel, templateSuffix)
			if f.template, err = parseTemplate(name, data); err != nil {
				return err
			}
		}
		if _, dup := p.files[rel]; dup {
			return fmt.Errorf("%s: duplicate file %s", name, rel)
		}
		p.files[rel] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// hasFileOrDir reports whether rel names a package file or a directory of
// package files ("." is the files directory itself).
func (p *Package) hasFileOrDir(rel string) bool {
	if rel == "." {
		return true
	}
	if !fs.ValidPath(rel) {
		return false
	}
	for name := range p.files {
		if name == rel || strings.HasPrefix(name, rel+"/") {
			return true
		}
	}
	return false
}

func parseTemplate(name string, data []byte) (*template.Template, error) {
	t, err := template.New(path.Base(name)).Option("missingkey=error").Funcs(placeholderFuncs).Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return t, nil
}
