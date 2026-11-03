// Package demo holds the demo datasets: human-readable, versioned fixture
// files used by demo mode (`orglane server --demo`), screenshots, end-to-end
// tests and module previews. The only dataset today is "acme" (Acme
// Robotics), in demo/acme.
//
// This package only reads and checks the files; the core loads a dataset
// into an empty instance through its identity service (package
// core/internal/demomode), so every change is audited like any other.
package demo

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

//go:generate go run ./cmd/avatars

// files holds every dataset, embedded into the binary.
//
//go:embed acme
var files embed.FS

// Acme is the name of the "Acme Robotics" dataset.
const Acme = "acme"

// Built-in role keys a demo person can hold (ADR 0004).
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Dataset is a parsed, checked demo dataset.
type Dataset struct {
	// Name identifies the dataset, e.g. "acme".
	Name string
	// Version changes when the dataset changes in a way loaders or tests
	// must know about.
	Version int
	Company Company
	// DefaultUsername is who one-click sign-in uses without a choice: the
	// Owner.
	DefaultUsername string
	// People in file order.
	People []Person
	// Teams in file order.
	Teams []Team

	fsys fs.FS
}

// Company describes the demo company.
type Company struct {
	Name string `yaml:"name"`
	// EmailDomain is the domain of every work email, e.g.
	// acme-robotics.example.
	EmailDomain string `yaml:"email_domain"`
}

// Person is a person of a dataset.
type Person struct {
	Username    string `yaml:"username"`
	DisplayName string `yaml:"display_name"`
	WorkEmail   string `yaml:"work_email"`
	Title       string `yaml:"title"`
	// Role is the built-in role key: RoleOwner, RoleAdmin or RoleMember.
	Role string `yaml:"role"`
	// Avatar is the image file, relative to the dataset directory.
	Avatar string `yaml:"avatar"`
}

