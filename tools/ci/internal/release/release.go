// Package release decides what a release publishes as container image tags
// (issue #10).
//
// A release tag is a semantic version with a "v" prefix (v0.1.0,
// v1.0.0-rc.1). Every release publishes its exact version (0.1.0) once its
// image is built and verified. The moving aliases are derived from the
// registry, not from the release being published:
//
//   - latest points at the newest published release;
//   - X.Y points at the newest published patch of that line.
//
// Prereleases (anything with a "-" suffix) publish only their exact version
// and never move an alias.
//
// Because the aliases are recomputed from the set of published versions,
// which only grows, they can only move forward: a release that finishes
// after a newer one (v0.1.0 after v0.1.1, or v0.1.5 after v0.2.0) leaves
// latest and the line alias where they are, and any promotion run fixes up
// aliases that an earlier, skipped run did not move.
package release

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
)

// semver is a SemVer 2.0.0 version without build metadata ("+..." is not
// allowed in image tags).
var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?$`)

// maxDockerTag is the maximum length of a Docker image tag.
const maxDockerTag = 128

// Version is a parsed release version.
type Version struct {
	Major, Minor, Patch uint64
	// Prerelease is the part after "-", empty for a release.
	Prerelease string
}

// String returns the version without a "v" prefix; it is also the exact
// image tag and the version the binary reports (orglane cli version).
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

// Line is the minor line alias of the version, for example "0.1".
func (v Version) Line() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

func compare(a, b Version) int {
	return cmp.Or(cmp.Compare(a.Major, b.Major), cmp.Compare(a.Minor, b.Minor), cmp.Compare(a.Patch, b.Patch))
}

func parse(s string) (Version, bool) {
	m := semver.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	var v Version
	for i, p := range []*uint64{&v.Major, &v.Minor, &v.Patch} {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return Version{}, false
		}
		*p = n
	}
	v.Prerelease = m[4]
	return v, true
}

// ParseTag parses a release tag such as v1.2.3 or v1.2.3-rc.1.
func ParseTag(tag string) (Version, error) {
	if len(tag) < 2 || tag[0] != 'v' {
		return Version{}, fmt.Errorf("release tag %q must start with v, like v1.2.3 or v1.2.3-rc.1", tag)
	}
	v, ok := parse(tag[1:])
	if !ok {
		return Version{}, fmt.Errorf("release tag %q is not a semantic version like v1.2.3 or v1.2.3-rc.1", tag)
	}
	if len(tag)-1 > maxDockerTag {
		return Version{}, fmt.Errorf("release tag %q is longer than an image tag may be (%d characters)", tag, maxDockerTag)
	}
	return v, nil
}

// Alias is a moving image tag and the exact version tag it must point at.
type Alias struct {
	Tag     string // "latest" or a line such as "0.1"
	Version string // exact version tag, such as "0.1.5"
}

// Aliases returns the aliases implied by the image tags in the registry:
// latest for the newest release and one alias per minor line for its newest
// patch. Prereleases, aliases and any other tags are ignored. latest comes
// first, then the lines from newest to oldest.
func Aliases(registryTags []string) []Alias {
	newest := map[string]Version{}
	var latest *Version
	for _, t := range registryTags {
		v, ok := parse(t)
		if !ok || v.Prerelease != "" {
			continue
		}
		if cur, ok := newest[v.Line()]; !ok || compare(v, cur) > 0 {
			newest[v.Line()] = v
		}
		if latest == nil || compare(v, *latest) > 0 {
			latest = &v
		}
	}
	if latest == nil {
		return nil
	}
	lines := make([]Version, 0, len(newest))
	for _, v := range newest {
		lines = append(lines, v)
	}
	slices.SortFunc(lines, func(a, b Version) int { return compare(b, a) })

	aliases := []Alias{{Tag: "latest", Version: latest.String()}}
	for _, v := range lines {
		aliases = append(aliases, Alias{Tag: v.Line(), Version: v.String()})
	}
	return aliases
}
