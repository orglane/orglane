// Package licenses checks dependency licenses against the ADR 0016 policy.
//
// Two policies exist:
//   - Server: dependencies of the AGPL-3.0-or-later server, web UI and
//     tooling. Permissive licenses, MPL-2.0 and copyleft licenses that can be
//     combined with AGPL-3.0 are allowed.
//   - Apache: production dependencies of the Apache-2.0 packages (SDKs,
//     shared UI packages). Only licenses that keep the package distributable
//     under Apache-2.0 are allowed: permissive licenses and MPL-2.0.
//
// Anything else, including missing or unrecognized licenses, fails the check
// and needs a maintainer decision.
package licenses

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
)

// Policy is a set of allowed SPDX license identifiers.
type Policy struct {
	Name    string
	allowed map[string]bool
}

// permissive licenses are OSI-approved, GPL-compatible and can be combined
// with Apache-2.0 code. Python-2.0 (the PSF license) is used by npm's
// argparse.
var permissive = []string{
	"0BSD", "Apache-2.0", "BlueOak-1.0.0", "BSD-2-Clause", "BSD-3-Clause",
	"CC0-1.0", "ISC", "MIT", "MIT-0", "Python-2.0", "Unlicense", "Zlib",
}

// weakCopyleft is file-level copyleft that is compatible with both Apache-2.0
// and AGPL-3.0 distribution.
var weakCopyleft = []string{"MPL-2.0"}

// agplCompatible are copyleft licenses that can be combined with
// AGPL-3.0-or-later. GPL-2.0-only is deliberately missing: it is incompatible
// with the GPLv3 family.
var agplCompatible = []string{
	"LGPL-2.1-only", "LGPL-2.1-or-later", "LGPL-3.0-only", "LGPL-3.0-or-later",
	"GPL-2.0-or-later", "GPL-3.0-only", "GPL-3.0-or-later",
	"AGPL-3.0-only", "AGPL-3.0-or-later",
}

func newPolicy(name string, lists ...[]string) Policy {
	p := Policy{Name: name, allowed: map[string]bool{}}
	for _, l := range lists {
		for _, id := range l {
			p.allowed[strings.ToLower(id)] = true
		}
	}
	return p
}

// Policies by name.
var (
	Server = newPolicy("server", permissive, weakCopyleft, agplCompatible)
	Apache = newPolicy("apache", permissive, weakCopyleft)
)

// PolicyByName returns the policy called name ("server" or "apache").
func PolicyByName(name string) (Policy, error) {
	switch name {
	case Server.Name:
		return Server, nil
	case Apache.Name:
		return Apache, nil
	}
	return Policy{}, fmt.Errorf("unknown license policy %q (want %q or %q)", name, Server.Name, Apache.Name)
}

// Allowed lists the allowed SPDX identifiers.
func (p Policy) Allowed() []string {
	var out []string
	for _, l := range [][]string{permissive, weakCopyleft, agplCompatible} {
		for _, id := range l {
			if p.allowed[strings.ToLower(id)] {
				out = append(out, id)
			}
		}
	}
	return out
}

// deprecated maps deprecated SPDX identifiers (still common in package
// metadata and license classifiers) to their current form.
var deprecated = map[string]string{
	"gpl-2.0+": "GPL-2.0-or-later", "gpl-3.0": "GPL-3.0-only", "gpl-3.0+": "GPL-3.0-or-later",
	"lgpl-2.1": "LGPL-2.1-only", "lgpl-2.1+": "LGPL-2.1-or-later",
	"lgpl-3.0": "LGPL-3.0-only", "lgpl-3.0+": "LGPL-3.0-or-later",
	"agpl-3.0": "AGPL-3.0-only", "agpl-3.0+": "AGPL-3.0-or-later",
}

// Permits reports whether an SPDX license expression is allowed. "A OR B"
// needs one allowed side, "A AND B" needs both, and "A WITH exception" is
// judged by A (exceptions only grant additional permissions).
func (p Policy) Permits(expr string) (bool, error) {
	toks, err := tokenize(expr)
	if err != nil {
		return false, err
	}
	if len(toks) == 0 {
		return false, errors.New("empty license expression")
	}
	ps := &parser{toks: toks, policy: p}
	ok, err := ps.or()
	if err != nil {
		return false, err
	}
	if ps.pos != len(toks) {
		return false, fmt.Errorf("unexpected %q in license expression %q", toks[ps.pos], expr)
	}
	return ok, nil
}

func tokenize(expr string) ([]string, error) {
	var toks []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range expr {
		switch {
		case r == '(' || r == ')':
			flush()
			toks = append(toks, string(r))
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		case r == '-' || r == '.' || r == '+' || r == ':' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			cur.WriteRune(r)
		default:
			return nil, fmt.Errorf("license %q is not an SPDX expression", expr)
		}
	}
	flush()
	return toks, nil
}