// Team is a team of a dataset.
type Team struct {
	Slug        string `yaml:"slug"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// Leads and Members are usernames of dataset people. A person is either
	// a lead or a member of a team, not both.
	Leads   []string `yaml:"leads"`
	Members []string `yaml:"members"`
}

type datasetFile struct {
	Version         int     `yaml:"version"`
	Company         Company `yaml:"company"`
	DefaultUsername string  `yaml:"default_username"`
}

type peopleFile struct {
	People []Person `yaml:"people"`
}

type teamsFile struct {
	Teams []Team `yaml:"teams"`
}

// Load returns the embedded dataset with the given name.
func Load(name string) (*Dataset, error) {
	if !fs.ValidPath(name) || strings.Contains(name, "/") {
		return nil, fmt.Errorf("demo: invalid dataset name %q", name)
	}
	sub, err := fs.Sub(files, name)
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "dataset.yaml"); err != nil {
		return nil, fmt.Errorf("demo: unknown dataset %q", name)
	}
	return LoadFS(name, sub)
}

// LoadFS reads and checks a dataset from fsys, the dataset directory.
func LoadFS(name string, fsys fs.FS) (*Dataset, error) {
	var meta datasetFile
	if err := decodeStrict(fsys, "dataset.yaml", &meta); err != nil {
		return nil, err
	}
	var people peopleFile
	if err := decodeStrict(fsys, "people.yaml", &people); err != nil {
		return nil, err
	}
	// teams.yaml is optional.
	var teams teamsFile
	if _, err := fs.Stat(fsys, "teams.yaml"); err == nil {
		if err := decodeStrict(fsys, "teams.yaml", &teams); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("demo: %w", err)
	}
	d := &Dataset{
		Name:            name,
		Version:         meta.Version,
		Company:         meta.Company,
		DefaultUsername: meta.DefaultUsername,
		People:          people.People,
		Teams:           teams.Teams,
		fsys:            fsys,
	}
	if err := d.check(); err != nil {
		return nil, fmt.Errorf("demo: dataset %q: %w", name, err)
	}
	return d, nil
}

func decodeStrict(fsys fs.FS, name string, v any) error {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("demo: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("demo: parse %s: %w", name, err)
	}
	return nil
}

// check validates what the files must agree on. Field formats (usernames,
// emails, lengths) are validated again by the identity service on load.
func (d *Dataset) check() error {
	var errs []error
	if d.Version < 1 {
		errs = append(errs, errors.New("version must be at least 1"))
	}
	if strings.TrimSpace(d.Company.Name) == "" {
		errs = append(errs, errors.New("company.name is required"))
	}
	if d.Company.EmailDomain == "" {
		errs = append(errs, errors.New("company.email_domain is required"))
	}
	if len(d.People) == 0 {
		errs = append(errs, errors.New("no people"))
	}
	usernames, emails := map[string]bool{}, map[string]bool{}
	owners := 0
	for i, p := range d.People {
		at := fmt.Sprintf("people[%d] (%s)", i, p.Username)
		switch {
		case p.Username == "" || p.Username != strings.ToLower(p.Username):
			errs = append(errs, fmt.Errorf("%s: username must be set and lower case", at))
		case usernames[p.Username]:
			errs = append(errs, fmt.Errorf("%s: duplicate username", at))
		}
		usernames[p.Username] = true
		email := strings.ToLower(p.WorkEmail)
		if !strings.HasSuffix(email, "@"+d.Company.EmailDomain) {
			errs = append(errs, fmt.Errorf("%s: work_email must be in the company domain %s", at, d.Company.EmailDomain))
		}
		if emails[email] {
			errs = append(errs, fmt.Errorf("%s: duplicate work_email", at))
		}
		emails[email] = true
		if strings.TrimSpace(p.DisplayName) == "" {
			errs = append(errs, fmt.Errorf("%s: display_name is required", at))
		}
		if !slices.Contains([]string{RoleOwner, RoleAdmin, RoleMember}, p.Role) {
			errs = append(errs, fmt.Errorf("%s: role must be owner, admin or member, got %q", at, p.Role))
		}
		if p.Role == RoleOwner {
			owners++
		}
		if p.Avatar != "" {
			if !fs.ValidPath(p.Avatar) || path.Ext(p.Avatar) != ".svg" && path.Ext(p.Avatar) != ".png" && path.Ext(p.Avatar) != ".webp" {
				errs = append(errs, fmt.Errorf("%s: avatar must be a .svg, .png or .webp file inside the dataset", at))
			} else if _, err := fs.Stat(d.fsys, p.Avatar); err != nil {
				errs = append(errs, fmt.Errorf("%s: avatar: %w", at, err))
			}
		}
	}
	if owners != 1 {
		errs = append(errs, fmt.Errorf("exactly one person must be the owner, found %d", owners))
	}
	errs = append(errs, d.checkTeams(usernames)...)
	if p, ok := d.Person(d.DefaultUsername); !ok {
		errs = append(errs, fmt.Errorf("default_username %q is not a person of the dataset", d.DefaultUsername))
	} else if p.Role != RoleOwner {
		errs = append(errs, fmt.Errorf("default_username %q must be the owner", d.DefaultUsername))
	}
	return errors.Join(errs...)
}

// teamSlugPattern matches the identity service and teams table constraints.
var teamSlugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkTeams validates canonical team slugs and the dataset's usernames.
func (d *Dataset) checkTeams(usernames map[string]bool) []error {
	var errs []error
	slugs := map[string]bool{}
	for i, t := range d.Teams {
		at := fmt.Sprintf("teams[%d] (%s)", i, t.Slug)
		if !teamSlugPattern.MatchString(t.Slug) {
			errs = append(errs, fmt.Errorf("%s: slug must be set to 1-63 lower case letters or digits with optional inner hyphens", at))
		}
		canonical := strings.ToLower(strings.TrimSpace(t.Slug))
		if slugs[canonical] {
			errs = append(errs, fmt.Errorf("%s: duplicate slug", at))
		}
		slugs[canonical] = true
		if strings.TrimSpace(t.Name) == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", at))
		}
		seen := map[string]bool{}
		for _, u := range slices.Concat(t.Leads, t.Members) {
			if !usernames[u] {
				errs = append(errs, fmt.Errorf("%s: %q is not a person of the dataset", at, u))
			}
			if seen[u] {
				errs = append(errs, fmt.Errorf("%s: %q is listed more than once", at, u))
			}
			seen[u] = true
		}
	}
	return errs
}

// Person returns the person with the given username.
func (d *Dataset) Person(username string) (Person, bool) {
	i := slices.IndexFunc(d.People, func(p Person) bool { return p.Username == username })
	if i < 0 {
		return Person{}, false
	}
	return d.People[i], true
}

// Owner returns the dataset's owner.
func (d *Dataset) Owner() Person {
	p, _ := d.Person(d.DefaultUsername)
	return p
}

// Avatar returns a person's avatar image and its content type. ok is false
// when the person has none.
func (d *Dataset) Avatar(p Person) (data []byte, contentType string, ok bool, err error) {
	if p.Avatar == "" {
		return nil, "", false, nil
	}
	data, err = fs.ReadFile(d.fsys, p.Avatar)
	if err != nil {
		return nil, "", false, fmt.Errorf("demo: avatar of %s: %w", p.Username, err)
	}
	return data, avatarContentTypes[path.Ext(p.Avatar)], true, nil
}

var avatarContentTypes = map[string]string{
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".webp": "image/webp",
}
