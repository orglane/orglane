package packages

import (
	"fmt"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// UpgradeError explains why a version change is not allowed
// (docs/specs/agent.md "Required upgrade stops").
type UpgradeError struct {
	ID, From, To string
	// Downgrade: To is not newer than From.
	Downgrade bool
	// Required is the version to upgrade to first (the next required stop),
	// if this binary embeds one (Registry.RequiredStop).
	Required string
	// MinFrom is the target's upgrade.min_from: the oldest version that may
	// upgrade to it directly. Without an embedded path, an earlier Orglane
	// release has to bring the instance there first.
	MinFrom string
}

func (e *UpgradeError) Error() string {
	switch {
	case e.Downgrade:
		return fmt.Sprintf("%s: %s cannot be changed to the older or equal version %s", e.ID, e.From, e.To)
	case e.Required != "":
		return fmt.Sprintf("%s: %s cannot upgrade to %s directly; upgrade to %s first", e.ID, e.From, e.To, e.Required)
	default:
		return fmt.Sprintf("%s: no embedded upgrade path from %s to %s (%s needs at least %s)", e.ID, e.From, e.To, e.To, e.MinFrom)
	}
}

// CheckUpgrade reports whether an instance running version from may upgrade
// directly to the package: the package must be newer, and not newer than
// what its upgrade.min_from allows. from need not be embedded.
func (p *Package) CheckUpgrade(from string) error {
	fv, err := ParseVersion(from)
	if err != nil {
		return err
	}
	tv, _ := ParseVersion(p.Manifest.Version) // validated on load
	if tv.Compare(fv) <= 0 {
		return &UpgradeError{ID: p.Manifest.ID, From: from, To: p.Manifest.Version, Downgrade: true}
	}
	if !p.acceptsFrom(fv) {
		return &UpgradeError{ID: p.Manifest.ID, From: from, To: p.Manifest.Version, MinFrom: p.Manifest.Upgrade.MinFrom}
	}
	return nil
}

func (p *Package) acceptsFrom(from Version) bool {
	if p.Manifest.Upgrade.MinFrom == "" {
		return true
	}
	minFrom, _ := ParseVersion(p.Manifest.Upgrade.MinFrom) // validated on load
	return from.Compare(minFrom) >= 0
}

// UpgradePath returns the versions an instance of package id upgrades
// through, one snapshot, upgrade and commit each, to get from version from
// to version to: every step but the last is a required stop, the last is
// to. It takes the newest embedded version that accepts the running
// version at each step, so the path has as few steps as the embedded
// versions allow. from need not be embedded; to must be. The error is an
// *UpgradeError when no embedded path exists.
func (r *Registry) UpgradePath(id, from, to string) ([]string, error) {
	target, ok := r.Get(agentapi.PackageRef{ID: id, Version: to})
	if !ok {
		return nil, fmt.Errorf("packages: %s@%s is not embedded", id, to)
	}
	fv, err := ParseVersion(from)
	if err != nil {
		return nil, err
	}
	tv, _ := ParseVersion(to)
	if tv.Compare(fv) <= 0 {
		return nil, &UpgradeError{ID: id, From: from, To: to, Downgrade: true}
	}
	var path []string
	cur := fv
	for cur.Compare(tv) < 0 {
		var next *Package
		var nextV Version
		for _, ref := range r.List() {
			if ref.ID != id {
				continue
			}
			v, _ := ParseVersion(ref.Version)
			if v.Compare(cur) <= 0 || v.Compare(tv) > 0 {
				continue
			}
			p, _ := r.Get(ref)
			if p.acceptsFrom(cur) && (next == nil || v.Compare(nextV) > 0) {
				next, nextV = p, v
			}
		}
		if next == nil {
			return nil, &UpgradeError{ID: id, From: from, To: to, MinFrom: target.Manifest.Upgrade.MinFrom}
		}
		path = append(path, next.Manifest.Version)
		cur = nextV
	}
	return path, nil
}

// Latest returns the newest embedded version of a package.
func (r *Registry) Latest(id string) (*Package, bool) {
	var best *Package
	for _, ref := range r.List() { // sorted by ID and version
		if ref.ID == id {
			best, _ = r.Get(ref)
		}
	}
	return best, best != nil
}

// RequiredStop returns the version an instance running from has to
// upgrade to before it can reach to: the first step of UpgradePath when the
// path has more than one step, "" otherwise (a direct upgrade, or no
// embedded path).
func (r *Registry) RequiredStop(id, from, to string) string {
	if path, err := r.UpgradePath(id, from, to); err == nil && len(path) > 1 {
		return path[0]
	}
	return ""
}