type parser struct {
	toks   []string
	pos    int
	policy Policy
}

func (ps *parser) peek() string {
	if ps.pos < len(ps.toks) {
		return ps.toks[ps.pos]
	}
	return ""
}

func (ps *parser) or() (bool, error) {
	ok, err := ps.and()
	if err != nil {
		return false, err
	}
	for strings.EqualFold(ps.peek(), "OR") {
		ps.pos++
		right, err := ps.and()
		if err != nil {
			return false, err
		}
		ok = ok || right
	}
	return ok, nil
}

func (ps *parser) and() (bool, error) {
	ok, err := ps.term()
	if err != nil {
		return false, err
	}
	for strings.EqualFold(ps.peek(), "AND") {
		ps.pos++
		right, err := ps.term()
		if err != nil {
			return false, err
		}
		ok = ok && right
	}
	return ok, nil
}

func (ps *parser) term() (bool, error) {
	tok := ps.peek()
	switch {
	case tok == "":
		return false, errors.New("license expression ends unexpectedly")
	case tok == "(":
		ps.pos++
		ok, err := ps.or()
		if err != nil {
			return false, err
		}
		if ps.peek() != ")" {
			return false, errors.New("missing ')' in license expression")
		}
		ps.pos++
		return ok, nil
	case tok == ")" || isOperator(tok):
		return false, fmt.Errorf("unexpected %q in license expression", tok)
	}
	ps.pos++
	ok := ps.policy.permitsID(tok)
	if strings.EqualFold(ps.peek(), "WITH") {
		ps.pos++
		if exc := ps.peek(); exc == "" || exc == "(" || exc == ")" || isOperator(exc) {
			return false, errors.New("missing exception after WITH")
		}
		ps.pos++
	}
	return ok, nil
}

func isOperator(tok string) bool {
	return strings.EqualFold(tok, "AND") || strings.EqualFold(tok, "OR") || strings.EqualFold(tok, "WITH")
}

func (p Policy) permitsID(id string) bool {
	id = strings.ToLower(id)
	if canonical, ok := deprecated[id]; ok {
		id = strings.ToLower(canonical)
	}
	return p.allowed[id]
}

// Dependency is a third-party package and its declared license.
type Dependency struct {
	Name    string
	Version string
	License string
}

// Violation is a dependency whose license the policy does not allow.
type Violation struct {
	Dependency
	Reason string
}

func (v Violation) String() string {
	name := v.Name
	if v.Version != "" {
		name += "@" + v.Version
	}
	return fmt.Sprintf("%s: %s", name, v.Reason)
}

// Check returns the dependencies whose license p does not allow.
func Check(p Policy, deps []Dependency) []Violation {
	var out []Violation
	for _, d := range deps {
		lic := strings.TrimSpace(d.License)
		if lic == "" || strings.EqualFold(lic, "unknown") {
			out = append(out, Violation{d, "license is unknown"})
			continue
		}
		ok, err := p.Permits(lic)
		switch {
		case err != nil:
			out = append(out, Violation{d, fmt.Sprintf("license %q is not recognized: %v", lic, err)})
		case !ok:
			out = append(out, Violation{d, fmt.Sprintf("license %q is not allowed by the %s policy", lic, p.Name)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ParsePnpm parses the output of `pnpm licenses list --json`. Packages
// without license metadata are reported under the "Unknown" key.
func ParsePnpm(r io.Reader) ([]Dependency, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var groups map[string][]struct {
		Name     string   `json:"name"`
		Versions []string `json:"versions"`
		License  string   `json:"license"`
	}
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, fmt.Errorf("parsing pnpm licenses output: %w", err)
	}
	var deps []Dependency
	for key, pkgs := range groups {
		for _, pkg := range pkgs {
			lic := pkg.License
			if lic == "" {
				lic = key
			}
			deps = append(deps, Dependency{Name: pkg.Name, Version: strings.Join(pkg.Versions, ", "), License: lic})
		}
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	return deps, nil
}

// ParseGoLicenses parses the CSV output of `go-licenses report`
// (package, license URL, license name).
func ParseGoLicenses(r io.Reader) ([]Dependency, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing go-licenses report: %w", err)
	}
	var deps []Dependency
	for _, rec := range records {
		if len(rec) == 0 || (len(rec) == 1 && strings.TrimSpace(rec[0]) == "") {
			continue
		}
		if len(rec) != 3 {
			return nil, fmt.Errorf("go-licenses report: want 3 fields, got %d in %q", len(rec), rec)
		}
		deps = append(deps, Dependency{Name: rec[0], License: rec[2]})
	}
	deps = slices.CompactFunc(deps, func(a, b Dependency) bool { return a == b })
	return deps, nil
}
